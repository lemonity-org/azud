package ssh

import (
	"sync"
	"time"
)

// Pool manages a pool of SSH connections
type Pool struct {
	connections map[string]*Connection
	mu          sync.RWMutex
	maxIdle     time.Duration
	cleanupDone chan struct{}
}

// NewPool creates a new connection pool
func NewPool() *Pool {
	p := &Pool{
		connections: make(map[string]*Connection),
		maxIdle:     5 * time.Minute,
		cleanupDone: make(chan struct{}),
	}

	// Start background cleanup goroutine
	go p.cleanup()

	return p
}

// Get retrieves a connection from the pool
func (p *Pool) Get(host string) *Connection {
	p.mu.RLock()
	defer p.mu.RUnlock()

	conn, ok := p.connections[host]
	if !ok {
		return nil
	}

	// Check if connection is still alive
	if !conn.IsAlive() {
		// Remove stale connection (will be done asynchronously)
		go func() {
			p.mu.Lock()
			delete(p.connections, host)
			p.mu.Unlock()
			conn.Close()
		}()
		return nil
	}

	return conn
}

// Put adds a connection to the pool
func (p *Pool) Put(host string, conn *Connection) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Close existing connection if any
	if existing, ok := p.connections[host]; ok {
		existing.Close()
	}

	p.connections[host] = conn
}

// Remove removes a connection from the pool
func (p *Pool) Remove(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if conn, ok := p.connections[host]; ok {
		conn.Close()
		delete(p.connections, host)
	}
}

// CloseAll closes all connections in the pool
func (p *Pool) CloseAll() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Signal cleanup goroutine to stop
	close(p.cleanupDone)

	var lastErr error
	for host, conn := range p.connections {
		if err := conn.Close(); err != nil {
			lastErr = err
		}
		delete(p.connections, host)
	}

	return lastErr
}

// Size returns the number of connections in the pool
func (p *Pool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.connections)
}

// Hosts returns all hosts with active connections
func (p *Pool) Hosts() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	hosts := make([]string, 0, len(p.connections))
	for host := range p.connections {
		hosts = append(hosts, host)
	}
	return hosts
}

// cleanup periodically removes idle connections
func (p *Pool) cleanup() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-p.cleanupDone:
			return
		case <-ticker.C:
			p.removeIdleConnections()
		}
	}
}

// removeIdleConnections removes connections that have been idle too long
func (p *Pool) removeIdleConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for host, conn := range p.connections {
		if now.Sub(conn.LastUsed()) > p.maxIdle {
			conn.Close()
			delete(p.connections, host)
		}
	}
}

// SetMaxIdle sets the maximum idle duration for connections
func (p *Pool) SetMaxIdle(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxIdle = d
}

// Stats returns pool statistics
func (p *Pool) Stats() PoolStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	stats := PoolStats{
		TotalConnections: len(p.connections),
		Hosts:            make([]string, 0, len(p.connections)),
	}

	for host, conn := range p.connections {
		stats.Hosts = append(stats.Hosts, host)
		if conn.IsAlive() {
			stats.AliveConnections++
		}
	}

	return stats
}

// PoolStats holds pool statistics
type PoolStats struct {
	TotalConnections int
	AliveConnections int
	Hosts            []string
}
