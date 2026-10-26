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

	"github.com/huzpsb/pdfcpu2/pkg/api"
	"github.com/huzpsb/pdfcpu2/pkg/cli"
)

func extractTextForObfuscation(t *testing.T, inFile, outFile string) string {
	t.Helper()
	if err := api.ExtractTextFile(t.Context(), inFile, outFile, nil, nil, conf); err != nil {
		t.Fatalf("extract text %s: %v\n", inFile, err)
	}
	bb, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(bb)
}

// TestObfuscateCommand verifies that obfuscate destroys the text extraction
// mapping of embedded fonts while the document stays valid.
func TestObfuscateCommand(t *testing.T) {
	msg := "TestObfuscateCommand"

	// go.pdf carries embedded fonts with cmap tables and ToUnicode CMaps.
	inFile := filepath.Join(inDir, "go.pdf")
	outFile := filepath.Join(outDir, "go_obfuscated.pdf")

	original := extractTextForObfuscation(t, inFile, filepath.Join(outDir, "go.txt"))
	if strings.TrimSpace(original) == "" {
		t.Fatalf("%s: %s has no extractable text\n", msg, inFile)
	}

	cmd := cli.ObfuscateCommand(inFile, outFile, conf)
	out, err := cli.Dispatch(t.Context(), cmd)
	if err != nil {
		t.Fatalf("%s: %v\n", msg, err)
	}
	if len(out) != 1 || !strings.Contains(out[0], "obfuscated 3 font program(s), 4 ToUnicode CMap(s), skipped 6 font(s)") {
		t.Fatalf("%s: unexpected summary %v\n", msg, out)
	}

	// The output must remain a valid PDF file.
	if _, err := cli.Dispatch(t.Context(), cli.ValidateCommand([]string{outFile}, conf)); err != nil {
		t.Fatalf("%s validate %s: %v\n", msg, outFile, err)
	}

	// Text extraction must no longer reproduce the original text.
	obfuscated := extractTextForObfuscation(t, outFile, filepath.Join(outDir, "go_obfuscated.txt"))
	if obfuscated == original {
		t.Fatalf("%s: text extraction still reproduces the original text\n", msg)
	}
}

// TestObfuscateCommandWithoutEmbeddedFonts verifies that a file without
// embedded font mappings passes through unchanged.
func TestObfuscateCommandWithoutEmbeddedFonts(t *testing.T) {
	msg := "TestObfuscateCommandWithoutEmbeddedFonts"

	inFile := filepath.Join(inDir, "testWithText.pdf")
	outFile := filepath.Join(outDir, "testWithText_obfuscated.pdf")

	cmd := cli.ObfuscateCommand(inFile, outFile, conf)
	out, err := cli.Dispatch(t.Context(), cmd)
	if err != nil {
		t.Fatalf("%s: %v\n", msg, err)
	}
	if len(out) != 1 || !strings.Contains(out[0], "obfuscated 0 font program(s), 0 ToUnicode CMap(s)") {
		t.Fatalf("%s: unexpected summary %v\n", msg, out)
	}

	if _, err := cli.Dispatch(t.Context(), cli.ValidateCommand([]string{outFile}, conf)); err != nil {
		t.Fatalf("%s validate %s: %v\n", msg, outFile, err)
	}
}
