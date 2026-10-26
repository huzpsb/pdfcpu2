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

package render

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"

	"github.com/huzpsb/pdfcpu2/pkg/log"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

var errUnsupported = errors.New("unsupported feature")

// textState is the PDF text state.
type textState struct {
	font        *pdfFont
	fontName    string
	size        float64
	charSpacing float64
	wordSpacing float64
	hScale      float64
	leading     float64
	rise        float64
	renderMode  int
	tm          mat // text matrix
	tlm         mat // text line matrix
}

func newTextState() textState {
	return textState{hScale: 1, tm: ident(), tlm: ident()}
}

// gstate is a graphics state snapshot.
type gstate struct {
	ctm                     mat
	lw                      float64
	lineCap, lineJoin       int
	miterLimit              float64
	dash                    []float64
	dashPhase               float64
	strokeCS, fillCS        *colorSpace
	strokeComps, fillComps  []float64
	fillPattern             string // pattern resource name or ""
	strokePattern           string
	fillAlpha, strokeAlpha  float64
	ts                      textState
	clip                    *image.Alpha
}

func newGState() *gstate {
	return &gstate{
		ctm:        ident(),
		lw:         1,
		miterLimit: 10,
		strokeCS:   csGray,
		fillCS:     csGray,
		strokeComps: []float64{0},
		fillComps:  []float64{0},
		fillAlpha:  1,
		strokeAlpha: 1,
		ts:         newTextState(),
	}
}

func (gs *gstate) clone() *gstate {
	c := *gs
	c.dash = append([]float64(nil), gs.dash...)
	c.strokeComps = append([]float64(nil), gs.strokeComps...)
	c.fillComps = append([]float64(nil), gs.fillComps...)
	return &c
}

func (gs *gstate) fillColor() rgba { return gs.fillCS.resolve(gs.fillComps) }

func (gs *gstate) strokeColor() rgba { return gs.strokeCS.resolve(gs.strokeComps) }

// device renders content streams into a canvas.
type device struct {
	ctx  *model.Context
	cv   *canvas
	base mat // page user space -> device space
	gs   *gstate
	stack []*gstate

	res     types.Dict
	resStack []types.Dict

	// Path construction state (user space).
	curPath  []pt
	subPaths []subPath

	// Pending clip set by W/W* and applied with the next path paint op.
	pendingClip    bool
	pendingClipEO  bool

	fonts   map[string]*pdfFont
	glyphs  map[glyphCacheKey][]subPath
	images  map[int]*rasterImage

	inText    bool
	warnings  []string
	depth     int
	objActive map[int]bool
	cancel    context.Context
	recent    []string
}

func newDevice(c context.Context, ctx *model.Context, cv *canvas, base mat) *device {
	return &device{
		ctx:       ctx,
		cv:        cv,
		base:      base,
		gs:        newGState(),
		fonts:     map[string]*pdfFont{},
		glyphs:    map[glyphCacheKey][]subPath{},
		images:    map[int]*rasterImage{},
		objActive: map[int]bool{},
		cancel:    c,
	}
}

func (d *device) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	d.warnings = append(d.warnings, msg)
	if log.InfoEnabled() {
		log.Info.Printf("render: %s\n", msg)
	}
}

func (d *device) decodeLimit() int64 {
	if d.ctx != nil && d.ctx.Configuration != nil {
		return d.ctx.Configuration.Limits.MaxDecodeBytes
	}
	return 0
}

func (d *device) maxImagePixels() int64 {
	if d.ctx != nil && d.ctx.Configuration != nil {
		return d.ctx.Configuration.Limits.MaxImagePixels
	}
	return model.DefaultResourceLimits().MaxImagePixels
}

// checkCancelled polls the context.
func (d *device) checkCancelled() error {
	select {
	case <-d.cancel.Done():
		return d.cancel.Err()
	default:
		return nil
	}
}

