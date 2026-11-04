package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lemonity-org/azud/internal/registry"
)

// MoveOptions tune a move away from the tag.
type MoveOptions struct {
	// NoVerify deploys a digest this server has not stored without verifying it.
	NoVerify bool
	// NoPull fails if the image is not on this server.
	NoPull bool
	// Back means this returns to an earlier release, whatever the record says of it,
	// as `azud rollback` does. An older image's pre-deploy command does not run.
	Back bool
}

// moveIntent is what a journal must say about a move for a crash to finish it.
type moveIntent struct {
	hold   string
	back   bool
	noPull bool
}

func (m *moveIntent) mark(j *Journal) {
	if m == nil {
		return
	}
	j.Move, j.Hold, j.Back, j.NoPull = true, m.hold, m.back, m.noPull
}

// Previous is the default rollback target: the first of Applied that is neither current
// nor FailedDigest, so a second `azud rollback` goes further back and not forward again.
// A move leaves what it moved away from out of Applied, which keeps a third rollback
// from coming forward again too.
func Previous(st State) (string, bool) {
	for _, id := range st.Applied {
		if id != st.Current && recordDigest(id) != digestHex(st.FailedDigest) {
			return id, true
		}
	}
	return "", false
}

// Move runs a digest or stored record instead of what the tag names, for `azud rollback`
// and `azud sync --digest`. A stored record is not verified again, because it was when
// it was stored. An unknown digest is, unless NoVerify. What the tag names now becomes
// FailedDigest, so the agent holds, and the next tag move proceeds as usual. The
// downgrade check does not apply, since a shell here already has that authority. Moving
// the tag alone never gets here, so registry access still cannot bring back an old
// bundle. A journal another release left open is replaced, which ends a mixed state. Going
// back to a release still in Applied runs no pre-deploy command, and anything else goes
// forward, and does.
func (a *Agent) Move(ctx context.Context, target string, opts MoveOptions) (Outcome, string, error) {
	return a.move(ctx, opts, func(*State) (string, error) { return target, nil })
}

// Rollback moves to the release before this one, for `azud rollback` with no digest.
// With a journal open that is the release the journal left behind, unless the journal is
// itself a move, which is then what the operator already asked for.
func (a *Agent) Rollback(ctx context.Context, opts MoveOptions) (Outcome, string, error) {
	opts.Back = true
	return a.move(ctx, opts, func(st *State) (string, error) {
		if j := st.Journal; j != nil {
			switch {
			case j.Move && j.Phase == PhaseForward:
				return j.Target, nil
			case j.Previous != "":
				return j.Previous, nil
			}
			return "", fmt.Errorf("the first release, %s, has not finished, so there is nothing to go back to. To finish or undo it, run `azud sync`", short(j.Digest))
		}
		if id, ok := Previous(*st); ok {
			return id, nil
		}
		return "", a.noPrevious()
	})
}

// noPrevious says nothing earlier was applied, and names what is stored, since a server
// upgraded from v1 has stored releases and no list of what it applied.
func (a *Agent) noPrevious() error {
	const none = "this server has applied no earlier release to roll back to"
	records, err := a.records().List()
	if err != nil || len(records) == 0 {
		return errors.New(none)
	}
	ids := make([]string, 0, len(records))
	for _, rec := range records {
		ids = append(ids, rec.ID)
	}
	return fmt.Errorf("%s. To go to one it has stored, run `azud rollback <digest>`. Stored: %s", none, strings.Join(ids, ", "))
}

func (a *Agent) move(ctx context.Context, opts MoveOptions, target func(*State) (string, error)) (Outcome, string, error) {
	unlock, err := a.State.Lock()
	if err != nil {
		return Failed, "", err
	}
	defer unlock()

	st, err := a.State.Load()
	if err != nil {
		return Failed, "", err
	}
	id, err := target(&st)
	if err != nil {
		return Failed, "", err
	}
	tag := a.tagNow(ctx, &st)
	candidate, err := a.moveCandidate(ctx, &st, id, opts)
	if err != nil {
		return Failed, "", err
	}
	rec := candidate.Record
	if tag == rec.Digest {
		tag = ""
	}
	if st.Journal == nil && rec.ID == st.Current {
		a.log().Info("%s is already deployed", a.Image(rec.Digest))
		a.hold(&st, st, tag)
		return Unchanged, rec.Digest, a.State.Save(st)
	}

	ctx = context.WithoutCancel(ctx)
	previous, err := a.previous(&st)
	if err != nil {
		return Failed, rec.Digest, err
	}
	intent := &moveIntent{hold: tag, back: opts.Back || slices.Contains(st.Applied, rec.ID), noPull: opts.NoPull}
	// An interrupted move to this release keeps its answer: its pre-deploy command may
	// not have run, whoever asks to finish it.
	if j := st.Journal; j != nil && j.Move && j.Target == rec.ID {
		intent.back = j.Back
		intent.noPull = intent.noPull || j.NoPull
	}
	candidate.Move = intent
	return a.forward(ctx, &st, candidate, previous, DeployOptions{Local: true, NoPull: intent.noPull, Undo: intent.back, Move: true})
}

