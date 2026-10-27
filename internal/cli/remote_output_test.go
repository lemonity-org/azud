package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/lemonity-org/azud/internal/ssh"
)

const remotePayload = "\x1b]52;c;ZXZpbA==\x07\x1b[2J\x1b[31mFORGED\x1b[0m\n"

func stubTerminalWriter(t *testing.T, terminal bool) {
	t.Helper()
	old := isTerminalWriter
	isTerminalWriter = func(io.Writer) bool { return terminal }
	t.Cleanup(func() { isTerminalWriter = old })
}

func TestRemoteWriterPolicy(t *testing.T) {
	tests := []struct {
		name     string
		kind     remoteStreamKind
		terminal bool
		raw      bool
		wantRaw  bool
	}{
		{name: "logs to pipe are sanitized", kind: remoteLogStream},
		{name: "logs to terminal are sanitized", kind: remoteLogStream, terminal: true},
		{name: "raw logs stay raw", kind: remoteLogStream, terminal: true, raw: true, wantRaw: true},
		{name: "exec to pipe stays byte-exact", kind: remoteExecStream, wantRaw: true},
		{name: "exec to terminal is sanitized", kind: remoteExecStream, terminal: true},
		{name: "raw exec to terminal stays raw", kind: remoteExecStream, terminal: true, raw: true, wantRaw: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubTerminalWriter(t, tt.terminal)
			var out bytes.Buffer
			writer, flush := remoteWriter(&out, tt.kind, tt.raw)
			_, _ = io.WriteString(writer, remotePayload)
			flush()

			if tt.wantRaw {
				if out.String() != remotePayload {
					t.Fatalf("output = %q, want raw payload", out.String())
				}
				return
			}
			if strings.Contains(out.String(), "\x1b") {
				t.Fatalf("output retained terminal controls: %q", out.String())
			}
			if !strings.Contains(out.String(), `\x1b]52;c;`) || !strings.Contains(out.String(), "FORGED") {
				t.Fatalf("output did not visibly escape the payload: %q", out.String())
			}
		})
	}
}

func TestRemoteExecToPipeKeepsBinaryOutput(t *testing.T) {
	stubTerminalWriter(t, false)
	binary := "PGDMP\x00\x01\x1b\r\n\xff\xfe"
	var stdout, stderr bytes.Buffer
	printRemoteResult(&stdout, &stderr, &ssh.Result{Stdout: binary}, remoteExecStream, false)
	if stdout.String() != binary {
		t.Fatalf("binary exec output changed: %q", stdout.String())
	}
}

func TestPrintRemoteResultSanitizesStdoutAndStderr(t *testing.T) {
	stubTerminalWriter(t, false)
	var stdout, stderr bytes.Buffer
	printRemoteResult(&stdout, &stderr, &ssh.Result{Stdout: remotePayload, Stderr: remotePayload}, remoteLogStream, false)

	for name, got := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
		if strings.Contains(got, "\x1b") {
			t.Fatalf("%s retained terminal controls: %q", name, got)
		}
		if want := `\x1b]52;c;ZXZpbA==\x07\x1b[2JFORGED` + "\n"; got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestRemoteStreamsFlushHeldBackSequence(t *testing.T) {
	stubTerminalWriter(t, false)
	var stdout, stderr bytes.Buffer
	safeStdout, safeStderr, flush := remoteStreams(&stdout, &stderr, remoteLogStream, false)
	_, _ = io.WriteString(safeStdout, "tail\x1b[3")
	_, _ = io.WriteString(safeStderr, "err\xe2\x82")
	flush()
	if stdout.String() != `tail\x1b[3` {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.String() != `err\xe2\x82` {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRawFlagRegisteredOnRemoteOutputCommands(t *testing.T) {
	for _, command := range []string{"app logs", "app exec", "accessory logs", "accessory exec", "cron logs", "proxy logs"} {
		cmd, _, err := rootCmd.Find(strings.Fields(command))
		if err != nil {
			t.Fatalf("find %s: %v", command, err)
		}
		if cmd.Flags().Lookup("raw") == nil {
			t.Errorf("%s has no --raw flag", command)
		}
	}
}
