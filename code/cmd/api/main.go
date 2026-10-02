// Command api runs the sandbox control plane: the REST API agents call with
// an API key (and, from Phase 4, the dashboard).
//
// Usage:
//
//	api                                     serve the API (default)
//	api create-user --name NAME [--admin]   create a user and print their first API key
//	api create-key --user NAME [--name L]   print an additional API key for a user
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

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
	"github.com/qfliu/sandbox-provider-temper/code/internal/db"
	"github.com/qfliu/sandbox-provider-temper/code/internal/httpapi"
	"github.com/qfliu/sandbox-provider-temper/code/internal/k8s"
	"github.com/qfliu/sandbox-provider-temper/code/internal/sandbox"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	var err error
	switch cmd := subcommand(); cmd {
	case "", "serve":
		err = serve(log)
	case "create-user":
		err = runCreateUser(context.Background(), os.Args[2:], os.Stdout)
	case "create-key":
		err = runCreateKey(context.Background(), os.Args[2:], os.Stdout)
	default:
		err = fmt.Errorf("unknown command %q (want serve, create-user or create-key)", cmd)
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

func serve(log *slog.Logger) error {
	cfg, err := config.LoadAPI()
	if err != nil {
		return err
	}

	// Stop on Ctrl-C locally or SIGTERM from Kubernetes.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	kube, err := k8s.NewClientset()
	if err != nil {
		return err
	}
	st := store.New(pool)
	sandboxes := sandbox.NewService(st, k8s.NewPods(kube, cfg.Sandbox), cfg.Sandbox, log)

	srv := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: httpapi.NewHandler(httpapi.Deps{
			Log:       log,
			DB:        pool,
			Auth:      st,
			Sandboxes: sandboxes,
		}),
	}

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

	// Let in-flight requests finish, but not forever.
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
