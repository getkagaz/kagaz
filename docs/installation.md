---
title: Installation
---

# Installation

## Requirements

- **Apple silicon Mac** (M1 or later). Intel Macs are not supported — see
  the FAQ entry in [the README](../README.md#faq) for why this is a
  deliberate, permanent decision rather than a temporary gap.
- **macOS 15 (Sequoia) or later.** The Apple Foundation Models classifier
  tier additionally needs **macOS 26**; on macOS 15–25 Kagaz runs with the
  Apple Vision OCR tier and the offline rules classifier, which is a fully
  functional vault manager on its own.

## Homebrew

```
brew tap getkagaz/kagaz
brew install kagaz
```

That installs `kagaz`, `kagaz-mcp` and `kagaz-machelper` from a prebuilt
arm64 bottle — no Go toolchain, no Xcode, no build. The MLX classifier is a
separate, opt-in formula:

```
brew install getkagaz/kagaz/kagaz-mlx
```

`kagaz-mlx` is bottled too, so it needs neither full Xcode nor a Metal build,
though its model weights are still a multi-gigabyte `kagaz model pull` away.

Note that `Formula/kagaz.rb` **in this repository** is not installable
directly: its `url` and `sha256` are placeholders that the release workflow
rewrites at tag time, and current Homebrew refuses a formula file outside a
tap regardless:

```
$ brew install --HEAD --dry-run ./Formula/kagaz.rb
Error: Homebrew requires formulae to be in a tap, rejecting:
  ./Formula/kagaz.rb
```

Install from the tap, not from a checkout.

## Homebrew, in detail

**What an install needs depends on whether you take the bottle or build from
source.**

### Bottled install (the normal path)

`brew install kagaz` downloads a prebuilt arm64 binary and needs **nothing
beyond Homebrew itself** — no Xcode, no Command Line Tools, no Go. The only
runtime dependency Homebrew pulls in is `poppler` (for `pdftotext`), itself
bottled. This is the everyday experience.

### Homebrew source build (`--build-from-source`)

An explicit `brew install --build-from-source kagaz` builds everything
locally, and that build needs:

- **Xcode's Command Line Tools** (`xcode-select --install`) — full Xcode is
  **not** required. `kagaz-machelper`'s Apple Foundation Models
  classification builds its guided-generation schema at run time with
  `DynamicGenerationSchema`, not the `@Generable` macro — the doctype
  catalog a vault classifies against is only known at run time (every vault
  can extend it via `vault.yaml`'s `doctypes:` block), and a macro's schema
  is fixed at compile time, so `@Generable` was never usable here to begin
  with. The macro plugin was the only thing that needed full Xcode, and
  `machelper` doesn't use it. `Formula/kagaz.rb` accordingly declares no
  `depends_on xcode: :build` at all. See `machelper/README.md`'s "Why not
  `@Generable`" for the full reasoning.
- **Go**, to build the CLI and MCP server. The formula declares
  `depends_on "go" => :build`.

So: for a Homebrew source build, `xcode-select -p` pointing at
`CommandLineTools` (the common case) is already sufficient — Homebrew
installs Go for you as a build dependency automatically.

### `kagaz-mlx` (separate, opt-in formula)

```
brew install getkagaz/kagaz/kagaz-mlx
```

Bottled since v0.1.1, so the normal install needs **no Xcode and no build** —
the caveat below applies only to `--build-from-source`. Kept as its own
formula,
never folded into the base `kagaz.rb`, both because it pulls in the whole
MLX-Swift stack and multi-gigabyte model weights nobody should get by
installing the base package, *and* because — unlike `kagaz.rb` — **this one
genuinely requires full Xcode to build**, for a reason specific to MLX, not
to Kagaz's own code:

1. MLX compiles bundled **C++** sources, which need a complete `libc++`
   header set. A Command Line Tools install can be missing it, which fails
   the build with `fatal error: 'cstdlib' file not found` — a broken/
   incomplete CLT install, not a code problem. Check yours with:

   ```
   printf '#include <cstdlib>\nint main(){}\n' | clang++ -x c++ -c - -o /dev/null
   ```

2. **`swift build` alone is not enough, even with a healthy C++ toolchain.**
   SwiftPM has no build rule for `.metal` shader sources at all — mlx-swift
   says so itself. Without a second step, the binary links and `--version`
   runs, but the first real MLX operation fails with `MLX error: Failed to
   load the default metallib`. That second step is
   `./Scripts/build-metallib.sh -c release` inside `machelper-mlx/`, which
   runs `xcrun metal`/`xcrun metallib` to produce `mlx.metallib` — and
   **`xcrun metal` ships only with full Xcode**, not Command Line Tools.
   There is no way around this one; it is a hard Xcode requirement for
   `kagaz-mlx`, not a "usually CLT is enough" situation like `kagaz.rb`.
   `mlx.metallib` then has to be installed in the *same directory* as
   `kagaz-machelper-mlx` (the formula does this), or the binary reports
   itself available and then fails on every classification. See
   `machelper-mlx/README.md` ("Why the second step exists", "Where
   mlx.metallib has to live") for the full detail.

If the C++ header check above fails, either reinstall Command Line Tools
(`sudo rm -rf /Library/Developer/CommandLineTools && xcode-select --install`)
or build with a full Xcode install's toolchain (see
[CONTRIBUTING.md](../CONTRIBUTING.md#swift-and-xcode)) — but building
`kagaz-mlx` from source needs Xcode installed regardless, for the Metal
step, even once your C++ toolchain is healthy.

**This is the opposite of the base `kagaz` formula, and it's worth stating
plainly so the two don't blur together: `kagaz.rb` needs no Xcode at all;
`kagaz-mlx.rb` needs it unconditionally, for `xcrun metal`.**

**Status recap:** Kagaz is pre-1.0 and not yet released. There is no
published bottle and no shipped Homebrew Cask for the menu-bar app yet.
Signing, notarization and cask packaging for the menu-bar app additionally
need an Apple Developer account, tracked as a remaining human-gated step
(see [HOMEBREW_CORE.md](HOMEBREW_CORE.md)).

## Building from source

```
git clone https://github.com/getkagaz/kagaz
cd kagaz
go build -o kagaz ./cmd/kagaz
go build -o kagaz-mcp ./cmd/kagaz-mcp
```

Copy the two binaries somewhere on your `PATH`. `kagaz` alone is a working
vault manager; the Swift helper and `poppler` (`brew install poppler`) each
add an optional tier, and `kagaz doctor` reports which ones it can see.

`kagaz-mcp` is the second binary only because MCP clients name a binary
rather than a subcommand — it is `kagaz mcp` under another name, and you
need it only if you want an agent to talk to a vault. See
[Wiring the MCP server into a client](agents.md#wiring-the-mcp-server-into-a-client).

The Swift helper needs a separate build. Command Line Tools alone is enough
— `machelper` needs no macro plugin and no full Xcode install, for the
reason explained above:

```
cd machelper && swift build -c release
```

`DEVELOPER_DIR` is not required for this build. It's worth knowing about
only if your Command Line Tools install is broken and you want to point at
Xcode's toolchain rather than fix CLT:

```
DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer swift build -c release
```

`machelper-mlx/` is a different story and genuinely needs Xcode installed —
see the `kagaz-mlx` section above for why (`swift build` alone links a
binary that cannot run a single MLX operation; a second, Xcode-only step
builds the Metal shader library it needs):

```
cd machelper-mlx
swift build -c release
./Scripts/build-metallib.sh -c release
```

Not part of the default build — see [CONTRIBUTING.md](../CONTRIBUTING.md)
for the full local dev loop, including running the Go test suite (which is
Linux-safe) versus the parts of the tree that only build on macOS.

## First vault

```
kagaz init
```

writes a `vault.yaml` at `~/Documents/vault.yaml` (override with `--root`),
creates the category folders, and you're done — see
[the README quickstart](../README.md#quickstart) for the next steps, or
`kagaz init --demo` for a populated vault you can explore immediately.

## Verifying your setup

```
kagaz doctor
```

reports which optional tools are present (`pdftotext`, `kagaz-machelper`,
`mdfind`, `brctl`, Ollama) and which classifier backends are usable, without
failing on anything that only degrades a feature rather than breaking core
function.
