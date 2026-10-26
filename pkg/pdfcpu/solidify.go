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
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"sort"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/log"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/types"
	"github.com/huzpsb/pdfcpu2/pkg/render"
)

// solidifyPage renders pageNr and replaces all its content with the
// resulting image. Text, vector graphics, annotations and group state are
// stripped; the page keeps its geometry.
func solidifyPage(c context.Context, ctx *model.Context, pageNr int, o *render.Options) error {
	if log.DebugEnabled() {
		log.Debug.Printf("solidify page:%d\n", pageNr)
	}

	pageDict, _, inhPAttrs, err := ctx.PageDict(c, pageNr, false)
	if err != nil {
		return err
	}

	res, err := render.Page(c, ctx, pageNr, o)
	if err != nil {
		return err
	}
	for _, warning := range res.Warnings {
		if log.InfoEnabled() {
			log.Info.Printf("solidify page %d: %s\n", pageNr, warning)
		}
	}

	img := res.Image
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()

	format, err := render.NormalizeFormat(o.Format)
	if err != nil {
		return fmt.Errorf("page %d: %w", pageNr, err)
	}

	var imageIndRef *types.IndirectRef
	switch format {
	case "jpg":
		var b bytes.Buffer
		quality := o.JPEGQuality
		if quality <= 0 || quality > 100 {
			quality = 85
		}
		if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: quality}); err != nil {
			return fmt.Errorf("page %d: encode JPEG: %w", pageNr, err)
		}
		sd, err := model.CreateDCTImageStreamDict(ctx.XRefTable, b.Bytes(), w, h, 8, model.DeviceRGBCS)
		if err != nil {
			return fmt.Errorf("page %d: create image stream: %w", pageNr, err)
		}
		if imageIndRef, err = ctx.IndRefForNewObject(*sd); err != nil {
			return fmt.Errorf("page %d: insert image stream: %w", pageNr, err)
		}
	default:
		rgb, smask, err := solidifyImageBuffers(img)
		if err != nil {
			return fmt.Errorf("page %d: collect pixels: %w", pageNr, err)
		}
		sd, err := model.CreateFlateImageStreamDict(ctx.XRefTable, rgb, smask, w, h, 8, model.DeviceRGBCS)
		if err != nil {
			return fmt.Errorf("page %d: create image stream: %w", pageNr, err)
		}
		if imageIndRef, err = ctx.IndRefForNewObject(*sd); err != nil {
			return fmt.Errorf("page %d: insert image stream: %w", pageNr, err)
		}
	}

	// The rendered bitmap already reflects the page rotation, therefore the
	// page box is the render box with origin zero and rotation reset.
	wp, hp := res.WidthPt, res.HeightPt
	rotate := inhPAttrs.Rotate % 360
	if rotate < 0 {
		rotate += 360
	}
	if rotate == 90 || rotate == 270 {
		wp, hp = hp, wp
	}

	var content bytes.Buffer
	fmt.Fprintf(&content, "q %.4f 0 0 %.4f 0 0 cm /Im0 Do Q", wp, hp)
	sd, err := ctx.NewStreamDictForBuf(content.Bytes())
	if err != nil {
		return fmt.Errorf("page %d: create content stream: %w", pageNr, err)
	}
	if err := sd.Encode(); err != nil {
		return fmt.Errorf("page %d: encode content stream: %w", pageNr, err)
	}
	contentsIndRef, err := ctx.IndRefForNewObject(*sd)
	if err != nil {
		return fmt.Errorf("page %d: insert content stream: %w", pageNr, err)
	}

	resDict := types.Dict(
		map[string]types.Object{
			"ProcSet": types.NewNameArray("PDF", "ImageC"),
			"XObject": types.Dict(map[string]types.Object{"Im0": *imageIndRef}),
		},
	)

	pageDict.Update("MediaBox", types.RectForDim(wp, hp).Array())
	pageDict.Update("Contents", *contentsIndRef)
	pageDict.Update("Resources", resDict)
	// Rotation is baked into the image; an inherited rotation must not
	// rotate the replacement image again.
	pageDict.Update("Rotate", types.Integer(0))
	if inhPAttrs.CropBox != nil {
		// Override a possibly inherited crop box covering the full page.
		pageDict.Update("CropBox", types.RectForDim(wp, hp).Array())
	} else {
		pageDict.Delete("CropBox")
	}
	// Content related entries without meaning for an image only page.
	for _, key := range []string{"Annots", "Group", "Thumb", "B", "AA", "Metadata"} {
		pageDict.Delete(key)
	}

	return nil
}

// solidifyImageBuffers converts img into DeviceRGB samples and an optional
// soft mask that is only populated for partially transparent pixels.
func solidifyImageBuffers(img *image.RGBA) (rgb, smask []byte, err error) {
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()
	rgb = make([]byte, 0, w*h*3)
	var sm []byte
	transparent := false
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			r, g, b, a := row[4*x], row[4*x+1], row[4*x+2], row[4*x+3]
			if a == 0 {
				rgb = append(rgb, 0, 0, 0)
			} else {
				// Un-premultiply RGBA stored as straight alpha.
				rgb = append(rgb, byte(int(r)*255/int(a)), byte(int(g)*255/int(a)), byte(int(b)*255/int(a)))
			}
			if a < 255 {
				transparent = true
			}
			if sm != nil || a < 255 {
				if sm == nil {
					// Backfill full opacity for all previous pixels.
					sm = make([]byte, 0, w*h)
					for i := 0; i < y*w+x; i++ {
						sm = append(sm, 255)
					}
				}
				sm = append(sm, a)
			}
		}
	}
	if !transparent {
		return rgb, nil, nil
	}
	return rgb, sm, nil
}

// SolidifyPages renders selected pages and replaces their content with the
// resulting image, effectively freezing the visual appearance, and supports
// cancellation.
func SolidifyPages(c context.Context, ctx *model.Context, selectedPages types.IntSet, o *render.Options) error {
	if err := contextutil.Check(c); err != nil {
		return err
	}
	if err := requireContextWithXRefTable(ctx); err != nil {
		return fmt.Errorf("solidify pages: source context: %w", err)
	}
	if o == nil {
		o = &render.Options{}
	}

	pageNrs := make([]int, 0, len(selectedPages))
	for pageNr, selected := range selectedPages {
		if selected {
			pageNrs = append(pageNrs, pageNr)
		}
	}
	sort.Ints(pageNrs)

	for _, pageNr := range pageNrs {
		if err := c.Err(); err != nil {
			return err
		}
		if err := solidifyPage(c, ctx, pageNr, o); err != nil {
			return fmt.Errorf("page %d: %w", pageNr, err)
		}
	}
	return nil
}
