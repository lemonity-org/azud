# Azud JIT security suite

This directory contains a build-tagged, adversarial test suite created during
the security review of the Azud CLI. It is **not** part of the normal
`go test ./...` run; use the `security` build tag.

```bash
./security/jit/run.sh
# or
go test -tags security -v ./security/jit/...
```

## What it does

- Builds the real `cmd/azud` binary once into a temporary directory.
- Starts an in-process **fake SSH server** on localhost. The server accepts a
  throwaway client key, records every command and stdin payload, and returns
  controlled stdout/stderr/exit statuses.
- Runs the actual CLI with a generated `config/deploy.yml` against that server.
- Asserts on the exact remote command strings (for example the `podman run`
  used to boot Caddy) and on local filesystem state (secrets file modes,
  symlinks).

No real host, Podman daemon, registry, or Caddy instance is required. The
suite only creates temporary directories, binds ephemeral localhost ports, and
uses harmless canary files.

## Test naming

- `TestControl_*` — positive controls. These assert a security property that
  should always hold. A failure is a real regression.
- `TestFinding_*` — adversarial proof tests. While the issue exists they pass
  and log a line beginning with `VULNERABLE` or `HARDENING`. Once an issue is
  fixed, invert the assertion (or move it into the control group) so it becomes
  a normal regression test.
- `Fuzz*` — seed-runnable fuzz targets. Run with `FUZZ=1` and optionally
  `FUZZTIME=5s`.

The captured evidence from the review run is in `results.log`.

## Coverage

| Area | Tests |
|---|---|
| Shell quoting round-trip (quotes, `$()`, backticks, newlines, CR, unicode) | `TestControl_ShellQuoteRoundTrip`, `TestControl_ShellJoinAndRemotePathAreDataSafe` |
| Podman command builders (container, exec, logs, build) | `TestControl_ContainerCommandBuilderKeepsPayloadsAsArguments`, `TestControl_ExecAndLogsBuildersKeepPayloadsAsArguments`, `TestControl_BuildCommandBuilderKeepsPayloadsAsArguments` |
| Config limits, strictness, validators | `TestControl_ConfigRejectsOversizedDuplicateAndDeepDocuments`, `TestControl_ConfigRejectsShellMetacharactersInExecutedFields` |
| Config env expansion / destination traversal / secret leak | `TestFinding_ConfigEnvExpansion*`, `TestFinding_DestinationPathTraversal`, `TestFinding_SecretsCommandOutputLeaksOnFailure` |
| Local secrets handling | `TestFinding_EnvSet*`, `TestFinding_WorldReadableSecretsFileOnlyWarns`, `TestControl_EnvPullWritesPrivateSecretsFile` |
| Secret delivery scope | `TestFinding_EnvPush*`, `TestFinding_EnvPrefixPushesUnrelatedSecrets` |
| Remote build context | `TestFinding_BuildContext*` |
| Caddy admin / TLS / proxy lifecycle | `TestFinding_Caddy*`, `TestFinding_Proxy*`, `TestFinding_CronLockExposesAzudStateAndCaddyConfig` |
| Registry | `TestFinding_Registry*` |
| Hooks | `TestControl_HooksRejectTraversalAndSymlinks` |
| Init scaffold | `TestFinding_InitGitignoreSubstringCheck`, `TestFinding_InitDoesNotProtectBuildContext` |
| Quadlet / systemd generation | `TestControl_QuadletContainerFieldsCannotInjectDirectives`, `TestFinding_QuadletUnsanitizedFieldsInjectDirectives` |
| Terminal output | `TestFinding_TerminalEscapeInjectionFromRemoteOutput` |
| Access logging | `TestFinding_AccessLogRedactionIsOptIn` |
| Latent unsafe helpers | `TestFinding_HealthcheckBuildersExecuteDollarSubstitution`, `TestFinding_BuildConfigOptionsAreShellEvaluated` |

## Requirements

- Go toolchain used by the module.
- `sh`, `git` (the CLI uses `git` to derive a build version; a non-repo context
  falls back to a timestamp).
- A writable `GOCACHE`. The run script uses
  `${TMPDIR:-/tmp}/azud-security-gocache`.
- `AZUD_TEST_BIN=/path/to/azud` to skip the one-time build.

## Limitations

The fake SSH server proves **what Azud sends**, not how a real Podman or Caddy
process reacts. In particular, the Caddy admin exposure is proven from the
generated `podman run` command (`CADDY_ADMIN=0.0.0.0:2019`, `--network azud`)
plus the fact that application containers join the same network. Confirm live
reachability on a disposable host with the commands in
`../poc/06-caddy-admin-exposure.sh`.

Config files and hooks are executable by design. Tests that require a malicious
configuration demonstrate the impact of that trust decision and are most
relevant when any part of the config/env input comes from a less-trusted source
(PR metadata, shared CI variables, generated configs).

See `../SECURITY_FINDINGS.md` for severity, impact, and remediation.
