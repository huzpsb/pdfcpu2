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

package pdfcpu

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// Subsets the bundled vivo Sans (see internal/fallbackfont) down to the
// glyphs needed by text replacement and embeds them as a CIDFontType2 with
// Identity-H encoding (see replace.go). The subset is a fully valid
// TrueType font: real outlines and metrics of vivo Sans, recomputed loca,
// hmtx, cmap (format 4 + 12), bbox, checksums and checkSumAdjustment.
// Composite glyphs are closed over their components and their component
// indices are rewritten to the subset gids.

// vivoSubset is a vivo Sans subset with the codes used for embedding.
// Code and subset gid of runes[i] are both i+1 (Identity CIDToGIDMap);
// missing runes map to the .notdef glyph but keep their code so W,
// ToUnicode and the drawing codes stay aligned.
type vivoSubset struct {
	ttf       []byte
	runes     []rune // sorted; code of runes[i] is i+1
	codes     map[rune]uint16
	widths    []int // glyph space (1000/em) advance per code
	bbox      [4]int
	ascent    int
	descent   int
	capHeight int
	missing   []rune // runes without a vivo Sans glyph (rendered as .notdef)
}

// code returns the subset character code of r (0 = .notdef).
func (s *vivoSubset) code(r rune) uint16 {
	if c, ok := s.codes[r]; ok {
		return c
	}
	return 0
}

