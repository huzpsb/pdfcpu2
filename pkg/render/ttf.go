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
	"encoding/binary"
	"errors"
)

// ttfFont is a tolerant TrueType outline reader. It extracts glyph outlines
// directly from the loca/glyf tables and therefore copes with subset fonts
// that the stricter sfnt package rejects.
type ttfFont struct {
	upem       float64
	numGlyphs  int
	locaFormat int
	loca, glyf []byte
	cmap       cmapSubtable
}

type cmapSubtable struct {
	format int
	// format 4
	segCount  int
	endCodes  []uint16
	starts    []uint16
	deltas    []int16
	rangeOffs []uint16
	glyphIds  []byte
	// format 12
	groups []cmapGroup
}

type cmapGroup struct {
	start, end, startGID uint32
}

func ttfTable(data []byte, tag string) ([]byte, bool) {
	if len(data) < 12 {
		return nil, false
	}
	// Support font collections by using the first font.
	if string(data[:4]) == "ttcf" {
		if len(data) < 16 {
			return nil, false
		}
		off := binary.BigEndian.Uint32(data[12:])
		if int(off)+12 <= len(data) {
			data = data[off:]
		}
	}
	numTables := int(binary.BigEndian.Uint16(data[4:]))
	for i := 0; i < numTables; i++ {
		e := 12 + i*16
		if e+16 > len(data) {
			return nil, false
		}
		if string(data[e:e+4]) == tag {
			off := binary.BigEndian.Uint32(data[e+8:])
			l := binary.BigEndian.Uint32(data[e+12:])
			if int64(off)+int64(l) > int64(len(data)) {
				// Tolerate overlong declared lengths (common in subsets).
				l = uint32(len(data)) - off
			}
			if int64(off)+int64(l) > int64(len(data)) {
				return nil, false
			}
			return data[off : off+l], true
		}
	}
	return nil, false
}

// parseTTF extracts the tables needed for glyph outlines.
func parseTTF(data []byte) (*ttfFont, error) {
	head, ok := ttfTable(data, "head")
	if !ok || len(head) < 54 {
		return nil, errors.New("ttf: missing head")
	}
	maxp, ok := ttfTable(data, "maxp")
	if !ok || len(maxp) < 6 {
		return nil, errors.New("ttf: missing maxp")
	}
	loca, ok := ttfTable(data, "loca")
	if !ok {
		return nil, errors.New("ttf: missing loca")
	}
	glyf, ok := ttfTable(data, "glyf")
	if !ok {
		return nil, errors.New("ttf: missing glyf")
	}
	tf := &ttfFont{
		upem:       float64(binary.BigEndian.Uint16(head[18:20])),
		numGlyphs:  int(binary.BigEndian.Uint16(maxp[4:6])),
		locaFormat: int(int16(binary.BigEndian.Uint16(head[50:52]))),
		loca:       loca,
		glyf:       glyf,
	}
	if tf.upem < 16 || tf.upem > 16384 {
		tf.upem = 1000
	}
	if tf.numGlyphs < 1 {
		return nil, errors.New("ttf: no glyphs")
	}
	if cm, ok := ttfTable(data, "cmap"); ok {
		tf.cmap = parseCmapTable(cm)
	}
	return tf, nil
}

// glyphBounds returns the byte range of a glyph in glyf.
func (tf *ttfFont) glyphRange(gid int) (int, int, bool) {
	if gid < 0 || gid >= tf.numGlyphs {
		return 0, 0, false
	}
	var start, end int
	if tf.locaFormat == 1 {
		if (gid+1)*4 > len(tf.loca) {
			return 0, 0, false
		}
		start = int(binary.BigEndian.Uint32(tf.loca[gid*4:]))
		end = int(binary.BigEndian.Uint32(tf.loca[(gid+1)*4:]))
	} else {
		if (gid+1)*2 > len(tf.loca) {
			return 0, 0, false
		}
		start = int(binary.BigEndian.Uint16(tf.loca[gid*2:])) * 2
		end = int(binary.BigEndian.Uint16(tf.loca[(gid+1)*2:])) * 2
	}
	if start > end || end > len(tf.glyf) {
		return 0, 0, false
	}
	return start, end, start != end
}

