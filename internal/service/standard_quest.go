package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/engine"
	"resy-snipe/internal/planner"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resolver"
)

// idempotencyTTL is the retention window for replayable Service
// calls. docs/v2/design/service-layer.md §idempotency fixes it at 24h.
const idempotencyTTL = 24 * time.Hour

// scopeCreateQuest / scopeCancelQuest are the idempotency-key scope
// strings persisted alongside the hash. Scoping makes "same plaintext
// key on a different verb" a primary-key miss rather than a silent
// replay.
const (
	scopeCreateQuest = "create_quest"
	scopeCancelQuest = "cancel_quest"
)

// ResolveVenue delegates to the resolver and translates the
// resolver-local error sentinels into service-level sentinels. The
// service does not persist anything here — resolution is pure.
//
// userID is accepted (and required at the interface) so the
// transport layer's tenancy contract is uniform: every Service
// method takes userID. The resolver itself is cross-tenant
// (venues_cache is global) so the parameter is not threaded into
// the resolve call.
func (s *Standard) ResolveVenue(ctx context.Context, userID domain.UserID, query domain.VenueQuery) (domain.Venue, error) {
	if userID == "" {
		return domain.Venue{}, fmt.Errorf("ResolveVenue: %w: userID is required", ErrInvalidArgument)
	}
	v, err := s.resolver.Resolve(ctx, query)
	if err != nil {
		return domain.Venue{}, mapResolverError(err)
	}
	return v, nil
}

// PlanQuest is the pure (Goal, Venue) → Plan computation. The
// Service is responsible for assembling the planner input: resolve
// the venue, gather the calendar snapshot (when a provider is wired),
// and let the planner choose the strategy.
//
// Goal validation runs first so an invalid-goal error is surfaced
// before any I/O lands. The returned Plan carries a content hash a
// later CreateQuest will recompute and verify.
func (s *Standard) PlanQuest(ctx context.Context, userID domain.UserID, goal domain.Goal) (domain.Plan, error) {
	if userID == "" {
		return domain.Plan{}, fmt.Errorf("PlanQuest: %w: userID is required", ErrInvalidArgument)
	}
	now := s.clock.Now()
	if err := goal.Validate(now); err != nil {
		return domain.Plan{}, fmt.Errorf("PlanQuest: %w: %w", ErrInvalidArgument, err)
	}

	venue, err := s.resolver.Resolve(ctx, goal.VenueQuery)
	if err != nil {
		return domain.Plan{}, mapResolverError(err)
	}

	// Calendar snapshot is best-effort: a missing/erroring provider
	// degrades the planner to the explicit/discovered branches per
	// the strategy-selection rules. We log the provider failure but
	// don't surface it — the plan stays computable.
	var snap providers.Calendar
	if s.provider != nil {
		cal, calErr := s.provider.Calendar(ctx, venue.AsRef(), providers.DateRange{
			Start:     goal.Date,
			End:       goal.Date,
			PartySize: goal.Party,
		})
		if calErr != nil {
			s.logger.Warn("plan.calendar_snapshot_failed",
				slog.String("user", string(userID)),
				slog.String("venue", venue.AsRef().String()),
				slog.String("err", calErr.Error()),
			)
		} else {
			snap = cal
		}
	}

	plan, err := planner.BuildPlan(ctx, planner.PlanInput{
		Goal:             goal,
		Venue:            venue,
		Now:              now,
		CalendarSnapshot: snap,
	})
	if err != nil {
		return domain.Plan{}, fmt.Errorf("PlanQuest: %w", err)
	}
	return plan, nil
}

