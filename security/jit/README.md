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

- `TestControl_*` — regression controls. Each asserts a security property that
  should always hold; a failure is a real regression. During the review, the
  findings were proven by `TestFinding_*` tests that passed while the issue
  existed and logged `VULNERABLE` or `HARDENING`. Every finding has since been
  fixed and its test inverted into a control, so no `TestFinding_*` tests
  remain.
- `Fuzz*` — seed-runnable fuzz targets. Run with `FUZZ=1` and optionally
  `FUZZTIME=5s`.

The captured evidence from the original review run (revision `28c2f20`, before
the fixes) is in `results.log`. It is a local, gitignored file and is not
regenerated.

## Coverage

| Area | Tests |
|---|---|
| Shell quoting round-trip (quotes, `$()`, backticks, newlines, CR, unicode) | `TestControl_ShellQuoteRoundTrip`, `TestControl_ShellJoinAndRemotePathAreDataSafe` |
| Podman command builders (container, exec, logs, build, build options) | `TestControl_ContainerCommandBuilderKeepsPayloadsAsArguments`, `TestControl_ExecAndLogsBuildersKeepPayloadsAsArguments`, `TestControl_BuildCommandBuilderKeepsPayloadsAsArguments`, `TestControl_BuildConfigOptionsAreQuoted` (L-02) |
| Health-check probe builders | `TestControl_HealthcheckBuildersQuoteURL` (L-01) |
| Config limits, strictness, validators | `TestControl_ConfigRejectsOversizedDuplicateAndDeepDocuments`, `TestControl_ConfigRejectsShellMetacharactersInExecutedFields`, `TestControl_ImageReferenceValidationRejectsNonReferences` (L-04) |
| Config env expansion (JIT-03) | `TestControl_ConfigEnvExpansionCannotInjectYAMLStructure`, `TestControl_ConfigEnvExpansionCannotOverrideSSHSecurity`, `TestControl_ConfigEnvExpansionInCommentIsInert`, `TestControl_ConfigEnvExpansionFlowSequenceStaysOneScalar` |
| Destination traversal (JIT-07) | `TestControl_DestinationPathTraversalIsRejected` |
| `secrets_command` output and execution (JIT-05, L-06) | `TestControl_SecretsCommandFailureDoesNotLeakStdout`, `TestControl_ConfigCommandDoesNotExecuteSecretsCommand`, `TestControl_ShellCompletionDoesNotExecuteSecretsCommand` |
| Local secrets writer (JIT-04) | `TestControl_EnvSetTightensInsecureSecretsMode`, `TestControl_EnvSetRefusesSymlinkedSecretsFile`, `TestControl_EnvDeleteRefusesSymlinkedSecretsFile`, `TestControl_EnvSetRejectsNewlineInValue`, `TestControl_EnvSetRejectsInvalidKey`, `TestControl_InitCreatesPrivateAzudDir`, `TestControl_InitKeepsExistingSecretsAndHooks` |
| `env set` from stdin (L-05) | `TestControl_EnvSetReadsSecretFromStdin`, `TestControl_EnvSetStdinRejectsEmbeddedNewline`, `TestControl_EnvSetArgvFormWarns` |
| Secrets file mode and preflight mode (JIT-15) | `TestControl_WorldReadableSecretsFileStillLoadsWithWarning`, `TestControl_PreflightRejectsWorldReadableSecretsFile` |
| `env pull` (JIT-12) | `TestControl_EnvPullWritesPrivateSecretsFile`, `TestControl_EnvPullFailsOnUnreadableRemoteFile`, `TestControl_EnvPullReportsMissingRemoteFile` |
| Secret delivery scope (JIT-08, JIT-13) | `TestControl_EnvPushTargetsOnlyWorkloadHosts`, `TestControl_EnvPushRejectsRemoteBuilderHost`, `TestControl_EnvPushRejectsNewlineInEnvProviderValue`, `TestControl_EnvPrefixSkipsReservedAzudVariables`, `TestControl_EnvPrefixDoesNotPushRegistryCredential` |
| Remote build context (JIT-09, JIT-14) | `TestControl_BuildContextExcludesConfiguredSecretsPath`, `TestControl_BuildContextExcludesNestedProjectSecrets`, `TestControl_BuildContextUsesPrivateTempDir`, `TestControl_BuildContextCleanedUpOnSyncFailure`, `TestControl_BuildContextRejectsUnexpectedTempDir` |
| Local build context and init scaffold (JIT-17, JIT-18) | `TestControl_InitGitignoreIgnoresCommentMentions`, `TestControl_InitRefusesSymlinkedGitignore`, `TestControl_InitProtectsBuildContext`, `TestControl_InitDoesNotShadowExistingDockerignore`, `TestControl_InitNeverWritesThroughSymlinkedContainerignore`, `TestControl_LocalBuildPassesMandatoryIgnoreFile` |
| Caddy admin socket and TLS (JIT-01) | `TestControl_CaddyAdminIsPrivateUnixSocket`, `TestControl_CustomTLSConfigLoadsOnlyThroughPrivateAdminSocket`, `TestControl_RestoredPersistedConfigCannotMoveAdmin` |
| Proxy admin migration (JIT-01) | `TestControl_ProxyBootRecreatesLegacyAdminContainer`, `TestControl_ProxyBootRestoresLegacyProxyWhenReplacementFails`, `TestControl_ProxyBootReportsAdminProbeError`, `TestControl_ProxyBootRefusesSystemdManagedLegacyProxy`, `TestControl_ProxyStatusReportsLegacyProxy`, `TestControl_PreflightBlocksOnlySystemdManagedLegacyProxy`, `TestControl_SystemdEnableMigratesProxyToPrivateSocket`, `TestControl_SystemdEnableStartsProxyUnitWhenPreparationFails` |
| Proxy lifecycle and hardening (JIT-06, JIT-16) | `TestControl_ProxyStopFailsClosed`, `TestControl_ProxyStopTreatsMissingContainerAsStopped`, `TestControl_ProxyStopRefusesSystemdManagedProxy`, `TestControl_ProxyContainerHardeningFlags` |
| Cron lock and boot ordering (JIT-02) | `TestControl_CronLockMountsOnlyReadOnlyJobLockDir`, `TestControl_CronBootRecreatesLegacyLockMount`, `TestControl_CronBootKeepsLegacyContainerWhenPreparationFails`, `TestControl_CronRunHoldsLegacyLockWhileLegacyContainerExists` |
| Registry (JIT-10) | `TestControl_RegistryLoginRejectsUnconfiguredHost`, `TestControl_RegistryServerIsValidated`, `TestControl_RegistryLoginTerminatesPodmanOptions` |
| Hooks | `TestControl_HooksRejectTraversalAndSymlinks` |
| Quadlet / systemd generation (L-03) | `TestControl_QuadletContainerFieldsCannotInjectDirectives`, `TestControl_QuadletServiceInstallAndNamesCannotInjectDirectives`, `FuzzQuadletContainerFileLineInjection` |
| Terminal output (JIT-11) | `TestControl_RemoteOutputNeutralizesTerminalEscapes` |
| Access logging (JIT-19) | `TestControl_AccessLogKeepsCaddyCredentialRedaction` |
| Shell quoting fuzz | `FuzzShellQuotePreservesData` |

