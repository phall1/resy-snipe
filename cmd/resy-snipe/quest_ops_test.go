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

// fakeQuestService is the in-memory service.Service the quest_ops tests
// drive. Every method records its inputs and returns canned data; the
// fields are public-ish (capitalized) inside the package so individual
// tests can assert on them after the call lands.
type fakeQuestService struct {
	listFilter   service.ListFilter
	listUser     domain.UserID
	listRows     []service.QuestSummary
	listErr      error
	getUser      domain.UserID
	getID        domain.QuestID
	getState     service.QuestState
	getErr       error
	cancelUser   domain.UserID
	cancelID     domain.QuestID
	cancelOpts   service.CancelOpts
	cancelErr    error
	cancelCalled bool
}

var _ service.Service = (*fakeQuestService)(nil)

func (f *fakeQuestService) ResolveVenue(_ context.Context, _ domain.UserID, _ domain.VenueQuery) (domain.Venue, error) {
	return domain.Venue{}, service.ErrNotImplemented
}
func (f *fakeQuestService) PlanQuest(_ context.Context, _ domain.UserID, _ domain.Goal) (domain.Plan, error) {
	return domain.Plan{}, service.ErrNotImplemented
}
func (f *fakeQuestService) CreateQuest(_ context.Context, _ domain.UserID, _ domain.Goal, _ service.CreateOpts) (domain.QuestID, error) {
	return "", service.ErrNotImplemented
}
func (f *fakeQuestService) GetQuest(_ context.Context, userID domain.UserID, questID domain.QuestID) (service.QuestState, error) {
	f.getUser = userID
	f.getID = questID
	return f.getState, f.getErr
}
func (f *fakeQuestService) ListQuests(_ context.Context, userID domain.UserID, filter service.ListFilter) ([]service.QuestSummary, error) {
	f.listUser = userID
	f.listFilter = filter
	return f.listRows, f.listErr
}
func (f *fakeQuestService) CancelQuest(_ context.Context, userID domain.UserID, questID domain.QuestID, opts service.CancelOpts) error {
	f.cancelCalled = true
	f.cancelUser = userID
	f.cancelID = questID
	f.cancelOpts = opts
	return f.cancelErr
}
func (f *fakeQuestService) SubscribeQuest(_ context.Context, _ domain.UserID, _ domain.QuestID, _ func(domain.Event)) error {
	return service.ErrNotImplemented
}
func (f *fakeQuestService) Login(_ context.Context, _ domain.UserID, _, _ string) (domain.AccountID, error) {
	return "", service.ErrNotImplemented
}
func (f *fakeQuestService) ListAccounts(_ context.Context, _ domain.UserID) ([]service.Account, error) {
	return nil, service.ErrNotImplemented
}
func (f *fakeQuestService) InviteUser(_ context.Context, _ domain.UserID, _, _ string) (service.Invite, error) {
	return service.Invite{}, service.ErrNotImplemented
}
func (f *fakeQuestService) AcceptInvite(_ context.Context, _, _, _ string) (domain.UserID, service.BearerToken, error) {
	return "", service.BearerToken{}, service.ErrNotImplemented
}
func (f *fakeQuestService) IssueToken(_ context.Context, _ domain.UserID, _, _ string) (service.BearerToken, error) {
	return service.BearerToken{}, service.ErrNotImplemented
}
func (f *fakeQuestService) RevokeToken(_ context.Context, _ domain.UserID, _ string) error {
	return service.ErrNotImplemented
}
func (f *fakeQuestService) ListTokens(_ context.Context, _ domain.UserID) ([]service.Token, error) {
	return nil, service.ErrNotImplemented
}
func (f *fakeQuestService) ListUsers(_ context.Context, _ domain.UserID) ([]service.User, error) {
	return nil, service.ErrNotImplemented
}

// testDefaultUser is the UserID swapQuestService installs into the
// resolveDefaultUserFn seam. Hardcoded (rather than a swapQuestService
// argument) so the unparam linter doesn't flag the helper; tests that
// need a different value pass -user explicitly to the handler.
const testDefaultUser = domain.UserID("u_op")

