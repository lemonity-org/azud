package agent

import (
	"fmt"
	"time"
)

// Input is everything a decision depends on.
type Input struct {
	// Tag is what the tag names now, or the last digest seen when the registry
	// cannot be reached and a journal is open.
	Tag            string
	DeployedDigest string
	FailedDigest   string
	Journal        *Journal
	// Retry is an operator asking for a failed release to be tried again.
	Retry bool
	Now   time.Time
}

// ActionKind is what to do next.
type ActionKind int

const (
	// ActionNone means the tag names what is deployed.
	ActionNone ActionKind = iota
	// ActionRefuse means the tag names a failed release and no retry was asked for.
	ActionRefuse
	// ActionWait means an undo failed and is backing off.
	ActionWait
	// ActionApply means verify the release the tag names and run the transaction.
	ActionApply
	// ActionResume means re-run the journal's target from its stored record.
	ActionResume
	// ActionUndo means return to the journal's previous release.
	ActionUndo
	// ActionCleanup means there is no previous release: stop what the target
	// started, restore the secrets file, and close the journal.
	ActionCleanup
)

func (k ActionKind) String() string {
	switch k {
	case ActionNone:
		return "none"
	case ActionRefuse:
		return "refuse"
	case ActionWait:
		return "wait"
	case ActionApply:
		return "apply"
	case ActionResume:
		return "resume"
	case ActionUndo:
		return "undo"
	case ActionCleanup:
		return "cleanup"
	default:
		return fmt.Sprintf("ActionKind(%d)", int(k))
	}
}

// Action is the decision: its kind, and the digest it concerns.
type Action struct {
	Kind   ActionKind
	Digest string
}

// Decide returns the next action. It has no I/O, so every state is a table test.
//
// An open journal comes first, because the server may be running parts of two
// releases. FailedDigest only ever stops a release from going forward: with a
// journal open, the result is still one that ends the journal, or a wait for the
// next attempt at it, so a failed digest cannot strand a mixed server.
func Decide(in Input) Action {
	refused := in.Tag == in.FailedDigest && !in.Retry

	j := in.Journal
	if j == nil {
		switch {
		case in.Tag == in.DeployedDigest:
			return Action{ActionNone, in.Tag}
		case refused:
			return Action{ActionRefuse, in.Tag}
		}
		return Action{ActionApply, in.Tag}
	}

	switch {
	case in.Tag != j.Digest && in.Tag != j.PreviousDigest && !refused:
		// A different release ends the mixed state as a whole.
		return Action{ActionApply, in.Tag}
	case in.Tag == j.Digest && !refused && (j.Phase == PhaseForward || in.Retry):
		return Action{ActionResume, j.Digest}
	case !in.Retry && in.Now.Before(j.NextTry):
		return Action{ActionWait, j.Digest}
	case j.Previous == "":
		return Action{ActionCleanup, j.Digest}
	}
	return Action{ActionUndo, j.Digest}
}

// NextTry returns when an undo that has failed tries times before this failure
// may run again.
func NextTry(tries int, now time.Time) time.Time {
	return now.Add(recoverDelay(tries))
}

// recoverDelay is how long to wait after a failed undo: 30s, doubling to 15m. An
// undo restarts containers, so it should not run on every check.
func recoverDelay(tries int) time.Duration {
	delay := 30 * time.Second
	for i := 0; i < tries && delay < 15*time.Minute; i++ {
		delay *= 2
	}
	return min(delay, 15*time.Minute)
}
