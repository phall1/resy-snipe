package resy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// IdempotencyKeyHeader is the HTTP header Resy honors for
// idempotent /3/book retries. The engine computes the key per-attempt
// (see domain.IdempotencyKey).
const IdempotencyKeyHeader = "X-Resy-Idempotency-Key"

// idempotencyTracker remembers keys we have already sent so a bug
// loop cannot double-book by re-issuing the same idempotency key with
// a fresh /3/book POST. It is in-memory only — Phase 2 may persist it
// — but for Phase 1 it covers the local "engine retried with the same
// key" failure mode.
type idempotencyTracker struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func (t *idempotencyTracker) check(key string) error {
	if key == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil {
		t.seen = map[string]struct{}{}
	}
	if _, dup := t.seen[key]; dup {
		return fmt.Errorf("resy: idempotency key %s already sent", key[:min(8, len(key))])
	}
	t.seen[key] = struct{}{}
	return nil
}

// Book implements providers.Provider.Book. It performs the
// find→details→book pipeline's last two legs synchronously: it fetches
// /3/details to mint a fresh short-TTL book_token, then immediately
// POSTs /3/book with the token plus the user's default payment
// method.
//
// The race-and-cancel orchestration (serialize /3/details across
// candidate slots, parallelize /3/book among the survivors) lives in
// the engine; from this method's perspective each invocation is a
// single synchronous fetch+post.
//
// Idempotency: if BookRequest.IdempotencyKey is set, it is sent as
// the X-Resy-Idempotency-Key header AND tracked in-memory so the
// adapter refuses to re-send the same key twice (which would mean an
// engine retry loop bug, not a legitimate retry — legitimate retries
// derive a new key by incrementing attempt_n).
func (c *Client) Book(ctx context.Context, slot providers.Slot, sess providers.Session) (providers.Confirmation, error) {
	rs, ok := sess.(*Session)
	if !ok {
		return providers.Confirmation{}, fmt.Errorf("resy.Book: expected *resy.Session, got %T", sess)
	}
	payload, ok := slot.Payload.(domain.ResySlotPayload)
	if !ok {
		return providers.Confirmation{}, fmt.Errorf("resy.Book: expected ResySlotPayload, got %T", slot.Payload)
	}

	// Step 1: /3/details — mint book_token + lift the user's default
	// payment method off the response.
	det, err := c.fetchDetails(ctx, payload.ConfigID, slot.Venue, slot, rs.jwt)
	if err != nil {
		return providers.Confirmation{}, err
	}
	if det.BookToken.Value == "" {
		return providers.Confirmation{}, errors.New("resy.Book: /3/details returned no book_token")
	}
	if len(det.User.PaymentMethods) == 0 {
		return providers.Confirmation{}, errors.New("resy.Book: no payment method on file")
	}
	paymentID := det.User.PaymentMethods[0].ID

	// Step 2: /3/book — post the token. This is the call that wins or
	// loses the race against other bookers.
	confirmation, err := c.postBook(ctx, det.BookToken.Value, paymentID, slot.Payload, rs.jwt)
	if err != nil {
		return providers.Confirmation{}, err
	}
	return confirmation, nil
}

// BookRequest carries an explicit idempotency key alongside a slot.
// The Provider interface's Book method does not surface it (engine
// passes it via the Slot.Payload-adjacent path), but this convenience
// wrapper is what the engine's race-and-cancel layer calls when it
// needs explicit control.
func (c *Client) BookWithIdempotency(
	ctx context.Context,
	slot providers.Slot,
	sess providers.Session,
	idempotencyKey string,
) (providers.Confirmation, error) {
	if err := c.idempotency.check(idempotencyKey); err != nil {
		return providers.Confirmation{}, err
	}
	rs, ok := sess.(*Session)
	if !ok {
		return providers.Confirmation{}, fmt.Errorf("resy.BookWithIdempotency: expected *resy.Session, got %T", sess)
	}
	payload, ok := slot.Payload.(domain.ResySlotPayload)
	if !ok {
		return providers.Confirmation{}, fmt.Errorf("resy.BookWithIdempotency: expected ResySlotPayload, got %T", slot.Payload)
	}

	det, err := c.fetchDetails(ctx, payload.ConfigID, slot.Venue, slot, rs.jwt)
	if err != nil {
		return providers.Confirmation{}, err
	}
	if det.BookToken.Value == "" {
		return providers.Confirmation{}, errors.New("resy.BookWithIdempotency: /3/details returned no book_token")
	}
	if len(det.User.PaymentMethods) == 0 {
		return providers.Confirmation{}, errors.New("resy.BookWithIdempotency: no payment method on file")
	}

	return c.postBookWithIdempotency(ctx, det.BookToken.Value,
		det.User.PaymentMethods[0].ID, rs.jwt, idempotencyKey)
}

