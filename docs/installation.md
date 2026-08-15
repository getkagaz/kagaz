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
from source — and today, that means building from source.**

### Bottled install (the normal path, once releases exist)

Once a tagged release has published prebuilt (bottled) binaries, `brew
install kagaz` downloads a prebuilt arm64 `kagaz` binary and needs
**nothing beyond Homebrew itself** — no Xcode, no Go. The only runtime
dependency Homebrew pulls in is `poppler` (for `pdftotext`), itself bottled.
This is the intended everyday experience for a Kagaz user.

### Source build (today's actual path, and `--build-from-source` always)

**No release has been tagged yet, so no bottles exist yet.** Until the
first tagged release publishes bottles, `brew install kagaz` — or any
explicit `brew install --build-from-source kagaz` afterwards — builds
everything locally, and that build needs:

- **Full Xcode**, not just Command Line Tools. `kagaz-machelper`'s Apple
  Foundation Models classification uses the `@Generable` macro for guided
  generation, and that macro plugin ships only inside Xcode. The formula
  declares `depends_on xcode: :build`.
- **Go**, to build the CLI and MCP server. The formula declares
  `depends_on "go" => :build`.

So: if you `brew install kagaz` today, install Xcode from the App Store
first (`xcode-select -p` should point inside `Xcode.app`, not
`CommandLineTools` — see [CONTRIBUTING.md](../CONTRIBUTING.md#swift-and-xcode)
if it doesn't) — Homebrew installs Go for you as a build dependency
automatically.

### `kagaz-mlx` (separate, opt-in formula)

```
brew install getkagaz/kagaz/kagaz-mlx
```

Builds the MLX classifier tier (`machelper-mlx/`). Kept as its own formula,
never folded into the base `kagaz.rb`, because it pulls in the whole
MLX-Swift stack and, on first use, `kagaz model pull` downloads several
gigabytes of model weights — nobody should get that by installing the base
package. (This is a different situation from the Xcode dependency above,
which the base formula does keep — see
[the FAQ entry on that decision](../README.md#faq).)

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

The Swift helper needs a separate build, and full Xcode (not just Command
Line Tools) for the same `@Generable`-macro reason as above:

```
cd machelper && swift build -c release
```

If `xcode-select -p` currently points at Command Line Tools rather than a
full Xcode install, point the build at Xcode explicitly for this one
invocation rather than changing your global `xcode-select` setting:

```
DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer swift build -c release
```

`machelper-mlx/` builds the same way but is not part of the default build —
see [CONTRIBUTING.md](../CONTRIBUTING.md) for the full local dev loop,
including running the Go test suite (which is Linux-safe) versus the parts
of the tree that only build on macOS.

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
