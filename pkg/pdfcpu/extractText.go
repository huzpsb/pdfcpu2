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

package pdfcpu

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/font"
	"github.com/huzpsb/pdfcpu2/pkg/log"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

// TextLine is a contiguous run of text sharing one baseline.
type TextLine struct {
	// Text is the extracted text of this line.
	Text string
	// X, Y are the coordinates of the first glyph in user space.
	X, Y float64
	// XMax is the right boundary of this line in user space.
	XMax float64
}

// PageText is the text extracted from a single page.
type PageText struct {
	// PageNr is the 1-based page number.
	PageNr int
	// Lines contains the reconstructed text lines.
	Lines []TextLine
}

// Text returns the plain text of pt using opts or the defaults for nil opts.
func (pt PageText) Text(opts *model.TextExtractOptions) string {
	if opts == nil {
		return pt.text(model.TextExtractOptions{}.EnsureDefaults())
	}
	return pt.text(opts.EnsureDefaults())
}

func (pt PageText) text(opts model.TextExtractOptions) string {
	var sb strings.Builder
	for i, line := range pt.Lines {
		if i > 0 {
			sb.WriteString(opts.EOL)
		}
		sb.WriteString(line.Text)
	}
	return sb.String()
}

// String implements the Stringer interface using default options.
func (pt PageText) String() string {
	return pt.Text(nil)
}

// affine is an affine transformation matrix in PDF row-vector convention.
type affine struct {
	a, b, c, d, e, f float64
}

func identityAffine() affine {
	return affine{a: 1, d: 1}
}

// multiply returns m x o (apply o first, then m).
func (m affine) multiply(o affine) affine {
	return affine{
		a: m.a*o.a + m.b*o.c,
		b: m.a*o.b + m.b*o.d,
		c: m.c*o.a + m.d*o.c,
		d: m.c*o.b + m.d*o.d,
		e: m.e*o.a + m.f*o.c + o.e,
		f: m.e*o.b + m.f*o.d + o.f,
	}
}

func (m affine) translate(tx, ty float64) affine {
	return affine{a: m.a, b: m.b, c: m.c, d: m.d, e: m.e + tx, f: m.f + ty}
}

// translation returns the pure translation matrix T(tx, ty).
func translation(tx, ty float64) affine {
	return affine{a: 1, d: 1, e: tx, f: ty}
}

// textRun is a chunk of text produced by one show operation.
type textRun struct {
	text     string
	x, y     float64
	xMax     float64
	fontSize float64
}

// textFont decodes character codes and provides glyph widths for one font.
type textFont struct {
	subtype  string
	baseFont string
	// codeLen is the character code byte length (simple fonts: 1).
	codeLen int
	// simple holds the 8-bit encoding for simple fonts.
	simple *simpleEncoding
	// cmap holds an optional ToUnicode CMap overriding the encoding.
	cmap *toUnicodeCMap
	// ucs2 marks Type0 fonts encoded via a UCS2 based CMap
	// where the 2-byte code is a Unicode scalar value.
	ucs2 bool
	// widths holds glyph space widths in thousandths for simple fonts.
	widths [256]float64
	// cidWidths holds glyph space widths in thousandths for composite fonts.
	cidWidths map[uint32]float64
	// cidDefaultWidth is the glyph space default width of composite fonts.
	cidDefaultWidth float64
	// vertical reports a vertical writing mode.
	vertical bool
	// ttfUnicode maps glyph indices to Unicode scalars parsed from an
	// embedded TrueType font program. It is the fallback for composite
	// fonts without ToUnicode CMap.
	ttfUnicode map[uint32]rune
	// legacy decodes character codes of composite fonts encoded via
	// legacy predefined CMaps like GBK-EUC-H.
	legacy legacyCodeDecoder
	// cidOrdering is the CIDSystemInfo Ordering of composite fonts
	// (GB1, CNS1, ...) enabling the Adobe character collection fallback.
	cidOrdering string
}

// legacyCodeDecoder decodes legacy character code bytes to Unicode text.
type legacyCodeDecoder func(bb []byte) string

// newLegacyCodeDecoder returns a decoder for a predefined CMap name whose
// codes carry a legacy byte encoding.
func newLegacyCodeDecoder(cmapName string) legacyCodeDecoder {
	switch cmapName {
	case "GBK-EUC-H", "GBK-EUC-V", "GBKp-EUC-H", "GBKp-EUC-V", "GBK2K-H", "GBK2K-V":
		return decodeGBK
	case "B5pc-H", "B5pc-V", "ETen-B5-H", "ETen-B5-V", "HKscs-B5-H", "HKscs-B5-V":
		return decodeBig5
	}
	return nil
}

