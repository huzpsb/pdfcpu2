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
	"strconv"
	"strings"

	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

// Color space families understood by the renderer.
type csFamily int

const (
	csDeviceGray csFamily = iota
	csDeviceRGB
	csDeviceCMYK
	csCalGray
	csCalRGB
	csLab
	csICCBased
	csIndexed
	csSeparation // also DeviceN
	csPattern
	csUnknown
)

// colorSpace describes a resolved PDF color space.
type colorSpace struct {
	family csFamily
	n      int
	// base is the alternate space for Separation/DeviceN and the base space
	// for Indexed color spaces.
	base *colorSpace
	// tintFn is the tint transform of a Separation/DeviceN color space.
	tintFn *pdfFunction
	// lookup and hival describe an Indexed color space.
	lookup []byte
	hival  int
	// cal holds the parameters of a CalRGB / CalGray space.
	cal *calParams
}

// calParams are the /WhitePoint, /BlackPoint, /Gamma and /Matrix entries of a
// CIE-based color space dictionary.
type calParams struct {
	wp     [3]float64
	gamma  [3]float64
	matrix [9]float64 // row major XYZ = M * linear(A,B,C)
	isGray bool
}

var (
	csGray = &colorSpace{family: csDeviceGray, n: 1}
	csRGB  = &colorSpace{family: csDeviceRGB, n: 3}
	csCMYK = &colorSpace{family: csDeviceCMYK, n: 4}
)

// parseColorSpace resolves o into a colorSpace using the resource dictionary
// for named spaces.
func (d *device) parseColorSpace(o types.Object) (*colorSpace, error) {
	xrt := d.ctx.XRefTable
	o, err := xrt.Dereference(o)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, errors.New("missing color space")
	}
	switch o := o.(type) {
	case types.Name:
		switch o.Value() {
		case "DeviceGray", "G":
			return csGray, nil
		case "DeviceRGB", "RGB":
			return csRGB, nil
		case "DeviceCMYK", "CMYK":
			return csCMYK, nil
		case "Pattern":
			return &colorSpace{family: csPattern, n: 1}, nil
		}
		// Look up the resource dict.
		if d.res != nil {
			csd, err := xrt.DereferenceDict(d.res["ColorSpace"])
			if err != nil {
				return nil, err
			}
			if csd != nil {
				if oo, found := csd.Find(o.Value()); found {
					return d.parseColorSpace(oo)
				}
			}
		}
		return nil, fmt.Errorf("unknown color space %s", o.Value())
	case types.Array:
		if len(o) == 0 {
			return nil, errors.New("empty color space array")
		}
		fam, err := xrt.DereferenceName(o[0], 0, nil)
		if err != nil {
			return nil, err
		}
		switch fam.Value() {
		case "CalGray", "Cal":
			cal := d.parseCalParams(o[1], true)
			return &colorSpace{family: csCalGray, n: 1, cal: cal}, nil
		case "CalRGB", "CalR":
			cal := d.parseCalParams(o[1], false)
			return &colorSpace{family: csCalRGB, n: 3, cal: cal}, nil
		case "Lab":
			return &colorSpace{family: csLab, n: 3}, nil
		case "Pattern":
			if len(o) > 1 {
				base, err := d.parseColorSpace(o[1])
				if err != nil {
					return nil, err
				}
				return &colorSpace{family: csPattern, n: base.n, base: base}, nil
			}
			return &colorSpace{family: csPattern, n: 1}, nil
		case "ICCBased", "ICCB":
			sd, _, err := xrt.DereferenceStreamDict(o[1])
			if err != nil {
				return nil, err
			}
			nv, _, err := xrt.DereferenceIntegerEntry(sd.Dict, "N")
			if err != nil {
				return nil, err
			}
			if nv == nil {
				return csRGB, nil
			}
			switch *nv {
			case 1:
				return &colorSpace{family: csICCBased, n: 1, base: csGray}, nil
			case 4:
				return &colorSpace{family: csICCBased, n: 4, base: csCMYK}, nil
			default:
				return &colorSpace{family: csICCBased, n: 3, base: csRGB}, nil
			}
		case "Indexed", "I":
			if len(o) < 4 {
				return nil, errors.New("indexed color space: corrupt")
			}
			base, err := d.parseColorSpace(o[1])
			if err != nil {
				return nil, err
			}
			hv, err := xrt.DereferenceNumber(o[2])
			if err != nil {
				return nil, err
			}
			lookup, err := d.colorLookupTable(o[3])
			if err != nil {
				return nil, err
			}
			return &colorSpace{family: csIndexed, n: 1, base: base, hival: int(hv), lookup: lookup}, nil
		case "Separation", "DeviceN":
			if len(o) < 4 {
				return nil, errors.New("separation color space: corrupt")
			}
			alt, err := d.parseColorSpace(o[len(o)-2])
			if err != nil {
				return nil, err
			}
			fn, err := d.parseFunction(o[len(o)-1])
			if err != nil {
				return nil, err
			}
			var n int
			if names, err := xrt.DereferenceArray(o[1]); err == nil && len(names) > 0 {
				n = len(names)
			} else {
				n = 1
			}
			return &colorSpace{family: csSeparation, n: n, base: alt, tintFn: fn}, nil
		}
		return nil, fmt.Errorf("unsupported color space family %s", fam.Value())
	}
	return nil, errors.New("color space must be name or array")
}

