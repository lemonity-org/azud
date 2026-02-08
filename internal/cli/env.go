package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
)

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Manage environment variables",
	Long: `Commands for managing environment variables and secrets.

Environment variables can be stored in the local .azud/secrets file
and synced to servers for use by your application containers.`,
}

var envPushCmd = &cobra.Command{
	Use:   "push",
	Short: "Push secrets to servers",
	Long: `Push secrets from local .azud/secrets file to remote servers.

Secrets are stored in $HOME/.azud/secrets on the server and loaded
when containers are started.

Example:
  azud env push           # Push to all servers
  azud env push --host x  # Push to specific host`,
	RunE: runEnvPush,
}

var envPullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Pull secrets from a server",
	Long: `Pull secrets from a remote server to local .azud/secrets file.

This is useful for syncing secrets from production to local development.

Example:
  azud env pull --host 192.168.1.1`,
	RunE: runEnvPull,
}

var envListCmd = &cobra.Command{
	Use:   "list",
	Short: "List environment variables",
	Long: `List all configured environment variables.

Shows both clear variables from config and secret variable names.

Example:
  azud env list`,
	RunE: runEnvList,
}

var envEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Edit secrets file",
	Long: `Open the secrets file in your default editor.

Uses $EDITOR or $VISUAL environment variable.

Example:
  azud env edit`,
	RunE: runEnvEdit,
}

var envGetCmd = &cobra.Command{
	Use:   "get [key]",
	Short: "Get a secret value",
	Args:  cobra.ExactArgs(1),
	Long: `Get the value of a specific secret from the local secrets file.

Example:
  azud env get DATABASE_PASSWORD`,
	RunE: runEnvGet,
}

var envSetCmd = &cobra.Command{
	Use:   "set [key] [value]",
	Short: "Set a secret value",
	Args:  cobra.ExactArgs(2),
	Long: `Set a secret value in the local secrets file.

Example:
  azud env set DATABASE_PASSWORD mypassword
  azud env push  # Push changes to servers`,
	RunE: runEnvSet,
}

var envDeleteCmd = &cobra.Command{
	Use:   "delete [key]",
	Short: "Delete a secret",
	Args:  cobra.ExactArgs(1),
	Long: `Delete a secret from the local secrets file.

Example:
  azud env delete OLD_SECRET
  azud env push  # Push changes to servers`,
	RunE: runEnvDelete,
}

var (
	envHost       string
	envShowValues bool
	envForce      bool
)

func init() {
	envPushCmd.Flags().StringVar(&envHost, "host", "", "Specific host")
	envPullCmd.Flags().StringVar(&envHost, "host", "", "Specific host (required)")
	_ = envPullCmd.MarkFlagRequired("host")

	envListCmd.Flags().BoolVar(&envShowValues, "reveal", false, "Show secret values")

	envSetCmd.Flags().BoolVarP(&envForce, "force", "f", false, "Overwrite existing value without confirmation")

	envCmd.AddCommand(envPushCmd)
	envCmd.AddCommand(envPullCmd)
	envCmd.AddCommand(envListCmd)
	envCmd.AddCommand(envEditCmd)
	envCmd.AddCommand(envGetCmd)
	envCmd.AddCommand(envSetCmd)
	envCmd.AddCommand(envDeleteCmd)

	rootCmd.AddCommand(envCmd)
}

func runEnvPush(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	// Load secrets based on provider
	secrets, err := loadSecretsForPush()
	if err != nil {
		return fmt.Errorf("failed to load secrets: %w", err)
	}

	if len(secrets) == 0 {
		log.Info("No secrets to push")
		return nil
	}

	// Get target hosts
	hosts := cfg.GetAllHosts()
	if envHost != "" {
		hosts = []string{envHost}
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	log.Header("Pushing Secrets")
	log.Info("Pushing %d secrets to %d host(s)...", len(secrets), len(hosts))

	// Build the secrets content
	var content strings.Builder
	content.WriteString("# Azud Secrets - synced from local\n")
	content.WriteString("# Do not edit directly, use 'azud env set' instead\n\n")

	// Sort keys for consistent output
	var keys []string
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		content.WriteString(fmt.Sprintf("%s=%s\n", k, secrets[k]))
	}

	// Push to each host
	hasErrors := false
	remoteDir := remoteSecretsDir()
	remoteSecrets := remoteSecretsPath()

	for _, host := range hosts {
		log.Host(host, "Pushing secrets...")

		// Create secrets directory
		mkdirCmd := fmt.Sprintf("mkdir -p \"%s\" && chmod 700 \"%s\"", remoteDir, remoteDir)
		result, err := sshClient.Execute(host, mkdirCmd)
		if err != nil {
			log.HostError(host, "Failed to create secrets directory: %v", err)
			hasErrors = true
			continue
		}
		if result.ExitCode != 0 {
			log.HostError(host, "Failed to create secrets directory: %s", result.Stderr)
			hasErrors = true
			continue
		}

		// Write secrets file via stdin to avoid heredoc delimiter injection
		writeCmd := fmt.Sprintf("cat > \"%s\"", remoteSecrets)
		result, err = sshClient.ExecuteWithStdin(host, writeCmd, strings.NewReader(content.String()))
		if err != nil {
			log.HostError(host, "Failed to write secrets: %v", err)
			hasErrors = true
			continue
		}
		if result.ExitCode != 0 {
			log.HostError(host, "Failed to write secrets: %s", result.Stderr)
			hasErrors = true
			continue
		}

		// Set permissions
		chmodCmd := fmt.Sprintf("chmod 600 \"%s\"", remoteSecrets)
		_, err = sshClient.Execute(host, chmodCmd)
		if err != nil {
			log.HostError(host, "Failed to set permissions: %v", err)
			hasErrors = true
			continue
		}

		log.HostSuccess(host, "Secrets pushed (%d variables)", len(secrets))
	}

	if hasErrors {
		return fmt.Errorf("failed to push secrets to one or more hosts")
	}

	log.Success("Secrets synced to all hosts")
	return nil
}

