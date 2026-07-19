# CI/CD Integration Guide

Azud is designed for automated deployment pipelines. Configuration is kept in
version control, while deployment history and in-progress canary state are
durable operational data and must survive clean CI checkouts.

## GitHub Actions

### Using the Azud Action (Recommended)

The easiest way to deploy with Azud from GitHub Actions is the official composite action.

#### 1. Generate Workflow

```bash
azud init --github-actions
```

This creates `.github/workflows/deploy.yml` with everything pre-configured.

#### 2. Configure Secrets

Add these secrets in your GitHub repository (**Settings** > **Secrets and variables** > **Actions**):

| Secret Name | Description |
|-------------|-------------|
| `AZUD_SSH_KEY` | SSH private key for connecting to your servers |
| `KNOWN_HOSTS` | Output of `ssh-keyscan your-server-ip` |
| `AZUD_SECRET_AZUD_REGISTRY_PASSWORD` | Registry password/token |
| `AZUD_SECRET_DATABASE_PASSWORD` | (Example) Your application secrets |
| `AZUD_SECRET_RAILS_MASTER_KEY` | (Example) Any other secrets from `config/deploy.yml` |

Secrets prefixed with `AZUD_SECRET_` are automatically loaded when using `secrets_provider: env` (see below).

#### 3. Example Workflow

```yaml
name: Deploy

on:
  push:
    branches: [main]

permissions:
  contents: read
  packages: write

concurrency:
  group: azud-deploy-${{ github.repository }}-${{ github.ref_name }}
  cancel-in-progress: false

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@df4cb1c069e1874edd31b4311f1884172cec0e10 # v6

      - name: Restore Azud deployment state
        uses: actions/cache@caa296126883cff596d87d8935842f9db880ef25 # v5
        with:
          path: ~/.local/share/azud
          key: azud-state-${{ runner.os }}-${{ github.repository_id }}-${{ github.ref_name }}-${{ github.run_id }}
          restore-keys: |
            azud-state-${{ runner.os }}-${{ github.repository_id }}-${{ github.ref_name }}-

      - name: Setup Azud
        uses: lemonity-org/azud@v1
        with:
          ssh-key: ${{ secrets.AZUD_SSH_KEY }}
          known-hosts: ${{ secrets.KNOWN_HOSTS }}
          registry-server: ghcr.io
          registry-username: ${{ github.actor }}
          registry-password: ${{ secrets.GITHUB_TOKEN }}

      - name: Deploy
        env:
          AZUD_SECRET_AZUD_REGISTRY_PASSWORD: ${{ secrets.AZUD_REGISTRY_PASSWORD }}
          AZUD_SECRET_DATABASE_PASSWORD: ${{ secrets.DATABASE_PASSWORD }}
        run: azud deploy
```

#### Action Inputs

| Input | Description | Default |
|-------|-------------|---------|
| `version` | Azud version to install | `latest` |
| `ssh-key` | SSH private key (triggers SSH setup) | — |
| `known-hosts` | Known hosts content | — |
| `registry-server` | Container registry server | — |
| `registry-username` | Registry username | — |
| `registry-password` | Registry password/token | — |

### Using `secrets_provider: env`

Instead of creating a `.azud/secrets` file in CI, you can configure Azud to read secrets directly from environment variables. Add this to your `config/deploy.yml`:

```yaml
secrets_provider: env
secrets_env_prefix: AZUD_SECRET_
```

With this configuration, an environment variable like `AZUD_SECRET_DATABASE_PASSWORD` is mapped to the secret `DATABASE_PASSWORD`. This is the recommended approach for CI/CD because:

- No file creation step needed
- Secrets never touch disk
- Works natively with GitHub Actions secrets, GitLab CI variables, etc.

### Alternative: curl-based Install

If you prefer not to use the composite action, you can install Azud directly:

