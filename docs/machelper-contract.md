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
Source of truth: `machelper/Sources/MacHelper/Contract.swift`,
`Main.swift`.

Every response object carries a top-level `"contract": 1` integer. A Go
caller that receives a contract number it does not understand must reject the
response with a clear error rather than guess at its shape — see
[Compatibility](#compatibility) below.

The helper is discovered by `internal/vaultkit/ocr.HelperPath()`: first
`$KAGAZ_MACHELPER`, then the directory of the running `kagaz` executable,
then `$PATH`, then the standard Homebrew prefixes (`/opt/homebrew/bin`).
`RunHelper` invokes it and returns stdout.

**Both success and failure payloads are written to stdout, never stderr** —
that is deliberate, not a fallback: it lets the Go core decode a single
stream regardless of outcome instead of having to merge two. stderr carries
only the plain-text `--help`/usage banner when no subcommand is recognized;
it is never part of the JSON contract and callers must not parse it.

## `kagaz-machelper ocr`

```
kagaz-machelper ocr <path> [--langs en-US,hi-IN] [--dpi 200] [--max-pages N] [--json]
```

Runs `VNRecognizeTextRequest` (Vision) at the accurate recognition level with
language correction on. Handles both images and PDFs (each PDF page is
rasterised and OCR'd separately). `<path>` is a single file; exactly one
positional argument is accepted.

| Flag | Meaning |
|---|---|
| `--langs` | Comma-separated Vision language hints, e.g. `en-US,hi-IN`. |
| `--dpi` | Rasterisation resolution for PDF pages before OCR. Defaults to 200; the long edge is clamped to 8000px regardless. |
| `--max-pages` | Caps how many PDF pages are rendered and OCR'd. **Defaults to 200** (a belt-and-braces bound on runtime, not on memory — pages are streamed one at a time, so memory use doesn't grow with page count). `--max-pages 0` means no limit. |
| `--json` | Accepted for symmetry/scriptability; output is always a single JSON object regardless. |

### Success shape

```json
{
  "contract": 1,
  "engine": "vision",
  "confidence": 0.94,
  "pages": 3,
  "total_pages": 49,
  "truncated": true,
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
- `confidence` is a **length-weighted mean** of block confidences, not a
  plain average: a long, well-recognized paragraph counts for more than a
  one-word caption recognized at the same confidence. It is provided as a
  convenience; callers that need their own aggregate recompute it from
  `blocks` rather than trust it blindly.
- `blocks` is one entry per recognized text block, **not required to be in
  reading order** — the caller (`internal/vaultkit/ocr.Vision`) sorts by
  `page`, then by `bbox` top-to-bottom, before joining block text into a
  single `Result.Text`.
- `bbox` is `[x, y, width, height]` in normalized (0–1) page coordinates,
  origin **top-left** — Vision's native bottom-left origin is converted by
  the helper before emission, so no caller needs to flip it again.
- `page` is 1-indexed. `Result.Pages` on the Go side is the highest `page`
  value seen across all blocks.
- `pages` is how many pages were actually recognised in this run.
  `total_pages` is how many pages the source document has, regardless of
  whether `--max-pages` capped the run short of it. `truncated` is `true`
  exactly when `--max-pages` cut the run off before `total_pages` was
  reached (`pages < total_pages`).
- **A `truncated: true` response must not be treated as a complete document
  read.** The fact a caller wanted may be on a page that was never looked
  at, and there is no way to distinguish "the document doesn't contain that
  fact" from "the run stopped before reaching the page that does" once
  truncation has happened — a consumer that cares (`kagaz ingest`, in
  particular) needs to either raise `--max-pages`, or treat a truncated
  extraction as lower-confidence than a complete one, not as equivalent to
  it.

### Error shape

```json
{
  "contract": 1,
  "error": "unsupported_format",
  "message": "the file at <path> is not a recognizable image or PDF"
}
```

Error codes the `ocr` subcommand can emit: `file_not_found` (path does not
exist or is not readable), `unsupported_format` (not decodable as an image
or PDF), `render_failed` (a PDF page could not be rasterised),
`ocr_failed` (Vision returned an error while recognizing), `no_text`
(recognition succeeded but produced nothing), `bad_usage` (missing/extra
arguments — exit status 2, everything else below exits 1), and
`internal_error` for anything unforeseen.

## `kagaz-machelper classify`

```
kagaz-machelper classify --backend apple --doctypes "invoice:financial,receipt:financial,passport:identity,..." [--max-chars N] [--json]
```

Text to classify is piped on **stdin** (never a network call, never written
to disk by the helper; empty or non-UTF-8 stdin is a `empty_input` error).
`--doctypes` is the compact `name:category,…` spec produced by
`doctypes.Catalog.Spec()` — this is how classifier output is constrained to
the vault's actual catalog rather than an invented category (Global
Constraint 8). `--max-chars` caps how much of stdin is sent to the model
(defaults to `AppleClassifier.defaultMaxChars`); text beyond the cap is
simply not considered, not an error.

This binary (`machelper/`) implements only `--backend apple`; `--backend
mlx` is a **different binary**, `kagaz-machelper-mlx` (`machelper-mlx/`),
which implements the same contract shape under its own binary name. Passing
`--backend mlx` to `kagaz-machelper` itself is a `unknown_backend` error
that says so explicitly, not a silent redirect.

Classification uses Apple Foundation Models' **guided generation**, gated
`@available(macOS 26, *)`, constrained to the supplied doctype list so the
model cannot emit anything outside it.

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
- `confidence` here is the model's own self-reported certainty (0.0–1.0),
  not a derived statistic — the model is prompted to give an honest low
  score rather than guess when uncertain.
- `fields` is always present but may be `{}` when nothing was extracted —
  it is a plain dictionary in the Swift struct, never an absent key.

### Error / unavailable shape

On macOS 15–25, when Apple Intelligence is off, the device is ineligible,
the model is still downloading, or generation otherwise fails, the helper
exits non-zero with:

```json
{
  "contract": 1,
  "error": "unsupported_os",
  "message": "requires macOS 26 or newer"
}
```

or, once the OS check passes but the model itself is not usable right now:

```json
{
  "contract": 1,
  "error": "backend_unavailable",
  "message": "Apple Foundation Models unavailable: <reason>"
}
```

Error codes the `classify` subcommand can emit: `empty_input` (no text on
stdin), `invalid_doctypes` (`--doctypes` empty or unparseable),
`unknown_backend` (an unimplemented `--backend`), `unsupported_os` (OS
older than macOS 26), `backend_unavailable` (OS is new enough but the model
itself isn't usable right now), `classify_failed` (the model ran but its
answer was unusable — empty, or not in the catalog), `bad_usage`, and
`internal_error`. `kagaz-machelper-mlx` additionally uses `model_not_found`
(the MLX weight directory does not exist — run `kagaz model pull`) and
`model_load_failed` (weights exist but would not load).

None of this is a failure of the Kagaz pipeline: the Go core
(`internal/vaultkit/classify`) falls back to the `rules` engine whenever the
helper is absent, exits non-zero, or returns a confidence below
`classify.min_confidence`.

### Model cache: "present" is not the same check as "ready"

`kagaz-machelper-mlx` reads **no manifest**. It considers a model present
purely by directory inspection: the cache directory for the repo
(`~/Library/Application Support/kagaz/models/<hf-repo>/`) holding a
`config.json` and at least one `.safetensors` file is enough for it to try
loading the model — that's the whole check, on the Swift side.

`kagaz model pull` (`internal/vaultkit/models`), by contrast, writes a real
manifest, `.kagaz-model.json`, in that same directory: the resolved
revision, the full file list, each file's size and SHA256, and a `status`
field that is only ever set to `"ready"` after **every** file in the
manifest has been re-hashed and matched. `Store.Ready` is the Go side's own
notion of complete, and it is stricter than the helper's.

**These two checks can disagree, and the direction they disagree in
matters:** an interrupted pull that already landed `config.json` plus one
weights shard — but not the rest — looks *present* to the Swift helper
(which would attempt to load it and likely fail with `model_load_failed`,
or worse, load a partial/wrong-shaped model) even though Kagaz's own
manifest would say `status: "downloading"`, not `"ready"`. The manifest,
not a directory listing, is the authoritative signal for "this download
actually completed" — anything that needs to know whether a model is
genuinely usable should check `.kagaz-model.json`'s `status`, not just
whether the directory has files in it. The Swift helper's directory check
exists because the helper is deliberately incapable of reading Go-side
state or downloading anything itself (project constraint 2) — it is a
reasonable cheap check for "has anyone ever tried to put a model here",
not a claim of verified completeness.

(`kagaz model pull` has been exercised for real, not just unit-tested: a
run against `mlx-community/Qwen2.5-3B-Instruct-4bit` fetched 9 files
totalling 1.6 GB, verified every one by SHA256, wrote `status: ready`, and
resolved the revision to `4f83f8f146fdf28b512a06562b671d7af4fab457`. That
revision is what the pull resolved `main` to *at pull time*, not a value
baked into the code — `internal/vaultkit/models/pull.go`'s
`pinnedRevisions` map is still empty, so a future pull can resolve a
different revision unless/until pinning is added. The downloader's
correctness itself is no longer a hedge; the revision-pinning gap is real
and separate.)

### `--probe`

```
kagaz-machelper --probe [--backend apple]
```

(also accepted as `kagaz-machelper classify --backend apple --probe` — both
forms reach the same code path.) A fast availability check with no text on
stdin, used by `classify.Available()` and cached for the process lifetime.
Always exits `0` when the probe itself ran; `available` carries the answer:

```json
{ "contract": 1, "engine": "apple", "available": true }
```

(`reason` is `String?` in Swift and the compiler-synthesized `Encodable`
conformance uses `encodeIfPresent` for it, so a `nil` reason is **omitted
from the JSON entirely**, not written as `"reason": null` — a Go caller
should treat a missing `reason` key the same as an empty string, not
attempt to distinguish the two.)

or

```json
{ "contract": 1, "engine": "apple", "available": false, "reason": "requires macOS 26 or newer" }
```

Note the field is **`reason`**, not `message` — this is the one payload in
the contract that differs from the `error`/`message` naming used elsewhere,
because a probe result isn't an error: `available: false` is a normal,
expected answer, not a failure. `internal/vaultkit/classify/helper.go`
decodes `reason` specifically; do not confuse it with the `message` field
on the `ocr`/`classify` error payloads above.

## `--version`

```
kagaz-machelper --version
```

```json
{ "contract": 1, "tool": "kagaz-machelper", "version": "1.0.0" }
```

(`kagaz-machelper-mlx --version` reports `"tool": "kagaz-machelper-mlx"`,
confirmed against `Formula/kagaz-mlx.rb`'s own `test do` block, which
asserts exactly this.)

The field is **`tool`**, not `engine` — `engine` is reserved elsewhere in
this contract for `vision`/`apple`/`mlx`, the backend that produced a
result, which has no meaning for `--version`; `tool` instead names which
binary answered, since both `kagaz-machelper` and `kagaz-machelper-mlx`
implement this flag. Do not confuse the two fields.

`version` is the helper binary's own release version, independent of and
not to be confused with `contract`.

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
