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
	"io"
	"os"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/api"
	"github.com/huzpsb/pdfcpu2/pkg/render"
)

func renderOptions(o *render.Options) (*render.Options, error) {
	if o == nil {
		o = &render.Options{}
	}
	if _, err := render.NormalizeFormat(o.Format); err != nil {
		return nil, err
	}
	return o, nil
}

func renderInputLabel(inFile string) string {
	if inFile == "-" {
		return "stdin"
	}
	return inFile
}

func renderPages(c context.Context, cmd *Command) ([]string, error) {
	if err := contextutil.Check(c); err != nil {
		return nil, err
	}
	if err := validateCommandRequirements(cmd, commandRequirements{
		operation: "render pages",
		inFile:    commandStringRequiredNonEmpty,
		outDir:    commandStringRequiredNonEmpty,
	}); err != nil {
		return nil, err
	}
	o, err := renderOptions(cmd.Render)
	if err != nil {
		return nil, commandValidationError("render pages", err)
	}
	cmd.Render = o

	reportCommandProgress(cmd, "rendering %s into %s/ ...\n", renderInputLabel(*cmd.InFile), *cmd.OutDir)
	if err := os.MkdirAll(*cmd.OutDir, 0755); err != nil {
		return nil, commandValidationError("render pages", fmt.Errorf("create output dir: %w", err))
	}
	if *cmd.InFile == "-" {
		format, err := render.NormalizeFormat(o.Format)
		if err != nil {
			return nil, commandValidationError("render pages", err)
		}
		digestPage := api.WritePageImageToDisk(c, *cmd.OutDir, "stdin", format, o.JPEGQuality)
		return withStdinReadSeeker(c, cmd.Conf, "render pages", func(rs io.ReadSeeker) ([]string, error) {
			return nil, api.RenderPages(c, rs, cmd.PageSelection, o, digestPage, cmd.Conf)
		})
	}

	return nil, api.RenderPagesFile(c, *cmd.InFile, *cmd.OutDir, cmd.PageSelection, o, cmd.Conf)
}

func solidifyPages(c context.Context, cmd *Command) ([]string, error) {
	if err := contextutil.Check(c); err != nil {
		return nil, err
	}
	if err := validatePageInputOutputCommand(cmd, "solidify"); err != nil {
		return nil, err
	}
	o, err := renderOptions(cmd.Render)
	if err != nil {
		return nil, commandValidationError("solidify", err)
	}
	cmd.Render = o

	reportCommandProgress(cmd, "solidifying %s\n", *cmd.InFile)
	reportCommandOutputPath(cmd)
	if *cmd.InFile != "-" && *cmd.OutFile != "-" {
		return nil, api.SolidifyFile(c, *cmd.InFile, *cmd.OutFile, cmd.PageSelection, o, cmd.Conf)
	}

	rs, w, finalize, err := streamInOutForOperation(c, cmd.Conf, *cmd.InFile, *cmd.OutFile, "solidify")
	if err != nil {
		return nil, err
	}
	return nil, finalize(api.Solidify(c, rs, w, cmd.PageSelection, o, cmd.Conf))
}
