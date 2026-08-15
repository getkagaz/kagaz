# kagaz-machelper-mlx

The **opt-in** MLX classification tier for Kagaz. Same JSON contract as
`kagaz-machelper classify`, different engine.

This is a separate SwiftPM package from `machelper/` on purpose: `kagaz-machelper` has zero
package dependencies and ships in the default Homebrew formula, while this one pulls the
whole MLX + swift-transformers graph and so gets its own opt-in formula. It is never on the
default install path.

## Building

```sh
swift build -c release
```

The binary lands at `.build/release/kagaz-machelper-mlx`.

**Toolchain.** Builds with either the Command Line Tools toolchain or full Xcode. If your
`xcode-select -p` points at CommandLineTools and you want Xcode's toolchain anyway, prefix
the command rather than switching the global setting:

```sh
DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer swift build -c release
```

**Network.** `swift build` resolves packages from GitHub, so the *build* needs network
access. The **binary** never does — see below.

**Platform.** macOS 15 (Sequoia) floor, Apple silicon (MLX requires a Metal GPU).

## Weights

Weights are read from

```
~/Library/Application Support/kagaz/models/<hf-repo>/
```

for example `.../models/mlx-community/Qwen2.5-3B-Instruct-4bit/`.

**This helper never downloads anything.** Project constraint 2 allows exactly one outbound
network call in the whole codebase, and it lives in `kagaz model pull`. The helper builds a
`ModelConfiguration(directory:)` pointing at an already-populated folder, so Hugging Face's
`HubApi` download path is never entered. A missing or half-finished cache (no `config.json`,
no `.safetensors`) is reported as a structured `model_not_found` error naming the exact
`kagaz model pull` command to run.

The default model is `mlx-community/Qwen2.5-3B-Instruct-4bit`, a **text** LLM — which is why
the loader is `LLMModelFactory` / the `MLXLLM` text path and **not** the VLM loader.

## Commands

```sh
kagaz-machelper-mlx classify --backend mlx --doctypes "invoice:financial,..." \
    [--model mlx-community/Qwen2.5-3B-Instruct-4bit] [--max-chars N] [--json]
kagaz-machelper-mlx --probe [--model <repo>]
kagaz-machelper-mlx --version
```

`classify` reads the document text from **stdin**. `--json` is accepted for symmetry;
output is always JSON.

```json
{ "contract": 1, "engine": "mlx", "doctype": "invoice", "category": "financial",
  "confidence": 0.92, "fields": { "amount": "4800" } }
```

### `--probe`

Checks only that the weight directory exists and looks complete. No model is loaded, so it
stays in the millisecond range — the Go core calls it before every classify. Exits 0 either
way; the answer is in `available`.

```json
{ "contract": 1, "engine": "mlx", "available": true }
{ "contract": 1, "engine": "mlx", "available": false,
  "reason": "no weights for mlx-community/Qwen2.5-3B-Instruct-4bit at ...; run: kagaz model pull ..." }
```

## Keeping the catalog honest

MLX has no equivalent of Foundation Models' guided generation, so the constraint is enforced
twice instead of once:

1. the system prompt names the permitted doctypes and the exact JSON shape; and
2. every answer is parsed and **re-validated against the catalog** before emission — an
   out-of-catalog doctype becomes `classify_failed`, never an invented category
   (project constraint 8).

The parser tolerates what small instruction-tuned models actually emit: prose around the
JSON, ```json fences, `confidence` given as `92` instead of `0.92`, and numeric field
values. It extracts the first *balanced* `{…}` run, string-aware so a brace inside a quoted
value does not end the object early.

## Errors

Structured JSON on **stdout** with a non-zero exit: **2** for usage errors, **1** otherwise.

| code                  | meaning                                                        |
| --------------------- | -------------------------------------------------------------- |
| `bad_usage`           | missing, unknown or malformed arguments (exit 2)                |
| `empty_input`         | empty or non-UTF-8 stdin                                        |
| `invalid_doctypes`    | `--doctypes` was empty or not `name:category`                   |
| `unknown_backend`     | `--backend` was not `mlx`                                       |
| `backend_unavailable` | the backend is not usable right now                             |
| `model_not_found`     | no weights at the cache path — run `kagaz model pull <repo>`    |
| `model_load_failed`   | weights present but MLX could not load them                     |
| `classify_failed`     | no JSON, malformed JSON, or a doctype outside the catalog       |
| `internal_error`      | anything unforeseen; always carries a human message             |

## A note on `async`

`Main.main()` is `async` and every call below it is awaited. Do **not** reintroduce a
`DispatchSemaphore` bridge around the MLX loader: it hops back to the main actor, so
blocking the main thread on a semaphore deadlocks the process. This is a recorded bug, not
a hypothetical one.
