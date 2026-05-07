package main

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"resy-snipe/internal/domain"
)

// pinnedNow is the deterministic "now" the tests pin so default
// reservation-date math (now+7d) is reproducible. It deliberately sits
// in late spring 2026 so applyDefaults' AddDate(0,0,7) doesn't cross a
// month boundary in the assertions below.
var pinnedNow = time.Date(2026, time.June, 1, 12, 0, 0, 0, time.Local)

func TestParseFlagsAllProvided(t *testing.T) {
	t.Parallel()
	args := []string{
		"-date", "2026-07-04",
		"-party-size", "2",
		"-venue-id", "466",
		"-res-times", "18:30,19:00",
		"-table-types", "Bar,Parlor",
		"-snipe-date", "2026-06-15",
		"-snipe-time", "09:00",
		"-release-strategy", "explicit",
		"-retry-window", "10m",
	}
	opts, err := parseFlags(args, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if opts.partySize != 2 || opts.venueID != 466 {
		t.Errorf("flag values not parsed: %+v", opts)
	}
	if !opts.snipeTimeProvided {
		t.Error("snipeTimeProvided should be true")
	}
	if !opts.releaseStrategyProvided {
		t.Error("releaseStrategyProvided should be true")
	}
	if opts.retryWindow != 10*time.Minute {
		t.Errorf("retryWindow=%v want 10m", opts.retryWindow)
	}
}

func TestToIntentFlagDriven(t *testing.T) {
	t.Parallel()
	opts := cliOptions{
		resDate:                 "2026-07-04",
		partySize:               4,
		venueID:                 466,
		resTimes:                "18:30,19:00",
		tableTypes:              "Bar,Parlor",
		snipeDate:               "2026-06-15",
		snipeTime:               "09:00",
		releaseStrategy:         strategyExplicit,
		retryWindow:             10 * time.Minute,
		snipeTimeProvided:       true,
		releaseStrategyProvided: true,
	}
	intent, err := toIntent(opts, pinnedNow)
	if err != nil {
		t.Fatalf("toIntent: %v", err)
	}
	if intent.Venue.Provider != "resy" || intent.Venue.Ref != "466" {
		t.Errorf("venue = %v", intent.Venue)
	}
	if intent.Date.String() != "2026-07-04" {
		t.Errorf("date = %s", intent.Date)
	}
	if intent.PartySize != 4 {
		t.Errorf("party = %d", intent.PartySize)
	}
	// 2 times * 2 table types = exactly 4 SlotPreferences. NEVER 6 (the
	// legacy bug also appended an empty-table-type variant per time).
	if got := len(intent.SlotPrefs); got != 4 {
		t.Fatalf("slot prefs = %d, want 4 (regression: legacy code produced 6)", got)
	}
	rel, ok := intent.Release.(domain.ExplicitRelease)
	if !ok {
		t.Fatalf("release = %T want ExplicitRelease", intent.Release)
	}
	wantAt := time.Date(2026, time.June, 15, 9, 0, 0, 0, time.Local)
	if !rel.At.Equal(wantAt) {
		t.Errorf("release.At = %v want %v", rel.At, wantAt)
	}
}

// TestBuildResTimeTypesRegression: the legacy buildResTimeTypes bug
// produced N*M + N SlotPreferences (one extra empty-table-type variant
// per time). C1 must produce exactly N*M when M>0.
func TestBuildResTimeTypesRegression(t *testing.T) {
	t.Parallel()
	opts := cliOptions{
		resDate:           "2026-07-04",
		partySize:         2,
		venueID:           38660,
		resTimes:          "18:30",
		tableTypes:        "Bar,Parlor",
		snipeDate:         "2026-06-15",
		snipeTime:         "09:00",
		snipeTimeProvided: true,
		retryWindow:       defaultRetryWindow,
	}
	intent, err := toIntent(opts, pinnedNow)
	if err != nil {
		t.Fatalf("toIntent: %v", err)
	}
	if got := len(intent.SlotPrefs); got != 2 {
		t.Fatalf("slot prefs = %d, want 2 (legacy bug produced 3)", got)
	}
	for _, p := range intent.SlotPrefs {
		if p.TableType == "" {
			t.Errorf("found unexpected empty table-type slot: %+v", p)
		}
	}
}

func TestToIntentNoTableTypes(t *testing.T) {
	t.Parallel()
	opts := cliOptions{
		resDate:           "2026-07-04",
		partySize:         2,
		venueID:           38660,
		resTimes:          "18:30,19:00,19:30",
		snipeDate:         "2026-06-15",
		snipeTime:         "09:00",
		snipeTimeProvided: true,
		retryWindow:       defaultRetryWindow,
	}
	intent, err := toIntent(opts, pinnedNow)
	if err != nil {
		t.Fatalf("toIntent: %v", err)
	}
	if got := len(intent.SlotPrefs); got != 3 {
		t.Fatalf("slot prefs = %d, want 3", got)
	}
	for _, p := range intent.SlotPrefs {
		if p.TableType != "" {
			t.Errorf("expected empty TableType, got %q", p.TableType)
		}
	}
}

func TestToIntentTableTypesNoneToken(t *testing.T) {
	t.Parallel()
	opts := cliOptions{
		resDate:           "2026-07-04",
		partySize:         2,
		venueID:           38660,
		resTimes:          "18:30",
		tableTypes:        "none",
		snipeDate:         "2026-06-15",
		snipeTime:         "09:00",
		snipeTimeProvided: true,
		retryWindow:       defaultRetryWindow,
	}
	intent, err := toIntent(opts, pinnedNow)
	if err != nil {
		t.Fatalf("toIntent: %v", err)
	}
	if got := len(intent.SlotPrefs); got != 1 {
		t.Fatalf("slot prefs = %d, want 1 (none → no table types)", got)
	}
	if intent.SlotPrefs[0].TableType != "" {
		t.Errorf("table type after 'none' = %q want empty", intent.SlotPrefs[0].TableType)
	}
}

func TestReleaseStrategySelection(t *testing.T) {
	t.Parallel()
	base := cliOptions{
		resDate:                 "2026-07-04",
		partySize:               2,
		venueID:                 38660,
		resTimes:                "18:30",
		snipeDate:               "2026-06-15",
		snipeTime:               "09:00",
		snipeTimeProvided:       true,
		releaseStrategyProvided: true,
		retryWindow:             15 * time.Minute,
	}

	cases := []struct {
		name     string
		strategy string
		check    func(t *testing.T, r domain.ReleaseStrategy)
	}{
		{
			name:     "explicit",
			strategy: strategyExplicit,
			check: func(t *testing.T, r domain.ReleaseStrategy) {
				t.Helper()
				v, ok := r.(domain.ExplicitRelease)
				if !ok {
					t.Fatalf("got %T want ExplicitRelease", r)
				}
				want := time.Date(2026, time.June, 15, 9, 0, 0, 0, time.Local)
				if !v.At.Equal(want) {
					t.Errorf("at = %v want %v", v.At, want)
				}
			},
		},
		{
			name:     "discovered",
			strategy: strategyDiscovered,
			check: func(t *testing.T, r domain.ReleaseStrategy) {
				t.Helper()
				v, ok := r.(domain.DiscoveredRelease)
				if !ok {
					t.Fatalf("got %T want DiscoveredRelease", r)
				}
				center := time.Date(2026, time.June, 15, 9, 0, 0, 0, time.Local)
				if !v.ProbeFrom.Equal(center.Add(-15 * time.Minute)) {
					t.Errorf("ProbeFrom = %v want %v", v.ProbeFrom, center.Add(-15*time.Minute))
				}
				if !v.ProbeUntil.Equal(center.Add(15 * time.Minute)) {
					t.Errorf("ProbeUntil = %v want %v", v.ProbeUntil, center.Add(15*time.Minute))
				}
			},
		},
		{
			name:     "continuous",
			strategy: strategyContinuous,
			check: func(t *testing.T, r domain.ReleaseStrategy) {
				t.Helper()
				v, ok := r.(domain.ContinuousRelease)
				if !ok {
					t.Fatalf("got %T want ContinuousRelease", r)
				}
				if !v.Until.Equal(pinnedNow.Add(15 * time.Minute)) {
					t.Errorf("Until = %v want %v", v.Until, pinnedNow.Add(15*time.Minute))
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := base
			opts.releaseStrategy = tc.strategy
			intent, err := toIntent(opts, pinnedNow)
			if err != nil {
				t.Fatalf("toIntent: %v", err)
			}
			tc.check(t, intent.Release)
		})
	}
}

func TestDefaultReleaseStrategyByContext(t *testing.T) {
	t.Parallel()
	t.Run("with snipe-time defaults to explicit", func(t *testing.T) {
		t.Parallel()
		opts := cliOptions{
			resDate:           "2026-07-04",
			partySize:         2,
			venueID:           38660,
			resTimes:          "18:30",
			snipeDate:         "2026-06-15",
			snipeTime:         "09:00",
			snipeTimeProvided: true,
			retryWindow:       defaultRetryWindow,
		}
		intent, err := toIntent(opts, pinnedNow)
		if err != nil {
			t.Fatalf("toIntent: %v", err)
		}
		if _, ok := intent.Release.(domain.ExplicitRelease); !ok {
			t.Fatalf("release = %T want ExplicitRelease", intent.Release)
		}
	})

	t.Run("without snipe-time defaults to discovered", func(t *testing.T) {
		t.Parallel()
		opts := cliOptions{
			resDate:           "2026-07-04",
			partySize:         2,
			venueID:           38660,
			resTimes:          "18:30",
			snipeDate:         "2026-06-15",
			snipeTime:         "00:00",
			snipeTimeProvided: false,
			retryWindow:       defaultRetryWindow,
		}
		intent, err := toIntent(opts, pinnedNow)
		if err != nil {
			t.Fatalf("toIntent: %v", err)
		}
		if _, ok := intent.Release.(domain.DiscoveredRelease); !ok {
			t.Fatalf("release = %T want DiscoveredRelease", intent.Release)
		}
	})
}

func TestRetryWindowReflectedInRelease(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		raw    string
		expect time.Duration
	}{
		{"5m", "5m", 5 * time.Minute},
		{"1h", "1h", time.Hour},
		{"45s", "45s", 45 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := []string{
				"-date", "2026-07-04",
				"-party-size", "2",
				"-venue-id", "38660",
				"-res-times", "18:30",
				"-snipe-date", "2026-06-15",
				"-release-strategy", "continuous",
				"-retry-window", tc.raw,
			}
			opts, err := parseFlags(args, io.Discard)
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if opts.retryWindow != tc.expect {
				t.Fatalf("retryWindow = %v want %v", opts.retryWindow, tc.expect)
			}
			opts.applyDefaults(pinnedNow)
			intent, err := toIntent(opts, pinnedNow)
			if err != nil {
				t.Fatalf("toIntent: %v", err)
			}
			rel, ok := intent.Release.(domain.ContinuousRelease)
			if !ok {
				t.Fatalf("release = %T want ContinuousRelease", intent.Release)
			}
			if !rel.Until.Equal(pinnedNow.Add(tc.expect)) {
				t.Errorf("Until = %v want %v", rel.Until, pinnedNow.Add(tc.expect))
			}
		})
	}
}

