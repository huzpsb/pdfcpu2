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

// Package fallbackfont provides the embedded fallback font used to render
// text with fonts that have no embedded font program (including the PDF core
// fonts).
//
// The bundled font is vivo Sans Light (vivoSans-Light.ttf), licensed by vivo
// under the vivo Sans Font Knowledge Property License Agreement, see
// vivoSans-LICENSE.txt. Distribution is part of this application as permitted
// for software embedding; keep the license file alongside.
package fallbackfont

import _ "embed"

//go:embed vivoSans-Light.ttf
var fontData []byte

// Bytes returns the raw TrueType data of the fallback font.
func Bytes() []byte { return fontData }
