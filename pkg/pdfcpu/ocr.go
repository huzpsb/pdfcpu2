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
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"

	_ "github.com/hhrutter/tiff" // CMYK rendering via TIFF

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/log"
	"github.com/huzpsb/pdfcpu2/pkg/ocr"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

// OCR overlay: recognize text in every image XObject drawn on a page and
// embed it as an invisible, selectable/searchable text layer so scanned PDFs
// become accessible (text extraction, readers, copy & paste).
//
// Design:
//   - every image XObject is OCR'd at most once per document run (dedup by
//     object number) and marked with a private dict entry afterwards, so
//     re-running the command never re-OCRs (p3 idempotency);
//   - text is drawn with a synthesized blank TrueType embedded as a
//     CIDFontType2 (one glyph per rune, Identity-H, ToUnicode CMap) under
//     render mode 3 — invisible in any viewer, but real font metrics keep
//     selection/copy/edit working everywhere (Type3 variants confuse Foxit);
//   - each recognized line is placed along its rotated detection quad:
//     characters are distributed uniformly over the line (uniform advance,
//     height = quad height, rotation = quad angle), which is exact enough
//     for selection rectangles.

const (
	ocrMarkerKey  = "Pdfcpu2Ocr"
	ocrFontPrefix = "P2OcrFont" // resource name prefix
	ocrMinImgDim  = 16          // skip tiny decorations
)

// ocrPlacement is one /Do invocation of an image XObject with its CTM.
type ocrPlacement struct {
	objNr int
	sd    *types.StreamDict
	ctm   affine
}

// ocrRun is one recognized line mapped into page space.
type ocrRun struct {
	p    [4][2]float64 // tl, tr, br, bl
	text string
}

// OCRPages runs the embedded OCR engine over all image XObjects drawn on the
// given pages (all pages if pageNrs is empty) and injects an invisible text
// layer. Returns the number of images processed (already-marked images are
// not re-processed and not counted).
func OCRPages(c context.Context, ctx *model.Context, pageNrs []int) (int, error) {
	if err := contextutil.Check(c); err != nil {
		return 0, err
	}
	if ctx == nil || ctx.XRefTable == nil {
		return 0, fmt.Errorf("ocr: missing context")
	}
	if len(pageNrs) == 0 {
		for i := 1; i <= ctx.PageCount; i++ {
			pageNrs = append(pageNrs, i)
		}
	}
	processed := 0
	for _, pageNr := range pageNrs {
		if err := contextutil.Check(c); err != nil {
			return processed, err
		}
		n, err := ocrPage(c, ctx, pageNr)
		if err != nil {
			return processed, fmt.Errorf("page %d: %w", pageNr, err)
		}
		processed += n
	}
	return processed, nil
}

func ocrPage(c context.Context, ctx *model.Context, pageNr int) (int, error) {
	pageDict, _, inhPAttrs, err := ctx.XRefTable.PageDict(c, pageNr, true)
	if err != nil {
		return 0, fmt.Errorf("page dict: %w", err)
	}
	res := pageResources(ctx, pageDict, inhPAttrs)

	content, err := pageContentBytes(c, ctx, pageDict)
	if err != nil {
		return 0, err
	}
	var placements []ocrPlacement
	leftover := identityAffine()
	if len(content) > 0 {
		placements, leftover, err = scanImagePlacements(c, ctx, content, res,
			identityAffine(), 0, nil)
		if err != nil {
			return 0, err
		}
	}
	if len(placements) == 0 {
		return 0, nil
	}

	// collect placements per image (OCR once, place for every occurrence)
	byObj := map[int][]ocrPlacement{}
	var order []int
	for _, pl := range placements {
		if _, done := byObj[pl.objNr]; !done {
			order = append(order, pl.objNr)
		}
		byObj[pl.objNr] = append(byObj[pl.objNr], pl)
	}

	var runs []ocrRun
	processed := 0
	for _, objNr := range order {
		pls := byObj[objNr]
		sd := pls[0].sd
		if _, found := sd.Find(ocrMarkerKey); found {
			continue // already OCR'd by an earlier run
		}
		sd.Dict.Insert(ocrMarkerKey, types.Boolean(true)) // mark even on empty results
		processed++
		w, h, lines, err := ocrImage(c, ctx, sd, objNr)
		if err != nil {
			if log.InfoEnabled() {
				log.Info.Printf("ocr: image obj#%d skipped: %v\n", objNr, err)
			}
			continue
		}
		if len(lines) == 0 {
			continue
		}
		for _, pl := range pls {
			for _, l := range lines {
				r := ocrMapRun(l, pl.ctm, w, h)
				if r != nil {
					runs = append(runs, *r)
				}
			}
		}
	}
	if len(runs) == 0 {
		return processed, nil
	}

	contentOps := ocrBuildContent(c, ctx, pageDict, runs, leftover)
	if contentOps == "" {
		return processed, nil
	}
	if err := ctx.XRefTable.AppendContent(pageDict, []byte(contentOps)); err != nil {
		return processed, fmt.Errorf("append content: %w", err)
	}
	return processed, nil
}