// execContent interprets a decoded content stream.
func (d *device) execContent(bb []byte) error {
	if d.depth > 32 {
		return errors.New("content nesting too deep")
	}
	d.depth++
	defer func() { d.depth-- }()

	lx := newLexer(bb)
	var operands []token
	var arrStack [][]token
	opCount := 0
	for {
		if opCount++; opCount%4096 == 0 {
			if err := d.checkCancelled(); err != nil {
				return err
			}
		}
		t, err := lx.next()
		if err != nil {
			// Skip the remainder of a corrupt stream but keep what we have.
			d.warn("content: %v", err)
			return nil
		}
		if t.kind == tokEOF {
			return nil
		}
		switch t.kind {
		case tokArrOpen:
			arrStack = append(arrStack, nil)
		case tokArrClose:
			if n := len(arrStack); n > 0 {
				arr := arrStack[n-1]
				arrStack = arrStack[:n-1]
				tok := token{kind: tokArr, arr: arr}
				if n := len(arrStack); n > 0 {
					arrStack[n-1] = append(arrStack[n-1], tok)
				} else {
					operands = append(operands, tok)
				}
			}
		case tokOp:
			if len(arrStack) > 0 {
				// Operator inside an array: corrupt content, bail out of the
				// array.
				arrStack = nil
			}
			if err := d.applyOperator(t.op, operands); err != nil {
				return err
			}
			operands = operands[:0]
		case tokInline:
			operands = append(operands, t)
			if err := d.applyOperator("INLINE", operands); err != nil {
				return err
			}
			operands = operands[:0]
		case tokDictOpen:
			// Dict operands belong to ignored operators (BDC/DP).
		default:
			tok := t
			if n := len(arrStack); n > 0 {
				arrStack[n-1] = append(arrStack[n-1], tok)
			} else if len(operands) < 1024 {
				operands = append(operands, tok)
			}
		}
	}
}

// num returns operand i as float64.
func num(ops []token, i int) float64 {
	if i < 0 || i >= len(ops) {
		return 0
	}
	return ops[i].num
}

func nameVal(ops []token, i int) string {
	if i < 0 || i >= len(ops) {
		return ""
	}
	return ops[i].name
}

func strVal(ops []token, i int) []byte {
	if i < 0 || i >= len(ops) {
		return nil
	}
	return ops[i].str
}

