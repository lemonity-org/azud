package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestCommandDeployerPassesImageThroughEnvironment(t *testing.T) {
	d := &CommandDeployer{
		Command: `test "$AZUD_AGENT_IMAGE" = "ghcr.io/o/a@` + digestA + `" && test "$AZUD_AGENT_DIGEST" = "` + digestA + `" && test "$AZUD_SERVICE" = app`,
		Env:     []string{"AZUD_SERVICE=app"},
	}
	if err := d.Deploy(context.Background(), "ghcr.io/o/a@"+digestA); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
}

func TestCommandDeployerStreamsOutputAndReportsItsTail(t *testing.T) {
	var live bytes.Buffer
	d := &CommandDeployer{
		Command: `for i in $(seq 1 100); do echo "step $i"; done; echo "migration failed" >&2; exit 4`,
		Output:  &live,
	}
	err := d.Deploy(context.Background(), "ghcr.io/o/a@"+digestA)
	if err == nil || !strings.Contains(err.Error(), "migration failed") {
		t.Fatalf("Deploy = %v; want the command's last output in the error", err)
	}
	if strings.Contains(err.Error(), "step 1\n") {
		t.Fatalf("Deploy error repeats the whole output: %v", err)
	}
	if !strings.Contains(live.String(), "step 1\n") || !strings.Contains(live.String(), "migration failed") {
		t.Fatalf("live output = %q; want every line streamed", live.String())
	}
}

func TestCommandDeployerAsksATimedOutDeployToStop(t *testing.T) {
	// The trap stands in for a deploy script's rollback: SIGTERM must reach
	// it before anything is killed.
	var live bytes.Buffer
	d := &CommandDeployer{
		Command: `trap 'kill $! 2>/dev/null; echo "rolled back"; exit 3' TERM; sleep 30 & wait`,
		Timeout: 300 * time.Millisecond,
		Output:  &live,
	}
	start := time.Now()
	err := d.Deploy(context.Background(), "ghcr.io/o/a@"+digestA)
	if err == nil || !strings.Contains(err.Error(), "timed out after 300ms") {
		t.Fatalf("Deploy = %v; want a timeout", err)
	}
	if !strings.Contains(live.String(), "rolled back") {
		t.Fatalf("output = %q; the deploy did not get SIGTERM", live.String())
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("deploy timeout was not enforced")
	}
}

func TestCommandDeployerFinishesWhenTheAgentIsStopped(t *testing.T) {
	// Reconcile hands Deploy an uncancelable context; only the timeout ends it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &CommandDeployer{Command: `sleep 0.2; echo done`}
	var live bytes.Buffer
	d.Output = &live
	if err := d.Deploy(context.WithoutCancel(ctx), "ghcr.io/o/a@"+digestA); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !strings.Contains(live.String(), "done") {
		t.Fatalf("output = %q", live.String())
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	tail := &tailBuffer{max: 8}
	_, _ = tail.Write([]byte("0123456789"))
	_, _ = tail.Write([]byte("ab"))
	if got := tail.String(); got != "456789ab" {
		t.Fatalf("tail = %q, want the last 8 bytes", got)
	}
}