// CreateQuest is the keystone v2 verb: it commits a Goal as a Quest,
// derives the engine-level Intent, and hands the Intent to the
// engine for scheduling.
//
// Algorithm:
//  1. Validate the goal (PlanQuest does this; we call it inline so a
//     hash mismatch surfaces ErrInvalidPlanHash, not a stale plan).
//  2. Recompute the Plan. If opts.PlanHash is non-nil and mismatches,
//     refuse with ErrInvalidPlanHash (ADR-0012).
//  3. Look up the (userID, goal.AccountID) account — wrong-tenant
//     surfaces as ErrNotFound (folded by the StoreBackend).
//  4. Mint a fresh QuestID; persist the quests row in Submitted state.
//  5. Build a domain.Intent from the Plan + Goal and submit it to the
//     engine. The engine writes its own bootstrap event into the v1
//     events table; M1-11 will switch to the v2 quest_events stream.
//
// Idempotency: opts.IdempotencyKey, when non-nil and non-empty, makes
// the call replayable for idempotencyTTL (24h). The Service consults
// the StoreBackend idempotency seam before executing fresh work; a
// matching prior outcome short-circuits with the persisted answer,
// and a key reused with a different goal surfaces
// ErrIdempotencyConflict. See docs/v2/design/service-layer.md
// §idempotency.
func (s *Standard) CreateQuest(
	ctx context.Context,
	userID domain.UserID,
	goal domain.Goal,
	opts CreateOpts,
) (domain.QuestID, error) {
	if userID == "" {
		return "", fmt.Errorf("CreateQuest: %w: userID is required", ErrInvalidArgument)
	}

	// Compute the goal payload hash up-front so the idempotency
	// pre-check can detect reused keys with mismatched goals. The
	// payload hash is sha256(canonical goal JSON); two calls with the
	// same in-memory Goal produce the same hash because
	// marshalGoalJSON is deterministic.
	goalJSON, err := marshalGoalJSON(goal)
	if err != nil {
		return "", fmt.Errorf("CreateQuest: %w", err)
	}
	payloadHash := payloadHashFor(goalJSON)

	now := s.clock.Now()

	// Idempotency pre-check. A found, non-expired, payload-matching
	// row short-circuits with the persisted outcome; a found row
	// with a mismatched payload surfaces ErrIdempotencyConflict; an
	// expired or missing row falls through to a fresh execution.
	if opts.IdempotencyKey != nil && *opts.IdempotencyKey != "" {
		lookup, lookupErr := s.store.GetIdempotencyResult(
			ctx, userID, scopeCreateQuest, *opts.IdempotencyKey, payloadHash, now,
		)
		if lookupErr != nil {
			return "", fmt.Errorf("CreateQuest idempotency lookup: %w", lookupErr)
		}
		if lookup.Found && !lookup.Expired {
			if !lookup.PayloadMatch {
				return "", fmt.Errorf("CreateQuest: %w: idempotency key %q previously used with a different goal",
					ErrIdempotencyConflict, *opts.IdempotencyKey)
			}
			if lookup.PrevErr != "" {
				return domain.QuestID(lookup.TargetID), replayServiceErr(lookup.PrevErr)
			}
			return domain.QuestID(lookup.TargetID), nil
		}
	}

	// PlanQuest validates the goal and resolves the venue — running
	// it here means a hash mismatch fires before any persistence.
	plan, err := s.PlanQuest(ctx, userID, goal)
	if err != nil {
		s.recordIdempotency(ctx, userID, scopeCreateQuest, opts.IdempotencyKey, payloadHash, "", err, now)
		return "", err
	}
	if opts.PlanHash != nil && *opts.PlanHash != plan.Hash {
		hashErr := fmt.Errorf("CreateQuest: %w (caller=%q service=%q)",
			ErrInvalidPlanHash, *opts.PlanHash, plan.Hash)
		s.recordIdempotency(ctx, userID, scopeCreateQuest, opts.IdempotencyKey, payloadHash, "", hashErr, now)
		return "", hashErr
	}

	// Account ownership check. ErrNotFound from the StoreBackend
	// already folds existence and tenancy; we pass it through.
	acct, err := s.store.GetAccountByEmail(ctx, userID, string(goal.AccountID))
	if err != nil {
		// Empty AccountID slipped past goal.Validate when no account
		// is bound — surface as InvalidArgument so the transport can
		// suggest a login.
		if errors.Is(err, ErrNotFound) {
			// Re-lookup by the AccountID-as-id form: goal.AccountID
			// may carry the canonical acct_… id (not the email).
			if found, listErr := s.findAccountByID(ctx, userID, goal.AccountID); listErr == nil {
				acct = found
			} else {
				notFound := fmt.Errorf("CreateQuest account %s: %w", goal.AccountID, ErrNotFound)
				s.recordIdempotency(ctx, userID, scopeCreateQuest, opts.IdempotencyKey, payloadHash, "", notFound, now)
				return "", notFound
			}
		} else {
			wrapped := fmt.Errorf("CreateQuest: %w", err)
			s.recordIdempotency(ctx, userID, scopeCreateQuest, opts.IdempotencyKey, payloadHash, "", wrapped, now)
			return "", wrapped
		}
	}

	questID, err := newQuestID()
	if err != nil {
		wrapped := fmt.Errorf("CreateQuest: %w", err)
		s.recordIdempotency(ctx, userID, scopeCreateQuest, opts.IdempotencyKey, payloadHash, "", wrapped, now)
		return "", wrapped
	}

	row := QuestRow{
		ID:        questID,
		UserID:    userID,
		AccountID: acct.ID,
		GoalJSON:  goalJSON,
		PlanHash:  plan.Hash,
		Status:    domain.StatusSubmitted,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.CreateQuest(ctx, row); err != nil {
		wrapped := fmt.Errorf("CreateQuest persist: %w", err)
		s.recordIdempotency(ctx, userID, scopeCreateQuest, opts.IdempotencyKey, payloadHash, "", wrapped, now)
		return "", wrapped
	}

	// TODO(M1-11): audit_events.write(user=userID, action=create_quest,
	// target=questID, ok=true).

	intent := intentFromPlan(userID, plan, goal)
	if _, err := s.engine.Submit(ctx, domain.SnipeID(questID), intent); err != nil {
		// The quest row is already persisted; leaving it visible with
		// status=Submitted (pending) is correct — a follow-up retry
		// can re-submit. We surface the engine error so the caller
		// knows scheduling failed. The idempotency row records the
		// minted questID so a replay still returns it.
		wrapped := fmt.Errorf("CreateQuest engine submit: %w", err)
		s.recordIdempotency(ctx, userID, scopeCreateQuest, opts.IdempotencyKey, payloadHash, string(questID), wrapped, now)
		return questID, wrapped
	}

	s.recordIdempotency(ctx, userID, scopeCreateQuest, opts.IdempotencyKey, payloadHash, string(questID), nil, now)
	return questID, nil
}

// GetQuest returns the QuestState for a quest the caller owns.
// Wrong-tenant surfaces as ErrNotFound — see the StoreBackend
// contract.
//
// Events are read from the StoreBackend's quest_events seam, bounded
// by defaultEventLimit. M1-10 returns an empty list until M1-11
// wires the quest_events writers.
func (s *Standard) GetQuest(ctx context.Context, userID domain.UserID, questID domain.QuestID) (QuestState, error) {
	if userID == "" {
		return QuestState{}, fmt.Errorf("GetQuest: %w: userID is required", ErrInvalidArgument)
	}
	row, err := s.store.GetQuest(ctx, userID, questID)
	if err != nil {
		return QuestState{}, mapStoreNotFound(err)
	}
	goal, err := unmarshalGoalJSON(row.GoalJSON)
	if err != nil {
		return QuestState{}, fmt.Errorf("GetQuest decode goal: %w", err)
	}
	events, err := s.store.ListEvents(ctx, userID, questID, defaultEventLimit)
	if err != nil {
		return QuestState{}, fmt.Errorf("GetQuest events: %w", err)
	}

	state := QuestState{
		Summary: summaryFromRow(row),
		Goal:    goal,
		// Plan is reconstructed on demand: M1-10 persists only the
		// hash, not the full plan body, so a fresh PlanQuest is the
		// most faithful way to surface "the plan as of now". The
		// caller can compare row.PlanHash against state.Plan.Hash to
		// detect drift.
	}
	// Best-effort plan rebuild; a failed rebuild leaves Plan zero
	// rather than surfacing — the summary + goal + events are still
	// useful even if the venue resolver is offline.
	if plan, planErr := s.PlanQuest(ctx, userID, goal); planErr == nil {
		state.Plan = plan
	} else {
		s.logger.Warn("get_quest.plan_rebuild_failed",
			slog.String("user", string(userID)),
			slog.String("quest", string(questID)),
			slog.String("err", planErr.Error()),
		)
	}
	for _, ev := range events {
		state.Events = append(state.Events, domain.Event{
			Type:  ev.Type,
			At:    ev.At,
			Attrs: ev.Attrs,
		})
	}
	return state, nil
}

// ListQuests returns userID's quest summaries narrowed by filter.
// Empty result is normal — a fresh user has no quests.
func (s *Standard) ListQuests(ctx context.Context, userID domain.UserID, filter ListFilter) ([]QuestSummary, error) {
	if userID == "" {
		return nil, fmt.Errorf("ListQuests: %w: userID is required", ErrInvalidArgument)
	}
	rows, err := s.store.ListQuests(ctx, userID, filter)
	if err != nil {
		return nil, fmt.Errorf("ListQuests: %w", err)
	}
	out := make([]QuestSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, summaryFromRow(r))
	}
	return out, nil
}

