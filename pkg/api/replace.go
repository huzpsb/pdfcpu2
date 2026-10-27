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
	"fmt"
	"os"
	"strings"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/fault"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
)

// ReplaceTextFile replaces every occurrence of find by repl on the selected
// pages (all pages by default) of inFile and writes the result to outFile.
// The original glyphs are removed from the content streams; the replacement
// is drawn with an embedded subset of the bundled vivo Sans, preserving the
// original text transform, font size and fill color. It returns the number
// of replaced occurrences. Supports cancellation.
func ReplaceTextFile(c context.Context, inFile, outFile, find, repl string, selectedPages []string, conf *model.Configuration) (n int, err error) {
	defer fault.Catch(&err)

	if err := contextutil.Check(c); err != nil {
		return 0, err
	}
	if inFile == "" {
		return 0, ErrMissingPDFInput
	}
	if find == "" {
		return 0, fmt.Errorf("replace: empty find string")
	}
	if strings.ContainsAny(find, "\n\r") {
		return 0, fmt.Errorf("replace: multi-line find strings are not supported")
	}

	f, err := os.Open(inFile)
	if err != nil {
		return 0, fmt.Errorf("replace: open input %s: %w", inFile, err)
	}
	defer func() {
		err = joinExtractionCleanupError(err, closeFile(f, "replace: close input"))
	}()

	if outFile == "" {
		outFile = strings.TrimSuffix(inFile, ".pdf") + "_replaced.pdf"
	}
	if outFile == inFile {
		return 0, fmt.Errorf("replace: outFile must differ from inFile")
	}

	ctx, err := ReadValidateAndOptimize(c, f, operationConfiguration(conf, model.REPLACETEXT), nil)
	if err != nil {
		return 0, fmt.Errorf("replace %s: %w", inFile, err)
	}

	pages, err := PagesForSelection(ctx.PageCount, selectedPages, true)
	if err != nil {
		return 0, fmt.Errorf("replace: parse page selection: %w", err)
	}

	pageNrs := sortedPages(pages)
	n, err = pdfcpu.ReplaceTextPages(c, ctx, find, repl, pageNrs)
	if err != nil {
		return n, fmt.Errorf("replace %s: %w", inFile, err)
	}
	if n == 0 {
		// nothing matched — do not rewrite the file
		return 0, contextutil.Check(c)
	}

	// clean up the incremental edits (appended draw streams, re-encoded
	// content fragments) before writing
	if err := OptimizeContext(c, ctx); err != nil {
		return n, fmt.Errorf("replace %s: optimize output: %w", inFile, err)
	}

	if err := WriteContextFile(c, ctx, outFile); err != nil {
		return n, fmt.Errorf("replace: write output %s: %w", outFile, err)
	}
	return n, contextutil.Check(c)
}
