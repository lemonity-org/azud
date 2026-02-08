package deploy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/lemonity-org/azud/internal/shell"
	"github.com/lemonity-org/azud/internal/ssh"
)

// ValidateRemoteSecrets ensures the remote secrets file exists and includes all required keys.
func ValidateRemoteSecrets(sshClient *ssh.Client, hosts []string, secretsPath string, requiredKeys []string) error {
	required := normalizeSecretKeys(requiredKeys)
	if len(required) == 0 || len(hosts) == 0 {
		return nil
	}

	existsCmd := fmt.Sprintf("test -f %s", shell.Quote(secretsPath))
	results := sshClient.ExecuteParallel(hosts, existsCmd)

	var missingFiles []string
	for _, result := range results {
		if !result.Success() {
			missingFiles = append(missingFiles, result.Host)
		}
	}

	if len(missingFiles) > 0 {
		sort.Strings(missingFiles)
		return fmt.Errorf("missing secrets file on host(s): %s (run 'azud env push')", strings.Join(missingFiles, ", "))
	}

	if err := validateSecretsPermissions(sshClient, hosts, secretsPath); err != nil {
		return err
	}

	readCmd := fmt.Sprintf("cat %s", shell.Quote(secretsPath))
	readResults := sshClient.ExecuteParallel(hosts, readCmd)

	unreadable := make([]string, 0)
	missingByHost := make(map[string][]string)
	emptyByHost := make(map[string][]string)

	for _, result := range readResults {
		if !result.Success() {
			unreadable = append(unreadable, result.Host)
			continue
		}

		secrets := parseSecretsContent(result.Stdout)
		var missing, empty []string
		for _, key := range required {
			value, ok := secrets[key]
			if !ok {
				missing = append(missing, key)
			} else if strings.TrimSpace(value) == "" {
				empty = append(empty, key)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			missingByHost[result.Host] = missing
		}
		if len(empty) > 0 {
			sort.Strings(empty)
			emptyByHost[result.Host] = empty
		}
	}

	if len(unreadable) > 0 {
		sort.Strings(unreadable)
		return fmt.Errorf("unable to read secrets file on host(s): %s", strings.Join(unreadable, ", "))
	}

	if len(missingByHost) > 0 || len(emptyByHost) > 0 {
		return formatSecretErrors(missingByHost, emptyByHost)
	}

	return nil
}

func formatSecretErrors(missingByHost, emptyByHost map[string][]string) error {
	allHosts := make(map[string]struct{})
	for host := range missingByHost {
		allHosts[host] = struct{}{}
	}
	for host := range emptyByHost {
		allHosts[host] = struct{}{}
	}

	sortedHosts := make([]string, 0, len(allHosts))
	for host := range allHosts {
		sortedHosts = append(sortedHosts, host)
	}
	sort.Strings(sortedHosts)

	onlyMissing := len(emptyByHost) == 0
	onlyEmpty := len(missingByHost) == 0

	var entries []string
	for _, host := range sortedHosts {
		missing := missingByHost[host]
		empty := emptyByHost[host]

		hasMissing := len(missing) > 0
		hasEmpty := len(empty) > 0

		var detail string
		switch {
		case hasMissing && hasEmpty:
			detail = fmt.Sprintf("missing: %s; empty: %s", strings.Join(missing, ", "), strings.Join(empty, ", "))
		case hasMissing:
			if onlyMissing {
				detail = strings.Join(missing, ", ")
			} else {
				detail = "missing: " + strings.Join(missing, ", ")
			}
		case hasEmpty:
			if onlyEmpty {
				detail = strings.Join(empty, ", ")
			} else {
				detail = "empty: " + strings.Join(empty, ", ")
			}
		}
		entries = append(entries, fmt.Sprintf("%s (%s)", host, detail))
	}

	joined := strings.Join(entries, "; ")
	suffix := " (update local secrets and run 'azud env push')"

	switch {
	case onlyMissing:
		return fmt.Errorf("missing required secrets on host(s): %s%s", joined, suffix)
	case onlyEmpty:
		return fmt.Errorf("empty required secrets on host(s): %s%s", joined, suffix)
	default:
		return fmt.Errorf("secret issues on host(s): %s%s", joined, suffix)
	}
}

func validateSecretsPermissions(sshClient *ssh.Client, hosts []string, secretsPath string) error {
	if len(hosts) == 0 || strings.TrimSpace(secretsPath) == "" {
		return nil
	}

	quotedPath := shell.Quote(secretsPath)
	cmd := fmt.Sprintf(`path=%s; dir="$(dirname "$path")"; uid=$(id -u); if stat -c '%%u %%a' "$path" >/dev/null 2>&1; then fstat=$(stat -c '%%u %%a' "$path"); dstat=$(stat -c '%%u %%a' "$dir"); elif stat -f '%%u %%Lp' "$path" >/dev/null 2>&1; then fstat=$(stat -f '%%u %%Lp' "$path"); dstat=$(stat -f '%%u %%Lp' "$dir"); elif busybox stat -c '%%u %%a' "$path" >/dev/null 2>&1; then fstat=$(busybox stat -c '%%u %%a' "$path"); dstat=$(busybox stat -c '%%u %%a' "$dir"); else echo "stat unsupported" >&2; exit 2; fi; echo "$uid $fstat $dstat"`, quotedPath)
	results := sshClient.ExecuteParallel(hosts, cmd)

	var insecure []string
	var unreadable []string

	for _, result := range results {
		if !result.Success() {
			unreadable = append(unreadable, result.Host)
			continue
		}

		fields := strings.Fields(strings.TrimSpace(result.Stdout))
		if len(fields) < 5 {
			unreadable = append(unreadable, result.Host)
			continue
		}

		uid, err := strconv.Atoi(fields[0])
		if err != nil {
			unreadable = append(unreadable, result.Host)
			continue
		}

		fileOwner, err := strconv.Atoi(fields[1])
		if err != nil {
			unreadable = append(unreadable, result.Host)
			continue
		}
		fileMode, err := strconv.Atoi(fields[2])
		if err != nil {
			unreadable = append(unreadable, result.Host)
			continue
		}

		dirOwner, err := strconv.Atoi(fields[3])
		if err != nil {
			unreadable = append(unreadable, result.Host)
			continue
		}
		dirMode, err := strconv.Atoi(fields[4])
		if err != nil {
			unreadable = append(unreadable, result.Host)
			continue
		}

		var issues []string
		if fileOwner != uid {
			issues = append(issues, fmt.Sprintf("file owner %d", fileOwner))
		}
		if fileMode%100 != 0 {
			issues = append(issues, fmt.Sprintf("file mode %03d", fileMode))
		}
		if dirOwner != uid {
			issues = append(issues, fmt.Sprintf("dir owner %d", dirOwner))
		}
		if dirMode%100 != 0 {
			issues = append(issues, fmt.Sprintf("dir mode %03d", dirMode))
		}

		if len(issues) > 0 {
			insecure = append(insecure, fmt.Sprintf("%s (%s)", result.Host, strings.Join(issues, ", ")))
		}
	}

	if len(unreadable) > 0 {
		sort.Strings(unreadable)
		return fmt.Errorf("unable to validate secrets permissions on host(s): %s", strings.Join(unreadable, ", "))
	}

	if len(insecure) > 0 {
		sort.Strings(insecure)
		return fmt.Errorf("insecure secrets permissions on host(s): %s (require owner-only permissions)", strings.Join(insecure, ", "))
	}

	return nil
}

func normalizeSecretKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func parseSecretsContent(content string) map[string]string {
	secrets := make(map[string]string)
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" {
			continue
		}

		secrets[key] = value
	}

	return secrets
}
