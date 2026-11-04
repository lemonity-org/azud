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
)

// restoreSeams records what Restore asks for, in order.
type restoreSeams struct {
	calls    []string
	stopped  map[string]func(string) bool
	recreate []bool
	images   []string
	roleErr  error
	kindErr  map[string]error
	locked   []bool
}

func (r *restoreSeams) attach(a *Agent, f *fakeServer) {
	r.stopped = map[string]func(string) bool{}
	a.RestoreRoles = func(_ context.Context, _ *config.Config, image string, stopped func(string) bool, recreate bool) error {
		r.calls = append(r.calls, "roles")
		r.stopped["roles"] = stopped
		r.recreate = append(r.recreate, recreate)
		r.images = append(r.images, image)
		r.locked = append(r.locked, f.locked)
		return r.roleErr
	}
	a.StartExisting = func(kind string, stopped func(string) bool) error {
		r.calls = append(r.calls, kind)
		r.stopped[kind] = stopped
		return r.kindErr[kind]
	}
}

func TestRestoreBringsBackTheCurrentReleaseWithNoRegistry(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	seams := &restoreSeams{}
	seams.attach(a, f)
	f.tag = ""
	a.Resolve = func(context.Context) (string, error) {
		t.Fatal("Restore contacted the registry")
		return "", nil
	}
	a.Verify = func(context.Context, string) error {
		t.Fatal("Restore verified")
		return nil
	}

	if err := a.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if want := []string{DriftAccessory, "roles", DriftCron}; !slices.Equal(seams.calls, want) {
		t.Fatalf("calls = %v, want %v: accessories before the app that may need them", seams.calls, want)
	}
	if len(f.deployed) != 0 {
		t.Fatalf("deployed %v; nothing was open", f.deployed)
	}
	if !seams.recreate[0] || seams.images[0] != imageOf(digestA) || !seams.locked[0] {
		t.Fatalf("recreate %v, image %v, locked %v", seams.recreate, seams.images, seams.locked)
	}
}

func TestRestoreLeavesWhatAnOperatorStopped(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	seams := &restoreSeams{}
	seams.attach(a, f)
	if err := a.State.UpdateIntent(func(intent *Intent) {
		intent.Roles = WithStopped(intent.Roles, "worker", "web")
		intent.Accessories = WithStopped(intent.Accessories, "db")
	}); err != nil {
		t.Fatal(err)
	}

	if err := a.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"web", "worker"} {
		if !seams.stopped["roles"](role) {
			t.Fatalf("role %s is not stopped", role)
		}
	}
	if seams.stopped["roles"]("db") || seams.stopped["roles"]("other") {
		t.Fatal("a role nobody stopped counts as stopped")
	}
	if !seams.stopped[DriftAccessory]("db") {
		t.Fatal("accessory db is not stopped")
	}
	if seams.stopped[DriftAccessory]("web") || seams.stopped[DriftCron]("db") {
		t.Fatal("something nobody stopped counts as stopped")
	}
}

// A stop takes no lock, so it can come while a restore waits for the deploy lock.
func TestRestoreSeesAStopThatCameWhileItWaitedForTheLock(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	seams := &restoreSeams{}
	seams.attach(a, f)
	lock := a.Lock
	a.Lock = func(fn func() error) error {
		if err := a.State.UpdateIntent(func(intent *Intent) { intent.Roles = WithStopped(intent.Roles, "web") }); err != nil {
			t.Fatal(err)
		}
		return lock(fn)
	}

	if err := a.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !seams.stopped["roles"]("web") {
		t.Fatal("the restore would start a role an operator has stopped")
	}
}

// The second question is what catches a stop during the start, and it must not stop a
// role over a record it cannot read.
func TestAStopRecordThatCannotBeReadKeepsTheLastAnswerAndFailsTheRestore(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	seams := &restoreSeams{}
	seams.attach(a, f)
	if err := a.State.UpdateIntent(func(intent *Intent) { intent.Roles = WithStopped(intent.Roles, "worker") }); err != nil {
		t.Fatal(err)
	}
	var web, worker bool
	roles := a.RestoreRoles
	a.RestoreRoles = func(ctx context.Context, cfg *config.Config, image string, stopped func(string) bool, recreate bool) error {
		if err := os.WriteFile(a.State.intentPath(), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		web, worker = stopped("web"), stopped("worker")
		return roles(ctx, cfg, image, stopped, recreate)
	}

	err := a.Restore(context.Background())
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("Restore = %v; a record it cannot read must not look restored", err)
	}
	if web || !worker {
		t.Fatalf("web stopped %v, worker stopped %v; want the answers from before the record broke", web, worker)
	}
}

