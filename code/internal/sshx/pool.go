package sshx

import (
	"context"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// IdleTimeout closes pooled connections nobody has used for this long.
const IdleTimeout = 5 * time.Minute

// Pool keeps one SSH connection per sandbox and reuses it, so each exec or
// file transfer skips the TCP + SSH handshake. A connection carries many
// sessions at once (sshd allows 64 per connection, see sshd_config).
type Pool struct {
	dialer Dialer
	idle   time.Duration
	mu     sync.Mutex
	conns  map[string]*pooledConn
}

type pooledConn struct {
	client   *ssh.Client
	addr     string
	hostKey  string
	inUse    int       // requests currently using it; never closed as idle while > 0
	lastUsed time.Time // when the last user released it
}

// NewPool returns an empty pool that dials with d.
func NewPool(d Dialer) *Pool {
	return &Pool{dialer: d, idle: IdleTimeout, conns: map[string]*pooledConn{}}
}

// Get returns a connection to sandbox id at addr, verified against hostKey,
// reusing a pooled one when its address and host key still match. The
// caller must call release when done, so CloseIdle never closes a
// connection mid-exec (review finding H2).
func (p *Pool) Get(ctx context.Context, id, addr, hostKey string) (client *ssh.Client, release func(), err error) {
	p.mu.Lock()
	if pc, ok := p.conns[id]; ok {
		if pc.addr == addr && pc.hostKey == hostKey {
			pc.inUse++
			p.mu.Unlock()
			return pc.client, p.releaser(pc), nil
		}
		// The sandbox moved (new pod IP): drop the stale connection.
		pc.client.Close()
		delete(p.conns, id)
	}
	p.mu.Unlock()

	// Dial without holding the lock, so one slow sandbox doesn't block others.
	client, err = p.dialer.Dial(ctx, addr, hostKey)
	if err != nil {
		return nil, nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if pc, ok := p.conns[id]; ok && pc.addr == addr && pc.hostKey == hostKey {
		client.Close() // another request connected first; use theirs
		pc.inUse++
		return pc.client, p.releaser(pc), nil
	}
	pc := &pooledConn{client: client, addr: addr, hostKey: hostKey, inUse: 1}
	p.conns[id] = pc
	return client, p.releaser(pc), nil
}

// releaser returns a func that marks one use of pc finished (safe to call
// more than once).
func (p *Pool) releaser(pc *pooledConn) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			pc.inUse--
			pc.lastUsed = time.Now()
		})
	}
}

// Drop closes and forgets the connection to a sandbox (after an error, or
// when the sandbox ends).
func (p *Pool) Drop(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pc, ok := p.conns[id]; ok {
		pc.client.Close()
		delete(p.conns, id)
	}
}

// Len reports how many connections are pooled.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// CloseIdle closes connections nobody is using and nobody has used for
// longer than the idle timeout.
func (p *Pool) CloseIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, pc := range p.conns {
		if pc.inUse == 0 && time.Since(pc.lastUsed) > p.idle {
			pc.client.Close()
			delete(p.conns, id)
		}
	}
}

// Run calls CloseIdle every minute until ctx ends, then closes everything.
func (p *Pool) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.mu.Lock()
			for id, pc := range p.conns {
				pc.client.Close()
				delete(p.conns, id)
			}
			p.mu.Unlock()
			return
		case <-t.C:
			p.CloseIdle()
		}
	}
}
