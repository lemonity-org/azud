package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/output"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeServer stands in for the registry, Podman, and the deployer.
type fakeServer struct {
	mu        sync.Mutex
	tag       string
	running   string
	verifyErr error
	deployErr error
	verified  []string
	deployed  []string
}

func (f *fakeServer) agent(t *testing.T) *Agent {
	t.Helper()
	return &Agent{
		Ref:        "ghcr.io/lemonity-org/app:production",
		Repository: "ghcr.io/lemonity-org/app",
		Resolve: func(context.Context) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.tag == "" {
				return "", errors.New("registry unreachable")
			}
			return f.tag, nil
		},
		Running: func(context.Context) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.running, nil
		},
		Verify: func(_ context.Context, image string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.verified = append(f.verified, image)
			return f.verifyErr
		},
		Deploy: func(ctx context.Context, image string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.deployed = append(f.deployed, image)
			if f.deployErr == nil {
				f.running = strings.TrimPrefix(image, "ghcr.io/lemonity-org/app@")
			}
			return f.deployErr
		},
		State: NewStore(t.TempDir(), "app"),
		Log:   output.NewLogger(&bytes.Buffer{}, &bytes.Buffer{}, false),
	}
}

func reconcile(t *testing.T, a *Agent, retry bool) (Outcome, error) {
	t.Helper()
	outcome, _, err := a.Reconcile(context.Background(), retry)
	return outcome, err
}

func TestReconcileDeploysNewDigestAfterVerifying(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a := f.agent(t)

	outcome, err := reconcile(t, a, false)
	if err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v; want deployed", outcome, err)
	}
	want := "ghcr.io/lemonity-org/app@" + digestA
	if len(f.verified) != 1 || f.verified[0] != want || len(f.deployed) != 1 || f.deployed[0] != want {
		t.Fatalf("verified=%v deployed=%v; want %s verified then deployed", f.verified, f.deployed, want)
	}

	st, _ := a.State.Load()
	if st.DeployedDigest != digestA || st.LastSeenDigest != digestA || st.Ref != a.Ref {
		t.Fatalf("state = %+v", st)
	}

	// The next check finds nothing to do.
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Unchanged {
		t.Fatalf("second Reconcile = %s, %v; want unchanged", outcome, err)
	}
	if len(f.deployed) != 1 {
		t.Fatalf("deployed %d times, want once", len(f.deployed))
	}
}

func TestReconcileNeverDeploysAnImageThatFailsVerification(t *testing.T) {
	f := &fakeServer{tag: digestA, verifyErr: errors.New("no matching signatures")}
	a := f.agent(t)

	outcome, err := reconcile(t, a, false)
	if outcome != Failed || err == nil || !strings.Contains(err.Error(), "no matching signatures") {
		t.Fatalf("Reconcile = %s, %v; want a verification failure", outcome, err)
	}
	if len(f.deployed) != 0 {
		t.Fatalf("deployed %v after failed verification", f.deployed)
	}
	st, _ := a.State.Load()
	if st.FailedDigest != digestA || !strings.Contains(st.LastError, "verification") {
		t.Fatalf("state = %+v; want the failure recorded", st)
	}
}

func TestReconcileSkipsFailedDigestUntilTagMoves(t *testing.T) {
	f := &fakeServer{tag: digestA, deployErr: errors.New("health check failed")}
	a := f.agent(t)

	if outcome, _ := reconcile(t, a, false); outcome != Failed {
		t.Fatalf("first Reconcile = %s; want failed", outcome)
	}
	if outcome, err := reconcile(t, a, false); outcome != SkippedFailed || err != nil {
		t.Fatalf("second Reconcile = %s, %v; want the failed digest skipped", outcome, err)
	}
	if len(f.deployed) != 1 {
		t.Fatalf("deployed %d times; a failed digest must not be retried every tick", len(f.deployed))
	}

	// A new release is deployed and clears the failure.
	f.deployErr = nil
	f.tag = digestB
	if outcome, err := reconcile(t, a, false); outcome != Deployed || err != nil {
		t.Fatalf("Reconcile after the tag moved = %s, %v; want deployed", outcome, err)
	}
	st, _ := a.State.Load()
	if st.DeployedDigest != digestB || st.FailedDigest != "" || st.LastError != "" {
		t.Fatalf("state = %+v; want digest B deployed and the failure cleared", st)
	}
}

func TestReconcileRetriesFailedDigestWhenAsked(t *testing.T) {
	f := &fakeServer{tag: digestA, deployErr: errors.New("registry timeout")}
	a := f.agent(t)
	_, _ = reconcile(t, a, false)

	f.deployErr = nil
	if outcome, err := reconcile(t, a, true); outcome != Deployed || err != nil {
		t.Fatalf("Reconcile with retry = %s, %v; want deployed", outcome, err)
	}
}

func TestReconcileAdoptsDigestThatIsAlreadyRunning(t *testing.T) {
	f := &fakeServer{tag: digestA, running: digestA}
	a := f.agent(t)

	if outcome, err := reconcile(t, a, false); outcome != Adopted || err != nil {
		t.Fatalf("Reconcile = %s, %v; want adopted", outcome, err)
	}
	if len(f.deployed) != 0 || len(f.verified) != 0 {
		t.Fatalf("verified=%v deployed=%v; an already running digest needs neither", f.verified, f.deployed)
	}
	st, _ := a.State.Load()
	if st.DeployedDigest != digestA {
		t.Fatalf("state = %+v", st)
	}
}

