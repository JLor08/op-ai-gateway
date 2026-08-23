// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
)

type captureSinkKey struct{}

// CaptureSink collects the raw bytes + headers the provider exchanges with the
// UPSTREAM, so the gateway can persist the TRANSLATED communication. It matters
// only for the built-in translation path: native passthrough already streams the
// upstream bytes back to the client (and captures them there). The gateway
// constructs a sink ONLY when capturing is enabled and threads it via context
// (WithCaptureSink); providers that speak to an upstream record the request
// headers+body and tee the response headers+body into it. The response is bounded
// by respCap. Every method is nil-safe so providers can call them unconditionally.
type CaptureSink struct {
	respCap int

	mu          sync.Mutex
	reqHeaders  http.Header
	reqBody     []byte
	respHeaders http.Header
	respBody    bytes.Buffer
}

// NewCaptureSink returns a sink that keeps at most respCap bytes of the upstream
// response body (respCap <= 0 keeps none).
func NewCaptureSink(respCap int) *CaptureSink {
	return &CaptureSink{respCap: respCap}
}

// WithCaptureSink returns ctx carrying sink. A nil sink leaves ctx unchanged, so a
// non-capturing request threads no sink and providers no-op.
func WithCaptureSink(ctx context.Context, sink *CaptureSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, captureSinkKey{}, sink)
}

// CaptureSinkFrom returns the sink carried by ctx (via WithCaptureSink), or nil
// when none is present. Provider implementations call it to record the upstream
// exchange; nil is the not-capturing signal and every CaptureSink method is
// nil-safe, so callers need no guard.
func CaptureSinkFrom(ctx context.Context) *CaptureSink {
	s, _ := ctx.Value(captureSinkKey{}).(*CaptureSink)
	return s
}

// RecordRequest stores the upstream request headers (cloned) + body (copied).
func (s *CaptureSink) RecordRequest(h http.Header, body []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqHeaders = h.Clone()
	s.reqBody = append([]byte(nil), body...)
}

// RecordResponseHeaders stores the upstream response headers (cloned).
func (s *CaptureSink) RecordResponseHeaders(h http.Header) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respHeaders = h.Clone()
}

// WriteResponse appends up to respCap total bytes of the raw upstream response.
func (s *CaptureSink) WriteResponse(b []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.respBody.Len() >= s.respCap {
		return
	}
	if remaining := s.respCap - s.respBody.Len(); len(b) > remaining {
		b = b[:remaining]
	}
	s.respBody.Write(b)
}

// ResponseWriter returns an io.Writer teeing into WriteResponse, or nil for a nil
// sink (so callers skip io.TeeReader entirely when not capturing).
func (s *CaptureSink) ResponseWriter() io.Writer {
	if s == nil {
		return nil
	}
	return captureRespWriter{s}
}

type captureRespWriter struct{ s *CaptureSink }

// Write always reports a full write so an io.TeeReader never sees a short-write
// error even after the bounded buffer stops accepting bytes.
func (w captureRespWriter) Write(b []byte) (int, error) {
	w.s.WriteResponse(b)
	return len(b), nil
}

// RequestHeaders / RequestBody / ResponseHeaders / ResponseBody return the
// collected upstream exchange for the gateway to persist (all nil-safe).
func (s *CaptureSink) RequestHeaders() http.Header {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqHeaders
}

func (s *CaptureSink) RequestBody() []byte {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqBody
}

func (s *CaptureSink) ResponseHeaders() http.Header {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.respHeaders
}

func (s *CaptureSink) ResponseBody() []byte {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.respBody.Bytes()...)
}
