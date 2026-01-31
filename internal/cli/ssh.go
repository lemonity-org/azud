package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/internal/output"
)

var sshCmd = &cobra.Command{
	Use:   "ssh",
	Short: "Manage SSH trust and connectivity",
	Long:  `Commands for managing SSH trust and connectivity to deployment hosts.`,
}

var sshTrustCmd = &cobra.Command{
	Use:   "trust [hosts...]",
	Short: "Add host keys to known_hosts",
	Long: `Fetch and record SSH host keys for deployment hosts.

Examples:
  azud ssh trust                 # Trust all configured hosts
  azud ssh trust 1.2.3.4         # Trust a specific host
  azud ssh trust --role web      # Trust hosts for a role
  azud ssh trust --refresh       # Replace existing entries`,
	RunE: runSSHTrust,
}

var (
	sshTrustRole     string
	sshTrustRefresh  bool
	sshTrustPrint    bool
	sshTrustTemplate bool
	sshTrustYes      bool
)

func init() {
	sshTrustCmd.Flags().StringVar(&sshTrustRole, "role", "", "Trust hosts for a specific role")
	sshTrustCmd.Flags().BoolVar(&sshTrustRefresh, "refresh", false, "Refresh existing host keys")
	sshTrustCmd.Flags().BoolVar(&sshTrustPrint, "print", false, "Print host fingerprints without writing known_hosts")
	sshTrustCmd.Flags().BoolVar(&sshTrustTemplate, "template", false, "Print YAML snippet for trusted_host_fingerprints")
	sshTrustCmd.Flags().BoolVar(&sshTrustYes, "yes", false, "Trust without prompting for confirmation")

	sshCmd.AddCommand(sshTrustCmd)
	rootCmd.AddCommand(sshCmd)
}

func runSSHTrust(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	hosts := args
	if len(hosts) == 0 {
		if sshTrustRole != "" {
			hosts = cfg.GetRoleHosts(sshTrustRole)
		} else {
			hosts = cfg.GetAllHosts()
		}
	}
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	if sshTrustPrint && sshTrustTemplate {
		return fmt.Errorf("--print and --template cannot be used together")
	}

	if sshTrustPrint || sshTrustTemplate {
		return printSSHTrust(hosts, sshTrustTemplate)
	}

	knownHosts := cfg.SSH.KnownHostsFile
	if knownHosts == "" {
		knownHosts = filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts")
	}

	if err := os.MkdirAll(filepath.Dir(knownHosts), 0700); err != nil {
		return fmt.Errorf("failed to create known_hosts directory: %w", err)
	}

	log.Header("Trusting SSH Hosts")
	log.Info("known_hosts: %s", knownHosts)

	for _, host := range hosts {
		target := host
		if cfg.SSH.Port != 0 && cfg.SSH.Port != 22 {
			target = fmt.Sprintf("[%s]:%d", host, cfg.SSH.Port)
		}

		expected := expectedFingerprints(target, host)
		if cfg.Security.RequireTrustedFingerprints && len(expected) == 0 {
			log.HostError(host, "No trusted fingerprint configured for %s", target)
			continue
		}

		if !sshTrustRefresh {
			if exists, _ := knownHostExists(knownHosts, target); exists {
				log.HostSuccess(host, "Already trusted")
				continue
			}
		}

		key, err := sshKeyscan(host, cfg.SSH.Port)
		if err != nil {
			log.HostError(host, "ssh-keyscan failed: %v", err)
			continue
		}

		fingerprints, err := extractFingerprints(key)
		if err != nil {
			log.HostError(host, "fingerprint parse failed: %v", err)
			continue
		}

		if len(expected) > 0 {
			if !fingerprintMatch(expected, fingerprints) {
				log.HostError(host, "fingerprint mismatch (expected %s, got %s)", strings.Join(expected, ", "), strings.Join(fingerprints, ", "))
				continue
			}
		}

		if !sshTrustYes {
			if !isatty.IsTerminal(os.Stdin.Fd()) {
				log.HostError(host, "confirmation required but stdin is not a TTY (use --yes to skip)")
				continue
			}

			ok, err := confirmTrust(host, target, fingerprints)
			if err != nil {
				log.HostError(host, "confirmation failed: %v", err)
				continue
			}
			if !ok {
				log.Host(host, "Skipped (not confirmed)")
				continue
			}
		}

		if sshTrustRefresh {
			_ = removeKnownHost(knownHosts, target)
		}

		if err := appendKnownHost(knownHosts, key); err != nil {
			log.HostError(host, "failed to write known_hosts: %v", err)
			continue
		}

		log.HostSuccess(host, "Trusted")
	}

	return nil
}

