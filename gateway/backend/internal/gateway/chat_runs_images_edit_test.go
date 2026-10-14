// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
	"time"
)

// The edit half of the image kind's executor (ADR-043 R1): a turn whose last user
// message carries an image is an EDIT and goes to /v1/images/edits as multipart;
// any other image turn is the generation it always was.

const imagesOKBody = `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`

// editSourceBytes is a stand-in for an uploaded image: the executor never decodes
// it, it only carries it, so what matters is that the bytes are distinctive, not
// binary-valid -- and that they contain every byte value, so a text-oriented
// copy anywhere on the path would corrupt them.
func editSourceBytes(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

func imageDataURL(mediaType string, raw []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// editRunPrep is imageRunPrep's edit counterpart: the last user message is the
// prompt text followed by one image part per url.
func editRunPrep(text string, urls ...string) PrepareRunResult {
	parts := []string{fmt.Sprintf(`{"type":"text","text":%q}`, text)}
	for _, u := range urls {
		parts = append(parts, fmt.Sprintf(`{"type":"image_url","image_url":{"url":%q}}`, u))
	}
	return PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage("[" + strings.Join(parts, ",") + "]")}},
		Settings: portal.ChatRunSettings{Model: "sd-turbo", Kind: chatRunKindImage},
	}
}

// editFormPart is one part of the multipart body an edit run sent.
type editFormPart struct {
	name        string
	filename    string
	contentType string
	data        []byte
}

// parseEditBody reads the upstream's request as the edit it must be: multipart
// with a boundary, parts in the order sent.
func parseEditBody(t *testing.T, got capturedUpstream) []editFormPart {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(got.contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		t.Fatalf("upstream Content-Type = %q, want multipart/form-data with a boundary", got.contentType)
	}
	mr := multipart.NewReader(bytes.NewReader(got.body), params["boundary"])
	var parts []editFormPart
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return parts
		}
		if err != nil {
			t.Fatalf("upstream body is not valid multipart: %v", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read part %q: %v", part.FormName(), err)
		}
		parts = append(parts, editFormPart{
			name: part.FormName(), filename: part.FileName(),
			contentType: part.Header.Get("Content-Type"), data: data,
		})
	}
}

func editField(t *testing.T, parts []editFormPart, name string) (string, bool) {
	t.Helper()
	var value string
	found := false
	for _, p := range parts {
		if p.name != name {
			continue
		}
		if found {
			t.Fatalf("field %q appears twice", name)
		}
		value, found = string(p.data), true
	}
	return value, found
}

func editImageParts(parts []editFormPart) []editFormPart {
	var out []editFormPart
	for _, p := range parts {
		if p.name == "image" || p.name == "image[]" {
			out = append(out, p)
		}
	}
	return out
}

// runToTerminal starts the run, waits for it to end and returns the request the
// upstream saw (zero when it saw none).
func runToTerminal(t *testing.T, srv *Server, owner auth.Token, chatID string, prep PrepareRunResult, seen <-chan capturedUpstream) (*ChatRun, capturedUpstream, bool) {
	t.Helper()
	run, err := srv.startChatRun(owner, chatID, prep)
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	select {
	case got := <-seen:
		return run, got, true
	default:
		return run, capturedUpstream{}, false
	}
}

