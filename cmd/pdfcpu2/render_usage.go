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

const (
	usageLongRender = `Render selected pages into image files.

         pages ... Please refer to "pdfcpu selectedpages"
        inFile ... input PDF file, use - to read from stdin
        outDir ... output directory for one image file per page

Flags:
     --dpi ... resolution in dots per inch (default: 150)
    --mode ... output format: png | jpg (default: png)
 --quality ... JPEG quality 1..100 (default: 85, jpg only)

Output files are named <inFile>_page_<pageNr>.<ext>.

Examples:
   pdfcpu render in.pdf outDir
      Render all pages of in.pdf as PNG at 150 dpi into outDir.

   pdfcpu render -m jpg -q 90 --dpi 300 in.pdf outDir
      Render all pages of in.pdf as JPEG with quality 90 at 300 dpi into outDir.

   pdfcpu render in.pdf outDir -p 1-3
      Render pages 1-3 of in.pdf into outDir.

Pipeline example:
   aws s3 cp s3://acme-docs/manual.pdf - \
      | pdfcpu render - /work/pages
`

	usageLongSolidify = `Replace the content of selected pages with its rendered image.

         pages ... Please refer to "pdfcpu selectedpages"
        inFile ... input PDF file, use - to read from stdin
       outFile ... output PDF file, use - to write to stdout
                   (default: inFile, which requires --force for existing files)

Every selected page is rasterized and rebuilt as an image only page with the
same geometry. Text, vector graphics and annotations are gone, which protects
the document against text copying and content modification.

Flags:
     --dpi ... resolution in dots per inch (default: 150)
    --mode ... embedded image format: png (lossless) | jpg (default: png)
 --quality ... JPEG quality 1..100 (default: 85, jpg only)

Examples:
   pdfcpu solidify in.pdf out.pdf
      Replace the content of all pages of in.pdf with rendered images.

   pdfcpu solidify in.pdf out.pdf -m jpg --dpi 200
      Same, but embed JPEG images rendered at 200 dpi.

   pdfcpu solidify in.pdf out.pdf -p 1-5
      Solidify pages 1-5 only.

Pipeline example:
   aws s3 cp s3://acme-docs/manual.pdf - \
      | pdfcpu solidify - - \
      | aws s3 cp - s3://acme-docs/manual-solid.pdf
`
)