// buildVivoSubset subsets the vivo Sans TrueType data to runes. runes must be
// deduplicated; their order defines the codes (code of runes[i] is i+1), so
// chained runs pass existing runes first to keep codes stable.
func buildVivoSubset(font []byte, runes []rune) (*vivoSubset, error) {
	tt, err := parseTTFTables(font)
	if err != nil {
		return nil, err
	}

	sub := &vivoSubset{codes: map[rune]uint16{}}
	sub.runes = append(sub.runes, runes...)

	// positional code assignment: code i+1 <-> runes[i]
	order := make([]uint16, 0, len(sub.runes)) // order[i] = old gid of code i+1
	for _, r := range sub.runes {
		old, ok := tt.cmap[r]
		if !ok {
			sub.missing = append(sub.missing, r)
			order = append(order, 0) // .notdef
			continue
		}
		sub.codes[r] = uint16(len(order) + 1)
		order = append(order, old)
	}

	// old gid -> subset gid
	newGid := map[uint16]uint16{0: 0}
	for i, old := range order {
		newGid[old] = uint16(i + 1)
	}
	addExtra := func(old uint16) uint16 {
		if n, ok := newGid[old]; ok {
			return n
		}
		n := uint16(len(order) + 1)
		newGid[old] = n
		order = append(order, old)
		return n
	}

	// component closure (BFS) — a shared component gets one subset gid
	queue := append([]uint16{}, order...)
	for len(queue) > 0 {
		g := queue[0]
		queue = queue[1:]
		comps, err := tt.components(g)
		if err != nil {
			return nil, err
		}
		for _, cg := range comps {
			if _, ok := newGid[cg]; !ok {
				addExtra(cg)
				queue = append(queue, cg)
			}
		}
	}

	numGlyphs := len(order) + 1 // + .notdef
	if numGlyphs > 0xFFFF {
		return nil, fmt.Errorf("subset too large: %d glyphs", numGlyphs)
	}

	// glyf + loca (long) + per-glyph metrics, patching component indices
	var glyf []byte
	loca := make([]uint32, numGlyphs+1)
	advances := make([]uint16, numGlyphs)
	lsbs := make([]int16, numGlyphs)
	bboxSet := false
	sub.bbox = [4]int{math.MaxInt32, math.MaxInt32, math.MinInt32, math.MinInt32}
	for newG := 1; newG < numGlyphs; newG++ {
		old := order[newG-1]
		start, end := tt.glyphRange(old)
		advances[newG], lsbs[newG] = tt.metrics(old)
		loca[newG] = uint32(len(glyf))
		if start == end {
			continue // empty glyph
		}
		if end-start < 10 || start+10 > len(tt.glyf) {
			return nil, fmt.Errorf("glyph %d out of range", old)
		}
		g := append([]byte{}, tt.glyf[start:end]...)
		// patch composite component gids
		if int16(binary.BigEndian.Uint16(g[0:2])) < 0 {
			p := 10
			for {
				if p+4 > len(g) {
					return nil, fmt.Errorf("glyph %d: truncated composite", old)
				}
				flags := binary.BigEndian.Uint16(g[p : p+2])
				compOld := binary.BigEndian.Uint16(g[p+2 : p+4])
				compNew, ok := newGid[compOld]
				if !ok {
					return nil, fmt.Errorf("glyph %d: unmapped component %d", old, compOld)
				}
				binary.BigEndian.PutUint16(g[p+2:p+4], compNew)
				p += 4
				if flags&0x0001 != 0 { // ARG_1_AND_2_ARE_WORDS
					p += 4
				} else {
					p += 2
				}
				switch {
				case flags&0x0008 != 0: // WE_HAVE_A_SCALE
					p += 2
				case flags&0x0020 != 0: // WE_HAVE_AN_X_AND_Y_SCALE
					p += 4
				case flags&0x0040 != 0: // WE_HAVE_A_TWO_BY_TWO
					p += 8
				}
				if flags&0x0010 == 0 { // MORE_COMPONENTS
					break
				}
			}
		}
		// bbox for the subset head
		xMin := int(int16(binary.BigEndian.Uint16(g[2:4])))
		yMin := int(int16(binary.BigEndian.Uint16(g[4:6])))
		xMax := int(int16(binary.BigEndian.Uint16(g[6:8])))
		yMax := int(int16(binary.BigEndian.Uint16(g[8:10])))
		if xMin < sub.bbox[0] {
			sub.bbox[0] = xMin
		}
		if yMin < sub.bbox[1] {
			sub.bbox[1] = yMin
		}
		if xMax > sub.bbox[2] {
			sub.bbox[2] = xMax
		}
		if yMax > sub.bbox[3] {
			sub.bbox[3] = yMax
		}
		bboxSet = true
		glyf = append(glyf, g...)
		for len(glyf)%4 != 0 {
			glyf = append(glyf, 0)
		}
	}
	loca[numGlyphs] = uint32(len(glyf))
	locaBytes := make([]byte, 4*(numGlyphs+1))
	for i, v := range loca {
		binary.BigEndian.PutUint32(locaBytes[4*i:], v)
	}

	// metrics scale: font units -> glyph space (1000/em)
	scale := func(v int) int {
		return int(math.Round(float64(v) * 1000 / float64(tt.upem)))
	}
	sub.widths = make([]int, len(sub.runes))
	for i := range sub.runes {
		sub.widths[i] = scale(int(advances[i+1]))
	}
	sub.ascent = scale(tt.ascent)
	sub.descent = scale(tt.descent)
	sub.capHeight = scale(tt.capHeight)
	if !bboxSet {
		sub.bbox = [4]int{0, tt.descent, tt.bbox[2], tt.ascent}
	}
	for i := 0; i < 4; i++ {
		sub.bbox[i] = scale(sub.bbox[i])
	}

	// hmtx: every glyph has its own entry
	hmtx := make([]byte, 4*numGlyphs)
	binary.BigEndian.PutUint16(hmtx[0:], 500) // .notdef
	for i := 1; i < numGlyphs; i++ {
		binary.BigEndian.PutUint16(hmtx[4*i:], advances[i])
		binary.BigEndian.PutUint16(hmtx[4*i+2:], uint16(lsbs[i]))
	}

	// head: copy, long loca, subset bbox
	head := append([]byte{}, tt.head...)
	binary.BigEndian.PutUint16(head[50:], 1) // indexToLocFormat
	putInt16(head, 36, sub.bbox[0])
	putInt16(head, 38, sub.bbox[1])
	putInt16(head, 40, sub.bbox[2])
	putInt16(head, 42, sub.bbox[3])

	// hhea: copy, one metric per glyph
	hhea := append([]byte{}, tt.hhea...)
	binary.BigEndian.PutUint16(hhea[34:], uint16(numGlyphs))

	// maxp: copy, subset glyph count
	maxp := append([]byte{}, tt.maxp...)
	binary.BigEndian.PutUint16(maxp[4:], uint16(numGlyphs))

	// cmap: format 4 (BMP) + format 12 (all), rune -> subset gid
	cmapTable, err := buildSubsetCmap(sub.runes, sub.codes)
	if err != nil {
		return nil, err
	}

	if tt.os2 == nil || tt.name == nil || tt.post == nil {
		return nil, fmt.Errorf("vivo sans: missing required tables")
	}
	tables := []struct {
		tag  string
		data []byte
	}{
		{"OS/2", tt.os2},
		{"cmap", cmapTable},
		{"glyf", glyf},
		{"head", head},
		{"hhea", hhea},
		{"hmtx", hmtx},
		{"loca", locaBytes},
		{"maxp", maxp},
		{"name", tt.name},
		{"post", tt.post},
	}

	nt := len(tables)
	off := 12 + 16*nt
	dir := make([]byte, 12+16*nt)
	binary.BigEndian.PutUint32(dir[0:], 0x00010000)
	binary.BigEndian.PutUint16(dir[4:], uint16(nt))
	es := 0
	for 1<<(es+1) <= nt {
		es++
	}
	sr := 16 << es
	binary.BigEndian.PutUint16(dir[6:], uint16(sr))
	binary.BigEndian.PutUint16(dir[8:], uint16(es))
	binary.BigEndian.PutUint16(dir[10:], uint16(nt*16-sr))

	var buf []byte
	buf = append(buf, dir...)
	recs := make([]byte, 16*nt)
	headOff := -1
	for i, t := range tables {
		r := recs[i*16:]
		copy(r[0:4], t.tag)
		binary.BigEndian.PutUint32(r[4:], ttfChecksum(t.data))
		binary.BigEndian.PutUint32(r[8:], uint32(off))
		binary.BigEndian.PutUint32(r[12:], uint32(len(t.data)))
		if t.tag == "head" {
			headOff = off
		}
		buf = append(buf, t.data...)
		pad := (4 - len(t.data)%4) % 4
		buf = append(buf, make([]byte, pad)...)
		off += len(t.data) + pad
	}
	copy(buf[12:], recs)

	if headOff < 0 {
		return nil, fmt.Errorf("vivo sans: head table lost")
	}
	// checkSumAdjustment: 0xB1B0AFBA - checksum(entire font, adjustment = 0)
	sum := ttfChecksum(buf)
	adj := (0xB1B0AFBA - sum) & 0xFFFFFFFF
	binary.BigEndian.PutUint32(buf[headOff+8:], adj)

	sub.ttf = buf
	return sub, nil
}

