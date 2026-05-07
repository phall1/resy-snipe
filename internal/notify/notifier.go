package notify

import (
	"context"
	"log/slog"

	"resy-snipe/internal/domain"
)

// Notifier is the seam between the engine's lifecycle events and the
// surface that renders them to a user. Phase 1 ships a single stdout
// implementation (StdoutNotifier in stdout.go); Phase 2+ may plug in
// SMS, iMessage, or a chat-bot bridge behind the same interface so the
// engine never grows a transport-specific branch.
//
// All methods take a ctx so future async fan-out can honor request
// cancellation without growing the interface. Implementations MUST NOT
// block the caller for non-trivial work — engine state-machine progress
// is synchronous and a slow notifier would stall booking.
type Notifier interface {
	// Transition reports a status change for snipe. attrs carry
	// transition-specific context (release-time, attempt count,
	// resy_request_id, etc.). The slog.Attr type is reused here rather
	// than introducing a parallel key/value type so callers that already
	// hold canonical attrs (domain.LogKey* constants) can pass them
	// straight through.
	Transition(
		ctx context.Context,
		snipe domain.SnipeID,
		from, to domain.Status,
		attrs []slog.Attr,
	)

	// Result reports the terminal outcome of a snipe. success=true means
	// the booking landed; success=false means the snipe failed in any
	// non-success terminal state (Failed, Canceled, Expired).
	//
	// message is the human-readable summary (confirmation id and details
	// on success; error class on failure). attrs carry structured
	// context for richer surfaces (a future Telegram bot may render
	// them as an inline keyboard, for example).
	Result(
		ctx context.Context,
		snipe domain.SnipeID,
		success bool,
		message string,
		attrs []slog.Attr,
	)

	// Close gives implementations a hook to flush buffered output or
	// drain background workers. Stdout implementations may legitimately
	// no-op. Returning an error is fine; callers should log and
	// continue — a notifier failure must not unwind the engine.
	Close() error
}
