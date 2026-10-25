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
	"strings"
	"testing"
)

func TestUserStringGuardsRejectEmptyStrings(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"keywords add", []string{"keywords", "add", "in.pdf", ""}, "keyword must not be empty"},
		{"keywords remove", []string{"keywords", "remove", "in.pdf", ""}, "keyword must not be empty"},
		{"properties add name", []string{"properties", "add", "in.pdf", "=value"}, "property name must not be empty"},
		{"properties add value", []string{"properties", "add", "in.pdf", "subject="}, "property value must not be empty"},
		{"properties remove", []string{"properties", "remove", "in.pdf", ""}, "property name must not be empty"},
		{"attachment add", []string{"attachments", "add", "in.pdf", ",desc"}, "attachment filename must not be empty"},
		{"attachment remove", []string{"attachments", "remove", "in.pdf", ""}, "attachment filename must not be empty"},
		{"annotation", []string{"annotations", "remove", "in.pdf", ""}, "annotation ID or type must not be empty"},
		{"font cheatsheet", []string{"fonts", "cheatsheet", ""}, "font name must not be empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runPDFCPU(t, tt.args...)
			if err == nil {
				t.Fatalf("expected command to fail, output:\n%s", out)
			}
			if !strings.Contains(string(out), tt.want) {
				t.Fatalf("expected output to contain %q, got:\n%s", tt.want, out)
			}
		})
	}
}
