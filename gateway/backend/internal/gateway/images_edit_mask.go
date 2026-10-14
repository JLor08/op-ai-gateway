// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // registers the JPEG decoder image.DecodeConfig sniffs the source image with
	"image/png"
	"io"
	"mime/multipart"
	"op-ai-gateway/internal/inference"
)

// An OpenAI image edit names the region to edit with the TRANSPARENCY of a mask:
// the transparent pixels of an RGBA PNG are the ones to regenerate. The sd-server
// this endpoint relays to reads the `mask` form field as LUMINANCE -- white means
// regenerate, black means keep -- and drops the alpha channel, so a mask relayed
// as the client sent it edits the wrong region (everything that is OPAQUE, which
// is usually the picture's content). The gateway therefore converts the mask on
// the way through, which is what makes the public endpoint OpenAI-compatible
// instead of sd-server-compatible, and what lets the portal's own mask UI emit an
// OpenAI-style mask through the same conversion.

const (
	// maxImageEditMaskPixels bounds the mask the conversion will DECODE. A PNG is
	// a few bytes of header for any number of pixels, so the request body cap does
	// not bound the memory a decode takes: a 70000 x 70000 all-transparent mask is
	// a few hundred KB on the wire and tens of GB as pixels. The cap is read off
	// the header, before any pixel is allocated. 16 Mpixels (4096 x 4096) is the
	// largest image sd-server itself generates (its capabilities document limits
	// the edge to 4096).
	//
	// What one conversion of a mask at the cap allocates, measured at 4096 x 4096
	// (convertImageEditMask end to end): about 81 MiB in total for an 8-bit RGBA
	// mask (64 MiB of decoded pixels and the 16 MiB luminance plane they are
	// converted into), and about 273 MiB for a 16-bit RGBA one -- 128 MiB of
	// decoded pixels and the same 16 MiB plane live at once, and another ~128 MiB
	// of short-lived garbage, one boxed color.Color per pixel, because
	// alphaMaskToLuminance has no fast path for NRGBA64. An all-transparent 16-bit
	// mask of that size is about 180 KiB on the wire. The cap bounds ONE
	// conversion, not how many run at once: that worst case is per concurrent
	// request.
	maxImageEditMaskPixels = 1 << 24

	// imageEditMaskFilename names the converted mask's file part: sd-server reads
	// the mask from a FILE part (one with a filename), and the name the client
	// gave -- possibly non-ASCII, which a part header can only carry in an
	// encoding sd-server's parser may not read -- no longer describes what the
	// part holds.
	imageEditMaskFilename = "mask.png"
)

// sourceProbe is what convertImageEditMask learns about one spelling's first
// source image: whether there was one, and whether its header could be read.
type sourceProbe struct {
	cfg      image.Config
	seen     bool
	readable bool
}

func invalidImageEditMask(message string) error {
	return &inference.Error{Code: imagesMaskInvalid, Message: message}
}

func unsupportedImageEditFormat(message string) error {
	return &inference.Error{Code: imagesEditUnsupportedFormat, Message: message}
}

