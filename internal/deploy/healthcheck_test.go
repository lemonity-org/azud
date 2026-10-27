package deploy

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/shell"
)

func TestProbeAdmitsTraffic(t *testing.T) {
	tests := []struct {
		name                string
		readinessConfigured bool
		readinessHealthy    bool
		livenessHealthy     bool
		want                bool
	}{
		{name: "live but not ready", readinessConfigured: true, livenessHealthy: true, want: false},
		{name: "ready while liveness is starting", readinessConfigured: true, readinessHealthy: true, want: true},
		{name: "readiness and liveness healthy", readinessConfigured: true, readinessHealthy: true, livenessHealthy: true, want: true},
		{name: "liveness only healthy", livenessHealthy: true, want: true},
		{name: "no successful probe", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := probeAdmitsTraffic(tt.readinessConfigured, tt.readinessHealthy, tt.livenessHealthy); got != tt.want {
				t.Fatalf("probeAdmitsTraffic() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestBuildHTTPCheckHelperCommandUsesPinnedDefault(t *testing.T) {
	command := BuildHTTPCheckHelperCommand("app", 3000, "/ready", "", "")
	for _, want := range []string{
		"--pull=missing",
		"--network container:app",
		config.DefaultHealthcheckHelperImage,
		"http://127.0.0.1:3000/ready",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("helper command %q does not contain %q", command, want)
		}
	}
}

func TestLivenessCommandModes(t *testing.T) {
	cfg := &config.Config{}
	cfg.Proxy.AppPort = 3000
	cfg.Proxy.Healthcheck.Path = "/up"

	if got := LivenessCommand(cfg); !strings.Contains(got, "http://127.0.0.1:3000/up") {
		t.Fatalf("default liveness command = %q", got)
	}
	cfg.Proxy.Healthcheck.LivenessCmd = "check-live"
	if got := LivenessCommand(cfg); got != "check-live" {
		t.Fatalf("custom liveness command = %q", got)
	}
	cfg.Proxy.Healthcheck.DisableLiveness = true
	if got := LivenessCommand(cfg); got != "" {
		t.Fatalf("disabled liveness command = %q", got)
	}
}

func TestReadinessCommandTakesPrecedenceOverDefaultPath(t *testing.T) {
	cfg := &config.Config{}
	cfg.Proxy.Healthcheck.Path = "/up"
	cfg.Proxy.Healthcheck.ReadinessCmd = "grpc_health_probe -addr 127.0.0.1:3000"

	if got := ReadinessCommand(cfg); got != "grpc_health_probe -addr 127.0.0.1:3000" {
		t.Fatalf("readiness command = %q", got)
	}
	if !HasReadinessProbe(cfg) {
		t.Fatal("custom readiness command was not recognized as a readiness probe")
	}
}

func TestHasReadinessProbeModes(t *testing.T) {
	cfg := &config.Config{}
	if HasReadinessProbe(cfg) {
		t.Fatal("empty healthcheck unexpectedly has readiness")
	}
	cfg.Proxy.Healthcheck.ReadinessPath = "/ready"
	if !HasReadinessProbe(cfg) {
		t.Fatal("readiness path was not recognized")
	}
}

func TestCommandNotFoundClassification(t *testing.T) {
	for _, output := range []string{
		"curl: not found",
		"executable file not found in $PATH",
		"no such file or directory",
	} {
		if !outputIndicatesCommandNotFound(output) {
			t.Fatalf("expected command-not-found classification for %q", output)
		}
	}
	if outputIndicatesCommandNotFound("connection refused") {
		t.Fatal("connection refusal must not be classified as a missing helper")
	}
}

func TestHTTPCheckBuildersRejectInvalidPaths(t *testing.T) {
	for _, path := range []string{
		"",
		"up",
		"/$(id)",
		"/`id`",
		"/a'b",
		"/a\"b",
		"/a b",
		"/a;id",
		"/a|id",
		"/a\nid",
	} {
		t.Run(path, func(t *testing.T) {
			if got := BuildHTTPCheckCommand(3000, path); got != "" {
				t.Errorf("BuildHTTPCheckCommand(%q) = %q, want empty", path, got)
			}
			if got := BuildHTTPCheckExecCandidates("app", 3000, path); got != nil {
				t.Errorf("BuildHTTPCheckExecCandidates(%q) = %q, want nil", path, got)
			}
			if got := BuildHTTPCheckHelperCommand("app", 3000, path, "", ""); got != "" {
				t.Errorf("BuildHTTPCheckHelperCommand(%q) = %q, want empty", path, got)
			}
		})
	}
	if got := BuildHTTPCheckCommand(0, "/up"); got != "" {
		t.Errorf("BuildHTTPCheckCommand with port 0 = %q, want empty", got)
	}
}

func TestHTTPCheckBuildersShellQuoteURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	// '&' and '?' are valid in a health path but are shell syntax, so the URL
	// only reaches the client intact when it is quoted.
	path := "/up?x=1&y=%2F&z=a:b@c"
	url := "http://127.0.0.1:3000" + path

	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{
			name:    "container command",
			command: BuildHTTPCheckCommand(3000, path),
			want:    []string{"curl", "-fsS", url},
		},
		{
			name:    "container command under health-cmd quoting",
			command: "sh -c " + shell.Quote(BuildHTTPCheckCommand(3000, path)),
			want:    []string{"curl", "-fsS", url},
		},
		{
			name:    "exec candidate",
			command: BuildHTTPCheckExecCandidates("app", 3000, path)[0],
			want:    []string{"podman", "exec", "app", "curl", "-fsS", url},
		},
		{
			name:    "helper container",
			command: BuildHTTPCheckHelperCommand("app", 3000, path, "", ""),
			want: []string{
				"podman", "run", "--rm", "--pull=missing", "--network", "container:app",
				"--name", "azud-hc-app", config.DefaultHealthcheckHelperImage,
				"-fsS", "-o", "/dev/null", url,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runWithRecordingStubs(t, tt.command)
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Fatalf("argv = %q, want %q (command %q)", got, tt.want, tt.command)
			}
		})
	}
}

// runWithRecordingStubs runs command under sh with curl and podman stubs on
// PATH and returns the argv (including the program name) they recorded.
func runWithRecordingStubs(t *testing.T, command string) []string {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	for _, name := range []string{"curl", "podman"} {
		script := "#!/bin/sh\nprintf '%s\\0' " + name + " \"$@\" > " + shell.Quote(record) + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run %q: %v\n%s", command, err, out)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("stub did not record argv for %q: %v", command, err)
	}
	return strings.Split(string(bytes.TrimSuffix(data, []byte{0})), "\x00")
}
