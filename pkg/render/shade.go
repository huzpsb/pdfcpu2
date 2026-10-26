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
	"errors"
	"fmt"
	"math"

	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

// shading is a resolved PDF shading object.
type shading struct {
	shType    int
	coords    []float64
	domain    [2]float64
	fn        *pdfFunction
	extend    [2]bool
	bbox      [4]float64 // user space, x0 y0 x1 y1
	hasBBox   bool
	cs        *colorSpace
	bg        *rgba
	bgComps   []float64
	hasBg     bool
}

// parseShading resolves a shading dictionary or stream.
func (d *device) parseShading(o types.Object) (*shading, error) {
	obj, err := d.ctx.Dereference(o)
	if err != nil {
		return nil, err
	}
	var sd types.Dict
	switch o := obj.(type) {
	case types.Dict:
		sd = o
	case types.StreamDict:
		sd = o.Dict
	default:
		return nil, errors.New("shading: must be dict or stream")
	}

	sh := &shading{domain: [2]float64{0, 1}}
	if sv, _, err := d.ctx.DereferenceIntegerEntry(sd, "ShadingType"); err == nil && sv != nil {
		sh.shType = sv.Value()
	} else {
		return nil, errors.New("shading: missing ShadingType")
	}
	if ca, err := d.ctx.DereferenceArray(sd["Coords"]); err == nil && ca != nil {
		for _, oo := range ca {
			if v, err := d.ctx.DereferenceNumber(oo); err == nil {
				sh.coords = append(sh.coords, v)
			}
		}
	}
	if da, err := d.ctx.DereferenceArray(sd["Domain"]); err == nil && len(da) >= 2 {
		v0, _ := d.ctx.DereferenceNumber(da[0])
		v1, _ := d.ctx.DereferenceNumber(da[1])
		sh.domain = [2]float64{v0, v1}
	}
	if ea, err := d.ctx.DereferenceArray(sd["Extend"]); err == nil && len(ea) >= 2 {
		if b0, err := d.ctx.DereferenceBoolean(ea[0], 0); err == nil && b0 != nil {
			sh.extend[0] = b0.Value()
		}
		if b1, err := d.ctx.DereferenceBoolean(ea[1], 0); err == nil && b1 != nil {
			sh.extend[1] = b1.Value()
		}
	}
	if ba, err := d.ctx.DereferenceArray(sd["BBox"]); err == nil && len(ba) >= 4 {
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
			sh.bbox = [4]float64{vals[0], vals[1], vals[2], vals[3]}
			sh.hasBBox = true
		}
	}
	if o, found := sd.Find("ColorSpace"); found && o != nil {
		cs, err := d.parseColorSpace(o)
		if err == nil {
			sh.cs = cs
		}
	}
	if sh.cs == nil {
		sh.cs = csGray
	}
	if ba, err := d.ctx.DereferenceArray(sd["Background"]); err == nil && ba != nil {
		comps := make([]float64, 0, len(ba))
		for _, oo := range ba {
			if v, err := d.ctx.DereferenceNumber(oo); err == nil {
				comps = append(comps, v)
			}
		}
		c := sh.cs.resolve(comps)
		sh.bg = &c
		sh.bgComps = comps
		sh.hasBg = true
	}
	if o, found := sd.Find("Function"); found && o != nil {
		fn, err := d.parseFunction(o)
		if err != nil {
			return nil, fmt.Errorf("shading: function: %w", err)
		}
		sh.fn = fn
	}
	return sh, nil
}

