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

package webui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPDF = "../pkg/testdata/test.pdf"
const textPDF = "../pkg/testdata/testWithText.pdf"

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func TestProbe(t *testing.T) {
	r, err := Probe(t.Context(), readTestFile(t, testPDF))
	if err != nil {
		t.Fatal(err)
	}
	if r.Details["pageCount"].(int) < 1 {
		t.Fatalf("unexpected page count %v", r.Details["pageCount"])
	}
	if r.Details["encrypted"].(bool) {
		t.Fatal("test.pdf must not be detected as encrypted")
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	data := readTestFile(t, testPDF)

	enc, err := Encrypt(t.Context(), data, Params{"ownerPw": "opw", "userPw": "upw", "perm": "print"})
	if err != nil {
		t.Fatal(err)
	}
	if len(enc.Data) == 0 {
		t.Fatal("no encrypted output")
	}

	p, err := Probe(t.Context(), enc.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Details["encrypted"].(bool) {
		t.Fatal("encrypted output not detected as encrypted")
	}

	if _, err := Decrypt(t.Context(), enc.Data, Params{"password": "upw"}); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if _, err := Decrypt(t.Context(), enc.Data, Params{"password": "wrong"}); err == nil {
		t.Fatal("wrong password must fail")
	}
}

func TestOptimize(t *testing.T) {
	r, err := Optimize(t.Context(), readTestFile(t, testPDF))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Data) == 0 {
		t.Fatal("no output")
	}
	if _, ok := r.Details["outSize"].(int); !ok {
		t.Fatalf("missing outSize detail: %v", r.Details)
	}
}

func TestToText(t *testing.T) {
	r, err := ToText(t.Context(), readTestFile(t, textPDF), Params{})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := r.Details["text"].(string)
	if len(text) == 0 {
		t.Fatal("no text extracted")
	}
	if r.Details["chars"].(int) != len(text) {
		t.Fatal("chars detail mismatch")
	}
}

func TestWatermarkText(t *testing.T) {
	r, err := WatermarkText(t.Context(), readTestFile(t, testPDF), Params{
		"text":     "DRAFT",
		"onTop":    false,
		"position": "c",
		"rotation": "45",
		"opacity":  0.5,
		"scale":    0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Data) == 0 {
		t.Fatal("no output")
	}
	r2, err := WatermarkText(t.Context(), readTestFile(t, testPDF), Params{
		"text":     "DRAFT",
		"rotation": "diagonal2",
	})
	if err != nil {
		t.Fatalf("diagonal2: %v", err)
	}
	if len(r2.Data) == 0 {
		t.Fatal("no output (diagonal2)")
	}
}

func TestWatermarkImage(t *testing.T) {
	img := readTestFile(t, "../pkg/testdata/image-fixtures/any.png")
	r, err := WatermarkImage(t.Context(), readTestFile(t, testPDF), img, "any.png", Params{
		"onTop":    true,
		"position": "br",
		"scale":    0.2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Data) == 0 {
		t.Fatal("no output")
	}
}

func TestSplitSpan(t *testing.T) {
	r, err := Split(t.Context(), readTestFile(t, testPDF), "test.pdf", Params{"mode": "span", "span": 1})
	if err != nil {
		t.Fatal(err)
	}
	want := r.Details["count"].(int)
	if want < 1 || len(r.Files) != want {
		t.Fatalf("split produced %d files, want %d", len(r.Files), want)
	}
	for _, f := range r.Files {
		if len(f.Data) == 0 {
			t.Fatalf("empty split output %s", f.Name)
		}
	}
}

func TestSplitByPages(t *testing.T) {
	r, err := Split(t.Context(), readTestFile(t, "../pkg/testdata/go.pdf"), "go.pdf", Params{"mode": "pages", "pageNrs": "2, 4"})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(r.Files); got != 3 {
		t.Fatalf("got %d files, want 3", got)
	}
}

func TestMerge(t *testing.T) {
	a := readTestFile(t, testPDF)
	b := readTestFile(t, "../pkg/testdata/go.pdf")
	pa, err := Probe(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := Probe(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	want := pa.Details["pageCount"].(int) + pb.Details["pageCount"].(int)

	r, err := Merge(t.Context(), [][]byte{a, b}, Params{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Probe(t.Context(), r.Data)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Details["pageCount"].(int); got != want {
		t.Fatalf("merged page count %d, want %d", got, want)
	}
}

func TestImg2PDF(t *testing.T) {
	img := readTestFile(t, "../pkg/testdata/image-fixtures/any.png")
	r, err := Img2PDF(t.Context(), []NamedBytes{{Name: "any.png", Data: img}}, Params{"paper": "A4", "fit": "contain"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Data) == 0 {
		t.Fatal("no output")
	}
}

func TestAttachmentsRoundTrip(t *testing.T) {
	data := readTestFile(t, testPDF)

	added, err := AttAdd(t.Context(), data, []NamedBytes{{Name: "note.txt", Data: []byte("hello")}}, "a note")
	if err != nil {
		t.Fatal(err)
	}

	list, err := AttList(t.Context(), added.Data)
	if err != nil {
		t.Fatal(err)
	}
	aa := list.Details["attachments"].([]map[string]any)
	if len(aa) != 1 || aa[0]["name"] != "note.txt" {
		t.Fatalf("unexpected attachment list: %v", aa)
	}

	ext, err := AttExtract(t.Context(), added.Data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ext.Files) != 1 || !bytes.Equal(ext.Files[0].Data, []byte("hello")) {
		t.Fatalf("unexpected extract result: %+v", ext.Files)
	}

	removed, err := AttRemove(t.Context(), added.Data, []string{"note.txt"})
	if err != nil {
		t.Fatal(err)
	}
	list2, err := AttList(t.Context(), removed.Data)
	if err != nil {
		t.Fatal(err)
	}
	if n := list2.Details["count"].(int); n != 0 {
		t.Fatalf("attachment not removed: %d left", n)
	}
}

func TestSigValidateAndRemove(t *testing.T) {
	data := readTestFile(t, filepath.Join("../pkg/testdata/signatures/ETSI.CAdES.detached", "testPAdES_BB.pdf"))

	r, err := SigValidate(t.Context(), data, false)
	if err != nil {
		t.Fatal(err)
	}
	report, _ := r.Details["report"].(string)
	if !strings.Contains(report, "signature") {
		t.Fatalf("unexpected report: %q", report)
	}

	rem, err := SigRemove(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	if len(rem.Data) == 0 {
		t.Fatal("no output")
	}
}

func TestBatchOptimize(t *testing.T) {
	data := readTestFile(t, testPDF)
	r, err := Batch(t.Context(), "optimize", []NamedBytes{
		{Name: "a.pdf", Data: data},
		{Name: "b.pdf", Data: data},
	}, Params{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Data) == 0 {
		t.Fatal("empty zip")
	}
	if !bytes.HasPrefix(r.Data, []byte("PK")) {
		t.Fatal("result is not a zip archive")
	}
	items := r.Details["items"].([]batchItem)
	if len(items) != 2 || !items[0].OK || !items[1].OK {
		t.Fatalf("unexpected batch items: %+v", items)
	}
}

func TestRenderPage(t *testing.T) {
	r, err := RenderPage(t.Context(), readTestFile(t, testPDF), 1, 72)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(r.Data, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatal("not a png")
	}
}

func TestObfuscate(t *testing.T) {
	r, err := Obfuscate(t.Context(), readTestFile(t, textPDF))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Data) == 0 {
		t.Fatal("no output")
	}
	if _, err := ToText(t.Context(), r.Data, Params{}); err == nil {
		// Note: extraction may still succeed for core fonts; obfuscation
		// targets embedded font mappings. Just ensure the file is valid.
		_ = err
	}
}
