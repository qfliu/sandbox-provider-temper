package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
	"github.com/qfliu/sandbox-provider-temper/code/internal/db"
	"github.com/qfliu/sandbox-provider-temper/code/internal/sshx"
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

// runCheckSSH connects to a sandbox exactly as the platform does (internal
// key + pinned host key) and prints who answered. --addr overrides the
// recorded pod IP, e.g. to check that another sandbox's address is refused.
// Must run where pod IPs are reachable (inside the cluster).
func runCheckSSH(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("check-ssh", flag.ContinueOnError)
	id := fs.String("sandbox", "", "sandbox id (required)")
	addr := fs.String("addr", "", "connect here instead of the recorded pod IP")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("check-ssh: --sandbox is required")
	}
	signer, err := sshx.LoadSigner(os.Getenv("SANDBOX_INTERNAL_KEY_FILE"))
	if err != nil {
		return err
	}
	s, done, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer done()

	sb, err := s.GetSandbox(ctx, *id)
	if err != nil {
		return fmt.Errorf("sandbox %s: %w", *id, err)
	}
	target := *addr
	if target == "" {
		if sb.PodIP == "" {
			return fmt.Errorf("sandbox %s has no pod IP yet (status %s)", sb.ID, sb.Status)
		}
		target = net.JoinHostPort(sb.PodIP, "22")
	}
	client, err := sshx.Dialer{Signer: signer}.Dial(ctx, target, sb.HostPublicKey)
	if err != nil {
		return fmt.Errorf("connect to %s at %s: %w", sb.ID, target, err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	whoami, err := sess.Output("echo $(whoami)@$(hostname) kernel $(uname -r)")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "OK %s at %s: host key verified, %s\n", sb.ID, target, strings.TrimSpace(string(whoami)))
	return nil
}

// runPublicKey prints the public half of a private SSH key file in
// authorized_keys / known_hosts format. The Helm chart generates keys in
// PKCS#8 PEM, which some ssh-keygen builds (macOS) can't read, so this is
// how to get the gateway's host key for known_hosts:
//
//	kubectl exec deploy/sandbox-gateway -- /api public-key /keys/host/ssh_host_ed25519_key
func runPublicKey(args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("public-key: want exactly one argument, the private key file")
	}
	signer, err := sshx.LoadSigner(args[0])
	if err != nil {
		return err
	}
	_, err = out.Write(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	return err
}
