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
| `Formula/kagaz.rb` (builds `kagaz` + `kagaz-mcp` (Go) and `kagaz-machelper` (Swift)) | **Not written yet.** `Formula/` is an empty directory in this repo as of this writing — there is no formula to install from at all today. The description here is the plan, not a description of existing code. |
| `Formula/kagaz-mlx.rb` (opt-in `machelper-mlx` build) | **Not written yet**, same as above — planned as a separate formula so the base install stays small. |
| `depends_on arch: :arm64`, `depends_on macos: :sequoia`, `depends_on xcode: :build`, `depends_on "go" => :build` | **Not declared anywhere yet** — there is no formula file for these to be declared in. `xcode: :build` and `"go" => :build` are intended to be build-time only, so a future bottled install would pull in neither; see [installation.md](installation.md) and [the README FAQ](../README.md#faq) for why the Xcode dependency is planned to stay on the base formula rather than move to a third, split-out formula the way `kagaz-mlx` is split out. |
| `livecheck` block | **Not written yet.** |
| Formula `test do` block | **Not written yet.** Planned to init a vault in the Homebrew sandbox and run `find --json`. |
| A tagged `v*` release with arm64 bottles | **Not done.** No release has been cut, and there is no formula yet to build one from. Bottle publication needs a live tagged release plus the `KAGAZ_TAP_TOKEN` secret configured in this repo's Actions settings — nobody should read this checklist and conclude a bottle exists. |
| `brew install --build-from-source` works on a clean Apple-silicon Mac with Xcode installed | **Unverifiable right now** — there is no formula to run `brew install` against. Needs a clean-machine test once `Formula/kagaz.rb` exists and before the first tagged release. |

## Homebrew Core submission (longer-term, optional)

Homebrew Core has its own bar, separate from and higher than "installs from
our tap." Submitting there is not required for Kagaz to be usable — most
users would install via `brew tap getkagaz/kagaz && brew install kagaz`
indefinitely — but if it's ever pursued:

| Requirement | Status |
|---|---|
| Notable, stable, maintained upstream project | Not applicable pre-1.0: no tagged release yet. |
| No `xcode: :build` dependency (Homebrew Core formulae must build with Command Line Tools alone) | **Would be violated once a formula exists.** `kagaz-machelper`'s use of Apple Foundation Models guided generation needs the `@Generable` macro plugin, which ships only with full Xcode, not Command Line Tools. Resolving this (if ever) would mean either accepting Xcode as a build dependency permanently in the tap (current plan) or finding a Command-Line-Tools-compatible path, which does not currently exist for this API. |
| License present and correct (MIT) | Done — see [`LICENSE`](../LICENSE). |
| No vendored dependencies beyond what the formula declares | Go module list is closed (Global Constraint 11); Swift packages declare their own dependencies per `Package.swift` (`machelper/`, `machelper-mlx/` exist; the formula that would reference them does not yet). |
| `depends_on` correctly scoped (no unnecessary deps) | Not yet applicable — planned as `poppler` (pdftotext), `go` (build-time), `xcode` (build-time) once `Formula/kagaz.rb` is written; not yet reviewable because it doesn't exist. |
| Test block exercises real functionality without network access | Not yet applicable — no formula, so no `test do` block exists yet to evaluate. |

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