func decodeGBK(bb []byte) string {
	s, err := simplifiedchinese.GBK.NewDecoder().Bytes(bb)
	if err != nil {
		return strings.Repeat(string(utf8.RuneError), len(bb))
	}
	return string(s)
}

func decodeBig5(bb []byte) string {
	s, err := traditionalchinese.Big5.NewDecoder().Bytes(bb)
	if err != nil {
		return strings.Repeat(string(utf8.RuneError), len(bb))
	}
	return string(s)
}

const defaultGlyphWidth = 500.0

func (f *textFont) effectiveCodeLen() int {
	// Simple fonts always use single byte codes even when a broken
	// ToUnicode CMap advertises a wider code space.
	if f.subtype != "Type0" {
		return 1
	}
	if f.cmap != nil && f.cmap.codeLen > 0 {
		return f.cmap.codeLen
	}
	if f.codeLen > 0 {
		return f.codeLen
	}
	return 1
}

// decode maps character code bytes to Unicode text.
func (f *textFont) decode(bb []byte) string {
	if len(bb) == 0 {
		return ""
	}
	l := f.effectiveCodeLen()

	if f.cmap != nil {
		var sb strings.Builder
		for i := 0; i < len(bb); {
			n := l
			if i+n > len(bb) {
				n = len(bb) - i
			}
			code := codeOfBytes(bb[i : i+n])
			if s, ok := f.cmap.lookup(code); ok {
				sb.WriteString(s)
			} else if r, ok := cidOrderingUnicode(f.cidOrdering, code); ok {
				// Adobe character collection fallback.
				sb.WriteRune(r)
			} else if f.simple != nil && n == 1 {
				// Fall back to the font encoding for codes the
				// ToUnicode CMap does not cover.
				sb.WriteString(f.simple.decode(bb[i : i+n]))
			} else {
				sb.WriteRune(utf8.RuneError)
			}
			i += n
		}
		return sb.String()
	}

	if f.ucs2 {
		var sb strings.Builder
		for i := 0; i < len(bb); i += 2 {
			if i+1 >= len(bb) {
				break
			}
			r := rune(uint16(bb[i])<<8 | uint16(bb[i+1]))
			if r == 0 {
				r = utf8.RuneError
			}
			sb.WriteRune(r)
		}
		return sb.String()
	}

	if f.ttfUnicode != nil {
		var sb strings.Builder
		for i := 0; i < len(bb); {
			n := l
			if i+n > len(bb) {
				n = len(bb) - i
			}
			code := codeOfBytes(bb[i : i+n])
			if r, ok := f.ttfUnicode[code]; ok {
				sb.WriteRune(r)
			} else {
				sb.WriteRune(utf8.RuneError)
			}
			i += n
		}
		return sb.String()
	}

	if f.legacy != nil {
		return f.legacy(bb)
	}

	if f.cidOrdering != "" && l > 0 {
		var sb strings.Builder
		for i := 0; i < len(bb); {
			n := l
			if i+n > len(bb) {
				n = len(bb) - i
			}
			code := codeOfBytes(bb[i : i+n])
			if r, ok := cidOrderingUnicode(f.cidOrdering, code); ok {
				sb.WriteRune(r)
			} else {
				sb.WriteRune(utf8.RuneError)
			}
			i += n
		}
		return sb.String()
	}

	if f.simple != nil {
		return f.simple.decode(bb)
	}

	return strings.Repeat(string(utf8.RuneError), (len(bb)+l-1)/l)
}

// w0 returns the glyph space width in thousandths for code.
func (f *textFont) w0(code uint32) float64 {
	if f.subtype == "Type0" {
		if w, ok := f.cidWidths[code]; ok {
			return w
		}
		if f.cidDefaultWidth > 0 {
			return f.cidDefaultWidth
		}
		return defaultGlyphWidth
	}
	if code < 256 && f.widths[code] > 0 {
		return f.widths[code]
	}
	return defaultGlyphWidth
}

// isSpaceCode reports whether code denotes a space eligible for word spacing.
func (f *textFont) isSpaceCode(code uint32) bool {
	return f.effectiveCodeLen() == 1 && code == 32
}

// textState is the text state of the graphics state.
type textState struct {
	font        *textFont
	fontSize    float64
	charSpacing float64
	wordSpacing float64
	hScale      float64
	leading     float64
	rise        float64
	tm, tlm     affine
}

func newTextState() textState {
	return textState{hScale: 1}
}

// reset reinitializes the text matrices as enforced by BT.
func (ts *textState) reset() {
	ts.tm = identityAffine()
	ts.tlm = identityAffine()
}

