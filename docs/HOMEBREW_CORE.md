---
title: Homebrew Core readiness
---

# Homebrew Core readiness checklist

This tracks what stands between the `getkagaz/kagaz` tap and eventual
submission to Homebrew Core, and — more immediately — what stands between
today and a first tap-installable release. Nothing on this page is done
just because it's listed; each row states its actual status.

## Tap release (near-term goal)

| Item | Status |
|---|---|
| `Formula/kagaz.rb` (builds `kagaz` + `kagaz-mcp` (Go) and `kagaz-machelper` (Swift)) | **Written.** Declares no `depends_on xcode: :build` — `machelper` needs only Command Line Tools; see the row below. `sha256`/`url` are still the placeholder tag `v0.1.0` will produce, filled in by `release.yml` at tag time. |
| `Formula/kagaz-mlx.rb` (opt-in `machelper-mlx` build) | **Written**, as a separate formula so the base install stays small. |
| `depends_on arch: :arm64`, `depends_on macos: :sequoia`, `depends_on "go" => :build` | **Declared in `kagaz.rb`.** No `depends_on xcode: :build` — correct, see below. `kagaz-mlx.rb` declares `arch: :arm64`, `macos: :sequoia`, **and now `depends_on xcode: ["16.0", :build]`** — this was a gap earlier in this doc's editing history (an in-progress `Formula/` change landed while this checklist was being written) and is now closed: `kagaz-mlx.rb`'s `install` step runs `swift build`, then `Scripts/build-metallib.sh` (which needs `xcrun metal`, hence the Xcode dependency), and installs both `kagaz-machelper-mlx` and `mlx.metallib` into `bin` together. |
| `livecheck` block | **Declared** in both formulae, `strategy :github_latest`. |
| Formula `test do` block | **Written** in both. `kagaz.rb`: inits a `--demo` vault, checks `--version`, runs `find --json`, probes `kagaz-machelper`. `kagaz-mlx.rb`: asserts `mlx.metallib` was installed alongside the binary, probes `kagaz-machelper-mlx`, asserts a clean sandbox reports no weights via a *weights* reason specifically (not a metallib/shader one — a regression guard against the exact "linked but shaderless binary" defect the Metal build step exists to prevent), and checks `--version`'s `tool` field. |
| A tagged `v*` release with arm64 bottles | **Not done.** No release has been cut. Bottle publication needs a live tagged release plus the `KAGAZ_TAP_TOKEN` secret configured in this repo's Actions settings — nobody should read this checklist and conclude a bottle exists; both formulae's `url`/`sha256` are still placeholders pointing at a tag that hasn't been created. |
| `brew install --build-from-source` works on a clean Apple-silicon Mac | **Partially verified.** A clean `swift build --package-path machelper -c release` succeeded twice under a Command-Line-Tools-only toolchain (confirmed `xcode-select -p` pointed at CommandLineTools, `rm -rf machelper/.build` first) and the resulting binary ran a real on-device classification. The full `brew install --build-from-source kagaz` path through the formula itself has not been run end-to-end on a genuinely clean machine — still needs that pass before the first tagged release. |

## Homebrew Core submission (longer-term, optional)

Homebrew Core has its own bar, separate from and higher than "installs from
our tap." Submitting there is not required for Kagaz to be usable — most
users would install via `brew tap getkagaz/kagaz && brew install kagaz`
indefinitely — but if it's ever pursued:

| Requirement | Status |
|---|---|
| Notable, stable, maintained upstream project | Not applicable pre-1.0: no tagged release yet. |
| No `xcode: :build` dependency (Homebrew Core formulae must build with Command Line Tools alone) | **Met for `kagaz.rb`, the formula that actually matters for this row.** `kagaz.rb` declares no `depends_on xcode: :build`. This was a real, non-obvious fix, not always true: `machelper`'s Apple Foundation Models guided generation initially looked like it needed the `@Generable` macro (full-Xcode-only), but the doctype catalog it constrains against is only known at run time — a vault can extend it via `vault.yaml` — and a macro's schema is fixed at compile time, so `@Generable` was never actually usable here regardless of toolchain. `machelper` builds its schema at run time with `DynamicGenerationSchema` instead, which needs only Command Line Tools. Verified directly: a clean `swift build --package-path machelper -c release` under a Command-Line-Tools-only `xcode-select -p` succeeds (confirmed twice, `.build` removed first each time), and the resulting binary performs a live on-device classification. **`kagaz-mlx.rb` is a separate formula and this row does not apply to it: it genuinely needs Xcode**, not just a working C++ toolchain as previously stated here — MLX's Metal shader library (`mlx.metallib`) can only be built with `xcrun metal`, which ships exclusively with Xcode, since SwiftPM has no build rule for `.metal` sources at all (`machelper-mlx/README.md`). That's a correction to what this row said before, not a new finding about `kagaz-mlx.rb`'s C++ requirement, which was already documented separately. Homebrew Core's bar is per-formula, and `kagaz-mlx` staying out of Core (or needing its own `xcode: :build` dependency if ever submitted) doesn't affect `kagaz.rb`'s standing here. |
| License present and correct (MIT) | Done — see [`LICENSE`](../LICENSE). |
| No vendored dependencies beyond what the formula declares | Go module list is closed (Global Constraint 11); Swift packages declare their own dependencies per `Package.swift`. `machelper/` has zero SwiftPM dependencies (hand-rolled argument parsing); `machelper-mlx/` resolves the MLX + swift-transformers graph from GitHub at build time, declared in its own `Package.swift`, not vendored. |
| `depends_on` correctly scoped (no unnecessary deps) | `kagaz.rb`: `poppler` (pdftotext), `"go" => :build`, `arch: :arm64`, `macos: :sequoia` — reviewed as of this writing, not independently audited by a Homebrew maintainer. `kagaz-mlx.rb`: `arch: :arm64`, `macos: :sequoia`, `xcode: ["16.0", :build]` (MLX needs a Metal GPU hence arm64; Xcode 16.0 is the floor that ships a Metal compiler targeting the macOS 15 SDK this package deploys against); deliberately no `depends_on "kagaz"`, so it can't disturb an existing core install. |
| Test block exercises real functionality without network access | Done in both formulae's `test do` — see the Tap release table above; needs re-verification against an actual bottle once one is built. |

## Signing, notarization and the Cask (menu-bar app)

Separate from the CLI formula: `app/` (the SwiftUI menu-bar app) needs
Apple code-signing and notarization, and a Homebrew Cask, before it can be
distributed as `Kagaz.app`. Both need an **Apple Developer Program account**,
which this project does not currently have configured. This is a
human-gated step: `build-app.sh` produces an unsigned, unnotarized
`Kagaz.app` locally today, and no cask exists in the tap. Nothing in this
repository should be read as claiming otherwise.

## What "done" will look like

1. A `v0.1.0` (or similar) tag pushed, triggering `release.yml`.
2. arm64 bottles built and attached to the GitHub Release.
3. The formula synced to `getkagaz/homebrew-kagaz` (the tap repo) with
   correct bottle SHA256s.
4. A clean-machine `brew tap getkagaz/kagaz && brew install kagaz` verified
   by someone who did not build the formula themselves.

Until all four are true, treat every install instruction elsewhere in this
repo's docs as "the intended path," not "the currently working path."
