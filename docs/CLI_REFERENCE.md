# Azud CLI Reference

This document provides a comprehensive reference for the Azud command-line interface.

## Global Flags

The following flags are available for all commands:

*   `-c, --config string`: Path to the configuration file (default: `config/deploy.yml`, `deploy.yml`, or `.azud/deploy.yml`)
*   `-d, --destination string`: Destination environment (e.g., `staging`, `production`). Merges configuration from `config/deploy.staging.yml`.
*   `-v, --verbose`: Enable verbose output for debugging.

## Commands

### Initialization

#### `azud init`

Initialize a new Azud deployment configuration. Creates the necessary directory structure and configuration files.

**Usage:**
```bash
azud init [flags]
```

**Flags:**
*   `--bundle string`: Use a template bundle (e.g., `rails`, `node`).
*   `--github-actions`: Generate a GitHub Actions workflow file (`.github/workflows/deploy.yml`) for CI/CD.

**Created Files:**
*   `config/deploy.yml`: Main deployment configuration.
*   `.azud/secrets`: Local secrets file (should be git-ignored).
*   `.azud/hooks/`: Directory for deployment hooks (`pre-connect`, `pre-build`, `pre-deploy`, `post-deploy`).

**Examples:**
```bash
azud init
azud init --github-actions
azud init --bundle rails
```

---

### Setup & Deployment

#### `azud preflight`

Verify readiness of hosts and configuration before deploying.

**Usage:**
```bash
azud preflight [flags]
```

**Flags:**
*   `--host string`: Check a specific host.
*   `--role string`: Check hosts for a specific role.

**Example:**
```bash
azud preflight
```

#### `azud setup`

Bootstrap servers and perform the initial deployment. This command is typically run once when setting up a new environment.

**Usage:**
```bash
azud setup [flags]
```

**What it does:**
1.  **Bootstrap:** Installs Podman and dependencies on target servers.
2.  **Registry Login:** Logs into the configured container registry.
3.  **Proxy Boot:** Starts the Caddy reverse proxy.
4.  **Accessories:** Deploys accessory services (databases, caches, etc.).
5.  **Build & Push:** Builds and pushes the application image.
6.  **Deploy:** Deploys the application containers.

**Flags:**
*   `--skip-bootstrap`: Skip server bootstrap (Podman installation).
*   `--skip-proxy`: Skip proxy setup.
*   `--skip-push`: Skip building and pushing the image.

**Example:**
```bash
azud setup
```

#### `azud deploy`

Deploy the application to all configured servers with zero downtime.

**Usage:**
```bash
azud deploy [flags]
```

**Process:**
1.  Pulls the new image.
2.  Starts new containers.
3.  Waits for health checks to pass.
4.  Registers new containers with the proxy.
5.  Drains and removes old containers.

**Flags:**
*   `--version string`: Deploy a specific version/tag (default: `latest`).
*   `--skip-pull`: Skip pulling the image (assumes image exists locally on server).
*   `--skip-build`: Skip building the image locally.
*   `--host string`: Deploy to a specific host only.
*   `--role string`: Deploy to a specific role only.

**Examples:**
```bash
azud deploy                    # Standard deployment
azud deploy --version v1.2.3   # Deploy specific tag
azud deploy --skip-build       # Deploy existing image without building
```

### Build

#### `azud build`

Build the container image and push it to the registry.

**Usage:**
```bash
azud build [flags]
```

**Flags:**
*   `--no-push`: Don't push the image after building.
*   `--no-cache`: Don't use cache when building.
*   `--pull`: Always pull the base image.

**Examples:**
```bash
azud build                 # Build and push
azud build --no-push       # Build only, don't push
azud build --no-cache      # Build without cache
```

#### `azud redeploy`

Quickly redeploy the application without building or pushing. Useful for restarting with the same image or applying config changes.

**Usage:**
```bash
azud redeploy [flags]
```

**Flags:**
*   `--host string`: Redeploy on a specific host only.
*   `--role string`: Redeploy on a specific role only.

#### `azud rollback`

Rollback the application to a previous version.

**Usage:**
```bash
azud rollback <version> [flags]
```

**Flags:**
*   `--host string`: Rollback on a specific host only.

**Example:**
```bash
azud rollback v1.2.2
```

#### `azud history`

