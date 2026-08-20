---
title: The JSON envelope contract
---

# The `--json` envelope contract

Every `kagaz` command supports `--json`, and every `--json` response is a single
JSON object in the shape defined here. This is a **stable contract**: scripts,
the MCP server and the Kagaz for Mac app all decode it, and unlike the rest of
this repository they are not all updated in the same commit.

Source of truth: `internal/cli/output.go` — `SchemaVersion`, the `Status*`
constants, the `Exit*` constants, and `Envelope()`. Where this document and that
file disagree, the file is right and this document is a bug.

> **Why this document exists.** Until the open-core split the envelope was an
> internal convention between two directories of one repository, and a change to
> the Go side and its Swift decoder landed in a single CI-verified commit. That
> is no longer true: the app ships from a separate repository on its own
> release cadence, so an arbitrary CLI version can meet an arbitrary app
> version in the wild. The envelope is now an interface between products, and
> needs the same treatment as
> [the machelper contract](machelper-contract.md).

## The envelope

```
$ kagaz doctypes --json
{
  "command": "doctypes",
  "counts": { "total": 45, "vault": 1, "built_in": 44 },
  "doctypes": [ … ],
  "schema_version": 1,
  "status": "ok"
}
```

Three keys are always present, and a fourth appears when it applies:

| Key | Type | Always? | Meaning |
|---|---|---|---|
| `command` | string | yes | The command that produced this, e.g. `"find"`. |
| `status` | string | yes | One of the statuses below. |
| `schema_version` | integer | yes | The version of *this* contract. Currently `1`. |
| `warnings` | array of strings | when non-empty | Non-fatal problems. Also written to stderr for a human. |

**The payload is flattened into the envelope, not nested under a wrapper key.**
`doctypes` contributes `counts` and `doctypes` at the top level; `find`
contributes `count`, `query` and `results`. This is deliberate — a documented
shape appears exactly as documented — and it has one consequence a client must
respect: **`command`, `status`, `schema_version` and `warnings` are reserved.**
A payload that tries to use one is a build-time bug in the CLI, not a runtime
possibility; `Envelope()` refuses to marshal it.

A payload that does not marshal to a JSON object (an array, say) appears under a
`data` key instead. No current command does this.

**Keys are sorted and the output is stably indented**, so two runs over the same
data are byte-identical. A client may diff envelopes; it must not depend on key
*order* beyond that guarantee, because sorted order changes when a field is
added.

## Statuses

| `status` | Meaning |
|---|---|
| `ok` | The command did what it was asked to do. |
| `proposed` | A mutating command stopped at the proposal — `--propose-only`, or confirmation declined. Nothing was changed. |
| `confirmation_required` | A gated operation needs explicit consent (`resolve --for-send` without `--confirm`). |
| `findings` | A read-only check completed and found problems. |
| `error` | The command failed. The payload carries `message`, and `hint` when there is one. |

`status` and the exit code are related but not interchangeable — see below.

## Exit codes

Documented in full in [commands.md](commands.md#exit-codes) and repeated here
because a decoder usually needs both halves:

| Code | Constant | Meaning |
|---|---|---|
| `0` | `ExitOK` | Success. |
| `1` | `ExitFailure` | Runtime failure — no vault, missing path, tag outside the vocabulary. |
| `2` | `ExitUsage` | Bad invocation — unknown flag, missing argument. |
| `3` | `ExitConfirmationRequired` | Refused to act without consent. |
| `4` | `ExitFindings` | `lint` completed with **error**-severity findings. |

**Branch on the exit code, not on `status`**, for the "did it work" question.
The distinction between `1` and `3` is the one that matters: `1` means the
command could not be carried out, `3` means it could and deliberately was not,
pending a human. A client that treats every non-zero exit as failure will report
a working confidential gate as a broken command.

## Compatibility

This is the rule that makes the contract worth having:

- **`schema_version` changes only when an existing field changes meaning or
  disappears.** A rename counts. A repurpose counts.
- **Adding a field is not a version bump.** Neither is adding a new command, a
  new `status` value that existing clients can ignore, or a new exit code.
- **A client must ignore fields it does not recognise.** This is what makes the
  additive rule safe, and it is not optional.
- **A client must reject a `schema_version` it was not built against** rather
  than guess at the shape — the same rule the machelper contract states for its
  `contract` field. Guessing produces a plausible wrong answer, which is worse
  than a clear refusal in a tool that files your passport.

### Version skew is now the normal case

The CLI and the app are installed separately (`brew install kagaz` and the app's
own download) and update independently, so a client will meet both older and
newer CLIs in the field. Handle both directions explicitly:

- **CLI older than the client expects** — the client is asking for a field that
  does not exist yet. Say which version is installed, which is needed, and that
  `brew upgrade kagaz` fixes it.
- **CLI newer than the client understands** — a `schema_version` above what the
  client was built against. Refuse, and say the app needs updating.

Detection is not compatibility. A client that detects skew and then renders a
blank screen has met this contract's letter and failed its purpose.

## What is *not* covered

- **Human (non-`--json`) output.** It is for people to read, and it changes
  whenever a clearer sentence exists. Never parse it; that is what `--json` is
  for.
- **`kagaz --version`,** which prints a bare version string, not an envelope.
- **The MCP surface**, which returns each command's envelope verbatim (see
  `internal/cli/mcp.go`) but wraps it in MCP's own protocol framing.
- **Payload shapes per command.** Those are documented per command in
  [commands.md](commands.md). This document defines the container only.

## Changing the envelope

1. Decide whether it is additive. If it is, no version bump — just add it.
2. If it is breaking, bump `SchemaVersion` in `internal/cli/output.go`.
3. `TestEnvelopeContract` in `internal/cli/envelope_contract_test.go` will fail
   deliberately. It pins the version as a **literal**, not against the constant,
   so a bump cannot pass unnoticed. Read its failure message.
4. Update this document, and the golden fixture in
   `internal/cli/testdata/envelope/`.
5. Ship the client update *before or with* the CLI release, and make sure the
   skew message names the fix.
