package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/deploy"
)

// apply verifies digest, stores what it names, and runs it forward.
func (a *Agent) apply(ctx context.Context, st *State, digest string) (Outcome, string, error) {
	image := a.Image(digest)
	if a.Verify != nil {
		if err := a.Verify(ctx, image); err != nil {
			// A stop request is not a verdict on the image: leave it to be
			// verified again on the next start.
			if ctx.Err() != nil {
				return Failed, digest, fmt.Errorf("verification of %s interrupted: %w", image, ctx.Err())
			}
			return a.recordFailure(st, digest, fmt.Errorf("verification of %s failed: %w", image, err))
		}
	}

	ctx = context.WithoutCancel(ctx)
	previous, err := a.previous(st)
	if err != nil {
		return a.recordFailure(st, digest, fmt.Errorf("deploy of %s failed: %w", image, err))
	}
	candidate, err := a.Source.Load(ctx, digest, previous)
	if err != nil {
		return a.recordFailure(st, digest, fmt.Errorf("deploy of %s failed: %w", image, err))
	}
	candidate.Record.Origin = OriginVerified
	if a.Verify == nil {
		candidate.Record.Origin = OriginVerifySkipped
	}
	candidate.Record.StoredAt = a.now()
	if err := a.records().Store(candidate.Record); err != nil {
		return a.recordFailure(st, digest, fmt.Errorf("deploy of %s failed: %w", image, err))
	}
	return a.forward(ctx, st, candidate, previous, DeployOptions{})
}

// resume runs the journal's target forward again from its stored record, with no
// verification and no registry. A record that is gone can only come from the
// registry again, so outage, the reason it could not be reached if so, comes back
// instead.
func (a *Agent) resume(ctx context.Context, st *State, outage error) (Outcome, string, error) {
	j := st.Journal
	rec, err := a.records().Load(j.Target)
	if err != nil {
		return a.resumeFailed(context.WithoutCancel(ctx), st, err)
	}
	if rec == nil {
		if outage != nil {
			return Failed, j.Digest, outage
		}
		return a.apply(ctx, st, j.Digest)
	}
	ctx = context.WithoutCancel(ctx)
	cfg, _, err := rec.Config(a.Server)
	if err != nil {
		return a.resumeFailed(ctx, st, err)
	}
	var previous *Record
	if j.Previous != "" {
		if previous, err = a.records().Load(j.Previous); err != nil {
			return a.resumeFailed(ctx, st, err)
		}
	}
	a.log().Warn("Resuming the interrupted deploy of %s", a.Image(j.Digest))
	return a.forward(ctx, st, &Candidate{Record: rec, Config: cfg}, previous, DeployOptions{Local: true})
}

// resumeFailed handles a target that cannot be read back: it is undone, as a
// release that failed. It holds the deploy lock for the undo.
func (a *Agent) resumeFailed(ctx context.Context, st *State, cause error) (Outcome, string, error) {
	digest := st.Journal.Digest
	cause = fmt.Errorf("deploy of %s failed: %w", a.Image(digest), cause)
	var outcome Outcome
	var result error
	if err := a.lock(func() error {
		outcome, result = a.fail(ctx, st, nil, false, cause)
		return nil
	}); err != nil {
		return Failed, digest, fmt.Errorf("%w: failed to acquire deployment lock: %w", cause, err)
	}
	return outcome, digest, result
}

// forward runs one release under the deploy lock.
func (a *Agent) forward(ctx context.Context, st *State, candidate *Candidate, previous *Record, opts DeployOptions) (Outcome, string, error) {
	rec := candidate.Record
	var outcome Outcome
	var result error
	err := a.lock(func() error {
		outcome, result = a.forwardLocked(ctx, st, candidate, previous, opts)
		return nil
	})
	if err != nil {
		err = fmt.Errorf("deploy of %s failed: failed to acquire deployment lock: %w", a.Image(rec.Digest), err)
		if st.Journal != nil && st.Journal.Target == rec.ID {
			// A busy lock says nothing about a release already under way, and
			// the journal stays open for the next check.
			return Failed, rec.Digest, a.saved(st, err)
		}
		return a.recordFailure(st, rec.Digest, err)
	}
	return outcome, rec.Digest, result
}

