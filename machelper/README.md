# kagaz-machelper

The macOS leaf utility behind Kagaz's OCR and classification tiers.

It holds **zero vault logic**. It reads one file (or one stdin buffer), asks an Apple
framework a question, prints a single JSON object on stdout and exits. All mutation stays
in the Go core (`internal/vaultkit`). Nothing here touches the network.

## Building

```sh
swift build -c release
```

The binary lands at `.build/release/kagaz-machelper`.

**Toolchain.** This package has **no external package dependencies** — argument parsing is
hand-rolled in `Arguments.swift` — so the default Homebrew formula can build it offline.

It builds with **either** the Command Line Tools toolchain **or** full Xcode. That includes
the `classify --backend apple` path: guided generation is built at run time with
`DynamicGenerationSchema`, so the `@Generable` macro plugin (which ships only with full
Xcode, not with CommandLineTools) is never needed. See "Why not `@Generable`" below.

If your `xcode-select -p` points at CommandLineTools but you want to build with Xcode
anyway, prefix the command rather than switching the global toolchain:

```sh
DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer swift build -c release
```

**Platform.** macOS 15 (Sequoia) is the deployment floor, Apple silicon. The
`classify --backend apple` tier additionally requires **macOS 26** at run time and is gated
`@available(macOS 26, *)`; on macOS 15–25 the same binary returns a structured
`unsupported_os` error so the Go core falls back to rules-based classification.

## Commands

```sh
kagaz-machelper ocr <path> [--langs en-US,hi-IN] [--dpi 200] [--max-pages 200] [--json]
kagaz-machelper classify --backend apple --doctypes "invoice:financial,..." [--max-chars N] [--json]
kagaz-machelper --probe [--backend apple]
kagaz-machelper --version
```

`--json` is accepted for symmetry with the Go CLI; output is always JSON regardless.
`classify` reads the document text from **stdin**.

Unknown options are a **hard error** (`bad_usage`, exit 2), including an option that belongs
to a different subcommand. Silently swallowing `--model foo` as a flag plus a stray
positional is how a caller ends up believing it selected something that was never read.
Note that `kagaz-machelper` has no `--model`: Foundation Models exposes exactly one system
model. Model selection belongs to `kagaz-machelper-mlx`.

### `ocr`

Vision's `VNRecognizeTextRequest` at `.accurate` with language correction on. Images are
handed to Vision directly; PDFs are rasterised page by page with Core Graphics first
(PDFKit is deliberately avoided so the helper stays headless), honouring the page's
`/Rotate` and cropbox.

```json
{ "contract": 1, "engine": "vision", "confidence": 0.94,
  "pages": 3, "total_pages": 49, "truncated": true,
  "blocks": [ { "text": "...", "bbox": [0.045, 0.068, 0.412, 0.056], "confidence": 0.97, "page": 1 } ] }
```

- `bbox` is `[x, y, w, h]` **normalised to 0…1** with the origin at the **top-left** of the
  page. Vision reports bottom-left; the flip happens in `VisionOCR.topLeftBox` and nowhere
  else.
- `page` is 1-based.
- `confidence` at the top level is the length-weighted mean of the block confidences, so a
  well-read paragraph is not dragged down by a doubtful two-character stamp.
- `--dpi` controls PDF rasterisation only (default 200). The long edge is clamped to 8000px.
- `pages` is how many pages were recognised, `total_pages` how many the document has, and
  `truncated` is true when `--max-pages` capped the run. **A capped run must not be treated
  as a complete read** — the text the caller wanted may be on a page that was never looked
  at, and a silent ceiling is indistinguishable from a document that simply lacks the fact.

### Memory

Pages are **streamed**: render one, recognise it, drop the bitmap, move on. A rasterised
page costs roughly 18 MB at 200 dpi, so holding a whole document would be linear in page
count — the batch version of this code peaked at **882 MB RSS** on a 49-page PDF and would
have reached several gigabytes on a book-length scan. Streaming holds one page at a time:
the same document now peaks at **165 MB** with byte-identical output.

