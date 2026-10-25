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

package test

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeBookmarkModeRequiresBookmarks(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "out.pdf")
	out, err := runPDFCPU(t, "merge", "--bookmarks=false", "--bookmark-mode", "preserve", outFile, "in.pdf")
	if err == nil {
		t.Fatalf("expected merge to fail, output:\n%s", out)
	}
	if want := "merge: --bookmark-mode requires --bookmarks"; !strings.Contains(string(out), want) {
		t.Fatalf("expected output to contain %q, got:\n%s", want, out)
	}
}