// forwardLocked saves the journal, swaps what the release reads outside its
// record, and deploys. The deployer's own fleet rollback stays off throughout: it
// would restart earlier roles with this release's configuration and secrets.
func (a *Agent) forwardLocked(ctx context.Context, st *State, candidate *Candidate, previous *Record, opts DeployOptions) (Outcome, error) {
	rec := candidate.Record
	image := a.Image(rec.Digest)

	// A journal that was already open means an earlier attempt may have switched
	// roles, whatever this attempt manages to do.
	fresh := st.Journal == nil
	if err := a.openJournal(st, candidate, previous); err != nil {
		_, _, err := a.recordFailure(st, rec.Digest, fmt.Errorf("deploy of %s failed: %w", image, err))
		return Failed, err
	}
	if a.stopAbandoned(st.Journal, candidate.Config.HasRole) {
		if err := a.State.Save(*st); err != nil {
			_, _, err := a.recordFailure(st, rec.Digest, fmt.Errorf("deploy of %s failed: %w", image, err))
			return Failed, err
		}
	}

	err := a.Source.Activate(rec, previous)
	nothingSwitched := err != nil
	if err == nil {
		err = a.deploy(ctx, candidate.Config, rec, opts)
		nothingSwitched = errors.Is(err, deploy.ErrNothingSwitched)
	}
	if err != nil {
		return a.fail(ctx, st, previous, fresh && nothingSwitched, fmt.Errorf("deploy of %s failed: %w", image, err))
	}

	a.recordDeployed(st, rec, candidate.Config.Deploy.RetainHistory)
	if err := a.State.Save(*st); err != nil {
		return Deployed, err
	}
	if len(rec.Ignored) > 0 {
		a.log().Info("Kept this server's settings for %s (the release's values are ignored)", strings.Join(rec.Ignored, ", "))
	}
	for _, change := range rec.NotApplied {
		a.log().Warn("Not applied: %s", change)
	}
	return Deployed, nil
}

// openJournal saves the journal before anything changes. A journal already open
// for this target goes forward again. One open for another target is replaced,
// and its answer on what ran before carries over, since the roles it started are
// running now.
func (a *Agent) openJournal(st *State, candidate *Candidate, previous *Record) error {
	rec := candidate.Record
	if j := st.Journal; j != nil && j.Target == rec.ID {
		j.Phase, j.Tries, j.NextTry = PhaseForward, 0, time.Time{}
		return a.State.Save(*st)
	}
	first, err := a.isFirstRelease(st, candidate.Config, previous)
	if err != nil {
		return err
	}
	j := &Journal{
		Target:    rec.ID,
		Digest:    rec.Digest,
		Phase:     PhaseForward,
		First:     first,
		StartedAt: a.now(),
	}
	if previous != nil {
		j.Previous, j.PreviousDigest = previous.ID, previous.Digest
	}
	if replaced := st.Journal; replaced != nil {
		j.Abandoned = append(slices.Clone(replaced.Abandoned), replaced.Target)
	}
	st.Journal = j
	return a.State.Save(*st)
}

// isFirstRelease reports whether nothing of this service ran before this deploy.
// An unfinished deploy keeps its answer, since the roles it started are running
// now. A deploy_command never asks: it owns its undo.
func (a *Agent) isFirstRelease(st *State, cfg *config.Config, previous *Record) (bool, error) {
	switch {
	case previous != nil || a.DeployUndoes:
		return false, nil
	case st.Journal != nil:
		return st.Journal.First, nil
	}
	running, err := a.RunningRoles(cfg)
	if err != nil {
		return false, fmt.Errorf("finding the roles running before this release: %w", err)
	}
	return len(running) == 0, nil
}

// stopAbandoned stops the roles of the releases j replaced, except those keep names,
// so a role one of them added does not keep running it beside what replaced it. A
// release stays listed until that worked, so a crash or a failure tries again. Nothing
// else waits on it: the release going forward must not wait on the one it replaced.
// It reports whether the list changed.
func (a *Agent) stopAbandoned(j *Journal, keep func(role string) bool) bool {
	var remaining []string
	for _, id := range j.Abandoned {
		if !a.stopAbandonedRelease(id, keep) {
			remaining = append(remaining, id)
		}
	}
	changed := len(remaining) != len(j.Abandoned)
	j.Abandoned = remaining
	return changed
}

