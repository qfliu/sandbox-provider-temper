package config

import (
	"testing"
	"time"
)

func TestLoadAPIDefaults(t *testing.T) {
	t.Setenv("API_LISTEN_ADDR", "")
	t.Setenv("API_SHUTDOWN_TIMEOUT", "")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("SANDBOX_INTERNAL_KEY_FILE", "/keys/id")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8080" || cfg.ShutdownTimeout != 25*time.Second {
		t.Fatalf("got %+v, want defaults", cfg)
	}
}

func TestLoadAPIFromEnv(t *testing.T) {
	t.Setenv("API_LISTEN_ADDR", "127.0.0.1:9000")
	t.Setenv("API_SHUTDOWN_TIMEOUT", "3s")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("SANDBOX_INTERNAL_KEY_FILE", "/keys/id")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" || cfg.ShutdownTimeout != 3*time.Second {
		t.Fatalf("got %+v", cfg)
	}
}

func TestLoadAPIRejectsBadDuration(t *testing.T) {
	t.Setenv("API_SHUTDOWN_TIMEOUT", "soon")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("SANDBOX_INTERNAL_KEY_FILE", "/keys/id")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("expected an error for an invalid duration")
	}
}

func TestLoadAPIRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("expected an error when DATABASE_URL is missing")
	}
}

func TestLoadAPIRequiresInternalKeyFile(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("SANDBOX_INTERNAL_KEY_FILE", "")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("expected an error when SANDBOX_INTERNAL_KEY_FILE is missing")
	}
}
