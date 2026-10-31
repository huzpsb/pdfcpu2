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

package api

import (
	"bytes"
	"context"
	"crypto"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/fault"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/sign"
)

// Timestamp stamps rs with an RFC 3161 document timestamp issued by tsaURL
// and writes the result to w. An empty tsaURL selects sign.DefaultTSAURL.
// fieldName names the signature field (empty selects an automatic name).
// The timestamp proves that the document bytes existed at the TSA asserted
// time; it does not identify a signer. Only the document digest is sent to
// the timestamp authority.
func Timestamp(c context.Context, rs io.ReadSeeker, w io.Writer, tsaURL, fieldName string, conf *model.Configuration) (info *sign.TSAInfo, err error) {
	defer fault.Catch(&err)

	if err := contextutil.Check(c); err != nil {
		return nil, err
	}
	if rs == nil {
		return nil, ErrMissingPDFReadSeeker
	}
	if w == nil {
		return nil, ErrMissingPDFWriter
	}
	conf = operationConfiguration(conf, model.TIMESTAMPPDF)
	if conf.Offline {
		return nil, errors.New("timestamp: offline mode forbids the timestamp authority request")
	}

	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("timestamp: rewind input: %w", err)
	}
	orig, err := io.ReadAll(rs)
	if err != nil {
		return nil, fmt.Errorf("timestamp: read input: %w", err)
	}

	// Read and validate without optimizing: the increment has to cover the
	// input bytes verbatim.
	ctx, err := ReadAndValidate(c, bytes.NewReader(orig), conf)
	if err != nil {
		if errors.Is(err, pdfcpu.ErrWrongPassword) {
			return nil, fmt.Errorf("timestamp: %w", pdfcpu.ErrTimestampEncryptedInput)
		}
		return nil, fmt.Errorf("timestamp: prepare PDF context: %w", err)
	}

	requestToken := func(digest []byte) ([]byte, error) {
		i, err := sign.RequestTimestamp(c, tsaURL, digest, crypto.SHA256)
		if err != nil {
			return nil, err
		}
		info = i
		return i.Token, nil
	}

	out, err := pdfcpu.TimestampPDF(c, orig, ctx, fieldName, requestToken)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, errors.New("timestamp: no token info")
	}
	if err := contextutil.Check(c); err != nil {
		return nil, err
	}
	if _, err := w.Write(out); err != nil {
		return nil, fmt.Errorf("timestamp: write output: %w", err)
	}
	return info, nil
}

// TimestampFile stamps inFile with an RFC 3161 document timestamp issued by
// tsaURL and writes the result to outFile. An empty outFile or one equal to
// inFile replaces inFile. fieldName names the signature field (empty selects
// an automatic name).
func TimestampFile(c context.Context, inFile, outFile, tsaURL, fieldName string, conf *model.Configuration) (info *sign.TSAInfo, err error) {
	if err := contextutil.Check(c); err != nil {
		return nil, err
	}
	if conf == nil {
		return nil, ErrMissingConfiguration
	}
	if inFile == "" {
		return nil, ErrMissingPDFInput
	}

	f1, err := os.Open(inFile)
	if err != nil {
		return nil, fmt.Errorf("timestamp: open input %s: %w", inFile, err)
	}

	tmpFile := ""
	if outFile != "" && inFile != outFile {
		tmpFile = outFile
	}
	staged, err := openStagedOutput(f1, inFile, tmpFile, "timestamp")
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("timestamp: create output: %w", err),
			closeTimestampInput(err, f1),
		)
	}
	f2 := staged.output.file

	ok := false
	defer func() {
		if !ok {
			err = staged.cleanup(err)
			return
		}
		err = staged.commit()
	}()

	info, err = Timestamp(c, f1, f2, tsaURL, fieldName, conf)
	if err != nil {
		return nil, err
	}
	if err := contextutil.Check(c); err != nil {
		return nil, err
	}

	ok = true
	return info, nil
}

func closeTimestampInput(err error, f *os.File) error {
	closeErr := closeFile(f, "timestamp: close input")
	if err != nil && closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if err != nil {
		return err
	}
	return closeErr
}
