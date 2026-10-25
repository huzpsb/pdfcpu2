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
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// toUnicodeCMap is the decoding table derived from a ToUnicode CMap stream.
type toUnicodeCMap struct {
	// mappings maps character codes to Unicode strings.
	mappings map[uint32]string
	// codeLen is the byte length of character codes (1, 2 or 4).
	codeLen int
}

// parseToUnicodeCMap parses the PostScript-like CMap syntax of a ToUnicode stream
// and extracts codespace, bfchar and bfrange mappings.
func parseToUnicodeCMap(bb []byte) (*toUnicodeCMap, error) {
	cu := &toUnicodeCMap{mappings: map[uint32]string{}}

	s := newCMapScanner(bb)
	maxLen := 0

	for {
		tok, err := s.next()
		if err != nil {
			return nil, err
		}
		if tok == "" {
			break
		}

		switch tok {
		case "begincodespacerange":
			n, err := cu.parseCodespaceRange(s, &maxLen)
			if err != nil {
				return nil, err
			}
			_ = n
		case "beginbfchar":
			if err := cu.parseBFChar(s); err != nil {
				return nil, err
			}
		case "beginbfrange":
			if err := cu.parseBFRange(s); err != nil {
				return nil, err
			}
		}
	}

	cu.codeLen = maxLen
	if cu.codeLen <= 0 {
		cu.codeLen = 2
	}
	return cu, nil
}

// parseCodespaceRange consumes a codespacerange block.
func (cu *toUnicodeCMap) parseCodespaceRange(s *cmapScanner, maxLen *int) (int, error) {
	n := 0
	for {
		tok, err := s.next()
		if err != nil {
			return n, err
		}
		switch {
		case tok == "endcodespacerange" || tok == "":
			return n, nil
		case isHexToken(tok):
			b, err := hexTokenBytes(tok)
			if err != nil {
				return n, fmt.Errorf("cmap: %w", err)
			}
			if len(b) > *maxLen {
				*maxLen = len(b)
			}
			n++
		}
		if n > 4096 {
			return n, fmt.Errorf("cmap: codespacerange exceeds %d entries", 4096)
		}
	}
}

// parseBFChar consumes a bfchar block.
func (cu *toUnicodeCMap) parseBFChar(s *cmapScanner) error {
	var src []byte
	n := 0
	for {
		tok, err := s.next()
		if err != nil {
			return err
		}
		switch {
		case tok == "endbfchar" || tok == "":
			return nil
		case tok == "[" || tok == "]":
			// Arrays are not allowed in bfchar, skip defensively.
		case isHexToken(tok):
			b, err := hexTokenBytes(tok)
			if err != nil {
				return fmt.Errorf("cmap: %w", err)
			}
			if src == nil {
				src = b
			} else {
				cu.add(src, b)
				src = nil
				n++
			}
		}
		if n > 65536 {
			return fmt.Errorf("cmap: bfchar block exceeds %d entries", 65536)
		}
	}
}

// parseBFRange consumes a bfrange block with hex or array destinations.
func (cu *toUnicodeCMap) parseBFRange(s *cmapScanner) error {
	n := 0
	for {
		tok, err := s.next()
		if err != nil {
			return err
		}
		if tok == "endbfrange" || tok == "" {
			return nil
		}
		if !isHexToken(tok) {
			continue
		}

		lo, err := hexTokenBytes(tok)
		if err != nil {
			return fmt.Errorf("cmap: %w", err)
		}

		tok, err = s.next()
		if err != nil {
			return err
		}
		if !isHexToken(tok) {
			continue
		}
		hi, err := hexTokenBytes(tok)
		if err != nil {
			return fmt.Errorf("cmap: %w", err)
		}

		tok, err = s.next()
		if err != nil {
			return err
		}

		switch {
		case isHexToken(tok):
			dst, err := hexTokenBytes(tok)
			if err != nil {
				return fmt.Errorf("cmap: %w", err)
			}
			cu.addRangeEntry(lo, hi, dst)
			n++
		case tok == "[":
			var dsts [][]byte
			for {
				tok, err = s.next()
				if err != nil {
					return err
				}
				if tok == "]" || tok == "" {
					break
				}
				if !isHexToken(tok) {
					continue
				}
				dst, err := hexTokenBytes(tok)
				if err != nil {
					return fmt.Errorf("cmap: %w", err)
				}
				dsts = append(dsts, dst)
				if len(dsts) > 65536 {
					return fmt.Errorf("cmap: bfrange array exceeds %d entries", 65536)
				}
			}
			cu.addArrayEntry(lo, dsts)
			n++
		}

		if n > 65536 {
			return fmt.Errorf("cmap: bfrange block exceeds %d entries", 65536)
		}
	}
}

