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

## `name`

Optional. A human label for this vault — `Personal & Family KYC`, `RelyWeb
Corporate` — so that several vaults are told apart by what they are for
rather than by where they happen to live.

Omitted (as in every `vault.yaml` written before this field existed), Kagaz
displays the **folder name of `vault_root`** instead. The fallback is
resolved at display time and is never written back into your file, so a
hand-edited `vault.yaml` does not silently grow a `name:` it never had.

The name is **display only**. It appears in `kagaz doctor` (and as
`vault_name` in `kagaz doctor --json`) and as the title of the generated
`INDEX.md` and `AGENTS.md`. It never becomes a folder name, a filename, or
any part of a destination, manifest or staging path — no path-building code
in Kagaz reads it. As a second line of defence it is rejected at load time
if it contains a path separator (`/`, `\`), `..`, or a control character, or
if it is longer than 80 characters.

`kagaz init --name "Personal & Family KYC"` writes it; without `--name`,
`init` leaves a commented example rather than guessing one from the folder.

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
| `ollama.enabled` | `"false"` | `"auto"` \| `"true"` \| `"false"`. |
| `ollama.model` | *(unset)* | Ollama vision-model name, e.g. `unlimited-ocr`. |
| `ollama.endpoint` | `"http://localhost:11434"` | Must resolve to localhost; enforced at parse time and re-checked at call time. |

The Ollama OCR runner is **opt-in, and an omitted `ollama.enabled` is not an
opt-in**. `"auto"` reaches for the runner only after `pdftotext` and Vision
have both failed on a document; `"true"` puts it in rotation; `"false"` — the
default — keeps document images away from the daemon entirely. Choosing to
hand a document to a model is a decision worth writing down, which is the same
reason `confidential.require_confirmation_on_resolve_for_send` fails closed and
`classify.model` refuses to guess a model.

`kagaz doctor` distinguishes the two ways this tier can be unusable: **not
enabled** names the key to set, and **no Ollama server responding at …** means
the vault opted in and the daemon is missing.

> **Upgrading.** `ollama.enabled` used to default to `"auto"`, so a vault that
> named an `ollama.model` but never wrote `enabled:` sent scans Vision could
> not read to the local daemon. Those vaults now skip the Ollama tier. To keep
> the old behaviour, write it down:
>
> ```yaml
> ocr:
>   ollama:
>     enabled: "auto"
> ```

## `classify`

| Field | Default | Meaning |
|---|---|---|
| `engine` | `"apple"` | `apple` \| `mlx` \| `ollama` \| `rules`. |
| `model` | *(unset)* | The `ollama` engine's model name, e.g. `qwen2.5:3b`. Not read by `mlx`. A value containing `/` is rejected as a Hugging Face repo path. |
| `endpoint` | `"http://localhost:11434"` | The `ollama` engine's daemon. Must resolve to localhost. |
| `min_confidence` | `0.5` | 0–1. Below this the tier has declined and the `rules` tier answers. |

`model` and `endpoint` belong to `ollama` only. The `mlx` engine always runs
`mlx-community/Qwen2.5-3B-Instruct-4bit` — the repo `kagaz model pull` fetches,
the one this build pins a revision for and compiled its Metal shaders against —
and there is no key to change it. `classify.model` has no default because a
model you did not choose would be named in `doctor` output and in every
sidecar's provenance; leaving it unset makes the `ollama` tier report itself
unavailable with reason `model_not_configured`. A `vault.yaml` whose
`classify.model` still holds the old shared default (any value containing `/`)
is **rejected** at parse time rather than handed to Ollama, which would 404 on
every document.

### The four engines

| `engine` | What runs |
|---|---|
| `apple` *(default)* | Apple's on-device model, then `rules` |
| `mlx` | the pinned MLX model, then `rules` |
| `ollama` | the `classify.model` Ollama model, then `rules` |
| `rules` | `rules` only — no model is ever run, and no availability probe is taken |

Ending at the deterministic `rules` tier is **part of what each model engine
is**, not a separate setting: there is no "MLX or nothing" mode, and `rules` is
the no-model choice. Each engine runs its own tier and never another one — a
machine with all three installed classifies exactly like one with only the
engine you named.

A tier hands over to `rules` when it is unavailable (`apple` only — see below),
declines with `unclassified`, answers below `min_confidence`, names a doctype
outside the catalog, errors, times out, emits malformed output, or speaks an
unknown contract version. An unmatched `rules` answer is `unclassified` with
zero confidence rather than a guess.

**There is no `auto`.** A `vault.yaml` that still says `engine: auto` is
**rejected** with an error naming the four values, not silently read as
something else.

`apple` is the default because it needs nothing downloaded and nothing running:
where Apple's on-device model is absent (anything before macOS 26) the `rules`
tier answers, so the default works everywhere. `mlx` and `ollama` are installed
on purpose, so naming one that is not available is an **error naming the fix**
(e.g. `run kagaz model pull --engine mlx`) rather than a quiet downgrade —
asking for MLX and getting keyword matching would misreport provenance in every
sidecar written afterwards. A *runtime* failure of an available tier still
falls back to `rules` rather than failing the whole `ingest`.

The catalog's regex **field extraction is unconditional**: it runs over every
accepted answer whichever tier produced it, so `invoice_number`, `amount` and
dates are in the sidecar regardless of engine.

`kagaz doctor` prints the order the chain will actually try (`classify:chain`,
e.g. `apple: apple -> rules`) alongside each tier's readiness and a
machine-readable `reason` for any tier that is not ready — see
[commands.md](commands.md#kagaz-doctor) — so a client shows the order and
decides what to offer without recomputing either.

The accepted answer's tier is recorded in `Result.Engine` and lands in the
sidecar's `classifier` field, so provenance names the tier that actually
answered (`apple`, `mlx:<model>`, `ollama:<model>` or `rules`).

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
