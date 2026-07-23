package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/output"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Create a new deployment configuration",
	Long: `Initialize a new Azud deployment configuration.

This creates:
  - config/deploy.yml: Main deployment configuration
  - .azud/secrets: File to store secrets
  - .azud/hooks/: Directory for hook scripts

Options:
  --bundle        Use a template bundle (e.g., rails, node)
  --github-actions Create a GitHub Actions workflow for CI/CD

You can specify a bundle to use a template from the Azud registry.

Example:
  azud init
  azud init --github-actions
  azud init --bundle rails`,
	RunE: runInit,
}

var (
	initBundle        string
	initGitHubActions bool
)

func init() {
	initCmd.Flags().StringVar(&initBundle, "bundle", "", "Template bundle to use (e.g., rails, node)")
	initCmd.Flags().BoolVar(&initGitHubActions, "github-actions", false, "Include GitHub Actions workflow")
}

func runInit(cmd *cobra.Command, args []string) error {
	log := output.NewLogger(cmd.OutOrStdout(), cmd.ErrOrStderr(), verbose)

	// Check if config already exists
	if existingConfig := findConfigFile(); existingConfig != "" {
		return fmt.Errorf("configuration already exists at %s", existingConfig)
	}

	log.Header("Initialize / configuration")

	// Create directories
	dirs := []string{
		"config",
		".azud",
		".azud/hooks",
	}

	if initGitHubActions {
		dirs = append(dirs, ".github/workflows")
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Create deploy.yml
	configPath := filepath.Join("config", "deploy.yml")
	if err := os.WriteFile(configPath, []byte(getConfigTemplate(initGitHubActions)), 0644); err != nil {
		return fmt.Errorf("failed to create %s: %w", configPath, err)
	}
	log.Success("Created %s", configPath)

	// Create secrets file
	secretsPath := filepath.Join(".azud", "secrets")
	if err := os.WriteFile(secretsPath, []byte(getSecretsTemplate()), 0600); err != nil {
		return fmt.Errorf("failed to create %s: %w", secretsPath, err)
	}
	log.Success("Created %s", secretsPath)

	// Create sample hook scripts
	hooks := map[string]string{
		"pre-connect":       getPreConnectHook(),
		"pre-build":         getPreBuildHook(),
		"post-build":        getPostBuildHook(),
		"pre-deploy":        getPreDeployHook(),
		"pre-app-boot":      getPreAppBootHook(),
		"post-app-boot":     getPostAppBootHook(),
		"post-deploy":       getPostDeployHook(),
		"pre-proxy-reboot":  getPreProxyRebootHook(),
		"post-proxy-reboot": getPostProxyRebootHook(),
		"post-rollback":     getPostRollbackHook(),
	}

	for name, content := range hooks {
		hookPath := filepath.Join(".azud", "hooks", name)
		if err := os.WriteFile(hookPath, []byte(content), 0755); err != nil {
			return fmt.Errorf("failed to create hook %s: %w", hookPath, err)
		}
	}
	log.Success("Created hooks in .azud/hooks/")

	// Create GitHub Actions workflow if requested
	if initGitHubActions {
		workflowPath := filepath.Join(".github", "workflows", "deploy.yml")
		if err := os.WriteFile(workflowPath, []byte(getGitHubActionsWorkflow()), 0644); err != nil {
			return fmt.Errorf("failed to create %s: %w", workflowPath, err)
		}
		log.Success("Created %s", workflowPath)
	}

	// Protect the generated secrets file even in a brand-new repository.
	if err := appendToGitignore(log); err != nil {
		return fmt.Errorf("failed to protect .azud/secrets in .gitignore: %w", err)
	}

	log.Header("Next actions")
	total := 3
	if initGitHubActions {
		total = 4
	}
	log.Step(1, total, "Edit config/deploy.yml")
	log.Step(2, total, "Add secrets to .azud/secrets")
	if initGitHubActions {
		log.Step(3, total, "Configure GitHub secrets: AZUD_SSH_KEY, AZUD_REGISTRY_PASSWORD")
		log.Step(4, total, "Push to main to start the workflow")
	} else {
		log.Step(3, total, "Run azud setup")
		log.Info("CI scaffold: run azud init --github-actions in a new project")
	}

	return nil
}

func getConfigTemplate(githubActions bool) string {
	template := `# Azud Deployment Configuration
# Documentation: https://github.com/lemonity-org/azud

# Service name (used as container name prefix)
service: my-app

# Container image name (registry/user/image)
image: my-user/my-app

# Container registry configuration
registry:
  # Registry server (omit for Docker Hub)
  # server: ghcr.io
  username: my-user
  password:
    - AZUD_REGISTRY_PASSWORD

# Target servers organized by role
servers:
  # Web servers (handle HTTP traffic)
  web:
    hosts:
      - 192.168.1.1
    # Uncomment for custom labels
    # labels:
    #   app: my-app
    # Uncomment for resource limits
    # options:
    #   memory: "512m"
    #   cpus: "0.5"

  # Uncomment to add worker servers
  # workers:
  #   hosts:
  #     - 192.168.1.2
  #   cmd: "./bin/jobs"

# Proxy configuration (for zero-downtime deployments)
proxy:
  # Your application's hostname
  host: my-app.example.com
  # Enable automatic SSL after setting a real ACME contact address.
  ssl: false
  # acme_email: ops@example.com
  # Run proxy with rootful Podman (useful when podman.rootless=true and binding 80/443)
  # rootful: true
  # Application port inside the container
  app_port: 3000
  # Protocol from Caddy to the application: http, h2c, or https
  # upstream_protocol: http
  # Health check configuration
  healthcheck:
    path: /up
    interval: 1s
    timeout: 5s
    # readiness_path: /ready
    # readiness_cmd: "grpc_health_probe -addr 127.0.0.1:3000"
    # liveness_path: /live
    # disable_liveness: true
    # liveness_cmd: "curl -fsS http://localhost:3000/up"
    # helper_image: "docker.io/curlimages/curl:8.5.0@sha256:08e466006f0860e54fc299378de998935333e0e130a15f6f98482e9f8dab3058"
    # helper_pull: "missing"

# Environment variables
env:
  # Non-secret environment variables
  clear:
    RAILS_ENV: production
    # Add your environment variables here
  # Secret environment variable names (values come from .azud/secrets)
  secret:
    - DATABASE_PASSWORD
    - RAILS_MASTER_KEY

# Accessories (databases, caches, etc.)
# Uncomment to add accessories
# accessories:
#   mysql:
#     image: mysql:8.0
#     host: 192.168.1.10
#     port: "3306:3306"
#     env:
#       clear:
#         MYSQL_DATABASE: my_app_production
#       secret:
#         - MYSQL_ROOT_PASSWORD
#     volumes:
#       - /var/lib/mysql:/var/lib/mysql
#
#   redis:
#     image: redis:7-alpine
#     host: 192.168.1.10
#     port: "6379:6379"
#     cmd: "redis-server --appendonly yes"

# SSH configuration
ssh:
  user: root
  # port: 22
  # keys:
  #   - ~/.ssh/id_rsa
  #   - ~/.ssh/id_ed25519
  # known_hosts_file: ~/.ssh/known_hosts
  # trusted_host_fingerprints:
  #   "192.168.1.1":
  #     - "SHA256:replace_with_fingerprint"
  # insecure_ignore_host_key: false
  # Uncomment for bastion/jump host
  # proxy:
  #   host: bastion.example.com
  #   user: deploy

# Secrets provider
{{AZUD_SECRETS_PROVIDER}}
# secrets_command: "printenv | grep '^AZUD_SECRET_' | sed 's/^AZUD_SECRET_//'"
# secrets_remote_path: $HOME/.azud/secrets

# Security policies (recommended for production)
# security:
#   require_non_root_ssh: true
#   require_rootless_podman: true
#   require_known_hosts: true
#   require_trusted_fingerprints: true
# NOTE: rootless Podman cannot bind proxy ports 80/443 directly.
# Set proxy.http_port/proxy.https_port >= 1024, or enable proxy.rootful.

# Deployment safety. Digest verification fails closed by default.
# deploy:
#   allow_unverified_image: false

# Builder configuration
builder:
  # dockerfile: Dockerfile
  # context: .
  # Build arguments
  # args:
  #   RUBY_VERSION: "3.2"
  # Remote builder (for CI/CD)
  # remote:
  #   host: builder.example.com
  #   arch: amd64

# Deployment settings
deploy:
  # Wait before health checks
  readiness_delay: 7s
  # Maximum deployment time
  deploy_timeout: 30s
  # Time to drain connections
  drain_timeout: 30s
  # Old containers to keep
  retain_containers: 5

# Hooks configuration (optional)
# Hooks are discovered by filename in the hooks_path directory (.azud/hooks/).
# hooks:
#   timeout: 5m

# Volume mounts
# volumes:
#   - /app/storage:/app/storage

# Cron jobs (scheduled tasks)
# cron:
#   db_backup:
#     schedule: "0 2 * * *"      # Daily at 2 AM
#     command: "bin/rails db:backup"
#     lock: true                  # Prevent overlapping runs
#
#   cleanup:
#     schedule: "0 4 * * 0"      # Weekly on Sunday at 4 AM
#     command: "bin/cleanup_old_files"
#     timeout: "1h"
#
#   send_reports:
#     schedule: "0 9 * * 1"      # Weekly on Monday at 9 AM
#     command: "bin/rails reports:send"
#     host: 192.168.1.1          # Run on specific host

`
	provider := "# secrets_provider: file # file | env | command\n# secrets_env_prefix: AZUD_SECRET_"
	if githubActions {
		provider = "secrets_provider: env\nsecrets_env_prefix: AZUD_SECRET_"
	}
	return strings.Replace(template, "{{AZUD_SECRETS_PROVIDER}}", provider, 1)
}

func getSecretsTemplate() string {
	return `# Azud Secrets
# This file should NOT be committed to version control
# Add it to .gitignore

# Registry password
AZUD_REGISTRY_PASSWORD=

# Database credentials
DATABASE_PASSWORD=

# Application secrets
RAILS_MASTER_KEY=

# MySQL root password (if using mysql accessory)
# MYSQL_ROOT_PASSWORD=
`
}

func getPreConnectHook() string {
	return `#!/bin/sh
# Pre-connect hook — runs before connecting to servers
# Exit with non-zero to abort deployment
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference
#   AZUD_VERSION      Image version/tag
#   AZUD_HOSTS        Target hosts (comma-separated)
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_ROLE         Target role (if deploying by role)
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running pre-connect hook..."
`
}

func getPreBuildHook() string {
	return `#!/bin/sh
# Pre-build hook — runs before building the container image
# Exit with non-zero to abort build
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference (with tag)
#   AZUD_VERSION      Image version/tag
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running pre-build hook..."
`
}

func getPostBuildHook() string {
	return `#!/bin/sh
# Post-build hook — runs after successful build, before push
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference (with tag)
#   AZUD_VERSION      Image version/tag
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running post-build hook..."
`
}

func getPreDeployHook() string {
	return `#!/bin/sh
# Pre-deploy hook — runs before deploying to servers
# Exit with non-zero to abort deployment
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference
#   AZUD_VERSION      Image version/tag
#   AZUD_HOSTS        Target hosts (comma-separated)
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_ROLE         Target role (if deploying by role)
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running pre-deploy hook..."
`
}

func getPreAppBootHook() string {
	return `#!/bin/sh
# Pre-app-boot hook — runs locally before starting a new container on each host.
# This hook executes on the machine running azud, NOT on the remote host.
# AZUD_HOSTS contains the target host being deployed to.
# Exit with non-zero to abort deployment on this host.
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference
#   AZUD_VERSION      Image version/tag
#   AZUD_HOSTS        Target host being deployed to
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_ROLE         Target role (if deploying by role)
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running pre-app-boot hook..."
`
}

func getPostAppBootHook() string {
	return `#!/bin/sh
# Post-app-boot hook — runs locally after container passes health check on each host.
# This hook executes on the machine running azud, NOT on the remote host.
# AZUD_HOSTS contains the target host that was deployed to.
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference
#   AZUD_VERSION      Image version/tag
#   AZUD_HOSTS        Target host that was deployed to
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_ROLE         Target role (if deploying by role)
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running post-app-boot hook..."
`
}

func getPostDeployHook() string {
	return `#!/bin/sh
# Post-deploy hook — runs after successful deployment
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference
#   AZUD_VERSION      Image version/tag
#   AZUD_HOSTS        Target hosts (comma-separated)
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_ROLE         Target role (if deploying by role)
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)
#   AZUD_RUNTIME      Deployment duration in seconds

echo "Running post-deploy hook..."
echo "Deployment complete!"
`
}

func getPreProxyRebootHook() string {
	return `#!/bin/sh
# Pre-proxy-reboot hook — runs before booting/rebooting the proxy
# Exit with non-zero to abort proxy reboot
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_HOSTS        Target hosts (comma-separated)
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running pre-proxy-reboot hook..."
`
}

func getPostProxyRebootHook() string {
	return `#!/bin/sh
# Post-proxy-reboot hook — runs after proxy boot/reboot completes
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_HOSTS        Target hosts (comma-separated)
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running post-proxy-reboot hook..."
`
}

func getPostRollbackHook() string {
	return `#!/bin/sh
# Post-rollback hook — runs after rollback completes
#
# Environment:
#   AZUD_SERVICE      Service name
#   AZUD_IMAGE        Full image reference
#   AZUD_VERSION      Version rolled back to
#   AZUD_HOSTS        Hosts that were rolled back (comma-separated)
#   AZUD_DESTINATION  Deployment destination
#   AZUD_PERFORMER    User running the command
#   AZUD_ROLE         Target role (if deploying by role)
#   AZUD_HOOK         This hook's name
#   AZUD_RECORDED_AT  Timestamp (RFC3339)

echo "Running post-rollback hook..."
`
}

func appendToGitignore(log *output.Logger) error {
	content, err := os.ReadFile(".gitignore")
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if strings.Contains(string(content), ".azud/secrets") {
		return nil
	}

	f, err := os.OpenFile(".gitignore", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	prefix := ""
	if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
		prefix = "\n"
	}
	if _, err := f.WriteString(prefix + "# Azud secrets\n.azud/secrets\n"); err != nil {
		return err
	}
	log.Success("Added .azud/secrets to .gitignore")
	return nil
}

func getGitHubActionsWorkflow() string {
	return `# Azud Deployment Workflow
# This workflow deploys your application when pushing to main.
#
# Required GitHub secrets:
#   AZUD_SSH_KEY              - SSH private key for server access
#   KNOWN_HOSTS               - Output of: ssh-keyscan your-server-ip
#   AZUD_REGISTRY_PASSWORD    - Container registry password/token
#   DATABASE_PASSWORD          - (Example) Application secrets
#
# Your config/deploy.yml should include:
#   secrets_provider: env
#   secrets_env_prefix: AZUD_SECRET_

name: Deploy

on:
  push:
    branches: [main]
  workflow_dispatch:

permissions:
  contents: read
  packages: write

concurrency:
  group: azud-deploy-${{ github.repository }}-${{ github.ref_name }}
  cancel-in-progress: false

jobs:
  deploy:
    name: Deploy Application
    runs-on: ubuntu-latest

    steps:
      - name: Checkout code
        uses: actions/checkout@df4cb1c069e1874edd31b4311f1884172cec0e10 # v6

      # Deployment history and canary state must survive clean hosted runners.
      # For stricter retention guarantees, mount a durable AZUD_STATE_DIR on a
      # self-hosted runner instead of relying on the GitHub cache lifecycle.
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
          NO_COLOR: "1"
          AZUD_SECRET_AZUD_REGISTRY_PASSWORD: ${{ secrets.AZUD_REGISTRY_PASSWORD }}
          AZUD_SECRET_DATABASE_PASSWORD: ${{ secrets.DATABASE_PASSWORD }}
        run: azud deploy

  # Optional: Run tests before deploying
  # test:
  #   name: Run Tests
  #   runs-on: ubuntu-latest
  #   steps:
  #     - uses: actions/checkout@df4cb1c069e1874edd31b4311f1884172cec0e10 # v6
  #     - name: Run tests
  #       run: |
  #         # Add your test commands here
  #         npm test

  # Optional: Deploy to staging first
  # staging:
  #   name: Deploy to Staging
  #   runs-on: ubuntu-latest
  #   environment: staging
  #   needs: [test]
  #   steps:
  #     - uses: actions/checkout@df4cb1c069e1874edd31b4311f1884172cec0e10 # v6
  #     - name: Setup Azud
  #       uses: lemonity-org/azud@v1
  #       with:
  #         ssh-key: ${{ secrets.AZUD_SSH_KEY }}
  #         known-hosts: ${{ secrets.KNOWN_HOSTS }}
  #     - name: Deploy to staging
  #       run: azud deploy -d staging
`
}
