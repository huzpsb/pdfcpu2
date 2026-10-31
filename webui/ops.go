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

// Package webui provides the in browser engine bindings for the pdfcpu2
// web UI. All operations are pure functions on byte slices so that they
// compile unchanged for native tests (go test) and for GOOS=js GOARCH=wasm.
package webui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/huzpsb/pdfcpu2/pkg/api"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
	"github.com/huzpsb/pdfcpu2/pkg/render"
)

// NamedBytes is one output file.
type NamedBytes struct {
	Name string
	Data []byte
}

// Result carries the outcome of one operation.
type Result struct {
	// Name is the suggested file name of Data.
	Name string
	// Data is the primary output (empty for report only operations).
	Data []byte
	// Files are additional or multiple outputs (split, extract, batch).
	Files []NamedBytes
	// Details carry op specific information for the UI.
	Details map[string]any
}

func newDetails(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			m[k] = kv[i+1]
		}
	}
	return m
}

// Params is the generic parameter bag of every operation.
type Params map[string]any

func (p Params) Str(k, def string) string {
	if v, ok := p[k]; ok {
		switch t := v.(type) {
		case string:
			return strings.TrimSpace(t)
		case float64:
			return strconv.FormatFloat(t, 'g', -1, 64)
		case bool:
			return strconv.FormatBool(t)
		}
	}
	return def
}

func (p Params) Bool(k string, def bool) bool {
	if v, ok := p[k]; ok {
		switch t := v.(type) {
		case bool:
			return t
		case float64:
			return t != 0
		case string:
			if b, err := strconv.ParseBool(t); err == nil {
				return b
			}
		}
	}
	return def
}

// Flag reports the boolean value of k and whether k was supplied at all.
// Unlike Bool it distinguishes "absent" from "false", which lets callers
// leave document entries untouched that the user never configured.
func (p Params) Flag(k string) (value, ok bool) {
	if v, exists := p[k]; exists {
		switch t := v.(type) {
		case bool:
			return t, true
		case float64:
			return t != 0, true
		case string:
			if b, err := strconv.ParseBool(strings.TrimSpace(t)); err == nil {
				return b, true
			}
		}
	}
	return false, false
}

func (p Params) Float(k string, def float64) float64 {
	if v, ok := p[k]; ok {
		switch t := v.(type) {
		case float64:
			return t
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return f
			}
		}
	}
	return def
}

func (p Params) Int(k string, def int) int {
	return int(p.Float(k, float64(def)))
}

// Pages returns the page selection parameter, nil meaning all pages.
func (p Params) Pages() []string {
	s := p.Str("pages", "")
	if s == "" {
		return nil
	}
	return []string{s}
}

// Strings returns a string list parameter.
func (p Params) Strings(k string) []string {
	v, ok := p[k]
	if !ok {
		return nil
	}
	var raw []string
	switch t := v.(type) {
	case []string:
		raw = t
	case []any:
		for _, it := range t {
			if s, ok := it.(string); ok {
				raw = append(raw, s)
			}
		}
	case string:
		raw = strings.Split(t, ",")
	default:
		return nil
	}
	ss := make([]string, 0, len(raw))
	for _, s := range raw {
		if s = strings.TrimSpace(s); s != "" {
			ss = append(ss, s)
		}
	}
	return ss
}

