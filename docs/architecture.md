---
title: Architecture
---

# Architecture

## The one rule

**The `kagaz` CLI is the only mutator.** Everything that changes anything in
a vault — moves a file, writes a tag, writes a sidecar, appends to the audit
log — goes through `internal/vaultkit/move.Engine`, reached only from the Go
core. The Swift menu-bar app and the MCP server hold **zero vault logic**:
they shell out to the `kagaz` binary with `--json` and parse its output,
exactly like a human would from a terminal, or like any other external agent
integrating with Kagaz. This is not an implementation detail — it is the
thing that makes Kagaz auditable: one code path writes files, one code path
writes manifests, one code path can be reviewed for safety.

```
                       ┌─────────────────────────┐
                       │        vault.yaml         │
                       │  (single source of truth) │
                       └────────────┬──────────────┘
                                    │ parsed by
                                    ▼
┌───────────────────────────────────────────────────────────────┐
│                     internal/vaultkit/*  (Go)                   │
│  config · fycal · conventions · doctypes · tags · sidecar ·     │
│  audit · keychain · ocr · classify · move · ingest · search ·   │
│  lint · index · models                                          │
└───────────────────────────┬───────────────────────┬─────────────┘
                             │                        │ execs (--json)
                             ▼                        ▼
                     ┌───────────────┐        ┌──────────────────┐
                     │  cmd/kagaz     │        │ kagaz-machelper   │
                     │  (cobra CLI)   │        │ kagaz-machelper-mlx│
                     └───────┬────────┘        │  (Swift, macOS)   │
                             │ execs (--json)   └──────────────────┘
             ┌───────────────┼────────────────┐
             ▼                                 ▼
     ┌───────────────┐                ┌────────────────────┐
     │ Kagaz for Mac  │                │ cmd/kagaz-mcp        │
     │ (separate app) │                │ (stdio MCP server)   │
     └───────────────┘                └────────────────────┘
```

## Packages

All mutating and reasoning logic lives under `internal/vaultkit/`:

- **`config`** — parses, defaults and validates `vault.yaml`. Every other
  package takes a `*config.Config`; nothing hardcodes a folder name, a
  filename grammar, a tag vocabulary or fiscal-year math.
- **`fycal`** — fiscal-year and quarter arithmetic, configurable start month.
- **`conventions`** — renders a `Doc` (doctype, owners, identifier, year,
  modifier) into a filename and a folder path, and parses filenames back.
- **`doctypes`** — the built-in document-type catalog plus per-vault
  extensions, and the offline rules-based classifier used as the universal
  fallback.
- **`classify`** — the tiered classifier chain (`apple|mlx|ollama|rules`)
  behind one interface; validates every result against the resolved catalog
  before it's trusted (Global Constraint 8).
