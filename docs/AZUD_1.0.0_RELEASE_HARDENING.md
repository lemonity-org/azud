# Azud 1.0.0 release hardening

Remediation date: 2026-07-19  
Audit baseline: `73541d3`  
Implementation status: **all 31 findings remediated in the working tree**  
Release status: **CONDITIONAL GO — do not tag until the exact-commit live matrix is green**

This is the closure record for [the 1.0.0 audit](AZUD_1.0.0_RELEASE_AUDIT.md).
It separates implementation completion from exact-commit release verification.
Local checks cover deterministic logic, race safety, schema behavior, release
assets, and security scanning. A disposable CentOS Stream target also exercised
real Podman, systemd, rootless user namespaces, and SSH in all three supported
topologies. The same matrix remains mandatory in CI because local working-tree
results cannot substitute for checks on the exact release commit.

## Closure map

| Finding | Status | Remediation and evidence |
|---|---|---|
| AZ-001 | Fixed; live-gated | Caddy listens on container-reachable admin addresses while host publication remains loopback-only; boot and restore fail closed. Covered by proxy unit tests and first-deploy/mixed-mode integration. |
| AZ-002 | Fixed | `init` emits a valid non-TLS default, an idempotent secrets ignore rule, and matching environment-secret CI. Covered by init integration and release scaffold smoke tests. |
| AZ-003 | Fixed; release-gated | Stable releases advance `v1` only after verification; generated CI uses the environment provider and durable state cache. Release and generated-workflow smoke gates cover the contract. |
| AZ-004 | Fixed; live-gated | Role identity now controls names, commands, labels, environment, resource limits, routing, canary, scale, rollback, and Quadlet units. Covered by role unit tests and web/worker integration. |
| AZ-005 | Fixed | Role and accessory options are restricted to typed memory/CPU limits and every emitted shell argument is quoted. Injection and allowlist regression cases are unit-tested. |
| AZ-006 | Fixed | The fleet scheduler stops after the first failure when rollback is enabled and rolls back every completed target, reporting rollback failures. A three-target regression test enforces the invariant. |
| AZ-007 | Fixed; live-gated | Deploy now preserves the old container and route until the new route is confirmed, and aggregates cleanup/restore errors. CI injects a live Caddy admin failure and requires the old HTTP route to remain healthy. |
| AZ-008 | Fixed; live-gated | Scale enumerates exact managed service/role/index labels, fills free indices, treats readiness and proxy failures as fatal, and aggregates host errors. The live matrix scales 1→2→1. |
| AZ-009 | Fixed; live-gated | Disabled canary use fails, both weights are verified, state changes are durable, partial changes clean up, and route failures block container removal. Unit state tests plus deploy/weight/rollback integration cover the lifecycle. |
| AZ-010 | Fixed | `AZUD_STATE_DIR` is an explicit durable source, generated CI persists it, writes are atomic and unique, and local locks serialize changes. Concurrent history and clean-workspace canary tests cover durability. |
| AZ-011 | Fixed; live-gated | Readiness is the exclusive admission gate when configured; liveness remains an independent hard-failure signal. The integration image must pass readiness before Caddy registration. |
| AZ-012 | Fixed; live-gated | Preflight, setup, linger, registry, proxy, accessory, and trust failures now aggregate to non-zero exits. The matrix checks successful setup and injected SSH/registry/preflight failures. |
| AZ-013 | Fixed | Proxy lifecycle and managed-setting transitions fail closed and live mutations are transactional with persisted state. Unit tests cover clearing disabled state; integration covers reboot and failed mutation. |
| AZ-014 | Fixed | Proxy mutations locate the `reverse_proxy` handler by type, preserve sibling handlers, and add upstreams idempotently. Focused route-shape tests cover request-body handlers and retries. |
| AZ-015 | Fixed; live-gated | Per-role Quadlets preserve command/env/options/labels, use systemd-safe paths and encoding, and start Caddy with its protected persisted JSON. Rootless units avoid system-only targets, and mixed-mode units preserve the routed host port across restart. Unit generation tests and cold-start integration cover the path. |
| AZ-016 | Fixed; live-gated | Remote secret paths are validated and safely expanded for the SSH user; upload uses a private temporary file, atomic rename, and verified modes. Path tests and live mode/env assertions cover it. |
| AZ-017 | Fixed | Persisted Caddy state uses a 0700 directory and 0600 file in root and non-root modes, repairs existing permissions, and does not log contents. Command-generation tests enforce this. |
| AZ-018 | Fixed | Image references are normalized across tags, digests, and registry ports; explicit `deploy --version` skips unrelated builds; generated tags are OCI-validated. Table tests cover the reference forms. |
| AZ-019 | Fixed | Remote context matching handles nested slashless patterns, negation, safe symlinks, escaping-symlink rejection, and pipe cancellation. Archive conformance tests cover each case. |
| AZ-020 | Fixed; live-gated | Cron validates its runtime tools inside the image, shares the same shell/timeout/secret/lock contract for scheduled and manual runs, resolves rootless paths, and aggregates all failures. Unit and lifecycle integration cover it. |
| AZ-021 | Fixed; live-gated | Named accessory operations target only that accessory, support explicit multi-host selection, directories, files, typed options, and quoted commands; unsupported role scoping is rejected. Unit and live stop/boot tests cover targeting. |
| AZ-022 | Fixed; live-gated | SSH exposes bounded streaming plus interactive PTY I/O with resize propagation, timeouts, cancellation, and remote exit codes. The matrix exercises non-interactive, PTY, failure, and pre-exit log streaming paths. |
| AZ-023 | Fixed | YAML is strict with full paths, line numbers, and typo suggestions; minimum versions and aliases execute; dead fields are rejected or consumed; destination merging honors explicit false/zero/clear values. Schema and merge tests cover this. |
| AZ-024 | Fixed | SSH dials and commands honor context deadlines; connection singleflight is per host; sessions have an explicit concurrency bound instead of whole-host serialization. Cancellation and parallel-host tests run under the race detector. |
| AZ-025 | Fixed; release-gated | Third-party actions and runtime/base images use immutable SHAs/digests, job permissions are minimal, Dependabot manages updates, and binary/container attestations are published and verified before install. |
| AZ-026 | Fixed | Registry passwords are supplied only through `--password-stdin`; action smoke tests inspect the invoked arguments and reject credential exposure. |
| AZ-027 | Fixed | Obsolete targets are removed, all supported cross-builds fail fast, installer output/version parsing is smoke-tested, and the repository includes the MIT license, changelog, and release policy. |
| AZ-028 | Fixed | Quick-start YAML nesting, TLS requirements, stable artifact references, and CI claims are corrected. Documentation config blocks are extracted and loaded by tests; the release scaffold is smoke-tested. |
| AZ-029 | Fixed; live-gated | CI now has rootful bridge, rootless bridge, and mixed rootless-app/rootful-proxy jobs covering deploy/update/rollback, roles, scale, canary, cron, accessories, proxy failure/reboot, SSH/registry failure, secrets, streaming/PTY, and systemd cold start. Release verification requires those exact-commit checks. |
| AZ-030 | Fixed | Digest lookup and cross-host mismatches fail closed; bypass requires an explicit pull skip and is recorded. Unit tests cover lookup failure, mismatch, pinned consistency, and bypass metadata. |
| AZ-031 | Fixed | Host, role, accessory, cron, scale, history-error, and generated-output ordering is defined and sorted while preserving configured host order. Repeatability tests enforce stable output. |

