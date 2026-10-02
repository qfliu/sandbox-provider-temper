package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"sync/atomic"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// execReply is what the test server answers to an "exec" request.
type execReply struct {
	stdout, stderr string
	code           uint32
	noExitStatus   bool // simulate a session killed before it reported
}

// testServer is a tiny in-process SSH server: it accepts one client key,
// presents a given host key, answers exec with onExec, and serves SFTP from
// memory.
type testServer struct {
	addr    string
	dials   atomic.Int32 // connections accepted
	lastCmd atomic.Value // last exec command line
}

func startServer(t *testing.T, hostKeyPEM []byte, clientKey ssh.PublicKey, onExec func(cmd string) execReply) *testServer {
	t.Helper()
	hostSigner, err := ssh.ParsePrivateKey(hostKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) == string(clientKey.Marshal()) {
				return nil, nil
			}
			return nil, os.ErrPermission
		},
	}
	cfg.AddHostKey(hostSigner)
	if onExec == nil {
		onExec = func(string) execReply { return execReply{stdout: "ok"} }
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := &testServer{addr: ln.Addr().String()}
	srv.lastCmd.Store("")
	sftpFS := sftp.InMemHandler() // shared across connections, like a disk

	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				srv.dials.Add(1)
				go func() { // answer keepalives like OpenSSH does
					for r := range reqs {
						r.Reply(r.Type == "keepalive@openssh.com", nil)
					}
				}()
				for nch := range chans {
					ch, creqs, err := nch.Accept()
					if err != nil {
						continue
					}
					go serveChannel(srv, ch, creqs, onExec, sftpFS)
				}
			}()
		}
	}()
	return srv
}

func serveChannel(srv *testServer, ch ssh.Channel, reqs <-chan *ssh.Request, onExec func(string) execReply, fs sftp.Handlers) {
	for r := range reqs {
		switch r.Type {
		case "exec":
			var p struct{ Cmd string }
			ssh.Unmarshal(r.Payload, &p)
			srv.lastCmd.Store(p.Cmd)
			r.Reply(true, nil)
			rep := onExec(p.Cmd)
			ch.Write([]byte(rep.stdout))
			ch.Stderr().Write([]byte(rep.stderr))
			if !rep.noExitStatus {
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{rep.code}))
			}
			ch.Close()
			return
		case "subsystem":
			r.Reply(true, nil)
			s := sftp.NewRequestServer(ch, fs)
			s.Serve()
			s.Close()
			return
		default:
			r.Reply(false, nil)
		}
	}
}

func newClientSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
