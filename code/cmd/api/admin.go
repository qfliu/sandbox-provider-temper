package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
	"github.com/qfliu/sandbox-provider-temper/code/internal/db"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// Admin commands bootstrap accounts before the dashboard exists. They talk to
// the database directly (DATABASE_URL), e.g. inside the cluster:
//
//	kubectl exec deploy/api -- api create-user --name admin --admin

// runCreateUser creates a user plus their first API key and prints the key.
func runCreateUser(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("create-user", flag.ContinueOnError)
	name := fs.String("name", "", "user name (required)")
	admin := fs.Bool("admin", false, "make the user an admin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("create-user: --name is required")
	}

	s, done, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer done()

	user, err := s.CreateUser(ctx, *name, *admin)
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("user %q already exists (use create-key to issue another key)", *name)
	}
	if err != nil {
		return err
	}
	key, _, err := s.CreateAPIKey(ctx, user.ID, "initial")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "created user %q (admin=%t)\n", user.Name, user.IsAdmin)
	printKey(out, key)
	return nil
}

// runCreateKey issues an additional API key for an existing user.
func runCreateKey(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("create-key", flag.ContinueOnError)
	userName := fs.String("user", "", "existing user name (required)")
	label := fs.String("name", "", "label to tell keys apart, e.g. ci-agent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *userName == "" {
		return errors.New("create-key: --user is required")
	}

	s, done, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer done()

	user, err := s.UserByName(ctx, *userName)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no user named %q", *userName)
	}
	if err != nil {
		return err
	}
	key, _, err := s.CreateAPIKey(ctx, user.ID, *label)
	if err != nil {
		return err
	}
	printKey(out, key)
	return nil
}

func printKey(out io.Writer, key string) {
	fmt.Fprintf(out, "API key (shown only once, store it now):\n%s\n", key)
}

// openStore connects and migrates, so admin commands also work on a fresh
// database.
func openStore(ctx context.Context) (*store.Store, func(), error) {
	url, err := config.DatabaseURL()
	if err != nil {
		return nil, nil, err
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	return store.New(pool), pool.Close, nil
}
