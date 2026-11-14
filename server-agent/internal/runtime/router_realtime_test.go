// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// realtimeWait bounds every wait in these tests. Nothing here sleeps: each test
// waits on a channel or a read, and this is only the point at which a wait that
// never resolves fails the test instead of hanging it.
const realtimeWait = 5 * time.Second

// realtimeManager is a managerPort fake that admits the models it knows and
// counts what the router does with the admission: held is the number of
// admissions not yet released, releases the number of release() calls, and
// released receives one value per release() call (buffered, so a double release
// is counted rather than blocking). ensured receives every model it is asked to
// admit.
type realtimeManager struct {
	endpoints map[string]string // model or instance address -> child endpoint
	held      atomic.Int64
	releases  atomic.Int64
	released  chan struct{}
	ensured   chan string
}

func newRealtimeManager(endpoints map[string]string) *realtimeManager {
	return &realtimeManager{
		endpoints: endpoints,
		released:  make(chan struct{}, 8),
		ensured:   make(chan string, 8),
	}
}

func (m *realtimeManager) EnsureRunning(ctx context.Context, model string) (string, func(), error) {
	return m.EnsureServing(ctx, model)
}

func (m *realtimeManager) EnsureServing(_ context.Context, model string) (string, func(), error) {
	select {
	case m.ensured <- model:
	default:
	}
	endpoint, ok := m.endpoints[model]
	if !ok {
		return "", nil, fmt.Errorf("runtime: %q: %w", model, ErrModelNotManaged)
	}
	m.held.Add(1)
	return endpoint, func() {
		m.held.Add(-1)
		m.releases.Add(1)
		m.released <- struct{}{}
	}, nil
}

// ResolveAddress reads an instance address the way the manager does for a
// replicable spec: "m#k" is instance k of m when the fake knows the address.
func (m *realtimeManager) ResolveAddress(model string) (string, int, bool) {
	base, k := ParseInstanceAddress(model)
	_, ok := m.endpoints[model]
	return base, k, ok
}
func (m *realtimeManager) LoadedModels() []string { return nil }
func (m *realtimeManager) Status() []Status       { return nil }

// childEnd is how a session ended at the fake child: the close status and
// reason it read, or self when the child ended the session itself.
type childEnd struct {
	code   websocket.StatusCode
	reason string
	self   bool
}

// realtimeChild is a fake realtime child: a WebSocket server at /v1/realtime
// that greets each session with one text and one binary message, echoes every
// message with its type, and obeys four text commands -- "child:close" closes
// with 4001 "child says bye", "child:drop" drops the connection without a close
// frame, "child:big" sends one binary message a byte over the router's limit,
// and "child:flood" starts sending text messages back to back until a write
// fails. seen receives each handshake's URL and headers its header, ended how
// each session ended, and oversize the length of any message over the router's
// limit that reached it (which it does not echo: the router must never have let
// it through).
type realtimeChild struct {
	srv      *httptest.Server
	seen     chan *url.URL
	headers  chan http.Header
	ended    chan childEnd
	oversize chan int
}

func newRealtimeChild(t *testing.T) *realtimeChild {
	t.Helper()
	c := &realtimeChild{
		seen:     make(chan *url.URL, 4),
		headers:  make(chan http.Header, 4),
		ended:    make(chan childEnd, 4),
		oversize: make(chan int, 4),
	}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != realtimeChildPath {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.SetReadLimit(-1)
		u := *r.URL
		c.seen <- &u
		c.headers <- r.Header.Clone()
		ctx := context.Background()
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created"}`))
		_ = conn.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3})
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				var ce websocket.CloseError
				errors.As(err, &ce)
				c.ended <- childEnd{code: websocket.CloseStatus(err), reason: ce.Reason}
				_ = conn.CloseNow()
				return
			}
			switch {
			case len(data) > realtimeMessageLimit:
				c.oversize <- len(data)
			case typ == websocket.MessageText && string(data) == "child:close":
				_ = conn.Close(4001, "child says bye")
				c.ended <- childEnd{self: true}
				return
			case typ == websocket.MessageText && string(data) == "child:drop":
				_ = conn.CloseNow()
				c.ended <- childEnd{self: true}
				return
			case typ == websocket.MessageText && string(data) == "child:big":
				_ = conn.Write(ctx, websocket.MessageBinary, make([]byte, realtimeMessageLimit+1))
			case typ == websocket.MessageText && string(data) == "child:flood":
				go func() {
					for {
						if err := conn.Write(ctx, websocket.MessageText, []byte("flood")); err != nil {
							return
						}
					}
				}()
			default:
				_ = conn.Write(ctx, typ, data)
			}
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// realtimeRig is a router over a realtimeManager, served by a real HTTP server
// (a WebSocket upgrade needs a real connection to hijack). done receives one
// value each time a /realtime handler has returned -- release() included, since
// it is deferred inside the handler -- which is when "release ran exactly once"
// can be asserted without waiting for a second release that might still come.
// m is nil for a rig over another managerPort (newRealtimeRigOver).
type realtimeRig struct {
	rt   *router
	m    *realtimeManager
	srv  *httptest.Server
	done chan struct{}
}

