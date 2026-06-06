package service_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/engine"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resolver"
	"resy-snipe/internal/service"
	"resy-snipe/internal/store"
)

// ---- fixtures ------------------------------------------------------------

// testStoreBackend binds *store.SQLiteStore to service.StoreBackend.
// Mirrors cmd/resy-snipe/service_store_adapter.go field-for-field; the
// duplication keeps internal/service compilable without importing
// internal/store (Law 5) while still letting the service test use a
// real SQLite backend end-to-end.
type testStoreBackend struct {
	store *store.SQLiteStore
}

func newTestStoreBackend(s *store.SQLiteStore) *testStoreBackend {
	return &testStoreBackend{store: s}
}

var _ service.StoreBackend = (*testStoreBackend)(nil)

func (a *testStoreBackend) CreateQuest(ctx context.Context, q service.QuestRow) error {
	return store.CreateQuest(ctx, a.store.DB(), store.QuestRow{
		ID: q.ID, UserID: q.UserID, AccountID: q.AccountID,
		GoalJSON: q.GoalJSON, PlanHash: q.PlanHash, Status: q.Status,
		CreatedAt: q.CreatedAt, UpdatedAt: q.UpdatedAt, CompletedAt: q.CompletedAt,
	})
}

func (a *testStoreBackend) GetQuest(ctx context.Context, userID domain.UserID, questID domain.QuestID) (service.QuestRow, error) {
	r, err := store.GetQuest(ctx, a.store.DB(), userID, questID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return service.QuestRow{}, service.ErrNotFound
		}
		return service.QuestRow{}, err
	}
	return service.QuestRow{
		ID: r.ID, UserID: r.UserID, AccountID: r.AccountID,
		GoalJSON: r.GoalJSON, PlanHash: r.PlanHash, Status: r.Status,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, CompletedAt: r.CompletedAt,
	}, nil
}

func (a *testStoreBackend) ListQuests(ctx context.Context, userID domain.UserID, filter service.ListFilter) ([]service.QuestRow, error) {
	rows, err := store.ListQuests(ctx, a.store.DB(), userID, store.QuestListFilter{
		Status: filter.Status, AccountID: filter.AccountID,
		Since: filter.Since, Until: filter.Until,
		Limit: filter.Limit, Cursor: filter.Cursor,
	})
	if err != nil {
		return nil, err
	}
	out := make([]service.QuestRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, service.QuestRow{
			ID: r.ID, UserID: r.UserID, AccountID: r.AccountID,
			GoalJSON: r.GoalJSON, PlanHash: r.PlanHash, Status: r.Status,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, CompletedAt: r.CompletedAt,
		})
	}
	return out, nil
}

func (a *testStoreBackend) UpdateQuestStatus(
	ctx context.Context, userID domain.UserID, questID domain.QuestID,
	newStatus domain.Status, completedAt *time.Time, updatedAt time.Time,
) error {
	err := store.UpdateQuestStatus(ctx, a.store.DB(), userID, questID, newStatus, completedAt, updatedAt)
	if err != nil && errors.Is(err, store.ErrNotFound) {
		return service.ErrNotFound
	}
	return err
}

func (a *testStoreBackend) ListAccounts(ctx context.Context, userID domain.UserID) ([]service.AccountRow, error) {
	rows, err := store.ListAccountsForUser(ctx, a.store.DB(), userID)
	if err != nil {
		return nil, err
	}
	out := make([]service.AccountRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, service.AccountRow{
			ID: r.ID, UserID: r.UserID, ResyEmail: r.ResyEmail,
			DisplayName: r.DisplayName, CreatedAt: r.CreatedAt, DisabledAt: r.DisabledAt,
		})
	}
	return out, nil
}

func (a *testStoreBackend) GetAccountByEmail(ctx context.Context, userID domain.UserID, email string) (service.AccountRow, error) {
	r, err := store.GetAccountByEmail(ctx, a.store.DB(), userID, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return service.AccountRow{}, service.ErrNotFound
		}
		return service.AccountRow{}, err
	}
	return service.AccountRow{
		ID: r.ID, UserID: r.UserID, ResyEmail: r.ResyEmail,
		DisplayName: r.DisplayName, CreatedAt: r.CreatedAt, DisabledAt: r.DisabledAt,
	}, nil
}

func (a *testStoreBackend) BindAccountToUser(ctx context.Context, userID domain.UserID, email string) (domain.AccountID, error) {
	id, err := store.BindLegacyAccountToUser(ctx, a.store.DB(), userID, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", service.ErrNotFound
		}
		return "", err
	}
	return id, nil
}

