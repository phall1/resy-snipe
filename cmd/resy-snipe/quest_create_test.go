package main

import (
	"bytes"
	"context"
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

// fakeQuestCreateService satisfies questCreateService for the CLI tests.
// It records the last (userID, goal, opts) and returns either a canned
// QuestID or a canned error from CreateQuest. Plan failures are also
// possible but most tests bypass them by supplying a valid plan.
type fakeQuestCreateService struct {
	// PlanQuest plumbing.
	planCalls int
	planUID   domain.UserID
	planGoal  domain.Goal
	plan      domain.Plan
	planErr   error
	// CreateQuest plumbing.
	createCalls int
	createUID   domain.UserID
	createGoal  domain.Goal
	createOpts  service.CreateOpts
	createID    domain.QuestID
	createErr   error
}

func (f *fakeQuestCreateService) PlanQuest(
	_ context.Context, uid domain.UserID, goal domain.Goal,
) (domain.Plan, error) {
	f.planCalls++
	f.planUID = uid
	f.planGoal = goal
	if f.planErr != nil {
		return domain.Plan{}, f.planErr
	}
	return f.plan, nil
}

func (f *fakeQuestCreateService) CreateQuest(
	_ context.Context, uid domain.UserID, goal domain.Goal, opts service.CreateOpts,
) (domain.QuestID, error) {
	f.createCalls++
	f.createUID = uid
	f.createGoal = goal
	f.createOpts = opts
	if f.createErr != nil {
		return "", f.createErr
	}
	return f.createID, nil
}

// swapQuestCreateService installs the fake for the duration of the test
// and restores the original seam on cleanup. Mirrors swapQuestPlanService.
func swapQuestCreateService(t *testing.T, svc questCreateService) {
	t.Helper()
	prev := openQuestCreateServiceFn
	openQuestCreateServiceFn = func(_ context.Context, _ *slog.Logger, _ clock.Clock) (questCreateService, func() error, error) {
		return svc, func() error { return nil }, nil
	}
	t.Cleanup(func() { openQuestCreateServiceFn = prev })
}

// createSamplePlan returns a Plan plausible enough to render and to use
// as a hash-pin in CreateOpts. Mirrors samplePlan in quest_plan_test.go
// but is duplicated to keep the test files independent (the harness
// builds each handler against its own narrowed service interface).
func createSamplePlan(t *testing.T) domain.Plan {
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
		PlanHashVersion: domain.PlanHashVersionV1,
		ComputedAt:      drop.Add(-time.Hour),
	}
}

// baseArgs returns the minimal flag set every happy-path test uses. The
// URL carries date=/seats= hints so -date/-party are not required.
func baseCreateArgs() []string {
	return []string{
		"-time", "18:30..20:30",
		"-account", "op@example.com",
		"-user", "usr_op01",
		"https://resy.com/cities/washington-dc/venues/astoria-dc?date=2026-06-09&seats=2",
	}
}

// fixedClock returns a fixed clock for deterministic days-from-now math.
func fixedClock() clock.Clock {
	return clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
}

// ---- happy-path tests ------------------------------------------------------

