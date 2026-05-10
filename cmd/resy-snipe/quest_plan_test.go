package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// ---- test doubles ----------------------------------------------------------

// fakeQuestPlanService satisfies questPlanService for the CLI tests.
// It records the last (userID, goal) pair and returns either a canned
// Plan or a canned error. Keeping the fake in this file avoids
// pulling the full Standard service wiring into the test binary —
// the CLI surface is the unit under test.
type fakeQuestPlanService struct {
	calls    int
	lastUID  domain.UserID
	lastGoal domain.Goal
	plan     domain.Plan
	err      error
}

func (f *fakeQuestPlanService) PlanQuest(
	_ context.Context, uid domain.UserID, goal domain.Goal,
) (domain.Plan, error) {
	f.calls++
	f.lastUID = uid
	f.lastGoal = goal
	if f.err != nil {
		return domain.Plan{}, f.err
	}
	return f.plan, nil
}

// swapQuestPlanService installs the fake for the duration of the test.
// The package-level openQuestServiceFn is the one mutable seam the
// CLI exposes; we restore it on cleanup so parallel-unfriendly tests
// (which we annotate) don't bleed state across runs.
func swapQuestPlanService(t *testing.T, svc questPlanService) {
	t.Helper()
	prev := openQuestServiceFn
	openQuestServiceFn = func(_ context.Context, _ *slog.Logger, _ clock.Clock) (questPlanService, func() error, error) {
		return svc, func() error { return nil }, nil
	}
	t.Cleanup(func() { openQuestServiceFn = prev })
}

// samplePlan builds a Plan plausible enough to render. Both table and
// JSON tests share it so the assertions stay focused on the formatter.
func samplePlan(t *testing.T) domain.Plan {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	drop := time.Date(2026, 5, 12, 0, 0, 0, 0, loc)
	venue := domain.Venue{Provider: "resy", Ref: "49716", Name: "Astoria DC", TZ: loc}
	goal := domain.Goal{
		VenueQuery: domain.VenueQueryURL{URL: "https://resy.com/cities/washington-dc/venues/astoria-dc"},
		Date:       domain.NewDate(2026, time.June, 9),
		Party:      2,
		TimePrefs: domain.TimeWindow{
			Start:    domain.NewWallTime(18, 30, 0),
			End:      domain.NewWallTime(20, 30, 0),
			Priority: domain.PriorityEarlier,
		},
		AccountID: "op@example.com",
	}
	return domain.Plan{
		Goal:            goal,
		Venue:           venue,
		DropMoment:      drop,
		Strategy:        domain.ExplicitRelease{At: drop},
		FireSchedule:    []time.Time{drop},
		SigningRequired: false,
		Hash:            "sha256:a8f2bbbbccccddddeeeeffff00001111",
		Notes:           []string{"observed_release_times has 4 prior samples within ±10s"},
		PlanHashVersion: domain.PlanHashVersionV1,
		ComputedAt:      drop.Add(-time.Hour),
	}
}

// ---- tests -----------------------------------------------------------------

