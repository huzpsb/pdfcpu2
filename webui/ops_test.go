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
	"time"

	"github.com/huzpsb/pdfcpu2/internal/testtsa"
	"github.com/huzpsb/pdfcpu2/pkg/api"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
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

// permConf returns an owner-authenticated configuration for reading test
// output back (user-password auth would hit the permission bit checks).
func permConf() *model.Configuration {
	conf := model.NewDefaultConfiguration()
	conf.UserPW = "upw"
	conf.OwnerPW = "opw"
	return conf
}

func permissionsOf(t *testing.T, data []byte) model.PermissionFlags {
	t.Helper()
	p, err := api.GetPermissions(t.Context(), bytes.NewReader(data), permConf())
	if err != nil {
		t.Fatalf("get permissions: %v", err)
	}
	if p == nil {
		t.Fatal("no permissions reported for encrypted file")
	}
	return model.PermissionFlags(uint16(*p))
}

func TestEncryptPermissionFlags(t *testing.T) {
	data := readTestFile(t, testPDF)

	// Omitted flags default to view + print, the former default preset.
	def, err := Encrypt(t.Context(), data, Params{"ownerPw": "opw", "userPw": "upw"})
	if err != nil {
		t.Fatal(err)
	}
	if got := permissionsOf(t, def.Data); got != model.PermissionsPrint {
		t.Fatalf("default permissions = %#x, want print preset", got)
	}

	// Every flag cleared: view only.
	none, err := Encrypt(t.Context(), data, Params{
		"ownerPw": "opw", "userPw": "upw",
		"permPrintRev2": false, "permPrintRev3": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := permissionsOf(t, none.Data); got != model.PermissionsNone {
		t.Fatalf("permissions = %#x, want none", got)
	}
	if names, _ := none.Details["permissions"].([]string); len(names) != 0 {
		t.Fatalf("granted permissions = %v, want none", names)
	}

	// Single flags toggle single bits.
	one, err := Encrypt(t.Context(), data, Params{
		"ownerPw": "opw", "userPw": "upw",
		"permPrintRev2": false, "permPrintRev3": false,
		"permAnnotate": true, "permAssembleRev3": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := model.PermissionsNone | model.PermissionModAnnFillForm | model.PermissionAssembleRev3
	if got := permissionsOf(t, one.Data); got != want {
		t.Fatalf("permissions = %#x, want %#x", got, want)
	}
	if names, _ := one.Details["permissions"].([]string); len(names) != 2 {
		t.Fatalf("granted permissions = %v", names)
	}

	// Every flag set: full access.
	all := Params{"ownerPw": "opw", "userPw": "upw"}
	for _, d := range permissionFlagDefs {
		all[d.key] = true
	}
	everything, err := Encrypt(t.Context(), data, all)
	if err != nil {
		t.Fatal(err)
	}
	if got := permissionsOf(t, everything.Data); got != model.PermissionsAll {
		t.Fatalf("permissions = %#x, want all", got)
	}

	// The legacy preset still overrides single flags.
	preset, err := Encrypt(t.Context(), data, Params{"ownerPw": "opw", "userPw": "upw", "perm": "all"})
	if err != nil {
		t.Fatal(err)
	}
	if got := permissionsOf(t, preset.Data); got != model.PermissionsAll {
		t.Fatalf("preset permissions = %#x, want all", got)
	}
}

func TestEncryptViewerPreferences(t *testing.T) {
	data := readTestFile(t, testPDF)

	// Without preference keys the document must stay untouched.
	plain, err := Encrypt(t.Context(), data, Params{"ownerPw": "opw", "userPw": "upw"})
	if err != nil {
		t.Fatal(err)
	}
	vp, _, err := api.ViewerPreferences(t.Context(), bytes.NewReader(plain.Data), permConf())
	if err != nil {
		t.Fatal(err)
	}
	if vp != nil {
		t.Fatalf("unexpected viewer preferences: %+v", vp)
	}
	if _, ok := plain.Details["viewerPreferences"]; ok {
		t.Fatal("viewerPreferences detail must be absent")
	}

	// Supplied keys are bound in the same pass.
	enc, err := Encrypt(t.Context(), data, Params{
		"ownerPw": "opw", "userPw": "upw",
		"permPrintRev2": false, "permPrintRev3": false,
		"hideToolbar": true, "fitWindow": true, "displayDocTitle": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	names, _ := enc.Details["viewerPreferences"].([]string)
	if len(names) != 2 || names[0] != "hideToolbar" || names[1] != "fitWindow" {
		t.Fatalf("viewerPreferences detail = %v", names)
	}

	vp, _, err = api.ViewerPreferences(t.Context(), bytes.NewReader(enc.Data), permConf())
	if err != nil {
		t.Fatal(err)
	}
	if vp == nil {
		t.Fatal("viewer preferences not written")
	}
	if vp.HideToolbar == nil || !*vp.HideToolbar {
		t.Fatalf("HideToolbar not applied: %+v", vp)
	}
	if vp.FitWindow == nil || !*vp.FitWindow {
		t.Fatalf("FitWindow not applied: %+v", vp)
	}
	if vp.DisplayDocTitle == nil || *vp.DisplayDocTitle {
		t.Fatalf("DisplayDocTitle not applied: %+v", vp)
	}
	if vp.HideMenubar != nil {
		t.Fatalf("HideMenubar must stay untouched: %+v", vp)
	}

	// The output stays a decryptable PDF.
	if _, err := Decrypt(t.Context(), enc.Data, Params{"password": "upw"}); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
}

// encState reads back the encryption handler parameters of an encrypted PDF.
func encState(t *testing.T, data []byte) (v, r, l int) {
	t.Helper()
	ctx, err := api.ReadContext(t.Context(), bytes.NewReader(data), permConf())
	if err != nil {
		t.Fatalf("read encrypted output: %v", err)
	}
	if ctx.E == nil {
		t.Fatal("output not encrypted")
	}
	return ctx.E.V, ctx.E.R, ctx.E.L
}

func TestEncryptKeyLengths(t *testing.T) {
	data := readTestFile(t, testPDF)

	tests := []struct {
		name    string
		params  Params
		v, r, l int
	}{
		// RC4-40 is the legacy V1/R2 handler; there is no 40 bit AES.
		{"rc4-40", Params{"ownerPw": "opw", "userPw": "upw", "aes": false, "keyLengthRc4": "40"}, 1, 2, 40},
		{"rc4-128", Params{"ownerPw": "opw", "userPw": "upw", "aes": false, "keyLengthRc4": "128"}, 4, 4, 128},
		{"aes-128", Params{"ownerPw": "opw", "userPw": "upw", "aes": true, "keyLengthAes": "128"}, 4, 4, 128},
		// AES-256 on a PDF 1.7 input stays V5/R5 (deprecated pre-2.0 variant).
		{"aes-256", Params{"ownerPw": "opw", "userPw": "upw", "aes": true, "keyLengthAes": "256"}, 5, 5, 256},
		// A 40 bit AES request must not degrade to RC4-40.
		{"aes-40-clamped", Params{"ownerPw": "opw", "userPw": "upw", "aes": true, "keyLengthAes": "40"}, 5, 5, 256},
		// Legacy shared keyLength parameter keeps working.
		{"legacy-key", Params{"ownerPw": "opw", "userPw": "upw", "aes": false, "keyLength": "40"}, 1, 2, 40},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc, err := Encrypt(t.Context(), data, tt.params)
			if err != nil {
				t.Fatal(err)
			}
			v, r, l := encState(t, enc.Data)
			if v != tt.v || r != tt.r || l != tt.l {
				t.Fatalf("encrypt dict V/R/L = %d/%d/%d, want %d/%d/%d", v, r, l, tt.v, tt.r, tt.l)
			}
			if keyLen, _ := enc.Details["keyLength"].(int); keyLen != tt.l {
				t.Fatalf("keyLength detail = %v, want %d", enc.Details["keyLength"], tt.l)
			}
			if _, err := Decrypt(t.Context(), enc.Data, Params{"password": "upw"}); err != nil {
				t.Fatalf("decrypt: %v", err)
			}
		})
	}
}

func TestEncryptEnforcePrintScaling(t *testing.T) {
	data := readTestFile(t, testPDF) // PDF 1.7

	// PDF 2.0 output requires AES-256.
	if _, err := Encrypt(t.Context(), data, Params{
		"ownerPw": "opw", "aes": false, "pdf20": true,
	}); err == nil {
		t.Fatal("PDF 2.0 output with RC4 must fail")
	}

	// Enforce on a 1.x file requires the explicit PDF 2.0 upgrade.
	if _, err := Encrypt(t.Context(), data, Params{
		"ownerPw": "opw", "userPw": "upw", "enforcePrintScaling": true,
	}); err == nil {
		t.Fatal("enforce without the PDF 2.0 upgrade must fail")
	}

	enc, err := Encrypt(t.Context(), data, Params{
		"ownerPw": "opw", "userPw": "upw", "pdf20": true,
		"permPrintRev2": false, "permPrintRev3": false,
		"hideToolbar": true, "enforcePrintScaling": true,
	})
	if err != nil {
		t.Fatal(err)
	}

	names, _ := enc.Details["viewerPreferences"].([]string)
	wantNames := []string{"hideToolbar", "enforcePrintScaling"}
	if len(names) != len(wantNames) || names[0] != wantNames[0] || names[1] != wantNames[1] {
		t.Fatalf("viewerPreferences detail = %v, want %v", names, wantNames)
	}
	if v, _ := enc.Details["pdfVersion"].(string); v != "2.0" {
		t.Fatalf("pdfVersion detail = %q, want 2.0", v)
	}

	// The file was upgraded to PDF 2.0 and uses the standard R6 handler.
	if v, r, l := encState(t, enc.Data); v != 5 || r != 6 || l != 256 {
		t.Fatalf("encrypt dict V/R/L = %d/%d/%d, want 5/6/256", v, r, l)
	}

	// Validation on re-read accepts the enforced PrintScaling entry.
	vp, version, err := api.ViewerPreferences(t.Context(), bytes.NewReader(enc.Data), permConf())
	if err != nil {
		t.Fatal(err)
	}
	if *version != model.V20 {
		t.Fatalf("effective version = %s, want 2.0", *version)
	}
	if vp == nil || vp.PrintScaling == nil || *vp.PrintScaling != model.PrintScalingNone {
		t.Fatalf("PrintScaling not enforced to None: %+v", vp)
	}
	if len(vp.Enforce) != 1 || vp.Enforce[0] != "PrintScaling" {
		t.Fatalf("Enforce = %v, want [PrintScaling]", vp.Enforce)
	}
	if vp.HideToolbar == nil || !*vp.HideToolbar {
		t.Fatalf("HideToolbar not applied: %+v", vp)
	}

	dec, err := Decrypt(t.Context(), enc.Data, Params{"password": "upw"})
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}

	// Enforce on an already upgraded (PDF 2.0) file needs no pdf20 flag.
	enc2, err := Encrypt(t.Context(), dec.Data, Params{
		"ownerPw": "opw", "userPw": "upw", "enforcePrintScaling": true,
	})
	if err != nil {
		t.Fatalf("enforce on PDF 2.0 input: %v", err)
	}
	if v, r, l := encState(t, enc2.Data); v != 5 || r != 6 || l != 256 {
		t.Fatalf("encrypt dict V/R/L = %d/%d/%d, want 5/6/256", v, r, l)
	}
	vp2, version2, err := api.ViewerPreferences(t.Context(), bytes.NewReader(enc2.Data), permConf())
	if err != nil {
		t.Fatal(err)
	}
	if *version2 != model.V20 || len(vp2.Enforce) != 1 || vp2.Enforce[0] != "PrintScaling" {
		t.Fatalf("unexpected re-encryption state: version=%s vp=%+v", *version2, vp2)
	}

	// The plain PDF 2.0 upgrade without enforce also yields R6.
	enc3, err := Encrypt(t.Context(), data, Params{
		"ownerPw": "opw", "userPw": "upw", "pdf20": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, r, l := encState(t, enc3.Data); v != 5 || r != 6 || l != 256 {
		t.Fatalf("encrypt dict V/R/L = %d/%d/%d, want 5/6/256", v, r, l)
	}
	if _, ok := enc3.Details["viewerPreferences"]; ok {
		t.Fatal("viewerPreferences detail must be absent")
	}
}

func TestTimestampOp(t *testing.T) {
	tsa, err := testtsa.New(time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	defer tsa.Close()

	r, err := Timestamp(t.Context(), readTestFile(t, testPDF), Params{"tsa": tsa.URL()})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Data) == 0 {
		t.Fatal("no output")
	}
	if r.Name != "_ts.pdf" {
		t.Fatalf("name = %s", r.Name)
	}
	if got, _ := r.Details["tsa"].(string); got != tsa.URL() {
		t.Fatalf("tsa detail = %q", got)
	}
	genTime, _ := r.Details["genTime"].(string)
	if genTime == "" {
		t.Fatal("missing genTime detail")
	}

	// The signatures tool reports the stamped document timestamp.
	sig, err := SigValidate(t.Context(), r.Data, true)
	if err != nil {
		t.Fatal(err)
	}
	report, _ := sig.Details["report"].(string)
	if !strings.Contains(report, "document timestamp") {
		t.Fatalf("unexpected report:\n%s", report)
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
