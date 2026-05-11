package httpclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	httpclient "resy-snipe/internal/client/http"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// newClient wires a test client against the given test server. The
// caller supplies the token; New requires an http or https URL, which
// httptest.NewServer always provides.
func newClient(t *testing.T, srv *httptest.Server, token string) *httpclient.Client {
	t.Helper()
	c, err := httpclient.New(srv.URL, token,
		httpclient.WithHTTPClient(srv.Client()),
		httpclient.WithUserAgent("test-agent/1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNew_ValidatesURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"bad-scheme", "ftp://example"},
		{"no-host", "http://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := httpclient.New(tc.url, ""); err == nil {
				t.Fatalf("New(%q) returned no error", tc.url)
			}
		})
	}
}

func TestClient_BearerHeader(t *testing.T) {
	t.Parallel()
	var seen atomic.Value
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		seen.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accounts":[]}`))
	}))
	defer srv.Close()
	c := newClient(t, srv, "sekret")
	if _, err := c.ListAccounts(context.Background()); err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	got, _ := seen.Load().(string)
	if got != "Bearer sekret" {
		t.Fatalf("Authorization header=%q, want %q", got, "Bearer sekret")
	}
}

func TestClient_IssueToken_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.Method != stdhttp.MethodPost || r.URL.Path != "/v1/auth/tokens" {
			stdhttp.Error(w, "wrong route", stdhttp.StatusNotFound)
			return
		}
		var req struct{ Label, Scope string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stdhttp.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":"tok_1","token":"plain","label":%q,"scope":%q,"created_at":"2026-05-10T12:00:00Z"}`, req.Label, req.Scope)
	}))
	defer srv.Close()
	c := newClient(t, srv, "op-token")
	bt, err := c.IssueToken(context.Background(), "laptop", "operator")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if bt.ID != "tok_1" || bt.Token != "plain" || bt.Label != "laptop" || bt.Scope != "operator" {
		t.Fatalf("BearerToken = %+v", bt)
	}
	if bt.CreatedAt.IsZero() {
		t.Errorf("CreatedAt should be set")
	}
}

func TestClient_ResolveVenue_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.URL.Path != "/v1/venues/resolve" || r.Method != stdhttp.MethodPost {
			stdhttp.Error(w, "wrong route", stdhttp.StatusNotFound)
			return
		}
		var req struct {
			VenueQuery struct {
				Kind, URL string
			} `json:"venue_query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.VenueQuery.Kind != "url" || req.VenueQuery.URL == "" {
			stdhttp.Error(w, "bad payload", stdhttp.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider":"resy","ref":"123","name":"Carbone","tz":"America/New_York"}`))
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	v, err := c.ResolveVenue(context.Background(), domain.VenueQueryURL{URL: "https://resy.com/cities/ny/carbone"})
	if err != nil {
		t.Fatalf("ResolveVenue: %v", err)
	}
	if v.Provider != "resy" || v.Ref != "123" || v.Name != "Carbone" || v.TZ.String() != "America/New_York" {
		t.Fatalf("Venue = %+v", v)
	}
}

// canonicalGoal returns a Goal that round-trips through encode/decode.
func canonicalGoal() domain.Goal {
	return domain.Goal{
		VenueQuery: domain.VenueQuerySlug{Slug: "carbone", City: "ny"},
		Date:       domain.NewDate(2026, time.June, 1),
		Party:      2,
		TimePrefs: domain.TimeWindow{
			Start:    domain.NewWallTime(18, 0, 0),
			End:      domain.NewWallTime(20, 30, 0),
			Priority: domain.PriorityEarlier,
		},
		AccountID:   "acct_1",
		Constraints: domain.Constraints{TableTypes: []string{"indoor"}},
	}
}

