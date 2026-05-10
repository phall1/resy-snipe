package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/planner"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resolver"
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
// Idempotency (CreateOpts.IdempotencyKey) is accepted but not yet
// enforced — M1-12 wires the dedup table.
func (s *Standard) CreateQuest(
	ctx context.Context,
	userID domain.UserID,
	goal domain.Goal,
	opts CreateOpts,
) (domain.QuestID, error) {
	if userID == "" {
		return "", fmt.Errorf("CreateQuest: %w: userID is required", ErrInvalidArgument)
	}

	// PlanQuest validates the goal and resolves the venue — running
	// it here means a hash mismatch fires before any persistence.
	plan, err := s.PlanQuest(ctx, userID, goal)
	if err != nil {
		return "", err
	}
	if opts.PlanHash != nil && *opts.PlanHash != plan.Hash {
		return "", fmt.Errorf("CreateQuest: %w (caller=%q service=%q)",
			ErrInvalidPlanHash, *opts.PlanHash, plan.Hash)
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
				return "", fmt.Errorf("CreateQuest account %s: %w", goal.AccountID, ErrNotFound)
			}
		} else {
			return "", fmt.Errorf("CreateQuest: %w", err)
		}
	}

	// TODO(M1-12): if opts.IdempotencyKey != nil, look up a prior
	// response in idempotency_keys before minting a fresh ID.
	_ = opts.IdempotencyKey

	questID, err := newQuestID()
	if err != nil {
		return "", fmt.Errorf("CreateQuest: %w", err)
	}

	goalJSON, err := marshalGoalJSON(goal)
	if err != nil {
		return "", fmt.Errorf("CreateQuest: %w", err)
	}

	now := s.clock.Now()
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
		return "", fmt.Errorf("CreateQuest persist: %w", err)
	}

	// TODO(M1-11): audit_events.write(user=userID, action=create_quest,
	// target=questID, ok=true).

	intent := intentFromPlan(userID, plan, goal)
	if _, err := s.engine.Submit(ctx, domain.SnipeID(questID), intent); err != nil {
		// The quest row is already persisted; leaving it visible with
		// status=Submitted (pending) is correct — a follow-up retry
		// can re-submit. We surface the engine error so the caller
		// knows scheduling failed.
		return questID, fmt.Errorf("CreateQuest engine submit: %w", err)
	}
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
	row, err := s.store.GetQuest(ctx, userID, questID)
	if err != nil {
		return mapStoreNotFound(err)
	}
	if row.Status.IsTerminal() {
		// Re-canceling a terminal quest is not an error — the API
		// contract is idempotent (docs/v2/design/service-layer.md
		// §error-model). We do not stamp a fresh completed_at.
		return nil
	}

	now := s.clock.Now()
	if err := s.store.UpdateQuestStatus(ctx, userID, questID, domain.StatusCanceled, &now, now); err != nil {
		return fmt.Errorf("CancelQuest: %w", mapStoreNotFound(err))
	}

	// TODO(M1-13/engine-cancel): signal the engine so an in-flight
	// snipe aborts its scheduler loop. engine.Engine currently has no
	// public Cancel(SnipeID) surface; the DB flip is the only
	// observable effect for now.
	// TODO(M1-11): audit_events.write(user=userID, action=cancel_quest,
	// target=questID, ok=true, reason=opts.Reason).
	_ = opts

	return nil
}

// SubscribeQuest is wired in M1-13. M1-10 returns ErrNotImplemented
// so callers can compile against the Service surface.
func (s *Standard) SubscribeQuest(
	_ context.Context,
	_ domain.UserID,
	_ domain.QuestID,
	_ func(domain.Event),
) error {
	// TODO(M1-13): wrap engine.Subscribe, filter by (userID, questID),
	// replay quest_events history, then bridge live notifications.
	return ErrNotImplemented
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
