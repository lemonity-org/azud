package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/release"
)

const digestD = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

// deployedChain returns a server that applied digests in order through the agent.
func deployedChain(t *testing.T, f *fakeServer, digests ...string) *Agent {
	t.Helper()
	a := f.agent(t)
	for _, digest := range digests {
		f.tag = digest
		if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
			t.Fatalf("Reconcile %s = %s, %v", digest, outcome, err)
		}
	}
	f.verified, f.deployed, f.options, f.journals = nil, nil, nil, nil
	return a
}

func rollback(t *testing.T, a *Agent) (Outcome, string, error) {
	t.Helper()
	return a.Rollback(context.Background(), MoveOptions{})
}

func TestPrevious(t *testing.T) {
	id := func(digest string) string { return digestHex(digest) + "-00000000" }
	cases := []struct {
		name string
		st   State
		want string
	}{
		{"nothing applied", State{}, ""},
		{"only the current release", State{Current: id(digestA), Applied: []string{id(digestA)}}, ""},
		{"the one before", State{Current: id(digestB), Applied: []string{id(digestB), id(digestA)}}, id(digestA)},
		{"skips what the tag is held at", State{Current: id(digestC), FailedDigest: digestB, Applied: []string{id(digestC), id(digestB), id(digestA)}}, id(digestA)},
		{"nothing but what is held", State{Current: id(digestC), FailedDigest: digestB, Applied: []string{id(digestC), id(digestB)}}, ""},
		{"an earlier configuration of the same image", State{Current: digestHex(digestA) + "-11111111", Applied: []string{digestHex(digestA) + "-11111111", id(digestA)}}, id(digestA)},
		{"not an earlier configuration of what is held", State{Current: id(digestB), FailedDigest: digestC, Applied: []string{id(digestB), digestHex(digestC) + "-11111111", id(digestC), id(digestA)}}, id(digestA)},
	}
	for _, c := range cases {
		got, ok := Previous(c.st)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: Previous = %q, %v; want %q", c.name, got, ok, c.want)
		}
	}
}

