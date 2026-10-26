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
)

// cffFont is a parsed bare CFF (FontFile3 /Type1C) font program with the
// tables needed for glyph outline extraction.
type cffFont struct {
	charStrings  [][]byte
	globalSubrs  [][]byte
	localSubrs   [][]byte   // non-CID private subrs
	fdLocalSubrs [][][]byte // per FD local subrs (CID fonts)
	fdSelect     func(gid int) int
	// cidToGid maps a CID to a charstring index for CID-keyed fonts whose
	// glyph subset is smaller than the character collection (Founder PKU2
	// fonts use the full CID space). nil means gid == cid.
	cidToGid map[uint32]int
	// encCodeToGid is the built-in encoding of a non-CID font (code -> GID,
	// -1 unmapped), valid when hasEncoding is set.
	encCodeToGid [256]int
	hasEncoding  bool
	// uniToGid maps a unicode rune to a GID via the charset glyph names for
	// non-CID subsets whose built-in encoding covers only a few codes.
	uniToGid map[rune]int
	// isCID reports a CID-keyed CFF (ROS present).
	isCID bool
	upem  float64
}

// --- INDEX parsing -------------------------------------------------------

func cffIndex(data []byte, off int) (items [][]byte, next int, err error) {
	if off+2 > len(data) {
		return nil, 0, errors.New("cff: index out of range")
	}
	count := int(data[off])<<8 | int(data[off+1])
	off += 2
	if count == 0 {
		return nil, off, nil
	}
	if off >= len(data) {
		return nil, 0, errors.New("cff: index header out of range")
	}
	offSize := int(data[off])
	off++
	if offSize < 1 || offSize > 4 {
		return nil, 0, errors.New("cff: invalid offSize")
	}
	readOff := func(pos int) (int, error) {
		if pos+offSize > len(data) {
			return 0, errors.New("cff: offset out of range")
		}
		v := 0
		for i := 0; i < offSize; i++ {
			v = v<<8 | int(data[pos+i])
		}
		return v, nil
	}
	start, err := readOff(off)
	if err != nil {
		return nil, 0, err
	}
	_ = start
	offs := make([]int, count+1)
	for i := 0; i <= count; i++ {
		v, err := readOff(off + i*offSize)
		if err != nil {
			return nil, 0, err
		}
		offs[i] = v
	}
	base := off + (count+1)*offSize - 1
	for i := 0; i < count; i++ {
		lo, hi := base+offs[i], base+offs[i+1]
		if lo < 0 || hi > len(data) || lo > hi {
			return nil, 0, errors.New("cff: invalid item bounds")
		}
		items = append(items, data[lo:hi])
	}
	next = base + offs[count]
	return items, next, nil
}

// --- DICT parsing --------------------------------------------------------

type cffDictEntry struct {
	op     int // 0-21 or 0x0c00+sub for escaped
	values []float64
}

func cffDict(data []byte) []cffDictEntry {
	var entries []cffDictEntry
	var vals []float64
	i := 0
	for i < len(data) {
		b0 := int(data[i])
		var v float64
		switch {
		case b0 <= 21:
			i++
			op := b0
			if b0 == 12 && i < len(data) {
				op = 0x0c00 + int(data[i])
				i++
			}
			entries = append(entries, cffDictEntry{op: op, values: vals})
			vals = nil
			continue
		case b0 == 28:
			if i+2 >= len(data) {
				return entries
			}
			v = float64(int16(int(data[i+1])<<8 | int(data[i+2])))
			i += 3
		case b0 == 29:
			if i+4 >= len(data) {
				return entries
			}
			v = float64(int32(int(data[i+1])<<24 | int(data[i+2])<<16 | int(data[i+3])<<8 | int(data[i+4])))
			i += 5
		case b0 == 30:
			// Real number.
			i++
			var sb []byte
			done := false
			for i < len(data) && !done {
				nibbles := []int{int(data[i] >> 4), int(data[i] & 0x0F)}
				i++
				for _, n := range nibbles {
					switch {
					case n <= 9:
						sb = append(sb, byte('0'+n))
					case n == 0xa:
						sb = append(sb, '.')
					case n == 0xb:
						sb = append(sb, 'E')
					case n == 0xc:
						sb = append(sb, 'E', '-')
					case n == 0xe:
						sb = append(sb, '-')
					case n == 0xf:
						done = true
					default: // 0xd reserved
					}
					if done {
						break
					}
				}
			}
			var f float64
			fmt.Sscanf(string(sb), "%g", &f)
			v = f
		case b0 == 255:
			if i+4 >= len(data) {
				return entries
			}
			bits := uint32(int(data[i+1])<<24 | int(data[i+2])<<16 | int(data[i+3])<<8 | int(data[i+4]))
			v = float64(math.Float32frombits(bits))
			i += 5
		case b0 >= 32 && b0 <= 246:
			v = float64(b0 - 139)
			i++
		case b0 >= 247 && b0 <= 250:
			if i+1 >= len(data) {
				return entries
			}
			v = float64((b0-247)*256 + int(data[i+1]) + 108)
			i += 2
		case b0 >= 251 && b0 <= 254:
			if i+1 >= len(data) {
				return entries
			}
			v = float64(-(b0-251)*256 - int(data[i+1]) - 108)
			i += 2
		default:
			i++
			continue
		}
		vals = append(vals, v)
	}
	return entries
}

