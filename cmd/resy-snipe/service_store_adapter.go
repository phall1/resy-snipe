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
//
//nolint:unused // wired in newStandardService; future M1-18+ CLI quest verbs call into it.
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
