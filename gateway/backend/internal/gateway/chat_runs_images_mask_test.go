// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"mime"
	"net/http"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
)

// Masked inpainting (ADR-043 R1, the mask upload): an edit whose last user message
// also carries a MASK -- an image part tagged portal.ImagePurposeMask -- sends it as
// the OpenAI `mask` form field of POST /v1/images/edits, next to the source image
// and never as a second source. The gateway's edits endpoint then converts the
// mask's alpha into the luminance sd-server reads; the executor only carries it.

// taggedMaskPart is a user message's mask part, tagged the way the composer writes it.
func taggedMaskPart(url string) string {
	return fmt.Sprintf(`{"type":"image_url","image_url":{"url":%q},"purpose":"mask"}`, url)
}

// maskedRunPrep is editRunPrep with the mask part(s) after the source image(s): the
// last user message is the prompt, one source part per sourceURL, then one tagged
// mask part per maskURL.
func maskedRunPrep(text string, sourceURLs, maskURLs []string) PrepareRunResult {
	parts := []string{fmt.Sprintf(`{"type":"text","text":%q}`, text)}
	for _, u := range sourceURLs {
		parts = append(parts, fmt.Sprintf(`{"type":"image_url","image_url":{"url":%q}}`, u))
	}
	for _, u := range maskURLs {
		parts = append(parts, taggedMaskPart(u))
	}
	return PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage("[" + strings.Join(parts, ",") + "]")}},
		Settings: portal.ChatRunSettings{Model: "sd-turbo", Kind: chatRunKindImage},
	}
}

func editPartsNamed(parts []editFormPart, name string) []editFormPart {
	var out []editFormPart
	for _, p := range parts {
		if p.name == name {
			out = append(out, p)
		}
	}
	return out
}

// The executor's own half, without the gateway in between: the mask becomes the
// `mask` file part, named and typed as a PNG, after the source -- and the source
// stays `image`: a mask is not a second image, so it must not turn the source into
// the repeated image[] spelling.
func TestBuildImageEditBodyCarriesTheMaskAsTheMaskPart(t *testing.T) {
	source := editSourceBytes(300, 1)
	mask := editSourceBytes(120, 2)
	prep := editRunPrep("fill the sky")

	body, contentType, err := buildImageEditBody(prep, imageEditInputs{
		sources: []imageEditSource{{mediaType: "image/png", data: source}},
		mask:    &imageEditSource{mediaType: "image/png", data: mask},
	})
	if err != nil {
		t.Fatalf("buildImageEditBody: %v", err)
	}
	parts := parseEditBody(t, capturedUpstream{body: body, contentType: contentType})

	if imgs := editImageParts(parts); len(imgs) != 1 || imgs[0].name != "image" || !bytes.Equal(imgs[0].data, source) {
		t.Fatalf("image parts = %+v, want the one source as `image`", imgs)
	}
	masks := editPartsNamed(parts, "mask")
	if len(masks) != 1 {
		t.Fatalf("%d mask parts, want 1: %+v", len(masks), parts)
	}
	if masks[0].filename != "mask.png" || masks[0].contentType != "image/png" || !bytes.Equal(masks[0].data, mask) {
		t.Fatalf("mask part = file %q type %q (%d bytes), want mask.png / image/png / the uploaded mask", masks[0].filename, masks[0].contentType, len(masks[0].data))
	}
	// The mask follows the source in the body.
	if parts[len(parts)-1].name != "mask" || parts[len(parts)-2].name != "image" {
		t.Fatalf("part order = %v, want ... image, mask", partNames(parts))
	}
	// Spelled the canonical way, like the image part (cpp-httplib's form parser).
	if !bytes.Contains(body, []byte("Content-Disposition: form-data; name=\"mask\"; filename=\"mask.png\"\r\nContent-Type: image/png\r\n\r\n")) {
		t.Fatalf("the mask part header is not the canonical form-data spelling: %.400q", body)
	}
}

func partNames(parts []editFormPart) []string {
	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = p.name
	}
	return names
}

