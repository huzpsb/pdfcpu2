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
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"

	"github.com/huzpsb/pdfcpu2/internal/corefont/metrics"
	"github.com/huzpsb/pdfcpu2/internal/fallbackfont"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// simpleFontInfo holds the properties of a simple (8 bit encoded) font.
type simpleFontInfo struct {
	enc           *simpleEnc
	first, last   int
	widths        []float64 // in 1000 unit glyph space
	missingWidth  float64
	isCore        bool
	coreName      string
	hasToUnicode  bool
	toUnicode     *toUnicodeCMap
	customGlyphID map[byte]sfnt.GlyphIndex // codes mapped through cmap (3,0)
}

// type0Info holds the properties of a composite (Type0) font.
type type0Info struct {
	codeLen   int
	cidToGid  map[uint32]uint32
	w         map[uint32]float64
	dw        float64
	ordering  string
	toUnicode *toUnicodeCMap
}

// type3Info holds the properties of a Type3 font.
type type3Info struct {
	charProcs   map[string]types.Object
	resources   types.Dict
	fontMatrix  mat
	first, last int
	widths      []float64
}

// pdfFont is a font resource prepared for rendering.
type pdfFont struct {
	name     string
	subtype  string
	isType0  bool
	sf       *sfnt.Font
	cff      *cffFont
	ttf      *ttfFont
	ttfRev   map[uint16]rune
	upem     float64
	buf      *sfnt.Buffer
	fallback bool

	simple *simpleFontInfo
	t0     *type0Info
	t3     *type3Info
}

var (
	fallbackOnce sync.Once
	fallbackFont *sfnt.Font
	fallbackErr  error
	fallbackUpem float64
)

func loadFallback() (*sfnt.Font, float64, error) {
	fallbackOnce.Do(func() {
		fallbackFont, fallbackErr = sfnt.Parse(fallbackfont.Bytes())
		if fallbackFont != nil {
			fallbackUpem = float64(fallbackFont.UnitsPerEm())
		}
	})
	return fallbackFont, fallbackUpem, fallbackErr
}

// glyph cache entry: flattened outline in font units (y axis down).
type glyphCacheKey struct {
	f   *pdfFont
	gid sfnt.GlyphIndex
}

// fontForName resolves and caches the font resource for a Tf name.
func (d *device) fontForName(name string) (*pdfFont, error) {
	if d.res == nil {
		return nil, fmt.Errorf("font %s: missing resources", name)
	}
	fdict, err := d.ctx.DereferenceDict(d.res["Font"])
	if err != nil {
		return nil, fmt.Errorf("font %s: %w", name, err)
	}
	if fdict == nil {
		return nil, fmt.Errorf("font %s: missing font resource", name)
	}
	o, found := fdict.Find(name)
	if !found {
		return nil, fmt.Errorf("font %s: not defined", name)
	}
	// Resource names are scoped to their resource dictionary: the same name in
	// a form's resources and in the page's resources may be different fonts.
	// Cache by font dict object number when possible so that a form's font
	// cannot leak into the page (or vice versa).
	key := "d/" + name
	if ir, ok := o.(types.IndirectRef); ok {
		key = fmt.Sprintf("o#%d", ir.ObjectNumber.Value())
	}
	if f, ok := d.fonts[key]; ok {
		return f, nil
	}
	fd, err := d.ctx.DereferenceDict(o)
	if err != nil {
		return nil, fmt.Errorf("font %s: %w", name, err)
	}
	if fd == nil {
		return nil, fmt.Errorf("font %s: null font dict", name)
	}
	f, err := d.loadFont(name, fd)
	if err != nil {
		return nil, err
	}
	d.fonts[key] = f
	return f, nil
}

func (d *device) loadFont(name string, fd types.Dict) (*pdfFont, error) {
	subtype := ""
	if st := fd.Subtype(); st != nil {
		subtype = *st
	}
	f := &pdfFont{name: name, subtype: subtype}

	switch subtype {
	case "Type0":
		f.isType0 = true
		if err := d.loadType0Font(f, fd); err != nil {
			return nil, err
		}
	case "Type3":
		if err := d.loadType3Font(f, fd); err != nil {
			return nil, err
		}
	default:
		// Type1, MMType1, TrueType, CIDFontType0/2 used directly.
		if err := d.loadSimpleFont(f, fd); err != nil {
			return nil, err
		}
	}

	// Resolve the embedded font program (not applicable to Type3).
	if f.t3 == nil {
		d.attachFontProgram(f, fd)
	}
	return f, nil
}

