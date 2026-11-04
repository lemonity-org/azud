package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// schemaKeyPaths lists every key of a configuration type, with map keys as "*".
func schemaKeyPaths(t reflect.Type, prefix string) []string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() == reflect.Map {
		return schemaKeyPaths(t.Elem(), prefix+".*")
	}
	if t.Kind() != reflect.Struct || t.PkgPath() == "time" {
		return nil
	}
	var paths []string
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		path := joinConfigPath(prefix, name)
		paths = append(paths, path)
		paths = append(paths, schemaKeyPaths(t.Field(i).Type, path)...)
	}
	return paths
}

func TestV1KeysCoverEveryKeyOfTheSchema(t *testing.T) {
	schema := schemaKeyPaths(reflect.TypeOf(Config{}), "")
	for _, path := range schema {
		if _, ok := v1KeyIndex[path]; !ok {
			t.Errorf("%s is in the schema and has no fate", path)
		}
	}
	for _, key := range v1Keys {
		// LoggingConfig accepts these spellings through UnmarshalYAML.
		if key.path == "proxy.logging.request_headers" || key.path == "proxy.logging.response_headers" {
			continue
		}
		if !slices.Contains(schema, key.path) {
			t.Errorf("%s has a fate and is not in the schema", key.path)
		}
	}
}

func TestV1KeysHaveOneRowEach(t *testing.T) {
	if len(v1KeyIndex) != len(v1Keys) {
		seen := map[string]bool{}
		for _, key := range v1Keys {
			if seen[key.path] {
				t.Errorf("%s appears twice", key.path)
			}
			seen[key.path] = true
		}
	}
}

func TestV1DroppedKeysSayWhy(t *testing.T) {
	for _, key := range v1Keys {
		if key.fate == fateDropped && key.reason == "" {
			t.Errorf("%s is dropped with no reason", key.path)
		}
	}
}

// decodeYAML reads a translation result as plain data, so tests compare meaning
// rather than formatting.
func decodeYAML(t *testing.T, data []byte) map[string]any {
	t.Helper()
	decoded := map[string]any{}
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	return decoded
}

