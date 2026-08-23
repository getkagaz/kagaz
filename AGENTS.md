# Repository Guidelines

## Project Overview

Kagaz (`github.com/getkagaz/kagaz`) is a **local-first, macOS-only document vault manager**. It organizes a folder of documents by filesystem convention — not a database — so the vault is self-describing to humans and AI agents alike. A Go CLI (`kagaz`) is the authoritative core; compute-heavy, macOS-native work (Apple Vision OCR, Foundation Models / MLX classification) is offloaded to thin Swift sidecar binaries.

Open core: this repo (CLI, MCP server, `machelper` binaries) is MIT. A separate paid, closed-source SwiftUI menu-bar app ("Kagaz for Mac") lives elsewhere and is **not** in this repo — there is nothing to build for it here.

## Architecture & Data Flow

**The one rule: the `kagaz` CLI is the only mutator.** Every filesystem change (move/rename, Finder tag, sidecar write, audit append) flows through `internal/vaultkit/move.Engine`, reached only from the Go core. The MCP server and the external Mac app hold **zero vault logic**; they exec the `kagaz` binary with `--json` and parse its output.

```
vault.yaml (single source of truth)
        │ parsed by
        ▼
internal/vaultkit/*  (Go: config · fycal · conventions · doctypes · tags ·
        │             sidecar · audit · keychain · ocr · classify · move ·
        │             ingest · search · lint · index · models)
        ├── cmd/kagaz (cobra CLI) ── execs --json ──► Kagaz for Mac (separate)
        │                          └─────────────────► cmd/kagaz-mcp (stdio MCP)
        └── execs --json ──► kagaz-machelper / kagaz-machelper-mlx (Swift, macOS)
```

Mutation pipeline is **Propose → Preview → Approve → Execute**: a command builds a manifest, previews it, and only mutates on explicit approval (`--yes` / `--accept-proposal`; `--propose-only` always exits without mutating). Moves are SHA256-verified copies; the source is **staged, never deleted**. `kagaz rollback <manifest>` reverses any mutation. All mutations and confidential resolutions append to a JSONL audit log.

Go→Swift is a versioned **JSON contract over stdin/stdout**: Go spawns the helper, writes input to stdin, parses JSON from stdout. Helpers are discovered at runtime (`internal/vaultkit/ocr/machelper.go`); a missing helper degrades gracefully rather than failing.

## Key Directories