func runEnvPull(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	log.Header("Pulling Secrets from %s", envHost)

	// Read secrets from remote
	readCmd := fmt.Sprintf("cat \"%s\" 2>/dev/null || echo ''", remoteSecretsPath())
	result, err := sshClient.Execute(envHost, readCmd)
	if err != nil {
		return fmt.Errorf("failed to read secrets from server: %w", err)
	}

	if strings.TrimSpace(result.Stdout) == "" {
		log.Info("No secrets found on server")
		return nil
	}

	// Parse secrets
	secrets := parseSecretsContent(result.Stdout)
	if len(secrets) == 0 {
		log.Info("No secrets found on server")
		return nil
	}

	// Write to local secrets file
	secretsPath := getSecretsFilePath()

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(secretsPath), 0755); err != nil {
		return fmt.Errorf("failed to create secrets directory: %w", err)
	}

	// Write secrets file
	var content strings.Builder
	content.WriteString("# Azud Secrets - pulled from server\n\n")

	var keys []string
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		content.WriteString(fmt.Sprintf("%s=%s\n", k, secrets[k]))
	}

	if err := os.WriteFile(secretsPath, []byte(content.String()), 0600); err != nil {
		return fmt.Errorf("failed to write secrets file: %w", err)
	}

	log.Success("Pulled %d secrets to %s", len(secrets), secretsPath)
	return nil
}

func runEnvList(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	log.Header("Environment Variables")

	// Show clear variables
	if len(cfg.Env.Clear) > 0 {
		log.Println("Clear variables:")
		var keys []string
		for k := range cfg.Env.Clear {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			log.Println("  %s=%s", k, cfg.Env.Clear[k])
		}
		log.Println("")
	}

	// Show secret variables
	if len(cfg.Env.Secret) > 0 {
		log.Println("Secret variables:")

		var secrets map[string]string
		if isFileSecretsProvider() {
			secretsPath := getSecretsFilePath()
			secrets, _ = loadSecretsFile(secretsPath)
		} else {
			secrets = config.AllSecrets()
		}

		for _, k := range cfg.Env.Secret {
			if envShowValues {
				if val, ok := secrets[k]; ok {
					log.Println("  %s=%s", k, val)
				} else {
					log.Println("  %s=<not set>", k)
				}
			} else {
				if _, ok := secrets[k]; ok {
					log.Println("  %s=********", k)
				} else {
					log.Println("  %s=<not set>", k)
				}
			}
		}
	}

	return nil
}

func runEnvEdit(cmd *cobra.Command, args []string) error {
	if !isFileSecretsProvider() {
		return fmt.Errorf("env edit requires secrets_provider=file")
	}

	secretsPath := getSecretsFilePath()

	// Ensure file exists
	if _, err := os.Stat(secretsPath); os.IsNotExist(err) {
		// Create empty secrets file
		if err := os.MkdirAll(filepath.Dir(secretsPath), 0755); err != nil {
			return fmt.Errorf("failed to create secrets directory: %w", err)
		}
		if err := os.WriteFile(secretsPath, []byte("# Azud Secrets\n\n"), 0600); err != nil {
			return fmt.Errorf("failed to create secrets file: %w", err)
		}
	}

	// Get editor
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}

	// Open editor
	fmt.Printf("Opening %s in %s...\n", secretsPath, editor)

	// We can't actually exec here, so just print the command
	fmt.Printf("Run: %s %s\n", editor, secretsPath)

	return nil
}

