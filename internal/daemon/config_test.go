package daemon_test

import (
	"errors"
	"flag"
	"strings"
	"testing"

	"resy-snipe/internal/daemon"
)

func TestDefaultConfigValidates(t *testing.T) {
	t.Parallel()
	cfg := daemon.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig.Validate: %v", err)
	}
}

func TestValidateAggregatesMultipleViolations(t *testing.T) {
	t.Parallel()
	cfg := daemon.DefaultConfig()
	cfg.Daemon.Bind = "not-an-addr-port"
	cfg.Daemon.LogFormat = "fancy"
	cfg.Daemon.LogLevel = "trace"
	cfg.Daemon.DataDir = ""
	cfg.Secrets.Mode = "shouting"
	cfg.Engine.MaxConcurrentBookings = 0
	cfg.HTTP.BasePath = "no-leading-slash"
	cfg.HTTP.TrustedProxies = []string{"127.0.0.1"} // not a CIDR

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a broken config")
	}
	if !errors.Is(err, daemon.ErrInvalidConfig) {
		t.Fatalf("errors.Is(err, ErrInvalidConfig)=false; err=%v", err)
	}
	var v *daemon.ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("errors.As ValidationError: false; err=%v", err)
	}
	if len(v.Violations) < 5 {
		t.Fatalf("expected ≥5 violations, got %d: %v", len(v.Violations), v.Violations)
	}
}

func TestApplyEnvOverlay(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"RESY_SNIPE_BIND":            "0.0.0.0:8080",
		"RESY_SNIPE_LOG_LEVEL":       "debug",
		"RESY_SNIPE_MAX_BOOKINGS":    "16",
		"RESY_SNIPE_TRUSTED_PROXIES": "10.0.0.0/8, 192.168.1.0/24",
		"RESY_SNIPE_SECRETS_MODE":    "keyfile",
		"RESY_SNIPE_KEYFILE":         "/etc/resy-snipe/key.hex",
	}
	getenv := func(k string) string { return env[k] }

	cfg := daemon.DefaultConfig()
	if err := cfg.ApplyEnv(getenv); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.Daemon.Bind != "0.0.0.0:8080" {
		t.Errorf("bind=%q want 0.0.0.0:8080", cfg.Daemon.Bind)
	}
	if cfg.Daemon.LogLevel != "debug" {
		t.Errorf("log_level=%q want debug", cfg.Daemon.LogLevel)
	}
	if cfg.Engine.MaxConcurrentBookings != 16 {
		t.Errorf("max_bookings=%d want 16", cfg.Engine.MaxConcurrentBookings)
	}
	if len(cfg.HTTP.TrustedProxies) != 2 ||
		cfg.HTTP.TrustedProxies[0] != "10.0.0.0/8" ||
		cfg.HTTP.TrustedProxies[1] != "192.168.1.0/24" {
		t.Errorf("trusted_proxies=%v", cfg.HTTP.TrustedProxies)
	}
	if cfg.Secrets.Mode != "keyfile" || cfg.Secrets.Keyfile != "/etc/resy-snipe/key.hex" {
		t.Errorf("secrets=%+v", cfg.Secrets)
	}
	// Unset env keeps default for log_format.
	if cfg.Daemon.LogFormat != "json" {
		t.Errorf("log_format=%q want json (unchanged)", cfg.Daemon.LogFormat)
	}
}

func TestApplyEnvAggregatesParseErrors(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"RESY_SNIPE_MAX_BOOKINGS":   "not-int",
		"RESY_SNIPE_SHUTDOWN_DRAIN": "also-not-int",
	}
	cfg := daemon.DefaultConfig()
	err := cfg.ApplyEnv(func(k string) string { return env[k] })
	if err == nil {
		t.Fatal("expected error")
	}
	var v *daemon.ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("errors.As ValidationError: false; err=%v", err)
	}
	if len(v.Violations) != 2 {
		t.Fatalf("expected 2 violations, got %d: %v", len(v.Violations), v.Violations)
	}
}

