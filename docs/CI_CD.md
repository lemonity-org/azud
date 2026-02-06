# CI/CD Integration Guide

Azud is designed with CI/CD in mind. Its stateless CLI and configuration-as-code approach make it perfect for automated deployment pipelines.

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

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6

      - name: Setup Azud
        uses: adriancarayol/azud@v1
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
    curl -fsSL https://raw.githubusercontent.com/adriancarayol/azud/main/scripts/install.sh | sh
    echo "$HOME/.azud/bin" >> $GITHUB_PATH

- name: Setup SSH
  run: |
    mkdir -p ~/.ssh
    echo "${{ secrets.AZUD_SSH_KEY }}" > ~/.ssh/id_ed25519
    chmod 600 ~/.ssh/id_ed25519
    ssh-keyscan -H your-server-ip >> ~/.ssh/known_hosts
```

---

## Docker Image

Azud publishes a Docker image to `ghcr.io/adriancarayol/azud` with every release. This is useful for container-based CI systems like GitLab CI.

```
ghcr.io/adriancarayol/azud:latest
ghcr.io/adriancarayol/azud:v1.0.0
```

## GitLab CI

```yaml
stages:
  - deploy

deploy:
  stage: deploy
  image: ghcr.io/adriancarayol/azud:latest
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

### 4. Known Hosts
To avoid "Host key verification failed" errors, use `ssh-keyscan` to populate `~/.ssh/known_hosts`, or pass `known-hosts` to the Azud action. You can also set `ssh.insecure_ignore_host_key: true` in your `config/deploy.yml` (less secure) if your internal policy allows.

---

## Related docs

- `docs/SECURITY.md`
- `docs/CLI_REFERENCE.md`