func (d *device) loadSimpleFont(f *pdfFont, fd types.Dict) error {
	baseFont := ""
	if bf, _, err := d.ctx.DereferenceNameEntry(fd, "BaseFont"); err == nil && bf != nil {
		baseFont = bf.Value()
	}
	f.name = baseFont
	info := &simpleFontInfo{missingWidth: 500}
	f.simple = info

	info.enc = d.resolveSimpleEncoding(fd, baseFont)

	if tu := d.parseToUnicodeEntry(fd); tu != nil {
		info.toUnicode = tu
		info.hasToUnicode = true
	}

	if fc, _, err := d.ctx.DereferenceIntegerEntry(fd, "FirstChar"); err == nil && fc != nil {
		info.first = fc.Value()
	}
	if lc, _, err := d.ctx.DereferenceIntegerEntry(fd, "LastChar"); err == nil && lc != nil {
		info.last = lc.Value()
	}
	if wa, err := d.ctx.DereferenceArray(fd["Widths"]); err == nil && wa != nil {
		for _, o := range wa {
			v, err := d.ctx.DereferenceNumber(o)
			if err != nil {
				return err
			}
			info.widths = append(info.widths, v)
		}
	}
	if n := coreFontName(baseFont); n != "" {
		info.isCore = true
		info.coreName = n
	}
	return nil
}

// coreFontName maps a base font name to its core font family name.
func coreFontName(baseFont string) string {
	switch baseFont {
	case "Helvetica", "Helvetica-Bold", "Helvetica-Oblique", "Helvetica-BoldOblique",
		"Times-Roman", "Times-Bold", "Times-Italic", "Times-BoldItalic",
		"Courier", "Courier-Bold", "Courier-Oblique", "Courier-BoldOblique",
		"Symbol", "ZapfDingbats":
		return baseFont
	}
	return ""
}

func (d *device) loadType0Font(f *pdfFont, fd types.Dict) error {
	if bf, _, err := d.ctx.DereferenceNameEntry(fd, "BaseFont"); err == nil && bf != nil {
		f.name = bf.Value()
	}
	info := &type0Info{codeLen: 2, dw: 1000, w: map[uint32]float64{}}
	f.t0 = info

	// Encoding: Identity-H/V or a predefined CMap name or a CMap stream.
	encName := ""
	o, found := fd.Find("Encoding")
	if !found || o == nil {
		return errors.New("type0 font: missing Encoding")
	}
	if n, err := d.ctx.DereferenceName(o, 0, nil); err == nil && n.Value() != "" {
		encName = n.Value()
	} else if _, _, err := d.ctx.DereferenceStreamDict(o); err == nil {
		// Embedded CMap stream: assume two byte codes.
		encName = "Identity-H"
	}
	info.codeLen = 1
	if isTwoByteCMap(encName) {
		info.codeLen = 2
	}
	info.ordering = encName

	if tu := d.parseToUnicodeEntry(fd); tu != nil {
		info.toUnicode = tu
	}

	// Descendant font.
	dfa, err := d.ctx.DereferenceArray(fd["DescendantFonts"])
	if err != nil {
		return fmt.Errorf("type0 font: descendant fonts: %w", err)
	}
	if len(dfa) == 0 {
		return errors.New("type0 font: missing descendant font")
	}
	dd, err := d.ctx.DereferenceDict(dfa[0])
	if err != nil {
		return fmt.Errorf("type0 font: descendant font: %w", err)
	}
	if dwv, _, err := d.ctx.DereferenceNumberEntry(dd, "DW"); err == nil && dwv != nil {
		info.dw = *dwv
	}
	if wa, err := d.ctx.DereferenceArray(dd["W"]); err == nil && wa != nil {
		i := 0
		for i < len(wa) {
			c, err := d.ctx.DereferenceNumber(wa[i])
			if err != nil {
				break
			}
			if i+1 < len(wa) {
				if a, err := d.ctx.DereferenceArray(wa[i+1]); err == nil && len(a) > 0 {
					// c [w1 w2 ...]
					for j, oo := range a {
						w, err := d.ctx.DereferenceNumber(oo)
						if err != nil {
							continue
						}
						info.w[uint32(c)+uint32(j)] = w
					}
					i += 2
					continue
				}
			}
			if i+2 < len(wa) {
				c2, err2 := d.ctx.DereferenceNumber(wa[i+1])
				w, err3 := d.ctx.DereferenceNumber(wa[i+2])
				if err2 == nil && err3 == nil {
					for k := uint32(c); k <= uint32(c2); k++ {
						info.w[k] = w
					}
				}
				i += 3
				continue
			}
			break
		}
	}
	// CIDSystemInfo ordering.
	if csi, err := d.ctx.DereferenceDict(dd["CIDSystemInfo"]); err == nil && csi != nil {
		if reg, _, err := d.ctx.DereferenceNameEntry(csi, "Registry"); err == nil && reg != nil {
			if ord, _, err := d.ctx.DereferenceNameEntry(csi, "Ordering"); err == nil && ord != nil {
				info.ordering = reg.Value() + "-" + ord.Value()
			}
		}
	}
	// CIDToGIDMap lives in the descendant's font descriptor context but is an
	// entry of the descendant dict itself.
	if o, found := dd.Find("CIDToGIDMap"); found && o != nil {
		if sd, _, err := d.ctx.DereferenceStreamDict(o); err == nil && sd != nil {
			if err := sd.DecodeWithLimit(d.decodeLimit()); err == nil {
				if len(sd.Content) >= 2 {
					m := map[uint32]uint32{}
					for i := 0; i+1 < len(sd.Content); i += 2 {
						gid := uint32(sd.Content[i])<<8 | uint32(sd.Content[i+1])
						if gid != 0 {
							m[uint32(i/2)] = gid
						}
					}
					info.cidToGid = m
				}
			}
		}
	}
	return nil
}

