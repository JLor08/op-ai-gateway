// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build ignore

package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
)

type spec struct {
	name  string
	left  color.RGBA
	right color.RGBA
}

func main() {
	specs := []spec{
		{"red_blue.png", color.RGBA{220, 30, 30, 255}, color.RGBA{30, 60, 220, 255}},
		{"green_yellow.png", color.RGBA{30, 180, 60, 255}, color.RGBA{235, 215, 30, 255}},
		{"purple_orange.png", color.RGBA{150, 40, 190, 255}, color.RGBA{240, 140, 20, 255}},
		{"black_white.png", color.RGBA{10, 10, 10, 255}, color.RGBA{245, 245, 245, 255}},
	}
	const w, h = 256, 128
	for _, s := range specs {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				if x < w/2 {
					img.Set(x, y, s.left)
				} else {
					img.Set(x, y, s.right)
				}
			}
		}
		f, err := os.Create(s.name)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(f, img); err != nil {
			panic(err)
		}
		_ = f.Close()
	}
}