// advance returns the text space displacement for a glyph with width w0.
func (ts *textState) advance(w0 float64, isSpace bool) (tx, ty float64) {
	if ts.font != nil && ts.font.vertical {
		return 0, -w0 / 1000 * ts.fontSize
	}
	tx = (w0-ts.charSpacing/1000)*ts.hScale + ts.charSpacing
	if isSpace {
		tx += ts.wordSpacing
	}
	return tx / 1000 * ts.fontSize, 0
}

// applyTranslation moves the text matrix by tx, ty in text space,
// transformed through the current text matrix.
func (ts *textState) applyTranslation(tx, ty float64) {
	ts.tm = affine{
		a: ts.tm.a, b: ts.tm.b, c: ts.tm.c, d: ts.tm.d,
		e: ts.tm.a*tx + ts.tm.c*ty + ts.tm.e,
		f: ts.tm.b*tx + ts.tm.d*ty + ts.tm.f,
	}
}

// effectiveFontSize returns the font size in user space.
func (ts *textState) effectiveFontSize() float64 {
	if s := math.Abs(ts.tm.a) * ts.fontSize; s > 0 {
		return s
	}
	if s := math.Abs(ts.tm.d) * ts.fontSize; s > 0 {
		return s
	}
	return ts.fontSize
}

// extractor holds the state of one text extraction run.
type extractor struct {
	ctx  *model.Context
	opts model.TextExtractOptions

	fonts    map[int]*textFont
	runs     []textRun
	visiting map[int]bool

	// lastRun tracks the previously emitted run for overprint suppression:
	// some producers draw the same glyph repeatedly at the exact same
	// position through different font resources to fake boldness.
	lastRun *textRun
}

const maxFormDepth = 24

// dbg enables extraction debug logging during development.
var dbg = os.Getenv("PDFCPU_TEXT_DEBUG") != ""

// ExtractPageText extracts the text of pageNr of an already read and validated
// context and supports cancellation.
// Line reconstruction is controlled by opts, nil opts select the defaults.
func ExtractPageText(c context.Context, ctx *model.Context, pageNr int, opts *model.TextExtractOptions) (*PageText, error) {
	if err := contextutil.Check(c); err != nil {
		return nil, err
	}
	if err := requireContextWithXRefTable(ctx); err != nil {
		return nil, err
	}
	if err := validatePageNumber(ctx, pageNr); err != nil {
		return nil, err
	}

	o := model.TextExtractOptions{}
	if opts != nil {
		o = *opts
	}
	o = o.EnsureDefaults()

	pageDict, _, inhPAttrs, err := ctx.PageDict(c, pageNr, false)
	if err != nil {
		return nil, fmt.Errorf("page %d: page dict: %w", pageNr, err)
	}

	bb, err := ctx.PageContent(pageDict, pageNr)
	if err != nil && err != model.ErrNoContent {
		return nil, fmt.Errorf("page %d: page content: %w", pageNr, err)
	}

	e := &extractor{
		ctx:      ctx,
		opts:     o,
		fonts:    map[int]*textFont{},
		visiting: map[int]bool{},
	}

	if err := e.extract(c, bb, pageResources(ctx, pageDict, inhPAttrs), identityAffine(), 0); err != nil {
		return nil, fmt.Errorf("page %d: %w", pageNr, err)
	}

	return &PageText{PageNr: pageNr, Lines: e.reconstructLines()}, nil
}

// pageResources returns the effective resource dict of a page.
func pageResources(ctx *model.Context, pageDict types.Dict, inhPAttrs *model.InheritedPageAttrs) types.Dict {
	if o, found := pageDict.Find("Resources"); found && o != nil {
		if d, err := ctx.DereferenceDict(o); err == nil && d != nil {
			return d
		}
	}
	if inhPAttrs != nil && inhPAttrs.Resources != nil {
		return inhPAttrs.Resources
	}
	return types.Dict{}
}

