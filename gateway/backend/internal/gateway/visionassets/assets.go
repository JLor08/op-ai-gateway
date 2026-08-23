// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package visionassets holds the embedded test images used by the vision
// benchmark. Each image has known, describable content (two color halves); the
// verify-mode probe checks the model's answer contains BOTH color words.
package visionassets

import (
	_ "embed"
)

//go:embed red_blue.png
var redBlue []byte

//go:embed green_yellow.png
var greenYellow []byte

//go:embed purple_orange.png
var purpleOrange []byte

//go:embed black_white.png
var blackWhite []byte

// Image is one probe asset: its PNG bytes and the tokens (lowercase) the model's
// answer must contain to count as vision-capable in verify mode.
type Image struct {
	PNG    []byte
	Tokens []string
}

// All returns the embedded probe images.
func All() []Image {
	return []Image{
		{PNG: redBlue, Tokens: []string{"red", "blue"}},
		{PNG: greenYellow, Tokens: []string{"green", "yellow"}},
		{PNG: purpleOrange, Tokens: []string{"purple", "orange"}},
		{PNG: blackWhite, Tokens: []string{"black", "white"}},
	}
}
