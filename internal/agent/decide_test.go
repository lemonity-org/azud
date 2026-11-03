package agent

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	now := time.Unix(1000, 0)
	later := now.Add(time.Hour)
	// A journal applying b, with a to return to.
	forward := func() *Journal {
		return &Journal{Target: "b-id", Digest: "b", Previous: "a-id", PreviousDigest: "a", Phase: PhaseForward}
	}
	back := func(next time.Time) *Journal {
		return &Journal{Target: "b-id", Digest: "b", Previous: "a-id", PreviousDigest: "a", Phase: PhaseBack, NextTry: next}
	}
	// A journal applying b, with nothing to return to.
	firstForward := func() *Journal {
		return &Journal{Target: "b-id", Digest: "b", First: true, Phase: PhaseForward}
	}
	firstBack := func(next time.Time) *Journal {
		return &Journal{Target: "b-id", Digest: "b", First: true, Phase: PhaseBack, NextTry: next}
	}
	waitingForward := func(next time.Time) *Journal {
		j := forward()
		j.NextTry = next
		return j
	}

	cases := []struct {
		name string
		in   Input
		want ActionKind
	}{
		// No journal: the server runs one whole release.
		{"first check of a new server", Input{Tag: "a"}, ActionApply},
		{"tag names deployed", Input{Tag: "a", DeployedDigest: "a"}, ActionNone},
		{"tag names a deployed digest that failed since", Input{Tag: "a", DeployedDigest: "a", FailedDigest: "a"}, ActionNone},
		{"new tag", Input{Tag: "b", DeployedDigest: "a"}, ActionApply},
		{"failed tag", Input{Tag: "b", DeployedDigest: "a", FailedDigest: "b"}, ActionRefuse},
		{"failed tag retried", Input{Tag: "b", DeployedDigest: "a", FailedDigest: "b", Retry: true}, ActionApply},
		{"retry with nothing failed and nothing new", Input{Tag: "a", DeployedDigest: "a", Retry: true}, ActionNone},
		{"failed first release does not loop", Input{Tag: "b", FailedDigest: "b"}, ActionRefuse},
		{"failed first release retried", Input{Tag: "b", FailedDigest: "b", Retry: true}, ActionApply},
		{"rollback holds: bad tag refused, old release deployed", Input{Tag: "bad", DeployedDigest: "old", FailedDigest: "bad"}, ActionRefuse},
		{"fix published after a failure is followed", Input{Tag: "fix", DeployedDigest: "old", FailedDigest: "bad"}, ActionApply},
		{"tag moved back to what runs", Input{Tag: "old", DeployedDigest: "old", FailedDigest: "bad"}, ActionNone},
		{"republished bundle is a new digest", Input{Tag: "old2", DeployedDigest: "bad"}, ActionApply},

		// A forward journal: the agent died while applying b.
		{"crash mid forward resumes", Input{Tag: "b", DeployedDigest: "a", Journal: forward(), Now: now}, ActionResume},
		{"crash on the deployed digest resumes", Input{Tag: "a", DeployedDigest: "a", Journal: &Journal{Digest: "a", Previous: "z-id", PreviousDigest: "z", Phase: PhaseForward}, Now: now}, ActionResume},
		{"crash in a first release resumes", Input{Tag: "b", Journal: firstForward(), Now: now}, ActionResume},
		{"resume ignores an old backoff", Input{Tag: "b", DeployedDigest: "a", Journal: waitingForward(later), Now: now}, ActionResume},
		{"a failed target is undone, not resumed", Input{Tag: "b", DeployedDigest: "a", FailedDigest: "b", Journal: forward(), Now: now}, ActionUndo},
		{"a failed first release is cleaned up, not resumed", Input{Tag: "b", FailedDigest: "b", Journal: firstForward(), Now: now}, ActionCleanup},
		{"v1 pending in backoff waits", Input{Tag: "b", DeployedDigest: "a", FailedDigest: "b", Journal: waitingForward(later), Now: now}, ActionWait},
		{"retry resumes a failed forward", Input{Tag: "b", DeployedDigest: "a", FailedDigest: "b", Retry: true, Journal: forward(), Now: now}, ActionResume},
		{"retry resumes a failed forward in backoff", Input{Tag: "b", DeployedDigest: "a", FailedDigest: "b", Retry: true, Journal: waitingForward(later), Now: now}, ActionResume},
		{"tag moved back to previous finishes with an undo", Input{Tag: "a", DeployedDigest: "a", Journal: forward(), Now: now}, ActionUndo},
		{"tag moved on replaces the forward", Input{Tag: "c", DeployedDigest: "a", Journal: forward(), Now: now}, ActionApply},
		{"tag moved on in a first release replaces it", Input{Tag: "c", Journal: firstForward(), Now: now}, ActionApply},
		{"tag moved to a failed release undoes", Input{Tag: "c", DeployedDigest: "a", FailedDigest: "c", Journal: forward(), Now: now}, ActionUndo},
		{"tag moved to a failed release cleans up a first release", Input{Tag: "c", FailedDigest: "c", Journal: firstForward(), Now: now}, ActionCleanup},
		{"tag moved to a failed release, retried", Input{Tag: "c", DeployedDigest: "a", FailedDigest: "c", Retry: true, Journal: forward(), Now: now}, ActionApply},

		// A back journal: b failed and its undo did not finish. v1 stayed here.
		{"failed rollback is not refused", Input{Tag: "b", DeployedDigest: "a", FailedDigest: "b", Journal: back(now), Now: now}, ActionUndo},
		{"failed rollback backs off", Input{Tag: "b", DeployedDigest: "a", FailedDigest: "b", Journal: back(later), Now: now}, ActionWait},
		{"failed first release cleans up", Input{Tag: "b", FailedDigest: "b", Journal: firstBack(now), Now: now}, ActionCleanup},
		{"failed cleanup backs off", Input{Tag: "b", FailedDigest: "b", Journal: firstBack(later), Now: now}, ActionWait},
		{"back with the failure no longer recorded still undoes", Input{Tag: "b", DeployedDigest: "a", Journal: back(now), Now: now}, ActionUndo},
		{"new tag ends the mixed state", Input{Tag: "c", DeployedDigest: "a", FailedDigest: "b", Journal: back(later), Now: now}, ActionApply},
		{"new tag ends a mixed first release", Input{Tag: "c", FailedDigest: "b", Journal: firstBack(later), Now: now}, ActionApply},
		{"tag moved back to previous finishes the undo", Input{Tag: "a", DeployedDigest: "a", FailedDigest: "b", Journal: back(now), Now: now}, ActionUndo},
		{"tag moved back to previous still backs off", Input{Tag: "a", DeployedDigest: "a", FailedDigest: "b", Journal: back(later), Now: now}, ActionWait},
		{"retry skips the backoff when the tag moved back", Input{Tag: "a", DeployedDigest: "a", FailedDigest: "b", Retry: true, Journal: back(later), Now: now}, ActionUndo},
		{"tag moved to another failed release undoes", Input{Tag: "c", DeployedDigest: "a", FailedDigest: "c", Journal: back(now), Now: now}, ActionUndo},
		{"retry goes forward from a failed rollback", Input{Tag: "b", DeployedDigest: "a", FailedDigest: "b", Retry: true, Journal: back(later), Now: now}, ActionResume},
		{"retry goes forward from a failed cleanup", Input{Tag: "b", FailedDigest: "b", Retry: true, Journal: firstBack(later), Now: now}, ActionResume},
	}
	for _, c := range cases {
		if got := Decide(c.in).Kind; got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestDecideNamesTheDigestItConcerns(t *testing.T) {
	now := time.Unix(1000, 0)
	back := &Journal{Target: "b-id", Digest: "b", Previous: "a-id", PreviousDigest: "a", Phase: PhaseBack}
	for name, c := range map[string]struct {
		in   Input
		want string
	}{
		"apply names the tag": {Input{Tag: "c", Journal: back, Now: now}, "c"},
		"undo names the target, even when the tag moved back": {Input{Tag: "a", Journal: back, Now: now}, "b"},
		"refuse names the tag":                                {Input{Tag: "b", FailedDigest: "b"}, "b"},
	} {
		if got := Decide(c.in).Digest; got != c.want {
			t.Errorf("%s: got %s, want %s", name, got, c.want)
		}
	}
}

// FailedDigest must never strand an open journal. Whatever the state, the decision
// ends the journal, or waits for an attempt that will.
func TestDecideNeverStrandsAnOpenJournal(t *testing.T) {
	now := time.Unix(1000, 0)
	digests := []string{"", "a", "b", "c"}
	for _, tag := range []string{"a", "b", "c"} {
		for _, failed := range digests {
			for _, deployed := range digests {
				for _, phase := range []Phase{PhaseForward, PhaseBack} {
					for _, previous := range []string{"", "a"} {
						for _, next := range []time.Time{{}, now, now.Add(time.Hour)} {
							for _, retry := range []bool{false, true} {
								j := &Journal{Target: "b-id", Digest: "b", Phase: phase, NextTry: next}
								if previous != "" {
									j.Previous, j.PreviousDigest = previous+"-id", previous
								}
								in := Input{Tag: tag, DeployedDigest: deployed, FailedDigest: failed, Journal: j, Retry: retry, Now: now}
								got := Decide(in)
								switch got.Kind {
								case ActionApply, ActionResume:
									if tag == failed && !retry {
										t.Fatalf("%+v: went forward to a failed release", in)
									}
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
									t.Fatalf("%+v: %s leaves the journal open with nothing to end it", in, got.Kind)
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestNextTryDoublesToAQuarterHour(t *testing.T) {
	now := time.Unix(1000, 0)
	for tries, want := range map[int]time.Duration{
		0:  30 * time.Second,
		1:  time.Minute,
		4:  8 * time.Minute,
		5:  15 * time.Minute,
		40: 15 * time.Minute,
	} {
		if got := NextTry(tries, now); !got.Equal(now.Add(want)) {
			t.Errorf("NextTry(%d) = %s, want %s later", tries, got, want)
		}
	}
}
