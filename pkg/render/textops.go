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

package render

// showText renders a text string using the current text state.
func (d *device) showText(bb []byte) {
	ts := &d.gs.ts
	if ts.font == nil || len(bb) == 0 {
		return
	}
	f := ts.font
	codeLen := 1
	if f.isType0 {
		codeLen = f.t0.codeLen
	}
	for i := 0; i+codeLen <= len(bb); {
		var code uint32
		for j := 0; j < codeLen; j++ {
			code = code<<8 | uint32(bb[i+j])
		}
		i += codeLen
		g := f.resolveGlyph(d, code)
		d.drawGlyph(f, g, code == 0x20 && codeLen == 1)
	}
}

// showTextArray renders a TJ array mixing strings and kerning adjustments.
func (d *device) showTextArray(arr []token) {
	ts := &d.gs.ts
	if ts.font == nil {
		return
	}
	for _, t := range arr {
		switch t.kind {
		case tokStr:
			d.showText(t.str)
		case tokNum:
			tx := -t.num / 1000 * ts.size * ts.hScale
			ts.tm = ts.tm.mul(translation(tx, 0))
		}
	}
}

// drawGlyph renders one glyph at the current text position and advances the
// text matrix.
func (d *device) drawGlyph(f *pdfFont, g glyph, applyWordSpacing bool) {
	ts := &d.gs.ts
	fs := ts.size

	// Text rendering matrix: device = base x ctm x tm x rise x fs.
	riseM := mat{1, 0, 0, 1, 0, ts.rise}
	textM := d.base.mul(d.gs.ctm).mul(ts.tm).mul(riseM)

	switch {
	case f.t3 != nil:
		if g.t3Name != "" {
			d.execType3CharProc(f, g.t3Name, textM.mul(scaling(fs, fs)).mul(f.t3.fontMatrix))
		}
	default:
		// Glyph space (font units, y down) -> device.
		gm := mat{ts.hScale, 0, 0, 1, 0, 0}.
			mul(mat{1, 0, 0, -1, 0, 0}).
			mul(scaling(fs/1000, fs/1000))
		m := textM.mul(gm)
		var sps []subPath
		if g.gid != 0 {
			sps = d.glyphOutline(f, g.gid)
		}
		if len(sps) == 0 {
			// Whitespace, control characters and glyphs without an available
			// outline are skipped: painting placeholder boxes renders Chinese
			// documents unreadable.
			sps = nil
		}
		if len(sps) > 0 {
			sps = transformPath(sps, m)
			switch ts.renderMode {
			case 0, 4:
				d.fillPath(sps, d.gs.fillAlpha)
			case 1, 5:
				d.strokeSubPaths(sps, d.gs.strokeAlpha)
			case 2, 6:
				d.fillPath(sps, d.gs.fillAlpha)
				d.strokeSubPaths(sps, d.gs.strokeAlpha)
			default:
				// 3, 7: invisible or clip only.
			}
		}
	}

	// Advance.
	tx := (g.w0/1000*fs + ts.charSpacing) * ts.hScale
	if applyWordSpacing {
		tx += ts.wordSpacing * ts.hScale
	}
	ts.tm = ts.tm.mul(translation(tx, 0))
}

// notdefBox returns a small box used when a glyph outline is unavailable.
func notdefBox(f *pdfFont) []subPath {
	u := f.upem
	if u <= 0 {
		u = 1000
	}
	s := u * 0.6
	sp := subPath{pts: []pt{
		{s * 0.1, s * 0.15}, {s * 0.9, s * 0.15}, {s * 0.9, s * 0.85}, {s * 0.1, s * 0.85}, {s * 0.1, s * 0.15},
	}}
	return []subPath{sp}
}

// execType3CharProc executes a Type3 character procedure.
func (d *device) execType3CharProc(f *pdfFont, name string, m mat) {
	o, ok := f.t3.charProcs[name]
	if !ok {
		return
	}
	sd, _, err := d.ctx.DereferenceStreamDict(o)
	if err != nil || sd == nil {
		return
	}
	if err := sd.DecodeWithLimit(d.decodeLimit()); err != nil {
		return
	}

	// Save state and install the glyph space transform.
	savedGS := d.gs
	savedRes := d.res
	d.stack = append(d.stack, savedGS.clone())
	d.resStack = append(d.resStack, savedRes)
	d.gs = savedGS.clone()
	d.gs.ctm = m
	if f.t3.resources != nil {
		d.res = f.t3.resources
	}

	_ = d.execContent(sd.Content)

	if n := len(d.stack); n > 0 {
		d.gs = d.stack[n-1]
		d.stack = d.stack[:n-1]
	} else {
		d.gs = savedGS
	}
	if rn := len(d.resStack); rn > 0 {
		d.res = d.resStack[rn-1]
		d.resStack = d.resStack[:rn-1]
	} else {
		d.res = savedRes
	}
}
