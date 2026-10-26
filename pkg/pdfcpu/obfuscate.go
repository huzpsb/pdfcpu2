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
	"context"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/log"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

// fontFileKeys lists the font descriptor entries carrying font programs.
var fontFileKeys = [...]string{"FontFile", "FontFile2", "FontFile3"}

// ObfuscateStats reports the text extraction mappings destroyed by
// ObfuscateFonts.
type ObfuscateStats struct {
	FontFiles int // embedded font programs with a garbage filled cmap table
	ToUnicode int // garbage filled ToUnicode CMap streams
	Skipped   int // fonts left untouched because rendering depends on their mappings
}

// ObfuscateFonts destroys the code to Unicode mappings that text extraction
// relies on, but only for fonts whose rendering does not depend on them:
//
//   - embedded CID-keyed fonts (Type0 with an embedded descendant font)
//     address their glyphs by glyph id, therefore the cmap table of their
//     font program and their ToUnicode CMap are both overwritten with
//     deterministic garbage,
//   - simple fonts and fonts without embedded font programs resolve glyphs
//     through their encoding, their ToUnicode CMap and the cmap table of
//     their font program. Destroying any of these mappings would change the
//     rendered output, so such fonts are skipped.
//
// Table offsets, lengths and glyph data are left untouched. Pages keep
// rendering exactly as before, while text extraction of the obfuscated
// fonts yields garbage. ObfuscateFonts supports cancellation.
func ObfuscateFonts(c context.Context, ctx *model.Context) (ObfuscateStats, error) {
	var stats ObfuscateStats

	if err := contextutil.Check(c); err != nil {
		return stats, err
	}
	if err := requireContextWithXRefTable(ctx); err != nil {
		return stats, fmt.Errorf("obfuscate fonts: source context: %w", err)
	}

	// Font programs of embedded CID fonts: safe to garbage fill, the
	// renderer addresses their glyphs by glyph id.
	cidFontFiles := map[int]bool{}
	// Font programs rendered through their cmap table by simple fonts:
	// never garbage filled.
	simpleFontFiles := map[int]bool{}
	// ToUnicode CMaps of embedded CID fonts.
	cidToUnicode := map[int]bool{}

	objNrs := make([]int, 0, len(ctx.Table))
	for objNr := range ctx.Table {
		objNrs = append(objNrs, objNr)
	}
	sort.Ints(objNrs)

	// Classify all font dicts.
	for _, objNr := range objNrs {
		if err := c.Err(); err != nil {
			return stats, err
		}

		entry := ctx.Table[objNr]
		if entry == nil || entry.Free || entry.Object == nil {
			continue
		}
		d, ok := entry.Object.(types.Dict)
		if !ok || !isFontDict(d) {
			continue
		}

		if subtypeIs(d, "Type0") {
			embedded := collectDescendantFontFiles(ctx, d, cidFontFiles)
			if !embedded {
				// Without embedded outlines the renderer substitutes a
				// fallback font selected through the ToUnicode mapping.
				stats.Skipped++
				continue
			}
			if ir := d.IndirectRefEntry("ToUnicode"); ir != nil {
				cidToUnicode[ir.ObjectNumber.Value()] = true
			}
			continue
		}

		if subtypeIs(d, "CIDFontType0") || subtypeIs(d, "CIDFontType2") {
			// Descendant dict of a Type0 font: it is classified through
			// its parent.
			continue
		}

		// Any other font is a simple font: encoding, ToUnicode and the
		// font program's cmap table all feed glyph resolution.
		collectFontFileRefs(ctx, d, simpleFontFiles)
		stats.Skipped++
	}

	// Garbage fill the cmap tables of all CID font programs that are not
	// shared with a simple font.
	for _, objNr := range sortedObjNrsFromSet(cidFontFiles) {
		if err := c.Err(); err != nil {
			return stats, err
		}
		if simpleFontFiles[objNr] {
			// Shared with a simple font: keep the cmap table.
			stats.Skipped++
			continue
		}
		destroyed, err := obfuscateFontFileStream(ctx, *types.NewIndirectRef(objNr, 0))
		if err != nil {
			if log.InfoEnabled() {
				log.Info.Printf("obfuscate: font program obj#%d: %v\n", objNr, err)
			}
			continue
		}
		if destroyed {
			stats.FontFiles++
		}
	}

	// Garbage fill the ToUnicode CMaps of all embedded CID fonts.
	for _, objNr := range sortedObjNrsFromSet(cidToUnicode) {
		if err := c.Err(); err != nil {
			return stats, err
		}
		destroyed, err := obfuscateToUnicodeStream(ctx, *types.NewIndirectRef(objNr, 0))
		if err != nil {
			if log.InfoEnabled() {
				log.Info.Printf("obfuscate: ToUnicode obj#%d: %v\n", objNr, err)
			}
			continue
		}
		if destroyed {
			stats.ToUnicode++
		}
	}

	return stats, nil
}