func dictValue(entries []cffDictEntry, op int) (float64, bool) {
	for _, e := range entries {
		if e.op == op && len(e.values) > 0 {
			return e.values[len(e.values)-1], true
		}
	}
	return 0, false
}

// --- Font parsing --------------------------------------------------------

func parseCFF(data []byte) (*cffFont, error) {
	if len(data) < 4 {
		return nil, errors.New("cff: too short")
	}
	hdrSize := int(data[2])
	off := hdrSize
	// Name INDEX.
	_, off, err := cffIndex(data, off)
	if err != nil {
		return nil, err
	}
	// Top DICT INDEX.
	topItems, off, err := cffIndex(data, off)
	if err != nil {
		return nil, err
	}
	if len(topItems) == 0 {
		return nil, errors.New("cff: empty top dict")
	}
	top := cffDict(topItems[0])
	// String INDEX.
	strItems, off, err := cffIndex(data, off)
	if err != nil {
		return nil, err
	}
	// Global Subr INDEX.
	gSubrs, _, err := cffIndex(data, off)
	if err != nil {
		return nil, err
	}

	cf := &cffFont{globalSubrs: gSubrs, upem: 1000}

	// FontMatrix (op 12 7): the scale is the FIRST element of the array
	// (the trailing zeros are translations).
	topHasMatrix := false
	for _, e := range top {
		if e.op == 0x0c07 && len(e.values) > 0 && e.values[0] != 0 {
			cf.upem = math.Round(1 / math.Abs(e.values[0]))
			topHasMatrix = true
			break
		}
	}
	if cf.upem <= 0 || cf.upem > 16384 {
		cf.upem = 1000
	}

	csOff, ok := dictValue(top, 17) // CharStrings
	if !ok {
		return nil, errors.New("cff: missing charstrings")
	}
	csItems, _, err := cffIndex(data, int(csOff))
	if err != nil {
		return nil, fmt.Errorf("cff: charstrings: %w", err)
	}
	cf.charStrings = csItems

	// CID-keyed fonts (ROS in the top dict) index their subset charstrings by
	// GID; the charset maps GID -> CID. Build the reverse map so that CIDs
	// beyond the subset size resolve to the right charstring.
	if _, isCID := dictValue(top, 0x0c1e); isCID {
		cf.isCID = true
		if charsetOff, ok := dictValue(top, 15); ok && int(charsetOff) > 2 {
			if gids := cffCharsetCIDs(data, int(charsetOff), len(cf.charStrings)); gids != nil {
				cf.cidToGid = make(map[uint32]int, len(gids))
				for gid, cid := range gids {
					cf.cidToGid[uint32(cid)] = gid
				}
			}
		}
	} else {
		// Non-CID: a custom built-in encoding maps character codes to GIDs
		// directly (subset fonts almost always carry one).
		for i := range cf.encCodeToGid {
			cf.encCodeToGid[i] = -1
		}
		if encOff, ok := dictValue(top, 16); ok && int(encOff) > 1 {
			if cffParseEncoding(data, int(encOff), cf) {
				cf.hasEncoding = true
			}
		}
		// Parse the String INDEX and the charset (GID -> glyph name). Subsets
		// with a nearly empty built-in encoding are mapped through glyph
		// names (code -> unicode -> name -> GID) instead.
		if charsetOff, ok := dictValue(top, 15); ok && int(charsetOff) > 2 {
			names := cffCharsetNames(data, int(charsetOff), len(cf.charStrings), strItems)
			if len(names) > 0 {
				cf.uniToGid = make(map[rune]int, len(names))
				for gid, name := range names {
					if r, ok := glyphNameToRune(name); ok && r != 0 {
						if _, dup := cf.uniToGid[r]; !dup {
							cf.uniToGid[r] = gid
						}
					}
				}
			}
		}
	}

	// CID fonts: FDArray (12 36 = 0x0c24) with per-FD private dicts,
	// FDSelect (12 37 = 0x0c25).
	if fdArrayOff, ok := dictValue(top, 0x0c24); ok {
		fdItems, _, err := cffIndex(data, int(fdArrayOff))
		if err == nil {
			for _, fdData := range fdItems {
				fd := cffDict(fdData)
				// FD dict: Private = [size offset]; the Private dict then
				// carries Subrs (relative to its own start).
				var subrs [][]byte
				privBase, privSize := -1, 0.0
				for _, e := range fd {
					if e.op == 18 && len(e.values) >= 2 {
						privSize, privBase = e.values[0], int(e.values[1])
						break
					}
				}
				if privBase >= 0 && privBase+int(privSize) <= len(data) {
					priv := cffDict(data[privBase : privBase+int(privSize)])
					subrs = cffPrivateSubrs(data, priv, privBase)
				}
				cf.fdLocalSubrs = append(cf.fdLocalSubrs, subrs)
				// CID fonts may carry the FontMatrix in the first FD dict
				// instead of the top dict (Founder subsets do); FontBox reads
				// it from there when the top dict has none.
				if len(cf.fdLocalSubrs) == 1 && !topHasMatrix {
					for _, e := range fd {
						if e.op == 0x0c07 && len(e.values) > 0 && e.values[0] > 0 {
							if u := math.Round(1 / math.Abs(e.values[0])); u > 0 && u <= 16384 {
								cf.upem = u
							}
							break
						}
					}
				}
			}
		}
		if fdSelOff, ok := dictValue(top, 0x0c25); ok {
			cf.fdSelect = cffFDSelect(data, int(fdSelOff), len(cf.charStrings))
		}
	}

	// Non-CID: Private DICT (op 18: size offset).
	if len(cf.fdLocalSubrs) == 0 {
		var privSize, privOff float64
		for _, e := range top {
			if e.op == 18 && len(e.values) >= 2 {
				privSize, privOff = e.values[0], e.values[1]
				break
			}
		}
		if privSize > 0 {
			lo, hi := int(privOff), int(privOff+privSize)
			if lo >= 0 && hi <= len(data) && lo < hi {
				priv := cffDict(data[lo:hi])
				if v, ok := dictValue(priv, 20); ok {
					_ = v
				}
				cf.localSubrs = cffPrivateSubrs(data, priv, lo)
			}
		}
	}
	return cf, nil
}

