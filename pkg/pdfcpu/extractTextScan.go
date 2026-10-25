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

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// contentOperand is one operand of a content stream operator.
type contentOperand struct {
	// operator is set if this token is an operator.
	operator string
	// num holds a numeric value.
	num float64
	// hasNum reports whether num is populated.
	hasNum bool
	// str holds the decoded bytes of a string literal.
	str []byte
	// name holds a resource name without its leading slash.
	name string
	// arr holds the elements of an array.
	arr []contentOperand
}

func (op contentOperand) isOperand() bool {
	return op.operator == "" && (op.hasNum || op.str != nil || op.name != "" || op.arr != nil)
}

func (op contentOperand) isZero() bool {
	return op.operator == "" && !op.hasNum && op.str == nil && op.name == "" && op.arr == nil
}

func (op contentOperand) number() (float64, bool) {
	return op.num, op.hasNum
}

// contentScanner tokenizes a decoded content stream into operands and operators.
type contentScanner struct {
	bb  []byte
	pos int
}

func newContentScanner(bb []byte) *contentScanner {
	return &contentScanner{bb: bb}
}

func isContentWhitespace(c byte) bool {
	return c == 0 || c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == ' '
}

func isContentDelimiter(c byte) bool {
	return c == '(' || c == ')' || c == '<' || c == '>' || c == '[' || c == ']' ||
		c == '{' || c == '}' || c == '/' || c == '%'
}

func isRegularByte(c byte) bool {
	return !isContentWhitespace(c) && !isContentDelimiter(c)
}

// next returns the next operand or operator token.
// It returns an empty contentOperand at the end of input.
func (s *contentScanner) next() (contentOperand, error) {
	s.skipWhitespaceAndComments()

	if s.pos >= len(s.bb) {
		return contentOperand{}, nil
	}

	c := s.bb[s.pos]

	switch {
	case c == '(':
		bb, err := s.scanLiteralString()
		if err != nil {
			return contentOperand{}, err
		}
		return contentOperand{str: bb}, nil
	case c == '<':
		if s.pos+1 < len(s.bb) && s.bb[s.pos+1] == '<' {
			if err := s.skipDict(); err != nil {
				return contentOperand{}, err
			}
			return s.next()
		}
		bb, err := s.scanHexString()
		if err != nil {
			return contentOperand{}, err
		}
		return contentOperand{str: bb}, nil
	case c == '[':
		s.pos++
		arr, err := s.scanArray()
		if err != nil {
			return contentOperand{}, err
		}
		return contentOperand{arr: arr}, nil
	case c == ']':
		s.pos++
		return contentOperand{}, fmt.Errorf("unbalanced array delimiter")
	case c == '/':
		s.pos++
		start := s.pos
		for s.pos < len(s.bb) && isRegularByte(s.bb[s.pos]) {
			s.pos++
		}
		name, err := types.DecodeName(string(s.bb[start:s.pos]))
		if err != nil {
			return contentOperand{}, err
		}
		return contentOperand{name: name}, nil
	case c == '{' || c == '}':
		s.pos++
		return s.next()
	default:
		start := s.pos
		for s.pos < len(s.bb) && isRegularByte(s.bb[s.pos]) {
			s.pos++
		}
		tok := string(s.bb[start:s.pos])
		if tok == "" {
			s.pos++
			return s.next()
		}

		// Numbers take precedence over operator detection.
		if v, err := strconv.ParseFloat(tok, 64); err == nil {
			return contentOperand{num: v, hasNum: true}, nil
		}

		// Operators consist of letters, digits and the characters ' and ".
		isOperator := true
		for i := 0; i < len(tok); i++ {
			b := tok[i]
			if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
				b == '\'' || b == '"' || b == '*') {
				isOperator = false
				break
			}
		}
		if isOperator {
			return contentOperand{operator: tok}, nil
		}

		// Junk token to ignore.
		return contentOperand{name: "#" + tok}, nil
	}
}

func (s *contentScanner) skipWhitespaceAndComments() {
	for s.pos < len(s.bb) {
		c := s.bb[s.pos]
		if isContentWhitespace(c) {
			s.pos++
			continue
		}
		if c == '%' {
			for s.pos < len(s.bb) && s.bb[s.pos] != '\n' && s.bb[s.pos] != '\r' {
				s.pos++
			}
			continue
		}
		return
	}
}

// scanArray scans array elements until the closing bracket.
func (s *contentScanner) scanArray() ([]contentOperand, error) {
	arr := []contentOperand{}
	for {
		s.skipWhitespaceAndComments()
		if s.pos >= len(s.bb) {
			return nil, fmt.Errorf("unterminated array")
		}
		if s.bb[s.pos] == ']' {
			s.pos++
			return arr, nil
		}
		op, err := s.next()
		if err != nil {
			return nil, err
		}
		if op.isZero() {
			return nil, fmt.Errorf("unterminated array")
		}
		arr = append(arr, op)
		if len(arr) > 8192 {
			return nil, fmt.Errorf("array exceeds %d elements", 8192)
		}
	}
}