func (d *device) colorLookupTable(o types.Object) ([]byte, error) {
	o, err := d.ctx.Dereference(o)
	if err != nil || o == nil {
		return nil, err
	}
	switch o := o.(type) {
	case types.StringLiteral:
		return []byte(o.Value()), nil
	case types.HexLiteral:
		b, err := o.Bytes()
		if err != nil {
			return nil, err
		}
		return b, nil
	case types.StreamDict:
		if err := o.DecodeWithLimit(d.decodeLimit()); err != nil {
			return nil, err
		}
		return o.Content, nil
	}
	return nil, nil
}

// resolve converts component values (each in [0,1]) into an rgba color.
func (cs *colorSpace) resolve(comps []float64) rgba {
	if cs == nil {
		return rgba{0, 0, 0, 255}
	}
	get := func(i int) float64 {
		if i < len(comps) {
			return clamp(comps[i], 0, 1)
		}
		return 0
	}
	switch cs.family {
	case csCalGray:
		if cs.cal != nil {
			return calGrayToRGB(get(0), cs.cal)
		}
		v := uint8(get(0) * 255)
		return rgba{v, v, v, 255}
	case csCalRGB:
		if cs.cal != nil {
			return calRGBToRGB(get(0), get(1), get(2), cs.cal)
		}
		return rgba{uint8(get(0) * 255), uint8(get(1) * 255), uint8(get(2) * 255), 255}
	case csDeviceGray:
		v := uint8(get(0) * 255)
		return rgba{v, v, v, 255}
	case csDeviceRGB:
		return rgba{uint8(get(0) * 255), uint8(get(1) * 255), uint8(get(2) * 255), 255}
	case csDeviceCMYK, csICCBased:
		if cs.n == 1 && cs.family == csICCBased {
			v := uint8(get(0) * 255)
			return rgba{v, v, v, 255}
		}
		if cs.n == 4 {
			r, g, b := cmykToRGB(get(0), get(1), get(2), get(3))
			return rgba{r, g, b, 255}
		}
		return rgba{uint8(get(0) * 255), uint8(get(1) * 255), uint8(get(2) * 255), 255}
	case csLab:
		return labToRGB(get(0)*100, get(1)*255-128, get(2)*255-128)
	case csIndexed:
		idx := int(get(0)*float64(cs.hival) + 0.5)
		if idx < 0 {
			idx = 0
		}
		if idx > cs.hival {
			idx = cs.hival
		}
		if cs.base == nil {
			return rgba{0, 0, 0, 255}
		}
		n := cs.base.n
		off := idx * n
		if off+n > len(cs.lookup) {
			return rgba{0, 0, 0, 255}
		}
		vals := make([]float64, n)
		for i := 0; i < n; i++ {
			vals[i] = float64(cs.lookup[off+i]) / 255
		}
		return cs.base.resolve(vals)
	case csSeparation:
		if cs.tintFn != nil && cs.base != nil {
			if out, err := evalFunction(cs.tintFn, comps); err == nil {
				return cs.base.resolve(out)
			}
		}
		if cs.base != nil {
			return cs.base.resolve(make([]float64, cs.base.n))
		}
		return rgba{0, 0, 0, 255}
	}
	return rgba{0, 0, 0, 255}
}

func cmykToRGB(c, m, y, k float64) (uint8, uint8, uint8) {
	r := (1 - c) * (1 - k)
	g := (1 - m) * (1 - k)
	b := (1 - y) * (1 - k)
	return uint8(clamp(r, 0, 1) * 255), uint8(clamp(g, 0, 1) * 255), uint8(clamp(b, 0, 1) * 255)
}

