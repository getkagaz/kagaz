# fixture-vault

A small, deterministic, hand-inspectable vault used by golden tests for
`internal/vaultkit/search`, `internal/vaultkit/lint` and
`internal/vaultkit/index`. Content is plain `.txt`, never a binary PDF, so
the whole fixture stays diffable in a code review. Finder tags are
deliberately **not** set on any file here — extended attributes cannot be
committed to git, so tag-dependent behavior (vocabulary checks, lifecycle
checks) is covered by unit tests that apply tags to a temp copy of this tree
at test time, not by anything baked into these files.

Two owners (`Alex Rao`, `Sam Rao`), three categories (`financial`, `travel`,
`identity`), and `vault_root: .` so the fixture is portable regardless of
where it's checked out. `vault.yaml` here is intentionally smaller than
[`examples/vault.yaml`](../../examples/vault.yaml) — only the three
categories actually used are declared, `classify.engine` is pinned to
`rules` (no external classifier needed to exercise this fixture), and
`ocr.ollama.enabled` is `"false"`.

## What's here, and why

| Path | Exercises |
|---|---|
| `Financial/Alex Rao/FY 2026/Invoice_Alex-Rao_Acme-Corp_2026.txt` | A correctly named, correctly placed document. `{Owner}/{FY}` layout for `financial`. Doctype `invoice`. |
| `Financial/Alex Rao/FY 2026/.Invoice_Alex-Rao_Acme-Corp_2026.txt.meta.yaml` | A **correct** sidecar: `source_sha256` matches the document's actual content. The happy-path sidecar-read case. |
| `Financial/Sam Rao/FY 2025/Receipt_Sam-Rao_Globex_2025.txt` | A second correctly named financial document, different owner and fiscal year, doctype `receipt`. |
| `Financial/Sam Rao/FY 2025/.Receipt_Sam-Rao_Globex_2025.txt.meta.yaml` | A **deliberately stale** sidecar: its `source_sha256` does not match the document. Exercises the lint "stale sidecar" rule. Do not "fix" this file to match — that removes the fixture's purpose. |
| `Travel/Sam Rao/Boarding-Pass_Sam-Rao_United-Airlines.txt` | A correctly named document with **no sidecar at all** — the common case (most files in a real vault have none) and a check that its absence is not itself a lint finding. `{Owner}` layout for `travel`. |
| `Travel/Sam Rao/.Ticket_Sam-Rao_Delta-Airlines.txt.meta.yaml` | A **deliberately orphaned** sidecar: there is no `Ticket_Sam-Rao_Delta-Airlines.txt` in this directory. Exercises the lint "orphan sidecar" rule. Do not add the matching document. |
| `Identity/Alex Rao/Passport_Alex-Rao_Passport-Office_2024.txt` | A correctly named document in a category with a configured `shared` folder (`_Shared`) that this particular document does *not* use, because it has exactly one owner. Doctype `passport`, which is also in `lint.single_active_per_doctype_per_person` in this vault's `vault.yaml`. |
| `Financial/Alex Rao/FY 2026/old invoice notes.txt` | A **deliberately convention-violating** filename: it does not fit the `{DocType}_{Names}_{Identifier}[_{Year}][_{Modifier}]` grammar at all (spaces, no recognizable fields). Exercises the lint "filename does not match the grammar" rule. It is intentionally **not** a safe `--fix` target — there are no recoverable facts in this filename to rename it correctly from, so a lint fixture needs at least one violation that must stay a permanent finding rather than be auto-repaired. |

## What golden tests should expect

- `kagaz find` (no filters) over this vault returns the three correctly
  named documents plus the one intentionally malformed file (search walks
  the tree; it doesn't require grammar conformance to find a file, only to
  parse facts out of its name).
- `kagaz lint` over this vault is **not** clean: it reports exactly three
  findings — the malformed filename, the stale sidecar, and the orphan
  sidecar — and nothing else. If a future change to the lint rule set adds
  a new finding against one of the *correct* files above, that's a
  regression, not a fixture bug.
- `kagaz index` regenerating `INDEX.md`/`AGENTS.md` against this tree must
  be byte-stable across runs (no timestamps in the generated body, output
  sorted deterministically) — that determinism is what makes this fixture
  usable for golden-file comparison in the first place.

## Adding to this fixture

Keep it small. If a new test needs another scenario, prefer adding one more
narrowly-scoped file over growing an existing one, and update the table
above in the same change.
