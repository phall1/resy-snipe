package service

import (
	"context"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
)

func TestSchedulerPollSubscriptionBooks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC))

	fake := &fakeSchedulerService{}
	sch := NewScheduler(fake, clk, nil)

	sub := Subscription{
		ID:           "sub_test",
		UserID:       "usr_test",
		Goal:         domain.Goal{Date: domain.NewDate(2026, 6, 12), Party: 2, AccountID: "acct_test"},
		Status:       domain.SubscriptionActive,
		PollInterval: 90 * time.Second,
		NextPollAt:   clk.Now(),
		CreatedAt:    clk.Now(),
	}

	fake.plan = domain.Plan{FireSchedule: []time.Time{time.Now()}}
	fake.questID = "q_1234"
	fake.questStatus = domain.StatusBooked

	if err := sch.PollSubscription(ctx, "usr_test", sub); err != nil {
		t.Fatalf("PollSubscription: %v", err)
	}

	if fake.createQuestCalled != 1 {
		t.Errorf("CreateQuest called %d times, want 1", fake.createQuestCalled)
	}
	if fake.subscribeCalled != 1 {
		t.Errorf("SubscribeQuest called %d times, want 1", fake.subscribeCalled)
	}
	if fake.fulfillCalled != 1 {
		t.Errorf("FulfillSubscription called %d times, want 1", fake.fulfillCalled)
	}
	if fake.updateNextPollCalled != 0 {
		t.Errorf("UpdateSubscriptionNextPoll called %d times, want 0", fake.updateNextPollCalled)
	}
}

func TestSchedulerPollSubscriptionNoSlots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC))

	fake := &fakeSchedulerService{}
	sch := NewScheduler(fake, clk, nil)

	sub := Subscription{
		ID:           "sub_test",
		UserID:       "usr_test",
		Goal:         domain.Goal{Date: domain.NewDate(2026, 6, 12), Party: 2, AccountID: "acct_test"},
		Status:       domain.SubscriptionActive,
		PollInterval: 90 * time.Second,
		NextPollAt:   clk.Now(),
		CreatedAt:    clk.Now(),
	}

	fake.plan = domain.Plan{FireSchedule: []time.Time{}}

	if err := sch.PollSubscription(ctx, "usr_test", sub); err != nil {
		t.Fatalf("PollSubscription: %v", err)
	}

	if fake.createQuestCalled != 0 {
		t.Errorf("CreateQuest called %d times, want 0", fake.createQuestCalled)
	}
	if fake.updateNextPollCalled != 1 {
		t.Errorf("UpdateSubscriptionNextPoll called %d times, want 1", fake.updateNextPollCalled)
	}
}

func TestSchedulerStartStopIdempotent(t *testing.T) {
	t.Parallel()
	fake := &fakeSchedulerService{}
	clk := clock.NewFake(time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC))
	sch := NewScheduler(fake, clk, nil)

	// Multiple starts should be safe
	sch.Start()
	sch.Start()
	sch.Start()

	// Multiple stops should be safe
	sch.Stop()
	sch.Stop()
	sch.Stop()

	// Restart after stop should be safe
	sch.Start()
	sch.Stop()
}

func TestSchedulerPollSubscriptionAuthExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC))

	fake := &fakeSchedulerService{}
	sch := NewScheduler(fake, clk, nil)

	sub := Subscription{
		ID:           "sub_test",
		UserID:       "usr_test",
		Goal:         domain.Goal{Date: domain.NewDate(2026, 6, 12), Party: 2, AccountID: "acct_test"},
		Status:       domain.SubscriptionActive,
		PollInterval: 90 * time.Second,
		NextPollAt:   clk.Now(),
		CreatedAt:    clk.Now(),
	}

	fake.planErr = ErrAuthExpired

	if err := sch.PollSubscription(ctx, "usr_test", sub); err != nil {
		t.Fatalf("PollSubscription: %v", err)
	}

	if fake.pauseCalled != 1 {
		t.Errorf("PauseSubscription called %d times, want 1", fake.pauseCalled)
	}
}

