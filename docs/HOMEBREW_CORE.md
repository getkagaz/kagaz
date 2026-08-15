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
| `Formula/kagaz.rb` builds `kagaz` + `kagaz-mcp` (Go) and `kagaz-machelper` (Swift) | In this repo; unverified end-to-end on a clean machine (see below). |
| `Formula/kagaz-mlx.rb` (opt-in `machelper-mlx` build) | In this repo, separate formula so the base install stays small. |
| `depends_on arch: :arm64`, `depends_on macos: :sequoia`, `depends_on xcode: :build`, `depends_on "go" => :build` | Declared in the base `kagaz.rb` formula. `xcode: :build` and `"go" => :build` are build-time only — a bottled install pulls in neither; see [installation.md](installation.md) and [the README FAQ](../README.md#faq) for why the Xcode dependency stays on the base formula rather than moving to a third, split-out formula the way `kagaz-mlx` is split out. |
| `livecheck` block | Declared, tracks `github_latest`. |
| Formula `test do` block | Inits a vault in the Homebrew sandbox and runs `find --json`. |
| A tagged `v*` release with arm64 bottles | **Not done.** No release has been cut. Bottle publication needs a live tagged release plus the `KAGAZ_TAP_TOKEN` secret configured in this repo's Actions settings — nobody should read this checklist and conclude a bottle exists. |
| `brew install --build-from-source` works on a clean Apple-silicon Mac with Xcode installed | **Unverified.** This has not been run on a machine without the development environment already present; it needs a clean-machine test before the first tagged release. |

## Homebrew Core submission (longer-term, optional)

Homebrew Core has its own bar, separate from and higher than "installs from
our tap." Submitting there is not required for Kagaz to be usable — most
users would install via `brew tap getkagaz/kagaz && brew install kagaz`
indefinitely — but if it's ever pursued:

| Requirement | Status |
|---|---|
| Notable, stable, maintained upstream project | Not applicable pre-1.0: no tagged release yet. |
| No `xcode: :build` dependency (Homebrew Core formulae must build with Command Line Tools alone) | **Currently violated.** `kagaz-machelper`'s use of Apple Foundation Models guided generation needs the `@Generable` macro plugin, which ships only with full Xcode, not Command Line Tools. Resolving this (if ever) would mean either accepting Xcode as a build dependency permanently in the tap (current plan) or finding a Command-Line-Tools-compatible path, which does not currently exist for this API. |
| License present and correct (MIT) | Done — see [`LICENSE`](../LICENSE). |
| No vendored dependencies beyond what the formula declares | Go module list is closed (Global Constraint 11); Swift packages declared per `Package.swift`. |
| `depends_on` correctly scoped (no unnecessary deps) | `poppler` (pdftotext), `go` (build-time), `xcode` (build-time) — reviewed as of this writing, not independently audited by a Homebrew maintainer. |
| Test block exercises real functionality without network access | Done in `kagaz.rb`'s `test do`; needs re-verification against an actual bottle once one is built. |

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
