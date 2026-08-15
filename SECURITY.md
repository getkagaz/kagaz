# Security Policy

A Kagaz vault is likely to hold some of the most sensitive documents you
have — passports, tax filings, insurance policies, medical records, signed
contracts. Please treat any issue that could weaken the guarantees below as
security-sensitive, even if you're not fully sure it qualifies.

## Reporting a vulnerability

**Please do not open a public GitHub issue for a security report.** Instead,
use GitHub's private vulnerability reporting for this repository:

1. Go to the [Security tab](https://github.com/getkagaz/kagaz/security) of
   `github.com/getkagaz/kagaz`.
2. Click **"Report a vulnerability"** to open a private advisory.

This reaches maintainers directly without exposing details (or the fact
that a vulnerability exists) publicly while it's being fixed.

If you believe the issue is urgent and GitHub's reporting flow is somehow
unavailable, open a regular issue asking for a maintainer to reach out
through GitHub's private channel — without any exploit details in the
public issue itself.

## What's in scope

- Anything that could cause Kagaz to delete, corrupt, or silently mutate a
  user's document.
- Anything that could exfiltrate document content, extracted text, or
  metadata off the machine — including a way to make a "localhost-only"
  network check pass for a non-local endpoint.
- Anything that could bypass the confidential-resolution gate
  (`kagaz resolve --for-send`) or suppress its audit log entry.
- Anything that could cause a password or Keychain secret to be written to
  a filename, sidecar, `INDEX.md`, manifest, or log.
- Anything that could cause the classifier to produce a doctype outside the
  vault's resolved catalog, if that miscategorization could route a
  document somewhere unexpected.
- Supply-chain issues in this repository (a compromised dependency, a
  malicious change to `Formula/`, CI/release workflow weaknesses that could
  ship a tampered binary).

## What's likely out of scope

- Issues that require an attacker to already have arbitrary code execution
  on the user's Mac, or root/admin access — at that point, the OS itself is
  compromised and Kagaz's own guarantees are moot.
- Denial-of-service against your own local machine (e.g., a crafted file
  that makes `kagaz lint` slow).
- Vulnerabilities in third-party dependencies that are already publicly
  disclosed and don't have a Kagaz-specific exploitation path — please
  report those upstream, though a note here is still welcome if it affects
  how Kagaz uses the dependency.

## What to expect

This is a young, volunteer-maintained open-source project without a formal
SLA. A maintainer will acknowledge a report as soon as reasonably possible
and work with you on a fix and coordinated disclosure timeline before any
public advisory is published.

## Supported versions

Kagaz is pre-1.0. Until a 1.0 release, only the latest tagged release (or
`main`, if no release exists yet) is supported with security fixes.
