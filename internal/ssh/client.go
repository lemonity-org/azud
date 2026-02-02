package ssh

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Client manages SSH connections to remote hosts
type Client struct {
	config    *Config
	pool      *Pool
	agentConn net.Conn // SSH agent connection, closed on Client.Close()
	mu        sync.RWMutex
}

// Config holds SSH client configuration
type Config struct {
	// SSH username
	User string

	// SSH port
	Port int

	// SSH key file paths
	Keys []string

	// Connection timeout
	ConnectTimeout time.Duration

	// Command execution timeout (0 = no timeout)
	CommandTimeout time.Duration

	// Proxy/bastion host configuration
	Proxy *ProxyConfig

	// Known hosts file path (empty for default)
	KnownHostsFile string

	// Trusted host fingerprints by hostname (SHA256:...)
	// If set, these are checked BEFORE known_hosts
	TrustedHostFingerprints map[string][]string

	// Require trusted fingerprints to match (reject if no fingerprint configured)
	RequireTrustedFingerprints bool

	// Skip host key verification (not recommended for production)
	InsecureIgnoreHostKey bool
}

// ProxyConfig holds SSH proxy/bastion configuration
type ProxyConfig struct {
	Host string
	User string
	Port int
	Keys []string
}

// NewClient creates a new SSH client with the given configuration
func NewClient(cfg *Config) *Client {
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	if cfg.User == "" {
		cfg.User = currentUsername()
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 30 * time.Second
	}

	return &Client{
		config: cfg,
		pool:   NewPool(),
	}
}

// Connect establishes a connection to the given host
func (c *Client) Connect(host string) (*Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check pool for existing connection
	if conn := c.pool.Get(host); conn != nil {
		return conn, nil
	}

	// Build SSH client config
	sshConfig, err := c.buildSSHConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build SSH config: %w", err)
	}

	// Connect through proxy if configured
	var client *ssh.Client
	if c.config.Proxy != nil && c.config.Proxy.Host != "" {
		client, err = c.connectViaProxy(host, sshConfig)
	} else {
		client, err = c.connectDirect(host, sshConfig)
	}

	if err != nil {
		return nil, err
	}

	// Create and store connection
	conn := &Connection{
		host:           host,
		client:         client,
		lastUsed:       time.Now(),
		commandTimeout: c.config.CommandTimeout,
	}

	c.pool.Put(host, conn)
	return conn, nil
}

// connectDirect establishes a direct SSH connection
func (c *Client) connectDirect(host string, sshConfig *ssh.ClientConfig) (*ssh.Client, error) {
	addr := fmt.Sprintf("%s:%d", host, c.config.Port)
	client, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}
	return client, nil
}

// connectViaProxy establishes an SSH connection through a bastion host
func (c *Client) connectViaProxy(host string, sshConfig *ssh.ClientConfig) (*ssh.Client, error) {
	// Build proxy SSH config
	proxyConfig, err := c.buildProxySSHConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build proxy SSH config: %w", err)
	}

	// Connect to proxy
	proxyPort := c.config.Proxy.Port
	if proxyPort == 0 {
		proxyPort = 22
	}
	proxyAddr := fmt.Sprintf("%s:%d", c.config.Proxy.Host, proxyPort)

	proxyClient, err := ssh.Dial("tcp", proxyAddr, proxyConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to proxy %s: %w", proxyAddr, err)
	}

	// Connect to target through proxy
	targetAddr := fmt.Sprintf("%s:%d", host, c.config.Port)
	conn, err := proxyClient.Dial("tcp", targetAddr)
	if err != nil {
		_ = proxyClient.Close()
		return nil, fmt.Errorf("failed to connect to %s via proxy: %w", targetAddr, err)
	}

	ncc, chans, reqs, err := ssh.NewClientConn(conn, targetAddr, sshConfig)
	if err != nil {
		_ = conn.Close()
		_ = proxyClient.Close()
		return nil, fmt.Errorf("failed to create SSH connection to %s: %w", targetAddr, err)
	}

	return ssh.NewClient(ncc, chans, reqs), nil
}

// buildSSHConfig creates an ssh.ClientConfig from the client configuration
func (c *Client) buildSSHConfig() (*ssh.ClientConfig, error) {
	authMethods, err := c.getAuthMethods(c.config.Keys)
	if err != nil {
		return nil, err
	}

	hostKeyCallback, err := c.getHostKeyCallback()
	if err != nil {
		return nil, err
	}

	return &ssh.ClientConfig{
		User:            c.config.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         c.config.ConnectTimeout,
	}, nil
}

