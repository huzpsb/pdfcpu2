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
	"strconv"
	"strings"

	coremetrics "github.com/huzpsb/pdfcpu2/internal/corefont/metrics"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
	"golang.org/x/text/encoding/charmap"
)

// simpleEnc maps single byte character codes to Unicode runes.
type simpleEnc struct {
	table     [256]rune
	isTwoByte bool // reserved for future use
}

func newSimpleEnc(base [256]rune) *simpleEnc {
	e := &simpleEnc{}
	e.table = base
	return e
}

// runeFor returns the rune for a character code.
func (e *simpleEnc) runeFor(c byte) rune {
	r := e.table[c]
	if r == 0 {
		return rune(c)
	}
	return r
}

func latin1Base() [256]rune {
	var t [256]rune
	for i := 0; i < 256; i++ {
		t[i] = rune(i)
	}
	return t
}

func charmapBase(cm *charmap.Charmap) [256]rune {
	var t [256]rune
	for i := 0; i < 256; i++ {
		t[i] = cm.DecodeByte(byte(i))
	}
	return t
}

// glyphMapBase derives a byte->rune table from a metrics glyph map.
func glyphMapBase(m map[int]string) [256]rune {
	var t [256]rune
	for c, name := range m {
		if r, ok := glyphNameToRune(name); ok {
			t[c] = r
		}
	}
	return t
}

var (
	encWinAnsi  = newSimpleEnc(charmapBase(charmap.Windows1252))
	encMacRoman = newSimpleEnc(charmapBase(charmap.Macintosh))
	encLatin1   = newSimpleEnc(latin1Base())
	encStandard = newSimpleEnc(glyphMapBase(coremetrics.StandardGlyphMap))
	encSymbol   = newSimpleEnc(glyphMapBase(coremetrics.SymbolGlyphMap))
	encZapf     = newSimpleEnc(glyphMapBase(coremetrics.ZapfDingbatsGlyphMap))
)

// baseEncodingByName resolves the predefined simple encodings.
func baseEncodingByName(name string) *simpleEnc {
	switch name {
	case "WinAnsiEncoding":
		return encWinAnsi
	case "MacRomanEncoding":
		return encMacRoman
	case "StandardEncoding":
		return encStandard
	case "PDFDocEncoding":
		return encLatin1
	case "Symbol":
		return encSymbol
	case "ZapfDingbats":
		return encZapf
	}
	return nil
}

// resolveSimpleEncoding determines the effective byte->rune mapping of a
// simple font given its font dictionary and base font name.
func (d *device) resolveSimpleEncoding(fontDict types.Dict, baseFont string) *simpleEnc {
	enc := encWinAnsi
	lower := strings.ToLower(baseFont)
	isSymbol := strings.Contains(lower, "symbol")
	isZapf := strings.Contains(lower, "zapfdingbats")
	if isSymbol {
		enc = encSymbol
	} else if isZapf {
		enc = encZapf
	}

	o, found := fontDict.Find("Encoding")
	if found && o != nil {
		if name, err := d.ctx.DereferenceName(o, 0, nil); err == nil && name.Value() != "" {
			if e := baseEncodingByName(name.Value()); e != nil {
				enc = e
			}
		} else if ed, err := d.ctx.DereferenceDict(o); err == nil && ed != nil {
			base := encLatin1
			if bn, _, err := d.ctx.DereferenceNameEntry(ed, "BaseEncoding"); err == nil && bn != nil {
				if e := baseEncodingByName(bn.Value()); e != nil {
					base = e
				}
			}
			merged := *newSimpleEnc(base.table)
			if diff, err := d.ctx.DereferenceArray(ed["Differences"]); err == nil && len(diff) > 0 {
				code := 0
				for _, oo := range diff {
					if iv, err := d.ctx.DereferenceInteger(oo); err == nil && iv != nil {
						code = iv.Value()
						continue
					}
					if nm, err := d.ctx.DereferenceName(oo, 0, nil); err == nil && nm.Value() != "" {
						if r, ok := glyphNameToRune(nm.Value()); ok && code >= 0 && code < 256 {
							merged.table[code] = r
						}
						code++
					}
				}
			}
			enc = &merged
		}
	}
	return enc
}