// ttfTables holds the parsed tables of a TrueType font.
type ttfTables struct {
	upem             int
	numGlyphs        int
	glyf             []byte
	loca             []uint32 // glyph start offsets (bytes), len numGlyphs+1
	cmap             map[rune]uint16
	ascent           int
	descent          int
	capHeight        int
	bbox             [4]int
	head             []byte
	hhea             []byte
	maxp             []byte
	hmtx             []byte
	os2              []byte
	name             []byte
	post             []byte
	numberOfHMetrics int
}

// parseTTFTables parses the tables needed for subsetting.
func parseTTFTables(font []byte) (*ttfTables, error) {
	if len(font) < 12 {
		return nil, fmt.Errorf("truncated font")
	}
	if v := binary.BigEndian.Uint32(font[0:]); v != 0x00010000 {
		return nil, fmt.Errorf("unsupported sfnt version 0x%08X (TrueType expected)", v)
	}
	nt := int(binary.BigEndian.Uint16(font[4:]))
	if len(font) < 12+16*nt {
		return nil, fmt.Errorf("truncated table directory")
	}
	tabs := map[string][]byte{}
	for i := 0; i < nt; i++ {
		r := font[12+16*i:]
		tag := string(r[0:4])
		off := int(binary.BigEndian.Uint32(r[8:]))
		l := int(binary.BigEndian.Uint32(r[12:]))
		if off < 0 || l < 0 || off+l > len(font) || off+l < off {
			return nil, fmt.Errorf("table %s out of range", tag)
		}
		tabs[tag] = font[off : off+l]
	}

	tt := &ttfTables{cmap: map[rune]uint16{}}

	head, ok := tabs["head"]
	if !ok || len(head) < 54 {
		return nil, fmt.Errorf("missing head table")
	}
	tt.head = head
	tt.upem = int(binary.BigEndian.Uint16(head[18:]))
	if tt.upem == 0 {
		tt.upem = 1000
	}
	tt.bbox = [4]int{
		int(int16(binary.BigEndian.Uint16(head[36:]))),
		int(int16(binary.BigEndian.Uint16(head[38:]))),
		int(int16(binary.BigEndian.Uint16(head[40:]))),
		int(int16(binary.BigEndian.Uint16(head[42:]))),
	}

	hhea, ok := tabs["hhea"]
	if !ok || len(hhea) < 36 {
		return nil, fmt.Errorf("missing hhea table")
	}
	tt.hhea = hhea
	tt.ascent = int(int16(binary.BigEndian.Uint16(hhea[4:])))
	tt.descent = int(int16(binary.BigEndian.Uint16(hhea[6:])))
	tt.numberOfHMetrics = int(binary.BigEndian.Uint16(hhea[34:]))

	maxp, ok := tabs["maxp"]
	if !ok || len(maxp) < 6 {
		return nil, fmt.Errorf("missing maxp table")
	}
	tt.maxp = maxp
	tt.numGlyphs = int(binary.BigEndian.Uint16(maxp[4:]))

	locaRaw, ok := tabs["loca"]
	if !ok {
		return nil, fmt.Errorf("missing loca table")
	}
	long := binary.BigEndian.Uint16(head[50:]) == 1
	var want int
	if long {
		want = 4 * (tt.numGlyphs + 1)
	} else {
		want = 2 * (tt.numGlyphs + 1)
	}
	if len(locaRaw) < want {
		return nil, fmt.Errorf("truncated loca table")
	}
	tt.loca = make([]uint32, tt.numGlyphs+1)
	for i := 0; i <= tt.numGlyphs; i++ {
		if long {
			tt.loca[i] = binary.BigEndian.Uint32(locaRaw[4*i:])
		} else {
			tt.loca[i] = uint32(binary.BigEndian.Uint16(locaRaw[2*i:])) * 2
		}
	}

	tt.glyf = tabs["glyf"]
	if tt.glyf == nil {
		return nil, fmt.Errorf("missing glyf table (no TrueType outlines)")
	}
	tt.hmtx = tabs["hmtx"]

	tt.os2 = tabs["OS/2"]
	tt.name = tabs["name"]
	tt.post = tabs["post"]
	tt.capHeight = tt.ascent * 7 / 10
	if len(tt.os2) >= 88 && binary.BigEndian.Uint16(tt.os2[0:]) >= 2 {
		tt.capHeight = int(int16(binary.BigEndian.Uint16(tt.os2[86:])))
	}

	if err := parseCmap(tabs["cmap"], tt.cmap); err != nil {
		return nil, err
	}
	return tt, nil
}

