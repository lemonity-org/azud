//go:build security

// Package jit contains just-in-time security tests for Azud.
//
// These tests are intentionally excluded from the normal build with the
// "security" build tag:
//
//	go test -tags security ./security/jit/...
//
// The suite drives the real CLI binary against a local, in-process fake SSH
// server. That lets us assert on the exact commands Azud would send to a host
// without requiring Podman, root, or a real deployment target.
package jit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

var (
	azudBin     string
	azudBinDir  string
	buildOnce   sync.Once
	buildErr    error
	clientKeyMu sync.Mutex
	clientKey   string
)

// TestMain builds the real CLI once and keeps the build output out of the
// repository. Set AZUD_TEST_BIN to reuse an already-built binary.
func TestMain(m *testing.M) {
	code := m.Run()
	if clientKey != "" {
		_ = os.RemoveAll(filepath.Dir(clientKey))
	}
	if azudBinDir != "" {
		_ = os.RemoveAll(azudBinDir)
	}
	os.Exit(code)
}

func requireCLI(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		azudBin = os.Getenv("AZUD_TEST_BIN")
		if azudBin != "" {
			if _, err := os.Stat(azudBin); err == nil {
				return
			}
			azudBin = ""
		}
		dir, err := os.MkdirTemp("", "azud-jit-bin-")
		if err != nil {
			buildErr = err
			return
		}
		azudBinDir = dir
		azudBin = filepath.Join(dir, "azud")
		cmd := exec.Command("go", "build", "-o", azudBin, "github.com/lemonity-org/azud/cmd/azud")
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %w\n%s", err, out)
			_ = os.RemoveAll(dir)
			azudBin = ""
		}
	})
	if buildErr != nil {
		t.Fatalf("failed to build azud: %v", buildErr)
	}
	if _, err := os.Stat(azudBin); err != nil {
		t.Fatalf("azud binary unavailable: %v", err)
	}
	return azudBin
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func newWorkdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".azud"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// clientKeyPath returns a per-process throwaway SSH key used by the CLI.
func clientKeyPath(t *testing.T) string {
	t.Helper()
	clientKeyMu.Lock()
	defer clientKeyMu.Unlock()
	if clientKey != "" {
		return clientKey
	}
	dir, err := os.MkdirTemp("", "azud-jit-key-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	path := filepath.Join(dir, "id_ed25519")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "azud-jit")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	writeFile(t, path, string(pem.EncodeToMemory(block)), 0o600)
	clientKey = path
	return clientKey
}

type cliResult struct {
	Stdout   string
	Stderr   string
	Combined string
	Exit     int
}

func (r cliResult) String() string {
	return fmt.Sprintf("exit=%d\n--- stdout ---\n%s\n--- stderr ---\n%s", r.Exit, r.Stdout, r.Stderr)
}

func cleanEnv(extra ...string) []string {
	base := make([]string, 0, len(os.Environ())+len(extra))
	for _, entry := range os.Environ() {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		switch {
		case key == "SSH_AUTH_SOCK":
			continue
		case strings.HasPrefix(key, "AZUD_"):
			continue
		case key == "EDITOR" || key == "VISUAL":
			continue
		}
		base = append(base, entry)
	}
	base = append(base, "NO_COLOR=1")
	return append(base, extra...)
}

func runAzud(t *testing.T, dir string, env []string, args ...string) cliResult {
	t.Helper()
	bin := requireCLI(t)
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv(env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exit = exitErr.ExitCode()
		} else {
			t.Fatalf("run azud %v: %v", args, err)
		}
	}
	return cliResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Combined: stdout.String() + stderr.String(),
		Exit:     exit,
	}
}

func runShell(t *testing.T, dir, script string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	exit := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exit = exitErr.ExitCode()
		} else {
			t.Fatalf("shell: %v", err)
		}
	}
	return string(out), exit
}