`--max-pages` defaults to **200** as a belt-and-braces bound on runtime rather than memory;
`--max-pages 0` means no limit.

### `classify`

Apple Foundation Models, on-device, with guided generation constrained to the doctype
catalog the Go core passes down:

```sh
echo "$text" | kagaz-machelper classify --backend apple \
  --doctypes "invoice:financial,passport:identity,lease:property" --json
```

```json
{ "contract": 1, "engine": "apple", "doctype": "invoice", "category": "financial",
  "confidence": 0.92, "fields": { "amount": "4800" } }
```

The `--doctypes` value is `name:category,...`. The model only ever chooses a **name**; the
category is looked up from that map rather than generated. Combined with the `anyOf` schema
constraint and a second check against the catalog before emission, an invented category is
impossible (project constraint 8).

`--max-chars` (default 2000) bounds the excerpt sent to the model.

### `--probe`

The fast availability check the Go core calls before every classify to decide which backend
to use. It queries `SystemLanguageModel.default.availability` only — no model load, no
warm-up — and always exits 0 when the probe itself ran:

```json
{ "contract": 1, "engine": "apple", "available": true }
{ "contract": 1, "engine": "apple", "available": false, "reason": "apple_intelligence_not_enabled" }
```

## Errors

Failures are structured JSON on **stdout** (not stderr, so the core decodes one stream)
with a non-zero exit: **2** for usage errors, **1** for everything else.

```json
{ "contract": 1, "error": "backend_unavailable", "message": "Apple Foundation Models unavailable: model_not_ready" }
```

| code                  | meaning                                                            |
| --------------------- | ------------------------------------------------------------------ |
| `bad_usage`           | missing, unknown or malformed arguments (exit 2)                    |
| `file_not_found`      | the input path does not exist or is not readable                    |
| `unsupported_format`  | the file is neither a decodable image nor a PDF                     |
| `render_failed`       | a page could not be rasterised                                      |
| `ocr_failed`          | Vision returned an error                                            |
| `no_text`             | recognition succeeded but produced nothing                          |
| `empty_input`         | `classify` got an empty or non-UTF-8 stdin                          |
| `invalid_doctypes`    | `--doctypes` was empty or not `name:category`                       |
| `unknown_backend`     | `--backend` names a backend this binary does not implement          |
| `unsupported_os`      | the Apple backend needs macOS 26; this is macOS 15–25               |
| `backend_unavailable` | Apple Intelligence off, device not eligible, or model not ready     |
| `classify_failed`     | the model answered with nothing usable, or outside the catalog      |
| `internal_error`      | anything unforeseen; always carries a human message                 |

`model_not_found` and `model_load_failed` belong to the MLX helper and are documented in
`machelper-mlx/README.md`; the shared code lists them so both binaries speak one vocabulary.

Every one of these is a **graceful degradation signal**, not a crash: the Go core treats a
non-zero exit from this helper as "fall back to the next tier" (project constraint 9).

## Why not `@Generable`

The obvious way to write guided generation is the `@Generable` macro. It is not usable
here, for a reason that is about the design rather than the toolchain: **the doctype catalog
is user-configurable and only known at run time**, and a macro-generated schema is fixed at
compile time. Constraining the model to the *supplied* list therefore requires
`DynamicGenerationSchema` / `GenerationSchema(root:dependencies:)`, which is exactly what
Apple provides it for.

Two useful consequences fall out of that choice:

- the constraint is genuinely the user's catalog, not a hard-coded enum; and
- the package builds under CommandLineTools, because the `@Generable` macro plugin
  (`libFoundationModelsMacros.dylib`) ships only with full Xcode.

Decoding is still validated: the response is read back property by property and the doctype
is re-checked against the catalog before anything is emitted.
