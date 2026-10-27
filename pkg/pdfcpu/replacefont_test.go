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

	"github.com/huzpsb/pdfcpu2/internal/fallbackfont"
	"golang.org/x/image/font"
	"golang.org/x/image/font/sfnt"
)

// TestVivoSubsetValidates verifies the subset is a well-formed TrueType font
// per x/image/font/sfnt's strict parser and that outlines, metrics and the
// cmap survive subsetting.
func TestVivoSubsetValidates(t *testing.T) {
	runes := []rune("AVela 你好世界!?.")
	sub, err := buildVivoSubset(fallbackfont.Bytes(), runes)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.missing) != 0 {
		t.Fatalf("vivo sans should cover the test runes, missing %q", string(sub.missing))
	}

	f, err := sfnt.Parse(sub.ttf)
	if err != nil {
		t.Fatalf("sfnt.Parse: %v", err)
	}
	if got := f.NumGlyphs(); got < len(runes)+1 {
		t.Fatalf("NumGlyphs = %d, want >= %d", got, len(runes)+1)
	}

	full, err := sfnt.Parse(fallbackfont.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	buf := &sfnt.Buffer{}
	for _, r := range runes {
		// subset cmap (via sfnt GlyphIndex) resolves the rune
		gidNew, err := f.GlyphIndex(buf, r)
		if err != nil {
			t.Fatalf("rune %q: GlyphIndex subset: %v", r, err)
		}
		if gidNew == 0 {
			t.Fatalf("rune %q not mapped in subset cmap", r)
		}
		if int(gidNew) != int(sub.code(r)) {
			t.Fatalf("rune %q: sfnt gid %d != subset code %d", r, gidNew, sub.code(r))
		}

		// the outline equals the full font's outline for the same rune
		gidOld, err := full.GlyphIndex(buf, r)
		if err != nil {
			t.Fatal(err)
		}
		segsNew, err := f.LoadGlyph(buf, gidNew, 12<<6, nil)
		if err != nil {
			t.Fatalf("rune %q: LoadGlyph subset: %v", r, err)
		}
		segsOld, err := full.LoadGlyph(buf, gidOld, 12<<6, nil)
		if err != nil {
			t.Fatalf("rune %q: LoadGlyph full: %v", r, err)
		}
		if len(segsNew) != len(segsOld) {
			t.Fatalf("rune %q: segments %d != %d", r, len(segsNew), len(segsOld))
		}

		// advance widths match (scaled 1000/em vs font units)
		advOld, err := full.GlyphAdvance(buf, gidOld, 12<<6, font.HintingNone)
		if err != nil {
			t.Fatal(err)
		}
		advNew, err := f.GlyphAdvance(buf, gidNew, 12<<6, font.HintingNone)
		if err != nil {
			t.Fatal(err)
		}
		if advNew != advOld {
			t.Fatalf("rune %q: advance %d != %d", r, advNew, advOld)
		}
	}
}

// TestVivoSubsetMissingRunes verifies unmapped runes keep code alignment.
func TestVivoSubsetMissingRunes(t *testing.T) {
	runes := []rune{'A', 0x10FFFE, 'Z'} // 0x10FFFE has no glyph
	sub, err := buildVivoSubset(fallbackfont.Bytes(), runes)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.missing) != 1 || sub.missing[0] != 0x10FFFE {
		t.Fatalf("missing = %v", sub.missing)
	}
	// input order kept: 'A'(1), 0x10FFFE(2), 'Z'(3) — codes are positional
	if sub.code('A') != 1 || sub.code('Z') != 3 {
		t.Fatalf("codes misaligned: A=%d Z=%d", sub.code('A'), sub.code('Z'))
	}
	if _, err := sfnt.Parse(sub.ttf); err != nil {
		t.Fatalf("sfnt.Parse: %v", err)
	}
}

// TestVivoSubsetRepeatability verifies deterministic output.
func TestVivoSubsetRepeatability(t *testing.T) {
	runes := []rune("pdfcpu2 你好")
	a, err := buildVivoSubset(fallbackfont.Bytes(), runes)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildVivoSubset(fallbackfont.Bytes(), runes)
	if err != nil {
		t.Fatal(err)
	}
	if string(a.ttf) != string(b.ttf) {
		t.Fatal("subset is not deterministic")
	}
}