// With several sources the repeated image[] spelling is chosen by the number of
// SOURCES, whatever the mask: a source and a mask alone are `image`, two sources
// and a mask are two `image[]` and the `mask`.
func TestBuildImageEditBodyCountsOnlySourcesForTheImageSpelling(t *testing.T) {
	src := func(seed byte) imageEditSource {
		return imageEditSource{mediaType: "image/png", data: editSourceBytes(80, seed)}
	}
	mask := &imageEditSource{mediaType: "image/png", data: editSourceBytes(40, 9)}
	for _, tc := range []struct {
		name      string
		sources   []imageEditSource
		wantImage string
	}{
		{"one source and a mask", []imageEditSource{src(1)}, "image"},
		{"two sources and a mask", []imageEditSource{src(1), src(2)}, "image[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, ct, err := buildImageEditBody(editRunPrep("x"), imageEditInputs{sources: tc.sources, mask: mask})
			if err != nil {
				t.Fatal(err)
			}
			parts := parseEditBody(t, capturedUpstream{body: body, contentType: ct})
			imgs := editImageParts(parts)
			if len(imgs) != len(tc.sources) {
				t.Fatalf("%d image parts, want %d", len(imgs), len(tc.sources))
			}
			for _, p := range imgs {
				if p.name != tc.wantImage {
					t.Fatalf("source part named %q, want %q", p.name, tc.wantImage)
				}
			}
			if n := len(editPartsNamed(parts, "mask")); n != 1 {
				t.Fatalf("%d mask parts, want 1", n)
			}
		})
	}
}

