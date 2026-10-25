// Copyright 2026 The pdfcpu Authors.
// Use of this source code is governed by an Apache-2.0 license that can be found in the LICENSE file.

package ocr

import (
	"math"
	"sort"
	"strings"
	"sync"
)

// Line is one recognized text line. Box holds the 4 quad corners
// (tl, tr, br, bl) as x,y pairs in original image pixel coordinates.
type Line struct {
	Box      [8]float64
	Text     string
	DetScore float64
	RecScore float64
}

// pipeline parameters — PaddleOCR defaults, fixed (no options by design).
const (
	detThresh     = 0.3
	boxThresh     = 0.6
	unclipRatio   = 1.5
	resizeLong    = 960
	recHeight     = 48
	recWidth      = 320
	maxRecW       = 3200
	recBatchMax   = 8
	recBatchWaste = 3 // /10
)

type pipeline struct {
	det, rec *net
}

var (
	pipeOnce sync.Once
	pipe     *pipeline
	pipeErr  error
)

func getPipeline() (*pipeline, error) {
	pipeOnce.Do(func() {
		m, err := loadModel()
		if err != nil {
			pipeErr = err
			return
		}
		pipe = &pipeline{
			det: newNet(m.det, m.weights, m.biases, nil),
			rec: newNet(m.rec, m.weights, m.biases, m.charset),
		}
	})
	return pipe, pipeErr
}

// RecognizeRGB runs the full det+rec pipeline on an RGB8 image.
func RecognizeRGB(rgb []uint8, w, h int) ([]Line, error) {
	p, err := getPipeline()
	if err != nil {
		return nil, err
	}
	lines := p.runDet(rgb, w, h)
	p.runRec(rgb, w, h, lines)
	return lines, nil
}

// ---------------------------------------------------------------- det

func (p *pipeline) runDet(rgb []uint8, W, H int) []Line {
	// official DetResizeForTest: long side -> 960, then 32-alignment
	ratio := float32(resizeLong) / float32(maxInt(W, H))
	rw := maxInt(32, int(float32(W)*ratio))
	rh := maxInt(32, int(float32(H)*ratio))
	rw = (rw + 31) / 32 * 32
	rh = (rh + 31) / 32 * 32
	ratioW := float32(rw) / float32(W)
	ratioH := float32(rh) / float32(H)

	xin, rowS := p.det.prepare(1, rh, rw)
	mean := [3]float32{0.485, 0.456, 0.406}
	stdv := [3]float32{0.229, 0.224, 0.225}
	parallelFor(rh, 8, func(b, e int) {
		var px [3]float32
		for oy := b; oy < e; oy++ {
			o := xin[oy*rowS:]
			for ox := 0; ox < rw; ox++ {
				sampleRGB(rgb, W, H,
					(float32(ox)+0.5)/float32(rw)*float32(W)-0.5,
					(float32(oy)+0.5)/float32(rh)*float32(H)-0.5, &px)
				for c := 0; c < 3; c++ { // BGR, NHWC
					o[ox*3+c] = (px[2-c]/255 - mean[c]) / stdv[c]
				}
			}
		}
	})

	p.det.execute()

	pb := p.det.out()
	pH, pW := pb.h, pb.w
	pd := pb.d // cout==1 prob map, stride 1

	var results []Line
	label := make([]int32, pW*pH)
	var stackv []int
	next := int32(1)
	for sy := 0; sy < pH; sy++ {
		for sx := 0; sx < pW; sx++ {
			s0 := sy*pW + sx
			if label[s0] != 0 || !(pd[s0] > detThresh) {
				continue
			}
			id := next
			next++
			label[s0] = id
			stackv = append(stackv[:0], s0)
			var pts []vec2
			scoreSum := float32(0)
			for len(stackv) > 0 {
				pt := stackv[len(stackv)-1]
				stackv = stackv[:len(stackv)-1]
				py, px2 := pt/pW, pt%pW
				pts = append(pts, vec2{float32(px2), float32(py)})
				scoreSum += pd[pt]
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						ny, nx := py+dy, px2+dx
						if ny < 0 || ny >= pH || nx < 0 || nx >= pW {
							continue
						}
						q := ny*pW + nx
						if label[q] == 0 && pd[q] > detThresh {
							label[q] = id
							stackv = append(stackv, q)
						}
					}
				}
			}
			if len(pts) < 6 {
				continue
			}
			score := scoreSum / float32(len(pts))
			if score < boxThresh {
				continue
			}
			rect := minAreaRect(pts)
			a, per := polyArea(rect[:]), polyPerimeter(rect[:])
			if a < 4 || per < 8 {
				continue
			}
			d := a * unclipRatio / per
			poly := unclipPoly(rect[:], d)
			if len(poly) != 4 {
				continue
			}
			var poly4 [4]vec2
			copy(poly4[:], poly)
			ordered := orderQuad(poly4)
			var line Line
			line.DetScore = float64(score)
			for k := 0; k < 4; k++ {
				x := maxF32(0, minF32(float32(W-1), ordered[k].x/ratioW))
				y := maxF32(0, minF32(float32(H-1), ordered[k].y/ratioH))
				line.Box[2*k] = float64(x)
				line.Box[2*k+1] = float64(y)
			}
			results = append(results, line)
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		ay := (results[i].Box[1] + results[i].Box[3] + results[i].Box[5] + results[i].Box[7]) / 4
		by := (results[j].Box[1] + results[j].Box[3] + results[j].Box[5] + results[j].Box[7]) / 4
		if math.Abs(ay-by) > 8 {
			return ay < by
		}
		return results[i].Box[0] < results[j].Box[0]
	})
	return results
}

