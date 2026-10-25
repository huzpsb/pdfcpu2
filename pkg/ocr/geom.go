// Copyright 2026 The pdfcpu Authors.
// Use of this source code is governed by an Apache-2.0 license that can be found in the LICENSE file.

package ocr

import (
	"math"
	"sort"
)

type vec2 struct{ x, y float32 }

func cross(o, a, b vec2) float32 {
	return (a.x-o.x)*(b.y-o.y) - (a.y-o.y)*(b.x-o.x)
}

// convexHull is the Andrew monotone chain (CCW), matching the C++ engine.
func convexHull(pts []vec2) []vec2 {
	sort.Slice(pts, func(i, j int) bool {
		return pts[i].x < pts[j].x || (pts[i].x == pts[j].x && pts[i].y < pts[j].y)
	})
	n := len(pts)
	if n <= 3 {
		return pts
	}
	h := make([]vec2, 2*n+1)
	k := 0
	for i := 0; i < n; i++ {
		for k >= 2 && cross(h[k-2], h[k-1], pts[i]) <= 0 {
			k--
		}
		h[k] = pts[i]
		k++
	}
	t := k + 1
	for i := n - 2; i >= 0; i-- {
		for k >= t && cross(h[k-2], h[k-1], pts[i]) <= 0 {
			k--
		}
		h[k] = pts[i]
		k++
	}
	return h[:k-1]
}

// minAreaRect returns the 4 corners of the minimum-area rectangle.
func minAreaRect(pts []vec2) [4]vec2 {
	hull := convexHull(pts)
	n := len(hull)
	if n < 3 {
		var mnx, mny, mxx, mxy float32 = 1e30, 1e30, -1e30, -1e30
		for _, p := range hull {
			mnx = minF32(mnx, p.x)
			mxx = maxF32(mxx, p.x)
			mny = minF32(mny, p.y)
			mxy = maxF32(mxy, p.y)
		}
		return [4]vec2{{mnx, mny}, {mxx, mny}, {mxx, mxy}, {mnx, mxy}}
	}
	bestArea := float32(1e30)
	var best [4]vec2
	for i := 0; i < n; i++ {
		a, b := hull[i], hull[(i+1)%n]
		ex, ey := b.x-a.x, b.y-a.y
		elen := float32(math.Sqrt(float64(ex*ex + ey*ey)))
		if elen < 1e-6 {
			continue
		}
		ex /= elen
		ey /= elen
		nx, ny := -ey, ex
		minU, maxU, minV, maxV := float32(1e30), float32(-1e30), float32(1e30), float32(-1e30)
		for _, p := range hull {
			u := (p.x-a.x)*ex + (p.y-a.y)*ey
			v := (p.x-a.x)*nx + (p.y-a.y)*ny
			minU = minF32(minU, u)
			maxU = maxF32(maxU, u)
			minV = minF32(minV, v)
			maxV = maxF32(maxV, v)
		}
		area := (maxU - minU) * (maxV - minV)
		if area < bestArea {
			bestArea = area
			best = [4]vec2{
				{a.x + ex*minU + nx*minV, a.y + ey*minU + ny*minV},
				{a.x + ex*maxU + nx*minV, a.y + ey*maxU + ny*minV},
				{a.x + ex*maxU + nx*maxV, a.y + ey*maxU + ny*maxV},
				{a.x + ex*minU + nx*maxV, a.y + ey*minU + ny*maxV},
			}
		}
	}
	return best
}

func polyArea(p []vec2) float32 {
	s := float32(0)
	for i := range p {
		a, b := p[i], p[(i+1)%len(p)]
		s += a.x*b.y - b.x*a.y
	}
	if s < 0 {
		s = -s
	}
	return s * 0.5
}

func polyPerimeter(p []vec2) float32 {
	s := float32(0)
	for i := range p {
		a, b := p[i], p[(i+1)%len(p)]
		dx, dy := b.x-a.x, b.y-a.y
		s += float32(math.Sqrt(float64(dx*dx + dy*dy)))
	}
	return s
}

// unclipPoly expands a polygon outward by d (miter offset).
func unclipPoly(p []vec2, d float32) []vec2 {
	area2 := float32(0)
	for i := range p {
		a, b := p[i], p[(i+1)%len(p)]
		area2 += a.x*b.y - b.x*a.y
	}
	var q []vec2
	if area2 < 0 {
		for i := len(p) - 1; i >= 0; i-- {
			q = append(q, p[i])
		}
	} else {
		q = append(q, p...)
	}
	m := len(q)
	out := make([]vec2, m)
	for i := 0; i < m; i++ {
		cur, prev, next := q[i], q[(i+m-1)%m], q[(i+1)%m]
		e1x, e1y := cur.x-prev.x, cur.y-prev.y
		l1 := float32(math.Sqrt(float64(e1x*e1x+e1y*e1y))) + 1e-9
		n1x, n1y := e1y/l1, -e1x/l1
		e2x, e2y := next.x-cur.x, next.y-cur.y
		l2 := float32(math.Sqrt(float64(e2x*e2x+e2y*e2y))) + 1e-9
		n2x, n2y := e2y/l2, -e2x/l2
		c1 := n1x*cur.x + n1y*cur.y + d
		c2 := n2x*cur.x + n2y*cur.y + d
		det := n1x*n2y - n2x*n1y
		if float32(math.Abs(float64(det))) < 1e-9 {
			out[i] = vec2{cur.x + n1x*d, cur.y + n1y*d}
		} else {
			out[i] = vec2{(c1*n2y - c2*n1y) / det, (n1x*c2 - n2x*c1) / det}
		}
	}
	return out
}