// The no-mask edit is exactly the edit it was before masks existed: the same
// fields in the same order, the source as `image`, and no mask part. Spelled out
// byte for byte (the boundary, which the writer picks at random, is normalized) so
// a change to how the body is assembled fails here instead of being compared
// against itself.
func TestBuildImageEditBodyWithoutAMaskIsUnchanged(t *testing.T) {
	source := []byte("SRC-BYTES")
	body, contentType, err := buildImageEditBody(editRunPrep("make it blue"), imageEditInputs{
		sources: []imageEditSource{{mediaType: "image/png", data: source}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		t.Fatalf("Content-Type %q has no boundary", contentType)
	}
	got := strings.ReplaceAll(string(body), params["boundary"], "B")
	want := "--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nsd-turbo\r\n" +
		"--B\r\nContent-Disposition: form-data; name=\"prompt\"\r\n\r\nmake it blue\r\n" +
		"--B\r\nContent-Disposition: form-data; name=\"response_format\"\r\n\r\nb64_json\r\n" +
		"--B\r\nContent-Disposition: form-data; name=\"image\"; filename=\"image.png\"\r\nContent-Type: image/png\r\n\r\nSRC-BYTES\r\n" +
		"--B--\r\n"
	if got != want {
		t.Fatalf("the no-mask edit body changed:\n%q\nwant\n%q", got, want)
	}
}

// The whole loopback: a source PNG and an OpenAI-style alpha mask of the same size
// reach the upstream as `image` and `mask`. The upstream's mask is the gateway's
// luminance conversion of the uploaded one (transparent -> white = edit, opaque ->
// black = keep), which is how the portal's mask gets the polarity sd-server reads.
func TestImageRunMaskedEditReachesTheUpstreamAsImageAndMask(t *testing.T) {
	upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	source := pngOf(t, sourceImage(8, 6))
	mask := pngOf(t, openAIMask(8, 6, 3)) // the left 3 columns are the region to edit

	prep := maskedRunPrep("make it blue",
		[]string{imageDataURL("image/png", source)}, []string{imageDataURL("image/png", mask)})
	run, got, ok := runToTerminal(t, srv, owner, chatID, prep, seen)
	if run.statusValue() != "completed" || !ok {
		t.Fatalf("status = %q (err %q), upstream saw a request: %v", run.statusValue(), runError(t, run), ok)
	}
	if got.path != "/v1/images/edits" {
		t.Fatalf("upstream path = %q, want the edit", got.path)
	}
	parts := parseEditBody(t, got)
	imgs := editImageParts(parts)
	if len(imgs) != 1 || imgs[0].name != "image" || !bytes.Equal(imgs[0].data, source) {
		t.Fatalf("image parts = %+v, want the source alone as `image`, byte for byte", partNames(parts))
	}
	masks := editPartsNamed(parts, "mask")
	if len(masks) != 1 || masks[0].filename != "mask.png" || masks[0].contentType != "image/png" {
		t.Fatalf("mask parts = %+v, want one mask.png", masks)
	}
	decoded, err := png.Decode(bytes.NewReader(masks[0].data))
	if err != nil {
		t.Fatalf("the upstream's mask is not a PNG: %v", err)
	}
	gray, isGray := decoded.(*image.Gray)
	if !isGray {
		t.Fatalf("the upstream's mask is a %T, want the 8-bit luminance PNG sd-server reads", decoded)
	}
	if gray.GrayAt(0, 0).Y != 255 || gray.GrayAt(2, 5).Y != 255 {
		t.Fatalf("the transparent (edit) columns are %d/%d, want white (255)", gray.GrayAt(0, 0).Y, gray.GrayAt(2, 5).Y)
	}
	if gray.GrayAt(3, 0).Y != 0 || gray.GrayAt(7, 5).Y != 0 {
		t.Fatalf("the opaque (keep) columns are %d/%d, want black (0)", gray.GrayAt(3, 0).Y, gray.GrayAt(7, 5).Y)
	}
}

// A mask is optional: the same source without one goes out as it always did, with
// no mask part at all (the edit is the whole-image img2img of before).
func TestImageRunEditWithoutAMaskSendsNoMaskPart(t *testing.T) {
	upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	source := pngOf(t, sourceImage(8, 6))
	run, got, ok := runToTerminal(t, srv, owner, chatID, editRunPrep("make it blue", imageDataURL("image/png", source)), seen)
	if run.statusValue() != "completed" || !ok {
		t.Fatalf("status = %q (err %q), upstream saw a request: %v", run.statusValue(), runError(t, run), ok)
	}
	parts := parseEditBody(t, got)
	if n := len(editPartsNamed(parts, "mask")); n != 0 {
		t.Fatalf("%d mask parts on an edit with no mask", n)
	}
	if imgs := editImageParts(parts); len(imgs) != 1 || !bytes.Equal(imgs[0].data, source) {
		t.Fatal("the source did not arrive as the one image part")
	}
}

// A mask is only ever the mask of an edit: with no source image in the last user
// message there is no edit, and the mask is ignored -- not sent on its own (a lone
// mask is an edit request with nothing to apply it to), not an error. The turn is
// the plain generation it would be without the part, byte for byte, and whatever the
// mask part holds is not even read.
func TestImageRunMaskWithoutASourceIsIgnored(t *testing.T) {
	const wantBody = `{"model":"sd-turbo","prompt":"a cat","response_format":"b64_json"}`
	for _, tc := range []struct{ name, maskURL string }{
		{"a readable mask", imageDataURL("image/png", pngOf(t, openAIMask(8, 6, 3)))},
		{"an unreadable mask", "https://example.com/mask.png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
			srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
			run, got, ok := runToTerminal(t, srv, owner, chatID, maskedRunPrep("a cat", nil, []string{tc.maskURL}), seen)
			if run.statusValue() != "completed" || !ok {
				t.Fatalf("status = %q (err %q), upstream saw a request: %v", run.statusValue(), runError(t, run), ok)
			}
			if got.path != "/v1/images/generations" || got.contentType != "application/json" {
				t.Fatalf("upstream got %s as %q, want the generation as application/json", got.path, got.contentType)
			}
			if string(got.body) != wantBody {
				t.Fatalf("generations body =\n%s\nwant\n%s", got.body, wantBody)
			}
		})
	}
}

// A mask that cannot be sent as one fails the whole run, like an unreadable source
// does, and nothing is sent: the edit without its mask would repaint what the user
// meant to keep, and the mask as a generation would answer a request nobody made.
func TestImageRunWithAnUnusableMaskFailsAndSendsNothing(t *testing.T) {
	source := imageDataURL("image/png", pngOf(t, sourceImage(8, 6)))
	goodMask := imageDataURL("image/png", pngOf(t, openAIMask(8, 6, 3)))
	for _, tc := range []struct {
		name  string
		masks []string
	}{
		{"an external url", []string{"https://example.com/mask.png"}},
		{"an unresolved gateway reference", []string{"/api/portal/chat-blobs/blob_" + strings.Repeat("a", 32)}},
		{"base64 that does not decode", []string{"data:image/png;base64,!!!not-base64!!!"}},
		{"a type outside the allow-list", []string{imageDataURL("text/html", []byte("<script>"))}},
		// The portal's mask is a PNG with an alpha channel; a jpeg or webp cannot carry one.
		{"a jpeg", []string{imageDataURL("image/jpeg", jpegOf(t, sourceImage(8, 6)))}},
		{"a webp", []string{imageDataURL("image/webp", editSourceBytes(60, 3))}},
		// An edit has one mask (OpenAI's single field): a second is not guessed away.
		{"two masks", []string{goodMask, goodMask}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
			srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
			run, _, sent := runToTerminal(t, srv, owner, chatID, maskedRunPrep("x", []string{source}, tc.masks), seen)
			if sent {
				t.Fatal("the upstream saw a request: an unusable mask must send nothing")
			}
			if run.statusValue() != "error" || runError(t, run) != imageRunSourceUnreadableMessage {
				t.Fatalf("status = %q error = %q, want error %q", run.statusValue(), runError(t, run), imageRunSourceUnreadableMessage)
			}
		})
	}
}

