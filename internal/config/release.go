package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// FromRelease builds the configuration a pull-mode server deploys a release
// bundle with: the bundle's deploy.yml merged with its destination file
// (dest may be nil), with every trust setting taken from server, the server's
// own configuration.
//
// Trust settings either run commands on the host or decide what the agent
// accepts: agent, ssh, security, registry, builder, the secrets provider and
// paths, and hooks. The repository's deploy.yml sets several of them for push
// mode, so a bundle's values are not an error; they are replaced, and their
// keys returned as ignored when they differ from the server's.
//
// A bundle must belong to the server's service and image repository and run
// only on the server's hosts.
func FromRelease(base, dest []byte, server *Config) (*Config, []string, error) {
	cfg, _, err := parseConfigBytes(base)
	if err != nil {
		return nil, nil, fmt.Errorf("release deploy.yml: %w", err)
	}
	if dest != nil {
		destCfg, destNode, err := parseConfigBytes(dest)
		if err != nil {
			return nil, nil, fmt.Errorf("release destination file: %w", err)
		}
		cfg = mergeConfigs(cfg, destCfg, destNode)
	}
	applyDefaults(cfg)

	var ignored []string
	trust := func(key string, bundle, own any) {
		if !reflect.ValueOf(bundle).Elem().IsZero() && !reflect.DeepEqual(reflect.ValueOf(bundle).Elem().Interface(), reflect.ValueOf(own).Elem().Interface()) {
			ignored = append(ignored, key)
		}
		reflect.ValueOf(bundle).Elem().Set(reflect.ValueOf(own).Elem())
	}
	trust("agent", &cfg.Agent, &server.Agent)
	trust("ssh", &cfg.SSH, &server.SSH)
	trust("security", &cfg.Security, &server.Security)
	trust("registry", &cfg.Registry, &server.Registry)
	trust("builder", &cfg.Builder, &server.Builder)
	trust("secrets_provider", &cfg.SecretsProvider, &server.SecretsProvider)
	trust("secrets_command", &cfg.SecretsCommand, &server.SecretsCommand)
	trust("secrets_env_prefix", &cfg.SecretsEnvPrefix, &server.SecretsEnvPrefix)
	trust("secrets_path", &cfg.SecretsPath, &server.SecretsPath)
	trust("secrets_remote_path", &cfg.SecretsRemotePath, &server.SecretsRemotePath)
	trust("hooks_path", &cfg.HooksPath, &server.HooksPath)
	trust("hooks", &cfg.Hooks, &server.Hooks)

	if cfg.Service != server.Service {
		return nil, nil, fmt.Errorf("release is for service %q, not %q", cfg.Service, server.Service)
	}
	if repositoryOf(cfg.Image) != repositoryOf(server.Image) {
		return nil, nil, fmt.Errorf("release deploys %s, not %s", repositoryOf(cfg.Image), repositoryOf(server.Image))
	}
	serverHosts := server.GetAllHosts()
	for _, host := range cfg.GetAllHosts() {
		if !slices.Contains(serverHosts, host) {
			return nil, nil, fmt.Errorf("release runs on %s, which is not this server (%s)", host, strings.Join(serverHosts, ", "))
		}
	}

	if err := Validate(cfg); err != nil {
		return nil, nil, fmt.Errorf("release configuration is invalid: %w", err)
	}
	return cfg, ignored, nil
}

// repositoryOf returns an image name without its tag or digest.
func repositoryOf(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	if idx := strings.LastIndex(image, ":"); idx > strings.LastIndex(image, "/") {
		image = image[:idx]
	}
	return image
}