func (a *testStoreBackend) ListEvents(_ context.Context, _ domain.UserID, _ domain.QuestID, _ int) ([]service.EventRow, error) {
	return nil, nil
}

func (a *testStoreBackend) ListQuestEvents(
	ctx context.Context, userID domain.UserID, questID domain.QuestID, limit int,
) ([]domain.Event, error) {
	return store.ListQuestEvents(ctx, a.store.DB(), userID, questID, limit)
}

func (a *testStoreBackend) GetIdempotencyResult(
	ctx context.Context,
	userID domain.UserID,
	scope, plaintextKey, payloadHash string,
	now time.Time,
) (service.IdempotencyLookup, error) {
	lookup, err := store.GetIdempotencyResult(ctx, a.store.DB(), userID, scope, plaintextKey, payloadHash, now)
	if err != nil {
		return service.IdempotencyLookup{}, err
	}
	return service.IdempotencyLookup{
		Found:        lookup.Found,
		PayloadMatch: lookup.PayloadMatch,
		Expired:      lookup.Expired,
		TargetID:     lookup.TargetID,
		PrevErr:      lookup.ResultErr,
	}, nil
}

func (a *testStoreBackend) PutIdempotencyResult(
	ctx context.Context,
	userID domain.UserID,
	scope, plaintextKey, payloadHash string,
	targetID string,
	errVal error,
	ttl time.Duration,
	now time.Time,
) error {
	return store.PutIdempotencyResult(ctx, a.store.DB(), userID, scope, plaintextKey, payloadHash, targetID, errVal, ttl, now)
}

func (a *testStoreBackend) WriteAuditEvent(ctx context.Context, evt service.AuditWrite) error {
	var target *domain.UserID
	if evt.TargetUserID != nil {
		t := *evt.TargetUserID
		target = &t
	}
	return store.WriteAuditEvent(ctx, a.store.DB(), store.AuditEventInput{
		UserID:       evt.UserID,
		TargetUserID: target,
		Action:       evt.Action,
		TargetID:     evt.TargetID,
		OK:           evt.OK,
		ErrorCode:    evt.ErrorCode,
		IP:           evt.IP,
		UserAgent:    evt.UserAgent,
		DetailsJSON:  evt.DetailsJSON,
	})
}

func (a *testStoreBackend) WriteQuestEvent(
	ctx context.Context,
	userID domain.UserID,
	questID domain.QuestID,
	evt domain.Event,
) error {
	return store.WriteQuestEvent(ctx, a.store.DB(), userID, questID, evt)
}

func (a *testStoreBackend) InsertToken(ctx context.Context, t service.TokenRecord) error {
	return store.InsertToken(ctx, a.store.DB(), store.TokenRow{
		ID:        t.ID,
		UserID:    t.UserID,
		Hash:      t.Hash,
		Scopes:    t.Scope,
		Label:     t.Label,
		CreatedAt: t.CreatedAt,
		LastSeen:  t.LastSeen,
		RevokedAt: t.RevokedAt,
	})
}

func (a *testStoreBackend) ListTokensForUser(
	ctx context.Context,
	userID domain.UserID,
) ([]service.TokenRecord, error) {
	rows, err := store.ListTokensForUser(ctx, a.store.DB(), userID)
	if err != nil {
		return nil, err
	}
	out := make([]service.TokenRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, service.TokenRecord{
			ID:        r.ID,
			UserID:    r.UserID,
			Hash:      r.Hash,
			Scope:     r.Scopes,
			Label:     r.Label,
			CreatedAt: r.CreatedAt,
			LastSeen:  r.LastSeen,
			RevokedAt: r.RevokedAt,
		})
	}
	return out, nil
}

func (a *testStoreBackend) RevokeToken(
	ctx context.Context,
	userID domain.UserID,
	tokenID string,
	now time.Time,
) error {
	err := store.RevokeToken(ctx, a.store.DB(), userID, tokenID, now)
	if err != nil && errors.Is(err, store.ErrNotFound) {
		return service.ErrNotFound
	}
	return err
}

func (a *testStoreBackend) CreateSubscription(_ context.Context, _ service.SubscriptionRow) error {
	return service.ErrNotImplemented
}

func (a *testStoreBackend) GetSubscription(_ context.Context, _ domain.UserID, _ domain.SubscriptionID) (service.SubscriptionRow, error) {
	return service.SubscriptionRow{}, service.ErrNotImplemented
}

