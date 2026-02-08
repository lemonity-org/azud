# Operations Guide

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
azud env set DATABASE_URL postgres://...
azud env push
```

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

## Systemd / Quadlet

```bash
azud systemd enable
```

Use this to auto-start services on reboot.

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
