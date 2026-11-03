package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/podman"
)

func driftConfig() *config.Config {
	return &config.Config{
		Service: "shop",
		Image:   "registry.test/shop@sha256:aa11",
		Servers: map[string]config.RoleConfig{"web": {Hosts: []string{"10.0.0.1"}}},
		Accessories: map[string]config.AccessoryConfig{
			"db":    {Image: "postgres:16", Host: "10.0.0.1"},
			"cache": {Image: "redis:7", Hosts: []string{"10.0.0.1"}},
			"far":   {Image: "redis:7", Host: "10.0.0.9"},
		},
		Cron: map[string]config.CronConfig{
			"backup": {Schedule: "0 2 * * *", Command: "bin/backup"},
			"far":    {Schedule: "0 2 * * *", Command: "bin/far", Host: "10.0.0.9"},
		},
	}
}

func accessoryContainer(cfg *config.Config, name, spec string) podman.Container {
	labels := map[string]string{"azud.service": cfg.Service, "azud.accessory": name}
	if spec != "" {
		labels[config.SpecLabel] = spec
	}
	return podman.Container{Name: cfg.Service + "-" + name, Labels: labels}
}

func cronContainer(cfg *config.Config, name, spec string) podman.Container {
	labels := map[string]string{"azud.service": cfg.Service, "azud.cron": name}
	if spec != "" {
		labels[config.SpecLabel] = spec
	}
	return podman.Container{Name: cfg.Service + "-cron-" + name, Labels: labels}
}

func TestDetectDriftInSync(t *testing.T) {
	cfg := driftConfig()
	containers := []podman.Container{
		accessoryContainer(cfg, "db", cfg.AccessorySpec("db")),
		accessoryContainer(cfg, "cache", cfg.AccessorySpec("cache")),
		cronContainer(cfg, "backup", cfg.CronSpec("backup")),
		// A role, another service's accessory, and a manual run are not this check's.
		{Name: "shop", Labels: map[string]string{"azud.service": "shop", "azud.role": "web"}},
		{Name: "other-db", Labels: map[string]string{"azud.service": "other", "azud.accessory": "db"}},
		{Name: "shop-cron-backup-run", Labels: map[string]string{"azud.service": "shop", "azud.cron": "gone", "azud.cron.run": "manual"}},
	}
	if drift := DetectDrift(cfg, "10.0.0.1", containers); len(drift) != 0 {
		t.Errorf("drift = %+v, want none", drift)
	}
}

func TestDetectDriftReportsEachDifferenceWithItsCommand(t *testing.T) {
	cfg := driftConfig()
	containers := []podman.Container{
		accessoryContainer(cfg, "db", "another-spec"),
		accessoryContainer(cfg, "gone", "any"),
		cronContainer(cfg, "backup", "another-spec"),
		cronContainer(cfg, "gone", "any"),
	}
	want := []Drift{
		{Kind: DriftAccessory, Name: "cache", Container: "shop-cache", State: DriftMissing, Command: "azud accessory boot cache"},
		{Kind: DriftAccessory, Name: "db", Container: "shop-db", State: DriftChanged, Command: "azud accessory remove db && azud accessory boot db"},
		{Kind: DriftAccessory, Name: "gone", Container: "shop-gone", State: DriftExtra, Command: "podman rm -f shop-gone"},
		{Kind: DriftCron, Name: "backup", Container: "shop-cron-backup", State: DriftChanged, Command: "azud cron boot backup"},
		{Kind: DriftCron, Name: "gone", Container: "shop-cron-gone", State: DriftExtra, Command: "podman rm -f shop-cron-gone"},
	}
	if got := DetectDrift(cfg, "10.0.0.1", containers); !reflect.DeepEqual(got, want) {
		t.Errorf("drift =\n%+v\nwant\n%+v", got, want)
	}
}

// A container an earlier Azud created has no label. Nothing says it differs, so it
// must not read as drift.
func TestDetectDriftReportsAContainerWithNoLabelAsUnknown(t *testing.T) {
	cfg := driftConfig()
	containers := []podman.Container{
		accessoryContainer(cfg, "db", ""),
		accessoryContainer(cfg, "cache", cfg.AccessorySpec("cache")),
		cronContainer(cfg, "backup", ""),
	}
	got := DetectDrift(cfg, "10.0.0.1", containers)
	if len(got) != 2 {
		t.Fatalf("drift = %+v", got)
	}
	for _, d := range got {
		if d.State != DriftUnknown || d.Command == "" {
			t.Errorf("%s = %+v, want unknown with a command", d.Name, d)
		}
	}
}

func TestDetectDriftOnlyLooksAtWhatTheReleasePlacesOnThisHost(t *testing.T) {
	cfg := driftConfig()
	for _, d := range DetectDrift(cfg, "10.0.0.1", nil) {
		if d.Name == "far" {
			t.Errorf("reported %+v, which runs on another host", d)
		}
	}
}

func TestDetectDriftNoticesANewImageForCronOnly(t *testing.T) {
	cfg := driftConfig()
	containers := []podman.Container{
		accessoryContainer(cfg, "db", cfg.AccessorySpec("db")),
		accessoryContainer(cfg, "cache", cfg.AccessorySpec("cache")),
		cronContainer(cfg, "backup", cfg.CronSpec("backup")),
	}
	cfg.Image = "registry.test/shop@sha256:bb22"
	got := DetectDrift(cfg, "10.0.0.1", containers)
	if len(got) != 1 || got[0].Kind != DriftCron || got[0].State != DriftChanged || !strings.Contains(got[0].Command, "cron boot") {
		t.Errorf("drift = %+v, want only the cron job, because accessories name their own image", got)
	}
}
