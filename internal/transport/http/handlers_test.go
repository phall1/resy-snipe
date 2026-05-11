package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
	transport "resy-snipe/internal/transport/http"
)

// stubBackend is a single fake satisfying the venuesBackend,
// plansBackend, questsBackend, and accountsBackend interfaces — the
// MountXRoutes calls accept whatever shape the route needs, so one
// struct is enough.
type stubBackend struct {
	// venue
	resolveVenueResult domain.Venue
	resolveVenueErr    error
	resolveVenueCalls  atomic.Int64
	lastVenueQuery     domain.VenueQuery

	// plan
	planResult  domain.Plan
	planErr     error
	planCalls   atomic.Int64
	lastPlanUID domain.UserID

	// quests
	createResult   domain.QuestID
	createErr      error
	lastCreateOpts service.CreateOpts
	getResult      service.QuestState
	getErr         error
	listResult     []service.QuestSummary
	listErr        error
	lastListFilter service.ListFilter
	cancelErr      error
	lastCancelOpts service.CancelOpts
	lastCancelQID  domain.QuestID
	lastCancelUID  domain.UserID

	// accounts
	loginResult domain.AccountID
	loginErr    error
	lastLogin   struct{ email, password string }
	listAccts   []service.Account
	listAcctErr error
}

func (s *stubBackend) ResolveVenue(_ context.Context, _ domain.UserID, q domain.VenueQuery) (domain.Venue, error) {
	s.resolveVenueCalls.Add(1)
	s.lastVenueQuery = q
	return s.resolveVenueResult, s.resolveVenueErr
}

func (s *stubBackend) PlanQuest(_ context.Context, uid domain.UserID, _ domain.Goal) (domain.Plan, error) {
	s.planCalls.Add(1)
	s.lastPlanUID = uid
	return s.planResult, s.planErr
}

func (s *stubBackend) CreateQuest(_ context.Context, _ domain.UserID, _ domain.Goal, opts service.CreateOpts) (domain.QuestID, error) {
	s.lastCreateOpts = opts
	return s.createResult, s.createErr
}

func (s *stubBackend) GetQuest(_ context.Context, _ domain.UserID, _ domain.QuestID) (service.QuestState, error) {
	return s.getResult, s.getErr
}

func (s *stubBackend) ListQuests(_ context.Context, _ domain.UserID, filter service.ListFilter) ([]service.QuestSummary, error) {
	s.lastListFilter = filter
	return s.listResult, s.listErr
}

func (s *stubBackend) CancelQuest(_ context.Context, uid domain.UserID, qid domain.QuestID, opts service.CancelOpts) error {
	s.lastCancelUID = uid
	s.lastCancelQID = qid
	s.lastCancelOpts = opts
	return s.cancelErr
}

func (s *stubBackend) Login(_ context.Context, _ domain.UserID, email, password string) (domain.AccountID, error) {
	s.lastLogin.email = email
	s.lastLogin.password = password
	return s.loginResult, s.loginErr
}

func (s *stubBackend) ListAccounts(_ context.Context, _ domain.UserID) ([]service.Account, error) {
	return s.listAccts, s.listAcctErr
}

// newRoutedServer builds a server with every route mounted and wraps
// it in a stub auth layer that stamps the supplied caller.
func newRoutedServer(t *testing.T, b *stubBackend, callerUID domain.UserID) *httptest.Server {
	t.Helper()
	s := transport.NewServer("127.0.0.1:0", "test", nil)
	transport.MountVenueRoutes(s, b)
	transport.MountPlanRoutes(s, b)
	transport.MountQuestRoutes(s, b)
	transport.MountAccountRoutes(s, b)

	wrap := stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		ctx := r.Context()
		if callerUID != "" {
			ctx = service.ContextWithCaller(ctx, callerUID, "user")
		}
		s.Handler().ServeHTTP(w, r.WithContext(ctx))
	})
	ts := httptest.NewServer(wrap)
	t.Cleanup(ts.Close)
	return ts
}

func doJSON(t *testing.T, ts *httptest.Server, method, path, body string) *stdhttp.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req, err := stdhttp.NewRequestWithContext(context.Background(), method, ts.URL+path, reader)
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do %s %s: %v", method, path, err)
	}
	return resp
}

// ---- ResolveVenue ---------------------------------------------------------

