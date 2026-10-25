/*
Copyright 2018 The pdfcpu Authors.

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

package log

import (
	stdlog "log"
	"strings"
	"testing"
)

// TestLog verifies log.
func TestLog(t *testing.T) {

	Debug.Printf("Test%s\n", "log")
	Debug.Println("Testlog")
	Debug.Fatalf("Test%s\n", "Fail")
	Debug.Fatalln("TestFail")

	SetDefaultLoggers()
	Debug.Printf("Testlog\n")
	Debug.Println("Testlog")
	DisableLoggers()
}

func TestPrintDoesNotAppendNewline(t *testing.T) {
	var b strings.Builder
	SetCLILogger(stdlog.New(&b, "", 0))
	defer SetCLILogger(nil)

	CLI.Print(".")

	if got := b.String(); got != "." {
		t.Fatalf("got %q, want %q", got, ".")
	}
}
