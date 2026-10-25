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

	"golang.org/x/image/font"
	"golang.org/x/image/font/sfnt"
)

// TestOcrBlankTTFValidates parses the synthesized OCR font with the strict
// x/image/font/sfnt parser. pdfcpu's PDF-level validation does not look into
// font binaries, so this is the independent gate for the embedded font: a
// malformed table would break Foxit/Acrobat even though pdfcpu is happy.
func TestOcrBlankTTFValidates(t *testing.T) {
	for _, n := range []int{1, 2, 100, 5000} {
		widths := make([]int, n)
		for i := range widths {
			widths[i] = 200 + i%600
		}
		b := ocrBlankTTF(widths)
		f, err := sfnt.Parse(b)
		if err != nil {
			t.Fatalf("n=%d: sfnt.Parse: %v", n, err)
		}
		if got := f.NumGlyphs(); got != n+1 {
			t.Errorf("n=%d: NumGlyphs = %d, want %d", n, got, n+1)
		}
		buf := &sfnt.Buffer{}
		for i := 0; i <= n; i++ {
			bounds, _, err := f.GlyphBounds(buf, sfnt.GlyphIndex(i), 12<<6, font.HintingNone)
			if err != nil {
				t.Fatalf("n=%d glyph %d: Bounds: %v", n, i, err)
			}
			if bounds.Min.X >= bounds.Max.X || bounds.Min.Y >= bounds.Max.Y {
				t.Fatalf("n=%d glyph %d: degenerate bounds %v", n, i, bounds)
			}
		}
		m, err := f.Metrics(nil, 12<<6, font.HintingNone)
		if err != nil {
			t.Fatalf("n=%d: Metrics: %v", n, err)
		}
		if m.Height <= 0 {
			t.Errorf("n=%d: non-positive line height %v", n, m.Height)
		}
	}
}
