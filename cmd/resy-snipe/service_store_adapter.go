package main

import (
	"context"
	"errors"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
	"resy-snipe/internal/store"
)

// serviceStoreAdapter binds a *store.SQLiteStore to the
// service.StoreBackend interface defined-at-the-consumer per Law 5.
// internal/service owns the contract; the store package owns the
// SQL implementations; this adapter — owned by the cmd/ wiring layer
// — bridges them. internal/service does not import internal/store
// directly, keeping the dependency arrow pointing from cmd/ → service
// → store and never the other way.
//
// The adapter is mechanically straightforward: every method translates
// the service-side row shape into store.QuestRow / store.AccountRow,
// invokes the package-level store function, and translates back.
// store.ErrNotFound becomes service.ErrNotFound on every reader.
type serviceStoreAdapter struct {
	store *store.SQLiteStore
}

// newServiceStoreAdapter wires the adapter. The store must be
// non-nil — a nil store is a programming error and panics at boot
// rather than nil-derefing on first call.
func newServiceStoreAdapter(s *store.SQLiteStore) *serviceStoreAdapter {
	if s == nil {
		panic("newServiceStoreAdapter: nil store")
	}
	return &serviceStoreAdapter{store: s}
}

// Compile-time interface check (Law 6).
var _ service.StoreBackend = (*serviceStoreAdapter)(nil)

// CreateQuest delegates to store.CreateQuest, translating the
// service-side row shape into the store-side row shape verbatim.
func (a *serviceStoreAdapter) CreateQuest(ctx context.Context, q service.QuestRow) error {
	return store.CreateQuest(ctx, a.store.DB(), store.QuestRow{
		ID:          q.ID,
		UserID:      q.UserID,
		AccountID:   q.AccountID,
		GoalJSON:    q.GoalJSON,
		PlanHash:    q.PlanHash,
		Status:      q.Status,
		CreatedAt:   q.CreatedAt,
		UpdatedAt:   q.UpdatedAt,
		CompletedAt: q.CompletedAt,
	})
}

// GetQuest delegates to store.GetQuest and maps store.ErrNotFound to
// service.ErrNotFound at the seam.
func (a *serviceStoreAdapter) GetQuest(
	ctx context.Context,
	userID domain.UserID,
	questID domain.QuestID,
) (service.QuestRow, error) {
	r, err := store.GetQuest(ctx, a.store.DB(), userID, questID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return service.QuestRow{}, service.ErrNotFound
		}
		return service.QuestRow{}, err
	}
	return toServiceQuestRow(r), nil
}

// ListQuests delegates to store.ListQuests, translating the service
// ListFilter into the store-side QuestListFilter.
func (a *serviceStoreAdapter) ListQuests(
	ctx context.Context,
	userID domain.UserID,
	filter service.ListFilter,
) ([]service.QuestRow, error) {
	rows, err := store.ListQuests(ctx, a.store.DB(), userID, store.QuestListFilter{
		Status:    filter.Status,
		AccountID: filter.AccountID,
		Since:     filter.Since,
		Until:     filter.Until,
		Limit:     filter.Limit,
		Cursor:    filter.Cursor,
	})
	if err != nil {
		return nil, err
	}
	out := make([]service.QuestRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toServiceQuestRow(r))
	}
	return out, nil
}

// UpdateQuestStatus delegates to store.UpdateQuestStatus and folds
// store.ErrNotFound into service.ErrNotFound.
func (a *serviceStoreAdapter) UpdateQuestStatus(
	ctx context.Context,
	userID domain.UserID,
	questID domain.QuestID,
	newStatus domain.Status,
	completedAt *time.Time,
	updatedAt time.Time,
) error {
	err := store.UpdateQuestStatus(ctx, a.store.DB(), userID, questID, newStatus, completedAt, updatedAt)
	if err != nil && errors.Is(err, store.ErrNotFound) {
		return service.ErrNotFound
	}
	return err
}

// ListAccounts delegates to store.ListAccountsForUser.
func (a *serviceStoreAdapter) ListAccounts(
	ctx context.Context,
	userID domain.UserID,
) ([]service.AccountRow, error) {
	rows, err := store.ListAccountsForUser(ctx, a.store.DB(), userID)
	if err != nil {
		return nil, err
	}
	out := make([]service.AccountRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toServiceAccountRow(r))
	}
	return out, nil
}

// GetAccountByEmail delegates to store.GetAccountByEmail and folds
// store.ErrNotFound into service.ErrNotFound.
func (a *serviceStoreAdapter) GetAccountByEmail(
	ctx context.Context,
	userID domain.UserID,
	email string,
) (service.AccountRow, error) {
	r, err := store.GetAccountByEmail(ctx, a.store.DB(), userID, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return service.AccountRow{}, service.ErrNotFound
		}
		return service.AccountRow{}, err
	}
	return toServiceAccountRow(r), nil
}