// ---------------------------------------------------------------- rec

type recItem struct {
	idx      int
	cw, ch   int
	rW, imgW int
	q        [4]vec2
	ok       bool
	patch    []uint8 // rW x 48 x 3
}

func (p *pipeline) runRec(rgb []uint8, W, H int, results []Line) {
	NL := len(results)
	if NL == 0 {
		return
	}
	items := make([]recItem, NL)
	for i := 0; i < NL; i++ {
		it := &items[i]
		it.idx = i
		var q [4]vec2
		for k := 0; k < 4; k++ {
			q[k] = vec2{float32(results[i].Box[2*k]), float32(results[i].Box[2*k+1])}
		}
		w1 := hypot(q[1].x-q[0].x, q[1].y-q[0].y)
		w2 := hypot(q[2].x-q[3].x, q[2].y-q[3].y)
		h1 := hypot(q[3].x-q[0].x, q[3].y-q[0].y)
		h2 := hypot(q[2].x-q[1].x, q[2].y-q[1].y)
		cw := maxInt(1, int(maxF32(w1, w2)))
		ch := maxInt(1, int(maxF32(h1, h2)))
		whRatio := float32(cw) / float32(ch)
		maxRatio := maxF32(float32(recWidth)/float32(recHeight), whRatio)
		imgW := int(float32(recHeight) * maxRatio)
		if imgW > maxRecW {
			imgW = maxRecW
		}
		rW := int(math.Ceil(float64(recHeight) * float64(whRatio)))
		if rW > imgW {
			rW = imgW
		}
		it.cw, it.ch, it.rW, it.imgW = cw, ch, rW, imgW
		it.q = q

		// stage 1: bicubic warp quad -> (cw, ch)
		dst := [4]vec2{{0, 0}, {float32(cw), 0}, {float32(cw), float32(ch)}, {0, float32(ch)}}
		hm, ok := solveH(dst, q)
		if !ok {
			continue
		}
		crop := make([]uint8, cw*ch*3)
		parallelFor(ch, 1, func(b, e int) {
			var pxs [3]float32
			for y := b; y < e; y++ {
				for x := 0; x < cw; x++ {
					dd := hm[6]*float32(x) + hm[7]*float32(y) + hm[8]
					fx := (hm[0]*float32(x) + hm[1]*float32(y) + hm[2]) / dd
					fy := (hm[3]*float32(x) + hm[4]*float32(y) + hm[5]) / dd
					sampleBicubic(rgb, W, H, fx, fy, &pxs)
					o := crop[(y*cw+x)*3 : (y*cw+x)*3+3]
					for c := 0; c < 3; c++ {
						o[c] = uint8(pxs[c])
					}
				}
			}
		})
		// stage 2: bilinear resize (cw,ch) -> (rW, 48), half-pixel
		it.patch = make([]uint8, rW*recHeight*3)
		parallelFor(recHeight, 1, func(b, e int) {
			var px [3]float32
			for y := b; y < e; y++ {
				for x := 0; x < it.rW; x++ {
					var fx, fy float32
					if it.rW != 1 {
						fx = (float32(x)+0.5)*float32(cw)/float32(it.rW) - 0.5
					}
					if recHeight != 1 {
						fy = (float32(y)+0.5)*float32(ch)/float32(recHeight) - 0.5
					}
					sampleRGB(crop, cw, ch, fx, fy, &px)
					o := it.patch[(y*it.rW+x)*3 : (y*it.rW+x)*3+3]
					for c := 0; c < 3; c++ {
						o[c] = uint8(px[c])
					}
				}
			}
		})
		it.ok = true
	}

	// greedy width-bucketed batching: sort by imgW, pack while waste < 30%
	byW := make([]int, NL)
	for i := range byW {
		byW[i] = i
	}
	sort.SliceStable(byW, func(a, b int) bool {
		return items[byW[a]].imgW < items[byW[b]].imgW
	})
	i := 0
	for i < NL {
		j := i + 1
		wMax := items[byW[i]].imgW
		wSum := wMax
		for j < NL && j-i < recBatchMax {
			wj := items[byW[j]].imgW
			newMax := maxInt(wMax, wj)
			if newMax*(j-i+1) > wSum+wj+(wSum+wj)*recBatchWaste/10 {
				break
			}
			wMax = newMax
			wSum += wj
			j++
		}
		grp := make([]*recItem, j-i)
		for k := i; k < j; k++ {
			grp[k-i] = &items[byW[k]]
		}
		p.runRecBatch(grp, wMax, results)
		i = j
	}
}

