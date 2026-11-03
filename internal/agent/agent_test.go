package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/release"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

const testRepository = "ghcr.io/lemonity-org/app"

const testServerYAML = `service: app
image: ` + testRepository + `
servers:
  web:
    hosts: [app-1]
proxy:
  host: app.example.com
agent:
  track: production
  verify_command: cosign verify "$$AZUD_AGENT_IMAGE"
`

// testServer loads a server's configuration from yaml, as the CLI does.
func testServer(t *testing.T, yaml string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploy.yml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := config.NewLoader(path, "").WithoutSecrets().Load()
	if err != nil {
		t.Fatalf("server config: %v", err)
	}
	return server
}

func testImageSource(t *testing.T, yaml string) *ImageSource {
	t.Helper()
	return &ImageSource{
		Server:     testServer(t, yaml),
		Files:      []release.File{{Name: "deploy.yml", Data: []byte(yaml)}},
		Repository: testRepository,
	}
}

// fakeServer stands in for the registry, Podman, and the deployer.
type fakeServer struct {
	mu        sync.Mutex
	tag       string
	running   string
	verifyErr error
	deployErr error
	// deployErrs fails the deploy of one digest, whatever else deploys.
	deployErrs   map[string]error
	verified     []string
	deployed     []string
	options      []DeployOptions
	runningRoles []string
	stopped      []string
	locked       bool
	// journals holds the journal saved when each deploy started.
	journals []*Journal
	store    *Store
}

func (f *fakeServer) agent(t *testing.T) *Agent {
	t.Helper()
	source := testImageSource(t, testServerYAML)
	f.store = NewStore(t.TempDir(), "app")
	return &Agent{
		Ref:        testRepository + ":production",
		Repository: testRepository,
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
		Source: source,
		Server: source.Server,
		Deploy: func(ctx context.Context, _ *config.Config, image string, opts DeployOptions) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.deployed = append(f.deployed, image)
			f.options = append(f.options, opts)
			st, err := f.store.Load()
			if err != nil {
				return err
			}
			f.journals = append(f.journals, st.Journal)
			digest := strings.TrimPrefix(image, testRepository+"@")
			if err := f.deployErrs[digest]; err != nil {
				return err
			}
			if f.deployErr == nil {
				f.running = digest
			}
			return f.deployErr
		},
		Lock: func(fn func() error) error {
			f.locked = true
			defer func() { f.locked = false }()
			return fn()
		},
		StopRoles: func(_ *config.Config, roles []string) error {
			f.stopped = append(f.stopped, roles...)
			return nil
		},
		RunningRoles: func(*config.Config) ([]string, error) {
			return append([]string(nil), f.runningRoles...), nil
		},
		State: f.store,
		Log:   output.NewLogger(&bytes.Buffer{}, &bytes.Buffer{}, false),
	}
}

