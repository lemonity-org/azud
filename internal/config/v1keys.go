package config

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// keyFate says what v2 does with a v1 configuration key.
type keyFate int

const (
	// fateApp keys stay in what a release sets.
	fateApp keyFate = iota
	// fateServer keys move to server.yml. A release's value is ignored, and
	// reported, because these decide what the server trusts or describe it.
	fateServer
	// fatePublisher keys are for CI, and do nothing on a server. A repository
	// sets them on purpose, so a release's value is ignored without a report.
	fatePublisher
	// fateDropped keys no longer exist.
	fateDropped
)

// v1Key is one row of the frozen table.
type v1Key struct {
	path string
	fate keyFate
	// newPath is where the key lives in v2, when its name changed.
	newPath string
	// reason says why a dropped key is gone.
	reason string
}

const (
	reasonOneServer   = "a service runs on one server in v2"
	reasonNoSSH       = "Azud 2 never dials SSH"
	reasonNoCanary    = "canary deploys are removed"
	reasonNoRemote    = "the remote builder is removed"
	reasonDigest      = "every deploy pulls by digest"
	reasonWhole       = "a failed release always rolls back whole"
	reasonRejected    = "v1 already rejected this key"
	reasonUseVolumes  = "use volumes; a copy on the same machine is a volume"
	reasonRootless    = "derived from the user running azud"
	reasonQuadletPath = "only azud server adopt reads it, through --quadlet-path"
)

func appKeys(paths ...string) []v1Key {
	return keysWithFate(fateApp, "", paths)
}

func serverKeys(paths ...string) []v1Key {
	return keysWithFate(fateServer, "", paths)
}

func publisherKeys(paths ...string) []v1Key {
	return keysWithFate(fatePublisher, "", paths)
}

func droppedKeys(reason string, paths ...string) []v1Key {
	return keysWithFate(fateDropped, reason, paths)
}

func keysWithFate(fate keyFate, reason string, paths []string) []v1Key {
	keys := make([]v1Key, len(paths))
	for i, path := range paths {
		keys[i] = v1Key{path: path, fate: fate, reason: reason}
	}
	return keys
}