// labToRGB converts CIE L*a*b* (D65) approximately to sRGB.
func labToRGB(l, a, b float64) rgba {
	fy := (l + 16) / 116
	fx := fy + a/500
	fz := fy - b/200
	d := 6/29.0
	finv := func(t float64) float64 {
		if t > d*d*d {
			return t * t * t
		}
		return 3 * d * d * (t - 4/29.0)
	}
	// D65 white point.
	xw, yw, zw := 0.9505, 1.0, 1.089
	x, y, z := finv(fx)*xw, finv(fy)*yw, finv(fz)*zw
	// Linear sRGB via the standard matrix.
	rl := 3.2406*x - 1.5372*y - 0.4986*z
	gl := -0.9689*x + 1.8758*y + 0.0415*z
	bl := 0.0557*x - 0.2040*y + 1.0570*z
	gamma := func(v float64) float64 {
		v = clamp(v, 0, 1)
		if v <= 0.0031308 {
			return 12.92 * v
		}
		return 1.055*math.Pow(v, 1/2.4) - 0.055
	}
	return rgba{uint8(gamma(rl) * 255), uint8(gamma(gl) * 255), uint8(gamma(bl) * 255), 255}
}

// parseCalParams extracts /WhitePoint, /Gamma and /Matrix from a CIE-based
// color space dictionary.
func (d *device) parseCalParams(o types.Object, gray bool) *calParams {
	cp := &calParams{wp: [3]float64{0.9505, 1, 1.089}, gamma: [3]float64{1, 1, 1}, isGray: gray,
		matrix: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}}
	if o == nil {
		return cp
	}
	dd, err := d.ctx.DereferenceDict(o)
	if err != nil || dd == nil {
		return cp
	}
	if a, err := d.ctx.DereferenceArray(dd["WhitePoint"]); err == nil && len(a) == 3 {
		for i := 0; i < 3; i++ {
			if v, err := d.ctx.DereferenceNumber(a[i]); err == nil {
				cp.wp[i] = v
			}
		}
	}
	if a, err := d.ctx.DereferenceArray(dd["Gamma"]); err == nil {
		for i := 0; i < 3 && i < len(a); i++ {
			if v, err := d.ctx.DereferenceNumber(a[i]); err == nil && v > 0 {
				cp.gamma[i] = v
			}
		}
		if len(a) == 1 {
			cp.gamma[1], cp.gamma[2] = cp.gamma[0], cp.gamma[0]
		}
	}
	if a, err := d.ctx.DereferenceArray(dd["Matrix"]); err == nil && len(a) == 9 {
		for i := 0; i < 9; i++ {
			if v, err := d.ctx.DereferenceNumber(a[i]); err == nil {
				cp.matrix[i] = v
			}
		}
	}
	return cp
}

// calGrayToRGB converts a CalGray value via its white point to sRGB.
func calGrayToRGB(a float64, cp *calParams) rgba {
	if !isUnitWhitePoint(cp.wp) {
		v := uint8(a * 255)
		return rgba{v, v, v, 255}
	}
	lin := math.Pow(a, cp.gamma[0])
	wn := cp.wp
	if wn[1] != 0 {
		wn[0], wn[1], wn[2] = cp.wp[0]/cp.wp[1], 1, cp.wp[2]/cp.wp[1]
	}
	x, y, z := wn[0]*lin, wn[1]*lin, wn[2]*lin
	return xyzToSRGB(x, y, z)
}

// isUnitWhitePoint mirrors PDFBox: only a WhitePoint of exactly (1, 1, 1)
// triggers full CIE math, everything else passes through as monitor values
// (generator files in the wild rely on this).
func isUnitWhitePoint(wp [3]float64) bool {
	return wp[0] == 1 && wp[1] == 1 && wp[2] == 1
}

// calRGBToRGB converts a CalRGB triple to sRGB honoring Gamma and Matrix.
func calRGBToRGB(a, b, c float64, cp *calParams) rgba {
	if !isUnitWhitePoint(cp.wp) {
		return rgba{uint8(a * 255), uint8(b * 255), uint8(c * 255), 255}
	}
	la, lb, lc := math.Pow(a, cp.gamma[0]), math.Pow(b, cp.gamma[1]), math.Pow(c, cp.gamma[2])
	m := cp.matrix
	x := m[0]*la + m[1]*lb + m[2]*lc
	y := m[3]*la + m[4]*lb + m[5]*lc
	z := m[6]*la + m[7]*lb + m[8]*lc
	return xyzToSRGB(x, y, z)
}

// xyzToSRGB converts CIE XYZ (D65) to gamma encoded sRGB.
func xyzToSRGB(x, y, z float64) rgba {
	rl := 3.2406*x - 1.5372*y - 0.4986*z
	gl := -0.9689*x + 1.8758*y + 0.0415*z
	bl := 0.0557*x - 0.2040*y + 1.0570*z
	gamma := func(v float64) float64 {
		v = clamp(v, 0, 1)
		if v <= 0.0031308 {
			return 12.92 * v
		}
		return 1.055*math.Pow(v, 1/2.4) - 0.055
	}
	return rgba{uint8(gamma(rl) * 255), uint8(gamma(gl) * 255), uint8(gamma(bl) * 255), 255}
}

