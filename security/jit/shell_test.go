//go:build security

package jit

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/shell"
)

// ---------------------------------------------------------------------------
// Control: shell.Quote must preserve arbitrary bytes when evaluated by sh.
// ---------------------------------------------------------------------------

func TestControl_ShellQuoteRoundTrip(t *testing.T) {
	payloads := []string{
		"",
		"simple",
		"a b",
		"it's",
		`"double"`,
		`back\slash`,
		"line\nbreak",
		"tab\there",
		"carriage\rreturn",
		"$(touch CANARY)",
		"`touch CANARY`",
		"${HOME}",
		"$HOME",
		"~root",
		"*",
		"?",
		"[a]",
		"{a,b}",
		"a; touch CANARY",
		"a && touch CANARY",
		"a || touch CANARY",
		"a | touch CANARY",
		"a > CANARY",
		"a < CANARY",
		"a & touch CANARY",
		"a\n; touch CANARY",
		"a\r; touch CANARY",
		"!",
		"#comment",
		"a=b",
		"-n",
		"--",
		"%s",
		"émoji-🙂",
		"\u2028line-separator",
		strings.Repeat("x", 4096) + "; touch CANARY",
	}

	for _, payload := range payloads {
		payload := payload
		t.Run(fmt.Sprintf("%x", []byte(payload)[:min(8, len(payload))]), func(t *testing.T) {
			dir := t.TempDir()
			canary := filepath.Join(dir, "CANARY")
			// Replace the literal canary token with an absolute path so any
			// accidental execution would be observable in this directory.
			payload = strings.ReplaceAll(payload, "CANARY", canary)
			quoted := shell.Quote(payload)

			script := "printf '%s' " + quoted
			cmd := exec.Command("sh", "-c", script)
			cmd.Dir = dir
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("sh failed for payload %q (quoted %q): %v", payload, quoted, err)
			}
			if got := string(out); got != payload {
				t.Fatalf("round trip mismatch:\nwant %q\ngot  %q\nquoted %q", payload, got, quoted)
			}
			if _, err := os.Stat(canary); err == nil {
				t.Fatalf("payload executed despite quoting: %q -> %q", payload, quoted)
			}
		})
	}
}