// ---------------------------------------------------------------- scanning

// pageContentBytes returns the concatenated decoded content streams of a page.
func pageContentBytes(c context.Context, ctx *model.Context, pageDict types.Dict) ([]byte, error) {
	obj, found := pageDict.Find("Contents")
	if !found || obj == nil {
		return nil, nil
	}
	var out []byte
	appendStream := func(o types.Object) error {
		sd, _, err := ctx.XRefTable.DereferenceStreamDict(o)
		if err != nil {
			return err
		}
		if sd == nil {
			return nil
		}
		if err := ctx.XRefTable.DecodeStream(sd); err != nil {
			return err
		}
		out = append(out, sd.Content...)
		return nil
	}
	ir, ok := obj.(types.IndirectRef)
	if ok {
		if err := appendStream(ir); err != nil {
			return nil, err
		}
		return out, nil
	}
	arr, ok := obj.(types.Array)
	if !ok {
		return nil, nil
	}
	for _, o := range arr {
		if err := appendStream(o); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanImagePlacements walks a content stream tracking q/Q/cm and records the
// CTM of every image /Do. Form XObjects are descended into. It also returns
// the leftover CTM after the stream (used to normalize appended content).
func scanImagePlacements(c context.Context, ctx *model.Context, bb []byte, resources types.Dict,
	base affine, depth int, out []ocrPlacement) ([]ocrPlacement, affine, error) {
	if out == nil {
		out = []ocrPlacement{}
	}
	if depth > 8 {
		return out, base, nil
	}
	xobjs := map[string]types.Object{}
	if resources != nil {
		if o, found := resources.Find("XObject"); found && o != nil {
			if d, err := ctx.DereferenceDict(o); err == nil && d != nil {
				for k, v := range d {
					xobjs[k] = v
				}
			}
		}
	}
	sc := newContentScanner(bb)
	var operands []contentOperand
	stack := []affine{base}
	for {
		if err := contextutil.Check(c); err != nil {
			return out, stack[len(stack)-1], err
		}
		tok, err := sc.next()
		if err != nil {
			break // tolerate trailing garbage
		}
		if tok.isZero() {
			break
		}
		if tok.isOperand() {
			if len(operands) < 32 {
				operands = append(operands, tok)
			}
			continue
		}
		switch tok.operator {
		case "q":
			stack = append(stack, stack[len(stack)-1])
		case "Q":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
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
					stack[len(stack)-1] = m.multiply(stack[len(stack)-1])
				}
			}
		case "Do":
			if len(operands) > 0 && operands[len(operands)-1].name != "" {
				name := operands[len(operands)-1].name
				o, found := xobjs[name]
				if found && o != nil {
					ctm := stack[len(stack)-1]
					out, err = ocrHandleXObject(c, ctx, o, ctm, depth, out)
					if err != nil {
						return out, ctm, err
					}
				}
			}
		case "BI":
			if err := sc.skipInlineImage(); err != nil {
				return out, stack[len(stack)-1], nil
			}
		}
		operands = operands[:0]
	}
	return out, stack[len(stack)-1], nil
}

func ocrHandleXObject(c context.Context, ctx *model.Context, o types.Object, ctm affine,
	depth int, out []ocrPlacement) ([]ocrPlacement, error) {
	sd, _, err := ctx.XRefTable.DereferenceStreamDict(o)
	if err != nil || sd == nil {
		return out, err
	}
	subtype, found := sd.Find("Subtype")
	if !found {
		return out, nil
	}
	name, ok := subtype.(types.Name)
	if !ok {
		return out, nil
	}
	switch name.String() {
	case "Image":
		objNr := 0
		if ir, ok := o.(types.IndirectRef); ok {
			objNr = ir.ObjectNumber.Value()
		}
		out = append(out, ocrPlacement{objNr: objNr, sd: sd, ctm: ctm})
	case "Form":
		m := identityAffine()
		if o, found := sd.Find("Matrix"); found && o != nil {
			if arr, err := ctx.DereferenceArray(o); err == nil && len(arr) == 6 {
				var nums [6]float64
				ok := true
				for i, el := range arr {
					if n, err := ctx.DereferenceNumber(el); err == nil {
						nums[i] = n
					} else {
						ok = false
						break
					}
				}
				if ok {
					m = affine{a: nums[0], b: nums[1], c: nums[2], d: nums[3], e: nums[4], f: nums[5]}
				}
			}
		}
		if err := ctx.XRefTable.DecodeStream(sd); err != nil {
			return out, nil
		}
		var fres types.Dict
		if o, found := sd.Find("Resources"); found && o != nil {
			if d, err := ctx.DereferenceDict(o); err == nil {
				fres = d
			}
		}
		inner := ctm.multiply(m)
		res2, _, err := scanImagePlacements(c, ctx, sd.Content, fres, inner, depth+1, out)
		return res2, err
	}
	return out, nil
}

