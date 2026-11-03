package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lemonity-org/azud/internal/agent"
	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/release"
)

func TestAgentInitCreatesAPrivateIdentityOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	saved := cfg
	t.Cleanup(func() { cfg = saved })
	cfg = &config.Config{Service: "app"}

	if err := runAgentInit(testCommand(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	path := filepath.Join(home, ".config", "azud", "agent", "app.age")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity = %v, %v", info, err)
	}
	first, _ := os.ReadFile(path)
	recipient := recipientFromIdentityFile(first)
	if err := release.ParseRecipient(recipient); err != nil {
		t.Fatalf("recipient %q: %v", recipient, err)
	}

	if err := runAgentInit(testCommand(), nil); err != nil {
		t.Fatalf("second init: %v", err)
	}
	if second, _ := os.ReadFile(path); string(second) != string(first) {
		t.Fatal("init replaced an existing identity")
	}
}

func TestLocalPathExpandsHome(t *testing.T) {
	t.Setenv("HOME", "/home/deploy")
	for in, want := range map[string]string{
		"$HOME/.azud/secrets":   "/home/deploy/.azud/secrets",
		"${HOME}/.azud/secrets": "/home/deploy/.azud/secrets",
		"~/.azud/secrets":       "/home/deploy/.azud/secrets",
		"/srv/app/secrets":      "/srv/app/secrets",
	} {
		if got, err := localPath(in); err != nil || got != want {
			t.Errorf("localPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestBundleStatusRowsReadTheCurrentRecord(t *testing.T) {
	store := agent.NewStore(t.TempDir(), "app")
	records := store.Records("ghcr.io/acme/app")
	if rows := bundleStatusRows(records, ""); rows != nil {
		t.Fatalf("rows with nothing deployed = %v", rows)
	}
	if rows := bundleStatusRows(records, "unknown"); rows != nil {
		t.Fatalf("rows with no record = %v", rows)
	}
	image := &agent.Record{ID: "img", Source: agent.SourceImage, Digest: "sha256:img", Files: []release.File{{Name: "deploy.yml", Data: []byte("x")}}}
	if err := records.Store(image); err != nil {
		t.Fatal(err)
	}
	if rows := bundleStatusRows(records, "img"); rows != nil {
		t.Fatalf("rows for an image record = %v", rows)
	}
	rec := &agent.Record{
		ID:         "abc",
		Source:     agent.SourceBundle,
		Digest:     "sha256:abc",
		Info:       &release.Info{Commit: "c0ffee", Image: "sha256:img", Secrets: []string{"A", "B"}},
		Files:      []release.File{{Name: "deploy.yml", Data: []byte("x")}},
		Ignored:    []string{"podman"},
		NotApplied: []string{"cron jobs differ"},
	}
	if err := records.Store(rec); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, row := range bundleStatusRows(records, "abc") {
		got[row[0]] = row[1]
	}
	for key, want := range map[string]string{
		"Release commit":          "c0ffee",
		"Release image":           "sha256:img",
		"Release secrets":         "A, B",
		"Server settings kept":    "podman",
		"Not applied (at deploy)": "cron jobs differ",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}