func isHexToken(tok string) bool {
	return len(tok) >= 3 && tok[0] == '<' && tok[len(tok)-1] == '>'
}

// hexTokenBytes converts a scanner hex token like <0041> into raw bytes.
func hexTokenBytes(tok string) ([]byte, error) {
	return hexBytes(tok[1 : len(tok)-1])
}

// add records a single bfchar mapping src -> dst.
func (cu *toUnicodeCMap) add(src, dst []byte) {
	if len(src) == 0 {
		return
	}
	cu.mappings[codeOfBytes(src)] = utf16BytesToString(dst)
}

// addRangeEntry maps lo..hi to dst, dst+1, ... by incrementing the final
// UTF-16 code unit of dst.
func (cu *toUnicodeCMap) addRangeEntry(lo, hi, dst []byte) {
	loC, hiC := codeOfBytes(lo), codeOfBytes(hi)
	if len(lo) == 0 || hiC < loC || hiC-loC > 65535 {
		return
	}
	dstUnits := utf16BytesToUnits(dst)
	for d := uint32(0); loC+d <= hiC; d++ {
		cu.mappings[loC+d] = unitsToString(shiftUnits(dstUnits, d))
	}
}

// addArrayEntry maps lo+k to dsts[k].
func (cu *toUnicodeCMap) addArrayEntry(lo []byte, dsts [][]byte) {
	if len(lo) == 0 {
		return
	}
	loC := codeOfBytes(lo)
	for i, dst := range dsts {
		cu.mappings[loC+uint32(i)] = utf16BytesToString(dst)
	}
}

// lookup decodes code according to codeLen and returns its Unicode string.
func (cu *toUnicodeCMap) lookup(code uint32) (string, bool) {
	s, ok := cu.mappings[code]
	return s, ok
}

func codeOfBytes(b []byte) uint32 {
	var c uint32
	for _, v := range b {
		c = c<<8 | uint32(v)
	}
	return c
}

func hexBytes(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
	if len(s)%2 == 1 {
		s += "0"
	}
	bb := make([]byte, len(s)/2)
	for i := 0; i < len(bb); i++ {
		v, err := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("invalid hex string <%s>: %w", s, err)
		}
		bb[i] = byte(v)
	}
	return bb, nil
}

// utf16BytesToUnits returns the raw UTF-16 code units of a BOM-less destination string.
func utf16BytesToUnits(b []byte) []uint16 {
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		b = b[2:]
	}
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return units
}

func unitsToString(units []uint16) string {
	return string(utf16.Decode(units))
}

// shiftUnits returns a copy of units with delta added to the final code unit.
func shiftUnits(units []uint16, delta uint32) []uint16 {
	if len(units) == 0 {
		return nil
	}
	c := append([]uint16(nil), units...)
	c[len(c)-1] += uint16(delta)
	return c
}

func utf16BytesToString(b []byte) string {
	return unitsToString(utf16BytesToUnits(b))
}

// cmapScanner tokenizes CMap data into keywords, numbers, names, hex strings
// and the array delimiters "[" and "]".
type cmapScanner struct {
	bb  []byte
	pos int
}

func newCMapScanner(bb []byte) *cmapScanner {
	return &cmapScanner{bb: bb}
}

func isCMapWhitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == 0
}

