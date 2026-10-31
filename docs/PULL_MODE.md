# Pull-Mode Deploys

In push mode, CI runs `azud deploy`, which connects to the server over SSH. In
pull mode, `azud agent` runs on the server itself. It follows a registry tag,
and when the tag moves to a new image it verifies the image and deploys it on
that server with the same steps as `azud deploy`.

CI then only builds, signs, and tags images. It needs no SSH key, no network
path to the server, and no deploy secrets.

## How it works

```text
CI:     build → push by digest → sign → move tag "production" → wait for /health
Server: every agent.interval → tag points to a new digest?
          → verify_command on image@digest → azud deploy image@digest locally
```

Each check:

1. Resolves the tag to a manifest digest through the registry API, without
   pulling the image.
2. Stops if that digest is already deployed, or if it failed before.
3. Records the digest as deployed, without deploying again, if every role
   container already runs `image@digest` (for example after a pushed deploy).
4. Runs `agent.verify_command` with `AZUD_AGENT_IMAGE` set to the image pinned
   by digest. The image is never deployed if the command fails.
5. Deploys the pinned image: registry login, pull, digest check, pre-deploy
   command, health-checked container swap, and rollback on failure, exactly
   as `azud deploy` does. The deploy takes the same host lock as a pushed
   deploy, so the two never overlap.

A release that fails verification or deployment is not tried again until the
tag moves, so a broken image is not redeployed every minute. Once
verification passes, the deploy runs to completion even if the agent is asked
to stop.

## Requirements

- The service runs on one server. Fleet-wide steps (the pre-deploy command and
  rollback across hosts) need a single coordinator, so validation rejects an
  `agent` section when `servers` lists more than one host.
- The agent runs as `ssh.user`, so state, rootless Podman, and the deploy lock
  are the ones pushed deploys use.
- The server has been set up once in push mode (`azud setup`), which installs
  Podman, enables lingering, and boots the proxy.

## Configure

Add an `agent` section, usually in a destination file used only on the
server. The image must name the repository; the agent supplies the digest:

```yaml
# config/deploy.agent.yml (merged with config/deploy.yml by --destination agent)
image: ghcr.io/acme/app

agent:
  track: production      # registry tag the agent follows
  interval: 1m           # how often to check (default 1m, minimum 10s)
  verify_timeout: 2m     # limit for verify_command (default 2m)
  verify_command: >-
    cosign verify
    --certificate-identity https://github.com/acme/app/.github/workflows/release.yml@refs/heads/main
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
    "$$AZUD_AGENT_IMAGE"
```

`verify_command` runs through `/bin/sh` with these variables. Write them with
`$$`: Azud expands `$VARS` from the environment when it loads the
configuration, and `$$` keeps a literal `$` for the shell. Validation rejects
a `verify_command` that does not name `AZUD_AGENT_IMAGE` or
`AZUD_AGENT_DIGEST`.

| Variable | Value |
|---|---|
| `AZUD_AGENT_IMAGE` | Image pinned by digest, e.g. `ghcr.io/acme/app@sha256:…` |
| `AZUD_AGENT_DIGEST` | The digest alone |
| `AZUD_AGENT_REF` | The followed tag, e.g. `ghcr.io/acme/app:production` |
| `AZUD_SERVICE` | Service name |
| `AZUD_DESTINATION` | Destination, if any |

The image is passed in the environment, never interpolated into the command,
so a reference cannot inject shell syntax.

Verification is required. Anyone who can move the tag decides what the agent
deploys, so the command must prove who built the image. With keyless cosign,
the certificate identity above accepts only images signed by the release
workflow on `main`. To deploy unverified images anyway, set
`insecure_skip_verify: true` instead of `verify_command`.

### Registry credentials

For a private image, the server needs a read-only registry credential. Put the
password secret named in `registry.password` in the server's secrets file
(`secrets_path`, mode 0600) or in the agent's environment. The agent sends
these credentials only to `registry.server`. `cosign` reads its own
credentials: run `cosign login` once as `ssh.user`.

The agent loads secrets with `secrets_provider` like any other command. When
`config/deploy.yml` reads them with a tool the server does not have, such as
`secrets_command: op inject ...`, override the provider in the agent's
destination file. The secrets file then needs only the registry password:

```yaml
# config/deploy.agent.yml
secrets_provider: file
secrets_path: .azud/agent-secrets   # AZUD_REGISTRY_PASSWORD=..., mode 0600
```

### Application secrets

The agent deploys with the runtime secrets file already on the server, as a
pushed deploy does. Change secrets with `azud env push` from a workstation.

## Install the agent

On the server, as `ssh.user`, from the directory that holds `config/`:

```bash
azud agent run --once -d agent      # check the tag once and deploy it
mkdir -p ~/.config/systemd/user
azud agent unit -d agent > ~/.config/systemd/user/azud-agent-app.service
systemctl --user daemon-reload
systemctl --user enable --now azud-agent-app.service
journalctl --user -u azud-agent-app.service -f
```

The unit restarts the agent if it exits, and gives a deploy in progress up to
30 minutes to finish when it is stopped.

## Publish from CI

CI pushes the image, signs it, and only then moves the tag:

