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

**What this needs depends on whether you're installing a bottle or building
from source — and today, that means building from source, because no
release has been tagged yet and no bottle has ever been published.**

### Bottled install (the normal path, once releases exist)

Once a tagged release has published prebuilt (bottled) binaries, `brew
install kagaz` downloads a prebuilt arm64 `kagaz` binary and needs
**nothing beyond Homebrew itself** — no Xcode, no Command Line Tools, no
Go. The only runtime dependency Homebrew pulls in is `poppler` (for
`pdftotext`), itself bottled. This is the intended everyday experience for
a Kagaz user.

### Source build (today's actual path, and `--build-from-source` always)

**No release has been tagged yet, so no bottles exist yet.** Until the
first tagged release publishes bottles, `brew install kagaz` — or any
explicit `brew install --build-from-source kagaz` afterwards — builds
everything locally, and that build needs:

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

So: if you `brew install kagaz` today, `xcode-select -p` pointing at
`CommandLineTools` (the common case) is already sufficient — Homebrew
installs Go for you as a build dependency automatically.

### `kagaz-mlx` (separate, opt-in formula — a different toolchain story)

```
brew install getkagaz/kagaz/kagaz-mlx
```

Builds the MLX classifier tier (`machelper-mlx/`). Kept as its own formula,
never folded into the base `kagaz.rb`, both because it pulls in the whole
MLX-Swift stack and multi-gigabyte model weights nobody should get by
installing the base package, *and* because its build needs more than
`kagaz.rb` does: MLX compiles bundled **C++** sources, which need a
complete `libc++` header set. Full Xcode's toolchain has this; a Command
Line Tools install can be missing it, which fails the build with `fatal
error: 'cstdlib' file not found` — a broken/incomplete CLT install, not a
code problem. Check yours before building:

```
printf '#include <cstdlib>\nint main(){}\n' | clang++ -x c++ -c - -o /dev/null
```

If that fails, either reinstall Command Line Tools
(`sudo rm -rf /Library/Developer/CommandLineTools && xcode-select --install`)
or build with a full Xcode install's toolchain instead (see
[CONTRIBUTING.md](../CONTRIBUTING.md#swift-and-xcode)). The requirement here
is **a working C++ toolchain**, not "Xcode" specifically — Xcode's toolchain
is simply one reliable way to get one.

**Status recap:** Kagaz is pre-1.0 and not yet released. There is no
published bottle and no shipped Homebrew Cask for the menu-bar app yet.
Signing, notarization and cask packaging for the menu-bar app additionally
need an Apple Developer account, tracked as a remaining human-gated step
(see [HOMEBREW_CORE.md](HOMEBREW_CORE.md)).

## Building from source directly (bypassing Homebrew)

```
git clone https://github.com/getkagaz/kagaz
cd kagaz
go build -o kagaz ./cmd/kagaz
go build -o kagaz-mcp ./cmd/kagaz-mcp
```

The Swift helper needs a separate build. Command Line Tools alone is enough
— `machelper` needs no macro plugin and no full Xcode install, for the
reason explained above:

```
cd machelper && swift build -c release
```

`DEVELOPER_DIR` is not required for this build. It's worth knowing about
only if your Command Line Tools install is broken, or if you're building
`machelper-mlx/` instead (which needs a working C++ toolchain — see the
`kagaz-mlx` section above) and want to point at Xcode's toolchain rather
than fix CLT:

```
DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer swift build -c release
```

`machelper-mlx/` builds the same way (`cd machelper-mlx && swift build -c
release`) but is not part of the default build — see
[CONTRIBUTING.md](../CONTRIBUTING.md) for the full local dev loop, including
running the Go test suite (which is Linux-safe) versus the parts of the
tree that only build on macOS.

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
