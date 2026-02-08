package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
)

func TestRunHistoryList(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})

	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	buf := setupHistoryTestState(t)
	history := deploy.NewHistoryStore(".", 20, output.DefaultLogger)

	base := time.Date(2026, 2, 8, 12, 0, 0, 0, time.UTC)
	if err := history.Record(newHistoryRecord(
		"deploy_1", "test-service", "v1.2.2", "ghcr.io/acme/test:v1.2.2",
		base, base.Add(40*time.Second), deploy.StatusSuccess, []string{"10.0.0.1"},
	)); err != nil {
		t.Fatalf("record first history entry: %v", err)
	}
	if err := history.Record(newHistoryRecord(
		"deploy_2", "test-service", "v1.2.3", "ghcr.io/acme/test:v1.2.3",
		base.Add(30*time.Second), base.Add(80*time.Second), deploy.StatusFailed, []string{"10.0.0.1", "10.0.0.2"},
	)); err != nil {
		t.Fatalf("record second history entry: %v", err)
	}

	historyLimit = 20
	if err := runHistoryList(nil, nil); err != nil {
		t.Fatalf("runHistoryList: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"Deployment History",
		"deploy_1",
		"deploy_2",
		"v1.2.2",
		"v1.2.3",
		"failed",
		"Show details with: azud history show <id>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestRunHistoryShow(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})

	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	buf := setupHistoryTestState(t)
	history := deploy.NewHistoryStore(".", 20, output.DefaultLogger)

	base := time.Date(2026, 2, 8, 12, 0, 0, 0, time.UTC)
	record := newHistoryRecord(
		"deploy_42", "test-service", "v2.0.0", "ghcr.io/acme/test:v2.0.0",
		base, base.Add(90*time.Second), deploy.StatusSuccess, []string{"10.0.0.1", "10.0.0.2"},
	)
	record.PreviousVersion = "v1.9.9"
	record.Metadata["type"] = "canary"
	record.Metadata["weight"] = "10"
	if err := history.Record(record); err != nil {
		t.Fatalf("record history entry: %v", err)
	}

	if err := runHistoryShow(nil, []string{"deploy_42"}); err != nil {
		t.Fatalf("runHistoryShow: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"Deployment deploy_42",
		"Service: test-service",
		"Version: v2.0.0",
		"Status: success",
		"Destination: production",
		"Hosts: 10.0.0.1, 10.0.0.2",
		"Previous Version: v1.9.9",
		"Metadata:",
		"type",
		"canary",
		"weight",
		"10",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestRunHistoryShowNotFound(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})

	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	setupHistoryTestState(t)

	err = runHistoryShow(nil, []string{"deploy_missing"})
	if err == nil {
		t.Fatal("expected error for missing deployment record")
	}
	if !strings.Contains(err.Error(), "deployment record deploy_missing not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFormatHistoryHosts(t *testing.T) {
	tests := []struct {
		name  string
		hosts []string
		want  string
	}{
		{name: "none", hosts: nil, want: "-"},
		{name: "single", hosts: []string{"10.0.0.1"}, want: "10.0.0.1"},
		{name: "two", hosts: []string{"10.0.0.1", "10.0.0.2"}, want: "10.0.0.1,10.0.0.2"},
		{name: "many", hosts: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, want: "10.0.0.1 +2"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatHistoryHosts(tc.hosts); got != tc.want {
				t.Fatalf("formatHistoryHosts() = %q, want %q", got, tc.want)
			}
		})
	}
}

func setupHistoryTestState(t *testing.T) *bytes.Buffer {
	t.Helper()

	oldLogger := output.DefaultLogger
	oldCfg := cfg
	oldVerbose := verbose
	oldLimit := historyLimit

	var buf bytes.Buffer
	output.DefaultLogger = output.NewLogger(&buf, &buf, false)
	cfg = &config.Config{
		Service: "test-service",
		Deploy: config.DeployConfig{
			RetainHistory: 20,
		},
	}
	verbose = false
	historyLimit = 20

	t.Cleanup(func() {
		output.DefaultLogger = oldLogger
		cfg = oldCfg
		verbose = oldVerbose
		historyLimit = oldLimit
	})

	return &buf
}

func newHistoryRecord(
	id, service, version, image string,
	started, completed time.Time,
	status deploy.DeploymentStatus,
	hosts []string,
) *deploy.DeploymentRecord {
	return &deploy.DeploymentRecord{
		ID:          id,
		Service:     service,
		Image:       image,
		Version:     version,
		Destination: "production",
		Hosts:       hosts,
		Status:      status,
		StartedAt:   started,
		CompletedAt: completed,
		Duration:    completed.Sub(started),
		Metadata:    map[string]string{},
	}
}
