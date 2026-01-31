# Azud Cheatsheet

Quick reference for common tasks.

## Setup and Deploy

```bash
azud init
azud preflight
azud setup
azud deploy
azud rollback <version>
```

## Builds

```bash
azud build
azud build --no-push
azud deploy --skip-build
```

## Logs and Debug

```bash
azud app logs --tail 200
azud app logs -f
azud app details
azud proxy logs -f
```

## Exec

```bash
azud app exec -- <command>
azud app exec -it -- /bin/sh
```

## Scaling and Canary

```bash
azud scale web=3
azud scale web=+1
azud canary deploy --version v1.2.3 --weight 10
azud canary weight 50
azud canary promote
azud canary rollback
```

## Secrets

```bash
azud env list
azud env set DATABASE_URL postgres://...
azud env push
azud env pull --host 203.0.113.10
```

## Registry

```bash
azud registry login
azud registry logout
```

## Server Admin

```bash
azud server exec --role web -- "podman ps"
azud server bootstrap
```

## Proxy

```bash
azud proxy status
azud proxy reboot
azud proxy remove --force
```

## Cron

```bash
azud cron list
azud cron run backup
azud cron logs backup
```

## Systemd

```bash
azud systemd enable
```