// pdfFunction is a PDF function (types 0, 2, 3 and 4).
type pdfFunction struct {
	fnType         int
	domain, rng    [][2]float64
	size           []int
	bps            int
	samples        []byte
	encode, decode [][2]float64
	c0, c1         []float64
	expN           float64
	fns            []*pdfFunction
	bounds         []float64
	prog           []string // type 4 tokens
}

// parseFunction resolves o into a pdfFunction.
func (d *device) parseFunction(o types.Object) (*pdfFunction, error) {
	xrt := d.ctx.XRefTable
	switch o := o.(type) {
	case types.IndirectRef:
		obj, err := xrt.Dereference(o)
		if err != nil {
			return nil, err
		}
		return d.parseFunction(obj)
	case types.Dict:
		return d.functionFromDict(o)
	case types.StreamDict:
		fn, err := d.functionFromDict(o.Dict)
		if err != nil {
			return nil, err
		}
		if fn.fnType == 0 {
			// Sample data lives in the decoded stream body.
			if err := o.DecodeWithLimit(d.decodeLimit()); err == nil && len(o.Content) > 0 {
				fn.samples = o.Content
			} else {
				fn.samples = o.Raw
			}
		}
		if fn.fnType == 4 {
			if err := o.DecodeWithLimit(d.decodeLimit()); err != nil {
				return nil, err
			}
			fn.prog = strings.Fields(string(o.Content))
		}
		return fn, nil
	}
	return nil, errors.New("function must be dict or stream")
}

func (d *device) functionFromDict(d1 types.Dict) (*pdfFunction, error) {
	xrt := d.ctx.XRefTable
	fn := &pdfFunction{}
	tv, _, err := xrt.DereferenceIntegerEntry(d1, "FunctionType")
	if err != nil {
		return nil, err
	}
	if tv == nil {
		return nil, errors.New("function: missing FunctionType")
	}
	fn.fnType = tv.Value()
	if a, err := xrt.DereferenceArray(d1["Domain"]); err == nil && len(a) > 0 {
		fn.domain = rangePairs(xrt, a)
	}
	if a, err := xrt.DereferenceArray(d1["Range"]); err == nil && len(a) > 0 {
		fn.rng = rangePairs(xrt, a)
	}
	switch fn.fnType {
	case 0:
		if a, err := xrt.DereferenceArray(d1["Size"]); err == nil {
			for _, oo := range a {
				if iv, err := xrt.DereferenceInteger(oo); err == nil && iv != nil {
					fn.size = append(fn.size, iv.Value())
				}
			}
		}
		if bps, _, err := xrt.DereferenceIntegerEntry(d1, "BitsPerSample"); err == nil && bps != nil {
			fn.bps = bps.Value()
		}
		if a, err := xrt.DereferenceArray(d1["Encode"]); err == nil && len(a) > 0 {
			fn.encode = rangePairs(xrt, a)
		}
		if a, err := xrt.DereferenceArray(d1["Decode"]); err == nil && len(a) > 0 {
			fn.decode = rangePairs(xrt, a)
		}
	case 2:
		if a, err := xrt.DereferenceArray(d1["C0"]); err == nil && len(a) > 0 {
			for _, oo := range a {
				if v, err := xrt.DereferenceNumber(oo); err == nil {
					fn.c0 = append(fn.c0, v)
				}
			}
		}
		if a, err := xrt.DereferenceArray(d1["C1"]); err == nil && len(a) > 0 {
			for _, oo := range a {
				if v, err := xrt.DereferenceNumber(oo); err == nil {
					fn.c1 = append(fn.c1, v)
				}
			}
		}
		if nv, _, err := xrt.DereferenceNumberEntry(d1, "N"); err == nil && nv != nil {
			fn.expN = *nv
		} else {
			fn.expN = 1
		}
	case 3:
		if a, err := xrt.DereferenceArray(d1["Functions"]); err == nil {
			for _, oo := range a {
				sub, err := d.parseFunction(oo)
				if err != nil {
					return nil, err
				}
				fn.fns = append(fn.fns, sub)
			}
		}
		if a, err := xrt.DereferenceArray(d1["Bounds"]); err == nil && len(a) > 0 {
			for _, oo := range a {
				if v, err := xrt.DereferenceNumber(oo); err == nil {
					fn.bounds = append(fn.bounds, v)
				}
			}
		}
		if a, err := xrt.DereferenceArray(d1["Encode"]); err == nil && len(a) > 0 {
			fn.encode = rangePairs(xrt, a)
		}
		// A stitching function with k subfunctions must carry k-1 bounds.
		// Tolerate missing/mismatched Bounds by distributing evenly.
		if n := len(fn.fns) - 1; n > 0 && len(fn.bounds) != n {
			fn.bounds = make([]float64, n)
			for i := 0; i < n; i++ {
				fn.bounds[i] = float64(i+1) / float64(n)
			}
		}
	case 4:
		// Program handled by caller for streams.
	default:
		return nil, fmt.Errorf("unsupported function type %d", fn.fnType)
	}
	return fn, nil
}

