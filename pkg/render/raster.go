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
	"image/color"
	"math"
)

// subSamples is the number of vertical subsampling lines per pixel used for
// anti-aliased rasterization.
const subSamples = 4

// rgba is an 8 bit per channel non-premultiplied color.
type rgba struct {
	r, g, b, a uint8
}

var colorWhite = rgba{255, 255, 255, 255}

func (c rgba) premul() (uint8, uint8, uint8, uint8) {
	if c.a == 255 {
		return c.r, c.g, c.b, c.a
	}
	return uint8(uint32(c.r) * uint32(c.a) / 255),
		uint8(uint32(c.g) * uint32(c.a) / 255),
		uint8(uint32(c.b) * uint32(c.a) / 255),
		c.a
}

// canvas is a premultiplied RGBA raster surface.
type canvas struct {
	img *image.RGBA
	w   int
	h   int
	// cov is a reusable per pixel coverage buffer.
	cov []float32
}

func newCanvas(w, h int) *canvas {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return &canvas{
		img: image.NewRGBA(image.Rect(0, 0, w, h)),
		w:   w,
		h:   h,
		cov: make([]float32, w*h),
	}
}

func (cv *canvas) fillWhite() {
	for i := range cv.img.Pix {
		cv.img.Pix[i] = 0xff
	}
}

// blend composites s over the pixel at (x,y) honoring a coverage value in [0,1].
func (cv *canvas) blend(x, y int, c rgba, cov float64) {
	if cov <= 0 || c.a == 0 {
		return
	}
	if cov > 1 {
		cov = 1
	}
	a := uint32(float64(c.a) * cov)
	if a > 255 {
		a = 255
	}
	sr, sg, sb, _ := c.premul()
	i := cv.img.PixOffset(x, y)
	p := cv.img.Pix[i:]
	da := uint32(p[3])
	// Premultiplied source with scaled alpha.
	sra := uint32(sr) * a / 255
	sga := uint32(sg) * a / 255
	sba := uint32(sb) * a / 255
	inv := 255 - a
	p[0] = uint8(sra + uint32(p[0])*inv/255)
	p[1] = uint8(sga + uint32(p[1])*inv/255)
	p[2] = uint8(sba + uint32(p[2])*inv/255)
	p[3] = uint8(a + da*inv/255)
}

// blendPixel composites s with full coverage.
func (cv *canvas) blendPixel(x, y int, c rgba) { cv.blend(x, y, c, 1) }

// rasterize computes anti-aliased coverage for sps into cov, restricted to the
// pixel rectangle [x0,x1) x [y0,y1) of the canvas. It returns the dirty
// rectangle actually touched.
func (cv *canvas) rasterize(sps []subPath, evenOdd bool) (image.Rectangle, bool) {
	bx0, by0, bx1, by1, ok := bbox(sps)
	if !ok {
		return image.Rectangle{}, false
	}
	x0 := int(math.Floor(bx0))
	y0 := int(math.Floor(by0))
	x1 := int(math.Ceil(bx1)) + 1
	y1 := int(math.Ceil(by1)) + 1
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > cv.w {
		x1 = cv.w
	}
	if y1 > cv.h {
		y1 = cv.h
	}
	if x0 >= x1 || y0 >= y1 {
		return image.Rectangle{}, false
	}

	// Collect edges.
	type edge struct {
		x1, y1, x2, y2 float64 // from (x1,y1) to (x2,y2), y1 < y2
		dir            float64
	}
	edges := make([]edge, 0, 64)
	for i := range sps {
		pts := sps[i].pts
		n := len(pts)
		if n < 2 {
			continue
		}
		for j := 0; j < n-1; j++ {
			p, q := pts[j], pts[j+1]
			if p.y == q.y {
				continue
			}
			if p.y < q.y {
				edges = append(edges, edge{p.x, p.y, q.x, q.y, 1})
			} else {
				edges = append(edges, edge{q.x, q.y, p.x, p.y, -1})
			}
		}
	}
	if len(edges) == 0 {
		return image.Rectangle{}, false
	}

	cross := make([]float64, 0, 32)
	dirs := make([]float64, 0, 32)

	for row := y0; row < y1; row++ {
		base := row * cv.w
		for sub := 0; sub < subSamples; sub++ {
			y := float64(row) + (float64(sub)+0.5)/subSamples
			cross = cross[:0]
			dirs = dirs[:0]
			for i := range edges {
				e := &edges[i]
				if e.y1 <= y && y < e.y2 {
					t := (y - e.y1) / (e.y2 - e.y1)
					cross = append(cross, e.x1+t*(e.x2-e.x1))
					dirs = append(dirs, e.dir)
				}
			}
			if len(cross) < 2 {
				continue
			}
			sortCrossings(cross, dirs)
			// Derive spans according to the fill rule.
			i := 0
			for i < len(cross)-1 {
				if evenOdd {
					// Pairs of crossings (0,1), (2,3), ...
					emitSpan(cv.cov[base:base+cv.w], cross[i], cross[i+1], 1.0/subSamples, x0, x1)
					i += 2
					continue
				}
				wind := 0.0
				j := i
				for ; j < len(cross); j++ {
					wind += dirs[j]
					if wind == 0 {
						break
					}
				}
				if j > i {
					emitSpan(cv.cov[base:base+cv.w], cross[i], cross[j], 1.0/subSamples, x0, x1)
				}
				i = j + 1
			}
		}
	}

	return image.Rect(x0, y0, x1, y1), true
}

