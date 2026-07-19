package deploy

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
)

func TestCanaryStatePersistsAcrossDeployerInstances(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "durable", "canary", "shop.json")
	cfg := &config.Config{Service: "shop"}
	first := NewCanaryDeployer(cfg, nil, output.DefaultLogger, statePath)
	want := &CanaryState{
		Service:         "shop",
		Status:          CanaryStatusRunning,
		StableVersion:   "v1",
		CanaryVersion:   "v2",
		CurrentWeight:   15,
		TargetWeight:    100,
		StartedAt:       time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		LastUpdated:     time.Date(2026, 7, 19, 12, 1, 0, 0, time.UTC),
		Hosts:           []string{"one", "two"},
		CanaryContainer: "shop-canary",
		StableContainer: "shop",
	}
	first.stateMu.Lock()
	first.state = want
	err := first.saveStateLocked()
	first.stateMu.Unlock()
	if err != nil {
		t.Fatalf("saveStateLocked: %v", err)
	}

	second := NewCanaryDeployer(cfg, nil, output.DefaultLogger, statePath)
	got, err := second.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restored state = %#v, want %#v", got, want)
	}

	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat state: %v", err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0600 {
		t.Fatalf("state mode = %04o, want 0600", gotMode)
	}
}