// v1Keys is every key the last 1.x release accepts, frozen for all of 2.x. A
// stored release is parsed through it, so rollback to a v1 release depends on it
// never changing, whatever happens to the v2 schema.
var v1Keys = slices.Concat(
	appKeys("service", "image", "minimum_version", "volumes"),

	// A v1 repository configured its registry, secrets and hooks for pushing.
	publisherKeys(
		"registry", "registry.server", "registry.username", "registry.password",
		"secrets_provider", "secrets_command", "secrets_env_prefix", "secrets_path",
		"hooks", "hooks.timeout", "hooks_path",
		"release", "release.recipients",
		"builder", "builder.arch", "builder.args", "builder.cache", "builder.cache.options",
		"builder.cache.type", "builder.context", "builder.dockerfile", "builder.multiarch",
		"builder.platforms", "builder.secrets", "builder.ssh", "builder.tag_template",
		"builder.target",
	),
	droppedKeys(reasonNoRemote, "builder.remote", "builder.remote.arch", "builder.remote.host"),

	[]v1Key{{path: "servers", fate: fateApp, newPath: "roles"}},
	appKeys(
		"servers.*.cmd", "servers.*.env", "servers.*.labels", "servers.*.options", "servers.*.strategy",
		"servers.*.runtime", "servers.*.runtime.cap_drop", "servers.*.runtime.disable_healthcheck",
		"servers.*.runtime.no_new_privileges", "servers.*.runtime.read_only",
		"servers.*.runtime.stop_timeout", "servers.*.runtime.user", "servers.*.runtime.tmpfs",
		"servers.*.runtime.tmpfs.allow_devices", "servers.*.runtime.tmpfs.allow_exec",
		"servers.*.runtime.tmpfs.allow_suid", "servers.*.runtime.tmpfs.mode",
		"servers.*.runtime.tmpfs.path", "servers.*.runtime.tmpfs.read_only",
		"servers.*.runtime.tmpfs.size",
	),
	droppedKeys(reasonOneServer, "servers.*.hosts", "servers.*.tags"),

	appKeys("env", "env.clear", "env.secret"),
	droppedKeys(reasonRejected, "env.tags", "aliases", "asset_path", "accessories.*.env.tags"),

	appKeys(
		"proxy", "proxy.host", "proxy.hosts", "proxy.app_port", "proxy.upstream_protocol",
		"proxy.flush_interval", "proxy.forward_headers", "proxy.response_header_timeout",
		"proxy.response_timeout", "proxy.stream_close_delay",
		"proxy.buffering", "proxy.buffering.max_request_body", "proxy.buffering.memory",
		"proxy.buffering.requests", "proxy.buffering.responses",
		"proxy.healthcheck", "proxy.healthcheck.disable_liveness", "proxy.healthcheck.helper_image",
		"proxy.healthcheck.helper_pull", "proxy.healthcheck.interval", "proxy.healthcheck.liveness_cmd",
		"proxy.healthcheck.liveness_path", "proxy.healthcheck.path", "proxy.healthcheck.readiness_cmd",
		"proxy.healthcheck.readiness_path", "proxy.healthcheck.timeout",
	),
	// These configure Caddy for every app on the machine.
	serverKeys(
		"proxy.acme_email", "proxy.acme_staging", "proxy.enable_full_duplex", "proxy.http_port",
		"proxy.https_port", "proxy.max_header_bytes", "proxy.rootful", "proxy.ssl",
		"proxy.ssl_certificate", "proxy.ssl_private_key", "proxy.ssl_redirect",
		"proxy.logging", "proxy.logging.enabled", "proxy.logging.redact_request_headers",
		"proxy.logging.redact_response_headers",
		// LoggingConfig accepts these spellings too.
		"proxy.logging.request_headers", "proxy.logging.response_headers",
	),

	appKeys(
		"accessories", "accessories.*.boot_timeout", "accessories.*.cmd", "accessories.*.directories",
		"accessories.*.env", "accessories.*.env.clear", "accessories.*.env.secret",
		"accessories.*.image", "accessories.*.options", "accessories.*.port", "accessories.*.roles",
		"accessories.*.volumes",
	),
	droppedKeys(reasonOneServer, "accessories.*.host", "accessories.*.hosts"),
	droppedKeys(reasonUseVolumes, "accessories.*.files", "accessories.*.files.local",
		"accessories.*.files.mode", "accessories.*.files.owner", "accessories.*.files.remote"),

	appKeys("cron", "cron.*.command", "cron.*.env", "cron.*.lock", "cron.*.log_path", "cron.*.schedule", "cron.*.timeout"),
	droppedKeys(reasonOneServer, "cron.*.host", "cron.*.hosts"),

	appKeys(
		"deploy", "deploy.deploy_timeout", "deploy.drain_timeout", "deploy.pre_deploy_command",
		"deploy.pull_timeout", "deploy.readiness_delay", "deploy.retain_containers",
		"deploy.retain_history", "deploy.stop_timeout",
	),
	droppedKeys(reasonDigest, "deploy.allow_unverified_image"),
	droppedKeys(reasonWhole, "deploy.rollback_on_failure"),
	droppedKeys(reasonNoCanary, "deploy.canary", "deploy.canary.auto_promote", "deploy.canary.enabled",
		"deploy.canary.initial_weight", "deploy.canary.step_interval", "deploy.canary.step_weight"),

	serverKeys("podman", "podman.network_backend"),
	droppedKeys(reasonRootless, "podman.rootless"),
	droppedKeys(reasonQuadletPath, "podman.quadlet_path"),

	serverKeys(
		"agent", "agent.allow_downgrade", "agent.deploy_command", "agent.deploy_stop_grace",
		"agent.deploy_timeout", "agent.identity", "agent.insecure_skip_verify", "agent.interval",
		"agent.source", "agent.track", "agent.verify_command", "agent.verify_timeout",
	),
	serverKeys("security", "security.require_rootless_podman"),
	[]v1Key{
		{path: "security.require_non_root_ssh", fate: fateServer, newPath: "security.require_non_root"},
		{path: "secrets_remote_path", fate: fateServer, newPath: "runtime_secrets_path"},
	},
	droppedKeys(reasonNoSSH, "security.require_known_hosts", "security.require_trusted_fingerprints"),

	droppedKeys(reasonNoSSH,
		"ssh", "ssh.connect_timeout", "ssh.insecure_ignore_host_key", "ssh.keys", "ssh.known_hosts_file",
		"ssh.port", "ssh.proxy", "ssh.proxy.host", "ssh.proxy.user", "ssh.trusted_host_fingerprints",
	),
	[]v1Key{
		{path: "ssh.user", fate: fateServer, newPath: "user"},
		{path: "ssh.command_timeout", fate: fateServer, newPath: "agent.command_timeout"},
	},
)

// v1KeyIndex finds a row by its path.
var v1KeyIndex = func() map[string]v1Key {
	index := make(map[string]v1Key, len(v1Keys))
	for _, key := range v1Keys {
		index[key.path] = key
	}
	return index
}()

// lookupV1Key finds the row for key under parent, whose wildcard segments are
// already "*". The key may be a name the user chose, such as a role's. A row with
// nothing under it is a leaf, whose value can hold any keys, such as a label.
func lookupV1Key(parent, key string) (v1Key, bool) {
	if row, ok := v1KeyIndex[joinConfigPath(parent, key)]; ok {
		return row, true
	}
	if hasV1KeysUnder(joinConfigPath(parent, "*")) {
		return v1Key{path: joinConfigPath(parent, "*"), fate: fateApp}, true
	}
	return v1Key{}, false
}

