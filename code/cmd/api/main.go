// Command api runs the sandbox control plane: the REST API agents call with
// an API key (and, from Phase 4, the dashboard).
//
// Usage:
//
//	api                                     serve the API (default)
//	api create-user --name NAME [--admin]   create a user and print their first API key
//	api create-key --user NAME [--name L]   print an additional API key for a user
//	api check-ssh --sandbox ID [--addr A]   SSH into a sandbox the way the platform does
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
	"github.com/qfliu/sandbox-provider-temper/code/internal/db"
	"github.com/qfliu/sandbox-provider-temper/code/internal/httpapi"
	"github.com/qfliu/sandbox-provider-temper/code/internal/k8s"
	"github.com/qfliu/sandbox-provider-temper/code/internal/sandbox"
	"github.com/qfliu/sandbox-provider-temper/code/internal/sshx"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log) // packages without an injected logger (e.g. store) log as JSON too

	var err error
	switch cmd := subcommand(); cmd {
	case "", "serve":
		err = serve(log)
	case "create-user":
		err = runCreateUser(context.Background(), os.Args[2:], os.Stdout)
	case "create-key":
		err = runCreateKey(context.Background(), os.Args[2:], os.Stdout)
	case "check-ssh":
		err = runCheckSSH(context.Background(), os.Args[2:], os.Stdout)
	default:
		err = fmt.Errorf("unknown command %q (want serve, create-user, create-key or check-ssh)", cmd)
	}
	if err != nil {
		log.Error("api exited", "err", err)
		os.Exit(1)
	}
}

func subcommand() string {
	if len(os.Args) < 2 {
		return ""
	}
	return os.Args[1]
}

// HTTP server limits (review finding M3). There's deliberately no overall
// ReadTimeout/WriteTimeout: exec may run 10 minutes and uploads stream up to
// 100 MiB, so per-request limits live in the handlers instead.
const (
	// readHeaderTimeout stops slow-header ("slowloris") clients from holding
	// connections open forever.
	readHeaderTimeout = 10 * time.Second
	// idleTimeout closes keep-alive connections nobody is using.
	idleTimeout = 2 * time.Minute
	// maxHeaderBytes caps request headers (Go's default is 1 MiB).
	maxHeaderBytes = 64 << 10
)

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

func serve(log *slog.Logger) error {
	cfg, err := config.LoadAPI()
	if err != nil {
		return err
	}

	// Stop on Ctrl-C locally or SIGTERM from Kubernetes.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Background workers (SSH pool, pod watcher, status syncer) get their own
	// context, cancelled only AFTER the HTTP server has drained: in-flight
	// execs need their SSH connections until they finish (review finding M4).
	bg, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	log.Info("database ready", "migrations_applied", applied)

	// The internal key the API logs into sandboxes with (D7).
	signer, err := sshx.LoadSigner(cfg.Sandbox.InternalKeyFile)
	if err != nil {
		return err
	}
	sshPool := sshx.NewPool(sshx.Dialer{Signer: signer})
	go sshPool.Run(bg) // closes idle connections; closes all once drained

	kube, err := k8s.NewClientset()
	if err != nil {
		return err
	}
	st := store.New(pool)
	pods := k8s.NewPods(kube, cfg.Sandbox)
	sandboxes := sandbox.NewService(st, pods, sshPool, cfg.Sandbox, log)

	// Keep sandbox statuses in line with their pods (live events + a
	// periodic reconcile). Both stop once the server has drained.
	syncer := sandbox.NewSyncer(st, pods, log)
	watcher, err := k8s.NewWatcher(kube, cfg.Sandbox.Namespace, syncer)
	if err != nil {
		return err
	}
	if err := watcher.Start(bg); err != nil {
		return err
	}
	syncer.SetCache(watcher)
	go syncer.Run(bg)
	// Delete sandboxes past their TTL or idle too long (D9).
	go sandboxes.RunReaper(bg, cfg.Sandbox.ReaperInterval)
	log.Info("sandbox status sync and reaper started", "namespace", cfg.Sandbox.Namespace,
		"idle_timeout", cfg.Sandbox.IdleTimeout, "reaper_interval", cfg.Sandbox.ReaperInterval)

	srv := newHTTPServer(cfg.ListenAddr, httpapi.NewHandler(httpapi.Deps{
		Log:       log,
		DB:        pool,
		Auth:      st,
		Sandboxes: sandboxes,
	}))

	errCh := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", cfg.ListenAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Stop accepting new requests and let in-flight ones finish, but not
	// forever. Running out of time is an expected outcome of shutting down
	// (e.g. a 10-minute exec), not a crash: close what's left and exit 0.
	log.Info("shutting down", "drain_timeout", cfg.ShutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("drain timed out; closing remaining connections", "err", err)
		srv.Close()
	}
	stopBackground() // only now close SSH connections and stop the syncer
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("shutdown complete")
	return nil
}