// next returns the next token or "" at end of input.
func (s *cmapScanner) next() (string, error) {
	for s.pos < len(s.bb) {
		c := s.bb[s.pos]
		if isCMapWhitespace(c) {
			s.pos++
			continue
		}
		if c == '%' {
			for s.pos < len(s.bb) && s.bb[s.pos] != '\n' && s.bb[s.pos] != '\r' {
				s.pos++
			}
			continue
		}
		break
	}
	if s.pos >= len(s.bb) {
		return "", nil
	}

	c := s.bb[s.pos]
	switch {
	case c == '<':
		if s.pos+1 < len(s.bb) && s.bb[s.pos+1] == '<' {
			// Dictionary: skip to matching >>.
			s.pos += 2
			depth := 1
			for s.pos < len(s.bb) && depth > 0 {
				if s.bb[s.pos] == '<' && s.pos+1 < len(s.bb) && s.bb[s.pos+1] == '<' {
					depth++
					s.pos += 2
					continue
				}
				if s.bb[s.pos] == '>' && s.pos+1 < len(s.bb) && s.bb[s.pos+1] == '>' {
					depth--
					s.pos += 2
					continue
				}
				s.pos++
			}
			return "<<dict>>", nil
		}
		end := strings.IndexByte(string(s.bb[s.pos:]), '>')
		if end < 0 {
			return "", fmt.Errorf("cmap: unterminated hex string")
		}
		tok := string(s.bb[s.pos+1 : s.pos+end])
		s.pos += end + 1
		return "<" + tok + ">", nil
	case c == '[':
		s.pos++
		return "[", nil
	case c == ']':
		s.pos++
		return "]", nil
	case c == '(':
		// Literal string: skip balanced.
		depth := 1
		s.pos++
		for s.pos < len(s.bb) && depth > 0 {
			switch s.bb[s.pos] {
			case '\\':
				s.pos++
			case '(':
				depth++
			case ')':
				depth--
			}
			s.pos++
		}
		return "(string)", nil
	case c == '/':
		start := s.pos + 1
		s.pos++
		for s.pos < len(s.bb) && !isCMapWhitespace(s.bb[s.pos]) && !strings.ContainsRune("[]/<>()%", rune(s.bb[s.pos])) {
			s.pos++
		}
		if s.pos == start {
			// A lone slash, e.g. //Name: return it as an ignorable token
			// instead of the end of input marker.
			return "/", nil
		}
		return string(s.bb[start:s.pos]), nil
	default:
		start := s.pos
		for s.pos < len(s.bb) && !isCMapWhitespace(s.bb[s.pos]) && !strings.ContainsRune("[]/<>()%", rune(s.bb[s.pos])) {
			s.pos++
		}
		return string(s.bb[start:s.pos]), nil
	}
}

// glyphNameToString maps a PostScript glyph name to its Unicode text based on a
// subset of the Adobe Glyph List supplemented by structured names of the form
// uniXXXX.
func glyphNameToString(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if name == "Th" {
		// Ligature used by various Type1 subsets.
		return "Th", true
	}
	if r, ok := glyphNameToRune(name); ok {
		return string(r), true
	}
	return "", false
}

func glyphNameToRune(name string) (rune, bool) {
	if name == "" {
		return utf8.RuneError, false
	}
	if strings.HasPrefix(name, "uni") && len(name) >= 7 {
		if v, err := strconv.ParseUint(name[3:7], 16, 32); err == nil {
			r := rune(v)
			if r != 0 && !utf16.IsSurrogate(r) && utf8.ValidRune(r) {
				return r, true
			}
		}
	}
	if strings.HasPrefix(name, "u") && len(name) >= 5 {
		if v, err := strconv.ParseUint(name[1:5], 16, 32); err == nil {
			r := rune(v)
			if r != 0 && !utf16.IsSurrogate(r) && utf8.ValidRune(r) {
				return r, true
			}
		}
	}
	if len(name) == 1 {
		r := rune(name[0])
		if r >= 0x20 && r < 0x7F {
			return r, true
		}
	}
	if r, ok := glyphList[name]; ok {
		return r, true
	}
	if n, ok := numberWordToRune(name); ok {
		return n, true
	}
	return utf8.RuneError, false
}

func numberWordToRune(name string) (rune, bool) {
	switch name {
	case "zero":
		return '0', true
	case "one":
		return '1', true
	case "two":
		return '2', true
	case "three":
		return '3', true
	case "four":
		return '4', true
	case "five":
		return '5', true
	case "six":
		return '6', true
	case "seven":
		return '7', true
	case "eight":
		return '8', true
	case "nine":
		return '9', true
	}
	return utf8.RuneError, false
}

