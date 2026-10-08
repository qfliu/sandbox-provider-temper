package config

import (
	"strings"
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

func TestLoadAPIReaperSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("SANDBOX_INTERNAL_KEY_FILE", "/keys/id")
	t.Setenv("SANDBOX_IDLE_TIMEOUT", "")
	t.Setenv("REAPER_INTERVAL", "")
	cfg, err := LoadAPI()
	if err != nil || cfg.Sandbox.IdleTimeout != 30*time.Minute || cfg.Sandbox.ReaperInterval != 30*time.Second {
		t.Fatalf("defaults: %+v, %v", cfg.Sandbox, err)
	}

	t.Setenv("REAPER_INTERVAL", "0s")
	if _, err := LoadAPI(); err == nil {
		t.Fatal("a zero interval would make the ticker panic; must be rejected")
	}
	t.Setenv("REAPER_INTERVAL", "")
	t.Setenv("SANDBOX_IDLE_TIMEOUT", "-5m")
	if _, err := LoadAPI(); err == nil {
		t.Fatal("negative idle timeout must be rejected")
	}
	for _, v := range []string{"5m", "10m"} { // not longer than the 10 min max exec
		t.Setenv("SANDBOX_IDLE_TIMEOUT", v)
		if _, err := LoadAPI(); err == nil {
			t.Fatalf("SANDBOX_IDLE_TIMEOUT=%s would let a long exec be reaped; must be rejected", v)
		}
	}
	t.Setenv("SANDBOX_IDLE_TIMEOUT", "11m")
	if _, err := LoadAPI(); err != nil {
		t.Fatalf("11m exceeds the max exec and should be accepted: %v", err)
	}
}

// SANDBOX_RUNTIMES (D65): gvisor by default; known names only, no repeats.
func TestLoadAPIRuntimes(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("SANDBOX_INTERNAL_KEY_FILE", "/keys/id")
	for _, c := range []struct {
		env  string
		want []string
		ok   bool
	}{
		{"", []string{"gvisor"}, true},
		{"gvisor,kata", []string{"gvisor", "kata"}, true},
		{" kata , gvisor ", []string{"kata", "gvisor"}, true},
		{"gvisor,runc", nil, false},
		{"gvisor,gvisor", nil, false},
		{",", nil, false},
	} {
		t.Setenv("SANDBOX_RUNTIMES", c.env)
		cfg, err := LoadAPI()
		if !c.ok {
			if err == nil {
				t.Errorf("SANDBOX_RUNTIMES=%q accepted", c.env)
			}
			continue
		}
		if err != nil || strings.Join(cfg.Sandbox.Runtimes, ",") != strings.Join(c.want, ",") {
			t.Errorf("SANDBOX_RUNTIMES=%q: %v, %v; want %v", c.env, cfg.Sandbox.Runtimes, err, c.want)
		}
	}
}
