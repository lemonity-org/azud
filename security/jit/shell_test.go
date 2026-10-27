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

// Control: BuildConfig.Options (and ManifestBuildConfig.Options) are quoted one
// argv value per element, like every other build field. No config field feeds
// Options today; this keeps the builder safe for future callers.
func TestControl_BuildConfigOptionsAreQuoted(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "CANARY")
	option := "--annotation=$(touch " + canary + ")"
	cfg := &podman.BuildConfig{
		Context: ".",
		Options: []string{option},
	}
	if args := podmanArgv(t, dir, cfg.BuildCommand()); !argvContains(args, option) {
		t.Errorf("build argv missing literal option %q: %q", option, bytesToArgs(args))
	}

	manifest := &podman.ManifestBuildConfig{
		BuildConfig: *cfg,
		Platforms:   []string{"linux/amd64", "linux/arm64"},
	}
	commands := manifest.ManifestBuildCommands()
	for _, command := range commands[1 : 1+len(manifest.Platforms)] {
		if args := podmanArgv(t, dir, command); !argvContains(args, option) {
			t.Errorf("manifest build argv missing literal option %q: %q", option, bytesToArgs(args))
		}
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("command substitution executed from podman build options")
	}
}

// ---------------------------------------------------------------------------
// Control: health-check helpers validate the probe path themselves and
// shell-quote the URL, so they stay safe for callers that skip config
// validation. The exec-candidate and helper commands run in the host's shell;
// the container command runs under podman's CMD-SHELL.
// ---------------------------------------------------------------------------

func TestControl_HealthcheckBuildersQuoteURL(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "CANARY")
	payload := "/$(touch " + canary + ")"

	if cmd := deploy.BuildHTTPCheckCommand(3000, payload); cmd != "" {
		t.Fatalf("BuildHTTPCheckCommand accepted an unsafe path: %q", cmd)
	}
	if cmds := deploy.BuildHTTPCheckExecCandidates("svc", 3000, payload); cmds != nil {
		t.Fatalf("BuildHTTPCheckExecCandidates accepted an unsafe path: %q", cmds)
	}
	if cmd := deploy.BuildHTTPCheckHelperCommand("svc", 3000, payload, "", ""); cmd != "" {
		t.Fatalf("BuildHTTPCheckHelperCommand accepted an unsafe path: %q", cmd)
	}

	// A valid path that contains shell syntax ('&', '?') must reach the HTTP
	// client as one literal argument from every builder. The stubs report argv
	// on stderr because the container command discards the client's stdout.
	path := "/up?x=1&y=%2F"
	url := "http://127.0.0.1:3000" + path
	stubDir := t.TempDir()
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\" >&2; done\nexit 0\n"
	for _, name := range []string{"curl", "podman"} {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	commands := append([]string{
		deploy.BuildHTTPCheckCommand(3000, path),
		"sh -c " + shell.Quote(deploy.BuildHTTPCheckCommand(3000, path)),
		deploy.BuildHTTPCheckHelperCommand("svc", 3000, path, "", ""),
	}, deploy.BuildHTTPCheckExecCandidates("svc", 3000, path)...)
	for _, cmd := range commands {
		run := exec.Command("sh", "-c", cmd)
		run.Dir = dir
		run.Env = append(os.Environ(), "PATH="+stubDir+":"+os.Getenv("PATH"))
		out, err := run.CombinedOutput()
		if err != nil {
			t.Fatalf("health command %q failed: %v", cmd, err)
		}
		if !argvContains(bytes.Split(out, []byte{0}), url) {
			t.Errorf("health command %q did not pass %q as one argument: %q", cmd, url, out)
		}
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("command substitution executed while building a health-check command")
	}
}