func runEnvGet(cmd *cobra.Command, args []string) error {
	key := args[0]

	if val, ok := config.GetSecret(key); ok {
		fmt.Println(val)
		return nil
	}

	// Check clear variables
	if val, ok := cfg.Env.Clear[key]; ok {
		fmt.Println(val)
		return nil
	}

	return fmt.Errorf("variable %s not found", key)
}

func runEnvSet(cmd *cobra.Command, args []string) error {
	if !isFileSecretsProvider() {
		return fmt.Errorf("env set requires secrets_provider=file")
	}

	output.SetVerbose(verbose)
	log := output.DefaultLogger

	key := args[0]
	value := args[1]

	secretsPath := getSecretsFilePath()

	// Load existing secrets
	secrets, _ := loadSecretsFile(secretsPath)
	if secrets == nil {
		secrets = make(map[string]string)
	}

	// Check if key exists
	if _, exists := secrets[key]; exists && !envForce {
		log.Info("Variable %s already exists. Use -f to overwrite.", key)
		return nil
	}

	// Set the value
	secrets[key] = value

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(secretsPath), 0755); err != nil {
		return fmt.Errorf("failed to create secrets directory: %w", err)
	}

	// Write secrets file
	var content strings.Builder
	content.WriteString("# Azud Secrets\n\n")

	var keys []string
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		content.WriteString(fmt.Sprintf("%s=%s\n", k, secrets[k]))
	}

	if err := os.WriteFile(secretsPath, []byte(content.String()), 0600); err != nil {
		return fmt.Errorf("failed to write secrets file: %w", err)
	}

	log.Success("Set %s in %s", key, secretsPath)
	log.Info("Run 'azud env push' to sync to servers")

	return nil
}

func runEnvDelete(cmd *cobra.Command, args []string) error {
	if !isFileSecretsProvider() {
		return fmt.Errorf("env delete requires secrets_provider=file")
	}

	output.SetVerbose(verbose)
	log := output.DefaultLogger

	key := args[0]

	secretsPath := getSecretsFilePath()

	// Load existing secrets
	secrets, err := loadSecretsFile(secretsPath)
	if err != nil {
		return fmt.Errorf("failed to load secrets: %w", err)
	}

	if _, exists := secrets[key]; !exists {
		return fmt.Errorf("variable %s not found", key)
	}

	// Remove the key
	delete(secrets, key)

	// Write secrets file
	var content strings.Builder
	content.WriteString("# Azud Secrets\n\n")

	var keys []string
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		content.WriteString(fmt.Sprintf("%s=%s\n", k, secrets[k]))
	}

	if err := os.WriteFile(secretsPath, []byte(content.String()), 0600); err != nil {
		return fmt.Errorf("failed to write secrets file: %w", err)
	}

	log.Success("Deleted %s from %s", key, secretsPath)
	log.Info("Run 'azud env push' to sync to servers")

	return nil
}

func getSecretsFilePath() string {
	if cfg != nil && cfg.SecretsPath != "" {
		return cfg.SecretsPath
	}

	// Default path
	configDir := getConfigDir()
	return filepath.Join(filepath.Dir(configDir), ".azud", "secrets")
}

func isFileSecretsProvider() bool {
	provider := strings.ToLower(strings.TrimSpace(cfg.SecretsProvider))
	return provider == "" || provider == "file"
}

func loadSecretsForPush() (map[string]string, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.SecretsProvider))
	if provider == "" || provider == "file" {
		secretsPath := getSecretsFilePath()
		return loadSecretsFile(secretsPath)
	}

	secrets := config.AllSecrets()
	if len(secrets) == 0 {
		return nil, fmt.Errorf("no secrets available from provider %q", provider)
	}
	return secrets, nil
}

func remoteSecretsDir() string {
	return filepath.Dir(config.RemoteSecretsPath(cfg))
}

func remoteSecretsPath() string {
	return config.RemoteSecretsPath(cfg)
}

func loadSecretsFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]string), nil
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()

	secrets := make(map[string]string)
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse key=value
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			secrets[key] = value
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return secrets, nil
}

func parseSecretsContent(content string) map[string]string {
	secrets := make(map[string]string)
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse key=value
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			secrets[key] = value
		}
	}

	return secrets
}

// LoadSecretsForDeployment loads secrets and returns them as environment variables
// This is used by the deployer to inject secrets into containers
func LoadSecretsForDeployment() (map[string]string, error) {
	// Filter to only include secrets that are declared in config
	result := make(map[string]string)
	if cfg != nil {
		for _, key := range cfg.Env.Secret {
			if val, ok := config.GetSecret(key); ok {
				result[key] = val
			}
		}
	}

	return result, nil
}
