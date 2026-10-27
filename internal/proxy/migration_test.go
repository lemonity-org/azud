package proxy

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/ssh"
)

// fakeProxyContainer is one container on a fakeProxyHost.
type fakeProxyContainer struct {
	legacy  bool // created by an older release: TCP admin, no spec label
	running bool
}

// fakeProxyHost simulates the Podman state Boot drives over SSH: proxy
// containers by name, the caddy_config autosave and the admin socket.
type fakeProxyHost struct {
	mu         sync.Mutex
	containers map[string]*fakeProxyContainer
	autosave   string
	runFails   bool // `podman run` of the replacement fails
	crashes    bool // the replacement starts and exits at once
	commands   []string
}

func newFakeProxyHost(autosave string, containers map[string]*fakeProxyContainer) *fakeProxyHost {
	return &fakeProxyHost{containers: containers, autosave: autosave}
}

func (h *fakeProxyHost) handle(command string, stdin io.Reader) (string, string, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commands = append(h.commands, command)
	fields := strings.Fields(command)
	switch {
	case strings.Contains(command, "echo LOCKED"):
		return "LOCKED\n", "", 0
	case strings.HasPrefix(command, "podman container inspect "):
		container := h.containers[fields[3]]
		if container == nil {
			return "", "Error: no such container", 125
		}
		switch {
		case strings.Contains(command, "{{.State.Running}}"):
			return fmt.Sprintf("%t\n", container.running), "", 0
		case strings.Contains(command, "{{.Id}}"):
			return fields[3] + "-id\n", "", 0
		}
		env, labels := `"CADDY_ADMIN=`+CaddyAdminListen+`"`, `"`+ProxySpecLabel+`":"`+ProxySpecVersion+`"`
		if container.legacy {
			env, labels = `"CADDY_ADMIN=0.0.0.0:2019"`, `"azud.type":"proxy"`
		}
		return fmt.Sprintf(`[{"Config":{"Cmd":["caddy","run","--resume"],"Env":[%s],"Labels":{%s}},"HostConfig":{"NetworkMode":"bridge"}}]`, env, labels), "", 0
	case strings.HasPrefix(command, "podman stop "):
		if container := h.containers[fields[len(fields)-1]]; container != nil {
			container.running = false
		}
		return "", "", 0
	case strings.HasPrefix(command, "podman rm -f "):
		delete(h.containers, fields[3])
		return "", "", 0
	case strings.HasPrefix(command, "podman rename "):
		container := h.containers[fields[2]]
		if container == nil || h.containers[fields[3]] != nil {
			return "", "Error: rename failed", 125
		}
		delete(h.containers, fields[2])
		h.containers[fields[3]] = container
		return "", "", 0
	case strings.HasPrefix(command, "podman start "):
		container := h.containers[fields[2]]
		if container == nil {
			return "", "Error: no such container", 125
		}
		container.running = true
		return "", "", 0
	case strings.HasPrefix(command, "podman logs "):
		return "", "Error: starting caddy administration endpoint: permission denied\n", 0
	case strings.Contains(command, "if [ -s /config/caddy/autosave.json ]"):
		return h.autosave, "", 0
	case strings.Contains(command, "autosave.json.azud-tmp"):
		data, _ := io.ReadAll(stdin)
		h.autosave = string(data)
		return "", "", 0
	case strings.Contains(command, "rm -f /config/caddy/autosave.json"):
		h.autosave = ""
		return "", "", 0
	case strings.HasPrefix(command, "podman run ") && strings.Contains(command, "--name azud-proxy "):
		if h.runFails {
			return "", "Error: rootlessport cannot expose privileged port 80", 126
		}
		h.containers[CaddyContainerName] = &fakeProxyContainer{running: !h.crashes}
		return "", "", 0
	case strings.Contains(command, " exec ") && strings.Contains(command, "azud-proxy curl "):
		container := h.containers[CaddyContainerName]
		switch {
		case container == nil:
			return "", `Error: no container with name or ID "azud-proxy" found: no such container`, 125
		case !container.running:
			return "", "Error: can only create exec sessions on running containers: container state improper", 255
		case container.legacy:
			return "", "curl: (7) Failed to connect to localhost: Could not connect to server", 7
		}
		_, _ = io.ReadAll(stdin)
		return `{"admin":{"listen":"` + CaddyAdminListen + `"},"apps":{}}`, "", 0
	case strings.Contains(command, "test -f "):
		return "", "", 1
	case strings.Contains(command, "cat > "):
		_, _ = io.ReadAll(stdin)
		return "", "", 0
	default:
		return "", "", 0
	}
}