type fakeSchedulerService struct {
	plan                 domain.Plan
	planErr              error
	questID              domain.QuestID
	createQuestCalled    int
	subscribeCalled      int
	questStatus          domain.Status
	fulfillCalled        int
	updateNextPollCalled int
	pauseCalled          int
}

// Implement ALL Service interface methods on fakeSchedulerService.
// For methods not used by the scheduler, return nil / zero values.
func (f *fakeSchedulerService) ResolveVenue(ctx context.Context, userID domain.UserID, query domain.VenueQuery) (domain.Venue, error) {
	return domain.Venue{}, nil
}
func (f *fakeSchedulerService) PlanQuest(ctx context.Context, userID domain.UserID, goal domain.Goal) (domain.Plan, error) {
	return f.plan, f.planErr
}
func (f *fakeSchedulerService) CreateQuest(ctx context.Context, userID domain.UserID, goal domain.Goal, opts CreateOpts) (domain.QuestID, error) {
	f.createQuestCalled++
	return f.questID, nil
}
func (f *fakeSchedulerService) GetQuest(ctx context.Context, userID domain.UserID, questID domain.QuestID) (QuestState, error) {
	return QuestState{Summary: QuestSummary{Status: f.questStatus}}, nil
}
func (f *fakeSchedulerService) ListQuests(ctx context.Context, userID domain.UserID, filter ListFilter) ([]QuestSummary, error) {
	return nil, nil
}
func (f *fakeSchedulerService) CancelQuest(ctx context.Context, userID domain.UserID, questID domain.QuestID, opts CancelOpts) error {
	return nil
}
func (f *fakeSchedulerService) SubscribeQuest(ctx context.Context, userID domain.UserID, questID domain.QuestID, callback func(domain.Event)) error {
	f.subscribeCalled++
	return nil
}
func (f *fakeSchedulerService) Login(ctx context.Context, userID domain.UserID, accountEmail, password string) (domain.AccountID, error) {
	return "", nil
}
func (f *fakeSchedulerService) ListAccounts(ctx context.Context, userID domain.UserID) ([]Account, error) {
	return nil, nil
}
func (f *fakeSchedulerService) InviteUser(ctx context.Context, userID domain.UserID, email, role string) (Invite, error) {
	return Invite{}, nil
}
func (f *fakeSchedulerService) AcceptInvite(ctx context.Context, token, email, password string) (domain.UserID, BearerToken, error) {
	return "", BearerToken{}, nil
}
func (f *fakeSchedulerService) IssueToken(ctx context.Context, userID domain.UserID, label, scope string) (BearerToken, error) {
	return BearerToken{}, nil
}
func (f *fakeSchedulerService) RevokeToken(ctx context.Context, userID domain.UserID, tokenID string) error {
	return nil
}
func (f *fakeSchedulerService) ListTokens(ctx context.Context, userID domain.UserID) ([]Token, error) {
	return nil, nil
}
func (f *fakeSchedulerService) ListUsers(ctx context.Context, userID domain.UserID) ([]User, error) {
	return nil, nil
}
func (f *fakeSchedulerService) CreateSubscription(ctx context.Context, userID domain.UserID, goal domain.Goal, compromise *domain.CompromisePolicy, expiresAt *time.Time) (domain.SubscriptionID, error) {
	return "", nil
}
func (f *fakeSchedulerService) GetSubscription(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID) (Subscription, error) {
	return Subscription{}, nil
}
func (f *fakeSchedulerService) ListSubscriptions(ctx context.Context, userID domain.UserID, filter SubscriptionFilter) ([]Subscription, error) {
	return nil, nil
}
func (f *fakeSchedulerService) CancelSubscription(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID) error {
	return nil
}
func (f *fakeSchedulerService) UpdateSubscriptionNextPoll(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID, nextPollAt time.Time) error {
	f.updateNextPollCalled++
	return nil
}
func (f *fakeSchedulerService) PauseSubscription(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID) error {
	f.pauseCalled++
	return nil
}
func (f *fakeSchedulerService) FulfillSubscription(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID, questID domain.QuestID) error {
	f.fulfillCalled++
	return nil
}
