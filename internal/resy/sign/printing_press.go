package sign

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"sync"
	"time"

	"resy-snipe/internal/clock"
)

// SubprocessConfig parameterises a Subprocess Signer.
//
// Bin is the absolute (or $PATH-resolvable) path to the signing
// binary. Empty Bin is invalid — construct via NewSubprocess and
// check the error.
//
// SignArgs and ResetArgs are the argv tails appended to Bin for the
// per-call sign and the post-challenge reset, respectively. Defaults:
// {"sign", "--provider", "resy"} and {"reset", "--provider", "resy"}.
//
// Timeout caps each subprocess invocation. Defaults to 10s; tests
// pass shorter values via Clock-driven exec contexts.
//
// Logger is required (panics fail-fast at construction). Clock is
// required for the same reason — Subprocess uses Clock.Now() for
// freshness tracking and Clock.After for the in-Reset bounded wait.
type SubprocessConfig struct {
	Bin       string
	SignArgs  []string
	ResetArgs []string
	Timeout   time.Duration
	Logger    *slog.Logger
	Clock     clock.Clock
}

// Subprocess is a Signer that shells out to a configured binary on
// every Sign call and on Reset.
//
// The expected stdout format is a JSON object:
//
//	{"headers": {"x-px-foo": "...", "x-resy-rotated": "..."}}
//
// Anything else (parse failure, non-zero exit, no headers field)
// surfaces as an error from Sign / Reset. The adapter treats Sign
// errors as best-effort and proceeds without the extra headers; a
// caller that wants strict mode should layer that policy on top.
//
// Subprocess caches the most recent successful Sign output and
// re-uses it for every subsequent call until Reset is invoked. This
// matches how a typical PerimeterX cookie set is valid for a
// short-but-stable window — re-shelling out per request would burn
// CPU and rate-limit on the upstream signer.
//
// Concurrency: a single subprocess invocation is serialized through
// mu. Multiple concurrent Sign calls share the cached headers; only
// the first call after a Reset (or at startup) actually runs the
// binary.
type Subprocess struct {
	bin       string
	signArgs  []string
	resetArgs []string
	timeout   time.Duration
	logger    *slog.Logger
	clock     clock.Clock

	mu       sync.Mutex
	cached   Headers
	cachedAt time.Time
}

// ErrSubprocessNotConfigured is returned by NewSubprocess when Bin is
// empty. Callers wire Subprocess only when the user has set
// RESY_SNIPE_SIGNER_BIN (or equivalent); the cmd/ layer falls back to
// Noop when this error is observed at boot.
var ErrSubprocessNotConfigured = errors.New("sign: subprocess bin not configured")

// NewSubprocess constructs a Subprocess Signer. Empty Bin returns
// ErrSubprocessNotConfigured so cmd/ can decide between fail-fast and
// fall-back-to-Noop.
func NewSubprocess(cfg SubprocessConfig) (*Subprocess, error) {
	if cfg.Logger == nil {
		return nil, errors.New("sign: nil logger")
	}
	if cfg.Clock == nil {
		return nil, errors.New("sign: nil clock")
	}
	if cfg.Bin == "" {
		return nil, ErrSubprocessNotConfigured
	}
	signArgs := cfg.SignArgs
	if len(signArgs) == 0 {
		signArgs = []string{"sign", "--provider", "resy"}
	}
	resetArgs := cfg.ResetArgs
	if len(resetArgs) == 0 {
		resetArgs = []string{"reset", "--provider", "resy"}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Subprocess{
		bin:       cfg.Bin,
		signArgs:  signArgs,
		resetArgs: resetArgs,
		timeout:   timeout,
		logger:    cfg.Logger,
		clock:     cfg.Clock,
	}, nil
}

// Sign returns the cached headers if present, else invokes the
// signing binary and caches the result. The path argument is
// forwarded to the binary as a trailing positional so future
// per-endpoint shaping is possible without an interface change.
func (s *Subprocess) Sign(ctx context.Context, path string) (Headers, error) {
	s.mu.Lock()
	if s.cached != nil {
		out := s.cached
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	headers, err := s.runSign(ctx, path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cached = headers
	s.cachedAt = s.clock.Now()
	s.mu.Unlock()
	return headers, nil
}

// Reset discards the cached headers and invokes the binary's reset
// subcommand so the upstream signer can mint a fresh cookie set. A
// non-zero exit from the reset call is logged but does not error —
// the adapter's contract is that Reset is best-effort, and the
// retry-once loop will surface ErrAntiBotChallenge from the second
// attempt regardless.
func (s *Subprocess) Reset(ctx context.Context) error {
	s.mu.Lock()
	s.cached = nil
	s.cachedAt = time.Time{}
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, s.bin, s.resetArgs...) //nolint:gosec // bin is operator-supplied via env var by design
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		s.logger.LogAttrs(ctx, slog.LevelWarn, "signer reset failed",
			slog.String("bin", s.bin),
			slog.String("stderr", truncate(stderr.String(), 256)),
			slog.String("err", err.Error()))
		return fmt.Errorf("sign: reset: %w", err)
	}
	return nil
}

func (s *Subprocess) runSign(ctx context.Context, path string) (Headers, error) {
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	args := append([]string{}, s.signArgs...)
	if path != "" {
		args = append(args, path)
	}
	cmd := exec.CommandContext(cctx, s.bin, args...) //nolint:gosec // bin is operator-supplied via env var by design
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		s.logger.LogAttrs(ctx, slog.LevelWarn, "signer invocation failed",
			slog.String("bin", s.bin),
			slog.String("path", path),
			slog.String("stderr", truncate(stderr.String(), 256)),
			slog.String("err", err.Error()))
		return nil, fmt.Errorf("sign: %s: %w", s.bin, err)
	}
	var env struct {
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		return nil, fmt.Errorf("sign: parse output: %w", err)
	}
	if len(env.Headers) == 0 {
		return Headers{}, nil
	}
	out := make(Headers, len(env.Headers))
	maps.Copy(out, env.Headers)
	return out, nil
}

// truncate is duplicated from internal/resy/errors.go to keep this
// package free of imports back into the parent. Returning the raw
// stderr risks leaking signing material; truncating mirrors the
// classifier's existing redaction floor.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// compile-time interface check.
var _ Signer = (*Subprocess)(nil)
