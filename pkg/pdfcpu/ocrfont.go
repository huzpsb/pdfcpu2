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
	"bytes"
	"encoding/binary"
	"math"
)

// Synthesized blank TrueType font for the OCR text layer.
//
// A Type3 font is the cheap way to draw invisible text, but several viewers
// (notably Foxit) mis-handle Type3 selection metrics. Established OCR layers
// therefore embed a real TrueType CIDFontType2. This file synthesizes one in
// pure Go: every glyph is a filled unit square covering the em box — it is
// never painted (the content stream uses render mode 3) but gives every
// consumer (PDFium hit boxes, Foxit line metrics, editors re-saving the font)
// a well-formed font with real outlines, metrics and checksums.
//
// Layout follows the TrueType specification (all integers big-endian). The
// result is verified against golang.org/x/image/font/sfnt's strict parser in
// ocrfont_test.go — pdfcpu's PDF-level validator does not parse font
// binaries, so that test is the independent gate.

const (
	ocrEm      = 1000   // units per em
	ocrDesc    = -200   // em box extends 20% below the baseline
	ocrDescU   = 0xFF38 // uint16 bit pattern of int16(-200)
	ocrAsc     = 1000
	ocrGlyphSz = 34 // one synthesized glyph: 1 contour, 4 on-curve points
)

// ocrBlankTTF returns a TrueType font with len(widths)+1 glyphs (gid 0 =
// .notdef). Glyph i+1 is a filled box [0,-200,w,1000] with advance w where w
// = widths[i] font units — one glyph per character instance lets the box
// width match the instance's true slot so consecutive glyph boxes neither
// overlap (which makes Foxit merge lines) nor leave gaps (which makes
// extractors insert spurious spaces).
func ocrBlankTTF(widths []int) []byte {
	numGlyphs := len(widths) + 1

	maxW := 500
	for _, w := range widths {
		if w > maxW {
			maxW = w
		}
	}

	head := ocrTTFHead(maxW)
	hhea := ocrTTFHHead(numGlyphs, maxW)
	maxp := ocrTTFMaxp(numGlyphs)

	// hmtx: gid 0 (.notdef) fixed, gid i+1 advance = widths[i]
	hmtx := make([]byte, numGlyphs*4)
	binary.BigEndian.PutUint16(hmtx[0:], 500)
	for i, w := range widths {
		binary.BigEndian.PutUint16(hmtx[(i+1)*4:], uint16(w))
	}

	// glyf + loca: variable-size glyphs
	var glyf bytes.Buffer
	loca := make([]byte, (numGlyphs+1)*4)
	binary.BigEndian.PutUint32(loca[0:], 0)
	glyf.Write(ocrTTFGlyph(500))
	for i, w := range widths {
		binary.BigEndian.PutUint32(loca[(i+1)*4:], uint32(glyf.Len()))
		glyf.Write(ocrTTFGlyph(w))
	}
	binary.BigEndian.PutUint32(loca[numGlyphs*4:], uint32(glyf.Len()))

	post := ocrTTFPost()
	os2 := ocrTTFOS2()
	name := ocrTTFName()
	cmap := ocrTTFCmap(numGlyphs)

	tables := []struct {
		tag  string
		data []byte
	}{
		{"OS/2", os2},
		{"cmap", cmap},
		{"glyf", glyf.Bytes()},
		{"head", head},
		{"hhea", hhea},
		{"hmtx", hmtx},
		{"loca", loca},
		{"maxp", maxp},
		{"name", name},
		{"post", post},
	}

	// offset table + directory (records must be sorted by tag)
	nt := len(tables)
	off := 12 + 16*nt
	dir := make([]byte, 12+16*nt)
	binary.BigEndian.PutUint32(dir[0:], 0x00010000) // sfnt version
	binary.BigEndian.PutUint16(dir[4:], uint16(nt))
	es := uint16(math.Floor(math.Log2(float64(nt))))
	sr := uint16(16 << es)
	binary.BigEndian.PutUint16(dir[6:], sr)
	binary.BigEndian.PutUint16(dir[8:], es)
	binary.BigEndian.PutUint16(dir[10:], uint16(nt*16)-sr)

	var buf bytes.Buffer
	buf.Write(dir)
	recs := make([]byte, 16*nt)
	for i, t := range tables {
		r := recs[i*16:]
		copy(r[0:4], t.tag)
		binary.BigEndian.PutUint32(r[4:], ttfChecksum(t.data))
		binary.BigEndian.PutUint32(r[8:], uint32(off))
		binary.BigEndian.PutUint32(r[12:], uint32(len(t.data)))
		buf.Write(t.data)
		pad := (4 - len(t.data)%4) % 4
		buf.Write(make([]byte, pad))
		off += len(t.data) + pad
	}
	copy(buf.Bytes()[12:], recs)

	// checkSumAdjustment: 0xB1B0AFBA - checksum(entire font, adjustment = 0)
	font := buf.Bytes()
	binary.BigEndian.PutUint32(head[8:], 0) // ensure zero before summing
	sum := ttfChecksum(font)
	adj := (0xB1B0AFBA - sum) & 0xFFFFFFFF
	// head was written into buf by value: patch the copy inside font
	binary.BigEndian.PutUint32(font[ttfTableOffset(font, "head")+8:], adj)
	return font
}