// swapQuestService installs a fake service.Service for the duration of
// t. The replacement closes over `fake` so the test can mutate /
// inspect it across the run. Cleanup restores the original seam so
// sibling tests in this file don't see the override. The
// resolveDefaultUserFn seam is also stubbed (to testDefaultUser) so
// the no-user-flag path doesn't reach for the real DB.
func swapQuestService(t *testing.T, fake *fakeQuestService) {
	t.Helper()
	prevSvc := newQuestServiceFn
	prevUser := resolveDefaultUserFn
	newQuestServiceFn = func(_ context.Context, _ *slog.Logger, _ clock.Clock) (service.Service, func() error, error) {
		return fake, func() error { return nil }, nil
	}
	resolveDefaultUserFn = func(_ context.Context, _ clock.Clock) (domain.UserID, error) {
		return testDefaultUser, nil
	}
	t.Cleanup(func() {
		newQuestServiceFn = prevSvc
		resolveDefaultUserFn = prevUser
	})
}

// --- parseStatusList --------------------------------------------------------

func TestParseStatusList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []domain.Status
	}{
		{"", nil},
		{"booked", []domain.Status{domain.StatusBooked}},
		{"pending", []domain.Status{domain.StatusSubmitted}},
		{"canceled", []domain.Status{domain.StatusCanceled}},
		{"firing", []domain.Status{domain.StatusAwaiting, domain.StatusFinding, domain.StatusBooking}},
		{"booked,failed", []domain.Status{domain.StatusBooked, domain.StatusFailed}},
		{" booked , failed ", []domain.Status{domain.StatusBooked, domain.StatusFailed}},
		{"booked,booked", []domain.Status{domain.StatusBooked}}, // dedup
	}
	for _, c := range cases {
		got, err := parseStatusList(c.in)
		if err != nil {
			t.Errorf("parseStatusList(%q): unexpected err: %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseStatusList(%q): len = %d, want %d (%v vs %v)", c.in, len(got), len(c.want), got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseStatusList(%q)[%d] = %v, want %v", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestParseStatusList_Unknown(t *testing.T) {
	t.Parallel()
	if _, err := parseStatusList("bogus"); err == nil {
		t.Fatal("expected error for unknown status")
	}
}

// --- runQuestListCmd --------------------------------------------------------

// TestRunQuestList_HappyPath covers the table-format render with the
// canned default user, two rows, no filter flags.
func TestRunQuestList_HappyPath(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{
		listRows: []service.QuestSummary{
			{
				ID:        "qst_a",
				UserID:    "u_op",
				AccountID: "acct_1",
				Status:    domain.StatusSubmitted,
				CreatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
				UpdatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
				PlanHash:  "sha256:abc",
			},
			{
				ID:        "qst_b",
				UserID:    "u_op",
				AccountID: "acct_2",
				Status:    domain.StatusBooked,
				CreatedAt: time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC),
				UpdatedAt: time.Date(2026, 5, 2, 13, 0, 0, 0, time.UTC),
				PlanHash:  "sha256:def",
			},
		},
	}
	swapQuestService(t, fake)

	var out bytes.Buffer
	err := runQuestListCmd(context.Background(), nil, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runQuestListCmd: %v", err)
	}
	got := out.String()
	for _, want := range []string{"ID", "ACCOUNT", "STATUS", "qst_a", "qst_b", "submitted", "booked"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q: %q", want, got)
		}
	}
	if fake.listUser != "u_op" {
		t.Errorf("listUser = %q, want u_op (from default-user seam)", fake.listUser)
	}
	if fake.listFilter.Limit != defaultQuestListLimit {
		t.Errorf("listFilter.Limit = %d, want %d", fake.listFilter.Limit, defaultQuestListLimit)
	}
}

// TestRunQuestList_Empty asserts the friendly empty-state message.
func TestRunQuestList_Empty(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{listRows: nil}
	swapQuestService(t, fake)

	var out bytes.Buffer
	if err := runQuestListCmd(context.Background(), nil, strings.NewReader(""), &out, clock.NewFake(fixedNow)); err != nil {
		t.Fatalf("runQuestListCmd: %v", err)
	}
	if !strings.Contains(out.String(), "(no quests)") {
		t.Errorf("expected '(no quests)' message, got %q", out.String())
	}
}

