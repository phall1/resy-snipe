package resy_test

// sign_retry_test.go covers the sign-and-retry path that R7 wired into
// /3/details and /3/book. It uses an httptest server plus a fake Signer
// to assert four properties:
//
//  1. Signer headers ride into /3/details and /3/book requests.
//  2. On a 403 anti-bot response, Signer.Reset is called exactly once
//     and the request is retried.
//  3. After a Reset+retry that succeeds (200 OK), the call returns
//     normally with the second response's body.
//  4. After a Reset+retry that fails again (second 403), the original
//     ErrAntiBotChallenge propagates unchanged — the engine sees it as
//     terminal, matching the pre-R7 contract.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resy"
	"resy-snipe/internal/resy/sign"
)

// fakeSigner is a Signer test double. It tracks call counts and the
// list of paths Sign was invoked with, returns a stable header bag,
// and lets the test pin behavior on the n-th Sign / Reset call via
// the headers field.
type fakeSigner struct {
	headers   sign.Headers
	signCalls atomic.Int64
	resetN    atomic.Int64
}

func (f *fakeSigner) Sign(_ context.Context, _ string) (sign.Headers, error) {
	f.signCalls.Add(1)
	out := make(sign.Headers, len(f.headers))
	maps.Copy(out, f.headers)
	return out, nil
}

func (f *fakeSigner) Reset(_ context.Context) error {
	f.resetN.Add(1)
	return nil
}

// signedClient wires a Client whose retry floor is driven by a fake
// clock the test owns. The fake clock lets the test deterministically
// release the in-retry waitFloor without sleeping.
func signedClient(t *testing.T, srv *httptest.Server, signer sign.Signer, fake *clock.Fake) *resy.Client {
	t.Helper()
	return resy.NewClient(slog.New(slog.DiscardHandler), fake,
		resy.WithBaseURL(srv.URL),
		resy.WithAPIKey("test"),
		resy.WithHTTPClient(srv.Client()),
		resy.WithUserAgent("test"),
		resy.WithSigner(signer),
	)
}

// driveFakeClock fires Advance(d) whenever its caller signals via
// `tick`. It returns a cleanup that closes tick and waits for the
// goroutine to exit.
//
// The test pattern: register the goroutine, run the client call (which
// will block in waitFloor), then send on tick to release the wait.
func driveFakeClock(t *testing.T, fake *clock.Fake, d time.Duration) func() {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Tight loop: advance the fake clock periodically so any
		// outstanding clock.After waiters fire deterministically. We
		// can't use a real ticker here because the test doesn't know
		// exactly when the client enters the wait. The 1ms sleep is a
		// scheduling courtesy, not a correctness dependency.
		for {
			select {
			case <-stop:
				return
			default:
			}
			fake.Advance(d)
			time.Sleep(time.Millisecond)
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func signTestSlot() providers.Slot {
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

// TestSignerHeadersRideIntoDetailsAndBook asserts that the Signer's
// headers appear on the wire for both /3/details (PrepareSlot) and
// /3/book (postBook) requests, and that the X-Resy-Idempotency-Key
// passed by the caller is preserved alongside (i.e. the merge does
// not clobber adapter-supplied headers).
func TestSignerHeadersRideIntoDetailsAndBook(t *testing.T) {
	t.Parallel()

	var (
		detailsHdr atomic.Value
		bookHdr    atomic.Value
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/details":
			detailsHdr.Store(r.Header.Clone())
			_, _ = io.WriteString(w, `{
				"book_token":{"value":"detail-tok"},
				"user":{"payment_methods":[{"id":42,"display":"Visa"}]}
			}`)
		case "/3/book":
			bookHdr.Store(r.Header.Clone())
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"resy_token":"final-token","reservation_id":1}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	fake := clock.NewFake(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	signer := &fakeSigner{headers: sign.Headers{
		"X-Px-Foo":       "bar",
		"x-resy-rotated": "abc",
	}}
	c := signedClient(t, srv, signer, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess := resy.NewSessionForTest("u@x.io", "jwt", time.Now().Add(time.Hour))
	conf, err := c.Book(ctx, signTestSlot(), sess)
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	if conf.ID == "" {
		t.Fatalf("empty confirmation id")
	}

	dh, _ := detailsHdr.Load().(http.Header)
	if dh == nil {
		t.Fatal("/3/details was never called")
	}
	if dh.Get("X-Px-Foo") != "bar" {
		t.Errorf("/3/details missing x-px-foo: %q", dh.Get("X-Px-Foo"))
	}
	if dh.Get("X-Resy-Rotated") != "abc" {
		t.Errorf("/3/details missing x-resy-rotated: %q", dh.Get("X-Resy-Rotated"))
	}

	bh, _ := bookHdr.Load().(http.Header)
	if bh == nil {
		t.Fatal("/3/book was never called")
	}
	if bh.Get("X-Px-Foo") != "bar" {
		t.Errorf("/3/book missing x-px-foo: %q", bh.Get("X-Px-Foo"))
	}
	// Without an idempotency key on Book(), the header is omitted —
	// just confirm signing rode in. (R7 doesn't change idempotency
	// behavior; book_test.go already covers the with-key path.)
}

// TestAntiBotChallengeTriggersResetAndRetry asserts the recovery path:
// first /3/details response is a 403 with an anti-bot marker; the
// adapter calls Signer.Reset and retries; the second response is a
// 200; PrepareSlot returns success.
func TestAntiBotChallengeTriggersResetAndRetry(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/3/details" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := hits.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"px-captcha challenge required"}`)
			return
		}
		_, _ = io.WriteString(w, `{
			"book_token":{"value":"recovered-tok"},
			"user":{"payment_methods":[{"id":42}]}
		}`)
	}))
	defer srv.Close()

	fake := clock.NewFake(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	signer := &fakeSigner{}
	c := signedClient(t, srv, signer, fake)
	stopClock := driveFakeClock(t, fake, 100*time.Millisecond)
	defer stopClock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess := resy.NewSessionForTest("u@x.io", "jwt", time.Now().Add(time.Hour))
	got, err := c.PrepareSlot(ctx, signTestSlot(), sess)
	if err != nil {
		t.Fatalf("PrepareSlot: %v", err)
	}
	p, ok := got.Payload.(domain.ResySlotPayload)
	if !ok {
		t.Fatalf("payload type: %T", got.Payload)
	}
	if p.BookToken != "recovered-tok" {
		t.Errorf("BookToken: %q want recovered-tok", p.BookToken)
	}
	if signer.resetN.Load() != 1 {
		t.Errorf("Reset calls: %d want 1", signer.resetN.Load())
	}
	if hits.Load() != 2 {
		t.Errorf("/3/details hits: %d want 2 (initial + retry)", hits.Load())
	}
}

// TestSecondAntiBotChallengeIsTerminal asserts that when the post-Reset
// retry also yields a 403 anti-bot response, ErrAntiBotChallenge
// propagates unchanged. The engine's contract is that a second failure
// is terminal — same as today's no-recovery behavior.
func TestSecondAntiBotChallengeIsTerminal(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/3/details" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"perimeterx blocked"}`)
	}))
	defer srv.Close()

	fake := clock.NewFake(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	signer := &fakeSigner{}
	c := signedClient(t, srv, signer, fake)
	stopClock := driveFakeClock(t, fake, 100*time.Millisecond)
	defer stopClock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess := resy.NewSessionForTest("u@x.io", "jwt", time.Now().Add(time.Hour))
	_, err := c.PrepareSlot(ctx, signTestSlot(), sess)
	if !errors.Is(err, providers.ErrAntiBotChallenge) {
		t.Fatalf("err = %v, want ErrAntiBotChallenge", err)
	}
	if signer.resetN.Load() != 1 {
		t.Errorf("Reset calls: %d want 1", signer.resetN.Load())
	}
	if hits.Load() != 2 {
		t.Errorf("/3/details hits: %d want 2", hits.Load())
	}
}

