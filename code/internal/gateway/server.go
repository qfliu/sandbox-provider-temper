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

// handshakeTimeout bounds the SSH handshake (incl. authentication), so a
// client that connects and stalls can't hold a goroutine forever (the same
// slowloris lesson as the API's review finding M3). A var so tests can
// shorten it.
var handshakeTimeout = 30 * time.Second

// maxAuthTries limits public keys tried per connection.
const maxAuthTries = 6

// sandboxIDPattern matches the SSH user name: the sandbox id.
var sandboxIDPattern = regexp.MustCompile(`^sb-[0-9a-f]{12}$`)

// ErrNotASandbox means the SSH user name isn't a sandbox id.
var ErrNotASandbox = errors.New("user name must be a sandbox id like sb-0123456789ab")

// Server accepts SSH connections for sandboxes.
type Server struct {
	config *ssh.ServerConfig
	log    *slog.Logger

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// New returns a Server presenting hostKey to clients.
func New(hostKey ssh.Signer, log *slog.Logger) *Server {
	s := &Server{log: log, conns: map[net.Conn]struct{}{}}
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
	s.log.Info("login refused (authentication not implemented yet)",
		"user", meta.User(), "remote", meta.RemoteAddr().String(), "key", ssh.FingerprintSHA256(key))
	return nil, fmt.Errorf("access denied")
}

// Serve accepts connections on ln until ctx ends, then closes the listener
// and every open connection and waits for their goroutines.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.wg.Wait()
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		s.track(conn, true)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.track(conn, false)
			s.handle(conn)
		}()
	}
}

func (s *Server) track(c net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.conns[c] = struct{}{}
	} else {
		delete(s.conns, c)
	}
}

// handle runs the SSH handshake for one connection.
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(handshakeTimeout))
	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		s.log.Debug("handshake failed", "remote", conn.RemoteAddr().String(), "err", err)
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
