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
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/sign"
)

// TimestampCommand creates a new command to stamp a PDF with an RFC 3161
// document timestamp. An empty tsaURL selects sign.DefaultTSAURL; fieldName
// (StringVals[0]) names the signature field, empty selects an automatic name.
func TimestampCommand(inFile, outFile, tsaURL, fieldName string, conf *model.Configuration) *Command {
	if conf == nil {
		conf = model.NewDefaultConfiguration()
		conf.Cmd = model.TIMESTAMPPDF
	}
	return &Command{
		Mode:       model.TIMESTAMPPDF,
		InFile:     &inFile,
		OutFile:    &outFile,
		StringVal:  tsaURL,
		StringVals: []string{fieldName},
		Conf:       conf,
	}
}

func timestamp(c context.Context, cmd *Command) ([]string, error) {
	if err := contextutil.Check(c); err != nil {
		return nil, err
	}
	if err := validateCryptoCommand(cmd, "timestamp"); err != nil {
		return nil, err
	}
	if cmd.Conf.Offline {
		return nil, fmt.Errorf("timestamp: offline mode forbids the timestamp authority request")
	}

	tsaURL := cmd.StringVal
	if tsaURL == "" {
		tsaURL = sign.DefaultTSAURL
	}
	fieldName := ""
	if len(cmd.StringVals) > 0 {
		fieldName = cmd.StringVals[0]
	}

	reportCommandOutputPath(cmd)

	var info *sign.TSAInfo
	var err error
	if *cmd.InFile != "-" && *cmd.OutFile != "-" {
		info, err = api.TimestampFile(c, *cmd.InFile, *cmd.OutFile, tsaURL, fieldName, cmd.Conf)
	} else {
		var finalize func(error) error
		rs, w, finalize, ferr := streamInOutForOperation(c, cmd.Conf, *cmd.InFile, *cmd.OutFile, "timestamp")
		if ferr != nil {
			return nil, ferr
		}
		info, err = api.Timestamp(c, rs, w, tsaURL, fieldName, cmd.Conf)
		if err != nil {
			return nil, finalize(err)
		}
		if err := finalize(nil); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}

	return []string{fmt.Sprintf("timestamped by %s at %s (serial %s)",
		info.CertSubject, info.GenTime.Format(model.SignTSFormat), info.Serial)}, nil
}
