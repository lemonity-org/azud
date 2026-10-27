package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

func useBuildSecretsPath(t *testing.T, secretsPath string) {
	t.Helper()
	previousConfig := cfg
	cfg = &config.Config{SecretsPath: secretsPath}
	t.Cleanup(func() { cfg = previousConfig })
}

func TestEffectiveContainerIgnorePrecedence(t *testing.T) {
	tests := []struct {
		name       string
		files      []string
		dockerfile string
		want       string
	}{
		{name: "none", want: ""},
		{name: "dockerignore only", files: []string{".dockerignore"}, dockerfile: "Dockerfile", want: ".dockerignore"},
		{name: "containerignore shadows dockerignore", files: []string{".dockerignore", ".containerignore"}, dockerfile: "Dockerfile", want: ".containerignore"},
		{name: "containerfile specific wins", files: []string{".containerignore", "Dockerfile.containerignore"}, dockerfile: "Dockerfile", want: "Dockerfile.containerignore"},
		{name: "containerfile dockerignore beats containerignore", files: []string{"Dockerfile.containerignore", "Dockerfile.dockerignore"}, dockerfile: "Dockerfile", want: "Dockerfile.dockerignore"},
		{name: "nested containerfile", files: []string{".dockerignore", "build/App.Containerfile.containerignore"}, dockerfile: "build/App.Containerfile", want: "build/App.Containerfile.containerignore"},
		{name: "default containerfile", files: []string{"Containerfile", "Containerfile.containerignore", ".dockerignore"}, want: "Containerfile.containerignore"},
		{name: "default dockerfile", files: []string{"Dockerfile.dockerignore", ".dockerignore"}, want: "Dockerfile.dockerignore"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contextDir := t.TempDir()
			for _, name := range tt.files {
				path := filepath.Join(contextDir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := effectiveContainerIgnore(contextDir, tt.dockerfile)
			if err != nil {
				t.Fatalf("effectiveContainerIgnore: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("effective ignore file = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMandatoryLocalBuildExcludes(t *testing.T) {
	contextDir := t.TempDir()
	tests := []struct {
		name        string
		secretsPath string
		want        []string
		wantErr     bool
	}{
		{name: "default", secretsPath: filepath.Join(contextDir, ".azud", "secrets"), want: []string{".azud/secrets", "**/.azud/secrets", ".azud/.secrets-*.tmp", "**/.azud/.secrets-*.tmp"}},
		{name: "custom in context", secretsPath: filepath.Join(contextDir, "config", "prod.secrets"), want: []string{".azud/secrets", "**/.azud/secrets", ".azud/.secrets-*.tmp", "**/.azud/.secrets-*.tmp", "config/prod.secrets", "config/.secrets-*.tmp"}},
		{name: "custom at context root", secretsPath: filepath.Join(contextDir, "app.env"), want: []string{".azud/secrets", "**/.azud/secrets", ".azud/.secrets-*.tmp", "**/.azud/.secrets-*.tmp", "app.env", ".secrets-*.tmp"}},
		{name: "outside context", secretsPath: filepath.Join(filepath.Dir(contextDir), "outside.env"), want: []string{".azud/secrets", "**/.azud/secrets", ".azud/.secrets-*.tmp", "**/.azud/.secrets-*.tmp"}},
		{name: "glob characters escaped", secretsPath: filepath.Join(contextDir, "s[1]", "*?.env"), want: []string{".azud/secrets", "**/.azud/secrets", ".azud/.secrets-*.tmp", "**/.azud/.secrets-*.tmp", `s\[1]/\*\?.env`, `s\[1]/.secrets-*.tmp`}},
		{name: "leading bang escaped", secretsPath: filepath.Join(contextDir, "!prod.env"), want: []string{".azud/secrets", "**/.azud/secrets", ".azud/.secrets-*.tmp", "**/.azud/.secrets-*.tmp", `\!prod.env`, ".secrets-*.tmp"}},
		{name: "newline rejected", secretsPath: filepath.Join(contextDir, "bad\nname"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.ContainsAny(tt.secretsPath, "*?\n") {
				t.Skip("path is not valid on Windows")
			}
			useBuildSecretsPath(t, tt.secretsPath)
			got, err := mandatoryLocalBuildExcludes(contextDir)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("mandatoryLocalBuildExcludes: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("excludes = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteLocalBuildIgnoreFileAppendsMandatoryExcludesLast(t *testing.T) {
	contextDir := t.TempDir()
	useBuildSecretsPath(t, filepath.Join(contextDir, "config", "prod.secrets"))
	user := "node_modules\n!.azud/secrets\n!config/prod.secrets"
	if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}

	path, cleanup, err := writeLocalBuildIgnoreFile(contextDir, "Dockerfile")
	if err != nil {
		t.Fatalf("writeLocalBuildIgnoreFile: %v", err)
	}
	if rel, err := filepath.Rel(contextDir, path); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("ignore file %s is inside the build context", path)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("ignore file mode = %04o, want 0600", info.Mode().Perm())
		}
	}

	body := readFileString(t, path)
	if !strings.HasPrefix(body, user+"\n") {
		t.Fatalf("user patterns were not preserved verbatim:\n%s", body)
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	tail := lines[len(lines)-6:]
	want := []string{".azud/secrets", "**/.azud/secrets", ".azud/.secrets-*.tmp", "**/.azud/.secrets-*.tmp", "config/prod.secrets", "config/.secrets-*.tmp"}
	if !slices.Equal(tail, want) {
		t.Fatalf("mandatory excludes must come last, got tail %q:\n%s", tail, body)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup left the ignore file behind: %v", err)
	}
}

func TestBuildLocalPassesMandatoryIgnoreFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script as a fake podman")
	}
	contextDir := t.TempDir()
	binDir := t.TempDir()
	record := filepath.Join(t.TempDir(), "record")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$AZUD_FAKE_PODMAN_RECORD.args\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = --ignorefile ]; then cp \"$2\" \"$AZUD_FAKE_PODMAN_RECORD.ignore\"; fi\n" +
		"  shift\n" +
		"done\n"
	if err := os.WriteFile(filepath.Join(binDir, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AZUD_FAKE_PODMAN_RECORD", record)

	previousConfig := cfg
	cfg = &config.Config{
		SecretsPath: filepath.Join(contextDir, "prod.env"),
		Builder:     config.BuilderConfig{Context: contextDir},
	}
	t.Cleanup(func() { cfg = previousConfig })

	if err := buildLocal("example/app:v1", "example/app:latest", false); err != nil {
		t.Fatalf("buildLocal: %v", err)
	}

	args := strings.Split(strings.TrimSpace(readFileString(t, record+".args")), "\n")
	idx := slices.Index(args, "--ignorefile")
	if idx < 0 || idx+2 != len(args)-1 || args[len(args)-1] != contextDir {
		t.Fatalf("expected --ignorefile <file> right before the context, got %q", args)
	}
	ignore := readFileString(t, record+".ignore")
	for _, pattern := range []string{".azud/secrets", "prod.env"} {
		if !slices.Contains(strings.Split(ignore, "\n"), pattern) {
			t.Fatalf("ignore file does not exclude %s:\n%s", pattern, ignore)
		}
	}
	if _, err := os.Stat(args[idx+1]); !os.IsNotExist(err) {
		t.Fatalf("temporary ignore file was not removed: %v", err)
	}
}

func TestLocalBuildExcludesMatchRemoteForNestedSecrets(t *testing.T) {
	root := t.TempDir()
	files := []string{
		"apps/web/Dockerfile",
		"apps/web/app.go",
		"apps/web/.azud/secrets",
		"apps/web/.azud/.secrets-1.tmp",
		"apps/api/.azud/secrets",
		"apps/api/.azud/.secrets-2.tmp",
		".azud/.secrets-3.tmp",
	}
	for _, name := range files {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(name)), name)
	}
	useSecretsPath(t, root, filepath.Join(root, "apps", "web", ".azud", "secrets"))

	path, cleanup, err := writeLocalBuildIgnoreFile(root, "apps/web/Dockerfile")
	if err != nil {
		t.Fatalf("writeLocalBuildIgnoreFile: %v", err)
	}
	defer cleanup()
	local := parseIgnorePatterns(readFileString(t, path))
	remote := archiveHeadersExcluding(t, root, readContainerIgnore(root), secretContextExclusions(root))

	for _, name := range files {
		localKept := !shouldIgnore(name, false, local)
		_, remoteKept := remote[name]
		if localKept != remoteKept {
			t.Errorf("%s: kept by local build = %v, by remote build = %v", name, localKept, remoteKept)
		}
		if wantKept := !strings.Contains(name, ".azud/"); localKept != wantKept {
			t.Errorf("%s: kept by local build = %v, want %v", name, localKept, wantKept)
		}
	}
}
