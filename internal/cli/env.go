package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/secretsfile"
	"github.com/lemonity-org/azud/internal/shell"
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

Runtime secrets are stored in $HOME/.azud/secrets on the server and loaded
when containers are started. Registry and proxy TLS credentials stay local to
Azud and must use distinct names from application env.secret entries.

Secrets are pushed only to hosts that run application, accessory, or cron
containers. The remote builder and SSH bastion do not receive them unless they
also run a workload.

Example:
  azud env push           # Push to all server, accessory, and cron hosts
  azud env push --host x  # Push to one of those hosts`,
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
	Use:   "get <key>",
	Short: "Get a secret value",
	Args:  cobra.ExactArgs(1),
	Long: `Get the value of a specific secret from the local secrets file.

Example:
  azud env get DATABASE_PASSWORD`,
	RunE: runEnvGet,
}

var envSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a secret value",
	Args:  cobra.ExactArgs(2),
	Long: `Set a secret value in the local secrets file.

Example:
  azud env set DATABASE_PASSWORD mypassword
  azud env push  # Push changes to servers`,
	RunE: runEnvSet,
}

var envDeleteCmd = &cobra.Command{
	Use:   "delete <key>",
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

	// Load secrets based on provider, then remove credentials that are consumed
	// locally by Azud infrastructure rather than application containers.
	loadedSecrets, err := loadSecretsForPush()
	if err != nil {
		return fmt.Errorf("failed to load secrets: %w", err)
	}
	if len(loadedSecrets) == 0 {
		log.Info("No secrets to push")
		return nil
	}
	secrets := remoteRuntimeSecrets(cfg, loadedSecrets)
	content, err := remoteSecretsContent(secrets)
	if err != nil {
		return err
	}

	// Get target hosts
	hosts, err := secretTargetHosts(cfg, envHost)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("no workload hosts configured")
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	log.Header("Pushing Secrets")
	log.Info("Pushing %d secrets to %d host(s)...", len(secrets), len(hosts))

	// Push to each host
	hasErrors := false
	remoteDir := remoteSecretsDir()
	remoteSecrets := remoteSecretsPath()
	remoteDirArg := remotePathShellArg(remoteDir)
	remoteSecretsArg := remotePathShellArg(remoteSecrets)

	for _, host := range hosts {
		log.Host(host, "Pushing secrets...")

		// Write a mode-0600 temporary file and atomically replace the final
		// file. Paths are assigned as quoted data while documented home
		// prefixes expand on the remote host.
		writeCmd := fmt.Sprintf(`dir=%s; path=%s; tmp="${path}.tmp.$$"; umask 077 && mkdir -p "$dir" && chmod 700 "$dir" && trap 'rm -f "$tmp"' EXIT HUP INT TERM && cat > "$tmp" && chmod 600 "$tmp" && mv "$tmp" "$path" && chmod 600 "$path" && test "$(stat -c '%%a' "$path" 2>/dev/null || stat -f '%%Lp' "$path")" = 600 && trap - EXIT`, remoteDirArg, remoteSecretsArg)
		result, err := sshClient.ExecuteWithStdin(host, writeCmd, bytes.NewReader(content))
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
	if _, err := secretTargetHosts(cfg, envHost); err != nil {
		return err
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	log.Header("Pulling Secrets from %s", envHost)

	// Read secrets from remote. A missing file is reported with a dedicated
	// exit status; any other failure (permissions, I/O, timeout) is an error
	// so partial or empty output never replaces the local file.
	result, err := sshClient.Execute(envHost, envPullReadCommand(remoteSecretsPath()))
	if err != nil {
		return fmt.Errorf("failed to read secrets from server: %w", err)
	}
	if result.Error != nil {
		return fmt.Errorf("failed to read secrets from %s: %w", envHost, result.Error)
	}
	if result.ExitCode == envPullMissingExitCode {
		log.Info("No secrets file on %s (%s)", envHost, remoteSecretsPath())
		return nil
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to read secrets from %s (exit %d): %s", envHost, result.ExitCode, strings.TrimSpace(result.Stderr))
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
	if err := writeSecretsFile(log, secretsPath, "# Azud Secrets - pulled from server\n\n", secrets); err != nil {
		return err
	}

	log.Success("Pulled %d secrets to %s", len(secrets), secretsPath)
	return nil
}

// remoteSecretsContent renders the remote runtime env file, sorted by key. A
// value containing CR, LF, or NUL would add or override entries in the
// line-based file the workloads read, so it is rejected before any host is
// contacted. Errors name the key, never the value.
func remoteSecretsContent(secrets map[string]string) ([]byte, error) {
	content, err := secretsfile.Format("# Azud Secrets - synced from local\n# Do not edit directly, use 'azud env set' instead\n\n", secrets)
	if err != nil {
		return nil, fmt.Errorf("cannot push secrets: %w", err)
	}
	return content, nil
}

// envPullMissingExitCode is the exit status envPullReadCommand uses when the
// remote secrets file does not exist.
const envPullMissingExitCode = 3

// envPullReadCommand reads the remote secrets file, exiting with
// envPullMissingExitCode only when the file does not exist. Read errors keep
// cat's own non-zero status and stderr.
//
// test -e is also false when a parent directory cannot be searched, so the
// file only counts as missing when its nearest existing ancestor is a
// searchable directory; otherwise the command fails with exit status 1.
func envPullReadCommand(path string) string {
	return fmt.Sprintf(`secrets_file=%s; if [ ! -e "$secrets_file" ] && [ ! -L "$secrets_file" ]; then d=$secrets_file; while d=$(dirname -- "$d") && [ ! -e "$d" ] && [ ! -L "$d" ]; do :; done; if [ -d "$d" ] && [ -x "$d" ]; then exit %d; fi; printf '%%s\n' "cannot read $secrets_file: $d is not a searchable directory" >&2; exit 1; fi; cat -- "$secrets_file"`, remotePathShellArg(path), envPullMissingExitCode)
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
	log := output.NewLogger(cmd.OutOrStdout(), cmd.ErrOrStderr(), verbose)

	// Ensure the file exists and is private before handing it to an editor.
	info, err := os.Lstat(secretsPath)
	switch {
	case os.IsNotExist(err):
		if _, err := secretsfile.WriteFile(secretsPath, []byte("# Azud Secrets\n\n")); err != nil {
			return fmt.Errorf("failed to create secrets file: %w", err)
		}
	case err != nil:
		return fmt.Errorf("failed to inspect secrets file: %w", err)
	case info.Mode()&os.ModeSymlink != 0:
		log.Warn("%s is a symbolic link; Azud will not create or change its target", secretsPath)
	default:
		tightened, err := secretsfile.Tighten(secretsPath)
		if err != nil {
			return fmt.Errorf("failed to secure secrets file: %w", err)
		}
		if tightened {
			log.Warn("Tightened permissions on %s from %04o to 0600", secretsPath, info.Mode().Perm())
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

	log.Header("Environment / editor")
	log.Info("File: %s", secretsPath)
	// We cannot replace the current process here, so expose the exact command.
	log.Command(fmt.Sprintf("%s %s", editor, secretsPath))

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

	log := output.NewLogger(cmd.OutOrStdout(), cmd.ErrOrStderr(), verbose)

	key := args[0]
	value := args[1]
	if err := secretsfile.ValidateKey(key); err != nil {
		return err
	}
	if err := secretsfile.ValidateValue(key, value); err != nil {
		return err
	}

	secretsPath := getSecretsFilePath()

	// Load existing secrets. A read failure must not be mistaken for an empty
	// file, or the rewrite below would drop every other secret.
	secrets, err := loadSecretsFile(secretsPath)
	if err != nil {
		return fmt.Errorf("failed to load secrets: %w", err)
	}

	// Check if key exists
	if _, exists := secrets[key]; exists && !envForce {
		log.Info("Variable %s already exists. Use -f to overwrite.", key)
		return nil
	}

	// Set the value
	secrets[key] = value

	if err := writeSecretsFile(log, secretsPath, "# Azud Secrets\n\n", secrets); err != nil {
		return err
	}

	log.Success("Set %s in %s", key, secretsPath)
	log.Info("Run 'azud env push' to sync to servers")

	return nil
}

func runEnvDelete(cmd *cobra.Command, args []string) error {
	if !isFileSecretsProvider() {
		return fmt.Errorf("env delete requires secrets_provider=file")
	}

	log := output.NewLogger(cmd.OutOrStdout(), cmd.ErrOrStderr(), verbose)

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

	if err := writeSecretsFile(log, secretsPath, "# Azud Secrets\n\n", secrets); err != nil {
		return err
	}

	log.Success("Deleted %s from %s", key, secretsPath)
	log.Info("Run 'azud env push' to sync to servers")

	return nil
}

// writeSecretsFile renders secrets and replaces the local secrets file through
// the hardened writer: mode 0600, no symlink following, atomic rename.
func writeSecretsFile(log *output.Logger, path, header string, secrets map[string]string) error {
	content, err := secretsfile.Format(header, secrets)
	if err != nil {
		return err
	}
	result, err := secretsfile.WriteFile(path, content)
	if err != nil {
		return err
	}
	if result.Tightened() {
		log.Warn("Tightened permissions on %s from %04o to 0600", path, result.PreviousMode)
	}
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

// secretTargetHosts returns the hosts that receive the runtime secrets file.
// Only workload hosts (servers, accessories, and cron) consume it, so the
// remote builder and SSH bastion never receive application secrets unless
// they also run a workload. An explicit host must be one of those hosts.
func secretTargetHosts(cfg *config.Config, host string) ([]string, error) {
	workloadHosts := cfg.GetWorkloadHosts()
	if host == "" {
		return workloadHosts, nil
	}
	if containsString(workloadHosts, host) {
		return []string{host}, nil
	}
	if containsString(cfg.GetAllSSHHosts(), host) {
		return nil, fmt.Errorf("host %s does not run application containers; secrets are only delivered to server, accessory, and cron hosts", host)
	}
	return nil, fmt.Errorf("host %s is not configured", host)
}

func isFileSecretsProvider() bool {
	provider := strings.ToLower(strings.TrimSpace(cfg.SecretsProvider))
	return provider == "" || provider == "file"
}

func remoteRuntimeSecrets(cfg *config.Config, loaded map[string]string) map[string]string {
	if cfg == nil {
		return maps.Clone(loaded)
	}

	infrastructureSecrets := make(map[string]struct{}, len(cfg.Registry.Password)+2)
	for _, key := range cfg.Registry.Password {
		if key != "" {
			infrastructureSecrets[key] = struct{}{}
		}
	}
	for _, key := range []string{cfg.Proxy.SSLCertificate, cfg.Proxy.SSLPrivateKey} {
		if key != "" {
			infrastructureSecrets[key] = struct{}{}
		}
	}

	filtered := make(map[string]string, len(loaded))
	for key, value := range loaded {
		if _, infrastructureOnly := infrastructureSecrets[key]; infrastructureOnly {
			continue
		}
		filtered[key] = value
	}
	return filtered
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

func remotePathShellArg(path string) string {
	return shell.QuoteRemotePath(path)
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
