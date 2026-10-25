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

import "testing"

// TestParseToUnicodeCMapT42 covers the Type42 style CMaps of Founder subsets.
func TestParseToUnicodeCMapT42(t *testing.T) {
	cmapData := `/CIDInit /ProcSet findresource begin 12 dict begin begincmap /CIDSystemInfo <<
/Registry (/FZXBSK--GBK1-0+511+0) /Ordering (T42UV) /Supplement 0 >> def
/CMapName //FZXBSK--GBK1-0+511+0 def
/CMapVersion 1.000 def
/CMapType 2 def
1 begincodespacerange <0105> <558f> endcodespacerange
100 beginbfrange
<0104> <0104> <201c>
<0105> <0105> <201d>
<0337> <0337> <4e0b>
<0359> <0359> <4e2d>
<0360> <0360> <4e34>
endbfrange
2 beginbfchar
<0041> <0041>
endbfchar
endcmap CMapName currentdict /CMap defineresource pop end end
`

	cu, err := parseToUnicodeCMap([]byte(cmapData))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(cu.mappings) == 0 {
		s := newCMapScanner([]byte(cmapData))
		for i := 0; i < 30; i++ {
			tok, err := s.next()
			if err != nil {
				t.Logf("token %d ERR %v", i, err)
				break
			}
			if tok == "" {
				t.Logf("token %d <EOF>", i)
				break
			}
			t.Logf("token %d: %q", i, tok)
		}
		t.Fatalf("no mappings parsed")
	}

	if cu.codeLen != 2 {
		t.Fatalf("codeLen = %d, want 2", cu.codeLen)
	}

	for code, want := range map[uint32]string{
		0x0104: "“", 0x0105: "”", 0x0337: "下", 0x0359: "中", 0x0360: "\u4e34",
		0x0041: "A",
	} {
		got, ok := cu.lookup(code)
		if !ok {
			t.Errorf("code %#06x: missing mapping", code)
			continue
		}
		if got != want {
			t.Errorf("code %#06x: got %q, want %q", code, got, want)
		}
	}
}
