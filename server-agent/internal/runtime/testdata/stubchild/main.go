// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Command stubchild is a tiny, test-only model-server stand-in for
// internal/runtime's manager and router tests. It serves a minimal health
// endpoint, echoes /v1/echo request bodies, writes two flushed chunks with a
// configurable gap at /v1/chunked (proving/exercising no-buffering and
// verbatim-splice behavior in the router), returns a fixed non-2xx status at
// /v1/fail, and supports a configurable startup delay and a scripted crash
// -- just enough real exec/health-poll/exit-code/streaming surface to drive
// the process manager's and router's tests without a real model runtime.
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
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	port := flag.Int("port", 0, "TCP port to listen on")
	healthDelay := flag.Duration("health-delay", 0, "delay after which /health starts answering 200")
	crashAfter := flag.Duration("crash-after", 0, "if > 0, exit with -exit-code this long after start")
	exitCode := flag.Int("exit-code", 1, "process exit code used by -crash-after")
	invocationLog := flag.String("invocation-log", "", "if set, append one line (this PID) per invocation -- proves how many times the binary was actually exec'd, independent of the manager's own bookkeeping")
	envLog := flag.String("env-log", "", "if set, write what this process ACTUALLY received for -env-name, as \"set:<value>\" or \"unset\" -- the only way to observe a child's own environment from the parent's test, and it deliberately distinguishes an absent variable from one set to the empty string")
	envName := flag.String("env-name", "", "the environment variable -env-log reports")
	ignoreSigterm := flag.Bool("ignore-sigterm", false, "ignore SIGTERM, so the manager's kill-grace escalation to SIGKILL is what actually ends this process -- gives a test a real, controllable window in which a signalled-but-still-live child keeps answering /health")
	flag.Parse()

	// -ignore-sigterm exists so a test can observe manager state while a
	// generation has been signalled but is NOT gone yet. A cooperative child
	// dies on SIGTERM within microseconds, which closes that window before
	// any assertion can look into it; a stubborn one keeps serving until
	// killGrace elapses and SIGKILL (which cannot be ignored) arrives. Real
	// model servers do behave this way -- a graceful shutdown that finishes
	// in-flight generations can easily outlast a SIGTERM by seconds.
	if *ignoreSigterm {
		signal.Ignore(syscall.SIGTERM)
	}

	start := time.Now()

	if *invocationLog != "" {
		f, err := os.OpenFile(*invocationLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("stubchild: open invocation log: %v", err)
		}
		fmt.Fprintf(f, "%d\n", os.Getpid())
		f.Close()
	}

	// "set:<value>" vs "unset", never a bare value: an EMPTY visibility
	// variable means "no device is visible" while an ABSENT one means "no
	// restriction", so a log format that rendered both as an empty line would
	// be blind to exactly the distinction the tests exist to check.
	if *envLog != "" {
		record := "unset"
		if value, ok := os.LookupEnv(*envName); ok {
			record = "set:" + value
		}
		if err := os.WriteFile(*envLog, []byte(record), 0o644); err != nil {
			log.Fatalf("stubchild: write env log: %v", err)
		}
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
	// /v1/chunked writes two flushed chunks (query params c1/c2, default
	// "chunk1"/"chunk2") with a real-time gap (query param "gap", a
	// time.Duration string, default 0) between them -- the router tests'
	// no-buffering and verbatim-splice proofs need a real, observable pause
	// between two flushed writes, which /v1/echo's single Write cannot
	// provide. An optional second gap (query param "gap2") sleeps AGAIN
	// after c2 is written and flushed, before this handler returns (which
	// is what actually closes the connection) -- needed to discriminate a
	// missing flush on the CALLER's side of a proxy from an merely-delayed
	// one: net/http flushes any still-buffered bytes automatically once a
	// handler returns, so a test whose connection closes immediately after
	// the second chunk cannot tell "flushed immediately" from "flushed only
	// because the response just happened to end" -- gap2 keeps the
	// connection open long enough that the two cases differ by seconds, not
	// nothing.
	mux.HandleFunc("/v1/chunked", func(w http.ResponseWriter, r *http.Request) {
		c1 := r.URL.Query().Get("c1")
		if c1 == "" {
			c1 = "chunk1"
		}
		c2 := r.URL.Query().Get("c2")
		if c2 == "" {
			c2 = "chunk2"
		}
		gap, _ := time.ParseDuration(r.URL.Query().Get("gap"))
		gap2, _ := time.ParseDuration(r.URL.Query().Get("gap2"))
		flusher, _ := w.(http.Flusher)

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, c1)
		if flusher != nil {
			flusher.Flush()
		}
		if gap > 0 {
			time.Sleep(gap)
		}
		fmt.Fprint(w, c2)
		if flusher != nil {
			flusher.Flush()
		}
		if gap2 > 0 {
			time.Sleep(gap2)
		}
	})
	// /v1/fail returns a fixed non-2xx status (query param "status", default
	// 500) immediately -- used to prove the router's post-heartbeat-commit
	// handling of a non-2xx upstream response.
	mux.HandleFunc("/v1/fail", func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusInternalServerError
		if s := r.URL.Query().Get("status"); s != "" {
			if v, err := strconv.Atoi(s); err == nil {
				status = v
			}
		}
		w.WriteHeader(status)
		fmt.Fprint(w, "stubchild: scripted failure")
	})

	addr := "127.0.0.1:" + strconv.Itoa(*port)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("stubchild: listen %s: %v", addr, err)
	}
}
