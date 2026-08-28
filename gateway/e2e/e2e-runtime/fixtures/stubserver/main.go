// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Command stubserver is a minimal, stdlib-only stand-in for a real model
// server (llama.cpp's `llama-server`, vLLM, ...) for the e2e:runtime
// Playwright suite. It is the binary the REAL server-agent's runtime manager
// starts, health-probes, proxies to, and drain-stops -- so this program is
// never started by the test harness itself. The harness only BUILDS it and
// puts its path on the agent's OP_AGENT_RUNTIME_ALLOWED_BINARIES allowlist;
// every process of it that ever runs was exec'd by the agent, which is
// precisely what the suite exists to prove. Not for production.
//
// Three behaviours exist specifically so the suite can assert things no
// in-process unit test can reach:
//
//   - -tag: echoed back inside the completion text, so an assertion can prove
//     WHICH child process answered a given inference -- not merely that some
//     OpenAI-shaped JSON came back. Two specs in the suite share this one
//     binary, and the eviction/admission steps hinge on telling their
//     processes apart.
//   - -ready-after: /health stays 503 for this long after start, so the
//     agent's `starting` (loading) state genuinely lasts long enough to be
//     observed on the portal's runtime SSE stream instead of being raced
//     past. Zero (the default) means ready immediately.
//   - a SIGTERM handler that shuts the listener down and exits promptly, so
//     the eviction and force-stop steps measure the admission/eviction
//     machinery rather than the manager's SIGKILL grace period.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// chatRequest is the subset of the OpenAI chat-completions request this stub
// reads. The agent's router forwards the gateway's body verbatim, so `model`
// here is the mapping's app_model_name (the upstream name), not the gateway
// model name the client asked for.
type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// chatResponse is a minimal OpenAI-shaped non-streaming completion. Only
// choices[0].message.content and usage are read by the gateway's
// OpenAI-compatible provider (internal/provider/openai_compatible.go), but
// the surrounding envelope is kept realistic so the stub stays a faithful
// stand-in.
type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
}

// chatChoice / chatMessage / chatUsage are the response sub-objects. Named
// (rather than inline anonymous structs) so completion below can build them
// as literals -- an anonymous struct type has to be restated verbatim at
// every use site, which is exactly how a JSON tag drifts.
type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// stub holds the one process's identity and its readiness gate.
type stub struct {
	tag string
	// readyAt is the instant /health starts answering 200. Compared against
	// the wall clock on every probe rather than flipped by a timer, so the
	// gate needs no goroutine and no synchronisation beyond the atomic
	// counter below.
	readyAt time.Time
	// completions counts served completions, exposed on /stats purely so a
	// test can assert this process (as opposed to a sibling with a
	// different -tag) is the one that answered.
	completions atomic.Int64
}

func (s *stub) ready() bool { return !time.Now().Before(s.readyAt) }

// writeJSON is the single response writer, so no handler can forget the
// content type.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// lastUserContent returns the content of the last user message, or the last
// message of any role when there is no user message -- the "prompt" this stub
// echoes.
func lastUserContent(req chatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content
		}
	}
	if len(req.Messages) > 0 {
		return req.Messages[len(req.Messages)-1].Content
	}
	return ""
}

// completion builds the echo response. The content deliberately leads with
// the process tag: the suite asserts on the exact prefix, which is what turns
// "some upstream answered" into "THIS child process answered".
func (s *stub) completion(req chatRequest) chatResponse {
	prompt := lastUserContent(req)
	// Non-zero, prompt-proportional token counts: the gateway records usage
	// from these, and a zero prompt_tokens would make the usage row
	// indistinguishable from "the provider reported nothing".
	promptTokens := len(strings.Fields(prompt))
	if promptTokens == 0 {
		promptTokens = 1
	}
	completionTokens := promptTokens + 2
	return chatResponse{
		ID:      "chatcmpl-stub-" + s.tag,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      chatMessage{Role: "assistant", Content: fmt.Sprintf("[%s] echo: %s", s.tag, prompt)},
			FinishReason: "stop",
		}},
		Usage: chatUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
}

func newMux(s *stub) *http.ServeMux {
	mux := http.NewServeMux()

	// The readiness endpoint the agent's runtime manager polls (a spec's
	// health_path; "/health" is the manager's own default). 503 until
	// readyAt, so the spec's `starting` state is observable.
	health := func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "loading", "tag": s.tag})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "tag": s.tag})
	}
	mux.HandleFunc("/health", health)
	mux.HandleFunc("/v1/health", health)

	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"tag":         s.tag,
			"ready":       s.ready(),
			"completions": s.completions.Load(),
		})
	})

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		// A real model server cannot answer before its weights are
		// loaded; refusing here keeps the stub honest about the readiness
		// gate rather than making /health a decoration.
		if !s.ready() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "model still loading"})
			return
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
			return
		}
		s.completions.Add(1)
		writeJSON(w, http.StatusOK, s.completion(req))
	})

	return mux
}

func main() {
	port := flag.Int("port", 0, "TCP port to listen on (loopback only)")
	tag := flag.String("tag", "stub", "identity echoed in completions so a test can tell two processes apart")
	readyAfter := flag.Duration("ready-after", 0, "delay before /health answers 200, simulating a model load")
	flag.Parse()

	if *port <= 0 || *port > 65535 {
		log.Fatalf("stubserver: -port must be 1..65535, got %d", *port)
	}

	s := &stub{tag: *tag, readyAt: time.Now().Add(*readyAfter)}

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", *port)))
	if err != nil {
		log.Fatalf("stubserver: listen on 127.0.0.1:%d: %v", *port, err)
	}

	srv := &http.Server{Handler: newMux(s), ReadHeaderTimeout: 10 * time.Second}

	// Prompt SIGTERM handling: the agent drain-stops an evicted or
	// force-stopped process with SIGTERM and only escalates to SIGKILL
	// after a grace period, so without this the suite's eviction timings
	// would be measuring that grace period instead of the admission
	// machinery.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("stubserver: tag=%s listening on %s ready_after=%s", s.tag, ln.Addr(), readyAfter.String())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("stubserver: serve: %v", err)
	}
}
