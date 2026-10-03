// Command gateway is the SSH entry point for sandboxes (D6):
//
//	ssh sb-0123456789ab@<gateway>
//
// Settings (environment):
//
//	GATEWAY_LISTEN_ADDR     listen address (default :2222)
//	GATEWAY_HOST_KEY_FILE   the gateway's own SSH host key (required); keep it
//	                        stable so users' known_hosts entries stay valid
//	DATABASE_URL            Postgres, to look up sandboxes (required; read-only
//	                        use: the API owns the schema and its migrations)
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/ssh"

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
	"github.com/qfliu/sandbox-provider-temper/code/internal/db"
	"github.com/qfliu/sandbox-provider-temper/code/internal/gateway"
	"github.com/qfliu/sandbox-provider-temper/code/internal/sshx"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("gateway exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	addr := os.Getenv("GATEWAY_LISTEN_ADDR")
	if addr == "" {
		addr = ":2222"
	}
	keyFile := os.Getenv("GATEWAY_HOST_KEY_FILE")
	if keyFile == "" {
		return errors.New("GATEWAY_HOST_KEY_FILE is required (the gateway's SSH host key)")
	}
	hostKey, err := sshx.LoadSigner(keyFile)
	if err != nil {
		return err
	}
	dbURL, err := config.DatabaseURL()
	if err != nil {
		return err
	}

	// Stop on Ctrl-C locally or SIGTERM from Kubernetes. After the first
	// signal, stop capturing them, so a second Ctrl-C force-quits a
	// shutdown that hangs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }()

	pool, err := db.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Info("gateway listening", "addr", ln.Addr().String(),
		"host_key", ssh.FingerprintSHA256(hostKey.PublicKey()))
	if err := gateway.New(hostKey, store.New(pool), log).Serve(ctx, ln); err != nil {
		return err
	}
	log.Info("gateway stopped")
	return nil
}