// applyOperator dispatches a content stream operator.
func (d *device) applyOperator(op string, ops []token) error {
	gs := d.gs
	switch op {

	// Graphics state.
	case "q":
		d.stack = append(d.stack, gs.clone())
		if len(d.stack) > 256 {
			d.stack = d.stack[:256]
		}
	case "Q":
		if n := len(d.stack); n > 0 {
			d.gs = d.stack[n-1]
			d.stack = d.stack[:n-1]
		}
	case "cm":
		if len(ops) >= 6 {
			m := mat{num(ops, 0), num(ops, 1), num(ops, 2), num(ops, 3), num(ops, 4), num(ops, 5)}
			gs.ctm = gs.ctm.mul(m)
		}
	case "w":
		if len(ops) >= 1 {
			gs.lw = math.Abs(num(ops, 0))
		}
	case "J":
		gs.lineCap = clampInt(int(num(ops, 0)), 0, 2)
	case "j":
		gs.lineJoin = clampInt(int(num(ops, 0)), 0, 2)
	case "M":
		if len(ops) >= 1 && num(ops, 0) > 0 {
			gs.miterLimit = num(ops, 0)
		}
	case "d":
		if len(ops) >= 2 {
			if a := arrayToken(ops, 0); a != nil {
				var dash []float64
				for _, e := range a {
					dash = append(dash, math.Abs(e.num))
				}
				gs.dash = dash
				gs.dashPhase = num(ops, 1)
			}
		}
	case "gs":
		d.applyExtGState(nameVal(ops, 0))
	case "ri", "i":
		// Rendering intent and flatness are ignored.

	// Path construction.
	case "m":
		d.flushSubPath()
		d.curPath = append(d.curPath, pt{num(ops, 0), num(ops, 1)})
	case "l":
		if len(d.curPath) == 0 {
			d.curPath = append(d.curPath, pt{num(ops, 0), num(ops, 1)})
		} else {
			d.curPath = append(d.curPath, pt{num(ops, 0), num(ops, 1)})
		}
	case "c":
		if len(d.curPath) > 0 {
			p0 := d.curPath[len(d.curPath)-1]
			c1 := pt{num(ops, 0), num(ops, 1)}
			c2 := pt{num(ops, 2), num(ops, 3)}
			p3 := pt{num(ops, 4), num(ops, 5)}
			d.curPath = flattenCubic(d.curPath, p0, c1, c2, p3, d.pathTolerance())
		}
	case "v":
		if len(d.curPath) > 0 {
			p0 := d.curPath[len(d.curPath)-1]
			c2 := pt{num(ops, 0), num(ops, 1)}
			p3 := pt{num(ops, 2), num(ops, 3)}
			d.curPath = flattenQuad(d.curPath, p0, c2, p3, d.pathTolerance())
		}
	case "y":
		if len(d.curPath) > 0 {
			p0 := d.curPath[len(d.curPath)-1]
			c1 := pt{num(ops, 0), num(ops, 1)}
			p3 := pt{num(ops, 2), num(ops, 3)}
			d.curPath = flattenQuad(d.curPath, p0, c1, p3, d.pathTolerance())
		}
	case "h":
		if len(d.curPath) > 0 {
			sp := subPath{pts: d.curPath}
			sp.close()
			d.subPaths = append(d.subPaths, sp)
			d.curPath = nil
		}
	case "re":
		if len(ops) >= 4 {
			x, y, w, h := num(ops, 0), num(ops, 1), num(ops, 2), num(ops, 3)
			d.flushSubPath()
			d.subPaths = append(d.subPaths, subPath{pts: []pt{
				{x, y}, {x + w, y}, {x + w, y + h}, {x, y + h}, {x, y},
			}})
		}

	// Path painting.
	case "S":
		d.pathPaint(false, true, false, false)
	case "s":
		d.pathPaint(false, true, true, false)
	case "f", "F":
		d.pathPaint(true, false, false, false)
	case "f*":
		d.pathPaint(true, false, false, true)
	case "B":
		d.pathPaint(true, true, false, false)
	case "B*":
		d.pathPaint(true, true, false, true)
	case "b":
		d.pathPaint(true, true, true, false)
	case "b*":
		d.pathPaint(true, true, true, true)
	case "n":
		d.pathPaint(false, false, false, false)

	// Clipping.
	case "W":
		d.pendingClip = true
		d.pendingClipEO = false
	case "W*":
		d.pendingClip = true
		d.pendingClipEO = true

	// Color.
	case "CS":
		if cs, err := d.parseColorSpaceName(ops); err == nil {
			gs.strokeCS = cs
		}
	case "cs":
		if cs, err := d.parseColorSpaceName(ops); err == nil {
			gs.fillCS = cs
		}
	case "SC", "SCN":
		gs.strokeComps, gs.strokePattern = d.colorOperands(ops, gs.strokeCS)
	case "sc", "scn":
		gs.fillComps, gs.fillPattern = d.colorOperands(ops, gs.fillCS)
	case "G":
		gs.strokeCS = csGray
		gs.strokeComps = []float64{num(ops, 0)}
	case "g":
		gs.fillCS = csGray
		gs.fillComps = []float64{num(ops, 0)}
	case "RG":
		gs.strokeCS = csRGB
		gs.strokeComps = []float64{num(ops, 0), num(ops, 1), num(ops, 2)}
	case "rg":
		gs.fillCS = csRGB
		gs.fillComps = []float64{num(ops, 0), num(ops, 1), num(ops, 2)}
	case "K":
		gs.strokeCS = csCMYK
		gs.strokeComps = []float64{num(ops, 0), num(ops, 1), num(ops, 2), num(ops, 3)}
	case "k":
		gs.fillCS = csCMYK
		gs.fillComps = []float64{num(ops, 0), num(ops, 1), num(ops, 2), num(ops, 3)}

	// Shadings and patterns.
	case "sh":
		d.paintShadingOp(nameVal(ops, 0))

	// XObjects and inline images.
	case "Do":
		if err := d.doXObject(nameVal(ops, 0)); err != nil {
			return err
		}
	case "INLINE":
		d.doInlineImage(ops)

	// Text.
	case "BT":
		d.inText = true
		gs.ts.tm = ident()
		gs.ts.tlm = ident()
	case "ET":
		d.inText = false
	case "Tc":
		gs.ts.charSpacing = num(ops, 0)
	case "Tw":
		gs.ts.wordSpacing = num(ops, 0)
	case "Tz":
		v := num(ops, 0)
		if v != 0 {
			gs.ts.hScale = v / 100
		}
	case "TL":
		gs.ts.leading = num(ops, 0)
	case "Tf":
		d.setFont(nameVal(ops, 0), num(ops, 1))
	case "Tr":
		gs.ts.renderMode = clampInt(int(num(ops, 0)), 0, 7)
	case "Ts":
		gs.ts.rise = num(ops, 0)
	case "Td":
		gs.ts.tlm = gs.ts.tlm.mul(translation(num(ops, 0), num(ops, 1)))
		gs.ts.tm = gs.ts.tlm
	case "TD":
		gs.ts.leading = -num(ops, 1)
		gs.ts.tlm = gs.ts.tlm.mul(translation(num(ops, 0), num(ops, 1)))
		gs.ts.tm = gs.ts.tlm
	case "Tm":
		if len(ops) >= 6 {
			gs.ts.tlm = mat{num(ops, 0), num(ops, 1), num(ops, 2), num(ops, 3), num(ops, 4), num(ops, 5)}
			gs.ts.tm = gs.ts.tlm
		}
	case "T*":
		gs.ts.tlm = gs.ts.tlm.mul(translation(0, -gs.ts.leading))
		gs.ts.tm = gs.ts.tlm
	case "Tj":
		d.showText(strVal(ops, 0))
	case "TJ":
		if a := arrayToken(ops, 0); a != nil {
			d.showTextArray(a)
		}
	case "'":
		gs.ts.tlm = gs.ts.tlm.mul(translation(0, -gs.ts.leading))
		gs.ts.tm = gs.ts.tlm
		d.showText(strVal(ops, 0))
	case `"`:
		gs.ts.wordSpacing = num(ops, 0)
		gs.ts.charSpacing = num(ops, 1)
		gs.ts.tlm = gs.ts.tlm.mul(translation(0, -gs.ts.leading))
		gs.ts.tm = gs.ts.tlm
		d.showText(strVal(ops, 2))

	// Type3 glyph metrics.
	case "d0":
		// wx wy d0: metrics only.
	case "d1":
		// wx wy llx lly urx ury d1: metrics and bbox; ignored.

	// Marked content and compatibility.
	case "BMC", "BDC", "EMC", "MP", "DP", "BX", "EX":

	default:
		// Unknown operator: ignore.
	}
	return nil
}