// glyphRange returns the byte range of a glyph in glyf.
func (tt *ttfTables) glyphRange(gid uint16) (int, int) {
	if int(gid) >= tt.numGlyphs {
		return 0, 0
	}
	return int(tt.loca[gid]), int(tt.loca[gid+1])
}

// metrics returns the advance width and lsb of a glyph.
func (tt *ttfTables) metrics(gid uint16) (uint16, int16) {
	if len(tt.hmtx) < 4 || tt.numberOfHMetrics == 0 {
		return 0, 0
	}
	i := int(gid)
	if i < tt.numberOfHMetrics {
		if 4*i+4 > len(tt.hmtx) {
			return 0, 0
		}
		return binary.BigEndian.Uint16(tt.hmtx[4*i:]),
			int16(binary.BigEndian.Uint16(tt.hmtx[4*i+2:]))
	}
	// beyond numberOfHMetrics: shared advance, individual lsb
	adv := binary.BigEndian.Uint16(tt.hmtx[4*(tt.numberOfHMetrics-1):])
	var lsb int16
	if p := 4*tt.numberOfHMetrics + 2*i; p+2 <= len(tt.hmtx) {
		lsb = int16(binary.BigEndian.Uint16(tt.hmtx[p:]))
	}
	return adv, lsb
}

// components returns the component gids of a composite glyph.
func (tt *ttfTables) components(gid uint16) ([]uint16, error) {
	start, end := tt.glyphRange(gid)
	if end-start < 12 {
		return nil, nil
	}
	g := tt.glyf[start:end]
	if int16(binary.BigEndian.Uint16(g[0:2])) >= 0 {
		return nil, nil // simple glyph
	}
	var comps []uint16
	p := 10
	for {
		if p+4 > len(g) {
			return nil, fmt.Errorf("glyph %d: truncated composite", gid)
		}
		flags := binary.BigEndian.Uint16(g[p : p+2])
		comps = append(comps, binary.BigEndian.Uint16(g[p+2:p+4]))
		p += 4
		if flags&0x0001 != 0 { // ARG_1_AND_2_ARE_WORDS
			p += 4
		} else {
			p += 2
		}
		switch {
		case flags&0x0008 != 0: // WE_HAVE_A_SCALE
			p += 2
		case flags&0x0020 != 0: // WE_HAVE_AN_X_AND_Y_SCALE
			p += 4
		case flags&0x0040 != 0: // WE_HAVE_A_TWO_BY_TWO
			p += 8
		}
		if flags&0x0010 == 0 { // MORE_COMPONENTS
			break
		}
	}
	return comps, nil
}

