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

package safemath

import (
	"math"
	"testing"
)

// TestAddIntRejectsOverflow verifies integer addition overflow is rejected.
func TestAddIntRejectsOverflow(t *testing.T) {
	if _, err := AddInt(math.MaxInt, 1); err == nil {
		t.Fatal("expected overflow")
	}
}

// TestMultiplyIntRejectsOverflow verifies integer multiplication overflow is rejected.
func TestMultiplyIntRejectsOverflow(t *testing.T) {
	if _, err := MultiplyInt(math.MaxInt, 2); err == nil {
		t.Fatal("expected overflow")
	}
}

// TestMultiplyInt64RejectsOverflow verifies int64 multiplication overflow is rejected.
func TestMultiplyInt64RejectsOverflow(t *testing.T) {
	if _, err := MultiplyInt64(math.MaxInt64, 2); err == nil {
		t.Fatal("expected overflow")
	}
}