func TestRestoreFinishesAnOpenJournalFirstWithNoRegistry(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA, digestB)
	seams := &restoreSeams{}
	seams.attach(a, f)
	st := loadState(t, a)
	second := st.Current
	rec, err := a.Source.Previous(nil, digestC)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.records().Store(rec); err != nil {
		t.Fatal(err)
	}
	st.LastSeenDigest = digestC
	st.Journal = &Journal{Target: rec.ID, Digest: digestC, Previous: second, PreviousDigest: digestB, Phase: PhaseForward}
	if err := a.State.Save(st); err != nil {
		t.Fatal(err)
	}
	a.Resolve = func(context.Context) (string, error) {
		t.Fatal("Restore contacted the registry")
		return "", nil
	}
	inner := a.Deploy
	a.Deploy = func(ctx context.Context, cfg *config.Config, image string, opts DeployOptions) error {
		seams.calls = append(seams.calls, "deploy")
		return inner(ctx, cfg, image, opts)
	}

	if err := a.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if want := []string{DriftAccessory, "deploy", "roles", DriftCron}; !slices.Equal(seams.calls, want) {
		t.Fatalf("calls = %v, want %v", seams.calls, want)
	}
	got := loadState(t, a)
	if got.Journal != nil || got.DeployedDigest != digestC {
		t.Fatalf("state = %+v", got)
	}
	if seams.images[0] != imageOf(digestC) || !seams.recreate[0] {
		t.Fatalf("restored %v, recreate %v; want the release the journal finished", seams.images, seams.recreate)
	}
}

func TestRestoreUndoesAFailedJournalEvenInBackoff(t *testing.T) {
	now := time.Unix(1000, 0)
	f := &fakeServer{tag: digestB}
	a := f.agent(t)
	a.Now = func() time.Time { return now }
	seams := &restoreSeams{}
	seams.attach(a, f)
	seed(t, a, State{DeployedDigest: digestA, FailedDigest: digestB, LastSeenDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseBack, Tries: 3, NextTry: now.Add(time.Hour)})

	if err := a.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(f.deployed) != 1 || f.deployed[0] != imageOf(digestA) || !f.options[0].Undo {
		t.Fatalf("deployed = %v", f.deployed)
	}
	st := loadState(t, a)
	if st.Journal != nil || st.DeployedDigest != digestA || st.FailedDigest != digestB {
		t.Fatalf("state = %+v", st)
	}
}

// Only the first attempt after a start skips the backoff. Later ones must not redeploy
// on every check while the undo is backing off.
func TestRunDoesNotRepeatAFailedUndoOnEveryCheck(t *testing.T) {
	now := time.Unix(1000, 0)
	f := &fakeServer{tag: digestB, deployErrs: map[string]error{digestA: errors.New("undo failed")}}
	a := f.agent(t)
	a.Now = func() time.Time { return now }
	seams := &restoreSeams{}
	seams.attach(a, f)
	seed(t, a, State{DeployedDigest: digestA, FailedDigest: digestB, LastSeenDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseBack})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks := 0
	resolve := a.Resolve
	a.Resolve = func(c context.Context) (string, error) {
		if checks++; checks == 4 {
			cancel()
		}
		return resolve(c)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, time.Millisecond) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
	if len(f.deployed) != 1 {
		t.Fatalf("deployed %v over %d checks; the clock did not move, so the undo is backing off", f.deployed, checks)
	}
	if len(seams.recreate) == 0 || slices.Contains(seams.recreate, true) {
		t.Fatalf("recreate = %v; nothing is created while the journal is open", seams.recreate)
	}
}

// A journal that could not close leaves a secrets file that may not be this release's,
// so nothing is created from it. Existing containers keep what they were created with.
func TestRestoreOnlyStartsWhatExistsWhileAJournalStaysOpen(t *testing.T) {
	now := time.Unix(1000, 0)
	f := &fakeServer{tag: digestB, deployErrs: map[string]error{digestA: errors.New("undo failed")}}
	a := f.agent(t)
	a.Now = func() time.Time { return now }
	seams := &restoreSeams{}
	seams.attach(a, f)
	seed(t, a, State{DeployedDigest: digestA, FailedDigest: digestB, LastSeenDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseBack})

	err := a.Restore(context.Background())
	if err == nil || !strings.Contains(err.Error(), "undo failed") {
		t.Fatalf("Restore = %v", err)
	}
	if len(seams.recreate) != 1 || seams.recreate[0] {
		t.Fatalf("recreate = %v", seams.recreate)
	}
	if !slices.Contains(seams.calls, DriftCron) {
		t.Fatalf("calls = %v; every step is tried", seams.calls)
	}
	if st := loadState(t, a); st.Journal == nil || st.Journal.Tries != 1 {
		t.Fatalf("state = %+v; want the undo backing off", st)
	}
}

