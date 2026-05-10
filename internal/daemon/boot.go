package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/secrets"
	"resy-snipe/internal/store"
)

// Options collects boot-time hooks and test overrides for Boot. The
// zero value is fine for production wiring; tests inject the fields
// they need.
type Options struct {
	// Getenv overrides os.Getenv (used by ResolvePassphraseSource).
	Getenv func(string) string

	// PassphraseSource overrides the auto-resolved source. Mainly
	// for tests; production callers leave it nil and rely on
	// ResolvePassphraseSource to pick env vs stdin vs keyfile.
	PassphraseSource PassphraseSource

	// AllowInsecureNoEncryption skips the secrets unwrap entirely.
	// The boot path refuses to honor this unless the tripwire
	// guards from design/secrets.md §Dev-mode are all clear. Tests
	// use this to skip the unwrap; production runs require the
	// matching CLI flag.
	AllowInsecureNoEncryption bool

	// BannerWriter receives the multi-line boot banner. Defaults to
	// os.Stderr. Tests can capture the banner via a *bytes.Buffer.
	BannerWriter io.Writer

	// NotifyReady is called once the daemon is ready to serve. The
	// production wiring binds this to sd_notify(READY=1); tests can
	// observe boot completion via a fake. Called even if the
	// banner emit fails.
	NotifyReady func()

	// Clock injects a deterministic clock for tests. Defaults to
	// clock.Real.
	Clock clock.Clock

	// Version, Commit are operator-facing strings stitched into the
	// banner. Production wiring threads them in from the linker;
	// tests can leave them blank.
	Version string
	Commit  string
}

// Daemon is the boot result. The struct owns every resource Boot
// acquired; Close releases them in reverse order. Higher-level code
// (Run, eventually serving HTTP) takes a *Daemon and uses its
// exported fields.
type Daemon struct {
	Cfg    Config
	DB     *sql.DB
	Sealer *secrets.Sealer
	Meta   secrets.Meta

	// BannerLines is the banner content the boot path emitted, kept
	// for tests / /readyz introspection. Production callers can
	// ignore it.
	BannerLines []string

	flock *fileLock
}

// Boot runs steps 1-5 + 9 of the boot sequence in
// docs/v2/design/daemon.md §2. Steps 6 (signer), 7 (engine), and 8
// (HTTP listener) are mounted by callers — they are not the boot
// orchestrator's concern, but Boot validates the prerequisites for
// each so a downstream wiring error surfaces at the right layer.
//
// On success the returned *Daemon holds: a flock on data.db.lock, a
// migrated *sql.DB, an unwrapped *secrets.Sealer, and a banner
// already emitted to opts.BannerWriter. Caller is responsible for
// Close.
func Boot(ctx context.Context, cfg Config, opts Options) (*Daemon, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("daemon: invalid config: %w", err)
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.BannerWriter == nil {
		opts.BannerWriter = os.Stderr
	}

	if err := os.MkdirAll(cfg.Daemon.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: prepare data dir %s: %w", cfg.Daemon.DataDir, err)
	}
	dbPath := filepath.Join(cfg.Daemon.DataDir, "data.db")
	lockPath := dbPath + ".lock"

	// Step 2: flock the lockfile.
	lock, err := acquireFileLock(lockPath)
	if err != nil {
		return nil, err
	}
	d := &Daemon{Cfg: cfg, flock: lock}
	defer func() {
		if d.DB == nil { // boot failed before we attached the DB; release.
			_ = d.flock.Close()
		}
	}()

	// Step 3: open SQLite + verify PRAGMAs (store.Open does both).
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		_ = d.flock.Close()
		return nil, fmt.Errorf("daemon: open db: %w", err)
	}
	d.DB = db

	// Step 4: migrate or refuse downgrade. ErrSchemaNewer is the
	// "exit code 3" path per design §12.2.
	if err := store.Migrate(ctx, db); err != nil {
		_ = d.Close()
		return nil, err
	}
	schemaVersion, err := store.AppliedMaxVersion(ctx, db)
	if err != nil {
		_ = d.Close()
		return nil, err
	}

	// Step 5: unwrap secrets unless dev-mode is enabled (and
	// permitted by the tripwire guard).
	var secretsDescription string
	if cfg.Secrets.Mode == "insecure" || opts.AllowInsecureNoEncryption {
		if !opts.AllowInsecureNoEncryption {
			_ = d.Close()
			return nil, errors.New("daemon: secrets.mode=insecure requires AllowInsecureNoEncryption=true")
		}
		if err := devModeTripwire(opts.Getenv); err != nil {
			_ = d.Close()
			return nil, err
		}
		secretsDescription = "dev-mode (insecure)" // #nosec G101 -- operator-facing status, not a credential.
		// Even in dev-mode we initialize meta so the rest of the
		// system has consistent state.
		meta, err := secrets.LoadOrInitMeta(ctx, db, opts.Clock)
		if err != nil {
			_ = d.Close()
			return nil, err
		}
		d.Meta = meta
	} else {
		if err := secrets.CheckMixedVersion(ctx, db); err != nil {
			_ = d.Close()
			return nil, err
		}
		meta, err := secrets.LoadOrInitMeta(ctx, db, opts.Clock)
		if err != nil {
			_ = d.Close()
			return nil, err
		}
		d.Meta = meta
		sealer, desc, err := unlockSealer(cfg, opts, db, meta)
		if err != nil {
			_ = d.Close()
			return nil, err
		}
		d.Sealer = sealer
		secretsDescription = desc
	}

	// Step 9: banner + ready notification.
	bi := bannerInfo{
		Version:       opts.Version,
		Commit:        opts.Commit,
		SchemaVersion: schemaVersion,
		Bind:          cfg.Daemon.Bind,
		DataDir:       cfg.Daemon.DataDir,
		DBPath:        dbPath,
		SignerStatus:  describeSigner(cfg),
		SecretsStatus: secretsDescription,
	}
	if cfg.Secrets.Mode != "insecure" && !opts.AllowInsecureNoEncryption {
		bi.SecretsStatus = fmt.Sprintf("unlocked (Argon2id, key-version=%d)", d.Meta.ActiveVersion)
		if cfg.Secrets.Mode == "keyfile" {
			bi.SecretsStatus = fmt.Sprintf("unlocked (keyfile, key-version=%d)", d.Meta.ActiveVersion)
		}
	}
	d.BannerLines = formatBanner(bi)
	for _, line := range d.BannerLines {
		_, _ = fmt.Fprintln(opts.BannerWriter, line)
	}
	if opts.NotifyReady != nil {
		opts.NotifyReady()
	}
	return d, nil
}

