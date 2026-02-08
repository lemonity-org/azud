package cli

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/proxy"
	"github.com/lemonity-org/azud/internal/server"
	"github.com/lemonity-org/azud/internal/ssh"
)

var preflightCmd = &cobra.Command{
	Use:   "preflight",
	Short: "Verify readiness of hosts and configuration",
	Long: `Run a preflight checklist before deploying.

Checks:
  - SSH connectivity and host key policy
  - Podman installation and rootless mode (if required)
  - Secrets file presence on hosts (if required)
  - Proxy status (if configured)
  - DNS resolution for proxy host
`,
	RunE: runPreflight,
}

var (
	preflightHost string
	preflightRole string
)

func init() {
	preflightCmd.Flags().StringVar(&preflightHost, "host", "", "Check a specific host")
	preflightCmd.Flags().StringVar(&preflightRole, "role", "", "Check hosts for a specific role")

	rootCmd.AddCommand(preflightCmd)
}

func runPreflight(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	hosts := getPreflightHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}

	log.Header("Preflight Checks")
	log.Info("Hosts: %d", len(hosts))

	// Local policy checks
	if cfg.Security.RequireNonRootSSH && cfg.SSH.User == "root" {
		return fmt.Errorf("security.require_non_root_ssh enabled but ssh.user is root")
	}
	if cfg.Security.RequireRootlessPodman && !cfg.Podman.Rootless {
		return fmt.Errorf("security.require_rootless_podman enabled but podman.rootless is false")
	}
	if cfg.Security.RequireKnownHosts && cfg.SSH.InsecureIgnoreHostKey {
		return fmt.Errorf("security.require_known_hosts enabled but ssh.insecure_ignore_host_key is true")
	}
	if err := validateLocalSecretRefs(); err != nil {
		return err
	}

	// DNS check for proxy host
	proxyHosts := cfg.Proxy.AllHosts()
	for _, host := range proxyHosts {
		if _, err := net.LookupHost(host); err != nil {
			log.Warn("DNS lookup failed for %s: %v", host, err)
		} else {
			log.Success("DNS OK for %s", host)
		}
	}

	sshClient := createSSHClient()
	defer func() { _ = sshClient.Close() }()

	if err := validateBuilderSecretRefs(sshClient); err != nil {
		return err
	}

	bootstrapper := server.NewBootstrapper(sshClient, log, cfg.Podman.NetworkBackend)
	proxyManager := proxy.NewManager(sshClient, log)

	rows := make([][]string, len(hosts))
	var wg sync.WaitGroup

	for i, host := range hosts {
		wg.Add(1)
		go func(idx int, h string) {
			defer wg.Done()
			rows[idx] = preflightHostRow(sshClient, h, bootstrapper, proxyManager)
		}(i, host)
	}

	wg.Wait()

	log.Table([]string{"Host", "SSH", "Trust", "Podman", "Rootless", "Secrets", "Proxy", "Helper", "Curl", "SSHD", "Firewall", "Cron"}, rows)
	return nil
}

func validateLocalSecretRefs() error {
	var missing []string

	// Registry password reference(s)
	for _, key := range cfg.Registry.Password {
		if !secretAvailable(key) {
			missing = append(missing, fmt.Sprintf("registry.password:%s", key))
		}
	}

	// SSL cert/key references
	if cfg.Proxy.SSLCertificate != "" && !secretAvailable(cfg.Proxy.SSLCertificate) {
		missing = append(missing, fmt.Sprintf("proxy.ssl_certificate:%s", cfg.Proxy.SSLCertificate))
	}
	if cfg.Proxy.SSLPrivateKey != "" && !secretAvailable(cfg.Proxy.SSLPrivateKey) {
		missing = append(missing, fmt.Sprintf("proxy.ssl_private_key:%s", cfg.Proxy.SSLPrivateKey))
	}

	// Accessory env secrets references
	for name, accessory := range cfg.Accessories {
		for _, key := range accessory.Env.Secret {
			if !secretAvailable(key) {
				missing = append(missing, fmt.Sprintf("accessories.%s.env.secret:%s", name, key))
			}
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("missing required local secrets: %s", strings.Join(missing, ", "))
	}

	return nil
}

func validateBuilderSecretRefs(sshClient *ssh.Client) error {
	var missing []string
	for _, spec := range cfg.Builder.Secrets {
		for _, ref := range validateBuilderSecret(spec, sshClient) {
			missing = append(missing, fmt.Sprintf("builder.secrets:%s", ref))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("missing required build secrets: %s", strings.Join(missing, ", "))
	}
	return nil
}

func secretAvailable(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}

	if val := os.Getenv(key); val != "" {
		return true
	}

	if val, ok := config.GetSecret(key); ok && val != "" {
		return true
	}

	return false
}

func validateBuilderSecret(spec string, sshClient *ssh.Client) []string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}

	// Plain key reference
	if !strings.Contains(spec, "=") {
		if cfg.Builder.Remote.Host != "" {
			return nil
		}
		if secretAvailable(spec) {
			return nil
		}
		return []string{spec}
	}

	parts := strings.Split(spec, ",")
	fields := make(map[string]string)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		fields[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}

	var missing []string
	if env := fields["env"]; env != "" {
		if cfg.Builder.Remote.Host != "" && sshClient != nil {
			if !remoteEnvAvailable(sshClient, cfg.Builder.Remote.Host, env) {
				missing = append(missing, fmt.Sprintf("env:%s", env))
			}
		} else if os.Getenv(env) == "" {
			missing = append(missing, fmt.Sprintf("env:%s", env))
		}
	}
	if src := fields["src"]; src != "" {
		if cfg.Builder.Remote.Host != "" && sshClient != nil {
			if !remoteFileExists(sshClient, cfg.Builder.Remote.Host, src) {
				missing = append(missing, fmt.Sprintf("src:%s", src))
			}
		} else if _, err := os.Stat(src); err != nil {
			missing = append(missing, fmt.Sprintf("src:%s", src))
		}
	}

	return missing
}