func sortCrossings(x []float64, d []float64) {
	for i := 1; i < len(x); i++ {
		for j := i; j > 0 && x[j] < x[j-1]; j-- {
			x[j], x[j-1] = x[j-1], x[j]
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

// emitSpan adds coverage of weight w for the horizontal span [xa,xb) into row.
func emitSpan(row []float32, xa, xb, w float64, x0, x1 int) {
	if xb <= xa {
		return
	}
	i0 := int(math.Floor(xa))
	i1 := int(math.Ceil(xb))
	if i0 < x0 {
		i0 = x0
	}
	if i1 > x1 {
		i1 = x1
	}
	for x := i0; x < i1; x++ {
		l := float64(x)
		r := l + 1
		ovl := math.Min(r, xb) - math.Max(l, xa)
		if ovl > 0 {
			row[x] += float32(ovl * w)
		}
	}
}

// fill paints sps with c modulated by alpha and the clip mask.
func (cv *canvas) fill(sps []subPath, evenOdd bool, c rgba, alpha float64, clipMask *image.Alpha) {
	if alpha <= 0 || c.a == 0 {
		return
	}
	rect, ok := cv.rasterize(sps, evenOdd)
	if !ok {
		return
	}
	col := rgba{c.r, c.g, c.b, c.a}
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		base := y * cv.w
		for x := rect.Min.X; x < rect.Max.X; x++ {
			cov := cv.cov[base+x]
			if cov <= 0 {
				continue
			}
			cv.cov[base+x] = 0
			a := alpha
			if clipMask != nil {
				a *= float64(clipMask.AlphaAt(x, y).A) / 255
			}
			if a <= 0 {
				continue
			}
			cv.blend(x, y, col, float64(cov)*a)
		}
	}
	// Zero any remaining coverage outside the fast loop (safety).
	cv.clearCoverage(rect)
}

func (cv *canvas) clearCoverage(rect image.Rectangle) {
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		base := y * cv.w
		for x := rect.Min.X; x < rect.Max.X; x++ {
			cv.cov[base+x] = 0
		}
	}
}

// clipMask returns the coverage of sps as an alpha mask.
func (cv *canvas) clipMask(sps []subPath, evenOdd bool) *image.Alpha {
	m := image.NewAlpha(image.Rect(0, 0, cv.w, cv.h))
	rect, ok := cv.rasterize(sps, evenOdd)
	if !ok {
		return m
	}
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		base := y * cv.w
		for x := rect.Min.X; x < rect.Max.X; x++ {
			cov := cv.cov[base+x]
			cv.cov[base+x] = 0
			if cov > 0 {
				v := uint32(cov * 255)
				if v > 255 {
					v = 255
				}
				m.Pix[m.PixOffset(x, y)] = uint8(v)
			}
		}
	}
	cv.clearCoverage(rect)
	return m
}

