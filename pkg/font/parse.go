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

package font

import (
	"bytes"
	"context"
	"fmt"

	"github.com/pdfcpu/pdfcpu/internal/contextutil"
)

// parseTTFTables fills fd from the standard set of TrueType tables.
func parseTTFTables(tables map[string]*table) (ttf, error) {
	fd := ttf{}
	for _, v := range []string{"head", "OS/2", "post", "name", "hhea", "maxp", "hmtx", "cmap"} {
		if err := parse(tables, v, &fd); err != nil {
			return fd, fmt.Errorf("parse %s table: %w", v, err)
		}
	}
	return fd, nil
}

// ParseTTF parses a TrueType font program from bb and returns its
// light-weight metrics including the glyph index to Unicode mapping.
// It operates on memory only and does not touch the file system.
// Missing optional tables are tolerated just like during font installation.
func ParseTTF(c context.Context, bb []byte) (TTFLight, error) {
	if err := contextutil.Check(c); err != nil {
		return TTFLight{}, err
	}
	if len(bb) == 0 {
		return TTFLight{}, invalidFontData("empty font program")
	}
	if len(bb) > maxFontFileSize {
		return TTFLight{}, invalidFontData("font size %d exceeds limit %d", len(bb), maxFontFileSize)
	}

	rd := bytes.NewReader(bb)
	_, tables, err := headerAndTables(rd, 0, int64(len(bb)))
	if err != nil {
		return TTFLight{}, fmt.Errorf("parse tables: %w", err)
	}

	fd, err := parseTTFTables(tables)
	if err != nil {
		return TTFLight{}, err
	}

	l := fd.light()
	if err := ValidateTTFLight(l); err != nil {
		return TTFLight{}, err
	}
	return l, nil
}