// isFontDict reports whether d is font related.
func isFontDict(d types.Dict) bool {
	for _, key := range [...]string{"BaseFont", "ToUnicode", "FontDescriptor", "DescendantFonts"} {
		if d.HasEntry(key) {
			return true
		}
	}
	return false
}

// subtypeIs reports whether d carries the given Subtype name.
func subtypeIs(d types.Dict, subtype string) bool {
	o, found := d.Find("Subtype")
	if !found {
		return false
	}
	s, ok := o.(types.Name)
	return ok && s.Value() == subtype
}

// collectDescendantFontFiles records the font file references of the
// descendant fonts of a Type0 font dict in m. It reports whether at least
// one embedded font program was found.
func collectDescendantFontFiles(ctx *model.Context, d types.Dict, m map[int]bool) bool {
	o, found := d.Find("DescendantFonts")
	if !found || o == nil {
		return false
	}
	arr, err := ctx.DereferenceArray(o)
	if err != nil {
		return false
	}
	embedded := false
	for _, df := range arr {
		dd, err := ctx.DereferenceDict(df)
		if err != nil || dd == nil {
			continue
		}
		if collectFontFileRefs(ctx, dd, m) {
			embedded = true
		}
	}
	return embedded
}

// collectFontFileRefs records the font file references found in the font
// descriptor of font dict d in m. It reports whether an embedded font
// program was found.
func collectFontFileRefs(ctx *model.Context, d types.Dict, m map[int]bool) bool {
	o, found := d.Find("FontDescriptor")
	if !found || o == nil {
		return false
	}
	fd, err := ctx.DereferenceDict(o)
	if err != nil || fd == nil {
		return false
	}
	embedded := false
	for _, key := range fontFileKeys {
		ir := fd.IndirectRefEntry(key)
		if ir == nil {
			continue
		}
		m[ir.ObjectNumber.Value()] = true
		embedded = true
	}
	return embedded
}

func sortedObjNrsFromSet(m map[int]bool) []int {
	objNrs := make([]int, 0, len(m))
	for objNr := range m {
		objNrs = append(objNrs, objNr)
	}
	sort.Ints(objNrs)
	return objNrs
}

// obfuscateToUnicodeStream overwrites the content of the ToUnicode CMap
// stream referenced by ir with deterministic garbage.
func obfuscateToUnicodeStream(ctx *model.Context, ir types.IndirectRef) (bool, error) {
	sd, err := streamForUpdate(ctx, ir)
	if err != nil || sd == nil {
		return false, err
	}
	if len(sd.Content) == 0 {
		return false, nil
	}

	content := obfuscationNoise(len(sd.Content), uint32(ir.ObjectNumber.Value()))
	if err := commitStreamContent(ctx, ir, sd, content); err != nil {
		return false, err
	}
	return true, nil
}

// obfuscateFontFileStream garbage fills the cmap table of the font program
// referenced by ir. Bare CFF (Type1C, CIDFontType0C) and Type1 programs
// carry no cmap table and remain unchanged.
func obfuscateFontFileStream(ctx *model.Context, ir types.IndirectRef) (bool, error) {
	sd, err := streamForUpdate(ctx, ir)
	if err != nil || sd == nil {
		return false, err
	}
	if !isSFNTFontProgram(sd.Content) {
		return false, nil
	}

	// Work on a copy: unfiltered streams share the backing array of
	// Content and Raw.
	content := make([]byte, len(sd.Content))
	copy(content, sd.Content)

	destroyed, err := destroyCMapTable(content, uint32(ir.ObjectNumber.Value()))
	if err != nil || !destroyed {
		return destroyed, err
	}
	if err := commitStreamContent(ctx, ir, sd, content); err != nil {
		return false, err
	}
	return true, nil
}

