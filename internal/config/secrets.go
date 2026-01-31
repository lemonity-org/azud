package config

var loadedSecretsStore = make(map[string]string)

// SetLoadedSecrets stores the loaded secrets for later retrieval via GetSecret.
func SetLoadedSecrets(secrets map[string]string) {
	loadedSecretsStore = secrets
}

// GetSecret retrieves a secret value by key.
func GetSecret(key string) (string, bool) {
	val, ok := loadedSecretsStore[key]
	return val, ok
}

// GetSecretOrEnv retrieves a secret value, returning an empty string if not found.
func GetSecretOrEnv(key string) string {
	if val, ok := loadedSecretsStore[key]; ok {
		return val
	}
	return ""
}

// AllSecrets returns a copy of all loaded secrets.
func AllSecrets() map[string]string {
	out := make(map[string]string, len(loadedSecretsStore))
	for k, v := range loadedSecretsStore {
		out[k] = v
	}
	return out
}

// DefaultRemoteSecretsPath is the default secrets file path on remote hosts.
func DefaultRemoteSecretsPath() string {
	return "$HOME/.azud/secrets"
}

// RemoteSecretsPath returns the configured remote secrets path or the default.
func RemoteSecretsPath(cfg *Config) string {
	if cfg != nil && cfg.SecretsRemotePath != "" {
		return cfg.SecretsRemotePath
	}
	return DefaultRemoteSecretsPath()
}
