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
	"sort"
	"strconv"
	"strings"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/internal/fallbackfont"
	"github.com/huzpsb/pdfcpu2/pkg/filter"
	"github.com/huzpsb/pdfcpu2/pkg/log"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

// Text replacement: replace every occurrence of a find string by drawing the
// replacement with an embedded subset of the bundled vivo Sans while the
// original glyphs are surgically removed from the content streams.
//
// Design:
//   - a dedicated offset-tracking scanner reuses the extractor's font
//     decoding machinery (textFont/loadFont) and produces a per-character
//     intermediate state {char, x/y, device matrix, owning op, owning object}
//     for every glyph of a page, including glyphs inside Form XObjects;
//   - matches are located over reconstructed text lines (baseline clustering,
//     gap-induced spaces), each match marks its glyphs deleted;
//   - deletion is byte-level surgery on the owning content stream: whole show
//     ops are dropped when fully matched, partially matched ops are rebuilt
//     with TJ adjustments compensating the removed glyph advances so that
//     surviving glyphs keep their exact positions (the state side effects of
//     ' and " are replayed when the show part is dropped);
//   - the replacement is drawn per occurrence anchored at the visually
//     topmost-left matched glyph, replaying the original text matrix
//     (rotation/skew/scale), font size, Tc/Tw/Tz/Ts, render mode and fill
//     color; bold originals are approximated with fill+stroke;
//   - replacement glyphs are always drawn with one embedded subset of the
//     bundled vivo Sans (Type0/CIDFontType2, Identity-H, ToUnicode), created
//     once per document and registered per page;
//   - original fonts that lose all their glyphs are intentionally not
//     garbage collected (see roadmap).

const (
	// replaceFontName is the fixed PostScript name of the embedded vivo Sans
	// subset. Runs reuse and extend the existing font (codes of already
	// embedded runes stay stable, new runes are appended), so repeated runs
	// never accumulate fonts. Font garbage is left to `optimize`.
	replaceFontName = "pdfcpu2-replace"
	maxReplaceDepth = 24
)

// replFill is a snapshot of the graphics fill color.
type replFill struct {
	op     string // replay operator: "g", "rg", "k" or "sc" (with cs)
	csName string // non-empty for cs+sc replay ("/DeviceRGB", ...)
	comps  []float64
}

// replayable reports whether the color can be replayed.
func (f replFill) replayable() bool {
	return f.op == "g" || f.op == "rg" || f.op == "k" ||
		(f.op == "sc" && f.csName != "")
}

// content returns the operator sequence replaying this fill color.
func (f replFill) content() string {
	if !f.replayable() {
		return ""
	}
	var sb strings.Builder
	if f.csName != "" {
		fmt.Fprintf(&sb, "%s cs\n", f.csName)
	}
	for i, v := range f.comps {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(numFmt(v))
	}
	sb.WriteByte(' ')
	sb.WriteString(f.op)
	sb.WriteByte('\n')
	return sb.String()
}

// replState is the text rendering context snapshot at one show op.
type replState struct {
	baseFont    string
	bold        bool
	fontSize    float64
	charSpacing float64
	wordSpacing float64
	hScale      float64
	rise        float64
	renderMode  float64
	fill        replFill
}

// replGlyph is one decoded character instance of a show op.
type replGlyph struct {
	txt     string  // decoded Unicode (1+ runes)
	code    []byte  // raw font code bytes
	tx      float64 // text space advance to the next glyph
	m       affine  // device (page space) matrix of the glyph origin
	fs      float64 // font size (Tf operand)
	x, y    float64 // page space origin
	advPage float64 // advance projected on the page x axis
	seg     *replSeg
	del     bool
	twin    *replGlyph // overprint duplicate (fake bold): linked, deleted together
	isTwin  bool       // true for the second draw of an overprint pair
}

// horizontalish reports whether the glyph advances (mostly) along +x.
func (g *replGlyph) horizontalish() bool {
	return math.Abs(g.m.b) <= 0.7*math.Abs(g.m.a) && math.Abs(g.m.a) > 1e-6
}

// effSize returns the effective glyph size in page units (Tf size times the
// device scale). Some producers pair a huge Tf size with a tiny text matrix
// scale — basing clustering on the Tf size alone would merge neighbouring
// visual lines.
func (g *replGlyph) effSize() float64 {
	if s := math.Abs(g.m.a) * g.fs; s > 0.1 {
		return s
	}
	if s := math.Abs(g.m.d) * g.fs; s > 0.1 {
		return s
	}
	return g.fs
}

// replElem is one element of a show op: a string (glyphs) or a number.
type replElem struct {
	num    *float64
	glyphs []*replGlyph
}

// replSeg is one show op (Tj, ', ", TJ) with its byte span in the stream.
type replSeg struct {
	stream   *replStream
	start    int
	end      int
	op       string
	base     affine // transform of the owning stream into page space
	state    replState
	aw, ac   *float64 // " operands
	elements []replElem
}

// glyphs returns all glyphs of the segment in order.
func (s *replSeg) glyphs() []*replGlyph {
	var gg []*replGlyph
	for i := range s.elements {
		gg = append(gg, s.elements[i].glyphs...)
	}
	return gg
}

// replStream is one editable content stream (page content or Form XObject).
type replStream struct {
	objNr    int
	genNr    int
	sd       *types.StreamDict
	segments []*replSeg // from every scan (a shared form is scanned per placement)
	editable bool
}

// replDraw is one replacement occurrence to be drawn on a page.
type replDraw struct {
	pageNr int
	text   string
	m      affine    // anchor glyph device matrix
	state  replState // anchor text rendering context
}

