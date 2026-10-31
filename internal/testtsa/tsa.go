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

// Package testtsa provides a minimal RFC 3161 timestamp authority for tests.
// Its tokens satisfy the structural requirements of pdfcpu2's document
// timestamp validator: a single signer, TSTInfo content, signed attributes
// incl. ESSSigningCertificateV2 and an exclusive critical timestamping EKU.
package testtsa

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/pkcs7"
)

var (
	oidTSTInfo              = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}
	oidSigningCertificateV2 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 47}
	oidExtKeyUsage          = asn1.ObjectIdentifier{2, 5, 29, 37}
	oidTimestamping         = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 8}
	oidTestPolicy           = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 54883, 1}
)

type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type requestMessageImprint struct {
	HashAlgorithm algorithmIdentifier
	HashedMessage []byte
}

type timeStampReq struct {
	Version        int
	MessageImprint requestMessageImprint
	Nonce          *big.Int `asn1:"optional"`
	CertReq        bool
}

type tstInfo struct {
	Version        int
	Policy         asn1.ObjectIdentifier
	MessageImprint requestMessageImprint
	SerialNumber   *big.Int
	GenTime        time.Time
	Accuracy       asn1.RawValue `asn1:"optional"`
	Nonce          *big.Int      `asn1:"optional"`
}

type pkiStatusInfo struct {
	Status       int
	StatusString asn1.RawValue `asn1:"optional"`
	FailInfo     asn1.BitString `asn1:"optional"`
}

type timeStampResp struct {
	Status pkiStatusInfo
	Token  asn1.RawValue `asn1:"optional"`
}

type essCertIDv2 struct {
	HashAlgorithm algorithmIdentifier `asn1:"optional"`
	CertHash      []byte
}

type signingCertificateV2 struct {
	Certs    []essCertIDv2
	Policies asn1.RawValue `asn1:"optional"`
}

// TSA is a test timestamp authority with a fixed clock. It models a
// production TSA: a CA root (the trust anchor) issues a dedicated
// timestamping leaf certificate whose revocation status is served as a CRL,
// so online validation can conclude a positive revocation status.
type TSA struct {
	// Certificate is the self-signed CA root. Add it to a certificate
	// pool to have the engine trust the tokens.
	Certificate *x509.Certificate

	// Leaf is the timestamping certificate that signs the tokens.
	Leaf *x509.Certificate

	rootKey *ecdsa.PrivateKey
	leafKey *ecdsa.PrivateKey

	// GenTime is the generation time asserted by every token.
	GenTime time.Time

	// Reject makes the handler answer with a PKIStatus rejection.
	Reject bool
	// CorruptImprint makes the handler stamp a different digest, which
	// RequestTimestamp's local verification rejects.
	CorruptImprint bool

	server *httptest.Server
	crl    []byte

	serial atomic.Int64
}

// New generates a test TSA with its own CA root and timestamping leaf
// certificate and starts its HTTP endpoint. Call Close when done.
func New(genTime time.Time) (*TSA, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("testtsa: listen: %w", err)
	}
	crlURL := "http://" + listener.Addr().String() + "/crl"

	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("testtsa: generate root key: %w", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("testtsa: generate leaf key: %w", err)
	}

	now := time.Now()
	root, err := createCertificate(&x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pdfcpu2 test TSA Root", Organization: []string{"pdfcpu2"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}, nil, &rootKey.PublicKey, rootKey)
	if err != nil {
		listener.Close()
		return nil, err
	}

	eku, err := asn1.Marshal([]asn1.ObjectIdentifier{oidTimestamping})
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("testtsa: marshal EKU: %w", err)
	}
	leaf, err := createCertificate(&x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "pdfcpu2 test TSA", Organization: []string{"pdfcpu2"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  false,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		CRLDistributionPoints: []string{crlURL},
		// Critical exclusive timestamping EKU via ExtraExtensions;
		// ExtKeyUsage is left unset so the encoder adds no duplicate.
		ExtraExtensions: []pkix.Extension{{
			Id:       oidExtKeyUsage,
			Critical: true,
			Value:    eku,
		}},
	}, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		listener.Close()
		return nil, err
	}

	crlDER, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:     big.NewInt(1),
		ThisUpdate: now.Add(-time.Hour),
		NextUpdate: now.Add(24 * time.Hour),
	}, root, rootKey)
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("testtsa: create CRL: %w", err)
	}

	tsa := &TSA{
		Certificate: root,
		Leaf:        leaf,
		rootKey:     rootKey,
		leafKey:     leafKey,
		GenTime:     genTime,
		crl:         crlDER,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", tsa.serveTimestamp)
	mux.HandleFunc("/crl", tsa.serveCRL)
	tsa.server = &httptest.Server{Listener: listener, Config: &http.Server{Handler: mux}}
	tsa.server.Start()
	return tsa, nil
}