func hasV1KeysUnder(path string) bool {
	return slices.ContainsFunc(v1Keys, func(key v1Key) bool {
		return strings.HasPrefix(key.path, path+".")
	})
}

// TranslateV1 rewrites the files of a format 1 release into the shape of a v2
// release, on YAML nodes, so references like ${VAR} stay as written. dest, the
// destination file, may be nil, and so is the result then. It returns notes on what
// the release set that a server ignores.
//
// A release on more than one host is refused, as v1 refused it. An accessory or
// cron job on another host is left out and noted: `azud accessory boot db` must
// never start an empty database on the app server. A key the table does not have
// is an error, because v1 could not have written it.
func TranslateV1(base, dest []byte) (newBase, newDest []byte, notes []string, err error) {
	baseFile, err := parseV1File(base)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("release deploy.yml: %w", err)
	}
	var destFile *v1File
	if dest != nil {
		if destFile, err = parseV1File(dest); err != nil {
			return nil, nil, nil, fmt.Errorf("release destination file: %w", err)
		}
	}

	hosts := v1RoleHosts(baseFile, destFile)
	if len(hosts) > 1 {
		return nil, nil, nil, fmt.Errorf("release runs on %d servers (%s), and Azud 2 runs a service on one", len(hosts), strings.Join(hosts, ", "))
	}
	leftOut := v1EntriesOnOtherHosts(baseFile, destFile, hosts)

	newBase, notes, err = baseFile.translate(leftOut)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("release deploy.yml: %w", err)
	}
	if destFile != nil {
		var destNotes []string
		newDest, destNotes, err = destFile.translate(leftOut)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("release destination file: %w", err)
		}
		for _, note := range destNotes {
			if !slices.Contains(notes, note) {
				notes = append(notes, note)
			}
		}
	}
	return newBase, newDest, notes, nil
}

// v1File is one parsed configuration file, with references like ${VAR} as written.
type v1File struct {
	root *yaml.Node
}

func parseV1File(data []byte) (*v1File, error) {
	if len(data) > maxConfigFileSize {
		return nil, fmt.Errorf("config file exceeds maximum size (%d bytes)", maxConfigFileSize)
	}
	protected, placeholders, err := protectEnvReferences(data)
	if err != nil {
		return nil, err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(protected, &document); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if document.Kind == yaml.DocumentNode && len(document.Content) == 1 {
		expander := yamlAliasExpander{placeholders: placeholders, budget: maxExpandedYAMLNodes}
		if root, err = expander.expand(document.Content[0], 0); err != nil {
			return nil, fmt.Errorf("failed to parse YAML: %w", err)
		}
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("failed to parse YAML: the top level is not a mapping")
	}
	return &v1File{root: root}, nil
}

const (
	// maxYAMLAliasDepth stops a document that refers to itself.
	maxYAMLAliasDepth = 64
	// maxExpandedYAMLNodes stops aliases that repeat each other, which grow
	// exponentially. It is far past any real configuration.
	maxExpandedYAMLNodes = 1 << 18
)

// yamlAliasExpander copies a document with every alias replaced by what it names, so
// a key can be removed even if something else refers to it, and with references
// like ${VAR} back as written, so two uses of one reference compare equal.
type yamlAliasExpander struct {
	placeholders *envPlaceholders
	budget       int
}

func (e *yamlAliasExpander) expand(node *yaml.Node, depth int) (*yaml.Node, error) {
	if depth > maxYAMLAliasDepth {
		return nil, errors.New("aliases nest too deeply")
	}
	if e.budget--; e.budget < 0 {
		return nil, errors.New("aliases expand to too many values")
	}
	if node.Kind == yaml.AliasNode {
		return e.expand(node.Alias, depth+1)
	}
	copied := *node
	copied.Anchor = ""
	copied.Alias = nil
	value, err := e.placeholders.restore(node.Value)
	if err != nil {
		return nil, err
	}
	copied.Value = value
	copied.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		if copied.Content[i], err = e.expand(child, depth+1); err != nil {
			return nil, err
		}
	}
	return &copied, nil
}

// v1RoleHosts returns the distinct hosts of the release's roles, in order. A
// destination file replaces a role it names, as v1 merged them.
func v1RoleHosts(base, dest *v1File) []string {
	roles := map[string]*yaml.Node{}
	var names []string
	for _, file := range []*v1File{base, dest} {
		if file == nil {
			continue
		}
		forEachMappingEntry(mappingValue(file.root, "servers"), func(name string, role *yaml.Node) {
			if _, seen := roles[name]; !seen {
				names = append(names, name)
			}
			roles[name] = role
		})
	}
	var hosts []string
	for _, name := range names {
		for _, host := range scalarValues(mappingValue(roles[name], "hosts")) {
			if !slices.Contains(hosts, host) {
				hosts = append(hosts, host)
			}
		}
	}
	return hosts
}

