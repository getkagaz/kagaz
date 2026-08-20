# Contributing to Kagaz

Thanks for considering a contribution.

**Kagaz is open core.** This repository — the CLI, the MCP server and the
`machelper` binaries — is MIT-licensed and free, and stays that way. A separate
paid, closed-source macOS app is built on top of it, and its source is not
here. Saying so up front matters: contributions you make under MIT can appear
in a commercial product, which is what MIT permits and what you should know
before you start. Nothing documented in the README is withheld or degraded to
sell that app.

Kagaz is young (pre-1.0) and its conventions are still settling, so an issue or
a discussion before a large PR is genuinely useful — it's much cheaper to align
on approach before code than after.

## Build and test

Go 1.23, no CGO, no new third-party dependencies beyond what's already in
`go.mod` (`cobra`, `pflag`, `yaml.v3`, `pkg/xattr`, `howett.net/plist`,
`fsnotify`, `golang.org/x/sys`) — a PR adding a new module dependency should
expect to be asked to remove it or justify it explicitly first.

```
go build ./...
go vet ./...
gofmt -l .            # must print nothing
go test ./...
```

The Go test suite is written to pass on Linux as well as macOS — every
macOS-only code path (Vision OCR, Keychain, Finder tags, Apple Foundation
Models) is tested against a recorded fixture, never by invoking the real
tool. If you add a new external-tool integration, add its test the same
way: record the tool's real output once, commit the fixture, assert against
it.

### Swift and Xcode

Swift packages need macOS, but the two packages have **different toolchain
requirements** — don't assume they're the same:

- **`machelper/`** needs only **Xcode's Command Line Tools**, not full
  Xcode. Its `classify --backend apple` path builds guided generation's
  schema at run time with `DynamicGenerationSchema`, not the `@Generable`
  macro — the doctype catalog it constrains against is only known at run
  time (a vault can extend it via `vault.yaml`), and a macro's schema is
  fixed at compile time, so `@Generable` was never a usable choice here
  regardless of toolchain. It follows that the macro plugin — the one thing
  that needs full Xcode — is never invoked. Verified directly: a clean
  `swift build --package-path machelper -c release` succeeds under a
  Command Line Tools-only `xcode-select -p`. See `machelper/README.md`'s
  "Why not `@Generable`" for the full reasoning.

  ```
  cd machelper && swift build
  ```

