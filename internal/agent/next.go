package agent

import (
	"fmt"
	"time"
)

// Next says what the agent does next, in the words `azud status` shows. tag is what the
// tag names, or "" if the registry could not be reached, which leaves a release going
// forward waiting for it and ends an open journal from the last digest seen.
func Next(st State, tag string, now time.Time, deployUndoes bool) string {
	known := tag != ""
	if !known {
		tag = st.LastSeenDigest
	}
	j := st.Journal
	if j == nil && !known {
		return "the next check that reaches the registry reads the tag"
	}
	if j != nil && tag == "" {
		tag = j.Digest
	}

	action := Decide(Input{Tag: tag, DeployedDigest: st.DeployedDigest, FailedDigest: st.FailedDigest, Journal: j, Now: now})
	switch action.Kind {
	case ActionNone:
		return "nothing: the tag names what is deployed"
	case ActionRefuse:
		return fmt.Sprintf("nothing until the tag moves: it names %s, which is held. azud sync --retry tries it", short(tag))
	case ActionApply:
		if !known {
			return fmt.Sprintf("the next check that reaches the registry deploys %s", short(tag))
		}
		return fmt.Sprintf("the next check verifies and deploys %s. azud sync does it now", short(tag))
	case ActionWait:
		return fmt.Sprintf("undoing %s failed %d time(s), and is tried again at %s. azud rollback tries now, and azud sync --retry goes forward instead", short(j.Digest), j.Tries, j.NextTry.Local().Format(time.RFC3339))
	case ActionResume:
		return "the next check finishes it, with no registry. azud sync does it now"
	case ActionUndo:
		if deployUndoes {
			return "the next check closes it, because agent.deploy_command owns its undo"
		}
		return fmt.Sprintf("the next check undoes it and returns to %s, with no registry. azud sync does it now", short(j.PreviousDigest))
	case ActionCleanup:
		if deployUndoes {
			return "the next check closes it, because agent.deploy_command owns its undo"
		}
		return "the next check stops what it started, since nothing ran before it. azud sync does it now"
	}
	return ""
}
