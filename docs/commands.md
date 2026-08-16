---
title: Command reference
---

# Command reference

`kagaz` is a single binary (cobra-based). Every command accepts these
persistent flags:

| Flag | Meaning |
|---|---|
| `--vault <path>` | Use this vault instead of discovering `vault.yaml` by walking up from the current directory. |
| `--json` | Emit a stable, documented JSON shape instead of human-formatted text. Human output is for humans; JSON is for agents (scripts, the menu-bar app, the MCP server) — both come from the same underlying data, never a separate code path. |
| `--version` | Print the `kagaz` version and exit. |

Mutating commands never mutate silently: they print a preview and require
confirmation unless run with `--accept-proposal`/`--yes`, and
`--propose-only` prints the proposal and exits `0` without touching the
vault. See [architecture.md](architecture.md#propose--preview--approve--execute-with-manifest).

## `kagaz init`

Creates a new vault: writes `vault.yaml`, creates the category folders from
`structure`, and (unless `--root` is given) puts the vault at
`~/Documents`.

| Flag | Meaning |
|---|---|
| `--fy-start <1-12>` | Sets `fiscal_year.start_month`. |
| `--root <path>` | Vault root, instead of the default. |
| `--demo` | Populates a fully explorable demo vault: synthetic documents (not real PDFs of anyone's actual paperwork) across several categories and owners, with sidecars and Finder tags already applied, seeded people and fiscal-year tag vocabulary — such that `kagaz find` returns sensible results and `kagaz lint` is clean immediately after `init --demo`. |

## `kagaz find`

The read-only query command; the one every agent integration leans on most.

Filters: `--person`, `--company`, `--area`, `--doctype`, `--tag`,
`--active`, `--period` (calendar or fiscal, see
[conventions-guide.md](conventions-guide.md#fiscal-years)), plus a bare
positional full-text query matched against filename, path, sidecar text and
extracted fields. `kagaz find --json` is what the MCP `find` tool and the
menu-bar app both call underneath.

## `kagaz ingest`

Runs the propose pipeline (OCR → classify → extract → propose) over one or
more paths, presents a numbered batch review, and on approval executes
through `move.Engine` with a single manifest for the whole batch. Accepts
`all`, `none`, or a subset expression like `1,3-5` at the review prompt (or
via a flag, non-interactively). `--propose-only` stops after the preview.
`--reindex` regenerates sidecars for already-ingested documents.

## `kagaz move`

Relocates a document to a specific path, or re-derives its conventional
path and moves it there. Always goes through `move.Engine`: SHA256-verified
copy, tag carry-over, sidecar carry-over, staged (never deleted) source.

## `kagaz tag`

Adds/removes Finder tags on a document, validated against the vault's
controlled vocabulary (`--force` to bypass, which is itself worth thinking
twice about — an unvalidated tag is a `kagaz lint` finding waiting to
happen).

## `kagaz supersede`

Marks an existing document as replaced by a newer one: moves the lifecycle
tag from `active` to `superseded` on the old document and applies `active`
to the new one, keeping the `single_active_per_doctype_per_person` lint rule
satisfied instead of violating it in the same motion that was meant to fix
it.

## `kagaz lint`

Runs the rule engine over the tree: filename-grammar violations, doctypes
outside the catalog, files in the wrong category folder for their doctype,
tags outside the vocabulary, missing lifecycle tags (when configured),
multiple `active` documents where the single-active rule applies,
password-looking tokens in filenames (when configured), stale sidecars, and
orphaned sidecars. Each finding carries a rule id, severity, path, message
and whether `--fix` can repair it automatically. `--fix` only ever applies
provably safe repairs (normalize a filename to the grammar, move a file to
its conventional folder, add an unambiguous missing lifecycle tag) and every
fix still goes through `move.Engine` with its own manifest — a lint fix is a
mutation like any other, not a special case.

## `kagaz index`

Regenerates `INDEX.md` (counts by category/doctype, the tag vocabulary, a
fiscal-year note, ready-to-paste `mdfind` smart-folder queries) and
`AGENTS.md` (rendered from [`docs/AGENTS.template.md`](AGENTS.template.md)
plus `vault.yaml`, teaching an agent the vault's conventions, tag
vocabulary, `mdfind` patterns and how to call `kagaz --json`) at the vault
root. Both files carry a GENERATED banner — hand-edits are overwritten on
the next `kagaz index`.

## `kagaz rollback <manifest>`

Reverses a manifest written by any prior mutating command, moving each file
from its post-operation path back to its pre-operation path. Safe to run
twice: a row whose current path is already gone, or whose original path is
occupied again, is reported and skipped rather than failed.

## `kagaz resolve`

Resolves a document reference (a path, or filter expression) to its current
on-disk path, materializing it from iCloud first if needed
(`search.Materialize`, via `brctl download`).

### `resolve --for-send`

The confidential-resolution gate (safety invariant 7): resolving a document
tagged `confidential` (or otherwise gated by
`confidential.require_confirmation_on_resolve_for_send`) for handoff to
something outside the vault — attaching it to an email, uploading it,
handing it to another tool — requires **explicit confirmation**, and writes
an **audit line either way**, confirmed or refused. There is no flag or
mode that skips the audit line.

- **Interactively**, without `--json`: prints what's about to be resolved
  and why it's gated, and prompts for confirmation.
- **With `--json`, `resolve --for-send` never prompts.** It returns a
  structured response and a non-zero exit instead of a path:

  ```json
  {
    "status": "confirmation_required",
    "path": "Financial/Alex Rao/FY 2026/Invoice_Alex-Rao_Acme-Corp_2026.pdf",
    "reason": "tagged confidential",
    "message": "re-run with --confirm to resolve this document for external send"
  }
  ```

  An explicit `--confirm` flag supplies the consent an interactive prompt
  would otherwise gather, and only then does the command emit the resolved
  path and exit `0`. This keeps the gate scriptable for an agent without
  ever letting `--json` become a silent bypass — the audit line is written
  on the `confirmation_required` response too, recording that resolution
  was *attempted* and *not yet* confirmed.

## `kagaz log`

Prints the tail of the audit log (`kagaz log -n 20`), the same append-only
JSONL file every mutation and every confidential resolution writes to.

## `kagaz model pull`

The **only** command in Kagaz that reaches the network. Downloads MLX model
weights from a pinned Hugging Face repo/revision into
`~/Library/Application Support/kagaz/models/<repo>/`, resumable, verified
per-file by SHA256, marked `status: ready` only once every file checks out.
Re-running when already `ready` is a no-op. `--engine ollama` instead
delegates the pull to your local Ollama daemon. Prints an informational
license note for the chosen model (see
[model-use.md](model-use.md)) without gating the download on it — the
license is your responsibility to read, not Kagaz's to enforce.

## `kagaz doctor`

Checks vault health and environment: vault found and valid, Spotlight
(`mdfind`) available, `pdftotext` available, `kagaz-machelper` available,
which classifier backends are usable, Ollama reachable, iCloud/`brctl`
available, extended-attribute (xattr) support on the vault's filesystem.
Exits non-zero only for problems that actually break core function — a
missing optional tool degrades a feature and is reported, not treated as
fatal (Global Constraint 9).

## `kagaz watch`

Watches the vault (via `fsnotify`, debounced) for new or changed files and
runs the ingest pipeline's *propose* stage only — it never auto-executes a
move. Meant to run under `brew services start kagaz` as a background helper
that keeps proposals current without you needing to remember to run
`kagaz ingest`.

## `kagaz mcp`

**Not implemented in this build.** `kagaz mcp` prints the planned tool surface
and exits 1; there is no server to connect to yet. Until it lands, an agent
should drive the CLI directly with `--json`, which is the same data by
construction.

The planned surface is a stdio MCP server (JSON-RPC 2.0): `initialize`,
`tools/list`, `tools/call` for `find`, `ingest_propose`, `tag`,
`resolve_for_send`. Each tool will be a thin wrapper over the same vaultkit
calls the CLI itself uses, returning the same JSON shapes — an MCP client and a
`kagaz --json` caller will see identical data. `resolve_for_send` will preserve
the confidential gate exactly as described above: it cannot auto-confirm, and
returns the same `confirmation_required` structure. See
[docs/agents.md](agents.md) for how an agent is expected to use this.

## `kagaz completion`

Shell completion scripts (bash, zsh, fish, PowerShell), generated by cobra —
`kagaz completion zsh > ~/.zsh/completions/_kagaz`, or via the Homebrew
formula's completion hooks once installed with `brew install kagaz`.
