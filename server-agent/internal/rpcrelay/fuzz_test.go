// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package rpcrelay

import (
	"bytes"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// FuzzRelayFirstBytes feeds arbitrary opening bytes through the relay. From an
// unlisted source nothing may reach the member and nothing may come back; from
// the allowlisted head the bytes arrive at the member exactly as sent. Neither
// may panic the relay.
func FuzzRelayFirstBytes(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add([]byte("\x00\x00\x00\x00\x00\x00\x00\x00\x00")) // an RPC_CMD_HELLO-shaped frame
	f.Add(bytes.Repeat([]byte{0xff}, 70_000))
	f.Add([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))

	ev := &recEvents{}
	m := NewManager(ev, always)
	f.Cleanup(m.Close)

	// The refused member's target counts every connection it ever sees.
	refusedTarget := newFuzzTarget(f)
	allowedTarget := newFuzzTarget(f)
	refused := Config{SpecID: "rpcm_refused", Bind: fuzzBind(f), Target: refusedTarget.ln.Addr().String(), AllowedIPs: allow("192.0.2.1"), HeadSpecID: "h"}
	allowed := Config{SpecID: "rpcm_allowed", Bind: fuzzBind(f), Target: allowedTarget.ln.Addr().String(), AllowedIPs: allow("127.0.0.1"), HeadSpecID: "h"}
	if err := m.Apply([]Config{refused, allowed}); err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() {
		time.Sleep(50 * time.Millisecond)
		if n := refusedTarget.accepted.Load(); n != 0 {
			f.Errorf("an unlisted source reached the member (%d connection(s))", n)
		}
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Unlisted: closed without a byte either way.
		c, err := net.DialTimeout("tcp4", refused.Bind, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c.Write(data)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, _ := c.Read(make([]byte, 1)); n != 0 {
			t.Fatal("an unlisted source received bytes")
		}
		c.Close()
		// The relay decides before it reads, so a leak would show up at the
		// latest while the allowlisted half below runs; it is checked again
		// after that, and once more when fuzzing ends.
		if n := refusedTarget.accepted.Load(); n != 0 {
			t.Fatalf("an unlisted source reached the member (%d connection(s))", n)
		}

		// Allowlisted: delivered unaltered. The member reads exactly what was
		// sent and closes first, so the relay and then this client only ever
		// close passively: the fuzzer opens connections far faster than
		// TIME_WAIT sockets on the client side would release their ports.
		// Without payload there is nothing to deliver (and the relay never
		// dials the member for a silent connection).
		if len(data) == 0 {
			return
		}
		allowedTarget.reset()
		allowedTarget.want <- len(data)
		c, err = net.DialTimeout("tcp4", allowed.Bind, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write(data); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-allowedTarget.conns:
			if !bytes.Equal(got, data) {
				t.Fatalf("member received %d byte(s), want the %d sent, unaltered", len(got), len(data))
			}
		case <-time.After(5 * time.Second):
			t.Fatal("allowlisted bytes never reached the member")
		}
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
			t.Fatalf("session not closed after the member closed (n=%d, err=%v)", n, err)
		}
		if n := refusedTarget.accepted.Load(); n != 0 {
			t.Fatalf("an unlisted source reached the member (%d connection(s))", n)
		}
	})
}

// fuzzTarget stands in for the member. A connection to the refused member's
// target must never happen; each one to the allowed member's target reads the
// number of bytes announced on want, delivers them on conns and closes.
type fuzzTarget struct {
	ln       net.Listener
	accepted atomic.Int32
	want     chan int
	conns    chan []byte
}

func newFuzzTarget(f *testing.F) *fuzzTarget {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		f.Fatal(err)
	}
	ft := &fuzzTarget{ln: ln, want: make(chan int, 1), conns: make(chan []byte, 16)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			ft.accepted.Add(1)
			go func() {
				defer c.Close()
				n := 0
				select {
				case n = <-ft.want:
				default:
				}
				got := make([]byte, n)
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				k, _ := io.ReadFull(c, got)
				ft.conns <- got[:k]
			}()
		}
	}()
	f.Cleanup(func() { ln.Close() })
	return ft
}

// reset drops what an earlier, failed input left behind, so one failure cannot
// wedge every later input behind it.
func (ft *fuzzTarget) reset() {
	for {
		select {
		case <-ft.want:
		case <-ft.conns:
		default:
			return
		}
	}
}

func fuzzBind(f *testing.F) string {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		f.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}
