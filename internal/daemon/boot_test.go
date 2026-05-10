package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/daemon"
	"resy-snipe/internal/store"
)

// fakePassphraseSource is the test double we use to bypass stdin
// prompts and env vars during boot.
type fakePassphraseSource struct {
	bytes []byte
	desc  string
}

func (f *fakePassphraseSource) Passphrase() ([]byte, error) {
	out := make([]byte, len(f.bytes))
	copy(out, f.bytes)
	return out, nil
}

func (f *fakePassphraseSource) Describe() string {
	if f.desc != "" {
		return f.desc
	}
	return "fake passphrase"
}

func bootHarness(t *testing.T, modify func(c *daemon.Config), modifyOpts func(o *daemon.Options)) (*daemon.Daemon, *bytes.Buffer) {
	t.Helper()
	cfg := daemon.DefaultConfig()
	cfg.Daemon.DataDir = t.TempDir()
	if modify != nil {
		modify(&cfg)
	}
	var banner bytes.Buffer
	opts := daemon.Options{
		PassphraseSource: &fakePassphraseSource{bytes: []byte("hunter2-correct")},
		BannerWriter:     &banner,
		Clock:            clock.NewFake(time.Unix(1_700_000_000, 0)),
		Version:          "test",
		Commit:           "deadbeef",
	}
	if modifyOpts != nil {
		modifyOpts(&opts)
	}
	d, err := daemon.Boot(context.Background(), cfg, opts)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, &banner
}

func TestBootHappyPath(t *testing.T) {
	t.Parallel()
	d, banner := bootHarness(t, nil, nil)
	if d.DB == nil {
		t.Fatal("DB not attached")
	}
	if d.Sealer == nil {
		t.Fatal("Sealer not attached")
	}
	if got := banner.String(); !strings.Contains(got, "ready.") {
		t.Fatalf("banner missing 'ready.': %q", got)
	}
	if got := banner.String(); !strings.Contains(got, "schema v") {
		t.Fatalf("banner missing schema version: %q", got)
	}
}

func TestBootNotifyReadyFires(t *testing.T) {
	t.Parallel()
	var ready bool
	bootHarness(t, nil, func(o *daemon.Options) {
		o.NotifyReady = func() { ready = true }
	})
	if !ready {
		t.Fatal("NotifyReady was not called")
	}
}

func TestBootLockfileContention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := daemon.DefaultConfig()
	cfg.Daemon.DataDir = dir

	first, err := daemon.Boot(context.Background(), cfg, daemon.Options{
		PassphraseSource: &fakePassphraseSource{bytes: []byte("p")},
		BannerWriter:     &bytes.Buffer{},
		Clock:            clock.NewFake(time.Unix(1, 0)),
	})
	if err != nil {
		t.Fatalf("first Boot: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	_, err = daemon.Boot(context.Background(), cfg, daemon.Options{
		PassphraseSource: &fakePassphraseSource{bytes: []byte("p")},
		BannerWriter:     &bytes.Buffer{},
		Clock:            clock.NewFake(time.Unix(1, 0)),
	})
	if !errors.Is(err, daemon.ErrLockHeld) {
		t.Fatalf("second Boot: err=%v want ErrLockHeld", err)
	}
}

func TestBootReleasesLockOnClose(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := daemon.DefaultConfig()
	cfg.Daemon.DataDir = dir

	first, err := daemon.Boot(context.Background(), cfg, daemon.Options{
		PassphraseSource: &fakePassphraseSource{bytes: []byte("p")},
		BannerWriter:     &bytes.Buffer{},
		Clock:            clock.NewFake(time.Unix(1, 0)),
	})
	if err != nil {
		t.Fatalf("first Boot: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Reboot succeeds — the lock was released.
	second, err := daemon.Boot(context.Background(), cfg, daemon.Options{
		PassphraseSource: &fakePassphraseSource{bytes: []byte("p")},
		BannerWriter:     &bytes.Buffer{},
		Clock:            clock.NewFake(time.Unix(2, 0)),
	})
	if err != nil {
		t.Fatalf("reboot: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
}

func TestBootRefusesNewerSchema(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.db")
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Inject a schema_migrations row claiming a version newer than
	// any embedded migration — simulates an older binary running
	// against a newer DB.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
		9999, "from-the-future"); err != nil {
		t.Fatalf("inject future version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db close: %v", err)
	}

	cfg := daemon.DefaultConfig()
	cfg.Daemon.DataDir = dir
	_, err = daemon.Boot(context.Background(), cfg, daemon.Options{
		PassphraseSource: &fakePassphraseSource{bytes: []byte("p")},
		BannerWriter:     &bytes.Buffer{},
		Clock:            clock.NewFake(time.Unix(3, 0)),
	})
	if !errors.Is(err, store.ErrSchemaNewer) {
		t.Fatalf("Boot: err=%v want ErrSchemaNewer", err)
	}
}

func TestBootInsecureRequiresExplicitOption(t *testing.T) {
	t.Parallel()
	cfg := daemon.DefaultConfig()
	cfg.Daemon.DataDir = t.TempDir()
	cfg.Secrets.Mode = "insecure"
	_, err := daemon.Boot(context.Background(), cfg, daemon.Options{
		BannerWriter: &bytes.Buffer{},
		Clock:        clock.NewFake(time.Unix(1, 0)),
	})
	if err == nil {
		t.Fatal("Boot accepted insecure without AllowInsecureNoEncryption")
	}
}

func TestBootInsecureTripwireProd(t *testing.T) {
	t.Parallel()
	cfg := daemon.DefaultConfig()
	cfg.Daemon.DataDir = t.TempDir()
	cfg.Secrets.Mode = "insecure"
	getenv := func(k string) string {
		if k == "RESY_SNIPE_PROD" {
			return "1"
		}
		return ""
	}
	_, err := daemon.Boot(context.Background(), cfg, daemon.Options{
		AllowInsecureNoEncryption: true,
		BannerWriter:              &bytes.Buffer{},
		Clock:                     clock.NewFake(time.Unix(1, 0)),
		Getenv:                    getenv,
	})
	if err == nil {
		t.Fatal("Boot accepted insecure with RESY_SNIPE_PROD=1")
	}
	if !strings.Contains(err.Error(), "RESY_SNIPE_PROD") {
		t.Fatalf("error should mention RESY_SNIPE_PROD: %v", err)
	}
}

func TestBootInsecureSucceedsWithTripwireClear(t *testing.T) {
	t.Parallel()
	cfg := daemon.DefaultConfig()
	cfg.Daemon.DataDir = t.TempDir()
	cfg.Secrets.Mode = "insecure"
	d, err := daemon.Boot(context.Background(), cfg, daemon.Options{
		AllowInsecureNoEncryption: true,
		BannerWriter:              &bytes.Buffer{},
		Clock:                     clock.NewFake(time.Unix(1, 0)),
		Getenv:                    func(string) string { return "" },
	})
	if err != nil {
		t.Fatalf("Boot insecure: %v", err)
	}
	defer func() { _ = d.Close() }()
	if d.Sealer != nil {
		t.Errorf("Sealer should be nil in insecure mode")
	}
	if d.Meta.ActiveVersion == 0 {
		t.Errorf("Meta should still be loaded in insecure mode")
	}
}

func TestBootRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	cfg := daemon.DefaultConfig()
	cfg.Daemon.Bind = "not-an-addr-port"
	_, err := daemon.Boot(context.Background(), cfg, daemon.Options{
		PassphraseSource: &fakePassphraseSource{bytes: []byte("p")},
		BannerWriter:     &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("Boot accepted invalid config")
	}
}