// Ints returns an int list parameter, ignoring non numeric items.
func (p Params) Ints(k string) []int {
	ss := p.Strings(k)
	n := make([]int, 0, len(ss))
	for _, s := range ss {
		if i, err := strconv.Atoi(s); err == nil {
			n = append(n, i)
		}
	}
	return n
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func baseName(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

func defaultConf() *model.Configuration {
	return model.NewDefaultConfiguration()
}

// readContext parses and optimizes data into a pdfcpu context.
func readContext(c context.Context, data []byte, conf *model.Configuration) (*model.Context, error) {
	return api.ReadValidateAndOptimize(c, bytes.NewReader(data), conf, nil)
}

// sortedPageNrs turns a page selection into ascending page numbers.
func sortedPageNrs(c context.Context, ctx *model.Context, selectedPages []string) ([]int, error) {
	pages, err := api.PagesForSelection(ctx.PageCount, selectedPages, true)
	if err != nil {
		return nil, fmt.Errorf("parse page selection: %w", err)
	}
	nrs := make([]int, 0, len(pages))
	for k := range pages {
		nrs = append(nrs, k)
	}
	sort.Ints(nrs)
	return nrs, nil
}

// ---- probe ----

// headerVersion extracts the PDF version from the file header, e.g. "1.7".
// The header is plain text even in encrypted files; the effective version of
// a rare catalog-/Version override may differ, the encrypt operation treats
// those files as already upgraded.
func headerVersion(data []byte) string {
	n := len(data)
	if n > 1024 {
		n = 1024
	}
	i := bytes.Index(data[:n], []byte("%PDF-"))
	if i < 0 || len(data) < i+8 {
		return ""
	}
	s := string(data[i+5 : i+8])
	if s[1] != '.' || s[0] < '0' || s[0] > '9' || s[2] < '0' || s[2] > '9' {
		return ""
	}
	return s
}

// Probe inspects a PDF: page count and encryption state.
func Probe(c context.Context, data []byte) (*Result, error) {
	conf := defaultConf()
	n, err := api.PageCount(c, bytes.NewReader(data), conf)
	encrypted := false
	if err != nil {
		encrypted = errors.Is(err, pdfcpu.ErrWrongPassword)
		if !encrypted {
			return nil, err
		}
	}
	return &Result{Details: newDetails(
		"pageCount", n,
		"encrypted", encrypted,
		"pdfVersion", headerVersion(data),
	)}, nil
}

// RenderPage rasterizes one page into a PNG.
func RenderPage(c context.Context, data []byte, page int, dpi float64) (*Result, error) {
	dpi = clamp(dpi, 36, 300)
	conf := defaultConf()
	var buf bytes.Buffer
	rendered := false
	err := api.RenderPages(c, bytes.NewReader(data), []string{strconv.Itoa(page)}, &render.Options{DPI: dpi, Format: "png"}, func(res *render.Result) error {
		rendered = true
		return render.Encode(&buf, res.Image, "png", 0)
	}, conf)
	if err != nil {
		return nil, err
	}
	if !rendered {
		return nil, fmt.Errorf("page %d: no render result", page)
	}
	return &Result{Name: fmt.Sprintf("page_%d.png", page), Data: buf.Bytes(), Details: newDetails("dpi", dpi, "page", page)}, nil
}

// ---- P0: security ----

// permissionFlagDef maps one toggleable parameter to one user access
// permission bit, see PDF 32000 table 22.
type permissionFlagDef struct {
	key  string // parameter name
	bit  model.PermissionFlags
	name string // canonical name reported in the result details
}

// permissionFlagDefs are the individually toggleable permission flags.
// Bits 3-6 are interpreted by revision 2 handlers (RC4), bits 9-12 by
// revision 3 and later handlers (AES, PDF 2.0); a handler simply ignores
// the bits it does not implement.
var permissionFlagDefs = []permissionFlagDef{
	{"permPrintRev2", model.PermissionPrintRev2, "print"},
	{"permModify", model.PermissionModify, "modify"},
	{"permExtract", model.PermissionExtract, "extract"},
	{"permAnnotate", model.PermissionModAnnFillForm, "annotate"},
	{"permFillForms", model.PermissionFillRev3, "fillForms"},
	{"permExtractRev3", model.PermissionExtractRev3, "extractRev3"},
	{"permAssembleRev3", model.PermissionAssembleRev3, "assemble"},
	{"permPrintRev3", model.PermissionPrintRev3, "printHi"},
}

// permissionFlags computes the user access permission bit set.
// The legacy preset parameter ("perm": none|print|all) takes precedence
// when present; otherwise the flags start from the view-only default set
// with both print bits enabled (the former default preset).
func permissionFlags(p Params) model.PermissionFlags {
	switch preset := p.Str("perm", ""); preset {
	case "all":
		return model.PermissionsAll
	case "none":
		return model.PermissionsNone
	case "print":
		return model.PermissionsPrint
	}
	f := model.PermissionsNone
	for _, d := range permissionFlagDefs {
		if p.Bool(d.key, d.bit == model.PermissionPrintRev2 || d.bit == model.PermissionPrintRev3) {
			f |= d.bit
		}
	}
	return f
}

// grantedPermissions lists the canonical names of all set permission flags.
func grantedPermissions(f model.PermissionFlags) []string {
	ss := []string{}
	for _, d := range permissionFlagDefs {
		if f&d.bit != 0 {
			ss = append(ss, d.name)
		}
	}
	return ss
}

// viewerPrefDef maps one toggleable parameter to one viewer preference,
// see PDF 32000 table 147.
type viewerPrefDef struct {
	key string
	set func(*model.ViewerPreferences, bool)
}

// viewerPrefDefs are the viewer preferences that encryption can apply
// in the same pass, e.g. hideToolbar forces conforming viewers to hide
// their tool bars while the document is open.
var viewerPrefDefs = []viewerPrefDef{
	{"hideToolbar", (*model.ViewerPreferences).SetHideToolBar},
	{"hideMenubar", (*model.ViewerPreferences).SetHideMenuBar},
	{"hideWindowUI", (*model.ViewerPreferences).SetHideWindowUI},
	{"fitWindow", (*model.ViewerPreferences).SetFitWindow},
	{"centerWindow", (*model.ViewerPreferences).SetCenterWindow},
	{"displayDocTitle", (*model.ViewerPreferences).SetDisplayDocTitle},
}

// viewerPreferences collects the viewer preferences to apply and the
// canonical names of those set to true. Only supplied keys are set;
// absent keys leave the document untouched.
func viewerPreferences(p Params) (*model.ViewerPreferences, []string) {
	var vp *model.ViewerPreferences
	names := []string{}
	for _, d := range viewerPrefDefs {
		v, ok := p.Flag(d.key)
		if !ok {
			continue
		}
		if vp == nil {
			vp = &model.ViewerPreferences{}
		}
		d.set(vp, v)
		if v {
			names = append(names, d.key)
		}
	}
	return vp, names
}

// Encrypt encrypts data and returns the protected PDF.
// User access permissions are configured per flag (see permissionFlagDefs)
// or through the legacy "perm" preset (none|print|all). Viewer preferences
// (hideToolbar, hideMenubar, hideWindowUI, fitWindow, centerWindow,
// displayDocTitle) are bound in the same pass for every supplied key.
// pdf20 explicitly upgrades the output to PDF 2.0 (AES-256 only, turns the
// deprecated R5 handler into the standard R6); enforcePrintScaling locks
// PrintScaling=None via the PDF 2.0 Enforce entry and therefore requires
// pdf20 unless the input already is PDF 2.0.
func Encrypt(c context.Context, data []byte, p Params) (*Result, error) {
	ownerPW := p.Str("ownerPw", "")
	if ownerPW == "" {
		return nil, errors.New("owner password required")
	}

	conf := defaultConf()
	conf.Cmd = model.ENCRYPT
	conf.UserPW = p.Str("userPw", "")
	conf.OwnerPW = ownerPW
	conf.EncryptUsingAES = p.Bool("aes", true)
	// AES keys are 128 or 256 bits - a 40 bit request has no AES variant and
	// the engine would silently emit legacy RC4-40 (V1/R2). RC4 keys are 40
	// or 128 bits. The algorithm specific key parameters win over the legacy
	// shared "keyLength".
	keyLen := 0
	if conf.EncryptUsingAES {
		keyLen = p.Int("keyLengthAes", 0)
		if keyLen == 0 {
			keyLen = p.Int("keyLength", 256)
		}
		if keyLen != 128 && keyLen != 256 {
			keyLen = 256
		}
	} else {
		keyLen = p.Int("keyLengthRc4", 0)
		if keyLen == 0 {
			keyLen = p.Int("keyLength", 128)
		}
		if keyLen != 40 && keyLen != 128 {
			keyLen = 128
		}
	}
	conf.EncryptKeyLength = keyLen
	conf.Permissions = permissionFlags(p)

	pdf20 := p.Bool("pdf20", false)
	enforcePrintScaling := p.Bool("enforcePrintScaling", false)
	if (pdf20 || enforcePrintScaling) && (!conf.EncryptUsingAES || keyLen != 256) {
		return nil, errors.New("PDF 2.0 output requires AES-256")
	}

	// One pass: read the context, bind requested viewer preferences and
	// write everything back encrypted (api.WriteContext runs the encryption
	// setup for model.ENCRYPT).
	ctx, err := readContext(c, data, conf)
	if err != nil {
		return nil, err
	}
	if enforcePrintScaling && !pdf20 && ctx.XRefTable.Version() < model.V20 {
		return nil, errors.New("enforce print scaling requires PDF 2.0 - enable PDF 2.0 output for this file")
	}
	if pdf20 && ctx.XRefTable.Version() < model.V20 {
		v20 := model.V20
		ctx.XRefTable.HeaderVersion = &v20
	}

	prefNames := []string{}
	vp, names := viewerPreferences(p)
	if enforcePrintScaling {
		// The engine supports Enforce for PrintScaling only.
		if vp == nil {
			vp = &model.ViewerPreferences{}
		}
		vp.PrintScaling = model.PrintScalingFor("None")
		vp.Enforce = []string{"PrintScaling"}
		names = append(names, "enforcePrintScaling")
	}
	if vp != nil {
		if ctx.ViewerPref == nil {
			ctx.ViewerPref = &model.ViewerPreferences{}
		}
		ctx.ViewerPref.Populate(vp)
		ctx.XRefTable.BindViewerPreferences()
		prefNames = names
	}

	var buf bytes.Buffer
	if err := api.WriteContext(c, ctx, &buf); err != nil {
		return nil, err
	}
	algo := "RC4"
	if conf.EncryptUsingAES {
		algo = "AES"
	}
	details := newDetails(
		"algorithm", algo,
		"keyLength", keyLen,
		"pdfVersion", ctx.XRefTable.Version().String(),
		"permissions", grantedPermissions(conf.Permissions),
	)
	if len(prefNames) > 0 {
		details["viewerPreferences"] = prefNames
	}
	return &Result{Name: "_enc.pdf", Data: buf.Bytes(), Details: details}, nil
}

// Decrypt removes encryption from data. An empty user password is valid
// (owner-password-only protection opens freely), so no password is required
// upfront — decryption itself reports wrong passwords.
func Decrypt(c context.Context, data []byte, p Params) (*Result, error) {
	pw := p.Str("password", "")
	conf := defaultConf()
	conf.UserPW = p.Str("userPw", pw)
	conf.OwnerPW = p.Str("ownerPw", pw)

	var buf bytes.Buffer
	if err := api.Decrypt(c, bytes.NewReader(data), &buf, conf); err != nil {
		return nil, err
	}
	return &Result{Name: "_dec.pdf", Data: buf.Bytes()}, nil
}

// Timestamp stamps data with an RFC 3161 document timestamp. It proves that
// the document bytes existed at the TSA asserted time without identifying a
// signer; only the document digest leaves the device. An empty tsa selects
// the engine default (DigiCert); field names the signature field.
func Timestamp(c context.Context, data []byte, p Params) (*Result, error) {
	conf := defaultConf()
	var buf bytes.Buffer
	info, err := api.Timestamp(c, bytes.NewReader(data), &buf, p.Str("tsa", ""), p.Str("field", ""), conf)
	if err != nil {
		return nil, err
	}
	return &Result{
		Name: "_ts.pdf",
		Data: buf.Bytes(),
		Details: newDetails(
			"tsa", info.URL,
			"genTime", info.GenTime.Format(time.RFC3339),
			"tsaCert", info.CertSubject,
		),
	}, nil
}

// ---- P0: content ----

// OCR recognizes text in scanned images and adds a hidden text layer.
func OCR(c context.Context, data []byte, p Params) (*Result, error) {
	conf := defaultConf()
	ctx, err := readContext(c, data, conf)
	if err != nil {
		return nil, err
	}
	pageNrs, err := sortedPageNrs(c, ctx, p.Pages())
	if err != nil {
		return nil, err
	}
	n, err := pdfcpu.OCRPages(c, ctx, pageNrs)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return &Result{
			Details: newDetails("imagesProcessed", 0, "message", "ocrNone"),
		}, nil
	}
	var buf bytes.Buffer
	if err := api.WriteContext(c, ctx, &buf); err != nil {
		return nil, err
	}
	return &Result{
		Name:    "_ocr.pdf",
		Data:    buf.Bytes(),
		Details: newDetails("imagesProcessed", n),
	}, nil
}

// Solidify replaces page content with its rasterization.
func Solidify(c context.Context, data []byte, p Params) (*Result, error) {
	format := "png"
	if p.Str("format", "png") == "jpg" {
		format = "jpg"
	}
	o := &render.Options{
		DPI:         clamp(p.Float("dpi", 150), 36, 600),
		Format:      format,
		JPEGQuality: int(clamp(p.Float("quality", 85), 1, 100)),
	}
	var buf bytes.Buffer
	if err := api.Solidify(c, bytes.NewReader(data), &buf, p.Pages(), o, defaultConf()); err != nil {
		return nil, err
	}
	return &Result{
		Name:    "_solid.pdf",
		Data:    buf.Bytes(),
		Details: newDetails("dpi", o.DPI, "format", format),
	}, nil
}

// Replace removes every occurrence of the find string and draws the
// replacement with an embedded vivo Sans subset (see the replace command).
func Replace(c context.Context, data []byte, p Params) (*Result, error) {
	find := p.Str("find", "")
	if find == "" {
		return nil, errors.New("find text required")
	}
	repl := p.Str("replace", "")

	conf := defaultConf()
	ctx, err := readContext(c, data, conf)
	if err != nil {
		return nil, err
	}
	pageNrs, err := sortedPageNrs(c, ctx, p.Pages())
	if err != nil {
		return nil, err
	}
	n, err := pdfcpu.ReplaceTextPages(c, ctx, find, repl, pageNrs)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return &Result{
			Details: newDetails("occurrences", 0, "message", "replaceNone"),
		}, nil
	}
	var buf bytes.Buffer
	if err := api.WriteContext(c, ctx, &buf); err != nil {
		return nil, err
	}
	return &Result{
		Name:    "_replaced.pdf",
		Data:    buf.Bytes(),
		Details: newDetails("occurrences", n),
	}, nil
}