## Local verification evidence

The final working tree passed these gates on 2026-07-19:

- `gofmt -l .` returned no files; `go test ./...`, `go test -race ./...`,
  `go vet ./...`, `go mod verify`, and `golangci-lint run` all passed.
- `scripts/security-lint.sh` reported no reachable or imported-package
  vulnerabilities and no security-lint errors. One vulnerability exists in a
  required module but is not called by Azud.
- Release installer/registry-login smoke passed, all repository workflow YAML
  parsed, and `make release` built Darwin amd64/arm64, Linux amd64/arm64, and
  Windows amd64.
- The produced Darwin arm64 release binary initialized a fresh GitHub Actions
  project, emitted private secrets and valid YAML, and reloaded its own config.
- Full live integration passed for `rootful-bridge`, `rootless-bridge`, and
  `mixed-rootless-app-rootful-proxy`. Each run covered first setup, secrets,
  web/worker roles, update/rollback, scale, canary, cron, accessory stop/boot,
  streaming and PTY SSH, injected proxy/registry/SSH failures, proxy reboot,
  and a systemd/Quadlet cold start with HTTP reachability.

The live target was disposable and local; these results do not change the
requirement that GitHub report the same three checks as successful for the exact
candidate SHA.

## Required verification

The following local gates must all pass from a clean worktree candidate:

```sh
go mod verify
go test ./...
go test -race ./...
go vet ./...
golangci-lint run
./scripts/security-lint.sh
sh scripts/release_smoke_test.sh
make release
```

The exact commit must then pass these GitHub checks before a stable tag is
allowed:

- `Go test & lint`
- `Integration (rootful-bridge)`
- `Integration (rootless-bridge)`
- `Integration (mixed-rootless-app-rootful-proxy)`

The release workflow queries the checks API for the tagged SHA and refuses to
publish when any required check is absent or unsuccessful. Stable `latest` and
`v1` aliases move only after the versioned container, its attestation, the
release binaries, checksums, and binary attestations all succeed. Pre-releases
never move stable aliases.

## Residual operational assumptions

- Rootless hosts must provide user namespaces, `slirp4netns`, `fuse-overlayfs`,
  and a working user systemd session; bootstrap installs the known distribution
  packages but cannot repair a host kernel or policy that disables them.
- A non-root SSH user needs narrowly scoped passwordless privilege for rootful
  Podman/proxy and system service operations. Commands use `sudo -n` so missing
  authority fails instead of prompting or hanging.
- `AZUD_STATE_DIR` must be durable across CI jobs. The generated GitHub workflow
  uses the Actions cache, while production operators may mount a persistent
  directory instead.
- Mutable application tags are allowed only because Azud resolves and compares
  their digest on every target. `--skip-pull` is an explicit, logged decision to
  skip that verification.

No stable 1.0.0 release should be called production-ready solely from this
source review. The conditional verdict becomes a full GO only when the live
matrix and release workflow succeed for the exact candidate commit.
