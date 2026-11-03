//go:build security

package jit

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	deployedDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	agentSection   = "agent:\n  track: production\n  insecure_skip_verify: true\n"
)

var specLabelInCommand = regexp.MustCompile(`azud\.spec=([0-9a-f]+)`)

const noSuchCronContainer = `Error: no container with name or ID "shop-cron-backup" found: no such container`

// noCronContainer fakes `azud cron boot` on a server with no scheduled container.
func noCronContainer(command string, _ io.Reader) fakeResult {
	if strings.Contains(command, "container inspect shop-cron-backup") {
		return fakeResult{Stderr: noSuchCronContainer, Exit: 125}
	}
	return fakeResult{}
}

// runningCronHandler fakes `azud cron boot` with a scheduled container running. It
// carries spec as its azud.spec label, or no label when spec is empty, until a
// `podman rm` of it.
func runningCronHandler(spec string) fakeHandler {
	var mu sync.Mutex
	exists := true
	labels := `"azud.cron":"backup","azud.managed":"true"`
	if spec != "" {
		labels += `,"azud.spec":"` + spec + `"`
	}
	return func(command string, stdin io.Reader) fakeResult {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(command, "podman rm") && strings.Contains(command, "shop-cron-backup"):
			exists = false
			return fakeResult{}
		case strings.Contains(command, "container inspect shop-cron-backup"):
			if !exists {
				return fakeResult{Stderr: noSuchCronContainer, Exit: 125}
			}
			if strings.Contains(command, "State.Running") {
				return fakeResult{Stdout: "true\n"}
			}
			return fakeResult{Stdout: `[{"Id":"abc","Config":{"Labels":{` + labels + `}},"Mounts":[],"State":{"Running":true}}]`}
		default:
			return fakeResult{}
		}
	}
}