func TestDecideOnAnOperatorsMove(t *testing.T) {
	now := time.Unix(1000, 0)
	// A rollback to old, from what runs, while the tag names bad.
	move := func() *Journal {
		return &Journal{Target: "old-id", Digest: "old", Previous: "bad-id", PreviousDigest: "bad", Phase: PhaseForward, Move: true, Hold: "bad"}
	}
	cases := []struct {
		name string
		in   Input
		want ActionKind
	}{
		{"a crash finishes the move and does not return to what it left", Input{Tag: "bad", DeployedDigest: "bad", Journal: move(), Now: now}, ActionResume},
		{"a crash after the hold was recorded still finishes", Input{Tag: "bad", DeployedDigest: "bad", FailedDigest: "bad", Journal: move(), Now: now}, ActionResume},
		{"the tag moved on: that release ends the mixed state", Input{Tag: "fix", DeployedDigest: "bad", Journal: move(), Now: now}, ActionApply},
		{"the tag moved to the target", Input{Tag: "old", DeployedDigest: "bad", Journal: move(), Now: now}, ActionResume},
		{"retry goes back to what the tag names", Input{Tag: "bad", DeployedDigest: "bad", Retry: true, Journal: move(), Now: now}, ActionUndo},
		{"a hold that is not what the move left is held too", Input{Tag: "new", DeployedDigest: "cur", Journal: &Journal{Target: "old-id", Digest: "old", Previous: "cur-id", PreviousDigest: "cur", Phase: PhaseForward, Move: true, Hold: "new"}, Now: now}, ActionResume},
		{"a failed move is undone", Input{Tag: "bad", DeployedDigest: "bad", Journal: &Journal{Target: "old-id", Digest: "old", Previous: "bad-id", PreviousDigest: "bad", Phase: PhaseBack, Move: true, Hold: "bad"}, Now: now}, ActionUndo},
		{"a failed move backs off", Input{Tag: "bad", DeployedDigest: "bad", Journal: &Journal{Target: "old-id", Digest: "old", Previous: "bad-id", PreviousDigest: "bad", Phase: PhaseBack, Move: true, Hold: "bad", NextTry: now.Add(time.Hour)}, Now: now}, ActionWait},
		{"a redeploy of what runs, with the tag held elsewhere, finishes", Input{Tag: "bad", DeployedDigest: "old", FailedDigest: "bad", Journal: &Journal{Target: "old2-id", Digest: "old", Previous: "old-id", PreviousDigest: "old", Phase: PhaseForward, Move: true, Hold: "bad"}, Now: now}, ActionResume},
	}
	for _, c := range cases {
		if got := Decide(c.in).Kind; got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// A journal of a move, whatever the state, ends or waits. Retry aside, it never goes
// forward to what it holds.
func TestDecideOnAMoveNeverStrandsItsJournal(t *testing.T) {
	now := time.Unix(1000, 0)
	digests := []string{"", "a", "b", "c"}
	for _, tag := range []string{"a", "b", "c", "d"} {
		for _, failed := range digests {
			for _, hold := range digests {
				for _, phase := range []Phase{PhaseForward, PhaseBack} {
					for _, previous := range []string{"", "a"} {
						for _, next := range []time.Time{{}, now.Add(time.Hour)} {
							for _, retry := range []bool{false, true} {
								j := &Journal{Target: "b-id", Digest: "b", Phase: phase, NextTry: next, Move: true, Hold: hold}
								if previous != "" {
									j.Previous, j.PreviousDigest = previous+"-id", previous
								}
								in := Input{Tag: tag, FailedDigest: failed, Journal: j, Retry: retry, Now: now}
								got := Decide(in)
								switch got.Kind {
								case ActionApply:
									if !retry && (tag == failed || tag == hold) {
										t.Fatalf("%+v: went forward to what it holds", in)
									}
								case ActionResume:
								case ActionUndo:
									if previous == "" {
										t.Fatalf("%+v: undo with nothing to return to", in)
									}
								case ActionCleanup:
									if previous != "" {
										t.Fatalf("%+v: cleanup with a release to return to", in)
									}
								case ActionWait:
									if retry || !now.Before(next) {
										t.Fatalf("%+v: waited with nothing to wait for", in)
									}
								default:
									t.Fatalf("%+v: %s leaves the journal open", in, got.Kind)
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestRollbackHoldsTheTagAndALaterFixIsFollowed(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)

	outcome, digest, err := rollback(t, a)
	if err != nil || outcome != Deployed || digest != digestA {
		t.Fatalf("Rollback = %s, %s, %v", outcome, digest, err)
	}
	if len(f.verified) != 0 {
		t.Fatalf("verified %v; a stored release was verified when it was stored", f.verified)
	}
	if len(f.deployed) != 1 || f.deployed[0] != imageOf(digestA) {
		t.Fatalf("deployed = %v", f.deployed)
	}
	if opts := f.options[0]; !opts.Local || !opts.Undo || !opts.Move || opts.NoPull {
		t.Fatalf("options = %+v; want the image used if present, no pre-deploy, and a stop at the first failure", opts)
	}
	st := loadState(t, a)
	if st.DeployedDigest != digestA || st.FailedDigest != digestB || st.Journal != nil || st.FailedAt.IsZero() || st.LastError != "" {
		t.Fatalf("state = %+v", st)
	}

	// The tag still names what was left behind, and nothing changes.
	if outcome, err := reconcile(t, a, false); err != nil || outcome != SkippedFailed || len(f.deployed) != 1 {
		t.Fatalf("Reconcile = %s, %v, deployed %v", outcome, err, f.deployed)
	}

	f.tag = digestC
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile after a fix = %s, %v", outcome, err)
	}
	st = loadState(t, a)
	if st.DeployedDigest != digestC || st.FailedDigest != "" {
		t.Fatalf("state after the fix = %+v", st)
	}
}

func TestRetryAfterARollbackDeploysWhatTheTagNames(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)
	if _, _, err := rollback(t, a); err != nil {
		t.Fatal(err)
	}
	if outcome, err := reconcile(t, a, true); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile with retry = %s, %v", outcome, err)
	}
	if st := loadState(t, a); st.DeployedDigest != digestB || st.FailedDigest != "" {
		t.Fatalf("state = %+v", st)
	}
}

func TestRollbackWorksWithTheRegistryDown(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)
	f.tag = ""

	outcome, _, err := rollback(t, a)
	if err != nil || outcome != Deployed {
		t.Fatalf("Rollback = %s, %v", outcome, err)
	}
	if st := loadState(t, a); st.DeployedDigest != digestA || st.FailedDigest != digestB {
		t.Fatalf("state = %+v; want the last digest seen held", st)
	}
}

// Previous alone would bounce between two releases on a third rollback if a move left
// the release it moved away from in Applied.
func TestEachRollbackGoesFurtherBack(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB, digestC, digestD)
	for _, want := range []string{digestC, digestB, digestA} {
		outcome, digest, err := rollback(t, a)
		if err != nil || outcome != Deployed || digest != want {
			t.Fatalf("Rollback = %s, %s, %v; want %s", outcome, digest, err, want)
		}
		st := loadState(t, a)
		if st.DeployedDigest != want || st.FailedDigest != digestD {
			t.Fatalf("state = %+v", st)
		}
	}
	_, _, err := rollback(t, a)
	if err == nil || !strings.Contains(err.Error(), "no earlier release") || !strings.Contains(err.Error(), digestHex(digestB)) {
		t.Fatalf("a rollback past the oldest = %v; want it to name what is stored", err)
	}
	if got := f.deployed; len(got) != 3 {
		t.Fatalf("deployed %v", got)
	}
}

func TestRollbackFromAReleaseThatFailedKeepsWhyItFailed(t *testing.T) {
	f := &fakeServer{deployErrs: map[string]error{digestC: errors.New("health check failed")}}
	a := deployedChain(t, f, digestA, digestB)
	f.tag = digestC
	if outcome, err := reconcile(t, a, false); outcome != Failed || err == nil {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	failed := loadState(t, a)

	outcome, digest, err := rollback(t, a)
	if err != nil || outcome != Deployed || digest != digestA {
		t.Fatalf("Rollback = %s, %s, %v", outcome, digest, err)
	}
	st := loadState(t, a)
	if st.FailedDigest != digestC || st.LastError != failed.LastError || !st.FailedAt.Equal(failed.FailedAt) {
		t.Fatalf("state = %+v; want the failure kept", st)
	}
}

func TestInterruptedRollbackFinishesAndDoesNotReturnToWhatItLeft(t *testing.T) {
	for _, tagDown := range []bool{false, true} {
		f := &fakeServer{}
		a := deployedChain(t, f, digestA, digestB)

		// What the journal said when the deploy started is what a crash leaves.
		var journal Journal
		deploy := a.Deploy
		a.Deploy = func(ctx context.Context, cfg *config.Config, image string, opts DeployOptions) error {
			st, err := a.State.Load()
			if err != nil || st.Journal == nil {
				t.Fatalf("the rollback's deploy started with state %+v, %v", st, err)
			}
			journal = *st.Journal
			return deploy(ctx, cfg, image, opts)
		}
		if _, _, err := rollback(t, a); err != nil {
			t.Fatal(err)
		}
		a.Deploy = deploy
		if !journal.Move || journal.Hold != digestB || !journal.Back || journal.Digest != digestA || journal.PreviousDigest != digestB {
			t.Fatalf("journal = %+v", journal)
		}

		st := loadState(t, a)
		st.DeployedDigest, st.Current, st.FailedDigest, st.FailedAt = digestB, journal.Previous, "", time.Time{}
		st.Applied = []string{journal.Previous, journal.Target}
		st.Journal = &journal
		if err := a.State.Save(st); err != nil {
			t.Fatal(err)
		}
		f.deployed, f.options = nil, nil
		if tagDown {
			f.tag = ""
		}

		outcome, _, err := a.Reconcile(context.Background(), false)
		if err != nil || outcome != Deployed {
			t.Fatalf("Reconcile = %s, %v", outcome, err)
		}
		if len(f.deployed) != 1 || f.deployed[0] != imageOf(digestA) || !f.options[0].Undo || !f.options[0].Move {
			t.Fatalf("deployed = %v, options %+v", f.deployed, f.options)
		}
		got := loadState(t, a)
		if got.DeployedDigest != digestA || got.FailedDigest != digestB || got.Journal != nil {
			t.Fatalf("state = %+v; want the rollback finished and the tag held", got)
		}
		if tagDown {
			f.tag = digestB
		}
		if outcome, err := reconcile(t, a, false); err != nil || outcome != SkippedFailed {
			t.Fatalf("next check = %s, %v; want it held", outcome, err)
		}
	}
}

func TestRollbackThatFailsReturnsAndChangesNothingAboutTheTag(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)
	f.deployErrs = map[string]error{digestA: errors.New("image is corrupt")}

	outcome, _, err := rollback(t, a)
	if outcome != Failed || err == nil || !strings.Contains(err.Error(), "image is corrupt") {
		t.Fatalf("Rollback = %s, %v", outcome, err)
	}
	st := loadState(t, a)
	if st.DeployedDigest != digestB || st.FailedDigest != "" || st.Journal != nil {
		t.Fatalf("state = %+v; want what ran before, and no hold", st)
	}
	if !strings.Contains(st.LastError, "image is corrupt") {
		t.Fatalf("last error = %q", st.LastError)
	}
	if len(f.deployed) != 2 || f.deployed[1] != imageOf(digestB) {
		t.Fatalf("deployed = %v; want the rollback, then back", f.deployed)
	}
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Unchanged {
		t.Fatalf("next check = %s, %v", outcome, err)
	}
}

// A hold that was in place must survive a move that fails, or the next check would deploy
// what the operator was moving away from.
func TestMoveThatFailsKeepsAnEarlierHold(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB, digestC)
	if _, _, err := rollback(t, a); err != nil { // to B, holding C
		t.Fatal(err)
	}
	f.deployErrs = map[string]error{digestA: errors.New("image is corrupt")}
	if _, _, err := rollback(t, a); err == nil {
		t.Fatal("a move that failed reported success")
	}
	st := loadState(t, a)
	if st.DeployedDigest != digestB || st.FailedDigest != digestC || st.Journal != nil {
		t.Fatalf("state = %+v; want the hold kept", st)
	}
	f.deployed = nil
	if outcome, err := reconcile(t, a, false); err != nil || outcome != SkippedFailed || len(f.deployed) != 0 {
		t.Fatalf("next check = %s, %v, deployed %v", outcome, err, f.deployed)
	}
}

func TestSyncDigestVerifiesAnUnknownDigestUnlessToldNotTo(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	f.verifyErr = errors.New("no matching signatures")
	before := loadState(t, a)

	outcome, _, err := a.Move(context.Background(), digestB, MoveOptions{})
	if outcome != Failed || err == nil || !strings.Contains(err.Error(), "no matching signatures") {
		t.Fatalf("Move = %s, %v", outcome, err)
	}
	if rec, _ := a.records().Find(digestB, nil); rec != nil {
		t.Fatalf("stored %+v; a record exists only if this server accepted it", rec)
	}
	st := loadState(t, a)
	if st.DeployedDigest != before.DeployedDigest || st.FailedDigest != "" || st.Journal != nil || len(f.deployed) != 0 {
		t.Fatalf("state = %+v, deployed %v; nothing may change", st, f.deployed)
	}

	f.verifyErr = errors.New("verified despite --no-verify")
	f.verified = nil
	outcome, digest, err := a.Move(context.Background(), digestB, MoveOptions{NoVerify: true})
	if err != nil || outcome != Deployed || digest != digestB || len(f.verified) != 0 {
		t.Fatalf("Move = %s, %s, %v", outcome, digest, err)
	}
	rec, err := a.records().Find(digestB, nil)
	if err != nil || rec == nil || rec.Origin != OriginVerifySkipped {
		t.Fatalf("record = %+v, %v", rec, err)
	}
	if opts := f.options[0]; opts.Undo {
		t.Fatalf("options = %+v; a release this server never applied runs its pre-deploy command", opts)
	}
	if st := loadState(t, a); st.DeployedDigest != digestB || st.FailedDigest != digestA {
		t.Fatalf("state = %+v; want the tag held", st)
	}
}

// A release that failed before its pre-deploy command ran is stored, and was never
// applied, so moving to it must run the command.
func TestMoveToAReleaseStoredButNeverAppliedRunsItsPreDeploy(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	f.tag = digestB
	f.deployErrs = map[string]error{digestB: fmt.Errorf("pull failed: %w", deploy.ErrNothingSwitched)}
	if outcome, _ := reconcile(t, a, false); outcome != Failed {
		t.Fatalf("Reconcile = %s", outcome)
	}
	f.deployErrs, f.options = nil, nil
	if _, _, err := a.Move(context.Background(), digestB, MoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if opts := f.options[0]; opts.Undo {
		t.Fatalf("options = %+v", opts)
	}
	if len(f.verified) != 1 {
		t.Fatalf("verified %v; want only the first attempt", f.verified)
	}
}

// An interrupted move forward keeps its answer on the pre-deploy command, whoever finishes it.
func TestARollbackFinishingAnInterruptedMoveForwardRunsItsPreDeploy(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	rec, err := a.Source.Previous(nil, digestB)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.records().Store(rec); err != nil {
		t.Fatal(err)
	}
	st := loadState(t, a)
	st.Journal = &Journal{Target: rec.ID, Digest: digestB, Previous: st.Current, PreviousDigest: digestA, Phase: PhaseForward, Move: true, Hold: digestA}
	if err := a.State.Save(st); err != nil {
		t.Fatal(err)
	}
	if outcome, digest, err := rollback(t, a); err != nil || outcome != Deployed || digest != digestB {
		t.Fatalf("Rollback = %s, %s, %v", outcome, digest, err)
	}
	if opts := f.options[0]; opts.Undo {
		t.Fatalf("options = %+v; its pre-deploy command may not have run", opts)
	}
}

func TestNoPullStaysWithAMoveThroughACrash(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)
	var journal Journal
	inner := a.Deploy
	a.Deploy = func(ctx context.Context, cfg *config.Config, image string, opts DeployOptions) error {
		st, err := a.State.Load()
		if err != nil || st.Journal == nil {
			t.Fatalf("state = %+v, %v", st, err)
		}
		journal = *st.Journal
		return inner(ctx, cfg, image, opts)
	}
	if _, _, err := a.Rollback(context.Background(), MoveOptions{NoPull: true}); err != nil {
		t.Fatal(err)
	}
	a.Deploy = inner
	if !journal.NoPull {
		t.Fatalf("journal = %+v", journal)
	}

	st := loadState(t, a)
	st.DeployedDigest, st.Current, st.FailedDigest = digestB, journal.Previous, ""
	st.Journal = &journal
	if err := a.State.Save(st); err != nil {
		t.Fatal(err)
	}
	f.options = nil
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if !f.options[0].NoPull {
		t.Fatalf("options = %+v; the resume must not fetch what the operator said not to", f.options[0])
	}
}

// The tag moved during an interrupted move, to a release that failed its verification.
// That is what the tag names when the move finishes, so it is what stays held.
func TestAMoveFinishingKeepsAReleaseThatFailedSinceItBegan(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)
	st := loadState(t, a)
	second := st.Current
	first := imageRecordID(digestA, a.Source.(*ImageSource).Files)
	st.Journal = &Journal{Target: first, Digest: digestA, Previous: second, PreviousDigest: digestB, Phase: PhaseForward, Move: true, Hold: digestB, Back: true}
	if err := a.State.Save(st); err != nil {
		t.Fatal(err)
	}

	f.tag = digestC
	f.verifyErr = errors.New("no matching signatures")
	if outcome, err := reconcile(t, a, false); outcome != Failed || err == nil {
		t.Fatalf("Reconcile = %s, %v; want the new tag's verification to fail", outcome, err)
	}
	failed := loadState(t, a)
	if failed.FailedDigest != digestC || failed.Journal == nil {
		t.Fatalf("state = %+v", failed)
	}

	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v; want the move finished", outcome, err)
	}
	got := loadState(t, a)
	if got.DeployedDigest != digestA || got.FailedDigest != digestC || got.LastError != failed.LastError || got.Journal != nil {
		t.Fatalf("state = %+v", got)
	}
	f.verified = nil
	if outcome, err := reconcile(t, a, false); err != nil || outcome != SkippedFailed || len(f.verified) != 0 {
		t.Fatalf("next check = %s, %v, verified %v; the tag has not moved", outcome, err, f.verified)
	}
}

// A journal another release left open names roles of a config the command made. They are
// not the agent's to stop.
func TestAMoveOverAJournalLeavesADeployCommandsContainers(t *testing.T) {
	const withWorker = "service: app\nimage: " + testRepository + "\nservers:\n  web:\n    hosts: [app-1]\n  worker:\n    hosts: [app-1]\n    cmd: work\nproxy:\n  host: app.example.com\nagent:\n  track: production\n  verify_command: cosign verify \"$$AZUD_AGENT_IMAGE\"\n"
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	a.DeployUndoes = true
	f.runningRoles = []string{"web", "worker"}
	rec := testImageSource(t, withWorker)
	abandoned, err := rec.Previous(nil, digestB)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.records().Store(abandoned); err != nil {
		t.Fatal(err)
	}
	st := loadState(t, a)
	st.Journal = &Journal{Target: abandoned.ID, Digest: digestB, Previous: st.Current, PreviousDigest: digestA, Phase: PhaseForward}
	if err := a.State.Save(st); err != nil {
		t.Fatal(err)
	}
	f.tag = digestC
	if _, _, err := a.Move(context.Background(), digestC, MoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(f.stopped) != 0 {
		t.Fatalf("stopped %v", f.stopped)
	}
}

func TestRollbackStopsRolesOnlyTheReleaseItLeftHas(t *testing.T) {
	const withWorker = "service: app\nimage: " + testRepository + "\nservers:\n  web:\n    hosts: [app-1]\n  worker:\n    hosts: [app-1]\n    cmd: work\nproxy:\n  host: app.example.com\nagent:\n  track: production\n  verify_command: cosign verify \"$$AZUD_AGENT_IMAGE\"\n"
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	// The second release was applied with a worker the first does not have.
	source := testImageSource(t, withWorker)
	a.Source, a.Server = source, source.Server
	f.tag = digestB
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	f.runningRoles = []string{"web", "worker"}

	// A crash before the journal closes must come back to this, so it is the journal's
	// to finish, and it stops nothing until the target has deployed.
	var stoppedDuringDeploy []string
	inner := a.Deploy
	a.Deploy = func(ctx context.Context, cfg *config.Config, image string, opts DeployOptions) error {
		stoppedDuringDeploy = slices.Clone(f.stopped)
		return inner(ctx, cfg, image, opts)
	}
	if _, _, err := rollback(t, a); err != nil {
		t.Fatal(err)
	}
	if len(stoppedDuringDeploy) != 0 {
		t.Fatalf("stopped %v before the target deployed", stoppedDuringDeploy)
	}
	if !slices.Equal(f.stopped, []string{"worker"}) {
		t.Fatalf("stopped = %v", f.stopped)
	}
	if st := loadState(t, a); st.Journal != nil || st.DeployedDigest != digestA {
		t.Fatalf("state = %+v", st)
	}
}

func TestSyncDigestDeploysAVerifiedUnknownDigestAndAStoredOneUnverified(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)

	if _, _, err := a.Move(context.Background(), digestC, MoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(f.verified) != 1 || f.verified[0] != imageOf(digestC) {
		t.Fatalf("verified = %v", f.verified)
	}
	rec, err := a.records().Find(digestC, nil)
	if err != nil || rec == nil || rec.Origin != OriginVerified {
		t.Fatalf("record = %+v, %v", rec, err)
	}

	f.verified, f.options = nil, nil
	// The record's id also names it.
	id := imageRecordID(digestA, a.Source.(*ImageSource).Files)
	if _, _, err := a.Move(context.Background(), id, MoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(f.verified) != 0 {
		t.Fatalf("verified %v; a stored release is not verified again", f.verified)
	}
	if opts := f.options[0]; !opts.Undo {
		t.Fatalf("options = %+v; a release this server applied does not run its pre-deploy command again", opts)
	}
	if st := loadState(t, a); st.DeployedDigest != digestA || st.FailedDigest != digestB {
		t.Fatalf("state = %+v", st)
	}
}

func TestMovingToTheTaggedReleaseHoldsNothing(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)
	f.tag = digestA
	if _, _, err := a.Move(context.Background(), digestA, MoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if st := loadState(t, a); st.DeployedDigest != digestA || st.FailedDigest != "" {
		t.Fatalf("state = %+v", st)
	}
}

func TestMovingToWhatIsDeployedOnlyHolds(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	f.tag = digestB
	outcome, _, err := a.Move(context.Background(), digestA, MoveOptions{})
	if err != nil || outcome != Unchanged || len(f.deployed) != 0 {
		t.Fatalf("Move = %s, %v, deployed %v", outcome, err, f.deployed)
	}
	if st := loadState(t, a); st.FailedDigest != digestB || st.DeployedDigest != digestA {
		t.Fatalf("state = %+v", st)
	}
	if outcome, err := reconcile(t, a, false); err != nil || outcome != SkippedFailed {
		t.Fatalf("next check = %s, %v", outcome, err)
	}
}

func TestMoveRefusesAMalformedTarget(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	for _, target := range []string{"../../etc", "v1.2.2", "sha256:zz", "sha256:" + strings.Repeat("a", 64) + "-../x"} {
		if _, _, err := a.Move(context.Background(), target, MoveOptions{}); err == nil {
			t.Errorf("Move(%q) succeeded", target)
		}
	}
	if len(f.deployed) != 0 {
		t.Fatalf("deployed %v", f.deployed)
	}
}

func TestMoveEndsAMixedStateAndRollbackGoesToWhatRanBefore(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)
	st := loadState(t, a)
	second := st.Current
	target := imageRecordID(digestC, a.Source.(*ImageSource).Files)
	rec, err := a.Source.Previous(nil, digestC)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.records().Store(rec); err != nil {
		t.Fatal(err)
	}
	st.Journal = &Journal{Target: target, Digest: digestC, Previous: second, PreviousDigest: digestB, Phase: PhaseForward}
	if err := a.State.Save(st); err != nil {
		t.Fatal(err)
	}
	f.tag = digestC

	outcome, digest, err := rollback(t, a)
	if err != nil || outcome != Deployed || digest != digestB {
		t.Fatalf("Rollback = %s, %s, %v; want the release the journal left behind", outcome, digest, err)
	}
	got := loadState(t, a)
	if got.Journal != nil || got.DeployedDigest != digestB || got.FailedDigest != digestC {
		t.Fatalf("state = %+v", got)
	}
}

func TestRollbackDuringAFirstReleaseSaysWhatToDo(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a := f.agent(t)
	seed(t, a, State{}, &Journal{Digest: digestA, Phase: PhaseForward})
	_, _, err := rollback(t, a)
	if err == nil || !strings.Contains(err.Error(), "azud sync") {
		t.Fatalf("Rollback = %v", err)
	}
}

func TestMoveDoesNotCrossSources(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	bundle := &Record{ID: digestHex(digestB), Source: SourceBundle, Digest: digestB, Image: imageOf(digestC), Info: &release.Info{Service: "app"}, Files: []release.File{{Name: "deploy.yml", Data: []byte(testServerYAML)}}}
	if err := a.records().Store(bundle); err != nil {
		t.Fatal(err)
	}
	_, _, err := a.Move(context.Background(), digestB, MoveOptions{})
	if err == nil || !strings.Contains(err.Error(), "secrets") {
		t.Fatalf("Move to a bundle on an image server = %v", err)
	}
	if len(f.deployed) != 0 {
		t.Fatalf("deployed %v", f.deployed)
	}
}

// A failure to take the lock says nothing about the release, and a move must not mark it
// failed, which would stop the agent from following the tag to it.
func TestBusyLockDoesNotFailAMove(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)
	lock := a.Lock
	a.Lock = func(func() error) error { return errors.New("lock is busy") }
	if _, _, err := rollback(t, a); err == nil {
		t.Fatal("Rollback succeeded with no lock")
	}
	a.Lock = lock
	if st := loadState(t, a); st.FailedDigest != "" || st.Journal != nil || st.DeployedDigest != digestB {
		t.Fatalf("state = %+v", st)
	}
}

func TestDeployCommandMoveFailsWithoutAnUndoOrAMark(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)
	a.DeployUndoes = true
	f.deployErrs = map[string]error{digestA: errors.New("script failed")}
	if _, _, err := rollback(t, a); err == nil {
		t.Fatal("Rollback succeeded")
	}
	if len(f.deployed) != 1 {
		t.Fatalf("deployed %v; the command owns its undo", f.deployed)
	}
	if st := loadState(t, a); st.FailedDigest != "" || st.Journal != nil || st.DeployedDigest != digestB {
		t.Fatalf("state = %+v", st)
	}
}

func imageServerWith(t *testing.T, f *fakeServer, yaml string) (*Agent, *ImageSource) {
	t.Helper()
	a := f.agent(t)
	source := testImageSource(t, yaml)
	a.Source, a.Server = source, source.Server
	return a, source
}

func TestRedeployAppliesAnEditedConfigurationAsANewRecord(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a, source := imageServerWith(t, f, testServerYAML)
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	first := loadState(t, a).Current

	// Nothing changed: the same record, deployed again.
	f.deployed, f.options, f.verified = nil, nil, nil
	outcome, digest, err := a.Redeploy(context.Background())
	if err != nil || outcome != Deployed || digest != digestA {
		t.Fatalf("Redeploy = %s, %s, %v", outcome, digest, err)
	}
	if st := loadState(t, a); st.Current != first || len(st.Applied) != 1 || len(f.deployed) != 1 {
		t.Fatalf("state = %+v, deployed %v", st, f.deployed)
	}
	if opts := f.options[0]; !opts.Local || opts.Undo {
		t.Fatalf("options = %+v", opts)
	}

	edited := strings.Replace(testServerYAML, "proxy:\n", "env:\n  clear:\n    EDITED: \"yes\"\nproxy:\n", 1)
	source.Files = []release.File{{Name: "deploy.yml", Data: []byte(edited)}}
	var deployedWith *config.Config
	inner := a.Deploy
	a.Deploy = func(ctx context.Context, cfg *config.Config, image string, opts DeployOptions) error {
		deployedWith = cfg
		return inner(ctx, cfg, image, opts)
	}
	if _, _, err := a.Redeploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := loadState(t, a)
	if st.Current == first || !strings.HasPrefix(st.Current, digestHex(digestA)+"-") || st.DeployedDigest != digestA {
		t.Fatalf("state = %+v; want a new record for the same digest", st)
	}
	if !slices.Equal(st.Applied, []string{st.Current, first}) {
		t.Fatalf("applied = %v", st.Applied)
	}
	if deployedWith.Env.Clear["EDITED"] != "yes" {
		t.Fatalf("deployed with %+v; want the edit", deployedWith.Env)
	}
	if len(f.verified) != 0 {
		t.Fatalf("verified %v; the image is the one already verified", f.verified)
	}
	rec, err := a.records().Load(st.Current)
	if err != nil || rec.Origin != OriginVerified {
		t.Fatalf("record = %+v, %v; want the origin of the release it re-applies", rec, err)
	}

	// A rollback returns to the configuration before the edit, on the same image.
	if outcome, digest, err := rollback(t, a); err != nil || outcome != Deployed || digest != digestA {
		t.Fatalf("Rollback = %s, %s, %v", outcome, digest, err)
	}
	if st := loadState(t, a); st.Current != first || st.FailedDigest != "" {
		t.Fatalf("state = %+v", st)
	}
}

func TestRedeployKeepsWhatTheTagIsHeldAt(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)
	if _, _, err := rollback(t, a); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Redeploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := loadState(t, a)
	if st.DeployedDigest != digestA || st.FailedDigest != digestB {
		t.Fatalf("state = %+v; a redeploy must not release the hold", st)
	}
	f.deployed = nil
	if outcome, err := reconcile(t, a, false); err != nil || outcome != SkippedFailed || len(f.deployed) != 0 {
		t.Fatalf("next check = %s, %v, deployed %v", outcome, err, f.deployed)
	}
}

