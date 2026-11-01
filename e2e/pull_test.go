//go:build e2e

package e2e

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFirstDeploy(t *testing.T) {
	s := newService(t, "first")
	digest := s.release("v1")

	s.deploys()

	s.waitServes("v1")
	if got := s.agentState().DeployedDigest; got != digest {
		t.Errorf("deployed digest = %q, want %q", got, digest)
	}
	if got, want := s.inspect("{{.ImageName}}"), s.repo+"@"+digest; got != want {
		t.Errorf("container runs %q, want it pinned to %q", got, want)
	}

	before := s.inspect("{{.Id}}")
	s.deploys()
	if after := s.inspect("{{.Id}}"); after != before {
		t.Errorf("a second check with the tag unchanged replaced the container: %s to %s", before, after)
	}
}

func TestUnverifiedImageIsNotDeployed(t *testing.T) {
	s := newService(t, "unverified")
	good := s.release("v1")
	s.deploys()
	s.waitServes("v1")

	unsigned := s.publish("v2")
	s.promote(unsigned)

	if out, err := s.agentOnce(); err == nil {
		t.Fatalf("agent accepted an image that failed verify_command:\n%s", out)
	}
	s.waitServes("v1")
	st := s.agentState()
	if st.DeployedDigest != good {
		t.Errorf("deployed digest = %q, want %q", st.DeployedDigest, good)
	}
	if st.FailedDigest != unsigned {
		t.Errorf("failed digest = %q, want %q", st.FailedDigest, unsigned)
	}
}

func TestNewReleaseIsZeroDowntime(t *testing.T) {
	s := newService(t, "rolling")
	s.release("v1")
	s.deploys()
	s.waitServes("v1")

	var served, failed atomic.Int64
	var lastFailure atomic.Value
	stop := make(chan struct{})
	done := make(chan struct{})
	finish := sync.OnceFunc(func() {
		close(stop)
		<-done
	})
	t.Cleanup(finish)
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := s.serves()
			switch {
			case err != nil:
				failed.Add(1)
				lastFailure.Store(err.Error())
			case got != "v1" && got != "v2":
				failed.Add(1)
				lastFailure.Store("unexpected body " + got)
			default:
				served.Add(1)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	s.release("v2")
	s.deploys()
	s.waitServes("v2")
	finish()

	if failed.Load() != 0 {
		t.Errorf("%d of %d requests failed during the swap, last: %v", failed.Load(), failed.Load()+served.Load(), lastFailure.Load())
	}
	if served.Load() < 10 {
		t.Errorf("only %d requests ran during the swap, so this proves little", served.Load())
	}
}

func TestBadReleaseKeepsThePreviousOneServing(t *testing.T) {
	s := newService(t, "badrelease")
	good := s.release("v1")
	s.deploys()
	s.waitServes("v1")
	running := s.inspect("{{.Id}}")

	bad := s.publishBroken("v2")
	s.sign(bad)
	s.promote(bad)
	if out, err := s.agentOnce(); err == nil {
		t.Fatalf("agent reported success for a release that never became ready:\n%s", out)
	}

	s.waitServes("v1")
	if after := s.inspect("{{.Id}}"); after != running {
		t.Errorf("a release that failed readiness replaced the serving container: %s to %s", running, after)
	}
	st := s.agentState()
	if st.DeployedDigest != good || st.FailedDigest != bad {
		t.Errorf("state = %+v, want deployed %s and failed %s", st, good, bad)
	}

	// A one-shot check reports a known failed release as an error, and does not retry it.
	running = s.inspect("{{.Id}}")
	out, err := s.agentOnce()
	if err == nil || !strings.Contains(out, "failed before") {
		t.Errorf("a check on a known failed release should refuse it, got %v\n%s", err, out)
	}
	if after := s.inspect("{{.Id}}"); after != running {
		t.Errorf("a refused release replaced the serving container: %s to %s", running, after)
	}
	s.waitServes("v1")

	fixed := s.release("v3")
	s.deploys()
	s.waitServes("v3")
	st = s.agentState()
	if st.DeployedDigest != fixed || st.FailedDigest != "" {
		t.Errorf("after a fix, state = %+v, want deployed %s and nothing failed", st, fixed)
	}
	if !strings.Contains(s.inspect("{{.ImageName}}"), fixed) {
		t.Errorf("container runs %q, want %s", s.inspect("{{.ImageName}}"), fixed)
	}
}