// glyphID resolves a rune through the cmap (best effort).
func (tf *ttfFont) glyphID(r rune) int {
	if tf.cmap.format == 4 {
		c := uint16(r)
		if r > 0xFFFF {
			return 0
		}
		for i := 0; i < tf.cmap.segCount; i++ {
			if c <= tf.cmap.endCodes[i] {
				if c < tf.cmap.starts[i] {
					return 0
				}
				if tf.cmap.rangeOffs[i] == 0 {
					return int(int32(c) + int32(tf.cmap.deltas[i])) & 0xFFFF
				}
				// Range offset addressing.
				idx := int(tf.cmap.rangeOffs[i])/2 + int(c-tf.cmap.starts[i]) - tf.cmap.segCount + i
				if idx*2+1 < len(tf.cmap.glyphIds) {
					g := int(binary.BigEndian.Uint16(tf.cmap.glyphIds[idx*2:]))
					if g != 0 {
						return g
					}
				}
				return 0
			}
		}
		return 0
	}
	if tf.cmap.format == 12 {
		c := uint32(r)
		lo, hi := 0, len(tf.cmap.groups)-1
		for lo <= hi {
			mid := (lo + hi) / 2
			g := tf.cmap.groups[mid]
			if c < g.start {
				hi = mid - 1
			} else if c > g.end {
				lo = mid + 1
			} else {
				return int(g.startGID + c - g.start)
			}
		}
	}
	return 0
}

func parseCmapTable(cm []byte) cmapSubtable {
	if len(cm) < 4 {
		return cmapSubtable{}
	}
	n := int(binary.BigEndian.Uint16(cm[2:4]))
	var best cmapSubtable
	for i := 0; i < n; i++ {
		e := 4 + i*8
		if e+8 > len(cm) {
			break
		}
		off := int(binary.BigEndian.Uint32(cm[e+4:]))
		if off+2 > len(cm) {
			continue
		}
		format := int(binary.BigEndian.Uint16(cm[off:]))
		sub, ok := parseCmapSubtable(cm[off:], format)
		if ok {
			// Prefer Unicode formats.
			if format == 4 || best.format == 0 {
				best = sub
			}
			if format == 4 {
				return best
			}
		}
	}
	return best
}

func parseCmapSubtable(b []byte, format int) (cmapSubtable, bool) {
	switch format {
	case 4:
		if len(b) < 14 {
			return cmapSubtable{}, false
		}
		segCountX2 := int(binary.BigEndian.Uint16(b[6:8]))
		segCount := segCountX2 / 2
		need := 14 + segCountX2*4 + 2
		if segCount == 0 || len(b) < need {
			return cmapSubtable{}, false
		}
		st := cmapSubtable{format: 4, segCount: segCount}
		p := 14
		st.endCodes = make([]uint16, segCount)
		for i := 0; i < segCount; i++ {
			st.endCodes[i] = binary.BigEndian.Uint16(b[p+i*2:])
		}
		p += segCountX2 + 2
		st.starts = make([]uint16, segCount)
		for i := 0; i < segCount; i++ {
			st.starts[i] = binary.BigEndian.Uint16(b[p+i*2:])
		}
		p += segCountX2
		st.deltas = make([]int16, segCount)
		for i := 0; i < segCount; i++ {
			st.deltas[i] = int16(binary.BigEndian.Uint16(b[p+i*2:]))
		}
		p += segCountX2
		st.rangeOffs = make([]uint16, segCount)
		for i := 0; i < segCount; i++ {
			st.rangeOffs[i] = binary.BigEndian.Uint16(b[p+i*2:])
		}
		if len(b) > need {
			st.glyphIds = b[need:]
		}
		return st, true
	case 12:
		if len(b) < 16 {
			return cmapSubtable{}, false
		}
		nGroups := int(binary.BigEndian.Uint32(b[12:16]))
		if nGroups <= 0 || len(b) < 16+nGroups*12 {
			return cmapSubtable{}, false
		}
		st := cmapSubtable{format: 12}
		for i := 0; i < nGroups; i++ {
			e := 16 + i*12
			st.groups = append(st.groups, cmapGroup{
				binary.BigEndian.Uint32(b[e:]),
				binary.BigEndian.Uint32(b[e+4:]),
				binary.BigEndian.Uint32(b[e+8:]),
			})
		}
		return st, true
	}
	return cmapSubtable{}, false
}