func TestRestoreWithNothingDeployedStartsOnlyWhatExists(t *testing.T) {
	f := &fakeServer{}
	a := f.agent(t)
	seams := &restoreSeams{}
	seams.attach(a, f)
	if err := a.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{DriftAccessory, DriftCron}; !slices.Equal(seams.calls, want) {
		t.Fatalf("calls = %v, want %v: an accessory may exist before the first deploy", seams.calls, want)
	}
}

func TestRestoreLeavesADeployCommandsContainers(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	a.DeployUndoes = true
	seams := &restoreSeams{}
	seams.attach(a, f)
	if err := a.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{DriftAccessory, DriftCron}; !slices.Equal(seams.calls, want) {
		t.Fatalf("calls = %v, want %v", seams.calls, want)
	}
}

func TestRestoreReRunsAnInterruptedDeployCommand(t *testing.T) {
	f := &fakeServer{tag: digestB}
	a := f.agent(t)
	a.DeployUndoes = true
	seams := &restoreSeams{}
	seams.attach(a, f)
	seed(t, a, State{DeployedDigest: digestA, LastSeenDigest: digestB}, &Journal{Digest: digestB, PreviousDigest: digestA, Phase: PhaseForward})
	if err := a.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.deployed) != 1 || f.deployed[0] != imageOf(digestB) {
		t.Fatalf("deployed = %v", f.deployed)
	}
	if st := loadState(t, a); st.Journal != nil || st.DeployedDigest != digestB {
		t.Fatalf("state = %+v", st)
	}
}

func TestRestoreTriesEveryStep(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	seams := &restoreSeams{roleErr: errors.New("proxy is down"), kindErr: map[string]error{DriftAccessory: errors.New("db will not start")}}
	seams.attach(a, f)
	err := a.Restore(context.Background())
	if err == nil || !strings.Contains(err.Error(), "proxy is down") || !strings.Contains(err.Error(), "db will not start") {
		t.Fatalf("Restore = %v", err)
	}
	if want := []string{DriftAccessory, "roles", DriftCron}; !slices.Equal(seams.calls, want) {
		t.Fatalf("calls = %v", seams.calls)
	}
}

func TestRestoreAppliesNoEditFromDisk(t *testing.T) {
	f := &fakeServer{tag: digestA}
	a, source := imageServerWith(t, f, testServerYAML)
	seams := &restoreSeams{}
	seams.attach(a, f)
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	before := loadState(t, a).Current
	var restored *config.Config
	inner := a.RestoreRoles
	a.RestoreRoles = func(ctx context.Context, cfg *config.Config, image string, stopped func(string) bool, recreate bool) error {
		restored = cfg
		return inner(ctx, cfg, image, stopped, recreate)
	}
	source.Server.Env.Clear = map[string]string{"EDITED": "yes"}

	if err := a.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restored.Env.Clear["EDITED"] != "" {
		t.Fatalf("restored with %+v; the record says what runs", restored.Env)
	}
	if st := loadState(t, a); st.Current != before {
		t.Fatalf("current = %s, want %s", st.Current, before)
	}
}

func TestRestoreAdoptsAnImageDeployedBeforeRecords(t *testing.T) {
	f := &fakeServer{}
	a := f.agent(t)
	seams := &restoreSeams{}
	seams.attach(a, f)
	if err := a.State.Save(State{DeployedDigest: digestA}); err != nil {
		t.Fatal(err)
	}
	if err := a.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seams.images) != 1 || seams.images[0] != imageOf(digestA) {
		t.Fatalf("restored %v", seams.images)
	}
	st := loadState(t, a)
	rec, err := a.records().Load(st.Current)
	if err != nil || rec == nil || rec.Origin != OriginAdopted {
		t.Fatalf("record = %+v, %v", rec, err)
	}
}

