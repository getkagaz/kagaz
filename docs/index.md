---
title: Kagaz documentation
---

# Kagaz documentation

Kagaz is a local-first, global document vault manager for macOS: your
documents stay as ordinary files in ordinary folders, and Kagaz adds
convention, Finder tags and searchable facts on top. Start with the
[project README](https://github.com/getkagaz/kagaz#readme) if you haven't
yet; this site is the reference documentation.

- **[Installation](installation.md)** — requirements, Homebrew, building
  from source.
- **[Conventions guide](conventions-guide.md)** — the filename grammar,
  folder layout, fiscal years, tags, sidecars and doctypes.
- **[Configuration reference](configuration.md)** — every `vault.yaml`
  field, with [`vault.schema.json`](vault.schema.json) as the
  machine-readable counterpart and [`examples/vault.yaml`](https://github.com/getkagaz/kagaz/blob/main/examples/vault.yaml)
  as a fully commented starting point.
- **[Command reference](commands.md)** — every `kagaz` subcommand and its
  `--json` behavior.
- **[Architecture](architecture.md)** — the one-mutator rule,
  propose/preview/approve/execute, why moves never delete.
- **[Using Kagaz as an agent](agents.md)** — the MCP server, `--json`
  contracts, and the invariants that hold no matter how you connect.
- **[Model use and licensing](model-use.md)** — the classifier tiers,
  the only network call in the codebase, and model licensing.
- **[kagaz-machelper JSON contract](machelper-contract.md)** — the
  versioned Go↔Swift boundary.
- **[Homebrew Core readiness](HOMEBREW_CORE.md)** — what's done and what
  isn't, honestly.

Kagaz is pre-1.0 and not yet released. See the README for current status.
