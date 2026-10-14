// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"op-ai-gateway/internal/inference"
	"testing"
)

// pngOf encodes img as a PNG.
func pngOf(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// jpegOf encodes img as a JPEG.
func jpegOf(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// sourceImage is a w x h opaque picture: the init image of an edit.
func sourceImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x * 10), G: uint8(y * 10), B: 200, A: 255})
		}
	}
	return img
}

// openAIMask is an OpenAI-style mask: the columns left of transparentCols are
// fully TRANSPARENT (the region to edit -- carrying a vivid red in RGB, which
// must be irrelevant) and the rest are opaque WHITE (kept -- white in RGB, which
// a luminance-of-RGB conversion would wrongly read as "regenerate").
func openAIMask(w, h, transparentCols int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			if x < transparentCols {
				img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 20, B: 20, A: 0})
			} else {
				img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
			}
		}
	}
	return img
}

// pngHeaderOnly is the first bytes of a PNG that claims to be w x h: signature,
// IHDR (8-bit RGBA) and one IDAT chunk -- everything image/png's DecodeConfig
// reads, and nothing it would need to decode the pixels. It lets a test state a
// size no encoder should be asked to produce.
func pngHeaderOnly(w, h uint32) []byte {
	var buf bytes.Buffer
	buf.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(kind string, data []byte) {
		_ = binary.Write(&buf, binary.BigEndian, uint32(len(data)))
		buf.WriteString(kind)
		buf.Write(data)
		sum := crc32.NewIEEE()
		sum.Write([]byte(kind))
		sum.Write(data)
		_ = binary.Write(&buf, binary.BigEndian, sum.Sum32())
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	chunk("IHDR", ihdr)
	chunk("IDAT", []byte{0})
	return buf.Bytes()
}

// maskForm builds the multipart body of an edit carrying init and mask, and
// returns its boundary and bytes.
func maskForm(t *testing.T, parts ...editPart) (string, []byte) {
	t.Helper()
	body, ct := buildEditMultipart(t, parts)
	return mustBoundary(t, ct), body
}

func initPart(b []byte) editPart {
	return editPart{name: "image", filename: "src.png", contentType: "image/png", body: b}
}

func maskPart(b []byte) editPart {
	return editPart{name: "mask", filename: "mask.png", contentType: "image/png", body: b}
}

// requireImageEditError asserts err is the request refusal with the given code.
func requireImageEditError(t *testing.T, err error, code string) {
	t.Helper()
	var ie *inference.Error
	if !errors.As(err, &ie) || ie.Code != code {
		t.Fatalf("err = %v, want an *inference.Error with code %q", err, code)
	}
}

// The conversion itself: sd reads a mask as LUMINANCE (white = regenerate) and
// drops alpha, while OpenAI's mask says "edit here" with TRANSPARENCY. So a fully
// transparent pixel becomes white, a fully opaque one black -- whatever colour
// either carries -- and a partly transparent one lands in between (a soft edge
// stays soft).
func TestAlphaMaskToLuminance(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 4, 1))
	src.SetNRGBA(0, 0, color.NRGBA{R: 200, G: 10, B: 10, A: 0})     // transparent, vivid red
	src.SetNRGBA(1, 0, color.NRGBA{R: 255, G: 255, B: 255, A: 255}) // opaque, white
	src.SetNRGBA(2, 0, color.NRGBA{R: 0, G: 0, B: 0, A: 128})       // half transparent, black
	src.SetNRGBA(3, 0, color.NRGBA{R: 0, G: 0, B: 0, A: 255})       // opaque, black

	got := alphaMaskToLuminance(src)

	if b := got.Bounds(); b.Dx() != 4 || b.Dy() != 1 {
		t.Fatalf("bounds = %v, want 4x1", b)
	}
	want := []uint8{255, 0, 127, 0}
	for x, w := range want {
		if v := got.GrayAt(x, 0).Y; v != w {
			t.Errorf("pixel %d = %d, want %d", x, v, w)
		}
	}
}

