# Contributing to Kagaz

Thanks for considering a contribution. Kagaz is young (pre-1.0) and its
conventions are still settling, so an issue or a discussion before a large
PR is genuinely useful — it's much cheaper to align on approach before code
than after.

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

Swift packages (`machelper/`, `machelper-mlx/`) need macOS and (for
`machelper`'s Apple Foundation Models path) full Xcode, not just Command
Line Tools — the `@Generable` macro plugin used for guided generation ships
only inside Xcode. (`app/`, the planned SwiftUI menu-bar app, is an empty
placeholder directory as of this writing — nothing to build there yet.)

```
cd machelper && swift build
```

If `xcode-select -p` points at Command Line Tools rather than a full Xcode
install (common if you installed Xcode after CLT, or use CLT day-to-day for
other work), point this one build at Xcode explicitly instead of changing
your global `xcode-select`:

```
DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer swift build
```

The same applies to `swift test`, `swift build -c release`, and to
`machelper-mlx/`'s own build.

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
