package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/internal/config"
)

var (
	// Global flags
	configPath  string
	destination string
	verbose     bool

	// Config instance
	cfg *config.Config

	rootCmd = &cobra.Command{
		Use:   "azud",
		Short: "Deploy web apps anywhere with zero downtime",
		Long: `Azud is a deployment tool for containerized web applications.

It deploys your application to any server with Docker, providing:
  - Zero-downtime deployments
  - Multi-server support with roles
  - Automatic SSL via Let's Encrypt
  - Accessory management (databases, caches, etc.)

Get started:
  azud init       Create a new deployment configuration
  azud setup      Bootstrap servers and deploy
  azud deploy     Deploy your application`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Skip config loading for commands that don't need it
			if cmd.Name() == "init" || cmd.Name() == "version" || cmd.Name() == "help" {
				return nil
			}

			// Load configuration
			var err error
			cfg, err = loadConfig()
			if err != nil {
				return err
			}
			return nil
		},
	}
)

func init() {
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "Path to config file (default: config/deploy.yml)")
	rootCmd.PersistentFlags().StringVarP(&destination, "destination", "d", "", "Destination environment (e.g., staging, production)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")

	// Add subcommands
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(initCmd)
}

// Execute runs the root command
func Execute() error {
	return rootCmd.Execute()
}

// loadConfig loads the configuration file
func loadConfig() (*config.Config, error) {
	path := configPath
	if path == "" {
		path = findConfigFile()
	}

	if path == "" {
		return nil, fmt.Errorf("no configuration file found. Run 'azud init' to create one")
	}

	loader := config.NewLoader(path, destination)
	return loader.Load()
}

// findConfigFile searches for a config file in standard locations
func findConfigFile() string {
	// Check common locations
	paths := []string{
		"config/deploy.yml",
		"config/deploy.yaml",
		"deploy.yml",
		"deploy.yaml",
		".azud/deploy.yml",
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	return ""
}

// GetConfig returns the loaded configuration
func GetConfig() *config.Config {
	return cfg
}

// GetConfigPath returns the resolved config file path
func GetConfigPath() string {
	if configPath != "" {
		return configPath
	}
	return findConfigFile()
}

// GetDestination returns the destination environment
func GetDestination() string {
	return destination
}

// IsVerbose returns whether verbose mode is enabled
func IsVerbose() bool {
	return verbose
}

// getConfigDir returns the directory containing the config file
func getConfigDir() string {
	path := GetConfigPath()
	if path == "" {
		return "config"
	}
	return filepath.Dir(path)
}
