/*
Copyright 2026 The pdfcpu Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package render

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"

	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

// Options controls page rasterization.
type Options struct {
	// DPI is the resolution in dots per inch. Defaults to 150.
	DPI float64
	// Format selects the encoded output format: "png" (default) or "jpg".
	Format string
	// JPEGQuality is the encoder quality for jpg output (1..100, default 85).
	JPEGQuality int
	// MaxPixels caps the resulting pixel count per page (0: use the
	// context resource limits).
	MaxPixels int64
}

func (o *Options) ensureDefaults() {
	if o == nil {
		return
	}
	if o.DPI <= 0 {
		o.DPI = 150
	}
	if o.Format == "" {
		o.Format = "png"
	}
	if o.JPEGQuality <= 0 || o.JPEGQuality > 100 {
		o.JPEGQuality = 85
	}
}

// NormalizeFormat validates and expands format aliases.
func NormalizeFormat(f string) (string, error) {
	switch f {
	case "", "png":
		return "png", nil
	case "jpg", "jpeg":
		return "jpg", nil
	}
	return "", fmt.Errorf("unsupported image format %q: want png or jpg", f)
}

// Result is the outcome of rendering one page.
type Result struct {
	Image    *image.RGBA
	WidthPt  float64
	HeightPt float64
	PageNr   int
	Warnings []string
}

// ErrNoContent is reported when a page has no content stream.
var ErrNoContent = errors.New("page: no content")

// Page rasterizes a single page of ctx and supports cancellation.
func Page(c context.Context, ctx *model.Context, pageNr int, o *Options) (*Result, error) {
	if o == nil {
		o = &Options{}
	}
	o.ensureDefaults()
	if ctx == nil || ctx.XRefTable == nil {
		return nil, model.ErrMissingXRefTable
	}

	pageDict, _, inhPAttrs, err := ctx.PageDict(c, pageNr, false)
	if err != nil {
		return nil, fmt.Errorf("render page %d: %w", pageNr, err)
	}

	box := pageRenderBox(inhPAttrs)

	s := o.DPI / 72
	userUnit := 1.0
	if uu, _, err := ctx.DereferenceNumberEntry(pageDict, "UserUnit"); err == nil && uu != nil && *uu > 0 {
		userUnit = *uu
	}
	s *= userUnit

	wp := box.Width()
	hp := box.Height()
	wRaw := int(math.Ceil(wp * s))
	hRaw := int(math.Ceil(hp * s))
	rotate := inhPAttrs.Rotate % 360
	if rotate < 0 {
		rotate += 360
	}
	swapped := rotate == 90 || rotate == 270
	w, h := wRaw, hRaw
	if swapped {
		w, h = hRaw, wRaw
	}
	if w < 1 || h < 1 {
		return nil, fmt.Errorf("render page %d: invalid pixel dimensions %dx%d", pageNr, w, h)
	}
	maxPixels := o.MaxPixels
	if maxPixels <= 0 {
		maxPixels = model.DefaultResourceLimits().MaxImagePixels
		if ctx.Configuration != nil {
			maxPixels = ctx.Configuration.Limits.MaxImagePixels
		}
	}
	if int64(w)*int64(h) > maxPixels {
		return nil, fmt.Errorf("render page %d: %dx%d pixels exceeds limit of %d (lower the DPI)", pageNr, w, h, maxPixels)
	}

	// User space -> unrotated pixel space (y down).
	m := mat{s, 0, 0, -s, -box.LL.X*s, box.UR.Y*s}
	// Rotation applied to the pixel bitmap (clockwise display rotation).
	var rot mat
	switch rotate {
	case 90:
		rot = mat{0, 1, -1, 0, float64(hRaw), 0}
	case 180:
		rot = mat{-1, 0, 0, -1, float64(wRaw), float64(hRaw)}
	case 270:
		rot = mat{0, -1, 1, 0, 0, float64(wRaw)}
	default:
		rot = ident()
	}
	base := rot.mul(m)

	cv := newCanvas(w, h)
	cv.fillWhite()

	res, _ := pageDict.Find("Resources")
	if res == nil && inhPAttrs.Resources != nil {
		res = inhPAttrs.Resources
	}
	if rd, err := ctx.DereferenceDict(res); err == nil && rd != nil {
		res = rd
	} else if res != nil {
		res = nil
	}

	dev := newDevice(c, ctx, cv, base)
	if rd, ok := res.(types.Dict); ok {
		dev.res = rd
	}

	bb, err := ctx.PageContent(pageDict, pageNr)
	if err != nil {
		if errors.Is(err, model.ErrNoContent) {
			return &Result{Image: cv.img, WidthPt: wp, HeightPt: hp, PageNr: pageNr}, nil
		}
		return nil, fmt.Errorf("render page %d: content: %w", pageNr, err)
	}
	if err := dev.execContent(bb); err != nil {
		return nil, fmt.Errorf("render page %d: %w", pageNr, err)
	}

	return &Result{
		Image:    cv.img,
		WidthPt:  wp,
		HeightPt: hp,
		PageNr:   pageNr,
		Warnings: dev.warnings,
	}, nil
}

// pageRenderBox returns the effective visible box of a page.
func pageRenderBox(inhPAttrs *model.InheritedPageAttrs) *types.Rectangle {
	box := inhPAttrs.MediaBox
	if inhPAttrs.CropBox != nil {
		box = inhPAttrs.CropBox
	}
	if box == nil {
		box = types.RectForDim(612, 792)
	}
	return box
}

// Encode writes img to w using the given format ("png" or "jpg").
func Encode(w io.Writer, img image.Image, format string, quality int) error {
	switch format {
	case "", "png":
		return png.Encode(w, img)
	case "jpg", "jpeg":
		if quality <= 0 || quality > 100 {
			quality = 85
		}
		return jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
	}
	return fmt.Errorf("unsupported image format %q", format)
}

// PageEncoded rasterizes one page and writes it to w in the configured
// format.
func PageEncoded(c context.Context, ctx *model.Context, pageNr int, o *Options, w io.Writer) error {
	res, err := Page(c, ctx, pageNr, o)
	if err != nil {
		return err
	}
	format, err := NormalizeFormat(o.Format)
	if err != nil {
		return err
	}
	quality := 85
	if o != nil {
		quality = o.JPEGQuality
	}
	return Encode(w, res.Image, format, quality)
}
