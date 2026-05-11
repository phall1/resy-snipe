package service

// audit.go: the Service-side audit-log hook. Every public Service
// method routes through Standard.audit, which translates the call's
// outcome into one AuditWrite and hands it to the StoreBackend. The
// underlying SQL lives in internal/store/audit.go; this file is the
// Service-side glue that decides the action string, error code, and
// failure-mode policy.
//
// Spec: docs/v2/design/service-layer.md §audit-log-contract.
// "Every Service call writes one audit_events row before returning.
// Failed auth still logs."
//
// Failure-mode policy: an audit-write failure MUST NOT fail the
// parent Service call. The Service has already done the user's work
// (or already decided to refuse); a persistence-layer hiccup on the
// audit row is the Service's problem, not the caller's. We log a
// WARN with the action, target, and the underlying error so the
// operator can investigate, and we proceed.

import (
	"context"
	"errors"
	"log/slog"

	"resy-snipe/internal/domain"
)

// Audit action constants. The dot-namespaced shape mirrors the design
// doc (docs/v2/design/multi-user.md §8 sample rows: "quest.create",
// "quest.cancel", "audit.read"). Keeping them as `const` here gives
// callers one source of truth and lets the operator-admin reader
// (M2 wave) filter on a typed enum if it grows one.
const (
	actionVenueResolve   = "venue.resolve"
	actionQuestPlan      = "quest.plan"
	actionQuestCreate    = "quest.create"
	actionQuestGet       = "quest.get"
	actionQuestList      = "quest.list"
	actionQuestCancel    = "quest.cancel"
	actionQuestSubscribe = "quest.subscribe"
	actionAccountLogin   = "account.login"
	actionAccountList    = "account.list"
	actionUserInvite     = "user.invite"
	actionInviteAccept   = "user.accept"
	actionTokenIssue     = "token.issue"
	actionTokenRevoke    = "token.revoke"
	actionTokenList      = "token.list"
	actionUserList       = "user.list"
)

// audit writes one audit_events row for a completed Service call.
// userID is the actor; action is the dot-namespaced verb; targetID
// identifies the affected resource (questID, accountID, "" for list
// verbs); err is the call's return value (nil on success).
//
// On err != nil, ok=0 and error_code is the sentinel name (see
// errorCodeFor). On a successful auth, ok=1 and error_code is empty.
//
// An audit-write failure (the store returns an error) is logged at
// WARN and SWALLOWED — the parent call's outcome is unchanged. This
// is the §audit-log-contract failure policy: an unaudited Service
// call is a degraded experience, not a correctness bug.
func (s *Standard) audit(
	ctx context.Context,
	userID domain.UserID,
	action, targetID string,
	err error,
) {
	if s == nil || s.store == nil {
		return
	}
	ok := err == nil
	errCode := ""
	if !ok {
		errCode = errorCodeFor(err)
	}
	evt := AuditWrite{
		UserID:    userID,
		Action:    action,
		TargetID:  targetID,
		OK:        ok,
		ErrorCode: errCode,
	}
	if writeErr := s.store.WriteAuditEvent(ctx, evt); writeErr != nil {
		// Swallow: an audit-write failure must not fail the parent
		// call. Surface enough context that an operator scanning
		// logs can correlate the missing audit row with the live
		// call's structured-log line.
		s.logger.Warn("service.audit_write_failed",
			slog.String("user", string(userID)),
			slog.String("action", action),
			slog.String("target", targetID),
			slog.Bool("ok", ok),
			slog.String("err", writeErr.Error()),
		)
	}
}

// errorCodeFor maps a Service-layer error to a short, stable string
// suitable for the audit_events.error_code column. The mapping is
// the sentinel name verbatim — callers branching on
// errors.Is(err, ErrFoo) can search audit rows by the same identifier.
//
// Unknown errors fall back to "internal" so a missing case in this
// switch never produces an empty error_code on a !ok row.
func errorCodeFor(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrInvalidPlanHash):
		return "invalid_plan_hash"
	case errors.Is(err, ErrVenueNotFound):
		return "venue_not_found"
	case errors.Is(err, ErrVenueAmbiguous):
		return "venue_ambiguous"
	case errors.Is(err, ErrUpstreamUnavailable):
		return "upstream_unavailable"
	case errors.Is(err, ErrInvalidArgument):
		return "invalid_argument"
	case errors.Is(err, ErrTokenExpired):
		return "token_expired"
	case errors.Is(err, ErrTokenRevoked):
		return "token_revoked"
	case errors.Is(err, ErrInviteExpired):
		return "invite_expired"
	case errors.Is(err, ErrInviteConsumed):
		return "invite_consumed"
	case errors.Is(err, ErrIdempotencyConflict):
		return "idempotency_conflict"
	case errors.Is(err, ErrNotImplemented):
		return "not_implemented"
	default:
		return "internal"
	}
}

// writeQuestEvent persists one engine-emitted Event into the
// quest_events stream. The Service calls this from:
//   - CreateQuest after a successful engine.Submit (type=submitted).
//   - CancelQuest after the status flip (type=canceled).
//   - SubscribeQuest's live pump, for every engine notification that
//     matches the subscribed quest (one row per state transition).
//
// Tenancy is enforced at the SQL level (the store helper gates on
// quests.user_id), so a wrong-tenant call inserts zero rows.
//
// Failure-mode policy mirrors audit: a write failure is logged at
// WARN and swallowed. A missing quest_event row degrades replay
// (the next GetQuest sees an incomplete history) but never fails
// the parent call.
func (s *Standard) writeQuestEvent(
	ctx context.Context,
	userID domain.UserID,
	questID domain.QuestID,
	evt domain.Event,
) {
	if s == nil || s.store == nil {
		return
	}
	if !s.persistEvents {
		return
	}
	if err := s.store.WriteQuestEvent(ctx, userID, questID, evt); err != nil {
		s.logger.Warn("service.quest_event_write_failed",
			slog.String("user", string(userID)),
			slog.String("quest", string(questID)),
			slog.String("event", string(evt.Type)),
			slog.String("err", err.Error()),
		)
	}
}