// A turn whose last user message carries an image is an edit: the source reaches
// the upstream as the multipart `image` part -- its own bytes, name and type -- on
// /v1/images/edits, with the prompt, the thread's image settings and the model,
// and the edit's one extra knob (strength) rides the prompt's extra-args block.
func TestImageRunWithASourceImagePostsAMultipartEdit(t *testing.T) {
	upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
	chats := store.NewMemoryChatStore(0)
	srv, _, owner, chatID := newImageRunTestServerWithChats(t, upstream.URL, chats)

	source := editSourceBytes(4096, 3)
	prep := editRunPrep("make it blue", imageDataURL("image/png", source))
	strength := 0.6
	prep.Settings.Image = &portal.ChatImageSettings{Size: "512x512", N: 2, Strength: &strength}

	run, got, ok := runToTerminal(t, srv, owner, chatID, prep, seen)
	if run.statusValue() != "completed" {
		t.Fatalf("status = %q (err %q), want completed", run.statusValue(), runError(t, run))
	}
	if !ok {
		t.Fatal("upstream saw no request")
	}
	if got.path != "/v1/images/edits" {
		t.Fatalf("upstream path = %q, want /v1/images/edits", got.path)
	}
	parts := parseEditBody(t, got)

	imgs := editImageParts(parts)
	if len(imgs) != 1 {
		t.Fatalf("%d image parts, want 1: %+v", len(imgs), parts)
	}
	if imgs[0].name != "image" || imgs[0].filename != "image.png" || imgs[0].contentType != "image/png" {
		t.Fatalf("image part = name %q file %q type %q, want image / image.png / image/png", imgs[0].name, imgs[0].filename, imgs[0].contentType)
	}
	if !bytes.Equal(imgs[0].data, source) {
		t.Fatalf("the source arrived as %d bytes that differ from the %d uploaded", len(imgs[0].data), len(source))
	}
	// The part header is spelled the way every client spells it -- quoted values,
	// name first -- because sd-server's form parser (cpp-httplib) matches that
	// shape. Go's own reader (parseEditBody above) accepts other spellings, so it
	// cannot pin this.
	if !bytes.Contains(got.body, []byte("Content-Disposition: form-data; name=\"image\"; filename=\"image.png\"\r\nContent-Type: image/png\r\n\r\n")) {
		t.Fatalf("the image part header is not the canonical form-data spelling: %.300q", got.body[:300])
	}
	if !bytes.Contains(got.body, []byte("Content-Disposition: form-data; name=\"model\"\r\n\r\nsd-turbo\r\n")) {
		t.Fatalf("a text field is not spelled the canonical way: %.300q", got.body[:300])
	}

	for name, want := range map[string]string{
		"model": "sd-turbo", "response_format": "b64_json", "size": "512x512", "n": "2",
	} {
		if v, ok := editField(t, parts, name); !ok || v != want {
			t.Fatalf("field %q = %q (present %v), want %q", name, v, ok, want)
		}
	}
	prompt, _ := editField(t, parts, "prompt")
	if !strings.HasPrefix(prompt, "make it blue") || !strings.HasSuffix(prompt, `<sd_cpp_extra_args>{"strength":0.6}</sd_cpp_extra_args>`) {
		t.Fatalf("prompt = %q, want the user's text then the strength block", prompt)
	}
	// Nothing of the JSON generations body leaks into the form.
	for _, p := range parts {
		switch p.name {
		case "model", "prompt", "response_format", "size", "n", "image":
		default:
			t.Fatalf("unexpected form part %q: only the fields the endpoint reads are sent", p.name)
		}
	}

	// The edit's result is committed exactly like a generation's.
	chat, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(chat.Content), `"type":"image_url"`); n != 1 {
		t.Fatalf("the committed turn carries %d image parts, want the 1 generated: %s", n, chat.Content)
	}
}

// Every format the store holds is sent under its own name and type: the part's
// filename extension and Content-Type come from the source's media type, never
// from a constant.
func TestImageRunEditNamesTheSourcePartAfterItsMediaType(t *testing.T) {
	for _, tc := range []struct {
		mediaType, filename string
	}{
		{"image/png", "image.png"},
		{"image/jpeg", "image.jpg"},
		{"image/webp", "image.webp"},
		{"image/gif", "image.gif"},
	} {
		t.Run(tc.mediaType, func(t *testing.T) {
			upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
			srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
			raw := editSourceBytes(300, 9)
			run, got, ok := runToTerminal(t, srv, owner, chatID, editRunPrep("x", imageDataURL(tc.mediaType, raw)), seen)
			if run.statusValue() != "completed" || !ok {
				t.Fatalf("status = %q (err %q), upstream saw a request: %v", run.statusValue(), runError(t, run), ok)
			}
			imgs := editImageParts(parseEditBody(t, got))
			if len(imgs) != 1 || imgs[0].filename != tc.filename || imgs[0].contentType != tc.mediaType || !bytes.Equal(imgs[0].data, raw) {
				t.Fatalf("image part = %+v, want %s as %s", imgs, tc.filename, tc.mediaType)
			}
		})
	}
}

