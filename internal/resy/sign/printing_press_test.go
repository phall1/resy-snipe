package sign_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/resy/sign"
)

// writeShellScript drops `body` at a path inside t.TempDir as an
// executable shell script. POSIX-only — the test skips on Windows.
func writeShellScript(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("subprocess tests rely on /bin/sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "signer.sh")
	const header = "#!/bin/sh\n"
	if err := os.WriteFile(path, []byte(header+body), 0o700); err != nil { //nolint:gosec // test fixture; needs +x to run
		t.Fatalf("write script: %v", err)
	}
	return path
}

func TestNewSubprocessRequiresBin(t *testing.T) {
	t.Parallel()
	_, err := sign.NewSubprocess(sign.SubprocessConfig{
		Logger: slog.New(slog.DiscardHandler),
		Clock:  clock.NewReal(),
	})
	if !errors.Is(err, sign.ErrSubprocessNotConfigured) {
		t.Fatalf("err = %v, want ErrSubprocessNotConfigured", err)
	}
}

func TestSubprocessSignParsesHeaders(t *testing.T) {
	t.Parallel()
	bin := writeShellScript(t, `printf '{"headers":{"x-px-foo":"bar","x-resy-rotated":"abc"}}'`)
	s, err := sign.NewSubprocess(sign.SubprocessConfig{
		Bin:    bin,
		Logger: slog.New(slog.DiscardHandler),
		Clock:  clock.NewReal(),
	})
	if err != nil {
		t.Fatalf("NewSubprocess: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := s.Sign(ctx, "/3/details")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if h["x-px-foo"] != "bar" {
		t.Errorf("x-px-foo: %q want bar", h["x-px-foo"])
	}
	if h["x-resy-rotated"] != "abc" {
		t.Errorf("x-resy-rotated: %q want abc", h["x-resy-rotated"])
	}
}

func TestSubprocessSignCachesAcrossCalls(t *testing.T) {
	t.Parallel()
	// Script counts invocations and emits a different value each time.
	// The cache contract is "second Sign returns first Sign's output";
	// if caching breaks we'll see "2" come back.
	bin := writeShellScript(t, `
n_file="$0.n"
[ -f "$n_file" ] || echo 0 > "$n_file"
n=$(cat "$n_file")
n=$((n+1))
echo "$n" > "$n_file"
printf '{"headers":{"n":"%s"}}' "$n"
`)
	s, err := sign.NewSubprocess(sign.SubprocessConfig{
		Bin:    bin,
		Logger: slog.New(slog.DiscardHandler),
		Clock:  clock.NewReal(),
	})
	if err != nil {
		t.Fatalf("NewSubprocess: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first, err := s.Sign(ctx, "/3/details")
	if err != nil {
		t.Fatalf("first Sign: %v", err)
	}
	second, err := s.Sign(ctx, "/3/book")
	if err != nil {
		t.Fatalf("second Sign: %v", err)
	}
	if first["n"] != "1" || second["n"] != "1" {
		t.Errorf("cache miss: first=%q second=%q (want both 1)", first["n"], second["n"])
	}
}

func TestSubprocessResetRefreshesCache(t *testing.T) {
	t.Parallel()
	bin := writeShellScript(t, `
n_file="$0.n"
[ -f "$n_file" ] || echo 0 > "$n_file"
n=$(cat "$n_file")
n=$((n+1))
echo "$n" > "$n_file"
# reset subcommand: just exit 0 with empty stdout (still records the bump).
case "$1" in
  reset) exit 0 ;;
esac
printf '{"headers":{"n":"%s"}}' "$n"
`)
	s, err := sign.NewSubprocess(sign.SubprocessConfig{
		Bin:    bin,
		Logger: slog.New(slog.DiscardHandler),
		Clock:  clock.NewReal(),
	})
	if err != nil {
		t.Fatalf("NewSubprocess: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first, err := s.Sign(ctx, "/3/details")
	if err != nil {
		t.Fatalf("first Sign: %v", err)
	}
	if first["n"] != "1" {
		t.Errorf("first n = %q, want 1", first["n"])
	}
	if err := s.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	second, err := s.Sign(ctx, "/3/details")
	if err != nil {
		t.Fatalf("second Sign: %v", err)
	}
	// Reset bumped n once (via reset call), Sign bumps once more → 3.
	if second["n"] != "3" {
		t.Errorf("after Reset, n = %q want 3 (reset bumped to 2, refetch to 3)", second["n"])
	}
}

func TestSubprocessSignNonZeroExitErrors(t *testing.T) {
	t.Parallel()
	bin := writeShellScript(t, `echo "boom" >&2; exit 7`)
	s, err := sign.NewSubprocess(sign.SubprocessConfig{
		Bin:    bin,
		Logger: slog.New(slog.DiscardHandler),
		Clock:  clock.NewReal(),
	})
	if err != nil {
		t.Fatalf("NewSubprocess: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Sign(ctx, "/3/details"); err == nil {
		t.Fatal("expected error from non-zero exit")
	}
}

func TestSubprocessSignParseErrorsBubble(t *testing.T) {
	t.Parallel()
	bin := writeShellScript(t, `printf 'not json'`)
	s, err := sign.NewSubprocess(sign.SubprocessConfig{
		Bin:    bin,
		Logger: slog.New(slog.DiscardHandler),
		Clock:  clock.NewReal(),
	})
	if err != nil {
		t.Fatalf("NewSubprocess: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Sign(ctx, "/3/details"); err == nil {
		t.Fatal("expected parse error")
	}
}