func remoteEnvAvailable(sshClient *ssh.Client, host, name string) bool {
	if !validEnvName(name) {
		return false
	}
	cmd := fmt.Sprintf("printenv %s >/dev/null 2>&1", name)
	result, err := sshClient.Execute(host, cmd)
	return err == nil && result.ExitCode == 0
}

func remoteFileExists(sshClient *ssh.Client, host, path string) bool {
	cmd := fmt.Sprintf("test -f %s", shellQuote(path))
	result, err := sshClient.Execute(host, cmd)
	return err == nil && result.ExitCode == 0
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if i == 0 {
			if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
				return false
			}
			continue
		}
		if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
func getPreflightHosts() []string {
	if preflightHost != "" {
		return []string{preflightHost}
	}
	if preflightRole != "" {
		return cfg.GetRoleHosts(preflightRole)
	}
	return cfg.GetAllSSHHosts()
}

func preflightHostRow(sshClient *ssh.Client, host string, bootstrapper *server.Bootstrapper, proxyManager *proxy.Manager) []string {
	sshStatus := "ok"
	trustStatus := "n/a"
	rootlessStatus := "n/a"
	secretsStatus := "n/a"
	var podmanStatus string
	proxyStatus := "n/a"
	helperStatus := "n/a"
	var curlStatus string
	var sshdStatus string
	var firewallStatus string
	proxyHosts := cfg.Proxy.AllHosts()
	appHostSet := make(map[string]bool)
	for _, h := range cfg.GetAllHosts() {
		appHostSet[h] = true
	}
	for _, h := range cfg.GetAccessoryHosts() {
		appHostSet[h] = true
	}
	for _, h := range cfg.GetAllCronHosts() {
		appHostSet[h] = true
	}
	proxyHostSet := make(map[string]bool)
	roleProxyHosts := cfg.GetRoleHosts("web")
	if len(roleProxyHosts) == 0 {
		roleProxyHosts = cfg.GetAllHosts()
	}
	for _, h := range roleProxyHosts {
		proxyHostSet[h] = true
	}
	isAppHost := appHostSet[host]
	isProxyHost := proxyHostSet[host]
	isBastion := cfg.SSH.Proxy.Host != "" && host == cfg.SSH.Proxy.Host

	// SSH connectivity + user id check
	results := bootstrapper.ExecuteOnAll([]string{host}, "id -u")
	if len(results) == 0 || !results[0].Success() {
		sshStatus = "fail"
	} else {
		uid := strings.TrimSpace(results[0].Stdout)
		if cfg.Security.RequireNonRootSSH && uid == "0" {
			sshStatus = "fail"
		}
		if cfg.Security.RequireRootlessPodman && !isBastion {
			if uid == "0" {
				rootlessStatus = "fail"
			} else {
				rootlessStatus = checkLinger(bootstrapper, host, cfg.SSH.User)
			}
		}
	}

	// Trust verification (local known_hosts / fingerprint)
	if cfg.Security.RequireKnownHosts || cfg.Security.RequireTrustedFingerprints || len(cfg.SSH.TrustedHostFingerprints) > 0 {
		if ok := verifyTrustedHost(host); ok {
			trustStatus = "ok"
		} else {
			trustStatus = "fail"
		}
	}

	// Podman status
	if !isBastion {
		status, err := bootstrapper.CheckPodman(host)
		if err != nil || !status.Installed {
			podmanStatus = "missing"
		} else if status.Running {
			podmanStatus = "ok"
		} else {
			podmanStatus = "stopped"
		}
	} else {
		podmanStatus = "n/a"
	}

	// Secrets file
	if isAppHost && len(cfg.Env.Secret) > 0 {
		if err := ensureRemoteSecretsFile(sshClient, []string{host}, cfg.Env.Secret); err != nil {
			secretsStatus = "missing"
		} else {
			secretsStatus = "ok"
		}
	}

	// Proxy status
	if len(proxyHosts) > 0 && isProxyHost {
		if status, err := proxyManager.Status(host); err == nil && status.Running {
			proxyStatus = "ok"
		} else {
			proxyStatus = "down"
		}
	}

	// Helper image presence when pulls are disabled
	readinessPath := cfg.Proxy.Healthcheck.GetReadinessPath()
	helperPull := strings.TrimSpace(cfg.Proxy.Healthcheck.HelperPull)
	if helperPull == "" {
		helperPull = "missing"
	}
	helperImage := strings.TrimSpace(cfg.Proxy.Healthcheck.HelperImage)
	if helperImage == "" {
		helperImage = "curlimages/curl:8.5.0"
	}
	if isProxyHost && readinessPath != "" && helperPull == "never" {
		cmd := fmt.Sprintf("podman image exists %s", shellQuote(helperImage))
		helperStatus = checkRemoteCommand(bootstrapper, host, cmd)
	}

	// Host dependencies
	if len(proxyHosts) > 0 && isProxyHost {
		curlStatus = checkRemoteCommand(bootstrapper, host, "command -v curl >/dev/null 2>&1")
	} else {
		curlStatus = "n/a"
	}

	// SSH hardening checks
	sshdStatus = checkSSHDHardening(bootstrapper, host)

	// Firewall status
	firewallStatus = checkFirewall(bootstrapper, host)

	// Cron runtime deps check
	cronStatus := "n/a"
	cronHostSet := make(map[string]bool)
	for _, h := range cfg.GetAllCronHosts() {
		cronHostSet[h] = true
	}
	if cronHostSet[host] {
		cronStatus = checkCronDeps(bootstrapper, host)
	}

	return []string{host, sshStatus, trustStatus, podmanStatus, rootlessStatus, secretsStatus, proxyStatus, helperStatus, curlStatus, sshdStatus, firewallStatus, cronStatus}
}

