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
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

func TestObfuscationNoise(t *testing.T) {
	a := obfuscationNoise(64, 42)
	b := obfuscationNoise(64, 42)
	if !bytes.Equal(a, b) {
		t.Fatal("obfuscation noise must be deterministic for a given seed")
	}
	if c := obfuscationNoise(64, 43); bytes.Equal(a, c) {
		t.Fatal("different seeds must produce different noise")
	}
	if got := obfuscationNoise(0, 7); len(got) != 0 {
		t.Fatalf("expected empty noise, got %d bytes", len(got))
	}

	zeroes := true
	for _, b := range obfuscationNoise(512, 1) {
		if b != 0 {
			zeroes = false
			break
		}
	}
	if zeroes {
		t.Fatal("noise must not degrade to zero bytes")
	}
}

func TestSfntChecksum(t *testing.T) {
	if got := sfntChecksum([]byte{1, 0, 0, 0}); got != 0x01000000 {
		t.Fatalf("got %#x", got)
	}
	// Padding with zero bytes for lengths not divisible by 4.
	if got := sfntChecksum([]byte{1, 2, 3}); got != 0x01020300 {
		t.Fatalf("got %#x", got)
	}
	if got := sfntChecksum(nil); got != 0 {
		t.Fatalf("got %#x", got)
	}
}

func TestIsSFNTFontProgram(t *testing.T) {
	tt := []struct {
		name string
		b    []byte
		want bool
	}{
		{"truetype", []byte{0x00, 0x01, 0x00, 0x00, 0, 0, 0, 0, 0, 0, 0, 0}, true},
		{"true", append([]byte("true"), make([]byte, 8)...), true},
		{"otto", append([]byte("OTTO"), make([]byte, 8)...), true},
		{"bare cff", []byte{0x01, 0x00, 0x04, 0x01, 0, 0, 0, 0, 0, 0, 0, 0}, false},
		{"type1", []byte("%!PS-AdobeFont"), false},
		{"short", []byte{0x00, 0x01, 0x00}, false},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSFNTFontProgram(tc.b); got != tc.want {
				t.Fatalf("got %t", got)
			}
		})
	}
}

// testSFNT returns a minimal SFNT font program with head and cmap tables.
func testSFNT(cmap []byte) []byte {
	b := make([]byte, 12+2*16+8+len(cmap))
	binary.BigEndian.PutUint32(b[0:], 0x00010000) // sfnt version
	binary.BigEndian.PutUint16(b[4:], 2)          // numTables

	headRec := b[12:28]
	copy(headRec[0:4], "head")
	binary.BigEndian.PutUint32(headRec[8:], 44)
	binary.BigEndian.PutUint32(headRec[12:], 8)
	copy(b[44:52], "HEADDATA")

	cmapRec := b[28:44]
	copy(cmapRec[0:4], "cmap")
	binary.BigEndian.PutUint32(cmapRec[8:], 52)
	binary.BigEndian.PutUint32(cmapRec[12:], uint32(len(cmap)))
	copy(b[52:], cmap)

	return b
}