// podmanCommands returns the Podman commands that touched a named proxy
// container, in order.
func (h *fakeProxyHost) podmanCommands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, command := range h.commands {
		for _, verb := range []string{"podman stop ", "podman rm ", "podman rename ", "podman start ", "podman logs "} {
			if strings.HasPrefix(command, verb) {
				out = append(out, command)
			}
		}
		if strings.HasPrefix(command, "podman run ") && strings.Contains(command, "--name azud-proxy ") {
			out = append(out, "podman run --name azud-proxy")
		}
	}
	return out
}

// startFakeProxySSH serves h over SSH on loopback and returns a Manager
// connected to it, plus the buffer that receives the Manager's stderr.
func startFakeProxySSH(t *testing.T, h *fakeProxyHost) (*Manager, *bytes.Buffer) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &gossh.ServerConfig{
		PublicKeyCallback: func(gossh.ConnMetadata, gossh.PublicKey) (*gossh.Permissions, error) { return nil, nil },
	}
	config.AddHostKey(signer)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveFakeProxySSH(conn, config, h)
		}
	}()

	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", "")
	client := ssh.NewClient(&ssh.Config{
		User:                  "root",
		Port:                  listener.Addr().(*net.TCPAddr).Port,
		Keys:                  []string{keyPath},
		ConnectTimeout:        5 * time.Second,
		InsecureIgnoreHostKey: true,
	})
	t.Cleanup(func() { _ = client.Close() })
	var stderr bytes.Buffer
	return NewManagerWithOptions(client, output.NewLogger(io.Discard, &stderr, false), "root", false, false), &stderr
}

func serveFakeProxySSH(conn net.Conn, config *gossh.ServerConfig, h *fakeProxyHost) {
	defer func() { _ = conn.Close() }()
	server, channels, requests, err := gossh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	go gossh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(gossh.UnknownChannelType, "session only")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer func() { _ = channel.Close() }()
			for request := range channelRequests {
				if request.Type != "exec" {
					_ = request.Reply(request.Type == "env", nil)
					continue
				}
				var payload struct{ Command string }
				if err := gossh.Unmarshal(request.Payload, &payload); err != nil {
					_ = request.Reply(false, nil)
					return
				}
				_ = request.Reply(true, nil)
				stdout, stderr, exit := h.handle(payload.Command, channel)
				_, _ = io.WriteString(channel, stdout)
				_, _ = io.WriteString(channel.Stderr(), stderr)
				_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{uint32(exit)}))
				return
			}
		}()
	}
}

const legacyTestAutosave = `{"admin":{"listen":"0.0.0.0:2019"},"apps":{"http":{"servers":{"srv0":{"routes":[{"@id":"azud-route-shop"}]}}}}}`

func TestBootMigrationRemovesLegacyContainerOnlyAfterReplacementAnswers(t *testing.T) {
	h := newFakeProxyHost(legacyTestAutosave, map[string]*fakeProxyContainer{
		CaddyContainerName: {legacy: true, running: true},
	})
	manager, _ := startFakeProxySSH(t, h)

	if err := manager.Boot("127.0.0.1", nil); err != nil {
		t.Fatalf("Boot() error = %v", err)
	}

	want := []string{
		"podman stop -t 30 azud-proxy",
		"podman rename azud-proxy azud-proxy-legacy",
		"podman run --name azud-proxy",
		"podman rm -f azud-proxy-legacy",
	}
	if got := h.podmanCommands(); !slices.Equal(got, want) {
		t.Fatalf("podman commands = %q, want %q", got, want)
	}
	if len(h.containers) != 1 || h.containers[CaddyContainerName] == nil || h.containers[CaddyContainerName].legacy {
		t.Fatalf("containers after migration = %#v", h.containers)
	}
	if !strings.Contains(h.autosave, CaddyAdminSocket) || !strings.Contains(h.autosave, "azud-route-shop") {
		t.Fatalf("autosave was not normalized with its routes: %s", h.autosave)
	}
}

