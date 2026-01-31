# Getting Started

This guide takes you from zero to a first successful deploy.

## 1) Install the CLI

```bash
go install github.com/adriancarayol/azud@latest
```

## 2) Initialize your project

```bash
azud init
```

This creates:

- `config/deploy.yml` (main config)
- `.azud/secrets` (local secrets; keep out of git)
- `.azud/hooks/` (optional deployment hooks)

## 3) Configure a minimal deploy

Edit `config/deploy.yml`:

```yaml
service: my-app
image: ghcr.io/your-org/my-app

servers:
  web:
    hosts:
      - 203.0.113.10

proxy:
  hosts:
    - example.com
  ssl: true
  app_port: 3000
  healthcheck:
    path: /up

env:
  clear:
    RAILS_ENV: production
  secret:
    - DATABASE_URL
```

Put secrets in `.azud/secrets`:

```
DATABASE_URL=postgres://...
```

## 4) First setup and deploy

Optional but recommended preflight:

```bash
azud preflight
```

```bash
azud setup
```

This bootstraps servers, starts the proxy, builds/pushes the image, and deploys.

## 5) Verify

- Visit `https://example.com`
- Check logs: `azud app logs --tail 200`
- Inspect containers: `azud app details`

## 6) Deploy updates

```bash
azud deploy
```

## 6b) Environments (staging, production)

Create a destination-specific file like `config/deploy.staging.yml` with overrides:

```yaml
servers:
  web:
    hosts:
      - 203.0.113.20

proxy:
  hosts:
    - staging.example.com
```

Then deploy with:

```bash
azud deploy --destination staging
```

## 6c) Secrets providers

By default, secrets are read from `.azud/secrets`. You can also load secrets from:

- Environment variables (`secrets_provider: env`, `secrets_env_prefix`)
- A command output (`secrets_provider: command`, `secrets_command`)

See `docs/SECURITY.md` for recommendations.

## 6d) Accessories and cron

Example accessory (Postgres):

```yaml
accessories:
  postgres:
    image: postgres:15
    host: 203.0.113.10
    port: 5432
    env:
      secret:
        - POSTGRES_PASSWORD
```

Start accessories:

```bash
azud accessory boot postgres
```

Example cron job:

```yaml
cron:
  nightly_backup:
    schedule: "0 2 * * *"
    command: bin/backup
```

## 7) Roll back if needed

```bash
azud rollback <version>
```

Find the version tag from your deploy output or image tags.

## Next steps

- `docs/OPERATIONS.md` - day-2 tasks (logs, scaling, canary)
- `docs/HOW_IT_WORKS.md` - deployment model and proxy behavior
- `docs/ADVANCED_USAGE.md` - accessories, volumes, and recipes
- `docs/PRODUCTION_CHECKLIST.md` - production readiness checklist
- `docs/CHEATSHEET.md` - quick command reference
