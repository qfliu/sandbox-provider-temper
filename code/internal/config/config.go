// Package config loads service settings from environment variables, so the
// same binary runs unchanged on a laptop and inside Kubernetes.
package config

import (
	"fmt"
	"os"
	"time"
)

// API holds the control plane API server's settings.
type API struct {
	// ListenAddr is the host:port the HTTP server binds to.
	ListenAddr string
	// ShutdownTimeout bounds how long in-flight requests get to finish on
	// SIGTERM before the server exits.
	ShutdownTimeout time.Duration
	// DatabaseURL is the Postgres connection string (required), e.g.
	// postgres://user:pass@host:5432/sandbox?sslmode=disable
	DatabaseURL string
	// Sandbox controls how sandbox pods are created.
	Sandbox Sandbox
}

// Sandbox holds settings for sandbox pods.
type Sandbox struct {
	// Namespace is where sandbox pods run, apart from platform services (D26).
	Namespace string
	// ImageRepo + image name + ":" + ImageTag gives the container image,
	// e.g. sandbox/ubuntu-dev:0.1 (D2, D23).
	ImageRepo string
	ImageTag  string
	// DefaultTTL applies when a request doesn't set ttl_seconds; MaxTTL caps
	// what users may ask for (D9).
	DefaultTTL time.Duration
	MaxTTL     time.Duration
	// IdleTimeout: sandboxes with no exec, file transfer or SSH for this
	// long are reaped (D9). Must exceed the longest exec (10 min).
	IdleTimeout time.Duration
	// ReaperInterval is how often expired/idle sandboxes are checked for.
	ReaperInterval time.Duration
	// InternalKeySecret is the Secret holding the platform-internal SSH
	// public key that sandbox sshd trusts (D7).
	InternalKeySecret string
	// InternalKeyFile is the matching private key the API logs in with.
	InternalKeyFile string
}

// LoadAPI reads API settings from the environment, applying defaults.
func LoadAPI() (API, error) {
	cfg := API{
		ListenAddr: getenv("API_LISTEN_ADDR", ":8080"),
		// Drain window on SIGTERM; fits inside Kubernetes' default 30 s
		// terminationGracePeriodSeconds before it force-kills the pod.
		ShutdownTimeout: 25 * time.Second,
		Sandbox: Sandbox{
			Namespace:         getenv("SANDBOX_NAMESPACE", "sandboxes"),
			ImageRepo:         getenv("SANDBOX_IMAGE_REPO", "sandbox/"),
			ImageTag:          getenv("SANDBOX_IMAGE_TAG", "0.1"),
			DefaultTTL:        time.Hour,
			MaxTTL:            24 * time.Hour,
			IdleTimeout:       30 * time.Minute,
			ReaperInterval:    30 * time.Second,
			InternalKeySecret: getenv("SANDBOX_INTERNAL_KEY_SECRET", "sandbox-internal-key"),
			InternalKeyFile:   os.Getenv("SANDBOX_INTERNAL_KEY_FILE"),
		},
	}
	if cfg.Sandbox.InternalKeyFile == "" {
		return API{}, fmt.Errorf("SANDBOX_INTERNAL_KEY_FILE is required (the platform's private SSH key)")
	}
	durations := []struct {
		env string
		dst *time.Duration
	}{
		{"API_SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeout},
		{"SANDBOX_DEFAULT_TTL", &cfg.Sandbox.DefaultTTL},
		{"SANDBOX_MAX_TTL", &cfg.Sandbox.MaxTTL},
		{"SANDBOX_IDLE_TIMEOUT", &cfg.Sandbox.IdleTimeout},
		{"REAPER_INTERVAL", &cfg.Sandbox.ReaperInterval},
	}
	for _, d := range durations {
		if v := os.Getenv(d.env); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				return API{}, fmt.Errorf("%s: %w", d.env, err)
			}
			if parsed <= 0 {
				return API{}, fmt.Errorf("%s must be positive, got %s", d.env, v)
			}
			*d.dst = parsed
		}
	}
	if cfg.Sandbox.DefaultTTL > cfg.Sandbox.MaxTTL {
		return API{}, fmt.Errorf("SANDBOX_DEFAULT_TTL (%s) exceeds SANDBOX_MAX_TTL (%s)",
			cfg.Sandbox.DefaultTTL, cfg.Sandbox.MaxTTL)
	}

	url, err := DatabaseURL()
	if err != nil {
		return API{}, err
	}
	cfg.DatabaseURL = url
	return cfg, nil
}

// DatabaseURL returns the required DATABASE_URL setting.
func DatabaseURL() (string, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return "", fmt.Errorf("DATABASE_URL is required")
	}
	return url, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