// tagNow returns what the tag names, or the last digest seen if the registry cannot be
// reached, since a rollback is for when it might not be.
func (a *Agent) tagNow(ctx context.Context, st *State) string {
	st.Ref = a.Ref
	tag, err := a.Resolve(ctx)
	if err == nil {
		st.LastSeenDigest = tag
		st.LastCheckAt = a.now()
		return tag
	}
	last := st.LastSeenDigest
	if last == "" {
		last = st.DeployedDigest
	}
	a.log().Warn("Cannot resolve %s, so this holds the last digest seen, %s: %v", a.Ref, short(last), err)
	return last
}

// moveCandidate returns what a move to target deploys. Nothing has changed if it fails.
func (a *Agent) moveCandidate(ctx context.Context, st *State, target string, opts MoveOptions) (*Candidate, error) {
	rec, err := a.records().Find(target, st.Applied)
	if err != nil {
		return nil, err
	}
	if rec != nil {
		if rec.Source != a.Source.Kind() {
			return nil, fmt.Errorf("stored release %s is from %s and this server follows %s, so its secrets would not be the ones the runtime file holds", rec.ID, rec.Source, a.Source.Kind())
		}
		cfg, _, err := rec.Config(a.Server)
		if err != nil {
			return nil, fmt.Errorf("stored release %s: %w", rec.ID, err)
		}
		return &Candidate{Record: rec, Config: cfg}, nil
	}
	if !registry.ValidDigest(target) {
		return nil, fmt.Errorf("no stored release is named %s", target)
	}

	origin := OriginVerifySkipped
	if a.Verify != nil && !opts.NoVerify {
		image := a.Image(target)
		if err := a.Verify(ctx, image); err != nil {
			return nil, fmt.Errorf("verification of %s failed: %w", image, err)
		}
		origin = OriginVerified
	}
	candidate, err := a.Source.Load(ctx, target, nil)
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", short(target), err)
	}
	candidate.Record.Origin = origin
	candidate.Record.StoredAt = a.now()
	if err := a.storeCandidate(candidate); err != nil {
		return nil, err
	}
	return candidate, nil
}

// Redeploy re-applies what is deployed through the transaction: from its record with a
// bundle, and with the configuration on disk with an image, which is how an edit gets
// applied. FailedDigest stays as it is, so what the tag was held at stays held, whether
// this works or not.
func (a *Agent) Redeploy(ctx context.Context) (Outcome, string, error) {
	unlock, err := a.State.Lock()
	if err != nil {
		return Failed, "", err
	}
	defer unlock()

	st, err := a.State.Load()
	if err != nil {
		return Failed, "", err
	}
	if st.Journal != nil {
		return Failed, st.Journal.Digest, fmt.Errorf("a deploy of %s has not finished, so there is no one release to apply again. To finish or undo it, run `azud sync`", short(st.Journal.Digest))
	}
	if st.DeployedDigest == "" {
		return Failed, "", errors.New("nothing is deployed on this server yet, so there is nothing to apply again")
	}

	ctx = context.WithoutCancel(ctx)
	var current *Record
	if st.Current != "" {
		if current, err = a.records().Load(st.Current); err != nil {
			return Failed, st.DeployedDigest, err
		}
	}
	candidate, err := a.Source.Reapply(current, st.DeployedDigest)
	if err != nil {
		return Failed, st.DeployedDigest, err
	}
	candidate.Record.StoredAt = a.now()
	if err := a.records().Store(candidate.Record); err != nil {
		return Failed, st.DeployedDigest, err
	}
	previous, err := a.previous(&st)
	if err != nil {
		return Failed, st.DeployedDigest, err
	}
	candidate.Move = &moveIntent{hold: st.FailedDigest}
	return a.forward(ctx, &st, candidate, previous, DeployOptions{Local: true, Move: true})
}