// stopAbandonedRelease reports whether nothing is left to do for the release. One
// that cannot be read never will be.
func (a *Agent) stopAbandonedRelease(id string, keep func(role string) bool) bool {
	failed := a.failedConfig(&Journal{Target: id, Digest: id})
	if failed == nil {
		return true
	}
	running, err := a.RunningRoles(failed)
	if err != nil {
		a.log().Warn("Cannot find roles the unfinished deploy of %s added: %v", short(id), err)
		return false
	}
	stale := slices.DeleteFunc(running, keep)
	if len(stale) == 0 {
		return true
	}
	a.log().Warn("Stopping roles the unfinished deploy of %s added: %s", short(id), strings.Join(stale, ", "))
	if err := a.StopRoles(failed, stale); err != nil {
		a.log().Warn("Cannot stop roles the unfinished deploy of %s added: %v", short(id), err)
		return false
	}
	return true
}

// fail records that the journal's target failed and undoes it. The deploy lock is
// held. When no container switched, the previous release still runs whole, so only
// the secrets file goes back: redeploying would restart every role for nothing, and
// stop a stop_first role. An undo that fails leaves the journal open with a backoff,
// so the next check tries again without the tag moving, as a full undo.
func (a *Agent) fail(ctx context.Context, st *State, previous *Record, nothingSwitched bool, cause error) (Outcome, error) {
	j := st.Journal
	st.FailedDigest = j.Digest
	st.FailedAt = a.now()
	st.LastError = cause.Error()
	if a.DeployUndoes {
		st.Journal = nil
		return Failed, a.saved(st, cause)
	}

	j.Phase = PhaseBack
	if err := a.State.Save(*st); err != nil {
		return Failed, errors.Join(cause, err)
	}
	var undoErr error
	if nothingSwitched {
		a.log().Warn("No role switched to this release, so nothing is rolled back")
		if err := a.Source.Restore(previous); err != nil {
			undoErr = fmt.Errorf("restoring the previous secrets file: %w", err)
		}
	} else {
		undoErr = a.undo(ctx, j)
	}
	if undoErr != nil {
		cause = errors.Join(cause, undoErr)
		a.backOff(st, cause)
		return Failed, a.saved(st, cause)
	}
	st.Journal = nil
	return Failed, a.saved(st, cause)
}

// undoOpen undoes the journal a check found open, under the deploy lock, backing
// off while it fails. The release stays marked failed if it was. Like a deploy, it
// runs to completion even if ctx is canceled.
func (a *Agent) undoOpen(ctx context.Context, st *State) (Outcome, string, error) {
	ctx = context.WithoutCancel(ctx)
	j := st.Journal
	err := a.lock(func() error {
		j.Phase = PhaseBack
		if err := a.State.Save(*st); err != nil {
			return err
		}
		return a.undo(ctx, j)
	})
	if err != nil {
		cause := fmt.Errorf("undoing the failed deploy of %s failed: %w", a.Image(j.Digest), err)
		a.backOff(st, cause)
		return Failed, j.Digest, a.saved(st, cause)
	}
	st.Journal = nil
	return Recovered, j.Digest, a.State.Save(*st)
}

// undo returns the server to one whole release after j's target did not finish:
// what the deploy reads outside its record, then the previous release, then
// whatever only the target started. The deploy lock is held.
func (a *Agent) undo(ctx context.Context, j *Journal) error {
	var previous *Record
	if j.Previous != "" {
		var err error
		if previous, err = a.records().Load(j.Previous); err != nil {
			return fmt.Errorf("rolling back to %s: %w", short(j.PreviousDigest), err)
		}
	}
	if err := a.Source.Restore(previous); err != nil {
		return fmt.Errorf("restoring the previous secrets file: %w", err)
	}
	failed := a.failedConfig(j)
	if previous == nil {
		return a.undoFirstRelease(j, failed)
	}

	a.log().Warn("Rolling back to release %s", short(previous.Digest))
	cfg, _, err := previous.Config(a.Server)
	if err != nil {
		return fmt.Errorf("rolling back to %s: %w", short(previous.Digest), err)
	}
	if err := a.deploy(ctx, cfg, previous, DeployOptions{Undo: true, Local: true}); err != nil {
		return fmt.Errorf("rolling back to %s: %w", short(previous.Digest), err)
	}
	a.stopAbandoned(j, cfg.HasRole)
	if failed == nil {
		return nil
	}
	running, err := a.RunningRoles(failed)
	if err != nil {
		return fmt.Errorf("finding roles the failed release added: %w", err)
	}
	added := slices.DeleteFunc(running, cfg.HasRole)
	if len(added) > 0 {
		if err := a.StopRoles(failed, added); err != nil {
			return fmt.Errorf("stopping roles the failed release added (%s): %w", strings.Join(added, ", "), err)
		}
	}
	return nil
}