func TestDestroyCMapTable(t *testing.T) {
	cmap := bytes.Repeat([]byte{0xAB}, 10)
	font := testSFNT(cmap)

	destroyed, err := destroyCMapTable(font, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !destroyed {
		t.Fatal("expected cmap table to be destroyed")
	}

	// The cmap table must be garbage and must differ from the original.
	if bytes.Equal(font[52:], cmap) {
		t.Fatal("cmap data was not garbage filled")
	}
	if want := obfuscationNoise(len(cmap), 9); !bytes.Equal(font[52:], want) {
		t.Fatal("cmap data does not match the deterministic noise")
	}

	// Everything else must be untouched.
	if !bytes.Equal(font[44:52], []byte("HEADDATA")) {
		t.Fatal("head data was modified")
	}
	if got := binary.BigEndian.Uint32(font[36:40]); got != 52 {
		t.Fatalf("cmap offset modified: %d", got)
	}
	if got := binary.BigEndian.Uint32(font[40:44]); got != uint32(len(cmap)) {
		t.Fatalf("cmap length modified: %d", got)
	}

	// The table checksum must match the garbage content.
	if got := binary.BigEndian.Uint32(font[32:36]); got != sfntChecksum(font[52:]) {
		t.Fatalf("checksum %#x does not match table content", got)
	}
}

func TestDestroyCMapTableWithoutCMap(t *testing.T) {
	font := testSFNT(nil)
	// Rewrite second record to another table.
	copy(font[28:32], "kern")

	destroyed, err := destroyCMapTable(font, 1)
	if err != nil {
		t.Fatal(err)
	}
	if destroyed {
		t.Fatal("expected no cmap table to be destroyed")
	}
}

func TestDestroyCMapTableRejectsCorruptDirectory(t *testing.T) {
	font := make([]byte, 20)
	binary.BigEndian.PutUint16(font[4:], 200) // numTables exceeds file size

	if _, err := destroyCMapTable(font, 1); err == nil {
		t.Fatal("expected error for corrupt table directory")
	}
}

func TestDestroyCMapTableSkipsOutOfRangeEntry(t *testing.T) {
	font := testSFNT(bytes.Repeat([]byte{0xCD}, 4))
	// Let cmap point beyond the file.
	binary.BigEndian.PutUint32(font[36:40], 1<<20)

	destroyed, err := destroyCMapTable(font, 1)
	if err != nil {
		t.Fatal(err)
	}
	if destroyed {
		t.Fatal("expected out of range cmap entry to be skipped")
	}
}

// obfuscateTestContext builds a context with an embedded CID font, a simple
// font with its own embedded font program and a non embedded CID font.
func obfuscateTestContext(t *testing.T) *model.Context {
	t.Helper()

	ctx, err := model.NewContext(strings.NewReader(""), model.NewDefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}

	toUnicode := "%!PS-Adobe true\n1 beginbfchar\n<0001> <0041>\nendbfchar\n"
	type0Font := types.Dict{
		"Type":            types.Name("Font"),
		"Subtype":         types.Name("Type0"),
		"BaseFont":        types.Name("CIDTest"),
		"Encoding":        types.Name("Identity-H"),
		"ToUnicode":       *types.NewIndirectRef(2, 0),
		"DescendantFonts": types.Array{*types.NewIndirectRef(3, 0)},
	}
	cidFont := types.Dict{
		"Type":           types.Name("Font"),
		"Subtype":        types.Name("CIDFontType2"),
		"BaseFont":       types.Name("CIDTest"),
		"FontDescriptor": *types.NewIndirectRef(4, 0),
	}
	cidFontDescriptor := types.Dict{
		"Type":      types.Name("FontDescriptor"),
		"FontName":  types.Name("CIDTest"),
		"FontFile2": *types.NewIndirectRef(5, 0),
	}
	simpleFont := types.Dict{
		"Type":           types.Name("Font"),
		"Subtype":        types.Name("TrueType"),
		"BaseFont":       types.Name("SimpleTest"),
		"ToUnicode":      *types.NewIndirectRef(8, 0),
		"FontDescriptor": *types.NewIndirectRef(9, 0),
	}
	simpleFontDescriptor := types.Dict{
		"Type":      types.Name("FontDescriptor"),
		"FontName":  types.Name("SimpleTest"),
		"FontFile2": *types.NewIndirectRef(10, 0),
	}
	nonEmbeddedType0 := types.Dict{
		"Type":            types.Name("Font"),
		"Subtype":         types.Name("Type0"),
		"BaseFont":        types.Name("SubstTest"),
		"Encoding":        types.Name("Identity-H"),
		"ToUnicode":       *types.NewIndirectRef(12, 0),
		"DescendantFonts": types.Array{*types.NewIndirectRef(13, 0)},
	}
	nonEmbeddedCIDFont := types.Dict{
		"Type":      types.Name("Font"),
		"Subtype":   types.Name("CIDFontType2"),
		"BaseFont":  types.Name("SubstTest"),
	}

	ctx.Table = map[int]*model.XRefTableEntry{
		1:  model.NewXRefTableEntryGen0(type0Font),
		2:  model.NewXRefTableEntryGen0(types.StreamDict{Dict: types.NewDict(), Content: []byte(toUnicode)}),
		3:  model.NewXRefTableEntryGen0(cidFont),
		4:  model.NewXRefTableEntryGen0(cidFontDescriptor),
		5:  model.NewXRefTableEntryGen0(types.StreamDict{Dict: types.NewDict(), Content: testSFNT(bytes.Repeat([]byte{0x11}, 16))}),
		7:  model.NewXRefTableEntryGen0(simpleFont),
		8:  model.NewXRefTableEntryGen0(types.StreamDict{Dict: types.NewDict(), Content: []byte(toUnicode)}),
		9:  model.NewXRefTableEntryGen0(simpleFontDescriptor),
		10: model.NewXRefTableEntryGen0(types.StreamDict{Dict: types.NewDict(), Content: testSFNT(bytes.Repeat([]byte{0x22}, 16))}),
		11: model.NewXRefTableEntryGen0(nonEmbeddedType0),
		12: model.NewXRefTableEntryGen0(types.StreamDict{Dict: types.NewDict(), Content: []byte(toUnicode)}),
		13: model.NewXRefTableEntryGen0(nonEmbeddedCIDFont),
	}

	return ctx
}

func streamContent(t *testing.T, ctx *model.Context, objNr int) []byte {
	t.Helper()
	entry, found := ctx.FindTableEntry(objNr, 0)
	if !found || entry == nil {
		t.Fatalf("missing xref entry for obj#%d", objNr)
	}
	sd, ok := entry.Object.(types.StreamDict)
	if !ok {
		t.Fatalf("obj#%d is no stream dict", objNr)
	}
	return sd.Content
}

func TestObfuscateFonts(t *testing.T) {
	ctx := obfuscateTestContext(t)

	stats, err := ObfuscateFonts(context.Background(), ctx)
	if err != nil {
		t.Fatal(err)
	}

	if stats.FontFiles != 1 {
		t.Fatalf("expected 1 font program, got %d", stats.FontFiles)
	}
	if stats.ToUnicode != 1 {
		t.Fatalf("expected 1 ToUnicode stream, got %d", stats.ToUnicode)
	}
	// The simple font and the non embedded CID font are skipped.
	if stats.Skipped != 2 {
		t.Fatalf("expected 2 skipped fonts, got %d", stats.Skipped)
	}

	// CID font: ToUnicode replaced by deterministic garbage.
	content := streamContent(t, ctx, 2)
	if strings.Contains(string(content), "bfchar") {
		t.Fatal("ToUnicode CMap content survived")
	}
	if want := obfuscationNoise(len(content), 2); !bytes.Equal(content, want) {
		t.Fatal("ToUnicode content does not match deterministic noise")
	}
	if l := ctx.Table[2].Object.(types.StreamDict).Int64Entry("Length"); l == nil || *l != int64(len(content)) {
		t.Fatal("stream length not updated")
	}

	// CID font program: cmap garbage, head data intact.
	font := streamContent(t, ctx, 5)
	if bytes.Equal(font[52:], bytes.Repeat([]byte{0x11}, 16)) {
		t.Fatal("cmap table survived")
	}
	if !bytes.Equal(font[44:52], []byte("HEADDATA")) {
		t.Fatal("head data was modified")
	}

	// Simple font: ToUnicode and font program must survive untouched.
	if got := streamContent(t, ctx, 8); strings.Contains(string(got), "bfchar") == false {
		t.Fatal("simple font ToUnicode must not be destroyed")
	}
	if got := streamContent(t, ctx, 10); !bytes.Equal(got[52:], bytes.Repeat([]byte{0x22}, 16)) {
		t.Fatal("simple font program cmap must not be destroyed")
	}

	// Non embedded CID font: ToUnicode must survive untouched.
	if got := streamContent(t, ctx, 12); !strings.Contains(string(got), "bfchar") {
		t.Fatal("ToUnicode of a non embedded font must not be destroyed")
	}
}

// TestObfuscateFontsSharedFontFile verifies that a font program shared
// between a CID font and a simple font is preserved.
func TestObfuscateFontsSharedFontFile(t *testing.T) {
	ctx := obfuscateTestContext(t)

	// Let the simple font reference the CID fonts font program.
	sd := ctx.Table[9].Object.(types.Dict)
	sd["FontFile2"] = *types.NewIndirectRef(5, 0)

	stats, err := ObfuscateFonts(context.Background(), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.FontFiles != 0 {
		t.Fatalf("expected 0 font programs, got %d", stats.FontFiles)
	}
	if got := streamContent(t, ctx, 5); !bytes.Equal(got[52:], bytes.Repeat([]byte{0x11}, 16)) {
		t.Fatal("shared font program must not be destroyed")
	}
}

func TestObfuscateFontsRequiresContext(t *testing.T) {
	if _, err := ObfuscateFonts(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil context")
	}
}
