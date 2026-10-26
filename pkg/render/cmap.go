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
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// toUnicodeCMap maps character codes to Unicode strings.
type toUnicodeCMap struct {
	maps     map[uint32]string
	min, max int // code length in bytes
}

func newToUnicodeCMap() *toUnicodeCMap {
	return &toUnicodeCMap{maps: map[uint32]string{}, min: 4, max: 1}
}

func (tu *toUnicodeCMap) lookup(code uint32) (string, bool) {
	s, ok := tu.maps[code]
	return s, ok
}

// parseToUnicode parses the CMap syntax subset needed for ToUnicode streams.
func parseToUnicode(bb []byte) (*toUnicodeCMap, error) {
	tu := newToUnicodeCMap()
	toks := cmapTokens(bb)
	for i := 0; i < len(toks); i++ {
		switch toks[i] {
		case "begincodespacerange":
			i = skipCMapSection(toks, i, "endcodespacerange")
		case "beginbfchar":
			for i++; i < len(toks) && toks[i] != "endbfchar"; i += 2 {
				if i+1 >= len(toks) {
					break
				}
				src, err := hexCode(toks[i])
				if err != nil {
					return nil, err
				}
				dst := unicodeFromCMapToken(toks[i+1])
				if src != nil {
					tu.add(src, dst)
				}
			}
		case "beginbfrange":
			for i++; i < len(toks) && toks[i] != "endbfrange"; {
				if i+2 >= len(toks) {
					break
				}
				lo, err := hexCode(toks[i])
				if err != nil {
					return nil, err
				}
				hi, err := hexCode(toks[i+1])
				if err != nil {
					return nil, err
				}
				dstTok := toks[i+2]
				if strings.HasPrefix(dstTok, "[") {
					// Array form: [ <dst1> <dst2> ... ]
					parts := strings.FieldsFunc(dstTok, func(r rune) bool { return r == '[' || r == ']' })
					var dsts []string
					for _, p := range parts {
						dsts = append(dsts, unicodeFromCMapToken(p))
					}
					if lo != nil && hi != nil {
						tu.addRangeArray(lo, hi, dsts)
					}
					i += 3
					continue
				}
				dst := unicodeFromCMapToken(dstTok)
				if lo != nil && hi != nil {
					tu.addRange(lo, hi, dst)
				}
				i += 3
			}
		}
	}
	if len(tu.maps) == 0 {
		return nil, errors.New("empty ToUnicode CMap")
	}
	return tu, nil
}

func (tu *toUnicodeCMap) add(src []byte, dst string) {
	if len(src) < 1 || len(src) > 4 {
		return
	}
	tu.maps[codeOfBytes(src)] = dst
	if len(src) < tu.min {
		tu.min = len(src)
	}
	if len(src) > tu.max {
		tu.max = len(src)
	}
}

func (tu *toUnicodeCMap) addRange(lo, hi []byte, dst string) {
	l, h := codeOfBytes(lo), codeOfBytes(hi)
	if h < l || h-l > 65535 {
		tu.add(lo, dst)
		return
	}
	for c := l; ; c++ {
		tu.add(codeBytes(c, len(lo)), shiftUnicode(dst, int(c-l)))
		if c == h {
			break
		}
	}
}

func (tu *toUnicodeCMap) addRangeArray(lo, hi []byte, dsts []string) {
	l, h := codeOfBytes(lo), codeOfBytes(hi)
	for c := l; ; c++ {
		idx := int(c - l)
		if idx >= 0 && idx < len(dsts) {
			tu.add(codeBytes(c, len(lo)), dsts[idx])
		}
		if c == h {
			break
		}
	}
}

func codeOfBytes(b []byte) uint32 {
	var c uint32
	for _, x := range b {
		c = c<<8 | uint32(x)
	}
	return c
}

func codeBytes(c uint32, n int) []byte {
	b := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		b[i] = byte(c)
		c >>= 8
	}
	return b
}

// shiftUnicode adds delta code points to a UTF-16BE destination string.
func shiftUnicode(dst string, delta int) string {
	if delta == 0 {
		return dst
	}
	units := utf16Units(dst)
	if units == nil {
		return dst
	}
	for i := range units {
		units[i] = uint16(int(units[i]) + delta)
	}
	return unitsToString(units)
}

