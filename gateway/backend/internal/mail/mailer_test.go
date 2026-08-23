// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedMessage struct {
	from     string
	rcpt     []string
	data     string
	authUser string
	authPass string
	usedTLS  bool
	startTLS bool
}

type testSMTPServer struct {
	tlsConfig         *tls.Config
	implicitTLS       bool // ssl: wrap the accepted conn immediately
	advertiseSTARTTLS bool
	advertiseAUTH     bool

	mu   sync.Mutex
	msgs []capturedMessage
}

// start listens on a random loopback port and serves until the test ends.
func (s *testSMTPServer) start(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handleConn(conn)
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	pn, _ := strconv.Atoi(p)
	return h, pn
}

func (s *testSMTPServer) record(m capturedMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
}

// last returns the most recently recorded message. It locks the same mutex
// record() uses, so it's safe to call from the test goroutine while the
// server goroutine may still be appending (as opposed to indexing s.msgs
// directly, which races under `go test -race`).
func (s *testSMTPServer) last(t *testing.T) capturedMessage {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.msgs) == 0 {
		t.Fatalf("no message captured")
	}
	return s.msgs[len(s.msgs)-1]
}

func (s *testSMTPServer) handleConn(conn net.Conn) {
	defer conn.Close()
	msg := &capturedMessage{}
	if s.implicitTLS {
		tc := tls.Server(conn, s.tlsConfig)
		if err := tc.Handshake(); err != nil {
			return
		}
		conn = tc
		msg.usedTLS = true
	}
	r := bufio.NewReader(conn)
	write := func(line string) { fmt.Fprintf(conn, "%s\r\n", line) }
	write("220 test ESMTP")
	for {
		raw, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line := strings.TrimRight(raw, "\r\n")
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			lines := []string{"test greets you"}
			if s.advertiseSTARTTLS && !msg.usedTLS {
				lines = append(lines, "STARTTLS")
			}
			if s.advertiseAUTH {
				lines = append(lines, "AUTH PLAIN")
			}
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				write(fmt.Sprintf("250%s%s", sep, l))
			}
		case strings.HasPrefix(cmd, "STARTTLS"):
			write("220 go ahead")
			tc := tls.Server(conn, s.tlsConfig)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn = tc
			r = bufio.NewReader(conn)
			msg.usedTLS = true
			msg.startTLS = true
		case strings.HasPrefix(cmd, "AUTH PLAIN"):
			payload := ""
			if f := strings.Fields(line); len(f) >= 3 {
				payload = f[2]
			} else {
				write("334 ")
				resp, _ := r.ReadString('\n')
				payload = strings.TrimRight(resp, "\r\n")
			}
			if dec, err := base64.StdEncoding.DecodeString(payload); err == nil {
				if parts := strings.Split(string(dec), "\x00"); len(parts) == 3 {
					msg.authUser, msg.authPass = parts[1], parts[2]
				}
			}
			write("235 authenticated")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			msg.from = strings.TrimPrefix(line[len("MAIL FROM"):], ":")
			write("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			msg.rcpt = append(msg.rcpt, strings.TrimPrefix(line[len("RCPT TO"):], ":"))
			write("250 ok")
		case strings.HasPrefix(cmd, "DATA"):
			write("354 end data with <CR><LF>.<CR><LF>")
			var b strings.Builder
			for {
				dl, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if dl == ".\r\n" || dl == ".\n" {
					break
				}
				b.WriteString(dl)
			}
			msg.data = b.String()
			// Record now, before acknowledging DATA, so the message is
			// visible to the test goroutine no later than when Send's
			// blocking Data()/Close() call returns (well before the client
			// even issues QUIT). Recording only on QUIT let c.Quit() (and
			// thus Send) return before record() ran, racing the test's
			// read of srv.msgs and occasionally leaving it empty or stale.
			s.record(*msg)
			write("250 queued")
		case strings.HasPrefix(cmd, "QUIT"):
			write("221 bye")
			return
		default:
			write("250 ok")
		}
	}
}

// testTLSConfig builds a self-signed cert for the in-process server.
func testTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}}}
}

