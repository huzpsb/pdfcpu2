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
	"github.com/huzpsb/pdfcpu2/pkg/api"
	"github.com/huzpsb/pdfcpu2/pkg/cli"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/sign"
	"github.com/spf13/cobra"
)

var (
	tsaURL   string
	tsaField string
)

func timestampCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "timestamp inFile [outFile]",
		Short: "Add an RFC 3161 document timestamp",
		Long:  usageLongTimestamp,
		Args:  cobra.RangeArgs(1, 2),
		RunE:  wrapContextHandler(handleTimestampCommand),
	}
	cmd.Flags().StringVar(&tsaURL, "tsa", "", "timestamp authority URL (default: "+sign.DefaultTSAURL+")")
	cmd.Flags().StringVar(&tsaField, "field", "", "signature field name shown in the reader's signature panel (default: TimestampN)")

	return cmd
}

func handleTimestampCommand(c context.Context, conf *model.Configuration, args []string) error {
	if err := contextutil.Check(c); err != nil {
		return err
	}
	if conf == nil {
		return api.ErrMissingConfiguration
	}
	if err := validateCryptoCommandArgs("timestamp", args); err != nil {
		return err
	}
	if conf.Offline {
		return fmt.Errorf("timestamp: offline mode forbids the timestamp authority request")
	}

	inFile, outFile, err := inputOutputPDFArgs(conf, args)
	if err != nil {
		return fmt.Errorf("timestamp: parse arguments: %w", err)
	}

	return runCommand(c, cli.TimestampCommand(inFile, outFile, tsaURL, tsaField, conf))
}
