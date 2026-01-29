package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
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
	// Check if config already exists
	if existingConfig := findConfigFile(); existingConfig != "" {
		return fmt.Errorf("configuration already exists at %s", existingConfig)
	}

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
	if err := os.WriteFile(configPath, []byte(getConfigTemplate()), 0644); err != nil {
		return fmt.Errorf("failed to create %s: %w", configPath, err)
	}
	fmt.Printf("Created %s\n", configPath)

	// Create secrets file
	secretsPath := filepath.Join(".azud", "secrets")
	if err := os.WriteFile(secretsPath, []byte(getSecretsTemplate()), 0600); err != nil {
		return fmt.Errorf("failed to create %s: %w", secretsPath, err)
	}
	fmt.Printf("Created %s\n", secretsPath)

	// Create sample hook scripts
	hooks := map[string]string{
		"pre-connect": getPreConnectHook(),
		"pre-build":   getPreBuildHook(),
		"pre-deploy":  getPreDeployHook(),
		"post-deploy": getPostDeployHook(),
	}

	for name, content := range hooks {
		hookPath := filepath.Join(".azud", "hooks", name)
		if err := os.WriteFile(hookPath, []byte(content), 0755); err != nil {
			return fmt.Errorf("failed to create hook %s: %w", hookPath, err)
		}
	}
	fmt.Printf("Created hooks in .azud/hooks/\n")

	// Create GitHub Actions workflow if requested
	if initGitHubActions {
		workflowPath := filepath.Join(".github", "workflows", "deploy.yml")
		if err := os.WriteFile(workflowPath, []byte(getGitHubActionsWorkflow()), 0644); err != nil {
			return fmt.Errorf("failed to create %s: %w", workflowPath, err)
		}
		fmt.Printf("Created %s\n", workflowPath)
	}

	// Add .azud/secrets to .gitignore if it exists
	if _, err := os.Stat(".gitignore"); err == nil {
		appendToGitignore()
	}

	fmt.Println()
	fmt.Println("Azud configuration initialized!")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Edit config/deploy.yml with your settings")
	fmt.Println("  2. Add your secrets to .azud/secrets")
	if initGitHubActions {
		fmt.Println("  3. Configure GitHub secrets (AZUD_SSH_KEY, AZUD_REGISTRY_PASSWORD)")
		fmt.Println("  4. Push to main branch to trigger deployment")
	} else {
		fmt.Println("  3. Run 'azud setup' to bootstrap servers and deploy")
		fmt.Println("")
		fmt.Println("For CI/CD deployment, run 'azud init --github-actions' in a new project")
	}

	return nil
}

func getConfigTemplate() string {
	return `# Azud Deployment Configuration
# Documentation: https://github.com/adriancarayol/azud

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
  # Enable automatic SSL via Let's Encrypt
  ssl: true
  # Application port inside the container
  app_port: 3000
  # Health check configuration
  healthcheck:
    path: /up
    interval: 1s
    timeout: 5s

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
  # Uncomment for bastion/jump host
  # proxy:
  #   host: bastion.example.com
  #   user: deploy

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

# Hook scripts (optional)
# hooks:
#   pre_connect: .azud/hooks/pre-connect
#   pre_build: .azud/hooks/pre-build
#   pre_deploy: .azud/hooks/pre-deploy
#   post_deploy: .azud/hooks/post-deploy

# Volume mounts
# volumes:
#   - /app/storage:/app/storage

# Asset path for bridging between versions
# asset_path: /app/public/assets

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

# Command aliases
# aliases:
#   console: app exec --interactive -- bin/rails console
#   logs: app logs -f
`
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
# Pre-connect hook
# Runs before connecting to servers
# Exit with non-zero to abort deployment

echo "Running pre-connect hook..."
`
}

func getPreBuildHook() string {
	return `#!/bin/sh
# Pre-build hook
# Runs before building the container image
# Exit with non-zero to abort deployment

echo "Running pre-build hook..."
`
}

func getPreDeployHook() string {
	return `#!/bin/sh
# Pre-deploy hook
# Runs before deploying to servers
# Exit with non-zero to abort deployment

echo "Running pre-deploy hook..."
`
}

func getPostDeployHook() string {
	return `#!/bin/sh
# Post-deploy hook
# Runs after successful deployment

echo "Running post-deploy hook..."
echo "Deployment complete!"
`
}

func appendToGitignore() {
	content, err := os.ReadFile(".gitignore")
	if err != nil {
		return
	}

	if strings.Contains(string(content), ".azud/secrets") {
		return
	}

	// Append to .gitignore
	f, err := os.OpenFile(".gitignore", os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	_, _ = f.WriteString("\n# Azud secrets\n.azud/secrets\n")
	fmt.Println("Added .azud/secrets to .gitignore")
}

func getGitHubActionsWorkflow() string {
	return `# Azud Deployment Workflow
# This workflow deploys your application when pushing to main

name: Deploy

on:
  push:
    branches: [main, master]
  workflow_dispatch:
    inputs:
      destination:
        description: 'Deployment destination'
        required: false
        default: 'production'

env:
  AZUD_VERSION: "latest"

jobs:
  deploy:
    name: Deploy Application
    runs-on: ubuntu-latest

    steps:
      - name: Checkout code
        uses: actions/checkout@v4

      - name: Install Azud
        run: |
          curl -fsSL https://get.azud.dev | sh
          echo "$HOME/.azud/bin" >> $GITHUB_PATH

      - name: Setup SSH key
        run: |
          mkdir -p ~/.ssh
          echo "${{ secrets.AZUD_SSH_KEY }}" > ~/.ssh/id_ed25519
          chmod 600 ~/.ssh/id_ed25519
          ssh-keyscan -H ${{ secrets.DEPLOY_HOST }} >> ~/.ssh/known_hosts 2>/dev/null || true

      - name: Create secrets file
        run: |
          mkdir -p .azud
          cat > .azud/secrets << 'EOF'
          AZUD_REGISTRY_PASSWORD=${{ secrets.AZUD_REGISTRY_PASSWORD }}
          DATABASE_PASSWORD=${{ secrets.DATABASE_PASSWORD }}
          RAILS_MASTER_KEY=${{ secrets.RAILS_MASTER_KEY }}
          EOF
          chmod 600 .azud/secrets

      - name: Set up Podman
        run: |
          sudo apt-get update
          sudo apt-get install -y podman

      - name: Login to Container Registry
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Build and Push Container Image
        run: |
          azud build --no-cache

      - name: Deploy to servers
        run: |
          azud deploy --skip-build

      - name: Verify deployment
        run: |
          azud app details

  # Optional: Run tests before deploying
  # test:
  #   name: Run Tests
  #   runs-on: ubuntu-latest
  #   steps:
  #     - uses: actions/checkout@v4
  #     - name: Run tests
  #       run: |
  #         # Add your test commands here
  #         npm test

  # Optional: Deploy to staging first
  # staging:
  #   name: Deploy to Staging
  #   runs-on: ubuntu-latest
  #   environment: staging
  #   steps:
  #     - uses: actions/checkout@v4
  #     - name: Deploy to staging
  #       run: azud deploy -d staging
`
}
