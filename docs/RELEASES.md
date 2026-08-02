# Release process

Stable releases are created only from reviewed, verified signed annotated
`vMAJOR.MINOR.PATCH` tags whose commits are already on `main`. The release
workflow builds all supported binaries with embedded version metadata,
generates SHA-256 checksums, publishes GitHub/Sigstore SLSA provenance, and
attests the multi-platform container image. It uploads and verifies the exact
asset inventory in a draft before publishing an immutable GitHub release. A
successful stable release then advances the movable `v1` action tag.
Pre-release tags never advance `v1` or `latest`.

The installer verifies both the checksum and the GitHub artifact attestation.
This makes the checksum useful for transport integrity while the Sigstore-backed
attestation supplies an independent identity and provenance check.

Third-party GitHub Actions, build images, Caddy, and the readiness helper are
pinned to immutable commit or OCI digests. Dependabot proposes Actions, Go, and
Dockerfile updates weekly. Runtime constants such as Caddy and the helper image
are reviewed manually; their tag and digest must move together. Every update
must pass the test, security, integration, cross-build, and installer smoke
gates before its pin is changed.

Before tagging a stable release:

1. Move user-visible entries from `Unreleased` in `CHANGELOG.md` into the new
   version section.
2. Run `make test`, `make lint`, `make security-lint`, and `make release`.
3. Run the rootful/rootless integration matrix and the generated-project smoke.
4. Verify release immutability is enabled with an administrator credential.
   Then have an authorized release maintainer create and push a verified
   signed annotated semantic-version tag. Require every verify, container,
   binary-release, and stable-alias job to finish successfully.
5. Require `gh release verify <tag> --repo lemonity-org/azud` to confirm the
   immutable release attestation, then verify a downloaded binary with both
   `gh release verify-asset` and `gh attestation verify` before running its
   `version` and scaffold smokes.
