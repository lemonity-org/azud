package agent

import (
	"context"
	"errors"
	"fmt"
)

// Restore runs once when the agent starts, and needs no registry while the images are on
// this server. It applies no spec changes: what it creates comes from the current record.
//
// Accessories come first, because an app may need its database to become ready. Then it
// finishes or undoes an open journal, boots the proxy, starts or recreates role containers,
// and starts existing cron containers. What an operator stopped stays stopped. A
// deploy_command owns its containers and the proxy, so it touches neither of those there.
//
// Every step is tried, whatever the others do, and the errors come back together.
func (a *Agent) Restore(ctx context.Context) error {
	return a.restore(ctx, true)
}

// restore is Restore. skipBackoff means an undo that failed before may run now. Only the
// first attempt after a start asks for that, or a retry would restart containers on every
// check while the undo is backing off.
func (a *Agent) restore(ctx context.Context, skipBackoff bool) error {
	ctx = context.WithoutCancel(ctx)
	unlock, err := a.State.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	st, err := a.State.Load()
	if err != nil {
		return err
	}
	intent, err := a.State.Intent()
	if err != nil {
		return err
	}

	var errs []error
	if err := a.startExisting(DriftAccessory, intent.Accessories); err != nil {
		errs = append(errs, err)
	}
	if err := a.closeJournal(ctx, &st, skipBackoff); err != nil {
		errs = append(errs, err)
	}
	// Closing a journal deploys, and a deploy ends an operator's stop.
	if intent, err = a.State.Intent(); err != nil {
		errs = append(errs, err)
	} else if err := a.restoreRoles(ctx, &st, intent.Roles); err != nil {
		errs = append(errs, err)
	}
	if err := a.startExisting(DriftCron, nil); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (a *Agent) startExisting(kind string, stopped []string) error {
	if a.StartExisting == nil {
		return nil
	}
	if err := a.StartExisting(kind, stopped); err != nil {
		return fmt.Errorf("starting existing %s containers: %w", kind, err)
	}
	return nil
}

// closeJournal finishes or undoes an open journal without the registry. A restart skips
// an undo's backoff, since containers may be down until it finishes. If the tag last
// named a third release, which needs the registry, the journal goes on as if the tag
// named its own release.
func (a *Agent) closeJournal(ctx context.Context, st *State, skipBackoff bool) error {
	j := st.Journal
	if j == nil {
		return nil
	}
	tag := st.LastSeenDigest
	if tag == "" {
		tag = j.Digest
	}
	in := Input{Tag: tag, DeployedDigest: st.DeployedDigest, FailedDigest: st.FailedDigest, Journal: j, Now: a.now()}
	if skipBackoff && in.Now.Before(j.NextTry) {
		in.Now = j.NextTry
	}
	if Decide(in).Kind == ActionApply {
		in.Tag = j.Digest
	}

	var err error
	switch a.decide(st, in).Kind {
	case ActionResume:
		_, _, err = a.resume(ctx, st, errors.New("its record is gone, and a restore does not contact the registry"))
	case ActionUndo, ActionCleanup:
		_, _, err = a.undoOpen(ctx, st)
	default:
		err = a.State.Save(*st)
	}
	return err
}

// restoreRoles brings back the current release's role containers, under the deploy lock.
// A journal still open means the secrets file may not be this release's, so it starts
// only what exists: a container keeps the environment it was created with.
func (a *Agent) restoreRoles(ctx context.Context, st *State, stopped []string) error {
	if a.DeployUndoes {
		a.log().Info("agent.deploy_command owns this service's containers and proxy, so the agent starts none of them")
		return nil
	}
	if a.RestoreRoles == nil {
		return nil
	}
	rec, err := a.restoreRecord(st)
	if err != nil || rec == nil {
		return err
	}
	cfg, _, err := rec.Config(a.Server)
	if err != nil {
		return fmt.Errorf("stored release %s: %w", short(rec.Digest), err)
	}
	whole := st.Journal == nil
	return a.lock(func() error {
		if whole {
			if err := a.Source.Activate(rec, rec); err != nil {
				a.log().Warn("Cannot write this release's secrets file: %v", err)
			}
		}
		return a.RestoreRoles(ctx, cfg, rec.Image, stopped, whole)
	})
}

// restoreRecord returns the record of what is deployed, or nil if there is none to
// restore from. An image deployed before the agent kept records is adopted with today's
// configuration, which is what every command reads for it.
func (a *Agent) restoreRecord(st *State) (*Record, error) {
	if st.DeployedDigest == "" {
		return nil, nil
	}
	current := st.Current
	rec, err := a.previous(st)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		a.log().Warn("Release %s has no stored record, so the agent restores no role from it", short(st.DeployedDigest))
		return nil, nil
	}
	if st.Current != current {
		if err := a.State.Save(*st); err != nil {
			return nil, err
		}
	}
	return rec, nil
}
