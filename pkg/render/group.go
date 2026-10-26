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
	"math"

	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

// formMatrix resolves the /Matrix entry of a form dictionary.
func (d *device) formMatrix(sd *types.StreamDict) mat {
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
	return m
}

// formBBox resolves and normalizes the /BBox entry of a form dictionary.
func (d *device) formBBox(sd *types.StreamDict) ([4]float64, bool) {
	ba, err := d.ctx.DereferenceArray(sd.Dict["BBox"])
	if err != nil || len(ba) < 4 {
		return [4]float64{}, false
	}
	vals := make([]float64, 4)
	for i, oo := range ba {
		v, err := d.ctx.DereferenceNumber(oo)
		if err != nil {
			return [4]float64{}, false
		}
		vals[i] = v
	}
	return [4]float64{
		math.Min(vals[0], vals[2]),
		math.Min(vals[1], vals[3]),
		math.Max(vals[0], vals[2]),
		math.Max(vals[1], vals[3]),
	}, true
}

// bboxClipMask builds a clip mask for a form bbox in the coordinate system of
// m.
func (d *device) bboxClipMask(bb [4]float64, m mat) *image.Alpha {
	rect := subPath{pts: []pt{
		{bb[0], bb[1]}, {bb[2], bb[1]}, {bb[2], bb[3]}, {bb[0], bb[3]}, {bb[0], bb[1]},
	}}
	return d.cv.clipMask(transformPath([]subPath{rect}, m), false)
}

// renderFormToCanvas executes a form into a fresh transparent canvas and
// returns it. The form runs in an isolated transparency group state: the
// graphics state (including the constant alpha values) starts fresh.
func (d *device) renderFormToCanvas(sd *types.StreamDict, parentCTM mat, clip *image.Alpha) *canvas {
	savedCV := d.cv
	savedGS := d.gs
	savedRes := d.res
	savedStack := d.stack
	savedResStack := d.resStack

	tmp := newCanvas(savedCV.w, savedCV.h)
	d.cv = tmp
	d.gs = newGState()
	d.stack = nil
	d.resStack = nil
	d.gs.ctm = parentCTM.mul(d.formMatrix(sd))
	if res, err := d.ctx.DereferenceDict(sd.Dict["Resources"]); err == nil && res != nil {
		d.res = res
	}
	if bb, ok := d.formBBox(sd); ok {
		mask := d.bboxClipMask(bb, d.base.mul(d.gs.ctm))
		d.gs.clip = intersectMasks(clip, mask)
	} else {
		d.gs.clip = clip
	}

	_ = d.execContent(sd.Content)

	d.cv = savedCV
	d.gs = savedGS
	d.res = savedRes
	d.stack = savedStack
	d.resStack = savedResStack
	return tmp
}

// compositeCanvas blends a rendered group buffer over the main canvas with an
// additional constant alpha factor and clip.
func (d *device) compositeCanvas(src *canvas, alpha float64, clip *image.Alpha) {
	if src == nil || alpha <= 0 {
		return
	}
	dst := d.cv.img
	sw, sh := src.w, src.h
	for y := 0; y < d.cv.h && y < sh; y++ {
		for x := 0; x < d.cv.w && x < sw; x++ {
			si := 4 * (y*sw + x)
			sa := uint32(src.img.Pix[si+3])
			if sa == 0 {
				continue
			}
			a := float64(sa) * alpha
			if clip != nil {
				a *= float64(clip.AlphaAt(x, y).A) / 255
			}
			if a <= 0.4 {
				continue
			}
			ai := uint32(a)
			if ai > 255 {
				ai = 255
			}
			di := dst.PixOffset(x, y)
			p := dst.Pix[di:]
			// Scale the premultiplied source by ai/sa.
			f := uint32(ai) * 255 / sa
			sr := uint32(src.img.Pix[si]) * f / 255
			sg := uint32(src.img.Pix[si+1]) * f / 255
			sb := uint32(src.img.Pix[si+2]) * f / 255
			inv := 255 - ai
			p[0] = uint8(sr + uint32(p[0])*inv/255)
			p[1] = uint8(sg + uint32(p[1])*inv/255)
			p[2] = uint8(sb + uint32(p[2])*inv/255)
			p[3] = uint8(ai + uint32(p[3])*inv/255)
		}
	}
}
