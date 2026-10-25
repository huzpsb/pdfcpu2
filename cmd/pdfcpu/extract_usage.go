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

const usageLongExtract = `Export inFile's images, fonts, content or pages into outDir.

      mode ... extraction mode
     pages ... Please refer to "pdfcpu selectedpages"
    inFile ... input PDF file, use - to read from stdin
    outDir ... output directory, use - with mode page and one selected page to write to stdout

 The extraction modes are:

  image ... extract images
   font ... extract font files (supported font types: TrueType)
content ... extract raw page content
   page ... extract single page PDFs
   meta ... extract all metadata (page selection does not apply)

Pipeline example:
   aws s3 cp s3://acme-archive/contract.pdf - \
      | pdfcpu extract -m page -p 3 - - \
      | aws s3 cp - s3://acme-archive/pages/contract-page-3.pdf
`