func arrayToken(ops []token, i int) []token {
	if i < 0 || i >= len(ops) {
		return nil
	}
	if ops[i].kind != tokArr {
		return nil
	}
	return ops[i].arr
}

// flushSubPath moves the current point sequence into subPaths.
func (d *device) flushSubPath() {
	if len(d.curPath) > 1 {
		d.subPaths = append(d.subPaths, subPath{pts: d.curPath})
	}
	d.curPath = nil
}

func (d *device) closePath() {
	d.flushSubPath()
	if n := len(d.subPaths); n > 0 {
		d.subPaths[n-1].close()
	}
}

func (d *device) pathTolerance() float64 {
	// Flattening tolerance in user space units.
	return 0.2
}

// devicePath returns the current path transformed to device space and clears
// path construction state.
func (d *device) devicePath() []subPath {
	d.flushSubPath()
	sps := d.subPaths
	d.subPaths = nil
	d.curPath = nil
	return transformPath(sps, d.base.mul(d.gs.ctm))
}

// pathPaint paints the current path and applies any pending clip. The fill
// and stroke flags select the painting operations, close closes the path
// first and evenOdd selects the winding rule.
func (d *device) pathPaint(fill, stroke, close, evenOdd bool) {
	if close {
		d.closePath()
	}
	sps := d.devicePath()
	if len(sps) > 0 {
		if fill && d.gs.fillCS.family != csPattern {
			d.cv.fill(sps, evenOdd, d.gs.fillColor(), d.gs.fillAlpha, d.gs.clip)
		}
		if stroke && d.gs.strokeCS.family != csPattern {
			// The line width is defined in user space: scale it into
			// device space using the mean scale factor of the CTM.
			s := math.Sqrt(math.Abs(d.base.mul(d.gs.ctm).det()))
			if s <= 0 {
				s = 1
			}
			d.strokeDevice(sps, d.gs.lw*s, d.gs.strokeAlpha)
		}
	}
	if d.pendingClip {
		d.pendingClip = false
		if len(sps) > 0 {
			mask := d.cv.clipMask(sps, d.pendingClipEO)
			d.gs.clip = intersectMasks(d.gs.clip, mask)
		}
	}
}