func rangePairs(xrt *model.XRefTable, a types.Array) [][2]float64 {
	var out [][2]float64
	for i := 0; i+1 < len(a); i += 2 {
		v0, err0 := xrt.DereferenceNumber(a[i])
		v1, err1 := xrt.DereferenceNumber(a[i+1])
		if err0 == nil && err1 == nil {
			out = append(out, [2]float64{v0, v1})
		}
	}
	return out
}

// evalFunction evaluates fn for the input values.
func evalFunction(fn *pdfFunction, in []float64) ([]float64, error) {
	if fn == nil {
		return nil, errors.New("nil function")
	}
	switch fn.fnType {
	case 2:
		n := len(fn.c1)
		if len(fn.c0) > n {
			n = len(fn.c0)
		}
		if n == 0 {
			n = len(fn.rng)
		}
		t := 0.0
		if len(in) > 0 {
			t = clamp(in[0], 0, 1)
		}
		t = math.Pow(t, fn.expN)
		out := make([]float64, n)
		for i := 0; i < n; i++ {
			var c0, c1 float64
			if i < len(fn.c0) {
				c0 = fn.c0[i]
			}
			if i < len(fn.c1) {
				c1 = fn.c1[i]
			}
			out[i] = c0 + t*(c1-c0)
		}
		return clampRange(out, fn.rng), nil
	case 3:
		t := 0.0
		if len(in) > 0 {
			t = in[0]
		}
		if len(fn.fns) == 0 {
			return nil, errors.New("stitching function: no subfunctions")
		}
		if len(fn.fns) == 1 || (len(fn.bounds) > 0 && t <= fn.bounds[0]) {
			seg := 0
			hi := 1.0
			if len(fn.bounds) > 0 {
				hi = fn.bounds[0]
			}
			lo := 0.0
			e0, e1 := 0.0, 1.0
			if len(fn.encode) > 0 {
				e0, e1 = fn.encode[0][0], fn.encode[0][1]
			}
			return evalFunction(fn.fns[seg], []float64{remap(t, lo, hi, e0, e1)})
		}
		if len(fn.bounds) == 0 {
			return evalFunction(fn.fns[0], []float64{clamp(t, 0, 1)})
		}
		if t >= fn.bounds[len(fn.bounds)-1] {
			lo, hi := fn.bounds[len(fn.bounds)-1], 1.0
			e0, e1 := 0.0, 1.0
			if len(fn.encode) > len(fn.bounds) {
				e := fn.encode[len(fn.bounds)]
				e0, e1 = e[0], e[1]
			}
			return evalFunction(fn.fns[len(fn.fns)-1], []float64{remap(t, lo, hi, e0, e1)})
		}
		for i := 0; i < len(fn.bounds); i++ {
			lo, hi := 0.0, fn.bounds[0]
			if i > 0 {
				lo = fn.bounds[i-1]
			}
			hi = fn.bounds[i]
			if t >= lo && t <= hi {
				e0, e1 := 0.0, 1.0
				if len(fn.encode) > i {
					e0, e1 = fn.encode[i][0], fn.encode[i][1]
				}
				return evalFunction(fn.fns[i], []float64{remap(t, lo, hi, e0, e1)})
			}
		}
		return evalFunction(fn.fns[0], []float64{0})
	case 0:
		return evalSampled(fn, in)
	case 4:
		return evalPSProgram(fn, in)
	}
	return nil, fmt.Errorf("cannot evaluate function type %d", fn.fnType)
}

func remap(t, inLo, inHi, outLo, outHi float64) float64 {
	if inHi == inLo {
		return outLo
	}
	return outLo + (t-inLo)*(outHi-outLo)/(inHi-inLo)
}

func clampRange(vals []float64, rng [][2]float64) []float64 {
	for i := range vals {
		if i < len(rng) {
			vals[i] = clamp(vals[i], rng[i][0], rng[i][1])
		}
	}
	return vals
}

