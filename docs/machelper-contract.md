---
title: kagaz-machelper JSON contract
---

# kagaz-machelper JSON contract

`kagaz-machelper` is the Swift helper binary that gives the Go core access to
two macOS-only frameworks: Vision (OCR) and Apple Foundation Models
(on-device semantic classification, macOS 26+). The Go core never links
against Swift or Objective-C; it execs the helper and reads JSON from stdout,
exactly the way the Swift menu-bar app execs `kagaz` itself. This is the only
contract between the two languages, so it is versioned independently of both.

Every response object carries a top-level `"contract": 1` integer. A Go
caller that receives a contract number it does not understand must reject the
response with a clear error rather than guess at its shape — see
[Compatibility](#compatibility) below.

The helper is discovered by `internal/vaultkit/ocr.HelperPath()`: first
`$KAGAZ_MACHELPER`, then the directory of the running `kagaz` executable,
then `$PATH`, then the standard Homebrew prefixes (`/opt/homebrew/bin`).
`RunHelper` invokes it and returns stdout, wrapping stderr into the error on
failure.

## `kagaz-machelper ocr`

```
kagaz-machelper ocr <path> --langs en-US,hi-IN --json
```

Runs `VNRecognizeTextRequest` (Vision) at the accurate recognition level with
language correction on. Handles both images and PDFs (each PDF page is
rendered and OCR'd separately). `<path>` is a single file.

### Success shape

```json
{
  "contract": 1,
  "engine": "vision",
  "confidence": 0.94,
  "blocks": [
    {
      "text": "Invoice Number: INV-2026-0417",
      "bbox": [0.12, 0.08, 0.63, 0.11],
      "confidence": 0.97,
      "page": 1
    },
    {
      "text": "Total Due: $1,240.00",
      "bbox": [0.12, 0.55, 0.48, 0.58],
      "confidence": 0.91,
      "page": 1
    }
  ]
}
```

- `engine` is always `"vision"` for this subcommand.
- `confidence` is the document-level average of block confidences, provided
  as a convenience; callers that need it recompute it themselves rather than
  trust it blindly.
- `blocks` is one entry per recognized text block, **not required to be in
  reading order** — the caller (`internal/vaultkit/ocr.Vision`) sorts by
  `page`, then by `bbox` top-to-bottom, before joining block text into a
  single `Result.Text`.
- `bbox` is `[x, y, width, height]` in normalized (0–1) image coordinates,
  origin top-left.
- `page` is 1-indexed. `Result.Pages` on the Go side is the highest `page`
  value seen across all blocks.

### Error shape

```json
{
  "contract": 1,
  "error": "unreadable-image",
  "message": "the file at <path> is not a recognizable image or PDF"
}
```

The helper exits non-zero on any error. Known `error` codes: `unreadable-image`,
`unsupported-format`, `io-error`. A Go caller that cannot recognize the
`error` code still has `message` for a human-readable fallback.

## `kagaz-machelper classify`

```
kagaz-machelper classify --backend apple --doctypes "invoice:financial,receipt:financial,passport:identity,..." --json
```

Text to classify is piped on stdin (never a network call, never written to
disk by the helper). `--doctypes` is the compact `name:category,…` spec
produced by `doctypes.Catalog.Spec()` — this is how classifier output is
constrained to the vault's actual catalog rather than an invented category
(Global Constraint 8).

Classification uses Apple Foundation Models' **guided generation**, gated
`@available(macOS 26, *)`, constrained to the supplied doctype list so the
model cannot emit anything outside it. `--backend mlx` is the analogous
contract implemented by the separate `kagaz-machelper-mlx` binary
(`machelper-mlx/`), loading weights via `kagaz model pull`.

### Success shape

```json
{
  "contract": 1,
  "engine": "apple",
  "doctype": "invoice",
  "category": "financial",
  "confidence": 0.88,
  "fields": {
    "invoice_number": "INV-2026-0417",
    "amount": "1240.00"
  }
}
```

- `doctype` and `category` MUST be one of the pairs supplied in
  `--doctypes`; the Go caller re-validates this regardless (never trust a
  subprocess to have honored its own constraint).
- `fields` is optional and may be empty or absent.

### Error / unavailable shape

On macOS 15–25, when the on-device model is not yet downloaded, or when
generation fails for any reason, the helper exits non-zero with:

```json
{
  "contract": 1,
  "error": "unavailable",
  "message": "Apple Foundation Models requires macOS 26 or later"
}
```

Other `error` codes: `model-not-ready`, `generation-failed`,
`invalid-doctypes`. This is not a failure of the pipeline: the Go core
(`internal/vaultkit/classify`) falls back to the `rules` engine whenever the
helper is absent, exits non-zero, or returns a confidence below
`classify.min_confidence`.

### `--probe`

```
kagaz-machelper classify --backend apple --probe --json
```

A fast availability check with no text on stdin, used by
`classify.Available()` and cached for the process lifetime:

```json
{ "contract": 1, "available": true }
```

or

```json
{ "contract": 1, "available": false, "message": "Apple Foundation Models requires macOS 26 or later" }
```

## Compatibility

- The contract number changes only for a breaking shape change (a field
  removed, renamed, or repurposed — never for an additive field). Go callers
  reject a `contract` value they were not built against.
- Adding an optional field, a new `error` code, or a new subcommand is not a
  breaking change and does not bump the contract number.
- Both `kagaz-machelper` (`machelper/`) and `kagaz-machelper-mlx`
  (`machelper-mlx/`) implement the same `classify` contract; a Go caller
  written against one works unmodified against the other, distinguished only
  by which binary was invoked and the `engine` field in the response.
- The helper never performs network I/O. All model weights it uses are
  either bundled with macOS (Vision, Apple Foundation Models) or already
  present on disk in the `kagaz model pull` cache (MLX).