// CancelQuest flips a quest's status to Canceled and stamps
// completed_at. Canceling an already-terminal quest is a no-op (per
// docs/v2/design/service-layer.md §error-model).
//
// The engine cancellation signal is a documented TODO: engine.Engine
// has no public Cancel surface yet, so the DB update is the only
// observable effect. M1-13 (subscribe) and a follow-up engine-cancel
// issue will wire the in-flight abort.
func (s *Standard) CancelQuest(
	ctx context.Context,
	userID domain.UserID,
	questID domain.QuestID,
	opts CancelOpts,
) error {
	if userID == "" {
		return fmt.Errorf("CancelQuest: %w: userID is required", ErrInvalidArgument)
	}

	// For cancel the "payload" is the questID — the same idempotency
	// key targeted at two different quests is a mismatched-input
	// conflict, while a re-cancel of the same quest with the same
	// key is a pure replay.
	payloadHash := payloadHashFor(string(questID))
	now := s.clock.Now()

	if opts.IdempotencyKey != nil && *opts.IdempotencyKey != "" {
		lookup, lookupErr := s.store.GetIdempotencyResult(
			ctx, userID, scopeCancelQuest, *opts.IdempotencyKey, payloadHash, now,
		)
		if lookupErr != nil {
			return fmt.Errorf("CancelQuest idempotency lookup: %w", lookupErr)
		}
		if lookup.Found && !lookup.Expired {
			if !lookup.PayloadMatch {
				return fmt.Errorf("CancelQuest: %w: idempotency key %q previously used to cancel %q",
					ErrIdempotencyConflict, *opts.IdempotencyKey, lookup.TargetID)
			}
			if lookup.PrevErr != "" {
				return replayServiceErr(lookup.PrevErr)
			}
			return nil
		}
	}

	row, err := s.store.GetQuest(ctx, userID, questID)
	if err != nil {
		mapped := mapStoreNotFound(err)
		s.recordIdempotency(ctx, userID, scopeCancelQuest, opts.IdempotencyKey, payloadHash, string(questID), mapped, now)
		return mapped
	}
	if row.Status.IsTerminal() {
		// Re-canceling a terminal quest is not an error — the API
		// contract is idempotent (docs/v2/design/service-layer.md
		// §error-model). We do not stamp a fresh completed_at.
		s.recordIdempotency(ctx, userID, scopeCancelQuest, opts.IdempotencyKey, payloadHash, string(questID), nil, now)
		return nil
	}

	if err := s.store.UpdateQuestStatus(ctx, userID, questID, domain.StatusCanceled, &now, now); err != nil {
		wrapped := fmt.Errorf("CancelQuest: %w", mapStoreNotFound(err))
		s.recordIdempotency(ctx, userID, scopeCancelQuest, opts.IdempotencyKey, payloadHash, string(questID), wrapped, now)
		return wrapped
	}

	// TODO(M1-13/engine-cancel): signal the engine so an in-flight
	// snipe aborts its scheduler loop. engine.Engine currently has no
	// public Cancel(SnipeID) surface; the DB flip is the only
	// observable effect for now.
	// TODO(M1-11): audit_events.write(user=userID, action=cancel_quest,
	// target=questID, ok=true, reason=opts.Reason).
	_ = opts.Reason

	s.recordIdempotency(ctx, userID, scopeCancelQuest, opts.IdempotencyKey, payloadHash, string(questID), nil, now)
	return nil
}

