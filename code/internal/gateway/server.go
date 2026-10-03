// Package gateway is the public SSH entry point (D6): `ssh sb-<id>@gateway`
// reaches sandbox sb-<id>. It authenticates the user's own key against the
// sandbox, then proxies the session to the sandbox's sshd using the
// platform-internal key and the sandbox's pinned host key (D7, D29).
package gateway

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"regexp"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// SandboxLookup finds a sandbox by id (implemented by store.Store; faked in
// tests).
type SandboxLookup interface {
	GetSandbox(ctx context.Context, id string) (store.Sandbox, error)
}

// lookupTimeout bounds the database lookup for one authentication attempt.
const lookupTimeout = 5 * time.Second

// permSandboxID carries the authenticated sandbox id from authentication to
// the session handler.
const permSandboxID = "sandbox-id"

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
	sandboxes        SandboxLookup
	log              *slog.Logger
	handshakeTimeout time.Duration

	mu      sync.Mutex
	closing bool // set once shutdown starts; new connections are refused
	conns   map[net.Conn]struct{}
	wg      sync.WaitGroup
}

// New returns a Server presenting hostKey to clients and checking logins
// against sandboxes.
func New(hostKey ssh.Signer, sandboxes SandboxLookup, log *slog.Logger) *Server {
	s := &Server{
		sandboxes:        sandboxes,
		log:              log,
		handshakeTimeout: defaultHandshakeTimeout,
		conns:            map[net.Conn]struct{}{},
	}
	s.config = &ssh.ServerConfig{
		MaxAuthTries:      maxAuthTries,
		PublicKeyCallback: s.authenticate,
		ServerVersion:     "SSH-2.0-sandbox-gateway",
	}
	s.config.AddHostKey(hostKey)
	return s
}

// errDenied is what every refused key gets: the client learns nothing about
// why (no such sandbox, not running, wrong key), only the server log does.
var errDenied = errors.New("access denied")

// authenticate lets a key in only if the user name is a sandbox id, that
// sandbox is running, and the key is exactly the sandbox owner's
// ssh_public_key (D3; owner only, admins use the API — D33).
func (s *Server) authenticate(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	id := meta.User()
	if !sandboxIDPattern.MatchString(id) {
		return nil, ErrNotASandbox
	}
	// Debug, not Info: an unauthenticated client may offer several keys per
	// connection; handle() logs one Info line per refused connection.
	refuse := func(reason string, args ...any) (*ssh.Permissions, error) {
		s.log.Debug("key refused", append([]any{"sandbox", id, "reason", reason,
			"remote", meta.RemoteAddr().String(), "key", ssh.FingerprintSHA256(key)}, args...)...)
		return nil, errDenied
	}

	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	sb, err := s.sandboxes.GetSandbox(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return refuse("no such sandbox")
	}
	if err != nil {
		s.log.Warn("sandbox lookup failed", "sandbox", id, "err", err)
		return nil, errDenied
	}
	if sb.Status != store.StatusRunning {
		return refuse("sandbox not running", "status", sb.Status)
	}
	owner, _, _, _, err := ssh.ParseAuthorizedKey([]byte(sb.SSHPublicKey))
	if err != nil {
		s.log.Error("stored ssh_public_key doesn't parse", "sandbox", id, "err", err)
		return nil, errDenied
	}
	if !bytes.Equal(owner.Marshal(), key.Marshal()) {
		return refuse("not the sandbox's key")
	}
	s.log.Info("login accepted", "sandbox", id, "remote", meta.RemoteAddr().String(),
		"key", ssh.FingerprintSHA256(key))
	return &ssh.Permissions{Extensions: map[string]string{permSandboxID: id}}, nil
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

	// Authenticated; proxying sessions into the sandbox arrives in step 3.3.
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		nch.Reject(ssh.Prohibited, "sessions are not proxied yet (step 3.3)")
	}
}