// glyphNameToRune resolves an Adobe glyph name to a Unicode rune using a
// practical subset of the Adobe Glyph List.
func glyphNameToRune(name string) (rune, bool) {
	if name == "" {
		return 0, false
	}
	if len(name) == 1 {
		c := name[0]
		if c >= 33 && c <= 126 {
			return rune(c), true
		}
	}
	// uniXXXX and uXXXXX forms.
	if strings.HasPrefix(name, "uni") && len(name) == 7 {
		if v, err := strconv.ParseUint(name[3:], 16, 32); err == nil {
			return rune(v), true
		}
	}
	if strings.HasPrefix(name, "u") && len(name) >= 5 && len(name) <= 7 {
		if v, err := strconv.ParseUint(name[1:], 16, 32); err == nil {
			return rune(v), true
		}
	}
	if r, ok := aglNames[name]; ok {
		return r, true
	}
	return 0, false
}

// aglNames is a practical subset of the Adobe Glyph List covering Latin-1
// supplement names, Greek letters and common mathematical/dingbat names.
var aglNames = buildAGL()

func buildAGL() map[string]rune {
	entries := map[string]rune{
		"space": 0x0020, "exclam": 0x0021, "quotedbl": 0x0022, "numbersign": 0x0023,
		"dollar": 0x0024, "percent": 0x0025, "ampersand": 0x0026, "quotesingle": 0x0027,
		"parenleft": 0x0028, "parenright": 0x0029, "asterisk": 0x002A, "plus": 0x002B,
		"comma": 0x002C, "hyphen": 0x002D, "period": 0x002E, "slash": 0x002F,
		"zero": 0x0030, "one": 0x0031, "two": 0x0032, "three": 0x0033, "four": 0x0034,
		"five": 0x0035, "six": 0x0036, "seven": 0x0037, "eight": 0x0038, "nine": 0x0039,
		"colon": 0x003A, "semicolon": 0x003B, "less": 0x003C, "equal": 0x003D,
		"greater": 0x003E, "question": 0x003F, "at": 0x0040,
		"bracketleft": 0x005B, "backslash": 0x005C, "bracketright": 0x005D,
		"asciicircum": 0x005E, "underscore": 0x005F, "grave": 0x0060,
		"braceleft": 0x007B, "bar": 0x007C, "braceright": 0x007D, "asciitilde": 0x007E,
		"exclamdown": 0x00A1, "cent": 0x00A2, "sterling": 0x00A3, "currency": 0x00A4,
		"yen": 0x00A5, "brokenbar": 0x00A6, "section": 0x00A7, "dieresis": 0x00A8,
		"copyright": 0x00A9, "ordfeminine": 0x00AA, "guillemotleft": 0x00AB,
		"logicalnot": 0x00AC, "registered": 0x00AE, "macron": 0x00AF, "degree": 0x00B0,
		"plusminus": 0x00B1, "acute": 0x00B4, "mu": 0x00B5, "paragraph": 0x00B6,
		"periodcentered": 0x00B7, "cedilla": 0x00B8, "ordmasculine": 0x00BA,
		"guillemotright": 0x00BB, "onequarter": 0x00BC, "onehalf": 0x00BD,
		"threequarters": 0x00BE, "questiondown": 0x00BF,
		"Agrave": 0x00C0, "Aacute": 0x00C1, "Acircumflex": 0x00C2, "Atilde": 0x00C3,
		"Adieresis": 0x00C4, "Aring": 0x00C5, "AE": 0x00C6, "Ccedilla": 0x00C7,
		"Egrave": 0x00C8, "Eacute": 0x00C9, "Ecircumflex": 0x00CA, "Edieresis": 0x00CB,
		"Igrave": 0x00CC, "Iacute": 0x00CD, "Icircumflex": 0x00CE, "Idieresis": 0x00CF,
		"Eth": 0x00D0, "Ntilde": 0x00D1, "Ograve": 0x00D2, "Oacute": 0x00D3,
		"Ocircumflex": 0x00D4, "Otilde": 0x00D5, "Odieresis": 0x00D6, "multiply": 0x00D7,
		"Oslash": 0x00D8, "Ugrave": 0x00D9, "Uacute": 0x00DA, "Ucircumflex": 0x00DB,
		"Udieresis": 0x00DC, "Yacute": 0x00DD, "Thorn": 0x00DE, "germandbls": 0x00DF,
		"agrave": 0x00E0, "aacute": 0x00E1, "acircumflex": 0x00E2, "atilde": 0x00E3,
		"adieresis": 0x00E4, "aring": 0x00E5, "ae": 0x00E6, "ccedilla": 0x00E7,
		"egrave": 0x00E8, "eacute": 0x00E9, "ecircumflex": 0x00EA, "edieresis": 0x00EB,
		"igrave": 0x00EC, "iacute": 0x00ED, "icircumflex": 0x00EE, "idieresis": 0x00EF,
		"eth": 0x00F0, "ntilde": 0x00F1, "ograve": 0x00F2, "oacute": 0x00F3,
		"ocircumflex": 0x00F4, "otilde": 0x00F5, "odieresis": 0x00F6, "divide": 0x00F7,
		"oslash": 0x00F8, "ugrave": 0x00F9, "uacute": 0x00FA, "ucircumflex": 0x00FB,
		"udieresis": 0x00FC, "yacute": 0x00FD, "thorn": 0x00FE, "ydieresis": 0x00FF,
		"quotedblleft": 0x201C, "quotedblright": 0x201D, "quoteleft": 0x2018,
		"quoteright": 0x2019, "quotesinglbase": 0x201A, "quotedblbase": 0x201E,
		"endash": 0x2013, "emdash": 0x2014, "bullet": 0x2022, "dagger": 0x2020,
		"daggerdbl": 0x2021, "ellipsis": 0x2026, "perthousand": 0x2030,
		"guilsinglleft": 0x2039, "guilsinglright": 0x203A, "fraction": 0x2044,
		"florin": 0x0192, "Euro": 0x20AC, "trademark": 0x2122, "Lslash": 0x0141,
		"lslash": 0x0142, "OE": 0x0152, "oe": 0x0153, "Scaron": 0x0160, "scaron": 0x0161,
		"Ydieresis": 0x0178, "Zcaron": 0x017D, "zcaron": 0x017E, "fi": 0xFB01,
		"fl": 0xFB02, "middot": 0x00B7,
		// Greek letters for the Symbol font.
		"alpha": 0x03B1, "beta": 0x03B2, "gamma": 0x03B3, "delta": 0x03B4,
		"epsilon": 0x03B5, "zeta": 0x03B6, "eta": 0x03B7, "theta": 0x03B8,
		"iota": 0x03B9, "kappa": 0x03BA, "lambda": 0x03BB, "nu": 0x03BD,
		"xi": 0x03BE, "omicron": 0x03BF, "pi": 0x03C0, "rho": 0x03C1,
		"sigma": 0x03C3, "tau": 0x03C4, "upsilon": 0x03C5, "phi": 0x03C6,
		"chi": 0x03C7, "psi": 0x03C8, "omega": 0x03C9,
		"Alpha": 0x0391, "Beta": 0x0392, "Gamma": 0x0393, "Delta": 0x0394,
		"Epsilon": 0x0395, "Zeta": 0x0396, "Eta": 0x0397, "Theta": 0x0398,
		"Iota": 0x0399, "Kappa": 0x039A, "Lambda": 0x039B, "Nu": 0x039D,
		"Xi": 0x039E, "Omicron": 0x039F, "Pi": 0x03A0, "Rho": 0x03A1,
		"Sigma": 0x03A3, "Tau": 0x03A4, "Upsilon": 0x03A5, "Phi": 0x03A6,
		"Chi": 0x03A7, "Psi": 0x03A8, "Omega": 0x03A9,
		// Math symbols used by Symbol.
		"universal": 0x2200, "existential": 0x2203, "partialdiff": 0x2202,
		"nabla": 0x2207, "element": 0x2208, "notelement": 0x2209,
		"contains": 0x220B, "emptyset": 0x2205, "infinity": 0x221E,
		"integral": 0x222B, "summation": 0x2211, "product": 0x220F,
		"radical": 0x221A, "proportional": 0x221D, "angle": 0x2220,
		"logicaland": 0x2227, "logicalor": 0x2228, "intersection": 0x2229,
		"union": 0x222A, "equivalence": 0x2261,
		"greaterequal": 0x2265, "lessequal": 0x2264, "notequal": 0x2260,
		"identical": 0x2261, "approxequal": 0x2248, "similar": 0x223C,
		"arrowboth": 0x2194, "arrowleft": 0x2190, "arrowup": 0x2191,
		"arrowright": 0x2192, "arrowdown": 0x2193, "minus": 0x2212,
		"club": 0x2663, "diamond": 0x2666,
		"heart": 0x2665, "spade": 0x2660, "aleph": 0x2135, "weierstrass": 0x2118,
		"circlemultiply": 0x2297, "circleplus": 0x2295,
	}
	return entries
}
