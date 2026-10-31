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
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/pkcs7"
)

// DefaultTSAURL is the timestamp authority used when no other URL is
// configured: DigiCert's public RFC 3161 service.
const DefaultTSAURL = "http://timestamp.digicert.com"

const (
	tsaHTTPTimeout   = 30 * time.Second
	tsaMaxResponseIO = 4 << 20 // a token incl. certificate chain is a few KB
)

// timeStampReq is an RFC 3161 TimeStampReq.
type timeStampReq struct {
	Version        int
	MessageImprint requestMessageImprint
	Nonce          *big.Int `asn1:"optional"`
	CertReq        bool
}

// requestMessageImprint mirrors TSTInfo.MessageImprint for marshalling.
type requestMessageImprint struct {
	HashAlgorithm AlgorithmIdentifier
	HashedMessage []byte
}

// pkiStatusInfo is the RFC 3161 PKIStatusInfo of a timestamp response.
type pkiStatusInfo struct {
	Status       int
	StatusString asn1.RawValue `asn1:"optional"`
	FailInfo     asn1.BitString `asn1:"optional"`
}

// timeStampResp is an RFC 3161 TimeStampResp.
type timeStampResp struct {
	Status pkiStatusInfo
	Token  asn1.RawValue `asn1:"optional"`
}

// TSAInfo describes one granted timestamp.
type TSAInfo struct {
	// URL is the timestamp authority that produced the token.
	URL string
	// Token is the CMS TimeStampToken that goes into /Contents.
	Token []byte
	// GenTime is the time of stamping asserted by the TSA.
	GenTime time.Time
	// Serial is the TSA-specific token serial number.
	Serial *big.Int
	// Policy is the TSA timestamp policy OID.
	Policy asn1.ObjectIdentifier
	// CertSubject identifies the TSA signing certificate.
	CertSubject string
}

// RequestTimestamp posts an RFC 3161 timestamp request for digest to tsaURL
// and returns the verified token. An empty tsaURL selects DefaultTSAURL.
// The response token is validated locally (TSTInfo profile, message imprint)
// before it is returned, so callers can rely on it binding their digest.
func RequestTimestamp(c context.Context, tsaURL string, digest []byte, hash crypto.Hash) (*TSAInfo, error) {
	if c != nil {
		if err := c.Err(); err != nil {
			return nil, err
		}
	}
	if tsaURL = strings.TrimSpace(tsaURL); tsaURL == "" {
		tsaURL = DefaultTSAURL
	}
	u, err := ParseTSAURL(tsaURL)
	if err != nil {
		return nil, err
	}
	if len(digest) == 0 {
		return nil, errors.New("tsa: missing digest")
	}
	if !hash.Available() {
		return nil, fmt.Errorf("tsa: digest %s unavailable", hash)
	}

	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("tsa: nonce: %w", err)
	}
	req, err := asn1.Marshal(timeStampReq{
		Version: 1,
		MessageImprint: requestMessageImprint{
			HashAlgorithm: AlgorithmIdentifier{Algorithm: pkcs7.OIDDigestAlgorithmSHA256},
			HashedMessage: digest,
		},
		Nonce:  new(big.Int).SetBytes(nonce),
		CertReq: true,
	})
	if err != nil {
		return nil, fmt.Errorf("tsa: marshal request: %w", err)
	}

	token, err := postTimestampRequest(c, u, req)
	if err != nil {
		return nil, err
	}
	return verifiedTSAInfo(tsaURL, token, digest)
}

// ParseTSAURL validates a timestamp authority URL.
func ParseTSAURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("tsa: invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("tsa: URL scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("tsa: URL host missing")
	}
	if u.User != nil {
		return nil, errors.New("tsa: URL must not carry credentials")
	}
	return u, nil
}

func postTimestampRequest(c context.Context, u *url.URL, req []byte) ([]byte, error) {
	statusCode, body, err := doTSARequest(c, u, req)
	if err != nil {
		return nil, err
	}
	if len(body) > tsaMaxResponseIO {
		return nil, fmt.Errorf("tsa: response exceeds %d bytes", tsaMaxResponseIO)
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("tsa: %s: status %d", u.Host, statusCode)
	}

	var tsr timeStampResp
	rest, err := asn1.Unmarshal(body, &tsr)
	if err != nil {
		return nil, fmt.Errorf("tsa: parse response: %w", err)
	}
	if len(rest) > 0 {
		return nil, errors.New("tsa: parse response: trailing data")
	}
	switch tsr.Status.Status {
	case 0, 1: // granted, grantedWithMods
	default:
		return nil, fmt.Errorf("tsa: %s: request rejected: status %d%s",
			u.Host, tsr.Status.Status, tsaFailInfo(tsr.Status.FailInfo))
	}
	if len(tsr.Token.FullBytes) == 0 {
		return nil, fmt.Errorf("tsa: %s: granted without token", u.Host)
	}
	return tsr.Token.FullBytes, nil
}

func tsaFailInfo(failInfo asn1.BitString) string {
	if len(failInfo.Bytes) == 0 {
		return ""
	}
	return fmt.Sprintf(" failInfo %#02x", failInfo.Bytes[0])
}

// verifiedTSAInfo validates the token like the DTS validator will and
// extracts the reportable TSA information.
func verifiedTSAInfo(tsaURL string, token, digest []byte) (*TSAInfo, error) {
	p7, err := pkcs7.Parse(token)
	if err != nil {
		return nil, fmt.Errorf("tsa: parse token: %w", err)
	}
	if !p7.ContentType.Equal(oidTSTInfo) {
		return nil, errors.New("tsa: token content is not timestamp info")
	}
	if len(p7.Signers) != 1 {
		return nil, fmt.Errorf("tsa: token has %d signers, want 1", len(p7.Signers))
	}
	tstInfo, err := parseTSTInfo(p7.Content)
	if err != nil {
		return nil, fmt.Errorf("tsa: token timestamp info: %w", err)
	}
	if !bytes.Equal(tstInfo.MessageImprint.HashedMessage, digest) {
		return nil, errors.New("tsa: token message imprint does not match digest")
	}

	info := &TSAInfo{
		URL:     tsaURL,
		Token:   token,
		GenTime: tstInfo.GenTime,
		Serial:  new(big.Int).SetBytes(tstInfo.SerialNumber.Bytes),
		Policy:  tstInfo.Policy,
	}
	signer, err := pkcs7.GetCertFromCertsByIssuerAndSerial(
		p7.Certificates, p7.Signers[0].IssuerAndSerialNumber)
	if err == nil && signer != nil {
		info.CertSubject = signer.Subject.String()
	}
	return info, nil
}