// ReplaceTextPages replaces every occurrence of find by repl on the given
// pages (all pages if pageNrs is empty). It returns the number of replaced
// occurrences. Matching covers page content and Form XObjects; only
// (mostly) horizontal text lines are matched. The replacement is drawn with
// an embedded subset of the bundled vivo Sans, anchored at the visually
// topmost-left glyph of each occurrence and preserving the original text
// matrix, font size, spacings, rise, render mode and fill color.
func ReplaceTextPages(c context.Context, ctx *model.Context, find, repl string, pageNrs []int) (int, error) {
	if err := contextutil.Check(c); err != nil {
		return 0, err
	}
	if ctx == nil || ctx.XRefTable == nil {
		return 0, fmt.Errorf("replace: missing context")
	}
	if find == "" {
		return 0, fmt.Errorf("replace: empty find string")
	}
	if strings.ContainsAny(find, "\n\r") {
		return 0, fmt.Errorf("replace: multi-line find strings are not supported")
	}
	if len(stripSpaceRunes(find)) == 0 {
		return 0, fmt.Errorf("replace: find string contains only whitespace")
	}

	if len(pageNrs) == 0 {
		for i := 1; i <= ctx.PageCount; i++ {
			pageNrs = append(pageNrs, i)
		}
	}

	rs := &replScanner{
		ctx:     ctx,
		fe:      &extractor{ctx: ctx, fonts: map[int]*textFont{}, visiting: map[int]bool{}},
		streams: map[int]*replStream{},
		find:    find,
		repl:    repl,
	}

	var draws []replDraw
	occurrences := 0

	pageGlyphs := map[int][]*replGlyph{}
	for _, pageNr := range pageNrs {
		if err := contextutil.Check(c); err != nil {
			return occurrences, err
		}
		glyphs, err := rs.scanGlyphs(c, pageNr)
		if err != nil {
			return occurrences, fmt.Errorf("page %d: %w", pageNr, err)
		}
		pageGlyphs[pageNr] = glyphs
	}

	// link overprinted duplicates (fake bold) before matching
	rs.pairOverprints()

	for _, pageNr := range pageNrs {
		dd, n := rs.match(pageGlyphs[pageNr], pageNr)
		draws = append(draws, dd...)
		occurrences += n
	}

	// Delete matched glyphs from their owning streams.
	if err := rs.applyEdits(); err != nil {
		return occurrences, err
	}

	// Draw the replacements.
	if err := rs.drawReplacements(c, draws); err != nil {
		return occurrences, err
	}
	return occurrences, contextutil.Check(c)
}

// ---------------------------------------------------------------- scanning

// replScanner walks pages and produces glyph records and stream edits.
type replScanner struct {
	ctx     *model.Context
	fe      *extractor // font decoding reuse
	streams map[int]*replStream
	find    string
	repl    string
	fontRef *types.IndirectRef // reference of the (reused or new) replace font
}

// replGState is one graphics state stack entry.
type replGState struct {
	ctm  affine
	fill replFill
}

// replScanState is the state of one content stream scan.
type replScanState struct {
	ts        textState
	gs        replGState
	gsStack   []replGState
	csPending string // current fill colorspace name
	render    float64
	fontObj   types.Object // font object of the last Tf
}

// scanGlyphs scans all content streams of one page and returns its glyphs.
func (rs *replScanner) scanGlyphs(c context.Context, pageNr int) ([]*replGlyph, error) {
	pageDict, _, inhPAttrs, err := rs.ctx.PageDict(c, pageNr, false)
	if err != nil {
		return nil, fmt.Errorf("page dict: %w", err)
	}
	resources := pageResources(rs.ctx, pageDict, inhPAttrs)

	sds, err := rs.pageStreams(pageDict)
	if err != nil {
		return nil, err
	}

	rs.fe.visiting = map[int]bool{}

	var glyphs []*replGlyph
	for _, st := range sds {
		if err := contextutil.Check(c); err != nil {
			return nil, err
		}
		gg, err := rs.scanStream(c, st, resources, identityAffine(), 0)
		if err != nil {
			return nil, err
		}
		glyphs = append(glyphs, gg...)
	}
	return glyphs, nil
}

// pageStreams resolves the content stream dicts of a page. /Contents may be
// a direct stream reference, a direct array, an indirect reference to an
// array, or an array of indirect references.
func (rs *replScanner) pageStreams(pageDict types.Dict) ([]*replStream, error) {
	obj, found := pageDict.Find("Contents")
	if !found || obj == nil {
		return nil, nil
	}
	var refs []types.IndirectRef
	addRef := func(o types.Object) error {
		ir, ok := o.(types.IndirectRef)
		if !ok {
			return fmt.Errorf("content entry is no indirect reference")
		}
		refs = append(refs, ir)
		return nil
	}
	switch o := obj.(type) {
	case types.IndirectRef:
		d, err := rs.ctx.Dereference(o)
		if err != nil || d == nil {
			return nil, nil
		}
		switch dd := d.(type) {
		case types.Array:
			for _, el := range dd {
				if err := addRef(el); err != nil {
					return nil, err
				}
			}
		default:
			if err := addRef(o); err != nil {
				return nil, err
			}
		}
	case types.Array:
		for _, el := range o {
			if err := addRef(el); err != nil {
				return nil, err
			}
		}
	default:
		return nil, nil
	}

	var out []*replStream
	for _, ir := range refs {
		objNr := ir.ObjectNumber.Value()
		if st, found := rs.streams[objNr]; found {
			// content shared between pages: keep the (editable) record
			out = append(out, st)
			continue
		}
		sd, _, err := rs.ctx.XRefTable.DereferenceStreamDict(ir)
		if err != nil || sd == nil {
			if log.ParseEnabled() {
				log.Parse.Printf("replace: content obj#%d: %v\n", objNr, err)
			}
			continue
		}
		st := &replStream{objNr: objNr, genNr: ir.GenerationNumber.Value(), sd: sd, editable: true}
		if derr := rs.ctx.XRefTable.DecodeStream(sd); derr != nil {
			if derr == filter.ErrUnsupportedFilter {
				st.editable = false
			} else {
				return nil, fmt.Errorf("decode content obj#%d: %w", objNr, derr)
			}
		}
		rs.streams[objNr] = st
		out = append(out, st)
	}
	return out, nil
}

// streamFor registers (or returns) the editable stream record of an object.
func (rs *replScanner) streamFor(objNr, genNr int, sd *types.StreamDict, editable bool) *replStream {
	if objNr > 0 {
		if st, found := rs.streams[objNr]; found {
			return st
		}
	}
	st := &replStream{objNr: objNr, genNr: genNr, sd: sd, editable: editable}
	if objNr > 0 {
		rs.streams[objNr] = st
	}
	return st
}

// scanStream scans one content stream and returns the glyphs it produced.
func (rs *replScanner) scanStream(c context.Context, st *replStream, resources types.Dict,
	base affine, depth int) ([]*replGlyph, error) {

	if depth > maxReplaceDepth {
		return nil, nil
	}
	if !st.editable {
		return nil, nil
	}

	fontDicts, err := rs.fe.fontResources(c, resources)
	if err != nil {
		return nil, err
	}
	fontObjs := map[string]types.Object{}
	if o, found := resources.Find("Font"); found && o != nil {
		if d, err := rs.ctx.DereferenceDict(o); err == nil {
			for name, v := range d {
				fontObjs[name] = v
			}
		}
	}
	xobjs := map[string]types.Object{}
	if o, found := resources.Find("XObject"); found && o != nil {
		if d, err := rs.ctx.DereferenceDict(o); err == nil {
			for name, v := range d {
				xobjs[name] = v
			}
		}
	}

	bb := st.sd.Content
	if len(bb) == 0 {
		return nil, nil
	}

	state := &replScanState{}
	state.ts = newTextState()
	state.ts.reset()
	state.gs.ctm = identityAffine()

	sc := newContentScanner(bb)
	var operands []contentOperand
	opStart := 0

	var glyphs []*replGlyph

	for {
		if err := contextutil.Check(c); err != nil {
			return glyphs, err
		}
		tokStart := sc.pos
		op, err := sc.next()
		if err != nil {
			break // tolerate trailing garbage
		}
		if op.isZero() {
			break
		}
		if op.isOperand() {
			if len(operands) < 64 {
				operands = append(operands, op)
			}
			continue
		}

		spanEnd := sc.pos
		_ = tokStart
		if op.operator == "BI" {
			if err := sc.skipInlineImage(); err != nil {
				break
			}
			operands = operands[:0]
			opStart = sc.pos
			continue
		}

		glyphs, err = rs.applyOp(c, op.operator, operands, state,
			fontDicts, fontObjs, xobjs, st, base, depth, opStart, spanEnd, glyphs)
		if err != nil {
			return glyphs, err
		}

		operands = operands[:0]
		opStart = sc.pos
	}
	return glyphs, nil
}