// convertImageEditMask returns the sd-server luminance mask (an 8-bit grayscale
// PNG, transparent -> white, opaque -> black) for the OpenAI-style mask in the
// multipart body raw, or nil when the form carries none. It checks, from the
// image headers, that the mask is a PNG within the pixel cap and the same size
// as the FIRST source image (a mask applies to the first image; further `image[]`
// parts are references and may be any size; where a form has both spellings the
// first `image[]` part is the source, as sd-server picks it) BEFORE it decodes a
// pixel, and it refuses with a code of its own each way the mask cannot be used:
//
//   - images.mask_invalid: a size that is not the source's, a size past the pixel
//     cap, or no transparent pixel at all (nothing selected to edit -- a mask
//     with no alpha channel, or an sd-style luminance mask sent to the OpenAI
//     endpoint, ends up here rather than as a silent no-op edit);
//   - images.edit_unsupported_format: a mask that is not a readable PNG, or a
//     source that is not a readable PNG or JPEG (what sd-server's edit route
//     takes).
//
// It reads raw with the boundary parseImageEditForm validated the body against.
func convertImageEditMask(boundary string, raw []byte) ([]byte, error) {
	mr := multipart.NewReader(bytes.NewReader(raw), boundary)
	// The first non-empty part under each spelling of the source image. sd-server
	// takes its init image from the `image[]` parts when there are any and only
	// otherwise from `image`, so that is the order the two are consulted in here.
	var bracket, plain sourceProbe
	var maskBytes []byte
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, invalidImageEdit("malformed multipart body")
		}
		switch name := part.FormName(); name {
		case "image", "image[]":
			probe := &plain
			if name == "image[]" {
				probe = &bracket
			}
			br := bufio.NewReader(part)
			if _, err := br.Peek(1); err != nil {
				if errors.Is(err, io.EOF) {
					continue // an empty part is not the source image
				}
				return nil, invalidImageEdit("malformed multipart body")
			}
			if !probe.seen {
				probe.seen = true
				if cfg, _, err := image.DecodeConfig(br); err == nil {
					probe.cfg, probe.readable = cfg, true
				}
			}
			if _, err := io.Copy(io.Discard, br); err != nil {
				return nil, invalidImageEdit("malformed multipart body")
			}
		case "mask":
			b, err := io.ReadAll(part)
			if err != nil {
				return nil, invalidImageEdit("malformed multipart body")
			}
			maskBytes = b
		}
	}
	if len(maskBytes) == 0 {
		return nil, nil
	}

	maskCfg, err := png.DecodeConfig(bytes.NewReader(maskBytes))
	if err != nil {
		return nil, unsupportedImageEditFormat("mask must be a PNG image whose transparent pixels mark the region to edit")
	}
	if pixels := int64(maskCfg.Width) * int64(maskCfg.Height); pixels > maxImageEditMaskPixels {
		return nil, invalidImageEditMask(fmt.Sprintf("mask is %dx%d pixels, more than the supported maximum of %d megapixels", maskCfg.Width, maskCfg.Height, maxImageEditMaskPixels>>20))
	}
	source := plain
	if bracket.seen {
		source = bracket
	}
	if !source.seen || !source.readable {
		return nil, unsupportedImageEditFormat("with a mask, the source image must be a PNG or JPEG file")
	}
	if maskCfg.Width != source.cfg.Width || maskCfg.Height != source.cfg.Height {
		return nil, invalidImageEditMask(fmt.Sprintf("mask is %dx%d pixels but the image is %dx%d; they must be the same size", maskCfg.Width, maskCfg.Height, source.cfg.Width, source.cfg.Height))
	}

	decoded, err := png.Decode(bytes.NewReader(maskBytes))
	if err != nil {
		return nil, unsupportedImageEditFormat("mask could not be decoded as a PNG image")
	}
	luminance := alphaMaskToLuminance(decoded)
	if !hasEditRegion(luminance) {
		return nil, invalidImageEditMask("mask has no transparent pixel: the transparent area of the mask marks the region to edit")
	}
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&out, luminance); err != nil {
		return nil, fmt.Errorf("encode converted mask: %w", err)
	}
	return out.Bytes(), nil
}

// alphaMaskToLuminance maps an OpenAI-style mask onto sd-server's: a pixel's
// luminance is 255 minus its alpha, so a fully transparent pixel is white
// (regenerate), a fully opaque one black (keep) whatever colour either carries,
// and a partly transparent one lands in between -- a soft edge stays soft.
func alphaMaskToLuminance(img image.Image) *image.Gray {
	b := img.Bounds()
	out := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	switch src := img.(type) {
	case *image.NRGBA:
		// The type a PNG with an alpha channel decodes to (gray+alpha included):
		// the pixel loop of a 16-megapixel mask must not go through At and
		// color.Color per pixel.
		for y := 0; y < b.Dy(); y++ {
			row := src.Pix[src.PixOffset(b.Min.X, b.Min.Y+y):]
			dst := out.Pix[y*out.Stride:]
			for x := 0; x < b.Dx(); x++ {
				dst[x] = 255 - row[x*4+3]
			}
		}
	case *image.RGBA:
		for y := 0; y < b.Dy(); y++ {
			row := src.Pix[src.PixOffset(b.Min.X, b.Min.Y+y):]
			dst := out.Pix[y*out.Stride:]
			for x := 0; x < b.Dx(); x++ {
				dst[x] = 255 - row[x*4+3]
			}
		}
	case *image.Gray, *image.Gray16:
		// No alpha channel: everything is opaque, so every pixel is black.
	default:
		for y := 0; y < b.Dy(); y++ {
			for x := 0; x < b.Dx(); x++ {
				_, _, _, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				out.Pix[y*out.Stride+x] = 255 - uint8(a>>8)
			}
		}
	}
	return out
}

// hasEditRegion reports whether any pixel of the converted mask is not black,
// that is whether the client's mask had any transparency to edit.
func hasEditRegion(g *image.Gray) bool {
	for y := 0; y < g.Bounds().Dy(); y++ {
		for _, v := range g.Pix[y*g.Stride : y*g.Stride+g.Bounds().Dx()] {
			if v != 0 {
				return true
			}
		}
	}
	return false
}