// buildProxySSHConfig creates an ssh.ClientConfig for the proxy connection
func (c *Client) buildProxySSHConfig() (*ssh.ClientConfig, error) {
	keys := c.config.Proxy.Keys
	if len(keys) == 0 {
		keys = c.config.Keys
	}

	authMethods, err := c.getAuthMethods(keys)
	if err != nil {
		return nil, err
	}

	hostKeyCallback, err := c.getHostKeyCallback()
	if err != nil {
		return nil, err
	}

	user := c.config.Proxy.User
	if user == "" {
		user = c.config.User
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         c.config.ConnectTimeout,
	}, nil
}

// getAuthMethods returns the authentication methods to use
func (c *Client) getAuthMethods(keyPaths []string) ([]ssh.AuthMethod, error) {
	var authMethods []ssh.AuthMethod

	// Try SSH agent first
	if agentAuth := c.getAgentAuth(); agentAuth != nil {
		authMethods = append(authMethods, agentAuth)
	}

	// Add key file authentication
	for _, keyPath := range keyPaths {
		expandedPath := expandPath(keyPath)
		if _, err := os.Stat(expandedPath); os.IsNotExist(err) {
			continue
		}

		signer, err := c.loadPrivateKey(expandedPath)
		if err != nil {
			continue
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	// Try default key locations if no keys specified
	if len(keyPaths) == 0 {
		defaultKeys := []string{
			"~/.ssh/id_ed25519",
			"~/.ssh/id_rsa",
			"~/.ssh/id_ecdsa",
		}
		for _, keyPath := range defaultKeys {
			expandedPath := expandPath(keyPath)
			if _, err := os.Stat(expandedPath); os.IsNotExist(err) {
				continue
			}

			signer, err := c.loadPrivateKey(expandedPath)
			if err != nil {
				continue
			}
			authMethods = append(authMethods, ssh.PublicKeys(signer))
		}
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication methods available")
	}

	return authMethods, nil
}

// getAgentAuth returns SSH agent authentication if available.
// The agent connection is stored in c.agentConn and closed when c.Close() is called.
func (c *Client) getAgentAuth() ssh.AuthMethod {
	// If we already have an agent connection, reuse it
	if c.agentConn != nil {
		agentClient := agent.NewClient(c.agentConn)
		return ssh.PublicKeysCallback(agentClient.Signers)
	}

	socket := os.Getenv("SSH_AUTH_SOCK")
	if socket == "" {
		return nil
	}

	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil
	}

	agentClient := agent.NewClient(conn)

	// Check if agent has any keys before adding it as auth method
	signers, err := agentClient.Signers()
	if err != nil || len(signers) == 0 {
		_ = conn.Close()
		return nil
	}

	// Store the connection to close it later
	c.agentConn = conn

	return ssh.PublicKeysCallback(agentClient.Signers)
}

// loadPrivateKey loads a private key from a file
func (c *Client) loadPrivateKey(path string) (ssh.Signer, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file %s: %w", path, err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		// Try with empty passphrase first, then give up
		// In a real implementation, we'd prompt for passphrase
		return nil, fmt.Errorf("failed to parse key file %s: %w", path, err)
	}

	return signer, nil
}

// getHostKeyCallback returns the host key callback function.
// If TrustedHostFingerprints is configured, it checks fingerprints first.
// Falls back to known_hosts if no fingerprint match and RequireTrustedFingerprints is false.
func (c *Client) getHostKeyCallback() (ssh.HostKeyCallback, error) {
	if c.config.InsecureIgnoreHostKey {
		return ssh.InsecureIgnoreHostKey(), nil
	}

	// Prepare known_hosts callback as fallback
	var knownHostsCallback ssh.HostKeyCallback
	knownHostsPath := c.config.KnownHostsFile
	if knownHostsPath == "" {
		knownHostsPath = expandPath("~/.ssh/known_hosts")
	}

	// Check if known_hosts file exists
	if _, err := os.Stat(knownHostsPath); os.IsNotExist(err) {
		// Create the file if it doesn't exist
		dir := filepath.Dir(knownHostsPath)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("failed to create SSH directory: %w", err)
		}
		if _, err := os.Create(knownHostsPath); err != nil {
			return nil, fmt.Errorf("failed to create known_hosts file: %w", err)
		}
	}

	callback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load known_hosts: %w", err)
	}
	knownHostsCallback = callback

	// If no trusted fingerprints configured, just use known_hosts
	if len(c.config.TrustedHostFingerprints) == 0 {
		if c.config.RequireTrustedFingerprints {
			return nil, fmt.Errorf("require_trusted_fingerprints is enabled but no trusted_host_fingerprints configured")
		}
		return knownHostsCallback, nil
	}

	// Return a composite callback that checks fingerprints first
	return c.fingerprintCheckingCallback(knownHostsCallback), nil
}

