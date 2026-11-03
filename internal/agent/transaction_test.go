package agent

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
)

const digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func imageOf(digest string) string { return testRepository + "@" + digest }

// deployedAOnce returns a server where digest A is deployed through the agent.
func deployedAOnce(t *testing.T, f *fakeServer) *Agent {
	t.Helper()
	f.tag = digestA
	a := f.agent(t)
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	return a
}

func loadState(t *testing.T, a *Agent) State {
	t.Helper()
	st, err := a.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestJournalIsSavedBeforeTheDeployAndClosedAfter(t *testing.T) {
	f := &fakeServer{}
	a := deployedAOnce(t, f)
	f.tag = digestB
	var heldLock bool
	deploy := a.Deploy
	a.Deploy = func(ctx context.Context, cfg *config.Config, image string, opts DeployOptions) error {
		heldLock = f.locked
		return deploy(ctx, cfg, image, opts)
	}
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if !heldLock {
		t.Fatal("an image release deployed without the deploy lock")
	}

	j := f.journals[1]
	if j == nil {
		t.Fatal("the deploy ran with no journal on disk")
	}
	if j.Digest != digestB || j.PreviousDigest != digestA || j.Phase != PhaseForward || j.First || j.StartedAt.IsZero() {
		t.Fatalf("journal = %+v", j)
	}
	if !strings.HasPrefix(j.Target, digestHex(digestB)+"-") || !strings.HasPrefix(j.Previous, digestHex(digestA)+"-") {
		t.Fatalf("journal records = %q, %q", j.Target, j.Previous)
	}
	st := loadState(t, a)
	if st.Journal != nil || st.DeployedDigest != digestB || st.Current != j.Target {
		t.Fatalf("state = %+v", st)
	}
	if len(st.Applied) != 2 || st.Applied[0] != j.Target || st.Applied[1] != j.Previous {
		t.Fatalf("applied = %v; want newest first", st.Applied)
	}
}

func TestAppliedIsBoundedByRetainHistory(t *testing.T) {
	f := &fakeServer{}
	a := deployedAOnce(t, f)
	a.Server.Deploy.RetainHistory = 2
	source := a.Source.(*ImageSource)
	source.Server.Deploy.RetainHistory = 2
	for _, digest := range []string{digestB, digestC, digestA} {
		f.tag = digest
		if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
			t.Fatalf("Reconcile %s = %s, %v", digest, outcome, err)
		}
	}
	st := loadState(t, a)
	want := []string{imageRecordID(digestA, source.Files), imageRecordID(digestC, source.Files)}
	if !slices.Equal(st.Applied, want) {
		t.Fatalf("applied = %v, want %v: newest first, once each, at most retain_history", st.Applied, want)
	}
}

func TestImageReleaseKeepsAVerifiedRecord(t *testing.T) {
	f := &fakeServer{}
	a := deployedAOnce(t, f)
	st := loadState(t, a)
	rec, err := a.records().Load(st.Current)
	if err != nil || rec == nil {
		t.Fatalf("Load = %+v, %v", rec, err)
	}
	if rec.Source != SourceImage || rec.Origin != OriginVerified || rec.Digest != digestA || rec.Image != imageOf(digestA) || rec.StoredAt.IsZero() {
		t.Fatalf("record = %+v", rec)
	}
	if len(rec.Files) != 1 || string(rec.Files[0].Data) != testServerYAML {
		t.Fatalf("files = %+v", rec.Files)
	}
	if rec.Sealed != nil || rec.Info != nil {
		t.Fatalf("an image release has no secrets or release.json: %+v", rec)
	}
}

func TestSkippingVerificationIsRecordedAsTheOrigin(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a := f.agent(t)
	a.Verify = nil
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	rec, err := a.records().Load(loadState(t, a).Current)
	if err != nil || rec.Origin != OriginVerifySkipped {
		t.Fatalf("record = %+v, %v", rec, err)
	}
}

