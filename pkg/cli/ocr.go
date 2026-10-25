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

package cli

import (
	"context"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/api"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
)

// OCRCommand creates a new command to embed an invisible OCR text layer
// into a scanned PDF.
func OCRCommand(inFile, outFile string, pageSelection []string, conf *model.Configuration) *Command {
	if conf == nil {
		conf = model.NewDefaultConfiguration()
		conf.Cmd = model.OCRPDF
	}
	return &Command{
		Mode:          model.OCRPDF,
		InFile:        &inFile,
		OutFile:       &outFile,
		PageSelection: pageSelection,
		Conf:          conf,
	}
}

func ocrPDF(c context.Context, cmd *Command) ([]string, error) {
	if err := contextutil.Check(c); err != nil {
		return nil, err
	}
	if err := validateCommandRequirements(cmd, commandRequirements{
		operation: "ocr",
		inFile:    commandStringRequiredNonEmpty,
		outFile:   commandStringRequiredNonEmpty,
	}); err != nil {
		return nil, err
	}
	reportCommandProgress(cmd, "ocr %s to %s ...\n", *cmd.InFile, *cmd.OutFile)

	n, err := api.OCRFile(c, *cmd.InFile, *cmd.OutFile, cmd.PageSelection, cmd.Conf)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		reportCommandProgress(cmd, "no unprocessed images found, %s not written\n", *cmd.OutFile)
	} else {
		reportCommandProgress(cmd, "%d images processed, %s written\n", n, *cmd.OutFile)
	}
	return nil, nil
}