// intersectMasks returns a * b without mutating either operand.
func intersectMasks(a, b *image.Alpha) *image.Alpha {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := image.NewAlpha(a.Rect)
	for i := range a.Pix {
		out.Pix[i] = uint8(uint32(a.Pix[i]) * uint32(b.Pix[i]) / 255)
	}
	return out
}

// Stroke attributes.
const (
	capButt = iota
	capRound
	capProjecting
	joinMiter
	joinRound
	joinBevel
)

// dashPath splits flattened subpaths into dash segments.
func dashPath(sps []subPath, dash []float64, phase float64) []subPath {
	if len(dash) == 0 {
		return sps
	}
	var out []subPath
	total := 0.0
	for _, d := range dash {
		if d < 0 {
			return sps
		}
		total += d
	}
	if total == 0 {
		return sps
	}
	for phase < 0 {
		phase += total
	}
	phase = math.Mod(phase, total)
	idx := 0
	// Advance to starting pattern position.
	rem := dash[idx] - phase
	on := true
	var cur []pt
	flush := func() {
		if len(cur) > 1 {
			out = append(out, subPath{pts: cur})
		}
		cur = nil
	}
	for i := range sps {
		pts := sps[i].pts
		for j := 0; j < len(pts)-1; j++ {
			p, q := pts[j], pts[j+1]
			l := dist(p, q)
			if l == 0 {
				continue
			}
			ux, uy := (q.x-p.x)/l, (q.y-p.y)/l
			pos := 0.0
			for pos < l {
				step := math.Min(rem, l-pos)
				nx, ny := p.x+ux*(pos+step), p.y+uy*(pos+step)
				if on {
					if cur == nil {
						cur = append(cur, pt{p.x + ux*pos, p.y + uy*pos})
					}
					cur = append(cur, pt{nx, ny})
				} else {
					flush()
				}
				rem -= step
				pos += step
				if rem <= 1e-9 {
					idx = (idx + 1) % len(dash)
					rem = dash[idx]
					on = !on
					if !on {
						flush()
					}
				}
			}
		}
	}
	flush()
	return out
}

// strokeOutline converts flattened subpaths into fillable outline subpaths
// that can be rasterized with the non-zero winding fill rule.
func strokeOutline(sps []subPath, width float64, capStyle, joinStyle int, miterLimit float64) []subPath {
	if width <= 0 {
		return nil
	}
	hw := width / 2
	if miterLimit <= 0 {
		miterLimit = 10
	}
	var out []subPath

	addClosed := func(pts []pt) {
		sp := subPath{pts: pts}
		sp.close()
		out = append(out, sp)
	}

	for i := range sps {
		pts := append([]pt(nil), sps[i].pts...)
		if len(pts) < 2 {
			continue
		}
		closed := dist(pts[0], pts[len(pts)-1]) < 1e-9
		if closed {
			pts = pts[:len(pts)-1]
		}
		n := len(pts)
		if n < 2 {
			continue
		}

		left := offsetSide(pts, hw, 1, closed, miterLimit, joinStyle)
		right := offsetSide(pts, hw, -1, closed, miterLimit, joinStyle)
		if len(left) < 2 || len(right) < 2 {
			continue
		}
		// Reverse the right side so it runs back from the end to the start.
		rev := make([]pt, len(right))
		for k, p := range right {
			rev[len(right)-1-k] = p
		}
		if closed {
			var contour []pt
			contour = append(contour, left...)
			contour = append(contour, rev...)
			addClosed(contour)
		} else {
			var contour []pt
			contour = append(contour, left...)
			contour = append(contour, capArc(pts[n-1], pts[n-2], hw, capStyle)...)
			contour = append(contour, rev...)
			contour = append(contour, capArc(pts[0], pts[1], hw, capStyle)...)
			addClosed(contour)
			// Round joins add wedges on top of the ring.
			if joinStyle == joinRound {
				for k := 1; k < n-1; k++ {
					addClosed(arcPoints(pts[k], hw, 0, 2*math.Pi))
				}
			}
		}
	}
	return out
}