View deployment history records for the configured service.

**Usage:**
```bash
azud history list [--limit 20]
azud history show <id>
```

**Examples:**
```bash
azud history list
azud history list --limit 50
azud history show deploy_1739078148500123000
```

---

### Application Management

#### `azud app logs`

View logs from application containers.

**Usage:**
```bash
azud app logs [flags]
```

**Flags:**
*   `-f, --follow`: Follow log output.
*   `--tail string`: Number of lines to show (default: "100").
*   `--host string`: View logs from a specific host.
*   `--role string`: View logs from a specific role.

#### `azud app exec`

Execute a command inside a running application container.

**Usage:**
```bash
azud app exec [flags] -- <command>
```

**Flags:**
*   `-i, --interactive`: Keep STDIN open.
*   `-t, --tty`: Allocate a pseudo-TTY.
*   `--host string`: Execute on a specific host.

**Examples:**
```bash
azud app exec -- ls -la
azud app exec -it -- /bin/sh
azud app exec -- bin/rails console
```

#### `azud app start/stop/restart`

Control the application lifecycle.

**Usage:**
```bash
azud app start [flags]
azud app stop [flags]
azud app restart [flags]
```

**Flags:**
*   `--host string`: Target a specific host.

#### `azud app details`

Show detailed information about application containers, including status and stats.

**Usage:**
```bash
azud app details [flags]
```

**Flags:**
*   `--host string`: Target a specific host.

---

### Secrets & Environment

#### `azud env push`
Push secrets from local `.azud/secrets` to servers.
**Flags:** `--host`

#### `azud env pull`
Pull secrets from a server to local `.azud/secrets`.
**Flags:** `--host` (required)

#### `azud env list`
List configured environment variables.
**Flags:** `--reveal`

#### `azud env edit`
Open the secrets file in your editor.

#### `azud env get`
Get a secret value.
**Usage:** `azud env get <key>`

#### `azud env set`
Set a secret value.
**Usage:** `azud env set <key> <value> [--force]`

#### `azud env delete`
Delete a secret.
**Usage:** `azud env delete <key>`

---

### Registry Management

#### `azud registry login`
Login to the configured registry on all servers.
**Flags:** `--host`

#### `azud registry logout`
Logout from the configured registry on all servers.
**Flags:** `--host`

---

### Scaling

#### `azud scale`

Dynamically scale the number of container instances for a role.

**Usage:**
```bash
azud scale <role>=<count> [flags]
```

**Flags:**
*   `--host string`: Scale on a specific host only.

**Examples:**
```bash
azud scale web=3        # Set to exactly 3 instances
azud scale web=+1       # Add 1 instance
azud scale web=-1       # Remove 1 instance
```

#### `azud scale status`

Show the current number of running instances for each role.

**Usage:**
```bash
azud scale status
```

---

### Canary Deployments

Manage gradual rollouts using traffic splitting.

#### `azud canary deploy`

Start a canary deployment with a specific version and traffic weight.

**Usage:**
```bash
azud canary deploy --version <tag> [flags]
```

**Flags:**
*   `--version string` (Required): The version tag to deploy.
*   `--weight int`: Initial traffic percentage (0-100). Default is 10 or configured value.
*   `--skip-pull`: Skip image pull.
*   `--skip-health`: Skip health checks.

#### `azud canary promote`

Promote the canary version to full production (100% traffic) and remove the old stable version.

**Usage:**
```bash
azud canary promote
```

#### `azud canary rollback`

Rollback the canary deployment, directing all traffic back to the stable version.

**Usage:**
```bash
azud canary rollback
```

#### `azud canary weight`

Adjust the traffic percentage routed to the canary version.

**Usage:**
```bash
azud canary weight <percentage>
```

#### `azud canary status`

Show the current status of the canary deployment (versions, weight, duration).

**Usage:**
```bash
azud canary status
```

---

### Proxy Management

Manage the Caddy reverse proxy.

#### `azud proxy boot`
Start the proxy on servers.
**Flags:** `--host`

#### `azud proxy stop`
Stop the proxy.
**Flags:** `--host`

#### `azud proxy reboot`
Restart the proxy.
**Flags:** `--host`

#### `azud proxy logs`
View proxy logs.
**Flags:** `--host`, `-f/--follow`, `--tail`

