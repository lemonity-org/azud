# Troubleshooting

Common issues and fixes when deploying with Azud.

## SSH connection errors

Symptoms:
- `Permission denied (publickey)`
- `Host key verification failed`

Fixes:
- Ensure your SSH public key is in `~/.ssh/authorized_keys` on the server
- Use `azud ssh trust <host>` to add host keys to `known_hosts`
- Verify `ssh.user` and `ssh.port` in `config/deploy.yml`
- If you enforce trusted fingerprints, generate a template with `azud ssh trust --template`

## Registry login fails

Symptoms:
- `registry password not found`
- `unauthorized: authentication required`

Fixes:
- Add the secret to `.azud/secrets`
- Ensure `registry.username` is set
- Verify the secret key name matches `registry.password`

## Health check failing

Symptoms:
- Deploy hangs or rolls back
- Containers start but never become healthy

Fixes:
- Confirm `proxy.app_port` matches the port your app listens on
- Ensure `healthcheck.path` returns `200 OK`
- Increase readiness delay if the app boots slowly

## Preflight fails

Symptoms:
- Security policy errors (non-root SSH, rootless Podman, known hosts)
- `secrets file .azud/secrets is readable by group or others`

Fixes:
- Update `ssh.user` to a non-root user if required
- Enable rootless Podman in config
- Set `ssh.insecure_ignore_host_key` to false if `security.require_known_hosts` is enabled
- Run `chmod 600 .azud/secrets` (or the configured `secrets_path`). Any
  `azud env set` or `azud env delete` also rewrites the file with mode `0600`.
  Other commands only warn about a loose mode.

## HTTPS not provisioning

Symptoms:
- Caddy errors about ACME
- `too many certificates` or `no valid host`

Fixes:
- Verify DNS points to the server
- Ensure ports 80/443 are open
- Check proxy logs: `azud proxy logs -f`

## Rootless proxy cannot bind 80/443

Symptoms:
- Proxy startup fails with port bind errors
- Config validation fails for `proxy.http_port` / `proxy.https_port` with `podman.rootless: true`

Fixes:
- For rootless Podman, use unprivileged ports (for example `8080` / `8443`) and front with LB/NAT
- Or set `proxy.rootful: true` to run the proxy with rootful Podman while keeping app containers rootless
- In mixed mode (`podman.rootless: true` + `proxy.rootful: true`), keep proxy ports at `80/443`
- If `ssh.user` is non-root and `proxy.rootful: true`, ensure passwordless `sudo` is available for Podman commands

## Locked cron job runs twice

Symptoms:
- A `lock: true` job started by `azud cron run` overlaps a scheduled run

Fixes:
- Upgrade every machine and CI job that runs `azud cron run`. Older releases
  lock only the old file in the Azud state directory, which a cron container
  recreated by a newer `azud cron boot` no longer uses.
- To downgrade, run `azud cron stop <job>` and then that release's
  `azud cron boot`; older releases keep a running cron container as it is.

## App serves old version

Symptoms:
- Deploy succeeds but responses are unchanged

Fixes:
- Confirm image tag was updated in your registry
- Use `azud deploy --version <tag>` to force the new image
- Check `azud app details` for running image tags

## Secret not found at runtime

Symptoms:
- App crashes on boot due to missing env var

Fixes:
- Ensure the secret exists in `.azud/secrets`
- Run `azud env push` after changes
- Verify `env.secret` lists the key in `config/deploy.yml`

## Deploy works locally but fails in CI

Fixes:
- Recreate `.azud/secrets` in CI from secret store with mode `0600` (for
  example run `umask 077` before writing it); `azud preflight` fails on a
  group- or other-readable file
- Ensure the CI SSH key is trusted on all hosts
- Add `ssh-keyscan` to populate `known_hosts`

## Related docs

- `docs/OPERATIONS.md`
- `docs/SECURITY.md`