func (p *pipeline) runRecBatch(grp []*recItem, imgW int, results []Line) {
	N := len(grp)
	inBuf := &p.rec.bufs[p.rec.inBuf]
	rin0, rowS := p.rec.prepare(N, recHeight, imgW)
	inPad := inBuf.pad
	itemStep := (recHeight + 2*inPad) * rowS
	parallelFor(N*recHeight, 1, func(rb, re int) {
		for r := rb; r < re; r++ {
			n := r / recHeight
			it := grp[n]
			if !it.ok {
				continue
			}
			y := r - n*recHeight
			rin := rin0[n*itemStep:]
			o := rin[y*rowS:]
			patch := it.patch
			for x := 0; x < imgW; x++ {
				oo := o[x*3:]
				if x < it.rW {
					p := patch[(y*it.rW+x)*3:]
					for c := 0; c < 3; c++ { // (x/255-0.5)/0.5, BGR
						oo[c] = (float32(p[2-c])/255 - 0.5) / 0.5
					}
				} else {
					oo[0], oo[1], oo[2] = 0, 0, 0
				}
			}
		}
	})

	p.rec.execute()

	ob := p.rec.out()
	Sg := ob.w // padded timesteps (group width / 8)
	C := ob.c
	dictN := len(p.rec.charset)
	parallelFor(N, 1, func(nb, ne int) {
		for n := nb; n < ne; n++ {
			it := grp[n]
			line := &results[it.idx]
			Si := minInt(it.imgW/8, Sg)
			var sb strings.Builder
			sc := float32(0)
			kept := 0
			prev := -1
			for s := 0; s < Si; s++ {
				row := ob.d[(n*Sg+s)*C:]
				best, bv := 0, row[0]
				for c := 1; c < C; c++ {
					if row[c] > bv {
						bv = row[c]
						best = c
					}
				}
				if best == 0 {
					prev = -1
					continue
				}
				if best == prev {
					continue
				}
				prev = best
				sum := float32(0)
				for c := 0; c < C; c++ {
					sum += tExp(row[c] - bv)
				}
				sc += 1 / sum
				kept++
				if best == dictN+1 {
					sb.WriteByte(' ')
				} else if best <= dictN {
					sb.WriteString(p.rec.charset[best-1])
				}
			}
			line.Text = sb.String()
			if kept > 0 {
				line.RecScore = float64(sc / float32(kept))
			}
		}
	})
}

func hypot(x, y float32) float32 {
	return float32(math.Hypot(float64(x), float64(y)))
}
