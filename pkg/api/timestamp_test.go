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

package api

import (
	"bytes"
	"crypto/x509"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/huzpsb/pdfcpu2/internal/testtsa"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
)

func startTestTSA(t *testing.T, genTime time.Time) (*testtsa.TSA, *x509.CertPool, string) {
	t.Helper()
	tsa, err := testtsa.New(genTime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tsa.Close)

	pool := x509.NewCertPool()
	pool.AddCert(tsa.Certificate)
	return tsa, pool, tsa.URL()
}

func timestampValidationConf(host string) *model.Configuration {
	conf := model.NewStatelessConfiguration()
	conf.AllowedRevocationHosts = []string{host}
	conf.PreferredCertRevocationChecker = model.CRL
	return conf
}

func validateTimestamps(t *testing.T, data []byte, pool *x509.CertPool, host string) []*model.SignatureValidationResult {
	t.Helper()

	conf := timestampValidationConf(host)
	ctx, err := ReadAndValidate(t.Context(), bytes.NewReader(data), conf)
	if err != nil {
		t.Fatalf("re-read stamped file: %v", err)
	}

	results, err := pdfcpu.ValidateSignaturesWithCertificatePool(
		t.Context(), bytes.NewReader(data), ctx, true, pool,
	)
	if err != nil {
		t.Fatalf("validate signatures: %v", err)
	}
	return results
}

func TestTimestampRoundTrip(t *testing.T) {
	genTime := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	tsa, pool, tsaURL := startTestTSA(t, genTime)
	tsaHost := tsa.Hostname()

	source, err := os.ReadFile("../testdata/test.pdf")
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	info, err := Timestamp(t.Context(), bytes.NewReader(source), &out, tsaURL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !info.GenTime.Equal(genTime) {
		t.Fatalf("genTime = %s, want %s", info.GenTime, genTime)
	}
	if info.CertSubject == "" || info.Serial == nil {
		t.Fatalf("incomplete TSA info: %+v", info)
	}
	stamped := out.Bytes()

	// The original content must be preserved verbatim.
	if !bytes.HasPrefix(stamped, source) {
		t.Fatal("stamped file does not start with the original bytes")
	}

	results := validateTimestamps(t, stamped, pool, tsaHost)
	if len(results) != 1 {
		t.Fatalf("got %d signature results, want 1: %+v", len(results), results)
	}
	r := results[0]
	if r.Signature.Type != model.SigTypeDTS {
		t.Fatalf("signature type = %d, want SigTypeDTS", r.Signature.Type)
	}
	if r.Status != model.SignatureStatusValid {
		t.Fatalf("status = %s, report:\n%s\nproblems: %v\ndetails: %+v",
			r.Status, r.Signature.String(r.Status), r.Problems, r.Details)
	}
	if results[0].Details.FieldName != "Timestamp1" {
		t.Fatalf("automatic field name = %q, want Timestamp1", results[0].Details.FieldName)
	}
	if r.DocModified != model.False {
		t.Fatalf("DocModified = %v, want false", r.DocModified)
	}
	if len(r.Details.Signers) != 1 {
		t.Fatalf("signers = %+v", r.Details.Signers)
	}
	ts := r.Details.Signers[0].Evidence.Timestamp
	if !ts.Time.Equal(genTime) {
		t.Fatalf("timestamp evidence time = %s, want %s", ts.Time, genTime)
	}
	if ts.DigestVerified != model.True || ts.SignatureAuthenticated != model.True {
		t.Fatalf("timestamp evidence = %+v", ts)
	}
}

func TestTimestampChain(t *testing.T) {
	genTime := time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC)
	tsa, pool, tsaURL := startTestTSA(t, genTime)
	tsaHost := tsa.Hostname()

	source, err := os.ReadFile("../testdata/test.pdf")
	if err != nil {
		t.Fatal(err)
	}

	var first bytes.Buffer
	if _, err := Timestamp(t.Context(), bytes.NewReader(source), &first, tsaURL, "", nil); err != nil {
		t.Fatal(err)
	}

	// Second stamp on top of the first one extends the proof.
	tsa.GenTime = genTime.Add(time.Hour)
	var second bytes.Buffer
	if _, err := Timestamp(t.Context(), bytes.NewReader(first.Bytes()), &second, tsaURL, "", nil); err != nil {
		t.Fatal(err)
	}

	if !bytes.HasPrefix(second.Bytes(), first.Bytes()) {
		t.Fatal("second increment does not extend the first stamped file")
	}

	// The engine validates the latest document timestamp; earlier ones are
	// skipped as historical LTV evidence.
	results := validateTimestamps(t, second.Bytes(), pool, tsaHost)
	if len(results) != 1 {
		t.Fatalf("got %d signature results, want 1 (latest DTS only)", len(results))
	}
	r := results[0]
	if r.Signature.Type != model.SigTypeDTS {
		t.Fatalf("signature type = %d, want SigTypeDTS", r.Signature.Type)
	}
	if r.Status != model.SignatureStatusValid {
		t.Fatalf("status = %s, report:\n%s", r.Status, r.Signature.String(r.Status))
	}
	if len(r.Details.Signers) != 1 || !r.Details.Signers[0].Evidence.Timestamp.Time.Equal(genTime.Add(time.Hour)) {
		t.Fatalf("latest timestamp evidence = %+v, want %s",
			r.Details.Signers[0].Evidence.Timestamp, genTime.Add(time.Hour))
	}
}

