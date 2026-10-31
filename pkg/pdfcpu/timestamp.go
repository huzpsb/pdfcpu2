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

package pdfcpu

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
)

// timestampContentsSize is the number of zero bytes reserved for the
// timestamp token inside /Contents (hex encoded, i.e. double the characters).
// A DigiCert token incl. certificate chain is ~6 KB.
const timestampContentsSize = 8192

// byteRangeWidth is the fixed on-disk width of the patched /ByteRange value
// "[0 0000000000 0000000000 0000000000]" (see sigDictPDFString).
const byteRangeWidth = 36

// ErrTimestampEncryptedInput reports encrypted input that cannot be stamped.
var ErrTimestampEncryptedInput = errors.New("timestamp: encrypted documents are not supported, decrypt first")

// TimestampPDF appends an RFC 3161 document timestamp (DocTimeStamp, SubFilter
// ETSI.RFC3161) to the file that ctx was read from and returns the complete
// timestamped file. fieldName names the signature field (empty selects an
// automatic "TimestampN"); requestToken receives the SHA-256 digest of the
// covered bytes and must return the CMS timestamp token for exactly that
// digest. The covered range spans the whole resulting file except the
// reserved /Contents gap, so any later modification of the stamped document
// invalidates the timestamp.
func TimestampPDF(
	c context.Context,
	orig []byte,
	ctx *model.Context,
	fieldName string,
	requestToken func(digest []byte) ([]byte, error),
) ([]byte, error) {
	if err := contextutil.Check(c); err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, ErrMissingContext
	}
	if ctx.XRefTable == nil {
		return nil, ErrMissingXRefTable
	}
	if ctx.Read == nil || ctx.RootDict == nil {
		return nil, errors.New("timestamp: missing read context")
	}
	if requestToken == nil {
		return nil, errors.New("timestamp: missing token request")
	}
	if ctx.Encrypt != nil {
		return nil, ErrTimestampEncryptedInput
	}

	if err := prepareTimestampIncrement(c, ctx, fieldName); err != nil {
		return nil, err
	}

	// Write the increment continuing at the original file end.
	inc := &bytes.Buffer{}
	ctx.Write.Writer = bufio.NewWriter(inc)
	if err := WriteIncrement(c, ctx); err != nil {
		return nil, fmt.Errorf("timestamp: write increment: %w", err)
	}
	if err := ctx.Write.Flush(); err != nil {
		return nil, fmt.Errorf("timestamp: flush increment: %w", err)
	}

	out := make([]byte, 0, len(orig)+inc.Len())
	out = append(out, orig...)
	out = append(out, inc.Bytes()...)

	// The /Contents hex string sits between '<' and '>', both excluded
	// from the signed range.
	gapStart := ctx.Write.OffsetSigContents + 1 // first hex digit
	gapLen := 2 * timestampContentsSize
	gapEnd := gapStart + int64(gapLen) // at '>'
	byteRange := [4]int64{
		0,
		gapStart - 1,
		gapEnd + 1,
		int64(len(out)) - gapEnd - 1,
	}

	// The ByteRange patch lies inside the covered range, so it has to be
	// applied before the digest is computed.
	if err := patchByteRange(out, byteRange, ctx.Write.OffsetSigByteRange); err != nil {
		return nil, err
	}

	h := sha256.New()
	h.Write(out[:byteRange[1]])
	h.Write(out[byteRange[2]:])

	token, err := requestToken(h.Sum(nil))
	if err != nil {
		return nil, err
	}
	if err := patchContents(out, byteRange, token); err != nil {
		return nil, err
	}
	return out, nil
}