func evalSampled(fn *pdfFunction, in []float64) ([]float64, error) {
	if len(fn.size) == 0 || fn.bps == 0 || fn.samples == nil {
		return nil, errors.New("sampled function: corrupt")
	}
	nIn := len(fn.size)
	nOut := len(fn.rng)
	if nOut == 0 {
		return nil, errors.New("sampled function: missing Range")
	}
	// Map each input through its encode entry, clamp to [0, size-1].
	scaled := make([]float64, nIn)
	for i := 0; i < nIn; i++ {
		v := 0.0
		if i < len(in) {
			v = in[i]
		}
		lo, hi := 0.0, float64(fn.size[i]-1)
		if i < len(fn.encode) {
			lo, hi = fn.encode[i][0], fn.encode[i][1]
		}
		e := remap(v, domainLo(fn, i), domainHi(fn, i), lo, hi)
		scaled[i] = clamp(e, 0, float64(fn.size[i]-1))
	}
	sampleAt := func(idx []int) []float64 {
		off := 0
		for i := 0; i < nIn; i++ {
			off = off*fn.size[i] + idx[i]
		}
		out := make([]float64, nOut)
		for j := 0; j < nOut; j++ {
			raw := sampleBits(fn.samples, off*nOut+j, fn.bps)
			maxV := float64(uint64(1)<<uint(fn.bps) - 1)
			v := raw / maxV
			// Decode entry; its default is the Range endpoints.
			rLo, rHi := 0.0, 1.0
			if j < len(fn.rng) {
				rLo, rHi = fn.rng[j][0], fn.rng[j][1]
			}
			dLo, dHi := rLo, rHi
			if j < len(fn.decode) {
				dLo, dHi = fn.decode[j][0], fn.decode[j][1]
			}
			out[j] = remap(v, 0, 1, dLo, dHi)
		}
		return out
	}
	// Multilinear interpolation between the surrounding sample points.
	base := make([]int, nIn)
	frac := make([]float64, nIn)
	for i := 0; i < nIn; i++ {
		fi := math.Floor(scaled[i])
		if fi < 0 {
			fi = 0
		}
		if fi > float64(fn.size[i]-1) {
			fi = float64(fn.size[i] - 1)
		}
		base[i] = int(fi)
		frac[i] = clamp(scaled[i]-fi, 0, 1)
	}
	corners := 1 << uint(nIn)
	acc := make([]float64, nOut)
	for c := 0; c < corners; c++ {
		idx := make([]int, nIn)
		w := 1.0
		for i := 0; i < nIn; i++ {
			if c&(1<<uint(i)) != 0 {
				idx[i] = base[i] + 1
				w *= frac[i]
			} else {
				idx[i] = base[i]
				w *= 1 - frac[i]
			}
			if idx[i] >= fn.size[i] {
				idx[i] = fn.size[i] - 1
			}
		}
		if w == 0 {
			continue
		}
		s := sampleAt(idx)
		for j := 0; j < nOut; j++ {
			acc[j] += s[j] * w
		}
	}
	return acc, nil
}

func domainLo(fn *pdfFunction, i int) float64 {
	if i < len(fn.domain) {
		return fn.domain[i][0]
	}
	return 0
}

func domainHi(fn *pdfFunction, i int) float64 {
	if i < len(fn.domain) {
		return fn.domain[i][1]
	}
	return 1
}

func sampleBits(b []byte, i, bps int) float64 {
	switch bps {
	case 8:
		if i < len(b) {
			return float64(b[i])
		}
	case 16:
		if 2*i+1 < len(b) {
			return float64(uint16(b[2*i])<<8 | uint16(b[2*i+1]))
		}
	case 32:
		if 4*i+3 < len(b) {
			return float64(uint32(b[4*i])<<24 | uint32(b[4*i+1])<<16 | uint32(b[4*i+2])<<8 | uint32(b[4*i+3]))
		}
	default:
		bitOff := i * bps
		byteOff := bitOff / 8
		if byteOff < len(b) {
			shift := uint(8 - bps - bitOff%8)
			maxV := uint64(1)<<uint(bps) - 1
			return float64(uint64(b[byteOff]>>shift) & maxV)
		}
	}
	return 0
}

// evalPSProgram evaluates a type 4 (PostScript calculator) function using a
// small stack machine that covers the commonly used operator subset.
func evalPSProgram(fn *pdfFunction, in []float64) ([]float64, error) {
	if fn.prog == nil {
		return nil, errors.New("calculator function: missing program")
	}
	vm := &psVM{}
	// The input values start on the stack.
	vm.stack = append(vm.stack, in...)
	if err := vm.exec(fn.prog, 0, len(fn.prog), in); err != nil {
		return nil, err
	}
	nOut := len(fn.rng)
	if nOut == 0 || len(vm.stack) < nOut {
		if len(vm.stack) == 0 {
			return nil, errors.New("calculator function: empty result")
		}
		return vm.stack, nil
	}
	out := vm.stack[len(vm.stack)-nOut:]
	return clampRange(out, fn.rng), nil
}