// Obfuscate destroys the text extraction mappings of embedded fonts.
func Obfuscate(c context.Context, data []byte) (*Result, error) {
	var buf bytes.Buffer
	stats, err := api.Obfuscate(c, bytes.NewReader(data), &buf, defaultConf())
	if err != nil {
		return nil, err
	}
	return &Result{
		Name: "_obf.pdf",
		Data: buf.Bytes(),
		Details: newDetails(
			"fontFiles", stats.FontFiles,
			"toUnicode", stats.ToUnicode,
			"skipped", stats.Skipped,
		),
	}, nil
}

// Optimize rewrites data with deduplicated resources.
func Optimize(c context.Context, data []byte) (*Result, error) {
	var buf bytes.Buffer
	if err := api.Optimize(c, bytes.NewReader(data), &buf, defaultConf(), nil); err != nil {
		return nil, err
	}
	saved := 0.0
	if len(data) > 0 {
		saved = (1 - float64(buf.Len())/float64(len(data))) * 100
	}
	return &Result{
		Name: "_opt.pdf",
		Data: buf.Bytes(),
		Details: newDetails(
			"inSize", len(data),
			"outSize", buf.Len(),
			"savedPct", float64(int(saved*10))/10,
		),
	}, nil
}

// ToText extracts plain text.
func ToText(c context.Context, data []byte, p Params) (*Result, error) {
	opts := &model.TextExtractOptions{
		SortByPosition:      p.Bool("sortByPosition", false),
		NormalizeWhitespace: p.Bool("normalizeWhitespace", false),
	}
	texts, err := api.ExtractText(c, bytes.NewReader(data), p.Pages(), opts, defaultConf())
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	for i, t := range texts {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(t.Text(nil))
	}
	text := sb.String()
	return &Result{
		Name: ".txt",
		Data: []byte(text),
		Details: newDetails(
			"text", text,
			"chars", len(text),
			"pages", len(texts),
		),
	}, nil
}