func TestRestoreWritesTheBundlesSecretsBeforeBringingRolesBack(t *testing.T) {
	e := newBundleEnv(t)
	first := e.publish(webOnApp1, "DATABASE_URL=one\n", day)
	if err := e.apply(first); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(e.envPath, []byte("DATABASE_URL=stale\n")); err != nil {
		t.Fatal(err)
	}
	var seen string
	e.agent.RestoreRoles = func(context.Context, *config.Config, string, func(string) bool, bool) error {
		seen = e.env()
		return nil
	}
	if err := e.agent.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen != "DATABASE_URL=one\n" {
		t.Fatalf("secrets when roles came back = %q", seen)
	}
}

// A role created now reads whatever the file holds, which is not known to be this
// release's. One that exists keeps the environment it was created with.
func TestRestoreCreatesNothingUntilItCanWriteTheSecretsFile(t *testing.T) {
	e := newBundleEnv(t)
	first := e.publish(webOnApp1, "DATABASE_URL=one\n", day)
	if err := e.apply(first); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(e.envPath, []byte("DATABASE_URL=stale\n")); err != nil {
		t.Fatal(err)
	}
	var recreate []bool
	e.agent.RestoreRoles = func(_ context.Context, _ *config.Config, _ string, _ func(string) bool, create bool) error {
		recreate = append(recreate, create)
		return nil
	}
	identity := e.source.Identity
	e.source.Identity = filepath.Join(t.TempDir(), "missing")

	err := e.agent.Restore(context.Background())
	if err == nil || !strings.Contains(err.Error(), "secrets file") {
		t.Fatalf("Restore = %v; it must not look restored, or the agent never tries again", err)
	}
	if !slices.Equal(recreate, []bool{false}) {
		t.Fatalf("recreate = %v; existing containers still start, and nothing is created", recreate)
	}
	if got := e.env(); got != "DATABASE_URL=stale\n" {
		t.Fatalf("secrets = %q", got)
	}

	e.source.Identity = identity
	if err := e.agent.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(recreate, []bool{false, true}) {
		t.Fatalf("recreate = %v after the identity came back", recreate)
	}
	if got := e.env(); got != "DATABASE_URL=one\n" {
		t.Fatalf("secrets = %q", got)
	}
}

