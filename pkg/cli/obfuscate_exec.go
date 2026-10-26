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

package cli

import (
	"context"
	"fmt"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/api"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu"
)

// obfuscate destroys the text extraction mappings of all embedded fonts.
func obfuscate(c context.Context, cmd *Command) ([]string, error) {
	if err := contextutil.Check(c); err != nil {
		return nil, err
	}
	if err := validatePageInputOutputCommand(cmd, "obfuscate"); err != nil {
		return nil, err
	}

	inFile := *cmd.InFile
	outFile := optionalCommandString(cmd.OutFile)

	reportCommandProgress(cmd, "obfuscating %s\n", inFile)
	reportCommandOutputPath(cmd)

	var stats pdfcpu.ObfuscateStats
	var err error
	if inFile != "-" && outFile != "-" {
		stats, err = api.ObfuscateFile(c, inFile, outFile, cmd.Conf)
	} else {
		rs, w, finalize, streamErr := streamInOutForOperation(c, cmd.Conf, inFile, outFile, "obfuscate")
		if streamErr != nil {
			return nil, streamErr
		}
		stats, err = api.Obfuscate(c, rs, w, cmd.Conf)
		err = finalize(err)
	}
	if err != nil {
		return nil, err
	}

	if commandWritesPDFToStdout(cmd) {
		return nil, nil
	}
	return []string{fmt.Sprintf("obfuscated %d font program(s), %d ToUnicode CMap(s), skipped %d font(s)", stats.FontFiles, stats.ToUnicode, stats.Skipped)}, nil
}