func confirmTrust(host, target string, fingerprints []string) (bool, error) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("\nHost: %s\nTarget: %s\nFingerprints: %s\n", host, target, strings.Join(fingerprints, ", "))
	fmt.Print("Type 'yes' to trust and continue: ")

	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(line), "yes"), nil
}

func printSSHTrust(hosts []string, template bool) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	type entry struct {
		target string
		fps    []string
	}

	var entries []entry
	hasErrors := false

	for _, host := range hosts {
		target := host
		if cfg.SSH.Port != 0 && cfg.SSH.Port != 22 {
			target = fmt.Sprintf("[%s]:%d", host, cfg.SSH.Port)
		}

		key, err := sshKeyscan(host, cfg.SSH.Port)
		if err != nil {
			log.HostError(host, "ssh-keyscan failed: %v", err)
			hasErrors = true
			continue
		}

		fps, err := extractFingerprints(key)
		if err != nil {
			log.HostError(host, "fingerprint parse failed: %v", err)
			hasErrors = true
			continue
		}

		entries = append(entries, entry{target: target, fps: fps})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].target < entries[j].target
	})

	if template {
		var sb strings.Builder
		sb.WriteString("ssh:\n")
		sb.WriteString("  trusted_host_fingerprints:\n")
		for _, e := range entries {
			sb.WriteString(fmt.Sprintf("    %q:\n", e.target))
			for _, fp := range e.fps {
				sb.WriteString(fmt.Sprintf("      - %q\n", fp))
			}
		}
		fmt.Print(sb.String())
	} else {
		for _, e := range entries {
			fmt.Printf("%s: %s\n", e.target, strings.Join(e.fps, ", "))
		}
	}

	if hasErrors {
		return fmt.Errorf("failed to read fingerprints for one or more hosts")
	}

	return nil
}

func sshKeyscan(host string, port int) (string, error) {
	args := []string{"-H"}
	if port != 0 && port != 22 {
		args = append(args, "-p", fmt.Sprintf("%d", port))
	}
	args = append(args, host)

	cmd := exec.Command("ssh-keyscan", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out.String()))
	}

	result := strings.TrimSpace(out.String())
	if result == "" {
		return "", fmt.Errorf("no keys returned")
	}
	return result + "\n", nil
}

func knownHostExists(path, host string) (bool, error) {
	cmd := exec.Command("ssh-keygen", "-F", host, "-f", path)
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() != 0 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func removeKnownHost(path, host string) error {
	cmd := exec.Command("ssh-keygen", "-R", host, "-f", path)
	_ = cmd.Run()
	return nil
}

func appendKnownHost(path, content string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(content)
	return err
}

func expectedFingerprints(target, host string) []string {
	if fps := cfg.SSH.TrustedHostFingerprints[target]; len(fps) > 0 {
		return normalizeFingerprints(fps)
	}
	if fps := cfg.SSH.TrustedHostFingerprints[host]; len(fps) > 0 {
		return normalizeFingerprints(fps)
	}
	return nil
}

func normalizeFingerprints(fps []string) []string {
	out := make([]string, 0, len(fps))
	for _, fp := range fps {
		fp = strings.TrimSpace(fp)
		if fp == "" {
			continue
		}
		out = append(out, fp)
	}
	sort.Strings(out)
	return out
}

func extractFingerprints(keys string) ([]string, error) {
	tmp, err := os.CreateTemp("", "azud-keyscan-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.WriteString(keys); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	cmd := exec.Command("ssh-keygen", "-lf", tmp.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}

	var fps []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		fps = append(fps, fields[1])
	}

	if len(fps) == 0 {
		return nil, fmt.Errorf("no fingerprints parsed")
	}
	sort.Strings(fps)
	return fps, nil
}

func fingerprintMatch(expected, actual []string) bool {
	exp := make(map[string]bool, len(expected))
	for _, fp := range expected {
		exp[fp] = true
	}
	for _, fp := range actual {
		if exp[fp] {
			return true
		}
	}
	return false
}