// cffPrivateSubrs resolves the local subr index of a private dict. Subrs
// offsets are relative to the start of the private dict (privBase).
func cffPrivateSubrs(data []byte, priv []cffDictEntry, privBase int) [][]byte {
	off, ok := dictValue(priv, 19) // Subrs
	if !ok || privBase < 0 {
		return nil
	}
	base := privBase + int(off)
	if base < 0 || base >= len(data) {
		return nil
	}
	items, _, err := cffIndex(data, base)
	if err != nil {
		return nil
	}
	return items
}

// cffCharsetCIDs decodes the charset (formats 0, 1, 2) into a per-GID CID
// slice. Glyph 0 is .notdef (CID 0 for CID-keyed fonts).
func cffCharsetCIDs(data []byte, off, nGlyphs int) []int {
	if off < 0 || off >= len(data) || nGlyphs <= 0 {
		return nil
	}
	cids := make([]int, nGlyphs)
	switch data[off] {
	case 0:
		if off+1+2*(nGlyphs-1) > len(data) {
			return nil
		}
		for gid := 1; gid < nGlyphs; gid++ {
			p := off + 1 + 2*(gid-1)
			cids[gid] = int(data[p])<<8 | int(data[p+1])
		}
	case 1, 2:
		pos := off + 1
		gid := 1
		for gid < nGlyphs {
			if pos+2 > len(data) {
				return nil
			}
			first := int(data[pos])<<8 | int(data[pos+1])
			var nLeft int
			if data[off] == 1 {
				if pos+3 > len(data) {
					return nil
				}
				nLeft = int(data[pos+2])
				pos += 3
			} else {
				if pos+4 > len(data) {
					return nil
				}
				nLeft = int(data[pos+2])<<8 | int(data[pos+3])
				pos += 4
			}
			for k := 0; k <= nLeft && gid < nGlyphs; k++ {
				cids[gid] = first + k
				gid++
			}
		}
	default:
		return nil
	}
	return cids
}

