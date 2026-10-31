//go:build js && wasm

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

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"syscall/js"

	"github.com/huzpsb/pdfcpu2/pkg/api"
)

// Run installs the pdfcpu2 engine bridge on the JS global object.
//
// Every function is synchronous and returns {ok:true, ...} or
// {ok:false, error:"..."}; binary payloads are Uint8Arrays.
func Run() {
	// The browser has no configuration directory: switch the engine to
	// its built-in stateless defaults before anything touches the fs.
	api.DisableConfigDir()

	if g := js.Global().Get("pdfcpu2"); !g.IsUndefined() && !g.IsNull() {
		return // already installed
	}

	obj := js.ValueOf(map[string]any{})
	reg := func(name string, fn func(args []js.Value) any) {
		obj.Set(name, js.FuncOf(func(_ js.Value, args []js.Value) any { return fn(args) }))
	}

	// one PDF in, params, one result out
	op1 := func(name string, f func(context.Context, []byte, Params) (*Result, error)) {
		reg(name, func(args []js.Value) any {
			r, err := f(context.Background(), bytesArg(args, 0), paramsArg(args, 1))
			return finish(r, err)
		})
	}
	// one PDF in, one result out
	op0 := func(name string, f func(context.Context, []byte) (*Result, error)) {
		reg(name, func(args []js.Value) any {
			r, err := f(context.Background(), bytesArg(args, 0))
			return finish(r, err)
		})
	}

	reg("version", func(args []js.Value) any {
		return ok("version", Version())
	})

	reg("probe", func(args []js.Value) any {
		r, err := Probe(context.Background(), bytesArg(args, 0))
		return finish(r, err)
	})

	reg("renderPage", func(args []js.Value) any {
		r, err := RenderPage(context.Background(), bytesArg(args, 0), int(numArg(args, 1, 1)), numArg(args, 2, 96))
		return finish(r, err)
	})

	op1("encrypt", Encrypt)
	op1("decrypt", Decrypt)
	op1("timestamp", Timestamp)
	op1("ocr", OCR)
	op1("solidify", Solidify)
	op1("totext", ToText)
	op1("replace", Replace)
	op0("obfuscate", Obfuscate)
	op0("optimize", Optimize)

	reg("watermarkText", func(args []js.Value) any {
		r, err := WatermarkText(context.Background(), bytesArg(args, 0), paramsArg(args, 1))
		return finish(r, err)
	})

	reg("watermarkImage", func(args []js.Value) any {
		r, err := WatermarkImage(context.Background(), bytesArg(args, 0), bytesArg(args, 1), strArg(args, 2, "logo"), paramsArg(args, 3))
		return finish(r, err)
	})

	reg("split", func(args []js.Value) any {
		r, err := Split(context.Background(), bytesArg(args, 0), strArg(args, 1, "document.pdf"), paramsArg(args, 2))
		return finish(r, err)
	})

	reg("merge", func(args []js.Value) any {
		r, err := Merge(context.Background(), datasArg(args, 0), paramsArg(args, 1))
		return finish(r, err)
	})

	reg("img2pdf", func(args []js.Value) any {
		r, err := Img2PDF(context.Background(), namedArg(args, 0), paramsArg(args, 1))
		return finish(r, err)
	})

	reg("attList", func(args []js.Value) any {
		r, err := AttList(context.Background(), bytesArg(args, 0))
		return finish(r, err)
	})

	reg("attAdd", func(args []js.Value) any {
		r, err := AttAdd(context.Background(), bytesArg(args, 0), namedArg(args, 1), strArg(args, 2, ""))
		return finish(r, err)
	})

	reg("attRemove", func(args []js.Value) any {
		r, err := AttRemove(context.Background(), bytesArg(args, 0), stringListArg(args, 1))
		return finish(r, err)
	})

	reg("attExtract", func(args []js.Value) any {
		r, err := AttExtract(context.Background(), bytesArg(args, 0), stringListArg(args, 1))
		return finish(r, err)
	})

	reg("sigValidate", func(args []js.Value) any {
		r, err := SigValidate(context.Background(), bytesArg(args, 0), boolArg(args, 1, false))
		return finish(r, err)
	})

	reg("sigRemove", func(args []js.Value) any {
		r, err := SigRemove(context.Background(), bytesArg(args, 0))
		return finish(r, err)
	})

	reg("batch", func(args []js.Value) any {
		op := strArg(args, 0, "")
		items := namedArg(args, 1)
		p := paramsArg(args, 2)
		logo := bytesArgOrEmpty(args, 3)
		logoName := strArg(args, 4, "logo")
		r, err := Batch(context.Background(), op, items, p, logo, logoName)
		return finish(r, err)
	})

	js.Global().Set("pdfcpu2", obj)
}

// Version returns the webui build identifier.
func Version() string {
	return fmt.Sprintf("pdfcpu2-wasm (%s)", runtime.Version())
}

// ---- result wrapping ----

func finish(r *Result, err error) any {
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	m := map[string]any{"ok": true}
	for k, v := range r.Details {
		m[k] = plainValue(v)
	}
	if r.Data != nil {
		m["data"] = jsBytes(r.Data)
		m["name"] = r.Name
	}
	if len(r.Files) > 0 {
		files := make([]any, 0, len(r.Files))
		for _, f := range r.Files {
			files = append(files, map[string]any{"name": f.Name, "data": jsBytes(f.Data)})
		}
		m["files"] = files
	}
	return m
}