func TestRedeployThatFailsReturnsToTheRecordItReplaced(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a, source := imageServerWith(t, f, testServerYAML)
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	first := loadState(t, a).Current
	edited := strings.Replace(testServerYAML, "proxy:\n", "env:\n  clear:\n    EDITED: \"yes\"\nproxy:\n", 1)
	source.Files = []release.File{{Name: "deploy.yml", Data: []byte(edited)}}

	failures := []error{errors.New("the edit broke it"), nil}
	var configs []*config.Config
	a.Deploy = func(_ context.Context, cfg *config.Config, _ string, _ DeployOptions) error {
		configs = append(configs, cfg)
		err := failures[0]
		failures = failures[1:]
		return err
	}
	outcome, _, err := a.Redeploy(context.Background())
	if outcome != Failed || err == nil || !strings.Contains(err.Error(), "the edit broke it") {
		t.Fatalf("Redeploy = %s, %v", outcome, err)
	}
	st := loadState(t, a)
	if st.Current != first || st.DeployedDigest != digestA || st.FailedDigest != "" || st.Journal != nil {
		t.Fatalf("state = %+v; want the record it replaced, and the digest not marked failed", st)
	}
	if len(configs) != 2 || configs[0].Env.Clear["EDITED"] != "yes" || configs[1].Env.Clear["EDITED"] != "" {
		t.Fatalf("deployed with %d configurations; want the edit, then the record before it", len(configs))
	}
}

