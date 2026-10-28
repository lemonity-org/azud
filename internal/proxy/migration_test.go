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
	id      string
}

// fakeProxyHost simulates the Podman state Boot drives over SSH: proxy
// containers by name, the caddy_config autosave, the admin socket, files in
// the Azud state directory and the host-wide Caddy flock.
type fakeProxyHost struct {
	mu         sync.Mutex
	containers map[string]*fakeProxyContainer
	autosave   string
	files      map[string]string // Azud state files by base name
	runFails   bool              // `podman run` of the replacement fails
	crashes    bool              // the replacement starts and exits at once
	commands   []string
	nextID     int

	// interruptAt simulates a Boot that dies when it reaches the first
	// command containing this text: that command and every later one fail
	// without touching the host until interruptAt is cleared.
	interruptAt string
	interrupted bool

	// beforeRun, when set, runs once before the replacement is created,
	// without holding mu, so a test can interleave a second command.
	beforeRun func()

	// caddyLock is the flock on the Caddy lock file; lockWaiters counts lock
	// sessions blocked on it.
	caddyLock   chan struct{}
	lockWaiters int
}

func newFakeProxyHost(autosave string, containers map[string]*fakeProxyContainer) *fakeProxyHost {
	h := &fakeProxyHost{containers: containers, autosave: autosave, files: map[string]string{}, caddyLock: make(chan struct{}, 1)}
	for _, container := range containers {
		container.id = h.newID()
	}
	return h
}

func (h *fakeProxyHost) newID() string {
	h.nextID++
	return fmt.Sprintf("%064x", h.nextID)
}

// lock serves a WithRemoteLock session like flock -x -w: it confirms with
// LOCKED once the lock is free, holds it until the client closes stdin and
// gives up after a timeout. The lock is not re-entrant: a nested
// WithRemoteLock from the same command times out instead of deadlocking
// the test.
func (h *fakeProxyHost) lock(channel gossh.Channel) int {
	h.mu.Lock()
	h.commands = append(h.commands, "flock caddy.lock")
	h.lockWaiters++
	h.mu.Unlock()
	acquired := false
	select {
	case h.caddyLock <- struct{}{}:
		acquired = true
	case <-time.After(5 * time.Second):
	}
	h.mu.Lock()
	h.lockWaiters--
	h.mu.Unlock()
	if !acquired {
		return 1
	}
	defer func() { <-h.caddyLock }()
	_, _ = io.WriteString(channel, "LOCKED\n")
	_, _ = io.Copy(io.Discard, channel)
	return 0
}

func (h *fakeProxyHost) waitingForLock() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lockWaiters > 0
}

// stateFile returns the base name of the Azud state file a command reads,
// writes or removes, if any.
func stateFile(command string) string {
	for _, name := range []string{CaddyConfigFileName, proxyRollbackFileName} {
		if strings.Contains(command, "/"+name) {
			return name
		}
	}
	return ""
}