func TestReleaseThatFailsVerificationLeavesNoRecord(t *testing.T) {
	f := &fakeServer{tag: digestA, verifyErr: errors.New("no matching signatures")}
	a := f.agent(t)
	if outcome, err := reconcile(t, a, false); err == nil || outcome != Failed {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	rec, err := a.records().Load(imageRecordID(digestA, a.Source.(*ImageSource).Files))
	if rec != nil || err != nil {
		t.Fatalf("record = %+v, %v; only what this server verified is stored", rec, err)
	}
	if st := loadState(t, a); st.Journal != nil || st.FailedDigest != digestA {
		t.Fatalf("state = %+v; nothing changed, so no journal stays open", st)
	}
}

func TestFailedImageReleaseRollsBackAsAWhole(t *testing.T) {
	f := &fakeServer{}
	a := deployedAOnce(t, f)
	f.tag = digestB
	f.deployErrs = map[string]error{digestB: errors.New("worker failed to start")}

	outcome, err := reconcile(t, a, false)
	if outcome != Failed || err == nil || !strings.Contains(err.Error(), "worker failed to start") {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	want := []string{imageOf(digestA), imageOf(digestB), imageOf(digestA)}
	if !slices.Equal(f.deployed, want) {
		t.Fatalf("deployed = %v, want %v", f.deployed, want)
	}
	if undo := f.options[2]; !undo.Undo || !undo.Local || !undo.Hooks {
		t.Fatalf("undo options = %+v; want the pre-deploy command skipped, no registry needed, and hooks on for an image", undo)
	}
	if forward := f.options[1]; forward.Undo || forward.Local {
		t.Fatalf("forward options = %+v", forward)
	}
	st := loadState(t, a)
	if st.Journal != nil || st.DeployedDigest != digestA || st.FailedDigest != digestB || !strings.Contains(st.LastError, "worker failed to start") {
		t.Fatalf("state = %+v, journal %+v", st, st.Journal)
	}
	if st.Current == "" || !strings.HasPrefix(st.Current, digestHex(digestA)) {
		t.Fatalf("current = %q; the deployed release stays current", st.Current)
	}
	if outcome, err := reconcile(t, a, false); outcome != SkippedFailed || err != nil {
		t.Fatalf("next check = %s, %v", outcome, err)
	}
}

func TestDeployNeverKeepsTheDeployersFleetRollback(t *testing.T) {
	f := &fakeServer{}
	a := deployedAOnce(t, f)
	a.Server.Deploy.RollbackOnFailure = true
	f.tag = digestB
	f.deployErrs = map[string]error{digestB: errors.New("boom")}
	deploy := a.Deploy
	var seen []bool
	a.Deploy = func(ctx context.Context, cfg *config.Config, image string, opts DeployOptions) error {
		seen = append(seen, cfg.Deploy.RollbackOnFailure)
		return deploy(ctx, cfg, image, opts)
	}
	if _, err := reconcile(t, a, false); err == nil {
		t.Fatal("Reconcile succeeded")
	}
	if len(seen) != 2 || seen[0] || seen[1] {
		t.Fatalf("fleet rollback = %v; it would restart earlier roles with the failed release's configuration", seen)
	}
}

func TestFailedUndoStaysOpenWithBackoffUntilItWorks(t *testing.T) {
	now := time.Unix(1000, 0)
	f := &fakeServer{}
	a := deployedAOnce(t, f)
	a.Now = func() time.Time { return now }
	f.tag = digestB
	f.deployErrs = map[string]error{digestA: errors.New("old image gone"), digestB: errors.New("new image broken")}

	outcome, err := reconcile(t, a, false)
	if outcome != Failed || err == nil || !strings.Contains(err.Error(), "new image broken") || !strings.Contains(err.Error(), "old image gone") {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	st := loadState(t, a)
	j := st.Journal
	if j == nil || j.Phase != PhaseBack || j.Tries != 1 || !j.NextTry.Equal(now.Add(30*time.Second)) {
		t.Fatalf("journal = %+v", j)
	}
	if st.FailedDigest != digestB || st.DeployedDigest != digestA {
		t.Fatalf("state = %+v", st)
	}

	deploys := len(f.deployed)
	if outcome, err := reconcile(t, a, false); outcome != Waiting || err != nil || len(f.deployed) != deploys {
		t.Fatalf("during backoff = %s, %v, deploys %d", outcome, err, len(f.deployed))
	}
	now = now.Add(30 * time.Second)
	if outcome, err := reconcile(t, a, false); outcome != Failed || err == nil || !strings.Contains(err.Error(), "undoing the failed deploy") {
		t.Fatalf("second undo = %s, %v", outcome, err)
	}
	if j := loadState(t, a).Journal; j == nil || j.Tries != 2 || !j.NextTry.Equal(now.Add(time.Minute)) {
		t.Fatalf("journal = %+v", j)
	}
	now = now.Add(time.Minute)
	delete(f.deployErrs, digestA)
	if outcome, err := reconcile(t, a, false); outcome != Recovered || err != nil {
		t.Fatalf("third undo = %s, %v", outcome, err)
	}
	if f.deployed[len(f.deployed)-1] != imageOf(digestA) {
		t.Fatalf("deployed = %v", f.deployed)
	}
	if st := loadState(t, a); st.Journal != nil || st.FailedDigest != digestB {
		t.Fatalf("state = %+v", st)
	}
}

func TestCrashedDeployResumesFromItsRecordWithoutVerifyingAgain(t *testing.T) {
	f := &fakeServer{tag: digestB}
	a := f.agent(t)
	seed(t, a, State{DeployedDigest: digestA, LastSeenDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseForward, First: false})
	a.Source = failingSource{a.Source}

	outcome, digest, err := a.Reconcile(context.Background(), false)
	if outcome != Deployed || err != nil || digest != digestB {
		t.Fatalf("Reconcile = %s, %s, %v", outcome, digest, err)
	}
	if len(f.verified) != 0 {
		t.Fatalf("verified %v; the record was verified before it was stored", f.verified)
	}
	if len(f.deployed) != 1 || f.deployed[0] != imageOf(digestB) || !f.options[0].Local || f.options[0].Undo {
		t.Fatalf("deployed %v with %+v", f.deployed, f.options)
	}
	if st := loadState(t, a); st.Journal != nil || st.DeployedDigest != digestB {
		t.Fatalf("state = %+v", st)
	}
}

// failingSource is a source whose registry is down: only what it does with no
// network still works.
type failingSource struct{ Source }

func (failingSource) Load(context.Context, string, *Record) (*Candidate, error) {
	return nil, errors.New("registry unreachable")
}

func TestResumeNeedsNoRegistry(t *testing.T) {
	f := &fakeServer{}
	a := f.agent(t)
	seed(t, a, State{DeployedDigest: digestA, LastSeenDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseForward})
	a.Source = failingSource{a.Source}

	if outcome, digest, err := a.Reconcile(context.Background(), false); outcome != Deployed || err != nil || digest != digestB {
		t.Fatalf("Reconcile = %s, %s, %v", outcome, digest, err)
	}
	st := loadState(t, a)
	if st.DeployedDigest != digestB || st.LastSeenDigest != digestB {
		t.Fatalf("state = %+v", st)
	}
}

func TestUndoNeedsNoRegistry(t *testing.T) {
	f := &fakeServer{}
	a := f.agent(t)
	seed(t, a, State{DeployedDigest: digestA, FailedDigest: digestB, LastSeenDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseBack})
	a.Source = failingSource{a.Source}

	if outcome, digest, err := a.Reconcile(context.Background(), false); outcome != Recovered || err != nil || digest != digestB {
		t.Fatalf("Reconcile = %s, %s, %v", outcome, digest, err)
	}
	if !slices.Equal(f.deployed, []string{imageOf(digestA)}) {
		t.Fatalf("deployed = %v", f.deployed)
	}
}

func TestOutageNeverAppliesANewRelease(t *testing.T) {
	f := &fakeServer{}
	a := f.agent(t)
	// The tag moved to C at the last check, which then stopped before acting.
	seed(t, a, State{DeployedDigest: digestA, LastSeenDigest: digestC}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseForward})

	outcome, _, err := a.Reconcile(context.Background(), false)
	if outcome != Failed || err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	st := loadState(t, a)
	if st.FailedDigest != "" || st.Journal == nil || len(f.deployed) != 0 {
		t.Fatalf("state = %+v, deployed %v; an outage says nothing about the release", st, f.deployed)
	}
}

func TestStopRequestDuringResolveLeavesAnOpenJournalAlone(t *testing.T) {
	f := &fakeServer{}
	a := f.agent(t)
	seed(t, a, State{DeployedDigest: digestA, FailedDigest: digestB, LastSeenDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseBack})
	ctx, cancel := context.WithCancel(context.Background())
	a.Resolve = func(context.Context) (string, error) {
		cancel()
		return "", context.Canceled
	}
	if outcome, _, err := a.Reconcile(ctx, false); outcome != Failed || err == nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if len(f.deployed) != 0 {
		t.Fatalf("deployed %v after a stop request", f.deployed)
	}
}

func TestResumeThatFailsUndoes(t *testing.T) {
	f := &fakeServer{tag: digestB, deployErrs: map[string]error{digestB: errors.New("still broken")}}
	a := f.agent(t)
	seed(t, a, State{DeployedDigest: digestA}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseForward})

	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Failed || err == nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if !slices.Equal(f.deployed, []string{imageOf(digestB), imageOf(digestA)}) {
		t.Fatalf("deployed = %v", f.deployed)
	}
	if st := loadState(t, a); st.Journal != nil || st.FailedDigest != digestB {
		t.Fatalf("state = %+v", st)
	}
}

func TestResumeWithoutItsRecordComesFromTheRegistryAgain(t *testing.T) {
	f := &fakeServer{tag: digestB}
	a := f.agent(t)
	if err := a.State.Save(State{Journal: &Journal{Target: "gone", Digest: digestB, Phase: PhaseForward}}); err != nil {
		t.Fatal(err)
	}
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Deployed || err != nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if len(f.verified) != 1 {
		t.Fatalf("verified %v; a record that is gone must be verified again", f.verified)
	}
}

func TestOutageWithItsRecordGoneDoesNotFailTheRelease(t *testing.T) {
	f := &fakeServer{}
	a := f.agent(t)
	if err := a.State.Save(State{LastSeenDigest: digestB, Journal: &Journal{Target: "gone", Digest: digestB, Phase: PhaseForward}}); err != nil {
		t.Fatal(err)
	}
	outcome, _, err := a.Reconcile(context.Background(), false)
	if outcome != Failed || err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if st := loadState(t, a); st.FailedDigest != "" || st.Journal == nil {
		t.Fatalf("state = %+v, journal %+v; an outage says nothing about the release", st, st.Journal)
	}
}

func TestBusyLockLeavesAResumeOpenAndUnfailed(t *testing.T) {
	f := &fakeServer{tag: digestB}
	a := f.agent(t)
	seed(t, a, State{DeployedDigest: digestA}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseForward})
	a.Lock = func(func() error) error { return errors.New("timed out waiting for the lock") }

	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Failed || err == nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	st := loadState(t, a)
	if st.FailedDigest != "" || st.Journal == nil || st.Journal.Phase != PhaseForward {
		t.Fatalf("state = %+v, journal %+v", st, st.Journal)
	}
}