## Requirements

- Go toolchain used by the module.
- `sh`, `git` (the CLI uses `git` to derive a build version; a non-repo context
  falls back to a timestamp).
- A writable `GOCACHE`. The run script uses
  `${TMPDIR:-/tmp}/azud-security-gocache`.
- `AZUD_TEST_BIN=/path/to/azud` to skip the one-time build.

## Limitations

The fake SSH server proves **what Azud sends**, not how a real Podman or Caddy
process reacts. For the Caddy admin API, the suite asserts that the generated
`podman run` and Quadlet commands configure only the private unix socket
(`CADDY_ADMIN=unix//run/azud-admin/admin.sock|0600` on a private tmpfs, nothing
published on 2019) and that Azud reaches it through `podman exec ... curl
--unix-socket`. Live proof against the pinned Caddy image, including that a
peer container on the same network cannot reach the admin API and that a
normalized autosave keeps the socket listener, is in
`../../scripts/caddy_admin_contract_test.sh`. The pre-fix reachability PoC is
`../poc/06-caddy-admin-exposure.sh`.

Config files and hooks are executable by design. Tests that require a malicious
configuration demonstrate the impact of that trust decision and are most
relevant when any part of the config/env input comes from a less-trusted source
(PR metadata, shared CI variables, generated configs).

See `../SECURITY_FINDINGS.md` for severity, impact, and remediation.