func TestToIntentValidation(t *testing.T) {
	t.Parallel()
	good := func() cliOptions {
		return cliOptions{
			resDate:           "2026-07-04",
			partySize:         2,
			venueID:           38660,
			resTimes:          "18:30",
			snipeDate:         "2026-06-15",
			snipeTime:         "09:00",
			snipeTimeProvided: true,
			retryWindow:       defaultRetryWindow,
		}
	}

	cases := []struct {
		name    string
		mut     func(o *cliOptions)
		wantSub string
	}{
		{"party-size zero", func(o *cliOptions) { o.partySize = 0 }, "party-size"},
		{"party-size negative", func(o *cliOptions) { o.partySize = -1 }, "party-size"},
		{"missing venue", func(o *cliOptions) { o.venueID = 0 }, "venue"},
		{"missing date", func(o *cliOptions) { o.resDate = "" }, "date"},
		{"unparseable date", func(o *cliOptions) { o.resDate = "next-tuesday" }, "date"},
		{"empty res-times", func(o *cliOptions) { o.resTimes = "" }, "res-times"},
		{"unparseable res-time", func(o *cliOptions) { o.resTimes = "ten thirty" }, "res-times"},
		{"bad release-strategy", func(o *cliOptions) {
			o.releaseStrategy = "yolo"
			o.releaseStrategyProvided = true
		}, "release-strategy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := good()
			tc.mut(&opts)
			_, err := toIntent(opts, pinnedNow)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

// TestPromptHelpersWithCannedInput exercises the prompt helpers using
// strings.NewReader so the deterministic input/output contract is
// pinned without involving real stdin.
func TestPromptHelpersWithCannedInput(t *testing.T) {
	t.Parallel()

	t.Run("promptDate accepts default on blank line", func(t *testing.T) {
		t.Parallel()
		out := &bytes.Buffer{}
		got, err := promptDate(bufio.NewReader(strings.NewReader("\n")), out, "Date", "2026-07-04")
		if err != nil {
			t.Fatalf("promptDate: %v", err)
		}
		if got != "2026-07-04" {
			t.Errorf("got %q want default", got)
		}
		if !strings.Contains(out.String(), "Date [2026-07-04]:") {
			t.Errorf("prompt label missing: %q", out.String())
		}
	})

	t.Run("promptDate retries on invalid", func(t *testing.T) {
		t.Parallel()
		out := &bytes.Buffer{}
		// First line: invalid date. Second line: valid override.
		got, err := promptDate(bufio.NewReader(strings.NewReader("not-a-date\n2026-08-01\n")), out, "Date", "2026-07-04")
		if err != nil {
			t.Fatalf("promptDate: %v", err)
		}
		if got != "2026-08-01" {
			t.Errorf("got %q want 2026-08-01", got)
		}
		if !strings.Contains(out.String(), "Invalid date") {
			t.Errorf("validation msg missing: %q", out.String())
		}
	})

	t.Run("promptInt accepts integer override", func(t *testing.T) {
		t.Parallel()
		got, err := promptInt(bufio.NewReader(strings.NewReader("4\n")), io.Discard, "Party", 2)
		if err != nil {
			t.Fatalf("promptInt: %v", err)
		}
		if got != 4 {
			t.Errorf("got %d want 4", got)
		}
	})

	t.Run("promptInt blank returns default", func(t *testing.T) {
		t.Parallel()
		got, err := promptInt(bufio.NewReader(strings.NewReader("\n")), io.Discard, "Party", 2)
		if err != nil {
			t.Fatalf("promptInt: %v", err)
		}
		if got != 2 {
			t.Errorf("got %d want 2", got)
		}
	})

	t.Run("promptVenue accepts list number", func(t *testing.T) {
		t.Parallel()
		got, err := promptVenue(bufio.NewReader(strings.NewReader("1\n")), io.Discard, venueRubirosa)
		if err != nil {
			t.Fatalf("promptVenue: %v", err)
		}
		if got != venueDeadRabbit {
			t.Errorf("got %d want %d (Dead Rabbit at index 1)", got, venueDeadRabbit)
		}
	})

	t.Run("promptVenue accepts venue name", func(t *testing.T) {
		t.Parallel()
		got, err := promptVenue(bufio.NewReader(strings.NewReader("Carbone\n")), io.Discard, venueDeadRabbit)
		if err != nil {
			t.Fatalf("promptVenue: %v", err)
		}
		if got != venueCarbone {
			t.Errorf("got %d want %d", got, venueCarbone)
		}
	})

	t.Run("promptResTimes rejects empty list", func(t *testing.T) {
		t.Parallel()
		out := &bytes.Buffer{}
		got, err := promptResTimes(bufio.NewReader(strings.NewReader(",\n18:30\n")), out, "Times", "")
		if err != nil {
			t.Fatalf("promptResTimes: %v", err)
		}
		if got != "18:30" {
			t.Errorf("got %q want 18:30", got)
		}
		if !strings.Contains(out.String(), "Provide at least one") {
			t.Errorf("validation msg missing: %q", out.String())
		}
	})

	t.Run("runInteractive end-to-end", func(t *testing.T) {
		t.Parallel()
		opts := &cliOptions{
			resDate:    "2026-07-04",
			partySize:  2,
			venueID:    venueDeadRabbit,
			resTimes:   "18:30",
			tableTypes: "Bar",
			snipeDate:  "2026-06-15",
			snipeTime:  "09:00",
		}
		// Empty answers → keep all defaults.
		input := strings.Repeat("\n", 7)
		if err := runInteractive(opts, strings.NewReader(input), io.Discard); err != nil {
			t.Fatalf("runInteractive: %v", err)
		}
		if opts.resDate != "2026-07-04" || opts.partySize != 2 || opts.venueID != venueDeadRabbit {
			t.Errorf("defaults clobbered: %+v", opts)
		}
		if opts.resTimes != "18:30" || opts.tableTypes != "Bar" {
			t.Errorf("res/table defaults clobbered: %+v", opts)
		}
		if opts.snipeDate != "2026-06-15" || opts.snipeTime != "09:00" {
			t.Errorf("snipe defaults clobbered: %+v", opts)
		}
	})
}
