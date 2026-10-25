// Copyright 2026 The pdfcpu Authors.
// Use of this source code is governed by an Apache-2.0 license that can be found in the LICENSE file.

package ocr

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------- parallelism

var workers = runtime.GOMAXPROCS(0)

// parallelFor splits [0,total) into dynamic chunks processed by the worker
// set. Runs inline for small totals (mirrors the C++ TPool contract).
func parallelFor(total, minChunk int, f func(lo, hi int)) {
	if total <= 0 {
		return
	}
	if minChunk < 1 {
		minChunk = 1
	}
	n := workers
	if total <= minChunk || n <= 1 {
		f(0, total)
		return
	}
	chunk := total / (n * 4)
	if chunk < minChunk {
		chunk = minChunk
	}
	if chunk > 64 {
		chunk = 64
	}
	if chunk < 1 {
		chunk = 1
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				b := int(next.Add(int64(chunk))) - chunk
				if b >= total {
					return
				}
				e := b + chunk
				if e > total {
					e = total
				}
				f(b, e)
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------- buffers

// buffer is a plain-NHWC feature buffer: item n, interior row y, column x,
// channel c at d[itemOff(n) + y*rowStride + x*c + c'] (zero borders of width
// `pad` surround every item when pad > 0).
type buffer struct {
	d                      []float32
	c, pad                 int
	h, w                   int
	cleanH, cleanW, cleanN int
}

func (b *buffer) rowStride() int { return (b.w + 2*b.pad) * b.c }

func (b *buffer) item(n int) []float32 {
	if b.pad == 0 {
		return b.d[n*b.h*b.w*b.c:]
	}
	return b.d[((n*(b.h+2*b.pad)+b.pad)*(b.w+2*b.pad)+b.pad)*b.c:]
}

// physItem returns the physical start of item n INCLUDING its zero border;
// interiorOff() gives the offset of the interior origin inside it. Kernels
// that address taps across the border index from physItem so that the
// (negative) tap offsets stay in range.
func (b *buffer) physItem(n int) []float32 {
	if b.pad == 0 {
		return b.d[n*b.h*b.w*b.c:]
	}
	return b.d[n*(b.h+2*b.pad)*(b.w+2*b.pad)*b.c:]
}

func (b *buffer) interiorOff() int {
	if b.pad == 0 {
		return 0
	}
	return (b.pad*(b.w+2*b.pad) + b.pad) * b.c
}

func (nt *net) ensure(b *buffer, need int) {
	if len(b.d) < need {
		b.d = make([]float32, need)
		b.cleanH, b.cleanW, b.cleanN = -1, -1, -1
	}
}

func (nt *net) zeroBorder(b *buffer) {
	pd := b.pad
	n := nt.batch
	if pd > 0 {
		nt.ensure(b, n*(b.h+2*pd)*(b.w+2*pd)*b.c)
	} else {
		nt.ensure(b, n*b.h*b.w*b.c)
	}
	if pd <= 0 {
		return
	}
	if b.cleanH == b.h && b.cleanW == b.w && b.cleanN == n {
		return
	}
	wtot := b.w + 2*pd
	rowsEach := b.h + 2*pd
	for i := 0; i < n; i++ {
		for r := 0; r < rowsEach; r++ {
			interior := r >= pd && r < pd+b.h
			row := b.d[(i*rowsEach+r)*wtot*b.c:]
			if !interior {
				clear(row[:wtot*b.c])
			} else {
				clear(row[:pd*b.c])
				clear(row[(b.w+pd)*b.c : (b.w+2*pd)*b.c])
			}
		}
	}
	b.cleanH, b.cleanW, b.cleanN = b.h, b.w, n
}

// ---------------------------------------------------------------- net

type net struct {
	bufs    []buffer
	def     *netDef
	weights []float32
	biases  []float32
	charset []string
	batch   int
	inBuf   int
	outBuf  int
}

func newNet(def *netDef, weights, biases []float32, charset []string) *net {
	nt := &net{def: def, weights: weights, biases: biases, charset: charset}
	nt.bufs = make([]buffer, len(def.bufs))
	for i, bd := range def.bufs {
		nt.bufs[i].c = bd.c
		nt.bufs[i].pad = bd.pad
		nt.bufs[i].cleanH = -1
	}
	nt.inBuf = int(def.layers[0].inb)
	nt.outBuf = int(def.layers[len(def.layers)-1].outb)
	return nt
}

// prepare sizes the input buffer for a batch and returns the interior slice
// of item 0 plus the row stride (callers write the raw input, then execute).
func (nt *net) prepare(n, h, w int) ([]float32, int) {
	nt.batch = n
	b := &nt.bufs[nt.inBuf]
	b.h, b.w = h, w
	nt.zeroBorder(b)
	return b.item(0), b.rowStride()
}

func (nt *net) out() *buffer { return &nt.bufs[nt.outBuf] }

func (nt *net) execute() {
	for i := range nt.def.layers {
		nt.runLayer(&nt.def.layers[i])
	}
}

func (nt *net) runLayer(ly *layer) {
	switch ly.k {
	case kPW, kConv, kFC:
		nt.gemm(ly)
	case kDW:
		nt.dw(ly)
	case kConvT:
		nt.convT(ly)
	case kSE:
		nt.se(ly)
	default:
		nt.copyLayer(ly)
	}
}

// ---------------------------------------------------------------- GEMM family: PW / CONV / FC
// weights: [taps][cout][cin], ci contiguous.
func (nt *net) gemm(ly *layer) {
	xb, ob := &nt.bufs[ly.inb], &nt.bufs[ly.outb]
	fc := ly.k == kFC
	sh, sw := int(ly.sh), int(ly.sw)
	ph, pw := int(ly.ph), int(ly.pw)
	pbe, pre := 0, 0
	if fc {
		sh, sw, ph, pw = 1, 1, 0, 0
	} else {
		pbe, pre = int(ly.r)>>4, int(ly.r)&15
	}
	ntaps := ly.ntaps()
	kh, kw := int(ly.kh), int(ly.kw)
	H, W, N := xb.h, xb.w, nt.batch
	Ho, Wo := H, W
	if !fc {
		Ho = (H+ph+pbe-kh)/sh + 1
		Wo = (W+pw+pre-kw)/sw + 1
	}
	ob.h, ob.w = Ho, Wo
	nt.zeroBorder(ob)
	cin, cout := int(ly.cin), int(ly.cout)
	rowS := (W + 2*xb.pad) * cin
	outRowS := ob.rowStride()
	outRows := N * Ho
	var bias []float32
	if ly.hasBias {
		bias = nt.biases[ly.boff:]
	}
	var inse, rsse []float32
	if ly.hasInscale {
		inse = nt.bufs[ly.se].d
	}
	if ly.hasResScale {
		rsse = nt.bufs[ly.se].d
	}
	var resb *buffer
	var resRowS int
	if ly.hasRes {
		resb = &nt.bufs[ly.res]
		resRowS = resb.rowStride()
	}
	act, alpha, beta := ly.act, ly.alpha, ly.beta
	w := nt.weights
	woff := ly.woff
	xd := xb

	parallelFor(outRows*Wo, 8, func(pb, pe int) {
		row := pb / Wo
		x := pb - row*Wo
		n := row / Ho
		y := row - n*Ho
		xr := xd.physItem(n)
		padOff := xd.interiorOff()
		orow := ob.item(n)[y*outRowS:]
		var inseN, rsseN []float32
		if inse != nil {
			inseN = inse[n*cin : n*cin+cin]
		}
		if rsse != nil {
			rsseN = rsse[n*cout : n*cout+cout]
		}
		var rrowBase []float32
		if resb != nil {
			rrowBase = resb.item(n)[y*resRowS:]
		}
		for idx := pb; idx < pe; {
			span := pe - idx
			if span > Wo-x {
				span = Wo - x
			}
			if span > 4 {
				span = 4
			}
			for co := 0; co < cout; co++ {
				var acc [4]float32
				for t := 0; t < ntaps; t++ {
					dy, dx := 0, 0
					if ly.k == kConv {
						dy, dx = t/kw, t%kw
					}
					wr := w[woff+(t*cout+co)*cin : woff+(t*cout+co)*cin+cin]
					base := padOff + (y*sh+dy-ph)*rowS + (x*sw+dx-pw)*cin
					for m := 0; m < span; m++ {
						xp := xr[base+m*sw*cin : base+m*sw*cin+cin]
						s := float32(0)
						if inseN != nil {
							for ci := 0; ci < cin; ci++ {
								s += xp[ci] * inseN[ci] * wr[ci]
							}
						} else {
							for ci := 0; ci < cin; ci++ {
								s += xp[ci] * wr[ci]
							}
						}
						acc[m] += s
					}
				}
				var bv float32
				if bias != nil {
					bv = bias[co]
				}
				for m := 0; m < span; m++ {
					v := acc[m] + bv
					v = act1(act, v, alpha, beta)
					if rrowBase != nil {
						rv := rrowBase[(x+m)*cout+co]
						if rsseN != nil {
							rv *= rsseN[co]
						}
						v += rv
					}
					orow[(x+m)*cout+co] = v
				}
			}
			idx += span
			x += span
			if x == Wo {
				x = 0
				row++
				if row < outRows {
					n = row / Ho
					y = row - n*Ho
					xr = xd.physItem(n)
					orow = ob.item(n)[y*outRowS:]
					if inse != nil {
						inseN = inse[n*cin : n*cin+cin]
					}
					if rsse != nil {
						rsseN = rsse[n*cout : n*cout+cout]
					}
					if resb != nil {
						rrowBase = resb.item(n)[y*resRowS:]
					}
				}
			}
		}
	})
}

// ---------------------------------------------------------------- depthwise conv
// weights: [co][taps].
func (nt *net) dw(ly *layer) {
	xb, ob := &nt.bufs[ly.inb], &nt.bufs[ly.outb]
	H, W, N := xb.h, xb.w, nt.batch
	pbe, pre := int(ly.r)>>4, int(ly.r)&15
	kh, kw := int(ly.kh), int(ly.kw)
	sh, sw := int(ly.sh), int(ly.sw)
	ph, pw := int(ly.ph), int(ly.pw)
	Ho := (H+int(ly.ph)+pbe-kh)/sh + 1
	Wo := (W+int(ly.pw)+pre-kw)/sw + 1
	ob.h, ob.w = Ho, Wo
	nt.zeroBorder(ob)
	C := int(ly.cout)
	kk := kh * kw
	rowS := (W + 2*xb.pad) * C
	outRowS := ob.rowStride()
	outRows := N * Ho
	var bias []float32
	if ly.hasBias {
		bias = nt.biases[ly.boff:]
	}
	var inse, rsse []float32
	if ly.hasInscale {
		inse = nt.bufs[ly.se].d
	}
	if ly.hasResScale {
		rsse = nt.bufs[ly.se].d
	}
	var resb *buffer
	var resRowS int
	if ly.hasRes {
		resb = &nt.bufs[ly.res]
		resRowS = resb.rowStride()
	}
	act, alpha, beta := ly.act, ly.alpha, ly.beta
	w := nt.weights[ly.woff:]
	xd := xb

	parallelFor(outRows*Wo, 8, func(pb, pe int) {
		row := pb / Wo
		x := pb - row*Wo
		n := row / Ho
		y := row - n*Ho
		xr := xd.physItem(n)
		padOff := xd.interiorOff()
		orow := ob.item(n)[y*outRowS:]
		var inseN, rsseN []float32
		if inse != nil {
			inseN = inse[n*C : n*C+C]
		}
		if rsse != nil {
			rsseN = rsse[n*C : n*C+C]
		}
		var rrowBase []float32
		if resb != nil {
			rrowBase = resb.item(n)[y*resRowS:]
		}
		for idx := pb; idx < pe; {
			span := 1
			if pe-idx >= 2 && Wo-x >= 2 {
				span = 2
			}
			for co := 0; co < C; co++ {
				var acc [2]float32
				var sc float32 = 1
				if inseN != nil {
					sc = inseN[co]
				}
				for t := 0; t < kk; t++ {
					dy, dx := t/kw, t%kw
					wv := w[co*kk+t] * sc
					base := padOff + (y*sh+dy-ph)*rowS + (x*sw+dx-pw)*C + co
					for m := 0; m < span; m++ {
						acc[m] += xr[base+m*sw*C] * wv
					}
				}
				var bv float32
				if bias != nil {
					bv = bias[co]
				}
				for m := 0; m < span; m++ {
					v := acc[m] + bv
					v = act1(act, v, alpha, beta)
					if rrowBase != nil {
						rv := rrowBase[(x+m)*C+co]
						if rsseN != nil {
							rv *= rsseN[co]
						}
						v += rv
					}
					orow[(x+m)*C+co] = v
				}
			}
			idx += span
			x += span
			if x == Wo {
				x = 0
				row++
				if row < outRows {
					n = row / Ho
					y = row - n*Ho
					xr = xd.physItem(n)
					orow = ob.item(n)[y*outRowS:]
					if inse != nil {
						inseN = inse[n*C : n*C+C]
					}
					if rsse != nil {
						rsseN = rsse[n*C : n*C+C]
					}
					if resb != nil {
						rrowBase = resb.item(n)[y*resRowS:]
					}
				}
			}
		}
	})
}

// ---------------------------------------------------------------- convT 2x2 s2
// out(2y+dy, 2x+dx)[co] = sum_ci w[t][co][ci] * in(y,x)[ci], t = dy*2+dx.
func (nt *net) convT(ly *layer) {
	xb, ob := &nt.bufs[ly.inb], &nt.bufs[ly.outb]
	H, W, N := xb.h, xb.w, nt.batch
	Ho, Wo := 2*H, 2*W
	ob.h, ob.w = Ho, Wo
	nt.ensure(ob, N*Ho*Wo*int(ly.cout)) // 2x2 s2: every output written exactly once
	cin, cout := int(ly.cin), int(ly.cout)
	rowS := xb.rowStride()
	outRowS := ob.rowStride()
	var bias []float32
	if ly.hasBias {
		bias = nt.biases[ly.boff:]
	}
	act, alpha, beta := ly.act, ly.alpha, ly.beta
	w := nt.weights
	woff := ly.woff
	xd := xb

	parallelFor(N*H*W, 8, func(pb, pe int) {
		row := pb / W
		x := pb - row*W
		n := row / H
		y := row - n*H
		xr := xd.item(n)[y*rowS:]
		for idx := pb; idx < pe; idx++ {
			xp := xr[x*cin : x*cin+cin]
			for t := 0; t < 4; t++ {
				dy, dx := t/2, t%2
				orow := ob.item(n)[(2*y+dy)*outRowS:]
				oo := (2*x + dx) * cout
				for co := 0; co < cout; co++ {
					wr := w[woff+(t*cout+co)*cin : woff+(t*cout+co)*cin+cin]
					s := float32(0)
					for ci := 0; ci < cin; ci++ {
						s += xp[ci] * wr[ci]
					}
					if bias != nil {
						s += bias[co]
					}
					orow[oo+co] = act1(act, s, alpha, beta)
				}
			}
			x++
			if x == W {
				x = 0
				row++
				if row < N*H {
					n = row / H
					y = row - n*H
					xr = xd.item(n)[y*rowS:]
				}
			}
		}
	})
}

// ---------------------------------------------------------------- SE (GAP + pw + relu + pw + hsig [+1])
func (nt *net) se(ly *layer) {
	xb := &nt.bufs[ly.inb]
	H, W, N := xb.h, xb.w, nt.batch
	C := int(ly.cin)
	r := int(ly.r)
	inPad := xb.pad
	rowS := (W + 2*inPad) * C
	ob := &nt.bufs[ly.outb]
	nt.ensure(ob, N*C)
	out := ob.d
	w1 := nt.weights[ly.woff:]
	w2 := nt.weights[ly.woff2:]
	var b1, b2 []float32
	if ly.boff != noOff {
		b1 = nt.biases[ly.boff:]
	}
	if ly.boff2 != noOff {
		b2 = nt.biases[ly.boff2:]
	}
	alpha, beta := ly.alpha, ly.beta
	mean := make([]float32, C)
	h := make([]float32, r)
	for n := 0; n < N; n++ {
		clear(mean)
		xr := xb.item(n)
		for y := 0; y < H; y++ {
			rowp := xr[y*rowS:]
			for x := 0; x < W; x++ {
				p := rowp[x*C : x*C+C]
				for c := 0; c < C; c++ {
					mean[c] += p[c]
				}
			}
		}
		inv := float32(1) / float32(H*W)
		for c := 0; c < C; c++ {
			mean[c] *= inv
		}
		for ri := 0; ri < r; ri++ {
			a := float32(0)
			if b1 != nil {
				a = b1[ri]
			}
			wrow := w1[ri*C : ri*C+C]
			for ci := 0; ci < C; ci++ {
				a += mean[ci] * wrow[ci]
			}
			if a > 0 {
				h[ri] = a
			} else {
				h[ri] = 0
			}
		}
		o := out[n*C : n*C+C]
		for c := 0; c < C; c++ {
			a := float32(0)
			if b2 != nil {
				a = b2[c]
			}
			wrow := w2[c*r : c*r+r]
			for ri := 0; ri < r; ri++ {
				a += h[ri] * wrow[ri]
			}
			s := alpha*a + beta
			o[c] = clamp01(s)
			if ly.gated {
				o[c] = 1 + o[c]
			}
		}
	}
	ob.h, ob.w = 1, 1
}

// ---------------------------------------------------------------- resize / concat / pools / add
func (nt *net) copyLayer(ly *layer) {
	switch ly.k {
	case kResize:
		xb, ob := &nt.bufs[ly.inb], &nt.bufs[ly.outb]
		s := int(ly.kh)
		H, W, N := xb.h, xb.w, nt.batch
		Ho, Wo := H*s, W*s
		ob.h, ob.w = Ho, Wo
		nt.zeroBorder(ob)
		C := ob.c
		rowS := xb.rowStride()
		oRowS := ob.rowStride()
		var inse []float32
		if ly.hasInscale {
			inse = nt.bufs[ly.se].d
		}
		xd := xb
		parallelFor(N*Ho, 2, func(rb, re int) {
			for row := rb; row < re; row++ {
				n := row / Ho
				y := row - n*Ho
				src := xd.item(n)[(y/s)*rowS:]
				dst := ob.item(n)[y*oRowS:]
				var inseN []float32
				if inse != nil {
					inseN = inse[n*C : n*C+C]
				}
				if inseN != nil {
					for x := 0; x < Wo; x++ {
						sp := src[(x/s)*C : (x/s)*C+C]
						dp := dst[x*C : x*C+C]
						for ch := 0; ch < C; ch++ {
							dp[ch] = sp[ch] * inseN[ch]
						}
					}
				} else {
					for x := 0; x < Wo; x++ {
						copy(dst[x*C:x*C+C], src[(x/s)*C:(x/s)*C+C])
					}
				}
			}
		})
	case kConcat:
		ob := &nt.bufs[ly.outb]
		n := len(ly.srcs)
		H, W := 0, 0
		for i := 0; i < n; i++ {
			H = nt.bufs[ly.srcs[i]].h
			W = nt.bufs[ly.srcs[i]].w
		}
		ob.h, ob.w = H, W
		nt.zeroBorder(ob)
		off := make([]int, n)
		cOff := 0
		srcs := make([]*buffer, n)
		for i := 0; i < n; i++ {
			off[i] = cOff
			srcs[i] = &nt.bufs[ly.srcs[i]]
			cOff += srcs[i].c
		}
		C := ob.c
		Nb := nt.batch
		oRowS := ob.rowStride()
		parallelFor(Nb*H, 2, func(rb, re int) {
			for row := rb; row < re; row++ {
				bn := row / H
				y := row - bn*H
				dst := ob.item(bn)[y*oRowS:]
				for i := 0; i < n; i++ {
					sb := srcs[i]
					src := sb.item(bn)[y*sb.rowStride():]
					var scN []float32
					if ly.ses[i] != 0xFF {
						scN = nt.bufs[ly.ses[i]].d[bn*sb.c : bn*sb.c+sb.c]
					}
					if scN != nil {
						for x := 0; x < W; x++ {
							sp := src[x*sb.c : x*sb.c+sb.c]
							dp := dst[x*C+off[i] : x*C+off[i]+sb.c]
							for c := 0; c < sb.c; c++ {
								dp[c] = sp[c] * scN[c]
							}
						}
					} else {
						for x := 0; x < W; x++ {
							copy(dst[x*C+off[i]:x*C+off[i]+sb.c], src[x*sb.c:x*sb.c+sb.c])
						}
					}
				}
			}
		})
	case kMaxPool:
		xb, ob := &nt.bufs[ly.inb], &nt.bufs[ly.outb]
		H, W, N := xb.h, xb.w, nt.batch
		pbe, pre := int(ly.r)>>4, int(ly.r)&15
		kh, kw := int(ly.kh), int(ly.kw)
		sh, sw := int(ly.sh), int(ly.sw)
		ph, pw := int(ly.ph), int(ly.pw)
		Ho := (H+ph+pbe-kh)/sh + 1
		Wo := (W+pw+pre-kw)/sw + 1
		ob.h, ob.w = Ho, Wo
		nt.zeroBorder(ob)
		C := ob.c
		rowS := xb.rowStride()
		oRowS := ob.rowStride()
		xd := xb
		parallelFor(N*Ho, 2, func(rb, re int) {
			for row := rb; row < re; row++ {
				bn := row / Ho
				y := row - bn*Ho
				xbase := xd.item(bn)
				dst := ob.item(bn)[y*oRowS:]
				for x := 0; x < Wo; x++ {
					o := dst[x*C : x*C+C]
					for c := 0; c < C; c++ {
						o[c] = -1e30
					}
					for dy := 0; dy < kh; dy++ {
						iy := y*sh + dy - ph
						if iy < 0 || iy >= H {
							continue
						}
						for dx := 0; dx < kw; dx++ {
							ix := x*sw + dx - pw
							if ix < 0 || ix >= W {
								continue
							}
							p := xbase[iy*rowS+ix*C : iy*rowS+ix*C+C]
							for c := 0; c < C; c++ {
								if p[c] > o[c] {
									o[c] = p[c]
								}
							}
						}
					}
				}
			}
		})
	case kAvgPool:
		xb, ob := &nt.bufs[ly.inb], &nt.bufs[ly.outb]
		H, W, N := xb.h, xb.w, nt.batch
		kh, kw := int(ly.kh), int(ly.kw)
		sh, sw := int(ly.sh), int(ly.sw)
		Ho := (H-kh)/sh + 1
		Wo := (W-kw)/sw + 1
		ob.h, ob.w = Ho, Wo
		nt.zeroBorder(ob)
		C := ob.c
		rowS := xb.rowStride()
		oRowS := ob.rowStride()
		inv := float32(1) / float32(kh*kw)
		xd := xb
		parallelFor(N*Ho, 2, func(rb, re int) {
			for row := rb; row < re; row++ {
				bn := row / Ho
				y := row - bn*Ho
				xbase := xd.item(bn)
				dst := ob.item(bn)[y*oRowS:]
				for x := 0; x < Wo; x++ {
					o := dst[x*C : x*C+C]
					for c := 0; c < C; c++ {
						o[c] = 0
					}
					for dy := 0; dy < kh; dy++ {
						rp := xbase[(y*sh+dy)*rowS+(x*sw)*C : (y*sh+dy)*rowS+(x*sw)*C+C]
						for dx := 0; dx < kw; dx++ {
							p := rp[dx*C : dx*C+C]
							for c := 0; c < C; c++ {
								o[c] += p[c]
							}
						}
					}
					for c := 0; c < C; c++ {
						o[c] *= inv
					}
				}
			}
		})
	case kAdd:
		xb, ob := &nt.bufs[ly.inb], &nt.bufs[ly.outb]
		H, W, N := xb.h, xb.w, nt.batch
		ob.h, ob.w = H, W
		nt.zeroBorder(ob)
		C := ob.c
		rowS := xb.rowStride()
		rrowS := nt.bufs[ly.res].rowStride()
		oRowS := ob.rowStride()
		var inse, rsse []float32
		if ly.hasInscale {
			inse = nt.bufs[ly.se].d
		}
		if ly.hasResScale {
			rsse = nt.bufs[ly.se].d
		}
		act, alpha, beta := ly.act, ly.alpha, ly.beta
		xd := xb
		rd := &nt.bufs[ly.res]
		parallelFor(N*H, 2, func(rb, re int) {
			for row := rb; row < re; row++ {
				bn := row / H
				y := row - bn*H
				a := xd.item(bn)[y*rowS:]
				b := rd.item(bn)[y*rrowS:]
				o := ob.item(bn)[y*oRowS:]
				var inseN, rsseN []float32
				if inse != nil {
					inseN = inse[bn*C : bn*C+C]
				}
				if rsse != nil {
					rsseN = rsse[bn*C : bn*C+C]
				}
				for x := 0; x < W; x++ {
					pa := a[x*C : x*C+C]
					pb := b[x*C : x*C+C]
					po := o[x*C : x*C+C]
					for c := 0; c < C; c++ {
						u := pa[c]
						if inseN != nil {
							u *= inseN[c]
						}
						v := pb[c]
						if rsseN != nil {
							v *= rsseN[c]
						}
						po[c] = act1(act, u+v, alpha, beta)
					}
				}
			}
		})
	}
}
