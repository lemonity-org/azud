# Advanced Usage & Recipes

This guide covers advanced configuration scenarios and common recipes for Azud.

## Deployment Hooks

Hooks allow you to execute custom scripts at specific points in the deployment lifecycle. They are useful for tasks like running database migrations, notifying Slack/Discord, or cleaning up temporary files.

Hooks are executable scripts located in `.azud/hooks/`.

### Available Hooks

| Hook Name | Description |
|-----------|-------------|
| `pre-connect` | Runs locally before establishing SSH connections. |
| `pre-build` | Runs locally before building the container image. |
| `pre-deploy` | Runs locally before starting the deployment process on servers. |
| `post-deploy` | Runs locally after a successful deployment. |

### Example: Slack Notification

Create `.azud/hooks/post-deploy`:

```bash
#!/bin/bash
# Notify Slack on successful deployment

WEBHOOK_URL="https://hooks.slack.com/services/..."
VERSION=$(azud config | grep Image | awk '{print $2}')

curl -X POST -H 'Content-type: application/json' --data "{
  \"text\": \"🚀 Deployment Successful!\nService: My App\nVersion: $VERSION\" 
}" $WEBHOOK_URL
```

Make it executable:
```bash
chmod +x .azud/hooks/post-deploy
```

---

## Persistent Storage (Volumes)

To persist data (like user uploads or SQLite databases) across deployments, use the `volumes` configuration in `config/deploy.yml`.

```yaml
# config/deploy.yml
volumes:
  - /var/lib/my-app/storage:/app/storage
  - /var/lib/my-app/uploads:/app/public/uploads
```

**Note:** Ensure the directory `/var/lib/my-app/storage` exists on the server and has the correct permissions for the user inside the container (usually UID 1000 or root, depending on your image).

---

## Private Registries

To use a private registry (like GHCR, Docker Hub Private, or AWS ECR):

1.  **Configure Registry in `deploy.yml`:**
    ```yaml
    registry:
      server: ghcr.io
      username: my-github-username
      password:
        - AZUD_REGISTRY_PASSWORD # Reference to secret
    ```

2.  **Add Password to Secrets:**
    Add `AZUD_REGISTRY_PASSWORD=your_token` to your `.azud/secrets` file.

3.  **Deploy:** Azud will automatically log in to the registry on all servers during setup/deployment.

---

## SSH Bastion / Jump Host

If your target servers are in a private network (VPC) and only accessible via a bastion host:

```yaml
# config/deploy.yml
ssh:
  user: ubuntu
  proxy:
    host: bastion.example.com
    user: admin
    key: ~/.ssh/bastion_key # Optional
```

Azud will tunnel all connections through the bastion.

---

## Recipes

### PostgreSQL Database (Accessory)

Deploy a managed PostgreSQL instance alongside your app.

```yaml
# config/deploy.yml
accessories:
  postgres:
    image: postgres:15
    host: 192.168.1.1
    port: 5432
    env:
      clear:
        POSTGRES_DB: my_app_production
        POSTGRES_USER: my_app
      secret:
        - POSTGRES_PASSWORD
    volumes:
      - /var/lib/postgresql/data:/var/lib/postgresql/data
```

**Secrets:**
Add `POSTGRES_PASSWORD=secure_password` to `.azud/secrets`.

### Redis Cache

```yaml
accessories:
  redis:
    image: redis:7-alpine
    host: 192.168.1.1
    port: 6379
    cmd: "redis-server --appendonly yes --requirepass $REDIS_PASSWORD"
    env:
      secret:
        - REDIS_PASSWORD
    volumes:
      - /var/lib/redis/data:/data
```

### Worker Servers (Sidekiq / Background Jobs)

Run background workers on a separate set of servers (or the same servers with a different command).

```yaml
servers:
  web:
    hosts: [192.168.1.1]
  
  worker:
    hosts: [192.168.1.2] # Can be same as web
    cmd: bundle exec sidekiq
    options:
      memory: 1g
```

### Multiple Services on the Same Server

When deploying multiple services to the same host, each service must have its own
`secrets_remote_path` to avoid overwriting the other's secrets. By default all
services share `$HOME/.azud/secrets`.

```yaml
# my-api/config/deploy.yml
service: my-api
secrets_remote_path: $HOME/.azud/my-api/secrets

servers:
  web:
    hosts:
      - 203.0.113.10

# my-web/config/deploy.yml
service: my-web
secrets_remote_path: $HOME/.azud/my-web/secrets

servers:
  web:
    hosts:
      - 203.0.113.10
```

After setting distinct paths, run `azud env push` from each project directory.

### Custom Caddy Configuration

To customize Caddy beyond the defaults, you can inject custom configuration snippets (future feature) or manage the `Caddyfile` manually if you opt out of the managed proxy.

Currently, Azud manages the `Caddyfile` entirely. If you need complex routing, you might consider running your own proxy and pointing it to Azud's containers, though you lose the automatic zero-downtime integration that Azud's proxy manager provides.

For simple needs like increasing body size limits, check the `proxy` options:

```yaml
proxy:
  # ...
  # Future versions will support:
  # config_snippet: |
  #   request_body {
  #     max_size 20MB
  #   }
```

## Related docs

- `docs/OPERATIONS.md`
- `docs/CONFIG_REFERENCE.md`
