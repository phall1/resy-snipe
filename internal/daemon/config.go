package daemon

import (
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the full daemon configuration. Each field is reachable
// through a TOML key, an env var, and a CLI flag — see
// docs/v2/design/daemon.md §3.2 for the canonical table. Validation
// enforces the cross-field invariants (mode/keyfile pairing, bind
// host:port shape, etc.) and is run after every Apply* layer so a
// downstream invariant break gets blamed on the layer that caused it.
type Config struct {
	Daemon  DaemonConfig  `toml:"daemon"`
	Secrets SecretsConfig `toml:"secrets"`
	HTTP    HTTPConfig    `toml:"http"`
	Signer  SignerConfig  `toml:"signer"`
	Engine  EngineConfig  `toml:"engine"`
}

// DaemonConfig groups process-level keys.
type DaemonConfig struct {
	Bind                 string `toml:"bind"`
	DataDir              string `toml:"data_dir"`
	LogFormat            string `toml:"log_format"`
	LogLevel             string `toml:"log_level"`
	ShutdownDrainSeconds int    `toml:"shutdown_drain_seconds"`
}

// SecretsConfig groups sealed-store keys. See internal/secrets and
// docs/v2/design/secrets.md.
type SecretsConfig struct {
	Mode             string `toml:"mode"` // "passphrase" | "keyfile" | "insecure"
	Keyfile          string `toml:"keyfile"`
	Argon2IDMemoryKB int    `toml:"argon2id_memory_kb"`
	Argon2IDTime     int    `toml:"argon2id_time"`
	Argon2IDParallel int    `toml:"argon2id_parallel"`
}

// HTTPConfig groups transport-layer keys. See M2-09, M2-13.
type HTTPConfig struct {
	BasePath       string   `toml:"base_path"`
	TrustedProxies []string `toml:"trusted_proxies"`
	ExternalURL    string   `toml:"external_url"`
}

// SignerConfig groups signer-subprocess keys.
type SignerConfig struct {
	Bin                   string `toml:"bin"`
	RestartBackoffSeconds int    `toml:"restart_backoff_seconds"`
}

// EngineConfig groups Engine-tuning keys.
type EngineConfig struct {
	MaxConcurrentBookings int `toml:"max_concurrent_bookings"`
	DefaultLookaheadDays  int `toml:"default_lookahead_days"`
}

// DefaultConfig returns the baseline Config — every field at its
// compiled-in default per docs/v2/design/daemon.md §3.2. Higher
// precedence layers (TOML, env, flag) overlay this in turn.
func DefaultConfig() Config {
	return Config{
		Daemon: DaemonConfig{
			Bind:                 "127.0.0.1:7765",
			DataDir:              defaultDataDir(),
			LogFormat:            "json",
			LogLevel:             "info",
			ShutdownDrainSeconds: 15,
		},
		Secrets: SecretsConfig{
			Mode:             "passphrase",
			Keyfile:          "",
			Argon2IDMemoryKB: 64 * 1024,
			Argon2IDTime:     3,
			Argon2IDParallel: 1,
		},
		HTTP: HTTPConfig{
			BasePath: "/",
			TrustedProxies: []string{
				"127.0.0.0/8",
				"::1/128",
				"10.0.0.0/8",
				"172.16.0.0/12",
				"192.168.0.0/16",
				"fc00::/7",
			},
			ExternalURL: "",
		},
		Signer: SignerConfig{
			Bin:                   "",
			RestartBackoffSeconds: 5,
		},
		Engine: EngineConfig{
			MaxConcurrentBookings: 8,
			DefaultLookaheadDays:  60,
		},
	}
}

// defaultDataDir resolves the daemon.data_dir default at runtime so
// each operator's box gets its own XDG-conformant path without a
// hard-coded /var/lib path baked into the binary.
func defaultDataDir() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "resy-snipe")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "resy-snipe")
	}
	return "./.resy-snipe"
}

// ValidationError is the aggregated failure type returned by
// Validate. The daemon's boot sequence prints every violation rather
// than failing on the first; operators editing a fresh config.toml
// see the whole picture instead of fixing one line at a time.
type ValidationError struct {
	Violations []string
}

func (v *ValidationError) Error() string {
	if v == nil || len(v.Violations) == 0 {
		return "config: no violations"
	}
	return fmt.Sprintf("config: %d violation(s):\n  - %s",
		len(v.Violations), strings.Join(v.Violations, "\n  - "))
}

// ErrInvalidConfig is the sentinel wrapping ValidationError so
// callers can branch on errors.Is independent of the message body.
var ErrInvalidConfig = errors.New("config: invalid")

// Is satisfies errors.Is by delegating to ErrInvalidConfig — the
// "any validation failure" sentinel.
func (v *ValidationError) Is(target error) bool {
	return target == ErrInvalidConfig
}

