// Package agent implements pull-mode deploys. The agent runs on the server,
// follows a registry tag, and deploys each new digest the tag points to on
// the server itself, so CI only builds and publishes images and never needs
// access to the server.
package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/output"
)

// Outcome describes what one check did.
type Outcome int

const (
	// Unchanged means the tag still points to the deployed digest.
	Unchanged Outcome = iota
	// Adopted means the new digest was already running (for example after a
	// pushed deploy) and was recorded without deploying again.
	Adopted
	// Deployed means the new digest was verified and deployed.
	Deployed
	// SkippedFailed means the tag points to a digest that failed before.
	SkippedFailed
	// Failed means verification or deployment of a new digest failed.
	Failed
	// Recovered means an interrupted deploy of a failed release was undone:
	// the server runs one whole release again, without the failed one.
	Recovered
	// Waiting means undoing an interrupted deploy failed and is backing off.
	Waiting
)

func (o Outcome) String() string {
	switch o {
	case Unchanged:
		return "unchanged"
	case Adopted:
		return "adopted"
	case Deployed:
		return "deployed"
	case SkippedFailed:
		return "skipped-failed"
	case Failed:
		return "failed"
	case Recovered:
		return "recovered"
	case Waiting:
		return "waiting"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// DeployOptions tune one deploy.
type DeployOptions struct {
	// Undo means this deploy returns to an earlier release after a later one
	// failed, so the release's pre-deploy command does not run.
	Undo bool
	// Local means use the image if it is already on this server, so the deploy
	// needs no registry.
	Local bool
	// Hooks means run hooks. They never run for a bundle: they are host commands
	// outside what its signature covers.
	Hooks bool
}

// Agent checks one tag and deploys what it points to. The functions are the
// seams to the registry, Podman, and the deployer, so the decision logic can
// be tested on its own.
type Agent struct {
	// Ref is the followed tag as repository:tag, for logs and state.
	Ref string
	// Repository is the image name without tag, used to pin deploys by digest.
	Repository string

	// Resolve returns the digest the tag points to now.
	Resolve func(ctx context.Context) (string, error)
	// Running returns the digest every role runs now, or "" if unknown or
	// mixed. Optional.
	Running func(ctx context.Context) (string, error)
	// Verify must succeed before image is deployed. Nil skips verification,
	// which configuration only allows with insecure_skip_verify.
	Verify func(ctx context.Context, image string) error
	// Source produces the candidate for a digest.
	Source Source
	// Server is the configuration the agent runs with, and the trust anchor for
	// every release it deploys from a record.
	Server *config.Config

	// Deploy deploys image, which is pinned by digest, with cfg. The deploy lock
	// is already held.
	Deploy func(ctx context.Context, cfg *config.Config, image string, opts DeployOptions) error
	// DeployUndoes means Deploy runs agent.deploy_command, which owns its own
	// undo, so a failure closes the journal without one.
	DeployUndoes bool
	// Lock runs fn holding the host's deploy lock, the one pushed deploys take,
	// so a pushed deploy never overlaps a release. Nil runs fn without a lock.
	Lock func(fn func() error) error
	// StopRoles stops the role containers of cfg named by roles.
	StopRoles func(cfg *config.Config, roles []string) error
	// RunningRoles returns the roles of cfg with a running container.
	RunningRoles func(cfg *config.Config) ([]string, error)

	State *Store
	Log   *output.Logger
	Now   func() time.Time
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) log() *output.Logger {
	if a.Log != nil {
		return a.Log
	}
	return output.DefaultLogger
}

// Image returns the image reference the agent deploys for digest.
func (a *Agent) Image(digest string) string {
	return a.Repository + "@" + digest
}

func (a *Agent) records() *Records {
	return a.State.Records(a.Repository)
}

// Reconcile checks the tag once and acts on what Decide returns. A digest that
// failed before is skipped unless retryFailed is set.
//
// It holds the state lock throughout, so nothing else changes the state between
// the decision and the action. Once verification passes, a deploy or undo runs to
// completion even if ctx is canceled: stopping halfway would leave the server
// between releases.
func (a *Agent) Reconcile(ctx context.Context, retryFailed bool) (Outcome, string, error) {
	unlock, err := a.State.Lock()
	if err != nil {
		return Failed, "", err
	}
	defer unlock()

	st, err := a.State.Load()
	if err != nil {
		return Failed, "", err
	}

	tag, resolveErr := a.Resolve(ctx)
	if resolveErr != nil {
		resolveErr = fmt.Errorf("failed to resolve %s: %w", a.Ref, resolveErr)
		// Undoing a release must not depend on the registry that served it.
		if st.Journal == nil || ctx.Err() != nil {
			return Failed, "", resolveErr
		}
		tag = st.LastSeenDigest
		if tag == "" {
			tag = st.Journal.Digest
		}
		a.log().Warn("%v; acting on the last digest seen, %s", resolveErr, short(tag))
	} else {
		st.LastSeenDigest = tag
	}
	st.Ref = a.Ref
	st.LastCheckAt = a.now()

	in := Input{Tag: tag, DeployedDigest: st.DeployedDigest, FailedDigest: st.FailedDigest, Journal: st.Journal, Retry: retryFailed, Now: a.now()}
	action := Decide(in)
	if a.DeployUndoes && (action.Kind == ActionUndo || action.Kind == ActionCleanup) {
		// The agent has no undo to run, and the command was the one to run it.
		a.log().Warn("Closing the interrupted deploy of %s: agent.deploy_command owns its undo", a.Image(action.Digest))
		st.Journal, in.Journal = nil, nil
		action = Decide(in)
	}

	switch action.Kind {
	case ActionNone:
		return Unchanged, tag, a.State.Save(st)
	case ActionWait:
		return Waiting, action.Digest, a.State.Save(st)
	case ActionResume:
		return a.resume(ctx, &st, resolveErr)
	case ActionUndo, ActionCleanup:
		return a.undoOpen(ctx, &st)
	}

	// Checked before skipping a failed digest: a pushed deploy may have put
	// that release live since the agent's own attempt failed. An open journal
	// is not asked: the running digest may be the failed release.
	if st.Journal == nil && a.Running != nil {
		running, err := a.Running(ctx)
		if err != nil {
			a.log().Debug("Could not read the running image: %v", err)
		} else if running == tag {
			if err := a.adopt(&st, tag); err != nil {
				return Failed, tag, err
			}
			return Adopted, tag, a.State.Save(st)
		}
	}

	if action.Kind == ActionRefuse {
		return SkippedFailed, tag, a.State.Save(st)
	}
	if resolveErr != nil {
		// A release is verified and fetched from the registry, so an outage
		// says nothing about it.
		return Failed, tag, resolveErr
	}
	return a.apply(ctx, &st, tag)
}

// Run checks the tag every interval until ctx is canceled. Failed checks are
// logged and retried on the next tick; they never stop the agent.
func (a *Agent) Run(ctx context.Context, interval time.Duration) error {
	log := a.log()
	log.Info("Following %s every %s", a.Ref, interval)

	skippedDigest := ""
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("Agent stopped")
			return nil
		case <-timer.C:
		}

		outcome, digest, err := a.Reconcile(ctx, false)
		switch outcome {
		case Unchanged:
			log.Debug("%s still points to %s", a.Ref, digest)
		case Adopted:
			log.Info("%s is already running; recorded it as deployed", a.Image(digest))
		case Deployed:
			log.Success("Deployed %s", a.Image(digest))
		case SkippedFailed:
			if skippedDigest != digest {
				log.Warn("%s failed before; waiting for %s to move (retry with: azud agent run --once --retry-failed)", a.Image(digest), a.Ref)
			}
		case Failed:
			if err != nil {
				log.Error("%v", err)
			}
		case Recovered:
			log.Warn("Undid the failed deploy of %s; waiting for %s to move (retry with: azud agent run --once --retry-failed)", a.Image(digest), a.Ref)
		case Waiting:
			log.Debug("Waiting to retry undoing the failed deploy of %s", a.Image(digest))
		}
		if outcome == SkippedFailed || outcome == Recovered || outcome == Failed && digest != "" {
			skippedDigest = digest
		} else {
			skippedDigest = ""
		}

		timer.Reset(interval)
	}
}