func isTwoByteCMap(name string) bool {
	// All predefined CMaps that matter here use two byte codes except some
	// rare 1-byte variants; Identity and Uni* are two byte.
	return true
}

func (d *device) loadType3Font(f *pdfFont, fd types.Dict) error {
	info := &type3Info{
		charProcs:  map[string]types.Object{},
		fontMatrix: mat{0.001, 0, 0, 0.001, 0, 0},
	}
	f.t3 = info
	if cp, err := d.ctx.DereferenceDict(fd["CharProcs"]); err == nil && cp != nil {
		for k, v := range cp {
			info.charProcs[k] = v
		}
	}
	if res, err := d.ctx.DereferenceDict(fd["Resources"]); err == nil && res != nil {
		info.resources = res
	}
	if fm, err := d.ctx.DereferenceArray(fd["FontMatrix"]); err == nil && len(fm) == 6 {
		vals := make([]float64, 6)
		ok := true
		for i, o := range fm {
			v, err := d.ctx.DereferenceNumber(o)
			if err != nil {
				ok = false
				break
			}
			vals[i] = v
		}
		if ok {
			info.fontMatrix = mat{vals[0], vals[1], vals[2], vals[3], vals[4], vals[5]}
		}
	}
	if fc, _, err := d.ctx.DereferenceIntegerEntry(fd, "FirstChar"); err == nil && fc != nil {
		info.first = fc.Value()
	}
	if lc, _, err := d.ctx.DereferenceIntegerEntry(fd, "LastChar"); err == nil && lc != nil {
		info.last = lc.Value()
	}
	if wa, err := d.ctx.DereferenceArray(fd["Widths"]); err == nil && wa != nil {
		for _, o := range wa {
			v, err := d.ctx.DereferenceNumber(o)
			if err != nil {
				return err
			}
			info.widths = append(info.widths, v)
		}
	}
	// Encoding differences map codes to char proc names.
	f.simple = &simpleFontInfo{enc: d.resolveSimpleEncoding(fd, "Type3"), missingWidth: 500}
	return nil
}