func (a *testStoreBackend) ListSubscriptions(_ context.Context, _ domain.UserID, _ service.SubscriptionFilter) ([]service.SubscriptionRow, error) {
	return nil, service.ErrNotImplemented
}

func (a *testStoreBackend) UpdateSubscriptionStatus(
	_ context.Context,
	_ domain.UserID,
	_ domain.SubscriptionID,
	_ domain.SubscriptionStatus,
	_ *domain.QuestID,
	_ *time.Time,
	_ time.Time,
) error {
	return service.ErrNotImplemented
}

// ---- fake resolver / provider / auth -------------------------------------

type fakeResolver struct {
	venues map[string]domain.Venue // key: query.String()
	err    error
}

func (f *fakeResolver) Resolve(_ context.Context, q domain.VenueQuery) (domain.Venue, error) {
	if f.err != nil {
		return domain.Venue{}, f.err
	}
	v, ok := f.venues[q.String()]
	if !ok {
		return domain.Venue{}, resolver.ErrVenueNotFound
	}
	return v, nil
}

type fakeAuth struct {
	mu         sync.Mutex
	lastEmail  string
	upsertedDB *store.SQLiteStore
	failWith   error
	clock      clock.Clock
}

func (f *fakeAuth) Login(ctx context.Context, email, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return "", f.failWith
	}
	f.lastEmail = email
	// Mimic the v1 bridge UpsertSession side-effect: ensure a legacy
	// accounts row exists with user_id IS NULL.
	if f.upsertedDB != nil {
		now := f.clock.Now()
		if err := f.upsertedDB.UpsertSession(ctx, store.SessionRow{
			UserID:    domain.UserID(email),
			Provider:  "resy",
			JWT:       "fake-jwt",
			ExpiresAt: now.Add(24 * time.Hour),
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			return "", err
		}
	}
	return email, nil
}

// fakeProvider implements providers.Provider with a minimal Calendar
// that the planner is happy to consume.
type fakeProvider struct{}

func (fakeProvider) ID() domain.ProviderID { return "resy" }
func (fakeProvider) Login(context.Context, providers.Credentials) (providers.Session, error) {
	return nil, errors.New("unused")
}
func (fakeProvider) Ping(context.Context, providers.Session) error { return nil }
func (fakeProvider) SearchVenues(context.Context, providers.Query) ([]domain.Venue, error) {
	return nil, nil
}
func (fakeProvider) ResolveVenue(context.Context, string, string) (domain.Venue, error) {
	return domain.Venue{}, providers.ErrVenueNotFound
}
func (fakeProvider) SearchVenuesByName(context.Context, string) ([]domain.Venue, error) {
	return nil, nil
}
func (fakeProvider) Calendar(_ context.Context, ref domain.VenueRef, _ providers.DateRange) (providers.Calendar, error) {
	// Empty calendar — planner falls through to Explicit/Discovered.
	return providers.Calendar{Venue: ref}, nil
}
func (fakeProvider) Find(context.Context, providers.FindRequest) ([]providers.Slot, error) {
	return nil, nil
}
func (fakeProvider) Book(context.Context, providers.Slot, providers.Session) (providers.Confirmation, error) {
	return providers.Confirmation{}, errors.New("unused")
}

// ---- harness -------------------------------------------------------------

type harness struct {
	svc     *service.Standard
	db      *store.SQLiteStore
	clk     *clock.Fake
	eng     *engine.Engine
	uid     domain.UserID
	otherID domain.UserID
	acctID  domain.AccountID
	venue   domain.Venue
	prov    fakeProvider
	auth    *fakeAuth
}