// strokeDevice strokes device space subpaths with a device space line width.
func (d *device) strokeDevice(sps []subPath, lw float64, alpha float64) {
	if d.gs.strokeCS.family == csPattern {
		return
	}
	dashed := dashPath(sps, d.gs.dash, d.gs.dashPhase)
	outline := strokeOutline(dashed, lw, d.gs.lineCap, d.gs.lineJoin, d.gs.miterLimit)
	d.cv.fill(outline, false, d.gs.strokeColor(), alpha, d.gs.clip)
}

// fillPath paints the current path as a fill (used by text rendering).
func (d *device) fillPath(sps []subPath, alpha float64) {
	if len(sps) == 0 || d.gs.fillCS.family == csPattern {
		return
	}
	d.cv.fill(sps, false, d.gs.fillColor(), alpha, d.gs.clip)
}

// strokeSubPaths strokes already flattened device space subpaths (used for
// text outlines).
func (d *device) strokeSubPaths(sps []subPath, alpha float64) {
	if len(sps) == 0 || d.gs.strokeCS.family == csPattern {
		return
	}
	s := math.Sqrt(math.Abs(d.base.mul(d.gs.ctm).det()))
	if s <= 0 {
		s = 1
	}
	d.strokeDevice(sps, d.gs.lw*s, alpha)
}

// colorOperands splits sc/scn operands into components and an optional
// pattern name.
func (d *device) colorOperands(ops []token, cs *colorSpace) ([]float64, string) {
	var comps []float64
	pattern := ""
	for i := 0; i < len(ops); i++ {
		if ops[i].kind == tokName {
			pattern = ops[i].name
			continue
		}
		comps = append(comps, ops[i].num)
	}
	if len(comps) == 0 && pattern == "" {
		comps = make([]float64, cs.n)
	}
	return comps, pattern
}

