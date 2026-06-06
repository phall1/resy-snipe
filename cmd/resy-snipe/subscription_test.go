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

// --- test fakes -------------------------------------------------------------

type fakeSubscriptionService struct {
	createUser       domain.UserID
	createGoal       domain.Goal
	createCompromise *domain.CompromisePolicy
	createExpiresAt  *time.Time
	createID         domain.SubscriptionID
	createErr        error

	listUser   domain.UserID
	listFilter service.SubscriptionFilter
	listRows   []service.Subscription
	listErr    error

	getUser domain.UserID
	getID   domain.SubscriptionID
	getSub  service.Subscription
	getErr  error

	cancelUser   domain.UserID
	cancelID     domain.SubscriptionID
	cancelErr    error
	cancelCalled bool

	resumeUser   domain.UserID
	resumeID     domain.SubscriptionID
	resumeErr    error
	resumeCalled bool
}

var _ service.Service = (*fakeSubscriptionService)(nil)

func (f *fakeSubscriptionService) ResolveVenue(_ context.Context, _ domain.UserID, _ domain.VenueQuery) (domain.Venue, error) {
	return domain.Venue{}, service.ErrNotImplemented
}
func (f *fakeSubscriptionService) PlanQuest(_ context.Context, _ domain.UserID, _ domain.Goal) (domain.Plan, error) {
	return domain.Plan{}, service.ErrNotImplemented
}
func (f *fakeSubscriptionService) CreateQuest(_ context.Context, _ domain.UserID, _ domain.Goal, _ service.CreateOpts) (domain.QuestID, error) {
	return "", service.ErrNotImplemented
}
func (f *fakeSubscriptionService) GetQuest(_ context.Context, _ domain.UserID, _ domain.QuestID) (service.QuestState, error) {
	return service.QuestState{}, service.ErrNotImplemented
}
func (f *fakeSubscriptionService) ListQuests(_ context.Context, _ domain.UserID, _ service.ListFilter) ([]service.QuestSummary, error) {
	return nil, service.ErrNotImplemented
}
func (f *fakeSubscriptionService) CancelQuest(_ context.Context, _ domain.UserID, _ domain.QuestID, _ service.CancelOpts) error {
	return service.ErrNotImplemented
}
func (f *fakeSubscriptionService) SubscribeQuest(_ context.Context, _ domain.UserID, _ domain.QuestID, _ func(domain.Event)) error {
	return service.ErrNotImplemented
}
func (f *fakeSubscriptionService) Login(_ context.Context, _ domain.UserID, _, _ string) (domain.AccountID, error) {
	return "", service.ErrNotImplemented
}
func (f *fakeSubscriptionService) ListAccounts(_ context.Context, _ domain.UserID) ([]service.Account, error) {
	return nil, service.ErrNotImplemented
}
func (f *fakeSubscriptionService) InviteUser(_ context.Context, _ domain.UserID, _, _ string) (service.Invite, error) {
	return service.Invite{}, service.ErrNotImplemented
}
func (f *fakeSubscriptionService) AcceptInvite(_ context.Context, _, _, _ string) (domain.UserID, service.BearerToken, error) {
	return "", service.BearerToken{}, service.ErrNotImplemented
}
func (f *fakeSubscriptionService) IssueToken(_ context.Context, _ domain.UserID, _, _ string) (service.BearerToken, error) {
	return service.BearerToken{}, service.ErrNotImplemented
}
func (f *fakeSubscriptionService) RevokeToken(_ context.Context, _ domain.UserID, _ string) error {
	return service.ErrNotImplemented
}
func (f *fakeSubscriptionService) ListTokens(_ context.Context, _ domain.UserID) ([]service.Token, error) {
	return nil, service.ErrNotImplemented
}
func (f *fakeSubscriptionService) ListUsers(_ context.Context, _ domain.UserID) ([]service.User, error) {
	return nil, service.ErrNotImplemented
}
func (f *fakeSubscriptionService) CreateSubscription(_ context.Context, userID domain.UserID, goal domain.Goal, compromise *domain.CompromisePolicy, expiresAt *time.Time) (domain.SubscriptionID, error) {
	f.createUser = userID
	f.createGoal = goal
	f.createCompromise = compromise
	f.createExpiresAt = expiresAt
	return f.createID, f.createErr
}
func (f *fakeSubscriptionService) GetSubscription(_ context.Context, userID domain.UserID, subID domain.SubscriptionID) (service.Subscription, error) {
	f.getUser = userID
	f.getID = subID
	return f.getSub, f.getErr
}
func (f *fakeSubscriptionService) ListSubscriptions(_ context.Context, userID domain.UserID, filter service.SubscriptionFilter) ([]service.Subscription, error) {
	f.listUser = userID
	f.listFilter = filter
	return f.listRows, f.listErr
}
func (f *fakeSubscriptionService) CancelSubscription(_ context.Context, userID domain.UserID, subID domain.SubscriptionID) error {
	f.cancelCalled = true
	f.cancelUser = userID
	f.cancelID = subID
	return f.cancelErr
}
func (f *fakeSubscriptionService) UpdateSubscriptionNextPoll(_ context.Context, _ domain.UserID, _ domain.SubscriptionID, _ time.Time) error {
	return service.ErrNotImplemented
}
func (f *fakeSubscriptionService) PauseSubscription(_ context.Context, _ domain.UserID, _ domain.SubscriptionID) error {
	return service.ErrNotImplemented
}
func (f *fakeSubscriptionService) FulfillSubscription(_ context.Context, _ domain.UserID, _ domain.SubscriptionID, _ domain.QuestID) error {
	return service.ErrNotImplemented
}
func (f *fakeSubscriptionService) ExpireSubscription(_ context.Context, _ domain.UserID, _ domain.SubscriptionID) error {
	return service.ErrNotImplemented
}
func (f *fakeSubscriptionService) ResumeSubscription(_ context.Context, userID domain.UserID, subID domain.SubscriptionID) error {
	f.resumeCalled = true
	f.resumeUser = userID
	f.resumeID = subID
	return f.resumeErr
}