func TestClient_PlanQuest_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.URL.Path != "/v1/quests/plan" {
			stdhttp.Error(w, "wrong route", stdhttp.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan":{
			"goal":{"venue_query":{"kind":"slug","slug":"carbone","city":"ny"},"date":"2026-06-01","party":2,
				"time_prefs":{"start":"18:00:00","end":"20:30:00","priority":"earlier"},
				"account_id":"acct_1","constraints":{"table_types":["indoor"]}},
			"venue":{"provider":"resy","ref":"123","name":"Carbone","tz":"America/New_York"},
			"drop_moment":"2026-05-08T14:00:00Z",
			"strategy":{"tag":"explicit","at":"2026-05-08T14:00:00Z"},
			"fire_schedule":["2026-05-08T14:00:00Z"],
			"signing_required":true,
			"hash":"sha256:abc",
			"plan_hash_version":1,
			"computed_at":"2026-05-07T12:00:00Z"
		}}`))
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	plan, err := c.PlanQuest(context.Background(), canonicalGoal())
	if err != nil {
		t.Fatalf("PlanQuest: %v", err)
	}
	if plan.Hash != "sha256:abc" {
		t.Errorf("plan.Hash = %q, want sha256:abc", plan.Hash)
	}
	if plan.Venue.TZ == nil || plan.Venue.TZ.String() != "America/New_York" {
		t.Errorf("plan.Venue.TZ = %v", plan.Venue.TZ)
	}
	if _, ok := plan.Strategy.(domain.ExplicitRelease); !ok {
		t.Errorf("plan.Strategy = %T, want ExplicitRelease", plan.Strategy)
	}
}

func TestClient_CreateQuest_HappyPath(t *testing.T) {
	t.Parallel()
	hash := "sha256:abc"
	idem := "k1"
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.URL.Path != "/v1/quests" || r.Method != stdhttp.MethodPost {
			stdhttp.Error(w, "wrong route", stdhttp.StatusNotFound)
			return
		}
		var req struct {
			Goal           map[string]any `json:"goal"`
			PlanHash       *string        `json:"plan_hash"`
			IdempotencyKey *string        `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			stdhttp.Error(w, err.Error(), stdhttp.StatusBadRequest)
			return
		}
		if req.PlanHash == nil || *req.PlanHash != hash {
			stdhttp.Error(w, "missing plan_hash", stdhttp.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stdhttp.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"q_42"}`))
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	id, err := c.CreateQuest(context.Background(), canonicalGoal(), httpclient.CreateOpts{
		PlanHash:       &hash,
		IdempotencyKey: &idem,
	})
	if err != nil {
		t.Fatalf("CreateQuest: %v", err)
	}
	if id != "q_42" {
		t.Fatalf("id = %q, want q_42", id)
	}
}

func TestClient_GetQuest_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.URL.Path != "/v1/quests/q_42" {
			stdhttp.Error(w, "wrong route", stdhttp.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"summary":{"id":"q_42","user_id":"u1","account_id":"acct_1","status":"submitted",
				"created_at":"2026-05-10T12:00:00Z","updated_at":"2026-05-10T12:00:00Z"},
			"goal":{"venue_query":{"kind":"slug","slug":"carbone","city":"ny"},"date":"2026-06-01","party":2,
				"time_prefs":{"start":"18:00:00","end":"20:30:00","priority":"earlier"},
				"account_id":"acct_1","constraints":{}},
			"plan":{"goal":{"venue_query":{"kind":"slug","slug":"carbone","city":"ny"},"date":"2026-06-01","party":2,
					"time_prefs":{"start":"18:00:00","end":"20:30:00","priority":"earlier"},
					"account_id":"acct_1","constraints":{}},
				"venue":{"provider":"resy","ref":"123","tz":"America/New_York"},
				"drop_moment":"2026-05-08T14:00:00Z",
				"strategy":{"tag":"explicit","at":"2026-05-08T14:00:00Z"},
				"signing_required":false,
				"plan_hash_version":1,
				"computed_at":"2026-05-07T12:00:00Z"},
			"events":[{"type":"submitted","at":"2026-05-10T12:00:00Z","attrs":{"reason":"ok"}}]
		}`))
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	state, err := c.GetQuest(context.Background(), "q_42")
	if err != nil {
		t.Fatalf("GetQuest: %v", err)
	}
	if state.Summary.ID != "q_42" || state.Summary.Status != domain.StatusSubmitted {
		t.Errorf("summary = %+v", state.Summary)
	}
	if len(state.Events) != 1 || state.Events[0].Type != domain.EventSubmitted {
		t.Errorf("events = %+v", state.Events)
	}
}