// A running agent must not put back what it read when it started, after `azud redeploy`
// applied an edit, and a release deploys what the files hold when it is applied.
func TestANewReleaseReadsTheConfigurationOnDisk(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a, source := imageServerWith(t, f, testServerYAML)
	disk := []release.File{{Name: "deploy.yml", Data: []byte(testServerYAML)}}
	source.Reload = func() ([]release.File, error) { return disk, nil }
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}

	edited := strings.Replace(testServerYAML, "proxy:\n", "env:\n  clear:\n    EDITED: \"yes\"\nproxy:\n", 1)
	disk = []release.File{{Name: "deploy.yml", Data: []byte(edited)}}
	var deployedWith *config.Config
	inner := a.Deploy
	a.Deploy = func(ctx context.Context, cfg *config.Config, image string, opts DeployOptions) error {
		deployedWith = cfg
		return inner(ctx, cfg, image, opts)
	}
	if _, _, err := a.Redeploy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deployedWith.Env.Clear["EDITED"] != "yes" {
		t.Fatalf("the redeploy used %+v", deployedWith.Env)
	}

	f.tag = digestB
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if deployedWith.Env.Clear["EDITED"] != "yes" {
		t.Fatalf("the next release used %+v; it put back what the agent read when it started", deployedWith.Env)
	}
	rec, err := a.records().Load(loadState(t, a).Current)
	if err != nil || !strings.Contains(string(rec.Files[0].Data), "EDITED") {
		t.Fatalf("record = %+v, %v", rec, err)
	}

	// A file that no longer reads refuses the release, and changes nothing.
	source.Reload = func() ([]release.File, error) { return nil, errors.New("file is half written") }
	f.tag = digestC
	if outcome, err := reconcile(t, a, false); outcome != Failed || err == nil || !strings.Contains(err.Error(), "half written") {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	if st := loadState(t, a); st.DeployedDigest != digestB || st.Journal != nil {
		t.Fatalf("state = %+v", st)
	}
}