// A PNG decodes to a different image type by colour type and depth, and the
// conversion has a fast path for the common ones and a generic one for the rest:
// every representation of "transparent, half, opaque" must give white, grey, black.
func TestAlphaMaskToLuminanceAcrossImageTypes(t *testing.T) {
	want := []uint8{255, 127, 0}

	nrgba64 := image.NewNRGBA64(image.Rect(0, 0, 3, 1))
	nrgba64.SetNRGBA64(0, 0, color.NRGBA64{R: 0xffff, A: 0})
	nrgba64.SetNRGBA64(1, 0, color.NRGBA64{G: 0xffff, A: 0x8080})
	nrgba64.SetNRGBA64(2, 0, color.NRGBA64{B: 0xffff, A: 0xffff})

	rgba := image.NewRGBA(image.Rect(0, 0, 3, 1)) // premultiplied
	rgba.SetRGBA(0, 0, color.RGBA{A: 0})
	rgba.SetRGBA(1, 0, color.RGBA{G: 64, A: 128})
	rgba.SetRGBA(2, 0, color.RGBA{B: 255, A: 255})

	pal := image.NewPaletted(image.Rect(0, 0, 3, 1), color.Palette{
		color.NRGBA{R: 9, A: 0}, color.NRGBA{G: 9, A: 128}, color.NRGBA{B: 9, A: 255},
	})
	pal.SetColorIndex(0, 0, 0)
	pal.SetColorIndex(1, 0, 1)
	pal.SetColorIndex(2, 0, 2)

	// A sub-image: its pixels do not start at the buffer's origin.
	wide := image.NewNRGBA(image.Rect(0, 0, 5, 3))
	wide.SetNRGBA(2, 1, color.NRGBA{A: 0})
	wide.SetNRGBA(3, 1, color.NRGBA{A: 128})
	wide.SetNRGBA(4, 1, color.NRGBA{A: 255})
	sub := wide.SubImage(image.Rect(2, 1, 5, 2))

	for name, img := range map[string]image.Image{"NRGBA64": nrgba64, "RGBA": rgba, "Paletted": pal, "sub-image": sub} {
		t.Run(name, func(t *testing.T) {
			got := alphaMaskToLuminance(img)
			if b := got.Bounds(); b.Dx() != 3 || b.Dy() != 1 {
				t.Fatalf("bounds = %v, want 3x1", b)
			}
			for x, w := range want {
				if v := got.GrayAt(x, 0).Y; v != w {
					t.Errorf("pixel %d = %d, want %d", x, v, w)
				}
			}
		})
	}

	// No alpha channel at all: every pixel opaque, so black.
	gray := image.NewGray(image.Rect(0, 0, 2, 1))
	gray.SetGray(0, 0, color.Gray{Y: 255})
	if g := alphaMaskToLuminance(gray); g.GrayAt(0, 0).Y != 0 || g.GrayAt(1, 0).Y != 0 {
		t.Fatalf("a grayscale (no alpha) mask must convert to all black, got %v", g.Pix)
	}
}