// prepareTimestampIncrement creates the signature field (attached to page 1
// as a hidden widget so every reader lists it), the DocTimeStamp dictionary
// with a reserved zeroed /Contents placeholder and registers every object of
// the upcoming increment.
func prepareTimestampIncrement(c context.Context, ctx *model.Context, fieldName string) error {
	xRefTable := ctx.XRefTable

	sigDict := types.Dict{
		"Type":      types.Name("DocTimeStamp"),
		"Filter":    types.Name("Adobe.PPKLite"),
		"SubFilter": types.Name("ETSI.RFC3161"),
		"ByteRange": types.Array{types.Integer(0), types.Integer(0), types.Integer(0), types.Integer(0)},
		"Contents":  types.NewHexLiteral(make([]byte, timestampContentsSize)),
	}
	sigDictObjNr, err := xRefTable.InsertObject(sigDict)
	if err != nil {
		return fmt.Errorf("timestamp: create signature dict: %w", err)
	}

	pageRef, err := xRefTable.PageDictIndRef(c, 1)
	if err != nil {
		return fmt.Errorf("timestamp: resolve page 1: %w", err)
	}
	if pageRef == nil {
		return errors.New("timestamp: document has no page 1")
	}

	sigField := types.Dict{
		"Type":    types.Name("Annot"),
		"Subtype": types.Name("Widget"),
		"FT":      types.Name("Sig"),
		"T":       types.StringLiteral(timestampFieldName(ctx, fieldName)),
		"Rect":    types.Array{types.Integer(0), types.Integer(0), types.Integer(0), types.Integer(0)},
		"F":       types.Integer(2), // hidden annotation
		"P":       *pageRef,
		"V":       *types.NewIndirectRef(sigDictObjNr, 0),
	}
	sigFieldObjNr, err := xRefTable.InsertObject(sigField)
	if err != nil {
		return fmt.Errorf("timestamp: create signature field: %w", err)
	}
	fieldRef := types.NewIndirectRef(sigFieldObjNr, 0)

	rootObjNr, err := attachTimestampField(ctx, fieldRef)
	if err != nil {
		return err
	}
	pageObjNr, err := attachTimestampWidget(ctx, pageRef, fieldRef)
	if err != nil {
		return err
	}

	// Classic flat increment: the signature dictionary must stay a
	// standalone object at a known offset. Follow the input file's xref
	// style; a hybrid chain (stream original + classic increment) is what
	// readers trip over.
	useXRefStream := ctx.Read.UsingXRefStreams
	ctx.WriteObjectStream = false
	ctx.WriteXRefStream = useXRefStream

	// PDF 1.7 needs the Adobe extension level 3 declaration for document
	// timestamps. Empirically the combination of a classic xref increment
	// with the extension declaration trips older Foxit parsers, so the
	// declaration is only emitted for xref stream based documents, where it
	// has been verified to be safe.
	if useXRefStream && xRefTable.Version() < model.V20 {
		declareAdobeExtensionLevel3(ctx)
	}

	ctx.Write.Increment = true
	ctx.Write.Offset = ctx.Read.FileSize

	ctx.Write.IncrementWithObjNr(sigDictObjNr)
	ctx.Write.IncrementWithObjNr(sigFieldObjNr)
	if rootObjNr > 0 {
		ctx.Write.IncrementWithObjNr(rootObjNr)
	}
	if pageObjNr > 0 {
		ctx.Write.IncrementWithObjNr(pageObjNr)
	}
	return nil
}

// attachTimestampWidget appends the hidden signature widget to page /Annots
// so the field is listed by readers that only walk page annotations. Returns
// the object number that carries the Annots array (0 if the page dict holds
// it directly).
func attachTimestampWidget(ctx *model.Context, pageRef, fieldRef *types.IndirectRef) (int, error) {
	pageObjNr := pageRef.ObjectNumber.Value()
	pageDict, err := ctx.DereferenceDict(*pageRef)
	if err != nil {
		return 0, fmt.Errorf("timestamp: dereference page 1: %w", err)
	}
	if pageDict == nil {
		return 0, errors.New("timestamp: page 1 dict is missing")
	}

	obj, found := pageDict.Find("Annots")
	if !found {
		pageDict["Annots"] = types.Array{*fieldRef}
		return pageObjNr, nil
	}
	switch t := obj.(type) {
	case types.Array:
		pageDict["Annots"] = append(t, *fieldRef)
		return pageObjNr, nil
	case types.IndirectRef:
		annotsObjNr := t.ObjectNumber.Value()
		a, err := ctx.DereferenceArray(t)
		if err != nil {
			return 0, fmt.Errorf("timestamp: dereference page Annots: %w", err)
		}
		entry, ok := ctx.FindTableEntryLight(annotsObjNr)
		if !ok || entry == nil {
			return 0, errors.New("timestamp: page Annots object is missing")
		}
		entry.Object = append(a, *fieldRef)
		ctx.Write.IncrementWithObjNr(annotsObjNr)
		return 0, nil
	default:
		return 0, fmt.Errorf("timestamp: unexpected page Annots type %T", obj)
	}
}

// declareAdobeExtensionLevel3 records the extension that introduces document
// timestamps for PDF 1.7 documents.
func declareAdobeExtensionLevel3(ctx *model.Context) {
	adbe := types.Dict{
		"BaseVersion":   types.Name("1.7"),
		"ExtensionLevel": types.Integer(3),
	}
	if ext, ok := ctx.RootDict["Extensions"].(types.Dict); ok {
		ext["ADBE"] = adbe
		return
	}
	ctx.RootDict["Extensions"] = types.Dict{"ADBE": adbe}
}

// timestampFieldName picks the signature field label: fieldName if given,
// otherwise an automatic "TimestampN" counting existing signatures.
func timestampFieldName(ctx *model.Context, fieldName string) string {
	if fieldName = strings.TrimSpace(fieldName); fieldName != "" {
		if len(fieldName) > maxTimestampFieldNameLen {
			fieldName = fieldName[:maxTimestampFieldNameLen]
		}
		return fieldName
	}
	n := 1
	for _, m := range ctx.Signatures {
		n += len(m)
	}
	return "Timestamp" + strconv.Itoa(n)
}