// bootCron runs `azud cron boot` against handler, on a server whose agent, if it has
// one, deployed deployedDigest. It returns the result and the fake.
func bootCron(t *testing.T, section string, handler fakeHandler) (cliResult, *fakeSSH) {
	t.Helper()
	fake := startFakeSSH(t, "127.0.0.1:0", handler)
	dir := newWorkdir(t)
	stateDir := filepath.Join(dir, "state")
	writeFile(t, filepath.Join(stateDir, "agent", "shop.json"), `{"ref":"example/shop:production","deployed_digest":"`+deployedDigest+`"}`, 0o600)
	config := baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop") + `
cron:
  backup:
    schedule: "0 2 * * *"
    command: bin/backup
` + section
	writeConfig(t, dir, config)
	return runAzud(t, dir, []string{"AZUD_STATE_DIR=" + stateDir}, "cron", "boot"), fake
}

func TestControl_CronBootRunsTheDeployedDigestWithItsSpec(t *testing.T) {
	res, fake := bootCron(t, agentSection, noCronContainer)
	if res.Exit != 0 {
		t.Fatalf("cron boot failed: %s", res)
	}
	run := fake.waitCommand(t, "--name shop-cron-backup ", 5*time.Second)
	if !strings.Contains(run.Command, "example/shop@"+deployedDigest) {
		t.Errorf("the job does not run the deployed digest:\n%s", run.Command)
	}
	if strings.Contains(run.Command, "example/shop:v1") {
		t.Errorf("the job runs the configured tag:\n%s", run.Command)
	}
	if !specLabelInCommand.MatchString(run.Command) {
		t.Errorf("the job has no azud.spec label:\n%s", run.Command)
	}
}

func TestControl_CronBootRecreatesAJobWhoseSpecDiffers(t *testing.T) {
	for name, spec := range map[string]string{"another spec": "0123", "no label, as an earlier Azud made": ""} {
		t.Run(name, func(t *testing.T) {
			res, fake := bootCron(t, agentSection, runningCronHandler(spec))
			if res.Exit != 0 {
				t.Fatalf("cron boot failed: %s", res)
			}
			if strings.Contains(res.Combined, "already running") {
				t.Fatalf("cron boot kept a job that differs: %s", res)
			}
			records := fake.snapshot()
			check := commandIndex(records, func(command string) bool { return strings.Contains(command, "--name shop-cron-backup-runtime-check-") })
			remove := commandIndex(records, func(command string) bool { return strings.Contains(command, "podman rm -f shop-cron-backup") })
			run := commandIndex(records, func(command string) bool { return strings.Contains(command, "--name shop-cron-backup ") })
			if check < 0 || remove < check || run < remove {
				t.Fatalf("the job was not replaced in order, after the new image was checked (check=%d rm=%d run=%d):\n%s", check, remove, run, commandDump(records))
			}
		})
	}
}

func TestControl_CronBootLeavesAJobThatMatches(t *testing.T) {
	res, fake := bootCron(t, agentSection, noCronContainer)
	if res.Exit != 0 {
		t.Fatalf("cron boot failed: %s", res)
	}
	match := specLabelInCommand.FindStringSubmatch(fake.waitCommand(t, "--name shop-cron-backup ", 5*time.Second).Command)
	if match == nil {
		t.Fatal("the job has no azud.spec label")
	}

	res, fake = bootCron(t, agentSection, runningCronHandler(match[1]))
	if res.Exit != 0 {
		t.Fatalf("cron boot failed: %s", res)
	}
	if !strings.Contains(res.Combined, "already running") {
		t.Errorf("cron boot did not leave a job that matches: %s", res)
	}
	if got := fake.commandsMatching("podman rm"); len(got) != 0 {
		t.Errorf("cron boot removed a job that matches:\n%s", commandDump(fake.snapshot()))
	}
}

// A configuration with no agent section behaves as it did: no pin, no label, and a
// running job stays whatever its labels say.
func TestControl_CronBootWithNoAgentSectionIsUnchanged(t *testing.T) {
	res, fake := bootCron(t, "", runningCronHandler("0123"))
	if res.Exit != 0 {
		t.Fatalf("cron boot failed: %s", res)
	}
	if !strings.Contains(res.Combined, "already running") {
		t.Errorf("cron boot replaced a running job: %s", res)
	}
	if got := fake.commandsMatching("podman rm"); len(got) != 0 {
		t.Errorf("cron boot removed a running job:\n%s", commandDump(fake.snapshot()))
	}

	res, fake = bootCron(t, "", noCronContainer)
	if res.Exit != 0 {
		t.Fatalf("cron boot failed: %s", res)
	}
	run := fake.waitCommand(t, "--name shop-cron-backup ", 5*time.Second)
	if !strings.Contains(run.Command, "example/shop:v1") {
		t.Errorf("the job does not run the configured image:\n%s", run.Command)
	}
	if specLabelInCommand.MatchString(run.Command) {
		t.Errorf("the job gained a label:\n%s", run.Command)
	}
}

// Nothing is sent to the server for a command the agent's state refuses.
func TestControl_CronBootRefusedWhileAJournalIsOpenTouchesNothing(t *testing.T) {
	fake := startFakeSSH(t, "127.0.0.1:0", permissive())
	dir := newWorkdir(t)
	stateDir := filepath.Join(dir, "state")
	writeFile(t, filepath.Join(stateDir, "agent", "shop.json"), `{"deployed_digest":"`+deployedDigest+`","journal":{"target":"2222","digest":"sha256:2222","phase":"forward"}}`, 0o600)
	writeConfig(t, dir, baseConfig("127.0.0.1", fake.port(), clientKeyPath(t), "shop")+`
cron:
  backup:
    schedule: "0 2 * * *"
    command: bin/backup
`+agentSection)

	res := runAzud(t, dir, []string{"AZUD_STATE_DIR=" + stateDir}, "cron", "boot")
	if res.Exit == 0 {
		t.Fatalf("cron boot ran with a journal open: %s", res)
	}
	for _, want := range []string{"sha256:2222", "azud agent run --once"} {
		if !strings.Contains(res.Combined, want) {
			t.Errorf("the refusal does not name %q: %s", want, res)
		}
	}
	if got := fake.snapshot(); len(got) != 0 {
		t.Errorf("a refused command reached the server:\n%s", commandDump(got))
	}
	if _, err := os.Stat(filepath.Join(stateDir, "agent", "shop.json.lock")); !os.IsNotExist(err) {
		t.Errorf("reading the state took its lock: %v", err)
	}
}
