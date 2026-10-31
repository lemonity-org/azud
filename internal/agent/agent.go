// Package agent implements pull-mode deploys. The agent runs on the server,
// follows a registry tag, and deploys each new digest the tag points to on
// the server itself, so CI only builds and publishes images and never needs
// access to the server.
package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
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
	// Deploy deploys image, which is pinned by digest.
	Deploy func(ctx context.Context, image string) error
	// Interrupted reports that a deploy started and did not finish, so the
	// recorded digest may not be what runs. The agent then deploys even an
	// unchanged digest. Optional.
	Interrupted func() bool

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

// Reconcile checks the tag once and deploys a new digest. A digest that
// failed before is skipped unless retryFailed is set.
//
// Once verification passes, the deploy runs to completion even if ctx is
// canceled: stopping halfway would leave the server between releases.
func (a *Agent) Reconcile(ctx context.Context, retryFailed bool) (Outcome, string, error) {
	unlock, err := a.State.Lock()
	if err != nil {
		return Failed, "", err
	}
	defer unlock()

	digest, err := a.Resolve(ctx)
	if err != nil {
		return Failed, "", fmt.Errorf("failed to resolve %s: %w", a.Ref, err)
	}

	st, err := a.State.Load()
	if err != nil {
		return Failed, digest, err
	}
	st.Ref = a.Ref
	st.LastSeenDigest = digest
	st.LastCheckAt = a.now()

	interrupted := a.Interrupted != nil && a.Interrupted()
	if st.DeployedDigest == digest && !interrupted {
		return Unchanged, digest, a.State.Save(st)
	}

	// Checked before skipping a failed digest: a pushed deploy may have put
	// that release live since the agent's own attempt failed.
	if a.Running != nil {
		running, err := a.Running(ctx)
		if err != nil {
			a.log().Debug("Could not read the running image: %v", err)
		} else if running == digest {
			a.recordDeployed(&st, digest)
			return Adopted, digest, a.State.Save(st)
		}
	}

	if st.FailedDigest == digest && !retryFailed {
		return SkippedFailed, digest, a.State.Save(st)
	}

	image := a.Image(digest)
	if a.Verify != nil {
		if err := a.Verify(ctx, image); err != nil {
			// A stop request is not a verdict on the image: leave it to be
			// verified again on the next start.
			if ctx.Err() != nil {
				return Failed, digest, fmt.Errorf("verification of %s interrupted: %w", image, ctx.Err())
			}
			return a.recordFailure(&st, digest, fmt.Errorf("verification of %s failed: %w", image, err))
		}
	}

	if err := a.Deploy(context.WithoutCancel(ctx), image); err != nil {
		return a.recordFailure(&st, digest, fmt.Errorf("deploy of %s failed: %w", image, err))
	}

	a.recordDeployed(&st, digest)
	return Deployed, digest, a.State.Save(st)
}

func (a *Agent) recordDeployed(st *State, digest string) {
	st.DeployedDigest = digest
	st.DeployedAt = a.now()
	st.FailedDigest = ""
	st.FailedAt = time.Time{}
	st.LastError = ""
}

func (a *Agent) recordFailure(st *State, digest string, cause error) (Outcome, string, error) {
	st.FailedDigest = digest
	st.FailedAt = a.now()
	st.LastError = cause.Error()
	if err := a.State.Save(*st); err != nil {
		return Failed, digest, errors.Join(cause, err)
	}
	return Failed, digest, cause
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
		}
		if outcome == SkippedFailed || outcome == Failed && digest != "" {
			skippedDigest = digest
		} else {
			skippedDigest = ""
		}

		timer.Reset(interval)
	}
}
