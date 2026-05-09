package resy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/providers"
)

// signRetryFloor is the bounded backoff between the first
// ErrAntiBotChallenge response and the post-Reset retry. It mirrors
// the engine's empirical anti-bot floor (I-6) so the retry never
// pushes us below the safe request cadence.
const signRetryFloor = 100 * time.Millisecond

// doSignedAndRetry is the single path /3/details and /3/book share
// for sign-and-retry behavior:
//
//  1. Ask the Signer for any per-call headers; merge them with extra.
//     A Signer error is logged-but-ignored so a misconfigured signer
//     does not break the happy path.
//  2. Call the underlying do(). If the response classifies as
//     providers.ErrAntiBotChallenge, call Signer.Reset(ctx), wait
//     signRetryFloor (or until ctx cancels), and retry exactly once.
//  3. The second response is returned unchanged — even if it is
//     another ErrAntiBotChallenge. The engine sees the second failure
//     as terminal, matching today's contract.
//
// bodyFactory rebuilds the request body for the retry. POST callers
// pass a closure that returns a fresh io.Reader; GET callers pass nil.
// We can't reuse a single io.Reader because the first request consumes
// it.
func (c *Client) doSignedAndRetry(
	ctx context.Context,
	method, path string,
	query url.Values,
	bodyFactory func() io.Reader,
	authToken string,
	extra map[string]string,
) ([]byte, *Response, error) {
	body, resp, err := c.attemptSigned(ctx, method, path, query, bodyFactory, authToken, extra)
	if err != nil {
		return body, resp, err
	}
	if resp == nil {
		return body, nil, nil
	}
	cls := classify(resp.StatusCode, body)
	if cls == nil || !errors.Is(cls, providers.ErrAntiBotChallenge) {
		// Either success or some other classified failure — let the caller
		// run the canonical classify() for itself; we just return raw.
		return body, resp, nil
	}

	// Reset the signer's cached state and wait the floor before retry.
	if rerr := c.signer.Reset(ctx); rerr != nil {
		c.log.LogAttrs(ctx, slog.LevelWarn, "signer reset failed",
			slog.String(domain.LogKeyProvider, "resy"),
			slog.String("path", path),
			slog.String("err", rerr.Error()))
	} else {
		c.log.LogAttrs(ctx, slog.LevelInfo, "signer reset on anti-bot challenge",
			slog.String(domain.LogKeyProvider, "resy"),
			slog.String("path", path))
	}
	if waitErr := c.waitFloor(ctx, signRetryFloor); waitErr != nil {
		// ctx canceled mid-wait; surface the original anti-bot response
		// (body + status) so the caller's classify() yields the
		// upstream sentinel, matching the no-retry-possible contract.
		// The waitErr is logged as debug; engine code checks ctx.Err()
		// for cancellation if it cares.
		c.log.LogAttrs(ctx, slog.LevelDebug, "sign-and-retry wait canceled",
			slog.String(domain.LogKeyProvider, "resy"),
			slog.String("path", path),
			slog.String("err", waitErr.Error()))
		return body, resp, nil
	}

	return c.attemptSigned(ctx, method, path, query, bodyFactory, authToken, extra)
}

// attemptSigned performs one signed round-trip: it consults the
// Signer, merges its headers into extra, and calls
// doWithExtraHeaders. It is intentionally minimal so the retry path
// in doSignedAndRetry can call it twice without duplicating header
// assembly.
func (c *Client) attemptSigned(
	ctx context.Context,
	method, path string,
	query url.Values,
	bodyFactory func() io.Reader,
	authToken string,
	extra map[string]string,
) ([]byte, *Response, error) {
	signed, err := c.signer.Sign(ctx, path)
	if err != nil {
		// Best-effort signing: we log and continue with whatever extra
		// headers the caller passed. A real signing requirement on the
		// upstream will surface as ErrAntiBotChallenge from the response,
		// which the retry path then handles deterministically.
		c.log.LogAttrs(ctx, slog.LevelWarn, "signer error",
			slog.String(domain.LogKeyProvider, "resy"),
			slog.String("path", path),
			slog.String("err", err.Error()))
	}

	merged := mergeHeaders(extra, signed)

	var body io.Reader
	if bodyFactory != nil {
		body = bodyFactory()
	}
	bs, resp, derr := c.doWithExtraHeaders(ctx, method, path, query, body, authToken, merged)
	return bs, resp, derr
}

// mergeHeaders combines caller-supplied extra headers with the
// Signer's contribution. Caller wins on key collision so an explicit
// X-Resy-Idempotency-Key from the book path is never overwritten by a
// stray Signer.
func mergeHeaders(base, overlay map[string]string) map[string]string {
	if len(overlay) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(overlay))
	maps.Copy(out, overlay)
	maps.Copy(out, base)
	return out
}

// waitFloor blocks for d as measured by the client's clock, returning
// ctx.Err() if cancellation lands first. It mirrors the engine's
// pollInterval pattern so retry pacing stays deterministic under
// clock.Fake in tests.
func (c *Client) waitFloor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	ch := c.clock.After(d)
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("resy: wait floor: %w", ctx.Err())
	}
}