func verifyTrustedHost(host string) bool {
	knownHosts := cfg.SSH.KnownHostsFile
	if knownHosts == "" {
		knownHosts = filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts")
	}

	target := host
	port := cfg.SSH.Port
	if port == 0 {
		port = 22
	}
	if port != 22 {
		target = fmt.Sprintf("[%s]:%d", host, port)
	}

	if exists, _ := knownHostExists(knownHosts, target); !exists {
		return false
	}

	expected := expectedFingerprints(target, host)
	if cfg.Security.RequireTrustedFingerprints && len(expected) == 0 {
		return false
	}
	if len(expected) == 0 {
		return true
	}

	keys, err := knownHostKeys(knownHosts, target)
	if err != nil {
		return false
	}
	fps, err := extractFingerprints(keys)
	if err != nil {
		return false
	}
	return fingerprintMatch(expected, fps)
}

func knownHostKeys(path, target string) (string, error) {
	cmd := exec.Command("ssh-keygen", "-F", target, "-f", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}

	var keys []string
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("no keys found")
	}
	return strings.Join(keys, "\n") + "\n", nil
}

func checkRemoteCommand(bootstrapper *server.Bootstrapper, host, cmd string) string {
	results := bootstrapper.ExecuteOnAll([]string{host}, cmd)
	if len(results) == 0 || !results[0].Success() {
		return "missing"
	}
	return "ok"
}

func checkSSHDHardening(bootstrapper *server.Bootstrapper, host string) string {
	results := bootstrapper.ExecuteOnAll([]string{host}, "sshd -T 2>/dev/null")
	if len(results) == 0 || !results[0].Success() {
		return "unknown"
	}

	out := strings.ToLower(results[0].Stdout)
	if strings.Contains(out, "passwordauthentication no") && !strings.Contains(out, "permitrootlogin yes") {
		return "ok"
	}
	return "warn"
}

func checkFirewall(bootstrapper *server.Bootstrapper, host string) string {
	cmd := "sh -c 'if command -v ufw >/dev/null 2>&1; then ufw status | head -n1; elif command -v firewall-cmd >/dev/null 2>&1; then firewall-cmd --state; else echo unknown; fi'"
	results := bootstrapper.ExecuteOnAll([]string{host}, cmd)
	if len(results) == 0 || !results[0].Success() {
		return "unknown"
	}
	out := strings.ToLower(strings.TrimSpace(results[0].Stdout))
	switch {
	case strings.Contains(out, "active"):
		return "ok"
	case strings.Contains(out, "running"):
		return "ok"
	case strings.Contains(out, "inactive"):
		return "warn"
	default:
		return "unknown"
	}
}

func checkCronDeps(bootstrapper *server.Bootstrapper, host string) string {
	cmd := "command -v crond >/dev/null 2>&1 && command -v flock >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1"
	results := bootstrapper.ExecuteOnAll([]string{host}, cmd)
	if len(results) == 0 || !results[0].Success() {
		return "missing"
	}
	return "ok"
}

func checkLinger(bootstrapper *server.Bootstrapper, host, user string) string {
	if user == "" {
		user = "root"
	}
	cmd := fmt.Sprintf("loginctl show-user %s -p Linger 2>/dev/null", user)
	results := bootstrapper.ExecuteOnAll([]string{host}, cmd)
	if len(results) == 0 || !results[0].Success() {
		return "unknown"
	}
	out := strings.ToLower(strings.TrimSpace(results[0].Stdout))
	if strings.Contains(out, "linger=yes") {
		return "ok"
	}
	return "warn"
}
