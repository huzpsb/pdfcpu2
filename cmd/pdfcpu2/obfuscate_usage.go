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

package main

const usageLongObfuscate = `Destroy the text extraction mappings of embedded CID fonts (anti copy & paste).

        inFile ... input PDF file, use - to read from stdin
       outFile ... output PDF file, use - to write to stdout
                   (default: inFile, which requires --force for existing files)

The cmap table of every embedded CID font program and its ToUnicode CMap
stream are overwritten with deterministic garbage. Glyph data, metrics and
table layout are not touched, and CID fonts address their glyphs by glyph
id, so pages keep rendering exactly as before. Text extraction and text
based copy & paste of these fonts, however, yield garbage.

Simple fonts (plain Latin TrueType/Type1 text) resolve their glyphs through
their encoding and their own cmap/ToUnicode mapping, therefore obfuscating
them would change the rendered output. Such fonts are skipped and reported.

Note:
 - Obfuscation is irreversible: keep the original.
 - Use "solidify" instead to also protect simple font text (rasterizes pages).

Examples:
   pdfcpu obfuscate in.pdf out.pdf
      Garbage fill the font mappings of in.pdf and write out.pdf.

   pdfcpu obfuscate in.pdf
      Overwrite in.pdf (requires --force for existing files).

Pipeline example:
   aws s3 cp s3://acme-docs/manual.pdf - \
      | pdfcpu obfuscate - - \
      | aws s3 cp - s3://acme-docs/manual-obf.pdf
`