// extract processes one content stream with its resources and base transformation.
func (e *extractor) extract(c context.Context, bb []byte, resources types.Dict, base affine, depth int) error {
	if err := contextutil.Check(c); err != nil {
		return err
	}
	if depth > maxFormDepth {
		if log.ParseEnabled() {
			log.Parse.Println("extractText: max form nesting depth exceeded")
		}
		return nil
	}

	fontDicts, err := e.fontResources(c, resources)
	if err != nil {
		return err
	}

	xObjDict, err := e.xObjectResources(resources)
	if err != nil {
		return err
	}

	ts := newTextState()
	ts.reset()

	sc := newContentScanner(bb)

	var operands []contentOperand
	opCount := 0

	for {
		if err := contextutil.Check(c); err != nil {
			return err
		}

		op, err := sc.next()
		if err != nil {
			// Tolerate trailing garbage but stop processing.
			if dbg && opCount == 0 {
				fmt.Fprintf(os.Stderr, "extractText: scanner failed on first token: %v\n", err)
			}
			return nil
		}
		if op.isZero() {
			if dbg && opCount == 0 {
				fmt.Fprintf(os.Stderr, "extractText: scanner produced no tokens (content len %d)\n", len(bb))
			}
			return nil
		}
		if op.operator != "" {
			opCount++
			if dbg && opCount <= 10 {
				fmt.Fprintf(os.Stderr, "extractText: op[%d] %q operands=%d\n", opCount, op.operator, len(operands))
			}
		}
		if op.isZero() {
			return nil
		}

		if op.operator == "BI" {
			if err := sc.skipInlineImage(); err != nil {
				if log.ParseEnabled() {
					log.Parse.Printf("extractText: inline image: %v\n", err)
				}
				return nil
			}
			operands = operands[:0]
			continue
		}

		if op.isOperand() {
			operands = append(operands, op)
			if len(operands) > 64 {
				operands = operands[len(operands)-64:]
			}
			continue
		}

		if err := e.applyOperator(c, op.operator, operands, &ts, fontDicts, xObjDict, base, depth); err != nil {
			return err
		}
		operands = operands[:0]
	}
}

// applyOperator interprets one content stream operator.
func (e *extractor) applyOperator(
	c context.Context,
	operator string,
	operands []contentOperand,
	ts *textState,
	fontDicts map[string]*textFont,
	xObjDict map[string]types.Object,
	base affine,
	depth int,
) error {
	switch operator {
	case "BT":
		ts.reset()
	case "Tf":
		if len(operands) < 2 {
			return nil
		}
		if f, ok := fontDicts[operands[0].name]; ok {
			ts.font = f
		} else if dbg {
			fmt.Fprintf(os.Stderr, "extractText: Tf font %q not in resources (have %d fonts)\n", operands[0].name, len(fontDicts))
		}
		if sz, ok := operands[1].number(); ok {
			ts.fontSize = sz
		}
	case "Tc":
		if v, ok := operandNumber(operands, 0); ok {
			ts.charSpacing = v
		}
	case "Tw":
		if v, ok := operandNumber(operands, 0); ok {
			ts.wordSpacing = v
		}
	case "Tz":
		if v, ok := operandNumber(operands, 0); ok {
			ts.hScale = v / 100
		}
	case "TL":
		if v, ok := operandNumber(operands, 0); ok {
			ts.leading = v
		}
	case "Ts":
		if v, ok := operandNumber(operands, 0); ok {
			ts.rise = v
		}
	case "Td":
		tx, ty := operandFloats(operands, 0, 1)
		ts.tlm = translation(tx, ty).multiply(ts.tlm)
		ts.tm = ts.tlm
	case "TD":
		tx, ty := operandFloats(operands, 0, 1)
		ts.leading = -ty
		ts.tlm = translation(tx, ty).multiply(ts.tlm)
		ts.tm = ts.tlm
	case "Tm":
		if len(operands) >= 6 {
			a, b := operandFloats(operands, 0, 1)
			cc, d := operandFloats(operands, 2, 3)
			e1, f2 := operandFloats(operands, 4, 5)
			ts.tlm = affine{a: a, b: b, c: cc, d: d, e: e1, f: f2}
			ts.tm = ts.tlm
		}
	case "T*":
		ts.tlm = translation(0, -ts.leading).multiply(ts.tlm)
		ts.tm = ts.tlm
	case "Tj", "'", "\"":
		if operator == "'" {
			ts.tlm = translation(0, -ts.leading).multiply(ts.tlm)
			ts.tm = ts.tlm
		}
		if operator == "\"" && len(operands) >= 3 {
			if v, ok := operandNumber(operands, 0); ok {
				ts.wordSpacing = v
			}
			if v, ok := operandNumber(operands, 1); ok {
				ts.charSpacing = v
			}
		}
		var bb []byte
		if len(operands) > 0 {
			bb = operands[len(operands)-1].str
		}
		e.showString(ts, bb)
	case "TJ":
		if len(operands) == 0 {
			return nil
		}
		for _, el := range operands[0].arr {
			if el.str != nil {
				e.showString(ts, el.str)
				continue
			}
			if v, ok := el.number(); ok {
				tx, _ := ts.advance(-v, false)
				ts.applyTranslation(tx, 0)
			}
		}
	case "Do":
		if len(operands) == 0 {
			return nil
		}
		if o, ok := xObjDict[operands[0].name]; ok {
			if err := e.processForm(c, o, base, depth); err != nil {
				return err
			}
		}
	}

	return nil
}