// swapSubscriptionService installs a fake service.Service for the duration of t.
func swapSubscriptionService(t *testing.T, fake *fakeSubscriptionService) {
	t.Helper()
	prevSvc := newSubscriptionServiceFn
	prevUser := resolveDefaultUserFn
	newSubscriptionServiceFn = func(_ context.Context, _ *slog.Logger, _ clock.Clock) (service.Service, func() error, error) {
		return fake, func() error { return nil }, nil
	}
	resolveDefaultUserFn = func(_ context.Context, _ clock.Clock) (domain.UserID, error) {
		return testDefaultUser, nil
	}
	t.Cleanup(func() {
		newSubscriptionServiceFn = prevSvc
		resolveDefaultUserFn = prevUser
	})
}

// --- TestSubscriptionCreateCmd ----------------------------------------------

func TestSubscriptionCreateCmd(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeSubscriptionService{
		createID: "sub_abc123",
	}
	swapSubscriptionService(t, fake)

	args := []string{
		"-venue", "Carbone",
		"-date", "2026-06-15",
		"-party", "4",
		"-time-start", "18:00",
		"-time-end", "21:00",
	}
	var out bytes.Buffer
	err := runSubscriptionCreateCmd(context.Background(), args, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runSubscriptionCreateCmd: %v", err)
	}

	if fake.createUser != testDefaultUser {
		t.Errorf("createUser = %q, want %q", fake.createUser, testDefaultUser)
	}
	vn, ok := fake.createGoal.VenueQuery.(domain.VenueQueryName)
	if !ok || vn.Name != "Carbone" {
		t.Errorf("createGoal.VenueQuery = %v, want Carbone", fake.createGoal.VenueQuery)
	}
	if fake.createGoal.Date != (domain.Date{Year: 2026, Month: time.June, Day: 15}) {
		t.Errorf("createGoal.Date = %v, want 2026-06-15", fake.createGoal.Date)
	}
	if fake.createGoal.Party != 4 {
		t.Errorf("createGoal.Party = %d, want 4", fake.createGoal.Party)
	}
	if fake.createGoal.TimePrefs.Start != (domain.WallTime{Hour: 18}) {
		t.Errorf("createGoal.TimePrefs.Start = %v, want 18:00", fake.createGoal.TimePrefs.Start)
	}
	if fake.createGoal.TimePrefs.End != (domain.WallTime{Hour: 21}) {
		t.Errorf("createGoal.TimePrefs.End = %v, want 21:00", fake.createGoal.TimePrefs.End)
	}
	if fake.createGoal.TimePrefs.Priority != domain.PriorityNone {
		t.Errorf("createGoal.TimePrefs.Priority = %v, want PriorityNone", fake.createGoal.TimePrefs.Priority)
	}
	if !fake.createGoal.Constraints.AnyTable {
		t.Errorf("createGoal.Constraints.AnyTable = %v, want true", fake.createGoal.Constraints.AnyTable)
	}
	if fake.createCompromise != nil {
		t.Errorf("createCompromise = %v, want nil", fake.createCompromise)
	}
	if fake.createExpiresAt != nil {
		t.Errorf("createExpiresAt = %v, want nil", fake.createExpiresAt)
	}

	got := out.String()
	if !strings.Contains(got, "sub_abc123") {
		t.Errorf("output missing subscription ID: %q", got)
	}
	if !strings.Contains(got, "created subscription") {
		t.Errorf("output missing confirmation: %q", got)
	}
}

