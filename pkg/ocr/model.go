// Copyright 2026 The pdfcpu Authors.
// Use of this source code is governed by an Apache-2.0 license that can be found in the LICENSE file.

// Package ocr provides embedded OCR (PP-OCRv6 tiny, det+rec) for scanned PDFs.
// Pure Go: no cgo, no SIMD, no quantization — fp32 scalar math, goroutine parallel.
//
// ppocr.bin is generated from the PP-OCRv6_tiny ONNX models
// (https://paddle-model-ecosphere.bj.bcebos.com, paddle3.0.0,
// PP-OCRv6_tiny_{det,rec}_onnx_infer.tar) by a pattern-matching converter
// that folds the graph into the fused layer program described below. The
// converter lives outside this repository (format spec: this file).
package ocr

import (
	_ "embed"
	"encoding/binary"
	"fmt"
)

//go:embed ppocr.bin
var modelBin []byte

const modelMagic = "pdfcpu2_ppocr"

// kernel types (must match the converter that emits ppocr.bin).
const (
	kConv = iota
	kDW
	kPW
	kConvT
	kUp
	kResize
	kConcat
	kMaxPool
	kAvgPool
	kSE
	kFC
	kAdd
)

// activations.
const (
	actNone = iota
	actRelu
	actGelu
	actHsig
	actSigmoid
	actGated
)

// layer flag bits.
const (
	lfBias = 1 << iota
	lfRes
	lfResScale
	lfInscale
	lfAlpha
	lfSrcs
	lfWoff2
	lfGated
)

const noOff = -1 // sentinel for woff/boff (0xFFFFFFFF in the file)

type layer struct {
	k, act, inb, outb   uint8
	kh, kw, sh, sw, ph  uint8
	pw, r, flags        uint8
	res, se             int8 // -1 = none
	cin, cout           uint16
	woff, boff          int
	woff2, boff2        int
	srcs, ses           []uint8
	alpha, beta         float32
	hasBias             bool
	hasRes, hasResScale bool
	hasInscale          bool
	gated               bool
}

func (ly *layer) ntaps() int {
	if ly.k == kConv {
		return int(ly.kh) * int(ly.kw)
	}
	if ly.k == kConvT {
		return 4
	}
	return 1
}

type bufDef struct{ c, pad int }

type netDef struct {
	bufs   []bufDef
	layers []layer
}

type model struct {
	det, rec *netDef
	weights  []float32
	biases   []float32
	charset  []string // rec: class 1..n
}

type rd struct {
	b []byte
	o int
}

func (r *rd) u8() uint8 {
	v := r.b[r.o]
	r.o++
	return v
}

func (r *rd) u16() uint16 {
	v := binary.LittleEndian.Uint16(r.b[r.o:])
	r.o += 2
	return v
}

func (r *rd) u32() uint32 {
	v := binary.LittleEndian.Uint32(r.b[r.o:])
	r.o += 4
	return v
}

func (r *rd) f32() float32 {
	return float32frombits(r.u32())
}

func off(u uint32) int {
	if u == 0xFFFFFFFF {
		return noOff
	}
	return int(u)
}

func (r *rd) net() (*netDef, error) {
	nb := r.u32()
	nd := &netDef{bufs: make([]bufDef, nb)}
	for i := range nd.bufs {
		nd.bufs[i].c = int(r.u16())
		nd.bufs[i].pad = int(r.u8())
	}
	nl := r.u32()
	nd.layers = make([]layer, nl)
	for i := range nd.layers {
		ly := &nd.layers[i]
		ly.k, ly.act, ly.inb, ly.outb = r.u8(), r.u8(), r.u8(), r.u8()
		ly.kh, ly.kw, ly.sh, ly.sw, ly.ph, ly.pw, ly.r, ly.flags =
			r.u8(), r.u8(), r.u8(), r.u8(), r.u8(), r.u8(), r.u8(), r.u8()
		res, se := int8(r.u8()), int8(r.u8())
		ly.res, ly.se = -1, -1
		if res != -1 { // 0xFF as int8
			ly.res = res
		}
		if se != -1 {
			ly.se = se
		}
		ly.cin, ly.cout = r.u16(), r.u16()
		ly.woff = off(r.u32())
		ly.boff = off(r.u32())
		if ly.flags&lfSrcs != 0 {
			n := int(r.u8())
			ly.srcs = make([]uint8, n)
			ly.ses = make([]uint8, n)
			for j := 0; j < n; j++ {
				ly.srcs[j] = r.u8()
			}
			for j := 0; j < n; j++ {
				ly.ses[j] = r.u8()
			}
		}
		if ly.flags&lfAlpha != 0 {
			ly.alpha, ly.beta = r.f32(), r.f32()
		} else {
			ly.alpha, ly.beta = 0.2, 0.5
		}
		if ly.flags&lfWoff2 != 0 {
			ly.woff2 = off(r.u32())
			ly.boff2 = off(r.u32())
		}
		ly.hasBias = ly.flags&lfBias != 0
		ly.hasRes = ly.flags&lfRes != 0
		ly.hasResScale = ly.flags&lfResScale != 0
		ly.hasInscale = ly.flags&lfInscale != 0
		ly.gated = ly.flags&lfGated != 0
	}
	return nd, nil
}

var loadedModel *model
var modelErr error

func loadModel() (*model, error) {
	if loadedModel != nil || modelErr != nil {
		return loadedModel, modelErr
	}
	m := &model{}
	r := &rd{b: modelBin}
	if len(r.b) < len(modelMagic) || string(r.b[:len(modelMagic)]) != modelMagic {
		modelErr = fmt.Errorf("ocr: bad model magic")
		return nil, modelErr
	}
	r.o = len(modelMagic)
	if m.det, modelErr = r.net(); modelErr != nil {
		return nil, modelErr
	}
	if m.rec, modelErr = r.net(); modelErr != nil {
		return nil, modelErr
	}
	wn := r.u32()
	if r.o+int(wn)*4 > len(r.b) {
		modelErr = fmt.Errorf("ocr: truncated weights")
		return nil, modelErr
	}
	m.weights = readF32(r.b[r.o : r.o+int(wn)*4])
	r.o += int(wn) * 4
	bn := r.u32()
	if r.o+int(bn)*4 > len(r.b) {
		modelErr = fmt.Errorf("ocr: truncated biases")
		return nil, modelErr
	}
	m.biases = readF32(r.b[r.o : r.o+int(bn)*4])
	r.o += int(bn) * 4
	// charset tail ('\n'-joined UTF-8)
	var cs []byte
	if r.o+4 <= len(r.b) {
		n := r.u32()
		cs = r.b[r.o:]
		for i := 0; i < int(n); i++ {
			j := 0
			for j < len(cs) && cs[j] != '\n' {
				j++
			}
			if j > len(cs) {
				j = len(cs)
			}
			m.charset = append(m.charset, string(cs[:j])) // entries may be empty ("" at 616)
			if j < len(cs) {
				cs = cs[j+1:]
			} else {
				cs = nil
			}
		}
	}
	loadedModel = m
	return m, nil
}
