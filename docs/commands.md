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
| `--name <label>` | Human label for the vault, e.g. `"Personal & Family KYC"`, written as `name:` in the new `vault.yaml`. Without it, `init` leaves the field commented out and Kagaz displays the root folder name. Display only — it never becomes part of a path. See [configuration.md](configuration.md#name). |
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

### Saying what a document is

A document no tier can identify comes back `unclassified` and is skipped —
the correct outcome, because guessing a category is how documents get lost.
To file it anyway, say what it is:

```
kagaz ingest <path>... --set-doctype <name> [--set-owner <person>] \
                       [--set-identifier <text>] [--set-year <yyyy>]
```

The overrides apply to **every path in that invocation** — one doctype per
call, which is what a triage view wants: select the rows that are all the
same kind of document and call once. `--set-owner` takes a display name or a
tag from `people:` and may be repeated for several owners. Anything not
overridden is still inferred, and OCR, extraction and the sidecar text still
happen in full: only the inference is replaced.

`--set-doctype` is validated against the resolved catalog and rejected, with
close matches named, if the doctype does not exist — a human may pick any
real doctype but may not invent one (Global Constraint 8). There is no
`--set-category`: the category always comes from the catalog.

A human assignment is recorded as one. The sidecar's `classifier` reads
`human`, no confidence is recorded (a person's decision is not a
probability), and the proposal's `why` lines say you specified the doctype,
naming what the classifier had answered instead. Kagaz does not learn from
the assignment — it prints a one-line suggestion to add a keyword to the
`doctypes:` block in `vault.yaml`, and never edits your config itself.

Readable formats: PDFs, images, plain text, Office `.docx`/`.xlsx`/`.pptx`
(read in-process, no Microsoft Office or external tool needed), and the
legacy `.doc`/`.rtf`/`.rtfd`/`.odt`/`.wordml`/`.xls`/`.ppt` — the first five
of those go through macOS's built-in `/usr/bin/textutil` and are macOS-only;
`.xls`/`.ppt` are parsed in-process like the modern formats. A file whose
format isn't in this list, or that can't be extracted for some other
reason, is skipped with guidance in the batch review explaining why and
what to do instead.

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
It also reports the vault's name — `name:` from `vault.yaml`, or the root
folder name — as a `vault:` line and as `vault_name` in `--json`, which is
how a GUI learns which vault it is pointed at.
Exits non-zero only for problems that actually break core function — a
missing optional tool degrades a feature and is reported, not treated as
fatal (Global Constraint 9).

The `classify:chain` check reports the tier order that will actually run,
e.g. `apple: apple -> rules`, so a client shows the order instead of
recomputing it.

Each classifier check carries a `reason` alongside its prose `detail` in
`--json`, naming **which** precondition is unmet in a stable vocabulary.
`detail` is written for a person and is reworded whenever it reads better;
`reason` is an API and its values do not change:

| `reason` | Meaning | Fixed by |
|---|---|---|
| `weights_missing` | model weights absent or the download is incomplete | `kagaz model pull` |
| `helper_missing` | the helper binary is not installed or not found | building/installing the helper |
| `shader_library_missing` | the MLX Metal shader library is not beside the helper | `Scripts/build-metallib.sh` — never a download |
| `no_metal_device` | no Apple silicon GPU for MLX | nothing |
| `os_unsupported` | this macOS cannot host the tier (Apple's model needs macOS 26) | nothing |
| `model_unavailable` | the OS supports it, but the model is not usable right now | waiting, or enabling it in System Settings |
| `model_not_configured` | `classify.model` is empty | setting it |
| `daemon_unreachable` | no Ollama server answered at the endpoint | starting the daemon |
| `model_not_pulled` | the daemon answers but has not pulled the model | `ollama pull <model>` |
| `probe_timeout`, `contract_mismatch`, `unreadable_probe`, `unknown` | the probe did not give a usable answer | see `detail` |

The distinction that earns the field: MLX has three independent
preconditions and only `weights_missing` is fixed by a download. A client
that decided from the prose would offer a 1.6 GB pull for a missing helper
binary — minutes of waiting that change nothing.

## `kagaz watch`

Watches the vault (via `fsnotify`, debounced) for new or changed files and
runs the ingest pipeline's *propose* stage only — it never auto-executes a
move. Meant to run under `brew services start kagaz` as a background helper
that keeps proposals current without you needing to remember to run
`kagaz ingest`.

## `kagaz mcp`

Runs the Model Context Protocol server on stdin/stdout: newline-delimited
JSON-RPC 2.0, one message per line, answering `initialize`, `tools/list` and
`tools/call`. It serves until stdin closes. The protocol revision reported by
`initialize` is `2025-06-18`; `2025-03-26` and `2024-11-05` are accepted if a
client asks for them. `kagaz-mcp` is the same server under its own name, for
client configurations that name a binary rather than a subcommand — every
argument is forwarded to `kagaz mcp`.

The tool surface is fixed at four propose-only tools:

| Tool | Wraps |
| --- | --- |
| `find` | `kagaz find --json` |
| `ingest_propose` | `kagaz ingest --propose-only --json` |
| `tag` | `kagaz tag --propose-only --json` |
| `resolve_for_send` | `kagaz resolve --for-send --json` |

Each returns that command's envelope verbatim, so an MCP client and a
`kagaz --json` caller see identical data. There is deliberately **no tool that
executes a proposal**: an agent proposes and a human runs the CLI.
`resolve_for_send` preserves the confidential gate exactly as described above —
it cannot auto-confirm, only the caller's explicit `confirm: true` argument
supplies consent, the audit line is written on both branches before any path is
handed over, and a gated call without confirmation returns the
`confirmation_required` structure and no path.

`kagaz mcp --describe` prints the surface (with `--json`, as an envelope)
instead of serving it. See [docs/agents.md](agents.md) for how an agent is
expected to use this.

## `kagaz completion`

Shell completion scripts (bash, zsh, fish, PowerShell), generated by cobra —
`kagaz completion zsh > ~/.zsh/completions/_kagaz`, or via the Homebrew
formula's completion hooks once installed with `brew install kagaz`.