// newHarness opens an in-process SQLite DB, migrates, seeds an
// operator + invited user + accounts, and constructs a Standard
// service ready for end-to-end exercise.
func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	rawDB, err := store.Open(ctx, filepath.Join(dir, "svc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	if err := store.Migrate(ctx, rawDB); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	uid, err := store.SeedOperator(ctx, rawDB, store.SeedOpts{Email: "op@example.com"})
	if err != nil {
		t.Fatalf("SeedOperator: %v", err)
	}

	// Second user for isolation tests.
	otherUID := domain.UserID("usr_other")
	if _, err := rawDB.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, role, invited_by, created_at)
		VALUES (?, 'other@example.com', x'', 'user', ?, ?)`,
		string(otherUID), string(uid), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert other user: %v", err)
	}

	// Operator-owned account.
	acctID := domain.AccountID("acct_op01")
	if _, err := rawDB.ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, resy_email, display_name, created_at)
		VALUES (?, ?, 'op@example.com', 'operator', ?)`,
		string(acctID), string(uid), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	sqlStore := store.NewSQLiteStore(rawDB)
	clk := clock.NewFake(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	logger := slog.New(slog.DiscardHandler)

	venue := domain.Venue{Provider: "resy", Ref: "vn-42", Name: "Carbone", TZ: time.UTC}
	res := &fakeResolver{venues: map[string]domain.Venue{
		domain.VenueQuerySlug{Slug: "carbone", City: "ny"}.String(): venue,
	}}
	prov := fakeProvider{}
	eng := engine.New(sqlStore, clk, logger, engine.WithProvider(prov))
	auth := &fakeAuth{upsertedDB: sqlStore, clock: clk}

	svc, err := service.New(res, eng, newTestStoreBackend(sqlStore), clk,
		service.WithLogger(logger),
		service.WithProvider(prov),
		service.WithAuth(auth),
	)
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	return &harness{
		svc: svc, db: sqlStore, clk: clk, eng: eng,
		uid: uid, otherID: otherUID, acctID: acctID,
		venue: venue, prov: prov, auth: auth,
	}
}

// sampleGoal returns a Goal valid for the harness fixture.
func (h *harness) sampleGoal() domain.Goal {
	return domain.Goal{
		VenueQuery: domain.VenueQuerySlug{Slug: "carbone", City: "ny"},
		Date:       domain.NewDate(2026, time.June, 10),
		Party:      2,
		TimePrefs: domain.TimeWindow{
			Start: domain.WallTime{Hour: 19, Minute: 0},
			End:   domain.WallTime{Hour: 21, Minute: 0},
		},
		AccountID: h.acctID,
	}
}

// ---- tests ---------------------------------------------------------------

func TestStandardResolveVenue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	v, err := h.svc.ResolveVenue(t.Context(), h.uid,
		domain.VenueQuerySlug{Slug: "carbone", City: "ny"})
	if err != nil {
		t.Fatalf("ResolveVenue: %v", err)
	}
	if v.Ref != h.venue.Ref {
		t.Errorf("venue ref: got %q want %q", v.Ref, h.venue.Ref)
	}
}

func TestStandardResolveVenueNotFoundMapsToService(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	_, err := h.svc.ResolveVenue(t.Context(), h.uid,
		domain.VenueQuerySlug{Slug: "missing", City: "??"})
	if !errors.Is(err, service.ErrVenueNotFound) {
		t.Fatalf("ResolveVenue: want ErrVenueNotFound, got %v", err)
	}
}

func TestStandardPlanQuestProducesHashedPlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	plan, err := h.svc.PlanQuest(t.Context(), h.uid, h.sampleGoal())
	if err != nil {
		t.Fatalf("PlanQuest: %v", err)
	}
	if plan.Hash == "" {
		t.Error("PlanQuest returned plan with empty Hash")
	}
	if plan.Venue.Ref != h.venue.Ref {
		t.Errorf("plan.Venue: got %q want %q", plan.Venue.Ref, h.venue.Ref)
	}
}

func TestStandardCreateQuestPersistsAndSubmits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	ctx := t.Context()
	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	if qid == "" {
		t.Fatal("CreateQuest returned empty QuestID")
	}

	// Read back through the StoreBackend's path via GetQuest.
	state, err := h.svc.GetQuest(ctx, h.uid, qid)
	if err != nil {
		t.Fatalf("GetQuest: %v", err)
	}
	if state.Summary.ID != qid {
		t.Errorf("GetQuest id: got %q want %q", state.Summary.ID, qid)
	}
	if state.Summary.AccountID != h.acctID {
		t.Errorf("GetQuest account: got %q want %q", state.Summary.AccountID, h.acctID)
	}

	// The engine also wrote a v1 snipes row at the same SnipeID; the
	// state machine should be in Submitted (pending in the v2 quest
	// status mapping).
	if state.Summary.Status != domain.StatusSubmitted {
		t.Errorf("status: got %v want StatusSubmitted", state.Summary.Status)
	}
}

func TestStandardCreateQuestWrongPlanHashRefuses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wrong := "sha256:badbadbad"
	_, err := h.svc.CreateQuest(t.Context(), h.uid, h.sampleGoal(),
		service.CreateOpts{PlanHash: &wrong})
	if !errors.Is(err, service.ErrInvalidPlanHash) {
		t.Fatalf("CreateQuest wrong hash: want ErrInvalidPlanHash, got %v", err)
	}
}

