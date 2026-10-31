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

//go:build js && wasm

package sign

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"syscall/js"
)

// doTSARequest posts the timestamp request through a synchronous
// XMLHttpRequest.
//
// The webui bridge executes engine operations inside synchronous syscall/js
// callbacks. Go's fetch based net/http transport cannot block there: the
// response needs a JS event loop dispatch, and the event loop is occupied by
// exactly this callback - using it deadlocks ("all goroutines are asleep").
// Synchronous XHR is the only blocking HTTP primitive available inside a
// Web Worker. Binary responses are recovered through the x-user-defined
// charset mapping because sync XHR cannot switch responseType.
//
// Limitations: context timeouts are not supported (sync XHR ignores them)
// and a hung TSA blocks the worker until the browser gives up.
func doTSARequest(_ context.Context, u *url.URL, req []byte) (statusCode int, body []byte, err error) {
	defer func() {
		if p := recover(); p != nil {
			statusCode, body, err = 0, nil, fmt.Errorf(
				"tsa: %s: request failed (network error or blocked by CORS): %v", u.Host, p)
		}
	}()

	xhr := js.Global().Get("XMLHttpRequest")
	if xhr.IsUndefined() || xhr.IsNull() {
		return 0, nil, errors.New("tsa: XMLHttpRequest unavailable")
	}
	r := xhr.New()
	r.Call("open", "POST", u.String(), false)
	r.Call("setRequestHeader", "Content-Type", "application/timestamp-query")
	r.Call("setRequestHeader", "Accept", "application/timestamp-reply")
	r.Call("overrideMimeType", "text/plain; charset=x-user-defined")

	reqBody := js.Global().Get("Uint8Array").New(len(req))
	if len(req) > 0 {
		js.CopyBytesToJS(reqBody, req)
	}
	r.Call("send", reqBody)

	status := r.Get("status").Int()
	text := r.Get("responseText")
	if text.Type() != js.TypeString {
		return status, nil, fmt.Errorf("tsa: %s: empty response", u.Host)
	}
	data, decodeErr := decodeUserDefined(text.String())
	if decodeErr != nil {
		return status, nil, fmt.Errorf("tsa: %s: decode response: %w", u.Host, decodeErr)
	}
	return status, data, nil
}

// decodeUserDefined reverses the x-user-defined charset mapping: response
// bytes 0x80..0xFF are delivered as code points U+F780..U+F7FF
// (code point = 0xF700 + byte).
func decodeUserDefined(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0x80:
			out = append(out, byte(r))
		case r >= 0xF780 && r <= 0xF7FF:
			out = append(out, byte(r-0xF700))
		default:
			return nil, fmt.Errorf("unexpected code point %#04x", r)
		}
	}
	return out, nil
}
