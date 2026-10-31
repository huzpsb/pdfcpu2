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

//go:build !js || !wasm

package sign

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
)

// doTSARequest posts the timestamp request via net/http with the configured
// timeout. See tsa_http_js.go for the browser transport.
func doTSARequest(c context.Context, u *url.URL, req []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(c, tsaHTTPTimeout)
	defer cancel()

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(req))
	if err != nil {
		return 0, nil, err
	}
	hreq.Header.Set("Content-Type", "application/timestamp-query")
	hreq.Header.Set("Accept", "application/timestamp-reply")

	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, tsaMaxResponseIO+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}