// ---------------------------------------------------------------- OCR + mapping

func ocrImage(c context.Context, ctx *model.Context, sd *types.StreamDict,
	objNr int) (int, int, []ocr.Line, error) {
	w := sd.IntEntry("Width")
	h := sd.IntEntry("Height")
	if w == nil || h == nil {
		return 0, 0, nil, fmt.Errorf("missing width/height")
	}
	if *w < ocrMinImgDim || *h < ocrMinImgDim {
		return 0, 0, nil, fmt.Errorf("image too small")
	}
	im, err := ExtractImage(c, ctx, sd, false, fmt.Sprintf("obj%d", objNr), objNr, false)
	if err != nil {
		return 0, 0, nil, err
	}
	decoded, _, err := image.Decode(im.Reader)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("decode %s: %w", im.FileType, err)
	}
	lines, err := ocr.Recognize(decoded)
	if err != nil {
		return 0, 0, nil, err
	}
	return *w, *h, lines, nil
}

// ocrMapRun maps a recognized quad (image pixels, y down) into page space
// (PDF user space, y up) through the image's placement CTM.
func ocrMapRun(l ocr.Line, ctm affine, imgW, imgH int) *ocrRun {
	px2page := func(x, y float64) [2]float64 {
		u := x / float64(imgW)
		v := 1 - y/float64(imgH)
		return [2]float64{
			ctm.a*u + ctm.c*v + ctm.e,
			ctm.b*u + ctm.d*v + ctm.f,
		}
	}
	var r ocrRun
	for k := 0; k < 4; k++ {
		r.p[k] = px2page(l.Box[2*k], l.Box[2*k+1])
	}
	r.text = l.Text
	if r.text == "" {
		return nil
	}
	return &r
}

// ---------------------------------------------------------------- text layer

// ocrFontCodes assigns one code per character INSTANCE (not per rune): every
// glyph's box and advance equal its exact slot width in font units, so
// consecutive glyph boxes tile the line with neither overlaps (overlaps make
// Foxit merge the whole page into one line) nor gaps (gaps make viewers and
// extractors insert spurious spaces). One chunk holds up to ocrMaxCodes
// instances; pages rarely need more than one.
type ocrFontCodes struct {
	runes  []rune // rune per code (code = i+1)
	widths []int  // advance/box width in font units per code
}

func newOcrFontCodes() *ocrFontCodes { return &ocrFontCodes{} }

func (fc *ocrFontCodes) add(r rune, units int) uint16 {
	fc.runes = append(fc.runes, r)
	fc.widths = append(fc.widths, units)
	return uint16(len(fc.runes))
}

const ocrMaxCodes = 60000 // instances per font (2-byte codes cap at 65535)

// ocrCharRef locates one character instance's code.
type ocrCharRef struct {
	chunk int
	code  uint16
}

