package resy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

var jsonUnmarshal = json.Unmarshal

// PrepareSlot fetches /3/details for `slot` and returns a copy of the
// slot with its ResySlotPayload's BookToken and PaymentMethodID
// populated. The engine calls PrepareSlot serially across the candidate
// slots (per BookingPolicy.DetailsSerial) to avoid burning short-TTL
// book_tokens on slots we won't actually try to book.
//
// Resy's book_token has a short TTL — the engine should call
// ConfirmSlot promptly after PrepareSlot succeeds.
func (c *Client) PrepareSlot(ctx context.Context, slot providers.Slot, sess providers.Session) (providers.Slot, error) {
	rs, ok := sess.(*Session)
	if !ok {
		return providers.Slot{}, fmt.Errorf("resy.PrepareSlot: expected *resy.Session, got %T", sess)
	}
	payload, ok := slot.Payload.(domain.ResySlotPayload)
	if !ok {
		return providers.Slot{}, fmt.Errorf("resy.PrepareSlot: expected ResySlotPayload, got %T", slot.Payload)
	}
	if payload.ConfigID == "" {
		return providers.Slot{}, errors.New("resy.PrepareSlot: ConfigID is required")
	}

	q := url.Values{
		"config_id":  {payload.ConfigID},
		"day":        {dayFromSlot(slot)},
		"party_size": {strconv.Itoa(slot.PartySize)},
	}
	body, resp, err := c.doSignedAndRetry(ctx, http.MethodGet, "/3/details", q, nil, rs.jwt, nil)
	if err != nil {
		return providers.Slot{}, fmt.Errorf("resy.PrepareSlot: %w", err)
	}
	if cls := classify(resp.StatusCode, body); cls != nil {
		return providers.Slot{}, fmt.Errorf("resy.PrepareSlot: %w", cls)
	}
	det, err := decodeDetails(body)
	if err != nil {
		return providers.Slot{}, err
	}
	if det.BookToken.Value == "" {
		return providers.Slot{}, errors.New("resy.PrepareSlot: /3/details returned no book_token")
	}
	if len(det.User.PaymentMethods) == 0 {
		return providers.Slot{}, errors.New("resy.PrepareSlot: no payment method on file")
	}
	payload.BookToken = det.BookToken.Value
	payload.PaymentMethodID = det.User.PaymentMethods[0].ID

	out := slot
	out.Payload = payload
	return out, nil
}

// ConfirmSlot performs the /3/book call for a slot whose Payload has
// already been populated by PrepareSlot. The race-and-cancel layer
// fires N ConfirmSlot calls concurrently; the first to win cancels
// the rest via shared ctx.
//
// idempotencyKey, when non-empty, is sent as the Resy idempotency
// header AND tracked in-memory so a bug-loop cannot double-book by
// re-issuing the same key.
func (c *Client) ConfirmSlot(ctx context.Context, slot providers.Slot, sess providers.Session, idempotencyKey string) (providers.Confirmation, error) {
	rs, ok := sess.(*Session)
	if !ok {
		return providers.Confirmation{}, fmt.Errorf("resy.ConfirmSlot: expected *resy.Session, got %T", sess)
	}
	payload, ok := slot.Payload.(domain.ResySlotPayload)
	if !ok {
		return providers.Confirmation{}, fmt.Errorf("resy.ConfirmSlot: expected ResySlotPayload, got %T", slot.Payload)
	}
	if payload.BookToken == "" {
		return providers.Confirmation{}, errors.New("resy.ConfirmSlot: BookToken is required (run PrepareSlot first)")
	}
	if payload.PaymentMethodID == 0 {
		return providers.Confirmation{}, errors.New("resy.ConfirmSlot: PaymentMethodID is required (run PrepareSlot first)")
	}

	if err := c.idempotency.check(idempotencyKey); err != nil {
		return providers.Confirmation{}, err
	}
	return c.postBookWithIdempotency(ctx, payload.BookToken, payload.PaymentMethodID, rs.jwt, idempotencyKey)
}

// decodeDetails reuses the same shape the existing Book path uses.
func decodeDetails(body []byte) (DetailsResponse, error) {
	var det DetailsResponse
	if err := jsonUnmarshal(body, &det); err != nil {
		return DetailsResponse{}, fmt.Errorf("resy.PrepareSlot: parse: %w", err)
	}
	return det, nil
}
