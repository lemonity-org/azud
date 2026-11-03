package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/agent"
	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/release"
)

// agentIdentityPath returns agent.identity, or the default
// ~/.config/azud/agent/<service>.age, with ~ expanded.
func agentIdentityPath(cfg *config.Config) (string, error) {
	path := cfg.Agent.Identity
	if path == "" {
		path = filepath.Join("~", ".config", "azud", "agent", cfg.Service+".age")
	}
	return expandHome(path)
}

// localPath turns a path written for a remote shell, which may use $HOME,
// ${HOME} or ~, into a path on this machine.
func localPath(path string) (string, error) {
	for _, prefix := range []string{"$HOME/", "${HOME}/"} {
		if strings.HasPrefix(path, prefix) {
			path = "~/" + strings.TrimPrefix(path, prefix)
		}
	}
	return expandHome(path)
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
}

func runAgentInit(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	path, err := agentIdentityPath(cfg)
	if err != nil {
		return err
	}
	log.Header("Agent / %s", cfg.Service)
	if _, err := os.Lstat(path); err == nil {
		identity, err := agent.ReadIdentity(path)
		if err != nil {
			return err
		}
		recipient := recipientFromIdentityFile(identity)
		log.Info("An identity already exists at %s; it is kept", path)
		if recipient != "" {
			fmt.Println(recipient)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	identity, recipient, err := release.GenerateIdentity()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(identity); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	log.Success("Created %s", path)
	log.Info("Add this recipient to release.recipients in the repository's config/deploy.yml:")
	fmt.Println(recipient)
	return nil
}

// recipientFromIdentityFile returns the public key recorded in an identity
// file's "# public key:" comment, which azud agent init writes.
func recipientFromIdentityFile(identity []byte) string {
	for _, line := range strings.Split(string(identity), "\n") {
		if after, ok := strings.CutPrefix(line, "# public key: "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// bundleStatusRows describes the deployed release bundle for agent status.
func bundleStatusRows(records *agent.Records, current string) [][]string {
	if current == "" {
		return nil
	}
	rec, err := records.Load(current)
	if err != nil || rec == nil || rec.Info == nil {
		return nil
	}
	rows := [][]string{
		{"Release commit", orDash(rec.Info.Commit)},
		{"Release image", rec.Info.Image},
		{"Release secrets", orDash(strings.Join(rec.Info.Secrets, ", "))},
	}
	if len(rec.Ignored) > 0 {
		rows = append(rows, []string{"Server settings kept", strings.Join(rec.Ignored, ", ")})
	}
	for _, change := range rec.NotApplied {
		rows = append(rows, []string{"Not applied (at deploy)", change})
	}
	return rows
}

// accessoryEnvironment reads the environment an accessory container runs
// with, or nil when the container does not exist.
func accessoryEnvironment(podmanClient *podman.Client, service, host string) func(string) (map[string]string, error) {
	return func(name string) (map[string]string, error) {
		container := service + "-" + name
		result, err := podmanClient.Execute(host, "container", "inspect", "--format", "{{json .Config.Env}}", container) // safe: Execute quotes each argv element
		if err != nil {
			return nil, err
		}
		if result.ExitCode != 0 {
			return nil, nil
		}
		var env []string
		if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &env); err != nil {
			return nil, err
		}
		values := make(map[string]string, len(env))
		for _, kv := range env {
			if key, value, ok := strings.Cut(kv, "="); ok {
				values[key] = value
			}
		}
		return values, nil
	}
}
