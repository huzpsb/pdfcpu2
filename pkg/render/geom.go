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

// Package render rasterizes PDF pages.
//
// It implements a self-contained content stream interpreter that turns page
// content (vector paths, text, images and shadings) into bitmaps. Text that
// uses fonts without an embedded font program is rendered using an embedded
// fallback font.
package render

import "math"

// mat is an affine 2D transformation matrix in PDF notation:
//
//	| a b 0 |
//	| c d 0 |
//	| e f 1 |
type mat struct {
	a, b, c, d, e, f float64
}

func ident() mat { return mat{1, 0, 0, 1, 0, 0} }

func translation(tx, ty float64) mat { return mat{1, 0, 0, 1, tx, ty} }

func scaling(sx, sy float64) mat { return mat{sx, 0, 0, sy, 0, 0} }

// mul returns m1 x m2 (apply m2 first, then m1).
func (m1 mat) mul(m2 mat) mat {
	return mat{
		a: m1.a*m2.a + m1.b*m2.c,
		b: m1.a*m2.b + m1.b*m2.d,
		c: m1.c*m2.a + m1.d*m2.c,
		d: m1.c*m2.b + m1.d*m2.d,
		e: m1.a*m2.e + m1.c*m2.f + m1.e,
		f: m1.b*m2.e + m1.d*m2.f + m1.f,
	}
}

func (m mat) apply(x, y float64) (float64, float64) {
	return m.a*x + m.c*y + m.e, m.b*x + m.d*y + m.f
}

func (m mat) det() float64 { return m.a*m.d - m.b*m.c }

// invert returns the inverse of m and reports whether it exists.
func (m mat) invert() (mat, bool) {
	det := m.det()
	if math.Abs(det) < 1e-12 {
		return mat{}, false
	}
	inv := 1 / det
	return mat{
		a: m.d * inv,
		b: -m.b * inv,
		c: -m.c * inv,
		d: m.a * inv,
		e: (m.b*m.f - m.d*m.e) * inv,
		f: (m.c*m.e - m.a*m.f) * inv,
	}, true
}

// pt is a point in device space.
type pt struct{ x, y float64 }

// subPath is a flattened, implicitly closed polygon.
type subPath struct {
	pts []pt
}

func (sp *subPath) close() {
	n := len(sp.pts)
	if n > 1 && sp.pts[0] != sp.pts[n-1] {
		sp.pts = append(sp.pts, sp.pts[0])
	}
}

// bbox returns the bounding box of all subpaths.
func bbox(sps []subPath) (x0, y0, x1, y1 float64, ok bool) {
	first := true
	for i := range sps {
		for _, p := range sps[i].pts {
			if first {
				x0, y0, x1, y1, first = p.x, p.y, p.x, p.y, false
				continue
			}
			x0 = math.Min(x0, p.x)
			y0 = math.Min(y0, p.y)
			x1 = math.Max(x1, p.x)
			y1 = math.Max(y1, p.y)
		}
	}
	return x0, y0, x1, y1, !first
}

// transform applies m to all points of sps.
func transformPath(sps []subPath, m mat) []subPath {
	if m == ident() {
		return sps
	}
	out := make([]subPath, len(sps))
	for i := range sps {
		out[i].pts = make([]pt, len(sps[i].pts))
		for j, p := range sps[i].pts {
			out[i].pts[j].x, out[i].pts[j].y = m.apply(p.x, p.y)
		}
	}
	return out
}

// flattenCubic appends points of the cubic bezier from p0 via c1, c2 to p3.
func flattenCubic(dst []pt, p0, c1, c2, p3 pt, tol float64) []pt {
	// Estimate required subdivision count from control polygon length.
	l := dist(p0, c1) + dist(c1, c2) + dist(c2, p3)
	n := int(l/tol) + 2
	if n > 64 {
		n = 64
	}
	if n < 4 {
		n = 4
	}
	for i := 1; i <= n; i++ {
		t := float64(i) / float64(n)
		u := 1 - t
		w0 := u * u * u
		w1 := 3 * u * u * t
		w2 := 3 * u * t * t
		w3 := t * t * t
		dst = append(dst, pt{
			w0*p0.x + w1*c1.x + w2*c2.x + w3*p3.x,
			w0*p0.y + w1*c1.y + w2*c2.y + w3*p3.y,
		})
	}
	return dst
}

// flattenQuad appends points of the quadratic bezier from p0 via c to p1.
func flattenQuad(dst []pt, p0, c, p1 pt, tol float64) []pt {
	l := dist(p0, c) + dist(c, p1)
	n := int(l/tol) + 2
	if n > 48 {
		n = 48
	}
	if n < 3 {
		n = 3
	}
	for i := 1; i <= n; i++ {
		t := float64(i) / float64(n)
		u := 1 - t
		dst = append(dst, pt{
			u*u*p0.x + 2*u*t*c.x + t*t*p1.x,
			u*u*p0.y + 2*u*t*c.y + t*t*p1.y,
		})
	}
	return dst
}

func dist(a, b pt) float64 {
	return math.Hypot(a.x-b.x, a.y-b.y)
}
