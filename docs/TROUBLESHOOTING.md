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

Fixes:
- Update `ssh.user` to a non-root user if required
- Enable rootless Podman in config
- Set `ssh.insecure_ignore_host_key` to false if `security.require_known_hosts` is enabled

## HTTPS not provisioning

Symptoms:
- Caddy errors about ACME
- `too many certificates` or `no valid host`

Fixes:
- Verify DNS points to the server
- Ensure ports 80/443 are open
- Check proxy logs: `azud proxy logs -f`

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
- Recreate `.azud/secrets` in CI from secret store
- Ensure the CI SSH key is trusted on all hosts
- Add `ssh-keyscan` to populate `known_hosts`

## Related docs

- `docs/OPERATIONS.md`
- `docs/SECURITY.md`