// maxTimestampFieldNameLen bounds the /T text string.
const maxTimestampFieldNameLen = 120

// attachTimestampField wires fieldRef into the interactive form dictionary
// and returns the object number that carries the form (0 if the root dict
// holds it directly).
func attachTimestampField(ctx *model.Context, fieldRef *types.IndirectRef) (int, error) {
	root := ctx.RootDict

	obj, found := root.Find("AcroForm")
	if !found {
		root["AcroForm"] = types.Dict{
			"Fields":   types.Array{*fieldRef},
			"SigFlags": types.Integer(3), // signatures exist, append only
		}
		return ctx.Root.ObjectNumber.Value(), nil
	}

	// Existing AcroForm: dict may be direct or indirect.
	var (
		d     types.Dict
		objNr int
	)
	switch t := obj.(type) {
	case types.Dict:
		d = t
		objNr = ctx.Root.ObjectNumber.Value()
	case types.IndirectRef:
		d1, err := ctx.DereferenceDict(t)
		if err != nil {
			return 0, fmt.Errorf("timestamp: dereference AcroForm: %w", err)
		}
		if d1 == nil {
			return 0, errors.New("timestamp: AcroForm object is missing")
		}
		d = d1
		objNr = t.ObjectNumber.Value()
	default:
		return 0, fmt.Errorf("timestamp: unexpected AcroForm type %T", obj)
	}

	if err := appendFormFieldRef(ctx, d, fieldRef); err != nil {
		return objNr, err
	}
	if sf, ok := d["SigFlags"].(types.Integer); ok {
		d["SigFlags"] = sf | 3
	} else {
		d["SigFlags"] = types.Integer(3)
	}
	return objNr, nil
}

// appendFormFieldRef appends fieldRef to the AcroForm /Fields array which may
// be direct or an indirect array object; indirect containers are registered
// as increment objects.
func appendFormFieldRef(ctx *model.Context, d types.Dict, fieldRef *types.IndirectRef) error {
	obj, found := d.Find("Fields")
	if !found {
		d["Fields"] = types.Array{*fieldRef}
		return nil
	}

	switch t := obj.(type) {
	case types.Array:
		d["Fields"] = append(t, *fieldRef)
		return nil
	case types.IndirectRef:
		fieldsObjNr := t.ObjectNumber.Value()
		a, err := ctx.DereferenceArray(t)
		if err != nil {
			return fmt.Errorf("timestamp: dereference AcroForm fields: %w", err)
		}
		entry, ok := ctx.FindTableEntryLight(fieldsObjNr)
		if !ok || entry == nil {
			return errors.New("timestamp: AcroForm fields object is missing")
		}
		entry.Object = append(a, *fieldRef)
		ctx.Write.IncrementWithObjNr(fieldsObjNr)
		return nil
	default:
		return fmt.Errorf("timestamp: unexpected AcroForm Fields type %T", obj)
	}
}

// patchByteRange writes the computed values over the /ByteRange placeholder.
func patchByteRange(out []byte, byteRange [4]int64, byteRangeOff int64) error {
	br, err := formatByteRange(byteRange)
	if err != nil {
		return err
	}
	copy(out[byteRangeOff:], br)
	return nil
}

// patchContents writes the token hex, zero padded, into the reserved
// /Contents gap.
func patchContents(out []byte, byteRange [4]int64, token []byte) error {
	tokenHex := []byte(hex.EncodeToString(token))
	if len(tokenHex) > 2*timestampContentsSize {
		return fmt.Errorf(
			"timestamp: token %d bytes exceeds reserved /Contents of %d bytes",
			len(token), timestampContentsSize,
		)
	}

	gap := make([]byte, 2*timestampContentsSize)
	copy(gap, tokenHex)
	for i := len(tokenHex); i < len(gap); i++ {
		gap[i] = '0'
	}
	copy(out[byteRange[1]+1:], gap)
	return nil
}

// formatByteRange renders the fixed width /ByteRange value.
func formatByteRange(byteRange [4]int64) (string, error) {
	for _, v := range byteRange[1:] {
		if v < 0 || v > 9999999999 {
			return "", fmt.Errorf("timestamp: byte range value %d exceeds reserved width", v)
		}
	}
	s := fmt.Sprintf("[0 %010d %010d %010d]", byteRange[1], byteRange[2], byteRange[3])
	if len(s) != byteRangeWidth {
		return "", fmt.Errorf("timestamp: byte range %q has unexpected width", s)
	}
	return s, nil
}