// ---- P0: watermark / stamp ----

var wmPositions = map[string]bool{
	"tl": true, "tc": true, "tr": true, "l": true, "c": true, "r": true,
	"bl": true, "bc": true, "br": true,
}

func buildWatermarkDesc(p Params, textMode bool) string {
	var kv []string
	if pos := p.Str("position", ""); pos != "" && wmPositions[pos] {
		kv = append(kv, "position:"+pos)
	}
	dx, dy := p.Float("offsetX", 0), p.Float("offsetY", 0)
	if dx != 0 || dy != 0 {
		kv = append(kv, fmt.Sprintf("offset:%g %g", dx, dy))
	}
	if rot := p.Str("rotation", ""); rot != "" {
		switch strings.ToLower(rot) {
		case "diagonal1", "d1":
			kv = append(kv, "diagonal:1")
		case "diagonal2", "d2":
			kv = append(kv, "diagonal:2")
		default:
			kv = append(kv, "rotation:"+rot)
		}
	}
	if op := p.Float("opacity", 0); op > 0 {
		kv = append(kv, fmt.Sprintf("opacity:%g", clamp(op, 0, 1)))
	}
	if sc := p.Float("scale", 0); sc > 0 {
		mode := "rel"
		if p.Bool("scaleAbs", false) || sc > 1 {
			mode = "abs"
		}
		kv = append(kv, fmt.Sprintf("scalefactor:%g %s", sc, mode))
	}
	if textMode {
		if fn := p.Str("fontName", ""); fn != "" {
			kv = append(kv, "fontname:"+fn)
		}
		if fs := p.Float("fontSize", 0); fs > 0 {
			kv = append(kv, fmt.Sprintf("points:%g", clamp(fs, 4, 400)))
		}
		if col := p.Str("color", ""); col != "" {
			kv = append(kv, "color:"+col)
		}
		if col := p.Str("bgColor", ""); col != "" {
			kv = append(kv, "bgcolor:"+col)
		}
	}
	return strings.Join(kv, ", ")
}