// payloadHashFor returns the lowercase hex sha256 of s. It is the
// digest the idempotency layer treats as the "payload" component of
// a key — two calls with identical payloads share the same hash and
// thus the same idempotency row.
func payloadHashFor(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// recordIdempotency persists the outcome of an idempotency-keyed
// call. A nil opts.IdempotencyKey or an empty string is a no-op —
// callers that did not opt in pay no storage cost. A failure to
// write the idempotency row is logged but not surfaced: an unaudited
// replay-result is a degraded experience, not a correctness bug, and
// the user already got their CreateQuest answer back.
func (s *Standard) recordIdempotency(
	ctx context.Context,
	userID domain.UserID,
	scope string,
	key *string,
	payloadHash string,
	targetID string,
	errVal error,
	now time.Time,
) {
	if key == nil || *key == "" {
		return
	}
	if putErr := s.store.PutIdempotencyResult(ctx, userID, scope, *key, payloadHash, targetID, errVal, idempotencyTTL, now); putErr != nil {
		s.logger.Warn("idempotency.put_failed",
			slog.String("user", string(userID)),
			slog.String("scope", scope),
			slog.String("err", putErr.Error()),
		)
	}
}

// replayServiceErr rewraps a persisted error string into a service
// sentinel. We match on the well-known sentinel substrings — the
// persisted form is the error's String() which contains the sentinel
// text plus any fmt.Errorf decoration. An unrecognized string falls
// back to a generic error wrapping the persisted text verbatim; the
// caller still sees an error, just without the sentinel-branch
// granularity.
func replayServiceErr(persisted string) error {
	switch {
	case strings.Contains(persisted, ErrInvalidPlanHash.Error()):
		return fmt.Errorf("%w (replay: %s)", ErrInvalidPlanHash, persisted)
	case strings.Contains(persisted, ErrNotFound.Error()):
		return fmt.Errorf("%w (replay: %s)", ErrNotFound, persisted)
	case strings.Contains(persisted, ErrInvalidArgument.Error()):
		return fmt.Errorf("%w (replay: %s)", ErrInvalidArgument, persisted)
	case strings.Contains(persisted, ErrVenueNotFound.Error()):
		return fmt.Errorf("%w (replay: %s)", ErrVenueNotFound, persisted)
	case strings.Contains(persisted, ErrVenueAmbiguous.Error()):
		return fmt.Errorf("%w (replay: %s)", ErrVenueAmbiguous, persisted)
	case strings.Contains(persisted, ErrUpstreamUnavailable.Error()):
		return fmt.Errorf("%w (replay: %s)", ErrUpstreamUnavailable, persisted)
	default:
		return errors.New("service: idempotent replay: " + persisted)
	}
}

// SubscribeQuest streams a quest's lifecycle events to cb. The
// algorithm is the "replay-then-live" pattern from
// docs/v2/design/service-layer.md §streaming:
//
//  1. Tenancy gate. GetQuest folds wrong-tenant and missing-quest
//     into ErrNotFound — a caller subscribing to somebody else's
//     quest, or to a typo'd ID, gets the same opaque signal.
//  2. Replay. Pump every persisted quest_events row for
//     (userID, questID) through cb in chronological order. M1-13
//     ships before the quest_events writers (M1-11) so this is
//     usually empty; the call is still made so the contract is
//     stable for the day M1-11 lands.
//  3. Subscribe to the engine. engine.Subscribe is callback-based
//     and global — the subscriber sees every snipe's notifications
//     — so we filter on SnipeID == questID (the engine's SnipeID
//     and the service's QuestID are intentionally the same string
//     by ADR-0001). engine.Subscribe's contract forbids blocking
//     on the emit path, so the subscriber stages each match into
//     a buffered channel rather than invoking cb inline.
//  4. Pump. Read the channel on the calling goroutine and invoke
//     cb synchronously — the Service contract is "cb runs on the
//     caller's goroutine, in arrival order". A terminal status or
//     ctx.Done ends the pump; both return nil because both are
//     graceful exits from the subscriber's perspective.
//
// The "subscribe before the engine knows about the quest" edge
// case is benign: engine.Subscribe always succeeds (it's a no-op
// registration). If the quest exists in persistence but has never
// been Submitted to this engine instance (e.g. after a daemon
// restart where the engine has not yet rehydrated), the live path
// simply produces no notifications and the call blocks until
// ctx.Done.
//
// A nil cb is a programming error and fails fast at the boundary.
func (s *Standard) SubscribeQuest(
	ctx context.Context,
	userID domain.UserID,
	questID domain.QuestID,
	cb func(domain.Event),
) error {
	if userID == "" {
		return fmt.Errorf("SubscribeQuest: %w: userID is required", ErrInvalidArgument)
	}
	if cb == nil {
		return fmt.Errorf("SubscribeQuest: %w: callback is required", ErrInvalidArgument)
	}

	// 1. Tenancy gate. Wrong-tenant and missing-quest both fold
	// into ErrNotFound (StoreBackend contract). A ctx-canceled
	// error here means the caller bailed before we could check
	// ownership — treat as a graceful close (return nil) rather
	// than leaking a context error from the persistence seam.
	row, err := s.store.GetQuest(ctx, userID, questID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return mapStoreNotFound(err)
	}
	// If the quest is already terminal we can short-circuit after
	// the replay: there will never be a live emission for it.
	terminal := row.Status.IsTerminal()

	// 2. Replay history. limit=0 means "no limit" at the store
	// seam; the quest_events table is bounded by a quest's
	// lifetime so an unbounded scan is acceptable for M1-13.
	//
	// A ctx-canceled error from the store is the caller already
	// having walked away — return nil (graceful close), not the
	// underlying context error, because SubscribeQuest's contract
	// is "ctx.Done is the normal close signal, not an error".
	history, err := s.store.ListQuestEvents(ctx, userID, questID, 0)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return fmt.Errorf("SubscribeQuest replay: %w", err)
	}
	for _, ev := range history {
		// Honor ctx between replay events: a long history with a
		// disconnecting client should not be forced to drain.
		if cerr := ctx.Err(); cerr != nil {
			return nil
		}
		cb(ev)
	}

	// Already-terminal quest: nothing live can arrive. We do not
	// block on ctx — the caller wants the history and then a
	// natural close.
	if terminal {
		return nil
	}

	// 3. Subscribe to the engine. Use a buffered channel so the
	// engine's emit path is never blocked by a slow service cb
	// (engine.Subscribe documents subscribers MUST NOT block). The
	// buffer is generous enough to absorb a burst from a normal
	// snipe run (Submitted -> Scheduled -> Released -> Found ->
	// Booked is five events; 64 gives ample headroom).
	const liveBuf = 64
	live := make(chan domain.Event, liveBuf)
	cancel := s.engine.Subscribe(func(n engine.Notification) {
		if domain.QuestID(n.SnipeID) != questID {
			return
		}
		// Non-blocking send: if the buffer is saturated the
		// subscriber drops the event rather than stall the
		// engine. A dropped event surfaces as a gap in the
		// stream; transports that need at-most-once-with-replay
		// can reconnect and pull history from the store.
		select {
		case live <- n.Event:
		default:
			s.logger.Warn("subscribe_quest.live_buffer_full",
				slog.String("user", string(userID)),
				slog.String("quest", string(questID)),
				slog.String("event", string(n.Event.Type)),
			)
		}
	})
	defer cancel()

	// 4. Pump. The subscriber runs on the engine's emit goroutine;
	// cb runs here, on the caller's goroutine, in arrival order.
	for {
		select {
		case <-ctx.Done():
			// ctx cancellation is the documented graceful close
			// signal for SubscribeQuest. Return nil so transports
			// can treat "client went away" as a non-error.
			return nil
		case ev, ok := <-live:
			if !ok {
				// The channel is owned by this method — it never
				// closes from the engine side. This branch is
				// defensive: a future refactor that closes the
				// channel can use it as a "stop streaming" signal.
				return nil
			}
			cb(ev)
			// Terminal-event short-circuit: once the quest reaches
			// a final state no more emissions will arrive, so we
			// can release the subscription early instead of
			// pinning resources until ctx.Done.
			if isTerminalEvent(ev.Type) {
				return nil
			}
		}
	}
}

