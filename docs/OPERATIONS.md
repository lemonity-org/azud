# Operations Guide

## Durable state

Azud stores deployment history and canary progress outside the checkout in
`~/.local/share/azud` (or `/var/lib/azud` when run as root). You may set
`AZUD_STATE_DIR` to an absolute, access-controlled persistent path. Back up this
directory and ensure only one deployment pipeline mutates a service at a time.
Ephemeral CI runners must restore the directory before any deploy, rollback, or
canary command; see `docs/CI_CD.md`.

Day-2 tasks for running Azud in production.

## Logs and Debugging

```bash
azud app logs --tail 200
azud app logs -f
azud app details
```

## Preflight Checks

```bash
azud preflight
azud preflight --role web
```

## Executing Commands

```bash
azud app exec -- bin/rails console
azud app exec -it -- /bin/sh
```

## Secrets Management

```bash
azud env list
azud env set DATABASE_URL    # prompts for the value without echo
azud env push
```

In scripts, pipe the value on stdin (`printf '%s' "$DATABASE_URL" | azud env
set DATABASE_URL`). Passing it as a second argument still works but is
deprecated: it lands in shell history and process listings.

Pull from a specific host:

```bash
azud env pull --host 203.0.113.10
```

## Registry Auth

```bash
azud registry login
azud registry logout
```

## Scaling

```bash
azud scale web=3
azud scale web=+1
azud scale web=-1
azud scale status
```

## Canary Deployments

```bash
azud canary deploy --version v1.2.3 --weight 10
azud canary status
azud canary weight 50
azud canary promote
```

## Rollbacks

```bash
azud history list
azud rollback v1.2.2
```

## Accessories

```bash
azud accessory boot postgres
azud accessory logs postgres --tail 200
azud accessory exec postgres -- psql -U postgres
```

## Cron Jobs

```bash
azud cron list
azud cron run backup
azud cron logs backup
```

## Proxy Management

```bash
azud proxy status
azud proxy logs -f
azud proxy reboot
azud proxy remove --force
```

The Caddy admin API is only reachable through the proxy container's private
unix socket, not on `localhost:2019`:

```bash
azud server exec --role web -- "podman exec azud-proxy curl -s --unix-socket /run/azud-admin/admin.sock http://localhost/config/"
```

Use `sudo -n podman` instead of `podman` for a rootful proxy managed by a
non-root SSH user. The first boot or deploy after upgrading recreates a proxy
created by an older release once.

## Systemd / Quadlet

```bash
azud systemd enable
```

Use this to auto-start services on reboot. The proxy unit resumes Caddy's
autosave, which `systemd enable` normalizes (or seeds from Azud's protected
copy) before restarting the unit; it does not mount Azud's state directory.
Rerun it after upgrading so an existing proxy unit picks up the current
container definition.

## Server Admin Commands

```bash
azud server exec --role web -- "podman ps"
azud server exec --host 203.0.113.10 -- "uptime"
```

## Related docs

- `docs/TROUBLESHOOTING.md`
- `docs/CLI_REFERENCE.md`
- `docs/PRODUCTION_CHECKLIST.md`
- `docs/CHEATSHEET.md`