func TestBootMigrationRollsBackWhenReplacementFails(t *testing.T) {
	tests := []struct {
		name      string
		runFails  bool
		crashes   bool
		autosave  string
		wantCause string
	}{
		{name: "run fails", runFails: true, autosave: legacyTestAutosave, wantCause: "failed to start proxy container"},
		{name: "admin never answers", crashes: true, autosave: legacyTestAutosave, wantCause: "proxy container azud-proxy is not running"},
		{name: "no autosave", crashes: true, wantCause: "proxy container azud-proxy is not running"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeProxyHost(tt.autosave, map[string]*fakeProxyContainer{
				CaddyContainerName: {legacy: true, running: true},
			})
			h.runFails, h.crashes = tt.runFails, tt.crashes
			manager, stderr := startFakeProxySSH(t, h)

			started := time.Now()
			err := manager.Boot("127.0.0.1", nil)
			if err == nil {
				t.Fatal("Boot() succeeded with a replacement that cannot start")
			}
			if !strings.Contains(err.Error(), "rolled back to its previous, outdated container") || !strings.Contains(err.Error(), tt.wantCause) {
				t.Fatalf("Boot() error = %v, want rollback reported with %q", err, tt.wantCause)
			}
			if elapsed := time.Since(started); elapsed > 10*time.Second {
				t.Fatalf("Boot() waited %s for a container that is not running", elapsed)
			}

			want := []string{
				"podman stop -t 30 azud-proxy",
				"podman rename azud-proxy azud-proxy-legacy",
				"podman run --name azud-proxy",
				"podman logs --tail 20 azud-proxy",
				"podman rm -f azud-proxy",
				"podman rename azud-proxy-legacy azud-proxy",
				"podman start azud-proxy",
			}
			if got := h.podmanCommands(); !slices.Equal(got, want) {
				t.Fatalf("podman commands = %q, want %q", got, want)
			}
			restored := h.containers[CaddyContainerName]
			if len(h.containers) != 1 || restored == nil || !restored.legacy || !restored.running {
				t.Fatalf("previous proxy was not restored and started: %#v", h.containers)
			}
			// The previous container resumes its own autosave, not the copy
			// normalized to a socket it cannot create.
			if h.autosave != tt.autosave {
				t.Fatalf("autosave = %q, want the original %q", h.autosave, tt.autosave)
			}
			if tt.crashes && !strings.Contains(stderr.String(), "permission denied") {
				t.Fatalf("replacement logs were not shown before its removal: %q", stderr.String())
			}
		})
	}
}

func TestBootRestoresProxyLeftByInterruptedMigration(t *testing.T) {
	tests := []struct {
		name       string
		containers map[string]*fakeProxyContainer
		want       []string
	}{
		{
			name: "replacement never created",
			containers: map[string]*fakeProxyContainer{
				legacyProxyContainerName: {legacy: true},
			},
			want: []string{
				"podman rename azud-proxy-legacy azud-proxy",
				"podman rename azud-proxy azud-proxy-legacy",
				"podman run --name azud-proxy",
				"podman rm -f azud-proxy-legacy",
			},
		},
		{
			name: "replacement stopped",
			containers: map[string]*fakeProxyContainer{
				CaddyContainerName:       {},
				legacyProxyContainerName: {legacy: true},
			},
			want: []string{
				"podman rm -f azud-proxy",
				"podman rename azud-proxy-legacy azud-proxy",
				"podman rename azud-proxy azud-proxy-legacy",
				"podman run --name azud-proxy",
				"podman rm -f azud-proxy-legacy",
			},
		},
		{
			name: "replacement running",
			containers: map[string]*fakeProxyContainer{
				CaddyContainerName:       {running: true},
				legacyProxyContainerName: {legacy: true},
			},
			want: []string{
				"podman rm -f azud-proxy-legacy",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeProxyHost(legacyTestAutosave, tt.containers)
			manager, _ := startFakeProxySSH(t, h)

			if err := manager.Boot("127.0.0.1", nil); err != nil {
				t.Fatalf("Boot() error = %v", err)
			}
			if got := h.podmanCommands(); !slices.Equal(got, tt.want) {
				t.Fatalf("podman commands = %q, want %q", got, tt.want)
			}
			if len(h.containers) != 1 || h.containers[CaddyContainerName] == nil || h.containers[CaddyContainerName].legacy {
				t.Fatalf("containers after Boot = %#v", h.containers)
			}
		})
	}
}

