# Migration from Dokku

Move a typical Dokku app to Azud with minimal downtime.

## 1) Create Azud config

```bash
azud init
```

Set `service`, `image`, and `servers` in `config/deploy.yml`.

## 2) Map domains

For each Dokku app domain, add it to `proxy.hosts`:

```yaml
proxy:
  hosts:
    - app.example.com
  ssl: true
  app_port: 3000
```

## 3) Environment variables

Add clear env vars to `env.clear` and secrets to `.azud/secrets`.

## 4) Move data

If you used Dokku plugins (Postgres, Redis, etc.), migrate data to:

- A managed service, or
- An Azud accessory (defined in `accessories`)

## 5) First deploy

```bash
azud setup
```

Update DNS to point to the Azud server(s).

## 6) Differences to keep in mind

- Azud uses a stateless CLI and config files instead of git push
- Azud has built-in zero-downtime deploys via blue-green
- Caddy manages HTTPS automatically

## Related docs

- `docs/GETTING_STARTED.md`
- `docs/ADVANCED_USAGE.md`
