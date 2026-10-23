// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// countingRefusalConn counts the bytes the SERVER reads off this connection and,
// at the instant the server first WRITES to it (the response flush), snapshots
// that read tally. The snapshot is "how much request body the server consumed
// BEFORE the first response byte reached the client" -- exactly the pre-response
// drain FIX A1 removes.
type countingRefusalConn struct {
	net.Conn
	readN           *atomic.Int64
	firstWriteReadN *atomic.Int64
	wroteFirst      *atomic.Bool
}

func (c *countingRefusalConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.readN.Add(int64(n))
	return n, err
}

func (c *countingRefusalConn) Write(p []byte) (int, error) {
	// The first socket write is the response flush. net/http's pre-response body
	// drain (chunkWriter.writeHeader) reads from the connection BEFORE this write,
	// so readN snapshotted here is the body consumed ahead of the response.
	if c.wroteFirst.CompareAndSwap(false, true) {
		c.firstWriteReadN.Store(c.readN.Load())
	}
	return c.Conn.Write(p)
}

type countingRefusalListener struct {
	net.Listener
	readN           *atomic.Int64
	firstWriteReadN *atomic.Int64
	wroteFirst      *atomic.Bool
}

func (l *countingRefusalListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &countingRefusalConn{Conn: c, readN: l.readN, firstWriteReadN: l.firstWriteReadN, wroteFirst: l.wroteFirst}, nil
}

// TestRouterLiveRefusalDoesNotDrainStreamedBody is the regression for FIX A1: a
// REFUSAL on POST /live/{model} (here a nil manager -> 404
// runtime.model_not_managed) must reach the client WITHOUT the server first
// draining the client's streamed PCM upload.
//
// The defect: Go's net/http, on a non-full-duplex HTTP/1.1 connection, drains up
// to ~256 KiB (maxPostHandlerReadBytes) of the UNREAD request body in
// chunkWriter.writeHeader -- BEFORE the response is flushed -- so a refusal
// written while full-duplex was still off made the client stream ~8 s of 16 kHz
// mono s16le into the void before the refusal reached it. net/http gates that
// pre-response drain on w.fullDuplex (server.go: "!w.fullDuplex"), so enabling
// full-duplex at the TOP of serveLive -- ahead of the model guard and
// EnsureRunning -- makes EVERY path, refusals included, skip it. (A separate,
// unavoidable drain in finishRequest runs AFTER the response is flushed and so
// never delays the client; this test therefore measures the read tally at the
// FIRST response write, not at the end.)
//
// How it is proved deterministically: a wrapped connection counts the bytes the
// server reads, and snapshots that count the moment the server first writes the
// response. The client streams an endless body from a synchronous io.Pipe (each
// write blocks until read), so a draining server can pull as much as it wants.
// With full-duplex enabled early the server writes the 404 after reading only
// the request header (~one bufio read); without it the server reads 256 KiB+1
// first.
//
// RED without the move: with EnableFullDuplex still after the nil-manager guard,
// the guard returns before it runs, writeHeader drains 256 KiB+1 ahead of the
// response, and the snapshot trips the limit below. GREEN with the move: the
// snapshot stays near zero.
func TestRouterLiveRefusalDoesNotDrainStreamedBody(t *testing.T) {
	readN := &atomic.Int64{}
	firstWriteReadN := &atomic.Int64{}
	wroteFirst := &atomic.Bool{}

	srv := httptest.NewUnstartedServer(newRouter(nil)) // nil manager -> every /live is a 404 refusal
	srv.Listener = &countingRefusalListener{Listener: srv.Listener, readN: readN, firstWriteReadN: firstWriteReadN, wroteFirst: wroteFirst}
	srv.Start()
	defer srv.Close()

	// Stream an endless body from a synchronous pipe. A pre-response drain
	// (full-duplex off) would pull 256 KiB+1 from it before the 404; the producer
	// supplies on demand until aborted, so the drain is never starved into
	// finishing early and mimicking the fixed behavior.
	pr, pw := io.Pipe()
	abort := make(chan struct{})
	defer close(abort) // LIFO: unblocks the producer before srv.Close() waits on the connection
	go func() {
		chunk := bytes.Repeat([]byte("PCM"), 4096) // ~12 KiB per write
		for {
			select {
			case <-abort:
				_ = pw.CloseWithError(errors.New("live refusal-no-drain test aborted"))
				return
			default:
			}
			if _, err := pw.Write(chunk); err != nil {
				return // the client transport closed the body; nothing more to feed
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/live/whisper-1", pr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do streaming refusal request: %v", err)
	}
	// Snapshot is taken on the server's first response write, which has happened
	// by the time Do returns with the response header.
	consumedBeforeReply := firstWriteReadN.Load()

	defer resp.Body.Close() //nolint:errcheck // read-side close, best-effort
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", resp.StatusCode, body)
	}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope %q: %v", body, err)
	}
	if env.Error.Code != codeModelNotManaged {
		t.Fatalf("error code = %q, want %q", env.Error.Code, codeModelNotManaged)
	}
	if !wroteFirst.Load() {
		t.Fatal("server never wrote a response (snapshot not taken)")
	}

	// The refusal must have been flushed before the streamed body was drained.
	// The limit sits well below net/http's 256 KiB maxPostHandlerReadBytes and
	// above the single header-sized bufio read the server makes before replying:
	// ~one 4 KiB read (fixed) vs >=256 KiB (pre-response drain).
	const limit = 64 << 10
	if consumedBeforeReply >= limit {
		t.Fatalf("server consumed %d body bytes before flushing the 404 (>= %d KiB): the refusal drained the streamed PCM because full-duplex was enabled too late (FIX A1)", consumedBeforeReply, limit>>10)
	}
}