// parseToUnicodeEntry parses the ToUnicode entry of a font dictionary.
func (d *device) parseToUnicodeEntry(fd types.Dict) *toUnicodeCMap {
	o, found := fd.Find("ToUnicode")
	if !found || o == nil {
		return nil
	}
	sd, _, err := d.ctx.DereferenceStreamDict(o)
	if err != nil || sd == nil {
		return nil
	}
	if err := sd.DecodeWithLimit(d.decodeLimit()); err != nil {
		return nil
	}
	tu, err := parseToUnicode(sd.Content)
	if err != nil {
		return nil
	}
	return tu
}

// attachFontProgram loads the embedded font program or selects the fallback.
func (d *device) attachFontProgram(f *pdfFont, fd types.Dict) {
	var descendant types.Dict
	if f.isType0 {
		if dfa, err := d.ctx.DereferenceArray(fd["DescendantFonts"]); err == nil && len(dfa) > 0 {
			if dd, err := d.ctx.DereferenceDict(dfa[0]); err == nil {
				descendant = dd
			}
		}
	} else {
		descendant = fd
	}
	if descendant == nil {
		d.useFallbackFont(f)
		return
	}
	fp, err := d.ctx.DereferenceDict(descendant["FontDescriptor"])
	if err != nil || fp == nil {
		d.useFallbackFont(f)
		return
	}
	// FontFile2: TrueType. FontFile3: OpenType (SFNT) or bare CFF/Type1C.
	for _, key := range []string{"FontFile2", "FontFile3"} {
		ir := fp.IndirectRefEntry(key)
		if ir == nil {
			continue
		}
		sd, _, err := d.ctx.DereferenceStreamDict(types.Object(*ir))
		if err != nil || sd == nil {
			continue
		}
		if err := sd.DecodeWithLimit(d.decodeLimit()); err != nil {
			continue
		}
		bb := sd.Content
		if len(bb) == 0 {
			continue
		}
		sub := ""
		if st := sd.Subtype(); st != nil {
			sub = *st
		}
		if key == "FontFile3" && sub != "OpenType" {
			// Bare Type1C/CFF: parsed by the CFF outline engine.
			if cf, err := parseCFF(bb); err == nil {
				f.cff = cf
				f.upem = 1000
				if !cf.isCID && !cf.hasEncoding && len(cf.uniToGid) == 0 {
					// A non-CID Type1C without any usable mapping cannot be
					// resolved: fall back to the bundled font so the text
					// still renders (like PDFBox's substitution).
					d.useFallbackFont(f)
				}
				return
			}
			continue
		}
		sfont, err := sfnt.Parse(bb)
		if err == nil {
			f.sf = sfont
			f.upem = float64(sfont.UnitsPerEm())
			f.buf = &sfnt.Buffer{}
			return
		}
		// The strict sfnt parser rejects many valid subset fonts (missing
		// or crooked cmap tables are common after subsetting). Use the
		// tolerant TrueType reader for outlines; keep its cmap for Unicode
		// reverse lookup.
		if tf, err2 := parseTTF(bb); err2 == nil {
			f.ttf = tf
			f.upem = tf.upem
			if tf.cmap.format != 0 {
				f.ttfRev = tf.reverseCmap()
			}
			return
		}
	}
	d.useFallbackFont(f)
}

// reverseCmap builds a GID to Unicode map from the font's cmap.
func (tf *ttfFont) reverseCmap() map[uint16]rune {
	out := map[uint16]rune{}
	if tf.cmap.format == 4 {
		for i := 0; i < tf.cmap.segCount; i++ {
			lo, hi := tf.cmap.starts[i], tf.cmap.endCodes[i]
			if lo == 0xFFFF {
				continue
			}
			n := uint32(hi-lo) + 1
			if n > 20000 {
				n = 20000
			}
			for k := uint32(0); k < n; k++ {
				g := tf.glyphID(rune(lo + uint16(k)))
				if g != 0 {
					if _, ok := out[uint16(g)]; !ok {
						out[uint16(g)] = rune(lo + uint16(k))
					}
				}
			}
		}
	} else if tf.cmap.format == 12 {
		for _, g := range tf.cmap.groups {
			n := g.end - g.start + 1
			if n > 50000 {
				n = 50000
			}
			for k := uint32(0); k < n; k++ {
				gid := g.startGID + k
				if gid != 0 && gid < 0x10000 {
					if _, ok := out[uint16(gid)]; !ok {
						out[uint16(gid)] = rune(g.start + k)
					}
				}
			}
		}
	}
	return out
}

