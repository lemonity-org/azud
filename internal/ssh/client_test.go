package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func startBlackholeSSHListener(t *testing.T) (port int) {
	t.Helper()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skipf("local TCP listeners unavailable in this sandbox: %v", err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		_ = listener.Close()
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				<-stop
				_ = conn.Close()
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func TestConnectHonorsCancellationDuringSSHHandshake(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	port := startBlackholeSSHListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient(&Config{
		Context:               ctx,
		Port:                  port,
		ConnectTimeout:        5 * time.Second,
		InsecureIgnoreHostKey: true,
	})
	t.Cleanup(func() { _ = client.Close() })
	time.AfterFunc(75*time.Millisecond, cancel)

	started := time.Now()
	_, err := client.Connect("127.0.0.1")
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Connect error = %v, want context cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled connect took %s", elapsed)
	}
}

func TestDifferentHostConnectionsAreNotGloballySerialized(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	port := startBlackholeSSHListener(t)
	client := NewClient(&Config{
		Port:                  port,
		ConnectTimeout:        250 * time.Millisecond,
		InsecureIgnoreHostKey: true,
	})
	t.Cleanup(func() { _ = client.Close() })

	hosts := []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}
	errs := make(chan error, len(hosts))
	var wg sync.WaitGroup
	started := time.Now()
	for _, host := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			_, err := client.Connect(host)
			errs <- err
		}(host)
	}
	wg.Wait()
	elapsed := time.Since(started)
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("expected blackhole connection to time out")
		}
	}
	if elapsed > 550*time.Millisecond {
		t.Fatalf("three host connections took %s; expected one timeout window", elapsed)
	}
}

// testPublicKey generates a fresh ed25519 SSH public key and returns it along
// with its SHA256 fingerprint (the same format used in trusted_host_fingerprints).
func testPublicKey(t *testing.T) (ssh.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate ed25519 key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("failed to wrap public key: %v", err)
	}
	return sshPub, ssh.FingerprintSHA256(sshPub)
}

// rejectFallback returns a fallback callback that fails the test if invoked.
// Use it when a match must come from the fingerprint map, not known_hosts.
func rejectFallback(t *testing.T) ssh.HostKeyCallback {
	t.Helper()
	return func(string, net.Addr, ssh.PublicKey) error {
		t.Error("fallback callback should not have been called")
		return errors.New("unexpected fallback")
	}
}

// countingFallback returns a callback that records whether it was called and
// returns the supplied error.
func countingFallback(called *bool, ret error) ssh.HostKeyCallback {
	return func(string, net.Addr, ssh.PublicKey) error {
		*called = true
		return ret
	}
}

func TestGetHostKeyCallback_InsecureIgnore(t *testing.T) {
	c := NewClient(&Config{InsecureIgnoreHostKey: true})

	cb, err := c.getHostKeyCallback()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	key, _ := testPublicKey(t)
	if err := cb("anything:22", nil, key); err != nil {
		t.Errorf("insecure callback should accept any key, got: %v", err)
	}
}

func TestGetHostKeyCallback_RequireFingerprintsButNoneConfigured(t *testing.T) {
	// Point known_hosts at an empty temp file so callback construction succeeds
	// and we exercise the RequireTrustedFingerprints guard specifically.
	knownHosts := t.TempDir() + "/known_hosts"
	c := NewClient(&Config{
		KnownHostsFile:             knownHosts,
		RequireTrustedFingerprints: true,
	})

	if _, err := c.getHostKeyCallback(); err == nil {
		t.Error("expected error when require_trusted_fingerprints is set but none are configured")
	}
}

func TestFingerprintCheckingCallback_Match(t *testing.T) {
	key, fp := testPublicKey(t)
	c := NewClient(&Config{
		TrustedHostFingerprints: map[string][]string{"example.com": {fp}},
	})

	cb := c.fingerprintCheckingCallback(rejectFallback(t))
	if err := cb("example.com:22", nil, key); err != nil {
		t.Errorf("expected matching fingerprint to be accepted, got: %v", err)
	}
}