// cffCharsetNames decodes the charset of a non-CID font into a per-GID glyph
// name slice, resolving SIDs through the standard strings and the font's own
// String INDEX.
func cffCharsetNames(data []byte, off, nGlyphs int, strItems [][]byte) []string {
	if off < 0 || off >= len(data) || nGlyphs <= 0 {
		return nil
	}
	sids := make([]int, nGlyphs) // glyph 0 = .notdef
	switch data[off] {
	case 0:
		if off+1+2*(nGlyphs-1) > len(data) {
			return nil
		}
		for gid := 1; gid < nGlyphs; gid++ {
			p := off + 1 + 2*(gid-1)
			sids[gid] = int(data[p])<<8 | int(data[p+1])
		}
	case 1, 2:
		pos := off + 1
		gid := 1
		for gid < nGlyphs {
			if pos+2 > len(data) {
				return nil
			}
			first := int(data[pos])<<8 | int(data[pos+1])
			var nLeft int
			if data[off] == 1 {
				if pos+3 > len(data) {
					return nil
				}
				nLeft = int(data[pos+2])
				pos += 3
			} else {
				if pos+4 > len(data) {
					return nil
				}
				nLeft = int(data[pos+2])<<8 | int(data[pos+3])
				pos += 4
			}
			for k := 0; k <= nLeft && gid < nGlyphs; k++ {
				sids[gid] = first + k
				gid++
			}
		}
	default:
		return nil
	}
	names := make([]string, nGlyphs)
	for gid, sid := range sids {
		switch {
		case gid == 0:
			names[gid] = ".notdef"
		case sid < 391:
			if s, ok := cffStandardStrings[sid]; ok {
				names[gid] = s
			}
		default:
			i := sid - 391
			if i < len(strItems) {
				names[gid] = string(strItems[i])
			}
		}
	}
	return names
}

// cffParseEncoding decodes a custom built-in CFF encoding (formats 0 and 1,
// supplements included) into encCodeToGid.
func cffParseEncoding(data []byte, off int, cf *cffFont) bool {
	if off < 0 || off >= len(data) {
		return false
	}
	switch data[off] {
	case 0:
		pos := off + 1
		if pos >= len(data) {
			return false
		}
		n := int(data[pos])
		pos++
		if pos+n > len(data) {
			return false
		}
		// code[i] is the character code assigned to glyph i+1.
		for i := 0; i < n; i++ {
			gid := i + 1
			if gid < 256 {
				cf.encCodeToGid[int(data[pos+i])] = gid
			}
		}
		return true
	case 1:
		pos := off + 1
		if pos+1 > len(data) {
			return false
		}
		nRanges := int(data[pos])
		pos++
		code := 1
		for i := 0; i < nRanges; i++ {
			if pos+2 > len(data) {
				return false
			}
			first := int(data[pos])
			nLeft := int(data[pos+1])
			pos += 2
			for k := 0; k <= nLeft; k++ {
				c := first + k
				if c < 256 {
					cf.encCodeToGid[c] = code
				}
				code++
			}
		}
		return true
	}
	return false
}