// undoFirstRelease cleans up a failed release with no earlier release to return
// to. Its roles are stopped only if none of them ran before it: on a server
// already running this service by other means, stopping a role that switched
// could take the service down, so those roles keep running.
func (a *Agent) undoFirstRelease(j *Journal, failed *config.Config) error {
	if j.First {
		a.stopAbandoned(j, func(string) bool { return false })
	}
	if !j.First || failed == nil {
		a.log().Warn("Roles that switched before the failure keep this release until the next deploy")
		return nil
	}
	running, err := a.RunningRoles(failed)
	if err != nil {
		return fmt.Errorf("finding roles the failed release started: %w", err)
	}
	if len(running) > 0 {
		a.log().Warn("Stopping roles the failed first release started: %s", strings.Join(running, ", "))
		if err := a.StopRoles(failed, running); err != nil {
			return fmt.Errorf("stopping roles the failed release started (%s): %w", strings.Join(running, ", "), err)
		}
	}
	return nil
}

// failedConfig returns the configuration j's target deploys with, or nil if it
// cannot be read, which must not stop the server from recovering.
func (a *Agent) failedConfig(j *Journal) *config.Config {
	rec, err := a.records().Load(j.Target)
	if err == nil && rec == nil {
		err = errors.New("it is not stored on this server")
	}
	if err != nil {
		a.log().Warn("Cannot read failed release %s: %v", short(j.Digest), err)
		return nil
	}
	cfg, _, err := rec.Config(a.Server)
	if err != nil {
		a.log().Warn("Cannot read failed release %s: %v", short(j.Digest), err)
		return nil
	}
	return cfg
}

// deploy deploys rec with cfg. The deploy lock is held.
func (a *Agent) deploy(ctx context.Context, cfg *config.Config, rec *Record, opts DeployOptions) error {
	release := *cfg
	release.Deploy.RollbackOnFailure = false
	opts.Hooks = rec.Source == SourceImage
	return a.Deploy(ctx, &release, rec.Image, opts)
}

func (a *Agent) lock(fn func() error) error {
	if a.Lock == nil {
		return fn()
	}
	return a.Lock(fn)
}

// previous returns the record a failure returns to, storing and noting one the
// source adopts.
func (a *Agent) previous(st *State) (*Record, error) {
	var current *Record
	if st.Current != "" {
		var err error
		if current, err = a.records().Load(st.Current); err != nil {
			return nil, err
		}
	}
	rec, err := a.Source.Previous(current, st.DeployedDigest)
	if err != nil || rec == nil {
		return nil, err
	}
	if rec != current {
		rec.StoredAt = a.now()
		if err := a.records().Store(rec); err != nil {
			return nil, err
		}
		st.Current = rec.ID
		st.Applied = prependApplied(st.Applied, rec.ID, a.Server.Deploy.RetainHistory)
	}
	return rec, nil
}

// adopt records digest as deployed: every role already runs it.
func (a *Agent) adopt(st *State, digest string) error {
	rec, err := a.Source.Previous(nil, digest)
	if err != nil {
		return err
	}
	rec.StoredAt = a.now()
	if err := a.records().Store(rec); err != nil {
		return err
	}
	a.recordDeployed(st, rec, a.Server.Deploy.RetainHistory)
	return nil
}

func (a *Agent) recordDeployed(st *State, rec *Record, retain int) {
	st.DeployedDigest = rec.Digest
	st.DeployedAt = a.now()
	st.Current = rec.ID
	st.Applied = prependApplied(st.Applied, rec.ID, retain)
	st.Journal = nil
	st.FailedDigest = ""
	st.FailedAt = time.Time{}
	st.LastError = ""
}

func (a *Agent) recordFailure(st *State, digest string, cause error) (Outcome, string, error) {
	st.FailedDigest = digest
	st.FailedAt = a.now()
	st.LastError = cause.Error()
	return Failed, digest, a.saved(st, cause)
}

// backOff delays the next attempt at an undo that failed with cause.
func (a *Agent) backOff(st *State, cause error) {
	st.Journal.NextTry = NextTry(st.Journal.Tries, a.now())
	st.Journal.Tries++
	st.LastError = cause.Error()
}

// saved saves the state and returns cause, with any error saving it.
func (a *Agent) saved(st *State, cause error) error {
	if err := a.State.Save(*st); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}