// psVM is a stack machine for PostScript calculator functions.
type psVM struct {
	stack []float64
}

func (vm *psVM) push(v float64) { vm.stack = append(vm.stack, v) }

func (vm *psVM) pop() (float64, error) {
	if len(vm.stack) == 0 {
		return 0, errors.New("calculator function: stack underflow")
	}
	v := vm.stack[len(vm.stack)-1]
	vm.stack = vm.stack[:len(vm.stack)-1]
	return v, nil
}

// exec executes tokens[start:end). Procs ({...}) are collected and passed to
// branch operators.
func (vm *psVM) exec(prog []string, start, end int, in []float64) error {
	i := start
	for i < end {
		tok := prog[i]
		switch tok {
		case "{":
			// Find matching close brace and skip: procs are only meaningful
			// for if/ifelse which scan them explicitly.
			depth := 1
			j := i + 1
			for j < end && depth > 0 {
				if prog[j] == "{" {
					depth++
				} else if prog[j] == "}" {
					depth--
				}
				j++
			}
			i = j
			continue
		case "if", "ifelse":
			nProcs := 1
			if tok == "ifelse" {
				nProcs = 2
			}
			// Collect the preceding nProcs procs by scanning backwards.
			procs := make([][]string, nProcs)
			pos := i
			for k := nProcs - 1; k >= 0; k-- {
				pStart, pEnd, ok := scanProcBack(prog, pos)
				if !ok {
					return errors.New("calculator function: proc scan failed")
				}
				procs[k] = prog[pStart:pEnd]
				pos = pStart - 1
			}
			var err error
			if tok == "if" {
				var cond float64
				cond, err = vm.pop()
				if err == nil && cond != 0 {
					err = vm.exec(procs[0], 0, len(procs[0]), in)
				}
			} else {
				var cond float64
				cond, err = vm.pop()
				if err == nil {
					if cond != 0 {
						err = vm.exec(procs[0], 0, len(procs[0]), in)
					} else {
						err = vm.exec(procs[1], 0, len(procs[1]), in)
					}
				}
			}
			if err != nil {
				return err
			}
			i++
			continue
		}
		if v, err := strconv.ParseFloat(tok, 64); err == nil {
			vm.push(v)
			i++
			continue
		}
		if err := vm.op(tok, in); err != nil {
			return err
		}
		i++
	}
	return nil
}

// scanProcBack finds the {...} proc ending just before index limit.
func scanProcBack(prog []string, limit int) (int, int, bool) {
	if limit <= 0 || limit > len(prog) || prog[limit-1] != "}" {
		return 0, 0, false
	}
	depth := 0
	j := limit - 1
	for j >= 0 {
		if prog[j] == "}" {
			depth++
		} else if prog[j] == "{" {
			depth--
			if depth == 0 {
				return j + 1, limit - 1, true
			}
		}
		j--
	}
	return 0, 0, false
}