// ocrBuildContent builds the Type0 CID fonts and the appended content stream
// for all runs of one page. Returns "" if nothing could be placed.
func ocrBuildContent(c context.Context, ctx *model.Context, pageDict types.Dict,
	runs []ocrRun, leftover affine) string {

	type runInfo struct {
		r            ocrRun
		dirX, dirY   float64
		downX, downY float64
		adv, fs      float64
		chars        []ocrCharRef
	}
	var infos []runInfo
	chunks := []*ocrFontCodes{newOcrFontCodes()}
	for _, r := range runs {
		dx, dy := r.p[1][0]-r.p[0][0], r.p[1][1]-r.p[0][1]
		ux, uy := r.p[3][0]-r.p[0][0], r.p[3][1]-r.p[0][1]
		length := math.Hypot(dx, dy)
		height := math.Hypot(ux, uy)
		if length < 1e-3 || height < 1e-3 {
			continue
		}
		runes := []rune(r.text)
		if len(runes) == 0 {
			continue
		}
		var info runInfo
		info.r = r
		info.dirX, info.dirY = dx/length, dy/length
		info.downX, info.downY = ux/height, uy/height
		info.adv = length / float64(len(runes))
		info.fs = height
		for _, ch := range runes {
			ck := chunks[len(chunks)-1]
			if len(ck.runes) >= ocrMaxCodes {
				ck = newOcrFontCodes()
				chunks = append(chunks, ck)
			}
			units := int(math.Round(info.adv / info.fs * ocrEm))
			if units < 1 {
				units = 1
			}
			if units > 8000 {
				units = 8000
			}
			info.chars = append(info.chars, ocrCharRef{len(chunks) - 1, ck.add(ch, units)})
		}
		infos = append(infos, info)
	}
	if len(chunks) == 1 && len(chunks[0].runes) == 0 {
		return ""
	}

	// page font resources (created on demand) + collision-free names
	fontResDict, err := ocrEnsureFontResources(ctx, pageDict)
	if err != nil {
		if log.InfoEnabled() {
			log.Info.Printf("ocr: font resources: %v\n", err)
		}
		return ""
	}
	var names []string
	for i := range chunks {
		name := ocrFontPrefix
		if i > 0 {
			name = fmt.Sprintf("%s%d", ocrFontPrefix, i+1)
		}
		for {
			if _, taken := fontResDict.Find(name); !taken {
				break
			}
			name += "x"
		}
		if err := ocrCreateCIDFont(ctx.XRefTable, name, chunks[i], fontResDict); err != nil {
			if log.InfoEnabled() {
				log.Info.Printf("ocr: cid font: %v\n", err)
			}
			return ""
		}
		names = append(names, name)
	}

	// build the content stream
	var b strings.Builder
	b.WriteString("q\n")
	if inv, ok := affineInverse(leftover); ok {
		fmt.Fprintf(&b, "%s cm\n", affineString(inv))
	}
	for _, info := range infos {
		runes := []rune(info.r.text)
		for i := range runes {
			ref := info.chars[i]
			// glyph box spans [i*adv, (i+1)*adv] along the line;
			// baseline at 80% height
			cx := float64(i) * info.adv
			bx := info.r.p[0][0] + info.dirX*cx + info.downX*0.8*info.fs
			by := info.r.p[0][1] + info.dirY*cx + info.downY*0.8*info.fs
			fmt.Fprintf(&b, "BT\n3 Tr\n/%s %s Tf\n", names[ref.chunk], num(info.fs))
			fmt.Fprintf(&b, "%s %s %s %s %s %s Tm\n", num(info.dirX), num(info.dirY),
				num(-info.dirY), num(info.dirX), num(bx), num(by))
			fmt.Fprintf(&b, "<%04X> Tj\nET\n", ref.code)
		}
	}
	b.WriteString("Q\n")
	return b.String()
}

// ocrEnsureFontResources returns the page's /Resources/Font dict, creating the
// entries if necessary.
func ocrEnsureFontResources(ctx *model.Context, pageDict types.Dict) (types.Dict, error) {
	res, err := ocrSubDict(ctx, pageDict, "Resources", true)
	if err != nil {
		return nil, err
	}
	return ocrSubDict(ctx, res, "Font", true)
}

// ocrSubDict looks up key in d (following indirect references); with create
// it inserts an empty dict when missing.
func ocrSubDict(ctx *model.Context, d types.Dict, key string, create bool) (types.Dict, error) {
	o, found := d.Find(key)
	if !found || o == nil {
		if !create {
			return nil, fmt.Errorf("entry %s missing", key)
		}
		nd := types.NewDict()
		d.Insert(key, nd)
		return nd, nil
	}
	dd, err := ctx.DereferenceDict(o)
	if err != nil {
		return nil, err
	}
	if dd == nil {
		if !create {
			return nil, fmt.Errorf("entry %s not a dict", key)
		}
		nd := types.NewDict()
		d.Insert(key, nd)
		return nd, nil
	}
	return dd, nil
}

