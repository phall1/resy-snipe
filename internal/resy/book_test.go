package resy_test

import (
	"context"
	"errors"
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

func newBookClient(t *testing.T, srv *httptest.Server) *resy.Client {
	t.Helper()
	return resy.NewClient(slog.New(slog.DiscardHandler), clock.NewReal(),
		resy.WithBaseURL(srv.URL),
		resy.WithAPIKey("test"),
		resy.WithHTTPClient(srv.Client()),
		resy.WithUserAgent("test"),
	)
}

func sampleSlot() providers.Slot {
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

func sessionWithExp(exp time.Time) *resy.Session {
	return resy.NewSessionForTest("u@x.io", "jwt-token", exp)
}

func TestFindParsesSlots(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"results":{"venues":[{
				"venue":{"id":38660,"name":"Carbone"},
				"slots":[
					{"config":{"id":111,"token":"tok-A","type":"Bar"},"date":{"start":"2026-06-01 19:30:00","end":"2026-06-01 21:00:00"}},
					{"config":{"id":222,"token":"tok-B","type":"Dining Room"},"date":{"start":"2026-06-01 20:00:00","end":"2026-06-01 21:30:00"}}
				]
			}]}
		}`)
	}))
	defer srv.Close()

	c := newBookClient(t, srv)
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
	if len(slots) != 2 {
		t.Fatalf("expected 2 slots, got %d", len(slots))
	}
	if slots[0].Time != domain.NewWallTime(19, 30, 0) {
		t.Errorf("slot[0] time: %v", slots[0].Time)
	}
	if slots[1].Time != domain.NewWallTime(20, 0, 0) {
		t.Errorf("slot[1] time: %v", slots[1].Time)
	}
	if p, ok := slots[0].Payload.(domain.ResySlotPayload); !ok || p.ConfigID != "tok-A" {
		t.Errorf("slot[0] payload: %+v", slots[0].Payload)
	}
	if slots[0].Date != domain.NewDate(2026, time.June, 1) {
		t.Errorf("slot[0] date: %v", slots[0].Date)
	}
}

func TestFindEmptyVenuesIsInventoryEmpty(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":{"venues":[]}}`)
	}))
	defer srv.Close()

	c := newBookClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.Find(ctx, providers.FindRequest{
		Venue:     domain.VenueRef{Provider: "resy", Ref: "1"},
		Date:      domain.NewDate(2026, 6, 1),
		PartySize: 2,
	})
	if !errors.Is(err, providers.ErrInventoryEmpty) {
		t.Fatalf("err = %v, want ErrInventoryEmpty", err)
	}
}

func TestFindRejectsBadInputs(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)
	c := newBookClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)

	cases := []struct {
		name string
		req  providers.FindRequest
	}{
		{"wrong provider", providers.FindRequest{Venue: domain.VenueRef{Provider: "x", Ref: "1"}, Date: domain.NewDate(2026, 6, 1), PartySize: 2}},
		{"empty ref", providers.FindRequest{Venue: domain.VenueRef{Provider: "resy"}, Date: domain.NewDate(2026, 6, 1), PartySize: 2}},
		{"missing date", providers.FindRequest{Venue: domain.VenueRef{Provider: "resy", Ref: "1"}, PartySize: 2}},
		{"zero party", providers.FindRequest{Venue: domain.VenueRef{Provider: "resy", Ref: "1"}, Date: domain.NewDate(2026, 6, 1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := c.Find(ctx, tc.req); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestBookFullPipeline(t *testing.T) {
	t.Parallel()
	var detailsHits, bookHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/details":
			detailsHits.Add(1)
			_, _ = io.WriteString(w, `{
				"book_token":{"value":"book-tok-xyz"},
				"user":{"payment_methods":[{"id":42,"display":"Visa ending 0000"}]}
			}`)
		case "/3/book":
			bookHits.Add(1)
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "book_token=book-tok-xyz") {
				t.Errorf("book POST body missing token: %s", body)
			}
			if !strings.Contains(string(body), "struct_payment_method") {
				t.Errorf("book POST body missing payment method: %s", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"resy_token":"final-tok","reservation_id":98765}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newBookClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	conf, err := c.Book(ctx, sampleSlot(), sessionWithExp(time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	if conf.ID != "final-tok" {
		t.Errorf("confirmation id: %s", conf.ID)
	}
	if detailsHits.Load() != 1 || bookHits.Load() != 1 {
		t.Errorf("hit counts: details=%d book=%d", detailsHits.Load(), bookHits.Load())
	}
}

func TestBookSurfacesBookTokenExpired(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/details":
			w.WriteHeader(http.StatusGone)
			_, _ = io.WriteString(w, `{"specific_code":"BOOK_TOKEN_EXPIRED"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newBookClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.Book(ctx, sampleSlot(), sessionWithExp(time.Now().Add(time.Hour)))
	if !errors.Is(err, providers.ErrBookTokenExpired) {
		t.Fatalf("err = %v, want ErrBookTokenExpired", err)
	}
}

func TestBookSurfacesSlotTakenOnBookCall(t *testing.T) {
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
	c := newBookClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.Book(ctx, sampleSlot(), sessionWithExp(time.Now().Add(time.Hour)))
	if !errors.Is(err, providers.ErrSlotTaken) {
		t.Fatalf("err = %v, want ErrSlotTaken", err)
	}
}

func TestBookRejectsMissingPaymentMethod(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"book_token":{"value":"tok"},
			"user":{"payment_methods":[]}
		}`)
	}))
	defer srv.Close()
	c := newBookClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.Book(ctx, sampleSlot(), sessionWithExp(time.Now().Add(time.Hour)))
	if err == nil {
		t.Fatal("expected error on missing payment method")
	}
	if !strings.Contains(err.Error(), "payment method") {
		t.Errorf("err: %v", err)
	}
}

func TestBookWithIdempotencySendsHeaderAndRefusesRepeat(t *testing.T) {
	t.Parallel()
	var seenHeaderValue atomic.Value
	var bookHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/details":
			_, _ = io.WriteString(w, `{
				"book_token":{"value":"tok"},
				"user":{"payment_methods":[{"id":42}]}
			}`)
		case "/3/book":
			bookHits.Add(1)
			seenHeaderValue.Store(r.Header.Get(resy.IdempotencyKeyHeader))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"resy_token":"tok","reservation_id":1}`)
		}
	}))
	defer srv.Close()
	c := newBookClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	const key = "deadbeef-cafef00d"

	if _, err := c.BookWithIdempotency(ctx, sampleSlot(),
		sessionWithExp(time.Now().Add(time.Hour)), key); err != nil {
		t.Fatalf("first Book: %v", err)
	}
	if v, _ := seenHeaderValue.Load().(string); v != key {
		t.Errorf("header missing/wrong: %q", v)
	}

	// Second send with same key must be refused without burning a /3/book.
	_, err := c.BookWithIdempotency(ctx, sampleSlot(),
		sessionWithExp(time.Now().Add(time.Hour)), key)
	if err == nil {
		t.Fatal("second Book with same key must error")
	}
	if !strings.Contains(err.Error(), "already sent") {
		t.Errorf("err: %v", err)
	}
	if bookHits.Load() != 1 {
		t.Errorf("/3/book hit count: %d, want 1", bookHits.Load())
	}
}

func TestBookRejectsWrongSessionType(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := newBookClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type fakeSession struct{ providers.Session }
	if _, err := c.Book(ctx, sampleSlot(), fakeSession{}); err == nil {
		t.Fatal("expected wrong-session-type error")
	}
}
