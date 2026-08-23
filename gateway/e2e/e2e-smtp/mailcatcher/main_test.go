// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

func TestCatcherCapturesMessage(t *testing.T) {
	st := &store{}
	smtpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer smtpLn.Close()
	go acceptLoop(smtpLn, st)

	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer httpLn.Close()
	go http.Serve(httpLn, newMux(st))

	body := "To: bob@example.test\r\nSubject: Hi\r\n\r\nvisit /set-password?token=abc\r\n"
	if err := smtp.SendMail(smtpLn.Addr().String(), nil, "gw@example.test", []string{"bob@example.test"}, []byte(body)); err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		res, err := http.Get("http://" + httpLn.Addr().String() + "/messages")
		if err != nil {
			t.Fatal(err)
		}
		var got []message
		_ = json.NewDecoder(res.Body).Decode(&got)
		res.Body.Close()
		if len(got) == 1 {
			if got[0].From != "gw@example.test" || len(got[0].To) != 1 || got[0].To[0] != "bob@example.test" {
				t.Fatalf("envelope = %+v", got[0])
			}
			if !strings.Contains(got[0].Data, "/set-password?token=abc") {
				t.Fatalf("data missing link: %q", got[0].Data)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no message caught, got %d", len(got))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