func (h *fakeProxyHost) handle(command string, stdin io.Reader) (string, string, int) {
	if strings.HasPrefix(command, "podman run ") && strings.Contains(command, "--name azud-proxy ") {
		h.mu.Lock()
		beforeRun := h.beforeRun
		h.beforeRun = nil
		h.mu.Unlock()
		if beforeRun != nil {
			beforeRun()
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commands = append(h.commands, command)
	if h.interruptAt != "" && (h.interrupted || strings.Contains(command, h.interruptAt)) {
		h.interrupted = true
		return "", "Error: connection lost", 255
	}
	fields := strings.Fields(command)
	switch {
	case strings.HasPrefix(command, "podman container inspect "):
		container := h.containers[fields[3]]
		if container == nil {
			return "", "Error: no such container", 125
		}
		switch {
		case strings.Contains(command, "{{.State.Running}}"):
			return fmt.Sprintf("%t\n", container.running), "", 0
		case strings.Contains(command, "{{.Id}}"):
			return container.id + "\n", "", 0
		}
		env, labels := `"CADDY_ADMIN=`+CaddyAdminListen+`"`, `"`+ProxySpecLabel+`":"`+ProxySpecVersion+`"`
		if container.legacy {
			env, labels = `"CADDY_ADMIN=0.0.0.0:2019"`, `"azud.type":"proxy"`
		}
		return fmt.Sprintf(`[{"Id":"%s","Config":{"Cmd":["caddy","run","--resume"],"Env":[%s],"Labels":{%s}},"HostConfig":{"NetworkMode":"bridge"}}]`, container.id, env, labels), "", 0
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
		if h.containers[CaddyContainerName] != nil {
			return "", `Error: creating container storage: the container name "azud-proxy" is already in use`, 125
		}
		if h.runFails {
			return "", "Error: rootlessport cannot expose privileged port 80", 126
		}
		h.containers[CaddyContainerName] = &fakeProxyContainer{running: !h.crashes, id: h.newID()}
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
		data, ok := h.files[stateFile(command)]
		if !ok {
			return "", "", 1
		}
		return data, "", 0
	case strings.Contains(command, "cat > "):
		data, _ := io.ReadAll(stdin)
		if name := stateFile(command); name != "" {
			h.files[name] = string(data)
		}
		return "", "", 0
	case strings.HasPrefix(command, "rm -f ") && stateFile(command) != "":
		for _, name := range []string{CaddyConfigFileName, proxyRollbackFileName} {
			if strings.Contains(command, "/"+name) {
				delete(h.files, name)
			}
		}
		return "", "", 0
	default:
		return "", "", 0
	}
}

// sawCommand reports whether the host received command.
func (h *fakeProxyHost) sawCommand(command string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.commands, command)
}

// lockSessions counts the Caddy lock sessions the host served.
func (h *fakeProxyHost) lockSessions() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, command := range h.commands {
		if command == "flock caddy.lock" {
			count++
		}
	}
	return count
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
				if strings.Contains(payload.Command, "echo LOCKED") {
					exit := h.lock(channel)
					_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{uint32(exit)}))
					return
				}
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
	if _, kept := h.files[proxyRollbackFileName]; kept {
		t.Fatal("rollback state was kept after a successful migration")
	}
	if got := h.lockSessions(); got != 1 {
		t.Fatalf("Boot() opened %d Caddy lock sessions, want the whole migration under one", got)
	}
}

func TestRebootMigratesOutdatedProxyUnderOneLock(t *testing.T) {
	h := newFakeProxyHost(legacyTestAutosave, map[string]*fakeProxyContainer{
		CaddyContainerName: {legacy: true, running: true},
	})
	manager, _ := startFakeProxySSH(t, h)

	if err := manager.Reboot("127.0.0.1", nil); err != nil {
		t.Fatalf("Reboot() error = %v", err)
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
	if got := h.lockSessions(); got != 1 {
		t.Fatalf("Reboot() opened %d Caddy lock sessions, want one", got)
	}
}

// Two services deploying to one host hold different deployment locks, so
// both can Boot an outdated proxy at once. Unless the migration holds the
// host-wide Caddy lock throughout, the second Boot takes the first one's
// azud-proxy-legacy for an interrupted migration and renames it back, and
// the first then cannot create its replacement.
func TestBootMigrationExcludesConcurrentBoot(t *testing.T) {
	h := newFakeProxyHost(legacyTestAutosave, map[string]*fakeProxyContainer{
		CaddyContainerName: {legacy: true, running: true},
	})
	first, _ := startFakeProxySSH(t, h)
	second, _ := startFakeProxySSH(t, h)

	secondErr := make(chan error, 1)
	h.beforeRun = func() {
		go func() { secondErr <- second.Boot("127.0.0.1", nil) }()
		// Hold the first Boot between moving the old proxy aside and creating
		// its replacement until the second waits for the lock or interferes.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && !h.waitingForLock() && !h.sawCommand("podman rename azud-proxy-legacy azud-proxy") {
			time.Sleep(time.Millisecond)
		}
	}

	if err := first.Boot("127.0.0.1", nil); err != nil {
		t.Fatalf("first Boot() error = %v", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("second Boot() error = %v", err)
	}
	want := []string{
		"podman stop -t 30 azud-proxy",
		"podman rename azud-proxy azud-proxy-legacy",
		"podman run --name azud-proxy",
		"podman rm -f azud-proxy-legacy",
	}
	if got := h.podmanCommands(); !slices.Equal(got, want) {
		t.Fatalf("podman commands = %q, want one migration and nothing from the second Boot", got)
	}
	if len(h.containers) != 1 || h.containers[CaddyContainerName] == nil || h.containers[CaddyContainerName].legacy || !h.containers[CaddyContainerName].running {
		t.Fatalf("containers after concurrent Boots = %#v", h.containers)
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
			if _, kept := h.files[proxyRollbackFileName]; kept {
				t.Fatal("rollback state was kept after a completed rollback")
			}
		})
	}
}