func applyWatermark(c context.Context, data []byte, wm *model.Watermark, p Params) (*Result, error) {
	var buf bytes.Buffer
	if err := api.AddWatermarks(c, bytes.NewReader(data), &buf, p.Pages(), wm, defaultConf()); err != nil {
		return nil, err
	}
	suffix := "_wm"
	onTop := wm.OnTop
	if onTop {
		suffix = "_stamp"
	}
	return &Result{Name: suffix + ".pdf", Data: buf.Bytes(), Details: newDetails("onTop", onTop)}, nil
}

// WatermarkText stamps a text watermark onto data.
func WatermarkText(c context.Context, data []byte, p Params) (*Result, error) {
	text := p.Str("text", "")
	if text == "" {
		return nil, errors.New("watermark text required")
	}
	onTop := p.Bool("onTop", true)
	wm, err := api.TextWatermark(c, text, buildWatermarkDesc(p, true), onTop, false, types.POINTS, defaultConf())
	if err != nil {
		return nil, err
	}
	return applyWatermark(c, data, wm, p)
}

// WatermarkImage stamps an image watermark (logo) onto data.
func WatermarkImage(c context.Context, data, img []byte, imgName string, p Params) (*Result, error) {
	if len(img) == 0 {
		return nil, errors.New("logo image required")
	}
	onTop := p.Bool("onTop", true)
	wm, err := api.ImageWatermarkForReader(c, bytes.NewReader(img), buildWatermarkDesc(p, false), onTop, false, types.POINTS)
	if err != nil {
		return nil, err
	}
	return applyWatermark(c, data, wm, p)
}