// Validate returns nil if cfg is internally consistent. On failure
// the returned error is *ValidationError wrapping every violation
// found. The check is purely structural — no I/O, no DB.
func (c *Config) Validate() error {
	var v ValidationError

	// daemon.bind: host:port shape. netip.ParseAddrPort handles both
	// IPv4 (127.0.0.1:7765) and IPv6 ([::1]:7765); we also accept
	// 0.0.0.0:port and the loopback aliases.
	if _, err := netip.ParseAddrPort(c.Daemon.Bind); err != nil {
		v.addf("daemon.bind %q is not a valid host:port: %v", c.Daemon.Bind, err)
	}

	switch c.Daemon.LogFormat {
	case "json", "text":
	default:
		v.addf("daemon.log_format %q: want \"json\" or \"text\"", c.Daemon.LogFormat)
	}
	switch c.Daemon.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		v.addf("daemon.log_level %q: want one of debug/info/warn/error", c.Daemon.LogLevel)
	}
	if c.Daemon.ShutdownDrainSeconds < 0 {
		v.addf("daemon.shutdown_drain_seconds %d: must be >= 0", c.Daemon.ShutdownDrainSeconds)
	}
	if strings.TrimSpace(c.Daemon.DataDir) == "" {
		v.addf("daemon.data_dir is empty")
	}

	switch c.Secrets.Mode {
	case "passphrase", "keyfile", "insecure":
	default:
		v.addf("secrets.mode %q: want \"passphrase\", \"keyfile\", or \"insecure\"", c.Secrets.Mode)
	}
	if c.Secrets.Mode == "keyfile" && strings.TrimSpace(c.Secrets.Keyfile) == "" {
		v.addf("secrets.mode=keyfile but secrets.keyfile is empty")
	}
	if c.Secrets.Argon2IDMemoryKB < 8*1024 {
		v.addf("secrets.argon2id_memory_kb %d: minimum is 8192 (8 MiB)", c.Secrets.Argon2IDMemoryKB)
	}
	if c.Secrets.Argon2IDTime < 1 {
		v.addf("secrets.argon2id_time %d: must be >= 1", c.Secrets.Argon2IDTime)
	}
	if c.Secrets.Argon2IDParallel < 1 {
		v.addf("secrets.argon2id_parallel %d: must be >= 1", c.Secrets.Argon2IDParallel)
	}

	if !strings.HasPrefix(c.HTTP.BasePath, "/") {
		v.addf("http.base_path %q: must start with \"/\"", c.HTTP.BasePath)
	}
	for _, cidr := range c.HTTP.TrustedProxies {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			v.addf("http.trusted_proxies entry %q is not a CIDR: %v", cidr, err)
		}
	}

	if c.Signer.RestartBackoffSeconds < 0 {
		v.addf("signer.restart_backoff_seconds %d: must be >= 0", c.Signer.RestartBackoffSeconds)
	}
	if c.Engine.MaxConcurrentBookings < 1 {
		v.addf("engine.max_concurrent_bookings %d: must be >= 1", c.Engine.MaxConcurrentBookings)
	}
	if c.Engine.DefaultLookaheadDays < 1 {
		v.addf("engine.default_lookahead_days %d: must be >= 1", c.Engine.DefaultLookaheadDays)
	}

	if len(v.Violations) > 0 {
		return &v
	}
	return nil
}

func (v *ValidationError) addf(format string, args ...any) {
	v.Violations = append(v.Violations, fmt.Sprintf(format, args...))
}

// ApplyEnv overlays RESY_SNIPE_* environment variables onto cfg. Only
// set vars apply — an unset env var leaves the field at whatever the
// caller passed in. Returns an error wrapping every malformed value
// (e.g. RESY_SNIPE_MAX_BOOKINGS="not-an-int") so the operator sees
// the full list at once.
//
// Source-of-truth is the §3.2 table in docs/v2/design/daemon.md;
// drift between this code and that table is a bug.
func (c *Config) ApplyEnv(getenv func(string) string) error {
	if getenv == nil {
		getenv = os.Getenv
	}
	var v ValidationError

	envString(getenv, "RESY_SNIPE_BIND", &c.Daemon.Bind)
	envString(getenv, "RESY_SNIPE_DATA_DIR", &c.Daemon.DataDir)
	envString(getenv, "RESY_SNIPE_LOG_FORMAT", &c.Daemon.LogFormat)
	envString(getenv, "RESY_SNIPE_LOG_LEVEL", &c.Daemon.LogLevel)
	envInt(getenv, "RESY_SNIPE_SHUTDOWN_DRAIN", &c.Daemon.ShutdownDrainSeconds, &v)

	envString(getenv, "RESY_SNIPE_SECRETS_MODE", &c.Secrets.Mode)
	envString(getenv, "RESY_SNIPE_KEYFILE", &c.Secrets.Keyfile)
	envInt(getenv, "RESY_SNIPE_ARGON2_MEM", &c.Secrets.Argon2IDMemoryKB, &v)
	envInt(getenv, "RESY_SNIPE_ARGON2_TIME", &c.Secrets.Argon2IDTime, &v)
	envInt(getenv, "RESY_SNIPE_ARGON2_PARALLEL", &c.Secrets.Argon2IDParallel, &v)

	envString(getenv, "RESY_SNIPE_BASE_PATH", &c.HTTP.BasePath)
	envString(getenv, "RESY_SNIPE_EXTERNAL_URL", &c.HTTP.ExternalURL)
	if raw := getenv("RESY_SNIPE_TRUSTED_PROXIES"); raw != "" {
		c.HTTP.TrustedProxies = splitCSV(raw)
	}

	envString(getenv, "RESY_SNIPE_SIGNER_BIN", &c.Signer.Bin)
	envInt(getenv, "RESY_SNIPE_SIGNER_BACKOFF", &c.Signer.RestartBackoffSeconds, &v)
	envInt(getenv, "RESY_SNIPE_MAX_BOOKINGS", &c.Engine.MaxConcurrentBookings, &v)
	envInt(getenv, "RESY_SNIPE_LOOKAHEAD_DAYS", &c.Engine.DefaultLookaheadDays, &v)

	if len(v.Violations) > 0 {
		return &v
	}
	return nil
}