func TestRunQuestCreateCmd_HappyPathWithYes(t *testing.T) { //nolint:paralleltest // mutates openQuestCreateServiceFn.
	plan := createSamplePlan(t)
	fake := &fakeQuestCreateService{plan: plan, createID: domain.QuestID("q_abcdef01")}
	swapQuestCreateService(t, fake)

	out := &bytes.Buffer{}
	args := append([]string{"-yes"}, baseCreateArgs()...)
	err := runQuestCreateCmd(
		context.Background(),
		args,
		strings.NewReader(""),
		out,
		fixedClock(),
	)
	if err != nil {
		t.Fatalf("runQuestCreateCmd: %v\n%s", err, out.String())
	}
	if fake.planCalls != 1 {
		t.Errorf("PlanQuest calls = %d, want 1", fake.planCalls)
	}
	if fake.createCalls != 1 {
		t.Fatalf("CreateQuest calls = %d, want 1", fake.createCalls)
	}
	if fake.createUID != domain.UserID("usr_op01") {
		t.Errorf("createUID = %q, want usr_op01", fake.createUID)
	}
	if fake.createOpts.PlanHash == nil {
		t.Fatal("CreateOpts.PlanHash should be non-nil")
	}
	if *fake.createOpts.PlanHash != plan.Hash {
		t.Errorf("PlanHash = %q, want %q", *fake.createOpts.PlanHash, plan.Hash)
	}
	if fake.createOpts.IdempotencyKey != nil {
		t.Errorf("IdempotencyKey = %v, want nil", *fake.createOpts.IdempotencyKey)
	}
	body := out.String()
	if !strings.Contains(body, "created quest q_abcdef01") {
		t.Errorf("output missing 'created quest q_abcdef01':\n%s", body)
	}
	if !strings.Contains(body, "scheduled at") {
		t.Errorf("output missing 'scheduled at':\n%s", body)
	}
	// The plan-preview block must precede the confirmation line.
	if !strings.Contains(body, "PLAN") {
		t.Errorf("output missing plan-preview block:\n%s", body)
	}
}

func TestRunQuestCreateCmd_StdinConfirmYes(t *testing.T) { //nolint:paralleltest // mutates openQuestCreateServiceFn.
	plan := createSamplePlan(t)
	fake := &fakeQuestCreateService{plan: plan, createID: domain.QuestID("q_11223344")}
	swapQuestCreateService(t, fake)

	out := &bytes.Buffer{}
	err := runQuestCreateCmd(
		context.Background(),
		baseCreateArgs(),
		strings.NewReader("y\n"),
		out,
		fixedClock(),
	)
	if err != nil {
		t.Fatalf("runQuestCreateCmd: %v\n%s", err, out.String())
	}
	if fake.createCalls != 1 {
		t.Fatalf("CreateQuest calls = %d, want 1 after 'y' confirm", fake.createCalls)
	}
	if !strings.Contains(out.String(), "Create this quest? [y/N]") {
		t.Errorf("expected confirmation prompt in output:\n%s", out.String())
	}
}

func TestRunQuestCreateCmd_StdinConfirmYesUppercase(t *testing.T) { //nolint:paralleltest // mutates openQuestCreateServiceFn.
	plan := createSamplePlan(t)
	fake := &fakeQuestCreateService{plan: plan, createID: domain.QuestID("q_aa00bb11")}
	swapQuestCreateService(t, fake)

	out := &bytes.Buffer{}
	err := runQuestCreateCmd(
		context.Background(),
		baseCreateArgs(),
		strings.NewReader("YES\n"),
		out,
		fixedClock(),
	)
	if err != nil {
		t.Fatalf("runQuestCreateCmd: %v\n%s", err, out.String())
	}
	if fake.createCalls != 1 {
		t.Fatalf("CreateQuest calls = %d, want 1 after 'YES' confirm", fake.createCalls)
	}
}

func TestRunQuestCreateCmd_StdinConfirmNo(t *testing.T) { //nolint:paralleltest // mutates openQuestCreateServiceFn.
	plan := createSamplePlan(t)
	fake := &fakeQuestCreateService{plan: plan, createID: domain.QuestID("q_should_not_appear")}
	swapQuestCreateService(t, fake)

	out := &bytes.Buffer{}
	err := runQuestCreateCmd(
		context.Background(),
		baseCreateArgs(),
		strings.NewReader("n\n"),
		out,
		fixedClock(),
	)
	if err != nil {
		t.Fatalf("runQuestCreateCmd: unexpected error on user-cancel: %v\n%s", err, out.String())
	}
	if fake.createCalls != 0 {
		t.Errorf("CreateQuest called %d times after 'n'; should be 0", fake.createCalls)
	}
	body := out.String()
	if !strings.Contains(body, "canceled, no quest created") {
		t.Errorf("output missing 'canceled, no quest created':\n%s", body)
	}
	if strings.Contains(body, "created quest") {
		t.Errorf("output should not contain 'created quest' on cancel:\n%s", body)
	}
}