func mustSend(t *testing.T, m *Mailer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Send(ctx, "bob@example.com", "Betreff", "hallo\nwelt"); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestSendStartTLSWithAuth(t *testing.T) {
	srv := &testSMTPServer{tlsConfig: testTLSConfig(t), advertiseSTARTTLS: true, advertiseAUTH: true}
	host, port := srv.start(t)
	m := New(Config{Host: host, Port: port, Username: "u", Password: "p", From: "gw@example.com", TLSMode: TLSModeStartTLS})
	m.tlsConfig = &tls.Config{InsecureSkipVerify: true}
	mustSend(t, m)

	got := srv.last(t)
	if !got.startTLS || !got.usedTLS {
		t.Fatalf("expected STARTTLS upgrade, got %+v", got)
	}
	if got.authUser != "u" || got.authPass != "p" {
		t.Fatalf("auth = %q/%q, want u/p", got.authUser, got.authPass)
	}
	if !strings.Contains(got.from, "gw@example.com") || !strings.Contains(got.data, "Subject: Betreff") {
		t.Fatalf("bad envelope/data: %+v", got)
	}
}

func TestSendSSLImplicitTLS(t *testing.T) {
	srv := &testSMTPServer{tlsConfig: testTLSConfig(t), implicitTLS: true, advertiseAUTH: true}
	host, port := srv.start(t)
	m := New(Config{Host: host, Port: port, Username: "u", Password: "p", From: "gw@example.com", TLSMode: TLSModeSSL})
	m.tlsConfig = &tls.Config{InsecureSkipVerify: true}
	mustSend(t, m)

	got := srv.last(t)
	if !got.usedTLS || got.startTLS {
		t.Fatalf("expected implicit TLS (no STARTTLS), got %+v", got)
	}
	if got.authUser != "u" {
		t.Fatalf("auth not sent over ssl: %+v", got)
	}
}

func TestSendNoneNoAuth(t *testing.T) {
	srv := &testSMTPServer{advertiseAUTH: false} // no username set, no AUTH advertised
	host, port := srv.start(t)
	m := New(Config{Host: host, Port: port, From: "gw@example.com", TLSMode: TLSModeNone})
	mustSend(t, m)

	got := srv.last(t)
	if got.usedTLS {
		t.Fatalf("none mode must stay plaintext, got %+v", got)
	}
	if got.authUser != "" {
		t.Fatalf("no auth expected, got user %q", got.authUser)
	}
}

func TestSendNoneWithUnencryptedAuth(t *testing.T) {
	// PlainAuth refuses plaintext AUTH unless net/smtp's isLocalhost helper
	// recognizes the SMTP server name — and it only matches the exact strings
	// "localhost"/"127.0.0.1"/"::1". Binding (and pointing Config.Host) at
	// plain "127.0.0.1" — as an earlier version of this test did — would pass
	// even with the unencryptedAuth wrapper deleted from mailer.go, because
	// isLocalhost's own bypass already satisfies PlainAuth without it. To
	// actually exercise the wrapper, the server must present a host name
	// isLocalhost does NOT special-case. "<label>.localhost" fits: RFC 6761
	// requires resolvers to answer it with the loopback address without any
	// network query, so it dials straight through to our listener, yet the
	// literal string differs from isLocalhost's exact-match list — so
	// PlainAuth would abort here without the wrapper forcing server.TLS=true.
	const nonLocalhostAlias = "smtp-test-alias.localhost"
	srv := &testSMTPServer{advertiseAUTH: true}
	_, port := srv.start(t) // binds 127.0.0.1; the alias above resolves there
	m := New(Config{Host: nonLocalhostAlias, Port: port, Username: "relay", Password: "s3cr3t", From: "gw@example.com", TLSMode: TLSModeNone})
	mustSend(t, m)

	got := srv.last(t)
	if got.usedTLS {
		t.Fatalf("none mode must stay plaintext, got %+v", got)
	}
	if got.authUser != "relay" || got.authPass != "s3cr3t" {
		t.Fatalf("unencrypted auth = %q/%q, want relay/s3cr3t", got.authUser, got.authPass)
	}
}

func TestSendRejectsBadTLSMode(t *testing.T) {
	m := New(Config{Host: "127.0.0.1", Port: 25, From: "gw@example.com", TLSMode: "bogus"})
	if err := m.Send(context.Background(), "b@x", "s", "b"); err != ErrInvalidTLSMode {
		t.Fatalf("err = %v, want ErrInvalidTLSMode", err)
	}
}

func TestSendRejectsEmptyHost(t *testing.T) {
	m := New(Config{Port: 25, From: "gw@example.com", TLSMode: TLSModeStartTLS})
	if err := m.Send(context.Background(), "b@x", "s", "b"); err != ErrHostRequired {
		t.Fatalf("err = %v, want ErrHostRequired", err)
	}
}
