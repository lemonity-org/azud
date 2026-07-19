# Azud 1.0.0 release audit

> This document is the immutable audit baseline for revision `73541d3`. The
> remediation implementation and its release gates are tracked in
> [Azud 1.0.0 release hardening](AZUD_1.0.0_RELEASE_HARDENING.md). The original
> NO-GO verdict below is intentionally preserved rather than rewritten after
> the fact.

Audit date: 2026-07-19  
Audited revision: `73541d3` (`v0.0.6-dev-3-g73541d3`)  
Release verdict: **NO-GO**

This is a delegation backlog, not a patch set. Each finding is intended to be independently assignable. P0 items block a stable 1.0.0 release; P1 items are high-impact production or advertised-feature defects; P2 items are medium-risk hardening and determinism work.

## Verification summary

| Check | Result |
|---|---|
| `go test -count=1 -race -coverprofile=... ./...` | Passed locally |
| Total statement coverage | 20.7% |
| `go vet ./...` | Passed |
| `golangci-lint` 2.8.0 | Passed, 0 findings |
| `scripts/security-lint.sh` / `govulncheck` | Passed; no reachable or package vulnerabilities |
| Dependency integrity | `go mod verify` passed; `go mod tidy` produced no diff |
| Release cross-builds | Darwin amd64/arm64, Linux amd64/arm64, and Windows amd64 built successfully |
| Release-metadata smoke | A binary built with 1.0.0 ldflags correctly reported `v1.0.0`, commit, date, Go version, and platform |
| Generated-project smoke | Failed immediately: generated config is invalid, secrets are not ignored, and generated CI is unusable |
| Current `main` integration run | Failed in setup because the Caddy admin API was unreachable |
| Published releases | Only `v0.0.1-dev` through `v0.0.6-dev`; no stable release and no `v1` Git ref |

The local environment had Docker but not Podman, so destructive live-server testing was not performed. The repository's own Podman-over-SSH integration job was inspected instead; its current `main` run reproduces the primary proxy blocker. This is a Go CLI, so browser UI testing was not applicable.

## P0 — release blockers

### AZ-001 — Make the default Caddy admin API reachable and fail startup when it is not

Owner lane: proxy runtime / integration

