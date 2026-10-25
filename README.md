# pdfcpu2

**A PDF toolkit built for the AI era.** pdfcpu2 is a modernized fork of [pdfcpu](https://github.com/pdfcpu/pdfcpu) — everything upstream does, plus the two things modern pipelines actually need: **offline OCR** and **PDFBox-grade text extraction**, in one static binary.

> 🚀 **Cross-platform, single-file PDF OCR & text-extraction pipeline — no Python, no cgo, no external services. Go native. Offline. CJK friendly.**

```sh
pdfcpu2 ocr scan.pdf scan_ocr.pdf && pdfcpu2 extract -m text scan_ocr.pdf -
```

That's a complete scans → searchable PDF → plain text pipeline in one executable. Perfect for RAG ingestion, agent tooling and batch document processing.

## Why pdfcpu2

- **Single binary, zero dependencies.** The PP-OCRv6 det+rec models (~6 MB) are embedded in the executable. Runs offline forever — no API keys, no tokens, no rate limits, no data leaving your machine. Windows / Linux / macOS, amd64 / arm64.
- **Go native inference.** The OCR models run on a pure Go engine — fp32 scalar math, goroutine-parallel, no cgo, no ONNX runtime, no Python.
- **Beats GPT-5.5 at OCR.** On PaddlePaddle's published benchmark, PP-OCR models outperform every tested general-purpose VLM — on text *detection* by a factor of ~2:

  | Model | Text detection | Text recognition |
  | --- | --- | --- |
  | PP-OCRv6_medium | **86.2** | **83.2** |
  | PP-OCRv6_small | 84.1 | 81.3 |
  | **PP-OCRv6_tiny** (embedded) | 80.6 | 73.5 |
  | Gemini-3.1-Pro | 46.8 | 71.4 |
  | GPT-5.5 | 45.6 | 64.2 |
  | Qwen3-VL-235B | 38.3 | 74.9 |

  Source: [PP-OCRv6 model card](https://www.modelscope.cn/models/PaddlePaddle/PP-OCRv6_small_rec_onnx) — and unlike the VLMs, it costs nothing per page.
- **PDFBox-grade text extraction.** `extract -m text` is aligned with Apache PDFBox's default extractor, tuned via differential testing against PDFBox over a corpus of **1200+ real-world Chinese PDFs spanning different eras**.
- **CJK friendly.** ToUnicode CMaps (incl. Type42, array-form `bfrange`), embedded TrueType cmaps, legacy GBK/Big5, Adobe-GB1/CNS1 CID fallbacks, Latin-1 last resort — plus PP-OCR models trained on Chinese + English.
- **Idempotent OCR.** Every processed image is marked; re-running `ocr` only touches new content. Text is placed as an invisible layer mapped through each image's page transform — so selection lands exactly on the visible glyphs.
- **Pipeline native.** stdin/stdout streaming (`-`), page selection, JSON output, `context.Context` cancellation in the Go API.

## Fork features

| Feature | Command | Status |
| --- | --- | --- |
| Plain-text extraction (PDFBox-compatible) | `pdfcpu2 extract -m text` | ✅ Available |
| Invisible OCR text layer for scanned PDFs | `pdfcpu2 ocr` | ✅ Available |
| Page rendering / rasterization | `pdfcpu2 render` | 🚧 Planned |

```sh
# Text extraction
pdfcpu2 extract -m text in.pdf out.txt          # whole document
pdfcpu2 extract -m text -p 1-3 in.pdf -         # pages 1-3 to stdout

# OCR — make scans searchable & extractable
pdfcpu2 ocr scan.pdf scan_ocr.pdf               # all pages
pdfcpu2 ocr scan.pdf scan_ocr.pdf -pages 1-10   # selection
```

As Go code:

```go
conf := model.NewDefaultConfiguration()
ctx := context.Background()

n, err := api.OCRFile(ctx, "scan.pdf", "scan_ocr.pdf", nil, conf) // images processed
err = api.ExtractTextFile(ctx, "scan_ocr.pdf", "out.txt", nil, nil, conf)

// or structured, with line coordinates:
texts, err := api.ExtractText(ctx, rs, []string{"1-3"}, nil, conf) // []pdfcpu.PageText
```

## Everything else

The full upstream command set is intact and maintained — validate, optimize, merge/split/trim, pages & boxes, stamps & watermarks, forms, encryption, permissions, signatures, attachments, create, n-up/booklet/poster, and more:

```sh
pdfcpu2 validate in.pdf
pdfcpu2 merge out.pdf a.pdf b.pdf
pdfcpu2 encrypt --opw owner --upw user in.pdf out.pdf   # AES-256
pdfcpu2 stamp add "CONFIDENTIAL" "pos:tr, op:.6" in.pdf out.pdf
```

📖 **[Full documentation — the GitHub wiki](https://github.com/huzpsb/pdfcpu2/wiki)**: ~60 pages covering every command with both CLI usage and Go API samples, plus getting-started guides. For general PDF processing concepts, the [upstream docs](https://pdfcpu.io) are an excellent companion.

## Installation

```sh
go install github.com/huzpsb/pdfcpu2/cmd/pdfcpu2@latest
# or from a checkout:
go mod vendor && go build ./cmd/pdfcpu2
```

As a library:

```sh
go get github.com/huzpsb/pdfcpu2
```

## Relationship with upstream

Hard fork of [pdfcpu](https://github.com/pdfcpu/pdfcpu) (Apache 2.0), maintained independently — not guaranteed to track upstream releases, but actively maintained: issues and reasonable feature requests welcome. Upstream behavior and API conventions stay compatible; new code follows the upstream layering (`model` → `pkg/pdfcpu` → `pkg/api` → `pkg/cli`).

## License

Apache License 2.0, same as upstream (see `LICENSE.txt`). The embedded Adobe CID→Unicode tables are derived from Adobe's UCS2 CMap resources (Adobe BSD-style license; copyright notice retained in the generated file's header).