func TestRunQuestCreateCmd_StdinConfirmEmpty(t *testing.T) { //nolint:paralleltest // mutates openQuestCreateServiceFn.
	// Empty answer (just press enter) → default is "no".
	plan := createSamplePlan(t)
	fake := &fakeQuestCreateService{plan: plan}
	swapQuestCreateService(t, fake)

	out := &bytes.Buffer{}
	err := runQuestCreateCmd(
		context.Background(),
		baseCreateArgs(),
		strings.NewReader("\n"),
		out,
		fixedClock(),
	)
	if err != nil {
		t.Fatalf("runQuestCreateCmd: %v", err)
	}
	if fake.createCalls != 0 {
		t.Errorf("CreateQuest called %d times on blank confirm; should be 0", fake.createCalls)
	}
	if !strings.Contains(out.String(), "canceled") {
		t.Errorf("output missing 'canceled':\n%s", out.String())
	}
}

// ---- error-path tests ------------------------------------------------------

func TestRunQuestCreateCmd_InvalidPlanHash(t *testing.T) { //nolint:paralleltest // mutates openQuestCreateServiceFn.
	plan := createSamplePlan(t)
	fake := &fakeQuestCreateService{
		plan:      plan,
		createErr: service.ErrInvalidPlanHash,
	}
	swapQuestCreateService(t, fake)

	out := &bytes.Buffer{}
	args := append([]string{"-yes"}, baseCreateArgs()...)
	err := runQuestCreateCmd(
		context.Background(),
		args,
		strings.NewReader(""),
		out,
		fixedClock(),
	)
	if err == nil {
		t.Fatal("expected error when CreateQuest returns ErrInvalidPlanHash")
	}
	if !errors.Is(err, service.ErrInvalidPlanHash) {
		t.Errorf("err = %v, want wraps ErrInvalidPlanHash", err)
	}
	if !strings.Contains(out.String(), "the plan changed between preview and create") {
		t.Errorf("output missing plan-changed hint:\n%s", out.String())
	}
}

func TestRunQuestCreateCmd_IdempotencyKeyForwarded(t *testing.T) { //nolint:paralleltest // mutates openQuestCreateServiceFn.
	plan := createSamplePlan(t)
	fake := &fakeQuestCreateService{plan: plan, createID: domain.QuestID("q_idem01")}
	swapQuestCreateService(t, fake)

	out := &bytes.Buffer{}
	args := append([]string{"-yes", "-idempotency-key", "foo"}, baseCreateArgs()...)
	err := runQuestCreateCmd(
		context.Background(),
		args,
		strings.NewReader(""),
		out,
		fixedClock(),
	)
	if err != nil {
		t.Fatalf("runQuestCreateCmd: %v", err)
	}
	if fake.createOpts.IdempotencyKey == nil {
		t.Fatal("CreateOpts.IdempotencyKey should be non-nil when -idempotency-key is set")
	}
	if *fake.createOpts.IdempotencyKey != "foo" {
		t.Errorf("IdempotencyKey = %q, want %q", *fake.createOpts.IdempotencyKey, "foo")
	}
}

func TestRunQuestCreateCmd_ConflictSurfaced(t *testing.T) { //nolint:paralleltest // mutates openQuestCreateServiceFn.
	plan := createSamplePlan(t)
	fake := &fakeQuestCreateService{
		plan:      plan,
		createErr: service.ErrConflict,
	}
	swapQuestCreateService(t, fake)

	out := &bytes.Buffer{}
	args := append([]string{"-yes", "-idempotency-key", "foo"}, baseCreateArgs()...)
	err := runQuestCreateCmd(
		context.Background(),
		args,
		strings.NewReader(""),
		out,
		fixedClock(),
	)
	if err == nil {
		t.Fatal("expected error when CreateQuest returns ErrConflict")
	}
	if !errors.Is(err, service.ErrConflict) {
		t.Errorf("err = %v, want wraps ErrConflict", err)
	}
	if !strings.Contains(out.String(), "idempotency conflict") {
		t.Errorf("output missing 'idempotency conflict' hint:\n%s", out.String())
	}
}