- **`ocr`** — text extraction, in preference order: plain text read directly
  (`.txt`, `.md`, no tooling at all); modern Office documents (`.docx`,
  `.xlsx`, `.pptx`) read as ZIP-of-XML with `archive/zip` + `encoding/xml`,
  no external tool or Microsoft Office needed, so this tier is always
  available; legacy binary Office (`.xls`, `.ppt`) parsed in-process as
  OLE2/BIFF8 compound files; `.doc`/`.rtf`/`.rtfd`/`.odt`/`.wordml` converted
  by macOS's `/usr/bin/textutil` (macOS-only — this slice is unavailable on
  Linux); `pdftotext` (fast path for text-layer PDFs); Apple Vision via
  `kagaz-machelper` (scanned images/PDFs); and an opt-in local Ollama vision
  model. Office/legacy-Office documents are capped at 64 MiB on disk and a
  64 MiB *decompressed* budget spent across the whole archive (ZIP or OLE2
  compound file); going over either is a hard extraction error naming the
  limit, not a silent truncation. Extracted text itself is capped at 1 MiB
  (`MaxOfficeTextBytes`, shared with the plain-text tier's own cap) and text
  past that point is dropped silently at the extraction layer; if what
  survives is still over the sidecar's separate 256 KiB text cap, the
  sidecar records that with its own `text_truncated` flag, the same
  mechanism every other tier's output goes through. OpenDocument
  spreadsheets and presentations (`.ods`, `.odp`) are not read by any tier —
  only `.odt` is, via textutil.
- **`tags`** — reads and writes Finder tags (the `com.apple.metadata:_kMDItemUserTags`
  extended attribute) and enforces the controlled vocabulary.
- **`sidecar`** — reads/writes the `.<file>.meta.yaml` companion files.
- **`move`** — the only code that relocates a file: propose → manifest →
  SHA256-verified copy → tag/sidecar carry-over → stage the source (never
  delete it). `Rollback` reverses a manifest.
- **`audit`** — the append-only JSONL log every mutation and confidential
  resolution writes to.
- **`keychain`** — **not wired up yet.** It is the intended home for
  recording *which* Keychain item unlocks an encrypted document (never the
  password value), but nothing imports it today and Kagaz has no
  encrypted-document handling at all.
- **`ingest`** — OCR → classify → extract → propose → (on approval) execute,
  the pipeline behind `kagaz ingest`.
- **`search`** — the walk-and-filter engine behind `kagaz find`, with an
  optional `mdfind` (Spotlight) accelerator and iCloud-eviction handling.
- **`lint`** — convention checks with a `--fix` limited to provably safe
  repairs, every one of them routed through `move.Engine`.
- **`index`** — regenerates `INDEX.md` and `AGENTS.md` from the tree and
  `vault.yaml`.
- **`models`** — downloads and verifies MLX model weights for `kagaz model
  pull`, the only code in the entire codebase permitted to reach the network.

## Propose → preview → approve → execute-with-manifest

No Kagaz command silently changes a vault. Every mutating operation:

1. **Proposes** a plan (a set of moves, tag changes, or both) without
   touching anything.
2. **Previews** that plan to the caller — as formatted text for a human, or
   as structured JSON for an agent.
3. Requires **approval** — explicit confirmation, or `--yes`/`--accept-proposal`
   on the command line, or an equivalent MCP tool argument. `--propose-only`
   stops here on purpose, always exiting 0 without mutating anything.
4. **Executes** by writing a manifest (recording every planned move and its
   verified SHA256) *before* the first byte is touched, so a crash mid-batch
   leaves a resumable, reviewable record on disk.

`kagaz rollback <manifest>` reverses any manifest by moving files from their
post-operation path back to their original path — skipped, not failed, when
a row's current path is already gone or its original path is occupied again,
which is what makes rollback safe to run twice.

## Never delete

Kagaz never calls `os.Remove`/`os.RemoveAll` on a document you own. A
"move" is a byte-for-byte copy to the destination, a SHA256 verification of
that copy, and then a *rename* of the source into
`_To-Delete-After-Verification/<timestamp>/` inside the vault. Kagaz never
empties that folder — you do, from Finder, once you trust what's in it. The
one exception is the staging rename's own cross-device fallback: if a rename
isn't possible (a network mount, iCloud Drive), Kagaz copies to staging,
verifies the copy, and *then* removes the original — which is the same
verified-copy-before-delete discipline, just implemented with a copy instead
of a rename underneath it.

## No network at inference or query time

The only network call anywhere in the codebase is `kagaz model pull`, which
downloads MLX weights from a pinned Hugging Face repo/revision — explicit,
opt-in, never triggered implicitly by `ingest`, `find`, or anything else. Any
HTTP client that talks to Ollama (`classify.ollama`, `ocr.Ollama`) hard-fails
on a non-localhost endpoint, and re-checks this at call time rather than
trusting `vault.yaml` alone, because a config file is something an attacker
(or a careless edit) can change.

## Why a copy-then-stage move, not a rename

A plain `os.Rename` can silently invalidate the digital signature embedded in
some encrypted or signed PDFs, and behaves inconsistently across FUSE mounts
and iCloud Drive (which may need to materialize a file before it can be
moved at all). Copying, verifying by hash, and only then retiring the
original sidesteps both problems at the cost of a slightly slower move — a
trade Kagaz always takes, because global constraint 3 ("never delete a user
document") is non-negotiable and a corrupted rename is indistinguishable
from data loss until someone notices.

## Graceful degradation

`pdftotext`, `kagaz-machelper`, `mdfind`, `brctl`, `ollama`, and xattr
support are all optional. Their absence narrows what Kagaz can do — no
Vision OCR without the helper, no Spotlight acceleration without `mdfind` —
but never crashes a command. `kagaz doctor` reports exactly what's missing
and what that costs you. This is also why Kagaz's Go test suite runs clean
on Linux CI, where none of the above exist: every external-tool code path is
tested against a recorded fixture, never by invoking the real tool.