// simpleEncoding is a byte to Unicode mapping for 8-bit simple font encodings.
type simpleEncoding struct {
	runes [256]rune
	// multi holds multi-character mappings from Encoding Differences.
	multi map[byte]string
}

func (se *simpleEncoding) applyDifferences(diffs map[byte]string) {
	for code, name := range diffs {
		if s, ok := glyphNameToString(name); ok {
			if r := []rune(s); len(r) == 1 {
				se.runes[code] = r[0]
				continue
			}
			// Multi character mappings cannot be represented in the rune table.
			if se.multi == nil {
				se.multi = map[byte]string{}
			}
			se.multi[code] = s
		}
		// Unknown glyph names stay unmapped and decode to the Latin-1
		// fallback of their code, matching common reference extractors.
	}
}

func (se *simpleEncoding) decode(bb []byte) string {
	var sb strings.Builder
	for _, b := range bb {
		if se.multi != nil {
			if s, ok := se.multi[b]; ok {
				sb.WriteString(s)
				continue
			}
		}
		r := se.runes[b]
		if r == 0 {
			// Unmapped codes fall back to their Latin-1 interpretation,
			// matching common reference extractors.
			r = rune(b)
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

var (
	winAnsiEncoding  *simpleEncoding
	macRomanEncoding *simpleEncoding
	stdEncoding      *simpleEncoding
	latin1Encoding   *simpleEncoding
)

func init() {
	winAnsiEncoding = encodingFromCharmap(charmap.Windows1252)
	macRomanEncoding = encodingFromCharmap(charmap.Macintosh)
	latin1Encoding = encodingFromCharmap(charmap.ISO8859_1)
	stdEncoding = standardEncoding()
}

func encodingFromCharmap(cm *charmap.Charmap) *simpleEncoding {
	var se simpleEncoding
	for b := 0; b < 256; b++ {
		se.runes[b] = cm.DecodeByte(byte(b))
	}
	return &se
}

// standardEncoding implements the PDF StandardEncoding (Annex B.2) which
// is ASCII for 0x20..0x7E with a distinct high range.
func standardEncoding() *simpleEncoding {
	var se simpleEncoding
	for b := 0x20; b <= 0x7E; b++ {
		se.runes[b] = rune(b)
	}
	high := map[byte]rune{
		0xA1: 0x00A1, 0xA2: 0x00A2, 0xA3: 0x00A3, 0xA4: 0x2044, 0xA5: 0x00A5, 0xA6: 0x0192,
		0xA7: 0x00A7, 0xA8: 0x00A4, 0xA9: '\'', 0xAA: 0x201C, 0xAB: 0x00AB, 0xAC: 0x2039,
		0xAD: 0x203A, 0xAE: 0xFB01, 0xAF: 0xFB02, 0xB1: 0x2013, 0xB2: 0x2020, 0xB3: 0x2021,
		0xB4: 0x00B7, 0xB6: 0x00B6, 0xB7: 0x2022, 0xB8: 0x201A, 0xB9: 0x201E, 0xBA: 0x201D,
		0xBB: 0x00BB, 0xBC: 0x2026, 0xBD: 0x2122, 0xBF: 0x2030, 0xC1: 0x00C0, 0xC2: 0x00C1,
		0xC3: 0x00C2, 0xC4: 0x00C3, 0xC5: 0x00C4, 0xC7: 0x00C7, 0xC8: 0x00C8, 0xC9: 0x00C9,
		0xCA: 0x00CA, 0xCB: 0x00CB, 0xCC: 0x00CC, 0xCD: 0x00CD, 0xCE: 0x00CE, 0xCF: 0x00CF,
		0xD1: 0x00D1, 0xD2: 0x00D2, 0xD3: 0x00D3, 0xD4: 0x00D4, 0xD5: 0x00D5, 0xD6: 0x00D6,
		0xD8: 0x0152, 0xD9: 0x00D9, 0xDA: 0x00DA, 0xDB: 0x00DB, 0xDC: 0x00DC, 0xDD: 0x00DD,
		0xDE: 0x00DF, 0xE1: 0x00E0, 0xE2: 0x00E1, 0xE3: 0x00E2, 0xE4: 0x00E3, 0xE5: 0x00E4,
		0xE7: 0x00E7, 0xE8: 0x00E8, 0xE9: 0x00E9, 0xEA: 0x00EA, 0xEB: 0x00EB, 0xEC: 0x00EC,
		0xED: 0x00ED, 0xEE: 0x00EE, 0xEF: 0x00EF, 0xF1: 0x00F1, 0xF2: 0x00F2, 0xF3: 0x00F3,
		0xF4: 0x00F4, 0xF5: 0x00F5, 0xF6: 0x00F6, 0xF8: 0x0153, 0xF9: 0x00F9, 0xFA: 0x00FA,
		0xFB: 0x00FB, 0xFC: 0x00FC, 0xFD: 0x00FD, 0xFE: 0x00FF,
	}
	for b, r := range high {
		se.runes[b] = r
	}
	return &se
}

// glyphList is a subset of the Adobe Glyph List covering the glyph names
// used by the predefined simple font encodings and common custom encodings.
var glyphList = map[string]rune{
	// Ligatures.
	"fi": 0xFB01, "fl": 0xFB02, "ff": 0xFB00, "ffi": 0xFB03, "ffl": 0xFB04,
	// Quotes and dashes.
	"quoteright": 0x2019, "quotesingle": '\'', "quotedblleft": 0x201C,
	"quotedblright": 0x201D, "quotedbl": '"', "quoteleft": 0x2018,
	"quotesinglbase": 0x201A, "quotedblbase": 0x201E,
	"endash": 0x2013, "emdash": 0x2014, "hyphen": '-', "bullet": 0x2022,
	// Spaces.
	"space": ' ', "nbsp": 0x00A0,
	// Currency.
	"dollar": '$', "cent": 0x00A2, "sterling": 0x00A3, "currency": 0x00A4,
	"yen": 0x00A5, "Euro": 0x20AC, "florin": 0x0192,
	// Punctuation.
	"period": '.', "comma": ',', "colon": ':', "semicolon": ';',
	"exclam": '!', "exclamdown": 0x00A1, "question": '?', "questiondown": 0x00BF,
	"slash": '/', "backslash": '\\', "bar": '|', "underscore": '_',
	"parenleft": '(', "parenright": ')', "bracketleft": '[', "bracketright": ']',
	"braceleft": '{', "braceright": '}', "less": '<', "greater": '>',
	"equal": '=', "plus": '+', "minus": 0x2212, "asterisk": '*', "numbersign": '#',
	"percent": '%', "ampersand": '&', "at": '@', "asciicircum": '^',
	"periodcentered": 0x00B7, "ellipsis": 0x2026, "dagger": 0x2020, "daggerdbl": 0x2021,
	"perthousand": 0x2030, "guillemotleft": 0x00AB, "guillemotright": 0x00BB,
	"guilsinglleft": 0x2039, "guilsinglright": 0x203A, "fraction": 0x2044,
	// Accented Latin-1.
	"grave": '`', "acute": 0x00B4, "circumflex": 0x02C6, "tilde": '~',
	"dieresis": 0x00A8, "cedilla": 0x00B8, "macron": 0x00AF, "degree": 0x00B0,
	"Agrave": 0x00C0, "Aacute": 0x00C1, "Acircumflex": 0x00C2, "Atilde": 0x00C3,
	"Adieresis": 0x00C4, "Aring": 0x00C5, "AE": 0x00C6, "Ccedil": 0x00C7,
	"Egrave": 0x00C8, "Eacute": 0x00C9, "Ecircumflex": 0x00CA, "Edieresis": 0x00CB,
	"Igrave": 0x00CC, "Iacute": 0x00CD, "Icircumflex": 0x00CE, "Idieresis": 0x00CF,
	"Ntilde": 0x00D1, "Ograve": 0x00D2, "Oacute": 0x00D3, "Ocircumflex": 0x00D4,
	"Otilde": 0x00D5, "Odieresis": 0x00D6, "OE": 0x0152, "Oslash": 0x00D8,
	"Ugrave": 0x00D9, "Uacute": 0x00DA, "Ucircumflex": 0x00DB, "Udieresis": 0x00DC,
	"Yacute": 0x00DD, "agrave": 0x00E0, "aacute": 0x00E1, "acircumflex": 0x00E2,
	"atilde": 0x00E3, "adieresis": 0x00E4, "aring": 0x00E5, "ae": 0x00E6, "ccedil": 0x00E7,
	"egrave": 0x00E8, "eacute": 0x00E9, "ecircumflex": 0x00EA, "edieresis": 0x00EB,
	"igrave": 0x00EC, "iacute": 0x00ED, "icircumflex": 0x00EE, "idieresis": 0x00EF,
	"ntilde": 0x00F1, "ograve": 0x00F2, "oacute": 0x00F3, "ocircumflex": 0x00F4,
	"otilde": 0x00F5, "odieresis": 0x00F6, "oe": 0x0153, "oslash": 0x00F8,
	"ugrave": 0x00F9, "uacute": 0x00FA, "ucircumflex": 0x00FB, "udieresis": 0x00FC,
	"yacute": 0x00FD, "ydieresis": 0x00FF, "ss": 0x00DF, "mu": 0x00B5,
	"Eth": 0x00D0, "eth": 0x00F0, "Thorn": 0x00DE, "thorn": 0x00FE, "dotlessi": 0x0131,
	"trademark": 0x2122, "ordfeminine": 0x00AA, "ordmasculine": 0x00BA,
	"onesuperior": 0x00B9, "twosuperior": 0x00B2, "threesuperior": 0x00B3,
	"onehalf": 0x00BD, "onequarter": 0x00BC, "threequarters": 0x00BE,
	"multiply": 0x00D7, "divide": 0x00F7, "paragraph": 0x00B6, "section": 0x00A7,
	"registered": 0x00AE, "copyright": 0x00A9,
	// Greek letters used by Symbol encoded fonts.
	"alpha": 0x03B1, "beta": 0x03B2, "gamma": 0x03B3, "delta": 0x03B4, "epsilon": 0x03B5,
	"zeta": 0x03B6, "eta": 0x03B7, "theta": 0x03B8, "iota": 0x03B9, "kappa": 0x03BA,
	"lambda": 0x03BB, "mu1": 0x03BC, "nu": 0x03BD, "xi": 0x03BE, "omicron": 0x03BF,
	"pi": 0x03C0, "rho": 0x03C1, "sigma": 0x03C3, "tau": 0x03C4, "upsilon": 0x03C5,
	"phi": 0x03C6, "chi": 0x03C7, "psi": 0x03C8, "omega": 0x03C9,
	"Alpha": 0x0391, "Beta": 0x0392, "Gamma": 0x0393, "Delta": 0x0394, "Epsilon": 0x0395,
	"Zeta": 0x0396, "Eta": 0x0397, "Theta": 0x0398, "Iota": 0x0399, "Kappa": 0x039A,
	"Lambda": 0x039B, "Nu": 0x039D, "Xi": 0x039E, "Omicron": 0x039F, "Pi": 0x03A0,
	"Rho": 0x03A1, "Sigma": 0x03A3, "Tau": 0x03A4, "Upsilon": 0x03A5, "Phi": 0x03A6,
	"Chi": 0x03A7, "Psi": 0x03A8, "Omega": 0x03A9,
	// Math symbols frequently used in Differences arrays.
	"infinity": 0x221E, "plusminus": 0x00B1, "lessequal": 0x2264, "greaterequal": 0x2265,
	"notequal": 0x2260, "approximate": 0x2248, "integral": 0x222B, "summation": 0x2211,
	"radical": 0x221A, "partialdiff": 0x2202, "nabla": 0x2207, "product": 0x220F,
	"logicaland": 0x2227, "logicalor": 0x2228, "logicalnot": 0x00AC,
	"emptyset": 0x2205, "element": 0x2208, "owner": 0x220B, "intersection": 0x2229,
	"union": 0x222A, "propersubset": 0x2282, "propersuperset": 0x2283, "notsubset": 0x2284,
	"reflexsubset": 0x2286, "reflexsuperset": 0x2287, "circleplus": 0x2295,
	"circlemultiply": 0x2297, "angle": 0x2220,
}