func TestBootMigrationRestartsPreviousProxyWhenAutosaveSnapshotFails(t *testing.T) {
	h := newFakeProxyHost(`[]`, map[string]*fakeProxyContainer{
		CaddyContainerName: {legacy: true, running: true},
	})
	manager, _ := startFakeProxySSH(t, h)

	err := manager.Boot("127.0.0.1", nil)
	if err == nil || !strings.Contains(err.Error(), "restarted the previous container") {
		t.Fatalf("Boot() error = %v, want the restart reported", err)
	}
	want := []string{"podman stop -t 30 azud-proxy", "podman start azud-proxy"}
	if got := h.podmanCommands(); !slices.Equal(got, want) {
		t.Fatalf("podman commands = %q, want %q", got, want)
	}
	if container := h.containers[CaddyContainerName]; container == nil || !container.running {
		t.Fatalf("previous proxy is not running: %#v", h.containers)
	}
}

func TestWaitForAdminReportsLastProbeError(t *testing.T) {
	probeErr := adminRequestError(&ssh.Result{ExitCode: 7, Stderr: "curl: (7) Failed to connect"})
	calls := 0
	err := waitForAdmin(20*time.Millisecond, time.Millisecond, func() error {
		calls++
		return probeErr
	})
	if err == nil || !errors.Is(err, probeErr) || !strings.Contains(err.Error(), "not ready after 20ms") || !strings.Contains(err.Error(), "is not accepting connections") {
		t.Fatalf("waitForAdmin() error = %v", err)
	}
	if calls < 2 {
		t.Fatalf("waitForAdmin() probed %d times, want retries until the timeout", calls)
	}
}

func TestWaitForAdminStopsWhenProxyIsNotRunning(t *testing.T) {
	calls := 0
	err := waitForAdmin(time.Minute, time.Millisecond, func() error {
		calls++
		return adminRequestError(&ssh.Result{ExitCode: 255, Stderr: "Error: can only create exec sessions on running containers: container state improper"})
	})
	if !errors.Is(err, errProxyNotRunning) || calls != 1 {
		t.Fatalf("waitForAdmin() = %v after %d probes, want an immediate not-running error", err, calls)
	}

	calls = 0
	err = waitForAdmin(time.Minute, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return adminRequestError(&ssh.Result{ExitCode: 7, Stderr: "curl: (7) Failed to connect"})
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("waitForAdmin() = %v after %d probes, want success on the third", err, calls)
	}
}

func TestAdminRequestErrorMarksStoppedProxy(t *testing.T) {
	for _, stderr := range []string{
		"Error: can only create exec sessions on running containers: container state improper",
		`Error: no container with name or ID "azud-proxy" found: no such container`,
	} {
		if err := adminRequestError(&ssh.Result{ExitCode: 125, Stderr: stderr}); !errors.Is(err, errProxyNotRunning) {
			t.Fatalf("adminRequestError(%q) = %v, want errProxyNotRunning", stderr, err)
		}
	}
	if err := adminRequestError(&ssh.Result{ExitCode: 7, Stderr: "curl: (7) Failed to connect"}); errors.Is(err, errProxyNotRunning) {
		t.Fatalf("refused socket reported as a stopped container: %v", err)
	}
}
