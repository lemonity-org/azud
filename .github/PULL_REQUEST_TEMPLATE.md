## Summary

<!-- What changed, and why? Link related issues with "Closes #123". -->

## Validation

<!-- List the exact checks you ran and their results. -->

- [ ] Tests added or updated where behavior changed
- [ ] `go test -race ./...`
- [ ] `golangci-lint run ./...`
- [ ] `./scripts/security-lint.sh`
- [ ] Documentation and changelog updated where needed

## Security impact

<!-- Consider SSH commands, untrusted input, secrets, permissions, networking,
release integrity, and backward compatibility. -->

- [ ] I reviewed the security impact of this change
- [ ] No credentials, private keys, production data, or unredacted logs are included
- [ ] Breaking or operational risks are described above