// baseConfig builds a minimal valid config. host/port point the CLI at a fake
// SSH server; the key path is quoted because temp paths can contain spaces.
func baseConfig(host string, port int, keyPath, service string) string {
	return fmt.Sprintf(`service: %s
image: example/%s:v1
servers:
  web:
    hosts:
      - %s
proxy:
  host: shop.example.com
ssh:
  user: %s
  port: %d
  keys: ["%s"]
  connect_timeout: 2s
  command_timeout: 40s
  insecure_ignore_host_key: true
`, service, service, host, currentUser(), port, filepath.ToSlash(keyPath))
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "nobody"
}

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config", "deploy.yml")
	writeFile(t, path, content, 0o600)
	return path
}

func configArgs(path string, extra ...string) []string {
	args := []string{"-c", path}
	return append(args, extra...)
}

// ---------------------------------------------------------------------------
// Fake SSH server
// ---------------------------------------------------------------------------

type fakeResult struct {
	Stdout string
	Stderr string
	Exit   int
}

type fakeHandler func(command string, stdin io.Reader) fakeResult

type commandRecord struct {
	Command string
	Stdin   []byte
}

type fakeSSH struct {
	t        *testing.T
	listener net.Listener
	handler  fakeHandler

	mu          sync.Mutex
	commands    []commandRecord
	connections int
}

func startFakeSSH(t *testing.T, bind string, handler fakeHandler) *fakeSSH {
	t.Helper()
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		t.Fatalf("fake ssh listen: %v", err)
	}
	f := &fakeSSH{t: t, listener: ln, handler: handler}
	t.Cleanup(func() { _ = ln.Close() })
	go f.accept()
	return f
}

func (f *fakeSSH) port() int {
	return f.listener.Addr().(*net.TCPAddr).Port
}

func (f *fakeSSH) addrs() []string {
	return []string{f.listener.Addr().String()}
}

func (f *fakeSSH) accept() {
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		return
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)

	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.connections++
		f.mu.Unlock()
		go f.serveConn(conn, config)
	}
}

func (f *fakeSSH) serveConn(netConn net.Conn, config *ssh.ServerConfig) {
	defer func() { _ = netConn.Close() }()
	serverConn, chans, reqs, err := ssh.NewServerConn(netConn, config)
	if err != nil {
		return
	}
	defer func() { _ = serverConn.Close() }()
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		go f.serveSession(newChan)
	}
}

func (f *fakeSSH) serveSession(newChan ssh.NewChannel) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer func() { _ = ch.Close() }()

	for req := range reqs {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
				_ = req.Reply(false, nil)
				return
			}
			_ = req.Reply(true, nil)

			record := commandRecord{Command: payload.Command}
			result := fakeResult{}
			if f.handler != nil {
				// Buffer stdin only for handlers that ask for it. Handlers
				// that need stdin call io.ReadAll; the channel is closed by
				// the client after it has written its payload.
				stdin := &recordingReader{r: ch, record: &record}
				result = f.handler(payload.Command, stdin)
			}
			f.mu.Lock()
			f.commands = append(f.commands, record)
			f.mu.Unlock()

			if result.Stdout != "" {
				_, _ = io.WriteString(ch, result.Stdout)
			}
			if result.Stderr != "" {
				_, _ = io.WriteString(ch.Stderr(), result.Stderr)
			}
			statusPayload := ssh.Marshal(struct{ Status uint32 }{Status: uint32(result.Exit)})
			_, _ = ch.SendRequest("exit-status", false, statusPayload)
			return

		case "env", "pty-req", "window-change", "signal":
			_ = req.Reply(true, nil)
		case "subsystem":
			_ = req.Reply(false, nil)
		default:
			_ = req.Reply(false, nil)
		}
	}
}

// recordingReader tees stdin so tests can inspect it after the handler returns.
type recordingReader struct {
	r      io.Reader
	record *commandRecord
}

func (r *recordingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.record.Stdin = append(r.record.Stdin, p[:n]...)
	}
	return n, err
}

func (f *fakeSSH) snapshot() []commandRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]commandRecord, len(f.commands))
	copy(out, f.commands)
	return out
}