// seed saves st, with a journal to digest if one is given, and stores a record for
// each digest named so the agent can return to it.
func seed(t *testing.T, a *Agent, st State, j *Journal) {
	t.Helper()
	store := func(digest string) string {
		t.Helper()
		rec, err := a.Source.Previous(nil, digest)
		if err != nil {
			t.Fatal(err)
		}
		rec.Origin = OriginVerified
		if err := a.records().Store(rec); err != nil {
			t.Fatal(err)
		}
		return rec.ID
	}
	if st.DeployedDigest != "" {
		st.Current = store(st.DeployedDigest)
	}
	if j != nil {
		j.Target = store(j.Digest)
		if j.PreviousDigest != "" {
			j.Previous = store(j.PreviousDigest)
		}
		st.Journal = j
	}
	if err := a.State.Save(st); err != nil {
		t.Fatal(err)
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
	a.Deploy = func(deployCtx context.Context, _ *config.Config, _ string, _ DeployOptions) error {
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
	a.Deploy = func(context.Context, *config.Config, string, DeployOptions) error {
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
	if st, err := store.Load(); err != nil || !reflect.DeepEqual(st, State{}) {
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
	if err != nil || !reflect.DeepEqual(got, want) {
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
	digest := "sha256:" + strings.Repeat("a", 64)
	f := &fakeServer{tag: digest}
	a := f.agent(t)
	seed(t, a, State{DeployedDigest: digest}, &Journal{Digest: digest, PreviousDigest: digest, Phase: PhaseForward})

	outcome, _, err := a.Reconcile(context.Background(), false)
	if err != nil || outcome != Deployed || len(f.deployed) != 1 {
		t.Fatalf("Reconcile = %v, %v, deploys %d; an interrupted deploy must be redone", outcome, err, len(f.deployed))
	}
	if outcome, _, _ := a.Reconcile(context.Background(), false); outcome != Unchanged || len(f.deployed) != 1 {
		t.Fatalf("Reconcile = %v, deploys %d; nothing to redo once it finished", outcome, len(f.deployed))
	}
}

func TestFailedReleaseLeftInterruptedIsUndoneNotRedeployed(t *testing.T) {
	previous := "sha256:" + strings.Repeat("a", 64)
	digest := "sha256:" + strings.Repeat("b", 64)
	now := time.Unix(1000, 0)
	f := &fakeServer{tag: digest, deployErrs: map[string]error{previous: errors.New("previous release failed too")}}
	a := f.agent(t)
	a.Now = func() time.Time { return now }
	seed(t, a, State{DeployedDigest: previous, FailedDigest: digest}, &Journal{Digest: digest, PreviousDigest: previous, Phase: PhaseForward})

	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Failed || err == nil || len(f.deployed) != 1 {
		t.Fatalf("Reconcile = %v, %v, undos %d", outcome, err, len(f.deployed))
	}
	st, err := a.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Journal == nil || st.Journal.Tries != 1 || !st.Journal.NextTry.Equal(now.Add(30*time.Second)) || !strings.Contains(st.LastError, "previous release failed too") {
		t.Fatalf("state = %+v, journal %+v", st, st.Journal)
	}

	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Waiting || err != nil || len(f.deployed) != 1 {
		t.Fatalf("Reconcile during backoff = %v, %v, undos %d", outcome, err, len(f.deployed))
	}

	now = now.Add(31 * time.Second)
	f.deployErrs = nil
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Recovered || err != nil || len(f.deployed) != 2 {
		t.Fatalf("Reconcile after backoff = %v, %v, undos %d", outcome, err, len(f.deployed))
	}
	if st, _ := a.State.Load(); st.Journal != nil || st.FailedDigest != digest {
		t.Fatalf("state after recovery = %+v; the release stays failed and the journal closes", st)
	}
	if outcome, _, _ := a.Reconcile(context.Background(), false); outcome != SkippedFailed {
		t.Fatalf("Reconcile after recovery = %v", outcome)
	}
	for _, image := range f.deployed {
		if strings.HasSuffix(image, digest) {
			t.Fatalf("deployed %v; a failed release must not be deployed again without a retry", f.deployed)
		}
	}
}

func TestRetryFailedStillDeploysAnInterruptedRelease(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	f := &fakeServer{tag: digest}
	a := f.agent(t)
	a.Now = func() time.Time { return time.Unix(1000, 0) }
	seed(t, a, State{FailedDigest: digest}, &Journal{Digest: digest, Phase: PhaseForward, Tries: 3, NextTry: time.Unix(5000, 0)})

	if outcome, _, err := a.Reconcile(context.Background(), true); outcome != Deployed || err != nil || len(f.deployed) != 1 {
		t.Fatalf("Reconcile = %v, %v, deploys %d", outcome, err, len(f.deployed))
	}
	if st, _ := a.State.Load(); st.Journal != nil || st.FailedDigest != "" {
		t.Fatalf("state = %+v; a deployed release ends the backoff", st)
	}
}

func TestRecoverDelayDoublesToAQuarterHour(t *testing.T) {
	for tries, want := range map[int]time.Duration{
		0:  30 * time.Second,
		1:  time.Minute,
		4:  8 * time.Minute,
		5:  15 * time.Minute,
		40: 15 * time.Minute,
	} {
		if got := recoverDelay(tries); got != want {
			t.Errorf("recoverDelay(%d) = %s, want %s", tries, got, want)
		}
	}
}