// Close releases the daemon's resources in reverse boot order:
// sealer wipe → db close → flock release. Each step is best-effort;
// the first error is returned but later steps still run.
func (d *Daemon) Close() error {
	if d == nil {
		return nil
	}
	var firstErr error
	setErr := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if d.Sealer != nil {
		setErr(d.Sealer.Close())
		d.Sealer = nil
	}
	if d.DB != nil {
		setErr(d.DB.Close())
		d.DB = nil
	}
	if d.flock != nil {
		setErr(d.flock.Close())
		d.flock = nil
	}
	return firstErr
}

// unlockSealer dispatches to the secrets-unwrap path: passphrase or
// keyfile, depending on the resolved PassphraseSource.
func unlockSealer(cfg Config, opts Options, db *sql.DB, meta secrets.Meta) (*secrets.Sealer, string, error) {
	source, err := ResolvePassphraseSource(cfg, opts)
	if err != nil {
		return nil, "", err
	}

	// Keyfile path is a sentinel — ResolvePassphraseSource returns a
	// keyfileSource whose Passphrase returns ErrUseKeyfile and whose
	// KeyfilePath() points at the file. The boot path special-cases
	// it so we can use secrets.ReadKeyfile (no Argon2id required).
	if kf, ok := source.(*keyfileSource); ok {
		key, err := secrets.ReadKeyfile(kf.KeyfilePath())
		if err != nil {
			return nil, "", fmt.Errorf("daemon: read keyfile: %w", err)
		}
		sealer, err := secrets.NewSealer(db, key, meta, optsClock(opts))
		if err != nil {
			return nil, "", err
		}
		return sealer, source.Describe(), nil
	}

	passphrase, err := source.Passphrase()
	if err != nil {
		return nil, "", err
	}
	defer wipe(passphrase)

	key, err := secrets.DeriveKeyFromPassphrase(passphrase, meta.KDFSalt)
	if err != nil {
		return nil, "", err
	}
	sealer, err := secrets.NewSealer(db, key, meta, optsClock(opts))
	if err != nil {
		return nil, "", err
	}
	return sealer, source.Describe(), nil
}

func optsClock(o Options) clock.Clock {
	if o.Clock != nil {
		return o.Clock
	}
	return clock.Real{}
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// devModeTripwire enforces the "no quiet dev-mode" rule from
// design/secrets.md §Dev-mode. Any of these conditions trips the
// guard and refuses to boot insecure:
//
//   - RESY_SNIPE_PROD=1 (the production tripwire);
//   - /.dockerenv exists (we don't ship a "dev mode" container);
//   - data_dir resolves under /var/lib, /srv, or /opt.
//
// The TTY check from the design is enforced by the stdin source
// itself; if the operator is in dev-mode they are not prompting.
func devModeTripwire(getenv func(string) string) error {
	if getenv == nil {
		getenv = os.Getenv
	}
	if getenv("RESY_SNIPE_PROD") == "1" {
		return errors.New("daemon: RESY_SNIPE_PROD=1; refusing --insecure-no-encryption")
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return errors.New("daemon: /.dockerenv present; refusing --insecure-no-encryption inside a container")
	}
	return nil
}

func describeSigner(cfg Config) string {
	if cfg.Signer.Bin == "" {
		return "noop"
	}
	return "subprocess (" + cfg.Signer.Bin + ")"
}