// showString decodes and records one string of glyphs at the current text position.
func (e *extractor) showString(ts *textState, bb []byte) {
	if len(bb) == 0 || ts.font == nil {
		if dbg && len(bb) > 0 {
			fmt.Fprintf(os.Stderr, "extractText: showString skipped: %d bytes, font=%v\n", len(bb), ts.font != nil)
		}
		return
	}

	f := ts.font
	l := f.effectiveCodeLen()

	var sb strings.Builder
	startX, startY := ts.tm.e, ts.tm.f
	x := startX

	for i := 0; i < len(bb); {
		n := l
		if i+n > len(bb) {
			n = len(bb) - i
		}
		code := codeOfBytes(bb[i : i+n])
		sb.WriteString(f.decode(bb[i : i+n]))

		tx, ty := ts.advance(f.w0(code), f.isSpaceCode(code))
		ts.applyTranslation(tx, ty)
		x = ts.tm.e
		i += n
	}

	if sb.Len() == 0 {
		return
	}

	run := textRun{
		text:     sb.String(),
		x:        startX,
		y:        startY,
		xMax:     x,
		fontSize: ts.effectiveFontSize(),
	}

	// Suppress overprinted duplicates: identical text at the identical
	// position drawn right before is a styling artifact, not content.
	if e.lastRun != nil &&
		run.text == e.lastRun.text &&
		math.Abs(run.x-e.lastRun.x) < 0.01 &&
		math.Abs(run.y-e.lastRun.y) < 0.01 &&
		math.Abs(run.xMax-e.lastRun.xMax) < 0.01 {
		return
	}

	e.runs = append(e.runs, run)
	e.lastRun = &e.runs[len(e.runs)-1]
}

// processForm descends into a form XObject.
func (e *extractor) processForm(c context.Context, o types.Object, base affine, depth int) error {
	objNr := 0
	if ir, ok := o.(types.IndirectRef); ok {
		objNr = ir.ObjectNumber.Value()
	}

	sd, _, err := e.ctx.DereferenceStreamDict(o)
	if err != nil || sd == nil {
		return nil
	}
	if n, found := sd.Find("Subtype"); !found {
		return nil
	} else if name, ok := n.(types.Name); !ok || name.Value() != "Form" {
		return nil
	}

	if objNr > 0 {
		if e.visiting[objNr] {
			return nil
		}
		e.visiting[objNr] = true
		defer delete(e.visiting, objNr)
	}

	if err := e.ctx.DecodeStream(sd); err != nil {
		return nil
	}

	// Form matrix, defaults to identity.
	m := identityAffine()
	if o, found := sd.Find("Matrix"); found {
		if arr, ok := o.(types.Array); ok && len(arr) == 6 {
			vals := make([]float64, 6)
			for i := 0; i < 6; i++ {
				if v, err := e.ctx.DereferenceNumber(arr[i]); err == nil {
					vals[i] = v
				}
			}
			m = affine{a: vals[0], b: vals[1], c: vals[2], d: vals[3], e: vals[4], f: vals[5]}
		}
	}

	resources := types.Dict{}
	if o, found := sd.Find("Resources"); found && o != nil {
		if d, err := e.ctx.DereferenceDict(o); err == nil {
			resources = d
		}
	}

	return e.extract(c, sd.Content, resources, base.multiply(m), depth+1)
}

// fontResources resolves the font sub dictionary of resources into textFonts.
func (e *extractor) fontResources(c context.Context, resources types.Dict) (map[string]*textFont, error) {
	mm := map[string]*textFont{}
	o, found := resources.Find("Font")
	if !found || o == nil {
		return mm, nil
	}
	d, err := e.ctx.DereferenceDict(o)
	if err != nil {
		return nil, fmt.Errorf("font resources: %w", err)
	}
	for name, v := range d {
		f, err := e.loadFont(c, v)
		if err != nil {
			if log.ParseEnabled() {
				log.Parse.Printf("extractText: font %s: %v\n", name, err)
			}
			continue
		}
		if f != nil {
			mm[name] = f
		}
	}
	return mm, nil
}

// xObjectResources resolves the XObject sub dictionary of resources.
func (e *extractor) xObjectResources(resources types.Dict) (map[string]types.Object, error) {
	mm := map[string]types.Object{}
	o, found := resources.Find("XObject")
	if !found || o == nil {
		return mm, nil
	}
	d, err := e.ctx.DereferenceDict(o)
	if err != nil {
		return nil, fmt.Errorf("xobject resources: %w", err)
	}
	for name, v := range d {
		mm[name] = v
	}
	return mm, nil
}