```yaml
- name: Install Azud
  run: |
    curl -fsSL https://raw.githubusercontent.com/lemonity-org/azud/v1/scripts/install.sh | sh
    echo "$HOME/.azud/bin" >> $GITHUB_PATH

- name: Setup SSH
  run: |
    mkdir -p ~/.ssh
    echo "${{ secrets.AZUD_SSH_KEY }}" > ~/.ssh/id_ed25519
    chmod 600 ~/.ssh/id_ed25519
    ssh-keyscan -H your-server-ip >> ~/.ssh/known_hosts
```

The installer requires the GitHub CLI so it can verify the binary's signed
build provenance before installation.

## Durable deployment state

Azud writes history, locks, and canary state under the platform state directory
(`~/.local/share/azud` for a normal Unix user, `/var/lib/azud` for root). Set
`AZUD_STATE_DIR` to an absolute path to override it. State writes are atomic and
serialized, but the storage itself must be shared across CI runs.

The generated GitHub workflow serializes deploys and restores/saves that
directory with `actions/cache`. GitHub caches have lifecycle and quota limits,
so production environments that require guaranteed long-term rollback should
use a persistent self-hosted runner volume (or an explicit artifact/object-store
restore step) and set, for example:

```yaml
env:
  AZUD_STATE_DIR: /mnt/deploy-state/azud
```

Do not run concurrent pipelines for the same service and state directory.
Without the prior state, Azud cannot safely infer an earlier stable version or
resume an in-progress canary, and those operations intentionally fail instead
of guessing.

---

## Docker Image

Azud publishes a Docker image to `ghcr.io/lemonity-org/azud` with every release. This is useful for container-based CI systems like GitLab CI.

```
ghcr.io/lemonity-org/azud:latest
ghcr.io/lemonity-org/azud:v1.0.0
```

## GitLab CI

```yaml
stages:
  - deploy

deploy:
  stage: deploy
  image: ghcr.io/lemonity-org/azud:latest
  only:
    - main
  variables:
    AZUD_SECRET_AZUD_REGISTRY_PASSWORD: $CI_REGISTRY_PASSWORD
    AZUD_SECRET_DATABASE_PASSWORD: $DATABASE_PASSWORD
  before_script:
    - mkdir -p ~/.ssh
    - echo "$SSH_PRIVATE_KEY" > ~/.ssh/id_ed25519
    - chmod 600 ~/.ssh/id_ed25519
    - ssh-keyscan -H $DEPLOY_HOST >> ~/.ssh/known_hosts
  script:
    - azud deploy
```

Ensure your `config/deploy.yml` includes:

```yaml
secrets_provider: env
secrets_env_prefix: AZUD_SECRET_
```

---

## Best Practices

### 1. Use a Dedicated Deploy Key
Do not use your personal SSH key. Generate a new SSH key specifically for your CI/CD runner and add its public key to your servers.

```bash
ssh-keygen -t ed25519 -C "ci@azud" -f azud-deploy-key
```

### 2. Separate Build and Deploy Steps
In larger pipelines, you might want to separate the build and deploy jobs.
1.  **Build Job:** Build the image and push to registry using `azud build` or `docker build`.
2.  **Deploy Job:** Run `azud deploy --skip-build`.

### 3. Use `secrets_provider: env`
In CI environments, prefer `secrets_provider: env` over creating a `.azud/secrets` file. This avoids writing secrets to disk and integrates naturally with your CI system's secret management.

### 4. Persist and Serialize State

Use one durable `AZUD_STATE_DIR` per deployment environment and serialize
deployments that write it. The generated GitHub Actions workflow includes both
the cache and `concurrency` configuration.

### 5. Known Hosts
To avoid "Host key verification failed" errors, use `ssh-keyscan` to populate `~/.ssh/known_hosts`, or pass `known-hosts` to the Azud action. You can also set `ssh.insecure_ignore_host_key: true` in your `config/deploy.yml` (less secure) if your internal policy allows.

---

## Related docs

- `docs/SECURITY.md`
- `docs/CLI_REFERENCE.md`