func TestRunQuestCreateCmd_PlanFailure(t *testing.T) { //nolint:paralleltest // mutates openQuestCreateServiceFn.
	fake := &fakeQuestCreateService{planErr: service.ErrInvalidArgument}
	swapQuestCreateService(t, fake)

	out := &bytes.Buffer{}
	args := append([]string{"-yes"}, baseCreateArgs()...)
	err := runQuestCreateCmd(
		context.Background(),
		args,
		strings.NewReader(""),
		out,
		fixedClock(),
	)
	if err == nil {
		t.Fatal("expected error when PlanQuest fails")
	}
	if !errors.Is(err, service.ErrInvalidArgument) {
		t.Errorf("err = %v, want wraps ErrInvalidArgument", err)
	}
	if fake.createCalls != 0 {
		t.Errorf("CreateQuest called %d times after plan failure; should be 0", fake.createCalls)
	}
	if !strings.Contains(out.String(), "Plan failed:") {
		t.Errorf("output missing 'Plan failed:' prefix:\n%s", out.String())
	}
}

func TestRunQuestCreateCmd_MissingDateNoHint(t *testing.T) { //nolint:paralleltest // mutates openQuestCreateServiceFn.
	plan := createSamplePlan(t)
	fake := &fakeQuestCreateService{plan: plan}
	swapQuestCreateService(t, fake)

	out := &bytes.Buffer{}
	// slug+city carries no hints; -date is required.
	err := runQuestCreateCmd(
		context.Background(),
		[]string{
			"-yes",
			"-time", "18:30..20:30",
			"-party", "2",
			"-account", "op@example.com",
			"-user", "usr_op01",
			"astoria-dc+washington-dc",
		},
		strings.NewReader(""),
		out,
		fixedClock(),
	)
	if err == nil {
		t.Fatal("expected error when -date is missing without URL hint")
	}
	if !strings.Contains(err.Error(), "-date is required") {
		t.Errorf("err = %v, want '-date is required'", err)
	}
	if fake.planCalls != 0 {
		t.Errorf("PlanQuest called %d times despite flag error; should be 0", fake.planCalls)
	}
	if fake.createCalls != 0 {
		t.Errorf("CreateQuest called %d times despite flag error; should be 0", fake.createCalls)
	}
}

func TestRunQuestCreateCmd_MissingVenueArg(t *testing.T) { //nolint:paralleltest // mutates openQuestCreateServiceFn.
	plan := createSamplePlan(t)
	fake := &fakeQuestCreateService{plan: plan}
	swapQuestCreateService(t, fake)

	out := &bytes.Buffer{}
	err := runQuestCreateCmd(
		context.Background(),
		[]string{
			"-yes",
			"-time", "18:30..20:30",
			"-account", "op@example.com",
			"-user", "usr_op01",
		},
		strings.NewReader(""),
		out,
		fixedClock(),
	)
	if err == nil {
		t.Fatal("expected error when positional venue arg is missing")
	}
	if !strings.Contains(err.Error(), "missing venue") {
		t.Errorf("err = %v, want 'missing venue'", err)
	}
}

func TestRunQuestCreateCmd_InvalidFormat(t *testing.T) { //nolint:paralleltest // mutates openQuestCreateServiceFn.
	plan := createSamplePlan(t)
	fake := &fakeQuestCreateService{plan: plan}
	swapQuestCreateService(t, fake)

	out := &bytes.Buffer{}
	args := append([]string{"-yes", "-format=xml"}, baseCreateArgs()...)
	err := runQuestCreateCmd(
		context.Background(),
		args,
		strings.NewReader(""),
		out,
		fixedClock(),
	)
	if err == nil {
		t.Fatal("expected error on -format=xml")
	}
}
