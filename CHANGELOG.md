# Changelog

Azud follows [Semantic Versioning](https://semver.org/). Release entries are
generated from the changes accumulated under “Unreleased” and the GitHub
release notes for the corresponding signed tag.

## Unreleased

- Added stable Caddy route ownership IDs and explicit proxy reconciliation.
- Added configurable HTTP, h2c, and HTTPS application upstream transports.
- Added command-based readiness probes for gRPC, TCP, and custom checks.
- Fixed proxy listener separation so `ssl_redirect: true` delegates port 80
  redirects to Caddy and HTTP-only services never serve plaintext on port 443.

## 1.0.0 - 2026-07-20

- Hardened zero-downtime deploy, rollback, scale, canary, proxy, secrets, and
  SSH failure handling for the 1.0 release line.
- Added strict configuration validation, durable local state, role-aware
  workloads, reproducible Quadlet units, and release provenance.

Earlier `v0.0.x-dev` builds were development previews and did not carry a
stable compatibility promise.
