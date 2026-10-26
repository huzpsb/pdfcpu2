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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/fault"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/sanitize"
	"github.com/huzpsb/pdfcpu2/pkg/render"
)

// RenderPages renders selected pages of rs and digests every result.
// Rendering supports cancellation.
func RenderPages(c context.Context, rs io.ReadSeeker, selectedPages []string, o *render.Options, digestPage func(*render.Result) error, conf *model.Configuration) (err error) {
	defer fault.Catch(&err)

	if err := contextutil.Check(c); err != nil {
		return err
	}
	if rs == nil {
		return ErrMissingPDFReadSeeker
	}
	if digestPage == nil {
		return fmt.Errorf("render pages: %w", ErrMissingDigestFunction)
	}

	conf = operationConfiguration(conf, model.RENDERPAGES)

	ctx, err := ReadValidateAndOptimize(c, rs, conf, nil)
	if err != nil {
		return fmt.Errorf("render pages: %w", err)
	}

	pages, err := PagesForSelection(ctx.PageCount, selectedPages, true)
	if err != nil {
		return fmt.Errorf("render pages: parse page selection: %w", err)
	}

	for _, pageNr := range sortedPages(pages) {
		if err := contextutil.Check(c); err != nil {
			return err
		}
		res, err := render.Page(c, ctx, pageNr, o)
		if err != nil {
			return fmt.Errorf("render pages: %w", err)
		}
		if err := digestPage(res); err != nil {
			return fmt.Errorf("render pages: page %d: digest: %w", pageNr, err)
		}
	}

	return contextutil.Check(c)
}

// WritePageImageToDisk returns a closure for writing a rendered page
// transactionally into outDir with cancellation.
func WritePageImageToDisk(c context.Context, outDir, fnBase, ext string, quality int) func(*render.Result) error {
	fnBase = sanitize.PathOr(fnBase, "file")
	ext = sanitize.PathOr(ext, "img")

	return func(res *render.Result) error {
		if err := contextutil.Check(c); err != nil {
			return err
		}
		if res == nil || res.Image == nil {
			return fmt.Errorf("page image: %w", ErrMissingImageReader)
		}
		outFile := filepath.Join(outDir, fmt.Sprintf("%s_page_%d.%s", fnBase, res.PageNr, ext))
		var b bytes.Buffer
		if err := render.Encode(&b, res.Image, ext, quality); err != nil {
			return err
		}
		return pdfcpu.WriteReader(c, outFile, &b)
	}
}

// RenderPagesFile renders selected pages of inFile into outDir, one image
// file per page, and supports cancellation.
func RenderPagesFile(c context.Context, inFile, outDir string, selectedPages []string, o *render.Options, conf *model.Configuration) (err error) {
	if err := contextutil.Check(c); err != nil {
		return err
	}
	if inFile == "" {
		return ErrMissingPDFInput
	}
	if o == nil {
		o = &render.Options{}
	}
	format, err := render.NormalizeFormat(o.Format)
	if err != nil {
		return fmt.Errorf("render pages: %w", err)
	}

	if outDir != "" {
		if err := os.MkdirAll(outDir, 0755); err != nil {
			return fmt.Errorf("render pages: create output dir %s: %w", outDir, err)
		}
	}

	f, err := os.Open(inFile)
	if err != nil {
		return fmt.Errorf("render pages: open input %s: %w", inFile, err)
	}
	defer func() {
		err = errors.Join(err, closeFile(f, "render pages: close input"))
	}()

	fnBase := strings.TrimSuffix(filepath.Base(inFile), ".pdf")

	if err := RenderPages(c, f, selectedPages, o, WritePageImageToDisk(c, outDir, fnBase, format, o.JPEGQuality), conf); err != nil {
		return fmt.Errorf("render pages %s: %w", inFile, err)
	}
	return nil
}

// Solidify renders selected pages of rs, replaces their content with the
// resulting image and writes the modified PDF to w. It supports cancellation.
func Solidify(c context.Context, rs io.ReadSeeker, w io.Writer, selectedPages []string, o *render.Options, conf *model.Configuration) (err error) {
	defer fault.Catch(&err)

	if err := contextutil.Check(c); err != nil {
		return err
	}
	if rs == nil {
		return ErrMissingPDFReadSeeker
	}
	if w == nil {
		return ErrMissingPDFWriter
	}
	if o != nil {
		if _, err := render.NormalizeFormat(o.Format); err != nil {
			return fmt.Errorf("solidify: %w", err)
		}
	}

	conf = operationConfiguration(conf, model.SOLIDIFYPAGES)

	ctx, err := ReadValidateAndOptimize(c, rs, conf, nil)
	if err != nil {
		return fmt.Errorf("solidify: %w", err)
	}

	pages, err := PagesForSelection(ctx.PageCount, selectedPages, true)
	if err != nil {
		return fmt.Errorf("solidify: parse page selection: %w", err)
	}

	if err := contextutil.Check(c); err != nil {
		return err
	}

	if err = pdfcpu.SolidifyPages(c, ctx, pages, o); err != nil {
		return fmt.Errorf("solidify: apply: %w", err)
	}

	if err = Write(c, ctx, w, conf); err != nil {
		return fmt.Errorf("solidify: write output: %w", err)
	}
	return nil
}

// SolidifyFile renders selected pages of inFile, replaces their content with
// the resulting image and writes the result to outFile. It supports
// cancellation.
func SolidifyFile(c context.Context, inFile, outFile string, selectedPages []string, o *render.Options, conf *model.Configuration) (err error) {
	var f1, f2 *os.File
	ok := false

	if err := contextutil.Check(c); err != nil {
		return err
	}
	if inFile == "" {
		return ErrMissingPDFInput
	}
	if o != nil {
		if _, err := render.NormalizeFormat(o.Format); err != nil {
			return fmt.Errorf("solidify: %w", err)
		}
	}

	if f1, err = os.Open(inFile); err != nil {
		return fmt.Errorf("solidify: open input %s: %w", inFile, err)
	}

	tmpFile := ""
	if outFile != "" && inFile != outFile {
		tmpFile = outFile
	}
	staged, err := openStagedOutput(f1, inFile, tmpFile, "solidify")
	if err != nil {
		return errors.Join(
			fmt.Errorf("solidify: create output: %w", err),
			closeFile(f1, "solidify: close input"),
		)
	}
	f2 = staged.output.file

	defer func() {
		if !ok {
			err = staged.cleanup(err)
			return
		}
		err = staged.commit()
	}()

	if err = Solidify(c, f1, f2, selectedPages, o, conf); err != nil {
		return err
	}
	if err = contextutil.Check(c); err != nil {
		return err
	}

	ok = true

	return nil
}