func createCertificate(tmpl *x509.Certificate, parent *x509.Certificate, pub *ecdsa.PublicKey, signer *ecdsa.PrivateKey) (*x509.Certificate, error) {
	if parent == nil {
		parent = tmpl // self-signed
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		return nil, fmt.Errorf("testtsa: create certificate %q: %w", tmpl.Subject.CommonName, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("testtsa: parse certificate %q: %w", tmpl.Subject.CommonName, err)
	}
	return cert, nil
}

// URL returns the RFC 3161 endpoint of the TSA.
func (t *TSA) URL() string {
	return t.server.URL + "/"
}

// Hostname returns the hostname of the TSA endpoint, for
// Configuration.AllowedRevocationHosts.
func (t *TSA) Hostname() string {
	u, err := url.Parse(t.server.URL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Close shuts the TSA endpoint down.
func (t *TSA) Close() {
	t.server.Close()
}

func newCertificateAndCRL(key *ecdsa.PrivateKey, crlURL string) (*x509.Certificate, []byte, error) {
	eku, err := asn1.Marshal([]asn1.ObjectIdentifier{oidTimestamping})
	if err != nil {
		return nil, nil, fmt.Errorf("testtsa: marshal EKU: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "pdfcpu2 test TSA",
			Organization: []string{"pdfcpu2"},
		},
		NotBefore: now.Add(-time.Hour),
		NotAfter:  now.Add(24 * time.Hour),
		// Self-signed trust anchors have to be CAs for x509 chain
		// verification to accept their own signature; crlSign lets the
		// same key issue the CRL served at crlURL.
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		CRLDistributionPoints: []string{crlURL},
		// Critical exclusive timestamping EKU via ExtraExtensions;
		// ExtKeyUsage is left unset so the encoder adds no duplicate.
		ExtraExtensions: []pkix.Extension{{
			Id:       oidExtKeyUsage,
			Critical: true,
			Value:    eku,
		}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("testtsa: create certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("testtsa: parse certificate: %w", err)
	}

	crlDER, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:     big.NewInt(1),
		ThisUpdate: now.Add(-time.Hour),
		NextUpdate: now.Add(24 * time.Hour),
	}, cert, key)
	if err != nil {
		return nil, nil, fmt.Errorf("testtsa: create CRL: %w", err)
	}
	return cert, crlDER, nil
}

func (t *TSA) serveCRL(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/pkix-crl")
	_, _ = w.Write(t.crl)
}

func (t *TSA) serveTimestamp(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, respErr := t.respond(body)
	if respErr != nil {
		http.Error(w, respErr.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/timestamp-reply")
	_, _ = w.Write(resp)
}

func (t *TSA) respond(reqDER []byte) ([]byte, error) {
	var req timeStampReq
	rest, err := asn1.Unmarshal(reqDER, &req)
	if err != nil {
		return nil, fmt.Errorf("testtsa: parse request: %w", err)
	}
	if len(rest) > 0 {
		return nil, errors.New("testtsa: trailing request data")
	}
	if req.Version != 1 {
		return nil, fmt.Errorf("testtsa: request version %d", req.Version)
	}
	if t.Reject {
		return asn1.Marshal(timeStampResp{Status: pkiStatusInfo{
			Status:   2, // rejected
			FailInfo: asn1.BitString{Bytes: []byte{0x80}, BitLength: 8},
		}})
	}

	digest := req.MessageImprint.HashedMessage
	if t.CorruptImprint {
		digest = append([]byte(nil), digest...)
		digest[0] ^= 0xff
	}

	info := tstInfo{
		Version: 1,
		Policy:  oidTestPolicy,
		MessageImprint: requestMessageImprint{
			HashAlgorithm: algorithmIdentifier{Algorithm: pkcs7.OIDDigestAlgorithmSHA256},
			HashedMessage: digest,
		},
		SerialNumber: big.NewInt(t.serial.Add(1)),
		GenTime:      t.GenTime,
	}
	if req.Nonce != nil {
		info.Nonce = new(big.Int).Set(req.Nonce)
	}
	infoDER, err := asn1.Marshal(info)
	if err != nil {
		return nil, fmt.Errorf("testtsa: marshal TSTInfo: %w", err)
	}

	token, err := t.sign(infoDER)
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(timeStampResp{
		Status: pkiStatusInfo{Status: 0},
		Token:  asn1.RawValue{FullBytes: token},
	})
}

func (t *TSA) sign(infoDER []byte) ([]byte, error) {
	sd, err := pkcs7.NewSignedDataWithContent(oidTSTInfo, infoDER)
	if err != nil {
		return nil, fmt.Errorf("testtsa: signed data: %w", err)
	}

	certHash := sha256.Sum256(t.Leaf.Raw)
	essAttr := pkcs7.Attribute{
		Type: oidSigningCertificateV2,
		Value: signingCertificateV2{
			Certs: []essCertIDv2{{CertHash: certHash[:]}},
		},
	}

	digest := sha256.Sum256(infoDER)
	if err := sd.AddSignerChain(
		t.Leaf, t.leafKey, digest[:], pkcs7.OIDDigestAlgorithmSHA256,
		[]*x509.Certificate{t.Certificate},
		pkcs7.SignerInfoConfig{ExtraSignedAttributes: []pkcs7.Attribute{essAttr}},
	); err != nil {
		return nil, fmt.Errorf("testtsa: add signer: %w", err)
	}
	return sd.Finish()
}