func (d *device) useFallbackFont(f *pdfFont) {
	ff, upem, err := loadFallback()
	if err != nil {
		// Without any font we cannot render glyphs at all.
		f.sf = nil
		return
	}
	f.sf = ff
	f.upem = upem
	f.fallback = true
}

// glyph describes one resolved glyph.
type glyph struct {
	gid     sfnt.GlyphIndex
	unicode rune
	w0      float64 // advance in 1000 unit glyph space
	t3Name  string  // Type3 char proc name
}

// hasEmbeddedOutlines reports whether glyph outlines come from an embedded
// font program.
func (f *pdfFont) hasEmbeddedOutlines() bool {
	return f.sf != nil && !f.fallback || f.cff != nil || f.ttf != nil
}

// resolveGlyph maps a character code of f to a drawable glyph.
func (f *pdfFont) resolveGlyph(d *device, code uint32) glyph {
	if f.isType0 {
		return f.resolveCIDGlyph(code)
	}
	if f.t3 != nil {
		return f.resolveType3Glyph(code)
	}
	b := byte(code)
	g := glyph{w0: f.simple.missingWidth}
	// Unicode.
	unicode := rune(0)
	if f.simple.hasToUnicode {
		if s, ok := f.simple.toUnicode.lookup(code); ok {
			for _, r := range decodeToUnicodeString(s) {
				unicode = r
				break
			}
		}
	}
	if unicode == 0 {
		unicode = f.simple.enc.runeFor(b)
	}
	g.unicode = unicode
	// Advance width.
	if idx := int(b) - f.simple.first; idx >= 0 && idx < len(f.simple.widths) {
		g.w0 = f.simple.widths[idx]
	} else if f.simple.isCore {
		g.w0 = float64(metrics.CoreFontCharWidth(f.simple.coreName, int(b)))
	}
	// GID.
	if f.cff != nil {
		// Bare Type1C simple font: the built-in encoding maps codes to GIDs;
		// subsets with a nearly empty encoding are mapped through glyph names
		// (code -> unicode -> name -> GID via the charset).
		if f.cff.hasEncoding {
			if gid := f.cff.encCodeToGid[b]; gid > 0 && gid < len(f.cff.charStrings) {
				g.gid = sfnt.GlyphIndex(gid)
				return g
			}
		}
		if unicode != 0 && len(f.cff.uniToGid) > 0 {
			if gid, ok := f.cff.uniToGid[unicode]; ok && gid > 0 && gid < len(f.cff.charStrings) {
				g.gid = sfnt.GlyphIndex(gid)
				return g
			}
		}
		return g
	}
	if f.sf != nil {
		if gid, err := f.sf.GlyphIndex(f.buffer(), unicode); err == nil && gid != 0 {
			g.gid = gid
			return g
		}
		// Symbol encoded embedded fonts map the code directly (cmap 3,0).
		if gid, err := f.sf.GlyphIndex(f.buffer(), rune(0xF000)|rune(unicode)); err == nil && gid != 0 {
			g.gid = gid
			return g
		}
		if !f.fallback {
			if gid, err := f.sf.GlyphIndex(f.buffer(), rune(code)); err == nil && gid != 0 {
				g.gid = gid
				return g
			}
		}
		return g
	}
	if f.ttf != nil && !f.fallback {
		// Tolerant TrueType engine: resolve through its cmap.
		if unicode != 0 {
			if gid := f.ttf.glyphID(unicode); gid != 0 {
				g.gid = sfnt.GlyphIndex(gid)
				return g
			}
		}
		// Symbol encoded fonts: try the raw character code.
		if gid := f.ttf.glyphID(rune(b)); gid != 0 {
			g.gid = sfnt.GlyphIndex(gid)
		}
	}
	return g
}