func TestRedeployRefusesWhenThereIsNoOneReleaseToApply(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a := f.agent(t)
	if _, _, err := a.Redeploy(context.Background()); err == nil || !strings.Contains(err.Error(), "nothing is deployed") {
		t.Fatalf("Redeploy with nothing deployed = %v", err)
	}
	seed(t, a, State{DeployedDigest: digestA}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseForward})
	if _, _, err := a.Redeploy(context.Background()); err == nil || !strings.Contains(err.Error(), "azud sync") {
		t.Fatalf("Redeploy with a journal open = %v", err)
	}
	if len(f.deployed) != 0 {
		t.Fatalf("deployed %v", f.deployed)
	}
}

func TestRedeployAdoptsAnImageWithNoRecord(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a := f.agent(t)
	if err := a.State.Save(State{DeployedDigest: digestA}); err != nil {
		t.Fatal(err)
	}
	if outcome, _, err := a.Redeploy(context.Background()); err != nil || outcome != Deployed {
		t.Fatalf("Redeploy = %s, %v", outcome, err)
	}
	st := loadState(t, a)
	rec, err := a.records().Load(st.Current)
	if err != nil || rec == nil || rec.Origin != OriginAdopted {
		t.Fatalf("record = %+v, %v", rec, err)
	}
}

