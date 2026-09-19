// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"regexp"
	"strings"
)

// This file is the image kind's half of the run executor. It shares the
// reservation, the deadline, the header set and the single terminal commit
// with the text half (chat_runs.go) and NOTHING else -- deliberately.
//
// consumeRunStream cannot be reused here even in part: it opens a
// bufio.Scanner over the response body, drives the periodic checkpoint
// goroutine and computes TTFT/chars-per-second/tokens-per-second, and every
// one of those assumes a stream of deltas. /v1/images/generations answers ONE
// buffered JSON body -- no deltas, so nothing to checkpoint (a checkpoint
// would write a `pending` assistant turn with empty content that the terminal
// commit then has to replace) and no rate that could be measured rather than
// invented.

// imagesResponse is the upstream body this relay consumes. The shape is taken
// from the repository's own fixtures (images_handler_test.go):
// {"created":1,"output_format":"png","data":[{"b64_json":"..."}]}.
//
// OutputFormat is what makes the data: URL's media type a REPORTED value
// rather than an assumption. Hardcoding image/png would be a fabricated
// measurement of the same class as a progress bar over an endpoint that
// reports no progress.
//
// Data is plural by design: imagesDataCounter's own doc comment
// (images_handler.go) says the billed quantity comes from the RESPONSE
// because "a partial failure makes those differ". One content part per item;
// never data[0].
type imagesResponse struct {
	OutputFormat string `json:"output_format"`
	Data         []struct {
		B64JSON       string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
}

// imageTextPart and imageURLPart are the two content parts an image turn
// emits. Declared types rather than map literals so the emitted key ORDER is
// the one the shape is documented and rendered in (`type` first), and so the
// shape is stated once here instead of at each append site.
type imageTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imageURLTarget struct {
	URL string `json:"url"`
}

type imageURLPart struct {
	Type     string         `json:"type"`
	ImageURL imageURLTarget `json:"image_url"`
}

// The two terminal codes this path adds. Both are CODES, not prose, for the
// reason runTimedOutMessage already is one: the frontend maps a run's error to
// a localized label, and both of these describe a condition a user has to be
// able to tell apart from an ordinary failure.
const (
	// imageRunNoImageMessage: a 2xx that produced no usable image. This is an
	// ERROR, not an empty success -- the endpoint's own counter already logs a
	// counted zero on a 2xx at Error level, and committing a turn with no
	// image would render as a blank bubble the user cannot distinguish from a
	// bug.
	imageRunNoImageMessage = "gateway.chat_run_no_image"
	// imageRunFormatUnknownMessage: a 2xx whose output_format is missing or is
	// not a media subtype this can safely name. The media type is the one
	// thing about the image that MUST come from the response; defaulting it
	// would state an unmeasured fact, and it would stay stated -- in the
	// transcript, in the download's file name, and in every later request
	// buildAPIHistory carries the part into.
	imageRunFormatUnknownMessage = "gateway.chat_run_image_format_unknown"
)

// imageSubtypePattern bounds what may be interpolated into the data: URL's
// media type. output_format is upstream-controlled text, and it lands in a URL
// the browser loads and the download names a file from, so only a bare media
// SUBTYPE is accepted (png, jpeg, webp -- the values sd-server and OpenAI's
// own images API report). Anything else, including a full "image/png", is
// refused rather than repaired: the alternative is letting an upstream choose
// the whole media type (a forged "text/html;base64,..." would be saved and
// opened as markup), and a refusal here is loud where a repair would be a
// guess.
var imageSubtypePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+.-]*$`)

// executeImageRun is executeRun's image branch: one buffered request to the
// gateway's own /v1/images/generations, and one terminal commit carrying the
// generated image(s) as the assistant turn's structured content.
//
// It calls finishRunWithParts EXACTLY ONCE, on every path, because the work
// is split into a function that only COMPUTES the outcome (generateImageTurn,
// below) and this one, which only commits it. Nothing in the computation can
// return without producing an outcome, so "one terminal commit per run" is a
// property of the shape rather than of remembering to call finishRun on each
// of its error branches.
//
// The deadline is NOT applied here: executeRun already derived the bounded
// context from the run's kind before branching (see runDeadlineFor), and a
// second bound would be a second, divergent promise about the same wait.
func (s *Server) executeImageRun(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult) {
	parts, status, errMsg := s.generateImageTurn(ctx, owner, run, prep)
	s.finishRunWithParts(ctx, owner, run, status, errMsg, parts)
}

// generateImageTurn performs the single loopback request an image run makes
// and returns the turn's outcome: the content parts to commit, the run status,
// and the terminal error message. It never commits, never finishes the run and
// never publishes a delta -- there are none to publish.
func (s *Server) generateImageTurn(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult) (json.RawMessage, string, string) {
	body, err := buildImagesBody(prep)
	if err != nil {
		return nil, "error", err.Error()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.selfBaseURL+"/v1/images/generations", bytes.NewReader(body))
	if err != nil {
		return nil, "error", err.Error()
	}
	s.setRunLoopbackHeaders(req, owner, run, prep)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if status, msg, ended := runContextOutcome(ctx); ended {
			return nil, status, msg
		}
		return nil, "error", err.Error()
	}
	defer resp.Body.Close()

	// Read the WHOLE body: this endpoint answers one buffered JSON payload and
	// the images cannot be decoded from a prefix of it. Deliberately not
	// bounded by s.captureMaxBytes the way relayImagesUpstreamError bounds an
	// ERROR body -- that cap (1 MiB) is smaller than an ordinary successful
	// image response, so applying it here would truncate exactly the bodies
	// this path exists for. The backstop on an absurd response is the chat
	// store's own 4 MiB content cap, which refuses the commit.
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		// The other half of the same conflation consumeRunStream handles at
		// its scanner error: once the status line has arrived, a deadline or
		// a cancel lands here as a read failure, and reporting either as a
		// generic error -- or a deadline as a cancel -- would tell the user
		// they pressed Stop when they pressed nothing.
		if status, msg, ended := runContextOutcome(ctx); ended {
			return nil, status, msg
		}
		return nil, "error", err.Error()
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "error", upstreamErrorCode(payload, resp.Status)
	}

	var decoded imagesResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, "error", err.Error()
	}
	parts, err := imagePartsFrom(decoded)
	if err != nil {
		return nil, "error", err.Error()
	}
	return parts, "completed", ""
}

// runContextOutcome maps an ended run context to its terminal (status,
// message), reporting false when the context is still live and the caller's
// own error is the real one. A DEADLINE IS NOT A CANCEL: a cancel carries the
// empty message the UI reads as "the user pressed Stop", and a run its own
// deadline ended must not claim that.
func runContextOutcome(ctx context.Context) (status, errMsg string, ended bool) {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "error", runTimedOutMessage, true
	case ctx.Err() != nil:
		return "canceled", "", true
	}
	return "", "", false
}

// upstreamErrorCode pulls error.code out of the gateway's own error envelope,
// falling back to the status line when the body is not that shape.
//
// It exists because a non-2xx that is turned into "upstream status 400 Bad
// Request" DISCARDS every code the images endpoint was built to return --
// images.prompt_required, images.stream_unsupported,
// images.response_format_unsupported, images.upstream_error -- and the run is
// then the one place in the system that cannot say what went wrong. The
// envelope is apierror.Body rather than a local anonymous struct so this
// stays tied to the shape writeJSON actually emits.
//
// The text path's identical loss (executeRun's non-200 branch reads no body at
// all) is a separate fix with its own frontend half; this function is written
// to be reusable by it verbatim.
func upstreamErrorCode(body []byte, status string) string {
	var envelope apierror.Body
	if json.Unmarshal(body, &envelope) == nil {
		if code := strings.TrimSpace(envelope.Error.Code); code != "" {
			return code
		}
	}
	return "upstream status " + status
}

// buildImagesBody is the images endpoint's own request shape.
//
// Three fields, and no more. response_format is stated EXPLICITLY although
// validateImagesRequest also accepts its absence: it is the contract this
// executor depends on (the relay can only consume b64_json), and an inherited
// default states nothing. n, size, temperature and max_tokens are absent
// because /v1/images/generations uses none of them and n is not exposed by
// this feature at all -- sending a parameter the endpoint ignores would be an
// enabled-but-inert control.
//
// A history with no user text yields an empty prompt, which is NOT rejected
// here: validateImagesRequest owns "a prompt is required" and answers
// images.prompt_required, and a second copy of that rule in the executor
// would be free to drift from the endpoint's. The refusal reaches the run
// through upstreamErrorCode above.
func buildImagesBody(prep PrepareRunResult) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model":           prep.Settings.Model,
		"prompt":          lastUserPrompt(prep.History),
		"response_format": "b64_json",
	})
}

// lastUserPrompt is the text of the LAST user message in the prepared history
// -- the wish this run is about. The text is pulled with portal.MessageText,
// the extractor deriveChatTitle already uses, so the two shapes a content can
// have (a plain string, or an array of parts) are understood in exactly one
// place.
func lastUserPrompt(history []portal.ChatAPIMessage) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			return portal.MessageText(history[i].Content)
		}
	}
	return ""
}

// imagePartsFrom turns the upstream response into the assistant turn's
// content: one image_url part per produced image, each preceded by its own
// revised_prompt when the upstream reported one.
//
// A revised_prompt becomes an ORDINARY {"type":"text"} part rather than a new
// field on the image part: the existing text renderer then shows it and
// buildAPIHistory carries it forward, with no new shape for anything to learn.
//
// An item with no base64 payload is skipped -- it carries no image, and a
// data: URL with nothing behind it is the same blank bubble a zero-length
// data[] would be. A response that leaves no image part at all is an error,
// never an empty success.
// The two failures are checked in this order on purpose: "produced nothing"
// is the more fundamental fact, and a response with no image at all should
// not be reported as one whose media type could not be named.
func imagePartsFrom(resp imagesResponse) (json.RawMessage, error) {
	produced := 0
	for _, item := range resp.Data {
		if item.B64JSON != "" {
			produced++
		}
	}
	if produced == 0 {
		return nil, errors.New(imageRunNoImageMessage)
	}
	mediaType, err := imageMediaType(resp.OutputFormat)
	if err != nil {
		return nil, err
	}
	parts := make([]any, 0, produced*2)
	for _, item := range resp.Data {
		if item.B64JSON == "" {
			continue
		}
		if strings.TrimSpace(item.RevisedPrompt) != "" {
			parts = append(parts, imageTextPart{Type: "text", Text: item.RevisedPrompt})
		}
		parts = append(parts, imageURLPart{
			Type:     "image_url",
			ImageURL: imageURLTarget{URL: "data:" + mediaType + ";base64," + item.B64JSON},
		})
	}
	return json.Marshal(parts)
}

// imageMediaType turns the response's own output_format ("png") into the data:
// URL's media type ("image/png"), and refuses rather than invents one when the
// response reported nothing usable. Lower-cased because a media type is
// case-insensitive and the portal's download names its file from this subtype.
func imageMediaType(outputFormat string) (string, error) {
	format := strings.TrimSpace(outputFormat)
	if !imageSubtypePattern.MatchString(format) {
		return "", errors.New(imageRunFormatUnknownMessage)
	}
	return "image/" + strings.ToLower(format), nil
}