func newRealtimeRig(t *testing.T, endpoints map[string]string, tune func(*router)) *realtimeRig {
	t.Helper()
	m := newRealtimeManager(endpoints)
	rig := newRealtimeRigOver(t, m, tune)
	rig.m = m
	return rig
}

// newRealtimeRigOver is newRealtimeRig over any managerPort.
func newRealtimeRigOver(t *testing.T, mp managerPort, tune func(*router)) *realtimeRig {
	t.Helper()
	rt := newRouter(mp)
	if tune != nil {
		tune(rt)
	}
	rig := &realtimeRig{rt: rt, done: make(chan struct{}, 8)}
	rig.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, realtimePathPrefix) {
			rig.done <- struct{}{}
		}
	}))
	t.Cleanup(rig.srv.Close)
	t.Cleanup(rt.Close)
	return rig
}

// wsURL is the ws:// form of path on srv.
func wsURL(srv *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + path
}

// dialRealtime opens a client session on path and fails the test unless the
// upgrade succeeds. The client reads messages of any size, so a test can see
// exactly what the router lets through.
func dialRealtime(t *testing.T, srv *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), realtimeWait)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(srv, path), nil)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	c.SetReadLimit(-1)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// readMessage reads one message from c, failing the test on an error.
func readMessage(t *testing.T, c *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), realtimeWait)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read a message: %v", err)
	}
	return typ, data
}

// writeMessage writes one message to c, failing the test on an error.
func writeMessage(t *testing.T, c *websocket.Conn, typ websocket.MessageType, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), realtimeWait)
	defer cancel()
	if err := c.Write(ctx, typ, data); err != nil {
		t.Fatalf("write a message: %v", err)
	}
}

// readGreeting reads the fake child's two greeting messages through the router,
// which also proves the session is up end to end.
func readGreeting(t *testing.T, c *websocket.Conn) {
	t.Helper()
	if typ, data := readMessage(t, c); typ != websocket.MessageText || string(data) != `{"type":"session.created"}` {
		t.Fatalf("first message = %v %q, want the child's text greeting", typ, data)
	}
	if typ, data := readMessage(t, c); typ != websocket.MessageBinary || !bytes.Equal(data, []byte{1, 2, 3}) {
		t.Fatalf("second message = %v %v, want the child's binary greeting", typ, data)
	}
}

// readClose reads c until it fails, skipping data messages, and returns the
// close status and reason the router sent.
func readClose(t *testing.T, c *websocket.Conn) (websocket.StatusCode, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), realtimeWait)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			var ce websocket.CloseError
			errors.As(err, &ce)
			return websocket.CloseStatus(err), ce.Reason
		}
	}
}

// recv waits for one value on ch.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(realtimeWait):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// assertHeld fails the test unless the session holds exactly one admission and
// has released nothing.
func (rig *realtimeRig) assertHeld(t *testing.T) {
	t.Helper()
	if held, releases := rig.m.held.Load(), rig.m.releases.Load(); held != 1 || releases != 0 {
		t.Fatalf("while the session is open: held = %d, releases = %d; want the one admission held and nothing released", held, releases)
	}
}

// assertReleasedOnce waits for the release and for the handler to return, then
// fails the test unless release() ran exactly once.
func (rig *realtimeRig) assertReleasedOnce(t *testing.T) {
	t.Helper()
	recv(t, rig.m.released, "the session's release")
	recv(t, rig.done, "the realtime handler to return")
	if held, releases := rig.m.held.Load(), rig.m.releases.Load(); held != 0 || releases != 1 {
		t.Fatalf("after the session: held = %d, releases = %d; want nothing held and exactly one release", held, releases)
	}
}