func TestRunQuestPlanCmd_URLHintsPopulateGoal(t *testing.T) { //nolint:paralleltest // mutates openQuestServiceFn.
	fake := &fakeQuestPlanService{plan: samplePlan(t)}
	swapQuestPlanService(t, fake)

	out := &bytes.Buffer{}
	err := runQuestPlanCmd(
		context.Background(),
		[]string{
			"-time", "18:30..20:30",
			"-account", "op@example.com",
			"-user", "usr_op01",
			"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
		},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err != nil {
		t.Fatalf("runQuestPlanCmd: %v\n%s", err, out.String())
	}

	if fake.calls != 1 {
		t.Fatalf("PlanQuest calls = %d, want 1", fake.calls)
	}
	if fake.lastUID != domain.UserID("usr_op01") {
		t.Errorf("lastUID = %q, want usr_op01", fake.lastUID)
	}
	// URL hints should pre-fill date + party.
	if fake.lastGoal.Date != (domain.NewDate(2026, time.June, 9)) {
		t.Errorf("goal date = %s, want 2026-06-09", fake.lastGoal.Date)
	}
	if fake.lastGoal.Party != 2 {
		t.Errorf("goal party = %d, want 2", fake.lastGoal.Party)
	}

	body := out.String()
	wantSubstrings := []string{
		"PLAN",
		"Astoria DC",
		"resy:49716",
		"2026-06-09",
		"party:        2",
		"Explicit",
		"hash:",
		"a8f2",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(body, w) {
			t.Errorf("output missing %q\n----\n%s", w, body)
		}
	}
	// Time prefs should be expanded into the 30-minute grid.
	if !strings.Contains(body, "18:30") || !strings.Contains(body, "20:30") {
		t.Errorf("output missing expanded slot times: %s", body)
	}
}

func TestRunQuestPlanCmd_FlagOverridesURLHint(t *testing.T) { //nolint:paralleltest // mutates openQuestServiceFn.
	fake := &fakeQuestPlanService{plan: samplePlan(t)}
	swapQuestPlanService(t, fake)

	out := &bytes.Buffer{}
	// URL says seats=2, date=2026-06-09; flag forces party=4, date=2026-07-04.
	err := runQuestPlanCmd(
		context.Background(),
		[]string{
			"-time", "19:00..21:00",
			"-date", "2026-07-04",
			"-party", "4",
			"-account", "op@example.com",
			"-user", "usr_op01",
			"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
		},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err != nil {
		t.Fatalf("runQuestPlanCmd: %v\n%s", err, out.String())
	}
	if fake.lastGoal.Date != domain.NewDate(2026, time.July, 4) {
		t.Errorf("goal date = %s, want 2026-07-04", fake.lastGoal.Date)
	}
	if fake.lastGoal.Party != 4 {
		t.Errorf("goal party = %d, want 4", fake.lastGoal.Party)
	}
}

func TestRunQuestPlanCmd_MissingDateNoHint(t *testing.T) { //nolint:paralleltest // mutates openQuestServiceFn.
	fake := &fakeQuestPlanService{plan: samplePlan(t)}
	swapQuestPlanService(t, fake)

	out := &bytes.Buffer{}
	// Slug+city has no hints; -date and -party are both required.
	err := runQuestPlanCmd(
		context.Background(),
		[]string{
			"-time", "18:30..20:30",
			"-party", "2",
			"-account", "op@example.com",
			"-user", "usr_op01",
			"astoria-dc+washington-dc",
		},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err == nil {
		t.Fatal("expected error on missing -date")
	}
	if !strings.Contains(err.Error(), "-date is required") {
		t.Errorf("err = %v, want '-date is required' message", err)
	}
	if fake.calls != 0 {
		t.Errorf("PlanQuest called %d times despite flag error; should be 0", fake.calls)
	}
}

func TestRunQuestPlanCmd_MissingPartyNoHint(t *testing.T) { //nolint:paralleltest // mutates openQuestServiceFn.
	fake := &fakeQuestPlanService{plan: samplePlan(t)}
	swapQuestPlanService(t, fake)

	out := &bytes.Buffer{}
	err := runQuestPlanCmd(
		context.Background(),
		[]string{
			"-time", "18:30..20:30",
			"-date", "2026-06-09",
			"-account", "op@example.com",
			"-user", "usr_op01",
			"astoria-dc+washington-dc",
		},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err == nil {
		t.Fatal("expected error on missing -party")
	}
	if !strings.Contains(err.Error(), "-party is required") {
		t.Errorf("err = %v, want '-party is required' message", err)
	}
}

func TestRunQuestPlanCmd_MissingRequiredFlags(t *testing.T) { //nolint:paralleltest // mutates openQuestServiceFn.
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing -time",
			args: []string{
				"-account", "op@example.com",
				"-user", "usr_op01",
				"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
			},
			want: "-time HH:MM..HH:MM is required",
		},
		{
			name: "missing -account",
			args: []string{
				"-time", "18:30..20:30",
				"-user", "usr_op01",
				"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
			},
			want: "-account is required",
		},
		{
			name: "missing -user",
			args: []string{
				"-time", "18:30..20:30",
				"-account", "op@example.com",
				"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
			},
			want: "-user is required",
		},
		{
			name: "missing venue arg",
			args: []string{
				"-time", "18:30..20:30",
				"-account", "op@example.com",
				"-user", "usr_op01",
			},
			want: "missing venue",
		},
	}
	for _, tc := range cases { //nolint:paralleltest // subtests share the package-level openQuestServiceFn seam.
		t.Run(tc.name, func(t *testing.T) {
			swapQuestPlanService(t, &fakeQuestPlanService{plan: samplePlan(t)})
			out := &bytes.Buffer{}
			err := runQuestPlanCmd(
				context.Background(),
				tc.args,
				strings.NewReader(""),
				out,
				clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
			)
			if err == nil {
				t.Fatalf("expected error containing %q; got nil\nout=%s", tc.want, out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRunQuestPlanCmd_InvalidTimeWindow(t *testing.T) { //nolint:paralleltest // mutates openQuestServiceFn.
	swapQuestPlanService(t, &fakeQuestPlanService{plan: samplePlan(t)})
	out := &bytes.Buffer{}
	err := runQuestPlanCmd(
		context.Background(),
		[]string{
			"-time", "not-a-window",
			"-account", "op@example.com",
			"-user", "usr_op01",
			"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
		},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err == nil {
		t.Fatal("expected error on malformed -time")
	}
}

func TestRunQuestPlanCmd_InvalidPriority(t *testing.T) { //nolint:paralleltest // mutates openQuestServiceFn.
	swapQuestPlanService(t, &fakeQuestPlanService{plan: samplePlan(t)})
	out := &bytes.Buffer{}
	err := runQuestPlanCmd(
		context.Background(),
		[]string{
			"-time", "18:30..20:30",
			"-priority", "sideways",
			"-account", "op@example.com",
			"-user", "usr_op01",
			"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
		},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err == nil {
		t.Fatal("expected error on invalid -priority")
	}
}

func TestRunQuestPlanCmd_FormatJSON(t *testing.T) { //nolint:paralleltest // mutates openQuestServiceFn.
	fake := &fakeQuestPlanService{plan: samplePlan(t)}
	swapQuestPlanService(t, fake)

	out := &bytes.Buffer{}
	err := runQuestPlanCmd(
		context.Background(),
		[]string{
			"-format=json",
			"-time", "18:30..20:30",
			"-account", "op@example.com",
			"-user", "usr_op01",
			"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
		},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err != nil {
		t.Fatalf("runQuestPlanCmd: %v\n%s", err, out.String())
	}
	var decoded map[string]any
	if jerr := json.Unmarshal(out.Bytes(), &decoded); jerr != nil {
		t.Fatalf("json.Unmarshal: %v\nbody=%s", jerr, out.String())
	}
	if decoded["hash"] != "sha256:a8f2bbbbccccddddeeeeffff00001111" {
		t.Errorf("hash = %v, want full sha256:a8f2…", decoded["hash"])
	}
	// Goal nested object should be present.
	if _, ok := decoded["goal"].(map[string]any); !ok {
		t.Errorf("decoded.goal not a map: %T", decoded["goal"])
	}
}

func TestRunQuestPlanCmd_InvalidFormat(t *testing.T) { //nolint:paralleltest // mutates openQuestServiceFn.
	swapQuestPlanService(t, &fakeQuestPlanService{plan: samplePlan(t)})
	out := &bytes.Buffer{}
	err := runQuestPlanCmd(
		context.Background(),
		[]string{
			"-format=xml",
			"-time", "18:30..20:30",
			"-account", "op@example.com",
			"-user", "usr_op01",
			"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
		},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err == nil {
		t.Fatal("expected error on invalid -format")
	}
}

func TestRunQuestPlanCmd_ServiceErrorPropagates(t *testing.T) { //nolint:paralleltest // mutates openQuestServiceFn.
	// Mimic the planner refusing a past date — the Service wraps
	// ErrInvalidArgument and the CLI surfaces it unchanged (Plan
	// failed: <wrapped>).
	fake := &fakeQuestPlanService{err: service.ErrInvalidArgument}
	swapQuestPlanService(t, fake)

	out := &bytes.Buffer{}
	err := runQuestPlanCmd(
		context.Background(),
		[]string{
			"-time", "18:30..20:30",
			"-account", "op@example.com",
			"-user", "usr_op01",
			"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
		},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err == nil {
		t.Fatal("expected error when service returns one")
	}
	if !errors.Is(err, service.ErrInvalidArgument) {
		t.Errorf("err = %v, want wraps ErrInvalidArgument", err)
	}
	if !strings.Contains(out.String(), "Plan failed:") {
		t.Errorf("output missing 'Plan failed:' prefix:\n%s", out.String())
	}
}

func TestRunQuestPlanCmd_TablesFlagPopulatesConstraints(t *testing.T) { //nolint:paralleltest // mutates openQuestServiceFn.
	fake := &fakeQuestPlanService{plan: samplePlan(t)}
	swapQuestPlanService(t, fake)

	out := &bytes.Buffer{}
	err := runQuestPlanCmd(
		context.Background(),
		[]string{
			"-time", "18:30..20:30",
			"-tables", "indoor,patio",
			"-account", "op@example.com",
			"-user", "usr_op01",
			"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
		},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err != nil {
		t.Fatalf("runQuestPlanCmd: %v\n%s", err, out.String())
	}
	gotTables := fake.lastGoal.Constraints.TableTypes
	wantTables := []string{"indoor", "patio"}
	if len(gotTables) != len(wantTables) {
		t.Fatalf("tables = %v, want %v", gotTables, wantTables)
	}
	for i, w := range wantTables {
		if gotTables[i] != w {
			t.Errorf("tables[%d] = %q, want %q", i, gotTables[i], w)
		}
	}
	if fake.lastGoal.Constraints.AnyTable {
		t.Error("AnyTable should be false when -tables is supplied")
	}
}

func TestRunQuestPlanCmd_NoTablesFlagDefaultsAnyTable(t *testing.T) { //nolint:paralleltest // mutates openQuestServiceFn.
	fake := &fakeQuestPlanService{plan: samplePlan(t)}
	swapQuestPlanService(t, fake)

	out := &bytes.Buffer{}
	err := runQuestPlanCmd(
		context.Background(),
		[]string{
			"-time", "18:30..20:30",
			"-account", "op@example.com",
			"-user", "usr_op01",
			"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
		},
		strings.NewReader(""),
		out,
		clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)),
	)
	if err != nil {
		t.Fatalf("runQuestPlanCmd: %v\n%s", err, out.String())
	}
	if !fake.lastGoal.Constraints.AnyTable {
		t.Error("AnyTable should be true when -tables is empty")
	}
}