func (vm *psVM) op(tok string, in []float64) error {
	binErr := errors.New("calculator function: division by zero")
	binary := map[string]func(a, b float64) (float64, error){
		"add": func(a, b float64) (float64, error) { return a + b, nil },
		"sub": func(a, b float64) (float64, error) { return a - b, nil },
		"mul": func(a, b float64) (float64, error) { return a * b, nil },
		"div": func(a, b float64) (float64, error) {
			if b == 0 {
				return 0, binErr
			}
			return a / b, nil
		},
		"idiv": func(a, b float64) (float64, error) {
			if b == 0 {
				return 0, binErr
			}
			return float64(int(a) / int(b)), nil
		},
		"mod": func(a, b float64) (float64, error) {
			if b == 0 {
				return 0, binErr
			}
			return float64(int(a) % int(b)), nil
		},
	}
	if f, ok := binary[tok]; ok {
		b, err := vm.pop()
		if err != nil {
			return err
		}
		a, err := vm.pop()
		if err != nil {
			return err
		}
		v, err := f(a, b)
		if err != nil {
			return err
		}
		vm.push(v)
		return nil
	}
	switch tok {
	case "true":
		vm.push(1)
	case "false":
		vm.push(0)
	case "atan":
		b, err := vm.pop()
		if err != nil {
			return err
		}
		a, err := vm.pop()
		if err != nil {
			return err
		}
		d := math.Mod(math.Atan2(a, b)*180/math.Pi, 360)
		if d < 0 {
			d += 360
		}
		vm.push(d)
	case "exp":
		b, err := vm.pop()
		if err != nil {
			return err
		}
		a, err := vm.pop()
		if err != nil {
			return err
		}
		vm.push(math.Pow(a, b))
	case "neg", "abs", "ceiling", "floor", "round", "truncate", "sqrt", "sin", "cos", "ln", "log", "not":
		a, err := vm.pop()
		if err != nil {
			return err
		}
		switch tok {
		case "neg":
			vm.push(-a)
		case "abs":
			vm.push(math.Abs(a))
		case "ceiling":
			vm.push(math.Ceil(a))
		case "floor":
			vm.push(math.Floor(a))
		case "round":
			if a >= 0 {
				vm.push(math.Floor(a + 0.5))
			} else {
				vm.push(math.Ceil(a - 0.5))
			}
		case "truncate":
			vm.push(math.Trunc(a))
		case "sqrt":
			if a < 0 {
				return errors.New("calculator function: sqrt domain")
			}
			vm.push(math.Sqrt(a))
		case "sin":
			vm.push(math.Sin(a * math.Pi / 180))
		case "cos":
			vm.push(math.Cos(a * math.Pi / 180))
		case "ln":
			if a <= 0 {
				return errors.New("calculator function: ln domain")
			}
			vm.push(math.Log(a))
		case "log":
			if a <= 0 {
				return errors.New("calculator function: log domain")
			}
			vm.push(math.Log10(a))
		case "not":
			if a == 0 || a == 1 {
				vm.push(1 - a)
			} else {
				vm.push(float64(^uint32(int32(a))))
			}
		}
	case "and", "or", "xor", "bitshift", "eq", "ne", "gt", "ge", "lt", "le":
		b, err := vm.pop()
		if err != nil {
			return err
		}
		a, err := vm.pop()
		if err != nil {
			return err
		}
		pushBool := func(v bool) {
			if v {
				vm.push(1)
			} else {
				vm.push(0)
			}
		}
		switch tok {
		case "and":
			if (a == 0 || a == 1) && (b == 0 || b == 1) {
				pushBool(a != 0 && b != 0)
			} else {
				vm.push(float64(int32(a) & int32(b)))
			}
		case "or":
			if (a == 0 || a == 1) && (b == 0 || b == 1) {
				pushBool(a != 0 || b != 0)
			} else {
				vm.push(float64(int32(a) | int32(b)))
			}
		case "xor":
			if (a == 0 || a == 1) && (b == 0 || b == 1) {
				pushBool((a != 0) != (b != 0))
			} else {
				vm.push(float64(int32(a) ^ int32(b)))
			}
		case "bitshift":
			if b > 0 {
				vm.push(float64(int32(a) << uint(b)))
			} else {
				vm.push(float64(int32(a) >> uint(-b)))
			}
		case "eq":
			pushBool(a == b)
		case "ne":
			pushBool(a != b)
		case "gt":
			pushBool(a > b)
		case "ge":
			pushBool(a >= b)
		case "lt":
			pushBool(a < b)
		case "le":
			pushBool(a <= b)
		}
	case "dup", "exch", "pop", "copy", "index", "roll":
		switch tok {
		case "dup":
			a, err := vm.pop()
			if err != nil {
				return err
			}
			vm.push(a)
			vm.push(a)
		case "exch":
			b, err := vm.pop()
			if err != nil {
				return err
			}
			a, err := vm.pop()
			if err != nil {
				return err
			}
			vm.push(b)
			vm.push(a)
		case "pop":
			_, err := vm.pop()
			return err
		case "copy":
			n, err := vm.pop()
			if err != nil {
				return err
			}
			cnt := int(n)
			if cnt > 0 && cnt <= len(vm.stack) {
				src := vm.stack[len(vm.stack)-cnt:]
				vm.stack = append(vm.stack, src...)
			}
		case "index":
			n, err := vm.pop()
			if err != nil {
				return err
			}
			idx := int(n)
			if idx >= 0 && idx < len(vm.stack) {
				vm.push(vm.stack[len(vm.stack)-1-idx])
			} else {
				vm.push(0)
			}
		case "roll":
			j, err := vm.pop()
			if err != nil {
				return err
			}
			n, err := vm.pop()
			if err != nil {
				return err
			}
			cnt := int(n)
			if cnt > 0 && cnt <= len(vm.stack) {
				seg := vm.stack[len(vm.stack)-cnt:]
				k := int(j) % cnt
				if k < 0 {
					k += cnt
				}
				tmp := append([]float64(nil), seg[cnt-k:]...)
				tmp = append(tmp, seg[:cnt-k]...)
				copy(seg, tmp)
			}
		}
	default:
		return fmt.Errorf("calculator function: unsupported operator %q", tok)
	}
	return nil
}
