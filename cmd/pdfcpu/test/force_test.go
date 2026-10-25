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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForceFlagAllowsExplicitOverwrite(t *testing.T) {
	inFile := repoFile(t, "pkg", "testdata", "test.pdf")

	for _, tt := range []struct {
		name    string
		exists  bool
		force   bool
		wantErr bool
	}{
		{name: "new output"},
		{name: "existing output", exists: true, wantErr: true},
		{name: "existing output with force", exists: true, force: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			outFile := filepath.Join(t.TempDir(), "out.pdf")
			if tt.exists {
				if err := os.WriteFile(outFile, []byte("existing"), 0644); err != nil {
					t.Fatal(err)
				}
			}

			args := []string{"optimize", inFile, outFile}
			if tt.force {
				args = append(args, "--force")
			}
			out, err := runPDFCPU(t, args...)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected optimize to fail, output:\n%s", out)
				}
				if want := "refusing to overwrite existing file:"; !strings.Contains(string(out), want) {
					t.Fatalf("expected output to contain %q, got:\n%s", want, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected optimize to succeed, got %v:\n%s", err, out)
			}
		})
	}
}

func TestForceFlagProtectsRepresentativeOutputs(t *testing.T) {
	inFile := repoFile(t, "pkg", "testdata", "test.pdf")
	for _, tt := range []struct {
		name string
		args func(outFile string) []string
	}{
		{"merge", func(outFile string) []string { return []string{"merge", outFile, inFile} }},
		{"trim", func(outFile string) []string { return []string{"trim", "-p", "1", inFile, outFile} }},
		{"watermark add", func(outFile string) []string { return []string{"watermark", "add", "Draft", "pos:c", inFile, outFile} }},
		{"keywords add", func(outFile string) []string { return []string{"keywords", "add", inFile, outFile, "force-test"} }},
		{"bookmarks remove", func(outFile string) []string { return []string{"bookmarks", "remove", inFile, outFile} }},
		{"viewerpref reset", func(outFile string) []string { return []string{"viewerpref", "reset", inFile, outFile} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			outFile := filepath.Join(t.TempDir(), "out.pdf")
			if err := os.WriteFile(outFile, []byte("existing"), 0644); err != nil {
				t.Fatal(err)
			}
			out, err := runPDFCPU(t, tt.args(outFile)...)
			if err == nil {
				t.Fatalf("expected command to fail for existing output, output:\n%s", out)
			}
			want := "refusing to overwrite existing file: " + outFile
			if !strings.Contains(string(out), want) {
				t.Fatalf("expected output to contain %q, got:\n%s", want, out)
			}
		})
	}
}