// parseColorSpaceName handles CS/cs with a name or array operand.
func (d *device) parseColorSpaceName(ops []token) (*colorSpace, error) {
	if len(ops) == 0 {
		return nil, errors.New("missing color space operand")
	}
	if ops[0].kind == tokName {
		// Look through resources or the device families.
		switch ops[0].name {
		case "DeviceGray", "G":
			return csGray, nil
		case "DeviceRGB", "RGB":
			return csRGB, nil
		case "DeviceCMYK", "CMYK":
			return csCMYK, nil
		case "Pattern":
			return &colorSpace{family: csPattern, n: 1}, nil
		}
		if d.res != nil {
			if o, found := d.res["ColorSpace"]; found {
				if csd, err := d.ctx.DereferenceDict(o); err == nil && csd != nil {
					if oo, found := csd.Find(ops[0].name); found {
						return d.parseColorSpace(oo)
					}
				}
			}
		}
		return nil, fmt.Errorf("unknown color space %s", ops[0].name)
	}
	return d.parseColorSpace(colorSpaceObject(ops))
}

// colorSpaceObject converts array operands into a types.Array.
func colorSpaceObject(ops []token) types.Object {
	a := types.Array{}
	for _, t := range ops {
		switch t.kind {
		case tokNum:
			a = append(a, types.Float(t.num))
		case tokName:
			a = append(a, types.Name(t.name))
		case tokStr:
			a = append(a, types.HexLiteral(t.str))
		}
	}
	return a
}

// applyExtGState processes a gs operator.
func (d *device) applyExtGState(name string) {
	if name == "" || d.res == nil {
		return
	}
	gsd, err := d.ctx.DereferenceDict(d.res["ExtGState"])
	if err != nil || gsd == nil {
		return
	}
	egs, err := d.ctx.DereferenceDict(gsd[name])
	if err != nil || egs == nil {
		return
	}
	gs := d.gs
	if lw, _, err := d.ctx.DereferenceNumberEntry(egs, "LW"); err == nil && lw != nil {
		gs.lw = math.Abs(*lw)
	}
	if ca, _, err := d.ctx.DereferenceNumberEntry(egs, "ca"); err == nil && ca != nil {
		gs.fillAlpha = clamp(*ca, 0, 1)
	}
	if ca, _, err := d.ctx.DereferenceNumberEntry(egs, "CA"); err == nil && ca != nil {
		gs.strokeAlpha = clamp(*ca, 0, 1)
	}
	if o, found := egs.Find("SMask"); found && o != nil {
		if mask := d.renderSoftMask(o); mask != nil {
			gs.clip = intersectMasks(gs.clip, mask)
		}
	}
	if da, err := d.ctx.DereferenceArray(egs["D"]); err == nil && da != nil && len(da) >= 1 {
		// [dash] phase
		if arr, err := d.ctx.DereferenceArray(da[0]); err == nil && arr != nil {
			var dash []float64
			for _, oo := range arr {
				if v, err := d.ctx.DereferenceNumber(oo); err == nil {
					dash = append(dash, math.Abs(v))
				}
			}
			gs.dash = dash
			if len(da) > 1 {
				if ph, err := d.ctx.DereferenceNumber(da[1]); err == nil {
					gs.dashPhase = ph
				}
			}
		}
	}
	if fa, err := d.ctx.DereferenceArray(egs["Font"]); err == nil && len(fa) >= 2 {
		if n, err := d.ctx.DereferenceName(fa[0], 0, nil); err == nil && n.Value() != "" {
			if sz, err := d.ctx.DereferenceNumber(fa[1]); err == nil {
				d.setFont(n.Value(), sz)
			}
		}
	}
}

// setFont loads a font resource and updates the text state.
func (d *device) setFont(name string, size float64) {
	if name == "" {
		return
	}
	f, err := d.fontForName(name)
	if err != nil {
		d.warn("font %s: %v", name, err)
		return
	}
	d.gs.ts.font = f
	d.gs.ts.fontName = name
	d.gs.ts.size = size
}