// TestRunQuestList_Filters exercises every filter flag together: the
// fake records ListFilter and the test asserts the shape end-to-end.
func TestRunQuestList_Filters(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{}
	swapQuestService(t, fake)

	args := []string{
		"-status", "booked,failed",
		"-account", "phall@example.com",
		"-since", "2026-05-01T00:00:00Z",
		"-until", "2026-06-01T00:00:00Z",
		"-limit", "10",
		"-user", "u_explicit",
	}
	var out bytes.Buffer
	if err := runQuestListCmd(context.Background(), args, strings.NewReader(""), &out, clock.NewFake(fixedNow)); err != nil {
		t.Fatalf("runQuestListCmd: %v", err)
	}

	if fake.listUser != "u_explicit" {
		t.Errorf("listUser = %q, want u_explicit", fake.listUser)
	}
	if fake.listFilter.Limit != 10 {
		t.Errorf("limit = %d, want 10", fake.listFilter.Limit)
	}
	if len(fake.listFilter.Status) != 2 ||
		fake.listFilter.Status[0] != domain.StatusBooked ||
		fake.listFilter.Status[1] != domain.StatusFailed {
		t.Errorf("status = %v, want [Booked Failed]", fake.listFilter.Status)
	}
	if fake.listFilter.AccountID == nil || *fake.listFilter.AccountID != "phall@example.com" {
		t.Errorf("accountID = %v, want phall@example.com", fake.listFilter.AccountID)
	}
	if fake.listFilter.Since == nil || fake.listFilter.Since.Year() != 2026 {
		t.Errorf("since = %v, want 2026-05-01", fake.listFilter.Since)
	}
	if fake.listFilter.Until == nil || fake.listFilter.Until.Month() != 6 {
		t.Errorf("until = %v, want 2026-06-01", fake.listFilter.Until)
	}
}

// TestRunQuestList_FormatJSON renders as JSON and parses the round-
// trip output to confirm the shape.
func TestRunQuestList_FormatJSON(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{
		listRows: []service.QuestSummary{
			{
				ID:        "qst_a",
				UserID:    "u_op",
				AccountID: "acct_1",
				Status:    domain.StatusBooked,
				CreatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
				UpdatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
				PlanHash:  "sha256:abc",
			},
		},
	}
	swapQuestService(t, fake)

	var out bytes.Buffer
	if err := runQuestListCmd(context.Background(), []string{"-format=json"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow)); err != nil {
		t.Fatalf("runQuestListCmd: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0]["id"] != "qst_a" {
		t.Errorf("id = %v, want qst_a", rows[0]["id"])
	}
	if rows[0]["status"] != "booked" {
		t.Errorf("status = %v, want booked", rows[0]["status"])
	}
}

// TestRunQuestList_BadStatus surfaces the parseStatusList error path.
func TestRunQuestList_BadStatus(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{}
	swapQuestService(t, fake)
	var out bytes.Buffer
	err := runQuestListCmd(context.Background(),
		[]string{"-status", "wat"}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error for unknown status")
	}
}

// TestRunQuestList_BadFormat surfaces parseQuestFormat's error path.
func TestRunQuestList_BadFormat(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{}
	swapQuestService(t, fake)
	var out bytes.Buffer
	err := runQuestListCmd(context.Background(),
		[]string{"-format=yaml"}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error for unknown format")
	}
}

// --- runQuestGetCmd ---------------------------------------------------------

// TestRunQuestGet_HappyPath asserts the table renders the summary +
// the "no events recorded yet" line when state.Events is empty.
func TestRunQuestGet_HappyPath(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{
		getState: service.QuestState{
			Summary: service.QuestSummary{
				ID:        "qst_a",
				UserID:    "u_op",
				AccountID: "acct_1",
				Status:    domain.StatusBooked,
				CreatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
				UpdatedAt: time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC),
				PlanHash:  "sha256:abc",
			},
			Goal: domain.Goal{
				VenueQuery: domain.VenueQuerySlug{Slug: "carbone", City: "ny"},
				AccountID:  "acct_1",
				Party:      2,
				Date:       domain.Date{Year: 2026, Month: time.June, Day: 1},
				TimePrefs: domain.TimeWindow{
					Start: domain.WallTime{Hour: 18},
					End:   domain.WallTime{Hour: 21},
				},
			},
			Plan: domain.Plan{
				Hash:            "sha256:abc",
				SigningRequired: true,
			},
			// Events deliberately empty — exercises the M1-10 path.
		},
	}
	swapQuestService(t, fake)

	var out bytes.Buffer
	err := runQuestGetCmd(context.Background(), []string{"qst_a"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runQuestGetCmd: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Quest qst_a",
		"Status:    booked",
		"Plan hash: sha256:abc",
		"Venue: slug=carbone city=ny",
		"Party: 2",
		"(no events recorded yet)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q: %s", want, got)
		}
	}
	if fake.getID != "qst_a" {
		t.Errorf("getID = %q, want qst_a", fake.getID)
	}
	if fake.getUser != "u_op" {
		t.Errorf("getUser = %q, want u_op", fake.getUser)
	}
}