// loadFont builds a textFont for a font dictionary object.
func (e *extractor) loadFont(c context.Context, o types.Object) (*textFont, error) {
	objNr := 0
	if ir, ok := o.(types.IndirectRef); ok {
		objNr = ir.ObjectNumber.Value()
	}
	if objNr > 0 {
		if f, ok := e.fonts[objNr]; ok {
			return f, nil
		}
	}

	d, err := e.ctx.DereferenceDict(o)
	if err != nil {
		return nil, err
	}

	f := &textFont{subtype: "unknown", codeLen: 1}

	if st := d.Subtype(); st != nil {
		f.subtype = *st
	}
	if bf, found := d.Find("BaseFont"); found {
		if n, ok := bf.(types.Name); ok {
			f.baseFont = n.Value()
		}
	}

	// ToUnicode overrides all encodings.
	if o, found := d.Find("ToUnicode"); found && o != nil {
		sd, _, err := e.ctx.DereferenceStreamDict(o)
		if err == nil && sd != nil {
			if derr := e.ctx.DecodeStream(sd); derr == nil && len(sd.Content) > 0 {
				cu, perr := parseToUnicodeCMap(sd.Content)
				if perr != nil {
					if dbg {
						fmt.Fprintf(os.Stderr, "extractText: font %s ToUnicode parse error: %v\n", f.baseFont, perr)
					}
				} else {
					f.cmap = cu
				}
			} else if dbg {
				fmt.Fprintf(os.Stderr, "extractText: font %s ToUnicode decode error: %v len=%d\n", f.baseFont, derr, len(sd.Content))
			}
		} else if dbg {
			fmt.Fprintf(os.Stderr, "extractText: font %s ToUnicode deref error: %v\n", f.baseFont, err)
		}
	}

	switch f.subtype {
	case "Type0":
		if err := e.loadType0Font(c, d, f); err != nil {
			return nil, err
		}
	case "Type1", "TrueType", "MMType1", "Type3":
		if err := e.loadSimpleFont(d, f); err != nil {
			return nil, err
		}
	default:
		if log.ParseEnabled() {
			log.Parse.Printf("extractText: unsupported font subtype %q\n", f.subtype)
		}
	}

	if objNr > 0 {
		e.fonts[objNr] = f
	}

	return f, nil
}

// loadType0Font initializes f from a Type0 font dictionary.
func (e *extractor) loadType0Font(c context.Context, d types.Dict, f *textFont) error {
	f.codeLen = 2
	f.cidWidths = map[uint32]float64{}
	f.cidDefaultWidth = 1000

	if enc, found := d.Find("Encoding"); found {
		if n, ok := enc.(types.Name); ok {
			name := n.Value()
			f.vertical = strings.HasSuffix(name, "-V")
			if strings.Contains(name, "UCS2") {
				f.ucs2 = true
			}
			if f.cmap == nil {
				f.legacy = newLegacyCodeDecoder(name)
			}
		}
	}

	o, found := d.Find("DescendantFonts")
	if !found || o == nil {
		return nil
	}
	arr, err := e.ctx.DereferenceArray(o)
	if err != nil || len(arr) == 0 {
		return nil
	}
	dd, err := e.ctx.DereferenceDict(arr[0])
	if err != nil {
		return nil
	}

	// Record the Adobe character collection for the CID fallback.
	if o, found := dd.Find("CIDSystemInfo"); found {
		if csi, err := e.ctx.DereferenceDict(o); err == nil {
			if o, found := csi.Find("Ordering"); found {
				if s, err := e.ctx.DereferenceText(o); err == nil && s != "" {
					f.cidOrdering = strings.TrimSuffix(s, "-0")
				}
			}
		}
	}

	if v, found, err := e.ctx.DereferenceIntegerEntry(dd, "DW"); err == nil && found {
		f.cidDefaultWidth = float64(*v)
	}

	if o, found := dd.Find("W"); found && o != nil {
		if err := e.parseCIDWidths(o, f); err != nil {
			if log.ParseEnabled() {
				log.Parse.Printf("extractText: W array: %v\n", err)
			}
		}
	}

	// Fallback: decode via the character map of an embedded TrueType font program.
	if f.cmap == nil && !f.ucs2 {
		if st := dictNameValue(dd, "Subtype"); st == "CIDFontType2" || st == "" {
			if u := e.embeddedTrueTypeUnicode(c, dd); u != nil {
				f.ttfUnicode = u
			}
		}
	}

	return nil
}

