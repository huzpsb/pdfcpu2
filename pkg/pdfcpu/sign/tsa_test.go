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

package sign

import (
	"crypto"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huzpsb/pdfcpu2/internal/testtsa"
)

func TestParseTSAURL(t *testing.T) {
	tests := []struct {
		url     string
		wantErr string
	}{
		{"http://timestamp.digicert.com", ""},
		{"https://tsa.example.com/api", ""},
		{"ftp://tsa.example.com", `scheme must be http or https`},
		{"https://user:pw@tsa.example.com", "must not carry credentials"},
		{"https://", "host missing"},
	}
	for _, tt := range tests {
		u, err := ParseTSAURL(tt.url)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("%q: unexpected error %v", tt.url, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%q: got %v, want error containing %q", tt.url, err, tt.wantErr)
		}
		_ = u
	}
}

func TestRequestTimestampRoundTrip(t *testing.T) {
	genTime := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tsa, err := testtsa.New(genTime)
	if err != nil {
		t.Fatal(err)
	}
	defer tsa.Close()

	digest := sha256.Sum256([]byte("covered document bytes"))
	info, err := RequestTimestamp(t.Context(), tsa.URL(), digest[:], crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if !info.GenTime.Equal(genTime) {
		t.Fatalf("genTime = %s, want %s", info.GenTime, genTime)
	}
	if info.CertSubject == "" || info.Serial == nil || len(info.Policy) == 0 {
		t.Fatalf("incomplete info: %+v", info)
	}
	if len(info.Token) == 0 {
		t.Fatal("no token")
	}
	if !strings.Contains(info.CertSubject, "pdfcpu2 test TSA") {
		t.Fatalf("unexpected signer %q", info.CertSubject)
	}
}

func TestRequestTimestampRejectsAndCorrupts(t *testing.T) {
	tsa, err := testtsa.New(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer tsa.Close()

	digest := sha256.Sum256([]byte("covered document bytes"))

	tsa.Reject = true
	if _, err := RequestTimestamp(t.Context(), tsa.URL(), digest[:], crypto.SHA256); err == nil ||
		!strings.Contains(err.Error(), "rejected") {
		t.Fatalf("rejection: got %v, want rejected error", err)
	}
	tsa.Reject = false

	tsa.CorruptImprint = true
	if _, err := RequestTimestamp(t.Context(), tsa.URL(), digest[:], crypto.SHA256); err == nil ||
		!strings.Contains(err.Error(), "imprint does not match") {
		t.Fatalf("corrupt imprint: got %v, want imprint mismatch", err)
	}
}

func TestRequestTimestampHTTPFailures(t *testing.T) {
	digest := sha256.Sum256([]byte("covered document bytes"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer server.Close()

	if _, err := RequestTimestamp(t.Context(), server.URL, digest[:], crypto.SHA256); err == nil ||
		!strings.Contains(err.Error(), "status 500") {
		t.Fatalf("got %v, want status error", err)
	}

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not-der"))
	}))
	defer garbage.Close()

	if _, err := RequestTimestamp(t.Context(), garbage.URL, digest[:], crypto.SHA256); err == nil {
		t.Fatal("garbage response must fail")
	}
}

func TestRequestTimestampRequiresDigest(t *testing.T) {
	if _, err := RequestTimestamp(t.Context(), "http://127.0.0.1:1", nil, crypto.SHA256); err == nil {
		t.Fatal("missing digest must fail")
	}
	if _, err := RequestTimestamp(t.Context(), "://bad", []byte{1}, crypto.SHA256); err == nil {
		t.Fatal("invalid URL must fail")
	}
}