// ttfGlyphOutline returns the outline of gid in font units, y axis down.
func ttfGlyphOutline(tf *ttfFont, gid int, depth int) []subPath {
	start, end, ok := tf.glyphRange(gid)
	if !ok || depth > 8 {
		return nil
	}
	g := tf.glyf[start:end]
	if len(g) < 10 {
		return nil
	}
	nContours := int(int16(binary.BigEndian.Uint16(g[:2])))
	if nContours >= 0 {
		return simpleGlyphOutline(g, nContours)
	}
	// Composite glyph.
	var out []subPath
	i := 10
	for {
		if i+4 > len(g) {
			break
		}
		flags := int(binary.BigEndian.Uint16(g[i:]))
		idx := int(binary.BigEndian.Uint16(g[i+2:]))
		i += 4
		var dx, dy float64
		if flags&0x0001 != 0 {
			if i+4 > len(g) {
				break
			}
			dx = float64(int16(binary.BigEndian.Uint16(g[i:])))
			dy = float64(int16(binary.BigEndian.Uint16(g[i+2:])))
			i += 4
		} else {
			if i+2 > len(g) {
				break
			}
			dx = float64(int8(g[i]))
			dy = float64(int8(g[i+1]))
			i += 2
		}
		if flags&0x0002 == 0 {
			// Point matching compound positioning: rare, treat as no offset.
			dx, dy = 0, 0
		}
		var a, b, c, dd float64 = 1, 0, 0, 1
		switch {
		case flags&0x0008 != 0: // WE_HAVE_A_SCALE
			if i+2 > len(g) {
				return out
			}
			s := float64(int16(binary.BigEndian.Uint16(g[i:]))) / 16384
			a, dd = s, s
			i += 2
		case flags&0x0040 != 0: // X_AND_Y_SCALE
			if i+4 > len(g) {
				return out
			}
			a = float64(int16(binary.BigEndian.Uint16(g[i:]))) / 16384
			dd = float64(int16(binary.BigEndian.Uint16(g[i+2:]))) / 16384
			i += 4
		case flags&0x0080 != 0: // TWO_BY_TWO
			if i+8 > len(g) {
				return out
			}
			a = float64(int16(binary.BigEndian.Uint16(g[i:]))) / 16384
			b = float64(int16(binary.BigEndian.Uint16(g[i+2:]))) / 16384
			c = float64(int16(binary.BigEndian.Uint16(g[i+4:]))) / 16384
			dd = float64(int16(binary.BigEndian.Uint16(g[i+6:]))) / 16384
			i += 8
		}
		sub := ttfGlyphOutline(tf, idx, depth+1)
		// Points are stored y-flipped (y axis down): adjust the component
		// transform accordingly (X' = aX - cY + dx, Y' = -bX + dY - dy).
		for _, sp := range sub {
			var np []pt
			for _, p := range sp.pts {
				np = append(np, pt{a*p.x - c*p.y + dx, -b*p.x + dd*p.y - dy})
			}
			if len(np) > 1 {
				nsp := subPath{pts: np}
				nsp.close()
				out = append(out, nsp)
			}
		}
		if flags&0x0020 == 0 { // MORE_COMPONENTS
			break
		}
	}
	return out
}

