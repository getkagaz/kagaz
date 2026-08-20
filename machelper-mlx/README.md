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
./Scripts/build-metallib.sh -c release
```

The binary lands at `.build/release/kagaz-machelper-mlx` and the Metal shader library
beside it at `.build/release/mlx.metallib`.

Both steps need **Xcode's** toolchain, not just the Command Line Tools. If `xcode-select -p`
points at `/Library/Developer/CommandLineTools`, prefix both with
`export DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer` — see
[Toolchain](#toolchain) for why, and for what the failure looks like when you do not.

### Why the second step exists

**`swift build` alone produces a binary that cannot run a single MLX operation.** SwiftPM
has no build rule for `.metal` sources. mlx-swift says so itself:

> SwiftPM (command line) cannot build the Metal shaders so the ultimate build has to be done
> via Xcode.
> — [mlx-swift `README.md`](https://github.com/ml-explore/mlx-swift)

So the link succeeds, `--version` works, and the first MLX op dies from C++ with

```
MLX error: Failed to load the default metallib. library not found library not found library not found
```

`Scripts/build-metallib.sh` is the missing step. It runs `xcrun metal` / `xcrun metallib`
over the nine kernels mlx pre-compiles under `MLX_METAL_JIT` — mirroring
`mlx/backend/metal/kernels/CMakeLists.txt` upstream — against the mlx-swift checkout
`swift build` already resolved. It takes a few seconds; the rest of MLX's kernels are
JIT-compiled at run time from string preambles, which is why the file is small.

Compiling Metal needs **Xcode**, not just the Command Line Tools (`xcrun metal` ships only
with Xcode). The script says so if it is missing.

### Where mlx.metallib has to live

mlx looks for its shader library in this order
(`Source/Cmlx/mlx/mlx/backend/metal/device.cpp`, `load_default_library`):

1. `<directory of the running binary>/mlx.metallib` ← what we build
2. `<directory of the running binary>/Resources/mlx.metallib`
3. `<some loaded bundle>/mlx-swift_Cmlx.bundle/default.metallib` (only an Xcode build
   populates this)

(1) is what mlx's own CMake install emits, so **`mlx.metallib` must be installed in the
same directory as `kagaz-machelper-mlx`** and stays with it. mlx finds the directory via
`dladdr`, which resolves symlinks, so a symlinked launcher still points at the real
install directory.

<a id="toolchain"></a>
**Toolchain.** No macro plugin is used, so nothing here *requires* Xcode. But unlike
`machelper`, this package compiles MLX's bundled **C++** sources, which need a complete
libc++ header set. A CommandLineTools installation with an incomplete
`/Library/Developer/CommandLineTools/usr/include/c++/v1` fails on whichever standard
header the first translation unit reaches — `'cassert' file not found`, `'cmath'`, `'map'`,
`'cstdlib'` — several at once, since the C++ sources compile in parallel. It is a broken
CLT install rather than a code problem. Check it with:

```sh
printf '#include <cstdlib>\nint main(){}\n' | clang++ -x c++ -c - -o /dev/null
```

If that fails, build against Xcode's toolchain:

```sh
export DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer
swift build -c release
./Scripts/build-metallib.sh -c release
```

**Set it for both steps.** `DEVELOPER_DIR` is what step 2 needs anyway — `xcrun metal`
ships only with Xcode — so exporting it once covers the build and the shaders. Setting it
inline on `swift build` alone leaves the shader step to fail separately, with its own
message saying the same thing.

**Having Xcode installed is not enough.** If `xcode-select -p` prints
`/Library/Developer/CommandLineTools`, SwiftPM uses the CLT headers no matter what is in
`/Applications`, and the build fails exactly as it does on a machine with no Xcode at all.
`DEVELOPER_DIR` overrides that for one command without `sudo` and without changing the
machine's selected toolchain, which is why it is the first thing to reach for. Making it
permanent is `sudo xcode-select -s /Applications/Xcode.app/Contents/Developer`; reinstalling
the Command Line Tools
(`sudo rm -rf /Library/Developer/CommandLineTools && xcode-select --install`) also works and
is the only option when Xcode is genuinely absent.

### Packaging

`Formula/kagaz-mlx.rb` must build **and install both files into the same directory**:

```ruby
system "swift", "build", "--disable-sandbox", "-c", "release"
system "./Scripts/build-metallib.sh", "-c", "release"
bin.install ".build/release/kagaz-machelper-mlx"
bin.install ".build/release/mlx.metallib"
```

Installing the binary without `mlx.metallib` installs something that dies on first use.
The formula's `test do` block can catch that: with the metallib present but no weights,
`--probe` must report `available: false` with a *weights* reason, never a shader one.

## Tests

```sh
swift test
```

Covers the pure parsing and validation half of the classifier — JSON extraction from model
prose, confidence rescaling, field coercion, catalog validation and model-path rejection.
It uses `swift-testing` from the toolchain, so it adds no external dependency.

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
output is always JSON. Unknown options are a hard error (`bad_usage`, exit 2).

`--model` takes a Hugging Face repo id (`org/name`) and nothing else: absolute paths and
components that are empty, `.` or `..` are rejected, so the resolved directory can never
escape the model cache.

```json
{ "contract": 1, "engine": "mlx", "doctype": "invoice", "category": "financial",
  "confidence": 0.92, "fields": { "amount": "4800" } }
```

### `--probe`

Answers **"can this backend actually classify?"** — not "does this binary exist". The Go
`Chain` selects the MLX tier on this answer, so a false positive is worse than no probe at
all: it makes the Chain pick a tier that fails on every document.

Two things must hold, and both are checked without loading a model:

1. **the MLX runtime works** — a Metal device exists, and mlx's shader library is present
   *where mlx looks for it* and loads on that device (see "Where mlx.metallib has to live");
2. **the weights are there** — the cache directory for the repo exists and looks complete.

No model is loaded, so it stays in the millisecond range. Exits 0 either way; the answer is
in `available`, and a `false` always carries a `reason`.

```json
{ "contract": 1, "engine": "mlx", "available": true }
{ "contract": 1, "engine": "mlx", "available": false,
  "reason": "no weights for mlx-community/Qwen2.5-3B-Instruct-4bit at ...; run: kagaz model pull ..." }
{ "contract": 1, "engine": "mlx", "available": false,
  "reason": "MLX shader library not found: no mlx.metallib beside the helper at ..." }
```

`classify` runs the same runtime check before it touches MLX, so a broken install is a
structured `backend_unavailable` on stdout rather than an MLX C++ error the Go core cannot
decode.

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