func TestRunRetriesARestoreUntilItWorks(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	seams := &restoreSeams{roleErr: errors.New("podman is not up yet")}
	seams.attach(a, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	inner := a.RestoreRoles
	a.RestoreRoles = func(ctx2 context.Context, cfg *config.Config, image string, stopped func(string) bool, recreate bool) error {
		attempts++
		if attempts == 2 {
			seams.roleErr = nil
		}
		if attempts == 3 {
			cancel()
		}
		return inner(ctx2, cfg, image, stopped, recreate)
	}
	checks := 0
	resolve := a.Resolve
	a.Resolve = func(c context.Context) (string, error) {
		checks++
		if checks == 4 {
			cancel()
		}
		return resolve(c)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, time.Millisecond) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
	if attempts != 2 {
		t.Fatalf("restored %d times, want until it worked", attempts)
	}
	if checks < 2 {
		t.Fatalf("%d checks; a failed restore must not stop them", checks)
	}
}

func TestADeployEndsAnOperatorsStopOfRoles(t *testing.T) {
	f := &fakeServer{}
	a := deployedChain(t, f, digestA)
	if err := a.State.UpdateIntent(func(intent *Intent) {
		intent.Roles = []string{"web"}
		intent.Accessories = []string{"db"}
	}); err != nil {
		t.Fatal(err)
	}

	f.tag = digestB
	if outcome, err := reconcile(t, a, false); err != nil || outcome != Deployed {
		t.Fatalf("Reconcile = %s, %v", outcome, err)
	}
	intent, err := a.State.Intent()
	if err != nil || len(intent.Roles) != 0 || !slices.Equal(intent.Accessories, []string{"db"}) {
		t.Fatalf("intent = %+v, %v; a deploy starts roles, and leaves accessories alone", intent, err)
	}
}

func TestADeployThatChangedNothingKeepsAnOperatorsStop(t *testing.T) {
	f := &fakeServer{deployErrs: map[string]error{digestB: fmt.Errorf("pull failed: %w", deploy.ErrNothingSwitched)}}
	a := deployedChain(t, f, digestA)
	if err := a.State.UpdateIntent(func(intent *Intent) { intent.Roles = []string{"web"} }); err != nil {
		t.Fatal(err)
	}
	f.tag = digestB
	if outcome, _ := reconcile(t, a, false); outcome != Failed {
		t.Fatalf("Reconcile = %s", outcome)
	}
	intent, err := a.State.Intent()
	if err != nil || !slices.Equal(intent.Roles, []string{"web"}) {
		t.Fatalf("intent = %+v, %v; nothing started the role", intent, err)
	}
}

func TestAnUndoEndsAnOperatorsStopOfRoles(t *testing.T) {
	f := &fakeServer{deployErrs: map[string]error{digestB: errors.New("the worker failed")}}
	a := deployedChain(t, f, digestA)
	if err := a.State.UpdateIntent(func(intent *Intent) { intent.Roles = []string{"web"} }); err != nil {
		t.Fatal(err)
	}
	f.tag = digestB
	if outcome, _ := reconcile(t, a, false); outcome != Failed {
		t.Fatalf("Reconcile = %s", outcome)
	}
	intent, err := a.State.Intent()
	if err != nil || len(intent.Roles) != 0 {
		t.Fatalf("intent = %+v, %v; the undo that ran started the roles", intent, err)
	}
}

func TestIntentRoundTripsAndTouchesNothingWhenUnchanged(t *testing.T) {
	store := NewStore(t.TempDir(), "app")
	if err := store.UpdateIntent(func(intent *Intent) { intent.Roles = WithStarted(intent.Roles, "web") }); err != nil {
		t.Fatal(err)
	}
	if entries := dirEntries(t, store); len(entries) != 0 {
		t.Fatalf("files = %v; nothing was stopped", entries)
	}

	if err := store.UpdateIntent(func(intent *Intent) { intent.Roles = WithStopped(intent.Roles, "worker", "web", "web") }); err != nil {
		t.Fatal(err)
	}
	intent, err := store.Intent()
	if err != nil || !slices.Equal(intent.Roles, []string{"web", "worker"}) {
		t.Fatalf("intent = %+v, %v", intent, err)
	}
	if err := store.UpdateIntent(func(intent *Intent) { intent.Roles = WithStarted(intent.Roles, "web") }); err != nil {
		t.Fatal(err)
	}
	if intent, err := store.Intent(); err != nil || !slices.Equal(intent.Roles, []string{"worker"}) {
		t.Fatalf("intent = %+v, %v", intent, err)
	}
}

func TestNextSaysWhatHappens(t *testing.T) {
	now := time.Unix(1000, 0)
	back := &Journal{Target: "b-id", Digest: digestB, Previous: "a-id", PreviousDigest: digestA, Phase: PhaseBack}
	waiting := *back
	waiting.Tries, waiting.NextTry = 2, now.Add(time.Hour)
	forward := &Journal{Target: "b-id", Digest: digestB, Previous: "a-id", PreviousDigest: digestA, Phase: PhaseForward}
	first := &Journal{Target: "b-id", Digest: digestB, Phase: PhaseBack}
	cases := []struct {
		name string
		st   State
		tag  string
		undo bool
		want string
	}{
		{"nothing to do", State{DeployedDigest: digestA}, digestA, false, "nothing"},
		{"held", State{DeployedDigest: digestA, FailedDigest: digestB}, digestB, false, "until the tag moves"},
		{"new tag", State{DeployedDigest: digestA}, digestB, false, "verifies and deploys"},
		{"registry down with nothing open", State{DeployedDigest: digestA, LastSeenDigest: digestA}, "", false, "reaches the registry"},
		{"forward", State{DeployedDigest: digestA, Journal: forward}, digestB, false, "finishes it"},
		{"registry down with a journal", State{DeployedDigest: digestA, LastSeenDigest: digestB, Journal: forward}, "", false, "finishes it"},
		{"undo", State{DeployedDigest: digestA, FailedDigest: digestB, Journal: back}, digestB, false, "undoes it"},
		{"waiting", State{DeployedDigest: digestA, FailedDigest: digestB, Journal: &waiting}, digestB, false, "tried again"},
		{"cleanup", State{FailedDigest: digestB, Journal: first}, digestB, false, "stops what it started"},
		{"undo with a deploy_command", State{DeployedDigest: digestA, FailedDigest: digestB, Journal: back}, digestB, true, "owns its undo"},
		{"cleanup with a deploy_command", State{FailedDigest: digestB, Journal: first}, digestB, true, "owns its undo"},
	}
	for _, c := range cases {
		if got := Next(c.st, c.tag, now, c.undo); !strings.Contains(got, c.want) {
			t.Errorf("%s: Next = %q, want %q", c.name, got, c.want)
		}
	}
}

// dirEntries lists what the store keeps beside the state, for a test of what it touches.
func dirEntries(t *testing.T, store *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(store.releases))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