func ttfTableOffset(font []byte, tag string) int {
	nt := int(binary.BigEndian.Uint16(font[4:]))
	for i := 0; i < nt; i++ {
		r := font[12+16*i:]
		if string(r[0:4]) == tag {
			return int(binary.BigEndian.Uint32(r[8:]))
		}
	}
	return 0
}

func ttfChecksum(b []byte) uint32 {
	pad := (4 - len(b)%4) % 4
	if pad > 0 {
		b = append(append([]byte{}, b...), make([]byte, pad)...)
	}
	var sum uint32
	for i := 0; i < len(b); i += 4 {
		sum += binary.BigEndian.Uint32(b[i:])
	}
	return sum
}

// ocrTTFGlyph is one filled square contour: (0,-200) (1000,-200) (1000,1000) (0,1000).
func ocrTTFGlyph(w int) []byte {
	g := make([]byte, ocrGlyphSz)
	binary.BigEndian.PutUint16(g[0:], 1) // numberOfContours
	binary.BigEndian.PutUint16(g[2:], 0)
	binary.BigEndian.PutUint16(g[4:], ocrDescU)
	binary.BigEndian.PutUint16(g[6:], uint16(w))
	binary.BigEndian.PutUint16(g[8:], ocrAsc)
	binary.BigEndian.PutUint16(g[10:], 3) // endPtsOfContours[0] = point 3
	binary.BigEndian.PutUint16(g[12:], 0) // instructionLength
	for i := 0; i < 4; i++ {
		g[14+i] = 0x01 // flags: on-curve, int16 deltas
	}
	ox := g[18:]
	dxs := []int16{0, int16(w), 0, -int16(w)}
	dys := []int16{ocrDesc, 0, ocrAsc - ocrDesc, 0}
	for i, v := range dxs {
		binary.BigEndian.PutUint16(ox[i*2:], uint16(v))
	}
	for i, v := range dys {
		binary.BigEndian.PutUint16(ox[8+i*2:], uint16(v))
	}
	return g
}