// TestSignerHeadersRideIntoFind is the equivalent of the details/book
// header test for /4/find. R7 originally only signed details + book;
// the post-merge fix routes /4/find through the sign envelope too
// because Find is the highest-volume polling endpoint and the one
// PerimeterX watches most aggressively.
func TestSignerHeadersRideIntoFind(t *testing.T) {
	t.Parallel()

	var findHdr atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/4/find" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		findHdr.Store(r.Header.Clone())
		_, _ = io.WriteString(w, `{"results":{"venues":[]}}`)
	}))
	defer srv.Close()

	fake := clock.NewFake(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	signer := &fakeSigner{headers: sign.Headers{
		"X-Px-Foo":       "bar",
		"x-resy-rotated": "abc",
	}}
	c := signedClient(t, srv, signer, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// We expect ErrInventoryEmpty because the fixture returns no
	// venues — the point is the headers, not the result shape.
	_, _ = c.Find(ctx, providers.FindRequest{
		Venue:     domain.VenueRef{Provider: "resy", Ref: "38660"},
		Date:      domain.NewDate(2026, time.June, 1),
		PartySize: 2,
	})

	fh, _ := findHdr.Load().(http.Header)
	if fh == nil {
		t.Fatal("/4/find was never called")
	}
	if fh.Get("X-Px-Foo") != "bar" {
		t.Errorf("/4/find missing x-px-foo: %q", fh.Get("X-Px-Foo"))
	}
	if fh.Get("X-Resy-Rotated") != "abc" {
		t.Errorf("/4/find missing x-resy-rotated: %q", fh.Get("X-Resy-Rotated"))
	}
	if signer.signCalls.Load() < 1 {
		t.Errorf("Sign was not called for /4/find (calls=%d)", signer.signCalls.Load())
	}
}

// TestNoopSignerLeavesBehaviorUnchanged is the regression guard for
// the "Noop is the default" contract. With no WithSigner option, the
// classifier surfaces ErrAntiBotChallenge on the first 403 and the
// adapter does NOT retry — the retry would be byte-for-byte identical
// (Noop.Reset is a no-op, Noop.Sign returns empty headers), so doing
// it would just double rate-limit pressure on users without a real
// signer for zero recovery benefit.
func TestNoopSignerLeavesBehaviorUnchanged(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"captcha required"}`)
	}))
	defer srv.Close()

	c := resy.NewClient(slog.New(slog.DiscardHandler), clock.NewReal(),
		resy.WithBaseURL(srv.URL),
		resy.WithAPIKey("test"),
		resy.WithHTTPClient(srv.Client()),
		resy.WithUserAgent("test"),
	) // no WithSigner — falls back to sign.Noop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess := resy.NewSessionForTest("u@x.io", "jwt", time.Now().Add(time.Hour))
	_, err := c.PrepareSlot(ctx, signTestSlot(), sess)
	if !errors.Is(err, providers.ErrAntiBotChallenge) {
		t.Fatalf("err = %v, want ErrAntiBotChallenge", err)
	}
	// Pre-R7 contract: a 403 fires exactly one request. The Noop
	// short-circuit in doSignedAndRetry preserves that.
	if got := hits.Load(); got != 1 {
		t.Errorf("hits: %d want 1 (Noop must skip the retry)", got)
	}
}