// doXObject renders an XObject.
func (d *device) doXObject(name string) error {
	if name == "" || d.res == nil {
		return nil
	}
	xd, err := d.ctx.DereferenceDict(d.res["XObject"])
	if err != nil || xd == nil {
		return nil
	}
	ir, ok := xd[name].(types.IndirectRef)
	if !ok {
		return nil
	}
	objNr := ir.ObjectNumber.Value()
	if d.objActive[objNr] {
		return nil // recursion guard
	}
	sd, _, err := d.ctx.DereferenceStreamDict(types.Object(ir))
	if err != nil || sd == nil {
		return nil
	}
	subtype := ""
	if st := sd.Subtype(); st != nil {
		subtype = *st
	}
	switch subtype {
	case "Image":
		ri, err := d.loadXObjectImage(objNr, sd)
		if err != nil {
			if errors.Is(err, errUnsupported) {
				d.warn("image %s: %v", name, err)
				return nil
			}
			return fmt.Errorf("image %s: %w", name, err)
		}
		d.drawImage(ri, d.base.mul(d.gs.ctm), d.gs.fillAlpha, d.gs.fillColor())
	case "Form":
		return d.doForm(objNr, sd)
	case "PS":
		// PostScript XObjects are ignored.
	}
	return nil
}

// doForm executes a form XObject.
func (d *device) doForm(objNr int, sd *types.StreamDict) error {
	d.objActive[objNr] = true
	defer delete(d.objActive, objNr)

	if err := sd.DecodeWithLimit(d.decodeLimit()); err != nil {
		return fmt.Errorf("form obj#%d: decode: %w", objNr, err)
	}

	var execErr error

	// Transparency group: render into a separate buffer and composite with
	// the constant alpha in effect at the Do (PDF 11.4.2).
	if o, found := sd.Dict.Find("Group"); found && o != nil {
		if gd, err := d.ctx.DereferenceDict(o); err == nil && gd != nil {
			groupAlpha := d.gs.fillAlpha
			clip := d.gs.clip
			parentCTM := d.gs.ctm
			tmp := d.renderFormToCanvas(sd, parentCTM, clip)
			d.compositeCanvas(tmp, groupAlpha, clip)
			return nil
		}
	}

	// Save state.
	savedGS := d.gs
	savedRes := d.res
	d.stack = append(d.stack, savedGS.clone())
	d.resStack = append(d.resStack, savedRes)
	d.gs = savedGS.clone()

	// Matrix.
	m := ident()
	if ma, err := d.ctx.DereferenceArray(sd.Dict["Matrix"]); err == nil && len(ma) == 6 {
		vals := make([]float64, 6)
		ok := true
		for i, oo := range ma {
			v, err := d.ctx.DereferenceNumber(oo)
			if err != nil {
				ok = false
				break
			}
			vals[i] = v
		}
		if ok {
			m = mat{vals[0], vals[1], vals[2], vals[3], vals[4], vals[5]}
		}
	}
	d.gs.ctm = savedGS.ctm.mul(m)

	// Resources.
	if res, err := d.ctx.DereferenceDict(sd.Dict["Resources"]); err == nil && res != nil {
		d.res = res
	} else {
		// inherit
	}

	// BBox clip.
	if ba, err := d.ctx.DereferenceArray(sd.Dict["BBox"]); err == nil && len(ba) >= 4 {
		vals := make([]float64, 4)
		ok := true
		for i, oo := range ba {
			v, err := d.ctx.DereferenceNumber(oo)
			if err != nil {
				ok = false
				break
			}
			vals[i] = v
		}
		if ok {
			// Normalize inverted corner order (layout software often writes
			// [left top right bottom]).
			llx, urx := math.Min(vals[0], vals[2]), math.Max(vals[0], vals[2])
			lly, ury := math.Min(vals[1], vals[3]), math.Max(vals[1], vals[3])
			rect := subPath{pts: []pt{
				{llx, lly}, {urx, lly}, {urx, ury}, {llx, ury}, {llx, lly},
			}}
			t := d.base.mul(d.gs.ctm)
			mask := d.cv.clipMask(transformPath([]subPath{rect}, t), false)
			d.gs.clip = intersectMasks(d.gs.clip, mask)
		}
	}

	execErr = d.execContent(sd.Content)

	// Restore state.
	if n := len(d.stack); n > 0 {
		d.gs = d.stack[n-1]
		d.stack = d.stack[:n-1]
	} else {
		d.gs = savedGS
	}
	if rn := len(d.resStack); rn > 0 {
		d.res = d.resStack[rn-1]
		d.resStack = d.resStack[:rn-1]
	} else {
		d.res = savedRes
	}
	return execErr
}

