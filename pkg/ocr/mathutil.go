// Copyright 2026 The pdfcpu Authors.
// Use of this source code is governed by an Apache-2.0 license that can be found in the LICENSE file.

package ocr

import "math"

func float32frombits(b uint32) float32 { return math.Float32frombits(b) }

// readF32 reinterprets a little-endian byte slice as []float32 without copy.
func readF32(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(
			uint32(b[i*4]) | uint32(b[i*4+1])<<8 | uint32(b[i*4+2])<<16 | uint32(b[i*4+3])<<24)
	}
	return out
}

// tExp is exp(x) = 2^k * e^r with k = round(x*log2e) and a 5th-order Taylor for
// e^r (|err| < 3e-6) — same polynomial and op order as the C++ engine.
func tExp(x float32) float32 {
	const log2e = 1.44269504088896340736
	const ln2 = 0.69314718055994530942
	k := float32(int32(x*log2e + fsel(x, 0.5, -0.5)))
	r := x - k*ln2
	p := float32(1.0 / 120.0)
	p = p*r + 1.0/24.0
	p = p*r + 1.0/6.0
	p = p*r + 0.5
	p = p*r + 1.0
	p = p*r + 1.0
	e := int32(k)
	if e < -126 {
		return 0
	}
	return p * math.Float32frombits(uint32(e+127)<<23)
}

func fsel(x, a, b float32) float32 {
	if x >= 0 {
		return a
	}
	return b
}

// tErf is erf via Abramowitz-Stegun 7.1.26 (|err| <= 1.5e-7).
func tErf(x float32) float32 {
	neg := x < 0
	if neg {
		x = -x
	}
	t := float32(1.0) / (1.0 + 0.3275911*x)
	y := 1.0 - (((((1.061405429*t-1.453152027)*t)+1.421413741)*t-0.284496736)*t+0.254829592)*
		t*tExp(-x*x)
	if neg {
		return -y
	}
	return y
}

// act1 applies the fused activation (scalar path).
func act1(act uint8, v, alpha, beta float32) float32 {
	switch act {
	case actRelu:
		if v > 0 {
			return v
		}
		return 0
	case actGelu:
		return 0.5 * v * (1 + tErf(v*0.70710678118654752440))
	case actHsig:
		r := alpha*v + beta
		return clamp01(r)
	case actSigmoid:
		return 1 / (1 + tExp(-v))
	case actGated:
		r := alpha*v + beta
		return v * clamp01(r)
	default:
		return v
	}
}

func clamp01(r float32) float32 {
	if r < 0 {
		return 0
	}
	if r > 1 {
		return 1
	}
	return r
}

func minF32(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}

func maxF32(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}
