package proxy

import (
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/ssh"
)

type recordingCaddyExecutor struct {
	config *podman.ExecConfig
	stdin  []byte
	result *ssh.Result
	err    error
}

func (r *recordingCaddyExecutor) Exec(_ string, config *podman.ExecConfig) (*ssh.Result, error) {
	r.config = config
	return r.response()
}

func (r *recordingCaddyExecutor) ExecWithStdin(_ string, config *podman.ExecConfig, stdin io.Reader) (*ssh.Result, error) {
	r.config = config
	r.stdin, _ = io.ReadAll(stdin)
	return r.response()
}

func (r *recordingCaddyExecutor) response() (*ssh.Result, error) {
	if r.result == nil {
		r.result = &ssh.Result{}
	}
	return r.result, r.err
}

func TestCaddyAPIRequestExecutesExactContainerLocalCurl(t *testing.T) {
	executor := &recordingCaddyExecutor{result: &ssh.Result{Stdout: `{"ok":true}`}}
	client := NewCaddyClient(executor)
	body := struct {
		Value string `json:"value"`
	}{Value: "quote ' newline\n$(touch /tmp/nope)"}

	got, err := client.apiRequest("proxy.example", "PATCH", "/id/route%20one", body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("response = %q", got)
	}
	wantCommand := []string{
		"curl", "--silent", "--show-error", "--fail-with-body",
		"--max-time", "30", "--noproxy", "*", "--proto", "=http",
		"--request", "PATCH", "--header", "Content-Type: application/json",
		"--data-binary", "@-", "--url", "http://127.0.0.1:2019/id/route%20one",
	}
	if executor.config.Container != CaddyContainerName || !executor.config.Interactive || !reflect.DeepEqual(executor.config.Command, wantCommand) {
		t.Fatalf("exec config = %#v", executor.config)
	}
	wantBody := `{"value":"quote ' newline\n$(touch /tmp/nope)"}`
	if string(executor.stdin) != wantBody {
		t.Fatalf("stdin = %q, want %q", executor.stdin, wantBody)
	}
	built := executor.config.BuildExecCommand()
	if !strings.HasPrefix(built, "podman exec -i azud-proxy curl ") || strings.Contains(built, wantBody) {
		t.Fatalf("body escaped stdin or command is not exact podman exec curl: %q", built)
	}
}

func TestCaddyAPIRequestWithoutBodyDoesNotOpenStdin(t *testing.T) {
	executor := &recordingCaddyExecutor{}
	client := NewCaddyClient(executor)
	if _, err := client.apiRequest("proxy.example", "GET", "/config/?path=a%2Fb", nil); err != nil {
		t.Fatal(err)
	}
	if executor.config.Interactive || len(executor.stdin) != 0 {
		t.Fatalf("bodyless request opened stdin: %#v body=%q", executor.config, executor.stdin)
	}
	if strings.Contains(strings.Join(executor.config.Command, " "), "Content-Type") || strings.Contains(strings.Join(executor.config.Command, " "), "@-") {
		t.Fatalf("bodyless request contains body flags: %v", executor.config.Command)
	}
}

func TestCaddyAPIRequestRejectsUnsupportedMethodAndUnsafePath(t *testing.T) {
	executor := &recordingCaddyExecutor{}
	client := NewCaddyClient(executor)
	if _, err := client.apiRequest("proxy.example", "TRACE", "/config/", nil); err == nil {
		t.Fatal("TRACE was accepted")
	}
	if _, err := client.apiRequest("proxy.example", "GET", "http://attacker.invalid/", nil); err == nil {
		t.Fatal("absolute URL was accepted as an API path")
	}
	if executor.config != nil {
		t.Fatal("invalid request reached the container executor")
	}
}

func TestLoadConfigAlwaysForcesContainerLoopbackAdmin(t *testing.T) {
	executor := &recordingCaddyExecutor{}
	client := NewCaddyClient(executor)
	config := &CaddyConfig{Admin: &AdminConfig{Listen: "0.0.0.0:2019"}}
	if err := client.LoadConfig("proxy.example", config); err != nil {
		t.Fatal(err)
	}
	if config.Admin.Listen != CaddyAdminListen {
		t.Fatalf("admin listener = %q", config.Admin.Listen)
	}
	if strings.Contains(string(executor.stdin), "0.0.0.0") || !strings.Contains(string(executor.stdin), CaddyAdminListen) {
		t.Fatalf("unsafe admin listener reached Caddy: %s", executor.stdin)
	}
}
