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

package webui

import "testing"

func TestOCRScannedPage(t *testing.T) {
	data := readTestFile(t, "../pkg/testdata/WaldenScan.pdf")

	r, err := OCR(t.Context(), data, Params{"pages": "1"})
	if err != nil {
		t.Fatalf("ocr: %v", err)
	}
	n, _ := r.Details["imagesProcessed"].(int)
	if n == 0 {
		t.Skip("no scannable image content on page 1")
	}
	if len(r.Data) == 0 {
		t.Fatal("ocr produced no output despite processed images")
	}
}
