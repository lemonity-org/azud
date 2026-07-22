# Contributing to Azud

Thanks for helping improve Azud. Contributions of all sizes are welcome.

## Before you start

- Search existing issues and pull requests before opening a duplicate.
- Open an issue before a large behavioral or architectural change so the
  approach can be agreed before substantial work begins.
- Do not open a public issue for a suspected vulnerability. Follow the
  private reporting process in [`docs/SECURITY.md`](docs/SECURITY.md).

## Development setup

Azud uses the Go version declared in `go.mod`.

```bash
git clone https://github.com/lemonity-org/azud.git
cd azud
go mod download
go test -race ./...
```

Install the tools used by CI:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.8.0
go install golang.org/x/vuln/cmd/govulncheck@v1.6.0
```

Before submitting a pull request, run:

```bash
go fmt ./...
golangci-lint run ./...
go mod verify
go test -race ./...
./scripts/security-lint.sh
sh scripts/release_smoke_test.sh
```

Run the relevant integration tests when changing setup, SSH, Podman, proxy,
or deployment behavior. The full integration matrix also runs in GitHub
Actions.

## Making a change

1. Create a focused branch from the latest `main`.
2. Keep commits and pull requests small enough to review safely.
3. Add or update tests for behavior changes.
4. Update documentation and `CHANGELOG.md` when the user-visible behavior
   changes.
5. Never commit credentials, private keys, production configuration, or
   unredacted logs.
6. Open a pull request and complete its security and testing checklist.

Pull requests require all status checks to pass, all review conversations to
be resolved, and approval from at least one code owner. A new push dismisses a
stale approval. Maintainers use squash or rebase merges to keep `main` linear.

## Review expectations

Reviews consider correctness, tests, compatibility, documentation, and the
security impact of remote command execution, secrets, file permissions,
network exposure, and release integrity. Maintainers may ask for a change to
be split into smaller pull requests when that makes it safer to review.

By participating, you agree to follow the
[`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).