// orderQuad orders 4 points as tl, tr, br, bl (heuristic, same as C++).
func orderQuad(p [4]vec2) [4]vec2 {
	best := float32(1e30)
	s := -1
	for i := 0; i < 4; i++ {
		var cx, cy float32
		for k := 0; k < 4; k++ {
			cx += p[(i+k)%4].x / 4
			cy += p[(i+k)%4].y / 4
		}
		d := (p[i].x-cx)*(p[i].x-cx) + (p[i].y-cy)*(p[i].y-cy)
		if d < best {
			best = d
			s = i
		}
	}
	var t [4]vec2
	for k := 0; k < 4; k++ {
		t[k] = p[(s+k)%4]
	}
	area2 := float32(0)
	for i := 0; i < 4; i++ {
		a, b := t[i], t[(i+1)%4]
		area2 += a.x*b.y - b.x*a.y
	}
	var o [4]vec2
	if area2 < 0 {
		o[0], o[1], o[2], o[3] = t[0], t[3], t[2], t[1]
	} else {
		o = t
	}
	mnx, mny := float32(1e30), float32(1e30)
	for i := 0; i < 4; i++ {
		mnx = minF32(mnx, o[i].x)
		mny = minF32(mny, o[i].y)
	}
	tl, bd := 0, float32(1e30)
	for i := 0; i < 4; i++ {
		d := (o[i].x-mnx)*(o[i].x-mnx) + (o[i].y-mny)*(o[i].y-mny)
		if d < bd {
			bd = d
			tl = i
		}
	}
	for k := 0; k < 4; k++ {
		t[k] = o[(tl+k)%4]
	}
	return t
}

// sampleRGB bilinear-samples an RGB8 image.
func sampleRGB(img []uint8, w, h int, x, y float32, out *[3]float32) {
	x = maxF32(0, minF32(float32(w-1), x))
	y = maxF32(0, minF32(float32(h-1), y))
	x0, y0 := int(x), int(y)
	x1, y1 := minInt(w-1, x0+1), minInt(h-1, y0+1)
	fx, fy := x-float32(x0), y-float32(y0)
	for c := 0; c < 3; c++ {
		out[c] = float32(img[(y0*w+x0)*3+c])*(1-fx)*(1-fy) +
			float32(img[(y0*w+x1)*3+c])*fx*(1-fy) +
			float32(img[(y1*w+x0)*3+c])*(1-fx)*fy +
			float32(img[(y1*w+x1)*3+c])*fx*fy
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func cubicW(t, a float32) float32 {
	if t < 0 {
		t = -t
	}
	t2 := t * t
	if t <= 1 {
		return (a+2)*t*t2 - (a+3)*t2 + 1
	}
	if t < 2 {
		return a*t*t2 - 5*a*t2 + 8*a*t - 4*a
	}
	return 0
}

// sampleBicubic samples with cv2 INTER_CUBIC semantics (a=-0.75).
func sampleBicubic(img []uint8, w, h int, x, y float32, out *[3]float32) {
	x = maxF32(0, minF32(float32(w-1), x))
	y = maxF32(0, minF32(float32(h-1), y))
	x0 := int(math.Floor(float64(x))) - 1
	y0 := int(math.Floor(float64(y))) - 1
	fx := x - float32(x0+1)
	fy := y - float32(y0+1)
	var acc [3]float32
	for dy := 0; dy < 4; dy++ {
		sy := minInt(h-1, maxInt(0, y0+dy))
		wy := cubicW(fy-float32(dy), -0.75)
		if wy == 0 {
			continue
		}
		for dx := 0; dx < 4; dx++ {
			sx := minInt(w-1, maxInt(0, x0+dx))
			wt := wy * cubicW(fx-float32(dx), -0.75)
			if wt == 0 {
				continue
			}
			p := img[(sy*w+sx)*3 : (sy*w+sx)*3+3]
			for c := 0; c < 3; c++ {
				acc[c] += wt * float32(p[c])
			}
		}
	}
	for c := 0; c < 3; c++ {
		out[c] = maxF32(0, minF32(255, acc[c]))
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// solveH solves the 8x8 system for a homography mapping dst -> src.
func solveH(dst, src [4]vec2) ([9]float32, bool) {
	var a [8][9]float32
	for i := 0; i < 4; i++ {
		u, v := dst[i].x, dst[i].y
		x, y := src[i].x, src[i].y
		a[2*i][0], a[2*i][1], a[2*i][2] = u, v, 1
		a[2*i][6], a[2*i][7], a[2*i][8] = -u*x, -v*x, x
		a[2*i+1][3], a[2*i+1][4], a[2*i+1][5] = u, v, 1
		a[2*i+1][6], a[2*i+1][7], a[2*i+1][8] = -u*y, -v*y, y
	}
	for col := 0; col < 8; col++ {
		piv := col
		for r := col + 1; r < 8; r++ {
			if absF32(a[r][col]) > absF32(a[piv][col]) {
				piv = r
			}
		}
		if absF32(a[piv][col]) < 1e-12 {
			return [9]float32{}, false
		}
		if piv != col {
			a[col], a[piv] = a[piv], a[col]
		}
		for r := 0; r < 8; r++ {
			if r == col {
				continue
			}
			f := a[r][col] / a[col][col]
			for c := col; c < 9; c++ {
				a[r][c] -= f * a[col][c]
			}
		}
	}
	var h [9]float32
	for i := 0; i < 8; i++ {
		h[i] = a[i][8] / a[i][i]
	}
	h[8] = 1
	return h, true
}

func absF32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}