// parseCmap builds rune -> gid from the best unicode cmap subtable.
func parseCmap(data []byte, out map[rune]uint16) error {
	if len(data) < 4 {
		return fmt.Errorf("missing cmap table")
	}
	n := int(binary.BigEndian.Uint16(data[2:]))
	if len(data) < 4+8*n {
		return fmt.Errorf("truncated cmap header")
	}
	var offs []int
	for i := 0; i < n; i++ {
		e := data[4+8*i:]
		platform := binary.BigEndian.Uint16(e[0:])
		encoding := binary.BigEndian.Uint16(e[2:])
		off := int(binary.BigEndian.Uint32(e[4:]))
		switch {
		case platform == 3 && (encoding == 1 || encoding == 10), platform == 0:
			if off > 0 && off < len(data) {
				offs = append(offs, off)
			}
		}
	}
	if len(offs) == 0 {
		return fmt.Errorf("no unicode cmap subtable")
	}
	// prefer the last subtable (usually the most capable)
	for i := len(offs) - 1; i >= 0; i-- {
		if err := parseCmapSubtable(data[offs[i]:], out); err == nil && len(out) > 0 {
			return nil
		}
	}
	return fmt.Errorf("no usable unicode cmap subtable")
}

// parseCmapSubtable parses cmap formats 0, 4, 6 and 12.
func parseCmapSubtable(data []byte, out map[rune]uint16) error {
	if len(data) < 4 {
		return fmt.Errorf("truncated cmap subtable")
	}
	format := binary.BigEndian.Uint16(data[0:])
	switch format {
	case 0:
		if len(data) < 262 {
			return fmt.Errorf("truncated cmap format 0")
		}
		for i := 0; i < 256; i++ {
			if gid := data[6+i]; gid != 0 {
				out[rune(i)] = uint16(gid)
			}
		}
		return nil

	case 4:
		if len(data) < 14 {
			return fmt.Errorf("truncated cmap format 4")
		}
		segCount := int(binary.BigEndian.Uint16(data[6:])) / 2
		if segCount == 0 || len(data) < 16+8*segCount {
			return fmt.Errorf("truncated cmap format 4 segments")
		}
		endOff := 14
		startOff := endOff + 2*segCount + 2 // endCode + reservedPad
		deltaOff := startOff + 2*segCount
		rangeOff := deltaOff + 2*segCount
		for s := 0; s < segCount; s++ {
			end := int(binary.BigEndian.Uint16(data[endOff+2*s:]))
			start := int(binary.BigEndian.Uint16(data[startOff+2*s:]))
			delta := int(int16(binary.BigEndian.Uint16(data[deltaOff+2*s:])))
			ro := int(binary.BigEndian.Uint16(data[rangeOff+2*s:]))
			if start == 0xFFFF {
				continue
			}
			for c := start; c <= end && c != 0xFFFF; c++ {
				var gid int
				if ro == 0 {
					gid = c + delta
				} else {
					idx := rangeOff + 2*s + ro + 2*(c-start)
					if idx+1 >= len(data) {
						continue
					}
					gid = int(binary.BigEndian.Uint16(data[idx:]))
					if gid != 0 {
						gid += delta
					}
				}
				gid &= 0xFFFF
				if gid != 0 {
					out[rune(c)] = uint16(gid)
				}
			}
		}
		return nil

	case 6:
		if len(data) < 10 {
			return fmt.Errorf("truncated cmap format 6")
		}
		first := int(binary.BigEndian.Uint16(data[6:]))
		count := int(binary.BigEndian.Uint16(data[8:]))
		if len(data) < 10+2*count {
			return fmt.Errorf("truncated cmap format 6")
		}
		for i := 0; i < count; i++ {
			if gid := binary.BigEndian.Uint16(data[10+2*i:]); gid != 0 {
				out[rune(first+i)] = gid
			}
		}
		return nil

	case 12:
		if len(data) < 16 {
			return fmt.Errorf("truncated cmap format 12")
		}
		nGroups := int(binary.BigEndian.Uint32(data[12:]))
		if len(data) < 16+12*nGroups {
			return fmt.Errorf("truncated cmap format 12 groups")
		}
		for g := 0; g < nGroups; g++ {
			e := data[16+12*g:]
			start := binary.BigEndian.Uint32(e[0:])
			end := binary.BigEndian.Uint32(e[4:])
			startGid := binary.BigEndian.Uint32(e[8:])
			if end < start || end > 0x10FFFF {
				continue
			}
			for c := start; c <= end; c++ {
				out[rune(c)] = uint16(startGid + (c - start))
			}
		}
		return nil
	}
	return fmt.Errorf("unsupported cmap format %d", format)
}