func TestResolveVenueRoute_HappyPath(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("America/New_York")
	b := &stubBackend{
		resolveVenueResult: domain.Venue{
			Provider: "resy", Ref: "vn-42", Name: "Carbone", TZ: loc,
		},
	}
	ts := newRoutedServer(t, b, "usr_op")
	resp := doJSON(t, ts, "POST", "/v1/venues/resolve",
		`{"venue_query":{"kind":"slug","slug":"carbone","city":"ny"}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["provider"] != "resy" || got["ref"] != "vn-42" || got["tz"] != "America/New_York" {
		t.Errorf("body: %+v", got)
	}
	if vq, ok := b.lastVenueQuery.(domain.VenueQuerySlug); !ok || vq.Slug != "carbone" || vq.City != "ny" {
		t.Errorf("forwarded query: %+v", b.lastVenueQuery)
	}
}

func TestResolveVenueRoute_BadKindReturns422(t *testing.T) {
	t.Parallel()
	b := &stubBackend{}
	ts := newRoutedServer(t, b, "usr_op")
	resp := doJSON(t, ts, "POST", "/v1/venues/resolve",
		`{"venue_query":{"kind":"alien"}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusUnprocessableEntity {
		t.Errorf("status=%d, want 422", resp.StatusCode)
	}
}

func TestResolveVenueRoute_NoCallerReturns401(t *testing.T) {
	t.Parallel()
	b := &stubBackend{}
	ts := newRoutedServer(t, b, "")
	resp := doJSON(t, ts, "POST", "/v1/venues/resolve",
		`{"venue_query":{"kind":"slug","slug":"x","city":"ny"}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
}

// ---- PlanQuest ------------------------------------------------------------

func TestPlanQuestRoute_HappyPath(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("America/New_York")
	b := &stubBackend{
		planResult: domain.Plan{
			Venue:           domain.Venue{Provider: "resy", Ref: "v1", Name: "x", TZ: loc},
			DropMoment:      time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
			Strategy:        domain.ExplicitRelease{At: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)},
			SigningRequired: true,
			Hash:            "sha256:abc",
			PlanHashVersion: 1,
			ComputedAt:      time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
		},
	}
	ts := newRoutedServer(t, b, "usr_op")
	body := `{"goal":{"venue_query":{"kind":"slug","slug":"x","city":"ny"},"date":"2026-06-10","party":2,"time_prefs":{"start":"19:00","end":"21:00","priority":"earlier"},"account_id":"acct_1","constraints":{}}}`
	resp := doJSON(t, ts, "POST", "/v1/quests/plan", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var got struct {
		Plan struct {
			Hash     string `json:"hash"`
			Strategy struct {
				Tag string `json:"tag"`
				At  string `json:"at"`
			} `json:"strategy"`
		} `json:"plan"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Plan.Hash != "sha256:abc" || got.Plan.Strategy.Tag != "explicit" {
		t.Errorf("body: %+v", got)
	}
	if b.lastPlanUID != "usr_op" {
		t.Errorf("forwarded uid: %s, want usr_op", b.lastPlanUID)
	}
}

func TestPlanQuestRoute_InvalidGoalReturns422(t *testing.T) {
	t.Parallel()
	b := &stubBackend{}
	ts := newRoutedServer(t, b, "usr_op")
	resp := doJSON(t, ts, "POST", "/v1/quests/plan",
		`{"goal":{"venue_query":{"kind":"slug","slug":"x","city":"ny"},"date":"NOT-A-DATE","party":2,"time_prefs":{"start":"19:00","end":"21:00"},"account_id":"a","constraints":{}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusUnprocessableEntity {
		t.Errorf("status=%d, want 422", resp.StatusCode)
	}
}

// ---- CreateQuest ----------------------------------------------------------

func TestCreateQuestRoute_HappyPathAndPlanHashForwarded(t *testing.T) {
	t.Parallel()
	b := &stubBackend{createResult: "q_abcd1234"}
	ts := newRoutedServer(t, b, "usr_op")
	body := `{"goal":{"venue_query":{"kind":"slug","slug":"x","city":"ny"},"date":"2026-06-10","party":2,"time_prefs":{"start":"19:00","end":"21:00"},"account_id":"a","constraints":{}},"plan_hash":"sha256:pin","idempotency_key":"key-1"}`
	resp := doJSON(t, ts, "POST", "/v1/quests", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["id"] != "q_abcd1234" {
		t.Errorf("id: %q", got["id"])
	}
	if b.lastCreateOpts.PlanHash == nil || *b.lastCreateOpts.PlanHash != "sha256:pin" {
		t.Errorf("PlanHash not forwarded: %+v", b.lastCreateOpts.PlanHash)
	}
	if b.lastCreateOpts.IdempotencyKey == nil || *b.lastCreateOpts.IdempotencyKey != "key-1" {
		t.Errorf("IdempotencyKey not forwarded: %+v", b.lastCreateOpts.IdempotencyKey)
	}
}

func TestCreateQuestRoute_PlanHashMismatchMapsTo422(t *testing.T) {
	t.Parallel()
	b := &stubBackend{createErr: service.ErrInvalidPlanHash}
	ts := newRoutedServer(t, b, "usr_op")
	body := `{"goal":{"venue_query":{"kind":"slug","slug":"x","city":"ny"},"date":"2026-06-10","party":2,"time_prefs":{"start":"19:00","end":"21:00"},"account_id":"a","constraints":{}},"plan_hash":"sha256:stale"}`
	resp := doJSON(t, ts, "POST", "/v1/quests", body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusUnprocessableEntity {
		t.Errorf("status=%d, want 422", resp.StatusCode)
	}
	var env transport.Envelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Code != "plan_hash_mismatch" {
		t.Errorf("code=%q, want plan_hash_mismatch", env.Code)
	}
}

// ---- ListQuests -----------------------------------------------------------

func TestListQuestsRoute_FiltersForwarded(t *testing.T) {
	t.Parallel()
	b := &stubBackend{listResult: []service.QuestSummary{
		{ID: "q_1", UserID: "usr_op", AccountID: "a", Status: domain.StatusSubmitted, CreatedAt: time.Now()},
		{ID: "q_2", UserID: "usr_op", AccountID: "a", Status: domain.StatusBooked, CreatedAt: time.Now()},
	}}
	ts := newRoutedServer(t, b, "usr_op")
	resp := doJSON(t, ts, "GET", "/v1/quests?status=submitted,booked&account_id=a&limit=10&since=2026-01-01T00:00:00Z", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := b.lastListFilter; len(got.Status) != 2 || got.Limit != 10 || got.AccountID == nil || *got.AccountID != "a" || got.Since == nil {
		t.Errorf("forwarded filter: %+v", got)
	}
}

func TestListQuestsRoute_UnknownStatusReturns422(t *testing.T) {
	t.Parallel()
	b := &stubBackend{}
	ts := newRoutedServer(t, b, "usr_op")
	resp := doJSON(t, ts, "GET", "/v1/quests?status=lunching", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusUnprocessableEntity {
		t.Errorf("status=%d, want 422", resp.StatusCode)
	}
}

// ---- GetQuest -------------------------------------------------------------

func TestGetQuestRoute_NotFoundIncludesQuestID(t *testing.T) {
	t.Parallel()
	b := &stubBackend{getErr: service.ErrNotFound}
	ts := newRoutedServer(t, b, "usr_op")
	resp := doJSON(t, ts, "GET", "/v1/quests/q_missing", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
	var env transport.Envelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Details["quest_id"] != "q_missing" {
		t.Errorf("details.quest_id=%v", env.Details["quest_id"])
	}
}

// ---- CancelQuest ----------------------------------------------------------

func TestCancelQuestRoute_HappyPathReturns204(t *testing.T) {
	t.Parallel()
	b := &stubBackend{}
	ts := newRoutedServer(t, b, "usr_op")
	resp := doJSON(t, ts, "DELETE", "/v1/quests/q_xyz", `{"reason":"user changed plans"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusNoContent {
		t.Errorf("status=%d, want 204", resp.StatusCode)
	}
	if b.lastCancelQID != "q_xyz" || b.lastCancelOpts.Reason != "user changed plans" {
		t.Errorf("forwarded: qid=%s reason=%q", b.lastCancelQID, b.lastCancelOpts.Reason)
	}
}

func TestCancelQuestRoute_EmptyBodyIsOK(t *testing.T) {
	t.Parallel()
	b := &stubBackend{}
	ts := newRoutedServer(t, b, "usr_op")
	resp := doJSON(t, ts, "DELETE", "/v1/quests/q_xyz", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusNoContent {
		t.Errorf("status=%d, want 204", resp.StatusCode)
	}
	if b.lastCancelOpts.Reason != "" {
		t.Errorf("expected empty reason, got %q", b.lastCancelOpts.Reason)
	}
}

// ---- Login ----------------------------------------------------------------

func TestLoginRoute_HappyPath(t *testing.T) {
	t.Parallel()
	b := &stubBackend{loginResult: "acct_aaa"}
	ts := newRoutedServer(t, b, "usr_op")
	resp := doJSON(t, ts, "POST", "/v1/accounts/login", `{"email":"x@y.z","password":"hunter2"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusCreated {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got["account_id"] != "acct_aaa" {
		t.Errorf("account_id=%q", got["account_id"])
	}
	if b.lastLogin.email != "x@y.z" || b.lastLogin.password != "hunter2" {
		t.Errorf("forwarded creds: %+v", b.lastLogin)
	}
}

func TestLoginRoute_MissingPasswordReturns422(t *testing.T) {
	t.Parallel()
	b := &stubBackend{}
	ts := newRoutedServer(t, b, "usr_op")
	resp := doJSON(t, ts, "POST", "/v1/accounts/login", `{"email":"x@y.z"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusUnprocessableEntity {
		t.Errorf("status=%d, want 422", resp.StatusCode)
	}
}

// ---- ListAccounts ---------------------------------------------------------

func TestListAccountsRoute_OmitsNullDisabledAt(t *testing.T) {
	t.Parallel()
	disabled := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := &stubBackend{listAccts: []service.Account{
		{ID: "a_live", UserID: "usr_op", Email: "live@e.x", CreatedAt: time.Now()},
		{ID: "a_dead", UserID: "usr_op", Email: "dead@e.x", CreatedAt: time.Now(), DisabledAt: &disabled},
	}}
	ts := newRoutedServer(t, b, "usr_op")
	resp := doJSON(t, ts, "GET", "/v1/accounts", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got struct {
		Accounts []map[string]any `json:"accounts"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Accounts) != 2 {
		t.Fatalf("len=%d", len(got.Accounts))
	}
	if _, has := got.Accounts[0]["disabled_at"]; has {
		t.Errorf("row 0 leaked nil disabled_at: %+v", got.Accounts[0])
	}
	if got.Accounts[1]["disabled_at"] == nil {
		t.Errorf("row 1 disabled_at: nil, want set")
	}
}
