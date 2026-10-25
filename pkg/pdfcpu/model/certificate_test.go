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

package model

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResetCertificates(t *testing.T) {
	certDir := t.TempDir()
	restoreTrustedCertDir(t, certDir)

	staleCert := filepath.Join(certDir, "stale.pem")
	if err := os.WriteFile(staleCert, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ResetCertificates(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(staleCert); !os.IsNotExist(err) {
		t.Fatalf("stale certificate still exists: %v", err)
	}
}

func restoreTrustedCertDir(t *testing.T, certDir string) {
	t.Helper()
	orig := TrustedCertDir
	TrustedCertDir = certDir
	t.Cleanup(func() {
		TrustedCertDir = orig
	})
}
