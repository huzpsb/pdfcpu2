package render

import (
	"testing"

	"golang.org/x/image/font/sfnt"
)

// TestFallbackLoad checks sfnt glyph loading for the bundled fallback font.
func TestFallbackLoad(t *testing.T) {
	ff, upem, err := loadFallback()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("upem=%d glyphs=%d", int(upem), ff.NumGlyphs())
	for _, r := range []rune("EuropeanRespiratorySociety2024journal") {
		gid, err := ff.GlyphIndex(&sfnt.Buffer{}, r)
		if err != nil {
			t.Fatalf("rune %U GlyphIndex: %v", r, err)
		}
		segs, err := ff.LoadGlyph(&sfnt.Buffer{}, gid, fixed266(upem), nil)
		t.Logf("rune %U gid=%d segs=%d err=%v", r, gid, len(segs), err)
		if err != nil {
			t.Fatalf("rune %U LoadGlyph: %v", r, err)
		}
	}
}