func TestReconcileAdoptsFailedDigestFixedByPushedDeploy(t *testing.T) {
	f := &fakeServer{tag: digestA, deployErr: errors.New("health check failed")}
	a := f.agent(t)
	_, _ = reconcile(t, a, false)

	// An operator fixes the server and deploys the same release by hand.
	f.running = digestA
	if outcome, err := reconcile(t, a, false); outcome != Adopted || err != nil {
		t.Fatalf("Reconcile = %s, %v; want the running release adopted", outcome, err)
	}
	st, _ := a.State.Load()
	if st.DeployedDigest != digestA || st.FailedDigest != "" {
		t.Fatalf("state = %+v; want the failure cleared", st)
	}
}

func TestReconcileDoesNotFailReleaseWhenVerificationIsInterrupted(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a := f.agent(t)
	ctx, cancel := context.WithCancel(context.Background())
	a.Verify = func(verifyCtx context.Context, image string) error {
		cancel()
		return verifyCtx.Err()
	}

	outcome, _, err := a.Reconcile(ctx, false)
	if outcome != Failed || err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("Reconcile = %s, %v; want an interrupted verification", outcome, err)
	}
	if len(f.deployed) != 0 {
		t.Fatalf("deployed %v without a verification verdict", f.deployed)
	}
	st, _ := a.State.Load()
	if st.FailedDigest != "" {
		t.Fatalf("state = %+v; an interrupted check must not mark the release failed", st)
	}
}

func TestReconcileLeavesStateAloneWhenTagCannotBeResolved(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a := f.agent(t)
	_, _ = reconcile(t, a, false)

	f.tag = ""
	outcome, err := reconcile(t, a, false)
	if outcome != Failed || err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("Reconcile = %s, %v; want a resolve failure", outcome, err)
	}
	st, _ := a.State.Load()
	if st.DeployedDigest != digestA || st.FailedDigest != "" {
		t.Fatalf("state = %+v; a registry outage must not mark the release failed", st)
	}
}

func TestReconcileFinishesDeployWhenCanceled(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a := f.agent(t)
	ctx, cancel := context.WithCancel(context.Background())
	a.Deploy = func(deployCtx context.Context, image string) error {
		cancel()
		if deployCtx.Err() != nil {
			return errors.New("deploy saw the cancellation")
		}
		return nil
	}

	if outcome, _, err := a.Reconcile(ctx, false); outcome != Deployed || err != nil {
		t.Fatalf("Reconcile = %s, %v; a started deploy must run to completion", outcome, err)
	}
}

func TestRunChecksUntilCanceled(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a := f.agent(t)
	ctx, cancel := context.WithCancel(context.Background())
	a.Deploy = func(context.Context, string) error {
		cancel()
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, time.Hour) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestStoreSavesPrivately(t *testing.T) {
	store := NewStore(t.TempDir(), "app")
	if st, err := store.Load(); err != nil || st != (State{}) {
		t.Fatalf("Load of missing state = %+v, %v", st, err)
	}
	want := State{Ref: "r:t", DeployedDigest: digestA, DeployedAt: time.Unix(1700000000, 0).UTC()}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(store.Path())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file = %v, %v; want mode 600", info, err)
	}
	got, err := store.Load()
	if err != nil || got != want {
		t.Fatalf("Load = %+v, %v; want %+v", got, err, want)
	}
}

func TestCommandVerifierPassesImageThroughEnvironment(t *testing.T) {
	v := &CommandVerifier{Command: `test "$AZUD_AGENT_IMAGE" = "ghcr.io/o/a@` + digestA + `" && test "$AZUD_AGENT_DIGEST" = "` + digestA + `" && test "$AZUD_SERVICE" = app`, Env: []string{"AZUD_SERVICE=app"}}
	if err := v.Verify(context.Background(), "ghcr.io/o/a@"+digestA); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestCommandVerifierReportsOutputOnFailure(t *testing.T) {
	v := &CommandVerifier{Command: `echo "Error: no matching signatures" >&2; exit 1`}
	err := v.Verify(context.Background(), "ghcr.io/o/a@"+digestA)
	if err == nil || !strings.Contains(err.Error(), "no matching signatures") {
		t.Fatalf("Verify = %v; want the command output in the error", err)
	}
}

func TestCommandVerifierTimesOut(t *testing.T) {
	v := &CommandVerifier{Command: "sleep 30", Timeout: 100 * time.Millisecond}
	start := time.Now()
	err := v.Verify(context.Background(), "ghcr.io/o/a@"+digestA)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Verify = %v; want a timeout", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("verify timeout was not enforced")
	}
}

func TestInterruptedDeployIsRedoneEvenIfTheTagDidNotMove(t *testing.T) {
	store := NewStore(t.TempDir(), "app")
	digest := "sha256:" + strings.Repeat("a", 64)
	if err := store.Save(State{DeployedDigest: digest}); err != nil {
		t.Fatal(err)
	}
	deployed := 0
	interrupted := true
	a := &Agent{
		Ref:         "ghcr.io/acme/app:production",
		Repository:  "ghcr.io/acme/app",
		Resolve:     func(context.Context) (string, error) { return digest, nil },
		Deploy:      func(context.Context, string) error { deployed++; return nil },
		Interrupted: func() bool { return interrupted },
		State:       store,
	}

	outcome, _, err := a.Reconcile(context.Background(), false)
	if err != nil || outcome != Deployed || deployed != 1 {
		t.Fatalf("Reconcile = %v, %v, deploys %d; an interrupted deploy must be redone", outcome, err, deployed)
	}
	interrupted = false
	if outcome, _, _ := a.Reconcile(context.Background(), false); outcome != Unchanged || deployed != 1 {
		t.Fatalf("Reconcile = %v, deploys %d; nothing to redo once it finished", outcome, deployed)
	}
}