func TestApplyTOMLRoundTrip(t *testing.T) {
	t.Parallel()
	body := `
[daemon]
bind        = "127.0.0.1:9000"
log_format  = "text"
log_level   = "warn"

[secrets]
mode    = "keyfile"
keyfile = "/srv/keys/resy.hex"

[http]
base_path        = "/api"
trusted_proxies  = ["10.0.0.0/8"]
external_url     = "https://snipe.example.com"

[engine]
max_concurrent_bookings = 4
`
	cfg := daemon.DefaultConfig()
	if err := cfg.ApplyTOML(strings.NewReader(body)); err != nil {
		t.Fatalf("ApplyTOML: %v", err)
	}
	if cfg.Daemon.Bind != "127.0.0.1:9000" {
		t.Errorf("bind=%q", cfg.Daemon.Bind)
	}
	if cfg.Daemon.LogFormat != "text" || cfg.Daemon.LogLevel != "warn" {
		t.Errorf("daemon=%+v", cfg.Daemon)
	}
	if cfg.Secrets.Mode != "keyfile" || cfg.Secrets.Keyfile != "/srv/keys/resy.hex" {
		t.Errorf("secrets=%+v", cfg.Secrets)
	}
	if cfg.HTTP.BasePath != "/api" || cfg.HTTP.ExternalURL != "https://snipe.example.com" {
		t.Errorf("http=%+v", cfg.HTTP)
	}
	if cfg.Engine.MaxConcurrentBookings != 4 {
		t.Errorf("engine.max_concurrent_bookings=%d", cfg.Engine.MaxConcurrentBookings)
	}
}

func TestApplyTOMLRejectsUnknownKeys(t *testing.T) {
	t.Parallel()
	body := `
[daemon]
bind = "127.0.0.1:7765"

[daemon.bonus]
not_a_real_key = 42

[wildcard]
why = "this should fail"
`
	cfg := daemon.DefaultConfig()
	err := cfg.ApplyTOML(strings.NewReader(body))
	if err == nil {
		t.Fatal("ApplyTOML accepted unknown keys")
	}
	var v *daemon.ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("errors.As ValidationError: false; err=%v", err)
	}
	if len(v.Violations) < 2 {
		t.Fatalf("expected ≥2 unknown-key violations, got %d: %v", len(v.Violations), v.Violations)
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	t.Parallel()
	cfg := daemon.DefaultConfig()
	env := map[string]string{"RESY_SNIPE_BIND": "0.0.0.0:1111"}
	if err := cfg.ApplyEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.Daemon.Bind != "0.0.0.0:1111" {
		t.Fatalf("env did not apply: %q", cfg.Daemon.Bind)
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.RegisterFlags(fs)
	if err := fs.Parse([]string{"--bind", "127.0.0.1:2222"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.Bind != "127.0.0.1:2222" {
		t.Fatalf("flag did not override env: %q", cfg.Daemon.Bind)
	}
}

func TestPrecedenceDefaultTOMLEnvFlag(t *testing.T) {
	t.Parallel()
	// default daemon.bind = 127.0.0.1:7765
	// TOML sets daemon.bind = 10.0.0.10:1000
	// env  sets daemon.bind = 10.0.0.20:2000
	// flag sets daemon.bind = 10.0.0.30:3000  -> winner
	cfg := daemon.DefaultConfig()
	if err := cfg.ApplyTOML(strings.NewReader(`[daemon]
bind = "10.0.0.10:1000"
`)); err != nil {
		t.Fatalf("ApplyTOML: %v", err)
	}
	if cfg.Daemon.Bind != "10.0.0.10:1000" {
		t.Fatalf("TOML did not apply: %q", cfg.Daemon.Bind)
	}
	if err := cfg.ApplyEnv(func(k string) string {
		if k == "RESY_SNIPE_BIND" {
			return "10.0.0.20:2000"
		}
		return ""
	}); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.Daemon.Bind != "10.0.0.20:2000" {
		t.Fatalf("env did not apply: %q", cfg.Daemon.Bind)
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.RegisterFlags(fs)
	if err := fs.Parse([]string{"--bind", "10.0.0.30:3000"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.Bind != "10.0.0.30:3000" {
		t.Fatalf("precedence failed: %q", cfg.Daemon.Bind)
	}
}

func TestApplyTOMLFileMissingIsNoError(t *testing.T) {
	t.Parallel()
	cfg := daemon.DefaultConfig()
	if err := cfg.ApplyTOMLFile("/nonexistent/path/config.toml"); err != nil {
		t.Fatalf("missing file should be a no-op: %v", err)
	}
}

func TestKeyfileModeRequiresPath(t *testing.T) {
	t.Parallel()
	cfg := daemon.DefaultConfig()
	cfg.Secrets.Mode = "keyfile"
	cfg.Secrets.Keyfile = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted keyfile mode without keyfile path")
	}
	if !strings.Contains(err.Error(), "keyfile") {
		t.Fatalf("error should mention keyfile: %v", err)
	}
}