- Failure: Caddy is started with `CADDY_ADMIN=127.0.0.1:2019` inside a bridged container while host port `127.0.0.1:2019:2019` is published. Container-local loopback cannot be reached through that published port. `Boot` warns after ten seconds, prints “Proxy started,” and the first service registration then fails.
- Evidence: [manager.go:395](../internal/proxy/manager.go#L395), [manager.go:407](../internal/proxy/manager.go#L407), [manager.go:416](../internal/proxy/manager.go#L416), and the [failing current main run](https://github.com/lemonity-org/azud/actions/runs/28703688612).
- Acceptance: bridge and mixed rootful/rootless modes both expose a deliberately secured admin endpoint; `Boot` returns non-zero if readiness or initial config load fails; the setup integration test completes a first deploy and route registration.

### AZ-002 — Make `azud init` produce a valid, secret-safe project

Owner lane: CLI onboarding

- Failure: the template enables SSL but omits the required ACME email, so `azud init` succeeds and the next `azud config` fails validation. In a new repository, `.gitignore` is not created, leaving `.azud/secrets` eligible for commit.
- Evidence: [init.go:113](../internal/cli/init.go#L113), [init.go:178](../internal/cli/init.go#L178), and the release-binary smoke: secrets were mode `0600`, but `.gitignore` was missing and `azud config` exited 1 on `proxy.acme_email`.
- Acceptance: a fresh `azud init` project passes `azud config`; either SSL is off or a clearly editable valid ACME configuration is emitted; `.gitignore` is created or updated idempotently with `.azud/secrets`; integration tests cover empty and existing repositories.

### AZ-003 — Ship a resolvable, actually functional GitHub Actions scaffold

Owner lane: release engineering / CI integration

- Failure: generated workflows and docs use `lemonity-org/azud@v1`, but that Git ref returns HTTP 404 and the release workflow never creates or advances it. The scaffold exports `AZUD_SECRET_*` values while the generated config remains on the default file provider, so those values are not loaded even after the ref exists.
- Evidence: [init.go:581](../internal/cli/init.go#L581), [CI_CD.md:49](CI_CD.md#L49), generated workflow comments, and the public repository's absent `refs/tags/v1` checked on the audit date.
- Acceptance: a versioned action ref exists and is advanced by a controlled release process; generated config uses `secrets_provider: env` with the matching prefix or the workflow writes a protected secrets file; a fixture repository runs the generated workflow through install, config load, and a mocked or disposable deployment.

### AZ-004 — Implement role-specific deployment semantics before advertising roles

Owner lane: deploy core

- Failure: role selection only chooses hosts. Normal, canary, and systemd app deployments use one global container config and ignore `RoleConfig.Cmd`, `Labels`, `Options`, and `Env`. Every selected host receives the same service-named container and proxy route, including worker hosts; shared multi-role hosts cannot run distinct role workloads.
- Evidence: [config.go:107](../internal/config/config.go#L107), [deployer.go:512](../internal/deploy/deployer.go#L512), [deployer.go:625](../internal/deploy/deployer.go#L625), and [container.go:66](../internal/deploy/container.go#L66).
- Acceptance: role identity reaches container construction, naming, labels, command, environment, and resource options; non-web roles are not automatically routed; shared-host and distinct-host web/worker E2E cases pass; canary, rollback, scale, and Quadlet use the same role model.

### AZ-005 — Eliminate host-shell injection through role scale options

Owner lane: security / scale

- Failure: every unknown `servers.<role>.options` key/value is converted to a raw `--key=value` string. `BuildRunCommand` appends those strings without shell quoting. The denylist only recognizes a few exact Podman flags, so YAML-controlled shell metacharacters can execute commands on the SSH host during `azud scale`.
- Evidence: [scale.go:281](../internal/cli/scale.go#L281), [client.go:64](../internal/podman/client.go#L64), and [client.go:200](../internal/podman/client.go#L200).
- Acceptance: options are typed or allowlisted; every emitted argument is shell-quoted as data; privilege-escalating Podman flags remain blocked; regression tests include semicolons, command substitution, whitespace, quotes, newlines, and denylist-bypass forms and prove no host command is executed.

## P1 — high-impact production defects

### AZ-006 — Make `rollback_on_failure` restore the entire fleet

Owner lane: deploy core

- Failure: after one host fails, already successful hosts are rolled back and `succeededHosts` is cleared, but deployment continues to later hosts. Those later successes are never rolled back, leaving a mixed-version fleet despite the option's stated invariant.
- Evidence: [deployer.go:216](../internal/deploy/deployer.go#L216).
- Acceptance: stop scheduling new hosts on the first relevant failure or track and roll back every success; report rollback failures explicitly; a three-host test with failure on host two ends with all hosts on the prior version.

### AZ-007 — Fail deploys before destroying the last valid route or container

Owner lane: deploy core / proxy

- Failure: old-route removal errors are ignored before the old container is stopped; old-container existence errors are discarded; final route replacement failures after rename only warn and return success. These paths can leave Caddy pointing at a removed temporary name or dead old upstream while the deploy exits zero.
- Evidence: [deployer.go:330](../internal/deploy/deployer.go#L330), [deployer.go:427](../internal/deploy/deployer.go#L427), and [deployer.go:486](../internal/deploy/deployer.go#L486).
- Acceptance: proxy mutation is confirmed before container destruction; final routing failure returns non-zero and triggers a safe rollback; existence/stop/remove errors are classified rather than discarded; fault-injection tests prove one healthy route remains at every transition.

### AZ-008 — Rebuild scale around actual labeled instances and truthful results

Owner lane: scale

- Failure: `runScale` logs per-host errors but always returns success. Counts treat the base service container as an instance of every role and use prefix matching; new indices are derived from count, so gaps collide; scale-down assumes contiguous indices. Failed readiness and proxy registration only warn, and route-removal failures are ignored before containers are killed. Role labels are defined but user labels are not applied.
- Evidence: [scale.go:59](../internal/cli/scale.go#L59), [scale.go:208](../internal/cli/scale.go#L208), [scale.go:242](../internal/cli/scale.go#L242), and [scale.go:317](../internal/cli/scale.go#L317).
- Acceptance: enumerate instances by exact service/role/instance labels; choose free indices; return non-zero for any failed host; never register an unready instance or stop one still routed; preserve a documented role/base-container counting model; add gap, prefix-collision, partial-failure, and proxy-failure tests.

### AZ-009 — Make canary percentages and lifecycle failure-safe

Owner lane: canary / proxy

- Failure: canary deploy continues when the feature is disabled. Failure to set the stable upstream's weight is ignored, after which a canary weight such as 10 may be combined with the stable default weight rather than represent 10%. Partial multi-host deploys are not cleaned up. Promote and rollback ignore route-removal/weight failures before stopping containers, and final proxy registration can warn while promotion reports success.
- Evidence: [cli/canary.go:136](../internal/cli/canary.go#L136), [canary.go:261](../internal/deploy/canary.go#L261), [canary.go:315](../internal/deploy/canary.go#L315), and [canary.go:438](../internal/deploy/canary.go#L438).
- Acceptance: disabled means hard failure; both stable and canary weights are verified before success; percentage tests confirm observed routing; partial deploys clean up all touched hosts; no container is removed until route changes succeed; promote/rollback remain recoverable after any injected failure.

### AZ-010 — Move deployment and canary state out of ephemeral local checkouts

Owner lane: state / release operations

- Failure: history and canary state live under the local repository. Fresh CI jobs cannot discover the prior stable version or an in-progress canary, so automatic rollback, later promote/rollback, and status are unreliable. History filenames only have one-second resolution and concurrent same-service deployments overwrite each other even though record IDs are unique. Canary state persistence errors are debug-only.
- Evidence: [history.go:72](../internal/deploy/history.go#L72), [history.go:105](../internal/deploy/history.go#L105), [cli/canary.go:131](../internal/cli/canary.go#L131), and [canary.go:660](../internal/deploy/canary.go#L660).
- Acceptance: state has a documented durable source of truth usable from a clean CI checkout; writes are unique and atomic; persistence failures fail state-changing commands; concurrency tests preserve all records; deploy in one workspace and promote/rollback from another succeeds.

### AZ-011 — Require readiness, not liveness-or-readiness, before routing

Owner lane: health checks

- Failure: when both probes exist, `waitForContainerHealthy` succeeds as soon as either Podman liveness becomes healthy or the readiness endpoint passes. A live but not ready process can therefore enter the proxy early.
- Evidence: [container.go:121](../internal/deploy/container.go#L121), especially the OR behavior at [container.go:149](../internal/deploy/container.go#L149).
- Acceptance: readiness gates initial traffic whenever configured; liveness independently detects long-term failure; tests cover live/not-ready, ready/not-live, unhealthy, helper-image fallback, and disabled-liveness cases.

### AZ-012 — Make preflight, setup, and trust commands enforce their safety contract

Owner lane: CLI operations

- Failure: preflight displays failed SSH, trust, Podman, secrets, proxy, firewall, and cron cells but returns nil. Setup continues after registry-login, linger, proxy, and accessory failures. `ssh trust` similarly can finish zero after declined or failed hosts. These commands cannot safely gate CI.
- Evidence: [preflight.go:96](../internal/cli/preflight.go#L96), [setup.go:79](../internal/cli/setup.go#L79), [setup.go:90](../internal/cli/setup.go#L90), and [setup.go:119](../internal/cli/setup.go#L119).
- Acceptance: critical failures produce a summarized non-zero result; warnings are explicitly distinguished from blockers; setup does not claim completion after a failed prerequisite; tests assert exit status for every failed check category and partial host failure.

### AZ-013 — Make proxy lifecycle and configuration application fail closed

Owner lane: proxy runtime

- Failure: `Boot` and `Reboot` suppress admin readiness, restore, initial load, and config-apply failures, then report success. Updating an existing config only sets enabled values: turning off Auto HTTPS, redirects, logging, or custom certificates leaves stale Caddy state behind.
- Evidence: [manager.go:337](../internal/proxy/manager.go#L337), [manager.go:416](../internal/proxy/manager.go#L416), [manager.go:620](../internal/proxy/manager.go#L620), and [manager.go:514](../internal/proxy/manager.go#L514).
- Acceptance: required admin/config operations return errors; every managed setting has explicit set-and-clear behavior; enable/disable transition tests verify resulting Caddy JSON; success is printed only after the loaded config is read back.

### AZ-014 — Stop assuming the first Caddy handler is `reverse_proxy`

Owner lane: proxy routing

- Failure: request buffering inserts a `request_body` handler before `reverse_proxy`, but scale/canary/upstream mutation reads and patches `route.Handle[0].Upstreams`. With body limits or buffering enabled, mutations target the wrong handler. `AddUpstream` also blindly appends duplicates, skewing traffic after retries.
- Evidence: [manager.go:953](../internal/proxy/manager.go#L953), [manager.go:1059](../internal/proxy/manager.go#L1059), and [manager.go:1123](../internal/proxy/manager.go#L1123).
- Acceptance: locate the reverse-proxy handler by type rather than index; preserve all sibling handlers; upstream additions are idempotent; request-body-enabled deploy, scale, and canary tests pass across retry paths.

### AZ-015 — Make Quadlet/systemd units reproduce a valid Azud runtime after reboot

Owner lane: systemd / proxy

- Failure: the proxy unit starts the stock `/etc/caddy/Caddyfile` and has no mechanism to restore Azud's persisted JSON, so routes disappear after host reboot. The app unit ignores role command/options/env/labels. Default `EnvironmentFile=$HOME/.azud/secrets` relies on shell expansion that systemd/Quadlet does not perform, and raw `Environment=KEY=value` output does not safely encode spaces and special characters.
- Evidence: [systemd.go:189](../internal/cli/systemd.go#L189), [systemd.go:216](../internal/cli/systemd.go#L216), [systemd.go:229](../internal/cli/systemd.go#L229), and [generator.go:69](../internal/quadlet/generator.go#L69).
- Acceptance: cold host reboot restores routes before serving; `%h` or an absolute evaluated path is used for user secrets; Quadlet encoding round-trips special values; per-role units match normal deploy behavior; reboot E2E covers rootful and rootless modes.

### AZ-016 — Make remote secret writes atomic, correctly quoted, and permission-verified

Owner lane: secrets / security

- Failure: `secrets_remote_path` is interpolated inside hand-built double-quoted shell commands, so quotes or command substitutions can break into the host shell. A documented `$HOME` value is expanded locally by the config loader instead of remotely. Upload overwrites the final file before permissions are fixed and ignores a non-zero `chmod` exit code while reporting success.
- Evidence: [loader.go:71](../internal/config/loader.go#L71), [secrets.go:46](../internal/config/secrets.go#L46), and [env.go:180](../internal/cli/env.go#L180).
- Acceptance: path syntax is validated and safely represented without host-shell injection; home expansion occurs for the SSH user; write a mode-0600 temporary file then atomically rename; inspect and enforce final file/directory modes; malicious-path and chmod-failure tests return non-zero.

### AZ-017 — Protect persisted Caddy JSON that contains private keys

Owner lane: proxy security

- Failure: custom TLS certificate and private-key PEM are embedded in Caddy JSON, then persisted through ordinary `mkdir`, `cat`, and `mv` without enforcing directory or file modes. A normal remote umask can leave the private key readable by other local users.
- Evidence: [caddy.go:175](../internal/proxy/caddy.go#L175), [manager.go:565](../internal/proxy/manager.go#L565), and [manager.go:192](../internal/proxy/manager.go#L192).
- Acceptance: state directory is mode 0700 and private config is mode 0600 before data is written; existing permissive files are repaired; tests verify root and non-root paths and ensure logs never contain PEM material.

### AZ-018 — Align build tags with configured images and `deploy --version`

Owner lane: build / deploy CLI

- Failure: `generateImageTag` and `latestTag` append `:<tag>` to `cfg.Image` without stripping an existing tag or digest, producing invalid references. `azud deploy --version X` still builds and pushes an unrelated commit-derived and `latest` image before deploying X. Tag templates are not fully validated.
- Evidence: [build.go:49](../internal/cli/build.go#L49), [build.go:589](../internal/cli/build.go#L589), and [deploy.go:84](../internal/cli/deploy.go#L84).
- Acceptance: normalize registry ports, tags, and digests before tagging; an explicit deploy version either skips build or builds exactly that version by documented contract; validate OCI tag output; table tests cover tagged, digested, registry-port, destination, and dirty-tree inputs.

### AZ-019 — Match real container-ignore semantics for remote builds

Owner lane: remote builder / security

- Failure: the custom `.dockerignore` matcher does not make slashless patterns such as `*.env` match nested files, so remote contexts can upload files a local Podman/Docker build would exclude. Symlinks are silently omitted, making local and remote contexts differ. If SSH extraction returns early, the pipe reader is not closed and the archive goroutine can remain blocked.
- Evidence: [context_sync.go:54](../internal/cli/context_sync.go#L54), [context_sync.go:117](../internal/cli/context_sync.go#L117), and [context_sync.go:240](../internal/cli/context_sync.go#L240).
- Acceptance: use a standards-compatible ignore implementation or exhaustive conformance fixtures; preserve safe symlinks with escape checks; close/cancel both pipe ends on every return; nested-secret, negation, directory, symlink, and SSH-failure tests pass.

### AZ-020 — Give cron a supported runtime contract and truthful lifecycle commands

Owner lane: cron

- Failure: scheduled jobs assume the application image contains `crontab`, `crond -f -l 2`, `flock`, and `timeout`, while preflight checks those binaries on the host rather than in the image. Non-root lock mounts pass `${HOME}` through a single-quoted volume argument, preventing expansion. Boot and stop discard accumulated failures; stop never consumes its errors channel. Manual runs do not preflight secrets or pull the image, and logs require secrets unnecessarily.
- Evidence: [cron.go:122](../internal/cli/cron.go#L122), [cron.go:194](../internal/cli/cron.go#L194), [cron.go:302](../internal/cli/cron.go#L302), [cron.go:470](../internal/cli/cron.go#L470), and [preflight.go:518](../internal/cli/preflight.go#L518).
- Acceptance: define a portable scheduler mechanism or validate dependencies inside the target image; rootless lock paths resolve correctly; all host/job errors aggregate to non-zero; manual and scheduled paths share image/secrets/lock semantics; Alpine, Debian/distroless rejection, rootless, and multi-host E2E cases exist.

### AZ-021 — Honor the documented accessory configuration and target only the named accessory

Owner lane: accessories

- Failure: `accessory boot NAME` calls the deploy-all function. Only `PrimaryHost` is used, so additional `hosts` are ignored. `directories`, `options`, and `roles` are parsed but unused. `cmd` is split with `strings.Fields`, losing quoted argument boundaries, and accessory exec ignores remote non-zero exit status.
- Evidence: [app.go:390](../internal/cli/app.go#L390), [setup.go:229](../internal/cli/setup.go#L229), and [config.go:417](../internal/config/config.go#L417).
- Acceptance: named boot affects exactly one accessory; all documented fields are either implemented or rejected as unsupported; multi-host behavior is explicit; commands preserve arguments; exec returns the remote status; focused tests cover two accessories and quoted commands.

### AZ-022 — Implement real streaming logs and interactive exec

Owner lane: SSH / CLI UX

- Failure: app, proxy, accessory, and cron log commands call buffered SSH execution. `-f` therefore emits nothing until the remote command exits and can grow memory without bound. `app exec -it` adds Podman flags but the SSH session has no local stdin or PTY. Several log/status/accessory operations print stderr or stopped/error state and still return zero.
- Evidence: [container.go:140](../internal/podman/container.go#L140), [app.go:126](../internal/cli/app.go#L126), [app.go:168](../internal/cli/app.go#L168), and [proxy.go:294](../internal/cli/proxy.go#L294).
- Acceptance: expose streaming and PTY methods at the SSH client layer; wire stdin/stdout/stderr and terminal resize; propagate remote exit codes; `logs -f` shows lines incrementally with bounded memory; interactive and non-interactive integration tests pass.

### AZ-023 — Make the configuration schema strict and remove or implement dead fields

Owner lane: configuration

- Failure: YAML uses non-strict `yaml.Unmarshal`, so typos are silently accepted. `minimum_version` is format-checked but runtime enforcement is never called; config aliases are merged but never registered. `asset_path`, role tags, environment tags, and accessory roles have no runtime consumers. Destination merging omits `deploy.pre_deploy_command`, so that override is ignored.
- Evidence: [loader.go:88](../internal/config/loader.go#L88), [validator.go:838](../internal/config/validator.go#L838), [loader.go:530](../internal/config/loader.go#L530), and source-consumer searches for the fields in [config.go](../internal/config/config.go).
- Acceptance: unknown keys fail with path and suggestion; every documented field has an integration-level consumer or is removed before 1.0; minimum-version and aliases work end to end if retained; every destination-overridable field has explicit set/clear/merge tests.

### AZ-024 — Enforce command timeouts and remove SSH serialization bottlenecks

Owner lane: SSH / performance

- Failure: `deploy_timeout` only bounds readiness and remote-lock waiting; `createSSHClient` never sets `CommandTimeout`, so a remote Podman or shell command can hang indefinitely. `Client.Connect` holds one global mutex during network dial, serializing concurrent connections to different hosts. Each host connection also serializes complete sessions, making same-host scale goroutines effectively sequential.
- Evidence: [server.go:151](../internal/cli/server.go#L151), [ssh/client.go:87](../internal/ssh/client.go#L87), and [ssh/session.go:51](../internal/ssh/session.go#L51).
- Acceptance: all operations honor command context/deadline and cancellation; connection creation is singleflight per host rather than globally locked; concurrency limits are explicit; N unreachable hosts complete near one connect timeout rather than N times it; hung-command and cancellation tests exist.

### AZ-025 — Harden the release supply chain

Owner lane: release security

- Failure: release and test jobs grant meaningful tokens while third-party actions are pinned only to mutable major tags. Runtime Caddy uses mutable `caddy:2-alpine`. Installer checksum and binary are downloaded from the same unsigned release with no signature, attestation, or provenance verification.
- Evidence: [.github/workflows/release.yml](../.github/workflows/release.yml), [.github/workflows/tests.yml](../.github/workflows/tests.yml), [manager.go:18](../internal/proxy/manager.go#L18), and [install.sh:65](../scripts/install.sh#L65).
- Acceptance: external actions and runtime images are pinned by reviewed immutable digest/SHA with an update policy; release artifacts include verifiable signatures and SLSA-style provenance or an explicitly chosen equivalent; installer verifies an independent trust root; permissions are job-minimal.

### AZ-026 — Stop exposing registry passwords in action process arguments

Owner lane: GitHub Action / security

- Failure: the composite action invokes `podman login ... -p "$REGISTRY_PASSWORD"` or Docker equivalent. The secret is placed in process arguments instead of stdin.
- Evidence: [action.yml:72](../action.yml#L72).
- Acceptance: pipe the environment-provided password to `--password-stdin` without echoing it; action logs remain masked; a runner-level test inspects the spawned argv and confirms the password is absent.

### AZ-027 — Repair the stable-release packaging and installation contract

Owner lane: release engineering

- Failure: `make build-proxy`, `build-all`, and `podman-build` reference missing `cmd/azud-proxy` and `Dockerfile.proxy`. The primary README installation command, `go install ...@latest`, cannot receive ldflags and therefore reports `dev`. The installer embeds the full five-line `azud version` output inside its one-line success banner. README declares MIT, but no LICENSE file exists; no changelog is present.
- Evidence: [Makefile:43](../Makefile#L43), [README.md:52](../README.md#L52), [install.sh:105](../scripts/install.sh#L105), and the repository file inventory.
- Acceptance: remove obsolete targets or ship their sources; every documented install path reports the released version; installer output is parsed and smoke-tested on supported OS/architectures; the declared license exists as a full file; release notes/changelog policy is documented.

### AZ-028 — Make every quick-start and CI example executable as written

Owner lane: documentation / developer experience

- Failure: README's main YAML indents `proxy` and `env` under `servers`, so the parser sees them as roles and validation reports no proxy host. Getting Started and other SSL examples omit the required ACME email. CI claims the generated workflow is preconfigured even though its action ref and provider are broken. Published examples reference unavailable stable image/action versions.
- Evidence: [README.md:73](../README.md#L73), [GETTING_STARTED.md:23](GETTING_STARTED.md#L23), and [CI_CD.md:11](CI_CD.md#L11). The release binary was run against the first two examples and both exited non-zero.
- Acceptance: extract every YAML and shell quick-start block in CI; parse and validate config examples; run help/install/scaffold smoke tests against the release artifact; versioned docs only reference artifacts produced by the release workflow.

### AZ-029 — Add E2E coverage for the advertised production workflows

Owner lane: quality engineering

- Failure: total coverage is 20.7%; `cmd`, proxy, server, and state are 0%, SSH is 12%, CLI 12%, and deploy 22.8%. The only remote setup integration is a root/insecure-key happy path and it is currently red. There is no deploy/rollback, role, multi-host, scale, canary, cron, accessory, proxy-reboot, systemd-reboot, rootless, or failure-injection E2E suite.
- Evidence: local race/coverage results and [setup_integration_test.go](../internal/cli/setup_integration_test.go).
- Acceptance: establish a disposable Podman-over-SSH matrix for rootful/rootless and bridge/mixed modes; cover first deploy, zero-downtime update, rollback, role workloads, scale, canary, cron, accessories, reboot restoration, and injected proxy/SSH/registry failures; make the stable release conditional on the matrix being green.

## P2 — medium-risk hardening

### AZ-030 — Do not silently disable cross-host image digest verification

Owner lane: deploy security

- Failure: if digest lookup fails on the first host, verification returns success with an empty digest and skips every other host, despite being described as mutable-tag attack detection.
- Evidence: [deployer.go:583](../internal/deploy/deployer.go#L583).
- Acceptance: registry-backed images fail closed or require an explicit, logged opt-out; local-only images follow a separate documented policy; history records the verified digest; tests cover lookup failure, mismatch, digest-pinned images, and explicit bypass.

### AZ-031 — Make host, role, accessory, cron, and scale ordering deterministic

Owner lane: configuration / CLI

- Failure: multiple getters and command loops iterate Go maps directly. Host order, “first host” migration/build behavior, default cron placement, scale order, generated tables, and error strings can change between runs.
- Evidence: [config.go:642](../internal/config/config.go#L642), [config.go:700](../internal/config/config.go#L700), and map iteration in [scale.go:94](../internal/cli/scale.go#L94).
- Acceptance: define and document stable ordering, normally sorted names plus configured host order; operations that require an explicit primary host do not derive it from a map; repeatability tests run many iterations and produce identical plans/output.

## Release gate

Do not tag 1.0.0 until AZ-001 through AZ-005 are closed and verified on the release-built artifacts. Before calling the release production-ready, close all P1 findings or explicitly remove the affected commands/features from the 1.0.0 surface and documentation. AZ-029 should be used as the verification umbrella rather than accepting source-only fixes.
