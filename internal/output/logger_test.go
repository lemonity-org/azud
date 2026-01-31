package output

import (
	"bytes"
	"strings"
	"testing"
)

func newTestLogger() (*Logger, *bytes.Buffer, *bytes.Buffer) {
	var outBuf, errBuf bytes.Buffer
	l := NewLogger(&outBuf, &errBuf, false)
	return l, &outBuf, &errBuf
}

// --- TrafficBar tests ---

func TestTrafficBar_ProfileNone(t *testing.T) {
	SetProfile(ProfileNone)
	defer SetProfile(ProfileNone)

	l, out, _ := newTestLogger()
	l.TrafficBar(30, "canary (v2.0)", "stable (v1.5)")

	got := out.String()
	if strings.Contains(got, "\033[") {
		t.Errorf("expected no ANSI escapes in ProfileNone, got %q", got)
	}
	if !strings.Contains(got, "30%") {
		t.Errorf("expected '30%%' in output, got %q", got)
	}
	if !strings.Contains(got, "70%") {
		t.Errorf("expected '70%%' in output, got %q", got)
	}
	if !strings.Contains(got, "canary (v2.0)") {
		t.Errorf("expected canary label in output, got %q", got)
	}
	if !strings.Contains(got, "stable (v1.5)") {
		t.Errorf("expected stable label in output, got %q", got)
	}
}

func TestTrafficBar_TrueColor(t *testing.T) {
	SetProfile(ProfileTrueColor)
	defer SetProfile(ProfileNone)

	l, out, _ := newTestLogger()
	l.TrafficBar(25, "canary (v2.0)", "stable (v1.5)")

	got := out.String()
	if !strings.Contains(got, SymFilled) {
		t.Errorf("expected SymFilled in TrueColor output, got %q", got)
	}
	if !strings.Contains(got, "25%") {
		t.Errorf("expected '25%%' in output, got %q", got)
	}
	if !strings.Contains(got, "75%") {
		t.Errorf("expected '75%%' in output, got %q", got)
	}
}

func TestTrafficBar_Boundaries(t *testing.T) {
	tests := []struct {
		name       string
		input      int
		wantCanary string
		wantStable string
	}{
		{"zero", 0, "0%", "100%"},
		{"hundred", 100, "100%", "0%"},
		{"clamp negative", -10, "0%", "100%"},
		{"clamp over 100", 150, "100%", "0%"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetProfile(ProfileNone)
			defer SetProfile(ProfileNone)

			l, out, _ := newTestLogger()
			l.TrafficBar(tt.input, "canary", "stable")

			got := out.String()
			if !strings.Contains(got, tt.wantCanary) {
				t.Errorf("expected %q in output, got %q", tt.wantCanary, got)
			}
			if !strings.Contains(got, tt.wantStable) {
				t.Errorf("expected %q in output, got %q", tt.wantStable, got)
			}
		})
	}
}

// --- HostPhase tests ---

func TestHostPhase_PendingPhases(t *testing.T) {
	SetProfile(ProfileNone)
	defer SetProfile(ProfileNone)

	l, out, _ := newTestLogger()
	phases := []Phase{
		{Name: "Pull", Complete: false},
		{Name: "Health", Complete: false},
	}
	l.HostPhase("host1", phases)

	got := out.String()
	if !strings.Contains(got, SymPending) {
		t.Errorf("expected SymPending for pending phases, got %q", got)
	}
	if !strings.Contains(got, "host1") {
		t.Errorf("expected host name in output, got %q", got)
	}
	if !strings.Contains(got, "Pull") {
		t.Errorf("expected phase name 'Pull' in output, got %q", got)
	}
	if !strings.Contains(got, "Health") {
		t.Errorf("expected phase name 'Health' in output, got %q", got)
	}
}

func TestHostPhase_CompletedPhases(t *testing.T) {
	SetProfile(ProfileNone)
	defer SetProfile(ProfileNone)

	l, out, _ := newTestLogger()
	phases := []Phase{
		{Name: "Pull", Complete: true},
		{Name: "Container", Complete: true},
	}
	l.HostPhase("host2", phases)

	got := out.String()
	if !strings.Contains(got, SymSuccess) {
		t.Errorf("expected SymSuccess for completed phases, got %q", got)
	}
	if !strings.Contains(got, "host2") {
		t.Errorf("expected host name in output, got %q", got)
	}
}

func TestHostPhase_MixedPhases(t *testing.T) {
	SetProfile(ProfileNone)
	defer SetProfile(ProfileNone)

	l, out, _ := newTestLogger()
	phases := []Phase{
		{Name: "Pull", Complete: true},
		{Name: "Container", Complete: false},
	}
	l.HostPhase("host3", phases)

	got := out.String()
	if !strings.Contains(got, SymSuccess) {
		t.Errorf("expected SymSuccess for completed phase, got %q", got)
	}
	if !strings.Contains(got, SymPending) {
		t.Errorf("expected SymPending for pending phase, got %q", got)
	}
}

// --- StatusBadge tests ---

func TestStatusBadge_TrueColor(t *testing.T) {
	tests := []struct {
		status     string
		wantEscape string
		colorName  string
	}{
		{"running", "\033[38;2;168;230;207m", "Mint"},
		{"deploying", "\033[38;2;255;218;185m", "Peach"},
		{"promoting", "\033[38;2;137;207;240m", "SkyBlue"},
		{"rolling_back", "\033[38;2;255;107;107m", "Rose"},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			SetProfile(ProfileTrueColor)
			defer SetProfile(ProfileNone)

			l, out, _ := newTestLogger()
			l.StatusBadge("Status:", tt.status)

			got := out.String()
			if !strings.Contains(got, tt.wantEscape) {
				t.Errorf("expected %s TrueColor escape for %q, got %q", tt.colorName, tt.status, got)
			}
			if !strings.Contains(got, tt.status) {
				t.Errorf("expected %q in output, got %q", tt.status, got)
			}
		})
	}
}

func TestStatusBadge_ProfileNone(t *testing.T) {
	SetProfile(ProfileNone)
	defer SetProfile(ProfileNone)

	l, out, _ := newTestLogger()
	l.StatusBadge("Status:", "running")

	got := out.String()
	if strings.Contains(got, "\033[") {
		t.Errorf("expected no ANSI escapes in ProfileNone, got %q", got)
	}
	if !strings.Contains(got, "running") {
		t.Errorf("expected 'running' in output, got %q", got)
	}
	if !strings.Contains(got, "Status:") {
		t.Errorf("expected label 'Status:' in output, got %q", got)
	}
}
