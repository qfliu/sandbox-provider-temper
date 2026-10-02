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
}

// LoadAPI reads API settings from the environment, applying defaults.
func LoadAPI() (API, error) {
	cfg := API{
		ListenAddr:      getenv("API_LISTEN_ADDR", ":8080"),
		ShutdownTimeout: 10 * time.Second,
		DatabaseURL:     os.Getenv("DATABASE_URL"),
	}
	if cfg.DatabaseURL == "" {
		return API{}, fmt.Errorf("DATABASE_URL is required")
	}
	if v := os.Getenv("API_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return API{}, fmt.Errorf("API_SHUTDOWN_TIMEOUT: %w", err)
		}
		cfg.ShutdownTimeout = d
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
