package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/release"
)

const effectiveRepository = "registry.test/shop"

// effectiveServerConfig is a server's configuration, with a track so it has an agent.
func effectiveServerConfig(t *testing.T, source string) *config.Config {
	t.Helper()
	server := &config.Config{
		Service: "shop",
		Image:   effectiveRepository + ":configured",
		Servers: map[string]config.RoleConfig{"web": {Hosts: []string{"10.0.0.1"}}},
		Proxy:   config.ProxyConfig{Host: "shop.test", AppPort: 8080},
		SSH:     config.SSHConfig{User: "deploy", Port: 22},
		Agent:   config.AgentConfig{Track: "production", Source: source, VerifyCommand: `cosign verify "$AZUD_AGENT_IMAGE"`},
	}
	return server
}

const effectiveRelease = `service: shop
image: registry.test/shop:from-the-release
servers:
  web:
    hosts: [10.0.0.1]
proxy:
  host: shop.test
  app_port: 8080
cron:
  backup:
    schedule: "0 2 * * *"
    command: bin/backup
`

func storeEffectiveRecord(t *testing.T, store *Store, rec *Record) {
	t.Helper()
	rec.Files = []release.File{{Name: "deploy.yml", Data: []byte(effectiveRelease)}}
	if err := store.Records(effectiveRepository).Store(rec); err != nil {
		t.Fatal(err)
	}
}

func TestLoadEffectiveBeforeTheFirstDeploy(t *testing.T) {
	server := effectiveServerConfig(t, "")
	effective, err := LoadEffective(server, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if effective.Deployed || effective.Settling || effective.Record != nil {
		t.Errorf("effective = %+v, want nothing deployed", effective)
	}
	if effective.Config != server {
		t.Error("before the first deploy, Config must be the server's configuration")
	}
}

func TestLoadEffectivePinsTheRecordsImage(t *testing.T) {
	const digest = "sha256:" + "aa11"
	tests := []struct {
		name   string
		source string
		rec    *Record
		image  string
	}{
		{
			name:  "image",
			rec:   &Record{ID: "aa11-abcd1234", Source: SourceImage, Digest: digest, Image: effectiveRepository + "@" + digest},
			image: effectiveRepository + "@" + digest,
		},
		{
			name:   "bundle, whose digest is not its image's",
			source: config.AgentSourceBundle,
			rec:    &Record{ID: "aa11", Source: SourceBundle, Digest: digest, Image: effectiveRepository + "@sha256:bb22", Info: &release.Info{Image: "sha256:bb22"}},
			image:  effectiveRepository + "@sha256:bb22",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewStore(dir, "shop")
			storeEffectiveRecord(t, store, tt.rec)
			if err := store.Save(State{DeployedDigest: digest, Current: tt.rec.ID}); err != nil {
				t.Fatal(err)
			}
			server := effectiveServerConfig(t, tt.source)
			effective, err := LoadEffective(server, dir)
			if err != nil {
				t.Fatal(err)
			}
			if !effective.Deployed || effective.Settling || effective.Record == nil {
				t.Fatalf("effective = %+v", effective)
			}
			if effective.Config.Image != tt.image {
				t.Errorf("Image = %q, want %q", effective.Config.Image, tt.image)
			}
			if got := effective.Config.Cron["backup"].Command; got != "bin/backup" {
				t.Errorf("cron command = %q, want the release's", got)
			}
			if effective.Config.SSH.User != "deploy" || effective.Config.Agent.Track != "production" {
				t.Errorf("trust settings must come from the server: %+v", effective.Config.Agent)
			}
			if server.Image != effectiveRepository+":configured" {
				t.Errorf("the server's configuration changed: %q", server.Image)
			}
		})
	}
}

