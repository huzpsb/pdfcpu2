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
	"fmt"
	"strconv"
)

// tokenKind classifies lexer tokens.
type tokenKind int

const (
	tokEOF tokenKind = iota
	tokNum
	tokStr      // literal or hex string
	tokName     // /Name
	tokOp       // operator
	tokArrOpen  // [
	tokArrClose // ]
	tokDictOpen // <<
	tokInline   // complete inline image (BI ... EI)
	tokArr      // [ ... ] collected into a single token
)

// token is one content stream token.
type token struct {
	kind  tokenKind
	num   float64
	str   []byte
	name  string
	op    string
	arr   []token     // array elements
	dict  map[string]token // inline image entries: key -> value token
	raw   []byte      // inline image data between ID and EI
	tokOK bool
}

func (t token) boolean() bool {
	// Boolean-ish token: true/false arrive as operators.
	return t.kind == tokOp && t.op == "true"
}

// lexer tokenizes decoded content streams. It differs from a strict PDF object
// lexer in that it understands inline image binary payloads.
type lexer struct {
	bb  []byte
	pos int
}

func newLexer(bb []byte) *lexer { return &lexer{bb: bb} }

func isWS(c byte) bool {
	return c == 0 || c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == ' '
}

func isDelim(c byte) bool {
	return c == '(' || c == ')' || c == '<' || c == '>' || c == '[' || c == ']' ||
		c == '{' || c == '}' || c == '/' || c == '%'
}

func isRegular(c byte) bool { return !isWS(c) && !isDelim(c) }

// next returns the next token.
func (lx *lexer) next() (token, error) {
	lx.skipWS()
	if lx.pos >= len(lx.bb) {
		return token{kind: tokEOF}, nil
	}
	c := lx.bb[lx.pos]
	switch {
	case c == '(':
		bb, err := lx.literalString()
		return token{kind: tokStr, str: bb}, err
	case c == '<':
		if lx.pos+1 < len(lx.bb) && lx.bb[lx.pos+1] == '<' {
			// A dict operand: skip it entirely (only used by ignored ops).
			lx.pos += 2
			depth := 1
			for lx.pos < len(lx.bb) && depth > 0 {
				if lx.bb[lx.pos] == '<' && lx.pos+1 < len(lx.bb) && lx.bb[lx.pos+1] == '<' {
					depth++
					lx.pos += 2
					continue
				}
				if lx.bb[lx.pos] == '>' && lx.pos+1 < len(lx.bb) && lx.bb[lx.pos+1] == '>' {
					depth--
					lx.pos += 2
					continue
				}
				lx.pos++
			}
			return token{kind: tokDictOpen}, nil
		}
		bb, err := lx.hexString()
		return token{kind: tokStr, str: bb}, err
	case c == '[':
		lx.pos++
		return token{kind: tokArrOpen}, nil
	case c == ']':
		lx.pos++
		return token{kind: tokArrClose}, nil
	case c == '/':
		lx.pos++
		start := lx.pos
		for lx.pos < len(lx.bb) && isRegular(lx.bb[lx.pos]) {
			lx.pos++
		}
		name, err := decodeName(string(lx.bb[start:lx.pos]))
		if err != nil {
			return token{}, err
		}
		return token{kind: tokName, name: name}, nil
	case c == '{' || c == '}':
		lx.pos++
		return lx.next()
	default:
		start := lx.pos
		for lx.pos < len(lx.bb) && isRegular(lx.bb[lx.pos]) {
			lx.pos++
		}
		tok := string(lx.bb[start:lx.pos])
		if tok == "" {
			lx.pos++
			return lx.next()
		}
		if v, err := strconv.ParseFloat(tok, 64); err == nil {
			return token{kind: tokNum, num: v}, nil
		}
		if tok == "BI" {
			return lx.inlineImage()
		}
		return token{kind: tokOp, op: tok}, nil
	}
}

func (lx *lexer) skipWS() {
	for lx.pos < len(lx.bb) {
		c := lx.bb[lx.pos]
		if isWS(c) {
			lx.pos++
			continue
		}
		if c == '%' {
			for lx.pos < len(lx.bb) && lx.bb[lx.pos] != '\n' && lx.bb[lx.pos] != '\r' {
				lx.pos++
			}
			continue
		}
		return
	}
}

