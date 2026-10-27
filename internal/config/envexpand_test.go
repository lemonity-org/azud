package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const envTestBaseConfig = `service: test
image: test:latest
servers:
  web:
    hosts: [localhost]
proxy:
  host: test.example.com
`

func writeEnvTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExpandEnvScalar(t *testing.T) {
	t.Setenv("HOME", "/local/home")
	t.Setenv("AZUD_TEST_VALUE", "expanded")
	t.Setenv("AZUD_TEST_OTHER", "other")

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "braced", input: "value: ${AZUD_TEST_VALUE}", want: "value: expanded"},
		{name: "bare", input: "value: $AZUD_TEST_VALUE", want: "value: expanded"},
		{name: "remote home", input: "$HOME/.azud/secrets", want: "$HOME/.azud/secrets"},
		{name: "remote home braced", input: "${HOME}/.azud/secrets", want: "$HOME/.azud/secrets"},
		{name: "dollar escape", input: "echo $$AZUD_TEST_VALUE", want: "echo $AZUD_TEST_VALUE"},
		{name: "escaped braced reference", input: "$${AZUD_TEST_VALUE}", want: "${AZUD_TEST_VALUE}"},
		{name: "lowercase left as is", input: "${lower}", want: "${lower}"},
		{name: "bare lowercase is braced", input: "robot$ci", want: "robot${ci}"},
		{name: "escaped literal dollar", input: "robot$$ci", want: "robot$ci"},
		{name: "default syntax left as is", input: "${FOO:-x}", want: "${FOO:-x}"},
		{name: "value is not expanded again", input: "${AZUD_TEST_SELF}", want: "$AZUD_TEST_OTHER"},
		{name: "unset", input: "a${AZUD_TEST_UNSET}b", want: "ab"},
	}
	t.Setenv("AZUD_TEST_SELF", "$AZUD_TEST_OTHER")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (&envExpander{}).expandScalar(tt.input)
			if err != nil {
				t.Fatalf("expandScalar(%q) error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("expandScalar(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestLoaderEnvExpansionCannotAddKeys(t *testing.T) {
	path := writeEnvTestConfig(t, `service: ${SERVICE_VALUE}
image: test:latest
servers:
  web:
    hosts: [localhost]
proxy:
  host: test.example.com
`)
	t.Setenv("SERVICE_VALUE", "test\nssh:\n  insecure_ignore_host_key: true")

	cfg, err := NewLoader(path, "").Load()
	if err == nil {
		t.Fatalf("expected an error, got config with insecure_ignore_host_key=%v", cfg.SSH.InsecureIgnoreHostKey)
	}
	for _, want := range []string{"line 1", "SERVICE_VALUE", "control character"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "insecure_ignore_host_key") {
		t.Fatalf("error leaked the environment value: %v", err)
	}
}

func TestLoaderEnvExpansionInCommentsIsIgnored(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "CANARY")
	path := writeEnvTestConfig(t, "# built from ${BRANCH}\n"+envTestBaseConfig)
	t.Setenv("BRANCH", "main\nsecrets_provider: command\nsecrets_command: touch "+canary)

	cfg, err := NewLoader(path, "").Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.SecretsProvider != "" || cfg.SecretsCommand != "" {
		t.Fatalf("comment reference changed the configuration: %+v", cfg)
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("comment reference executed secrets_command (stat err: %v)", err)
	}
}

func TestLoaderEnvExpansionFlowCollectionsKeepWorking(t *testing.T) {
	path := writeEnvTestConfig(t, `service: test
image: test:latest
servers:
  web:
    hosts: [${HOST_A}, ${HOST_B}]
proxy: {host: ${PROXY_HOST}}
env:
  clear: {APP_ENV: ${APP_ENV}}
`)
	t.Setenv("HOST_A", "10.0.0.1")
	t.Setenv("HOST_B", "10.0.0.2")
	t.Setenv("PROXY_HOST", "app.example.com")
	t.Setenv("APP_ENV", "production")

	cfg, err := NewLoader(path, "").Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got := strings.Join(cfg.Servers["web"].Hosts, ","); got != "10.0.0.1,10.0.0.2" {
		t.Fatalf("hosts = %q", got)
	}
	if cfg.Proxy.Host != "app.example.com" {
		t.Fatalf("proxy.host = %q", cfg.Proxy.Host)
	}
	if cfg.Env.Clear["APP_ENV"] != "production" {
		t.Fatalf("env.clear.APP_ENV = %q", cfg.Env.Clear["APP_ENV"])
	}
}

func TestLoaderEnvExpansionFlowValueStaysSingleScalar(t *testing.T) {
	path := writeEnvTestConfig(t, `service: test
image: test:latest
servers:
  web:
    hosts: [${HOSTS}]
proxy:
  host: test.example.com
`)
	t.Setenv("HOSTS", "10.0.0.1, 10.6.6.6")

	_, err := NewLoader(path, "").Load()
	if err == nil {
		t.Fatal("expected a comma-separated environment value to stay one (invalid) host")
	}
	if !strings.Contains(err.Error(), "servers.web.hosts") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoaderEnvExpansionResolvesTypedScalars(t *testing.T) {
	path := writeEnvTestConfig(t, envTestBaseConfig+`  app_port: ${PORT}
  ssl: ${SSL}
  acme_email: ops@example.com
deploy:
  readiness_delay: ${DELAY}
ssh:
  port: ${UNSET_SSH_PORT}
`)
	t.Setenv("PORT", "8080")
	t.Setenv("SSL", "true")
	t.Setenv("DELAY", "5s")

	cfg, err := NewLoader(path, "").Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Proxy.AppPort != 8080 {
		t.Fatalf("proxy.app_port = %d, want 8080", cfg.Proxy.AppPort)
	}
	if !cfg.Proxy.SSL {
		t.Fatal("proxy.ssl = false, want true")
	}
	if cfg.Deploy.ReadinessDelay != 5*time.Second {
		t.Fatalf("deploy.readiness_delay = %s, want 5s", cfg.Deploy.ReadinessDelay)
	}
	if cfg.SSH.Port != 22 {
		t.Fatalf("ssh.port = %d, want the default for an unset variable", cfg.SSH.Port)
	}
}

func TestLoaderEnvExpansionQuotedScalarIsLiteral(t *testing.T) {
	path := writeEnvTestConfig(t, envTestBaseConfig+`env:
  clear:
    DOUBLE: "${MESSAGE}"
    SINGLE: '${MESSAGE}'
    PLAIN: ${MESSAGE}
    NUMBER: "${PORT}"
`)
	message := `say "hi" # not a comment, [x] {y: z} &a *b !tag`
	t.Setenv("MESSAGE", message)
	t.Setenv("PORT", "8080")

	cfg, err := NewLoader(path, "").Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	for _, key := range []string{"DOUBLE", "SINGLE", "PLAIN"} {
		if got := cfg.Env.Clear[key]; got != message {
			t.Fatalf("env.clear.%s = %q, want %q", key, got, message)
		}
	}
	if got := cfg.Env.Clear["NUMBER"]; got != "8080" {
		t.Fatalf("env.clear.NUMBER = %q", got)
	}
}

func TestLoaderEnvExpansionPreservesRemoteHome(t *testing.T) {
	for _, reference := range []string{"$HOME", "${HOME}"} {
		t.Run(reference, func(t *testing.T) {
			path := writeEnvTestConfig(t, envTestBaseConfig+"secrets_remote_path: "+reference+"/.azud/secrets\n")
			t.Setenv("HOME", "/local/home")

			cfg, err := NewLoader(path, "").Load()
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if cfg.SecretsRemotePath != "$HOME/.azud/secrets" {
				t.Fatalf("secrets_remote_path = %q", cfg.SecretsRemotePath)
			}
		})
	}
}

func TestLoaderEnvExpansionRejectsReferencesInKeys(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name: "block key",
			content: `service: test
image: test:latest
servers:
  ${ROLE}:
    hosts: [localhost]
`,
		},
		{
			name: "flow key",
			content: `service: test
image: test:latest
servers: {$ROLE: {hosts: [localhost]}}
`,
		},
	}
	t.Setenv("ROLE", "web")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewLoader(writeEnvTestConfig(t, tt.content), "").Load()
			if err == nil {
				t.Fatal("expected a reference in a key to be rejected")
			}
			if !strings.Contains(err.Error(), "not supported in configuration keys") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoaderEnvExpansionRejectsControlCharacters(t *testing.T) {
	path := writeEnvTestConfig(t, envTestBaseConfig+`env:
  clear:
    VALUE: ${INJECTED}
`)
	for _, value := range []string{"a\nb", "a\rb", "a\x1b[31mb", "a\x00b", "a b", "a\u0085b"} {
		t.Run(strings.ToValidUTF8(value, "?"), func(t *testing.T) {
			t.Setenv("INJECTED", strings.ReplaceAll(value, "\x00", "\x01"))
			_, err := NewLoader(path, "").Load()
			if err == nil {
				t.Fatal("expected a control character to be rejected")
			}
			for _, want := range []string{"INJECTED", "control character", "line 10"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q missing %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "a\n") || strings.Contains(err.Error(), "\x1b") {
				t.Fatalf("error leaked the value: %q", err)
			}
		})
	}

	t.Setenv("INJECTED", "a\tb")
	cfg, err := NewLoader(path, "").Load()
	if err != nil {
		t.Fatalf("tab should be accepted: %v", err)
	}
	if cfg.Env.Clear["VALUE"] != "a\tb" {
		t.Fatalf("env.clear.VALUE = %q", cfg.Env.Clear["VALUE"])
	}
}

func TestLoaderEnvExpansionRejectsOversizedValues(t *testing.T) {
	t.Run("single value", func(t *testing.T) {
		path := writeEnvTestConfig(t, envTestBaseConfig+"env:\n  clear:\n    VALUE: ${HUGE}\n")
		t.Setenv("HUGE", strings.Repeat("a", maxExpandedEnvValue+1))
		_, err := NewLoader(path, "").Load()
		if err == nil || !strings.Contains(err.Error(), "HUGE exceeds maximum length") {
			t.Fatalf("expected an oversized value error, got %v", err)
		}
	})
	t.Run("total", func(t *testing.T) {
		var content strings.Builder
		content.WriteString(envTestBaseConfig + "env:\n  clear:\n")
		for i := 0; i < 20; i++ {
			content.WriteString("    VALUE" + string(rune('A'+i)) + ": ${LARGE}\n")
		}
		path := writeEnvTestConfig(t, content.String())
		t.Setenv("LARGE", strings.Repeat("a", 60<<10))
		_, err := NewLoader(path, "").Load()
		if err == nil || !strings.Contains(err.Error(), "maximum total size") {
			t.Fatalf("expected a total size error, got %v", err)
		}
	})
}

func TestLoaderEnvExpansionAnchorsAndAliases(t *testing.T) {
	path := writeEnvTestConfig(t, `service: test
image: test:latest
servers:
  web:
    hosts: &hosts [${HOST}]
  worker:
    hosts: *hosts
proxy:
  host: test.example.com
env:
  clear:
    FIRST: &value ${VALUE}
    SECOND: *value
`)
	t.Setenv("HOST", "10.0.0.1")
	t.Setenv("VALUE", "$OTHER")
	t.Setenv("OTHER", "expanded-twice")

	cfg, err := NewLoader(path, "").Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	for _, role := range []string{"web", "worker"} {
		if got := strings.Join(cfg.Servers[role].Hosts, ","); got != "10.0.0.1" {
			t.Fatalf("servers.%s.hosts = %q", role, got)
		}
	}
	for _, key := range []string{"FIRST", "SECOND"} {
		if got := cfg.Env.Clear[key]; got != "$OTHER" {
			t.Fatalf("env.clear.%s = %q, want the value expanded once", key, got)
		}
	}
}

func TestLoaderEnvExpansionLeavesUnsupportedSyntax(t *testing.T) {
	path := writeEnvTestConfig(t, envTestBaseConfig+`env:
  clear:
    LOWER: ${lower}
    DEFAULT: ${FOO:-x}
    ESCAPED: echo $$FOO
    PLACEHOLDER_TEXT: azudenv0000000000000000000000z
`)
	t.Setenv("lower", "expanded")
	t.Setenv("FOO", "expanded")

	cfg, err := NewLoader(path, "").Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	want := map[string]string{
		"LOWER":            "${lower}",
		"DEFAULT":          "${FOO:-x}",
		"ESCAPED":          "echo $FOO",
		"PLACEHOLDER_TEXT": "azudenv0000000000000000000000z",
	}
	for key, value := range want {
		if got := cfg.Env.Clear[key]; got != value {
			t.Fatalf("env.clear.%s = %q, want %q", key, got, value)
		}
	}
}

func TestLoaderEnvExpansionDecodesQuotingAroundReferences(t *testing.T) {
	t.Setenv("RVX", "hello")
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "single-quoted escape inside braces", value: `'a ${RVX:-it''s}'`, want: `a ${RVX:-it's}`},
		{name: "double-quoted escapes inside braces", value: `"a ${RVX:-\"q\"}"`, want: `a ${RVX:-"q"}`},
		{name: "escaped backslash before reference", value: `"\\${RVX}"`, want: `\hello`},
		{name: "double-quoted reference", value: `"${RVX}"`, want: "hello"},
		{name: "plain backslash before reference", value: `C:\${RVX}`, want: `C:\hello`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeEnvTestConfig(t, envTestBaseConfig+"env:\n  clear:\n    A: "+tt.value+"\n")
			cfg, err := NewLoader(path, "").WithoutSecrets().Load()
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if got := cfg.Env.Clear["A"]; got != tt.want {
				t.Fatalf("env.clear.A = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoaderEnvExpansionRejectsEscapedDollarInDoubleQuotes(t *testing.T) {
	t.Setenv("RVX", "hello")
	path := writeEnvTestConfig(t, envTestBaseConfig+"env:\n  clear:\n    A: \"pre \\${RVX} post\"\n")
	cfg, err := NewLoader(path, "").WithoutSecrets().Load()
	if err == nil {
		t.Fatalf("expected YAML to reject the \\$ escape, got env.clear.A = %q", cfg.Env.Clear["A"])
	}
	if !strings.Contains(err.Error(), "unknown escape character") || strings.Contains(err.Error(), "zudenv") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnvPlaceholdersRejectAlteredTokens(t *testing.T) {
	protected, placeholders, err := protectEnvReferences([]byte("hosts: [${HOST}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSuffix(strings.TrimPrefix(string(protected), "hosts: ["), "]\n")
	if got, err := placeholders.restore(token); err != nil || got != "${HOST}" {
		t.Fatalf("restore(token) = %q, %v", got, err)
	}
	if _, err := placeholders.restore("\a" + token[1:]); err == nil {
		t.Fatal("expected an error for a token altered by YAML decoding")
	}
}

func TestLoaderRejectsDuplicateKeys(t *testing.T) {
	path := writeEnvTestConfig(t, envTestBaseConfig+"service: other\n")
	_, err := NewLoader(path, "").Load()
	if err == nil || !strings.Contains(err.Error(), `"service" already defined`) {
		t.Fatalf("expected a duplicate key error, got %v", err)
	}
}

func TestLoaderRejectsUnknownNestedKeys(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "role runtime",
			content: "servers:\n  web:\n    hosts: [localhost]\n    runtime:\n      bogus: true\n",
			want:    `"servers.web.runtime.bogus"`,
		},
		{
			name:    "accessory",
			content: "accessories:\n  db:\n    image: postgres:16\n    host: localhost\n    bogus: true\n",
			want:    `"accessories.db.bogus"`,
		},
		{
			name:    "proxy logging",
			content: "proxy:\n  host: test.example.com\n  logging:\n    bogus: true\n",
			want:    `"proxy.logging.bogus"`,
		},
		{
			name:    "cron",
			content: "cron:\n  job:\n    schedule: \"0 0 * * *\"\n    command: echo hi\n    bogus: true\n",
			want:    `"cron.job.bogus"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeEnvTestConfig(t, "service: test\nimage: test:latest\n"+tt.content)
			_, err := NewLoader(path, "").Load()
			if err == nil || !strings.Contains(err.Error(), "unknown configuration key "+tt.want) {
				t.Fatalf("expected unknown key %s, got %v", tt.want, err)
			}
		})
	}
}

func TestLoaderRejectsEmptyBaseConfig(t *testing.T) {
	path := writeEnvTestConfig(t, "# nothing here\n")
	_, err := NewLoader(path, "").Load()
	if err == nil || !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("expected an EOF error, got %v", err)
	}
}

func TestLoaderDestinationOverrideIsExpandedSafely(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.yml")
	if err := os.WriteFile(path, []byte(envTestBaseConfig), 0600); err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(dir, "deploy.staging.yml")
	if err := os.WriteFile(destPath, []byte("proxy:\n  app_port: ${PORT}\nimage: ${IMAGE}\n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PORT", "8080")
	t.Setenv("IMAGE", "test:staging")
	cfg, err := NewLoader(path, "staging").Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Proxy.AppPort != 8080 || cfg.Image != "test:staging" {
		t.Fatalf("destination override not applied: app_port=%d image=%q", cfg.Proxy.AppPort, cfg.Image)
	}

	t.Setenv("IMAGE", "test:staging\nssh:\n  insecure_ignore_host_key: true")
	if _, err := NewLoader(path, "staging").Load(); err == nil || !strings.Contains(err.Error(), "IMAGE contains a newline") {
		t.Fatalf("expected the destination override to reject a multi-line value, got %v", err)
	}
}

func TestLoaderValidatesBeforeLoadingSecrets(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "CANARY")
	path := writeEnvTestConfig(t, `service: test
servers:
  web:
    hosts: [localhost]
proxy:
  host: test.example.com
secrets_provider: command
secrets_command: touch `+canary+`
`)
	_, err := NewLoader(path, "").Load()
	if err == nil || !strings.Contains(err.Error(), "configuration validation failed") {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("secrets_command ran for an invalid configuration (stat err: %v)", err)
	}
}