func utf16Units(s string) []uint16 {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	if len(b)%2 == 1 {
		b = append(b, 0)
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return units
}

func unitsToString(units []uint16) string {
	var sb strings.Builder
	for i := 0; i < len(units); i++ {
		u := rune(units[i])
		if u >= 0xD800 && u <= 0xDBFF && i+1 < len(units) {
			lo := rune(units[i+1])
			if lo >= 0xDC00 && lo <= 0xDFFF {
				sb.WriteRune((u-0xD800)<<10 + (lo - 0xDC00) + 0x10000)
				i++
				continue
			}
		}
		if u >= 0xD800 && u <= 0xDFFF {
			continue
		}
		sb.WriteRune(u)
	}
	return sb.String()
}

func cmapTokens(bb []byte) []string {
	var toks []string
	i := 0
	for i < len(bb) {
		c := bb[i]
		switch {
		case isWS(c):
			i++
		case c == '%':
			for i < len(bb) && bb[i] != '\n' && bb[i] != '\r' {
				i++
			}
		case c == '<' || c == '>':
			// Hex token <XXXX> or dict markers << >>.
			if i+1 < len(bb) && (bb[i+1] == '<' || bb[i+1] == '>') && bb[i] == bb[i+1] {
				toks = append(toks, string(bb[i:i+2]))
				i += 2
				continue
			}
			j := i + 1
			for j < len(bb) && bb[j] != '>' {
				j++
			}
			toks = append(toks, strings.TrimSpace(string(bb[i+1:j])))
			i = j + 1
		case c == '[' || c == ']' || c == '(' || c == ')' || c == '{' || c == '}':
			toks = append(toks, string(c))
			i++
		case c == '"':
			// CMap name.
			j := i + 1
			for j < len(bb) && bb[j] != '"' {
				j++
			}
			toks = append(toks, string(bb[i+1:j]))
			i = j + 1
		default:
			j := i
			for j < len(bb) && !isWS(bb[j]) && !isDelim(bb[j]) && bb[j] != '%' {
				j++
			}
			if j == i {
				i++
				continue
			}
			// Merge bracketed arrays in bfrange into one token.
			tok := string(bb[i:j])
			if tok == "[" {
				// Collect until closing ].
				k := j
				for k < len(bb) && bb[k] != ']' {
					k++
				}
				inner := strings.Fields(string(bb[j:k]))
				toks = append(toks, "["+strings.Join(inner, " ")+"]")
				i = k + 1
				continue
			}
			toks = append(toks, tok)
			i = j
		}
	}
	return toks
}

func skipCMapSection(toks []string, i int, end string) int {
	for i++; i < len(toks) && toks[i] != end; i++ {
	}
	return i
}

func hexCode(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "<"), ">")
	if s == "" {
		return nil, errors.New("empty code")
	}
	if len(s)%2 == 1 {
		s += "0"
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("cmap code: %w", err)
	}
	return b, nil
}

func unicodeFromCMapToken(tok string) string {
	if strings.HasPrefix(tok, "(") {
		// Literal string destination.
		s := strings.TrimSuffix(strings.TrimPrefix(tok, "("), ")")
		return stringToHexUTF16BE(s)
	}
	b, err := hexCode(tok)
	if err != nil {
		return ""
	}
	return strings.ToUpper(hex.EncodeToString(b))
}

func stringToHexUTF16BE(s string) string {
	units := []uint16{}
	for _, r := range s {
		if r > 0xFFFF {
			r -= 0x10000
			units = append(units, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
		} else {
			units = append(units, uint16(r))
		}
	}
	b := make([]byte, len(units)*2)
	for i, u := range units {
		b[2*i] = byte(u >> 8)
		b[2*i+1] = byte(u)
	}
	return strings.ToUpper(hex.EncodeToString(b))
}

// decodeToUnicodeString converts a UTF-16BE hex destination into runes.
func decodeToUnicodeString(hexStr string) string {
	units := utf16Units(hexStr)
	if units == nil {
		return ""
	}
	return unitsToString(units)
}

// onceFallback guards lazy parsing of the embedded fallback font.
var onceFallback sync.Once