// inlineImage parses BI ... ID <binary> EI and returns a complete token.
func (lx *lexer) inlineImage() (token, error) {
	entries := map[string]token{}
	for {
		lx.skipWS()
		if lx.pos >= len(lx.bb) {
			return token{}, fmt.Errorf("inline image: unterminated")
		}
		if lx.pos+1 < len(lx.bb) && lx.bb[lx.pos] == 'I' && lx.bb[lx.pos+1] == 'D' &&
			(lx.pos+2 >= len(lx.bb) || isWS(lx.bb[lx.pos+2])) {
			lx.pos += 2
			break
		}
		key, err := lx.next()
		if err != nil {
			return token{}, err
		}
		if key.kind != tokName {
			continue
		}
		val, err := lx.next()
		if err != nil {
			return token{}, err
		}
		entries[key.name] = val
	}
	// One whitespace byte separates ID from the data.
	if lx.pos < len(lx.bb) && lx.bb[lx.pos] == '\r' {
		lx.pos++
		if lx.pos < len(lx.bb) && lx.bb[lx.pos] == '\n' {
			lx.pos++
		}
	} else if lx.pos < len(lx.bb) && isWS(lx.bb[lx.pos]) {
		lx.pos++
	}
	// Scan for the terminating EI: preceded by whitespace, followed by
	// whitespace, delimiter or EOF.
	dataStart := lx.pos
	i := lx.pos
	end := -1
	for i < len(lx.bb) {
		if lx.bb[i] == 'E' && i+1 < len(lx.bb) && lx.bb[i+1] == 'I' {
			before := byte(' ')
			if i > 0 {
				before = lx.bb[i-1]
			}
			after := byte(' ')
			if i+2 < len(lx.bb) {
				after = lx.bb[i+2]
			}
			if isWS(before) && (isWS(after) || isDelim(after) || i+2 >= len(lx.bb)) {
				end = i
				break
			}
		}
		i++
	}
	if end < 0 {
		return token{}, fmt.Errorf("inline image: missing EI")
	}
	data := lx.bb[dataStart:end]
	// Trim the single whitespace directly in front of EI.
	if n := len(data); n > 0 && isWS(data[n-1]) {
		data = data[:n-1]
	}
	lx.pos = end + 2
	return token{kind: tokInline, dict: entries, raw: data, tokOK: true}, nil
}

func (lx *lexer) literalString() ([]byte, error) {
	lx.pos++ // consume (
	var bb []byte
	depth := 1
	for lx.pos < len(lx.bb) {
		c := lx.bb[lx.pos]
		switch c {
		case '\\':
			lx.pos++
			if lx.pos >= len(lx.bb) {
				return nil, fmt.Errorf("unterminated string escape")
			}
			e := lx.bb[lx.pos]
			switch e {
			case 'n':
				bb = append(bb, '\n')
				lx.pos++
			case 'r':
				bb = append(bb, '\r')
				lx.pos++
			case 't':
				bb = append(bb, '\t')
				lx.pos++
			case 'b':
				bb = append(bb, '\b')
				lx.pos++
			case 'f':
				bb = append(bb, '\f')
				lx.pos++
			case '(':
				bb = append(bb, '(')
				lx.pos++
			case ')':
				bb = append(bb, ')')
				lx.pos++
			case '\\':
				bb = append(bb, '\\')
				lx.pos++
			case '\r':
				lx.pos++
				if lx.pos < len(lx.bb) && lx.bb[lx.pos] == '\n' {
					lx.pos++
				}
			case '\n':
				lx.pos++
			default:
				v := 0
				digits := 0
				for digits < 3 && lx.pos < len(lx.bb) {
					d := lx.bb[lx.pos]
					if d < '0' || d > '7' {
						break
					}
					v = v*8 + int(d-'0')
					digits++
					lx.pos++
				}
				if digits == 0 {
					bb = append(bb, e)
					lx.pos++
					continue
				}
				bb = append(bb, byte(v))
			}
		case '(':
			depth++
			bb = append(bb, c)
			lx.pos++
		case ')':
			depth--
			lx.pos++
			if depth == 0 {
				return bb, nil
			}
			bb = append(bb, c)
		default:
			bb = append(bb, c)
			lx.pos++
		}
	}
	return nil, fmt.Errorf("unterminated literal string")
}

func (lx *lexer) hexString() ([]byte, error) {
	lx.pos++ // consume <
	var hex []byte
	for lx.pos < len(lx.bb) {
		c := lx.bb[lx.pos]
		if c == '>' {
			lx.pos++
			if len(hex)%2 == 1 {
				hex = append(hex, '0')
			}
			bb := make([]byte, len(hex)/2)
			for i := range bb {
				v, err := strconv.ParseUint(string(hex[2*i:2*i+2]), 16, 8)
				if err != nil {
					return nil, fmt.Errorf("invalid hex string: %w", err)
				}
				bb[i] = byte(v)
			}
			return bb, nil
		}
		if isWS(c) {
			lx.pos++
			continue
		}
		hex = append(hex, c)
		lx.pos++
	}
	return nil, fmt.Errorf("unterminated hex string")
}

// decodeName decodes #xx escapes in a name token.
func decodeName(s string) (string, error) {
	if len(s) == 0 {
		return "", nil
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '#' && i+2 < len(s) {
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err == nil {
				out = append(out, byte(v))
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out), nil
}