func ok(key string, v any) any {
	return map[string]any{"ok": true, key: v}
}

// plainValue makes Details values representable as JS values.
func plainValue(v any) any {
	switch t := v.(type) {
	case nil, bool, int, int64, float64, string:
		return t
	case []string:
		a := make([]any, len(t))
		for i, s := range t {
			a[i] = s
		}
		return a
	case []batchItem:
		a := make([]any, len(t))
		for i, it := range t {
			a[i] = map[string]any{"name": it.Name, "ok": it.OK, "error": it.Error, "note": it.Note}
		}
		return a
	case []map[string]any:
		a := make([]any, len(t))
		for i, m := range t {
			a[i] = plainMap(m)
		}
		return a
	case map[string]any:
		return plainMap(t)
	default:
		return fmt.Sprint(t)
	}
}

func plainMap(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = plainValue(v)
	}
	return out
}

// ---- argument conversion ----

func arg(args []js.Value, i int) js.Value {
	if i < len(args) {
		return args[i]
	}
	return js.Undefined()
}

func bytesArg(args []js.Value, i int) []byte {
	return jsToBytes(arg(args, i))
}

func bytesArgOrEmpty(args []js.Value, i int) []byte {
	v := arg(args, i)
	if v.IsUndefined() || v.IsNull() {
		return nil
	}
	return jsToBytes(v)
}

func jsToBytes(v js.Value) []byte {
	if v.IsUndefined() || v.IsNull() {
		return nil
	}
	if v.Length() == 0 {
		return []byte{}
	}
	b := make([]byte, v.Length())
	if n := js.CopyBytesToGo(b, v); n != len(b) {
		return b[:n]
	}
	return b
}

func jsBytes(b []byte) js.Value {
	a := js.Global().Get("Uint8Array").New(len(b))
	if len(b) > 0 {
		js.CopyBytesToJS(a, b)
	}
	return a
}

func goValue(v js.Value) any {
	switch v.Type() {
	case js.TypeUndefined, js.TypeNull:
		return nil
	case js.TypeBoolean:
		return v.Bool()
	case js.TypeNumber:
		return v.Float()
	case js.TypeString:
		return v.String()
	case js.TypeObject, js.TypeSymbol:
		if v.InstanceOf(js.Global().Get("Array")) {
			n := v.Length()
			a := make([]any, n)
			for i := 0; i < n; i++ {
				a[i] = goValue(v.Index(i))
			}
			return a
		}
		if v.InstanceOf(js.Global().Get("Uint8Array")) {
			return jsToBytes(v)
		}
		entries := js.Global().Get("Object").Call("entries", v)
		m := map[string]any{}
		n := entries.Length()
		for i := 0; i < n; i++ {
			pair := entries.Index(i)
			k := pair.Index(0).String()
			m[k] = goValue(pair.Index(1))
		}
		return m
	}
	return nil
}

func paramsArg(args []js.Value, i int) Params {
	v := goValue(arg(args, i))
	if m, ok := v.(map[string]any); ok {
		return Params(m)
	}
	return Params{}
}

func strArg(args []js.Value, i int, def string) string {
	v := arg(args, i)
	if v.Type() == js.TypeString {
		return v.String()
	}
	return def
}

func numArg(args []js.Value, i int, def float64) float64 {
	v := arg(args, i)
	if v.Type() == js.TypeNumber {
		return v.Float()
	}
	return def
}

func boolArg(args []js.Value, i int, def bool) bool {
	v := arg(args, i)
	if v.Type() == js.TypeBoolean {
		return v.Bool()
	}
	return def
}

func datasArg(args []js.Value, i int) [][]byte {
	v := arg(args, i)
	if v.Type() != js.TypeObject || !v.InstanceOf(js.Global().Get("Array")) {
		return nil
	}
	out := make([][]byte, 0, v.Length())
	for j := 0; j < v.Length(); j++ {
		if b := jsToBytes(v.Index(j)); b != nil {
			out = append(out, b)
		}
	}
	return out
}

func namedArg(args []js.Value, i int) []NamedBytes {
	v := arg(args, i)
	if v.Type() != js.TypeObject || !v.InstanceOf(js.Global().Get("Array")) {
		return nil
	}
	out := make([]NamedBytes, 0, v.Length())
	for j := 0; j < v.Length(); j++ {
		it := v.Index(j)
		if it.Type() != js.TypeObject {
			continue
		}
		name := it.Get("name").String()
		data := jsToBytes(it.Get("data"))
		if data == nil {
			continue
		}
		out = append(out, NamedBytes{Name: name, Data: data})
	}
	return out
}

func stringListArg(args []js.Value, i int) []string {
	v := goValue(arg(args, i))
	switch t := v.(type) {
	case []any:
		ss := make([]string, 0, len(t))
		for _, it := range t {
			if s, ok := it.(string); ok {
				ss = append(ss, s)
			}
		}
		return ss
	case string:
		return splitComma(t)
	}
	return nil
}

func splitComma(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
