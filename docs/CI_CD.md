# CI/CD Integration Guide

Azud is designed with CI/CD in mind. Its stateless CLI and configuration-as-code approach make it perfect for automated deployment pipelines.

## GitHub Actions

Azud provides native support for GitHub Actions.

### 1. Generate Workflow

You can generate a starter workflow using the `init` command:

```bash
azud init --github-actions
```

This creates `.github/workflows/deploy.yml`.

### 2. Configure Secrets

For the pipeline to work, you need to set up the following secrets in your GitHub repository settings (**Settings** > **Secrets and variables** > **Actions**).

| Secret Name | Description |
|-------------|-------------|
| `AZUD_SSH_KEY` | The private SSH key used to connect to your servers. The corresponding public key must be in `~/.ssh/authorized_keys` on your servers. |
| `AZUD_REGISTRY_PASSWORD` | Password/Token for your container registry (e.g., GitHub Personal Access Token for GHCR). |
| `DATABASE_PASSWORD` | (Example) Your application secrets. |
| `RAILS_MASTER_KEY` | (Example) Any other secrets defined in your `config/deploy.yml`. |

### 3. Example Workflow

Here is a typical workflow configuration:

```yaml
name: Deploy

on:
  push:
    branches: [main]

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Install Azud
        run: |
          curl -fsSL https://get.azud.dev | sh
          echo "$HOME/.azud/bin" >> $GITHUB_PATH

      - name: Setup SSH
        run: |
          mkdir -p ~/.ssh
          echo "${{ secrets.AZUD_SSH_KEY }}" > ~/.ssh/id_ed25519
          chmod 600 ~/.ssh/id_ed25519
          # Scan host keys to prevent interactive prompt
          ssh-keyscan -H 192.168.1.1 >> ~/.ssh/known_hosts

      - name: Create Secrets File
        run: |
          mkdir -p .azud
          cat > .azud/secrets << 'EOF'
          AZUD_REGISTRY_PASSWORD=${{ secrets.AZUD_REGISTRY_PASSWORD }}
          DATABASE_PASSWORD=${{ secrets.DATABASE_PASSWORD }}
          EOF
          chmod 600 .azud/secrets

      - name: Login to Registry
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Build and Push
        run: azud build --no-cache

      - name: Deploy
        run: azud deploy --skip-build
```

---

## GitLab CI

You can also use Azud with GitLab CI. Here is a sample `.gitlab-ci.yml`:

```yaml
stages:
  - deploy

deploy:
  stage: deploy
  image: golang:1.21
  only:
    - main
  before_script:
    # Install Azud
    - curl -fsSL https://get.azud.dev | sh
    - export PATH=$PATH:$HOME/.azud/bin
    
    # Setup SSH
    - mkdir -p ~/.ssh
    - echo "$SSH_PRIVATE_KEY" > ~/.ssh/id_ed25519
    - chmod 600 ~/.ssh/id_ed25519
    - ssh-keyscan -H $DEPLOY_HOST >> ~/.ssh/known_hosts

    # Setup Secrets
    - mkdir -p .azud
    - echo "AZUD_REGISTRY_PASSWORD=$CI_REGISTRY_PASSWORD" >> .azud/secrets
    - echo "DATABASE_PASSWORD=$DATABASE_PASSWORD" >> .azud/secrets
    - chmod 600 .azud/secrets

  script:
    - azud setup --skip-bootstrap # or azud deploy
```

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

### 3. Environment Secrets
Keep your `.azud/secrets` file out of version control. In CI, reconstruct this file dynamically from the CI system's secret store (as shown in the examples above).

### 4. Known Hosts
To avoid "Host key verification failed" errors, use `ssh-keyscan` in your CI script to populate `~/.ssh/known_hosts`, or set `ssh.insecure_ignore_host_key: true` (less secure) in your `config/deploy.yml` if your internal policy allows.

---

## Related docs

- `docs/SECURITY.md`
- `docs/CLI_REFERENCE.md`
