package resy_test

// integration_test.go gathers httptest-backed end-to-end coverage of every
// providers.* sentinel error class through the Client's public surface
// (Login / Find / Calendar / Book). Per-method test files in this
// package already exercise the happy paths and most failure modes;
// these tests close the remaining gaps so every sentinel from
// providers.* has at least one E2E test through the typical caller
// path AND the test name makes the contract obvious.
//
// Sentinel coverage map (post-R6):
//   ErrAuthExpired       — auth_test.go (login 401, ping 401), and
//                          calendar_test.go's classifier-mapping test.
//   ErrMFARequired       — auth_test.go + TestResyEndToEndAuthMFARequired.
//   ErrBookTokenExpired  — book_test.go + TestResyEndToEndBookTokenExpired.
//   ErrSlotTaken         — book_test.go + TestResyEndToEndSlotTaken.
//   ErrRateLimited       — TestResyEndToEndRateLimited (Find / Book / Calendar).
//   ErrAntiBotChallenge  — TestResyEndToEndAntiBotChallenge (Find).
//   ErrInventoryEmpty    — book_test.go + TestResyEndToEndFindEmpty.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resy"
)

// newE2EClient is the standard Client factory the end-to-end tests use.
// It mirrors the per-method test helpers (newAuthClient, newBookClient,
// newCalendarClient) but takes an explicit Clock so tests that pin
// time-sensitive behavior can pass clock.NewFake.
func newE2EClient(t *testing.T, srv *httptest.Server, c clock.Clock, store resy.SessionStore) *resy.Client {
	t.Helper()
	opts := []resy.Option{
		resy.WithBaseURL(srv.URL),
		resy.WithAPIKey("test"),
		resy.WithHTTPClient(srv.Client()),
		resy.WithUserAgent("test"),
	}
	if store != nil {
		opts = append(opts, resy.WithStore(store))
	}
	return resy.NewClient(slog.New(slog.DiscardHandler), c, opts...)
}

// e2eMakeJWT is integration_test.go's local copy of the JWT helper. The
// auth_test.go file's makeJWT is package-private to that file (Go test
// files in the same package share helpers, but we keep this local to
// keep integration_test.go drop-in-readable).
func e2eMakeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	type claims struct {
		Exp int64 `json:"exp"`
	}
	payload, err := json.Marshal(claims{Exp: exp.Unix()})
	if err != nil {
		t.Fatalf("marshal jwt claims: %v", err)
	}
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(payload) + ".sig"
}

// e2eSampleSlot mirrors book_test.go's sampleSlot but is local so the
// tests in this file are self-contained and obvious to read.
func e2eSampleSlot() providers.Slot {
	return providers.Slot{
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		Time:      domain.NewWallTime(19, 30, 0),
		TableType: "Bar",
		PartySize: 2,
		Payload: domain.ResySlotPayload{
			ConfigID:    "rgs://resy/cal/v2/abc",
			TemplateID:  "999",
			SeatingType: "Bar",
		},
	}
}

// e2eMemSessionStore is an in-memory SessionStore so the login tests
// can assert persistence happened without depending on auth_test.go's
// memSessionStore (same-package test files share types, but the
// duplication here makes the integration suite read independently).
type e2eMemSessionStore struct {
	row *resy.SessionRow
}

func (m *e2eMemSessionStore) UpsertSession(_ context.Context, s resy.SessionRow) error {
	cp := s
	m.row = &cp
	return nil
}

func (m *e2eMemSessionStore) GetSession(_ context.Context, user domain.UserID, provider domain.ProviderID, now time.Time) (resy.SessionRow, error) {
	if m.row == nil {
		return resy.SessionRow{}, errors.New("memstore: not found")
	}
	if m.row.UserID != user || m.row.Provider != provider {
		return resy.SessionRow{}, errors.New("memstore: not found")
	}
	if !m.row.ExpiresAt.IsZero() && !m.row.ExpiresAt.After(now) {
		return *m.row, errors.New("memstore: expired")
	}
	return *m.row, nil
}

