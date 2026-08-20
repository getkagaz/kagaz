## Summary

<!-- What does this change do, and why? Link any related issue. -->

## Which package(s)

<!-- e.g. internal/vaultkit/move, cmd/kagaz, machelper/, docs/ -->

## Checklist

- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...` is clean
- [ ] `go build ./...` succeeds
- [ ] `go test ./...` passes (Linux-safe tests, including on this branch)
- [ ] Tests added or updated for the behavior this PR changes
- [ ] If this PR touches `internal/vaultkit/move`, `audit`, `keychain`, or a
      confidential-resolution path: I've re-read the relevant safety
      invariants in [CONTRIBUTING.md](https://github.com/getkagaz/kagaz/blob/main/CONTRIBUTING.md)
      and this change does not weaken them
- [ ] Docs updated if this changes a `vault.yaml` field, a CLI command, or a
      JSON contract
- [ ] Commits are signed off (`git commit -s`) — see
      [CONTRIBUTING.md](https://github.com/getkagaz/kagaz/blob/main/CONTRIBUTING.md#developer-certificate-of-origin-dco)

## Test plan

<!-- How did you verify this works? Commands run, output observed. -->