func TestLoadEffectiveImageWithNoRecordYet(t *testing.T) {
	dir := t.TempDir()
	if err := NewStore(dir, "shop").Save(State{DeployedDigest: "sha256:cc33"}); err != nil {
		t.Fatal(err)
	}
	server := effectiveServerConfig(t, "")
	effective, err := LoadEffective(server, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !effective.Deployed || effective.Config.Image != effectiveRepository+"@sha256:cc33" {
		t.Errorf("effective = %+v, image %q", effective, effective.Config.Image)
	}
	if server.Image != effectiveRepository+":configured" {
		t.Errorf("the server's configuration changed: %q", server.Image)
	}
}

// A bundle's digest names a bundle, not an image, so it cannot be pinned without its
// record.
func TestLoadEffectiveBundleWithNoRecordIsNotDeployed(t *testing.T) {
	dir := t.TempDir()
	if err := NewStore(dir, "shop").Save(State{DeployedDigest: "sha256:cc33"}); err != nil {
		t.Fatal(err)
	}
	effective, err := LoadEffective(effectiveServerConfig(t, config.AgentSourceBundle), dir)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Deployed {
		t.Errorf("effective = %+v, want nothing deployed", effective)
	}
}

func TestLoadEffectiveWhileSettlingReadsTheLastWholeRelease(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir, "shop")
	storeEffectiveRecord(t, store, &Record{ID: "aa11-abcd1234", Source: SourceImage, Digest: "sha256:aa11", Image: effectiveRepository + "@sha256:aa11"})
	journal := &Journal{Target: "bb22-abcd1234", Digest: "sha256:bb22", Phase: PhaseForward}
	if err := store.Save(State{DeployedDigest: "sha256:aa11", Current: "aa11-abcd1234", Journal: journal}); err != nil {
		t.Fatal(err)
	}
	effective, err := LoadEffective(effectiveServerConfig(t, ""), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !effective.Settling || effective.Journal == nil || effective.Journal.Digest != "sha256:bb22" {
		t.Errorf("effective = %+v, want settling on sha256:bb22", effective)
	}
	if got := effective.Config.Image; got != effectiveRepository+"@sha256:aa11" {
		t.Errorf("Image = %q, want the last whole release", got)
	}
}

func TestLoadEffectiveFirstReleaseSettling(t *testing.T) {
	dir := t.TempDir()
	if err := NewStore(dir, "shop").Save(State{Journal: &Journal{Target: "aa11-abcd1234", Digest: "sha256:aa11", Phase: PhaseForward, First: true}}); err != nil {
		t.Fatal(err)
	}
	effective, err := LoadEffective(effectiveServerConfig(t, ""), dir)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Deployed || !effective.Settling {
		t.Errorf("effective = %+v, want settling with nothing deployed", effective)
	}
}

// A deploy_command script runs while the agent holds the state lock. A second lock
// on the same file would block forever, so reading must not take it.
func TestLoadEffectiveTakesNoLock(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir, "shop")
	if err := store.Save(State{DeployedDigest: "sha256:cc33"}); err != nil {
		t.Fatal(err)
	}
	unlock, err := store.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := LoadEffective(effectiveServerConfig(t, ""), dir); err != nil {
		t.Fatal(err)
	}
}

func TestLoadEffectiveStoredV1Release(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir, "shop")
	storeEffectiveRecord(t, store, &Record{ID: "aa11", Source: SourceBundle, Digest: "sha256:aa11", Info: &release.Info{Image: "sha256:bb22"}})
	// v1 wrote no record file.
	if err := os.Remove(filepath.Join(store.Records(effectiveRepository).Dir("aa11"), recordFile)); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(State{DeployedDigest: "sha256:aa11"}); err != nil {
		t.Fatal(err)
	}
	effective, err := LoadEffective(effectiveServerConfig(t, config.AgentSourceBundle), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !effective.Deployed || effective.Config.Image != effectiveRepository+"@sha256:bb22" {
		t.Errorf("effective = %+v, image %q", effective, effective.Config.Image)
	}
}
