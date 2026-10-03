// Package gateway is the public SSH entry point (D6): `ssh sb-<id>@gateway`
// reaches sandbox sb-<id>. It authenticates the user's own key against the
// sandbox, then proxies the session to the sandbox's sshd using the
// platform-internal key and the sandbox's pinned host key (D7, D29).
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// defaultHandshakeTimeout bounds the SSH handshake (incl. authentication),
// so a client that connects and stalls can't hold a goroutine forever (the
// same slowloris lesson as the API's review finding M3).
const defaultHandshakeTimeout = 30 * time.Second

// maxAuthTries limits public keys tried per connection.
const maxAuthTries = 6

// sandboxIDPattern matches the SSH user name: the sandbox id.
var sandboxIDPattern = regexp.MustCompile(`^sb-[0-9a-f]{12}$`)

// ErrNotASandbox means the SSH user name isn't a sandbox id.
var ErrNotASandbox = errors.New("user name must be a sandbox id like sb-0123456789ab")

// Server accepts SSH connections for sandboxes.
type Server struct {
	config           *ssh.ServerConfig
	log              *slog.Logger
	handshakeTimeout time.Duration

	mu      sync.Mutex
	closing bool // set once shutdown starts; new connections are refused
	conns   map[net.Conn]struct{}
	wg      sync.WaitGroup
}

// New returns a Server presenting hostKey to clients.
func New(hostKey ssh.Signer, log *slog.Logger) *Server {
	s := &Server{log: log, handshakeTimeout: defaultHandshakeTimeout, conns: map[net.Conn]struct{}{}}
	s.config = &ssh.ServerConfig{
		MaxAuthTries:      maxAuthTries,
		PublicKeyCallback: s.authenticate,
		ServerVersion:     "SSH-2.0-sandbox-gateway",
	}
	s.config.AddHostKey(hostKey)
	return s
}

// authenticate decides whether a client key may open the sandbox named by
// the user name. Step 3.1 only validates the name; key checking against the
// sandbox's stored key arrives in step 3.2, so for now everything is denied.
func (s *Server) authenticate(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	if !sandboxIDPattern.MatchString(meta.User()) {
		return nil, ErrNotASandbox
	}
	// Debug, not Info: an unauthenticated client may offer several keys per
	// connection; handle() logs one Info line per refused connection.
	s.log.Debug("key offered", "user", meta.User(), "remote", meta.RemoteAddr().String(),
		"key", ssh.FingerprintSHA256(key))
	return nil, fmt.Errorf("access denied")
}

// Serve accepts connections on ln until ctx ends, then closes the listener
// and every open connection and waits for their goroutines. Accept errors
// other than shutdown (e.g. out of file descriptors under a flood) are
// retried with backoff instead of taking the gateway down.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		s.shutdown()
		ln.Close()
	}()
	// On any exit (including an unexpected listener failure): stop
	// listening, close open connections, wait for their goroutines.
	defer s.wg.Wait()
	defer s.shutdown()
	defer ln.Close()

	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err // closed by someone else: nothing more to accept
			}
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			s.log.Warn("accept failed; retrying", "err", err, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			continue
		}
		backoff = 0
		if !s.track(conn) {
			continue // shutting down
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(conn)
			s.handle(conn)
		}()
	}
}

// track records an accepted connection. During shutdown it closes the
// connection instead and returns false: without this check, a connection
// accepted just as shutdown swept the map would be missed and shutdown
// would wait for it.
func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		c.Close()
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

// shutdown refuses new connections and closes open ones (idempotent).
func (s *Server) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
	for c := range s.conns {
		c.Close()
	}
}

// handle runs the SSH handshake for one connection.
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(s.handshakeTimeout))
	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		s.log.Info("connection refused", "remote", conn.RemoteAddr().String(), "err", err)
		return
	}
	conn.SetDeadline(time.Time{})
	defer sconn.Close()

	// Unreachable until step 3.2 lets someone authenticate; sessions are
	// proxied in step 3.3.
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		nch.Reject(ssh.Prohibited, "not implemented yet")
	}
}