func TestBusyLockFailsANewReleaseWithNoJournal(t *testing.T) {
	f := &fakeServer{tag: digestB}
	a := f.agent(t)
	a.Lock = func(func() error) error { return errors.New("timed out waiting for the lock") }
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Failed || err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	st := loadState(t, a)
	if st.FailedDigest != digestB || st.Journal != nil {
		t.Fatalf("state = %+v, journal %+v; nothing changed", st, st.Journal)
	}
}

func TestBusyLockBacksOffAnUndo(t *testing.T) {
	now := time.Unix(1000, 0)
	f := &fakeServer{tag: digestB}
	a := f.agent(t)
	a.Now = func() time.Time { return now }
	seed(t, a, State{DeployedDigest: digestA, FailedDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseBack})
	a.Lock = func(func() error) error { return errors.New("timed out waiting for the lock") }
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Failed || err == nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if j := loadState(t, a).Journal; j == nil || j.Tries != 1 || !j.NextTry.Equal(now.Add(30*time.Second)) {
		t.Fatalf("journal = %+v", j)
	}
}

func TestRetryGoesForwardFromAFailedUndo(t *testing.T) {
	f := &fakeServer{tag: digestB}
	a := f.agent(t)
	a.Now = func() time.Time { return time.Unix(1000, 0) }
	seed(t, a, State{DeployedDigest: digestA, FailedDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseBack, Tries: 2, NextTry: time.Unix(5000, 0)})

	if outcome, _, err := a.Reconcile(context.Background(), true); outcome != Deployed || err != nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if !slices.Equal(f.deployed, []string{imageOf(digestB)}) {
		t.Fatalf("deployed = %v; an operator asked for the failed release", f.deployed)
	}
	if st := loadState(t, a); st.Journal != nil || st.FailedDigest != "" || st.DeployedDigest != digestB {
		t.Fatalf("state = %+v", st)
	}
}