```yaml
- name: Build and push
  id: build
  uses: docker/build-push-action@v6
  with:
    push: true
    tags: ghcr.io/acme/app:${{ github.sha }}

- name: Sign
  run: cosign sign --yes ghcr.io/acme/app@${{ steps.build.outputs.digest }}

- uses: imjasonh/setup-crane@v0.4

- name: Release to production
  run: crane tag ghcr.io/acme/app@${{ steps.build.outputs.digest }} production

- name: Wait for the release to go live
  run: |
    for attempt in $(seq 1 60); do
      curl -fsS https://app.example.com/health | grep -q "${{ github.sha }}" && exit 0
      sleep 10
    done
    echo "production did not report ${{ github.sha }}" >&2
    exit 1
```

Move the tag with a command that points it at the signed manifest itself, such
as `crane tag`. `docker buildx imagetools create --tag` wraps a single-platform
image in a new index whose digest has no signature, so verification fails.

The job needs `packages: write` and `id-token: write`, and nothing that reaches
the server. The last step gives CI a pass or fail without any server access;
it relies on the health endpoint reporting the deployed commit.

## Operate

| Task | Command |
|---|---|
| See what the agent last saw and deployed | `azud agent status -d agent` |
| Retry a release that failed | `azud agent run --once --retry-failed -d agent` |
| Follow the log | `journalctl --user -u azud-agent-app.service -f` |
| Roll back | Move the tag back to the previous digest |
| Break-glass deploy | `azud deploy` from a workstation still works; the agent adopts the digest if the tag later points to it |

State lives in `~/.local/share/azud/agent/<service>.json` for non-root users
(`/var/lib/azud/agent/` for root). Deployment history is recorded on the
server, as for any deploy run there.

## Release bundles

A release bundle is one OCI artifact per release, in the image's repository,
that carries the image digest, `config/deploy.yml` and the destination file as
written, and the runtime secrets encrypted with [age](https://age-encryption.org)
to the keys in `release.recipients`. CI publishes, signs and promotes it:

```bash
azud release publish --image "$IMAGE_DIGEST" -d production --digest-file bundle.digest
cosign sign --yes "ghcr.io/acme/app@$(cat bundle.digest)"
azud release promote "$(cat bundle.digest)" --track production
azud release inspect production      # image, commit, secret names; never values
```

`publish` loads the secrets with `secrets_provider`, as `azud env push` does,
and refuses to publish when a secret named in `env.secret` or an accessory is
missing. It never tags, so a tag only ever points at a signed bundle.

### Apply bundles on the server

With `agent.source: bundle`, the tag points to a bundle instead of an image.
The agent verifies the bundle's signature with `verify_command`, decrypts the
secrets with the server's key, writes the runtime secrets file, and deploys
the bundle's image with the bundle's configuration. Changing a secret or the
configuration then needs no SSH: change it in the repository or the secrets
store and publish a release.

Set it up once, on the server, as `ssh.user`:

```bash
azud agent init -d agent     # creates ~/.config/azud/agent/<service>.age (0600), prints age1...
```

Add the printed recipient to `release.recipients` in the repository's
`config/deploy.yml` and publish a release. Then switch the server:

```yaml
# config/deploy.agent.yml on the server
agent:
  source: bundle
  track: production
  verify_command: >-
    cosign verify --certificate-identity https://github.com/acme/app/.github/workflows/release.yml@refs/heads/main
    --certificate-oidc-issuer https://token.actions.githubusercontent.com "$$AZUD_AGENT_IMAGE"
```

`verify_command` is required: the recipients are public keys, so only the
signature proves who built a bundle. `AZUD_AGENT_IMAGE` names the bundle by
digest; sign the bundle digest that `azud release publish` prints.

**The server's configuration is the trust anchor.** From it the agent takes
`agent`, `ssh`, `security`, `registry`, `builder`, `hooks`, `hooks_path` and
the `secrets_*` settings, and the bundle's values for them are ignored (the
repository's `deploy.yml` sets several of them for push mode). Everything else
comes from the bundle. A bundle for another service, another image
repository, or a host other than this server is refused. Hooks never run for
a bundle.

What the agent does for each bundle:

- **Secrets:** it writes them atomically to the runtime secrets file (0600).
  If the deploy fails, it puts the previous release's secrets back by
  decrypting that release again; before the first bundle, the file is copied
  once to `<file>.pre-bundle`. Decrypted secrets are never stored elsewhere.
- **Downgrades:** a bundle older than the deployed one is refused, so access to
  the registry alone cannot bring back an old release. To roll back, move the
  tag to the older bundle and run `azud agent run --once --allow-downgrade -d agent`
  (or set `agent.allow_downgrade: true`).
- **What it does not apply:** changes to accessories, cron, Podman, and the
  proxy settings shared by every app on the host. It deploys the app, and
  `azud agent status` lists those changes with the command to run from a
  workstation. A changed secret used only by an accessory is listed too.
- **Storage:** each applied release (configuration, `release.json`, encrypted
  secrets) is kept in `~/.local/share/azud/agent/<service>/releases/`.

## Limitations

- One server per service.
- With image tags, `config/deploy.yml` on the server is what the agent uses; a
  change in the repository reaches the server only when you copy it there.
  Release bundles carry the configuration instead.
- `azud setup`, `azud proxy`, and accessory commands still run in push mode.