func TestClient_ListQuests_EncodesFilter(t *testing.T) {
	t.Parallel()
	var gotQuery atomic.Value
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		gotQuery.Store(r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"quests":[{"id":"q1","user_id":"u1","account_id":"a1","status":"booked",
			"created_at":"2026-05-10T12:00:00Z","updated_at":"2026-05-10T12:00:00Z"}]}`))
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	aid := domain.AccountID("acct_1")
	rows, err := c.ListQuests(context.Background(), httpclient.ListFilter{
		Status:    []domain.Status{domain.StatusBooked, domain.StatusFailed},
		AccountID: &aid,
		Since:     &since,
		Limit:     10,
		Cursor:    "abc",
	})
	if err != nil {
		t.Fatalf("ListQuests: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "q1" {
		t.Errorf("rows = %+v", rows)
	}
	q, _ := gotQuery.Load().(string)
	parsed, err := url.ParseQuery(q)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if got := parsed.Get("status"); got != "booked,failed" {
		t.Errorf("status=%q, want booked,failed", got)
	}
	if got := parsed.Get("account_id"); got != "acct_1" {
		t.Errorf("account_id=%q", got)
	}
	if got := parsed.Get("limit"); got != "10" {
		t.Errorf("limit=%q", got)
	}
	if got := parsed.Get("cursor"); got != "abc" {
		t.Errorf("cursor=%q", got)
	}
}

func TestClient_CancelQuest_HappyPath(t *testing.T) {
	t.Parallel()
	var seenBody atomic.Value
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.Method != stdhttp.MethodDelete || r.URL.Path != "/v1/quests/q_42" {
			stdhttp.Error(w, "wrong route", stdhttp.StatusNotFound)
			return
		}
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		seenBody.Store(string(buf[:n]))
		w.WriteHeader(stdhttp.StatusNoContent)
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	if err := c.CancelQuest(context.Background(), "q_42", httpclient.CancelOpts{Reason: "user requested"}); err != nil {
		t.Fatalf("CancelQuest: %v", err)
	}
	body, _ := seenBody.Load().(string)
	if !strings.Contains(body, `"reason":"user requested"`) {
		t.Errorf("body = %q, want contains reason", body)
	}
}

func TestClient_ListAccounts_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accounts":[
			{"id":"a1","user_id":"u1","email":"a@b","created_at":"2026-05-10T12:00:00Z"},
			{"id":"a2","user_id":"u1","email":"c@d","display_name":"Work","created_at":"2026-05-10T12:00:00Z"}
		]}`))
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	rows, err := c.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(rows) != 2 || rows[0].Email != "a@b" || rows[1].DisplayName != "Work" {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestClient_ErrorEnvelopeRoundTrip verifies that the server's
// {"code":"not_found",...} envelope decodes to an *Error that satisfies
// errors.Is(err, service.ErrNotFound) and exposes Details.
func TestClient_ErrorEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		status   int
		body     string
		sentinel error
	}{
		{"not_found", 404, `{"code":"not_found","message":"missing","details":{"quest_id":"q_42"}}`, service.ErrNotFound},
		{"unauthenticated", 401, `{"code":"unauthenticated","message":"no token"}`, service.ErrUnauthorized},
		{"forbidden", 403, `{"code":"forbidden","message":"nope"}`, service.ErrForbidden},
		{"plan_hash_mismatch", 422, `{"code":"plan_hash_mismatch","message":"drift"}`, service.ErrInvalidPlanHash},
		{"validation_failed", 422, `{"code":"validation_failed","message":"bad input"}`, service.ErrInvalidArgument},
		{"conflict", 409, `{"code":"conflict","message":"dup"}`, service.ErrConflict},
		{"venue_ambiguous", 409, `{"code":"venue_ambiguous","message":"two"}`, service.ErrVenueAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := newClient(t, srv, "tok")
			_, err := c.GetQuest(context.Background(), "q_42")
			if err == nil {
				t.Fatalf("GetQuest returned no error")
			}
			if !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(err, %v) = false; err = %v", tc.sentinel, err)
			}
			herr := httpclient.AsError(err)
			if herr == nil {
				t.Fatalf("AsError returned nil for err=%v", err)
			}
			if herr.Status != tc.status || herr.Code != tc.name {
				t.Errorf("herr=%+v, want status=%d code=%s", herr, tc.status, tc.name)
			}
		})
	}
}

