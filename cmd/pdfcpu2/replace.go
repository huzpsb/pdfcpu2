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

const usageLongReplace = `Replace text in a PDF.

replace removes every occurrence of the find string from the selected pages
(all pages by default) — the matched glyphs are surgically deleted from the
content streams, including text inside Form XObjects — and draws the
replacement with an embedded subset of the bundled vivo Sans. Each
occurrence is anchored at its visually topmost-left glyph and preserves the
original text transform (position, rotation, skew, scale), font size,
character/word spacing, rise, render mode and fill color; bold originals are
approximated with fill+stroke. An empty replacement string deletes the text.

Original fonts that lose glyphs are not garbage collected yet. Matching
covers horizontal text lines only.

Usage:
  pdfcpu2 replace inFile outFile -find string [-replace string] [-pages pageSelection] [password flags]

Args:
  inFile   input PDF file ("-": stdin not supported for replace)
  outFile  output PDF file

Flags:
  find     the text to replace (required, single line)
  replace  the replacement text (default "", i.e. deletion)

Examples:

Replace a word on all pages:
  $ pdfcpu2 replace letter.pdf letter2.pdf -find "ACME" -replace "CONTOSO"

Delete all occurrences of a token:
  $ pdfcpu2 replace scan.pdf scan2.pdf -find "DRAFT"

Restrict to the first 10 pages:
  $ pdfcpu2 replace in.pdf out.pdf -find "old" -replace "new" -pages 1-10
`

func replaceCmd() *cobra.Command {
	var find, repl string
	cmd := &cobra.Command{
		Use:   "replace inFile outFile -find string [-replace string]",
		Short: "Replace text in a PDF",
		Long:  usageLongReplace,
		Args:  cobra.ExactArgs(2),
		RunE: wrapContextHandler(func(c context.Context, conf *model.Configuration, args []string) error {
			return handleReplaceCommand(c, conf, args, find, repl)
		}),
	}
	cmd.Flags().StringVarP(&find, "find", "f", "", "the text to replace (required, single line)")
	cmd.Flags().StringVarP(&repl, "replace", "r", "", "the replacement text (default \"\", i.e. deletion)")
	addSelectedPagesFlag(cmd)
	addPasswordFlags(cmd)
	return cmd
}

func handleReplaceCommand(c context.Context, conf *model.Configuration, args []string, find, repl string) error {
	if err := contextutil.Check(c); err != nil {
		return err
	}
	if find == "" {
		return errors.New("replace requires a non-empty -find string")
	}
	inFile := args[0]
	if inFile == "-" {
		return errors.New("replace requires a seekable input file")
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
	return runCommand(c, cli.ReplaceTextCommand(inFile, outFile, find, repl, pages, conf))
}