// --- TestSubscriptionListCmd ------------------------------------------------

func TestSubscriptionListCmd(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeSubscriptionService{
		listRows: []service.Subscription{
			{
				ID:     "sub_1",
				UserID: testDefaultUser,
				Goal: domain.Goal{
					VenueQuery: domain.VenueQueryName{Name: "Carbone"},
					Date:       domain.Date{Year: 2026, Month: time.June, Day: 15},
					Party:      2,
					TimePrefs:  domain.TimeWindow{Start: domain.WallTime{Hour: 18}, End: domain.WallTime{Hour: 21}},
				},
				Status:       domain.SubscriptionActive,
				CreatedAt:    time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
				PollInterval: 5 * time.Minute,
			},
			{
				ID:     "sub_2",
				UserID: testDefaultUser,
				Goal: domain.Goal{
					VenueQuery: domain.VenueQueryName{Name: "Don Angie"},
					Date:       domain.Date{Year: 2026, Month: time.June, Day: 20},
					Party:      4,
					TimePrefs:  domain.TimeWindow{Start: domain.WallTime{Hour: 19}, End: domain.WallTime{Hour: 22}},
				},
				Status:       domain.SubscriptionPaused,
				CreatedAt:    time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC),
				PollInterval: 10 * time.Minute,
			},
		},
	}
	swapSubscriptionService(t, fake)

	// Table format.
	var out bytes.Buffer
	err := runSubscriptionListCmd(context.Background(), []string{}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runSubscriptionListCmd: %v", err)
	}
	got := out.String()
	for _, want := range []string{"sub_1", "sub_2", "active", "paused", "Carbone", "Don Angie"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q: %q", want, got)
		}
	}
	if fake.listUser != testDefaultUser {
		t.Errorf("listUser = %q, want %q", fake.listUser, testDefaultUser)
	}
	if fake.listFilter.Limit != defaultQuestListLimit {
		t.Errorf("listFilter.Limit = %d, want %d", fake.listFilter.Limit, defaultQuestListLimit)
	}

	// JSON format.
	out.Reset()
	err = runSubscriptionListCmd(context.Background(), []string{"-format=json"}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runSubscriptionListCmd json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0]["id"] != "sub_1" {
		t.Errorf("row[0].id = %v, want sub_1", rows[0]["id"])
	}
	if rows[1]["id"] != "sub_2" {
		t.Errorf("row[1].id = %v, want sub_2", rows[1]["id"])
	}
}

// --- TestSubscriptionGetCmd -------------------------------------------------