// More than one image in the last user message is sent whole, in order, under the
// repeated `image[]` spelling -- none is silently dropped. (The composer attaches
// one source; this is what an API client of the run endpoint gets.)
func TestImageRunEditSendsEveryImageInOrder(t *testing.T) {
	upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	first, second := editSourceBytes(200, 1), editSourceBytes(300, 2)
	run, got, ok := runToTerminal(t, srv, owner, chatID, editRunPrep("blend", imageDataURL("image/png", first), imageDataURL("image/jpeg", second)), seen)
	if run.statusValue() != "completed" || !ok {
		t.Fatalf("status = %q (err %q), upstream saw a request: %v", run.statusValue(), runError(t, run), ok)
	}
	imgs := editImageParts(parseEditBody(t, got))
	if len(imgs) != 2 {
		t.Fatalf("%d image parts, want 2", len(imgs))
	}
	if imgs[0].name != "image[]" || imgs[1].name != "image[]" {
		t.Fatalf("part names = %q, %q, want the repeated image[] spelling", imgs[0].name, imgs[1].name)
	}
	if !bytes.Equal(imgs[0].data, first) || !bytes.Equal(imgs[1].data, second) {
		t.Fatal("the images were reordered or altered")
	}
	if imgs[0].contentType != "image/png" || imgs[1].contentType != "image/jpeg" {
		t.Fatalf("part types = %q, %q", imgs[0].contentType, imgs[1].contentType)
	}
	for _, header := range []string{
		"Content-Disposition: form-data; name=\"image[]\"; filename=\"image.png\"\r\n",
		"Content-Disposition: form-data; name=\"image[]\"; filename=\"image.jpg\"\r\n",
	} {
		if !bytes.Contains(got.body, []byte(header)) {
			t.Fatalf("the body lacks the canonical part header %q", header)
		}
	}
}

// A turn with NO image in its last user message is a generation, byte for byte
// what it was before edits existed -- even when an older message carries an image
// (an earlier edit's source, or the result of one): only the last user message
// decides. This is the regression pin for the shared executor.
func TestImageRunWithoutASourceStillPostsGenerations(t *testing.T) {
	older := imageDataURL("image/png", editSourceBytes(500, 4))
	compression := 80
	for _, tc := range []struct {
		name    string
		history []portal.ChatAPIMessage
	}{
		{"a plain prompt", []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"a cat"`)}}},
		{"an earlier user message carried a source", []portal.ChatAPIMessage{
			{Role: "user", Content: json.RawMessage(fmt.Sprintf(`[{"type":"text","text":"old"},{"type":"image_url","image_url":{"url":%q}}]`, older))},
			{Role: "assistant", Content: json.RawMessage(`"done"`)},
			{Role: "user", Content: json.RawMessage(`"a cat"`)},
		}},
		{"an earlier assistant turn holds a generated image", []portal.ChatAPIMessage{
			{Role: "user", Content: json.RawMessage(`"first wish"`)},
			{Role: "assistant", Content: json.RawMessage(fmt.Sprintf(`[{"type":"image_url","image_url":{"url":%q}}]`, older))},
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"a cat"}]`)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
			srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
			prep := PrepareRunResult{History: tc.history, Settings: portal.ChatRunSettings{
				Model: "sd-turbo", Kind: chatRunKindImage,
				Image: &portal.ChatImageSettings{Size: "512x512", N: 2, OutputFormat: "webp", OutputCompression: &compression},
			}}
			run, got, ok := runToTerminal(t, srv, owner, chatID, prep, seen)
			if run.statusValue() != "completed" || !ok {
				t.Fatalf("status = %q (err %q), upstream saw a request: %v", run.statusValue(), runError(t, run), ok)
			}
			if got.path != "/v1/images/generations" {
				t.Fatalf("upstream path = %q, want /v1/images/generations", got.path)
			}
			if got.contentType != "application/json" {
				t.Fatalf("upstream Content-Type = %q, want application/json", got.contentType)
			}
			// The exact bytes the executor has always sent: json.Marshal of the map,
			// keys sorted. Spelled out so a change to how the body is assembled
			// fails here rather than being compared against itself.
			const want = `{"model":"sd-turbo","n":2,"output_compression":80,"output_format":"webp","prompt":"a cat","response_format":"b64_json","size":"512x512"}`
			if string(got.body) != want {
				t.Fatalf("generations body =\n%s\nwant\n%s", got.body, want)
			}
		})
	}
}

// A source that is not a readable image fails the run with its own code, and
// nothing is sent: not the edit, and not a generation either -- an edit whose
// source was dropped would produce an unrelated image from the prompt alone. A url
// it cannot read is never fetched (an external link is the one a run must never
// follow), and an unresolved reference never reaches the wire as text.
func TestImageRunWithAnUnreadableSourceFailsAndSendsNothing(t *testing.T) {
	for _, tc := range []struct{ name, url string }{
		{"an external url", "https://example.com/cat.png"},
		{"an unresolved gateway reference", "/api/portal/chat-blobs/blob_" + strings.Repeat("a", 32)},
		{"base64 that does not decode", "data:image/png;base64,!!!not-base64!!!"},
		{"a type outside the allow-list", imageDataURL("text/html", []byte("<script>"))},
		{"an empty payload", "data:image/png;base64,"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
			srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
			run, _, sent := runToTerminal(t, srv, owner, chatID, editRunPrep("make it blue", tc.url), seen)
			if sent {
				t.Fatal("the upstream saw a request: an unreadable source must send nothing")
			}
			if run.statusValue() != "error" {
				t.Fatalf("status = %q, want error", run.statusValue())
			}
			if got := runError(t, run); got != imageRunSourceUnreadableMessage {
				t.Fatalf("error = %q, want %q", got, imageRunSourceUnreadableMessage)
			}
		})
	}
}

