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
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/fault"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
)

// Obfuscate destroys the text extraction mappings of all embedded fonts of
// rs (the cmap tables of embedded font programs and all ToUnicode CMaps) by
// garbage filling them and writes the result to w. Rendering is not
// affected. It supports cancellation.
func Obfuscate(c context.Context, rs io.ReadSeeker, w io.Writer, conf *model.Configuration) (stats pdfcpu.ObfuscateStats, err error) {
	defer fault.Catch(&err)

	if err := contextutil.Check(c); err != nil {
		return stats, err
	}
	if rs == nil {
		return stats, ErrMissingPDFReadSeeker
	}
	if w == nil {
		return stats, ErrMissingPDFWriter
	}

	conf = operationConfiguration(conf, model.OBFUSCATE)

	ctx, err := ReadValidateAndOptimize(c, rs, conf, nil)
	if err != nil {
		return stats, fmt.Errorf("obfuscate: %w", err)
	}

	if stats, err = pdfcpu.ObfuscateFonts(c, ctx); err != nil {
		return stats, fmt.Errorf("obfuscate: %w", err)
	}

	if err = Write(c, ctx, w, conf); err != nil {
		return stats, fmt.Errorf("obfuscate: write output: %w", err)
	}
	return stats, nil
}

// ObfuscateFile destroys the text extraction mappings of all embedded fonts
// of inFile (the cmap tables of embedded font programs and all ToUnicode
// CMaps) by garbage filling them and writes the result to outFile. An empty
// outFile overwrites inFile. It supports cancellation.
func ObfuscateFile(c context.Context, inFile, outFile string, conf *model.Configuration) (stats pdfcpu.ObfuscateStats, err error) {
	var f1, f2 *os.File
	ok := false

	if err := contextutil.Check(c); err != nil {
		return stats, err
	}
	if inFile == "" {
		return stats, ErrMissingPDFInput
	}

	if f1, err = os.Open(inFile); err != nil {
		return stats, fmt.Errorf("obfuscate: open input %s: %w", inFile, err)
	}

	tmpFile := ""
	if outFile != "" && inFile != outFile {
		tmpFile = outFile
	}
	staged, err := openStagedOutput(f1, inFile, tmpFile, "obfuscate")
	if err != nil {
		return stats, errors.Join(
			fmt.Errorf("obfuscate: create output: %w", err),
			closeFile(f1, "obfuscate: close input"),
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

	if stats, err = Obfuscate(c, f1, f2, conf); err != nil {
		return stats, err
	}
	if err := contextutil.Check(c); err != nil {
		return stats, err
	}

	ok = true

	return stats, nil
}