// doInlineImage renders a BI..EI inline image.
func (d *device) doInlineImage(ops []token) {
	var t *token
	for i := range ops {
		if ops[i].kind == tokInline {
			t = &ops[i]
			break
		}
	}
	if t == nil {
		return
	}
	entries := t.dict
	get := func(keys ...string) (float64, bool) {
		for _, k := range keys {
			if tk, ok := entries[k]; ok {
				return tk.num, true
			}
		}
		return 0, false
	}
	getName := func(keys ...string) string {
		for _, k := range keys {
			if tk, ok := entries[k]; ok && tk.kind == tokName {
				return tk.name
			}
		}
		return ""
	}
	w, _ := get("W", "Width")
	h, _ := get("H", "Height")
	bpc, hasBPC := get("BPC", "BitsPerComponent")
	if !hasBPC {
		bpc = 1
	}
	filterName := expandFilterName(getName("F", "Filter"))
	if filterName == "JPXDecode" || filterName == "JBIG2Decode" {
		d.warn("inline image: filter %s unsupported", filterName)
		return
	}

	sd := &types.StreamDict{Dict: types.Dict{}, Raw: t.raw}
	sd.Insert("Width", types.Integer(int(w)))
	sd.Insert("Height", types.Integer(int(h)))
	sd.Insert("BitsPerComponent", types.Integer(int(bpc)))
	if tk, ok := entries["IM"]; ok && tk.boolean() {
		sd.Insert("ImageMask", types.Boolean(true))
	}
	if filterName != "" {
		sd.FilterPipeline = []types.PDFFilter{{Name: filterName}}
	}
	csObj := types.Object(types.Name(expandCSName(getName("CS", "ColorSpace"))))
	if csName, _ := csObj.(types.Name); csName == "" {
		csObj = nil
	}

	ri, err := d.decodeImage(sd, csObj)
	if err != nil {
		d.warn("inline image: %v", err)
		return
	}
	d.drawImage(ri, d.base.mul(d.gs.ctm), d.gs.fillAlpha, d.gs.fillColor())
}

func expandFilterName(s string) string {
	switch s {
	case "AHx":
		return "ASCIIHexDecode"
	case "A85":
		return "ASCII85Decode"
	case "LZW":
		return "LZWDecode"
	case "Fl":
		return "FlateDecode"
	case "RL":
		return "RunLengthDecode"
	case "CCF":
		return "CCITTFaxDecode"
	case "DCT":
		return "DCTDecode"
	}
	return s
}

func expandCSName(s string) string {
	switch s {
	case "G":
		return "DeviceGray"
	case "RGB":
		return "DeviceRGB"
	case "CMYK":
		return "DeviceCMYK"
	case "I":
		return "Indexed"
	}
	return s
}

// paintShadingOp handles the sh operator.
func (d *device) paintShadingOp(name string) {
	if name == "" || d.res == nil {
		return
	}
	shd, err := d.ctx.DereferenceDict(d.res["Shading"])
	if err != nil || shd == nil {
		return
	}
	sh, err := d.parseShading(shd[name])
	if err != nil {
		d.warn("shading %s: %v", name, err)
		return
	}
	d.paintShading(sh, d.base.mul(d.gs.ctm))
}