// cffFDSelect parses FDSelect formats 0 and 3.
func cffFDSelect(data []byte, off, nGlyphs int) func(gid int) int {
	if off >= len(data) {
		return nil
	}
	switch data[off] {
	case 0:
		if off+1+nGlyphs > len(data) {
			return nil
		}
		fds := make([]byte, nGlyphs)
		copy(fds, data[off+1:off+1+nGlyphs])
		return func(gid int) int {
			if gid >= 0 && gid < len(fds) {
				return int(fds[gid])
			}
			return 0
		}
	case 3:
		if off+5 > len(data) {
			return nil
		}
		nRanges := int(data[off+1])<<8 | int(data[off+2])
		ranges := make([][2]int, 0, nRanges)
		pos := off + 3
		for i := 0; i < nRanges; i++ {
			if pos+3 > len(data) {
				return nil
			}
			first := int(data[pos])<<8 | int(data[pos+1])
			fd := int(data[pos+2])
			ranges = append(ranges, [2]int{first, fd})
			pos += 3
		}
		return func(gid int) int {
			for i := len(ranges) - 1; i >= 0; i-- {
				if gid >= ranges[i][0] {
					return ranges[i][1]
				}
			}
			return 0
		}
	}
	return nil
}

// --- Type 2 charstring interpreter ---------------------------------------

type cffInterp struct {
	cf     *cffFont
	subrs  [][]byte // current local subrs
	x, y   float64
	stack  []float64
	paths  []subPath
	cur    []pt
	open   bool
	nStems int
}

func (in *cffInterp) moveTo(x, y float64) {
	if in.open && len(in.cur) > 1 {
		sp := subPath{pts: in.cur}
		sp.close()
		in.paths = append(in.paths, sp)
	}
	in.cur = []pt{{x, y}}
	in.open = true
}

func (in *cffInterp) lineTo(x, y float64) {
	in.cur = append(in.cur, pt{x, y})
}

func (in *cffInterp) curveTo(c1, c2, end pt) {
	if !in.open || len(in.cur) == 0 {
		return
	}
	p0 := in.cur[len(in.cur)-1]
	in.cur = flattenCubic(in.cur, p0, c1, c2, end, 20)
}

func (in *cffInterp) pop() float64 {
	n := len(in.stack)
	if n == 0 {
		return 0
	}
	v := in.stack[n-1]
	in.stack = in.stack[:n-1]
	return v
}

func (in *cffInterp) popN(n int) []float64 {
	// Always return n values, zero-padding missing ones: malformed charstrings
	// (or state drift after skipping operators) must not panic the renderer.
	// Path operators consume their argument pairs in pushing order, so the
	// values are taken from the FRONT of the stack.
	vals := make([]float64, n)
	avail := len(in.stack)
	if avail > n {
		avail = n
	}
	copy(vals, in.stack[:avail])
	in.stack = in.stack[avail:]
	return vals
}

func subrBias(n int) int {
	switch {
	case n < 1240:
		return 107
	case n < 33900:
		return 1131
	default:
		return 32768
	}
}

// cffGlyphOutline returns the outline of gid in font units with the y axis
// pointing down (to match the sfnt convention used by the renderer).
func cffGlyphOutline(cf *cffFont, gid int) (sps []subPath) {
	// The charstring interpreter is best-effort: a panic on a corrupt font
	// must degrade to an empty glyph, not take down the renderer.
	defer func() {
		if r := recover(); r != nil {
			sps = nil
		}
	}()
	if gid < 0 || gid >= len(cf.charStrings) {
		return nil
	}
	in := &cffInterp{cf: cf}
	if cf.fdSelect != nil && len(cf.fdLocalSubrs) > 0 {
		fd := cf.fdSelect(gid)
		if fd >= 0 && fd < len(cf.fdLocalSubrs) {
			in.subrs = cf.fdLocalSubrs[fd]
		}
	} else {
		in.subrs = cf.localSubrs
	}
	in.exec(cf.charStrings[gid], 0)
	if in.open && len(in.cur) > 1 {
		sp := subPath{pts: in.cur}
		sp.close()
		in.paths = append(in.paths, sp)
	}
	// Flip y: CFF is y-up, the renderer expects y-down font units.
	for i := range in.paths {
		for j := range in.paths[i].pts {
			in.paths[i].pts[j].y = -in.paths[i].pts[j].y
		}
	}
	return in.paths
}