func TestControl_ShellJoinAndRemotePathAreDataSafe(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "CANARY")
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}

	// A documented $HOME prefix may expand, but its suffix must stay data.
	path := "$HOME/azud/$(touch " + canary + ")/it's"
	script := "printf '%s' " + shell.QuoteRemotePath(path)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	want := home + "/azud/$(touch " + canary + ")/it's"
	if got := string(out); got != want {
		t.Fatalf("QuoteRemotePath expanded incorrectly:\nwant %q\ngot  %q", want, got)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("command substitution executed through QuoteRemotePath")
	}

	joined := shell.Join("echo", "a b", "$(touch "+canary+")")
	joinOut, exit := runShell(t, dir, joined)
	if exit != 0 || strings.TrimSpace(joinOut) != "a b $(touch "+canary+")" {
		t.Fatalf("Join output = %q exit=%d", joinOut, exit)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("command substitution executed through Join")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Control: every podman command builder emits each user value as one argv.
// ---------------------------------------------------------------------------

// installStubPodman writes a podman stub that emits argv NUL-separated.
func installStubPodman(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done\nexit 0\n"
	path := filepath.Join(dir, "podman")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// podmanArgv executes a generated podman command under sh with the stub on
// PATH and returns the argv the stub observed.
func podmanArgv(t *testing.T, dir, command string) [][]byte {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+installStubPodman(t)+":"+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run generated command %q: %v", command, err)
	}
	parts := bytes.Split(out, []byte{0})
	args := make([][]byte, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		args = append(args, part)
	}
	return args
}

func argvContains(args [][]byte, want string) bool {
	for _, arg := range args {
		if string(arg) == want {
			return true
		}
	}
	return false
}

func TestControl_ContainerCommandBuilderKeepsPayloadsAsArguments(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "CANARY")
	payload := "$(touch " + canary + ");`touch " + canary + "`'\"\\;|&"

	cfg := &podman.ContainerConfig{
		Name:            "svc" + payload,
		Image:           "registry.example/app" + payload + ":v1",
		Command:         []string{"sh", "-c", "echo " + payload},
		Entrypoint:      "/bin/sh" + payload,
		Env:             map[string]string{"A" + payload: "V" + payload},
		Ports:           []string{"127.0.0.1:8080:80" + payload},
		Volumes:         []string{"/tmp/data:/data" + payload},
		Labels:          map[string]string{"l" + payload: "v" + payload},
		Network:         "azud" + payload,
		NetworkAliases:  []string{"alias" + payload},
		Memory:          "512m" + payload,
		CPUs:            "0.5" + payload,
		Restart:         "unless-stopped" + payload,
		User:            "1000:1000" + payload,
		ReadOnly:        true,
		CapDrop:         []string{"ALL" + payload},
		NoNewPrivileges: true,
		Tmpfs:           []string{"/tmp:rw,size=1m" + payload},
		StopTimeout:     10,
		HealthCmd:       "curl -fsS http://127.0.0.1/" + payload,
		HealthInterval:  "1s" + payload,
		HealthTimeout:   "2s" + payload,
		HealthRetries:   3,
		Options:         []string{"--annotation=" + payload},
		Detach:          true,
	}

	command := cfg.BuildRunCommand()
	args := podmanArgv(t, dir, command)

	wants := []string{
		"--name", "svc" + payload,
		"-e", "A" + payload + "=V" + payload,
		"-p", "127.0.0.1:8080:80" + payload,
		"-v", "/tmp/data:/data" + payload,
		"-l", "l" + payload + "=v" + payload,
		"--network", "azud" + payload,
		"--network-alias", "alias" + payload,
		"--user", "1000:1000" + payload,
		"--cap-drop", "ALL" + payload,
		"--tmpfs", "/tmp:rw,size=1m" + payload,
		"--memory", "512m" + payload,
		"--cpus", "0.5" + payload,
		"--restart", "unless-stopped" + payload,
		"--entrypoint", "/bin/sh" + payload,
		"--health-cmd", "curl -fsS http://127.0.0.1/" + payload,
		"--annotation=" + payload,
		"registry.example/app" + payload + ":v1",
		"sh", "-c", "echo " + payload,
	}
	for _, want := range wants {
		if !argvContains(args, want) {
			t.Errorf("generated argv is missing exact value %q\nargv: %q", want, bytesToArgs(args))
		}
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("command substitution executed while building a container command")
	}
}

func bytesToArgs(args [][]byte) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		out[i] = string(arg)
	}
	return out
}

func TestControl_ExecAndLogsBuildersKeepPayloadsAsArguments(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "CANARY")
	payload := "$(touch " + canary + ")"

	execCmd := (&podman.ExecConfig{
		Container: "svc" + payload,
		Command:   []string{"echo", payload},
		User:      "user" + payload,
		WorkDir:   "/work" + payload,
		Env:       map[string]string{"K" + payload: "V" + payload},
		Detach:    true,
	}).BuildExecCommand()
	execArgs := podmanArgv(t, dir, execCmd)
	for _, want := range []string{
		"-u", "user" + payload,
		"-w", "/work" + payload,
		"-e", "K" + payload + "=V" + payload,
		"svc" + payload,
		"echo", payload,
	} {
		if !argvContains(execArgs, want) {
			t.Errorf("exec argv missing %q: %q", want, bytesToArgs(execArgs))
		}
	}

	logsCmd := (&podman.LogsConfig{
		Container:  "svc" + payload,
		Follow:     true,
		Tail:       "all" + payload,
		Timestamps: true,
		Since:      "1h" + payload,
		Until:      "2h" + payload,
	}).BuildLogsCommand()
	logsArgs := podmanArgv(t, dir, logsCmd)
	for _, want := range []string{
		"--tail", "all" + payload,
		"--since", "1h" + payload,
		"--until", "2h" + payload,
		"svc" + payload,
	} {
		if !argvContains(logsArgs, want) {
			t.Errorf("logs argv missing %q: %q", want, bytesToArgs(logsArgs))
		}
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("command substitution executed while building exec/logs commands")
	}
}