func TestTranslateV1(t *testing.T) {
	const v1 = `
service: app
image: ghcr.io/acme/app
minimum_version: "1.6.0"
registry:
  server: ghcr.io
  username: bot
  password: [REGISTRY_TOKEN]
builder:
  dockerfile: Dockerfile
  remote:
    host: builder-1
servers:
  web:
    hosts: [app-1]
    labels:
      any.key: kept
    runtime:
      read_only: true
      tmpfs:
        - path: /tmp
          size: 64m
  worker:
    hosts: [app-1]
    tags: [gpu]
    cmd: bin/worker
env:
  clear:
    HOST: ${PUBLIC_HOST}
  secret: [DATABASE_URL]
proxy:
  host: app.example.com
  app_port: 3000
  ssl: true
  acme_email: ops@example.com
  logging:
    request_headers: [Authorization]
deploy:
  readiness_delay: 5s
  rollback_on_failure: false
  allow_unverified_image: true
  canary:
    enabled: true
accessories:
  db:
    image: postgres:17
    host: db-1
    env:
      clear:
        POSTGRES_DB: app
  cache:
    image: valkey:8
    host: app-1
    files:
      - local: a.conf
        remote: /etc/a.conf
cron:
  backup:
    schedule: "@daily"
    command: bin/backup
    hosts: [db-1, db-2]
  sweep:
    schedule: "@hourly"
    command: bin/sweep
    hosts: [db-1, app-1]
  plain:
    schedule: "@hourly"
    command: bin/plain
ssh:
  user: deploy
  keys: [~/.ssh/id_ed25519]
podman:
  rootless: true
  network_backend: netavark
security:
  require_known_hosts: true
agent:
  verify_command: "true"
hooks_path: .azud/hooks
secrets_path: .azud/secrets
secrets_remote_path: /srv/secrets
volumes:
  - /data:/data
`
	newBase, newDest, notes, err := TranslateV1([]byte(v1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if newDest != nil {
		t.Errorf("newDest = %q, want nil with no destination file", newDest)
	}

	want := decodeYAML(t, []byte(`
service: app
image: ghcr.io/acme/app
minimum_version: "1.6.0"
roles:
  web:
    labels:
      any.key: kept
    runtime:
      read_only: true
      tmpfs:
        - path: /tmp
          size: 64m
  worker:
    cmd: bin/worker
env:
  clear:
    HOST: ${PUBLIC_HOST}
  secret: [DATABASE_URL]
proxy:
  host: app.example.com
  app_port: 3000
deploy:
  readiness_delay: 5s
accessories:
  cache:
    image: valkey:8
cron:
  sweep:
    schedule: "@hourly"
    command: bin/sweep
  plain:
    schedule: "@hourly"
    command: bin/plain
volumes:
  - /data:/data
`))
	if got := decodeYAML(t, newBase); !reflect.DeepEqual(got, want) {
		t.Errorf("translation = \n%s", newBase)
	}

	for _, wantNote := range []string{
		"accessories.db runs on another host",
		"cron.backup runs on another host",
		"proxy.ssl is set by the server",
		"proxy.logging is set by the server",
		"agent is set by the server",
		"podman is set by the server",
		"secrets_remote_path is set by the server",
		"ssh is ignored: Azud 2 never dials SSH",
		"deploy.canary is ignored",
		"deploy.rollback_on_failure is ignored",
	} {
		if !slices.ContainsFunc(notes, func(note string) bool { return strings.Contains(note, wantNote) }) {
			t.Errorf("notes lack %q:\n%s", wantNote, strings.Join(notes, "\n"))
		}
	}
	for _, unwanted := range []string{"registry", "builder", "hosts", "secrets_path", "hooks_path"} {
		if slices.ContainsFunc(notes, func(note string) bool { return strings.HasPrefix(note, unwanted) }) {
			t.Errorf("notes mention %s, which a repository sets on purpose:\n%s", unwanted, strings.Join(notes, "\n"))
		}
	}
}

func TestTranslateV1ReadsTheDestinationFileTooAndKeepsItApart(t *testing.T) {
	base := []byte("service: app\nimage: ghcr.io/acme/app\nservers:\n  web:\n    hosts: [app-1]\n")
	dest := []byte("servers:\n  web:\n    hosts: [app-2]\nproxy:\n  host: staging.example.com\n  ssl: true\n")

	newBase, newDest, notes, err := TranslateV1(base, dest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(newBase), "hosts") || strings.Contains(string(newDest), "hosts") {
		t.Errorf("hosts survived:\n%s\n%s", newBase, newDest)
	}
	got := decodeYAML(t, newDest)
	if want := map[string]any{"roles": map[string]any{"web": map[string]any{}}, "proxy": map[string]any{"host": "staging.example.com"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("destination = %v", got)
	}
	if !slices.ContainsFunc(notes, func(note string) bool { return strings.HasPrefix(note, "proxy.ssl") }) {
		t.Errorf("notes lack the destination file's: %v", notes)
	}
}

func TestTranslateV1RefusesARelease(t *testing.T) {
	tests := []struct {
		name       string
		base, dest string
		want       string
	}{
		{
			name: "more than one host",
			base: "servers:\n  web:\n    hosts: [app-1, app-2]\n",
			want: "runs on 2 servers (app-1, app-2)",
		},
		{
			name: "more than one host across roles",
			base: "servers:\n  web:\n    hosts: [app-1]\n  worker:\n    hosts: [app-2]\n",
			want: "runs on 2 servers",
		},
		{
			name: "more than one host through the destination file",
			base: "servers:\n  web:\n    hosts: [app-1]\n",
			dest: "servers:\n  worker:\n    hosts: [app-2]\n",
			want: "runs on 2 servers",
		},
		{name: "an unknown key", base: "sevrice: app\n", want: `unknown configuration key "sevrice"`},
		{name: "an unknown nested key", base: "proxy:\n  hots: a\n", want: `unknown configuration key "proxy.hots"`},
		{name: "an unknown key in a role", base: "servers:\n  web:\n    cmmd: a\n", want: `unknown configuration key "servers.*.cmmd"`},
		{name: "an unknown key in the destination file", base: "service: app\n", dest: "nope: 1\n", want: "release destination file"},
		{name: "not a mapping", base: "- a\n", want: "not a mapping"},
		{name: "not YAML", base: "a: [", want: "failed to parse YAML"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dest []byte
			if tt.dest != "" {
				dest = []byte(tt.dest)
			}
			if _, _, _, err := TranslateV1([]byte(tt.base), dest); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("TranslateV1 = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestTranslateV1CountsTheHostADestinationFileReplaces(t *testing.T) {
	// Replacing the role's host leaves one host, not two.
	base := []byte("servers:\n  web:\n    hosts: [app-1]\n")
	dest := []byte("servers:\n  web:\n    hosts: [app-2]\n")
	if _, _, _, err := TranslateV1(base, dest); err != nil {
		t.Fatal(err)
	}
}

func TestTranslateV1KeepsAnAliasToARemovedKey(t *testing.T) {
	// The anchor is on a key that goes away, and the alias on one that stays.
	base := []byte("builder:\n  args: &shared\n    A: b\nenv:\n  clear: *shared\n")
	newBase, _, _, err := TranslateV1(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"env": map[string]any{"clear": map[string]any{"A": "b"}}}
	if got := decodeYAML(t, newBase); !reflect.DeepEqual(got, want) {
		t.Errorf("translation =\n%s", newBase)
	}
}

func TestTranslateV1RefusesAliasesThatNestWithoutEnd(t *testing.T) {
	var nested yaml.Node
	if err := yaml.Unmarshal([]byte("a: &a\n  b: 1\n"), &nested); err != nil {
		t.Fatal(err)
	}
	loop := nested.Content[0].Content[1]
	loop.Content = append(loop.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "c"}, &yaml.Node{Kind: yaml.AliasNode, Alias: loop})
	expander := yamlAliasExpander{placeholders: &envPlaceholders{}, budget: maxExpandedYAMLNodes}
	if _, err := expander.expand(loop, 0); err == nil {
		t.Error("an alias naming its own parent did not stop")
	}
}

func TestTranslateV1RefusesAliasesThatRepeatEachOther(t *testing.T) {
	var doc strings.Builder
	doc.WriteString("env:\n  clear:\n    a0: &a0 [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i < 12; i++ {
		fmt.Fprintf(&doc, "    a%d: &a%d [*a%d, *a%d, *a%d, *a%d, *a%d, *a%d, *a%d, *a%d, *a%d, *a%d]\n", i, i, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1)
	}
	if _, _, _, err := TranslateV1([]byte(doc.String()), nil); err == nil || !strings.Contains(err.Error(), "too many values") {
		t.Fatalf("TranslateV1 = %v", err)
	}
}

func TestTranslateV1ComparesAReferenceAsWritten(t *testing.T) {
	base := []byte(`servers:
  web:
    hosts: [${HOST}]
  worker:
    hosts: [${HOST}]
accessories:
  cache:
    image: valkey:8
    host: ${HOST}
env:
  clear:
    KEPT: ${KEPT}
`)
	newBase, _, notes, err := TranslateV1(base, nil)
	if err != nil {
		t.Fatalf("two roles on one reference are one host: %v", err)
	}
	got := decodeYAML(t, newBase)
	if _, ok := got["accessories"].(map[string]any)["cache"]; !ok {
		t.Errorf("an accessory on the roles' reference was left out: %v", notes)
	}
	if got["env"].(map[string]any)["clear"].(map[string]any)["KEPT"] != "${KEPT}" {
		t.Errorf("translation =\n%s", newBase)
	}
}

func TestTranslateV1DecidesPlacementFromTheDestinationFile(t *testing.T) {
	base := []byte(`servers:
  web:
    hosts: [app-1]
accessories:
  moved:
    image: postgres:17
    host: app-1
  here:
    image: valkey:8
    host: db-1
cron:
  moved:
    schedule: "@hourly"
    command: a
    host: app-1
`)
	dest := []byte(`accessories:
  moved:
    image: postgres:17
    host: db-1
  here:
    image: valkey:8
    host: app-1
cron:
  moved:
    schedule: "@hourly"
    command: a
    host: db-1
`)
	newBase, newDest, notes, err := TranslateV1(base, dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range [][]byte{newBase, newDest} {
		if strings.Contains(string(file), "moved") {
			t.Errorf("an entry the destination moved away came back:\n%s", file)
		}
	}
	if !strings.Contains(string(newDest), "here") {
		t.Errorf("an entry the destination moved here was left out:\n%s", newDest)
	}
	count := 0
	for _, note := range notes {
		if strings.HasPrefix(note, "accessories.moved") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("notes = %q; want accessories.moved noted once", notes)
	}
}
