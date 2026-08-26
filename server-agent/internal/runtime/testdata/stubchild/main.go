// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Command stubchild is a tiny, test-only model-server stand-in for
// internal/runtime's manager tests. It serves a minimal health endpoint and
// echoes /v1/echo request bodies, with a configurable startup delay and a
// scripted crash -- just enough real exec/health-poll/exit-code surface to
// drive the process manager's tests without a real model runtime.
//
// Never part of the module's production build: "testdata" directories are
// excluded from `go build ./...`, `go vet ./...`, and internal/archtest's
// package graph by the go tool itself.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	port := flag.Int("port", 0, "TCP port to listen on")
	healthDelay := flag.Duration("health-delay", 0, "delay after which /health starts answering 200")
	crashAfter := flag.Duration("crash-after", 0, "if > 0, exit with -exit-code this long after start")
	exitCode := flag.Int("exit-code", 1, "process exit code used by -crash-after")
	invocationLog := flag.String("invocation-log", "", "if set, append one line (this PID) per invocation -- proves how many times the binary was actually exec'd, independent of the manager's own bookkeeping")
	flag.Parse()

	start := time.Now()

	if *invocationLog != "" {
		f, err := os.OpenFile(*invocationLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("stubchild: open invocation log: %v", err)
		}
		fmt.Fprintf(f, "%d\n", os.Getpid())
		f.Close()
	}

	if *crashAfter > 0 {
		go func() {
			time.Sleep(*crashAfter)
			fmt.Fprintln(os.Stderr, "stubchild: scripted crash")
			os.Exit(*exitCode)
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(start) < *healthDelay {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})

	addr := "127.0.0.1:" + strconv.Itoa(*port)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("stubchild: listen %s: %v", addr, err)
	}
}
