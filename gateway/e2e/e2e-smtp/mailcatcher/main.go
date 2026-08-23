// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Command mailcatcher is a minimal, stdlib-only SMTP sink for the e2e:smtp
// Playwright suite. It accepts plain SMTP (TLS mode "none", no auth) and exposes
// the captured messages over HTTP so the test can assert what the gateway sent.
// Not for production.
package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

type message struct {
	From string   `json:"from"`
	To   []string `json:"to"`
	Data string   `json:"data"`
}

type store struct {
	mu       sync.Mutex
	messages []message
}

func (s *store) add(m message) {
	s.mu.Lock()
	s.messages = append(s.messages, m)
	s.mu.Unlock()
}

func (s *store) list() []message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]message, len(s.messages))
	copy(out, s.messages)
	return out
}

func (s *store) reset() {
	s.mu.Lock()
	s.messages = nil
	s.mu.Unlock()
}

func newMux(st *store) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			st.reset()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st.list())
	})
	return mux
}

func acceptLoop(ln net.Listener, st *store) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleSMTP(conn, st)
	}
}

func main() {
	smtpAddr := envOr("MAILCATCHER_SMTP_ADDR", "127.0.0.1:2525")
	httpAddr := envOr("MAILCATCHER_HTTP_ADDR", "127.0.0.1:8025")
	st := &store{}

	httpLn, err := net.Listen("tcp", httpAddr)
	if err != nil {
		log.Fatalf("mailcatcher: http listen %s: %v", httpAddr, err)
	}
	go func() { log.Fatal(http.Serve(httpLn, newMux(st))) }()

	smtpLn, err := net.Listen("tcp", smtpAddr)
	if err != nil {
		log.Fatalf("mailcatcher: smtp listen %s: %v", smtpAddr, err)
	}
	log.Printf("mailcatcher: smtp on %s, http on %s", smtpAddr, httpAddr)
	acceptLoop(smtpLn, st)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// handleSMTP implements just enough of RFC 5321 for net/smtp's client on the
// "none" (plain, no-auth) path: EHLO/HELO, MAIL, RCPT, DATA, RSET, NOOP, QUIT.
// It advertises neither STARTTLS nor AUTH, so the client sends in the clear.
func handleSMTP(conn net.Conn, st *store) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(s string) {
		_, _ = w.WriteString(s + "\r\n")
		_ = w.Flush()
	}
	write("220 mailcatcher ready")

	var from string
	var to []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(verb, "EHLO"):
			write("250-mailcatcher")
			write("250 OK")
		case strings.HasPrefix(verb, "HELO"):
			write("250 mailcatcher")
		case strings.HasPrefix(verb, "MAIL FROM"):
			from = addr(line)
			to = nil
			write("250 OK")
		case strings.HasPrefix(verb, "RCPT TO"):
			to = append(to, addr(line))
			write("250 OK")
		case verb == "DATA":
			write("354 End data with <CR><LF>.<CR><LF>")
			var b strings.Builder
			for {
				dl, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if dl == ".\r\n" || dl == ".\n" {
					break
				}
				if strings.HasPrefix(dl, "..") { // undo dot-stuffing
					dl = dl[1:]
				}
				b.WriteString(dl)
			}
			st.add(message{From: from, To: to, Data: b.String()})
			write("250 OK: queued")
		case verb == "RSET":
			from, to = "", nil
			write("250 OK")
		case verb == "NOOP":
			write("250 OK")
		case verb == "QUIT":
			write("221 Bye")
			return
		default:
			write("250 OK")
		}
	}
}

// addr extracts the address inside the angle brackets of a MAIL/RCPT command.
func addr(line string) string {
	if i := strings.Index(line, "<"); i >= 0 {
		if j := strings.Index(line[i:], ">"); j >= 0 {
			return line[i+1 : i+j]
		}
	}
	if i := strings.Index(line, ":"); i >= 0 {
		return strings.TrimSpace(line[i+1:])
	}
	return ""
}