func TestBundleRollbackBringsBackItsSecretsWithoutTheRegistry(t *testing.T) {
	e := newBundleEnv(t)
	first := e.publish(webOnApp1, "DATABASE_URL=one\n", day)
	second := e.publish(webOnApp1, "DATABASE_URL=two\n", day.Add(time.Hour))
	if err := e.apply(first); err != nil {
		t.Fatal(err)
	}
	if err := e.apply(second); err != nil {
		t.Fatal(err)
	}

	e.agent.Resolve = func(context.Context) (string, error) { return "", errors.New("registry unreachable") }
	e.source.Repo = nil
	outcome, digest, err := e.agent.Rollback(context.Background(), MoveOptions{})
	if err != nil || outcome != Deployed || digest != first {
		t.Fatalf("Rollback = %s, %s, %v", outcome, digest, err)
	}
	if last := e.deploys[len(e.deploys)-1]; last.env != "DATABASE_URL=one\n" {
		t.Fatalf("secrets during the rollback = %q; want the release's own", last.env)
	}
	if got := e.env(); got != "DATABASE_URL=one\n" {
		t.Fatalf("secrets = %q", got)
	}
	st, err := e.agent.State.Load()
	if err != nil || st.DeployedDigest != first || st.FailedDigest != second {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

func TestBundleRollbackThatFailsPutsTheSecretsBack(t *testing.T) {
	e := newBundleEnv(t)
	first := e.publish(webOnApp1, "DATABASE_URL=one\n", day)
	second := e.publish(webOnApp1, "DATABASE_URL=two\n", day.Add(time.Hour))
	if err := e.apply(first); err != nil {
		t.Fatal(err)
	}
	if err := e.apply(second); err != nil {
		t.Fatal(err)
	}

	e.fails = []error{fmt.Errorf("pull failed: %w", deploy.ErrNothingSwitched)}
	if _, _, err := e.agent.Rollback(context.Background(), MoveOptions{}); err == nil {
		t.Fatal("Rollback succeeded")
	}
	if got := e.env(); got != "DATABASE_URL=two\n" {
		t.Fatalf("secrets = %q; the image that runs is release 2's", got)
	}
	st, err := e.agent.State.Load()
	if err != nil || st.DeployedDigest != second || st.FailedDigest != "" || st.Journal != nil {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

// Moving the tag alone is refused; a move by someone with a shell on this server is not.
func TestMoveSkipsTheDowngradeCheckAndATagMoveDoesNot(t *testing.T) {
	e := newBundleEnv(t)
	old := e.publish(webOnApp1, "DATABASE_URL=old\n", day)
	current := e.publish(webOnApp1, "DATABASE_URL=current\n", day.Add(24*time.Hour))
	if err := e.apply(current); err != nil {
		t.Fatal(err)
	}

	if err := e.apply(old); err == nil || !strings.Contains(err.Error(), "older than") || !strings.Contains(err.Error(), "azud release republish") {
		t.Fatalf("a tag moved to an older bundle = %v", err)
	}
	if got := e.env(); got != "DATABASE_URL=current\n" {
		t.Fatalf("secrets = %q", got)
	}

	e.tag = current
	outcome, digest, err := e.agent.Move(context.Background(), old, MoveOptions{})
	if err != nil || outcome != Deployed || digest != old {
		t.Fatalf("Move = %s, %s, %v", outcome, digest, err)
	}
	if got := e.env(); got != "DATABASE_URL=old\n" {
		t.Fatalf("secrets = %q", got)
	}
	st, err := e.agent.State.Load()
	if err != nil || st.DeployedDigest != old || st.FailedDigest != current {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

// A rollback runs from an operator's shell, whose environment is not the agent's.
func TestBundleRollbackDeploysWhatItsEnvironmentReferencesMeantThen(t *testing.T) {
	e := newBundleEnv(t)
	t.Setenv("RELEASE_MODE", "as-deployed")
	withReference := strings.Replace(webOnApp1, "MODE: v1", "MODE: ${RELEASE_MODE}", 1)
	first := e.publish(withReference, "DATABASE_URL=one\n", day)
	second := e.publish(webOnApp1, "DATABASE_URL=two\n", day.Add(time.Hour))
	if err := e.apply(first); err != nil {
		t.Fatal(err)
	}
	if err := e.apply(second); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RELEASE_MODE", "another shell")
	if _, _, err := e.agent.Rollback(context.Background(), MoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := e.deploys[len(e.deploys)-1].cfg.Env.Clear["MODE"]; got != "as-deployed" {
		t.Fatalf("MODE = %q; a record must mean what it meant when it was deployed", got)
	}
	rec, err := e.agent.records().Load(digestHex(first))
	if err != nil || !strings.Contains(string(rec.Files[0].Data), "${RELEASE_MODE}") {
		t.Fatalf("files = %s, %v; the files stay as written, for a v1 agent to read", rec.Files[0].Data, err)
	}
}

// A record is the first one stored under its id, so an attempt in another environment
// deploys what the record says, or a later undo would read something else.
func TestARetriedBundleDeploysWhatIsStored(t *testing.T) {
	e := newBundleEnv(t)
	t.Setenv("RELEASE_MODE", "first attempt")
	withReference := strings.Replace(webOnApp1, "MODE: v1", "MODE: ${RELEASE_MODE}", 1)
	digest := e.publish(withReference, "DATABASE_URL=one\n", day)
	e.fails = []error{fmt.Errorf("pull failed: %w", deploy.ErrNothingSwitched)}
	if err := e.apply(digest); err == nil {
		t.Fatal("the first attempt succeeded")
	}

	t.Setenv("RELEASE_MODE", "another shell")
	if err := e.apply(digest); err != nil {
		t.Fatal(err)
	}
	if got := e.deploys[len(e.deploys)-1].cfg.Env.Clear["MODE"]; got != "first attempt" {
		t.Fatalf("MODE = %q", got)
	}
}

func TestBundleRedeployUsesTheStoredRecordWithoutTheRegistry(t *testing.T) {
	e := newBundleEnv(t)
	first := e.publish(webOnApp1, "DATABASE_URL=one\n", day)
	if err := e.apply(first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.envPath, []byte("DATABASE_URL=overwritten\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.source.Repo = nil
	before := len(e.deploys)
	outcome, digest, err := e.agent.Redeploy(context.Background())
	if err != nil || outcome != Deployed || digest != first || len(e.deploys) != before+1 {
		t.Fatalf("Redeploy = %s, %s, %v", outcome, digest, err)
	}
	if last := e.deploys[len(e.deploys)-1]; last.env != "DATABASE_URL=one\n" {
		t.Fatalf("secrets during the redeploy = %q", last.env)
	}
}

// The state a stored release left by v1.6.0 has no record file and no list of what was
// applied, so it is named.
func TestRollbackToAStoredV1Release(t *testing.T) {
	t.Setenv("PUBLIC_HOST", "fixture.example.com")
	dir := v1Dir(t, v160StateFile, "")
	identity := filepath.Join(t.TempDir(), "identity.txt")
	data, err := os.ReadFile(filepath.Join("..", "release", "testdata", "v1.6.0", "full", "identity.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, data, 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(t.TempDir(), "secrets")
	server := testServer(t, v1FixtureServer)
	store := NewStore(dir, "fixture")

	var deployed []string
	var opts []DeployOptions
	var envs []string
	a := &Agent{
		Ref:        "ghcr.io/acme/fixture:production",
		Repository: "ghcr.io/acme/fixture",
		Resolve:    func(context.Context) (string, error) { return "", errors.New("registry unreachable") },
		Source:     &BundleSource{Server: server, Repository: "ghcr.io/acme/fixture", SecretsPath: secrets, Identity: identity, Log: output.DefaultLogger},
		Server:     server,
		Deploy: func(_ context.Context, _ *config.Config, image string, o DeployOptions) error {
			env, _ := os.ReadFile(secrets)
			deployed, opts, envs = append(deployed, image), append(opts, o), append(envs, string(env))
			return nil
		},
		StopRoles:    func(*config.Config, []string) error { return nil },
		RunningRoles: func(*config.Config) ([]string, error) { return nil, nil },
		State:        store,
		Log:          output.DefaultLogger,
	}

	// What runs came from elsewhere, as after a tag moved on from the stored release.
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.DeployedDigest, st.Current, st.Applied = v1FailedDigest, "", nil
	st.LastSeenDigest = v1FailedDigest
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	outcome, digest, err := a.Move(context.Background(), v1FixtureDigest, MoveOptions{Back: true})
	if err != nil || outcome != Deployed || digest != v1FixtureDigest {
		t.Fatalf("Move = %s, %s, %v", outcome, digest, err)
	}
	if len(deployed) != 1 || deployed[0] != v1FixtureImage || !opts[0].Undo {
		t.Fatalf("deployed = %v, options %+v", deployed, opts)
	}
	if envs[0] != "DATABASE_URL=postgres://fixture\n" {
		t.Fatalf("secrets during the rollback = %q", envs[0])
	}
	st, err = store.Load()
	if err != nil || st.DeployedDigest != v1FixtureDigest || st.FailedDigest != v1FailedDigest || st.Current != digestHex(v1FixtureDigest) {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

func TestFindPrefersWhatWasAppliedForAnImageDigest(t *testing.T) {
	store := NewStore(t.TempDir(), "app")
	records := store.Records(testRepository)
	put := func(hash string, at time.Time) string {
		t.Helper()
		id := digestHex(digestA) + "-" + hash
		if err := records.Store(&Record{ID: id, Source: SourceImage, Digest: digestA, Image: imageOf(digestA), StoredAt: at, Files: []release.File{{Name: "deploy.yml", Data: []byte(testServerYAML)}}}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	older := put("00000000", day)
	newer := put("11111111", day.Add(time.Hour))

	for _, c := range []struct {
		target  string
		applied []string
		want    string
	}{
		{digestA, nil, newer},
		{digestA, []string{older}, older},
		{digestHex(digestA), []string{older, newer}, older},
		{older, []string{newer}, older},
		{digestB, nil, ""},
		{digestHex(digestA) + "-22222222", nil, ""},
	} {
		rec, err := records.Find(c.target, c.applied)
		if err != nil {
			t.Fatalf("Find(%s): %v", c.target, err)
		}
		got := ""
		if rec != nil {
			got = rec.ID
		}
		if got != c.want {
			t.Errorf("Find(%s, %v) = %q, want %q", c.target, c.applied, got, c.want)
		}
	}
}

func TestListSkipsWhatIsNotARecord(t *testing.T) {
	store := NewStore(t.TempDir(), "app")
	records := store.Records(testRepository)
	if got, err := records.List(); err != nil || got != nil {
		t.Fatalf("List with no directory = %v, %v", got, err)
	}
	if err := records.Store(&Record{ID: digestHex(digestA) + "-00000000", Source: SourceImage, Digest: digestA}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(records.Dir(".store-123"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(records.Dir("pending"), []byte(digestA), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := records.List()
	if err != nil || len(got) != 1 || got[0].Digest != digestA {
		t.Fatalf("List = %v, %v", got, err)
	}
}