// offsetSide computes one offset contour of a polyline at distance hw. sign
// +1 offsets to the left of the path direction, -1 to the right.
func offsetSide(pts []pt, hw float64, sign float64, closed bool, miterLimit float64, joinStyle int) []pt {
	n := len(pts)
	segCount := n - 1
	if closed {
		segCount = n
	}
	type seg struct{ nx, ny float64 }
	segs := make([]seg, segCount)
	for k := 0; k < segCount; k++ {
		a, b := pts[k], pts[(k+1)%n]
		l := dist(a, b)
		if l == 0 {
			segs[k] = seg{0, 0}
		} else {
			segs[k] = seg{sign * -(b.y - a.y) / l, sign * (b.x - a.x) / l}
		}
	}
	var out []pt
	for k := 0; k < segCount; k++ {
		if segs[k].nx == 0 && segs[k].ny == 0 {
			continue
		}
		a := pts[k]
		pk := k - 1
		if pk < 0 {
			if closed {
				pk = segCount - 1
			} else {
				pk = -1
			}
		}
		if pk >= 0 && segs[pk].nx != 0 {
			out = append(out, joinPoints(a, segs[pk].nx, segs[pk].ny, segs[k].nx, segs[k].ny, hw, miterLimit, joinStyle)...)
		} else {
			out = append(out, pt{a.x + segs[k].nx * hw, a.y + segs[k].ny * hw})
		}
	}
	if !closed && segCount > 0 {
		last := segCount - 1
		if segs[last].nx != 0 || segs[last].ny != 0 {
			b := pts[n-1]
			out = append(out, pt{b.x + segs[last].nx * hw, b.y + segs[last].ny * hw})
		}
	}
	return out
}

// joinPoints returns the offset contour points at a vertex between two
// segments with unit normals n1 (incoming) and n2 (outgoing): a miter point
// where the offset lines intersect within the miter limit, or both segment
// endpoints (bevel) otherwise.
func joinPoints(v pt, n1x, n1y, n2x, n2y float64, hw, miterLimit float64, joinStyle int) []pt {
	p1 := pt{v.x + n1x*hw, v.y + n1y*hw}
	p2 := pt{v.x + n2x*hw, v.y + n2y*hw}
	if joinStyle == joinRound {
		return []pt{p1}
	}
	// Direction of the incoming segment from its normal.
	d1x, d1y := n1y, -n1x
	d2x, d2y := n2y, -n2x
	den := d1x*d2y - d1y*d2x
	if math.Abs(den) < 1e-9 {
		return []pt{p1}
	}
	ex, ey := p2.x-p1.x, p2.y-p1.y
	t := (ex*d2y - ey*d2x) / den
	m := pt{p1.x + t*d1x, p1.y + t*d1y}
	if !(m.x == m.x && m.y == m.y) { // NaN guard
		return []pt{p1, p2}
	}
	if dist(m, v) > miterLimit*hw {
		return []pt{p1, p2} // bevel
	}
	return []pt{m}
}

// capArc returns the cap polygon points at vertex v for a stroke segment
// pointing from prev to v. The points connect the left offset end point to the
// right offset start point.
func capArc(v, prev pt, hw float64, capStyle int) []pt {
	dx, dy := v.x-prev.x, v.y-prev.y
	l := math.Hypot(dx, dy)
	if l == 0 {
		return nil
	}
	dx, dy = dx/l, dy/l
	switch capStyle {
	case capRound:
		// Half circle from the left offset point around the outward
		// direction to the right offset point.
		// Left normal is (-dy, dx) with angle atan2(dx, dy).
		start := math.Atan2(dx, dy)
		return arcPoints(v, hw, start, math.Pi)
	case capProjecting:
		// Square cap: extend by hw in direction (v - prev).
		lx, ly := -dy*hw, dx*hw
		return []pt{
			{v.x + dx*hw + lx, v.y + dy*hw + ly},
			{v.x + dx*hw - lx, v.y + dy*hw - ly},
		}
	default:
		// Butt cap: no additional points.
		return nil
	}
}

// arcPoints returns points along a circular arc centered at c with radius r,
// starting at angle start sweeping sweep radians.
func arcPoints(c pt, r float64, start, sweep float64) []pt {
	const steps = 12
	var dst []pt
	for i := 0; i <= steps; i++ {
		a := start + sweep*float64(i)/steps
		dst = append(dst, pt{c.x + r*math.Cos(a), c.y + r*math.Sin(a)})
	}
	return dst
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func rgbaOf(c color.Color) rgba {
	r, g, b, a := c.RGBA()
	return rgba{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}
