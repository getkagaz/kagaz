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
   repository and revision. Default:
   `mlx-community/Qwen2.5-3B-Instruct-4bit` — a **text** LLM, deliberately,
   not a vision-language model; classification runs on the OCR'd text, not
   on document images.
5. **Ollama** (classification and, separately, OCR) — opt-in, talks only to
   a local Ollama daemon (`http://localhost:11434` by default), for people
   who already run Ollama and want to point Kagaz at a model of their
   choosing.

`classify.engine: auto` (the default) chains every semantic tier this machine
actually has, cheapest first: **apple → mlx (if available) → ollama (if
available) → rules**. A tier hands over to the *next* one when it is
unavailable, errors, times out, emits malformed output, speaks an unknown
contract, declines with `unclassified`, answers below `min_confidence`, or
names a doctype outside the catalog — a decline by one model does not bind
another — and the answer falls to rules only when no tier does better.

Nothing is installed on your behalf: MLX and Ollama are reached only when
their weights or daemon are already present, so a machine that never ran
`kagaz model pull` behaves exactly as before. Naming an engine explicitly
still means "that tier, then rules": `mlx` never chains on to `ollama`.
`rules` is the explicit no-LLM choice — no model is run and no availability
probe is taken. `kagaz doctor` prints the order the chain will actually try.

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
community re-quantization of Qwen2.5-3B-Instruct; consult the model's page
on Hugging Face for its current license terms.

## Confidentiality

Regardless of which classifier tier is active, document text used for
classification never leaves the machine — every model above runs on-device
or against a localhost-only endpoint that Kagaz verifies at call time, not
just at config time. See [architecture.md](architecture.md#no-network-at-inference-or-query-time).