// TestResyEndToEndLoginHappyPath drives Login → store persistence →
// Ping with the same JWT through a single httptest server and asserts
// the JWT round-trips both ways: the Session carries it, the store
// captures it, and the subsequent Ping presents it.
func TestResyEndToEndLoginHappyPath(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	exp := now.Add(45 * 24 * time.Hour)
	token := e2eMakeJWT(t, exp)

	var (
		loginHits atomic.Int32
		pingHits  atomic.Int32
		pingAuth  atomic.Value
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/auth/password":
			loginHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":1,"em_address":"u@x.io","token":%q,"payment_method":{"id":42}}`, token)
		case "/2/user":
			pingHits.Add(1)
			pingAuth.Store(r.Header.Get("X-Resy-Universal-Auth"))
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":1}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := &e2eMemSessionStore{}
	c := newE2EClient(t, srv, clock.NewFake(now), store)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	sess, err := c.Login(ctx, resy.Credentials{Email: "u@x.io", Password: "pw"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if sess.JWT() != token {
		t.Errorf("session jwt mismatch")
	}
	if !sess.ExpiresAt().Equal(exp) {
		t.Errorf("session exp: got %v want %v", sess.ExpiresAt(), exp)
	}
	if loginHits.Load() != 1 {
		t.Errorf("login hits: %d", loginHits.Load())
	}

	row, err := store.GetSession(ctx, "u@x.io", "resy", now)
	if err != nil {
		t.Fatalf("store.GetSession: %v", err)
	}
	if row.JWT != token {
		t.Errorf("store jwt mismatch")
	}

	// Reuse the session for Ping; the same JWT must travel on the wire.
	if err := c.Ping(ctx, sess); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if pingHits.Load() != 1 {
		t.Errorf("ping hits: %d", pingHits.Load())
	}
	if v, _ := pingAuth.Load().(string); v != token {
		t.Errorf("ping auth header: got %q want %q", v, token)
	}
}

// TestResyEndToEndAuthMFARequired asserts Login surfaces *MFAError with
// the challenge token from the response, and that errors.Is bridges to
// providers.ErrMFARequired so engine code can branch generically.
func TestResyEndToEndAuthMFARequired(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"mfa_required":true,"challenge_id":"chal-9001","challenge_type":"sms"}`)
	}))
	defer srv.Close()

	c := newE2EClient(t, srv, clock.NewReal(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.Login(ctx, resy.Credentials{Email: "u@x.io", Password: "pw"})
	if !errors.Is(err, providers.ErrMFARequired) {
		t.Fatalf("err = %v, want providers.ErrMFARequired", err)
	}
	var mfa *resy.MFAError
	if !errors.As(err, &mfa) {
		t.Fatalf("err is not *MFAError: %v", err)
	}
	if mfa.Challenge != "chal-9001" {
		t.Errorf("challenge token: got %q want %q", mfa.Challenge, "chal-9001")
	}
}

// TestResyEndToEndFindEmpty asserts that a /4/find response with zero
// venues surfaces providers.ErrInventoryEmpty.
func TestResyEndToEndFindEmpty(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":{"venues":[]}}`)
	}))
	defer srv.Close()

	c := newE2EClient(t, srv, clock.NewReal(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.Find(ctx, providers.FindRequest{
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
	})
	if !errors.Is(err, providers.ErrInventoryEmpty) {
		t.Fatalf("err = %v, want providers.ErrInventoryEmpty", err)
	}
}

// TestResyEndToEndFindMultipleSlots asserts Find returns one Slot per
// /4/find slot in array order, with Time / TableType / Payload pulled
// from the right JSON fields.
func TestResyEndToEndFindMultipleSlots(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"results":{"venues":[{
				"venue":{"id":38660,"name":"Carbone"},
				"slots":[
					{"config":{"id":111,"token":"tok-A","type":"Bar"},"date":{"start":"2026-06-01 18:00:00","end":"2026-06-01 19:30:00"}},
					{"config":{"id":222,"token":"tok-B","type":"Dining Room"},"date":{"start":"2026-06-01 19:30:00","end":"2026-06-01 21:00:00"}},
					{"config":{"id":333,"token":"tok-C","type":"Patio"},"date":{"start":"2026-06-01 21:00:00","end":"2026-06-01 22:30:00"}}
				]
			}]}
		}`)
	}))
	defer srv.Close()

	c := newE2EClient(t, srv, clock.NewReal(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	slots, err := c.Find(ctx, providers.FindRequest{
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(slots) != 3 {
		t.Fatalf("slots: got %d want 3", len(slots))
	}

	want := []struct {
		hour      int
		minute    int
		tableType string
		configID  string
	}{
		{18, 0, "Bar", "tok-A"},
		{19, 30, "Dining Room", "tok-B"},
		{21, 0, "Patio", "tok-C"},
	}
	for i, w := range want {
		if slots[i].Time != domain.NewWallTime(w.hour, w.minute, 0) {
			t.Errorf("slot[%d] time: %v, want %02d:%02d", i, slots[i].Time, w.hour, w.minute)
		}
		if slots[i].TableType != w.tableType {
			t.Errorf("slot[%d] type: %q want %q", i, slots[i].TableType, w.tableType)
		}
		p, ok := slots[i].Payload.(domain.ResySlotPayload)
		if !ok {
			t.Errorf("slot[%d] payload type: %T", i, slots[i].Payload)
			continue
		}
		if p.ConfigID != w.configID {
			t.Errorf("slot[%d] config_id: %q want %q", i, p.ConfigID, w.configID)
		}
	}
}

// TestResyEndToEndBookHappyPath drives find→details→book and asserts
// the resy_token round-trips into Confirmation.ID. It also asserts the
// /3/details book_token rides into the /3/book POST body so the
// pipeline's data flow is observable in one place.
func TestResyEndToEndBookHappyPath(t *testing.T) {
	t.Parallel()

	var seenBookForm atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/details":
			_, _ = io.WriteString(w, `{
				"book_token":{"value":"detail-tok-abc"},
				"user":{"payment_methods":[{"id":42,"display":"Visa ending 0000"}]}
			}`)
		case "/3/book":
			body, _ := io.ReadAll(r.Body)
			seenBookForm.Store(string(body))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"resy_token":"final-token-xyz","reservation_id":98765}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newE2EClient(t, srv, clock.NewReal(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	sess := resy.NewSessionForTest("u@x.io", "jwt-token", time.Now().Add(time.Hour))
	conf, err := c.Book(ctx, e2eSampleSlot(), sess)
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	if conf.ID != "final-token-xyz" {
		t.Errorf("Confirmation.ID: %q want %q", conf.ID, "final-token-xyz")
	}
	body, _ := seenBookForm.Load().(string)
	if !strings.Contains(body, "book_token=detail-tok-abc") {
		t.Errorf("book POST body did not carry book_token: %s", body)
	}
}

// TestResyEndToEndBookTokenExpired asserts Book surfaces
// providers.ErrBookTokenExpired when /3/details returns 410 with
// specific_code BOOK_TOKEN_EXPIRED.
func TestResyEndToEndBookTokenExpired(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/3/details" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `{"specific_code":"BOOK_TOKEN_EXPIRED","message":"book token expired"}`)
	}))
	defer srv.Close()

	c := newE2EClient(t, srv, clock.NewReal(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	sess := resy.NewSessionForTest("u@x.io", "jwt", time.Now().Add(time.Hour))
	_, err := c.Book(ctx, e2eSampleSlot(), sess)
	if !errors.Is(err, providers.ErrBookTokenExpired) {
		t.Fatalf("err = %v, want ErrBookTokenExpired", err)
	}
}

// TestResyEndToEndSlotTaken asserts Book surfaces providers.ErrSlotTaken
// when /3/book returns 409.
func TestResyEndToEndSlotTaken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/details":
			_, _ = io.WriteString(w, `{
				"book_token":{"value":"tok"},
				"user":{"payment_methods":[{"id":42}]}
			}`)
		case "/3/book":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"specific_code":"SLOT_NOT_AVAILABLE"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newE2EClient(t, srv, clock.NewReal(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	sess := resy.NewSessionForTest("u@x.io", "jwt", time.Now().Add(time.Hour))
	_, err := c.Book(ctx, e2eSampleSlot(), sess)
	if !errors.Is(err, providers.ErrSlotTaken) {
		t.Fatalf("err = %v, want ErrSlotTaken", err)
	}
}

// TestResyEndToEndRateLimited asserts that a 429 response from any of
// the three GET-style endpoints (Find, Calendar, Book's /3/details)
// surfaces providers.ErrRateLimited. Each subtest is a fresh server so
// they exercise the classifier through three distinct call paths.
func TestResyEndToEndRateLimited(t *testing.T) {
	t.Parallel()

	rateLimit := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"rate limit exceeded"}`)
	}

	t.Run("find", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(rateLimit))
		defer srv.Close()
		c := newE2EClient(t, srv, clock.NewReal(), nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := c.Find(ctx, providers.FindRequest{
			Venue:     domain.VenueRef{Provider: "resy", Ref: "1"},
			Date:      domain.NewDate(2026, time.June, 1),
			PartySize: 2,
		})
		if !errors.Is(err, providers.ErrRateLimited) {
			t.Fatalf("Find err = %v, want ErrRateLimited", err)
		}
	})

	t.Run("calendar", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(rateLimit))
		defer srv.Close()
		c := newE2EClient(t, srv, clock.NewReal(), nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := c.Calendar(ctx,
			domain.VenueRef{Provider: "resy", Ref: "1"},
			providers.DateRange{
				Start: domain.NewDate(2026, time.June, 1),
				End:   domain.NewDate(2026, time.June, 7),
			},
		)
		if !errors.Is(err, providers.ErrRateLimited) {
			t.Fatalf("Calendar err = %v, want ErrRateLimited", err)
		}
	})

	t.Run("book", func(t *testing.T) {
		t.Parallel()
		// 429 on /3/details — the first leg of Book — is the most
		// likely place a rate-limit lands during the find→details→book
		// pipeline.
		srv := httptest.NewServer(http.HandlerFunc(rateLimit))
		defer srv.Close()
		c := newE2EClient(t, srv, clock.NewReal(), nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		sess := resy.NewSessionForTest("u@x.io", "jwt", time.Now().Add(time.Hour))
		_, err := c.Book(ctx, e2eSampleSlot(), sess)
		if !errors.Is(err, providers.ErrRateLimited) {
			t.Fatalf("Book err = %v, want ErrRateLimited", err)
		}
	})
}

// TestResyEndToEndAntiBotChallenge asserts that a 403 response carrying
// an anti-bot marker (captcha / px-captcha / challenge / blocked)
// surfaces providers.ErrAntiBotChallenge — distinct from the 403
// "permission denied" path which falls through to ErrAuthExpired.
func TestResyEndToEndAntiBotChallenge(t *testing.T) {
	t.Parallel()

	bodies := []struct {
		name string
		body string
	}{
		{"captcha marker", `{"message":"please solve the captcha"}`},
		{"px-captcha marker", `<html>px-captcha challenge required</html>`},
		{"blocked marker", `{"message":"request blocked by perimeterx"}`},
	}
	for _, tc := range bodies {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := newE2EClient(t, srv, clock.NewReal(), nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := c.Find(ctx, providers.FindRequest{
				Venue:     domain.VenueRef{Provider: "resy", Ref: "1"},
				Date:      domain.NewDate(2026, time.June, 1),
				PartySize: 2,
			})
			if !errors.Is(err, providers.ErrAntiBotChallenge) {
				t.Fatalf("err = %v, want ErrAntiBotChallenge", err)
			}
		})
	}
}
