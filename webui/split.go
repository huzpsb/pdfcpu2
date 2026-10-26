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
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/huzpsb/pdfcpu2/pkg/api"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/sanitize"
)

// zipWriter buffers a ZIP archive in memory.
type zipWriter struct {
	buf *bytes.Buffer
	z   *zip.Writer
}

func newZipWriter() *zipWriter {
	buf := &bytes.Buffer{}
	return &zipWriter{buf: buf, z: zip.NewWriter(buf)}
}

func (w *zipWriter) addBytes(name string, data []byte) error {
	fw, err := w.z.Create(strings.ReplaceAll(name, "\\", "/"))
	if err != nil {
		return err
	}
	_, err = fw.Write(data)
	return err
}

func (w *zipWriter) closeTo(buf *bytes.Buffer) error {
	if err := w.z.Close(); err != nil {
		return err
	}
	buf.Reset()
	_, err := buf.Write(w.buf.Bytes())
	return err
}

// span is a contiguous 1-based page range.
type span struct {
	from, thru int
	title      string // optional bookmark title for bookmark splits
}

func (sp span) fileName(base string) string {
	if sp.title != "" {
		if n, err := sanitize.Path(sp.title); err == nil && n != "" {
			return n + ".pdf"
		}
	}
	if sp.from == sp.thru {
		return fmt.Sprintf("%s_%d.pdf", base, sp.from)
	}
	return fmt.Sprintf("%s_%d-%d.pdf", base, sp.from, sp.thru)
}

// writeSpan extracts one span as PDF bytes.
func writeSpan(c context.Context, ctx *model.Context, sp span, out *bytes.Buffer) error {
	ctxNew, err := pdfcpu.ExtractPages(c, ctx, api.PagesForPageRange(sp.from, sp.thru), false)
	if err != nil {
		return fmt.Errorf("extract pages %d-%d: %w", sp.from, sp.thru, err)
	}
	out.Reset()
	return api.WriteContext(c, ctxNew, out)
}

// spansForSplit computes the page spans for the selected split mode.
func spansForSplit(c context.Context, ctx *model.Context, p Params) ([]span, error) {
	mode := p.Str("mode", "span")
	switch mode {
	case "bookmarks":
		bms, err := pdfcpu.Bookmarks(c, ctx)
		if err != nil {
			return nil, fmt.Errorf("read bookmarks: %w", err)
		}
		if len(bms) == 0 {
			return nil, fmt.Errorf("no bookmarks found")
		}
		spans := make([]span, 0, len(bms))
		for _, bm := range bms {
			thru := bm.PageThru
			if thru == 0 {
				thru = ctx.PageCount
			}
			spans = append(spans, span{from: bm.PageFrom, thru: thru, title: bm.Title})
		}
		return spans, nil

	case "pages":
		pageNrs := p.Ints("pageNrs")
		if len(pageNrs) == 0 {
			return nil, fmt.Errorf("no page numbers provided")
		}
		sort.Ints(pageNrs)
		spans := []span{}
		from := 1
		for _, nr := range pageNrs {
			if nr < 2 || nr > ctx.PageCount || nr-1 < from {
				continue
			}
			spans = append(spans, span{from: from, thru: nr - 1})
			from = nr
		}
		spans = append(spans, span{from: from, thru: ctx.PageCount})
		return spans, nil

	default: // every N pages
		n := p.Int("span", 1)
		if n < 1 {
			n = 1
		}
		spans := []span{}
		for from := 1; from <= ctx.PageCount; from += n {
			thru := from + n - 1
			if thru > ctx.PageCount {
				thru = ctx.PageCount
			}
			spans = append(spans, span{from: from, thru: thru})
		}
		return spans, nil
	}
}

