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
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/fault"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
)

// ExtractText extracts the text of selectedPages from rs and returns one
// PageText per page in ascending page order.
// selectedPages follows the common page selection syntax, an empty selection
// extracts all pages. Line reconstruction is controlled by opts,
// nil opts select the defaults. Extraction supports cancellation.
func ExtractText(c context.Context, rs io.ReadSeeker, selectedPages []string, opts *model.TextExtractOptions, conf *model.Configuration) (texts []pdfcpu.PageText, err error) {
	defer fault.Catch(&err)

	if err := contextutil.Check(c); err != nil {
		return nil, err
	}
	if rs == nil {
		return nil, ErrMissingPDFReadSeeker
	}

	conf = operationConfiguration(conf, model.EXTRACTTEXT)

	// Text extraction is a read only operation that does not depend on the
	// structure tree, so tolerate unusually deep object graphs that would
	// otherwise fail validation.
	if conf.Limits.MaxRecursionDepth < 1000 {
		conf = conf.Clone()
		conf.Limits.MaxRecursionDepth = 1000
	}

	ctx, err := ReadValidateAndOptimize(c, rs, conf, nil)
	if err != nil {
		return nil, fmt.Errorf("extract text: %w", err)
	}

	pages, err := PagesForSelection(ctx.PageCount, selectedPages, true)
	if err != nil {
		return nil, fmt.Errorf("extract text: parse page selection: %w", err)
	}

	for _, pageNr := range sortedPages(pages) {
		if err := contextutil.Check(c); err != nil {
			return nil, err
		}
		pt, err := pdfcpu.ExtractPageText(c, ctx, pageNr, opts)
		if err != nil {
			return nil, fmt.Errorf("extract text: %w", err)
		}
		texts = append(texts, *pt)
	}

	return texts, contextutil.Check(c)
}

// ExtractTextString extracts the text of selectedPages from rs as one string
// with pages separated by opts.PageSeparator. Extraction supports cancellation.
func ExtractTextString(c context.Context, rs io.ReadSeeker, selectedPages []string, opts *model.TextExtractOptions, conf *model.Configuration) (string, error) {
	if err := contextutil.Check(c); err != nil {
		return "", err
	}

	o := model.TextExtractOptions{}
	if opts != nil {
		o = *opts
	}
	o = o.EnsureDefaults()

	texts, err := ExtractText(c, rs, selectedPages, &o, conf)
	if err != nil {
		return "", err
	}

	pages := make([]string, len(texts))
	for i := range texts {
		pages[i] = texts[i].Text(&o)
	}

	return strings.Join(pages, o.PageSeparator), nil
}

// ExtractTextReader extracts the text of selectedPages from rs into an io.Reader.
// Extraction supports cancellation.
func ExtractTextReader(c context.Context, rs io.ReadSeeker, selectedPages []string, opts *model.TextExtractOptions, conf *model.Configuration) (io.Reader, error) {
	s, err := ExtractTextString(c, rs, selectedPages, opts, conf)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(s), nil
}

// ExtractTextFile writes the text of selectedPages of inFile to outFile.
// An empty outFile defaults to inFile with a .txt suffix,
// outFile "-" writes to stdout. Extraction supports cancellation.
func ExtractTextFile(c context.Context, inFile, outFile string, selectedPages []string, opts *model.TextExtractOptions, conf *model.Configuration) (err error) {
	if err := contextutil.Check(c); err != nil {
		return err
	}
	if inFile == "" {
		return ErrMissingPDFInput
	}

	f, err := os.Open(inFile)
	if err != nil {
		return fmt.Errorf("extract text: open input %s: %w", inFile, err)
	}
	defer func() {
		err = joinExtractionCleanupError(err, closeFile(f, "extract text: close input"))
	}()

	if outFile == "" {
		outFile = strings.TrimSuffix(inFile, ".pdf") + ".txt"
	}

	if outFile == "-" {
		s, err := ExtractTextString(c, f, selectedPages, opts, conf)
		if err != nil {
			return fmt.Errorf("extract text %s: %w", inFile, err)
		}
		fmt.Print(s)
		return nil
	}

	var b bytes.Buffer
	s, err := ExtractTextString(c, f, selectedPages, opts, conf)
	if err != nil {
		return fmt.Errorf("extract text %s: %w", inFile, err)
	}
	b.WriteString(s)

	if err := pdfcpu.WriteReader(c, outFile, &b); err != nil {
		return fmt.Errorf("extract text: write output %s: %w", outFile, err)
	}

	return nil
}
