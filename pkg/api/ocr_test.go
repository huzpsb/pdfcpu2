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

package api_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/huzpsb/pdfcpu2/pkg/api"
)

// ocrCopy copies testdata into dir and returns the path.
func ocrCopy(t *testing.T, src, dir string) string {
	t.Helper()
	bb, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, filepath.Base(src))
	if err := os.WriteFile(dst, bb, 0o666); err != nil {
		t.Fatal(err)
	}
	return dst
}

func ocrWords(s string) []string {
	return regexp.MustCompile(`[a-z]+`).FindAllString(strings.ToLower(s), -1)
}

// wordScores returns (precision, recall) of got vs want on word multisets.
func wordScores(got, want []string) (float64, float64) {
	if len(got) == 0 || len(want) == 0 {
		return 0, 0
	}
	count := map[string]int{}
	for _, w := range want {
		count[w]++
	}
	tp := 0
	for _, w := range got {
		if count[w] > 0 {
			count[w]--
			tp++
		}
	}
	return float64(tp) / float64(len(got)), float64(tp) / float64(len(want))
}

// TestOCRWaldenScan runs the full PDF-OCR pipeline over the committed scan
// (WaldenFull.pdf p5-10 @150dpi; p6 split top/bottom, p7 split left/right)
// and validates:
//   - the invisible text layer extracts with pdfcpu's own text extractor
//   - word-level precision/recall vs the original text layer (>= 0.93)
//   - text lands in the correct half of split-image pages (position mapping)
//   - a second run is a no-op (idempotency marker)
func TestOCRWaldenScan(t *testing.T) {
	if testing.Short() {
		t.Skip("slow OCR integration test")
	}
	ctx := context.Background()
	dir := t.TempDir()

	in := ocrCopy(t, filepath.Join("..", "testdata", "WaldenScan.pdf"), dir)
	out := filepath.Join(dir, "ocr.pdf")
	n, err := api.OCRFile(ctx, in, out, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := 8; n != want { // 1 + 2 + 2 + 1 + 1 + 1
		t.Fatalf("processed %d images, want %d", n, want)
	}

	// reference text: the original pages 5-10 carry a real text layer
	refText, err := api.ExtractTextString(ctx, mustOpenT(t, filepath.Join("..", "testdata", "WaldenFull.pdf")),
		[]string{"5-10"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotText, err := api.ExtractTextString(ctx, mustOpenT(t, out), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, r := wordScores(ocrWords(gotText), ocrWords(refText))
	t.Logf("word precision %.3f, recall %.3f", p, r)
	if p < 0.93 || r < 0.93 {
		t.Errorf("word precision/recall too low: %.3f/%.3f", p, r)
	}

	// position mapping on split pages
	texts, err := api.ExtractText(ctx, mustOpenT(t, out), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(texts) != 6 {
		t.Fatalf("got %d pages of text, want 6", len(texts))
	}

	// page 2 = top/bottom halves: probe words from reference lines that sit
	// well inside one half (the cut line sits at y=421) and verify they land
	// in the same half of the OCR page (allowing the unclip box to spill a
	// little across the cut).
	ref6, err := api.ExtractText(ctx, mustOpenT(t, filepath.Join("..", "testdata", "WaldenFull.pdf")),
		[]string{"6"}, nil, nil)
	if err != nil || len(ref6) != 1 {
		t.Fatalf("reference page 6: %v", err)
	}
	pick := func(yMin, yMax float64) string {
		for _, l := range ref6[0].Lines {
			if l.Y > yMin && l.Y < yMax {
				for _, w := range ocrWords(l.Text) {
					if len(w) >= 6 {
						return w
					}
				}
			}
		}
		return ""
	}
	topWord, botWord := pick(500, 800), pick(40, 340)
	if topWord == "" || botWord == "" {
		t.Fatal("no probe words from reference page 6")
	}
	find := func(word string, yMin, yMax float64) bool {
		for _, l := range texts[1].Lines {
			if l.Y >= yMin && l.Y <= yMax && strings.Contains(strings.ToLower(l.Text), word) {
				return true
			}
		}
		return false
	}
	if !find(topWord, 440, 842) {
		t.Errorf("word %q from top half not found in top half of page 2", topWord)
	}
	if !find(botWord, 0, 400) {
		t.Errorf("word %q from bottom half not found in bottom half of page 2", botWord)
	}

	// page 3 = left/right halves: every line starts either left of the cut
	// (x < 100, tile origin) or right of it (x > 280). No line may start in
	// the middle of the page — that would mean a CTM mix-up between tiles.
	for _, l := range texts[2].Lines {
		if l.X >= 100 && l.X <= 280 {
			t.Errorf("page 3 line %q starts at x=%.1f inside the tile gap", l.Text, l.X)
		}
	}

	// idempotency: the second run must not process anything nor write output
	out2 := filepath.Join(dir, "ocr2.pdf")
	n2, err := api.OCRFile(ctx, out, out2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("second run processed %d images, want 0", n2)
	}
	if _, err := os.Stat(out2); !os.IsNotExist(err) {
		t.Errorf("second run wrote %s, want no output", out2)
	}
}

// TestOCRChlScan covers the CJK path end to end: the invisible layer of the
// OCR'd chl scan must contain the reference lines' text.
func TestOCRChlScan(t *testing.T) {
	if testing.Short() {
		t.Skip("slow OCR integration test")
	}
	ctx := context.Background()
	dir := t.TempDir()
	in := ocrCopy(t, filepath.Join("..", "ocr", "testdata", "ChlScan.pdf"), dir)
	out := filepath.Join(dir, "chl_ocr.pdf")
	if _, err := api.OCRFile(ctx, in, out, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := api.ExtractTextString(ctx, mustOpenT(t, out), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// probes from ocr/testdata/golden_chl.txt (ASCII + CJK)
	for _, probe := range []string{"GregTech", "32578", "红石", "连接", "移除"} {
		if !strings.Contains(got, probe) {
			t.Errorf("extracted text missing probe %q", probe)
		}
	}
}

func mustOpenT(t *testing.T, p string) *os.File {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