func TestNewTagEndsAMixedState(t *testing.T) {
	f := &fakeServer{tag: digestC}
	a := f.agent(t)
	a.Now = func() time.Time { return time.Unix(1000, 0) }
	seed(t, a, State{DeployedDigest: digestA, FailedDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseBack, Tries: 2, NextTry: time.Unix(5000, 0)})

	if outcome, digest, err := a.Reconcile(context.Background(), false); outcome != Deployed || err != nil || digest != digestC {
		t.Fatalf("Reconcile = %s, %s, %v", outcome, digest, err)
	}
	if len(f.verified) != 1 || !slices.Equal(f.deployed, []string{imageOf(digestC)}) {
		t.Fatalf("verified %v, deployed %v", f.verified, f.deployed)
	}
	if j := f.journals[0]; j == nil || j.Digest != digestC || j.PreviousDigest != digestA || j.Phase != PhaseForward || j.Tries != 0 {
		t.Fatalf("journal while deploying = %+v", j)
	}
	if st := loadState(t, a); st.Journal != nil || st.FailedDigest != "" || st.DeployedDigest != digestC {
		t.Fatalf("state = %+v", st)
	}
}

func TestNewReleaseThatFailsVerificationLeavesTheJournalToBeUndone(t *testing.T) {
	f := &fakeServer{tag: digestC, verifyErr: errors.New("unsigned")}
	a := f.agent(t)
	seed(t, a, State{DeployedDigest: digestA}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseForward})

	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Failed || err == nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	st := loadState(t, a)
	if st.FailedDigest != digestC || st.Journal == nil || st.Journal.Digest != digestB {
		t.Fatalf("state = %+v, journal %+v", st, st.Journal)
	}
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Recovered || err != nil {
		t.Fatalf("next check = %s, %v; the failed digest must not strand the journal", outcome, err)
	}
	if !slices.Equal(f.deployed, []string{imageOf(digestA)}) {
		t.Fatalf("deployed = %v", f.deployed)
	}
}