- **`machelper-mlx/`** genuinely needs **full Xcode installed** — this is
  not the same situation as `machelper/` above, and it's not just about the
  C++ toolchain either. Two separate requirements stack here:

  1. MLX's bundled C++ sources need a complete `libc++` header set, which a
     Command Line Tools install can be missing (fails with `fatal error:
     'cstdlib' file not found`). Check yours before building:

     ```
     printf '#include <cstdlib>\nint main(){}\n' | clang++ -x c++ -c - -o /dev/null
     ```

     If that fails, reinstall Command Line Tools
     (`sudo rm -rf /Library/Developer/CommandLineTools && xcode-select --install`).

  2. Even with (1) satisfied, `swift build` alone is not enough: SwiftPM has
     no build rule for `.metal` shader sources, so the linked binary cannot
     run a single MLX operation until a second, **Xcode-only** step builds
     the Metal shader library:

     ```
     cd machelper-mlx
     swift build -c release
     ./Scripts/build-metallib.sh -c release
     ```

     `Scripts/build-metallib.sh` runs `xcrun metal`/`xcrun metallib`, which
     ship only with full Xcode — there is no Command Line Tools path around
     this step. If you only fixed the C++ toolchain, point the whole build
     at an installed Xcode instead:

     ```
     DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer swift build -c release
     DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer ./Scripts/build-metallib.sh -c release
     ```

  `mlx.metallib` (the script's output) then has to live in the same
  directory as the `kagaz-machelper-mlx` binary, or the binary reports
  itself available and fails on every classification instead. See
  `machelper-mlx/README.md` ("Why the second step exists", "Where
  mlx.metallib has to live") for the full detail.

  `DEVELOPER_DIR` is not needed for `machelper/` at all — that package
  builds correctly under Command Line Tools alone.

(Kagaz for Mac, the SwiftUI menu-bar app, is a separate paid, closed-source
product and is not part of this repository — there is nothing to build for it
here.)

## Package layout

All vault-mutating and vault-reasoning logic lives under
`internal/vaultkit/` — see [docs/architecture.md](docs/architecture.md) for
the full map. Two rules that apply to every change here:

1. **The CLI is the only mutator.** If your change relocates, renames, or
   otherwise changes a file on disk, it goes through
   `internal/vaultkit/move.Engine`, not a bespoke `os.Rename`/`os.Remove`.
   The Swift app and MCP server hold zero vault logic; they call the
   `kagaz` binary with `--json`.
2. **Never delete a user document.** The only permitted `os.Remove`/
   `os.RemoveAll` calls on a vault path are Kagaz's own temp files and the
   SHA256-verified staging fallback in `move.Engine.stage`. A PR that adds
   another one will be asked to route through staging instead.

Other constraints worth internalizing before you start (the full list is in
this repo's build-plan constraints; ask if you want the source doc):

- No outbound network call anywhere except `kagaz model pull`; any HTTP
  client talking to Ollama must hard-fail on a non-localhost host,
  re-checked at call time, never trusting config alone.
- Every copy is SHA256-verified; a mismatch aborts and leaves the source
  untouched.
- No password ever appears in a filename, sidecar, `INDEX.md`, manifest or
  log — only a Keychain item name.
- Classifier output is validated against the resolved doctype catalog:
  unknown or low-confidence results degrade to rules, then to
  `unclassified` — never an invented category.
- Every exported Go identifier carries a doc comment.

## Adding a doctype

Two places a new document type can live:

- **`vault.yaml`'s `doctypes:` block** — the right place for anything
  locale-specific (a country's particular tax form, a regional ID card) or
  personal to your own vault. See
  [docs/configuration.md#doctypes](docs/configuration.md#doctypes) and
  [`examples/vault.yaml`](examples/vault.yaml) for the shape.
- **`internal/vaultkit/doctypes/catalog.go`'s `builtins` list** — the right
  place for a document type that's genuinely global (not tied to one
  country's paperwork) and would benefit every vault. Add an entry with:
  - `name` — a lowercase, dash-separated slug.
  - `category` — must be one of the categories in
    `config.DefaultStructure()`.
  - `keywords` — phrases a real document of this kind *almost always*
    contains, chosen to avoid firing on the wrong document. Generic words
    ("premium", "platform") are deliberately excluded from the existing
    catalog for this reason; hold new entries to the same bar.
  - `patterns` (optional) — Go regexps for strong structural evidence (a
    machine-readable zone, a well-known ID format).
  - `extract` (optional) — field name → regex-with-one-capture-group, for
    structured facts worth pulling into the sidecar.

  Add a table-driven test in `internal/vaultkit/doctypes` asserting your
  keywords fire on a representative sample text and don't fire on a
  plausible near-miss (a receipt that mentions "invoice" in passing, say).

## Review expectations

- PRs are expected to include tests for new behavior, not just for bugs.
- A PR that touches `internal/vaultkit/move`, `audit`, `keychain`, or the
  confidential-resolution path in the CLI/MCP gets read line-by-line — these
  are the safety-critical surfaces; expect more scrutiny here than
  elsewhere, and don't take it personally.
- `gofmt` and `go vet` clean, and `go build ./...`/`go test ./...` passing
  on your machine before you open the PR — CI will re-check, but a red CI
  run slows review down for everyone.
- Keep PRs scoped to one change. A drive-by refactor bundled with a feature
  makes both harder to review.

## Developer Certificate of Origin (DCO)

Every commit must be signed off, certifying you have the right to submit the
change under this project's MIT license:

```
git commit -s -m "your message"
```

which appends a `Signed-off-by: Your Name <you@example.com>` trailer. PRs
with unsigned commits will be asked to amend before merge (`git rebase
--signoff HEAD~N`).