func (f *pdfFont) resolveCIDGlyph(code uint32) glyph {
	g := glyph{w0: f.t0.dw}
	cid := code
	// Unicode from ToUnicode (for the fallback path).
	if f.t0.toUnicode != nil {
		if s, ok := f.t0.toUnicode.lookup(code); ok {
			for _, r := range decodeToUnicodeString(s) {
				g.unicode = r
				break
			}
		}
	}
	// Missing ToUnicode entries: consult the embedded font's own cmap
	// (subset fonts often use custom glyph slots only documented there).
	if g.unicode == 0 && f.ttfRev != nil {
		if r, ok := f.ttfRev[uint16(cid)]; ok {
			g.unicode = r
		}
	}
	if w, ok := f.t0.w[cid]; ok {
		g.w0 = w
	}
	if f.hasEmbeddedOutlines() {
		if f.cff != nil {
			// CIDFontType0: the charstrings are indexed by GID; for CID-keyed
			// subset fonts (Founder PKU2 etc.) map CID -> GID through the CFF
			// charset, otherwise GID == CID.
			gid := int(code)
			if f.cff.cidToGid != nil {
				if g2, ok := f.cff.cidToGid[code]; ok {
					gid = g2
				} else {
					gid = 0 // not in subset: .notdef
				}
			}
			g.gid = sfnt.GlyphIndex(gid)
			return g
		}
		gid := sfnt.GlyphIndex(cid)
		if f.t0.cidToGid != nil {
			if g2, ok := f.t0.cidToGid[cid]; ok {
				gid = sfnt.GlyphIndex(g2)
			} else {
				gid = 0
			}
		}
		g.gid = gid
		return g
	}
	// Fallback font: try the code as Unicode or via the predefined CMap
	// encoding family (GBK/Big5/ShiftJIS/EUC-KR).
	if g.unicode == 0 {
		g.unicode = decodeCIDCode(f.t0.ordering, cid)
	}
	if f.sf != nil && g.unicode != 0 {
		if gid, err := f.sf.GlyphIndex(f.buffer(), g.unicode); err == nil {
			g.gid = gid
		}
	}
	return g
}

// decodeCIDCode decodes a raw CID according to the encoding family of the
// Type0 font's predefined CMap.
func decodeCIDCode(ordering string, cid uint32) rune {
	o := strings.ToLower(ordering)
	// Unicode passthrough encodings.
	if strings.Contains(o, "uni") || strings.Contains(o, "identity") || strings.Contains(o, "ucs2") ||
		strings.Contains(o, "utf16") {
		r := rune(cid)
		if unicode.IsPrint(r) {
			return r
		}
		return 0
	}
	lo, hi := byte(cid&0xFF), byte(cid>>8)
	b := []byte{hi, lo}
	var out []byte
	var err error
	switch {
	case strings.Contains(o, "gb"):
		out, err = simplifiedchinese.GBK.NewDecoder().Bytes(b)
	case strings.Contains(o, "cns") || strings.Contains(o, "b5"):
		out, err = traditionalchinese.Big5.NewDecoder().Bytes(b)
	case strings.Contains(o, "jis") || strings.Contains(o, "jp") || strings.Contains(o, "sjis"):
		out, err = japanese.ShiftJIS.NewDecoder().Bytes(b)
	case strings.Contains(o, "ks"):
		out, err = korean.EUCKR.NewDecoder().Bytes(b)
	default:
		r := rune(cid)
		if unicode.IsPrint(r) {
			return r
		}
		return 0
	}
	if err != nil || len(out) == 0 {
		return 0
	}
	r, _ := utf8.DecodeRune(out)
	return r
}

func (f *pdfFont) resolveType3Glyph(code uint32) glyph {
	g := glyph{w0: f.simple.missingWidth}
	b := byte(code)
	if idx := int(b) - f.t3.first; idx >= 0 && idx < len(f.t3.widths) {
		g.w0 = f.t3.widths[idx]
	}
	name := type3CharName(b, f.simple.enc)
	if _, ok := f.t3.charProcs[name]; ok {
		g.t3Name = name
	} else if cp, ok := f.t3.charProcs[fmt.Sprintf("%d", int(b))]; ok {
		_ = cp
		g.t3Name = fmt.Sprintf("%d", int(b))
	}
	return g
}