// isTerminalEvent reports whether ev marks the snipe's lifecycle as
// closed. Mirrors domain.Status.IsTerminal but on the event side so
// SubscribeQuest does not need to load the snipe state after every
// emission to decide whether to stop pumping.
func isTerminalEvent(t domain.EventType) bool {
	switch t {
	case domain.EventBooked,
		domain.EventFailed,
		domain.EventCanceled,
		domain.EventExpired:
		return true
	default:
		return false
	}
}

// defaultEventLimit caps the number of quest_events rows GetQuest
// returns. 50 mirrors the design-doc "most-recent N" sketch and is
// well below the size at which a typical SSE/MCP client begins to
// chunk.
const defaultEventLimit = 50

// summaryFromRow shrinks a QuestRow to its API-shaped QuestSummary.
func summaryFromRow(r QuestRow) QuestSummary {
	return QuestSummary{
		ID:          r.ID,
		UserID:      r.UserID,
		AccountID:   r.AccountID,
		Status:      r.Status,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
		CompletedAt: r.CompletedAt,
		PlanHash:    r.PlanHash,
	}
}

// findAccountByID is the AccountID-shape fallback for CreateQuest:
// when goal.AccountID carries the canonical acct_… id (not the email)
// the email lookup misses. We re-list and find by ID.
func (s *Standard) findAccountByID(ctx context.Context, userID domain.UserID, id domain.AccountID) (AccountRow, error) {
	accts, err := s.store.ListAccounts(ctx, userID)
	if err != nil {
		return AccountRow{}, err
	}
	for _, a := range accts {
		if a.ID == id {
			return a, nil
		}
	}
	return AccountRow{}, ErrNotFound
}

