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

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/api"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
)

// ReplaceTextCommand creates a new command to replace every occurrence of a
// find string by replacement text drawn with an embedded vivo Sans subset
// while the original glyphs are removed from the content streams.
func ReplaceTextCommand(inFile, outFile, find, repl string, pageSelection []string, conf *model.Configuration) *Command {
	if conf == nil {
		conf = model.NewDefaultConfiguration()
		conf.Cmd = model.REPLACETEXT
	}
	return &Command{
		Mode:          model.REPLACETEXT,
		InFile:        &inFile,
		OutFile:       &outFile,
		FindString:    &find,
		ReplaceString: &repl,
		PageSelection: pageSelection,
		Conf:          conf,
	}
}

func replaceText(c context.Context, cmd *Command) ([]string, error) {
	if err := contextutil.Check(c); err != nil {
		return nil, err
	}
	if err := validateCommandRequirements(cmd, commandRequirements{
		operation: "replace",
		inFile:    commandStringRequiredNonEmpty,
		outFile:   commandStringRequiredNonEmpty,
	}); err != nil {
		return nil, err
	}
	reportCommandProgress(cmd, "replace %q with %q in %s ...\n",
		*cmd.FindString, *cmd.ReplaceString, *cmd.InFile)

	n, err := api.ReplaceTextFile(c, *cmd.InFile, *cmd.OutFile, *cmd.FindString, *cmd.ReplaceString, cmd.PageSelection, cmd.Conf)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		reportCommandProgress(cmd, "no occurrences found, %s not written\n", *cmd.OutFile)
	} else {
		reportCommandProgress(cmd, "%d occurrences replaced, %s written\n", n, *cmd.OutFile)
	}
	return nil, nil
}