// fingerprintCheckingCallback returns a HostKeyCallback that verifies host keys
// against configured trusted fingerprints before falling back to known_hosts.
func (c *Client) fingerprintCheckingCallback(fallback ssh.HostKeyCallback) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		// Extract hostname and port
		host := hostname
		port := ""
		if h, p, err := net.SplitHostPort(hostname); err == nil {
			host = h
			port = p
		}

		// Compute the SHA256 fingerprint of the presented key
		fingerprint := ssh.FingerprintSHA256(key)

		// Look up fingerprints in order of specificity:
		// 1. Exact hostname:port as passed by SSH (e.g., "example.com:22")
		// 2. Bracketed format "[host]:port" for non-standard ports (e.g., "[example.com]:2222")
		// 3. Plain host without port
		lookupKeys := []string{hostname}
		if port != "" && port != "22" {
			lookupKeys = append(lookupKeys, fmt.Sprintf("[%s]:%s", host, port))
		}
		lookupKeys = append(lookupKeys, host)

		var trustedFPs []string
		var matchedKey string
		for _, k := range lookupKeys {
			if fps, ok := c.config.TrustedHostFingerprints[k]; ok && len(fps) > 0 {
				trustedFPs = fps
				matchedKey = k
				break
			}
		}

		if len(trustedFPs) > 0 {
			// Check if the presented fingerprint matches any trusted one
			for _, trusted := range trustedFPs {
				if fingerprint == trusted {
					// Fingerprint matches - accept the connection
					return nil
				}
			}
			// We have fingerprints configured but none match - reject
			return fmt.Errorf("host key fingerprint mismatch for %s: got %s, expected one of %v",
				matchedKey, fingerprint, trustedFPs)
		}

		// No fingerprints configured for this specific host
		if c.config.RequireTrustedFingerprints {
			return fmt.Errorf("no trusted fingerprint configured for host %s (got %s)", host, fingerprint)
		}

		// Fall back to known_hosts verification
		return fallback(hostname, remote, key)
	}
}

// Execute runs a command on the given host and returns the output
func (c *Client) Execute(host, cmd string) (*Result, error) {
	conn, err := c.Connect(host)
	if err != nil {
		return nil, err
	}

	return conn.Execute(cmd)
}

// ExecuteWithStdin runs a command on the remote host with provided stdin.
func (c *Client) ExecuteWithStdin(host, cmd string, stdin io.Reader) (*Result, error) {
	conn, err := c.Connect(host)
	if err != nil {
		return nil, err
	}

	return conn.ExecuteWithStdin(cmd, stdin)
}

// ExecuteParallel runs a command on multiple hosts concurrently
func (c *Client) ExecuteParallel(hosts []string, cmd string) []*Result {
	results := make([]*Result, len(hosts))
	var wg sync.WaitGroup

	for i, host := range hosts {
		wg.Add(1)
		go func(idx int, h string) {
			defer wg.Done()
			result, err := c.Execute(h, cmd)
			if err != nil {
				results[idx] = &Result{
					Host:     h,
					ExitCode: -1,
					Stderr:   err.Error(),
					Error:    err,
				}
				return
			}
			result.Host = h
			results[idx] = result
		}(i, host)
	}

	wg.Wait()
	return results
}

// Upload copies a local file to the remote host
func (c *Client) Upload(host, localPath, remotePath string) error {
	conn, err := c.Connect(host)
	if err != nil {
		return err
	}

	return conn.Upload(localPath, remotePath)
}

// Download copies a remote file to the local host
func (c *Client) Download(host, remotePath, localPath string) error {
	conn, err := c.Connect(host)
	if err != nil {
		return err
	}

	return conn.Download(remotePath, localPath)
}

// WithRemoteLock acquires an exclusive flock on the given remote host for the
// duration of fn. See Connection.WithRemoteLock for details.
func (c *Client) WithRemoteLock(host, lockFile string, timeout time.Duration, fn func() error) error {
	conn, err := c.Connect(host)
	if err != nil {
		return err
	}

	return conn.WithRemoteLock(lockFile, timeout, fn)
}

// Close closes all connections in the pool and the SSH agent connection
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Close SSH agent connection if open
	if c.agentConn != nil {
		_ = c.agentConn.Close()
		c.agentConn = nil
	}

	return c.pool.CloseAll()
}

// expandPath expands ~ to home directory
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// currentUsername returns the current OS user's username.
// Falls back to "root" if the user cannot be determined.
func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	// Fall back to checking USER environment variable
	if username := os.Getenv("USER"); username != "" {
		return username
	}
	// Last resort fallback
	return "root"
}