// mapResolverError translates resolver-local sentinels into service
// sentinels. The two error vocabularies are intentionally distinct
// (Law 9 — sentinel errors at package boundaries); this is the seam.
func mapResolverError(err error) error {
	switch {
	case errors.Is(err, resolver.ErrVenueNotFound):
		return fmt.Errorf("%w: %w", ErrVenueNotFound, err)
	case errors.Is(err, resolver.ErrVenueAmbiguous):
		return fmt.Errorf("%w: %w", ErrVenueAmbiguous, err)
	case errors.Is(err, resolver.ErrInvalidURL),
		errors.Is(err, resolver.ErrNotResyHost),
		errors.Is(err, resolver.ErrNotVenueURL):
		return fmt.Errorf("%w: %w", ErrInvalidArgument, err)
	default:
		return fmt.Errorf("%w: %w", ErrUpstreamUnavailable, err)
	}
}

// mapStoreNotFound translates a StoreBackend ErrNotFound into the
// service ErrNotFound. Other errors pass through verbatim wrapped.
func mapStoreNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotFound) {
		return err
	}
	// Treat the StoreBackend's "row not found" as our ErrNotFound. The
	// StoreBackend adapter is responsible for surfacing
	// store.ErrNotFound as service.ErrNotFound at its seam; if that
	// adapter forgets, this fallback still preserves the API contract.
	return err
}

// intentFromPlan builds a domain.Intent from the planner's Plan.
// The Intent is what the engine consumes (Plan is the user-facing
// approvable artifact); the two share Goal/Venue/Date but diverge on
// the slot/release representation.
//
// SlotPrefs are expanded from the goal's TimePrefs; the planner has
// already ordered the FireSchedule but the Intent's SlotPrefs list
// is the engine's working set. For M1-10 we emit one SlotPreference
// per Goal.TimePrefs entry, defaulting TableType to "" (any) when no
// constraint applies.
func intentFromPlan(userID domain.UserID, plan domain.Plan, goal domain.Goal) domain.Intent {
	slot := domain.SlotPreference{
		Time:      goal.TimePrefs.Start,
		TableType: "",
	}
	if !goal.Constraints.AnyTable && len(goal.Constraints.TableTypes) > 0 {
		slot.TableType = goal.Constraints.TableTypes[0]
	}
	return domain.Intent{
		User:      userID,
		Venue:     plan.Venue.AsRef(),
		Date:      goal.Date,
		PartySize: goal.Party,
		SlotPrefs: []domain.SlotPreference{slot},
		Release:   plan.Strategy,
	}
}