// scanLiteralString scans a literal string including escape sequences.
func (s *contentScanner) scanLiteralString() ([]byte, error) {
	s.pos++ // consume (
	var bb []byte
	depth := 1
	for s.pos < len(s.bb) {
		c := s.bb[s.pos]
		switch c {
		case '\\':
			s.pos++
			if s.pos >= len(s.bb) {
				return nil, fmt.Errorf("unterminated string escape")
			}
			e := s.bb[s.pos]
			switch e {
			case 'n':
				bb = append(bb, '\n')
				s.pos++
			case 'r':
				bb = append(bb, '\r')
				s.pos++
			case 't':
				bb = append(bb, '\t')
				s.pos++
			case 'b':
				bb = append(bb, '\b')
				s.pos++
			case 'f':
				bb = append(bb, '\f')
				s.pos++
			case '(':
				bb = append(bb, '(')
				s.pos++
			case ')':
				bb = append(bb, ')')
				s.pos++
			case '\\':
				bb = append(bb, '\\')
				s.pos++
			case '\r':
				s.pos++
				if s.pos < len(s.bb) && s.bb[s.pos] == '\n' {
					s.pos++
				}
			case '\n':
				s.pos++
			default:
				// Up to three octal digits.
				v := 0
				digits := 0
				for digits < 3 && s.pos < len(s.bb) {
					d := s.bb[s.pos]
					if d < '0' || d > '7' {
						break
					}
					v = v*8 + int(d-'0')
					digits++
					s.pos++
				}
				if digits == 0 {
					bb = append(bb, e)
					s.pos++
					continue
				}
				bb = append(bb, byte(v))
			}
		case '(':
			depth++
			bb = append(bb, c)
			s.pos++
		case ')':
			depth--
			s.pos++
			if depth == 0 {
				return bb, nil
			}
			bb = append(bb, c)
		default:
			bb = append(bb, c)
			s.pos++
		}
	}

	return nil, fmt.Errorf("unterminated literal string")
}

// scanHexString scans a hex string.
func (s *contentScanner) scanHexString() ([]byte, error) {
	s.pos++ // consume <
	var hex []byte
	for s.pos < len(s.bb) {
		c := s.bb[s.pos]
		if c == '>' {
			s.pos++
			if len(hex)%2 == 1 {
				hex = append(hex, '0')
			}
			bb := make([]byte, len(hex)/2)
			for i := 0; i < len(bb); i++ {
				v, err := strconv.ParseUint(string(hex[2*i:2*i+2]), 16, 8)
				if err != nil {
					return nil, fmt.Errorf("invalid hex string: %w", err)
				}
				bb[i] = byte(v)
			}
			return bb, nil
		}
		if isContentWhitespace(c) {
			s.pos++
			continue
		}
		hex = append(hex, c)
		s.pos++
	}

	return nil, fmt.Errorf("unterminated hex string")
}

// skipDict skips over a dictionary.
func (s *contentScanner) skipDict() error {
	s.pos += 2 // consume <<
	depth := 1
	for s.pos < len(s.bb) {
		c := s.bb[s.pos]
		if c == '<' && s.pos+1 < len(s.bb) && s.bb[s.pos+1] == '<' {
			depth++
			s.pos += 2
			continue
		}
		if c == '>' && s.pos+1 < len(s.bb) && s.bb[s.pos+1] == '>' {
			depth--
			s.pos += 2
			if depth == 0 {
				// A dictionary in a content stream belongs to an operator like BDC.
				// Skip the following operator token so it does not accumulate operands.
				return nil
			}
			continue
		}
		s.pos++
	}

	return fmt.Errorf("unterminated dictionary")
}

// skipInlineImage consumes an inline image from BI to the terminating EI.
// It must be called right after the BI operator was returned by next.
func (s *contentScanner) skipInlineImage() error {
	// Scan image entries until the ID operator.
	for {
		s.skipWhitespaceAndComments()
		if s.pos >= len(s.bb) {
			return nil
		}
		if s.pos+1 < len(s.bb) && s.bb[s.pos] == 'I' && s.bb[s.pos+1] == 'D' {
			if s.pos+2 >= len(s.bb) || isContentWhitespace(s.bb[s.pos+2]) {
				s.pos += 2
				break
			}
		}
		op, err := s.next()
		if err != nil {
			return err
		}
		if op.isZero() {
			return nil
		}
	}

	// Skip binary data up to a whitespace delimited EI.
	for s.pos < len(s.bb) {
		if s.bb[s.pos] == 'E' && s.pos+1 < len(s.bb) && s.bb[s.pos+1] == 'I' {
			before := byte(' ')
			if s.pos > 0 {
				before = s.bb[s.pos-1]
			}
			after := byte(' ')
			if s.pos+2 < len(s.bb) {
				after = s.bb[s.pos+2]
			}
			if isContentWhitespace(before) && (isContentWhitespace(after) || s.pos+2 >= len(s.bb)) {
				s.pos += 2
				return nil
			}
		}
		s.pos++
	}

	return nil
}
