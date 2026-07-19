package deploy

import (
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
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
