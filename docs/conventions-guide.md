---
title: Conventions guide
---

# Conventions guide

Kagaz does not invent a database. It enforces a naming and folder convention
strictly enough that the filesystem itself becomes the index — readable by
you in Finder, by Spotlight, and by any AI agent with shell access. This page
is the human-readable version of what `internal/vaultkit/conventions` and
`internal/vaultkit/config` implement; if this page and the Go source
disagree, the source wins.

## Filenames

Every conventional document name is built from fields joined by a
**field separator**, with each field's own words joined by a
**word separator**. The default pattern is:

```
{DocType}_{Names}_{Identifier}[_{Year}][_{Modifier}]
```

- `_` is the default field separator, `-` the default word separator, so a
  filename reads `Invoice_Alex-Rao_Acme-Corp_2026_Final.pdf`.
- Fields in `[...]` are optional. Required fields must all precede the first
  optional field in the pattern.
- `{DocType}` is mandatory in any pattern; it is the one field Kagaz always
  needs to place a document.

Recognized fields:

| Field | Meaning | Example |
|---|---|---|
| `{DocType}` | Catalog document type, title-cased | `Invoice`, `Boarding-Pass` |
| `{Names}` | Owner(s), joined by `owner_groups.separator_filename` | `Alex-Rao`, `Alex-Sam` |
| `{Identifier}` | Issuer, counterparty or subject | `Acme-Corp` |
| `{Year}` | A 4-digit year, when known | `2026` |
| `{Modifier}` | Free qualifier | `Final`, `Renewal` |

Parsing a filename back into facts is deliberately lenient: a name that does
not fit the grammar is reported by `kagaz lint`, not rejected outright — real
vaults accumulate files Kagaz did not create.

## Folders

A document's folder is `<vault_root>/<category path>/<layout>`, where
`<layout>` is the category's `layout` template (see
[configuration.md](configuration.md#structure)) expanded with:

- `{Owner}` — the single owner's folder name, or the category's `shared`
  folder for a document with zero or multiple owners, or nothing when
  neither is configured.
- `{FY}` — the fiscal-year label for the document's year, rendered through
  `fiscal_year.label_format`. Omitted entirely when the document has no year.

The built-in categories default to `{Owner}/{FY}` for **financial, company
and utility** documents (things that recur every fiscal period) and
`{Owner}` for everything else (things that don't, like a passport or a
lease).

## Fiscal years

`fiscal_year.start_month` moves the fiscal-year boundary: `1` is the global
default (calendar year), `4` matches India/Japan/UK-style years, `7`
Australia, `10` the US federal year. `fiscal_year.label_format` controls how
a fiscal year prints, with placeholders `{yyyy1}`/`{yyyy2}` (full years) and
`{yy1}`/`{yy2}` (two-digit years) — `"FY {yyyy1}"` for a calendar-aligned
vault, `"FY {yy1}-{yy2}"` for a split year like `FY 25-26`.

`kagaz find --period` accepts calendar expressions (`2026`, `2026-05`,
`2026-05-11`) and fiscal expressions (`FY2026`, `FY2026-27`, `FY2026Q3`,
`2026Q3`) resolved against this same calendar.

## Tags

Finder tags are the vault's controlled vocabulary, drawn from `tags:` in
`vault.yaml` plus every configured person's tag:

- `tags.companies`, `tags.areas`, `tags.fiscal_years` — open, vault-specific
  lists you define.
- `tags.lifecycle` — defaults to `active`, `superseded`, `encrypted`,
  `confidential`, `to-action`, `dont-touch`.

A tag outside the vocabulary is a `kagaz lint` finding, not a silent
acceptance — an uncontrolled vocabulary is what makes saved `mdfind` searches
stop being reliable. Tags are stored lowercased in the
`com.apple.metadata:_kMDItemUserTags` extended attribute, which is exactly
what Finder itself reads and writes; nothing about a Kagaz-tagged file is
special to any other tool.

## Sidecars

Ingest writes a `.<filename>.meta.yaml` sidecar next to (almost) every
document, holding the extracted text, the OCR/classifier engine used, the
detected doctype/category/confidence, extracted structured fields, and the
document's SHA256 at extraction time. Sidecars exist so that `kagaz find`
never has to re-OCR or re-classify at query time. They are:

- **Disposable.** Delete one and `kagaz ingest --reindex` regenerates it.
- **Diffable YAML**, capped at 256 KiB of stored text so a scanned 400-page
  PDF doesn't bloat the vault.
- **Detectable as stale**: `kagaz lint` flags a sidecar whose `source_sha256`
  no longer matches the document, which happens whenever a file was edited
  or replaced outside Kagaz.

## Document types (doctypes)

The built-in catalog (`internal/vaultkit/doctypes`) covers common document
kinds across financial, identity, travel, insurance, medical, legal,
property, vehicles, company and personal categories — see
[commands.md](commands.md) for how `kagaz find --doctype` uses it. A vault
extends or overrides the catalog with a `doctypes:` block in `vault.yaml`;
see [configuration.md](configuration.md#doctypes) and
[CONTRIBUTING.md](../CONTRIBUTING.md#adding-a-doctype) for how to add one
correctly — upstream to the built-in catalog when the doctype is not
locale-specific, keep it in your own `vault.yaml` when it is.