// The mask in the form is replaced by an sd luminance mask: decoded back, the
// transparent columns are white and the opaque ones black, at the init image's
// size.
func TestConvertImageEditMask(t *testing.T) {
	boundary, raw := maskForm(t,
		editPart{name: "model", body: []byte("sd-turbo")},
		editPart{name: "prompt", body: []byte("a cat")},
		initPart(pngOf(t, sourceImage(8, 6))),
		maskPart(pngOf(t, openAIMask(8, 6, 3))),
	)

	out, err := convertImageEditMask(boundary, raw)
	if err != nil {
		t.Fatalf("convertImageEditMask: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("the converted mask is not a PNG: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != 8 || b.Dy() != 6 {
		t.Fatalf("converted mask is %v, want 8x6", b)
	}
	gray, ok := decoded.(*image.Gray)
	if !ok {
		t.Fatalf("converted mask is a %T, want a single-channel grayscale image (sd reads luminance)", decoded)
	}
	for y := range 6 {
		for x := range 8 {
			want := uint8(0)
			if x < 3 {
				want = 255
			}
			if v := gray.GrayAt(x, y).Y; v != want {
				t.Fatalf("pixel (%d,%d) = %d, want %d (transparent -> white, opaque -> black)", x, y, v, want)
			}
		}
	}
}

// What counts as "the init image" and where the mask sits in the form must not
// matter: a mask that arrives BEFORE the image, a JPEG source, and the first of
// several `image[]` parts (a mask applies to the first image; the extra ones are
// references and may be any size).
func TestConvertImageEditMaskFindsTheSourceWhereverItIs(t *testing.T) {
	cases := []struct {
		name  string
		parts []editPart
	}{
		{"mask before the image", []editPart{maskPart(pngOf(t, openAIMask(8, 6, 3))), initPart(pngOf(t, sourceImage(8, 6)))}},
		{"jpeg source", []editPart{initPart(jpegOf(t, sourceImage(8, 6))), maskPart(pngOf(t, openAIMask(8, 6, 3)))}},
		{"first of several image[] parts", []editPart{
			{name: "image[]", filename: "a.png", contentType: "image/png", body: pngOf(t, sourceImage(8, 6))},
			{name: "image[]", filename: "b.png", contentType: "image/png", body: pngOf(t, sourceImage(20, 20))},
			maskPart(pngOf(t, openAIMask(8, 6, 3))),
		}},
		// sd-server takes its init image from `image[]` when there is one and only
		// otherwise from `image`: the mask is checked against the one it will use.
		{"image[] is the source even when an image part came first", []editPart{
			initPart(pngOf(t, sourceImage(20, 20))),
			{name: "image[]", filename: "a.png", contentType: "image/png", body: pngOf(t, sourceImage(8, 6))},
			maskPart(pngOf(t, openAIMask(8, 6, 3))),
		}},
		{"an empty image part does not count as the first", []editPart{
			{name: "image", filename: "empty.png", contentType: "image/png"},
			initPart(pngOf(t, sourceImage(8, 6))),
			maskPart(pngOf(t, openAIMask(8, 6, 3))),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boundary, raw := maskForm(t, tc.parts...)

			if _, err := convertImageEditMask(boundary, raw); err != nil {
				t.Fatalf("convertImageEditMask: %v", err)
			}
		})
	}
}

// Every way the mask can be unusable is a 400 with a code of its own -- the size
// disagreeing with the source's is images.mask_invalid, a mask or source the
// conversion cannot read is images.edit_unsupported_format.
func TestConvertImageEditMaskRefusals(t *testing.T) {
	goodInit := pngOf(t, sourceImage(8, 6))
	goodMask := pngOf(t, openAIMask(8, 6, 3))
	cases := []struct {
		name  string
		parts []editPart
		code  string
	}{
		{"mask wider than the source", []editPart{initPart(goodInit), maskPart(pngOf(t, openAIMask(9, 6, 3)))}, "images.mask_invalid"},
		{"mask taller than the source", []editPart{initPart(goodInit), maskPart(pngOf(t, openAIMask(8, 7, 3)))}, "images.mask_invalid"},
		{"mask swapped width and height", []editPart{initPart(goodInit), maskPart(pngOf(t, openAIMask(6, 8, 3)))}, "images.mask_invalid"},
		{"mask against the FIRST of several images", []editPart{
			{name: "image[]", filename: "a.png", contentType: "image/png", body: pngOf(t, sourceImage(20, 20))},
			{name: "image[]", filename: "b.png", contentType: "image/png", body: goodInit},
			maskPart(goodMask),
		}, "images.mask_invalid"},
		{"mask against the image[] source, not the image part that came first", []editPart{
			initPart(goodInit),
			{name: "image[]", filename: "a.png", contentType: "image/png", body: pngOf(t, sourceImage(20, 20))},
			maskPart(goodMask),
		}, "images.mask_invalid"},
		{"a mask with no transparent pixel selects nothing", []editPart{initPart(goodInit), maskPart(pngOf(t, openAIMask(8, 6, 0)))}, "images.mask_invalid"},
		{"a mask with no alpha channel at all", []editPart{initPart(goodInit), maskPart(pngOf(t, image.NewGray(image.Rect(0, 0, 8, 6))))}, "images.mask_invalid"},
		{"a mask past the pixel cap is refused from its header", []editPart{initPart(pngHeaderOnly(70000, 70000)), maskPart(pngHeaderOnly(70000, 70000))}, "images.mask_invalid"},
		{"a mask that is a JPEG", []editPart{initPart(goodInit), maskPart(jpegOf(t, sourceImage(8, 6)))}, "images.edit_unsupported_format"},
		{"a mask that is not an image", []editPart{initPart(goodInit), maskPart([]byte("not an image at all"))}, "images.edit_unsupported_format"},
		{"a mask whose pixel data is cut off", []editPart{initPart(goodInit), maskPart(goodMask[:len(goodMask)-30])}, "images.edit_unsupported_format"},
		{"a source that is not an image", []editPart{initPart([]byte("not an image either")), maskPart(goodMask)}, "images.edit_unsupported_format"},
		{"a source in a format the edit route does not take", []editPart{initPart([]byte("RIFF\x00\x00\x00\x00WEBPVP8 ")), maskPart(goodMask)}, "images.edit_unsupported_format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boundary, raw := maskForm(t, tc.parts...)

			out, err := convertImageEditMask(boundary, raw)

			requireImageEditError(t, err, tc.code)
			if out != nil {
				t.Fatalf("a refused mask still produced %d bytes", len(out))
			}
		})
	}
}

// parseImageEditForm tells the handler whether a mask came, and refuses a form
// that names two (which one is "the" mask is not a question to guess at). An
// empty mask part is "no mask", as an empty image part is "no image".
func TestParseImageEditFormMask(t *testing.T) {
	base := validEditParts("sd-turbo")
	t.Run("no mask", func(t *testing.T) {
		body, ct := buildEditMultipart(t, base)
		form, err := parseImageEditForm(ct, body)
		if err != nil || form.masks != 0 {
			t.Fatalf("err = %v, masks = %d, want none", err, form.masks)
		}
	})
	t.Run("one mask", func(t *testing.T) {
		body, ct := buildEditMultipart(t, append(append([]editPart(nil), base...), maskPart([]byte("m"))))
		form, err := parseImageEditForm(ct, body)
		if err != nil || form.masks != 1 {
			t.Fatalf("err = %v, masks = %d, want 1", err, form.masks)
		}
	})
	t.Run("an empty mask part is no mask", func(t *testing.T) {
		body, ct := buildEditMultipart(t, append(append([]editPart(nil), base...), maskPart(nil)))
		form, err := parseImageEditForm(ct, body)
		if err != nil || form.masks != 0 {
			t.Fatalf("err = %v, masks = %d, want none", err, form.masks)
		}
	})
	t.Run("two masks are refused", func(t *testing.T) {
		body, ct := buildEditMultipart(t, append(append([]editPart(nil), base...), maskPart([]byte("a")), maskPart([]byte("b"))))
		_, err := parseImageEditForm(ct, body)
		requireImageEditError(t, err, "request.invalid")
	})
}

// The mask edit, end to end against an external sd-server: the upstream receives
// a REBUILT body in which the mask is the luminance conversion and everything
// else -- the model, the prompt, the source image's bytes and headers, the part
// order -- is what the client sent, under a boundary of its own.
func TestImagesEditConvertsTheMaskForAnExternalTarget(t *testing.T) {
	up, got := newEditUpstream(t, http.StatusOK, editsOKBody)
	srv := newImageCapableTestServer(t, up.URL)
	source := pngOf(t, sourceImage(8, 6))
	clientMask := pngOf(t, openAIMask(8, 6, 3))
	parts := []editPart{
		{name: "model", body: []byte("sd-turbo")},
		initPart(source),
		maskPart(clientMask),
		{name: "prompt", body: []byte("a cat in a hat")},
		{name: "n", body: []byte("1")},
	}
	body, ct := buildEditMultipart(t, parts)

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	calls, _, path, upstreamCT, upstreamBody := got.snapshot()
	if calls != 1 || path != "/v1/images/edits" {
		t.Fatalf("upstream calls = %d, path = %q, want one call to /v1/images/edits", calls, path)
	}
	if upstreamCT == ct {
		t.Fatalf("the upstream got the client's Content-Type %q: a mask edit must be rebuilt, with a boundary of its own", ct)
	}
	sent := readEditParts(t, upstreamBody, upstreamCT)
	if len(sent) != len(parts) {
		t.Fatalf("upstream form has %d parts, want %d", len(sent), len(parts))
	}
	for i, want := range parts {
		if sent[i].name != want.name {
			t.Fatalf("part %d is %q, want %q (order must be kept)", i, sent[i].name, want.name)
		}
		if want.name == "mask" {
			continue
		}
		if sent[i].filename != want.filename || sent[i].contentType != want.contentType || !bytes.Equal(sent[i].body, want.body) {
			t.Fatalf("part %d (%s) was altered: %+v, want %+v", i, want.name, sent[i], want)
		}
	}
	mask := sent[2]
	if mask.filename == "" || mask.contentType != "image/png" {
		t.Fatalf("the converted mask part = filename %q type %q, want a named image/png file (sd-server reads a mask from a file part)", mask.filename, mask.contentType)
	}
	decoded, err := png.Decode(bytes.NewReader(mask.body))
	if err != nil {
		t.Fatalf("the relayed mask is not a PNG: %v", err)
	}
	gray, ok := decoded.(*image.Gray)
	if !ok || gray.GrayAt(0, 0).Y != 255 || gray.GrayAt(7, 5).Y != 0 {
		t.Fatalf("the relayed mask (%T) is not the luminance conversion: transparent corner must be white, opaque corner black", decoded)
	}
	if bytes.Equal(mask.body, clientMask) {
		t.Fatal("the client's alpha mask was relayed unconverted")
	}
}

// A mask edit to an agent-launched target is rebuilt ONCE, doing both jobs: the
// mask is converted and the model field is the name the agent's router keys its
// child on (the rebuild must not lose the model field -- without it the router
// answers 404 and no child starts).
func TestImagesEditConvertsTheMaskAndRewritesTheModelInOneRebuild(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newAgentEditTestServer(t, prov, "flux-gateway-name", "flux1-dev-q8")
	body, ct := buildEditMultipart(t, []editPart{
		{name: "model", body: []byte("flux-gateway-name")},
		{name: "prompt", body: []byte("a cat")},
		initPart(pngOf(t, sourceImage(8, 6))),
		maskPart(pngOf(t, openAIMask(8, 6, 3))),
	})

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var model string
	var converted bool
	for _, p := range readEditParts(t, prov.gotBody, prov.gotContentType) {
		switch p.name {
		case "model":
			model = string(p.body)
		case "mask":
			img, err := png.Decode(bytes.NewReader(p.body))
			g, isGray := img.(*image.Gray)
			converted = err == nil && isGray && g.GrayAt(0, 0).Y == 255
		}
	}
	if model != "flux1-dev-q8" {
		t.Fatalf("relayed model = %q, want the app name flux1-dev-q8", model)
	}
	if !converted {
		t.Fatal("the relayed mask is not the luminance conversion")
	}
}

// Even when the model needs no rewrite (an agent whose names are equal and a form
// that already carries the app name), a mask edit is rebuilt -- and the model
// field survives the rebuild untouched.
func TestImagesEditMaskRebuildPreservesTheModelFieldWhenNoRewriteIsNeeded(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newAgentEditTestServer(t, prov, "flux1-dev", "flux1-dev")
	body, ct := buildEditMultipart(t, []editPart{
		{name: "model", body: []byte("flux1-dev")},
		{name: "prompt", body: []byte("a cat")},
		initPart(pngOf(t, sourceImage(8, 6))),
		maskPart(pngOf(t, openAIMask(8, 6, 3))),
	})

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if prov.gotContentType == ct {
		t.Fatal("the body was relayed verbatim: a mask edit must carry the converted mask")
	}
	var model string
	for _, p := range readEditParts(t, prov.gotBody, prov.gotContentType) {
		if p.name == "model" {
			model = string(p.body)
		}
	}
	if model != "flux1-dev" {
		t.Fatalf("relayed model = %q, want flux1-dev preserved", model)
	}
}

// An edit WITHOUT a mask keeps the verbatim/rebuild split it had: nothing is
// decoded, an external target with nothing to rewrite gets the client's bytes
// (the source here is not even a decodable image -- the gateway does not look).
func TestImagesEditWithoutAMaskIsNotDecodedOrRebuilt(t *testing.T) {
	up, got := newEditUpstream(t, http.StatusOK, editsOKBody)
	srv := newImageCapableTestServer(t, up.URL)
	body, ct := buildEditMultipart(t, validEditParts("sd-turbo")) // editTestPNG is not a PNG

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if _, _, _, upstreamCT, upstreamBody := got.snapshot(); upstreamCT != ct || !bytes.Equal(upstreamBody, body) {
		t.Fatal("a no-mask external edit was rebuilt; it must be relayed verbatim")
	}
}

// The mask refusals are answered BEFORE the gate like every other validation
// refusal: 400, no upstream call, no usage row. (They also join
// invalidEditCases, which TestImagesEditValidationRunsBeforeAdmission replays
// against a limiter of one request per minute.)
func TestImagesEditRefusesAnUnusableMask(t *testing.T) {
	for _, tc := range maskEditCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: editsOKBody}
			srv := newImageCapableTestServerWithProvider(t, prov)

			rec := postImageEdit(t, srv, tc.parts)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), tc.code)
			if prov.proxyCalls != 0 {
				t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
			}
			if n := len(srv.Usage.All()); n != 0 {
				t.Fatalf("usage rows = %d, want 0 for a request refused before routing", n)
			}
		})
	}
}

