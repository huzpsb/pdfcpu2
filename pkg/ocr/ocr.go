// Copyright 2026 The pdfcpu Authors.
// Use of this source code is governed by an Apache-2.0 license that can be found in the LICENSE file.

package ocr

import (
	"fmt"
	"image"
	"image/draw"
)

// Recognize detects and recognizes text lines in img.
func Recognize(img image.Image) ([]Line, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("ocr: empty image")
	}
	rgb := imageToRGB(img)
	return RecognizeRGB(rgb, w, h)
}

// imageToRGB converts any image to tightly packed RGB8 (top-down).
func imageToRGB(img image.Image) []uint8 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
	out := make([]uint8, w*h*3)
	j := 0
	for i := 0; i < len(dst.Pix); i += 4 {
		out[j] = dst.Pix[i]
		out[j+1] = dst.Pix[i+1]
		out[j+2] = dst.Pix[i+2]
		j += 3
	}
	return out
}