func (in *cffInterp) exec(code []byte, depth int) {
	if depth > 10 {
		return
	}
	i := 0
	for i < len(code) {
		b0 := int(code[i])
		switch {
		case b0 == 28:
			in.stack = append(in.stack, float64(int16(int(code[i+1])<<8|int(code[i+2]))))
			i += 3
		case b0 >= 32 && b0 <= 246:
			in.stack = append(in.stack, float64(b0-139))
			i++
		case b0 >= 247 && b0 <= 250:
			in.stack = append(in.stack, float64((b0-247)*256+int(code[i+1])+108))
			i += 2
		case b0 >= 251 && b0 <= 254:
			in.stack = append(in.stack, float64(-(b0-251)*256-int(code[i+1])-108))
			i += 2
		case b0 == 255:
			// 16.16 fixed point number.
			v := int32(int(code[i+1])<<24 | int(code[i+2])<<16 | int(code[i+3])<<8 | int(code[i+4]))
			in.stack = append(in.stack, float64(v)/65536)
			i += 5
		case b0 == 1, b0 == 3, b0 == 18, b0 == 23: // hstem vstem hstemhm vstemhm
			if len(in.stack)%2 == 1 {
				in.stack = in.stack[1:] // width is the bottom value
			}
			in.nStems += len(in.stack) / 2
			in.stack = in.stack[:0]
			i++
		case b0 == 19 || b0 == 20: // hintmask cntrmask
			if len(in.stack)%2 == 1 {
				in.stack = in.stack[1:] // width is the bottom value
			}
			in.nStems += len(in.stack) / 2
			in.stack = in.stack[:0]
			i += 1 + (in.nStems+7)/8
		case b0 == 21: // rmoveto
			if len(in.stack) > 2 {
				in.stack = in.stack[1:] // width is the bottom value
			}
			y := in.pop()
			x := in.pop()
			in.x += x
			in.y += y
			in.moveTo(in.x, in.y)
			in.stack = in.stack[:0]
			i++
		case b0 == 22: // hmoveto
			if len(in.stack) > 1 {
				in.stack = in.stack[1:] // width is the bottom value
			}
			in.x += in.pop()
			in.moveTo(in.x, in.y)
			in.stack = in.stack[:0]
			i++
		case b0 == 4: // vmoveto
			if len(in.stack) > 1 {
				in.stack = in.stack[1:] // width is the the bottom value
			}
			in.y += in.pop()
			in.moveTo(in.x, in.y)
			in.stack = in.stack[:0]
			i++
		case b0 == 5: // rlineto
			for len(in.stack) >= 2 {
				a := in.popN(2)
				in.x += a[0]
				in.y += a[1]
				in.lineTo(in.x, in.y)
			}
			in.stack = in.stack[:0]
			i++
		case b0 == 6, b0 == 7: // hlineto / vlineto
			horiz := b0 == 6
			for len(in.stack) > 0 {
				v := in.stack[0]
				in.stack = in.stack[1:]
				if horiz {
					in.x += v
				} else {
					in.y += v
				}
				in.lineTo(in.x, in.y)
				horiz = !horiz
			}
			in.stack = in.stack[:0]
			i++
		case b0 == 8: // rrcurveto
			for len(in.stack) >= 6 {
				a := in.popN(6)
				in.curveTo(
					pt{in.x + a[0], in.y + a[1]},
					pt{in.x + a[0] + a[2], in.y + a[1] + a[3]},
					pt{in.x + a[0] + a[2] + a[4], in.y + a[1] + a[3] + a[5]})
				in.x += a[0] + a[2] + a[4]
				in.y += a[1] + a[3] + a[5]
			}
			in.stack = in.stack[:0]
			i++
		case b0 == 24: // rcurveline
			for len(in.stack) >= 8 {
				a := in.popN(6)
				in.curveTo(
					pt{in.x + a[0], in.y + a[1]},
					pt{in.x + a[0] + a[2], in.y + a[1] + a[3]},
					pt{in.x + a[0] + a[2] + a[4], in.y + a[1] + a[3] + a[5]})
				in.x += a[0] + a[2] + a[4]
				in.y += a[1] + a[3] + a[5]
			}
			if len(in.stack) >= 2 {
				a := in.popN(2)
				in.x += a[0]
				in.y += a[1]
				in.lineTo(in.x, in.y)
			}
			in.stack = in.stack[:0]
			i++
		case b0 == 25: // rlinecurve
			for len(in.stack) >= 8 {
				a := in.popN(2)
				in.x += a[0]
				in.y += a[1]
				in.lineTo(in.x, in.y)
			}
			if len(in.stack) >= 6 {
				a := in.popN(6)
				in.curveTo(
					pt{in.x + a[0], in.y + a[1]},
					pt{in.x + a[0] + a[2], in.y + a[1] + a[3]},
					pt{in.x + a[0] + a[2] + a[4], in.y + a[1] + a[3] + a[5]})
				in.x += a[0] + a[2] + a[4]
				in.y += a[1] + a[3] + a[5]
			}
			in.stack = in.stack[:0]
			i++
		case b0 == 26, b0 == 27: // vvcurveto / hhcurveto
			// vv: dx1? {dya dxb dyb dyc}+  hh: dy1? {dxa dxb dyb dxc}+
			var lead float64
			if len(in.stack)%2 == 1 {
				lead = in.stack[0]
				in.stack = in.stack[1:]
			}
			first := true
			for len(in.stack) >= 4 {
				a := in.popN(4)
				l := 0.0
				if first {
					l = lead
					first = false
				}
				var c1, c2, e pt
				if b0 == 26 { // rCurveTo((dx1, dya), (dxb, dyb), (0, dyc))
					c1 = pt{in.x + l, in.y + a[0]}
					c2 = pt{c1.x + a[1], c1.y + a[2]}
					e = pt{c2.x, c2.y + a[3]}
				} else { // rCurveTo((dxa, dy1), (dxb, dyb), (dxc, 0))
					c1 = pt{in.x + a[0], in.y + l}
					c2 = pt{c1.x + a[1], c1.y + a[2]}
					e = pt{c2.x + a[3], c2.y}
				}
				in.curveTo(c1, c2, e)
				in.x, in.y = e.x, e.y
			}
			in.stack = in.stack[:0]
			i++
		case b0 == 30, b0 == 31: // vhcurveto / hvcurveto
			// Alternating vertical/horizontal entry curves. A trailing single
			// argument is the missing end coordinate of the last segment.
			vert := b0 == 30
			for len(in.stack) >= 4 {
				a := in.popN(4)
				extra := 0.0
				if len(in.stack) == 1 {
					extra = in.pop()
				}
				var c1, c2, e pt
				if vert { // vcurveto: c1=(0,dya), c2=(dxb,dyb), end=(dxc,dyc)
					c1 = pt{in.x, in.y + a[0]}
					c2 = pt{c1.x + a[1], c1.y + a[2]}
					e = pt{c2.x + a[3], c2.y + extra}
				} else { // hcurveto: c1=(dxa,0), c2=(dxb,dyb), end=(dxc,dyc)
					c1 = pt{in.x + a[0], in.y}
					c2 = pt{c1.x + a[1], c1.y + a[2]}
					e = pt{c2.x + extra, c2.y + a[3]}
				}
				in.curveTo(c1, c2, e)
				in.x, in.y = e.x, e.y
				vert = !vert
			}
			in.stack = in.stack[:0]
			i++
		case b0 == 10: // callsubr
			idx := int(in.pop()) + subrBias(len(in.subrs))
			if idx >= 0 && idx < len(in.subrs) {
				in.exec(in.subrs[idx], depth+1)
			}
			i++
		case b0 == 29: // callgsubr
			idx := int(in.pop()) + subrBias(len(in.cf.globalSubrs))
			if idx >= 0 && idx < len(in.cf.globalSubrs) {
				in.exec(in.cf.globalSubrs[idx], depth+1)
			}
			i++
		case b0 == 11: // return
			return
		case b0 == 14: // endchar
			return
		case b0 == 12: // escape
			if i+1 >= len(code) {
				return
			}
			sub := int(code[i+1])
			i += 2
			switch sub {
			case 35: // flex: 13 args, fd ignored
				a := in.popN(13)
				in.curveTo(
					pt{in.x + a[0], in.y + a[1]},
					pt{in.x + a[0] + a[2], in.y + a[1] + a[3]},
					pt{in.x + a[0] + a[2] + a[4], in.y + a[1] + a[3] + a[5]})
				in.x += a[0] + a[2] + a[4]
				in.y += a[1] + a[3] + a[5]
				in.curveTo(
					pt{in.x + a[6], in.y + a[7]},
					pt{in.x + a[6] + a[8], in.y + a[7] + a[9]},
					pt{in.x + a[6] + a[8] + a[10], in.y + a[7] + a[9] + a[11]})
				in.x += a[6] + a[8] + a[10]
				in.y += a[7] + a[9] + a[11]
			case 34: // hflex: dx1 dx2 dy2 dx3 dx4 dx5 dx6
				a := in.popN(7)
				y0 := in.y
				in.curveTo(pt{in.x + a[0], y0}, pt{in.x + a[0] + a[1], y0 + a[2]}, pt{in.x + a[0] + a[1] + a[3], y0 + a[2]})
				in.x += a[0] + a[1] + a[3]
				in.curveTo(pt{in.x + a[4], y0}, pt{in.x + a[4] + a[5], y0 - a[2]}, pt{in.x + a[4] + a[5] + a[6], y0})
				in.x += a[4] + a[5] + a[6]
				in.y = y0
			case 36: // hflex1: dx1 dy1 dx2 dy2 dx3 dx4 dx5 dy5 dx6
				a := in.popN(9)
				y0 := in.y
				in.curveTo(pt{in.x + a[0], y0 + a[1]}, pt{in.x + a[0] + a[2], y0 + a[1] + a[3]}, pt{in.x + a[0] + a[2] + a[4], y0 + a[1] + a[3]})
				in.x += a[0] + a[2] + a[4]
				in.y += a[1] + a[3]
				dy6 := -(a[1] + a[3] + a[7])
				in.curveTo(pt{in.x + a[5], in.y}, pt{in.x + a[5] + a[6], in.y + a[7]}, pt{in.x + a[5] + a[6] + a[8], in.y + dy6})
				in.x += a[5] + a[6] + a[8]
				in.y = y0
			case 37: // flex1
				a := in.popN(11)
				sx, sy := in.x, in.y
				dx := a[0] + a[2] + a[4] + a[6] + a[8]
				dy := a[1] + a[3] + a[5] + a[7] + a[9]
				var dx6, dy6 float64
				if math.Abs(dx) > math.Abs(dy) {
					dx6, dy6 = a[10], -dy
				} else {
					dx6, dy6 = -dx, a[10]
				}
				in.curveTo(
					pt{in.x + a[0], in.y + a[1]},
					pt{in.x + a[0] + a[2], in.y + a[1] + a[3]},
					pt{in.x + a[0] + a[2] + a[4], in.y + a[1] + a[3] + a[5]})
				in.x += a[0] + a[2] + a[4]
				in.y += a[1] + a[3] + a[5]
				in.curveTo(
					pt{in.x + a[6], in.y + a[7]},
					pt{in.x + a[6] + a[8], in.y + a[7] + a[9]},
					pt{in.x + a[6] + a[8] + dx6, in.y + a[7] + a[9] + dy6})
				in.x += a[6] + a[8] + dx6
				in.y += a[7] + a[9] + dy6
				_ = sx
				_ = sy
			case 3, 4, 5, 9, 10, 11, 12, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30:
				// Arithmetic/logic operators: evaluate nothing (rarely needed
				// for outlines).
				in.stack = in.stack[:0]
			case 6: // seac
				// Accent composition: draw nothing beyond the base glyph.
				in.stack = in.stack[:0]
			default:
				in.stack = in.stack[:0]
			}
		case b0 <= 0, b0 >= 16 && b0 <= 17, b0 == 9, b0 == 13, b0 == 15:
			// Reserved or unsupported (random, storage): skip.
			i++
		default:
			i++
		}
	}
}
