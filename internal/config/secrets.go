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
