// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
)

// The edit route of sd-server reads png and jpeg only, and nothing between the
// portal and it checks a source's type on a whole-image edit (the gateway's edits
// endpoint converts a mask, and only then looks at the source). A webp or gif source
// would therefore be forwarded and come back as the upstream's own failure -- the
// generic "please try again" of images.upstream_error, which can never succeed. The
// portal's executor refuses it first, with a run code of its own that says what to
// do, before anything is sent. This covers every way such a source gets here: the
// refine of a generated webp result (a thread whose output format is webp), and the
// replay of an older upload the composer's own check (which only guards a NEW
// attachment) never saw.

// sdLikeUpstream stands in for sd-server's edit route: it answers body, except that an
// edit (multipart) whose image is a webp or gif is failed the way sd-server fails it --
// a 500 the gateway relays as images.upstream_error. So a test that lets such a source
// through the executor sees the real symptom (the generic "please try again") rather
// than a happy 200.
func sdLikeUpstream(t *testing.T, body string) (*httptest.Server, <-chan capturedUpstream) {
	t.Helper()
	seen := make(chan capturedUpstream, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		select {
		case seen <- capturedUpstream{body: raw, path: r.URL.Path, contentType: r.Header.Get("Content-Type")}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		if bytes.Contains(raw, []byte("Content-Type: image/webp")) || bytes.Contains(raw, []byte("Content-Type: image/gif")) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"unsupported image format"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func TestImageRunSourceUnsupportedCodeIsTheWireString(t *testing.T) {
	if imageRunSourceUnsupportedMessage != "gateway.chat_run_image_source_unsupported" {
		t.Fatalf("imageRunSourceUnsupportedMessage = %q: the portal labels this exact string", imageRunSourceUnsupportedMessage)
	}
}

// An edit whose source is neither png nor jpeg fails the run with the dedicated
// code, and the upstream is never called: there is no wait on sd-server for an
// answer that cannot be a success.
func TestImageRunRefusesAnEditSourceThatIsNeitherPNGNorJPEG(t *testing.T) {
	png := imageDataURL("image/png", editSourceBytes(80, 1))
	for _, tc := range []struct {
		name string
		prep func() PrepareRunResult
	}{
		{"a webp", func() PrepareRunResult { return editRunPrep("x", imageDataURL("image/webp", editSourceBytes(80, 2))) }},
		{"a gif", func() PrepareRunResult { return editRunPrep("x", imageDataURL("image/gif", editSourceBytes(80, 3))) }},
		// One unsupported source among supported ones fails the call: the rest are
		// not sent on their own.
		{"a png and a webp", func() PrepareRunResult {
			return editRunPrep("x", png, imageDataURL("image/webp", editSourceBytes(80, 4)))
		}},
		// With a mask the endpoint would call a non-png/jpeg source an unsupported
		// format as well, but only after the loopback round trip; the executor's own
		// check comes first and says the same thing the same way.
		{"a webp with a mask", func() PrepareRunResult {
			return maskedRunPrep("x", []string{imageDataURL("image/webp", editSourceBytes(80, 5))}, []string{imageDataURL("image/png", pngOf(t, openAIMask(8, 6, 3)))})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, seen := sdLikeUpstream(t, imagesOKBody)
			srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
			run, _, sent := runToTerminal(t, srv, owner, chatID, tc.prep(), seen)
			if sent {
				t.Fatal("the upstream saw a request: a source it cannot read must send nothing")
			}
			if run.statusValue() != "error" || runError(t, run) != imageRunSourceUnsupportedMessage {
				t.Fatalf("status = %q error = %q, want error %q (not the generic upstream error, not the unreadable-source one)", run.statusValue(), runError(t, run), imageRunSourceUnsupportedMessage)
			}
		})
	}
}

// The guard is about the SOURCE's type: a png or a jpeg source -- with or without a
// mask, which stays png-only -- goes out as it always did.
func TestImageRunStillEditsAPNGOrJPEGSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		prep func(t *testing.T) PrepareRunResult
	}{
		{"a png", func(*testing.T) PrepareRunResult {
			return editRunPrep("x", imageDataURL("image/png", editSourceBytes(80, 1)))
		}},
		{"a jpeg", func(*testing.T) PrepareRunResult {
			return editRunPrep("x", imageDataURL("image/jpeg", editSourceBytes(80, 2)))
		}},
		{"a jpeg with a mask", func(t *testing.T) PrepareRunResult {
			return maskedRunPrep("x", []string{imageDataURL("image/jpeg", jpegOf(t, sourceImage(8, 6)))}, []string{imageDataURL("image/png", pngOf(t, openAIMask(8, 6, 3)))})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
			srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
			run, got, sent := runToTerminal(t, srv, owner, chatID, tc.prep(t), seen)
			if !sent || run.statusValue() != "completed" || got.path != "/v1/images/edits" {
				t.Fatalf("sent=%v status=%q path=%q (err %q), want the edit sent and completed", sent, run.statusValue(), got.path, runError(t, run))
			}
		})
	}
}

// Through the real portal service and the loopback: the headline case -- a thread
// that generates webp, and the refine of its result -- and the replay of a legacy
// webp upload. Each ends in the dedicated code with nothing sent; the stored
// transcript keeps its references and gains no inline bytes.
func TestImageRunRefusesAStoredWebpSourceBeforeAnythingIsSent(t *testing.T) {
	generated := editSourceBytes(5000, 7)
	webpBody := fmt.Sprintf(`{"created":1,"output_format":"webp","data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(generated))
	upstream, seen := sdLikeUpstream(t, webpBody)
	chats := store.NewMemoryChatStore(0)
	srv, _, owner, _ := newImageRunTestServerWithChats(t, upstream.URL, chats)
	ctx := context.Background()
	newChat := func() string {
		created, err := srv.Portal.CreateChat(ctx, owner, portal.CreateChatRequest{Content: json.RawMessage(`{"settings":{},"messages":[]}`)})
		if err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	// turn runs one prepared turn and returns the run and whether the upstream saw a request.
	turn := func(chatID string, req portal.PrepareRunRequest) (*ChatRun, bool) {
		t.Helper()
		req.Settings = portal.ChatRunSettings{Model: "sd-turbo", Kind: chatRunKindImage}
		history, settings, err := srv.Portal.PrepareChatRun(ctx, owner, chatID, req)
		if err != nil {
			t.Fatalf("PrepareChatRun: %v", err)
		}
		run, _, sent := runToTerminal(t, srv, owner, chatID, PrepareRunResult{History: history, Settings: settings}, seen)
		return run, sent
	}
	storedMessages := func(chatID string) []json.RawMessage {
		t.Helper()
		chat, err := srv.Portal.GetChat(ctx, owner, chatID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(chat.Content), "data:") {
			t.Fatalf("the transcript holds inline image bytes: %.200s", chat.Content)
		}
		var doc struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(chat.Content, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Messages
	}
	refused := func(t *testing.T, run *ChatRun, sent bool) {
		t.Helper()
		if sent {
			t.Fatal("the upstream saw a request: a webp source must send nothing")
		}
		if run.statusValue() != "error" || runError(t, run) != imageRunSourceUnsupportedMessage {
			t.Fatalf("status = %q error = %q, want error %q", run.statusValue(), runError(t, run), imageRunSourceUnsupportedMessage)
		}
	}

	t.Run("the refine of a generated webp result", func(t *testing.T) {
		chatID := newChat()
		run, sent := turn(chatID, portal.PrepareRunRequest{UserMessage: json.RawMessage(`"a lighthouse"`)})
		if run.statusValue() != "completed" || !sent {
			t.Fatalf("the generation: status = %q (err %q), upstream saw a request: %v", run.statusValue(), runError(t, run), sent)
		}
		chat, err := srv.Portal.GetChat(ctx, owner, chatID)
		if err != nil {
			t.Fatal(err)
		}
		blobs := storedImageBlobs(t, chats, chat.Content)
		if len(blobs) != 1 || blobs[0].ContentType != "image/webp" || !bytes.Equal(blobs[0].plain, generated) {
			t.Fatalf("the result is stored as %+v, want one image/webp blob of the generated bytes", blobs)
		}
		ref := "/api/portal/chat-blobs/" + blobs[0].ID

		run, sent = turn(chatID, portal.PrepareRunRequest{UserMessage: json.RawMessage(fmt.Sprintf(`[{"type":"text","text":"make the sky purple"},{"type":"image_url","image_url":{"url":%q}}]`, ref))})
		refused(t, run, sent)

		// Replay of the same refine turn (regenerate: the history up to it, references
		// and all) is refused the same way.
		messages := storedMessages(chatID)
		if len(messages) < 3 {
			t.Fatalf("the transcript holds %d messages, want the prompt, the result and the refine turn", len(messages))
		}
		run, sent = turn(chatID, portal.PrepareRunRequest{EditedHistory: messages[:3]})
		refused(t, run, sent)
	})

	t.Run("the replay of a legacy webp upload", func(t *testing.T) {
		chatID := newChat()
		webp := imageDataURL("image/webp", editSourceBytes(900, 3))
		run, sent := turn(chatID, portal.PrepareRunRequest{UserMessage: json.RawMessage(fmt.Sprintf(`[{"type":"text","text":"make it blue"},{"type":"image_url","image_url":{"url":%q}}]`, webp))})
		refused(t, run, sent)

		// What the browser holds after the failed turn -- the upload as a stored
		// reference -- replayed (edit of the prompt keeps the source part).
		messages := storedMessages(chatID)
		if len(messages) < 1 {
			t.Fatal("the failed turn was not stored")
		}
		run, sent = turn(chatID, portal.PrepareRunRequest{EditedHistory: messages[:1]})
		refused(t, run, sent)
	})
}
