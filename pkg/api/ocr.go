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

// OCRFile runs the embedded OCR engine over the image content of selected
// pages and writes an augmented copy of inFile to outFile with an invisible,
// selectable text layer under every recognized image (scanned pages become
// accessible). Images already processed by an earlier run are skipped.
// It returns the number of images processed. Supports cancellation.
func OCRFile(c context.Context, inFile, outFile string, selectedPages []string, conf *model.Configuration) (n int, err error) {
	defer fault.Catch(&err)

	if err := contextutil.Check(c); err != nil {
		return 0, err
	}
	if inFile == "" {
		return 0, ErrMissingPDFInput
	}

	f, err := os.Open(inFile)
	if err != nil {
		return 0, fmt.Errorf("ocr: open input %s: %w", inFile, err)
	}
	defer func() {
		err = joinExtractionCleanupError(err, closeFile(f, "ocr: close input"))
	}()

	if outFile == "" {
		outFile = strings.TrimSuffix(inFile, ".pdf") + "_ocr.pdf"
	}
	if outFile == inFile {
		return 0, fmt.Errorf("ocr: outFile must differ from inFile")
	}

	ctx, err := ReadValidateAndOptimize(c, f, operationConfiguration(conf, model.OCRPDF), nil)
	if err != nil {
		return 0, fmt.Errorf("ocr %s: %w", inFile, err)
	}

	pages, err := PagesForSelection(ctx.PageCount, selectedPages, true)
	if err != nil {
		return 0, fmt.Errorf("ocr: parse page selection: %w", err)
	}

	pageNrs := sortedPages(pages)
	n, err = pdfcpu.OCRPages(c, ctx, pageNrs)
	if err != nil {
		return n, fmt.Errorf("ocr %s: %w", inFile, err)
	}
	if n == 0 {
		// nothing new recognized — do not rewrite the file
		return 0, contextutil.Check(c)
	}

	if err := WriteContextFile(c, ctx, outFile); err != nil {
		return n, fmt.Errorf("ocr: write output %s: %w", outFile, err)
	}
	return n, contextutil.Check(c)
}
