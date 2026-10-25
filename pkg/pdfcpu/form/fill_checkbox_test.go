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

package form

import (
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// TestFillCheckBoxWithKidWidgetUsesWidgetAP verifies checkbox filling resolves AS names from kid widgets.
func TestFillCheckBoxWithKidWidgetUsesWidgetAP(t *testing.T) {
	ctx, err := model.NewContext(strings.NewReader(""), model.NewDefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}

	kid := types.Dict{
		"AS": types.Name("Off"),
		"AP": types.Dict{
			"N": types.Dict{
				"Off":     types.Dict{},
				"Checked": types.Dict{},
			},
		},
	}
	kidIR, err := ctx.IndRefForObject(1, kid)
	if err != nil {
		t.Fatal(err)
	}

	field := types.Dict{
		"V":    types.Name("Off"),
		"Kids": types.Array{*kidIR},
	}

	ok := false
	if err := fillCheckBox(ctx, field, "agree", "agree", false, JSON, fillCheckBoxValue(true), &ok); err != nil {
		t.Fatal(err)
	}
	assertCheckBoxState(t, field, kid, ok, types.Name("Checked"))

	ok = false
	if err := fillCheckBox(ctx, field, "agree", "agree", false, JSON, fillCheckBoxValue(false), &ok); err != nil {
		t.Fatal(err)
	}
	assertCheckBoxState(t, field, kid, ok, types.Name("Off"))
}

func fillCheckBoxValue(checked bool) func(string, string, FieldType, DataFormat) ([]string, bool, bool) {
	value := "f"
	if checked {
		value = "t"
	}
	return func(string, string, FieldType, DataFormat) ([]string, bool, bool) {
		return []string{value}, false, true
	}
}

func assertCheckBoxState(t *testing.T, field, kid types.Dict, ok bool, want types.Name) {
	t.Helper()

	if !ok {
		t.Fatal("expected checkbox to be updated")
	}
	if got := field.NameEntry("V"); got == nil || types.Name(*got) != want {
		t.Fatalf("field V = %v, want %s", got, want)
	}
	if got := kid.NameEntry("AS"); got == nil || types.Name(*got) != want {
		t.Fatalf("kid AS = %v, want %s", got, want)
	}
}