const twoRoleYAML = `service: app
image: ` + testRepository + `
servers:
  web:
    hosts: [app-1]
  worker:
    hosts: [app-1]
    cmd: bin/worker
proxy:
  host: app.example.com
agent:
  track: production
  verify_command: cosign verify "$$AZUD_AGENT_IMAGE"
`

// recordWith stores a record of digest with its own configuration.
func recordWith(t *testing.T, a *Agent, digest, yaml string) *Record {
	t.Helper()
	source := *testImageSource(t, yaml)
	rec, err := source.Previous(nil, digest)
	if err != nil {
		t.Fatal(err)
	}
	rec.Origin = OriginVerified
	if err := a.records().Store(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestUndoStopsRolesOnlyTheFailedReleaseAdded(t *testing.T) {
	f := &fakeServer{runningRoles: []string{"web", "worker"}, tag: digestB}
	a := f.agent(t)
	target := recordWith(t, a, digestB, twoRoleYAML)
	previous := recordWith(t, a, digestA, testServerYAML)
	if err := a.State.Save(State{
		DeployedDigest: digestA,
		FailedDigest:   digestB,
		Current:        previous.ID,
		Journal:        &Journal{Target: target.ID, Digest: digestB, Previous: previous.ID, PreviousDigest: digestA, Phase: PhaseBack},
	}); err != nil {
		t.Fatal(err)
	}
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Recovered || err != nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if !slices.Equal(f.stopped, []string{"worker"}) {
		t.Fatalf("stopped = %v", f.stopped)
	}
}

func TestReplacingAJournalStopsRolesOnlyTheAbandonedReleaseAdded(t *testing.T) {
	f := &fakeServer{runningRoles: []string{"web", "worker"}, tag: digestC}
	a := f.agent(t)
	target := recordWith(t, a, digestB, twoRoleYAML)
	previous := recordWith(t, a, digestA, testServerYAML)
	if err := a.State.Save(State{
		DeployedDigest: digestA,
		Current:        previous.ID,
		Journal:        &Journal{Target: target.ID, Digest: digestB, Previous: previous.ID, PreviousDigest: digestA, Phase: PhaseForward},
	}); err != nil {
		t.Fatal(err)
	}
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Deployed || err != nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if !slices.Equal(f.stopped, []string{"worker"}) {
		t.Fatalf("stopped = %v; a role only the abandoned release has must not keep running it", f.stopped)
	}
}

func TestImageReleaseWithNoRecordOfThePreviousAdoptsIt(t *testing.T) {
	// deployed_digest from v1 state, or a pushed deploy: nothing stored.
	f := &fakeServer{tag: digestB, deployErrs: map[string]error{digestB: errors.New("broken")}}
	a := f.agent(t)
	if err := a.State.Save(State{Ref: "r:t", DeployedDigest: digestA}); err != nil {
		t.Fatal(err)
	}
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Failed || err == nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if !slices.Equal(f.deployed, []string{imageOf(digestB), imageOf(digestA)}) {
		t.Fatalf("deployed = %v; the adopted release is what the failure returns to", f.deployed)
	}
	st := loadState(t, a)
	rec, err := a.records().Load(st.Current)
	if err != nil || rec == nil {
		t.Fatalf("current %q = %+v, %v", st.Current, rec, err)
	}
	if rec.Origin != OriginAdopted || rec.Digest != digestA || rec.Source != SourceImage || string(rec.Files[0].Data) != testServerYAML {
		t.Fatalf("record = %+v", rec)
	}
	if st.DeployedDigest != digestA || st.Journal != nil || st.FailedDigest != digestB {
		t.Fatalf("state = %+v", st)
	}
}

func TestAdoptedRunningDigestGetsARecord(t *testing.T) {
	f := &fakeServer{tag: digestA, running: digestA}
	a := f.agent(t)
	if outcome, err := reconcile(t, a, false); outcome != Adopted || err != nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	st := loadState(t, a)
	rec, err := a.records().Load(st.Current)
	if err != nil || rec == nil || rec.Origin != OriginAdopted || rec.Digest != digestA {
		t.Fatalf("record = %+v, %v", rec, err)
	}
	if len(st.Applied) != 1 || st.Applied[0] != st.Current {
		t.Fatalf("applied = %v", st.Applied)
	}
}

func TestOpenJournalIsNotAdoptedAway(t *testing.T) {
	f := &fakeServer{tag: digestB, running: digestB}
	a := f.agent(t)
	seed(t, a, State{DeployedDigest: digestA, FailedDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseBack})
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Recovered || err != nil {
		t.Fatalf("Reconcile = %s, %v; the running digest is the failed release", outcome, err)
	}
	if st := loadState(t, a); st.DeployedDigest != digestA {
		t.Fatalf("state = %+v", st)
	}
}

func TestFirstImageDeployThatFailsStopsWhatItStarted(t *testing.T) {
	f := &fakeServer{tag: digestA, deployErr: errors.New("worker failed")}
	a := f.agent(t)
	deploy := a.Deploy
	a.Deploy = func(ctx context.Context, cfg *config.Config, image string, opts DeployOptions) error {
		f.runningRoles = []string{"web"} // started before the failure
		return deploy(ctx, cfg, image, opts)
	}
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Failed || err == nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if !slices.Equal(f.stopped, []string{"web"}) || len(f.deployed) != 1 {
		t.Fatalf("stopped = %v, deployed = %v", f.stopped, f.deployed)
	}
	if st := loadState(t, a); st.Journal != nil || st.FailedDigest != digestA || st.DeployedDigest != "" {
		t.Fatalf("state = %+v", st)
	}
}

func TestFirstImageDeployThatFailsLeavesAServiceThatRanBefore(t *testing.T) {
	f := &fakeServer{tag: digestA, deployErr: errors.New("health check failed"), runningRoles: []string{"web"}}
	a := f.agent(t)
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Failed || err == nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if len(f.stopped) != 0 {
		t.Fatalf("stopped = %v; that could take a running service down", f.stopped)
	}
	if st := loadState(t, a); st.Journal != nil {
		t.Fatalf("journal = %+v; nothing more can be undone", st.Journal)
	}
}

func TestFailedCleanupBacksOffAndRetries(t *testing.T) {
	now := time.Unix(1000, 0)
	f := &fakeServer{tag: digestA, deployErr: errors.New("worker failed")}
	a := f.agent(t)
	a.Now = func() time.Time { return now }
	deploy := a.Deploy
	a.Deploy = func(ctx context.Context, cfg *config.Config, image string, opts DeployOptions) error {
		f.runningRoles = []string{"web"}
		return deploy(ctx, cfg, image, opts)
	}
	stopErr := errors.New("podman is down")
	a.StopRoles = func(_ *config.Config, roles []string) error {
		if stopErr != nil {
			return stopErr
		}
		f.stopped = append(f.stopped, roles...)
		return nil
	}
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Failed || err == nil || !strings.Contains(err.Error(), "podman is down") {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	j := loadState(t, a).Journal
	if j == nil || !j.First || j.Phase != PhaseBack || j.Tries != 1 {
		t.Fatalf("journal = %+v", j)
	}
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Waiting || err != nil {
		t.Fatalf("during backoff = %s, %v", outcome, err)
	}
	now = now.Add(30 * time.Second)
	stopErr = nil
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Recovered || err != nil {
		t.Fatalf("after backoff = %s, %v", outcome, err)
	}
	if !slices.Equal(f.stopped, []string{"web"}) || len(f.deployed) != 1 {
		t.Fatalf("stopped = %v, deployed = %v", f.stopped, f.deployed)
	}
}

func TestDeployCommandFailureClosesTheJournalWithoutAnUndo(t *testing.T) {
	f := &fakeServer{}
	a := deployedAOnce(t, f)
	a.DeployUndoes = true
	f.tag = digestB
	f.deployErrs = map[string]error{digestB: errors.New("contract preflight failed")}

	outcome, err := reconcile(t, a, false)
	if outcome != Failed || err == nil || !strings.Contains(err.Error(), "contract preflight failed") {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if !slices.Equal(f.deployed, []string{imageOf(digestA), imageOf(digestB)}) {
		t.Fatalf("deployed = %v; a deploy_command owns its undo", f.deployed)
	}
	st := loadState(t, a)
	if st.Journal != nil || st.FailedDigest != digestB || st.DeployedDigest != digestA || !strings.Contains(st.LastError, "contract preflight failed") {
		t.Fatalf("state = %+v, journal %+v", st, st.Journal)
	}
	if outcome, err := reconcile(t, a, false); outcome != SkippedFailed || err != nil {
		t.Fatalf("next check = %s, %v", outcome, err)
	}
}

func TestDeployCommandNeverAsksWhatRanBefore(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a := f.agent(t)
	a.DeployUndoes = true
	a.RunningRoles = func(*config.Config) ([]string, error) {
		return nil, errors.New("this service's containers may not be Azud's")
	}
	if outcome, err := reconcile(t, a, false); outcome != Deployed || err != nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
}

func TestDeployCommandAgentThatDiedResumes(t *testing.T) {
	f := &fakeServer{tag: digestB}
	a := f.agent(t)
	a.DeployUndoes = true
	seed(t, a, State{DeployedDigest: digestA}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseForward})
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Deployed || err != nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if !slices.Equal(f.deployed, []string{imageOf(digestB)}) {
		t.Fatalf("deployed = %v", f.deployed)
	}
}

func TestDeployCommandInterruptedRetryIsClosedNotUndone(t *testing.T) {
	f := &fakeServer{tag: digestB}
	a := f.agent(t)
	a.DeployUndoes = true
	seed(t, a, State{DeployedDigest: digestA, FailedDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseForward})
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != SkippedFailed || err != nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if len(f.deployed) != 0 {
		t.Fatalf("deployed = %v", f.deployed)
	}
	if st := loadState(t, a); st.Journal != nil {
		t.Fatalf("journal = %+v", st.Journal)
	}
}

func TestUndoWithItsPreviousRecordGoneCleansUp(t *testing.T) {
	f := &fakeServer{tag: digestB}
	a := f.agent(t)
	seed(t, a, State{DeployedDigest: digestA, FailedDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseBack})
	st := loadState(t, a)
	if err := os.RemoveAll(a.records().Dir(st.Journal.Previous)); err != nil {
		t.Fatal(err)
	}
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Recovered || err != nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if len(f.deployed) != 0 {
		t.Fatalf("deployed = %v", f.deployed)
	}
	if st := loadState(t, a); st.Journal != nil {
		t.Fatalf("journal = %+v; a missing record must not strand the server", st.Journal)
	}
}

func TestUndoReturningToABrokenRecordKeepsTrying(t *testing.T) {
	f := &fakeServer{tag: digestB}
	a := f.agent(t)
	seed(t, a, State{DeployedDigest: digestA, FailedDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseBack})
	st := loadState(t, a)
	if err := os.WriteFile(a.records().Dir(st.Journal.Previous)+"/record.json", []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if outcome, _, err := a.Reconcile(context.Background(), false); outcome != Failed || err == nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if j := loadState(t, a).Journal; j == nil || j.Tries != 1 {
		t.Fatalf("journal = %+v", j)
	}
}

func TestEditedConfigurationIsANewRecordOfTheSameDigest(t *testing.T) {
	f := &fakeServer{tag: digestA, deployErr: errors.New("config was wrong")}
	a := f.agent(t)
	if outcome, err := reconcile(t, a, false); outcome != Failed || err == nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	first := imageRecordID(digestA, a.Source.(*ImageSource).Files)

	// The operator edits the file and restarts the agent.
	f.deployErr = nil
	edited := strings.Replace(testServerYAML, "app.example.com", "www.example.com", 1)
	a.Source = testImageSource(t, edited)
	if outcome, err := reconcile(t, a, true); outcome != Deployed || err != nil {
		t.Fatalf("Reconcile with retry = %s, %v", outcome, err)
	}
	second := loadState(t, a).Current
	if second == first || recordDigest(second) != digestHex(digestA) {
		t.Fatalf("records = %q, %q", first, second)
	}
	for id, want := range map[string]string{first: testServerYAML, second: edited} {
		rec, err := a.records().Load(id)
		if err != nil || rec == nil || string(rec.Files[0].Data) != want {
			t.Fatalf("record %s = %+v, %v", id, rec, err)
		}
	}
}