// type3CharName returns the char proc name for a code. Most Type3 fonts use
// glyph names from their Encoding Differences; without them we try the code
// number itself and common names.
func type3CharName(b byte, enc *simpleEnc) string {
	if r := enc.runeFor(b); r > 0x20 && r < 0x7F {
		return string(r)
	}
	return fmt.Sprintf("%d", int(b))
}

func (f *pdfFont) buffer() *sfnt.Buffer {
	if f.buf != nil {
		return f.buf
	}
	if f.sf != nil {
		if b, ok := d0FontBuffer(f.sf); ok {
			return b
		}
	}
	return &sfnt.Buffer{}
}

var globalBuffers sync.Map // *sfnt.Font -> *sfnt.Buffer

func d0FontBuffer(f *sfnt.Font) (*sfnt.Buffer, bool) {
	v, ok := globalBuffers.Load(f)
	if !ok {
		v, _ = globalBuffers.LoadOrStore(f, &sfnt.Buffer{})
	}
	b, _ := v.(*sfnt.Buffer)
	return b, b != nil
}

// glyphOutline returns the cached flattened outline of a glyph in 1000-unit
// per em glyph space with the y axis pointing down.
func (d *device) glyphOutline(f *pdfFont, gid sfnt.GlyphIndex) []subPath {
	key := glyphCacheKey{f, gid}
	if sps, ok := d.glyphs[key]; ok {
		return sps
	}
	var sps []subPath
	// Outline units per em of the engine that produced the outline (varies by
	// font: 256 for the FZ subsets, 1000 for most CJK fonts, 2048 for many
	// TrueType fonts). drawGlyph assumes 1000 units per em, so normalize.
	upem := 1000.0
	switch {
	case !f.fallback && f.cff != nil:
		sps = cffGlyphOutline(f.cff, int(gid))
		upem = f.cff.upem
	case f.ttf != nil:
		sps = ttfGlyphOutline(f.ttf, int(gid), 0)
		upem = f.ttf.upem
	case f.sf != nil && gid != 0:
		sps = d.sfntGlyphOutline(f, gid)
		upem = f.upem
	}
	if len(sps) > 0 && upem > 0 && upem != 1000 {
		sps = transformPath(sps, scaling(1000/upem, 1000/upem))
	}
	if len(d.glyphs) < 20000 {
		d.glyphs[key] = sps
	}
	return sps
}

func (d *device) sfntGlyphOutline(f *pdfFont, gid sfnt.GlyphIndex) []subPath {
	if f.sf == nil {
		return nil
	}
	// Request coordinates in font units by using ppem == unitsPerEm.
	// A fresh buffer per call: a shared sfnt.Buffer must be discarded after
	// any error, otherwise every later LoadGlyph fails silently.
	buf := &sfnt.Buffer{}
	ppem := fixed.Int26_6(int64(f.upem) << 6)
	segs, err := f.sf.LoadGlyph(buf, gid, ppem, nil)
	if err != nil {
		return nil
	}
	var sps []subPath
	var cur subPath
	var curPt pt
	tol := math.Max(8, f.upem/100)
	p26 := func(p fixed.Point26_6) pt {
		return pt{float64(p.X) / 64, float64(p.Y) / 64}
	}
	for _, seg := range segs {
		switch seg.Op {
		case sfnt.SegmentOpMoveTo:
			if len(cur.pts) > 1 {
				cur.close()
				sps = append(sps, cur)
			}
			cur = subPath{}
			curPt = p26(seg.Args[0])
			cur.pts = append(cur.pts, curPt)
		case sfnt.SegmentOpLineTo:
			curPt = p26(seg.Args[0])
			cur.pts = append(cur.pts, curPt)
		case sfnt.SegmentOpQuadTo:
			ctrl := p26(seg.Args[0])
			end := p26(seg.Args[1])
			cur.pts = flattenQuad(cur.pts, curPt, ctrl, end, tol)
			curPt = end
		case sfnt.SegmentOpCubeTo:
			c1 := p26(seg.Args[0])
			c2 := p26(seg.Args[1])
			end := p26(seg.Args[2])
			cur.pts = flattenCubic(cur.pts, curPt, c1, c2, end, tol)
			curPt = end
		}
	}
	if len(cur.pts) > 1 {
		cur.close()
		sps = append(sps, cur)
	}
	return sps
}