// TestRouterRealtimeRelaysBothWays is the headline case for GET
// /realtime/{model}: the child is dialed at /v1/realtime with the inbound query
// and model= set to the path model (a stray model= is replaced, never
// appended), every message crosses in both directions with its type kept, and
// the admission is held for the whole session.
func TestRouterRealtimeRelaysBothWays(t *testing.T) {
	child := newRealtimeChild(t)
	rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, nil)

	c := dialRealtime(t, rig.srv, "/realtime/m?intent=transcription&model=stray")
	u := recv(t, child.seen, "the child's handshake")
	if u.Path != realtimeChildPath {
		t.Errorf("child path = %q, want %q", u.Path, realtimeChildPath)
	}
	if u.RawQuery != "intent=transcription&model=m" {
		t.Errorf("child query = %q, want intent=transcription&model=m: the inbound params with model set to the path model", u.RawQuery)
	}

	readGreeting(t, c) // child -> client, text and binary
	writeMessage(t, c, websocket.MessageText, []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`))
	if typ, data := readMessage(t, c); typ != websocket.MessageText || string(data) != `{"type":"input_audio_buffer.append","audio":"AAAA"}` {
		t.Fatalf("echoed text = %v %q, want the text message back as text", typ, data)
	}
	writeMessage(t, c, websocket.MessageBinary, []byte{9, 8, 7})
	if typ, data := readMessage(t, c); typ != websocket.MessageBinary || !bytes.Equal(data, []byte{9, 8, 7}) {
		t.Fatalf("echoed binary = %v %v, want the binary message back as binary", typ, data)
	}
	rig.assertHeld(t)

	if err := c.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("client close: %v", err)
	}
	if end := recv(t, child.ended, "the child's end"); end.code != websocket.StatusNormalClosure || end.reason != "bye" {
		t.Errorf("child end = %+v, want the client's 1000 \"bye\"", end)
	}
	rig.assertReleasedOnce(t)
}

// TestRouterRealtimeChildHeader: the child is dialed with the inbound header --
// an Authorization the child may check passes -- minus what belongs to the
// inbound handshake alone: its Sec-WebSocket-* offers (a subprotocol or an
// extension the router does not speak would fail the child's answer) and its
// Origin (the router, not a browser, is the child's client).
func TestRouterRealtimeChildHeader(t *testing.T) {
	child := newRealtimeChild(t)
	rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), realtimeWait)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(rig.srv, "/realtime/m"), &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {"Bearer spec-token"},
			"Origin":        {"http://" + rig.srv.Listener.Addr().String()},
		},
		Subprotocols: []string{"realtime"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	if c.Subprotocol() != "" {
		t.Errorf("negotiated subprotocol %q, want none: the router speaks none", c.Subprotocol())
	}

	h := recv(t, child.headers, "the child's handshake header")
	if got := h.Get("Authorization"); got != "Bearer spec-token" {
		t.Errorf("child Authorization = %q, want the inbound one", got)
	}
	if got := h.Get("Origin"); got != "" {
		t.Errorf("child Origin = %q, want none", got)
	}
	if got := h.Get("Sec-WebSocket-Protocol"); got != "" {
		t.Errorf("child Sec-WebSocket-Protocol = %q, want none: the inbound offer is not the router's", got)
	}
	readGreeting(t, c)
	_ = c.Close(websocket.StatusNormalClosure, "")
	rig.assertReleasedOnce(t)
}

// TestRouterRealtimeEveryEndReleasesOnce: however a session ends -- the client
// closes, the child closes, the child drops, the session idles out, the router
// closes -- the other side is closed with the cause's status, and the
// admission held for the session is released exactly once.
func TestRouterRealtimeEveryEndReleasesOnce(t *testing.T) {
	cases := []struct {
		name string
		tune func(*router)
		// end ends the open session and asserts what the client saw.
		end func(t *testing.T, rig *realtimeRig, c *websocket.Conn)
		// wantChild is how the child must have seen the end.
		wantChild childEnd
	}{
		{
			name: "client closes",
			end: func(t *testing.T, _ *realtimeRig, c *websocket.Conn) {
				if err := c.Close(4002, "client says bye"); err != nil {
					t.Fatalf("client close: %v", err)
				}
			},
			wantChild: childEnd{code: 4002, reason: "client says bye"},
		},
		{
			name: "child closes",
			end: func(t *testing.T, _ *realtimeRig, c *websocket.Conn) {
				writeMessage(t, c, websocket.MessageText, []byte("child:close"))
				if code, reason := readClose(t, c); code != 4001 || reason != "child says bye" {
					t.Fatalf("client close = %d %q, want the child's own 4001 \"child says bye\"", code, reason)
				}
			},
			wantChild: childEnd{self: true},
		},
		{
			name: "child drops",
			end: func(t *testing.T, _ *realtimeRig, c *websocket.Conn) {
				writeMessage(t, c, websocket.MessageText, []byte("child:drop"))
				if code, _ := readClose(t, c); code != websocket.StatusInternalError {
					t.Fatalf("client close = %d, want 1011: a drop has no status that could be sent on", code)
				}
			},
			wantChild: childEnd{self: true},
		},
		{
			name: "idle",
			tune: func(rt *router) { rt.realtimeIdle = 20 * time.Millisecond },
			end: func(t *testing.T, _ *realtimeRig, c *websocket.Conn) {
				if code, reason := readClose(t, c); code != websocket.StatusPolicyViolation || reason != realtimeReasonIdle {
					t.Fatalf("client close = %d %q, want 1008 %q", code, reason, realtimeReasonIdle)
				}
			},
			wantChild: childEnd{code: websocket.StatusPolicyViolation, reason: realtimeReasonIdle},
		},
		{
			name: "router closes",
			end: func(t *testing.T, rig *realtimeRig, c *websocket.Conn) {
				closed := make(chan struct{})
				go func() {
					rig.rt.Close()
					close(closed)
				}()
				if code, reason := readClose(t, c); code != websocket.StatusGoingAway || reason != realtimeReasonClosing {
					t.Fatalf("client close = %d %q, want 1001 %q", code, reason, realtimeReasonClosing)
				}
				recv(t, closed, "the router's Close to return")
			},
			wantChild: childEnd{code: websocket.StatusGoingAway, reason: realtimeReasonClosing},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child := newRealtimeChild(t)
			rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, tc.tune)
			c := dialRealtime(t, rig.srv, "/realtime/m")
			if tc.tune == nil {
				// An idle session may already be over by now; every other one
				// must be up and holding its admission.
				readGreeting(t, c)
				rig.assertHeld(t)
			}
			tc.end(t, rig, c)
			if end := recv(t, child.ended, "the child's end"); end != tc.wantChild {
				t.Errorf("child end = %+v, want %+v", end, tc.wantChild)
			}
			rig.assertReleasedOnce(t)
		})
	}
}

// TestRouterRealtimeBusySessionIsNotIdle: the idle timeout counts silence, not
// session age -- a session that keeps relaying messages outlives its idle
// window twice over. Each round trip is real relay work, so the loop never
// sleeps; it stops after two idle windows have passed.
func TestRouterRealtimeBusySessionIsNotIdle(t *testing.T) {
	const idle = time.Second
	child := newRealtimeChild(t)
	rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, func(rt *router) { rt.realtimeIdle = idle })
	c := dialRealtime(t, rig.srv, "/realtime/m")
	readGreeting(t, c)
	for start := time.Now(); time.Since(start) < 2*idle; {
		writeMessage(t, c, websocket.MessageText, []byte("tick"))
		if _, data := readMessage(t, c); string(data) != "tick" {
			t.Fatalf("echo = %q, want tick", data)
		}
	}
	rig.assertHeld(t)
	_ = c.Close(websocket.StatusNormalClosure, "")
	rig.assertReleasedOnce(t)
}

// TestRouterRealtimeInstanceAddress: m#1 is admitted as the instance address
// and reaches instance 1's child, which sees the base name m as its model --
// the only name an instance's child knows.
func TestRouterRealtimeInstanceAddress(t *testing.T) {
	child0 := newRealtimeChild(t)
	child1 := newRealtimeChild(t)
	rig := newRealtimeRig(t, map[string]string{"m": child0.srv.URL, "m#1": child1.srv.URL}, nil)

	c := dialRealtime(t, rig.srv, "/realtime/m%231?intent=transcription")
	if got := recv(t, rig.m.ensured, "the admission"); got != "m#1" {
		t.Errorf("admitted %q, want the instance address m#1", got)
	}
	u := recv(t, child1.seen, "instance 1's handshake")
	if u.RawQuery != "intent=transcription&model=m" {
		t.Errorf("instance 1 query = %q, want intent=transcription&model=m: the base name in place of the address", u.RawQuery)
	}
	select {
	case u := <-child0.seen:
		t.Errorf("instance 0 was dialed too (%s)", u)
	default:
	}
	readGreeting(t, c)
	_ = c.Close(websocket.StatusNormalClosure, "")
	rig.assertReleasedOnce(t)
}

// TestRouterRealtimeMessageLimit: a message of exactly 4 MiB crosses in both
// directions; one byte more, from either side, ends the session with 1009 on
// both sides and releases the admission once.
func TestRouterRealtimeMessageLimit(t *testing.T) {
	t.Run("4 MiB crosses", func(t *testing.T) {
		child := newRealtimeChild(t)
		rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, nil)
		c := dialRealtime(t, rig.srv, "/realtime/m")
		readGreeting(t, c)
		exact := bytes.Repeat([]byte{'a'}, realtimeMessageLimit)
		writeMessage(t, c, websocket.MessageBinary, exact)
		if _, data := readMessage(t, c); len(data) != realtimeMessageLimit {
			t.Fatalf("echo of 4 MiB = %d bytes, want %d", len(data), realtimeMessageLimit)
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
		rig.assertReleasedOnce(t)
	})
	t.Run("one byte more from the client", func(t *testing.T) {
		child := newRealtimeChild(t)
		rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, nil)
		c := dialRealtime(t, rig.srv, "/realtime/m")
		readGreeting(t, c)
		writeMessage(t, c, websocket.MessageBinary, make([]byte, realtimeMessageLimit+1))
		if code, _ := readClose(t, c); code != websocket.StatusMessageTooBig {
			t.Fatalf("client close = %d, want 1009", code)
		}
		if end := recv(t, child.ended, "the child's end"); end.code != websocket.StatusMessageTooBig {
			t.Errorf("child end = %+v, want 1009", end)
		}
		select {
		case n := <-child.oversize:
			t.Errorf("the child received a %d-byte message; the router must stop it at %d", n, realtimeMessageLimit)
		default:
		}
		rig.assertReleasedOnce(t)
	})
	t.Run("one byte more from the child", func(t *testing.T) {
		child := newRealtimeChild(t)
		rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, nil)
		c := dialRealtime(t, rig.srv, "/realtime/m")
		readGreeting(t, c)
		writeMessage(t, c, websocket.MessageText, []byte("child:big"))
		if code, _ := readClose(t, c); code != websocket.StatusMessageTooBig {
			t.Fatalf("client close = %d, want 1009", code)
		}
		if end := recv(t, child.ended, "the child's end"); end.code != websocket.StatusMessageTooBig {
			t.Errorf("child end = %+v, want 1009", end)
		}
		rig.assertReleasedOnce(t)
	})
}

// TestRouterRealtimeOversizedFrameFromAStalledPeer: a peer that announces one
// frame far over the limit, sends a byte more than the limit and then stalls --
// neither sending the rest nor reading -- must not hold the session open. The
// router stops reading at the limit and closes that socket without waiting for
// a close handshake, which would first have to read the frame's unsent rest;
// the child is closed with 1009, and the admission is released once.
func TestRouterRealtimeOversizedFrameFromAStalledPeer(t *testing.T) {
	child := newRealtimeChild(t)
	rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, nil)

	conn, err := net.Dial("tcp", rig.srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial the router: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, _ = fmt.Fprintf(conn, "GET /realtime/m HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", rig.srv.Listener.Addr())
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake = %v, %v; want 101", resp, err)
	}

	// One masked binary frame announcing 8 MiB (mask key 0, so the payload is
	// sent as is), of which only the limit plus one byte is ever sent.
	header := []byte{0x82, 0x80 | 127, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint64(header[2:10], 8<<20)
	if _, err := conn.Write(append(header, make([]byte, realtimeMessageLimit+1)...)); err != nil {
		t.Fatalf("write the oversized frame: %v", err)
	}

	if end := recv(t, child.ended, "the child's end"); end.code != websocket.StatusMessageTooBig {
		t.Errorf("child end = %+v, want 1009", end)
	}
	rig.assertReleasedOnce(t)
}

// TestRouterRealtimeCloseUnderTrafficRelaysTheClosersStatus: a close is
// relayed with the closer's own status and reason even while messages flow
// towards the closer. Without settleEnd, the pump writing to the closer fails
// the moment the closer's socket closes and races the closer's own pump, and
// the far side got 1011 "<closer> connection lost" in most runs. Each direction
// runs 50 sessions, so a race left open fails it all but certainly.
func TestRouterRealtimeCloseUnderTrafficRelaysTheClosersStatus(t *testing.T) {
	const runs = 50
	t.Run("the client closes while the child floods it", func(t *testing.T) {
		for i := range runs {
			child := newRealtimeChild(t)
			rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, nil)
			c := dialRealtime(t, rig.srv, "/realtime/m")
			readGreeting(t, c)
			writeMessage(t, c, websocket.MessageText, []byte("child:flood"))
			for range 20 { // the flood is flowing towards the client
				if _, data := readMessage(t, c); string(data) != "flood" {
					t.Fatalf("run %d: message = %q, want flood", i, data)
				}
			}
			_ = c.Close(4002, "client says bye")
			if end := recv(t, child.ended, "the child's end"); end.code != 4002 || end.reason != "client says bye" {
				t.Fatalf("run %d: child end = %+v, want the client's own 4002 \"client says bye\"", i, end)
			}
			rig.assertReleasedOnce(t)
		}
	})
	t.Run("the child closes while the client floods it", func(t *testing.T) {
		for i := range runs {
			child := newRealtimeChild(t)
			rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, nil)
			c := dialRealtime(t, rig.srv, "/realtime/m")
			readGreeting(t, c)
			flooding := make(chan struct{})
			go func() {
				defer close(flooding)
				for {
					if err := c.Write(context.Background(), websocket.MessageText, []byte("flood")); err != nil {
						return
					}
				}
			}()
			for range 20 { // the flood is flowing towards the child, which echoes it
				if _, data := readMessage(t, c); string(data) != "flood" {
					t.Fatalf("run %d: message = %q, want the echoed flood", i, data)
				}
			}
			writeMessage(t, c, websocket.MessageText, []byte("child:close"))
			if code, reason := readClose(t, c); code != 4001 || reason != "child says bye" {
				t.Fatalf("run %d: client close = %d %q, want the child's own 4001 \"child says bye\"", i, code, reason)
			}
			recv(t, flooding, "the client's flood to stop")
			rig.assertReleasedOnce(t)
		}
	})
}

// TestRealtimeSettleEnd: a read's end stands; a failed write's end gives way to
// the other pump's read end, but not to another failed write, and stands alone
// once the wait is over.
func TestRealtimeSettleEnd(t *testing.T) {
	read := realtimeEnd{code: 4002, reason: "client says bye", cause: realtimeSideClient}
	write := realtimeEnd{code: websocket.StatusInternalError, reason: "client connection lost", cause: realtimeSideClient, write: true}
	otherWrite := realtimeEnd{code: websocket.StatusInternalError, reason: "child connection lost", cause: realtimeSideChild, write: true}
	cases := []struct {
		name  string
		first realtimeEnd
		next  []realtimeEnd // queued on ends before settleEnd runs
		wait  time.Duration
		want  realtimeEnd
	}{
		{"a read stands", read, []realtimeEnd{write}, realtimeWait, read},
		{"a write gives way to a read", write, []realtimeEnd{read}, realtimeWait, read},
		{"a write does not give way to a write", write, []realtimeEnd{otherWrite}, realtimeWait, write},
		{"a write stands once the wait is over", write, nil, time.Millisecond, write},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ends := make(chan realtimeEnd, 2)
			for _, e := range tc.next {
				ends <- e
			}
			if got := settleEnd(tc.first, ends, tc.wait); got != tc.want {
				t.Errorf("settleEnd = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// realtimeHandshake is a valid WebSocket handshake request for target, built
// for a direct ServeHTTP call.
func realtimeHandshake(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return r
}

// TestRouterRealtimeRefusesABadHandshakeBeforeAdmission: a handshake
// websocket.Accept would refuse is refused before anything is admitted, so a
// malformed or cross-origin request never starts or evicts a model: 400
// runtime.realtime_upgrade_required for a malformed one, 403
// runtime.realtime_origin_forbidden for a foreign Origin.
func TestRouterRealtimeRefusesABadHandshakeBeforeAdmission(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(r *http.Request)
		status int
		code   string
	}{
		{"HTTP/1.0", func(r *http.Request) { r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/1.0", 1, 0 }, http.StatusBadRequest, codeRealtimeUpgradeRequired},
		{"no Upgrade", func(r *http.Request) { r.Header.Del("Upgrade") }, http.StatusBadRequest, codeRealtimeUpgradeRequired},
		{"no version", func(r *http.Request) { r.Header.Del("Sec-WebSocket-Version") }, http.StatusBadRequest, codeRealtimeUpgradeRequired},
		{"version 8", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "8") }, http.StatusBadRequest, codeRealtimeUpgradeRequired},
		{"no key", func(r *http.Request) { r.Header.Del("Sec-WebSocket-Key") }, http.StatusBadRequest, codeRealtimeUpgradeRequired},
		{"two keys", func(r *http.Request) { r.Header.Add("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==") }, http.StatusBadRequest, codeRealtimeUpgradeRequired},
		{"key not base64", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "not base64!") }, http.StatusBadRequest, codeRealtimeUpgradeRequired},
		{"key of 15 bytes", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "MDEyMzQ1Njc4OWFiY2Rl") }, http.StatusBadRequest, codeRealtimeUpgradeRequired},
		{"foreign Origin", func(r *http.Request) { r.Header.Set("Origin", "http://elsewhere.example") }, http.StatusForbidden, codeRealtimeOriginForbidden},
		{"opaque Origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, http.StatusForbidden, codeRealtimeOriginForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newRealtimeManager(map[string]string{"m": "http://127.0.0.1:1"})
			r := realtimeHandshake("/realtime/m")
			tc.edit(r)
			rec := httptest.NewRecorder()
			newRouter(m).ServeHTTP(rec, r)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if got := decodeRouterError(t, rec.Body); got.Code != tc.code {
				t.Errorf("code = %q, want %q", got.Code, tc.code)
			}
			select {
			case model := <-m.ensured:
				t.Errorf("admitted %q; a refused handshake must start nothing", model)
			default:
			}
		})
	}
	t.Run("an Origin naming the request's host passes", func(t *testing.T) {
		r := realtimeHandshake("/realtime/m")
		r.Header.Set("Origin", "http://"+r.Host)
		if refusal := realtimeHandshakeRefusal(r); refusal != nil {
			t.Errorf("refused %+v, want the handshake taken", *refusal)
		}
	})
}

// TestRouterRealtimeAcceptFailureDropsTheChild: when websocket.Accept fails
// after the child was dialed -- here a writer that cannot be hijacked -- the
// child sees its connection dropped, and the admission is released once.
func TestRouterRealtimeAcceptFailureDropsTheChild(t *testing.T) {
	child := newRealtimeChild(t)
	m := newRealtimeManager(map[string]string{"m": child.srv.URL})
	rec := httptest.NewRecorder() // no http.Hijacker
	newRouter(m).ServeHTTP(rec, realtimeHandshake("/realtime/m"))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want Accept's own 501 for a writer it cannot hijack", rec.Code)
	}
	recv(t, child.seen, "the child's handshake")
	if end := recv(t, child.ended, "the child's end"); end.code != -1 {
		t.Errorf("child end = %+v, want a drop without a close frame", end)
	}
	if held, releases := m.held.Load(), m.releases.Load(); held != 0 || releases != 1 {
		t.Fatalf("held = %d, releases = %d; want nothing held and exactly one release", held, releases)
	}
}

// blockingAdmission is a managerPort whose admission blocks until its context
// ends: entered receives a value when an admission starts, left the context's
// error when it gives up.
type blockingAdmission struct {
	entered chan struct{}
	left    chan error
}

func newBlockingAdmission() *blockingAdmission {
	return &blockingAdmission{entered: make(chan struct{}, 1), left: make(chan error, 1)}
}

func (b *blockingAdmission) EnsureRunning(ctx context.Context, model string) (string, func(), error) {
	return b.EnsureServing(ctx, model)
}

func (b *blockingAdmission) EnsureServing(ctx context.Context, model string) (string, func(), error) {
	b.entered <- struct{}{}
	<-ctx.Done()
	b.left <- ctx.Err()
	return "", nil, fmt.Errorf("runtime: admission of %q abandoned: %w", model, ctx.Err())
}
func (b *blockingAdmission) ResolveAddress(string) (string, int, bool) { return "", 0, false }
func (b *blockingAdmission) LoadedModels() []string                    { return nil }
func (b *blockingAdmission) Status() []Status                          { return nil }

// TestRouterRealtimeAdmissionIsAbandoned: an admission still waiting -- a cold
// start, say -- is abandoned when the client leaves, and when the router is
// closed: the session cannot be opened any more, so nothing may be started for
// it. The second case is the router's stop reaching the admission's context.
func TestRouterRealtimeAdmissionIsAbandoned(t *testing.T) {
	t.Run("the client leaves", func(t *testing.T) {
		b := newBlockingAdmission()
		rig := newRealtimeRigOver(t, b, nil)
		ctx, cancel := context.WithCancel(context.Background())
		dialed := make(chan error, 1)
		go func() {
			c, _, err := websocket.Dial(ctx, wsURL(rig.srv, "/realtime/m"), nil)
			if err == nil {
				_ = c.CloseNow()
			}
			dialed <- err
		}()
		recv(t, b.entered, "the admission to start")
		cancel()
		if err := recv(t, b.left, "the admission to be abandoned"); !errors.Is(err, context.Canceled) {
			t.Errorf("admission ended with %v, want context.Canceled", err)
		}
		recv(t, rig.done, "the realtime handler to return")
		if err := recv(t, dialed, "the client's dial"); err == nil {
			t.Error("the client's dial succeeded")
		}
	})
	t.Run("the router closes", func(t *testing.T) {
		b := newBlockingAdmission()
		rig := newRealtimeRigOver(t, b, nil)
		ctx, cancel := context.WithTimeout(context.Background(), realtimeWait)
		defer cancel()
		dialed := make(chan *http.Response, 1)
		go func() {
			c, resp, err := websocket.Dial(ctx, wsURL(rig.srv, "/realtime/m"), nil)
			if err == nil {
				_ = c.CloseNow()
			}
			dialed <- resp
		}()
		recv(t, b.entered, "the admission to start")
		closed := make(chan struct{})
		go func() {
			rig.rt.Close()
			close(closed)
		}()
		if err := recv(t, b.left, "the admission to be abandoned"); !errors.Is(err, context.Canceled) {
			t.Errorf("admission ended with %v, want context.Canceled", err)
		}
		if resp := recv(t, dialed, "the client's answer"); resp == nil || resp.StatusCode != http.StatusBadGateway {
			t.Errorf("client answer = %v, want 502 before any upgrade", resp)
		}
		recv(t, closed, "the router's Close to return")
	})
}

// decodeRouterError reads the router's JSON error envelope from body.
func decodeRouterError(t *testing.T, body io.Reader) errorBody {
	t.Helper()
	var env errorEnvelope
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		t.Fatalf("decode the error envelope: %v", err)
	}
	return env.Error
}

// TestRouterRealtimeDialFailureIs502BeforeUpgrade: a child that does not answer
// the handshake with 101 -- here a 404, what sglang-omni answers without
// --enable-realtime -- is a plain HTTP 502 runtime.upstream_gone: the inbound
// socket is never accepted, and the admission is released once.
func TestRouterRealtimeDialFailureIs502BeforeUpgrade(t *testing.T) {
	notRealtime := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notRealtime.Close)
	rig := newRealtimeRig(t, map[string]string{"m": notRealtime.URL}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), realtimeWait)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsURL(rig.srv, "/realtime/m"), nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("the upgrade succeeded against a child that refused the handshake")
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("response = %v, want an HTTP 502 before any upgrade (err %v)", resp, err)
	}
	if got := decodeRouterError(t, resp.Body); got.Code != codeUpstreamGone {
		t.Errorf("error code = %q, want %q", got.Code, codeUpstreamGone)
	}
	rig.assertReleasedOnce(t)
}

// TestRouterRealtimeDialErrorNamesNoQuery: a child that cannot be reached at all
// (the transport fails before any handshake) is a 502 whose message names the
// endpoint and path but never the client's query, which the transport's own
// *url.Error would carry.
func TestRouterRealtimeDialErrorNamesNoQuery(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	endpoint := gone.URL
	gone.Close() // nothing listens any more: connection refused
	rig := newRealtimeRig(t, map[string]string{"m": endpoint}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), realtimeWait)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsURL(rig.srv, "/realtime/m")+"?api_key=s3cr3t-query", nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("the upgrade succeeded against an unreachable child")
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("response = %v, want an HTTP 502 (err %v)", resp, err)
	}
	got := decodeRouterError(t, resp.Body)
	if strings.Contains(got.Message, "s3cr3t-query") || strings.Contains(got.Message, "?") {
		t.Errorf("error message %q names the query", got.Message)
	}
	if !strings.Contains(got.Message, endpoint+realtimeChildPath) {
		t.Errorf("error message %q does not name the endpoint and path", got.Message)
	}
	rig.assertReleasedOnce(t)
}

// TestRouterRealtimeRefusals: a request that is not a WebSocket handshake is
// 400 runtime.realtime_upgrade_required before anything is admitted, and an
// unknown model is the 404 runtime.model_not_managed /live answers.
func TestRouterRealtimeRefusals(t *testing.T) {
	child := newRealtimeChild(t)
	rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, nil)

	resp, err := rig.srv.Client().Get(rig.srv.URL + "/realtime/m")
	if err != nil {
		t.Fatalf("plain GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("plain GET status = %d, want 400", resp.StatusCode)
	}
	if got := decodeRouterError(t, resp.Body); got.Code != codeRealtimeUpgradeRequired {
		t.Errorf("plain GET code = %q, want %q", got.Code, codeRealtimeUpgradeRequired)
	}
	select {
	case model := <-rig.m.ensured:
		t.Errorf("a plain GET admitted %q; a request that cannot be upgraded must start nothing", model)
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), realtimeWait)
	defer cancel()
	c, resp2, err := websocket.Dial(ctx, wsURL(rig.srv, "/realtime/nope"), nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("an unknown model was upgraded")
	}
	if resp2 == nil || resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model response = %v, want 404 (err %v)", resp2, err)
	}
	if got := decodeRouterError(t, resp2.Body); got.Code != codeModelNotManaged {
		t.Errorf("unknown model code = %q, want %q", got.Code, codeModelNotManaged)
	}
}

// TestRouterRealtimeRefusedAfterClose: once the router is closed it takes no new
// session -- no admission, no dial -- so none can outlive it unclosed.
func TestRouterRealtimeRefusedAfterClose(t *testing.T) {
	child := newRealtimeChild(t)
	rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, nil)
	rig.rt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), realtimeWait)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsURL(rig.srv, "/realtime/m"), nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("a closed router upgraded a new session")
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("response = %v, want 502 (err %v)", resp, err)
	}
	select {
	case model := <-rig.m.ensured:
		t.Errorf("a closed router admitted %q", model)
	default:
	}
}

// TestRouterRealtimeCloseIsBounded: Close does not wait for a session whose
// client never answers the close handshake -- it returns after
// realtimeCloseWait, long before coder/websocket's own 5 s handshake wait --
// and the session still ends and releases once the client reads.
func TestRouterRealtimeCloseIsBounded(t *testing.T) {
	child := newRealtimeChild(t)
	rig := newRealtimeRig(t, map[string]string{"m": child.srv.URL}, func(rt *router) { rt.realtimeCloseWait = 20 * time.Millisecond })
	c := dialRealtime(t, rig.srv, "/realtime/m")
	readGreeting(t, c)

	closed := make(chan struct{})
	go func() {
		rig.rt.Close() // the client is not reading, so the handshake hangs
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(4 * time.Second):
		t.Fatal("Close waited for a close handshake the client never answers")
	}
	if code, _ := readClose(t, c); code != websocket.StatusGoingAway {
		t.Fatalf("client close = %d, want 1001", code)
	}
	rig.assertReleasedOnce(t)
}

// TestRealtimeSendableCloseCode: a status that may be sent on is relayed as it
// is, a missing status stays missing (an empty close frame), and anything else
// becomes 1011.
func TestRealtimeSendableCloseCode(t *testing.T) {
	relayed := []websocket.StatusCode{1000, 1001, 1003, 1007, 1009, 1011, 1014, 3000, 4999, websocket.StatusNoStatusRcvd}
	for _, code := range relayed {
		if got := sendableCloseCode(code); got != code {
			t.Errorf("sendableCloseCode(%d) = %d, want it relayed as is", code, got)
		}
	}
	replaced := []websocket.StatusCode{-1, 999, 1004, websocket.StatusAbnormalClosure, 1015, 2999, 5000}
	for _, code := range replaced {
		if got := sendableCloseCode(code); got != websocket.StatusInternalError {
			t.Errorf("sendableCloseCode(%d) = %d, want 1011", code, got)
		}
	}
}

// realtimeDriverManager is a driverManager whose router admissions are a
// realtimeManager's, so a Driver-built router can open realtime sessions.
type realtimeDriverManager struct {
	*fakeManager
	rm *realtimeManager
}

func (d realtimeDriverManager) EnsureServing(ctx context.Context, model string) (string, func(), error) {
	return d.rm.EnsureServing(ctx, model)
}

func (d realtimeDriverManager) ResolveAddress(model string) (string, int, bool) {
	return d.rm.ResolveAddress(model)
}

// TestDriverRetiringTheRouterEndsRealtimeSessions: a hijacked realtime session
// is invisible to http.Server.Close, so every way the Driver retires a router --
// Close at shutdown, an unbind (StartRouter(0)), a rebind to another port --
// must close the retired router's sessions with 1001 and so release their
// admissions.
func TestDriverRetiringTheRouterEndsRealtimeSessions(t *testing.T) {
	cases := []struct {
		name   string
		retire func(t *testing.T, d *Driver)
	}{
		{"close", func(_ *testing.T, d *Driver) { d.Close() }},
		{"unbind", func(t *testing.T, d *Driver) {
			if err := d.StartRouter(0); err != nil {
				t.Errorf("StartRouter(0): %v", err)
			}
		}},
		{"rebind", func(t *testing.T, d *Driver) {
			if err := d.StartRouter(grabFreePort(t)); err != nil {
				t.Errorf("StartRouter(another port): %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child := newRealtimeChild(t)
			rm := newRealtimeManager(map[string]string{"m": child.srv.URL})
			d := newDriver(realtimeDriverManager{fakeManager: &fakeManager{}, rm: rm}, &fakeSource{}, nil, nil, "127.0.0.1")
			port := grabFreePort(t)
			if err := d.StartRouter(port); err != nil {
				t.Fatalf("StartRouter(%d): %v", port, err)
			}
			t.Cleanup(d.Close)

			ctx, cancel := context.WithTimeout(context.Background(), realtimeWait)
			defer cancel()
			c, _, err := websocket.Dial(ctx, fmt.Sprintf("ws://127.0.0.1:%d/realtime/m", port), nil)
			if err != nil {
				t.Fatalf("dial the bound router: %v", err)
			}
			t.Cleanup(func() { _ = c.CloseNow() })
			readGreeting(t, c)

			retired := make(chan struct{})
			go func() {
				tc.retire(t, d)
				close(retired)
			}()
			if code, reason := readClose(t, c); code != websocket.StatusGoingAway || reason != realtimeReasonClosing {
				t.Fatalf("client close = %d %q, want 1001 %q", code, reason, realtimeReasonClosing)
			}
			recv(t, retired, "the router to be retired")
			recv(t, rm.released, "the session's release")
			if held, releases := rm.held.Load(), rm.releases.Load(); held != 0 || releases != 1 {
				t.Fatalf("after retiring the router: held = %d, releases = %d; want nothing held and one release", held, releases)
			}
		})
	}
}