// What the gateway's edits endpoint says about the mask reaches the run as the
// endpoint's own code -- the portal labels them (images.mask_invalid,
// images.edit_unsupported_format) -- and, again, nothing reaches the upstream.
func TestImageRunMaskRefusalsReachTheRunAsTheirOwnCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source []byte
		mask   []byte
		want   string
	}{
		{"a mask of another size than the source", pngOf(t, sourceImage(8, 6)), pngOf(t, openAIMask(4, 4, 2)), "images.mask_invalid"},
		{"a mask with nothing transparent in it", pngOf(t, sourceImage(8, 6)), pngOf(t, openAIMask(8, 6, 0)), "images.mask_invalid"},
		{"a mask that is not a PNG after all", pngOf(t, sourceImage(8, 6)), editSourceBytes(200, 4), "images.edit_unsupported_format"},
		{"a source that is neither PNG nor JPEG", editSourceBytes(300, 5), pngOf(t, openAIMask(8, 6, 3)), "images.edit_unsupported_format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
			srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
			prep := maskedRunPrep("x", []string{imageDataURL("image/png", tc.source)}, []string{imageDataURL("image/png", tc.mask)})
			run, _, sent := runToTerminal(t, srv, owner, chatID, prep, seen)
			if sent {
				t.Fatal("the upstream saw a request for a mask the endpoint refused")
			}
			if run.statusValue() != "error" || runError(t, run) != tc.want {
				t.Fatalf("status = %q error = %q, want error %q", run.statusValue(), runError(t, run), tc.want)
			}
		})
	}
}

// The whole path through the real portal service: an upload is the first turn's
// source and mask; their stored copies -- /chat-blobs/{id} references -- are the
// second turn's, and the executor resolves both to the original bytes. The mask is
// reused, not copied, and the transcript never grows inline bytes.
func TestImageRunMaskedEditFromStoredReferences(t *testing.T) {
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
	source := pngOf(t, sourceImage(8, 6))
	mask := pngOf(t, openAIMask(8, 6, 3))

	turn := func(userMessage string) capturedUpstream {
		t.Helper()
		history, settings, err := srv.Portal.PrepareChatRun(ctx, owner, chatID, portal.PrepareRunRequest{
			UserMessage: json.RawMessage(userMessage),
			Settings:    portal.ChatRunSettings{Model: "sd-turbo", Kind: chatRunKindImage},
		})
		if err != nil {
			t.Fatalf("PrepareChatRun: %v", err)
		}
		run, got, ok := runToTerminal(t, srv, owner, chatID, PrepareRunResult{History: history, Settings: settings}, seen)
		if run.statusValue() != "completed" || !ok {
			t.Fatalf("status = %q (err %q), upstream saw a request: %v", run.statusValue(), runError(t, run), ok)
		}
		if got.path != "/v1/images/edits" {
			t.Fatalf("path = %q, want the edit", got.path)
		}
		return got
	}
	wantPair := func(got capturedUpstream) []byte {
		t.Helper()
		parts := parseEditBody(t, got)
		imgs, masks := editImageParts(parts), editPartsNamed(parts, "mask")
		if len(imgs) != 1 || !bytes.Equal(imgs[0].data, source) {
			t.Fatal("the source did not reach the upstream byte for byte")
		}
		if len(masks) != 1 {
			t.Fatalf("%d mask parts, want 1", len(masks))
		}
		return masks[0].data
	}

	// Turn 1: an upload of both.
	first := wantPair(turn(fmt.Sprintf(`[{"type":"text","text":"make it blue"},{"type":"image_url","image_url":{"url":%q}},%s]`,
		imageDataURL("image/png", source), taggedMaskPart(imageDataURL("image/png", mask)))))

	// Turn 2: name both stored copies.
	chat, err := srv.Portal.GetChat(ctx, owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	blobs := storedImageBlobs(t, chats, chat.Content)
	if len(blobs) != 3 {
		t.Fatalf("the transcript references %d blobs after turn 1, want the source, the mask and the result", len(blobs))
	}
	srcRef, maskRef := "/api/portal/chat-blobs/"+blobs[0].ID, "/api/portal/chat-blobs/"+blobs[1].ID
	second := wantPair(turn(fmt.Sprintf(`[{"type":"text","text":"now greener"},{"type":"image_url","image_url":{"url":%q}},%s]`, srcRef, taggedMaskPart(maskRef))))
	if !bytes.Equal(first, second) {
		t.Fatal("the stored mask reached the upstream as different bytes than the uploaded one did")
	}

	chat, err = srv.Portal.GetChat(ctx, owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chat.Content), "data:") {
		t.Fatalf("the transcript gained inline image bytes: %.200s", chat.Content)
	}
	blobs = storedImageBlobs(t, chats, chat.Content)
	if len(blobs) != 6 {
		t.Fatalf("the transcript references %d blobs, want source, mask, result, source, mask, result", len(blobs))
	}
	if blobs[0].ID != blobs[3].ID || blobs[1].ID != blobs[4].ID {
		t.Fatalf("turn 2 uses blobs %s/%s, want turn 1's %s/%s (references, not copies)", blobs[3].ID, blobs[4].ID, blobs[0].ID, blobs[1].ID)
	}
	if n := strings.Count(string(chat.Content), `"purpose":"mask"`); n != 2 {
		t.Fatalf("the transcript carries the mask tag %d times, want once per masked turn", n)
	}
}