// TestRunQuestGet_WrongTenant asserts ErrNotFound surfaces as a
// non-zero exit with the "quest not found" copy.
func TestRunQuestGet_WrongTenant(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{getErr: service.ErrNotFound}
	swapQuestService(t, fake)

	var out bytes.Buffer
	err := runQuestGetCmd(context.Background(), []string{"qst_other"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected non-nil error for wrong-tenant lookup")
	}
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("error should wrap service.ErrNotFound: %v", err)
	}
	if !strings.Contains(out.String(), "quest not found") {
		t.Errorf("output should say 'quest not found': %q", out.String())
	}
	// Critical: must NOT say 'unauthorized' — tenancy elides existence.
	if strings.Contains(strings.ToLower(out.String()), "unauthorized") {
		t.Errorf("output must not say 'unauthorized' (tenancy elides existence): %q", out.String())
	}
}

func TestRunQuestGet_MissingID(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{}
	swapQuestService(t, fake)
	var out bytes.Buffer
	err := runQuestGetCmd(context.Background(), nil, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error for missing <questID>")
	}
	if !strings.Contains(err.Error(), "questID") {
		t.Errorf("error should mention 'questID': %v", err)
	}
}

func TestRunQuestGet_FormatJSON(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{
		getState: service.QuestState{
			Summary: service.QuestSummary{
				ID:        "qst_a",
				UserID:    "u_op",
				AccountID: "acct_1",
				Status:    domain.StatusBooked,
				PlanHash:  "sha256:abc",
			},
			Goal: domain.Goal{
				VenueQuery: domain.VenueQueryName{Name: "Carbone"},
				AccountID:  "acct_1",
				Party:      2,
				Date:       domain.Date{Year: 2026, Month: time.June, Day: 1},
				TimePrefs: domain.TimeWindow{
					Start: domain.WallTime{Hour: 18},
					End:   domain.WallTime{Hour: 21},
				},
			},
		},
	}
	swapQuestService(t, fake)
	var out bytes.Buffer
	if err := runQuestGetCmd(context.Background(),
		[]string{"qst_a", "-format=json"}, strings.NewReader(""), &out, clock.NewFake(fixedNow)); err != nil {
		t.Fatalf("runQuestGetCmd: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if summary, ok := payload["summary"].(map[string]any); !ok || summary["id"] != "qst_a" {
		t.Errorf("json missing summary.id=qst_a: %v", payload)
	}
}

// --- runQuestCancelCmd ------------------------------------------------------

// TestRunQuestCancel_WithYes skips the prompt and calls
// Service.CancelQuest directly.
func TestRunQuestCancel_WithYes(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{}
	swapQuestService(t, fake)

	var out bytes.Buffer
	err := runQuestCancelCmd(context.Background(),
		[]string{"qst_a", "-yes", "-reason", "changed mind"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runQuestCancelCmd: %v", err)
	}
	if !fake.cancelCalled {
		t.Fatal("expected CancelQuest to be called")
	}
	if fake.cancelID != "qst_a" {
		t.Errorf("cancelID = %q, want qst_a", fake.cancelID)
	}
	if fake.cancelOpts.Reason != "changed mind" {
		t.Errorf("reason = %q, want 'changed mind'", fake.cancelOpts.Reason)
	}
	if !strings.Contains(out.String(), "canceled qst_a") {
		t.Errorf("expected 'canceled qst_a' confirmation, got %q", out.String())
	}
}

// TestRunQuestCancel_PromptAccepts feeds "y\n" to stdin and confirms
// the prompt path lands the cancel.
func TestRunQuestCancel_PromptAccepts(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{}
	swapQuestService(t, fake)

	var out bytes.Buffer
	err := runQuestCancelCmd(context.Background(),
		[]string{"qst_a"}, strings.NewReader("y\n"), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runQuestCancelCmd: %v", err)
	}
	if !fake.cancelCalled {
		t.Fatal("expected CancelQuest to be called after 'y' answer")
	}
	body := out.String()
	if !strings.Contains(body, "Cancel quest qst_a?") {
		t.Errorf("prompt missing: %q", body)
	}
	if !strings.Contains(body, "canceled qst_a") {
		t.Errorf("confirmation missing: %q", body)
	}
}

// TestRunQuestCancel_PromptDeclines feeds "n\n" and asserts the cancel
// path is NOT taken. The handler returns nil so the binary exits 0 —
// refusing to confirm is a successful exit.
func TestRunQuestCancel_PromptDeclines(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{}
	swapQuestService(t, fake)

	var out bytes.Buffer
	err := runQuestCancelCmd(context.Background(),
		[]string{"qst_a"}, strings.NewReader("n\n"), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runQuestCancelCmd (declined): %v", err)
	}
	if fake.cancelCalled {
		t.Fatal("expected CancelQuest NOT to be called after 'n' answer")
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Errorf("expected 'aborted' message, got %q", out.String())
	}
}

// TestRunQuestCancel_Terminal exercises the docs/v2 contract: cancel
// on a terminal quest is a no-op at the service layer, returns nil,
// and the CLI still prints the confirmation.
func TestRunQuestCancel_Terminal(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	// Service returns nil (per docs/v2/design/service-layer.md
	// §error-model — canceling a terminal quest is idempotent).
	fake := &fakeQuestService{cancelErr: nil}
	swapQuestService(t, fake)

	var out bytes.Buffer
	err := runQuestCancelCmd(context.Background(),
		[]string{"qst_terminal", "-yes"}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runQuestCancelCmd: %v", err)
	}
	if !fake.cancelCalled {
		t.Fatal("expected CancelQuest to be called")
	}
	if !strings.Contains(out.String(), "canceled qst_terminal") {
		t.Errorf("expected confirmation line, got %q", out.String())
	}
}

// TestRunQuestCancel_NotFound asserts the wrong-tenant / missing-id
// path surfaces "quest not found".
func TestRunQuestCancel_NotFound(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{cancelErr: service.ErrNotFound}
	swapQuestService(t, fake)

	var out bytes.Buffer
	err := runQuestCancelCmd(context.Background(),
		[]string{"qst_other", "-yes"}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("error should wrap ErrNotFound: %v", err)
	}
	if !strings.Contains(out.String(), "quest not found") {
		t.Errorf("output should say 'quest not found': %q", out.String())
	}
}

func TestRunQuestCancel_MissingID(t *testing.T) { //nolint:paralleltest // mutates package-level seams.
	fake := &fakeQuestService{}
	swapQuestService(t, fake)
	var out bytes.Buffer
	err := runQuestCancelCmd(context.Background(), nil, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error for missing <questID>")
	}
}

// --- resolveDefaultUser -----------------------------------------------------

// TestResolveQuestUser_ExplicitFlag bypasses the default-user seam
// when -user is supplied. We don't need to install a fake — the helper
// returns the raw input verbatim.
func TestResolveQuestUser_ExplicitFlag(t *testing.T) {
	t.Parallel()
	got, err := resolveQuestUser(context.Background(), "u_explicit", clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("resolveQuestUser: %v", err)
	}
	if got != "u_explicit" {
		t.Errorf("got %q, want u_explicit", got)
	}
}

// TestResolveQuestUser_DefaultViaSeam exercises the empty-flag branch
// through the test seam: the fake returns "u_seam" and resolveQuestUser
// must forward that value untouched.
func TestResolveQuestUser_DefaultViaSeam(t *testing.T) { //nolint:paralleltest // mutates package-level seam.
	prev := resolveDefaultUserFn
	t.Cleanup(func() { resolveDefaultUserFn = prev })
	resolveDefaultUserFn = func(_ context.Context, _ clock.Clock) (domain.UserID, error) {
		return "u_seam", nil
	}
	got, err := resolveQuestUser(context.Background(), "", clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("resolveQuestUser: %v", err)
	}
	if got != "u_seam" {
		t.Errorf("got %q, want u_seam", got)
	}
}