func TestControl_BuildCommandBuilderKeepsPayloadsAsArguments(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "CANARY")
	payload := "$(touch " + canary + ")"

	cfg := &podman.BuildConfig{
		Context:    "." + payload,
		Dockerfile: "Dockerfile" + payload,
		Tag:        "app" + payload + ":v1",
		Tags:       []string{"app" + payload + ":latest"},
		Args:       map[string]string{"K" + payload: "V" + payload},
		Target:     "target" + payload,
		CacheFrom:  []string{"type=local,src=/tmp" + payload},
		CacheTo:    "type=local,dest=/tmp" + payload,
		Platform:   "linux/amd64" + payload,
		NoCache:    true,
		Pull:       true,
		Secrets:    []string{"id=secret,src=/tmp" + payload},
		SSH:        []string{"default" + payload},
	}
	args := podmanArgv(t, dir, cfg.BuildCommand())
	for _, want := range []string{
		"-f", "Dockerfile" + payload,
		"-t", "app" + payload + ":v1",
		"-t", "app" + payload + ":latest",
		"--build-arg", "K" + payload + "=V" + payload,
		"--target", "target" + payload,
		"--cache-from", "type=local,src=/tmp" + payload,
		"--cache-to", "type=local,dest=/tmp" + payload,
		"--platform", "linux/amd64" + payload,
		"--secret", "id=secret,src=/tmp" + payload,
		"--ssh", "default" + payload,
		"." + payload,
	} {
		if !argvContains(args, want) {
			t.Errorf("build argv missing %q: %q", want, bytesToArgs(args))
		}
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("command substitution executed while building a podman build command")
	}
}

// Finding: BuildConfig.Options (and ManifestBuildConfig.Options) are appended
// to the shell command without quoting. No current config field feeds Options,
// so this is latent, but the exported builder is unsafe for future callers.
func TestFinding_BuildConfigOptionsAreShellEvaluated(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "CANARY")
	cfg := &podman.BuildConfig{
		Context: ".",
		Options: []string{"--annotation=$(touch " + canary + ")"},
	}
	args := podmanArgv(t, dir, cfg.BuildCommand())
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("expected BuildConfig.Options to execute substitution, but canary is absent; builder may have been fixed")
	}
	found := false
	for _, arg := range args {
		if strings.HasPrefix(string(arg), "--annotation=") && !strings.Contains(string(arg), "$(touch") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the option to be split into evaluated argv fragments: %q", bytesToArgs(args))
	}
	t.Logf("VULNERABLE (latent): podman.BuildConfig.Options are not shell-quoted; %q executed command substitution", cfg.Options[0])
}

// ---------------------------------------------------------------------------
// Finding: health-check helpers embed a URL using Go quoting, which is not
// shell quoting. The config validator currently blocks '$' and backticks in
// health paths, so this is unreachable through config/deploy.yml, but the
// exported helper itself is unsafe for any future caller.
// ---------------------------------------------------------------------------

func TestFinding_HealthcheckBuildersExecuteDollarSubstitution(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "CANARY")

	// A stub curl so BuildHTTPCheckCommand's `command -v curl` branch is used.
	stubDir := t.TempDir()
	stub := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	path := "/$(touch " + canary + ")"
	cmd := deploy.BuildHTTPCheckCommand(3000, path)
	run := exec.Command("sh", "-c", cmd)
	run.Dir = dir
	run.Env = append(os.Environ(), "PATH="+stubDir+":"+os.Getenv("PATH"))
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("health command failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("expected the unsafe helper to execute command substitution, but canary is absent; helper may have been fixed")
	}
	t.Logf("VULNERABLE (latent): deploy.BuildHTTPCheckCommand used Go quoting, not shell quoting; %q executed command substitution", cmd)

	// The same construction is used by the exec-candidate and helper builders.
	canary2 := filepath.Join(dir, "CANARY2")
	for _, cmd := range deploy.BuildHTTPCheckExecCandidates("svc", 3000, "/$(touch "+canary2+")") {
		run := exec.Command("sh", "-c", cmd)
		run.Dir = dir
		run.Env = append(os.Environ(), "PATH="+stubDir+":"+os.Getenv("PATH"))
		_, _ = run.CombinedOutput()
	}
	if _, err := os.Stat(canary2); err != nil {
		t.Fatalf("expected BuildHTTPCheckExecCandidates to execute substitution, but canary is absent; helper may have been fixed")
	}
	t.Logf("VULNERABLE (latent): deploy.BuildHTTPCheckExecCandidates also uses strconv.Quote for the URL")
}