// A Boot interrupted after it stopped the previous proxy leaves that
// container's running state and TCP autosave only on the host: the volume
// may already hold the socket autosave and the container is stopped or
// renamed. A retry must restore both whether its own replacement fails or
// succeeds.
func TestBootRetriesInterruptedMigrationWithDurableRollbackState(t *testing.T) {
	interruptions := []struct {
		name string
		at   string
	}{
		{name: "after stop", at: "podman rename azud-proxy azud-proxy-legacy"},
		{name: "after rename", at: "autosave.json.azud-tmp"},
		{name: "after socket autosave", at: "--name azud-proxy "},
	}
	for _, interruption := range interruptions {
		for _, retryFails := range []bool{true, false} {
			name := interruption.name + "/retry succeeds"
			if retryFails {
				name = interruption.name + "/retry fails"
			}
			t.Run(name, func(t *testing.T) {
				h := newFakeProxyHost(legacyTestAutosave, map[string]*fakeProxyContainer{
					CaddyContainerName: {legacy: true, running: true},
				})
				h.interruptAt = interruption.at
				manager, _ := startFakeProxySSH(t, h)
				if err := manager.Boot("127.0.0.1", nil); err == nil {
					t.Fatal("interrupted Boot() succeeded")
				}
				h.mu.Lock()
				h.interruptAt, h.interrupted, h.runFails = "", false, retryFails
				h.mu.Unlock()
				if container := h.containers[CaddyContainerName]; container != nil && container.running {
					t.Fatalf("interruption left the previous proxy running: %#v", h.containers)
				}

				err := manager.Boot("127.0.0.1", nil)
				if _, kept := h.files[proxyRollbackFileName]; kept {
					t.Fatal("rollback state was kept after the retry finished")
				}
				container := h.containers[CaddyContainerName]
				if !retryFails {
					if err != nil {
						t.Fatalf("retried Boot() error = %v", err)
					}
					if len(h.containers) != 1 || container == nil || container.legacy || !container.running {
						t.Fatalf("containers after retried migration = %#v", h.containers)
					}
					if !strings.Contains(h.autosave, CaddyAdminSocket) || !strings.Contains(h.autosave, "azud-route-shop") {
						t.Fatalf("autosave was not normalized with its routes: %s", h.autosave)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "rolled back to its previous, outdated container") {
					t.Fatalf("retried Boot() error = %v, want the rollback reported", err)
				}
				if len(h.containers) != 1 || container == nil || !container.legacy || !container.running {
					t.Fatalf("previous proxy was not restored and restarted: %#v", h.containers)
				}
				if h.autosave != legacyTestAutosave {
					t.Fatalf("autosave = %q, want the previous container's %q", h.autosave, legacyTestAutosave)
				}
			})
		}
	}
}

// Rollback state recorded for another container, such as one a completed
// migration failed to delete, must not replace the current container's.
func TestBootMigrationIgnoresRollbackStateOfAnotherContainer(t *testing.T) {
	h := newFakeProxyHost(legacyTestAutosave, map[string]*fakeProxyContainer{
		CaddyContainerName: {legacy: true, running: true},
	})
	h.files[proxyRollbackFileName] = `{"container":"0123","running":false,"snapshotted":true,"autosave":"{\"apps\":{}}"}`
	h.runFails = true
	manager, _ := startFakeProxySSH(t, h)

	if err := manager.Boot("127.0.0.1", nil); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("Boot() error = %v, want the rollback reported", err)
	}
	restored := h.containers[CaddyContainerName]
	if len(h.containers) != 1 || restored == nil || !restored.legacy || !restored.running {
		t.Fatalf("previous proxy was not restored and restarted: %#v", h.containers)
	}
	if h.autosave != legacyTestAutosave {
		t.Fatalf("autosave = %q, want the previous container's %q", h.autosave, legacyTestAutosave)
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