// ---- P1: merge / images ----

// Merge concatenates the given PDFs.
func Merge(c context.Context, datas [][]byte, p Params) (*Result, error) {
	if len(datas) < 2 {
		return nil, errors.New("merge requires at least two files")
	}
	readers := make([]io.ReadSeeker, len(datas))
	for i := range datas {
		readers[i] = bytes.NewReader(datas[i])
	}
	divider := p.Bool("divider", false)

	var buf bytes.Buffer
	if err := api.MergeRaw(c, readers, &buf, divider, defaultConf()); err != nil {
		return nil, err
	}
	data := buf.Bytes()
	if p.Bool("optimize", false) {
		var out bytes.Buffer
		if err := api.Optimize(c, bytes.NewReader(data), &out, defaultConf(), nil); err != nil {
			return nil, err
		}
		data = out.Bytes()
	}
	return &Result{
		Name:    "merged.pdf",
		Data:    data,
		Details: newDetails("divider", divider, "inputs", len(datas)),
	}, nil
}

var paperSizes = map[string]bool{
	"A3": true, "A4": true, "A5": true, "A6": true, "Letter": true,
	"Legal": true, "Ledger": true, "Tabloid": true, "Executive": true,
	"ANSIC": true, "ANSID": true, "ANSIE": true,
}