// buildSubsetCmap builds a cmap with format 4 (BMP) and format 12 subtables
// mapping the subset runes to their subset gids (= codes). Spans are sorted
// internally; callers may pass runes in any order.
func buildSubsetCmap(runes []rune, codes map[rune]uint16) ([]byte, error) {
	type span struct {
		start, end rune
		gid        uint16
	}
	var spans []span
	for _, r := range runes {
		gid := codes[r]
		if n := len(spans); n > 0 && spans[n-1].end+1 == r && spans[n-1].gid+uint16(r-spans[n-1].start) == gid {
			spans[n-1].end = r
			continue
		}
		spans = append(spans, span{start: r, end: r, gid: gid})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })

	// format 4 (BMP spans only)
	var bmp []span
	for _, s := range spans {
		if s.end <= 0xFFFF {
			bmp = append(bmp, s)
		}
	}
	segCount := len(bmp) + 1 // + sentinel
	sub4Len := 16 + 8*segCount
	sub4 := make([]byte, sub4Len)
	binary.BigEndian.PutUint16(sub4[0:], 4)
	binary.BigEndian.PutUint16(sub4[2:], uint16(sub4Len))
	binary.BigEndian.PutUint16(sub4[6:], uint16(2*segCount))
	es := 0
	for 1<<(es+1) <= segCount {
		es++
	}
	sr := 2 << es
	binary.BigEndian.PutUint16(sub4[8:], uint16(sr))
	binary.BigEndian.PutUint16(sub4[10:], uint16(es))
	binary.BigEndian.PutUint16(sub4[12:], uint16(2*segCount-sr))
	for i, s := range bmp {
		binary.BigEndian.PutUint16(sub4[14+2*i:], uint16(s.end))
		binary.BigEndian.PutUint16(sub4[16+2*segCount+2*i:], uint16(s.start))
		delta := int(s.gid) - int(s.start)
		binary.BigEndian.PutUint16(sub4[16+4*segCount+2*i:], uint16(int16(delta)))
	}
	// sentinel segment 0xFFFF..0xFFFF
	si := len(bmp)
	binary.BigEndian.PutUint16(sub4[14+2*si:], 0xFFFF)
	binary.BigEndian.PutUint16(sub4[16+2*segCount+2*si:], 0xFFFF)
	binary.BigEndian.PutUint16(sub4[16+4*segCount+2*si:], 1)
	// idRangeOffset zeros already

	// format 12
	sub12Len := 16 + 12*len(spans)
	sub12 := make([]byte, sub12Len)
	binary.BigEndian.PutUint16(sub12[0:], 12)
	binary.BigEndian.PutUint32(sub12[4:], uint32(sub12Len))
	binary.BigEndian.PutUint32(sub12[12:], uint32(len(spans)))
	for i, s := range spans {
		e := sub12[16+12*i:]
		binary.BigEndian.PutUint32(e[0:], uint32(s.start))
		binary.BigEndian.PutUint32(e[4:], uint32(s.end))
		binary.BigEndian.PutUint32(e[8:], uint32(s.gid))
	}

	// cmap header with (3,1) + (3,10) records
	out := make([]byte, 4+16, 4+16+sub4Len+sub12Len)
	binary.BigEndian.PutUint16(out[0:], 0)
	binary.BigEndian.PutUint16(out[2:], 2)
	binary.BigEndian.PutUint16(out[4:], 3)
	binary.BigEndian.PutUint16(out[6:], 1)
	binary.BigEndian.PutUint32(out[8:], uint32(20))
	binary.BigEndian.PutUint16(out[12:], 3)
	binary.BigEndian.PutUint16(out[14:], 10)
	binary.BigEndian.PutUint32(out[16:], uint32(20+sub4Len))
	out = append(out, sub4...)
	out = append(out, sub12...)
	return out, nil
}

func putInt16(b []byte, off, v int) {
	binary.BigEndian.PutUint16(b[off:], uint16(int16(v)))
}
