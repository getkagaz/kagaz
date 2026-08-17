---
title: Configuration reference
---

# Configuration reference

`vault.yaml` is the single authored source of truth for a vault. Nothing in
Kagaz hardcodes folder names, filename grammar, fiscal-year math, tag
vocabulary or the doctype catalog — all of it resolves from this file. This
page documents every field parsed by `internal/vaultkit/config`; the
machine-readable version is [`vault.schema.json`](vault.schema.json), and a
fully commented starting point is [`examples/vault.yaml`](../examples/vault.yaml).

`kagaz init` writes a minimal `vault.yaml`; every field below has a sensible
default, so a new vault needs only `people:` to be useful.

## `version`

Highest schema version this file was written for. Defaults to `1`. A
`vault.yaml` whose `version` is newer than the running `kagaz` build
understands fails to load with a message telling you to upgrade.

## `vault_root`

Root directory of the vault. Defaults to `~/Documents`. A leading `~`
expands to your home directory. A **relative** path resolves against the
directory holding `vault.yaml` itself, so a vault stays portable if you move
it (or its containing folder) wholesale — including into and out of iCloud
Drive.

## `fiscal_year`

| Field | Default | Meaning |
|---|---|---|
| `start_month` | `1` | 1–12. `1`=calendar year, `4`=India/Japan/UK-ish, `7`=Australia, `10`=US federal. |
| `label_format` | `"FY {yyyy1}"` (start_month 1) or `"FY {yy1}-{yy2}"` (otherwise) | Must contain a `{...}` placeholder: `{yyyy1}`, `{yyyy2}`, `{yy1}`, `{yy2}`. |

## `people`

A list of `{name, tag}`. `tag` defaults to a lowercase slug of `name` (e.g.
`"Alex Rao"` → `alex-rao`) and must be unique across the vault — two people
sharing a tag is a config error.

## `owner_groups`

| Field | Default | Meaning |
|---|---|---|
| `separator_folder` | `"+"` | Joins multiple owners in a folder path: `Alex+Sam`. |
| `separator_filename` | `"+"` | Joins multiple owners in a filename field: `Alex+Sam`. It must differ from `filename.word_separator`, or a filename stops being invertible: with both set to `-`, `Alex-Rao` reads equally well as one person or as two. |
| `order` | `"alphabetical"` | Only this value changes ordering behavior today. |

## `filename`

| Field | Default | Meaning |
|---|---|---|
| `pattern` | `"{DocType}_{Names}_{Identifier}[_{Year}][_{Modifier}]"` | Must contain `{DocType}`; required fields precede optional ones. |
| `word_separator` | `"-"` | Must differ from `field_separator`. |
| `field_separator` | `"_"` | Must differ from `word_separator`. |

## `structure`

A map from category name to:

| Field | Default | Meaning |
|---|---|---|
| `path` | title-cased category name | Relative folder beneath `vault_root`; no `..`. |
| `shared` | `_Shared` in the default structure; *(unset)* for a category you define yourself | Folder used instead of `{Owner}` for multi-owner or unowned documents. With no `shared`, Kagaz refuses to file an unowned document in the category rather than inventing an owner for it. |
| `layout` | `"{Owner}/{FY}"` for `financial`, `company`, `utility`; `"{Owner}"` otherwise | Slash-separated template of `{Owner}` and `{FY}` segments describing the subtree under `path`. |

Omitting `structure` entirely uses the global-first default: `personal`,
`company`, `financial`, `travel`, `identity`, `insurance`, `medical`,
`legal`, `property`, `vehicles`, `utility` — every one of them with
`shared: _Shared`, so a document with no owner to infer (a third party's
certificate, an incorporation document) can still be filed. `kagaz init`
writes this block into vault.yaml explicitly, so it is visible and editable.

