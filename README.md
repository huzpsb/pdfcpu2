# pdfcpu2

[中文版](README_CN.md)

pdfcpu2 is a fork of pdfcpu that adds offline OCR and text extraction while retaining the existing PDF processing commands. It builds as a single static binary, with no Python, cgo, or external services required.

```sh
pdfcpu2 ocr scan.pdf scan_ocr.pdf && pdfcpu2 extract -m text scan_ocr.pdf -
```

This adds a searchable text layer to a scanned PDF and extracts the resulting text. The same pipeline can be used for RAG ingestion, agent tools, and batch document processing.

| `pdfcpu2 extract -m text`: extract text from a PDF | `pdfcpu2 ocr`: add an invisible, selectable text layer to scans |
| --- | --- |
| ![Text extraction demo](media/extract.png) | ![OCR text layer](media/ocr.png) |

## Documentation

The [project wiki](https://github.com/huzpsb/pdfcpu2/wiki) contains about 60 pages of documentation, including CLI usage, Go API examples, and getting-started guides.

## Motivation

Horst Rutter, the maintainer of pdfcpu, is a member of the PDF Association. I admire his commitment to the PDF standard. But I genuinely need text extraction, OCR, and related features in my own work. Respect for his work does not remove that need, so I added them here and use them.

## Features

- **Single binary.** The PP-OCRv6 detection and recognition models (about 6 MB) are embedded in the executable. OCR runs offline without API keys, tokens, rate limits, or data leaving the local machine. Windows, Linux, and macOS are supported on amd64 and arm64.
- **Pure Go OCR inference.** The models run on a pure Go engine using fp32 scalar math and goroutine-based parallelism. No cgo, ONNX Runtime, or Python is required.
- **OCR benchmarks.** PaddlePaddle's published benchmark reports the following results for PP-OCRv6 and several general-purpose VLMs:

  ![PP-OCRv6 vs general-purpose VLMs](media/pp.png)

  | Model | Text detection | Text recognition |
  | --- | --- | --- |
  | PP-OCRv6_medium | **86.2** | **83.2** |
  | PP-OCRv6_small | 84.1 | 81.3 |
  | **PP-OCRv6_tiny** (embedded) | 80.6 | 73.5 |
  | Gemini-3.1-Pro | 46.8 | 71.4 |
  | GPT-5.5 | 45.6 | 64.2 |
  | Qwen3-VL-235B | 38.3 | 74.9 |

  Source: [PP-OCRv6 model card](https://www.modelscope.cn/models/PaddlePaddle/PP-OCRv6_small_rec_onnx). The specialized models also have no per-page API cost.
- **PDFBox-compatible text extraction.** `extract -m text` follows Apache PDFBox's default extractor. Behavior has been tuned through differential testing against PDFBox on a corpus of more than 1,200 real-world Chinese PDFs from different periods and generators.
- **Page rendering.** A self-contained pure-Go rasterizer (text including CJK/CID fonts, images, shadings, transparency groups) turns pages into PNG/JPEG images (`render`). `solidify` rebuilds pages as image-only pages to block text copying and content modification. Text drawn with fonts that are not embedded falls back to the bundled vivo Sans.
- **CJK support.** Text extraction handles ToUnicode CMaps (including Type42 and array-form `bfrange`), embedded TrueType cmaps, legacy GBK/Big5, Adobe-GB1/CNS1 CID fallbacks, and a Latin-1 last resort. The OCR models are trained for Chinese and English.
- **Repeatable OCR.** Processed images are marked, so running `ocr` again only processes new content. Text is added as an invisible layer mapped through each image's page transform, keeping selection aligned with the visible glyphs.
- **CLI and API integration.** Supports stdin/stdout streaming (`-`), page selection, JSON output, and `context.Context` cancellation in the Go API.

## Additional commands

| Feature | Command | Status |
| --- | --- | --- |
| Plain-text extraction (PDFBox-compatible) | `pdfcpu2 extract -m text` | Available |
| Invisible OCR text layer for scanned PDFs | `pdfcpu2 ocr` | Available |
| Page rendering / rasterization | `pdfcpu2 render` | Available |
| Content freezing (replace page content with its image) | `pdfcpu2 solidify` | Available |

```sh
# Text extraction
pdfcpu2 extract -m text in.pdf out.txt          # whole document
pdfcpu2 extract -m text -p 1-3 in.pdf -         # pages 1-3 to stdout

# OCR: make scans searchable and extractable
pdfcpu2 ocr scan.pdf scan_ocr.pdf               # all pages
pdfcpu2 ocr scan.pdf scan_ocr.pdf -pages 1-10   # selected pages

# Rendering: one image file per page
pdfcpu2 render in.pdf outDir                    # PNG at 150 dpi
pdfcpu2 render -m jpg -q 90 --dpi 300 in.pdf outDir

# Solidify: rasterize pages and replace their content (anti copy/modify)
pdfcpu2 solidify in.pdf out.pdf                 # lossless PNG images
pdfcpu2 solidify -m jpg --dpi 200 in.pdf out.pdf
```

As Go code:

```go
conf := model.NewDefaultConfiguration()
ctx := context.Background()

n, err := api.OCRFile(ctx, "scan.pdf", "scan_ocr.pdf", nil, conf) // images processed
err = api.ExtractTextFile(ctx, "scan_ocr.pdf", "out.txt", nil, nil, conf)

// Or extract structured text with line coordinates:
texts, err := api.ExtractText(ctx, rs, []string{"1-3"}, nil, conf) // []pdfcpu.PageText

// Render pages to PNG files:
o := &render.Options{DPI: 150, Format: "png"}
err = api.RenderPagesFile(ctx, "in.pdf", "outDir", nil, o, conf)

// Or replace page content with its rendered image:
err = api.SolidifyFile(ctx, "in.pdf", "out.pdf", nil, o, conf)
```

## Existing PDF commands

The existing command set remains available, including validation, optimization, merge/split/trim, pages and boxes, stamps and watermarks, forms, encryption, permissions, signatures, attachments, PDF creation, n-up, booklets, and posters.

```sh
pdfcpu2 validate in.pdf
pdfcpu2 merge out.pdf a.pdf b.pdf
pdfcpu2 encrypt --opw owner --upw user in.pdf out.pdf   # AES-256
pdfcpu2 stamp add "CONFIDENTIAL" "pos:tr, op:.6" in.pdf out.pdf
```

## Installation

```sh
go install github.com/huzpsb/pdfcpu2/cmd/pdfcpu2@latest
# Or from a checkout:
go mod vendor && go build ./cmd/pdfcpu2
```

As a library:

```sh
go get github.com/huzpsb/pdfcpu2
```

## Relationship with upstream

pdfcpu2 is an independently maintained hard fork of pdfcpu, licensed under Apache 2.0. It is not guaranteed to track upstream releases. Issues and reasonable feature requests are welcome.

Existing behavior and API conventions remain compatible where practical. New code follows the existing package layering: `model` → `pkg/pdfcpu` → `pkg/api` → `pkg/cli`.

Upstream project: [pdfcpu](https://github.com/pdfcpu/pdfcpu).

## License

Apache License 2.0, same as upstream (see `LICENSE.txt`).

This software embeds **vivo Sans Light** as the fallback font used for rendering text whose fonts are not embedded in a PDF. vivo Sans is licensed by vivo Mobile Communication Co., Ltd. under the *vivo Sans Font Knowledge Property License Agreement* (see `pkg/render/internal/fallbackfont/vivoSans-LICENSE.txt`): the font must be credited inside this software, may not be modified or redistributed standalone, and the copyright notice and agreement are retained alongside the font. Using pdfcpu2 to render documents does not transfer any vivo Sans rights to the output images beyond what that agreement grants for works created with the font.

The embedded Adobe CID-to-Unicode tables are derived from Adobe's UCS2 CMap resources (Adobe BSD-style license; copyright notice retained in the generated file's header).
