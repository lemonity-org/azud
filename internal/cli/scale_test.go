package cli

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/config"
)

type recordingRetirer struct {
	calls     []string
	stopErr   error
	removeErr error
}

func (r *recordingRetirer) Stop(host, container string, timeout int) error {
	r.calls = append(r.calls, fmt.Sprintf("stop %s %s timeout=%d", host, container, timeout))
	return r.stopErr
}

func (r *recordingRetirer) Remove(host, container string, force bool) error {
	r.calls = append(r.calls, fmt.Sprintf("remove %s %s force=%t", host, container, force))
	return r.removeErr
}

func TestRetireInstanceStopsBeforeRemoving(t *testing.T) {
	r := &recordingRetirer{}

	if err := retireInstance(r, "app.example.com", "shop-0", 45); err != nil {
		t.Fatalf("retireInstance: %v", err)
	}

	want := []string{
		"stop app.example.com shop-0 timeout=45",
		"remove app.example.com shop-0 force=true",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls = %v, want %v", r.calls, want)
	}
}

func TestRetireInstanceDoesNotRemoveAfterFailedStop(t *testing.T) {
	stopErr := errors.New("podman stop failed")
	r := &recordingRetirer{stopErr: stopErr}

	err := retireInstance(r, "app.example.com", "shop-0", 30)
	if !errors.Is(err, stopErr) || !strings.Contains(err.Error(), "failed to stop shop-0") {
		t.Fatalf("expected wrapped stop error, got %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("container must be left in place after a failed stop, calls = %v", r.calls)
	}
}

func TestRetireInstanceReportsRemoveFailure(t *testing.T) {
	removeErr := errors.New("podman rm failed")
	r := &recordingRetirer{removeErr: removeErr}

	err := retireInstance(r, "app.example.com", "shop-0", 30)
	if !errors.Is(err, removeErr) || !strings.Contains(err.Error(), "failed to remove shop-0") {
		t.Fatalf("expected wrapped remove error, got %v", err)
	}
}

func TestRunScaleRejectsStopFirstRole(t *testing.T) {
	previous := cfg
	t.Cleanup(func() { cfg = previous })
	cfg = &config.Config{
		Service: "shop",
		Servers: map[string]config.RoleConfig{
			"worker": {
				Hosts:    []string{"worker.example.com"},
				Strategy: "stop_first",
			},
		},
	}

	err := runScale(nil, []string{"worker=2"})
	if err == nil || !strings.Contains(err.Error(), "cannot be scaled") {
		t.Fatalf("expected singleton scale rejection, got %v", err)
	}
}