// Split splits data into multiple PDFs.
func Split(c context.Context, data []byte, name string, p Params) (*Result, error) {
	ctx, err := readContext(c, data, defaultConf())
	if err != nil {
		return nil, err
	}
	spans, err := spansForSplit(c, ctx, p)
	if err != nil {
		return nil, err
	}

	base := baseName(downloadName(name))
	files := make([]NamedBytes, 0, len(spans))
	var buf bytes.Buffer
	for _, sp := range spans {
		if err := writeSpan(c, ctx, sp, &buf); err != nil {
			return nil, err
		}
		b := make([]byte, buf.Len())
		copy(b, buf.Bytes())
		files = append(files, NamedBytes{Name: sp.fileName(base), Data: b})
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return &Result{Files: files, Details: newDetails("files", names, "count", len(files))}, nil
}

func downloadName(name string) string {
	if name = strings.TrimSpace(name); name == "" {
		return "document.pdf"
	}
	return name
}

// ---- batch ----

// batchCapable lists the operations the batch mode supports.
var batchCapable = map[string]bool{
	"encrypt": true, "decrypt": true, "ocr": true, "solidify": true,
	"obfuscate": true, "optimize": true, "totext": true, "watermark": true,
}

// batchItem reports one batch file.
type batchItem struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Note  string `json:"note,omitempty"`
}

// Batch applies op to every item and packs the results into one ZIP.
func Batch(c context.Context, op string, items []NamedBytes, p Params, logo []byte, logoName string) (*Result, error) {
	if !batchCapable[op] {
		return nil, fmt.Errorf("batch: operation %q is not supported", op)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("batch: no files")
	}

	zw := newZipWriter()
	reports := make([]batchItem, 0, len(items))
	names := []string{}
	seen := map[string]int{}
	anyOK := false

	for _, it := range items {
		res, err := runBatchOp(c, op, it.Data, p, logo, logoName)
		if err != nil {
			reports = append(reports, batchItem{Name: it.Name, Error: err.Error()})
			continue
		}
		if len(res.Data) == 0 {
			reports = append(reports, batchItem{Name: it.Name, OK: true, Note: "nothing to do"})
			continue
		}
		name := baseName(it.Name) + res.Name
		if n := seen[name]; n > 0 {
			name = fmt.Sprintf("%s(%d)%s", baseName(name), n+1, ext(name))
		}
		seen[name]++
		if err := zw.addBytes(name, res.Data); err != nil {
			reports = append(reports, batchItem{Name: it.Name, Error: err.Error()})
			continue
		}
		names = append(names, name)
		anyOK = true
		reports = append(reports, batchItem{Name: it.Name, OK: true})
	}

	buf := &bytes.Buffer{}
	if err := zw.closeTo(buf); err != nil {
		return nil, err
	}
	if !anyOK {
		firstErr := "all files failed"
		for _, r := range reports {
			if r.Error != "" {
				firstErr = r.Error
				break
			}
		}
		return nil, fmt.Errorf("batch %s: %s", op, firstErr)
	}
	return &Result{
		Name:    "batch_" + op + ".zip",
		Data:    buf.Bytes(),
		Details: newDetails("items", reports, "files", names, "count", len(reports)),
	}, nil
}

func runBatchOp(c context.Context, op string, data []byte, p Params, logo []byte, logoName string) (*Result, error) {
	switch op {
	case "encrypt":
		return Encrypt(c, data, p)
	case "decrypt":
		return Decrypt(c, data, p)
	case "ocr":
		return OCR(c, data, p)
	case "solidify":
		return Solidify(c, data, p)
	case "totext":
		return ToText(c, data, p)
	case "obfuscate":
		return Obfuscate(c, data)
	case "optimize":
		return Optimize(c, data)
	case "watermark":
		if p.Str("mode", "text") == "image" {
			return WatermarkImage(c, data, logo, logoName, p)
		}
		return WatermarkText(c, data, p)
	}
	return nil, fmt.Errorf("unsupported op %q", op)
}

func ext(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i:]
	}
	return ""
}

// ensure io usage (zip.Writer writes through addBytes).
var _ io.Writer = (*bytes.Buffer)(nil)