// applyOp interprets one operator, updating state and recording segments.
func (rs *replScanner) applyOp(c context.Context, operator string, operands []contentOperand,
	state *replScanState, fontDicts map[string]*textFont, fontObjs map[string]types.Object,
	xobjs map[string]types.Object, st *replStream, base affine, depth int,
	start, end int, glyphs []*replGlyph) ([]*replGlyph, error) {

	ts := &state.ts
	gs := &state.gs

	switch operator {
	case "q":
		state.gsStack = append(state.gsStack, *gs)
	case "Q":
		if n := len(state.gsStack); n > 0 {
			*gs = state.gsStack[n-1]
			state.gsStack = state.gsStack[:n-1]
		}
	case "cm":
		if len(operands) >= 6 {
			var nums [6]float64
			ok := true
			for i := 0; i < 6; i++ {
				if v, isNum := operands[len(operands)-6+i].number(); isNum {
					nums[i] = v
				} else {
					ok = false
					break
				}
			}
			if ok {
				m := affine{a: nums[0], b: nums[1], c: nums[2], d: nums[3], e: nums[4], f: nums[5]}
				gs.ctm = m.multiply(gs.ctm)
			}
		}
	case "g", "rg", "k":
		f := replFill{op: operator}
		for i := range operands {
			if v, ok := operands[i].number(); ok {
				f.comps = append(f.comps, v)
			}
		}
		gs.fill = f
		state.csPending = ""
	case "cs":
		if len(operands) > 0 && operands[0].name != "" {
			state.csPending = operands[0].name
		}
	case "sc", "scn":
		var comps []float64
		for i := range operands {
			if v, ok := operands[i].number(); ok {
				comps = append(comps, v)
			}
		}
		switch state.csPending {
		case "", "/DeviceGray", "DeviceGray":
			if len(comps) == 1 {
				gs.fill = replFill{op: "g", comps: comps}
			}
		case "/DeviceRGB", "DeviceRGB":
			if len(comps) == 3 {
				gs.fill = replFill{op: "rg", comps: comps}
			}
		case "/DeviceCMYK", "DeviceCMYK":
			if len(comps) == 4 {
				gs.fill = replFill{op: "k", comps: comps}
			}
		default:
			// Pattern or custom color space: replay via cs+scn when numeric.
			if len(comps) > 0 {
				gs.fill = replFill{op: "sc", csName: state.csPending, comps: comps}
			}
		}
	case "BT":
		ts.reset()
	case "Tf":
		if len(operands) >= 2 {
			state.fontObj = fontObjs[operands[0].name]
			if f, ok := fontDicts[operands[0].name]; ok {
				ts.font = f
			}
			if sz, ok := operands[1].number(); ok {
				ts.fontSize = sz
			}
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
	case "Tr":
		if v, ok := operandNumber(operands, 0); ok {
			state.render = v
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
		return rs.showOp(operator, operands, state, st, base, start, end, glyphs), nil
	case "TJ":
		return rs.showTJ(operands, state, st, base, start, end, glyphs), nil
	case "Do":
		if len(operands) == 0 || operands[0].name == "" {
			return glyphs, nil
		}
		o, found := xobjs[operands[0].name]
		if !found || o == nil {
			return glyphs, nil
		}
		return rs.doForm(c, o, state, base, depth, glyphs)
	}
	return glyphs, nil
}

// stateSnapshot captures the text rendering context at a show op.
func (rs *replScanner) stateSnapshot(state *replScanState) replState {
	s := replState{
		fontSize:    state.ts.fontSize,
		charSpacing: state.ts.charSpacing,
		wordSpacing: state.ts.wordSpacing,
		hScale:      state.ts.hScale,
		rise:        state.ts.rise,
		renderMode:  state.render,
		fill:        state.gs.fill,
	}
	if state.ts.font != nil {
		s.baseFont = state.ts.font.baseFont
	}
	s.bold = fontIsBold(rs.ctx, state.fontObj, s.baseFont)
	return s
}

// addString decodes one show string into glyphs at the current text position.
func (rs *replScanner) addString(state *replScanState, bb []byte, seg *replSeg) []*replGlyph {
	ts := &state.ts
	f := ts.font
	if f == nil || len(bb) == 0 {
		return nil
	}
	l := f.effectiveCodeLen()
	var gg []*replGlyph
	for i := 0; i < len(bb); {
		n := l
		if i+n > len(bb) {
			n = len(bb) - i
		}
		code := bb[i : i+n]
		m := ts.tm.multiply(state.gs.ctm).multiply(seg.base)
		codeV := codeOfBytes(code)
		tx, ty := ts.advance(f.w0(codeV), f.isSpaceCode(codeV))
		g := &replGlyph{
			txt:  f.decode(code),
			code: append([]byte{}, code...),
			tx:   tx,
			m:    m,
			fs:   ts.fontSize,
			x:    m.e,
			y:    m.f,
			seg:  seg,
		}
		g.advPage = tx*m.a + ty*m.c
		gg = append(gg, g)
		ts.applyTranslation(tx, ty)
		i += n
	}
	return gg
}

// showOp handles Tj, ' and ".
func (rs *replScanner) showOp(operator string, operands []contentOperand, state *replScanState,
	st *replStream, base affine, start, end int, glyphs []*replGlyph) []*replGlyph {

	ts := &state.ts

	seg := &replSeg{
		stream: st,
		start:  start,
		end:    end,
		op:     operator,
		base:   base,
	}

	var bb []byte
	if len(operands) > 0 {
		bb = operands[len(operands)-1].str
	}

	if operator == "'" {
		ts.tlm = translation(0, -ts.leading).multiply(ts.tlm)
		ts.tm = ts.tlm
	}
	if operator == "\"" && len(operands) >= 3 {
		if v, ok := operandNumber(operands, 0); ok {
			ts.wordSpacing = v
			aw := v
			seg.aw = &aw
		}
		if v, ok := operandNumber(operands, 1); ok {
			ts.charSpacing = v
			ac := v
			seg.ac = &ac
		}
		ts.tlm = translation(0, -ts.leading).multiply(ts.tlm)
		ts.tm = ts.tlm
	}

	seg.state = rs.stateSnapshot(state)

	gg := rs.addString(state, bb, seg)
	if len(gg) == 0 {
		return glyphs
	}
	seg.elements = []replElem{{glyphs: gg}}
	st.segments = append(st.segments, seg)
	return append(glyphs, gg...)
}

// showTJ handles TJ.
func (rs *replScanner) showTJ(operands []contentOperand, state *replScanState,
	st *replStream, base affine, start, end int, glyphs []*replGlyph) []*replGlyph {

	ts := &state.ts
	if len(operands) == 0 || operands[0].arr == nil {
		return glyphs
	}

	seg := &replSeg{
		stream: st,
		start:  start,
		end:    end,
		op:     "TJ",
		base:   base,
		state:  rs.stateSnapshot(state),
	}

	var all []*replGlyph
	for i := range operands[0].arr {
		el := operands[0].arr[i]
		if el.str != nil {
			gg := rs.addString(state, el.str, seg)
			seg.elements = append(seg.elements, replElem{glyphs: gg})
			all = append(all, gg...)
			continue
		}
		if v, ok := el.number(); ok {
			vv := v
			seg.elements = append(seg.elements, replElem{num: &vv})
			tx := -v / 1000 * ts.fontSize * ts.hScale
			ts.applyTranslation(tx, 0)
		}
	}
	if len(all) == 0 {
		return glyphs
	}
	st.segments = append(st.segments, seg)
	return append(glyphs, all...)
}

// doForm descends into a Form XObject drawn on the current graphics state.
func (rs *replScanner) doForm(c context.Context, o types.Object, state *replScanState,
	base affine, depth int, glyphs []*replGlyph) ([]*replGlyph, error) {

	objNr := 0
	genNr := 0
	if ir, ok := o.(types.IndirectRef); ok {
		objNr = ir.ObjectNumber.Value()
		genNr = ir.GenerationNumber.Value()
	}
	sd, _, err := rs.ctx.XRefTable.DereferenceStreamDict(o)
	if err != nil || sd == nil {
		return glyphs, nil
	}
	if n, found := sd.Find("Subtype"); !found {
		return glyphs, nil
	} else if name, ok := n.(types.Name); !ok || name.Value() != "Form" {
		return glyphs, nil
	}
	if objNr > 0 {
		if rs.fe.visiting[objNr] {
			return glyphs, nil
		}
		rs.fe.visiting[objNr] = true
		defer delete(rs.fe.visiting, objNr)
	}

	m := identityAffine()
	if o, found := sd.Find("Matrix"); found {
		if arr, ok := o.(types.Array); ok && len(arr) == 6 {
			vals := make([]float64, 6)
			for i := 0; i < 6; i++ {
				if v, err := rs.ctx.DereferenceNumber(arr[i]); err == nil {
					vals[i] = v
				}
			}
			m = affine{a: vals[0], b: vals[1], c: vals[2], d: vals[3], e: vals[4], f: vals[5]}
		}
	}
	inner := m.multiply(state.gs.ctm).multiply(base)

	resources := types.Dict{}
	if o, found := sd.Find("Resources"); found && o != nil {
		if d, err := rs.ctx.DereferenceDict(o); err == nil {
			resources = d
		}
	}

	derr := rs.ctx.XRefTable.DecodeStream(sd)
	if derr != nil && derr != filter.ErrUnsupportedFilter {
		return glyphs, nil
	}
	st := rs.streamFor(objNr, genNr, sd, derr == nil)

	gg, err := rs.scanStream(c, st, resources, inner, depth+1)
	if err != nil {
		return glyphs, err
	}
	return append(glyphs, gg...), nil
}

// fontIsBold detects bold-ish fonts by name or descriptor weight.
func fontIsBold(ctx *model.Context, o types.Object, baseFont string) bool {
	low := strings.ToLower(baseFont)
	for _, kw := range []string{"bold", "black", "heavy", "semibold", "-bd", ".bd"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	if o == nil {
		return false
	}
	d, err := ctx.DereferenceDict(o)
	if err != nil || d == nil {
		return false
	}
	if fd, found := d.Find("FontDescriptor"); found && fd != nil {
		if dd, err := ctx.DereferenceDict(fd); err == nil && dd != nil {
			if w, found := dd.Find("FontWeight"); found {
				if n, err := ctx.DereferenceNumber(w); err == nil && n >= 600 {
					return true
				}
			}
		}
	}
	return false
}

// pairOverprints links consecutive identical show ops (same text at the same
// position): producers draw text twice to fake boldness. Linked twins are
// excluded from matching and deleted together with their primary so one
// occurrence is replaced once but both draws disappear.
func (rs *replScanner) pairOverprints() {
	for _, st := range rs.streams {
		segs := st.segments
		for i := 1; i < len(segs); i++ {
			a, b := segs[i-1], segs[i]
			if b.end-a.start > 4096 { // keep pairing local in the stream
				continue
			}
			ga, gb := a.glyphs(), b.glyphs()
			if len(ga) == 0 || len(ga) != len(gb) {
				continue
			}
			same := true
			for j := range ga {
				if ga[j].txt != gb[j].txt ||
					math.Abs(ga[j].x-gb[j].x) > 0.5 ||
					math.Abs(ga[j].y-gb[j].y) > 0.5 {
					same = false
					break
				}
			}
			if !same {
				continue
			}
			for j := range ga {
				if ga[j].twin != nil || gb[j].isTwin {
					continue
				}
				ga[j].twin = gb[j]
				gb[j].twin = ga[j]
				gb[j].isTwin = true
			}
		}
	}
}

// ---------------------------------------------------------------- matching

// match finds find-string occurrences over the page glyphs, marks the
// matched glyphs deleted and returns the replacement draws and their count.
// Matching is whitespace-insensitive: spaces in the find string and spaces
// between the original glyphs (real or gap-induced) are ignored, so
// "医院" matches "医 院". Whitespace glyphs themselves carry no ink and are
// not deleted. The replacement text is drawn verbatim.
func (rs *replScanner) match(glyphs []*replGlyph, pageNr int) ([]replDraw, int) {
	var draws []replDraw
	count := 0

	needle := stripSpaceRunes(rs.find)
	if len(needle) == 0 {
		return nil, 0
	}

	for _, line := range clusterLines(glyphs) {
		cs, refs := compactLine(line)
		if len(cs) == 0 {
			continue
		}
		for i := 0; i+len(needle) <= len(cs); {
			if !runesEqual(cs[i:i+len(needle)], needle) {
				i++
				continue
			}
			seen := map[*replGlyph]bool{}
			var gg []*replGlyph
			overlaps := false
			for _, g := range refs[i : i+len(needle)] {
				if g.del {
					overlaps = true
					break
				}
				if !seen[g] {
					seen[g] = true
					gg = append(gg, g)
				}
			}
			if overlaps || len(gg) == 0 {
				i++
				continue
			}
			for _, g := range gg {
				g.del = true
				if g.twin != nil {
					g.twin.del = true
				}
			}
			count++
			if rs.repl != "" {
				if a := anchorGlyph(gg); a != nil {
					draws = append(draws, replDraw{
						pageNr: pageNr,
						text:   rs.repl,
						m:      a.m,
						state:  a.seg.state,
					})
				}
			}
			i += len(needle)
		}
	}
	return draws, count
}

// isSpaceRune reports whether r is a whitespace rune.
func isSpaceRune(r rune) bool {
	return r == ' ' || r == '\t' || r == '\u00A0' || r == '\u3000' ||
		r == '\u2002' || r == '\u2003' || r == '\u2009' || r == '\u200A'
}

// stripSpaceRunes removes all whitespace runes.
func stripSpaceRunes(s string) []rune {
	var out []rune
	for _, r := range s {
		if !isSpaceRune(r) {
			out = append(out, r)
		}
	}
	return out
}

func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// compactLine returns the line's text with all whitespace removed, plus
// parallel references to the glyph carrying each rune. Overprint twins are
// skipped: the primary draw represents the pair.
func compactLine(line []*replGlyph) ([]rune, []*replGlyph) {
	var cs []rune
	var refs []*replGlyph
	for _, g := range line {
		if g.isTwin {
			continue
		}
		for _, r := range g.txt {
			if isSpaceRune(r) {
				continue
			}
			cs = append(cs, r)
			refs = append(refs, g)
		}
	}
	return cs, refs
}

// anchorGlyph picks the visually topmost-left glyph of a match.
func anchorGlyph(gg []*replGlyph) *replGlyph {
	var best *replGlyph
	for _, g := range gg {
		if best == nil {
			best = g
			continue
		}
		if g.y > best.y+0.01 || (math.Abs(g.y-best.y) <= 0.01 && g.x < best.x) {
			best = g
		}
	}
	return best
}

// clusterLines groups glyphs into horizontal text lines. Clustering follows
// the content stream order (like the text extractor): consecutive draws with
// a close baseline join one line, which also tolerates slightly skewed
// baselines whose y drifts along the line. Each line is then ordered by x.
func clusterLines(glyphs []*replGlyph) [][]*replGlyph {
	var cand []*replGlyph
	for _, g := range glyphs {
		if g.horizontalish() && g.fs > 0.1 {
			cand = append(cand, g)
		}
	}

	var lines [][]*replGlyph
	var cur []*replGlyph
	flush := func() {
		if len(cur) > 0 {
			lines = append(lines, cur)
			cur = nil
		}
	}
	for _, g := range cand {
		if len(cur) == 0 {
			cur = []*replGlyph{g}
			continue
		}
		last := cur[len(cur)-1]
		tol := math.Max(1.0, 0.45*(g.effSize()+last.effSize())/2)
		if math.Abs(g.y-last.y) <= tol {
			cur = append(cur, g)
			continue
		}
		flush()
		cur = []*replGlyph{g}
	}
	flush()

	for _, l := range lines {
		sort.SliceStable(l, func(i, j int) bool { return l[i].x < l[j].x })
	}
	return lines
}

// ---------------------------------------------------------------- edits

type replEdit struct {
	start, end int
	text       string
}

// applyEdits merges the deletion marks of shared streams and rewrites the
// affected content streams.
func (rs *replScanner) applyEdits() error {
	for _, st := range rs.streams {
		if !st.editable {
			continue
		}
		edits, err := streamEdits(st)
		if err != nil {
			return fmt.Errorf("obj#%d: %w", st.objNr, err)
		}
		if len(edits) == 0 {
			continue
		}
		bb := st.sd.Content
		sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
		var out []byte
		pos := 0
		for _, e := range edits {
			if e.start < pos || e.start > len(bb) || e.end > len(bb) || e.end < e.start {
				return fmt.Errorf("obj#%d: overlapping edits", st.objNr)
			}
			out = append(out, bb[pos:e.start]...)
			out = append(out, []byte(e.text)...)
			pos = e.end
		}
		out = append(out, bb[pos:]...)
		st.sd.Content = out
		if err := st.sd.Encode(); err != nil {
			return fmt.Errorf("obj#%d: encode: %w", st.objNr, err)
		}
		// write the edited stream back into the xref table entry
		if entry, found := rs.ctx.XRefTable.FindTableEntry(st.objNr, st.genNr); found {
			entry.Object = *st.sd
		} else {
			return fmt.Errorf("obj#%d: xref entry lost", st.objNr)
		}
	}
	return nil
}

// streamEdits computes the byte edits for one stream.
func streamEdits(st *replStream) ([]replEdit, error) {
	// merge segments of shared streams scanned per placement by byte span
	byStart := map[int][]*replSeg{}
	for _, seg := range st.segments {
		if !segHasDeletion(seg) {
			continue
		}
		byStart[seg.start] = append(byStart[seg.start], seg)
	}
	if len(byStart) == 0 {
		return nil, nil
	}

	var edits []replEdit
	for _, group := range byStart {
		seg := group[0]
		for _, other := range group[1:] {
			mergeDeletions(seg, other)
		}
		text, emit := rebuildSeg(seg)
		if emit {
			edits = append(edits, replEdit{start: seg.start, end: seg.end, text: text})
		}
	}
	return edits, nil
}

// segHasDeletion reports whether any glyph of the segment is deleted.
func segHasDeletion(seg *replSeg) bool {
	for _, g := range seg.glyphs() {
		if g.del {
			return true
		}
	}
	return false
}

// mergeDeletions ORs the deletion flags of other onto seg (identical layout
// because both scans ran over the same bytes with the same scanner).
func mergeDeletions(seg, other *replSeg) {
	if len(seg.elements) != len(other.elements) {
		return // safety: layout mismatch, keep seg as-is
	}
	for i := range seg.elements {
		a := seg.elements[i].glyphs
		b := other.elements[i].glyphs
		if len(a) != len(b) {
			continue
		}
		for j := range a {
			if b[j].del {
				a[j].del = true
			}
		}
	}
}

// rebuildSeg regenerates a show op with its deleted glyphs removed.
// emit reports whether an edit has to be applied; text is the replacement
// ops ("" deletes the span entirely). Surviving glyphs keep their positions
// through TJ adjustments compensating the deleted advances.
func rebuildSeg(seg *replSeg) (text string, emit bool) {
	// replay the state side effects of ' and "
	var head []string
	if seg.op == "\"" {
		if seg.aw != nil {
			head = append(head, fmt.Sprintf("%s Tw", numFmt(*seg.aw)))
		}
		if seg.ac != nil {
			head = append(head, fmt.Sprintf("%s Tc", numFmt(*seg.ac)))
		}
	}
	if seg.op == "'" || seg.op == "\"" {
		head = append(head, "T*")
	}

	st := seg.state
	hz := st.hScale
	if hz == 0 {
		hz = 1
	}
	compDiv := st.fontSize * hz
	compensable := math.Abs(compDiv) > 1e-6

	var showTokens []string
	var delTx float64
	var numAcc float64

	// flush emits one TJ adjustment covering the accumulated original
	// numbers plus the removed glyph advances, positioning the next
	// surviving string exactly where it was.
	flush := func() {
		v := numAcc
		if compensable && delTx != 0 {
			v += -delTx * 1000 / compDiv
		}
		if v != 0 {
			showTokens = append(showTokens, numFmt(v))
		}
		numAcc, delTx = 0, 0
	}

	for _, el := range seg.elements {
		if el.num != nil {
			numAcc += *el.num
			continue
		}
		var keep []byte
		for _, g := range el.glyphs {
			if g.del {
				delTx += g.tx
				continue
			}
			keep = append(keep, g.code...)
		}
		if len(keep) == 0 {
			continue
		}
		flush()
		showTokens = append(showTokens, hexString(keep))
	}
	// trailing numbers and advances position nothing — dropped

	switch {
	case len(showTokens) == 0:
		// whole op deleted: only the state side effects remain
		if len(head) == 0 {
			return "", true
		}
		return strings.Join(head, "\n") + "\n", true

	case len(showTokens) == 1 && numAcc == 0 && delTx == 0 && seg.op != "'" && seg.op != "\"":
		ops := append(head, showTokens[0]+" Tj")
		return strings.Join(ops, "\n") + "\n", true

	default:
		body := "[" + strings.Join(showTokens, " ") + "] TJ"
		ops := append(head, body)
		return strings.Join(ops, "\n") + "\n", true
	}
}

// hexString renders raw code bytes as a PDF hex string.
func hexString(bb []byte) string {
	const hexDigits = "0123456789ABCDEF"
	out := make([]byte, 0, 2*len(bb)+2)
	out = append(out, '<')
	for _, b := range bb {
		out = append(out, hexDigits[b>>4], hexDigits[b&0x0F])
	}
	out = append(out, '>')
	return string(out)
}

// ---------------------------------------------------------------- drawing

// drawReplacements builds (or reuses) the replacement font subset and appends
// the replacement content per page. An existing replace font is extended in
// place: codes of already embedded runes stay stable, new runes are appended
// at the end, so chained runs keep exactly one font.
func (rs *replScanner) drawReplacements(c context.Context, draws []replDraw) error {
	needed := map[rune]bool{}
	for _, d := range draws {
		for _, r := range d.text {
			needed[r] = true
		}
	}
	if len(needed) == 0 {
		return nil
	}

	fontDict, existing, found := rs.findReplaceFont()

	// merge: existing runes keep their codes, new ones are appended sorted
	var runes []rune
	have := map[rune]bool{}
	for _, r := range existing {
		runes = append(runes, r)
		have[r] = true
	}
	var fresh []rune
	for r := range needed {
		if !have[r] {
			fresh = append(fresh, r)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i] < fresh[j] })
	runes = append(runes, fresh...)

	sub, err := buildVivoSubset(fallbackfont.Bytes(), runes)
	if err != nil {
		return fmt.Errorf("vivo sans subset: %w", err)
	}
	if len(sub.missing) > 0 && log.InfoEnabled() {
		log.Info.Printf("replace: vivo sans misses %q — .notdef is used\n", string(sub.missing))
	}

	if found {
		if err := rs.updateReplaceFont(fontDict, sub); err != nil {
			return fmt.Errorf("update replacement font: %w", err)
		}
	} else {
		var fontNr int
		fontNr, err = rs.createReplaceFont(sub)
		if err != nil {
			return fmt.Errorf("embed replacement font: %w", err)
		}
		fontRef := *types.NewIndirectRef(fontNr, 0)
		rs.fontRef = &fontRef
	}
	if rs.fontRef == nil {
		return fmt.Errorf("replace: font reference lost")
	}

	byPage := map[int][]replDraw{}
	for _, d := range draws {
		byPage[d.pageNr] = append(byPage[d.pageNr], d)
	}

	fontRef := *rs.fontRef
	for pageNr, dd := range byPage {
		if err := contextutil.Check(c); err != nil {
			return err
		}
		pageDict, _, inhPAttrs, err := rs.ctx.PageDict(c, pageNr, true)
		if err != nil {
			return fmt.Errorf("page %d: page dict: %w", pageNr, err)
		}
		fontRes, err := replaceFontResources(rs.ctx, pageDict, inhPAttrs)
		if err != nil {
			return fmt.Errorf("page %d: font resources: %w", pageNr, err)
		}
		name := replaceFontName
		for {
			if _, taken := fontRes.Find(name); !taken {
				break
			}
			name += "x"
		}
		fontRes.Insert(name, fontRef)

		var b strings.Builder
		for _, d := range dd {
			b.WriteString(replaceDrawOps(name, sub, d))
		}
		if err := rs.ctx.XRefTable.AppendContent(pageDict, []byte(b.String())); err != nil {
			return fmt.Errorf("page %d: append content: %w", pageNr, err)
		}
	}
	return nil
}

// findReplaceFont locates a replace font embedded by an earlier run (keyed by
// the fixed BaseFont) and returns its resolved dict plus its runes in code
// order. Optimizer-created reference chains are followed.
func (rs *replScanner) findReplaceFont() (types.Dict, []rune, bool) {
	for pageNr := 1; pageNr <= rs.ctx.PageCount; pageNr++ {
		pageDict, _, inhPAttrs, err := rs.ctx.PageDict(context.Background(), pageNr, false)
		if err != nil {
			continue
		}
		res := pageDict
		if o, found := pageDict.Find("Resources"); found && o != nil {
			if d, err := rs.ctx.DereferenceDict(o); err == nil && d != nil {
				res = d
			}
		} else if inhPAttrs != nil && inhPAttrs.Resources != nil {
			res = inhPAttrs.Resources
		}
		o, found := res.Find("Font")
		if !found || o == nil {
			continue
		}
		fd, err := rs.ctx.DereferenceDict(o)
		if err != nil || fd == nil {
			continue
		}
		for _, v := range fd {
			fontIr, isRef := v.(types.IndirectRef)
			d, ok := rs.resolveFontDict(v)
			if !ok {
				continue
			}
			if bf, found := d.Find("BaseFont"); !found {
				continue
			} else if n, ok := bf.(types.Name); !ok || n.Value() != replaceFontName {
				continue
			}
			tu, found := d.Find("ToUnicode")
			if !found {
				continue
			}
			sd, _, err := rs.ctx.XRefTable.DereferenceStreamDict(tu)
			if err != nil || sd == nil {
				continue
			}
			if err := rs.ctx.XRefTable.DecodeStream(sd); err != nil {
				continue
			}
			cu, err := parseToUnicodeCMap(sd.Content)
			if err != nil || cu == nil {
				continue
			}
			// codes must be consecutive 1..n with single-rune mappings
			existing := make([]rune, 0, len(cu.mappings))
			valid := true
			for i := 0; i < len(cu.mappings); i++ {
				s, found := cu.mappings[uint32(i+1)]
				if !found || len([]rune(s)) != 1 {
					valid = false
					break
				}
				existing = append(existing, []rune(s)[0])
			}
			if !valid {
				continue
			}
			if isRef {
				rs.fontRef = &fontIr
			}
			return d, existing, true
		}
	}
	return nil, nil, false
}

// resolveFontDict follows (optimizer-created) reference chains to a dict.
func (rs *replScanner) resolveFontDict(o types.Object) (types.Dict, bool) {
	for i := 0; i < 8; i++ {
		switch t := o.(type) {
		case types.IndirectRef:
			if d, err := rs.ctx.DereferenceDict(t); err == nil && d != nil {
				return d, true
			}
			entry, found := rs.ctx.XRefTable.FindTableEntry(t.ObjectNumber.Value(), t.GenerationNumber.Value())
			if !found || entry.Object == nil {
				return nil, false
			}
			o = entry.Object
		case *types.IndirectRef:
			o = *t
		case types.Dict:
			return t, true
		default:
			return nil, false
		}
	}
	return nil, false
}

// replaceStreamObj overwrites the content of a stream object in place.
func (rs *replScanner) replaceStreamObj(ir types.IndirectRef, content []byte, len1 bool) error {
	entry, found := rs.ctx.XRefTable.FindTableEntry(ir.ObjectNumber.Value(), ir.GenerationNumber.Value())
	if !found {
		return fmt.Errorf("obj#%d: entry lost", ir.ObjectNumber.Value())
	}
	sd, ok := entry.Object.(types.StreamDict)
	if !ok {
		return fmt.Errorf("obj#%d: not a stream", ir.ObjectNumber.Value())
	}
	sd.Content = content
	if len1 {
		sd.Dict.InsertInt("Length1", len(content))
	}
	if err := sd.Encode(); err != nil {
		return err
	}
	entry.Object = sd
	return nil
}

// updateReplaceFont rewrites the embedded font program, the W array, the
// ToUnicode CMap and the descriptor metrics of an existing replace font.
// Dicts may live inside object streams, so modifications are written back to
// the owning xref entries explicitly.
func (rs *replScanner) updateReplaceFont(d types.Dict, sub *vivoSubset) error {
	// ToUnicode
	if tu, found := d.Find("ToUnicode"); found {
		if ir, ok := tu.(types.IndirectRef); ok {
			if err := rs.replaceStreamObj(ir, []byte(toUnicodeCMapOps(sub.runes)), false); err != nil {
				return fmt.Errorf("tounicode: %w", err)
			}
		}
	}

	// DescendantFonts[0] -> W + FontDescriptor
	df, found := d.Find("DescendantFonts")
	if !found {
		return fmt.Errorf("missing DescendantFonts")
	}
	arr, err := rs.ctx.DereferenceArray(df)
	if err != nil || len(arr) == 0 {
		return fmt.Errorf("bad DescendantFonts")
	}
	cidEntry, cid, ok := rs.dictEntry(arr[0])
	if !ok {
		return fmt.Errorf("cid font lost")
	}
	inner := types.Array{}
	for _, w := range sub.widths {
		inner = append(inner, types.Integer(w))
	}
	cid.Insert("W", types.Array{types.Integer(1), inner})
	if cidEntry != nil {
		cidEntry.Object = cid
	}

	if fdo, found := cid.Find("FontDescriptor"); found {
		descEntry, desc, ok := rs.dictEntry(fdo)
		if !ok {
			return fmt.Errorf("font descriptor lost")
		}
		desc.Insert("FontBBox", bboxArray(sub.bbox))
		desc.Insert("Ascent", types.Integer(sub.ascent))
		desc.Insert("Descent", types.Integer(sub.descent))
		desc.Insert("CapHeight", types.Integer(sub.capHeight))
		if descEntry != nil {
			descEntry.Object = desc
		}
		if ff, found := desc.Find("FontFile2"); found {
			if ffIr, ok := ff.(types.IndirectRef); ok {
				if err := rs.replaceStreamObj(ffIr, sub.ttf, true); err != nil {
					return fmt.Errorf("fontfile2: %w", err)
				}
			}
		}
	}
	return nil
}

// dictEntry resolves o to its owning xref entry and dict, following
// optimizer-created reference chains. A nil entry means the dict is direct
// and map edits propagate by themselves.
func (rs *replScanner) dictEntry(o types.Object) (*model.XRefTableEntry, types.Dict, bool) {
	for i := 0; i < 8; i++ {
		switch t := o.(type) {
		case types.IndirectRef:
			entry, found := rs.ctx.XRefTable.FindTableEntry(t.ObjectNumber.Value(), t.GenerationNumber.Value())
			if !found || entry.Object == nil {
				return nil, nil, false
			}
			if dd, ok := entry.Object.(types.Dict); ok {
				return entry, dd, true
			}
			o = entry.Object
		case *types.IndirectRef:
			o = *t
		case types.Dict:
			return nil, t, true
		default:
			return nil, nil, false
		}
	}
	return nil, nil, false
}

// toUnicodeCMapOps renders the ToUnicode CMap for the subset runes.
func toUnicodeCMapOps(runes []rune) string {
	n := len(runes)
	var cmap strings.Builder
	cmap.WriteString("/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n")
	cmap.WriteString("/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n")
	cmap.WriteString("/CMapName /Adobe-Identity-UCS def\n/CMapType 2 def\n")
	cmap.WriteString("1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n")
	for i := 0; i < n; i += 100 {
		end := i + 100
		if end > n {
			end = n
		}
		fmt.Fprintf(&cmap, "%d beginbfchar\n", end-i)
		for j := i; j < end; j++ {
			fmt.Fprintf(&cmap, "<%04X> <%s>\n", j+1, utf16Hex(runes[j]))
		}
		cmap.WriteString("endbfchar\n")
	}
	cmap.WriteString("endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n")
	return cmap.String()
}

// replaceDrawOps renders one occurrence.
func replaceDrawOps(fontName string, sub *vivoSubset, d replDraw) string {
	var b strings.Builder
	b.WriteString("q\n")
	if col := d.state.fill.content(); col != "" {
		b.WriteString(col)
	}
	fs := d.state.fontSize
	if d.state.bold {
		if w := 0.025 * fs * math.Hypot(d.m.c, d.m.d); w > 0 {
			fmt.Fprintf(&b, "%s w\n", numFmt(w))
		}
	}
	b.WriteString("BT\n")
	if d.state.charSpacing != 0 {
		fmt.Fprintf(&b, "%s Tc\n", numFmt(d.state.charSpacing))
	}
	if d.state.wordSpacing != 0 {
		fmt.Fprintf(&b, "%s Tw\n", numFmt(d.state.wordSpacing))
	}
	if d.state.hScale != 0 && d.state.hScale != 1 {
		fmt.Fprintf(&b, "%s Tz\n", numFmt(d.state.hScale*100))
	}
	if d.state.rise != 0 {
		fmt.Fprintf(&b, "%s Ts\n", numFmt(d.state.rise))
	}
	fmt.Fprintf(&b, "/%s %s Tf\n", fontName, numFmt(fs))
	if d.state.bold {
		b.WriteString("2 Tr\n")
	} else if d.state.renderMode != 0 {
		fmt.Fprintf(&b, "%s Tr\n", numFmt(d.state.renderMode))
	}
	fmt.Fprintf(&b, "%s %s %s %s %s %s Tm\n", numFmt(d.m.a), numFmt(d.m.b),
		numFmt(d.m.c), numFmt(d.m.d), numFmt(d.m.e), numFmt(d.m.f))

	const hexDigits = "0123456789ABCDEF"
	codes := make([]byte, 0, 4*len(d.text)+2)
	codes = append(codes, '<')
	for _, r := range d.text {
		code := sub.code(r)
		codes = append(codes, hexDigits[(code>>12)&0xF], hexDigits[(code>>8)&0xF],
			hexDigits[(code>>4)&0xF], hexDigits[code&0xF])
	}
	codes = append(codes, '>')
	fmt.Fprintf(&b, "%s Tj\nET\nQ\n", codes)
	return b.String()
}

// replaceFontResources returns (creating if needed) the page font resources.
func replaceFontResources(ctx *model.Context, pageDict types.Dict,
	inhPAttrs *model.InheritedPageAttrs) (types.Dict, error) {

	res := pageDict
	if o, found := pageDict.Find("Resources"); found && o != nil {
		if d, err := ctx.DereferenceDict(o); err == nil && d != nil {
			res = d
		}
	} else if inhPAttrs != nil && inhPAttrs.Resources != nil {
		pageDict.Insert("Resources", inhPAttrs.Resources)
		res = inhPAttrs.Resources
	}

	o, found := res.Find("Font")
	if !found || o == nil {
		nd := types.NewDict()
		res.Insert("Font", nd)
		return nd, nil
	}
	fd, err := ctx.DereferenceDict(o)
	if err != nil {
		return nil, err
	}
	if fd == nil {
		nd := types.NewDict()
		res.Insert("Font", nd)
		return nd, nil
	}
	return fd, nil
}

// createReplaceFont embeds the subset as a Type0/CIDFontType2 font and
// returns its object number.
func (rs *replScanner) createReplaceFont(sub *vivoSubset) (int, error) {
	xRefTable := rs.ctx.XRefTable

	sd, err := xRefTable.NewStreamDictForBuf(sub.ttf)
	if err != nil {
		return 0, err
	}
	sd.Dict.InsertInt("Length1", len(sub.ttf))
	if err := sd.Encode(); err != nil {
		return 0, err
	}
	fontFileNr, err := xRefTable.InsertObject(*sd)
	if err != nil {
		return 0, err
	}

	cmapSD, err := xRefTable.NewStreamDictForBuf([]byte(toUnicodeCMapOps(sub.runes)))
	if err != nil {
		return 0, err
	}
	if err := cmapSD.Encode(); err != nil {
		return 0, err
	}
	toUniNr, err := xRefTable.InsertObject(*cmapSD)
	if err != nil {
		return 0, err
	}

	fontName := types.Name(replaceFontName)
	desc := types.Dict{
		"Type":        types.Name("FontDescriptor"),
		"FontName":    fontName,
		"Flags":       types.Integer(4),
		"FontBBox":    bboxArray(sub.bbox),
		"ItalicAngle": types.Integer(0),
		"Ascent":      types.Integer(sub.ascent),
		"Descent":     types.Integer(sub.descent),
		"CapHeight":   types.Integer(sub.capHeight),
		"StemV":       types.Integer(80),
		"FontFile2":   *types.NewIndirectRef(fontFileNr, 0),
	}
	descNr, err := xRefTable.InsertObject(desc)
	if err != nil {
		return 0, err
	}

	inner := types.Array{}
	for _, w := range sub.widths {
		inner = append(inner, types.Integer(w))
	}
	wArr := types.Array{types.Integer(1), inner}
	cid := types.Dict{
		"Type":           types.Name("Font"),
		"Subtype":        types.Name("CIDFontType2"),
		"BaseFont":       fontName,
		"CIDSystemInfo":  types.Dict{"Registry": types.StringLiteral("Adobe"), "Ordering": types.StringLiteral("Identity"), "Supplement": types.Integer(0)},
		"FontDescriptor": *types.NewIndirectRef(descNr, 0),
		"DW":             types.Integer(500),
		"W":              wArr,
		"CIDToGIDMap":    types.Name("Identity"),
	}
	cidNr, err := xRefTable.InsertObject(cid)
	if err != nil {
		return 0, err
	}

	fontDict := types.Dict{
		"Type":            types.Name("Font"),
		"Subtype":         types.Name("Type0"),
		"BaseFont":        fontName,
		"Encoding":        types.Name("Identity-H"),
		"DescendantFonts": types.Array{*types.NewIndirectRef(cidNr, 0)},
		"ToUnicode":       *types.NewIndirectRef(toUniNr, 0),
	}
	return xRefTable.InsertObject(fontDict)
}

func bboxArray(bb [4]int) types.Array {
	return types.Array{
		types.Integer(bb[0]), types.Integer(bb[1]),
		types.Integer(bb[2]), types.Integer(bb[3]),
	}
}

// numFmt formats a float with up to 4 decimals without trailing zeros.
func numFmt(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
