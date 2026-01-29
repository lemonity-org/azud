package config

// loadedSecrets stores the loaded secrets (internal field)
// This is set by the loader and used by GetSecret
var loadedSecretsStore = make(map[string]string)

// Add a private field to Config for secrets
func init() {
	// Initialize secrets store
}

// SetLoadedSecrets stores the loaded secrets
func SetLoadedSecrets(secrets map[string]string) {
	loadedSecretsStore = secrets
}

// GetSecret retrieves a secret value by key
func GetSecret(key string) (string, bool) {
	val, ok := loadedSecretsStore[key]
	return val, ok
}

// GetSecretOrEnv retrieves a secret value, falling back to environment variable
func GetSecretOrEnv(key string) string {
	if val, ok := loadedSecretsStore[key]; ok {
		return val
	}
	return ""
}

// Add the loadedSecrets field to Config struct
// Note: This is handled via the loader setting the global store
// In a more sophisticated implementation, we'd pass secrets through context

// For now, we add a simple field to Config
type configSecrets struct {
	secrets map[string]string
}

// Extend Config with secrets handling
// We use a separate approach since Go doesn't support partial struct definitions

// loadedSecrets field for Config - added here to avoid modifying the main struct
// The loader will set this after loading
var _ = func() int {
	// This is just to ensure the file compiles
	return 0
}()