func (c *Client) fetchDetails(
	ctx context.Context,
	configID string,
	_ domain.VenueRef,
	slot providers.Slot,
	jwt string,
) (DetailsResponse, error) {
	q := url.Values{
		"config_id":  {configID},
		"day":        {dayFromSlot(slot)},
		"party_size": {strconv.Itoa(slot.PartySize)},
	}
	body, resp, err := c.do(ctx, http.MethodGet, "/3/details", q, nil, jwt)
	if err != nil {
		return DetailsResponse{}, fmt.Errorf("resy.Book details: %w", err)
	}
	if cls := classify(resp.StatusCode, body); cls != nil {
		return DetailsResponse{}, fmt.Errorf("resy.Book details: %w", cls)
	}
	var det DetailsResponse
	if err := json.Unmarshal(body, &det); err != nil {
		return DetailsResponse{}, fmt.Errorf("resy.Book details: parse: %w", err)
	}
	return det, nil
}

func (c *Client) postBook(
	ctx context.Context,
	bookToken string,
	paymentID int64,
	_ domain.SlotPayload,
	jwt string,
) (providers.Confirmation, error) {
	return c.postBookWithIdempotency(ctx, bookToken, paymentID, jwt, "")
}

func (c *Client) postBookWithIdempotency(
	ctx context.Context,
	bookToken string,
	paymentID int64,
	jwt string,
	idempotencyKey string,
) (providers.Confirmation, error) {
	form := url.Values{
		"book_token":            {bookToken},
		"struct_payment_method": {fmt.Sprintf(`{"id":%d}`, paymentID)},
	}
	body, resp, err := c.doWithExtraHeaders(ctx,
		http.MethodPost, "/3/book", nil,
		formReader(form), jwt,
		map[string]string{IdempotencyKeyHeader: idempotencyKey},
	)
	if err != nil {
		return providers.Confirmation{}, fmt.Errorf("resy.Book post: %w", err)
	}
	if cls := classify(resp.StatusCode, body); cls != nil {
		return providers.Confirmation{}, fmt.Errorf("resy.Book post: %w", cls)
	}
	var br BookResponse
	if err := json.Unmarshal(body, &br); err != nil {
		return providers.Confirmation{}, fmt.Errorf("resy.Book post: parse: %w", err)
	}
	if br.ResyToken == "" {
		return providers.Confirmation{}, errors.New("resy.Book: response had no resy_token")
	}
	return providers.Confirmation{
		ID:            br.ResyToken,
		BookedAt:      c.clock.Now(),
		HumanReadable: fmt.Sprintf("Reservation %d (token %s)", br.ReservationID, br.ResyToken),
	}, nil
}

// formReader returns an io.Reader yielding the form-encoded values.
func formReader(v url.Values) io.Reader { return strings.NewReader(v.Encode()) }

// dayFromSlot returns the slot's calendar date in YYYY-MM-DD form. The
// Slot.Date field is populated by Find when it parses /4/find's
// response; engines that construct Slots by other means must set it
// or /3/details will be queried with an empty day.
func dayFromSlot(s providers.Slot) string {
	if s.Date.IsZero() {
		return ""
	}
	return s.Date.String()
}