func ocrTTFHead(maxW int) []byte {
	h := make([]byte, 54)
	binary.BigEndian.PutUint32(h[0:], 0x00010000) // version
	binary.BigEndian.PutUint32(h[4:], 0x00010000) // fontRevision
	// [8:] checkSumAdjustment — patched by the caller
	binary.BigEndian.PutUint32(h[12:], 0x5F0F3CF5) // magicNumber
	binary.BigEndian.PutUint16(h[16:], 0x0003)     // flags
	binary.BigEndian.PutUint16(h[18:], ocrEm)      // unitsPerEm
	// created/modified left zero
	binary.BigEndian.PutUint16(h[36:], 0)            // xMin
	binary.BigEndian.PutUint16(h[38:], ocrDescU)     // yMin
	binary.BigEndian.PutUint16(h[40:], uint16(maxW)) // xMax
	binary.BigEndian.PutUint16(h[42:], ocrAsc)       // yMax
	binary.BigEndian.PutUint16(h[46:], 8)            // lowestRecPPEM
	binary.BigEndian.PutUint16(h[48:], 2)            // fontDirectionHint
	binary.BigEndian.PutUint16(h[50:], 1)            // indexToLocFormat: long
	binary.BigEndian.PutUint16(h[52:], 0)            // glyphDataFormat
	return h
}

func ocrTTFHHead(numGlyphs, maxW int) []byte {
	h := make([]byte, 36)
	binary.BigEndian.PutUint32(h[0:], 0x00010000)
	binary.BigEndian.PutUint16(h[4:], ocrAsc)             // ascender
	binary.BigEndian.PutUint16(h[6:], ocrDescU)           // descender
	binary.BigEndian.PutUint16(h[10:], uint16(maxW))      // advanceWidthMax
	binary.BigEndian.PutUint16(h[16:], uint16(maxW))      // xMaxExtent
	binary.BigEndian.PutUint16(h[18:], 1)                 // caretSlopeRise
	binary.BigEndian.PutUint16(h[34:], uint16(numGlyphs)) // numberOfHMetrics
	return h
}

func ocrTTFMaxp(numGlyphs int) []byte {
	m := make([]byte, 32)
	binary.BigEndian.PutUint32(m[0:], 0x00010000)
	binary.BigEndian.PutUint16(m[4:], uint16(numGlyphs))
	return m
}

func ocrTTFPost() []byte {
	p := make([]byte, 32)
	binary.BigEndian.PutUint32(p[0:], 0x00030000) // version 3, no glyph names
	// italicAngle 0; underlinePosition/Thickness; isFixedPitch; memory fields 0
	binary.BigEndian.PutUint16(p[12:], 0xFF9C) // underlinePosition -100
	binary.BigEndian.PutUint16(p[14:], 50)     // underlineThickness
	return p
}

func ocrTTFOS2() []byte {
	b := make([]byte, 96)
	binary.BigEndian.PutUint16(b[0:], 4)     // version
	binary.BigEndian.PutUint16(b[2:], ocrEm) // xAvgCharWidth
	binary.BigEndian.PutUint16(b[4:], 400)   // usWeightClass (regular)
	binary.BigEndian.PutUint16(b[6:], 5)     // usWidthClass (normal)
	// fsType 0 (installable); subscript/superscript/strikeout metrics; panose 0
	binary.BigEndian.PutUint32(b[42:], 1) // ulUnicodeRange1: basic latin
	copy(b[46:], []byte("P2OC"))
	binary.BigEndian.PutUint16(b[50:], 0x0040)   // fsSelection: REGULAR
	binary.BigEndian.PutUint16(b[52:], 0x0020)   // usFirstCharIndex: space
	binary.BigEndian.PutUint16(b[54:], 0xFFFF)   // usLastCharIndex
	binary.BigEndian.PutUint16(b[56:], 800)      // sTypoAscender
	binary.BigEndian.PutUint16(b[58:], ocrDescU) // sTypoDescender -200
	binary.BigEndian.PutUint16(b[62:], ocrAsc)   // usWinAscent
	binary.BigEndian.PutUint16(b[64:], 200)      // usWinDescent
	binary.BigEndian.PutUint32(b[66:], 1)        // ulCodePageRange1: latin1
	binary.BigEndian.PutUint16(b[74:], 500)      // sxHeight (v4)
	binary.BigEndian.PutUint16(b[76:], 700)      // sCapHeight
	binary.BigEndian.PutUint16(b[80:], 32)       // usBreakChar
	binary.BigEndian.PutUint16(b[82:], 1)        // usMaxContext
	return b
}

