package config

import (
	"sort"
	"strings"
)

// reservedEnvVars lists the AZUD_* environment variables owned by Azud
// itself. secrets_provider=env never loads them as application secrets, even
// when they match secrets_env_prefix.
var reservedEnvVars = map[string]struct{}{
	// Read by the CLI.
	"AZUD_STATE_DIR": {},
	// Set by Azud for hook scripts (see deploy.HookContext).
	"AZUD_SERVICE":     {},
	"AZUD_IMAGE":       {},
	"AZUD_VERSION":     {},
	"AZUD_HOSTS":       {},
	"AZUD_DESTINATION": {},
	"AZUD_PERFORMER":   {},
	"AZUD_ROLE":        {},
	"AZUD_HOOK":        {},
	"AZUD_RECORDED_AT": {},
	"AZUD_RUNTIME":     {},
	// Read by the installer and the GitHub Action.
	"AZUD_INSTALL_DIR": {},
	"AZUD_INSTALL_TMP": {},
	// Documented CI credential for the deploy SSH key.
	"AZUD_SSH_KEY": {},
	// Read by the test harnesses.
	"AZUD_TEST_BIN": {},
}

// reservedEnvVarPrefixes lists prefixes of Azud-owned variable families.
var reservedEnvVarPrefixes = []string{
	// Integration test settings (AZUD_INTEGRATION, AZUD_INTEGRATION_HOST, ...).
	"AZUD_INTEGRATION",
}

// IsReservedEnvVar reports whether name is an environment variable owned by
// Azud rather than an application secret.
func IsReservedEnvVar(name string) bool {
	if _, ok := reservedEnvVars[name]; ok {
		return true
	}
	for _, prefix := range reservedEnvVarPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// envSecrets collects the variables in environ that start with prefix and
// returns them keyed by their name without the prefix. Variables owned by
// Azud and variables whose full name is an infrastructure secret reference
// (registry password, custom TLS) are skipped, because stripping the prefix
// would otherwise copy them into the application runtime secrets under a
// different name. Registry login still reads such a password directly from the
// environment; custom TLS is read only from the loaded secrets, so it must be
// referenced by the name without the prefix. The skipped names are returned
// sorted; values are never included.
func envSecrets(environ []string, prefix string, infrastructureRefs map[string]struct{}) (map[string]string, []string) {
	secrets := make(map[string]string)
	var skipped []string
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(key, prefix) {
			continue
		}
		name := strings.TrimPrefix(key, prefix)
		if name == "" {
			continue
		}
		if _, infrastructure := infrastructureRefs[key]; infrastructure || IsReservedEnvVar(key) {
			skipped = append(skipped, key)
			continue
		}
		secrets[name] = value
	}
	sort.Strings(skipped)
	return secrets, skipped
}

// infrastructureSecretRefs returns the registry and custom TLS secret
// references that Azud consumes locally.
func infrastructureSecretRefs(cfg *Config) map[string]struct{} {
	refs := make(map[string]struct{}, len(cfg.Registry.Password)+2)
	for _, ref := range append([]string{cfg.Proxy.SSLCertificate, cfg.Proxy.SSLPrivateKey}, cfg.Registry.Password...) {
		if ref != "" {
			refs[ref] = struct{}{}
		}
	}
	return refs
}