// BindAccountToUser delegates to store.BindLegacyAccountToUser and
// folds store.ErrNotFound into service.ErrNotFound.
func (a *serviceStoreAdapter) BindAccountToUser(
	ctx context.Context,
	userID domain.UserID,
	email string,
) (domain.AccountID, error) {
	id, err := store.BindLegacyAccountToUser(ctx, a.store.DB(), userID, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", service.ErrNotFound
		}
		return "", err
	}
	return id, nil
}

// ListEvents is a stub for M1-10. M1-11 wires quest_events writes;
// until then GetQuest returns an empty event list. The interface
// method exists so the Service contract is stable.
func (a *serviceStoreAdapter) ListEvents(
	_ context.Context,
	_ domain.UserID,
	_ domain.QuestID,
	_ int,
) ([]service.EventRow, error) {
	// TODO(M1-11): SELECT type, at, fields_json FROM quest_events
	// WHERE user_id = ? AND quest_id = ? ORDER BY at DESC LIMIT ?.
	return nil, nil
}

// ListQuestEvents delegates to store.ListQuestEvents. The store
// function already filters on user_id at the SQL level, so the
// adapter is a pass-through with no error translation needed —
// store.ListQuestEvents never returns ErrNotFound (an unknown quest
// produces an empty slice, consistent with the design's fold of
// existence into the SubscribeQuest GetQuest gate).
func (a *serviceStoreAdapter) ListQuestEvents(
	ctx context.Context,
	userID domain.UserID,
	questID domain.QuestID,
	limit int,
) ([]domain.Event, error) {
	return store.ListQuestEvents(ctx, a.store.DB(), userID, questID, limit)
}

// GetIdempotencyResult delegates to store.GetIdempotencyResult,
// repacking the store-side store.IdempotencyLookup into the
// service-side service.IdempotencyLookup. The store function reads
// no clock of its own — `now` is the caller's idea of the present
// and the only input that drives the Expired flag (Law 7).
func (a *serviceStoreAdapter) GetIdempotencyResult(
	ctx context.Context,
	userID domain.UserID,
	scope, plaintextKey, payloadHash string,
	now time.Time,
) (service.IdempotencyLookup, error) {
	lookup, err := store.GetIdempotencyResult(ctx, a.store.DB(), userID, scope, plaintextKey, payloadHash, now)
	if err != nil {
		return service.IdempotencyLookup{}, err
	}
	return service.IdempotencyLookup{
		Found:        lookup.Found,
		PayloadMatch: lookup.PayloadMatch,
		Expired:      lookup.Expired,
		TargetID:     lookup.TargetID,
		PrevErr:      lookup.ResultErr,
	}, nil
}

// PutIdempotencyResult delegates to store.PutIdempotencyResult.
// errVal may be nil (success row) or any error (the store persists
// errVal.Error()).
func (a *serviceStoreAdapter) PutIdempotencyResult(
	ctx context.Context,
	userID domain.UserID,
	scope, plaintextKey, payloadHash string,
	targetID string,
	errVal error,
	ttl time.Duration,
	now time.Time,
) error {
	return store.PutIdempotencyResult(ctx, a.store.DB(), userID, scope, plaintextKey, payloadHash, targetID, errVal, ttl, now)
}

// WriteAuditEvent delegates to store.WriteAuditEvent (M1-11). The
// adapter repacks the service-side AuditWrite into the store-side
// AuditEventInput verbatim — the two shapes carry the same fields
// (the consumer-defined-at-the-Service-layer projection from Law 5).
func (a *serviceStoreAdapter) WriteAuditEvent(ctx context.Context, evt service.AuditWrite) error {
	var target *domain.UserID
	if evt.TargetUserID != nil {
		t := *evt.TargetUserID
		target = &t
	}
	return store.WriteAuditEvent(ctx, a.store.DB(), store.AuditEventInput{
		UserID:       evt.UserID,
		TargetUserID: target,
		Action:       evt.Action,
		TargetID:     evt.TargetID,
		OK:           evt.OK,
		ErrorCode:    evt.ErrorCode,
		IP:           evt.IP,
		UserAgent:    evt.UserAgent,
		DetailsJSON:  evt.DetailsJSON,
	})
}

// WriteQuestEvent delegates to store.WriteQuestEvent (M1-11). The
// store helper gates the INSERT on quests.user_id at the SQL level
// so the adapter is a pure pass-through.
func (a *serviceStoreAdapter) WriteQuestEvent(
	ctx context.Context,
	userID domain.UserID,
	questID domain.QuestID,
	evt domain.Event,
) error {
	return store.WriteQuestEvent(ctx, a.store.DB(), userID, questID, evt)
}

// InsertToken delegates to store.InsertToken, repacking the service-
// side TokenRecord into the store-side TokenRow. Field-for-field
// passthrough — the two shapes carry the same columns by design.
func (a *serviceStoreAdapter) InsertToken(ctx context.Context, t service.TokenRecord) error {
	return store.InsertToken(ctx, a.store.DB(), store.TokenRow{
		ID:        t.ID,
		UserID:    t.UserID,
		Hash:      t.Hash,
		Scopes:    t.Scope,
		Label:     t.Label,
		CreatedAt: t.CreatedAt,
		LastSeen:  t.LastSeen,
		RevokedAt: t.RevokedAt,
	})
}

