package notify_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/notify"
)

// fixedClockTime is the canonical instant the table-driven cases pin.
// Using a single instant for all transitions keeps the expected output
// trivially comparable; concurrency / clock-advance behavior is
// covered separately.
var fixedClockTime = time.Date(2026, time.June, 1, 14, 32, 1, 0, time.UTC)

func newNotifierWithBuf() (*notify.StdoutNotifier, *bytes.Buffer) {
	var buf bytes.Buffer
	clk := clock.NewFake(fixedClockTime)
	return notify.NewStdoutNotifier(&buf, clk), &buf
}

func TestStdoutNotifier_Transition_TableDriven(t *testing.T) {
	t.Parallel()

	type tcase struct {
		name  string
		from  domain.Status
		to    domain.Status
		attrs []slog.Attr
		want  string
	}
	cases := []tcase{
		{
			name: "no attrs",
			from: domain.StatusDiscovering,
			to:   domain.StatusAwaiting,
			want: "[14:32:01] discovering -> awaiting\n",
		},
		{
			name: "one attr",
			from: domain.StatusDiscovering,
			to:   domain.StatusAwaiting,
			attrs: []slog.Attr{
				slog.String("release", "09:00:00 ET"),
			},
			want: "[14:32:01] discovering -> awaiting (release=09:00:00 ET)\n",
		},
		{
			name: "multi attrs preserve order",
			from: domain.StatusFinding,
			to:   domain.StatusBooking,
			attrs: []slog.Attr{
				slog.String(domain.LogKeyResyRequestID, "req-7"),
				slog.Int(domain.LogKeyAttempt, 3),
			},
			want: "[14:32:01] finding -> booking (resy_request_id=req-7 attempt=3)\n",
		},
		{
			name: "group attrs flatten to dotted keys",
			from: domain.StatusSubmitted,
			to:   domain.StatusScheduled,
			attrs: []slog.Attr{
				slog.Group("scheduled", slog.String("at", "2026-06-01T09:00:00Z")),
			},
			want: "[14:32:01] submitted -> scheduled (scheduled.at=2026-06-01T09:00:00Z)\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			n, buf := newNotifierWithBuf()
			n.Transition(context.Background(), domain.SnipeID("snp_x"), c.from, c.to, c.attrs)
			if got := buf.String(); got != c.want {
				t.Errorf("got %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestStdoutNotifier_Result_TableDriven(t *testing.T) {
	t.Parallel()

	type tcase struct {
		name    string
		success bool
		message string
		attrs   []slog.Attr
		want    string
	}
	cases := []tcase{
		{
			name:    "success without attrs",
			success: true,
			message: "confirmation OK",
			want:    "[14:32:01] BOOKED: confirmation OK\n",
		},
		{
			name:    "success with confirmation attrs",
			success: true,
			message: "Dead Rabbit 2026-07-04 19:00",
			attrs: []slog.Attr{
				slog.String(domain.LogKeyResyRequestID, "req-99"),
				slog.String("confirmation_id", "abc-123"),
			},
			want: "[14:32:01] BOOKED: Dead Rabbit 2026-07-04 19:00 (resy_request_id=req-99 confirmation_id=abc-123)\n",
		},
		{
			name:    "failure surfaces error class",
			success: false,
			message: "provider: anti-bot challenge",
			attrs: []slog.Attr{
				slog.String("error_class", "ErrAntiBotChallenge"),
			},
			want: "[14:32:01] FAILED: provider: anti-bot challenge (error_class=ErrAntiBotChallenge)\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			n, buf := newNotifierWithBuf()
			n.Result(context.Background(), domain.SnipeID("snp_x"), c.success, c.message, c.attrs)
			if got := buf.String(); got != c.want {
				t.Errorf("got %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestStdoutNotifier_Close_NoOpReturnsNil(t *testing.T) {
	t.Parallel()
	n, _ := newNotifierWithBuf()
	if err := n.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// Concurrent writes must not interleave mid-line. We assert a much
// weaker but still meaningful invariant: every emitted line must end
// with a single trailing newline and every body must begin with the
// "[15:04:05] " timestamp prefix. A torn write would violate either.
func TestStdoutNotifier_ConcurrentWritesDoNotInterleave(t *testing.T) {
	t.Parallel()
	n, buf := newNotifierWithBuf()

	var wg sync.WaitGroup
	const goroutines = 16
	const linesPer = 32
	for range goroutines {
		wg.Go(func() {
			ctx := context.Background()
			for range linesPer {
				n.Transition(ctx, domain.SnipeID("snp_x"),
					domain.StatusDiscovering, domain.StatusAwaiting, nil)
			}
		})
	}
	wg.Wait()

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if want := goroutines * linesPer; len(lines) != want {
		t.Fatalf("line count=%d want %d", len(lines), want)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "[14:32:01] ") {
			t.Errorf("line %d missing timestamp prefix: %q", i, line)
		}
		if line != "[14:32:01] discovering -> awaiting" {
			t.Errorf("line %d torn or wrong: %q", i, line)
		}
	}
}

// Smoke check: NewStdoutNotifier panics on nil writer / nil clock so a
// boot misconfig fails fast instead of producing silent dropped output.
func TestStdoutNotifier_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil writer")
		}
	}()
	notify.NewStdoutNotifier(nil, clock.NewFake(fixedClockTime))
}

func TestStdoutNotifier_RejectsNilClock(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil clock")
		}
	}()
	notify.NewStdoutNotifier(&bytes.Buffer{}, nil)
}