// streamForUpdate resolves ir into a decoded stream dict ready for content
// modification. A nil stream dict reports an undefined object.
func streamForUpdate(ctx *model.Context, ir types.IndirectRef) (*types.StreamDict, error) {
	sd, _, err := ctx.DereferenceStreamDict(ir)
	if err != nil {
		return nil, fmt.Errorf("dereference stream dict: %w", err)
	}
	if sd == nil {
		return nil, nil
	}
	if sd.Content == nil {
		if err := ctx.DecodeStream(sd); err != nil {
			return nil, fmt.Errorf("decode stream: %w", err)
		}
	}
	return sd, nil
}

// commitStreamContent applies the filter pipeline of sd to content, updates
// the stream length and stores the updated stream dict in the cross
// reference table entry of ir.
func commitStreamContent(ctx *model.Context, ir types.IndirectRef, sd *types.StreamDict, content []byte) error {
	sd.Content = content
	if err := sd.Encode(); err != nil {
		return fmt.Errorf("encode stream: %w", err)
	}
	entry, found := ctx.FindTableEntryForIndRef(&ir)
	if !found || entry == nil {
		return fmt.Errorf("obj#%d: missing cross reference entry", ir.ObjectNumber.Value())
	}
	entry.Object = *sd
	return nil
}

// isSFNTFontProgram reports whether b is a font program with an SFNT table
// directory, i.e. a TrueType or OpenType font that carries a cmap table.
// Bare CFF programs (FontFile3 subtypes Type1C, CIDFontType0C) and Type1
// programs (FontFile) are rejected by their magic bytes.
func isSFNTFontProgram(b []byte) bool {
	if len(b) < 12 {
		return false
	}
	switch binary.BigEndian.Uint32(b) {
	case 0x00010000, // TrueType
		0x74727565, // 'true'
		0x4F54544F: // 'OTTO' OpenType
		return true
	}
	return false
}

// destroyCMapTable overwrites the cmap tables of an SFNT font program with
// deterministic garbage and reports whether at least one cmap table was
// destroyed. Table directory offsets and lengths remain untouched,
// therefore the glyph data used for rendering stays intact.
func destroyCMapTable(b []byte, seed uint32) (bool, error) {
	if len(b) < 12 {
		return false, nil
	}
	numTables := int(binary.BigEndian.Uint16(b[4:6]))
	if numTables <= 0 {
		return false, nil
	}
	if 12+16*numTables > len(b) {
		return false, fmt.Errorf("invalid table directory: %d entries in %d bytes", numTables, len(b))
	}

	destroyed := false
	for i := 0; i < numTables; i++ {
		rec := b[12+16*i : 12+16*i+16]
		if string(rec[:4]) != "cmap" {
			continue
		}
		off := binary.BigEndian.Uint32(rec[8:12])
		length := binary.BigEndian.Uint32(rec[12:16])
		if length == 0 || off < 12+16*uint32(numTables) || uint64(off)+uint64(length) > uint64(len(b)) {
			// Corrupt entry: garbage filling an arbitrary range could
			// destroy glyph data.
			continue
		}
		table := b[off : off+length]
		copy(table, obfuscationNoise(int(length), seed))
		binary.BigEndian.PutUint32(rec[4:8], sfntChecksum(table))
		destroyed = true
	}
	return destroyed, nil
}

// sfntChecksum computes the checksum of an SFNT table: the sum of the
// table bytes padded to a multiple of four, interpreted as big endian
// uint32 values.
func sfntChecksum(b []byte) uint32 {
	var sum uint32
	for ; len(b) >= 4; b = b[4:] {
		sum += binary.BigEndian.Uint32(b)
	}
	var pad [4]byte
	copy(pad[:], b)
	return sum + binary.BigEndian.Uint32(pad[:])
}

// obfuscationNoise returns n deterministic pseudo random bytes derived
// from seed. The same input always produces the same output, which keeps
// obfuscation reproducible, while the result leaves no recoverable
// mapping behind.
func obfuscationNoise(n int, seed uint32) []byte {
	b := make([]byte, n)
	x := seed ^ 0x9E3779B9
	if x == 0 {
		x = 0x9E3779B9
	}
	for i := range b {
		// xorshift32
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x >> 16)
	}
	return b
}