// Img2PDF builds a PDF from images.
func Img2PDF(c context.Context, imgs []NamedBytes, p Params) (*Result, error) {
	if len(imgs) == 0 {
		return nil, errors.New("no images provided")
	}

	paper := p.Str("paper", "A4")
	if !paperSizes[paper] {
		paper = "A4"
	}
	var kv []string
	kv = append(kv, "formsize:"+paper)
	if p.Str("fit", "stretch") == "contain" {
		sc := clamp(p.Float("scale", 0.9), 0.01, 1)
		kv = append(kv, "position:c", fmt.Sprintf("scalefactor:%g rel", sc))
		if bg := p.Str("bgColor", ""); bg != "" {
			kv = append(kv, "bgcolor:"+bg)
		}
	} else {
		kv = append(kv, "position:full")
	}
	if p.Bool("gray", false) {
		kv = append(kv, "gray:on")
	}
	if p.Bool("sepia", false) {
		kv = append(kv, "sepia:on")
	}

	imp, err := api.Import(strings.Join(kv, ", "), types.POINTS)
	if err != nil {
		return nil, err
	}

	readers := make([]io.Reader, len(imgs))
	names := make([]string, len(imgs))
	for i, img := range imgs {
		readers[i] = bytes.NewReader(img.Data)
		names[i] = img.Name
	}

	var buf bytes.Buffer
	if err := api.ImportImages(c, nil, &buf, readers, imp, defaultConf()); err != nil {
		return nil, err
	}
	return &Result{
		Name:    "images.pdf",
		Data:    buf.Bytes(),
		Details: newDetails("paper", paper, "images", names),
	}, nil
}

// ---- P3: attachments ----

// AttList lists embedded files.
func AttList(c context.Context, data []byte) (*Result, error) {
	aa, err := api.Attachments(c, bytes.NewReader(data), defaultConf())
	if err != nil {
		return nil, err
	}
	list := make([]map[string]any, 0, len(aa))
	for _, a := range aa {
		list = append(list, map[string]any{"name": a.FileName, "desc": a.Desc})
	}
	return &Result{Details: newDetails("attachments", list, "count", len(list))}, nil
}

