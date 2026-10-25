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

package main

import (
	"context"
	"errors"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/cli"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/spf13/cobra"
)

const usageLongOCR = `Embed an invisible OCR text layer into a PDF.

ocr recognizes text in every image drawn on the selected pages (all pages by
default) using the embedded PP-OCR detection/recognition models and places
the recognized characters as an invisible text layer over each image, mapped
through the image's page transformation. Scanned PDFs become searchable,
selectable and extractable (pdfcpu2 extract -mode text, readers, copy&paste).

Every processed image is marked, so re-running ocr never re-processes
already-OCRed images. Pages without image content are left untouched.

Usage:
  pdfcpu2 ocr inFile outFile [-pages pageSelection] [password flags]

Args:
  inFile   input PDF file ("-": stdin not supported for ocr)
  outFile  output PDF file

Examples:

Embed text for all pages of a scanned document:
  $ pdfcpu2 ocr scan.pdf scan_ocr.pdf

Restrict to the first 10 pages:
  $ pdfcpu2 ocr scan.pdf scan_ocr.pdf -pages 1-10
`

func ocrCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ocr inFile outFile",
		Short: "Embed an invisible OCR text layer into scanned PDFs",
		Long:  usageLongOCR,
		Args:  cobra.ExactArgs(2),
		RunE: wrapContextHandler(func(c context.Context, conf *model.Configuration, args []string) error {
			return handleOCRCommand(c, conf, args)
		}),
	}
	addSelectedPagesFlag(cmd)
	addPasswordFlags(cmd)
	return cmd
}

func handleOCRCommand(c context.Context, conf *model.Configuration, args []string) error {
	if err := contextutil.Check(c); err != nil {
		return err
	}
	inFile := args[0]
	if inFile == "-" {
		return errors.New("ocr requires a seekable input file")
	}
	if err := inputPDFArg(conf, inFile); err != nil {
		return err
	}
	outFile := args[1]
	if err := ensurePDFExtension(outFile); err != nil {
		return err
	}
	if err := ensureOutputFileAvailable(outFile); err != nil {
		return err
	}
	pages, err := parseSelectedPages()
	if err != nil {
		return err
	}
	return runCommand(c, cli.OCRCommand(inFile, outFile, pages, conf))
}
