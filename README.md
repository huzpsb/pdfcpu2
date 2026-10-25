# pdfcpu (personally maintained fork)

This is a hard fork of [pdfcpu](https://github.com/pdfcpu/pdfcpu).

**For documentation, CLI usage and PDF processing concepts, please refer to the upstream project: https://github.com/pdfcpu/pdfcpu** (docs site: https://pdfcpu.io). This README only covers what is specific to this fork.

## Why this fork exists

My personal workflow needs a bunch of QoL (quality of life) features that pdfcpu does not have — and will not have any time soon — ranging from **plain text extraction** (text-extract, similar to Apache PDFBox's `PDFTextStripper`) to **function-level exports** and beyond.

The upstream maintainer's stance on feature requests is "features are on hold" while polishing existing commands. Nothing wrong with that — I simply need these features urgently and cannot wait, so I forked.

## Relationship with upstream

- This project does **not** promise to track upstream updates — frankly, it most likely won't.
- It is **not abandoned**, though: the features I need will keep being maintained. If you hit a problem, **just open an issue** — what I can fix, I will fix; what is within my needs (or reasonable enough) I may add.
- Existing upstream features, behavior and API conventions stay compatible. New code in this fork follows the upstream layering and style (`model` → `pkg/pdfcpu` → `pkg/api` → `pkg/cli`).

## What this fork adds

### Text extraction

Extract plain text from PDFs. Output behavior is aligned with Apache PDFBox's default extractor (tuned via differential testing against PDFBox over a corpus of 1200+ real-world Chinese PDFs spanning different eras).

CLI:

```
pdfcpu extract -m text inFile outFile|-
pdfcpu extract -m text -p 1-3 inFile -
```

Go API:

```go
// Per-page extraction (paginated results incl. line coordinates)
texts, err := api.ExtractText(ctx, rs, []string{"1-3"}, opts, conf)

// Whole document as one string
s, err := api.ExtractTextString(ctx, rs, nil, nil, conf)

// Write to a file
err = api.ExtractTextFile(ctx, "in.pdf", "out.txt", nil, nil, nil, conf)
```

Supported: ToUnicode CMaps (incl. Type42 and array-form `bfrange`), embedded TrueType cmap fallback, legacy GBK/Big5 encodings, Adobe-GB1/CNS1 character collection CID fallback, simple font Encoding/Differences, Latin-1 last resort (matching PDFBox), Form XObject recursion, overprint deduplication, stream-order line reconstruction (optional sort-by-position).

## Building

```
go mod vendor   # vendor/ is not committed; regenerate before building
go build ./cmd/pdfcpu
go test ./...
```

> Note: `vendor/` is excluded via `.gitignore`. After cloning, run `go mod vendor` before building (offline environments need a populated module cache).

## License

Same as upstream: Apache License 2.0 (see `LICENSE.txt`). The embedded Adobe CID→Unicode tables are derived from Adobe's UCS2 CMap resources (Adobe BSD-style license; the copyright notice is retained in the header of the generated file).