func TestTimestampDetectsModification(t *testing.T) {
	genTime := time.Date(2026, 10, 31, 17, 0, 0, 0, time.UTC)
	tsa, pool, tsaURL := startTestTSA(t, genTime)
	tsaHost := tsa.Hostname()

	source, err := os.ReadFile("../testdata/test.pdf")
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Timestamp(t.Context(), bytes.NewReader(source), &out, tsaURL, "", nil); err != nil {
		t.Fatal(err)
	}

	// Corrupt one byte inside the covered range (page 1 content area).
	tampered := bytes.Clone(out.Bytes())
	tampered[100] ^= 0xff

	results := validateTimestamps(t, tampered, pool, tsaHost)
	if len(results) != 1 {
		t.Fatalf("got %d signature results, want 1", len(results))
	}
	r := results[0]
	if r.Status == model.SignatureStatusValid {
		t.Fatal("tampered document passed validation")
	}
	if r.DocModified != model.True {
		t.Fatalf("DocModified = %v, want true", r.DocModified)
	}
}

func TestTimestampFieldName(t *testing.T) {
	genTime := time.Date(2026, 10, 31, 19, 0, 0, 0, time.UTC)
	tsa, pool, tsaURL := startTestTSA(t, genTime)
	tsaHost := tsa.Hostname()

	source, err := os.ReadFile("../testdata/test.pdf")
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := Timestamp(t.Context(), bytes.NewReader(source), &out, tsaURL, "Evidence-1", nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Evidence-1")) {
		t.Fatal("custom field name missing from output")
	}

	results := validateTimestamps(t, out.Bytes(), pool, tsaHost)
	if len(results) != 1 {
		t.Fatalf("got %d signature results, want 1", len(results))
	}
	if got := results[0].Details.FieldName; got != "Evidence-1" {
		t.Fatalf("custom field name = %q, want Evidence-1", got)
	}
}

func TestTimestampRejectsEncryptedInput(t *testing.T) {
	genTime := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	_, _, tsaURL := startTestTSA(t, genTime)

	source, err := os.ReadFile("../testdata/test.pdf")
	if err != nil {
		t.Fatal(err)
	}

	var encrypted bytes.Buffer
	conf := model.NewDefaultConfiguration()
	conf.UserPW = "upw"
	conf.OwnerPW = "opw"
	if err := Encrypt(t.Context(), bytes.NewReader(source), &encrypted, conf); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	_, err = Timestamp(t.Context(), bytes.NewReader(encrypted.Bytes()), &out, tsaURL, "", nil)
	if !errors.Is(err, pdfcpu.ErrTimestampEncryptedInput) {
		t.Fatalf("got %v, want ErrTimestampEncryptedInput", err)
	}
}
