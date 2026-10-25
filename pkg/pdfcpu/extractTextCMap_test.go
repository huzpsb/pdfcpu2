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
	"testing"
)

// TestParseToUnicodeCMapBFRangeArray covers hex and array destination forms.
func TestParseToUnicodeCMapBFRangeArray(t *testing.T) {
	cmapData := `/CIDInit /ProcSet findresource begin 12 dict begin begincmap /CIDSystemInfo <<
/Registry (F1+0) /Ordering (T1UV) /Supplement 0 >> def
/CMapName /F1+0 def
/CMapType 2 def
1 begincodespacerange <02> <e9> endcodespacerange
6 beginbfchar
<20> <0020>
<26> <0026>
<59> <0059>
<80> <2022>
<a9> <00A9>
<e9> <00E9>
endbfchar
7 beginbfrange
<28> <29> <0028>
<2c> <3b> <002C>
<40> <47> <0040>
<49> <50> <0049>
<52> <57> <0052>
<61> <69> <0061>
<6b> <79> <006B>
endbfrange
2 beginbfrange
<02> <02> [<00540068>]
<03> <03> [<00660066>]
endbfrange
endcmap CMapName currentdict /CMap defineresource pop end end
`

	cu, err := parseToUnicodeCMap([]byte(cmapData))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if cu.codeLen != 1 {
		t.Fatalf("codeLen = %d, want 1", cu.codeLen)
	}

	want := map[uint32]string{
		0x20: " ", 0x26: "&", 0x59: "Y", 0x80: "•", 0xa9: "©", 0xe9: "é",
		0x28: "(", 0x29: ")", 0x2c: ",", 0x3b: ";", 0x40: "@", 0x47: "G",
		0x49: "I", 0x50: "P", 0x52: "R", 0x57: "W", 0x61: "a", 0x69: "i",
		0x6b: "k", 0x79: "y", 0x4d: "M",
		0x02: "Th", 0x03: "ff",
	}

	for code, s := range want {
		got, ok := cu.lookup(code)
		if !ok {
			t.Errorf("code %#04x: missing mapping", code)
			continue
		}
		if got != s {
			t.Errorf("code %#04x: got %q, want %q", code, got, s)
		}
	}
}

// TestParseToUnicodeCMapTwoByteCode covers 2-byte codespace CID fonts.
func TestParseToUnicodeCMapTwoByteCode(t *testing.T) {
	cmapData := `2 beginbfrange
<0000> <005e> <0020>
<005f> <005f> <2019>
endbfrange
1 beginbfchar
<0060> <6D4B>
endbfchar
`

	cu, err := parseToUnicodeCMap([]byte(cmapData))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if cu.codeLen != 2 {
		t.Fatalf("codeLen = %d, want 2", cu.codeLen)
	}

	for code, s := range map[uint32]string{
		0x0000: " ", 0x005e: "~"[:0] + "\u007e", 0x005f: "’", 0x0060: "测",
	} {
		got, ok := cu.lookup(code)
		if !ok {
			t.Errorf("code %#06x: missing mapping", code)
			continue
		}
		if got != s {
			t.Errorf("code %#06x: got %q, want %q", code, got, s)
		}
	}
}

// TestGlyphNameToString covers structured and AGL subset names.
func TestGlyphNameToString(t *testing.T) {
	for name, want := range map[string]string{
		"A":      "A",
		"Th":     "Th",
		"fi":     "ﬁ",
		"uni4E2D": "中",
		"eacute": "é",
		"quotedblleft": "“",
		"infinity": "∞",
	} {
		got, ok := glyphNameToString(name)
		if !ok {
			t.Errorf("glyph %q: no mapping", name)
			continue
		}
		if got != want {
			t.Errorf("glyph %q: got %q, want %q", name, got, want)
		}
	}
}

// TestSimpleEncodingDifferencesMultiChar covers multi character Differences.
func TestSimpleEncodingDifferencesMultiChar(t *testing.T) {
	se := *stdEncoding
	se.applyDifferences(map[byte]string{2: "Th", 3: "space", 65: "eacute"})
	if got := se.decode([]byte{2, 3, 65}); got != "Th é" {
		t.Fatalf("decode = %q, want %q", got, "Th é")
	}
}