// ListTokensForUser delegates to store.ListTokensForUser and repacks
// each row into the service-side projection.
func (a *serviceStoreAdapter) ListTokensForUser(
	ctx context.Context,
	userID domain.UserID,
) ([]service.TokenRecord, error) {
	rows, err := store.ListTokensForUser(ctx, a.store.DB(), userID)
	if err != nil {
		return nil, err
	}
	out := make([]service.TokenRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, service.TokenRecord{
			ID:        r.ID,
			UserID:    r.UserID,
			Hash:      r.Hash,
			Scope:     r.Scopes,
			Label:     r.Label,
			CreatedAt: r.CreatedAt,
			LastSeen:  r.LastSeen,
			RevokedAt: r.RevokedAt,
		})
	}
	return out, nil
}

// RevokeToken delegates to store.RevokeToken and folds
// store.ErrNotFound into service.ErrNotFound.
func (a *serviceStoreAdapter) RevokeToken(
	ctx context.Context,
	userID domain.UserID,
	tokenID string,
	now time.Time,
) error {
	err := store.RevokeToken(ctx, a.store.DB(), userID, tokenID, now)
	if err != nil && errors.Is(err, store.ErrNotFound) {
		return service.ErrNotFound
	}
	return err
}

// CreateSubscription delegates to store.CreateSubscription.
func (a *serviceStoreAdapter) CreateSubscription(ctx context.Context, row service.SubscriptionRow) error {
	return store.CreateSubscription(ctx, a.store.DB(), store.SubscriptionRow{
		ID:             row.ID,
		UserID:         row.UserID,
		GoalJSON:       row.GoalJSON,
		Status:         row.Status,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
		ExpiresAt:      row.ExpiresAt,
		FulfilledBy:    row.FulfilledBy,
		CompromiseJSON: row.CompromiseJSON,
		PollInterval:   row.PollInterval,
		NextPollAt:     row.NextPollAt,
	})
}

// GetSubscription delegates to store.GetSubscription and folds
// store.ErrNotFound into service.ErrNotFound.
func (a *serviceStoreAdapter) GetSubscription(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID) (service.SubscriptionRow, error) {
	r, err := store.GetSubscription(ctx, a.store.DB(), userID, subID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return service.SubscriptionRow{}, service.ErrNotFound
		}
		return service.SubscriptionRow{}, err
	}
	return toServiceSubscriptionRow(r), nil
}

// ListSubscriptions delegates to store.ListSubscriptions.
func (a *serviceStoreAdapter) ListSubscriptions(ctx context.Context, userID domain.UserID, filter service.SubscriptionFilter) ([]service.SubscriptionRow, error) {
	rows, err := store.ListSubscriptions(ctx, a.store.DB(), userID, store.SubscriptionListFilter{
		Status: filter.Status,
		Limit:  filter.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]service.SubscriptionRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toServiceSubscriptionRow(r))
	}
	return out, nil
}

// UpdateSubscriptionStatus delegates to store.UpdateSubscriptionStatus and
// folds store.ErrNotFound into service.ErrNotFound.
func (a *serviceStoreAdapter) UpdateSubscriptionStatus(
	ctx context.Context,
	userID domain.UserID,
	subID domain.SubscriptionID,
	newStatus domain.SubscriptionStatus,
	fulfilledBy *domain.QuestID,
	nextPollAt *time.Time,
	updatedAt time.Time,
) error {
	err := store.UpdateSubscriptionStatus(ctx, a.store.DB(), userID, subID, newStatus, fulfilledBy, nextPollAt, updatedAt)
	if err != nil && errors.Is(err, store.ErrNotFound) {
		return service.ErrNotFound
	}
	return err
}

func toServiceQuestRow(r store.QuestRow) service.QuestRow {
	return service.QuestRow{
		ID:          r.ID,
		UserID:      r.UserID,
		AccountID:   r.AccountID,
		GoalJSON:    r.GoalJSON,
		PlanHash:    r.PlanHash,
		Status:      r.Status,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
		CompletedAt: r.CompletedAt,
	}
}

func toServiceAccountRow(r store.AccountRow) service.AccountRow {
	return service.AccountRow{
		ID:          r.ID,
		UserID:      r.UserID,
		ResyEmail:   r.ResyEmail,
		DisplayName: r.DisplayName,
		CreatedAt:   r.CreatedAt,
		DisabledAt:  r.DisabledAt,
	}
}

func toServiceSubscriptionRow(r store.SubscriptionRow) service.SubscriptionRow {
	return service.SubscriptionRow{
		ID:             r.ID,
		UserID:         r.UserID,
		GoalJSON:       r.GoalJSON,
		Status:         r.Status,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
		ExpiresAt:      r.ExpiresAt,
		FulfilledBy:    r.FulfilledBy,
		CompromiseJSON: r.CompromiseJSON,
		PollInterval:   r.PollInterval,
		NextPollAt:     r.NextPollAt,
	}
}