// RegisterFlags binds CLI flags onto fs that read into cfg. Flag
// defaults match cfg's current values so calling RegisterFlags after
// ApplyEnv preserves the env-driven values until the operator passes
// the flag explicitly.
//
// Per §3.2, flag > env > config > default. This function realizes
// the top of that stack: any flag the operator sets overrides what
// was already on cfg.
func (c *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.Daemon.Bind, "bind", c.Daemon.Bind, "host:port to bind the HTTP listener")
	fs.StringVar(&c.Daemon.DataDir, "data-dir", c.Daemon.DataDir, "data directory for data.db")
	fs.StringVar(&c.Daemon.LogFormat, "log-format", c.Daemon.LogFormat, "structured log format: json|text")
	fs.StringVar(&c.Daemon.LogLevel, "log-level", c.Daemon.LogLevel, "log level: debug|info|warn|error")
	fs.IntVar(&c.Daemon.ShutdownDrainSeconds, "shutdown-drain", c.Daemon.ShutdownDrainSeconds, "graceful shutdown drain budget in seconds")

	fs.StringVar(&c.Secrets.Mode, "secrets-mode", c.Secrets.Mode, "sealed-secrets mode: passphrase|keyfile|insecure")
	fs.StringVar(&c.Secrets.Keyfile, "keyfile", c.Secrets.Keyfile, "path to a hex-encoded 32-byte keyfile (secrets-mode=keyfile)")
	fs.IntVar(&c.Secrets.Argon2IDMemoryKB, "argon2-mem", c.Secrets.Argon2IDMemoryKB, "Argon2id memory parameter in KiB")
	fs.IntVar(&c.Secrets.Argon2IDTime, "argon2-time", c.Secrets.Argon2IDTime, "Argon2id time parameter")
	fs.IntVar(&c.Secrets.Argon2IDParallel, "argon2-parallel", c.Secrets.Argon2IDParallel, "Argon2id parallelism parameter")

	fs.StringVar(&c.HTTP.BasePath, "base-path", c.HTTP.BasePath, "HTTP base path (always starts with /)")
	fs.StringVar(&c.HTTP.ExternalURL, "external-url", c.HTTP.ExternalURL, "operator-set external base URL for link generation")
	fs.Var(&csvFlag{slot: &c.HTTP.TrustedProxies}, "trusted-proxies", "comma-separated CIDR list trusted for X-Forwarded-* headers")

	fs.StringVar(&c.Signer.Bin, "signer-bin", c.Signer.Bin, "absolute path to the signer subprocess binary (empty=Noop)")
	fs.IntVar(&c.Signer.RestartBackoffSeconds, "signer-backoff", c.Signer.RestartBackoffSeconds, "signer restart backoff in seconds")
	fs.IntVar(&c.Engine.MaxConcurrentBookings, "max-bookings", c.Engine.MaxConcurrentBookings, "maximum concurrent in-flight bookings")
	fs.IntVar(&c.Engine.DefaultLookaheadDays, "lookahead-days", c.Engine.DefaultLookaheadDays, "default lookahead window in days")
}

// envString overlays an env var onto target when the var is set
// (non-empty). Empty env vars are treated as "not set" — operators
// who explicitly want an empty value (e.g. clearing a default) use
// the flag, which sets unconditionally.
func envString(getenv func(string) string, key string, target *string) {
	if v := getenv(key); v != "" {
		*target = v
	}
}

func envInt(getenv func(string) string, key string, target *int, v *ValidationError) {
	raw := getenv(key)
	if raw == "" {
		return
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		v.addf("%s=%q is not an integer: %v", key, raw, err)
		return
	}
	*target = parsed
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// csvFlag is a flag.Value adapter for repeated CSV inputs. The flag
// package's Set is called once per occurrence of the flag on the
// command line; we treat each call as "replace the slot," so
// --trusted-proxies a,b --trusted-proxies c yields ["c"], not
// ["a","b","c"]. Operators get one shot to express the list.
type csvFlag struct {
	slot *[]string
}

func (c *csvFlag) Set(s string) error {
	*c.slot = splitCSV(s)
	return nil
}

func (c *csvFlag) String() string {
	if c == nil || c.slot == nil {
		return ""
	}
	return strings.Join(*c.slot, ",")
}