// paintShading paints sh into the device using the transform m (shading space
// to device space).
func (d *device) paintShading(sh *shading, m mat) {
	if sh == nil {
		return
	}
	// Determine the device area to fill.
	var userRect [4]float64
	if sh.hasBBox {
		userRect = sh.bbox
	} else {
		// Full page user space: derive from the inverse transform of the
		// device bounds.
		inv, ok := m.invert()
		if !ok {
			return
		}
		x0, y0 := inv.apply(0, 0)
		x1, y1 := inv.apply(float64(d.cv.w), float64(d.cv.h))
		userRect = [4]float64{math.Min(x0, x1), math.Min(y0, y1), math.Max(x0, x1), math.Max(y0, y1)}
	}
	corners := []pt{
		{userRect[0], userRect[1]}, {userRect[2], userRect[1]},
		{userRect[2], userRect[3]}, {userRect[0], userRect[3]},
	}
	x0, y0, x1, y1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, c := range corners {
		dx, dy := m.apply(c.x, c.y)
		x0 = math.Min(x0, dx)
		y0 = math.Min(y0, dy)
		x1 = math.Max(x1, dx)
		y1 = math.Max(y1, dy)
	}
	px0 := clampInt(int(math.Floor(x0)), 0, d.cv.w-1)
	py0 := clampInt(int(math.Floor(y0)), 0, d.cv.h-1)
	px1 := clampInt(int(math.Ceil(x1)), 0, d.cv.w)
	py1 := clampInt(int(math.Ceil(y1)), 0, d.cv.h)
	if px0 >= px1 || py0 >= py1 {
		return
	}
	inv, ok := m.invert()
	if !ok {
		return
	}

	eval := func(ux, uy float64) (rgba, bool) {
		if sh.fn == nil {
			if sh.hasBg {
				return *sh.bg, true
			}
			return rgba{}, false
		}
		var in []float64
		switch sh.shType {
		case 1:
			in = []float64{ux, uy}
		case 2, 3:
			t, inside := shadingParam(sh, ux, uy)
			if !inside {
				return rgba{}, false
			}
			in = []float64{t}
		default:
			if sh.hasBg {
				return *sh.bg, true
			}
			return rgba{}, false
		}
		out, err := evalFunction(sh.fn, in)
		if err != nil {
			if sh.hasBg {
				return *sh.bg, true
			}
			return rgba{}, false
		}
		return sh.cs.resolve(out), true
	}

	alpha := d.gs.fillAlpha
	for py := py0; py < py1; py++ {
		for px := px0; px < px1; px++ {
			ux, uy := inv.apply(float64(px)+0.5, float64(py)+0.5)
			c, ok := eval(ux, uy)
			if !ok || c.a == 0 {
				continue
			}
			a := alpha
			if d.gs.clip != nil {
				a *= float64(d.gs.clip.AlphaAt(px, py).A) / 255
			}
			if a <= 0.004 {
				continue
			}
			d.cv.blend(px, py, c, float64(c.a)/255*a)
		}
	}
}

// shadingParam returns the shading parameter t for a user space point.
func shadingParam(sh *shading, ux, uy float64) (float64, bool) {
	switch sh.shType {
	case 2:
		if len(sh.coords) < 4 {
			return 0, false
		}
		x0, y0, x1, y1 := sh.coords[0], sh.coords[1], sh.coords[2], sh.coords[3]
		dx, dy := x1-x0, y1-y0
		l2 := dx*dx + dy*dy
		if l2 == 0 {
			return sh.domain[0], true
		}
		t := ((ux-x0)*dx + (uy-y0)*dy) / l2
		return clampExtended(sh, t)
	case 3:
		if len(sh.coords) < 6 {
			return 0, false
		}
		x0, y0, r0 := sh.coords[0], sh.coords[1], sh.coords[2]
		x1, y1, r1 := sh.coords[3], sh.coords[4], sh.coords[5]
		dx, dy := x1-x0, y1-y0
		dr := r1 - r0
		fx, fy := ux-x0, uy-y0
		A := dx*dx + dy*dy - dr*dr
		B := 2 * (fx*dx + fy*dy + r0*dr)
		C := fx*fx + fy*fy - r0*r0
		if math.Abs(A) < 1e-12 {
			if math.Abs(B) < 1e-12 {
				return 0, false
			}
			t := -C / B
			return clampExtended(sh, t)
		}
		disc := B*B - 4*A*C
		if disc < 0 {
			return extendFallback(sh)
		}
		sq := math.Sqrt(disc)
		t1 := (-B + sq) / (2 * A)
		t2 := (-B - sq) / (2 * A)
		// Prefer the root inside [0,1].
		for _, t := range []float64{t1, t2} {
			if t >= 0 && t <= 1 {
				return clampExtended(sh, t)
			}
		}
		return extendFallback(sh)
	}
	return 0, false
}

func clampExtended(sh *shading, t float64) (float64, bool) {
	if t < 0 {
		if sh.extend[0] {
			return 0, true
		}
		return 0, false
	}
	if t > 1 {
		if sh.extend[1] {
			return 1, true
		}
		return 0, false
	}
	return t, true
}

func extendFallback(sh *shading) (float64, bool) {
	if sh.extend[0] {
		return 0, true
	}
	if sh.extend[1] {
		return 1, true
	}
	return 0, false
}