// embeddedTrueTypeUnicode extracts a glyph index to Unicode mapping from the
// FontFile2 program of a descendant CIDFont dict.
func (e *extractor) embeddedTrueTypeUnicode(c context.Context, dd types.Dict) map[uint32]rune {
	o, found := dd.Find("FontDescriptor")
	if !found || o == nil {
		return nil
	}
	fd, err := e.ctx.DereferenceDict(o)
	if err != nil || fd == nil {
		return nil
	}
	o, found = fd.Find("FontFile2")
	if !found || o == nil {
		return nil
	}
	sd, _, err := e.ctx.DereferenceStreamDict(o)
	if err != nil || sd == nil {
		return nil
	}
	if err := e.ctx.DecodeStream(sd); err != nil {
		return nil
	}
	if err := contextutil.Check(c); err != nil {
		return nil
	}

	ttf, err := font.ParseTTF(c, sd.Content)
	if err != nil {
		if dbg {
			fmt.Fprintf(os.Stderr, "extractText: font %s embedded TrueType: %v\n", dictNameValue(dd, "BaseFont")+
				"", err)
		}
		return nil
	}
	if len(ttf.ToUnicode) == 0 {
		return nil
	}

	u := make(map[uint32]rune, len(ttf.ToUnicode))
	for gid, code := range ttf.ToUnicode {
		u[uint32(gid)] = rune(code)
	}
	return u
}

func dictNameValue(d types.Dict, key string) string {
	if o, found := d.Find(key); found {
		if n, ok := o.(types.Name); ok {
			return n.Value()
		}
	}
	return ""
}

// parseCIDWidths parses the CIDFont W array into f.cidWidths.
func (e *extractor) parseCIDWidths(o types.Object, f *textFont) error {
	arr, err := e.ctx.DereferenceArray(o)
	if err != nil {
		return err
	}

	i := 0
	for i < len(arr) {
		c, err := e.ctx.DereferenceNumber(arr[i])
		if err != nil {
			return err
		}
		if i+1 >= len(arr) {
			return nil
		}
		if _, ok := arr[i+1].(types.Array); ok {
			wArr, err := e.ctx.DereferenceArray(arr[i+1])
			if err != nil {
				return err
			}
			for j, wo := range wArr {
				if w, err := e.ctx.DereferenceNumber(wo); err == nil {
					f.cidWidths[uint32(c)+uint32(j)] = w
				}
			}
			i += 2
			continue
		}

		if i+2 >= len(arr) {
			return nil
		}
		c2, err := e.ctx.DereferenceNumber(arr[i+1])
		if err != nil {
			return err
		}
		if wArr, ok := arr[i+2].(types.Array); ok {
			ws, err := e.ctx.DereferenceArray(wArr)
			if err != nil {
				return err
			}
			for j, wo := range ws {
				if wv, err := e.ctx.DereferenceNumber(wo); err == nil {
					f.cidWidths[uint32(c)+uint32(j)] = wv
				}
			}
		} else {
			w, err := e.ctx.DereferenceNumber(arr[i+2])
			if err != nil {
				return err
			}
			for cc := uint32(c); cc <= uint32(c2); cc++ {
				f.cidWidths[cc] = w
			}
		}
		i += 3
	}

	return nil
}

// loadSimpleFont initializes f from a simple font dictionary.
func (e *extractor) loadSimpleFont(d types.Dict, f *textFont) error {
	f.codeLen = 1

	enc := stdEncoding
	if strings.Contains(f.baseFont, "Symbol") || strings.Contains(f.baseFont, "Dingbats") {
		// Rely on ToUnicode or Differences only.
		enc = nil
	}

	if o, found := d.Find("Encoding"); found && o != nil {
		if encName, err := e.ctx.DereferenceName(o, model.V10, nil); err == nil {
			enc = baseEncodingForName(encName.Value(), enc)
		} else if ed, derr := e.ctx.DereferenceDict(o); derr == nil {
			if be, found, err := e.ctx.DereferenceNameEntry(ed, "BaseEncoding"); err == nil && found {
				enc = baseEncodingForName(be.Value(), enc)
			}
			if o, found := ed.Find("Differences"); found && o != nil {
				if diffs, err := e.parseDifferences(o); err == nil && len(diffs) > 0 {
					base := latin1Encoding
					if enc != nil {
						base = enc
					}
					var se simpleEncoding = *base
					se.applyDifferences(diffs)
					enc = &se
				}
			}
		}
	}

	if f.cmap == nil && enc == nil {
		enc = latin1Encoding
	}
	f.simple = enc

	// Widths.
	f.widths = [256]float64{}
	firstChar, foundFC, errFC := e.ctx.DereferenceIntegerEntry(d, "FirstChar")
	lastChar, foundLC, errLC := e.ctx.DereferenceIntegerEntry(d, "LastChar")
	if errFC != nil || errLC != nil || !foundFC || !foundLC {
		return nil
	}
	if o, found := d.Find("Widths"); found && o != nil {
		arr, err := e.ctx.DereferenceArray(o)
		if err != nil {
			return nil
		}
		for i, wo := range arr {
			code := int(*firstChar) + i
			if code > int(*lastChar) || code > 255 {
				break
			}
			if w, err := e.ctx.DereferenceNumber(wo); err == nil && w >= 0 {
				f.widths[code] = w
			}
		}
	}

	return nil
}