// simpleGlyphOutline interprets a simple glyf entry.
func simpleGlyphOutline(g []byte, nContours int) []subPath {
	p := 10
	if p+nContours*2+2 > len(g) {
		return nil
	}
	ends := make([]int, nContours)
	for i := 0; i < nContours; i++ {
		ends[i] = int(binary.BigEndian.Uint16(g[p+i*2:]))
	}
	p += nContours * 2
	nPoints := 0
	if nContours > 0 {
		nPoints = ends[nContours-1] + 1
	}
	instrLen := int(binary.BigEndian.Uint16(g[p:]))
	p += 2 + instrLen

	// Flags.
	flags := make([]byte, 0, nPoints)
	for len(flags) < nPoints {
		if p >= len(g) {
			return nil
		}
		f := g[p]
		p++
		flags = append(flags, f)
		if f&0x08 != 0 {
			if p >= len(g) {
				return nil
			}
			rep := int(g[p])
			p++
			for j := 0; j < rep && len(flags) < nPoints; j++ {
				flags = append(flags, f)
			}
		}
	}
	// X coordinates.
	xs := make([]float64, nPoints)
	x := 0
	for i := 0; i < nPoints; i++ {
		f := flags[i]
		var dx int
		switch {
		case f&0x02 != 0: // 1-byte delta, sign from bit 4
			if p >= len(g) {
				return nil
			}
			dx = int(g[p])
			p++
			if f&0x10 == 0 {
				dx = -dx
			}
		case f&0x10 == 0: // 2-byte signed delta
			if p+2 > len(g) {
				return nil
			}
			dx = int(int16(binary.BigEndian.Uint16(g[p:])))
			p += 2
		}
		x += dx
		xs[i] = float64(x)
	}
	// Y coordinates.
	ys := make([]float64, nPoints)
	y := 0
	for i := 0; i < nPoints; i++ {
		f := flags[i]
		var dy int
		switch {
		case f&0x04 != 0: // 1-byte delta, sign from bit 6
			if p >= len(g) {
				return nil
			}
			dy = int(g[p])
			p++
			if f&0x20 == 0 {
				dy = -dy
			}
		case f&0x20 == 0: // 2-byte signed delta
			if p+2 > len(g) {
				return nil
			}
			dy = int(int16(binary.BigEndian.Uint16(g[p:])))
			p += 2
		}
		y += dy
		ys[i] = float64(y)
	}
	onCurve := make([]bool, nPoints)
	for i := range flags {
		onCurve[i] = flags[i]&0x01 != 0
	}

	var out []subPath
	ptStart := 0
	for ci := 0; ci < nContours; ci++ {
		ptEnd := ends[ci]
		n := ptEnd - ptStart + 1
		if n < 2 {
			ptStart = ptEnd + 1
			continue
		}
		idx := make([]int, n)
		for j := 0; j < n; j++ {
			idx[j] = ptStart + j
		}
		out = append(out, buildTTFContour(idx, xs, ys, onCurve)...)
		ptStart = ptEnd + 1
	}
	return out
}

// buildTTFContour assembles one contour from TrueType points, inserting
// implied on-curve midpoints between consecutive off-curve points.
func buildTTFContour(idx []int, xs, ys []float64, onCurve []bool) []subPath {
	n := len(idx)
	start := -1
	for i, pi := range idx {
		if onCurve[pi] {
			start = i
			break
		}
	}
	var pts []pt
	if start == -1 {
		// All points off-curve: begin at the midpoint between the last and
		// the first point.
		a, b := idx[n-1], idx[0]
		pts = append(pts, pt{(xs[a] + xs[b]) / 2, -(ys[a] + ys[b]) / 2})
		start = 0
	} else {
		pts = append(pts, pt{xs[idx[start]], -ys[idx[start]]})
	}

	var pending []int
	emit := func(ctrl int, end pt) {
		c := pt{xs[ctrl], -ys[ctrl]}
		p0 := end
		if len(pts) > 0 {
			p0 = pts[len(pts)-1]
		}
		pts = flattenQuad(pts, p0, c, end, 20)
	}
	flushTo := func(end pt) {
		for k := 0; k < len(pending); k++ {
			var e pt
			if k == len(pending)-1 {
				e = end
			} else {
				nx := pending[k+1]
				e = pt{(xs[pending[k]] + xs[nx]) / 2, -(ys[pending[k]] + ys[nx]) / 2}
			}
			emit(pending[k], e)
		}
		pending = pending[:0]
	}

	for j := 1; j <= n; j++ {
		pi := idx[(start+j)%n]
		if onCurve[pi] {
			q := pt{xs[pi], -ys[pi]}
			if len(pending) > 0 {
				flushTo(q)
			} else {
				pts = append(pts, q)
			}
		} else {
			pending = append(pending, pi)
		}
	}
	if len(pending) > 0 {
		// Wrap around back to the start point.
		flushTo(pts[0])
	}
	if len(pts) > 2 {
		sp := subPath{pts: pts}
		sp.close()
		return []subPath{sp}
	}
	return nil
}
