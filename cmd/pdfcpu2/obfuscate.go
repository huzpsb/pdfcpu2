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

package main

import (
	"context"
	"fmt"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/cli"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/spf13/cobra"
)

func obfuscateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "obfuscate inFile [outFile]",
		Short: "Garbage fill font cmaps and ToUnicode CMaps (anti text extraction)",
		Long:  usageLongObfuscate,
		Args:  cobra.RangeArgs(1, 2),
		RunE: wrapContextHandler(func(c context.Context, conf *model.Configuration, args []string) error {
			return handleObfuscateCommand(c, conf, args)
		}),
	}
	addPasswordFlags(cmd)

	return cmd
}

func handleObfuscateCommand(c context.Context, conf *model.Configuration, args []string) error {
	if err := contextutil.Check(c); err != nil {
		return err
	}

	inFile, outFile, err := optionalOutputPDFArgs(conf, args)
	if err != nil {
		return fmt.Errorf("obfuscate: %w", err)
	}

	return runCommand(c, cli.ObfuscateCommand(inFile, outFile, conf))
}