func baseEncodingForName(name string, prev *simpleEncoding) *simpleEncoding {
	switch name {
	case "WinAnsiEncoding":
		return winAnsiEncoding
	case "MacRomanEncoding":
		return macRomanEncoding
	case "StandardEncoding":
		return stdEncoding
	case "MacExpertEncoding":
		// Mostly non-Unicode names; rely on Differences.
		return nil
	}
	return prev
}

// parseDifferences parses an Encoding Differences array.
func (e *extractor) parseDifferences(o types.Object) (map[byte]string, error) {
	arr, err := e.ctx.DereferenceArray(o)
	if err != nil {
		return nil, err
	}

	diffs := map[byte]string{}
	code := 0
	for _, el := range arr {
		switch t := el.(type) {
		case types.Integer:
			code = int(t)
		case types.Name:
			if code >= 0 && code <= 255 {
				diffs[byte(code)] = t.Value()
			}
			code++
		}
	}

	return diffs, nil
}

// reconstructLines groups the recorded runs into lines following content
// stream order: a line change occurs whenever the baseline leaves the
// tolerance band of the current line. This matches the natural writing
// order of well-formed documents including column layouts that are
// serialized column by column.
// With SortByPosition the resulting lines are sorted top-down, left-right.
func (e *extractor) reconstructLines() []TextLine {
	tol := e.opts.LineTolerance

	type line struct {
		y    float64
		runs []textRun
	}

	var lines []*line
	var cur *line
	for _, r := range e.runs {
		if cur != nil && math.Abs(cur.y-r.y) <= tol {
			cur.runs = append(cur.runs, r)
			continue
		}
		cur = &line{y: r.y, runs: []textRun{r}}
		lines = append(lines, cur)
	}

	sortLines := e.opts.SortByPosition
	if sortLines {
		sort.SliceStable(lines, func(i, j int) bool {
			if lines[i].y != lines[j].y {
				return lines[i].y > lines[j].y
			}
			if len(lines[i].runs) == 0 || len(lines[j].runs) == 0 {
				return false
			}
			return lines[i].runs[0].x < lines[j].runs[0].x
		})
	}

	tl := make([]TextLine, 0, len(lines))
	for _, l := range lines {
		runs := l.runs
		if sortLines {
			runs = append([]textRun(nil), runs...)
			sort.SliceStable(runs, func(i, j int) bool { return runs[i].x < runs[j].x })
		}
		tl = append(tl, e.renderLine(runs))
	}

	return tl
}

// renderLine converts the runs of one line into a TextLine.
func (e *extractor) renderLine(runs []textRun) TextLine {
	var sb strings.Builder
	x0, xMax := math.MaxFloat64, -math.MaxFloat64
	y := 0.0

	for i := range runs {
		r := runs[i]
		if i > 0 {
			prev := runs[i-1]
			gap := r.x - prev.xMax
			if gap > math.Max(1, r.fontSize*e.opts.WordGapFactor) &&
				!strings.HasSuffix(prev.text, " ") && !strings.HasPrefix(r.text, " ") {
				sb.WriteByte(' ')
			}
		}
		sb.WriteString(r.text)
		if r.x < x0 {
			x0 = r.x
		}
		if r.xMax > xMax {
			xMax = r.xMax
		}
		y = r.y
	}

	text := sb.String()
	if e.opts.NormalizeWhitespace {
		text = strings.Join(strings.Fields(text), " ")
	}

	return TextLine{Text: text, X: x0, Y: y, XMax: xMax}
}

// cidOrderingUnicode resolves an Adobe CID of a predefined character
// collection (CIDSystemInfo Ordering) to Unicode using the embedded
// UCS2 collection tables.
func cidOrderingUnicode(ordering string, cid uint32) (rune, bool) {
	var t []cidUnicodeEntry
	switch ordering {
	case "GB1":
		t = GB1Table
	case "CNS1":
		t = CNS1Table
	default:
		return 0, false
	}
	i := sort.Search(len(t), func(i int) bool { return t[i].cid >= cid })
	if i < len(t) && t[i].cid == cid {
		return t[i].ucs, true
	}
	return 0, false
}

// operandNumber returns the numeric operand at position i.
func operandNumber(operands []contentOperand, i int) (float64, bool) {
	if i >= len(operands) {
		return 0, false
	}
	return operands[i].number()
}

// operandFloats returns the numeric operands at positions i and j.
func operandFloats(operands []contentOperand, i, j int) (float64, float64) {
	v1, _ := operandNumber(operands, i)
	v2, _ := operandNumber(operands, j)
	return v1, v2
}