// One unreadable image among readable ones fails the run too: the rest are not
// sent on their own.
func TestImageRunWithOneUnreadableSourceAmongSeveralSendsNothing(t *testing.T) {
	upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, _, sent := runToTerminal(t, srv, owner, chatID, editRunPrep("x", imageDataURL("image/png", editSourceBytes(50, 1)), "https://example.com/b.png"), seen)
	if sent || run.statusValue() != "error" || runError(t, run) != imageRunSourceUnreadableMessage {
		t.Fatalf("sent=%v status=%q err=%q, want nothing sent and the unreadable-source error", sent, run.statusValue(), runError(t, run))
	}
}

// The whole path, through the real portal service: an upload is the first turn's
// source; its stored copy -- a /chat-blobs/{id} reference in the transcript -- is
// the second turn's. The executor resolves the reference to the original bytes
// (the same chat-scoped resolution a text run uses, PrepareChatRun), the reference
// is reused rather than copied, and the transcript never grows inline bytes.
func TestImageRunEditsFromAStoredReferenceWithoutCopyingIt(t *testing.T) {
	upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
	chats := store.NewMemoryChatStore(0)
	srv, _, owner, _ := newImageRunTestServerWithChats(t, upstream.URL, chats)
	ctx := context.Background()
	created, err := srv.Portal.CreateChat(ctx, owner, portal.CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	chatID := created.ID
	source := editSourceBytes(6000, 5)

	turn := func(userMessage json.RawMessage) (capturedUpstream, *ChatRun) {
		t.Helper()
		history, settings, err := srv.Portal.PrepareChatRun(ctx, owner, chatID, portal.PrepareRunRequest{
			UserMessage: userMessage,
			Settings:    portal.ChatRunSettings{Model: "sd-turbo", Kind: chatRunKindImage},
		})
		if err != nil {
			t.Fatalf("PrepareChatRun: %v", err)
		}
		run, got, ok := runToTerminal(t, srv, owner, chatID, PrepareRunResult{History: history, Settings: settings}, seen)
		if run.statusValue() != "completed" || !ok {
			t.Fatalf("status = %q (err %q), upstream saw a request: %v", run.statusValue(), runError(t, run), ok)
		}
		return got, run
	}

	// Turn 1: an upload.
	got, _ := turn(json.RawMessage(fmt.Sprintf(`[{"type":"text","text":"make it blue"},{"type":"image_url","image_url":{"url":%q}}]`, imageDataURL("image/png", source))))
	if got.path != "/v1/images/edits" {
		t.Fatalf("first turn path = %q, want the edit", got.path)
	}
	if imgs := editImageParts(parseEditBody(t, got)); len(imgs) != 1 || !bytes.Equal(imgs[0].data, source) {
		t.Fatal("the uploaded source did not reach the upstream byte for byte")
	}

	// Turn 2: name the stored copy as the source.
	chat, err := srv.Portal.GetChat(ctx, owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	blobs := storedImageBlobs(t, chats, chat.Content)
	if len(blobs) != 2 {
		t.Fatalf("the transcript references %d blobs after turn 1, want the source and the result", len(blobs))
	}
	ref := "/api/portal/chat-blobs/" + blobs[0].ID
	got, _ = turn(json.RawMessage(fmt.Sprintf(`[{"type":"text","text":"now greener"},{"type":"image_url","image_url":{"url":%q}}]`, ref)))
	if got.path != "/v1/images/edits" {
		t.Fatalf("second turn path = %q, want the edit", got.path)
	}
	imgs := editImageParts(parseEditBody(t, got))
	if len(imgs) != 1 || !bytes.Equal(imgs[0].data, source) || imgs[0].contentType != "image/png" {
		t.Fatalf("the stored source did not resolve to the original %d bytes (got %d, type %q)", len(source), len(imgs[0].data), imgs[0].contentType)
	}

	// No copy: the second user turn points at the SAME blob, and nothing inline
	// was written into the transcript.
	chat, err = srv.Portal.GetChat(ctx, owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chat.Content), "data:") {
		t.Fatalf("the transcript gained inline image bytes: %.200s", chat.Content)
	}
	blobs = storedImageBlobs(t, chats, chat.Content)
	if len(blobs) != 4 {
		t.Fatalf("the transcript references %d blobs, want source, result, source, result", len(blobs))
	}
	if blobs[0].ID != blobs[2].ID {
		t.Fatalf("the second turn's source is blob %s, want the first turn's %s (a reference, not a copy)", blobs[2].ID, blobs[0].ID)
	}
}

// A source the chat does not hold never reaches the executor: the run is refused
// at its start, so there is nothing to send -- and above all no foreign bytes.
func TestImageRunRefusesAForeignSourceBeforeAnythingIsSent(t *testing.T) {
	upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
	srv, _, owner, _ := newImageRunTestServerWithChats(t, upstream.URL, store.NewMemoryChatStore(0))
	ctx := context.Background()
	other, err := srv.Portal.CreateChat(ctx, owner, portal.CreateChatRequest{Content: json.RawMessage(`{"settings":{},"messages":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	// A blob held by ANOTHER chat of the same owner.
	if _, _, err := srv.Portal.PrepareChatRun(ctx, owner, other.ID, portal.PrepareRunRequest{
		UserMessage: json.RawMessage(fmt.Sprintf(`[{"type":"image_url","image_url":{"url":%q}}]`, imageDataURL("image/png", editSourceBytes(100, 1)))),
		Settings:    portal.ChatRunSettings{Model: "sd-turbo", Kind: chatRunKindImage},
	}); err != nil {
		t.Fatal(err)
	}
	otherChat, err := srv.Portal.GetChat(ctx, owner, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	foreign := storedImageURLsOf(t, otherChat.Content)[0]

	mine, err := srv.Portal.CreateChat(ctx, owner, portal.CreateChatRequest{Content: json.RawMessage(`{"settings":{},"messages":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = srv.Portal.PrepareChatRun(ctx, owner, mine.ID, portal.PrepareRunRequest{
		UserMessage: json.RawMessage(fmt.Sprintf(`[{"type":"text","text":"steal"},{"type":"image_url","image_url":{"url":%q}}]`, foreign)),
		Settings:    portal.ChatRunSettings{Model: "sd-turbo", Kind: chatRunKindImage},
	})
	if !errors.Is(err, portal.ErrChatBlobNotOwned) {
		t.Fatalf("err = %v, want portal.ErrChatBlobNotOwned: a run whose source is another chat's blob must be refused", err)
	}
	select {
	case got := <-seen:
		t.Fatalf("the upstream saw a request (%s)", got.path)
	default:
	}
}

// storedImageURLsOf is the image urls of a stored transcript, in order.
func storedImageURLsOf(t *testing.T, content json.RawMessage) []string {
	t.Helper()
	var doc struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, m := range doc.Messages {
		for _, img := range portal.MessageImages(m.Content) {
			urls = append(urls, img.URL)
		}
	}
	return urls
}

// An edit rides the same gate as any request to the edits endpoint: a model an
// operator opted out of editing (image_edit: no) is refused by the endpoint, and
// the refusal reaches the run as the endpoint's own code -- the composer never
// offers the attach control for it, but a replayed or API-started turn can still
// get here, and must fail loudly instead of generating something else.
func TestImageRunEditOfANonEditModelKeepsTheGatesCode(t *testing.T) {
	upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	if err := srv.Routes.UpsertMappingCapabilities(context.Background(), "route-sd-turbo", []routing.CapabilityRow{{Capability: routing.CapabilityImageEdit, Verdict: routing.CapabilityNo, Source: "manual", CheckedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	run, _, sent := runToTerminal(t, srv, owner, chatID, editRunPrep("x", imageDataURL("image/png", editSourceBytes(80, 1))), seen)
	if sent {
		t.Fatal("the upstream saw an edit for a model its operator opted out")
	}
	if run.statusValue() != "error" || runError(t, run) != "routing.model_not_capable" {
		t.Fatalf("status = %q error = %q, want error routing.model_not_capable", run.statusValue(), runError(t, run))
	}

	// ...and the same model still GENERATES: the opt-out is for edits only.
	run, got, sent := runToTerminal(t, srv, owner, chatID, imageRunPrep(), seen)
	if !sent || got.path != "/v1/images/generations" || run.statusValue() != "completed" {
		t.Fatalf("generation after the opt-out: sent=%v path=%q status=%q (err %q)", sent, got.path, run.statusValue(), runError(t, run))
	}
}
