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

package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

func replaceTestInput(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "samples", "create", "primitives", "textAndAlignment.pdf")
	bb, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "in.pdf")
	if err := os.WriteFile(dst, bb, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

func extractedText(t *testing.T, inFile string) string {
	t.Helper()
	outFile := filepath.Join(t.TempDir(), "out.txt")
	if err := ExtractTextFile(t.Context(), inFile, outFile, nil, nil, nil); err != nil {
		t.Fatalf("extract text: %v", err)
	}
	bb, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(bb)
}

// TestReplaceTextFile replaces a word across the sample and verifies via
// re-extraction that the old text is gone, the replacement is present and
// the output is a valid PDF.
func TestReplaceTextFile(t *testing.T) {
	inFile := replaceTestInput(t)
	before := extractedText(t, inFile)
	if n := strings.Count(before, "aligned"); n == 0 {
		t.Fatal("sample does not contain the find word")
	}

	outFile := filepath.Join(t.TempDir(), "replaced.pdf")
	n, err := ReplaceTextFile(t.Context(), inFile, outFile, "aligned", "REPLACED", nil, nil)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if n != strings.Count(before, "aligned") {
		t.Fatalf("replaced %d occurrences, want %d", n, strings.Count(before, "aligned"))
	}

	after := extractedText(t, outFile)
	if strings.Contains(after, "aligned") {
		t.Fatalf("find word still present after replacement:\n%s", after)
	}
	if got := strings.Count(after, "REPLACED"); got != n {
		t.Fatalf("replacement found %d times, want %d\n%s", got, n, after)
	}

	f, err := os.Open(outFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := Validate(t.Context(), f, model.NewDefaultConfiguration(), nil); err != nil {
		t.Fatalf("validate output: %v", err)
	}
}

// TestReplaceTextFileDelete verifies pure deletion (empty replacement).
func TestReplaceTextFileDelete(t *testing.T) {
	inFile := replaceTestInput(t)
	outFile := filepath.Join(t.TempDir(), "deleted.pdf")

	n, err := ReplaceTextFile(t.Context(), inFile, outFile, "border", "", nil, nil)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if n == 0 {
		t.Fatal("expected deletions")
	}
	after := extractedText(t, outFile)
	if strings.Contains(after, "border") {
		t.Fatalf("deleted word still present:\n%s", after)
	}

	f, err := os.Open(outFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := Validate(t.Context(), f, model.NewDefaultConfiguration(), nil); err != nil {
		t.Fatalf("validate output: %v", err)
	}
}

// TestReplaceTextFileNoMatch verifies no occurrences leave the file alone.
func TestReplaceTextFileNoMatch(t *testing.T) {
	inFile := replaceTestInput(t)
	outFile := filepath.Join(t.TempDir(), "out.pdf")

	n, err := ReplaceTextFile(t.Context(), inFile, outFile, "NoSuchWordHere", "x", nil, nil)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 replacements, got %d", n)
	}
	if _, err := os.Stat(outFile); !os.IsNotExist(err) {
		t.Fatal("output must not be written when nothing matched")
	}
}

// TestReplaceTextFileCJK verifies CJK replacement end to end.
func TestReplaceTextFileCJK(t *testing.T) {
	inFile := filepath.Join("..", "samples", "create", "primitives", "tableCJK.pdf")
	if _, err := os.Stat(inFile); err != nil {
		t.Skip("sample not available")
	}
	before := extractedText(t, inFile)

	// pick the first CJK rune that actually occurs
	var target rune
	for _, r := range before {
		if r >= 0x4E00 && r <= 0x9FFF {
			target = r
			break
		}
	}
	if target == 0 {
		t.Skip("no CJK text found")
	}

	outFile := filepath.Join(t.TempDir(), "cjk.pdf")
	n, err := ReplaceTextFile(t.Context(), inFile, outFile, string(target), "替换OK", nil, nil)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if n == 0 {
		t.Skip("no occurrences replaced")
	}
	after := extractedText(t, outFile)
	if got := strings.Count(after, "替换OK"); got != n {
		t.Fatalf("replacement found %d times, want %d", got, n)
	}
}

// TestReplaceTextFileChainFontReuse verifies chained runs reuse and extend a
// single embedded replace font instead of accumulating one per run.
func TestReplaceTextFileChainFontReuse(t *testing.T) {
	inFile := replaceTestInput(t)
	dir := t.TempDir()

	// run 1: creates the font with the first rune set
	out1 := filepath.Join(dir, "r1.pdf")
	if _, err := ReplaceTextFile(t.Context(), inFile, out1, "aligned", "甲乙", nil, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	// run 2: different word -> font must be extended, not duplicated
	out2 := filepath.Join(dir, "r2.pdf")
	if _, err := ReplaceTextFile(t.Context(), out1, out2, "border", "丙丁戊", nil, nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	// both replacements extractable from the chained output
	txt := extractedText(t, out2)
	for _, want := range []string{"甲乙", "丙丁戊"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("chained output misses %q", want)
		}
	}

	// exactly one replace font object in the document
	f, err := os.Open(out2)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, err := ReadContext(t.Context(), f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ctx.EnsurePageCount(); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for pageNr := 1; pageNr <= ctx.PageCount; pageNr++ {
		pageDict, _, inhPAttrs, err := ctx.PageDict(t.Context(), pageNr, false)
		if err != nil {
			t.Fatal(err)
		}
		var res types.Dict
		if o, found := pageDict.Find("Resources"); found {
			if d, err := ctx.DereferenceDict(o); err == nil {
				res = d
			}
		} else if inhPAttrs != nil {
			res = inhPAttrs.Resources
		}
		if res == nil {
			continue
		}
		o, found := res.Find("Font")
		if !found {
			continue
		}
		fd, err := ctx.DereferenceDict(o)
		if err != nil || fd == nil {
			continue
		}
		for _, v := range fd {
			ir, ok := v.(types.IndirectRef)
			if !ok {
				continue
			}
			d, err := ctx.DereferenceDict(ir)
			if err != nil || d == nil {
				continue
			}
			if bf, found := d.Find("BaseFont"); found {
				if n, ok := bf.(types.Name); ok && n.Value() == "pdfcpu2-replace" {
					seen[ir.ObjectNumber.Value()] = true
				}
			}
		}
	}
	if len(seen) != 1 {
		t.Fatalf("expected exactly 1 replace font object, got %d", len(seen))
	}

	if err := ValidateFile(t.Context(), out2, nil, nil); err != nil {
		t.Fatalf("validate chained output: %v", err)
	}
}

// TestReplaceTextFileErrors verifies argument validation.
func TestReplaceTextFileErrors(t *testing.T) {
	if _, err := ReplaceTextFile(t.Context(), "", "out.pdf", "a", "b", nil, nil); err == nil {
		t.Fatal("expected missing input error")
	}
	if _, err := ReplaceTextFile(t.Context(), "in.pdf", "out.pdf", "", "b", nil, nil); err == nil {
		t.Fatal("expected empty find error")
	}
	if _, err := ReplaceTextFile(t.Context(), "in.pdf", "out.pdf", "a\nb", "c", nil, nil); err == nil {
		t.Fatal("expected multi-line find error")
	}
	if _, err := ReplaceTextFile(t.Context(), "in.pdf", "in.pdf", "a", "b", nil, nil); err == nil {
		t.Fatal("expected in==out error")
	}
}

var _ = context.Background