#### `azud proxy status`
Show proxy status and route count.
**Flags:** `--host`

#### `azud proxy remove`
Remove the proxy container.
**Flags:** `--host`, `--force`

---

### Accessory Management

Manage accessory services (databases, caches, etc.) defined in `config/deploy.yml`.

#### `azud accessory boot`
Start an accessory.
**Usage:** `azud accessory boot <name>`

#### `azud accessory stop`
Stop an accessory.
**Usage:** `azud accessory stop <name>`

#### `azud accessory logs`
View accessory logs.
**Usage:** `azud accessory logs <name> [flags]`

#### `azud accessory exec`
Execute a command in an accessory container.
**Usage:** `azud accessory exec <name> -- <command>`

---

### Cron Jobs

Manage scheduled tasks defined in `config/deploy.yml`.

#### `azud cron boot`
Start the scheduler for cron jobs.
**Usage:** `azud cron boot [name]`

#### `azud cron run`
Run a cron job immediately (manual trigger).
**Usage:** `azud cron run <name>`

#### `azud cron list`
List configured cron jobs and their status.

#### `azud cron stop`
Stop cron jobs.
**Usage:** `azud cron stop [name]`

#### `azud cron logs`
View logs from a cron job.
**Usage:** `azud cron logs <name>`

---

### System Integration

#### `azud systemd enable`
Generate and enable systemd/quadlet units for the application and proxy. This ensures services start automatically on server reboot.

**Flags:**
*   `--host`, `--role`
*   `--no-start`: Only enable, do not start immediately.
*   `--skip-app`, `--skip-proxy`

---

### Server Management

#### `azud server bootstrap`
Install Podman and prepare servers.
**Usage:** `azud server bootstrap [hosts...]`

#### `azud server exec`
Execute a command on servers.
**Usage:** `azud server exec [flags] -- <command>`
**Flags:** `--host`, `--role`

---

### SSH Management

#### `azud ssh trust`
Fetch and add SSH host keys to your local `known_hosts` file.

**Usage:** `azud ssh trust [hosts...] [flags]`

**Flags:**
*   `--role`: Trust hosts for a specific role.
*   `--refresh`: Refresh/update existing keys.
*   `--yes`: Trust without confirmation prompt.
*   `--print`: Print fingerprints only.
*   `--template`: Print YAML snippet for `ssh.trusted_host_fingerprints`.

---

### Utilities

#### `azud config`
Display the resolved configuration (merging destination-specific configs).

#### `azud version`
Show the Azud CLI version.

---

## Configuration Reference (`config/deploy.yml`)

The configuration file is written in YAML.
For a more complete config walkthrough, see `docs/CONFIG_REFERENCE.md`.

### `service` (Required)
Name of your application. Used as the prefix for container names.
```yaml
service: my-app
```

### `image` (Required)
The container image to deploy.
```yaml
image: registry.example.com/user/my-app
```

### `servers` (Required)
Defines the target servers grouped by role. `web` is the primary role for HTTP traffic.
```yaml
servers:
  web:
    hosts:
      - 192.168.1.1
      - 192.168.1.2
    options:
      memory: "512m"
      cpus: "0.5"
  worker:
    hosts:
      - 192.168.1.3
    cmd: bin/jobs
```

### `proxy`
Configuration for the Caddy reverse proxy.
```yaml
proxy:
  host: example.com             # Single host
  # hosts: [example.com, www.example.com] # Multiple hosts
  ssl: true                     # Auto HTTPS
  app_port: 3000                # Container port
  healthcheck:
    path: /up
    interval: 3s
```

### `registry`
Container registry authentication.
```yaml
registry:
  server: ghcr.io
  username: user
  password:
    - GITHUB_TOKEN # References env var or secret
```

### `env`
Environment variables.
```yaml
env:
  clear:
    RAILS_ENV: production
  secret:
    - DATABASE_URL
```

### `accessories`
Sidecar services like databases.
```yaml
accessories:
  redis:
    image: redis:alpine
    host: 192.168.1.1
    port: 6379
```

### `cron`
Scheduled tasks.
```yaml
cron:
  backup:
    schedule: "0 2 * * *"
    command: bin/backup
```

### `ssh`
SSH connection details.
```yaml
ssh:
  user: root
  port: 22
```
