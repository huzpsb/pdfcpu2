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
	"image"

	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

// renderSoftMask renders an ExtGState /SMask entry into an alpha mask that
// modulates all painting while the graphics state is active. The mask is
// derived from the transparency group's alpha channel (/S /Alpha) or from its
// luminosity (/S /Luminosity, the default).
func (d *device) renderSoftMask(o types.Object) *image.Alpha {
	sm, err := d.ctx.DereferenceDict(o)
	if err != nil || sm == nil {
		if n, nerr := d.ctx.DereferenceName(o, 0, nil); nerr == nil && n.Value() == "None" {
			// /SMask /None explicitly resets any inherited soft mask.
			return nil
		}
		return nil
	}
	// S is /None: no masking.
	if s, _, err := d.ctx.DereferenceNameEntry(sm, "S"); err == nil && s != nil && s.Value() == "None" {
		return nil
	}
	luminosity := true
	if s, _, err := d.ctx.DereferenceNameEntry(sm, "S"); err == nil && s != nil && s.Value() == "Alpha" {
		luminosity = false
	}

	g := sm["G"]
	if g == nil {
		return nil
	}
	sd, _, err := d.ctx.DereferenceStreamDict(g)
	if err != nil || sd == nil {
		return nil
	}
	if err := sd.DecodeWithLimit(d.decodeLimit()); err != nil {
		return nil
	}

	// Save the whole interpreter state.
	savedCV := d.cv
	savedGS := d.gs
	savedRes := d.res
	savedStack := d.stack
	savedResStack := d.resStack
	savedFonts := d.fonts
	savedGlyphs := d.glyphs
	savedImages := d.images
	savedWarnings := d.warnings

	// Render the mask group into a transparent canvas.
	tmp := newCanvas(savedCV.w, savedCV.h)
	d.cv = tmp
	d.gs = newGState()
	d.stack = nil
	d.resStack = nil
	d.fonts = map[string]*pdfFont{}
	d.glyphs = map[glyphCacheKey][]subPath{}
	d.images = map[int]*rasterImage{}
	d.warnings = nil

	// Backdrop colour (BC) is painted into the group before its content.
	if bca, err := d.ctx.DereferenceArray(sm["BC"]); err == nil && len(bca) > 0 {
		var comps []float64
		for _, oo := range bca {
			if v, err := d.ctx.DereferenceNumber(oo); err == nil {
				comps = append(comps, clamp(v, 0, 1))
			}
		}
		cs := d.gs.fillCS
		if o, found := sd.Dict.Find("Group"); found && o != nil {
			if gd, err := d.ctx.DereferenceDict(o); err == nil && gd != nil {
				if csObj, found := gd.Find("CS"); found {
					if c, err := d.parseColorSpace(csObj); err == nil {
						cs = c
					}
				}
			}
		}
		bg := cs.resolve(comps)
		for y := 0; y < tmp.h; y++ {
			for x := 0; x < tmp.w; x++ {
				tmp.blendPixel(x, y, bg)
			}
		}
	}

	// Transform: form Matrix in the context of the current CTM.
	m := ident()
	if ma, err := d.ctx.DereferenceArray(sd.Dict["Matrix"]); err == nil && len(ma) == 6 {
		vals := make([]float64, 6)
		ok := true
		for i, oo := range ma {
			v, err := d.ctx.DereferenceNumber(oo)
			if err != nil {
				ok = false
				break
			}
			vals[i] = v
		}
		if ok {
			m = mat{vals[0], vals[1], vals[2], vals[3], vals[4], vals[5]}
		}
	}
	d.gs.ctm = savedGS.ctm.mul(m)
	if res, err := d.ctx.DereferenceDict(sd.Dict["Resources"]); err == nil && res != nil {
		d.res = res
	}
	// Restrict to the form bbox.
	if ba, err := d.ctx.DereferenceArray(sd.Dict["BBox"]); err == nil && len(ba) >= 4 {
		vals := make([]float64, 4)
		ok := true
		for i, oo := range ba {
			v, err := d.ctx.DereferenceNumber(oo)
			if err != nil {
				ok = false
				break
			}
			vals[i] = v
		}
		if ok {
			rect := subPath{pts: []pt{
				{vals[0], vals[1]}, {vals[2], vals[1]}, {vals[2], vals[3]}, {vals[0], vals[3]}, {vals[0], vals[1]},
			}}
			t := d.base.mul(d.gs.ctm)
			mask := d.cv.clipMask(transformPath([]subPath{rect}, t), false)
			d.gs.clip = intersectMasks(d.gs.clip, mask)
		}
	}

	_ = d.execContent(sd.Content)

	// Restore interpreter state.
	d.cv = savedCV
	d.gs = savedGS
	d.res = savedRes
	d.stack = savedStack
	d.resStack = savedResStack
	d.fonts = savedFonts
	d.glyphs = savedGlyphs
	d.images = savedImages
	d.warnings = savedWarnings

	// Extract the mask.
	mask := image.NewAlpha(image.Rect(0, 0, tmp.w, tmp.h))
	for y := 0; y < tmp.h; y++ {
		row := y * tmp.w
		for x := 0; x < tmp.w; x++ {
			i := 4 * (row + x)
			a := uint32(tmp.img.Pix[i+3])
			if luminosity {
				// Luminosity of the group composited over transparent black:
				// the canvas is premultiplied, so this is directly
				// 0.3R + 0.59G + 0.11B of the premultiplied values.
				lum := (77*uint32(tmp.img.Pix[i]) +
					151*uint32(tmp.img.Pix[i+1]) +
					28*uint32(tmp.img.Pix[i+2])) >> 8
				if lum > 255 {
					lum = 255
				}
				a = lum
			}
			mask.Pix[row+x] = uint8(a)
		}
	}
	return mask
}