func ocrTTFName() []byte {
	// one (3,1,0x409) record set: family, subfamily, full name, postscript
	names := []struct {
		id  uint16
		str string
	}{
		{1, "P2OcrFont"},
		{2, "Regular"},
		{4, "P2OcrFont"},
		{6, "P2OcrFont"},
	}
	strOffset := 6 + 12*len(names)
	var storage bytes.Buffer
	type rec struct {
		id, off uint16
	}
	recs := make([]rec, len(names))
	for i, n := range names {
		recs[i] = rec{n.id, uint16(storage.Len())}
		for _, r := range n.str {
			storage.WriteByte(byte(r >> 8))
			storage.WriteByte(byte(r))
		}
	}
	b := make([]byte, strOffset)
	binary.BigEndian.PutUint16(b[0:], 0) // format
	binary.BigEndian.PutUint16(b[2:], uint16(len(names)))
	binary.BigEndian.PutUint16(b[4:], uint16(strOffset))
	for i, r := range recs {
		e := b[6+i*12:]
		binary.BigEndian.PutUint16(e[0:], 3) // platformID
		binary.BigEndian.PutUint16(e[2:], 1) // encodingID
		binary.BigEndian.PutUint16(e[4:], 0x409)
		binary.BigEndian.PutUint16(e[6:], r.id)
		binary.BigEndian.PutUint16(e[8:], uint16(storage.Len()-int(r.off)))
		binary.BigEndian.PutUint16(e[10:], r.off)
	}
	return append(b, storage.Bytes()...)
}

func ocrTTFCmap(numGlyphs int) []byte {
	// (3,0) symbol cmap, format 4. Identity-H consumers never use it, but
	// some editors expect a cmap to exist. Codes 0xF000.. map to gids 0...
	// (capped at 0xFFFE).
	start := uint16(0xF000)
	count := numGlyphs
	if count > 0x1000 { // 0xF000..0xFFFF minus the sentinel
		count = 0x1000
	}
	end := start + uint16(count) - 1
	const segs = 2
	length := uint16(16 + segs*8)
	sub := make([]byte, length)
	binary.BigEndian.PutUint16(sub[0:], 4) // format
	binary.BigEndian.PutUint16(sub[2:], length)
	binary.BigEndian.PutUint16(sub[4:], 0) // language
	binary.BigEndian.PutUint16(sub[6:], segs*2)
	binary.BigEndian.PutUint16(sub[8:], 4)  // searchRange (2 segments)
	binary.BigEndian.PutUint16(sub[10:], 1) // entrySelector
	binary.BigEndian.PutUint16(sub[12:], 0) // rangeShift
	// endCode[]
	binary.BigEndian.PutUint16(sub[14:], end)
	binary.BigEndian.PutUint16(sub[16:], 0xFFFF)
	binary.BigEndian.PutUint16(sub[18:], 0) // reservedPad
	// startCode[]
	binary.BigEndian.PutUint16(sub[20:], start)
	binary.BigEndian.PutUint16(sub[22:], 0xFFFF)
	// idDelta[]
	binary.BigEndian.PutUint16(sub[24:], uint16(-start)) // code - 0xF000 = gid
	binary.BigEndian.PutUint16(sub[26:], 1)
	// idRangeOffset[]
	binary.BigEndian.PutUint16(sub[28:], 0)
	binary.BigEndian.PutUint16(sub[30:], 0)

	c := make([]byte, 12)
	binary.BigEndian.PutUint16(c[0:], 0) // version
	binary.BigEndian.PutUint16(c[2:], 1) // numTables
	binary.BigEndian.PutUint16(c[4:], 3)
	binary.BigEndian.PutUint16(c[6:], 0)
	binary.BigEndian.PutUint32(c[8:], 12)
	return append(c, sub...)
}