func (f *fakeSSH) commandsMatching(substr string) []commandRecord {
	var out []commandRecord
	for _, cmd := range f.snapshot() {
		if strings.Contains(cmd.Command, substr) {
			out = append(out, cmd)
		}
	}
	return out
}

func (f *fakeSSH) waitCommand(t *testing.T, substr string, timeout time.Duration) commandRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		matches := f.commandsMatching(substr)
		if len(matches) > 0 {
			return matches[len(matches)-1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for fake SSH command containing %q; got:\n%s", substr, commandDump(f.snapshot()))
	return commandRecord{}
}

func commandDump(commands []commandRecord) string {
	var b strings.Builder
	for i, cmd := range commands {
		fmt.Fprintf(&b, "[%02d] %s\n", i, cmd.Command)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Shared handlers
// ---------------------------------------------------------------------------

// fakeBuildDir is the remote build directory the fake builder reports for the
// "shop" service.
const fakeBuildDir = "/tmp/azud-build-shop.jit0000001"

// permissive returns a handler that succeeds everything and returns {} for
// Caddy admin GET requests. It is enough for most non-proxy commands.
func permissive() fakeHandler {
	return func(command string, stdin io.Reader) fakeResult {
		switch {
		case strings.Contains(command, "echo LOCKED"):
			// WithRemoteLock waits for this line before running its callback.
			return fakeResult{Stdout: "LOCKED\n"}
		case strings.Contains(command, "curl") && strings.Contains(command, "-d @-"):
			_, _ = io.ReadAll(stdin)
			return fakeResult{}
		case strings.Contains(command, "curl") && strings.Contains(command, "/config/"):
			return fakeResult{Stdout: "{}"}
		case strings.Contains(command, "test -f"):
			return fakeResult{Exit: 1}
		case strings.Contains(command, "mktemp -d /tmp/azud-build-"):
			// Remote build contexts live in a mktemp directory whose path
			// the CLI reads back and validates.
			return fakeResult{Stdout: fakeBuildDir + "\n"}
		case strings.Contains(command, "cat > \"") || strings.Contains(command, "cat > "):
			_, _ = io.ReadAll(stdin)
			return fakeResult{}
		case strings.Contains(command, "tar xzf -"):
			_, _ = io.ReadAll(stdin)
			return fakeResult{}
		case strings.Contains(command, "--password-stdin"):
			_, _ = io.ReadAll(stdin)
			return fakeResult{}
		default:
			return fakeResult{}
		}
	}
}

// proxyBootHandler satisfies the exact sequence used by `azud proxy boot` in
// bridge mode: inspect, optional autosave snapshot, container run, admin API
// readiness, initial /load, and persisted config write.
func proxyBootHandler() fakeHandler {
	return func(command string, stdin io.Reader) fakeResult {
		switch {
		case strings.Contains(command, "echo LOCKED"):
			return fakeResult{Stdout: "LOCKED\n"}
		case strings.Contains(command, "container inspect"):
			// Nothing exists yet: IsRunning and Exists both false.
			return fakeResult{Exit: 1, Stderr: "no such container"}
		case strings.Contains(command, "curl") && strings.Contains(command, "-d @-"):
			_, _ = io.ReadAll(stdin)
			return fakeResult{}
		case strings.Contains(command, "curl") && strings.Contains(command, "/config/"):
			return fakeResult{Stdout: "{}"}
		case strings.Contains(command, "test -f"):
			return fakeResult{Exit: 1}
		case strings.Contains(command, "cat > ") || strings.Contains(command, "umask 077"):
			_, _ = io.ReadAll(stdin)
			return fakeResult{}
		case strings.Contains(command, "tar xzf -"):
			_, _ = io.ReadAll(stdin)
			return fakeResult{}
		case strings.Contains(command, "podman run") && strings.Contains(command, "caddy"):
			return fakeResult{}
		case strings.Contains(command, "podman run"):
			return fakeResult{}
		default:
			return fakeResult{}
		}
	}
}

// tarEntries decodes a tar.gz stream and returns entry names with contents.
func tarEntries(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	entries := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		entries[strings.TrimSuffix(hdr.Name, "/")] = body
	}
	return entries
}
