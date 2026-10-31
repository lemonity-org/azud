# Azud CLI Reference

This document provides a comprehensive reference for the Azud command-line interface.

## Global Flags

The following flags are available for all commands:

*   `-c, --config string`: Path to the configuration file (default: `config/deploy.yml`, `deploy.yml`, or `.azud/deploy.yml`)
*   `-d, --destination string`: Destination environment (e.g., `staging`, `production`). Merges configuration from `config/deploy.staging.yml`. Names start with a letter or digit and contain only letters, digits, dots, underscores, and hyphens (max 63 characters, no `..`). A destination without an override file uses the base configuration; an override file that exists but cannot be read is an error.
*   `-v, --verbose`: Enable verbose output for debugging.

## Output and Automation

Interactive output uses compact status labels and functional ANSI color when
the destination supports it. Pipes, files, GitHub Actions logs, `TERM=dumb`,
and non-TTY destinations receive deterministic ASCII framing without ANSI
escapes; UTF-8 values remain intact. `NO_COLOR` and `CLICOLOR=0` also disable
color.

Remote output from containers and hosts is untrusted. Azud neutralizes
terminal control sequences in it (see
[Remote output](OUTPUT.md#remote-output)); log and non-interactive exec
commands accept `--raw` to print remote bytes unchanged.

Use stable machine surfaces instead of scraping display output:

```bash
azud version --short
azud env get DATABASE_PASSWORD
azud ssh trust --template
```

See [`OUTPUT.md`](OUTPUT.md) for render modes, labels, and compatibility
guarantees.

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
*   `.azud/secrets`: Local secrets file (mode `0600` in a `0700` `.azud` directory; should be git-ignored). An existing file is kept.
*   `.azud/hooks/`: Directory for deployment hooks (`pre-connect`, `pre-build`, `pre-deploy`, `post-deploy`). Existing hooks are kept.
*   `.gitignore`: Gains exact `.azud/secrets` and `.azud/.secrets-*.tmp` rules (the latter covers temporary copies an interrupted secrets write can leave behind) unless equivalent rules are already active. A symlinked `.gitignore` is refused.
*   `.dockerignore`: Excludes `.azud/`, `.git/`, and `.azud/secrets` from images. If a container ignore file already exists, the missing rules are appended to it instead; init never creates a `.containerignore` that would shadow an existing `.dockerignore`. A symlinked container ignore file is left unchanged with a warning, so add the rules to it yourself; local builds exclude `.azud/secrets` either way.

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

Local checks run first: security policies, required secret references, and,
with the file secrets provider, the secrets file mode. Preflight fails when the
secrets file is readable by group or others; run `chmod 600` on it.

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

#### `azud agent run`

Run on the server in pull mode: follow `agent.track` and deploy each new
image on this server after `agent.verify_command` passes. See
`docs/PULL_MODE.md`.

**Usage:**
```bash
azud agent run [flags]
```

**Flags:**
*   `--once`: Check the tag once and exit. Exits non-zero if the deploy fails
    or the tag points to a release that failed before.
*   `--retry-failed`: With `--once`, retry a release that failed before.

**Examples:**
```bash
azud agent run                        # Run until stopped (systemd)
azud agent run --once                 # Check the tag once
azud agent run --once --retry-failed  # Retry a release that failed
```

#### `azud agent status`

Show the tag the agent follows, the digest it last saw, the digest it
deployed, and the last failure.

#### `azud agent init`

Create this server's age identity for release bundles (default
`~/.config/azud/agent/<service>.age`, mode 0600) and print its public
recipient for `release.recipients`. An existing identity is kept.

```bash
azud agent init -d agent
```

`azud agent run --once --allow-downgrade` deploys a bundle older than the
deployed one: the rollback path for `agent.source: bundle`.

#### `azud agent unit`

Print a systemd user unit that runs `azud agent run` with the current
configuration, destination, and working directory.

```bash
mkdir -p ~/.config/systemd/user
azud agent unit > ~/.config/systemd/user/azud-agent-myapp.service
systemctl --user daemon-reload
systemctl --user enable --now azud-agent-myapp.service
```

#### `azud release publish`

Build a release bundle for an image and push it to the image's repository,
by digest only. The bundle carries `config/deploy.yml` and the destination
file as written, and the runtime secrets `azud env push` would write,
encrypted to `release.recipients`. It never tags.

```bash
azud release publish --image sha256:... -d production --digest-file bundle.digest
```

| Flag | Meaning |
|---|---|
| `--image` | Digest of the image to release (required) |
| `--commit` | Commit to record (default: `$GITHUB_SHA`, then `git rev-parse HEAD`) |
| `--digest-file` | Also write the bundle digest to this file |

#### `azud release promote`

Point a tag at a published bundle of this service, after signing it. The
registry password is read from the environment.

```bash
azud release promote sha256:... --track production
```

#### `azud release inspect`

Show a bundle's image, commit, secret names, recipients and configuration
files. Secret values are never decrypted. `--config` prints the files.

```bash
azud release inspect production
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
*   `--raw`: Print remote output byte-for-byte without neutralizing terminal
    control sequences.

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
*   `--raw`: Print output byte-for-byte even when stdout or stderr is a
    terminal. Piped and redirected output is always byte-exact, and `-i`/`-t`
    sessions are never filtered.

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
Push secrets from local `.azud/secrets` to servers. Secrets go only to hosts
that run application, accessory, or cron containers; `builder.remote.host` and
`ssh.proxy.host` do not receive them unless they also run one of those
workloads. `--host` must name one of those workload hosts. A secret value that
contains a CR, LF, or NUL is rejected, naming only the key, before any host is
contacted.
**Flags:** `--host`

#### `azud env pull`
Pull secrets from a server to local `.azud/secrets`. `--host` must name a
server, accessory, or cron host. A missing remote secrets file exits `0` with a
"No secrets file" message. A remote file that cannot be read (permissions,
including a parent directory the SSH user cannot search, I/O, SSH timeout)
exits non-zero and leaves the local file untouched.
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
**Usage:** `azud env set <key> [value] [--force]`

When `value` is omitted, it is read from stdin: a prompt without echo on a
terminal, or one line from a pipe (a single trailing newline is stripped). The
`<key> <value>` form still works but prints a deprecation warning on stderr,
because argv values end up in shell history and process listings. Keys must
match `[A-Za-z_][A-Za-z0-9_]*`; values must not contain CR, LF, or NUL and
are limited to 60 KiB, so every line stays readable by Azud and by
`podman run --env-file`. Without `--force`, an existing key is left
unchanged and no value is read.

```bash
azud env set DATABASE_PASSWORD
printf '%s' "$DATABASE_PASSWORD" | azud env set DATABASE_PASSWORD --force
```

#### `azud env delete`
Delete a secret.
**Usage:** `azud env delete <key>`

---

### Registry Management

#### `azud registry login`
Login to the configured registry on all servers. `--host` must name a
configured server, accessory, cron, or `builder.remote.host` host; the SSH
bastion and unconfigured hosts are rejected before the password is read.
**Flags:** `--host`

#### `azud registry logout`
Logout from the configured registry on all servers. `--host` accepts the same
hosts as `registry login`.
**Flags:** `--host`

---

### Scaling

#### `azud scale`

Dynamically scale the number of container instances for a role.
Roles configured with `strategy: stop_first` are intentionally rejected because
their lifecycle is limited to zero or one process.

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
Exits non-zero if the proxy cannot be stopped on any selected host. A host
without a proxy container counts as stopped. A proxy managed by a systemd
(Quadlet) unit is not stopped with Podman, because the unit's `Restart=always`
would start it again; the command fails and prints the `systemctl stop` command
to use instead.
**Flags:** `--host`

#### `azud proxy reboot`
Restart the proxy. A proxy container created by an older Azud release is
recreated with the current definition (private admin socket and hardening);
routes are kept through Caddy's autosave. If the new container does not
answer, the previous one is restored and the command fails.
**Flags:** `--host`

#### `azud proxy logs`
View proxy logs.
**Flags:** `--host`, `-f/--follow`, `--tail`, `--raw`

#### `azud proxy status`
Show proxy status and route count.
**Flags:** `--host`

#### `azud proxy reconcile`
Compare the configured service, running Azud-managed web containers, persisted
canary state, and the service's ID-owned Caddy route.

```bash
azud proxy reconcile --check
azud proxy reconcile --repair
```

`--check` is read-only and exits nonzero when drift is present. `--repair`
creates, updates, adopts a legacy ID-less route, or removes a stale ID-owned
route. Routes owned by other IDs and manual routes are left untouched.
**Flags:** exactly one of `--check` or `--repair`; optional `--host`.

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
**Flags:** `--host`, `-f/--follow`, `--tail`, `--raw`

#### `azud accessory exec`
Execute a command in an accessory container.
**Usage:** `azud accessory exec <name> -- <command>`
**Flags:** `--host`, `--raw`

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
**Flags:** `--host`, `-f/--follow`, `--tail`, `--raw`

---

### System Integration

#### `azud systemd enable`
Generate and install systemd/quadlet units for the application and proxy. This improves reboot reliability and can start services immediately unless `--no-start` is set.

**Flags:**
*   `--host`, `--role`
*   `--no-start`: Only enable, do not start immediately.
*   `--skip-app`, `--skip-proxy`

Notes:
*   With `podman.rootless: true` + `proxy.rootful: true`, app units stay rootless while the proxy unit is installed as a system unit.
*   When `ssh.user` is non-root and a system unit is managed, passwordless `sudo` is required for `systemctl` and quadlet file writes.
*   The proxy unit resumes Caddy's autosave. Before starting it, `systemd enable` stops the previous proxy unit or container, rewrites the autosave's admin listener to the private socket (or seeds the autosave from Azud's protected copy), and then starts the unit. If that preparation fails, the unit is still started so the host keeps a proxy, and the command fails. With `--no-start` only the autosave is prepared. Rerun it after upgrading Azud; Boot refuses to recreate a systemd-managed proxy itself.

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

`azud config` validates the configuration but does not load secrets: it does
not run `secrets_command` or read the secrets file. Use `azud env list` to
check that secrets load. Shell completion (`azud completion <shell>` and the
completion requests your shell sends) does not load the configuration at all.

#### `azud version`
Show the Azud CLI version.

Use `azud version --short` to print only the unstyled version value.

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
    strategy: stop_first
    runtime:
      user: "10001:10001"
      read_only: true
      cap_drop: [ALL]
      no_new_privileges: true
      disable_healthcheck: true
      stop_timeout: 30s
      tmpfs:
        - path: /tmp
          size: 64m
          mode: "1777"
```

Role `strategy` defaults to `rolling`. Use `stop_first` for a non-web
singleton that must not overlap its previous process. The typed `runtime`
mapping supports `user`, `read_only`, `cap_drop`, `no_new_privileges`,
bounded `tmpfs`, `disable_healthcheck`, and `stop_timeout`; Azud validates
these fields before constructing Podman commands. `stop_first` requires exactly
one host, cannot be managed with `azud scale`, and is refused while the role's
systemd unit is active. See
[Stop timeouts](CONFIG_REFERENCE.md#stop-timeouts) for how `stop_timeout`
interacts with `ssh.command_timeout`.

### `proxy`
Configuration for the Caddy reverse proxy.
```yaml
proxy:
  host: example.com             # Single host
  # hosts: [example.com, www.example.com] # Multiple hosts
  ssl: true                     # Auto HTTPS
  acme_email: ops@example.com   # Required when SSL is enabled
  # rootful: true               # Run proxy with rootful Podman (supports 80/443 with rootless app mode)
  app_port: 3000                # Container port
  upstream_protocol: http       # http, h2c, or https
  healthcheck:
    path: /up
    # readiness_cmd: "grpc_health_probe -addr 127.0.0.1:3000"
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