func TestStandardGetQuestWrongUserReturnsNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	_, err = h.svc.GetQuest(ctx, h.otherID, qid)
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("GetQuest wrong user: want ErrNotFound, got %v", err)
	}
}

func TestStandardListQuestsScopedToOwner(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}

	rows, err := h.svc.ListQuests(ctx, h.uid, service.ListFilter{})
	if err != nil {
		t.Fatalf("ListQuests: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != qid {
		t.Errorf("ListQuests owner: got %+v want one row with ID %s", rows, qid)
	}

	otherRows, err := h.svc.ListQuests(ctx, h.otherID, service.ListFilter{})
	if err != nil {
		t.Fatalf("ListQuests other: %v", err)
	}
	if len(otherRows) != 0 {
		t.Errorf("ListQuests other: got %d rows, want 0", len(otherRows))
	}
}

func TestStandardCancelQuestFlipsStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	if err := h.svc.CancelQuest(ctx, h.uid, qid, service.CancelOpts{Reason: "test"}); err != nil {
		t.Fatalf("CancelQuest: %v", err)
	}
	state, err := h.svc.GetQuest(ctx, h.uid, qid)
	if err != nil {
		t.Fatalf("GetQuest: %v", err)
	}
	if state.Summary.Status != domain.StatusCanceled {
		t.Errorf("status after cancel: got %v want Canceled", state.Summary.Status)
	}
	if state.Summary.CompletedAt == nil {
		t.Error("CancelQuest did not stamp completed_at")
	}
}

func TestStandardCancelTerminalQuestIsNoop(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	if err := h.svc.CancelQuest(ctx, h.uid, qid, service.CancelOpts{}); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	// Second cancel is a no-op — must NOT error.
	if err := h.svc.CancelQuest(ctx, h.uid, qid, service.CancelOpts{}); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
}

func TestStandardCancelWrongUserReturnsNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	qid, err := h.svc.CreateQuest(ctx, h.uid, h.sampleGoal(), service.CreateOpts{})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	err = h.svc.CancelQuest(ctx, h.otherID, qid, service.CancelOpts{})
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("CancelQuest wrong user: want ErrNotFound, got %v", err)
	}
}

func TestStandardLoginReturnsAccountID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	// fakeAuth.Login side-effects an UpsertSession that creates a
	// legacy NULL-user_id accounts row. Login then claims it for
	// h.uid and returns its AccountID.
	acctID, err := h.svc.Login(ctx, h.uid, "new-resy@example.com", "pw")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if acctID == "" {
		t.Fatal("Login returned empty AccountID")
	}

	// ListAccounts should now show the bound row.
	accts, err := h.svc.ListAccounts(ctx, h.uid)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	found := false
	for _, a := range accts {
		if a.ID == acctID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Login-bound account not in ListAccounts: %+v", accts)
	}
}

func TestStandardListAccountsScopedToUser(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	accts, err := h.svc.ListAccounts(ctx, h.uid)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	for _, a := range accts {
		if a.UserID != h.uid {
			t.Errorf("leaked account from %s: %+v", a.UserID, a)
		}
	}
	otherAccts, err := h.svc.ListAccounts(ctx, h.otherID)
	if err != nil {
		t.Fatalf("ListAccounts other: %v", err)
	}
	if len(otherAccts) != 0 {
		t.Errorf("other user accounts: got %d want 0", len(otherAccts))
	}
}

func TestStandardDeferredOperatorVerbsReturnNotImplemented(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	if _, err := h.svc.InviteUser(ctx, h.uid, "a@b.c", "user"); !errors.Is(err, service.ErrNotImplemented) {
		t.Errorf("InviteUser: %v", err)
	}
	if _, _, err := h.svc.AcceptInvite(ctx, "tok", "a@b.c", "pw"); !errors.Is(err, service.ErrNotImplemented) {
		t.Errorf("AcceptInvite: %v", err)
	}
	if _, err := h.svc.ListUsers(ctx, h.uid); !errors.Is(err, service.ErrNotImplemented) {
		t.Errorf("ListUsers: %v", err)
	}
}

func TestStandardNewValidatesRequiredDeps(t *testing.T) {
	t.Parallel()
	if _, err := service.New(nil, nil, nil, nil); err == nil {
		t.Error("service.New(nil...): want error, got nil")
	}
}