func TestFingerprintCheckingCallback_Mismatch(t *testing.T) {
	key, _ := testPublicKey(t)
	_, otherFP := testPublicKey(t)
	c := NewClient(&Config{
		TrustedHostFingerprints: map[string][]string{"example.com": {otherFP}},
	})

	cb := c.fingerprintCheckingCallback(rejectFallback(t))
	if err := cb("example.com:22", nil, key); err == nil {
		t.Error("expected mismatched fingerprint to be rejected")
	}
}

func TestFingerprintCheckingCallback_MultipleAcceptsAny(t *testing.T) {
	key, fp := testPublicKey(t)
	_, otherFP := testPublicKey(t)
	c := NewClient(&Config{
		TrustedHostFingerprints: map[string][]string{"example.com": {otherFP, fp}},
	})

	cb := c.fingerprintCheckingCallback(rejectFallback(t))
	if err := cb("example.com:22", nil, key); err != nil {
		t.Errorf("expected match against any listed fingerprint, got: %v", err)
	}
}

func TestFingerprintCheckingCallback_LookupSpecificity(t *testing.T) {
	tests := []struct {
		name        string
		configKey   string
		hostname    string
		shouldMatch bool
	}{
		{"plain host matches", "example.com", "example.com:22", true},
		{"exact hostname passthrough", "example.com:22", "example.com:22", true},
		{"bracketed non-standard port", "[example.com]:2222", "example.com:2222", true},
		{"plain host matches non-standard port", "example.com", "example.com:2222", true},
		{"bracketed key ignored for standard port", "[example.com]:22", "example.com:22", false},
		{"different host does not match", "other.com", "example.com:22", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, fp := testPublicKey(t)
			c := NewClient(&Config{
				TrustedHostFingerprints: map[string][]string{tt.configKey: {fp}},
			})

			var fallbackCalled bool
			cb := c.fingerprintCheckingCallback(countingFallback(&fallbackCalled, errors.New("no match")))
			err := cb(tt.hostname, nil, key)

			if tt.shouldMatch {
				if err != nil {
					t.Errorf("expected fingerprint lookup to match, got: %v", err)
				}
				if fallbackCalled {
					t.Error("fallback should not be used when fingerprint matches")
				}
			} else {
				// No fingerprint found for this host, so the callback defers to
				// the fallback (which we made fail here).
				if err == nil {
					t.Error("expected non-match to fail via fallback")
				}
				if !fallbackCalled {
					t.Error("expected fallback to be invoked for unconfigured lookup")
				}
			}
		})
	}
}

func TestFingerprintCheckingCallback_FallbackWhenNoFingerprintForHost(t *testing.T) {
	key, fp := testPublicKey(t)
	c := NewClient(&Config{
		TrustedHostFingerprints: map[string][]string{"other.com": {fp}},
	})

	var fallbackCalled bool
	cb := c.fingerprintCheckingCallback(countingFallback(&fallbackCalled, nil))

	// Host has no configured fingerprint and RequireTrustedFingerprints is
	// false, so verification must defer to the known_hosts fallback.
	if err := cb("example.com:22", nil, key); err != nil {
		t.Errorf("expected fallback to accept, got: %v", err)
	}
	if !fallbackCalled {
		t.Error("expected fallback to be used for unconfigured host")
	}
}

func TestFingerprintCheckingCallback_RequireRejectsUnconfiguredHost(t *testing.T) {
	key, fp := testPublicKey(t)
	c := NewClient(&Config{
		TrustedHostFingerprints:    map[string][]string{"other.com": {fp}},
		RequireTrustedFingerprints: true,
	})

	// Fallback would accept, but require mode must reject before reaching it.
	cb := c.fingerprintCheckingCallback(rejectFallback(t))
	if err := cb("example.com:22", nil, key); err == nil {
		t.Error("expected rejection for host without a trusted fingerprint in require mode")
	}
}