// v1EntriesOnOtherHosts returns the accessories and cron jobs a server leaves out, as
// "section.name". A destination file replaces an entry it names, as v1 merged them, so
// the entry that decides is the destination's, and it is left out of both files:
// otherwise the base's entry would come back when the two merge.
func v1EntriesOnOtherHosts(base, dest *v1File, roleHosts []string) map[string]bool {
	leftOut := map[string]bool{}
	if len(roleHosts) == 0 {
		return leftOut
	}
	for _, section := range []string{"accessories", "cron"} {
		entries := map[string]*yaml.Node{}
		for _, file := range []*v1File{base, dest} {
			if file != nil {
				forEachMappingEntry(mappingValue(file.root, section), func(name string, entry *yaml.Node) {
					entries[name] = entry
				})
			}
		}
		for name, entry := range entries {
			if onAnotherHost(entry, roleHosts) {
				leftOut[section+"."+name] = true
			}
		}
	}
	return leftOut
}

func (f *v1File) translate(leftOut map[string]bool) ([]byte, []string, error) {
	t := v1Translator{leftOut: leftOut}
	if err := t.mapping(f.root, ""); err != nil {
		return nil, nil, err
	}
	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(f.root); err != nil {
		return nil, nil, fmt.Errorf("failed to write YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, nil, fmt.Errorf("failed to write YAML: %w", err)
	}
	return encoded.Bytes(), t.notes, nil
}

type v1Translator struct {
	// leftOut holds the accessories and cron jobs on another host, as "section.name".
	leftOut map[string]bool
	notes   []string
}

// mapping removes from node what a release no longer sets and renames what moved.
// parent is the table path of node, with wildcard segments as "*".
func (t *v1Translator) mapping(node *yaml.Node, parent string) error {
	kept := make([]*yaml.Node, 0, len(node.Content))
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, value := node.Content[i], node.Content[i+1]
		row, ok := lookupV1Key(parent, keyNode.Value)
		if !ok {
			return fmt.Errorf("line %d: unknown configuration key %q", keyNode.Line, joinConfigPath(parent, keyNode.Value))
		}
		if row.fate != fateApp {
			t.remove(row)
			continue
		}
		if t.leftOut[parent+"."+keyNode.Value] {
			t.notes = append(t.notes, fmt.Sprintf("%s.%s runs on another host, so it is left out", parent, keyNode.Value))
			continue
		}
		if err := t.children(value, row.path); err != nil {
			return err
		}
		if row.newPath != "" {
			keyNode.Value = row.newPath
		}
		kept = append(kept, keyNode, value)
	}
	node.Content = kept
	return nil
}

func (t *v1Translator) children(value *yaml.Node, path string) error {
	if !hasV1KeysUnder(path) {
		return nil
	}
	switch value.Kind {
	case yaml.MappingNode:
		return t.mapping(value, path)
	case yaml.SequenceNode:
		for _, item := range value.Content {
			if err := t.children(item, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *v1Translator) remove(row v1Key) {
	switch row.fate {
	case fateServer:
		t.notes = append(t.notes, row.path+" is set by the server, so the release's value is ignored")
	case fateDropped:
		// The host keys belong to a check that already ran, and say nothing a
		// reader can act on.
		if row.reason != reasonOneServer {
			t.notes = append(t.notes, row.path+" is ignored: "+row.reason)
		}
	}
}

// onAnotherHost reports whether an accessory or cron job declares hosts, none of them
// this server's.
func onAnotherHost(entry *yaml.Node, roleHosts []string) bool {
	hosts := scalarValues(mappingValue(entry, "host"))
	hosts = append(hosts, scalarValues(mappingValue(entry, "hosts"))...)
	return len(hosts) > 0 && !slices.ContainsFunc(hosts, func(host string) bool {
		return slices.Contains(roleHosts, host)
	})
}

// mappingValue returns the value of key in node, or nil.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func forEachMappingEntry(node *yaml.Node, visit func(key string, value *yaml.Node)) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		visit(node.Content[i].Value, node.Content[i+1])
	}
}

// scalarValues returns a scalar's value, or a sequence's scalar values.
func scalarValues(node *yaml.Node) []string {
	switch {
	case node == nil:
		return nil
	case node.Kind == yaml.ScalarNode && node.Value != "":
		return []string{node.Value}
	case node.Kind == yaml.SequenceNode:
		var values []string
		for _, item := range node.Content {
			values = append(values, scalarValues(item)...)
		}
		return values
	}
	return nil
}