func TestClient_ErrorEnvelopeDetails(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stdhttp.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"missing","details":{"quest_id":"q_xyz"}}`))
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	_, err := c.GetQuest(context.Background(), "q_xyz")
	if err == nil {
		t.Fatalf("no error")
	}
	herr := httpclient.AsError(err)
	if herr == nil {
		t.Fatalf("AsError = nil")
	}
	qid, _ := herr.Details["quest_id"].(string)
	if qid != "q_xyz" {
		t.Errorf("details.quest_id = %q, want q_xyz", qid)
	}
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("errors.Is ErrNotFound = false")
	}
}

func TestClient_NetworkError_WrapsTransport(t *testing.T) {
	t.Parallel()
	// Start then immediately close the server so the address is dead.
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {}))
	addr := srv.URL
	srv.Close()
	c, err := httpclient.New(addr, "tok",
		httpclient.WithTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.ListAccounts(context.Background())
	if err == nil {
		t.Fatalf("expected error from dead server")
	}
	if !strings.Contains(err.Error(), "transport") {
		t.Errorf("err=%v, want mention of transport", err)
	}
	if httpclient.AsError(err) != nil {
		t.Errorf("AsError should be nil for transport errors; got %v", httpclient.AsError(err))
	}
}

// TestSubscribeQuest_HappyPath: server writes a few framed events
// then EOFs; the client invokes callback in order.
func TestSubscribeQuest_HappyPath(t *testing.T) {
	t.Parallel()
	frames := []string{
		"event: submitted\ndata: {\"type\":\"submitted\",\"at\":\"2026-05-10T12:00:00Z\"}\n\n",
		"event: scheduled\ndata: {\"type\":\"scheduled\",\"at\":\"2026-05-10T12:00:05Z\",\"attrs\":{\"reason\":\"plan\"}}\n\n",
		"event: booked\ndata: {\"type\":\"booked\",\"at\":\"2026-05-10T12:00:10Z\"}\n\n",
	}
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.URL.Path != "/v1/quests/q_42/events" {
			stdhttp.Error(w, "wrong route", stdhttp.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(stdhttp.StatusOK)
		fl, _ := w.(stdhttp.Flusher)
		for _, frame := range frames {
			_, _ = w.Write([]byte(frame))
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	var got []domain.EventType
	err := c.SubscribeQuest(context.Background(), "q_42", func(e domain.Event) {
		got = append(got, e.Type)
	})
	if err != nil {
		t.Fatalf("SubscribeQuest: %v", err)
	}
	want := []domain.EventType{domain.EventSubmitted, domain.EventScheduled, domain.EventBooked}
	if len(got) != len(want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d]=%q want=%q", i, got[i], w)
		}
	}
}

func TestSubscribeQuest_InStreamError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(stdhttp.StatusOK)
		fl, _ := w.(stdhttp.Flusher)
		_, _ = w.Write([]byte("event: submitted\ndata: {\"type\":\"submitted\",\"at\":\"2026-05-10T12:00:00Z\"}\n\n"))
		if fl != nil {
			fl.Flush()
		}
		_, _ = w.Write([]byte("event: error\ndata: \"engine died\"\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	var got []domain.EventType
	err := c.SubscribeQuest(context.Background(), "q_42", func(e domain.Event) {
		got = append(got, e.Type)
	})
	if err == nil {
		t.Fatalf("SubscribeQuest returned no error")
	}
	if !strings.Contains(err.Error(), "engine died") {
		t.Errorf("err=%v, want mention of engine died", err)
	}
	if len(got) != 1 || got[0] != domain.EventSubmitted {
		t.Errorf("got=%v, want [submitted]", got)
	}
}

func TestSubscribeQuest_PreflightErrorReturns(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stdhttp.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"no such quest"}`))
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	called := false
	err := c.SubscribeQuest(context.Background(), "q_42", func(e domain.Event) { called = true })
	if err == nil {
		t.Fatalf("expected error")
	}
	if called {
		t.Errorf("callback should not have fired on preflight 404")
	}
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("errors.Is ErrNotFound = false; err=%v", err)
	}
}

func TestClient_RevokeToken_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.Method != stdhttp.MethodDelete || r.URL.Path != "/v1/auth/tokens/tok_xyz" {
			stdhttp.Error(w, "wrong route", stdhttp.StatusNotFound)
			return
		}
		w.WriteHeader(stdhttp.StatusNoContent)
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	if err := c.RevokeToken(context.Background(), "tok_xyz"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
}

func TestClient_ListTokens_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tokens":[
			{"id":"t1","scope":"user","label":"laptop","created_at":"2026-05-10T12:00:00Z"},
			{"id":"t2","scope":"operator","label":"ops","created_at":"2026-05-10T12:00:00Z","last_seen":"2026-05-10T12:30:00Z"}
		]}`))
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	toks, err := c.ListTokens(context.Background())
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(toks) != 2 || toks[0].ID != "t1" || toks[1].LastSeen == nil {
		t.Fatalf("toks = %+v", toks)
	}
}

func TestClient_Login_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		if r.URL.Path != "/v1/accounts/login" || r.Method != stdhttp.MethodPost {
			stdhttp.Error(w, "wrong route", stdhttp.StatusNotFound)
			return
		}
		var req struct{ Email, Password string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Email == "" || req.Password == "" {
			stdhttp.Error(w, "missing", stdhttp.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stdhttp.StatusCreated)
		_, _ = w.Write([]byte(`{"account_id":"acct_42"}`))
	}))
	defer srv.Close()
	c := newClient(t, srv, "tok")
	aid, err := c.Login(context.Background(), "user@example.com", "secret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if aid != "acct_42" {
		t.Fatalf("aid = %q", aid)
	}
}
