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

It deploys your application to any server with Podman, providing:
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

func Execute() error {
	return rootCmd.Execute()
}

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

func GetConfig() *config.Config {
	return cfg
}

func GetConfigPath() string {
	if configPath != "" {
		return configPath
	}
	return findConfigFile()
}

func GetDestination() string {
	return destination
}

func IsVerbose() bool {
	return verbose
}

func getConfigDir() string {
	path := GetConfigPath()
	if path == "" {
		return "config"
	}
	return filepath.Dir(path)
}