func TestSubscriptionGetCmd(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeSubscriptionService{
		getSub: service.Subscription{
			ID:     "sub_get1",
			UserID: testDefaultUser,
			Goal: domain.Goal{
				VenueQuery: domain.VenueQueryName{Name: "Lilia"},
				Date:       domain.Date{Year: 2026, Month: time.July, Day: 4},
				Party:      3,
				TimePrefs:  domain.TimeWindow{Start: domain.WallTime{Hour: 17}, End: domain.WallTime{Hour: 20}},
			},
			Status:       domain.SubscriptionActive,
			CreatedAt:    time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
			PollInterval: 5 * time.Minute,
		},
	}
	swapSubscriptionService(t, fake)

	var out bytes.Buffer
	err := runSubscriptionGetCmd(context.Background(), []string{"sub_get1"}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runSubscriptionGetCmd: %v", err)
	}
	got := out.String()
	for _, want := range []string{"sub_get1", "active", "Lilia", "2026-07-04", "3", "17:00", "20:00"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q: %q", want, got)
		}
	}
	if fake.getID != "sub_get1" {
		t.Errorf("getID = %q, want sub_get1", fake.getID)
	}
	if fake.getUser != testDefaultUser {
		t.Errorf("getUser = %q, want %q", fake.getUser, testDefaultUser)
	}
}

// --- TestSubscriptionCancelCmd ----------------------------------------------

func TestSubscriptionCancelCmd(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeSubscriptionService{}
	swapSubscriptionService(t, fake)

	var out bytes.Buffer
	err := runSubscriptionCancelCmd(context.Background(), []string{"sub_cancel1"}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runSubscriptionCancelCmd: %v", err)
	}
	if !fake.cancelCalled {
		t.Fatal("expected CancelSubscription to be called")
	}
	if fake.cancelID != "sub_cancel1" {
		t.Errorf("cancelID = %q, want sub_cancel1", fake.cancelID)
	}
	if fake.cancelUser != testDefaultUser {
		t.Errorf("cancelUser = %q, want %q", fake.cancelUser, testDefaultUser)
	}
	if !strings.Contains(out.String(), "canceled sub_cancel1") {
		t.Errorf("output missing confirmation: %q", out.String())
	}
}

func TestSubscriptionCancelCmd_NotFound(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeSubscriptionService{cancelErr: service.ErrNotFound}
	swapSubscriptionService(t, fake)

	var out bytes.Buffer
	err := runSubscriptionCancelCmd(context.Background(), []string{"sub_missing"}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("error should wrap ErrNotFound: %v", err)
	}
	if !strings.Contains(out.String(), "subscription not found") {
		t.Errorf("output should say 'subscription not found': %q", out.String())
	}
}

// --- TestSubscriptionResumeCmd ----------------------------------------------

func TestSubscriptionResumeCmd(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeSubscriptionService{}
	swapSubscriptionService(t, fake)

	var out bytes.Buffer
	err := runSubscriptionResumeCmd(context.Background(), []string{"sub_resume1"}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runSubscriptionResumeCmd: %v", err)
	}
	if !fake.resumeCalled {
		t.Fatal("expected ResumeSubscription to be called")
	}
	if fake.resumeID != "sub_resume1" {
		t.Errorf("resumeID = %q, want sub_resume1", fake.resumeID)
	}
	if fake.resumeUser != testDefaultUser {
		t.Errorf("resumeUser = %q, want %q", fake.resumeUser, testDefaultUser)
	}
	if !strings.Contains(out.String(), "resumed sub_resume1") {
		t.Errorf("output missing confirmation: %q", out.String())
	}
}

func TestSubscriptionResumeCmd_NotFound(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeSubscriptionService{resumeErr: service.ErrNotFound}
	swapSubscriptionService(t, fake)

	var out bytes.Buffer
	err := runSubscriptionResumeCmd(context.Background(), []string{"sub_missing"}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("error should wrap ErrNotFound: %v", err)
	}
	if !strings.Contains(out.String(), "subscription not found") {
		t.Errorf("output should say 'subscription not found': %q", out.String())
	}
}