// ocrCreateCIDFont builds a Type0/CIDFontType2 font around the synthesized
// blank TrueType (ocrBlankTTF): one glyph per code, Identity-H encoding,
// ToUnicode CMap included. It is registered under name in the page font
// resources. Real TrueType metrics keep viewers' selection/copy/edit paths
// happy where Type3 fonts are mis-handled (e.g. Foxit).
func ocrCreateCIDFont(xRefTable *model.XRefTable, name string, fc *ocrFontCodes,
	fontResDict types.Dict) error {
	n := len(fc.runes)
	if n == 0 || n > 0xFFFE {
		return fmt.Errorf("bad code count %d", n)
	}

	// embedded font program: one glyph per instance, box width = slot width
	ttf := ocrBlankTTF(fc.widths)
	sd, err := xRefTable.NewStreamDictForBuf(ttf)
	if err != nil {
		return err
	}
	sd.Dict.InsertInt("Length1", len(ttf))
	if err := sd.Encode(); err != nil {
		return err
	}
	fontFileNr, err := xRefTable.InsertObject(*sd)
	if err != nil {
		return err
	}

	// ToUnicode CMap (2-byte codes)
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
			fmt.Fprintf(&cmap, "<%04X> <%s>\n", j+1, utf16Hex(fc.runes[j]))
		}
		cmap.WriteString("endbfchar\n")
	}
	cmap.WriteString("endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n")
	cmapSD, err := xRefTable.NewStreamDictForBuf([]byte(cmap.String()))
	if err != nil {
		return err
	}
	if err := cmapSD.Encode(); err != nil {
		return err
	}
	toUniNr, err := xRefTable.InsertObject(*cmapSD)
	if err != nil {
		return err
	}

	// font descriptor (metrics mirror the synthesized TTF)
	desc := types.Dict{
		"Type":        types.Name("FontDescriptor"),
		"FontName":    types.Name(name),
		"Flags":       types.Integer(4), // symbolic
		"FontBBox":    types.Array{types.Integer(0), types.Integer(ocrDesc), types.Integer(ocrEm), types.Integer(ocrAsc)},
		"ItalicAngle": types.Integer(0),
		"Ascent":      types.Integer(ocrAsc),
		"Descent":     types.Integer(ocrDesc),
		"CapHeight":   types.Integer(700),
		"StemV":       types.Integer(80),
		"FontFile2":   *types.NewIndirectRef(fontFileNr, 0),
	}
	descNr, err := xRefTable.InsertObject(desc)
	if err != nil {
		return err
	}

	// per-code widths (W array): [ 1 [w1 w2 ...] ] — codes 1..n consecutive
	inner := types.Array{}
	for _, w := range fc.widths {
		inner = append(inner, types.Integer(w))
	}
	wArr := types.Array{types.Integer(1), inner}
	cid := types.Dict{
		"Type":           types.Name("Font"),
		"Subtype":        types.Name("CIDFontType2"),
		"BaseFont":       types.Name(name),
		"CIDSystemInfo":  types.Dict{"Registry": types.StringLiteral("Adobe"), "Ordering": types.StringLiteral("Identity"), "Supplement": types.Integer(0)},
		"FontDescriptor": *types.NewIndirectRef(descNr, 0),
		"DW":             types.Integer(500),
		"W":              wArr,
		"CIDToGIDMap":    types.Name("Identity"),
	}
	cidNr, err := xRefTable.InsertObject(cid)
	if err != nil {
		return err
	}

	fontDict := types.Dict{
		"Type":            types.Name("Font"),
		"Subtype":         types.Name("Type0"),
		"BaseFont":        types.Name(name),
		"Encoding":        types.Name("Identity-H"),
		"DescendantFonts": types.Array{*types.NewIndirectRef(cidNr, 0)},
		"ToUnicode":       *types.NewIndirectRef(toUniNr, 0),
	}
	fontNr, err := xRefTable.InsertObject(fontDict)
	if err != nil {
		return err
	}
	fontResDict.Insert(name, *types.NewIndirectRef(fontNr, 0))
	return nil
}

// utf16Hex encodes a rune as UTF-16BE hex (4 or 8 digits).
func utf16Hex(r rune) string {
	u := utf16.Encode([]rune{r})
	var sb strings.Builder
	for _, v := range u {
		fmt.Fprintf(&sb, "%04X", v)
	}
	return sb.String()
}

// ---------------------------------------------------------------- helpers

func affineString(m affine) string {
	return fmt.Sprintf("%s %s %s %s %s %s", num(m.a), num(m.b), num(m.c), num(m.d), num(m.e), num(m.f))
}

func affineInverse(m affine) (affine, bool) {
	det := m.a*m.d - m.b*m.c
	if math.Abs(det) < 1e-9 {
		return affine{}, false
	}
	inv := affine{
		a: m.d / det, b: -m.b / det,
		c: -m.c / det, d: m.a / det,
	}
	inv.e = -(inv.a*m.e + inv.c*m.f)
	inv.f = -(inv.b*m.e + inv.d*m.f)
	return inv, true
}

func num(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
