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

package model

// Default values for TextExtractOptions.
const (
	// TextExtractDefaultLineTolerance is the default maximum vertical distance in points
	// under which glyphs are grouped into the same text line.
	TextExtractDefaultLineTolerance = 3.0

	// TextExtractDefaultWordGapFactor is the default fraction of the font size a
	// horizontal gap must exceed for a space to be inserted between two text runs.
	TextExtractDefaultWordGapFactor = 0.25

	// TextExtractDefaultPageSeparator is the default separator between pages
	// in consolidated text output.
	TextExtractDefaultPageSeparator = "\n\n"

	// TextExtractDefaultEOL is the default line delimiter.
	TextExtractDefaultEOL = "\n"
)

// TextExtractOptions configure plain text extraction.
// A nil *TextExtractOptions is valid and refers to the defaults returned by
// NewTextExtractOptions.
type TextExtractOptions struct {
	// SortByPosition sorts extracted lines top-down, left-right.
	// If false, lines are reported in content stream order which usually
	// reflects the natural writing order of well-formed documents.
	SortByPosition bool

	// LineTolerance is the maximum vertical distance in points for glyphs
	// to be grouped into the same text line.
	// Values <= 0 fall back to TextExtractDefaultLineTolerance.
	LineTolerance float64

	// WordGapFactor is the fraction of the font size a horizontal gap between
	// two text runs must exceed for a space to be inserted.
	// Values <= 0 fall back to TextExtractDefaultWordGapFactor.
	WordGapFactor float64

	// NormalizeWhitespace collapses whitespace runs within lines
	// and trims leading and trailing whitespace of each line.
	NormalizeWhitespace bool

	// PageSeparator separates pages in consolidated text output.
	// It defaults to TextExtractDefaultPageSeparator.
	PageSeparator string

	// EOL is the line delimiter used for text output.
	// It defaults to TextExtractDefaultEOL.
	EOL string
}

// NewTextExtractOptions returns the default text extraction options.
func NewTextExtractOptions() *TextExtractOptions {
	return &TextExtractOptions{
		SortByPosition: false,
		LineTolerance:  TextExtractDefaultLineTolerance,
		WordGapFactor:  TextExtractDefaultWordGapFactor,
		PageSeparator:  TextExtractDefaultPageSeparator,
		EOL:            TextExtractDefaultEOL,
	}
}

// EnsureDefaults returns opts with all empty values replaced by their defaults.
func (opts TextExtractOptions) EnsureDefaults() TextExtractOptions {
	o := opts
	if o.LineTolerance <= 0 {
		o.LineTolerance = TextExtractDefaultLineTolerance
	}
	if o.WordGapFactor <= 0 {
		o.WordGapFactor = TextExtractDefaultWordGapFactor
	}
	if o.PageSeparator == "" {
		o.PageSeparator = TextExtractDefaultPageSeparator
	}
	if o.EOL == "" {
		o.EOL = TextExtractDefaultEOL
	}
	return o
}
