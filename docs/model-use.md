---
title: Model use and licensing
---

# Model use and licensing

Kagaz's classification and OCR are tiered so that a useful vault manager
never *requires* a model download, and so that when you do choose to use a
model, you know exactly what it is, where it runs, and under what license.

## The tiers

1. **Rules** (`internal/vaultkit/doctypes`) — always available, on by
   default, zero download. High-precision keyword and pattern matching
   against the resolved doctype catalog. This is the universal fallback:
   every other tier degrades to this one on failure, unavailability, or low
   confidence, never to a hard error and never to an invented category.
2. **Apple Vision** (OCR only) — bundled with macOS via `kagaz-machelper`,
   no separate download, no license note needed.
3. **Apple Foundation Models** (classification, macOS 26+) — Apple's
   on-device model, bundled with the OS, gated
   `@available(macOS 26, *)`. Uses guided generation constrained to the
   vault's own doctype catalog. No download, no separate license note: use
   of this tier is covered by your macOS license and Apple's on-device
   model terms.
4. **MLX** (classification, opt-in) — a local model run via MLX-Swift,
   weights fetched by `kagaz model pull` from a **pinned** Hugging Face
   repository and revision. The model is
   `mlx-community/Qwen2.5-3B-Instruct-4bit` and is not configurable: it is
   what `model pull` fetches, what this build pins a revision for, and what
   the bundled Metal shader library was compiled against. It is a **text**
   LLM, deliberately, not a vision-language model; classification runs on the
   OCR'd text, not on document images.
5. **Ollama** (classification and, separately, OCR) — opt-in, talks only to
   a local Ollama daemon (`http://localhost:11434` by default), for people
   who already run Ollama and want to point Kagaz at a model of their
   choosing. `classify.model` is that choice, and it is Ollama's alone; it has
   no default, so until you set an Ollama name:tag the tier reports itself
   unavailable rather than run a model you did not pick. OCR is opted into
   separately and just as explicitly: `ocr.ollama.enabled` defaults to
   `"false"`, so a vault that does not ask for it never sends a document
   image to the daemon, even where one is running with a vision model
   loaded.

`classify.engine` names one of four engines, and every model engine ends at the
deterministic rules tier: **apple** (the default — Apple's on-device model,
then rules), **mlx**, **ollama**, and **rules** (no model is run and no
availability probe is taken). There is no `auto`: a chain you cannot see is one
you cannot reason about, and `engine: auto` is now rejected with an error
rather than quietly read as something else.

Nothing is installed on your behalf. `apple` needs no download and no daemon,
and on a Mac without the on-device model the rules tier answers — which is what
makes it safe as the default. MLX and Ollama are reached only when you name
them, and naming one whose weights or daemon are not there is an error naming
the fix, never a quiet fall back. `kagaz doctor` prints the order it will
actually try, and says which precondition is missing when one is.

## The only network call

`kagaz model pull` is the **only** place in the entire codebase permitted to
make an outbound network request (Global Constraint 2). It:

- downloads from a pinned repo **and revision** — never a moving `latest`
  tag, so what you get today is what you'll get if you `pull` again next
  year;
- writes into `~/Library/Application Support/kagaz/models/<repo>/`;
- verifies every file's SHA256 before marking the download `status: ready`,
  and is resumable if interrupted;
- is a no-op when a model is already `ready`;
- is never invoked implicitly — `ingest`, `find`, `classify` and everything
  else either use an already-downloaded model or fall back, they never
  trigger a download on your behalf.

Ollama pulls (`kagaz model pull --engine ollama`) delegate to your local
Ollama daemon's own pull mechanism, which is Ollama's network activity, not
Kagaz's — Kagaz still never dials out itself.

## Licensing

Every model `kagaz model pull` downloads carries its own license, set by
whoever trained and published it — Kagaz does not relicense, bundle, or
vendor model weights, and this repository contains none. `model pull`
prints an informational license note for the chosen model before/while
downloading, naming the model and pointing at its published license. This
note is **informational, not a gate**: Kagaz does not — and, as a
document-management tool, has no basis to — adjudicate a model's license on
your behalf. Reading and accepting a model's license is your responsibility
before you run `kagaz model pull` for it, same as installing any other
third-party model.

The default MLX model, `mlx-community/Qwen2.5-3B-Instruct-4bit`, is a
community re-quantization of Qwen2.5-3B-Instruct. **Note that the 3B size is
not Apache-2.0 like most of the Qwen2.5 family** — it ships under the separate
Qwen Research/Community licence, whose terms are more restrictive. Kagaz
redistributes no weights, so this creates no obligation for Kagaz itself, but
do not read "MIT project" as "MIT model": if you are using Kagaz commercially,
read that licence before enabling the MLX tier, or point `classify.model` at a
model whose terms you have checked. Consult the model's page on Hugging Face
for the current text.

## Confidentiality

Regardless of which classifier tier is active, document text used for
classification never leaves the machine — every model above runs on-device
or against a localhost-only endpoint that Kagaz verifies at call time, not
just at config time. See [architecture.md](architecture.md#no-network-at-inference-or-query-time).
