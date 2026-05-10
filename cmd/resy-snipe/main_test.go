package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
)

// fixedNow is the canonical pinned clock instant used by the cmd-level
// tests. Choosing a date well in the future keeps any default
// reservation-date math (now+7d) inside the same year.
var fixedNow = time.Date(2026, time.June, 1, 9, 0, 0, 0, time.UTC)

func newTestClock() clock.Clock { return clock.NewFake(fixedNow) }

func TestParseLogLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"  Info ", slog.LevelInfo},
	}
	for _, c := range cases {
		got, err := parseLogLevel(c.in)
		if err != nil {
			t.Errorf("parseLogLevel(%q) unexpected err: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseLogLevel(%q) = %v want %v", c.in, got, c.want)
		}
	}
}

func TestParseLogLevelRejectsUnknown(t *testing.T) {
	t.Parallel()
	if _, err := parseLogLevel("trace"); err == nil {
		t.Fatal("parseLogLevel(\"trace\") should error")
	}
	if _, err := parseLogLevel("loud"); err == nil {
		t.Fatal("parseLogLevel(\"loud\") should error")
	}
}

func TestRunRejectsBadLogLevel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := run([]string{"--log-level", "garbage"}, strings.NewReader(""), &buf, newTestClock())
	if err == nil {
		t.Fatal("run with bad log level should error")
	}
	if !strings.Contains(err.Error(), "log-level") {
		t.Fatalf("error should mention log-level: %v", err)
	}
}

func TestRunAcceptsValidLogLevel(t *testing.T) {
	t.Parallel()
	// Provide -snipe-time so the chosen release strategy default is
	// explicit and toIntent doesn't leave anything unset. -venue-id is
	// required now that the v1 hardcoded default (Dead Rabbit) is gone;
	// 38660 is Dead Rabbit, looked up via resy-snipe venue resolve.
	args := []string{"--log-level", "debug", "-snipe-time", "00:00", "-venue-id", "38660"}
	if err := run(args, strings.NewReader(""), io.Discard, newTestClock()); err != nil {
		t.Fatalf("run with valid log level: %v", err)
	}
}

func TestNewCLILoggerHonorsLevel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := newCLILogger(&buf, slog.LevelWarn)

	logger.Debug("d")
	logger.Info("i")
	if buf.Len() != 0 {
		t.Fatalf("debug/info emitted at warn level: %q", buf.String())
	}

	logger.Warn("w")
	logger.Error("e")
	out := buf.String()
	if !strings.Contains(out, "msg=w") {
		t.Errorf("warn line missing in output: %q", out)
	}
	if !strings.Contains(out, "msg=e") {
		t.Errorf("error line missing in output: %q", out)
	}
}

// Smoke check: a logger built by the CLI bootstrap can attach the
// canonical structured fields without any per-package magic.
func TestNewCLILoggerEmitsCanonicalFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := newCLILogger(&buf, slog.LevelInfo)
	logger.Info("snipe armed",
		slog.String(domain.LogKeySnipeID, "snp_abc"),
		slog.String(domain.LogKeyVenueRef, "resy:38660"),
		slog.Int(domain.LogKeyAttempt, 1),
		slog.String(domain.LogKeyProvider, "resy"),
	)
	out := buf.String()
	for _, want := range []string{
		"snipe_id=snp_abc",
		"venue_ref=resy:38660",
		"attempt=1",
		"provider=resy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log line missing %q: %s", want, out)
		}
	}
}