- `cmd/kagaz/` — CLI entry point (`main.go`, injects version, calls `cli.Main`).
- `cmd/kagaz-mcp/` — stdio MCP server entry point.
- `internal/cli/` — Cobra command tree; one file per command (`find.go`, `ingest.go`, `move.go`, `tag.go`, `resolve.go`, `lint.go`, `doctor.go`, `mcp.go`, …). `root.go` = command tree; `runtime.go` = `Runtime` DI struct; `output.go` = JSON envelope.
- `internal/vaultkit/` — **all** vault-mutating and vault-reasoning logic. Notable packages: `config` (parses/validates `vault.yaml`), `move` (the sole mutator + rollback), `ingest`, `classify` (tiered `apple|mlx|ollama|rules` chain), `ocr` (text extraction tiers), `conventions`, `doctypes`, `tags`, `sidecar`, `audit`, `search`, `lint`, `index`, `models`, `fycal`, `keychain` (not wired up yet).
- `machelper/` — Swift OCR/Apple-classification helper (`kagaz-machelper`). Builds under Command Line Tools alone; no package deps, no macro plugin.
- `machelper-mlx/` — opt-in Swift MLX classification helper (`kagaz-machelper-mlx`); pulls the MLX Swift package graph; needs full Xcode + a Metal shader build step.
- `docs/` — architecture, commands, configuration, JSON/machelper contracts, conventions guide. `docs/AGENTS.template.md` is a **vault-facing** template rendered by `kagaz index` (not repo docs — don't edit as prose).
- `testdata/fixture-vault/`, `examples/vault.yaml` — fixtures and a reference vault config.
- `Formula/` — Homebrew formulae (`kagaz.rb`, opt-in `kagaz-mlx.rb`).

## Development Commands

Go core (run from repo root):

```
go build ./...
go vet ./...
gofmt -l .            # must print NOTHING; CI fails on any listed file
go test ./...         # go test -race ./... in CI
```

Swift helpers (macOS only):

```
cd machelper && swift build                       # Command Line Tools suffice
cd machelper-mlx && swift build -c release && ./Scripts/build-metallib.sh -c release
# machelper-mlx needs FULL Xcode; if only C++ toolchain is fixed:
DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer swift build -c release
```
`mlx.metallib` (from `build-metallib.sh`) must sit beside the `kagaz-machelper-mlx` binary, or classification fails at runtime.

Run the CLI: `go run ./cmd/kagaz <command> [--json] [--vault PATH]`, e.g. `go run ./cmd/kagaz find --doctype invoice --json`.

## Code Conventions & Common Patterns

- **Go 1.23, no CGO.** No new third-party deps beyond `go.mod` (`cobra`, `pflag`, `yaml.v3`, `pkg/xattr`, `howett.net/plist`, `fsnotify`, `golang.org/x/sys`) without explicit justification.
- **`gofmt` + `go vet` clean** are non-negotiable. Follow `.editorconfig`.
- **Every exported Go identifier carries a doc comment.**
- **CLI structure:** each command is a `newXxxCommand(rt *Runtime) *cobra.Command` added in `root.go`. Persistent flags: `--vault`, `--json`, `--quiet`, `--version`. Dependency injection is via the `Runtime` struct (`internal/cli/runtime.go`) carrying `Out`/`Err`/`In`/`Vault`/`JSON`/`Quiet`.
- **JSON envelope contract** (`internal/cli/output.go`, `Envelope()`): every `--json` response is one object with reserved keys `command`, `status`, `schema_version` (currently `1`), and optional `warnings`. Payload is **flattened**, not nested. Keys sorted, stably indented → byte-identical across runs (golden tests depend on this). Statuses: `ok`, `proposed`, `confirmation_required`, `findings`, `error`. `status` ≠ exit code.
- **Error handling:** failures reported once, in the requested format — error envelope under `--json`, stderr message otherwise (`cli.reportError`). Usage errors distinguished from work failures (`isUsageError`).
- **Safety invariants** (expect line-by-line review when touching these): never `os.Rename`/`os.Remove` a vault path outside `move.Engine` (only permitted removals: Kagaz temp files + the SHA256-verified staging fallback); never delete a user document; no outbound network except `kagaz model pull` (Ollama clients hard-fail on non-localhost, re-checked at call time); no password in any filename/sidecar/INDEX/manifest/log (Keychain item name only); classifier output validated against the resolved catalog, degrading to rules then `unclassified` — never an invented category.
- **Adding a doctype:** vault-specific → `vault.yaml` `doctypes:`; global → `builtins` in `internal/vaultkit/doctypes/catalog.go` (lowercase dash-slug `name`, a `category` from `config.DefaultStructure()`, precise `keywords`, optional `patterns`/`extract`) plus a table-driven test asserting fire + near-miss non-fire.
- **Commits:** DCO required — sign off with `git commit -s`. Keep PRs single-purpose; include tests for new behavior.

## Important Files

- `cmd/kagaz/main.go`, `cmd/kagaz-mcp/main.go` — entry points.
- `internal/cli/root.go` — command tree + `Main`; `runtime.go` — DI struct; `output.go` — envelope source of truth.
- `internal/vaultkit/move/move.go` — the only mutator + rollback (safety-critical).
- `internal/vaultkit/config/config.go` — `vault.yaml` parse/defaults/validate.
- `internal/vaultkit/ocr/machelper.go` — Swift helper discovery + `RunHelper`.
- `go.mod` — Go 1.23, module `github.com/getkagaz/kagaz`.
- `Formula/kagaz.rb` (+ `kagaz-mlx.rb`) — Homebrew build/test.
- `docs/architecture.md`, `docs/commands.md`, `docs/json-envelope-contract.md`, `docs/conventions-guide.md` — canonical references.
- `CONTRIBUTING.md` — build/test matrix, Swift toolchain details, safety rules.

## Runtime/Tooling Preferences

- **Go 1.23**, CGO disabled, `GOTOOLCHAIN=local` (see formula). Standard `go` toolchain — no alternate build system.
- **Swift 6.0 tools** for helpers; `machelper/` = Command Line Tools only, `machelper-mlx/` = **full Xcode** + Metal shader build.
- **Runtime floor:** macOS 15 (Sequoia), **Apple silicon (arm64) only** — no x86_64 target. `pdftotext` (poppler) is a real runtime dependency.
- **Distribution:** Homebrew (`getkagaz/homebrew-kagaz` tap). Default install builds `kagaz` + `kagaz-machelper`; MLX is opt-in via `kagaz-mlx.rb`.
- No linters beyond `gofmt` + `go vet`; do not introduce new ones.

## Testing & QA

- **Go:** standard `testing`, table-driven, colocated `*_test.go`. Run `go test ./...` (CI uses `-race`). The suite **must pass on Linux and macOS** — every macOS-only path (Vision OCR, Keychain, Finder tags, Apple/MLX classification, `textutil`) is tested against a **recorded fixture or shell stub**, never by invoking the real tool. See `internal/vaultkit/ocr/machelper_test.go` for the stub pattern; a recent change (`test(cli): stop the suite reaching the real machelper binaries`) enforces this. New external-tool integrations must follow suit: record real output once, commit the fixture, assert against it.
- **Golden/contract tests:** `internal/cli/testdata/envelope/` + `envelope_contract_test.go` pin the JSON envelope; `internal/vaultkit/index/index_test.go` asserts the embedded template matches `docs/AGENTS.template.md` (why that `.md` is force-included in CI path filters). Byte-stability matters — keep output sorted and timestamp-free.
- **Swift:** swift-testing (ships with the toolchain), under `machelper/Tests` and `machelper-mlx/Tests`; covers the pure parse/validate half that never runs a live generation.
- **CI** (`.github/workflows/test.yml`): matrix `ubuntu-latest` + `macos-15`; steps = install poppler → `gofmt -l` gate → `go build ./...` → `go vet ./...` → `go test -race ./...`. `machelper.yml` builds/tests the Swift packages; `release.yml`/`tag-release.yml` handle bottling.
- **Before opening a PR:** `gofmt`/`go vet` clean and `go build ./...` + `go test ./...` green locally. PRs include tests for new behavior (not just bug fixes); `move`, `audit`, `keychain`, and the confidential-resolution path get line-by-line review.