// A mask the chat does not hold never reaches the executor, whatever the source: the
// run is refused at its start -- also for a stored transcript's own last message,
// the one nothing vetted at this request -- so there is nothing to send and no
// foreign bytes.
func TestImageRunRefusesAForeignMaskBeforeAnythingIsSent(t *testing.T) {
	upstream, seen := imagesUpstream(t, http.StatusOK, imagesOKBody)
	srv, _, owner, _ := newImageRunTestServerWithChats(t, upstream.URL, store.NewMemoryChatStore(0))
	ctx := context.Background()
	newChat := func(content string) string {
		t.Helper()
		c, err := srv.Portal.CreateChat(ctx, owner, portal.CreateChatRequest{Content: json.RawMessage(content)})
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	// A blob held by ANOTHER chat of the same owner.
	other := newChat(`{"settings":{},"messages":[]}`)
	if _, _, err := srv.Portal.PrepareChatRun(ctx, owner, other, portal.PrepareRunRequest{
		UserMessage: json.RawMessage(fmt.Sprintf(`[{"type":"image_url","image_url":{"url":%q}}]`, imageDataURL("image/png", pngOf(t, openAIMask(8, 6, 3))))),
		Settings:    portal.ChatRunSettings{Model: "sd-turbo", Kind: chatRunKindImage},
	}); err != nil {
		t.Fatal(err)
	}
	otherChat, err := srv.Portal.GetChat(ctx, owner, other)
	if err != nil {
		t.Fatal(err)
	}
	foreign := storedImageURLsOf(t, otherChat.Content)[0]
	source := imageDataURL("image/png", pngOf(t, sourceImage(8, 6)))

	// A new message naming the foreign blob as its mask.
	mine := newChat(`{"settings":{},"messages":[]}`)
	_, _, err = srv.Portal.PrepareChatRun(ctx, owner, mine, portal.PrepareRunRequest{
		UserMessage: json.RawMessage(fmt.Sprintf(`[{"type":"text","text":"steal"},{"type":"image_url","image_url":{"url":%q}},%s]`, source, taggedMaskPart(foreign))),
		Settings:    portal.ChatRunSettings{Model: "sd-turbo", Kind: chatRunKindImage},
	})
	if !errors.Is(err, portal.ErrChatBlobNotOwned) {
		t.Fatalf("new message: err = %v, want portal.ErrChatBlobNotOwned", err)
	}

	// A stored transcript whose last user message already carries it (created with
	// whatever it was given), run with no message of its own.
	grafted := newChat(fmt.Sprintf(`{"settings":{"kind":"image"},"messages":[{"id":"u1","role":"user","content":[{"type":"text","text":"steal"},{"type":"image_url","image_url":{"url":%q}},%s]}]}`, source, taggedMaskPart(foreign)))
	_, _, err = srv.Portal.PrepareChatRun(ctx, owner, grafted, portal.PrepareRunRequest{
		Settings: portal.ChatRunSettings{Model: "sd-turbo", Kind: chatRunKindImage},
	})
	if !errors.Is(err, portal.ErrChatBlobNotOwned) {
		t.Fatalf("stored last message: err = %v, want portal.ErrChatBlobNotOwned", err)
	}
	select {
	case got := <-seen:
		t.Fatalf("the upstream saw a request (%s)", got.path)
	default:
	}
}