`layout` is what decides whether a category accumulates one folder per
fiscal year (documents that recur every period, like utility bills or
invoices) or stays flat per owner (documents that don't, like a passport). A
vault that wants, say, medical records split by year as well just sets
`structure.medical.layout: "{Owner}/{FY}"`.

## `tags`

| Field | Default | Meaning |
|---|---|---|
| `companies` | `[]` | Free-form organisation tags. |
| `areas` | `[]` | Free-form subject-area tags. |
| `fiscal_years` | `[]` | Free-form fiscal-year tags. |
| `lifecycle` | `active, superseded, encrypted, confidential, to-action, dont-touch` | The controlled lifecycle-state vocabulary. |

Every configured person's `tag` is also implicitly part of the controlled
vocabulary. Anything outside all of these is a `kagaz lint` finding.

## `ocr`

| Field | Default | Meaning |
|---|---|---|
| `vision_languages` | `["en-US"]` | Language hints for Apple Vision OCR. |
| `ollama.enabled` | `"auto"` | `"auto"` \| `"true"` \| `"false"`. |
| `ollama.model` | *(unset)* | Ollama vision-model name, e.g. `unlimited-ocr`. |
| `ollama.endpoint` | `"http://localhost:11434"` | Must resolve to localhost; enforced at parse time and re-checked at call time. |

## `classify`

| Field | Default | Meaning |
|---|---|---|
| `engine` | `"auto"` | `auto` \| `apple` \| `mlx` \| `ollama` \| `rules`. |
| `model` | `"mlx-community/Qwen2.5-3B-Instruct-4bit"` | Model id for `mlx`/`ollama`. |
| `endpoint` | `"http://localhost:11434"` | Must resolve to localhost. |
| `min_confidence` | `0.5` | 0–1. Below this, Kagaz degrades to `rules`, then to `unclassified`. |

`auto` tries the Apple Foundation Models tier (when the OS and device
support it) and falls back to `rules`. A named engine that is unavailable is
an error naming the fix (e.g. `run kagaz model pull --engine mlx`) — except
that a *runtime* classifier failure, or a result under `min_confidence`,
always falls back to `rules` rather than failing the whole `ingest`.

## `encrypted_docs` — **not implemented**

**Nothing reads these settings yet.** They are parsed and validated so that
a `vault.yaml` written today stays valid, but Kagaz has no
encrypted-document handling: setting them changes no behaviour whatsoever.

| Field | Default | Meaning (planned) |
|---|---|---|
| `keep_encrypted` | `false` | Leave password-protected documents encrypted in place. |
| `password_store` | `"keychain"` | Where a document's password would be looked up. `"keychain"` is the only value the schema accepts, and no store is implemented. |

The design rule, for when it is built: passwords never appear anywhere in
the vault — not in a filename, sidecar, `INDEX.md`, manifest, or log. Only
the Keychain **item name** would be recorded.

## `lint`

| Field | Default | Meaning |
|---|---|---|
| `require_lifecycle_tag` | `false` | Every document must carry a lifecycle tag. |
| `single_active_per_doctype_per_person` | `[]` | Doctype names allowing only one `active` document per owner. |
| `forbid_passwords_in_filenames` | `false` | Flags password-looking tokens in filenames. |

## `confidential`

| Field | Effective value | Meaning |
|---|---|---|
| `require_confirmation_on_resolve_for_send` | optional; **omitted means required** | Gates `kagaz resolve --for-send`. |
| `audit_log` | `"vault.log"` | Relative to `vault_root` unless absolute. |

`require_confirmation_on_resolve_for_send` is a genuine tri-state, not a
bool with a default: the key is optional, and **omitting it means
confirmation is required** (fails closed). Setting it explicitly to
`false` in `vault.yaml` genuinely disables the confirmation prompt for
`resolve --for-send` — Kagaz honours an explicit choice either way; it does
not silently override `false` back to `true`. Only the *absence* of the
key is fail-closed, not the value itself. See
[commands.md](commands.md#resolve---for-send) for exactly what the gate
does.

## `doctypes`

A list of `{name, category, match: {keywords, patterns}, extract}` entries
that extend or override the built-in catalog. `category` must already exist
in `structure`. See
[CONTRIBUTING.md](../CONTRIBUTING.md#adding-a-doctype) for how to add one.