// AttAdd embeds files into the PDF.
func AttAdd(c context.Context, data []byte, files []NamedBytes, desc string) (*Result, error) {
	if len(files) == 0 {
		return nil, errors.New("no files to attach")
	}

	ctx, err := readContext(c, data, defaultConf())
	if err != nil {
		return nil, err
	}
	now := time.Now()
	names := make([]string, 0, len(files))
	for _, f := range files {
		a := model.Attachment{
			Reader:  bytes.NewReader(f.Data),
			ID:      f.Name,
			Desc:    desc,
			ModTime: &now,
		}
		if err := ctx.AddAttachment(c, a, false); err != nil {
			return nil, fmt.Errorf("embed %s: %w", f.Name, err)
		}
		names = append(names, f.Name)
	}

	var buf bytes.Buffer
	if err := api.Write(c, ctx, &buf, defaultConf()); err != nil {
		return nil, err
	}
	return &Result{Name: "_att.pdf", Data: buf.Bytes(), Details: newDetails("added", names)}, nil
}

// AttRemove removes embedded files by name.
func AttRemove(c context.Context, data []byte, names []string) (*Result, error) {
	if len(names) == 0 {
		return nil, errors.New("no attachment names provided")
	}
	var buf bytes.Buffer
	if err := api.RemoveAttachments(c, bytes.NewReader(data), &buf, names, defaultConf()); err != nil {
		return nil, err
	}
	return &Result{Name: "_att.pdf", Data: buf.Bytes(), Details: newDetails("removed", names)}, nil
}

// AttExtract extracts embedded files.
func AttExtract(c context.Context, data []byte, names []string) (*Result, error) {
	aa, err := api.ExtractAttachmentsRaw(c, bytes.NewReader(data), "", names, defaultConf())
	if err != nil {
		return nil, err
	}
	if len(aa) == 0 {
		return nil, errors.New("no attachments found")
	}
	files := make([]NamedBytes, 0, len(aa))
	for _, a := range aa {
		name := a.FileName
		if name == "" {
			name = a.ID
		}
		if name == "" {
			name = "attachment.bin"
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, a.Reader); err != nil {
			return nil, fmt.Errorf("read attachment %s: %w", name, err)
		}
		files = append(files, NamedBytes{Name: name, Data: buf.Bytes()})
	}
	return &Result{Files: files, Details: newDetails("count", len(files))}, nil
}

// ---- P3: signatures ----

// SigValidate validates digital signatures and returns a report.
func SigValidate(c context.Context, data []byte, all bool) (*Result, error) {
	conf := defaultConf()
	conf.Offline = true // the browser build never fetches CRLs/OCSP
	rs := bytes.NewReader(data)
	results, err := api.ValidateSignaturesRaw(c, rs, all, conf)
	if err != nil {
		if strings.Contains(err.Error(), "no signatures") {
			return &Result{Details: newDetails("report", "no signatures found", "count", 0)}, nil
		}
		return nil, err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d signature(s) present\n", len(results))
	for i, r := range results {
		fmt.Fprintf(&sb, "\n%d: %s\n", i+1, r.Signature.String(r.Status))
		fmt.Fprintf(&sb, "  status: %s\n", r.Status.String())
		fmt.Fprintf(&sb, "  signed: %s\n", r.SigningTime())
		fmt.Fprintf(&sb, "  reason: %s\n", r.Reason.String())
		if r.Visible {
			sb.WriteString("  visible: yes\n")
		}
		for _, p := range r.Problems {
			fmt.Fprintf(&sb, "  problem: %s\n", p)
		}
	}
	report := sb.String()
	return &Result{
		Details: newDetails("report", report, "count", len(results), "lines", strings.Split(report, "\n")),
	}, nil
}

// SigRemove strips all digital signatures.
func SigRemove(c context.Context, data []byte) (*Result, error) {
	var buf bytes.Buffer
	if err := api.RemoveSignatures(c, bytes.NewReader(data), &buf, defaultConf()); err != nil {
		return nil, err
	}
	return &Result{Name: "_unsigned.pdf", Data: buf.Bytes()}, nil
}