// maskEditCases are mask requests the handler must refuse, as invalidEditCases.
func maskEditCases(t *testing.T) []invalidEditCase {
	t.Helper()
	source := pngOf(t, sourceImage(8, 6))
	goodMask := pngOf(t, openAIMask(8, 6, 3))
	form := func(init, mask []byte) []editPart {
		return []editPart{
			{name: "model", body: []byte("sd-turbo")},
			{name: "prompt", body: []byte("a cat")},
			initPart(init),
			maskPart(mask),
		}
	}
	return []invalidEditCase{
		{"mask of another size", form(source, pngOf(t, openAIMask(9, 6, 3))), "images.mask_invalid"},
		{"mask that is not a PNG", form(source, []byte("not a png")), "images.edit_unsupported_format"},
		{"two masks", append(form(source, goodMask), maskPart([]byte("second"))), "request.invalid"},
	}
}

// The mask edit goes through the same routing as any edit: an unknown mapped
// model (a text model) is still the capability refusal, and the conversion's
// cost is only paid for requests that are otherwise valid.
func TestImagesEditMaskDoesNotBypassTheCapabilityGate(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newImageCapableTestServerWithProvider(t, prov)
	body, ct := buildEditMultipart(t, []editPart{
		{name: "model", body: []byte("qwen-coder")},
		{name: "prompt", body: []byte("a cat")},
		initPart(pngOf(t, sourceImage(8, 6))),
		maskPart(pngOf(t, openAIMask(8, 6, 3))),
	})

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "routing.model_not_capable")
	if prov.proxyCalls != 0 {
		t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
	}
}
