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

package sanitize

import "testing"

// TestPath verifies path sanitization.
func TestPath(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want string
	}{
		{"foo/.", "foo"},
		{"bar/..", "bar"},
		{"foo/bar/.", "foo_bar"},
		{"foo/bar/", "foo_bar"},
		{"foo/./bar/..", "foo_bar"},
		{"foo/./bar/./..", "foo_bar"},
		{"foo/./bar/../.", "foo_bar"},
		{"foo/./bar/../..", "foo_bar"},
		{"foo/./bar/", "foo_bar"},
		{"foo/../bar/..", "foo_bar"},
		{"docs/report.pdf", "docs_report.pdf"},
		{"../../etc/passwd", "etc_passwd"},
		{"/etc/passwd", "etc_passwd"},
		{"subdir/../bar//../file.txt", "subdir_bar_file.txt"},
		{`..\..\etc\passwd`, "etc_passwd"},
		{`C:\temp\report.pdf`, "temp_report.pdf"},
		{`\\server\share\report.pdf`, "server_share_report.pdf"},
		{`bad:name?.txt`, "bad_name_.txt"},
		{"NUL.txt", "_NUL.txt"},
		{"COM1", "_COM1"},
	} {
		got, err := Path(tt.in)
		if err != nil {
			t.Fatalf("Path(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Fatalf("Path(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestPathRejectsNoUsableFilename verifies unusable filenames are rejected.
func TestPathRejectsNoUsableFilename(t *testing.T) {
	for _, s := range []string{"", ".", "..", "../.."} {
		if _, err := Path(s); err == nil {
			t.Fatalf("Path(%q): expected error", s)
		}
	}
}

// TestPathRejectsNUL verifies NUL bytes are rejected.
func TestPathRejectsNUL(t *testing.T) {
	if _, err := Path("bad\x00name"); err == nil {
		t.Fatal("expected NUL byte error")
	}
}

// TestPathOrUsesFallback verifies fallback path behavior.
func TestPathOrUsesFallback(t *testing.T) {
	if got := PathOr("", "attachment"); got != "attachment" {
		t.Fatalf("PathOr returned %q, want attachment", got)
	}
}
