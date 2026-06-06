package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"resy-snipe/internal/domain"
)

const (
	defaultHotPollInterval  = 90 * time.Second
	defaultColdPollInterval = 5 * time.Minute
)

// CreateSubscription persists a subscription hunt.
func (s *Standard) CreateSubscription(
	ctx context.Context,
	userID domain.UserID,
	goal domain.Goal,
	compromise *domain.CompromisePolicy,
	expiresAt *time.Time,
) (sid domain.SubscriptionID, retErr error) {
	if userID == "" {
		return "", fmt.Errorf("CreateSubscription: %w: userID is required", ErrInvalidArgument)
	}
	defer func() {
		s.audit(ctx, userID, actionSubscriptionCreate, string(sid), retErr)
	}()
	now := s.clock.Now()
	if err := goal.Validate(now); err != nil {
		wrapped := fmt.Errorf("CreateSubscription: %w: %w", ErrInvalidArgument, err)
		return "", wrapped
	}

	subID, err := newSubscriptionID()
	if err != nil {
		return "", fmt.Errorf("CreateSubscription: %w", err)
	}

	goalJSON, err := marshalGoalJSON(goal)
	if err != nil {
		return "", fmt.Errorf("CreateSubscription: %w", err)
	}

	var compromiseJSON string
	if compromise != nil {
		b, err := json.Marshal(compromise)
		if err != nil {
			return "", fmt.Errorf("CreateSubscription: %w", err)
		}
		compromiseJSON = string(b)
	}

	pollInterval := defaultColdPollInterval
	if goal.Date.In(time.UTC).Sub(now) <= 7*24*time.Hour {
		pollInterval = defaultHotPollInterval
	}

	row := SubscriptionRow{
		ID:             subID,
		UserID:         userID,
		GoalJSON:       goalJSON,
		Status:         domain.SubscriptionActive,
		CreatedAt:      now,
		UpdatedAt:      now,
		ExpiresAt:      expiresAt,
		CompromiseJSON: compromiseJSON,
		PollInterval:   pollInterval,
		NextPollAt:     now.Add(pollInterval),
	}
	if err := s.store.CreateSubscription(ctx, row); err != nil {
		return "", fmt.Errorf("CreateSubscription persist: %w", err)
	}

	return subID, nil
}

// GetSubscription returns a subscription the caller owns.
func (s *Standard) GetSubscription(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID) (sub Subscription, retErr error) {
	if userID == "" {
		return Subscription{}, fmt.Errorf("GetSubscription: %w: userID is required", ErrInvalidArgument)
	}
	defer func() {
		s.audit(ctx, userID, actionSubscriptionGet, string(subID), retErr)
	}()
	row, err := s.store.GetSubscription(ctx, userID, subID)
	if err != nil {
		return Subscription{}, mapStoreNotFound(err)
	}
	sub, err = toServiceSubscription(row)
	if err != nil {
		return Subscription{}, fmt.Errorf("GetSubscription: %w", err)
	}
	return sub, nil
}

// ListSubscriptions returns the caller's subscriptions, narrowed by filter.
func (s *Standard) ListSubscriptions(ctx context.Context, userID domain.UserID, filter SubscriptionFilter) (out []Subscription, retErr error) {
	if userID == "" {
		return nil, fmt.Errorf("ListSubscriptions: %w: userID is required", ErrInvalidArgument)
	}
	defer func() {
		s.audit(ctx, userID, actionSubscriptionList, "", retErr)
	}()
	rows, err := s.store.ListSubscriptions(ctx, userID, filter)
	if err != nil {
		return nil, fmt.Errorf("ListSubscriptions: %w", err)
	}
	out = make([]Subscription, 0, len(rows))
	for _, r := range rows {
		sub, err := toServiceSubscription(r)
		if err != nil {
			return nil, fmt.Errorf("ListSubscriptions: %w", err)
		}
		out = append(out, sub)
	}
	return out, nil
}

// CancelSubscription transitions a subscription to Cancelled.
func (s *Standard) CancelSubscription(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID) (retErr error) {
	if userID == "" {
		return fmt.Errorf("CancelSubscription: %w: userID is required", ErrInvalidArgument)
	}
	defer func() {
		s.audit(ctx, userID, actionSubscriptionCancel, string(subID), retErr)
	}()
	row, err := s.store.GetSubscription(ctx, userID, subID)
	if err != nil {
		return mapStoreNotFound(err)
	}
	if row.Status.IsTerminal() {
		return nil
	}
	now := s.clock.Now()
	if err := s.store.UpdateSubscriptionStatus(ctx, userID, subID, domain.SubscriptionCancelled, nil, nil, now); err != nil {
		return fmt.Errorf("CancelSubscription: %w", mapStoreNotFound(err))
	}
	return nil
}

// UpdateSubscriptionNextPoll updates the next_poll_at of an active subscription.
func (s *Standard) UpdateSubscriptionNextPoll(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID, nextPollAt time.Time) (retErr error) {
	if userID == "" {
		return fmt.Errorf("UpdateSubscriptionNextPoll: %w: userID is required", ErrInvalidArgument)
	}
	defer func() {
		s.audit(ctx, userID, actionSubscriptionUpdate, string(subID), retErr)
	}()
	now := s.clock.Now()
	if err := s.store.UpdateSubscriptionStatus(ctx, userID, subID, domain.SubscriptionActive, nil, &nextPollAt, now); err != nil {
		return fmt.Errorf("UpdateSubscriptionNextPoll: %w", mapStoreNotFound(err))
	}
	return nil
}

// PauseSubscription transitions a subscription to Paused (used on auth expiry).
func (s *Standard) PauseSubscription(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID) (retErr error) {
	if userID == "" {
		return fmt.Errorf("PauseSubscription: %w: userID is required", ErrInvalidArgument)
	}
	defer func() {
		s.audit(ctx, userID, actionSubscriptionPause, string(subID), retErr)
	}()
	now := s.clock.Now()
	if err := s.store.UpdateSubscriptionStatus(ctx, userID, subID, domain.SubscriptionPaused, nil, nil, now); err != nil {
		return fmt.Errorf("PauseSubscription: %w", mapStoreNotFound(err))
	}
	return nil
}

// FulfillSubscription transitions a subscription to Fulfilled and records the quest ID.
func (s *Standard) FulfillSubscription(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID, questID domain.QuestID) (retErr error) {
	if userID == "" {
		return fmt.Errorf("FulfillSubscription: %w: userID is required", ErrInvalidArgument)
	}
	defer func() {
		s.audit(ctx, userID, actionSubscriptionFulfilled, string(subID), retErr)
	}()
	now := s.clock.Now()
	if err := s.store.UpdateSubscriptionStatus(ctx, userID, subID, domain.SubscriptionFulfilled, &questID, nil, now); err != nil {
		return fmt.Errorf("FulfillSubscription: %w", mapStoreNotFound(err))
	}
	return nil
}

// ExpireSubscription transitions a subscription to Expired.
func (s *Standard) ExpireSubscription(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID) (retErr error) {
	if userID == "" {
		return fmt.Errorf("ExpireSubscription: %w: userID is required", ErrInvalidArgument)
	}
	defer func() {
		s.audit(ctx, userID, actionSubscriptionExpired, string(subID), retErr)
	}()
	now := s.clock.Now()
	if err := s.store.UpdateSubscriptionStatus(ctx, userID, subID, domain.SubscriptionExpired, nil, nil, now); err != nil {
		return fmt.Errorf("ExpireSubscription: %w", mapStoreNotFound(err))
	}
	return nil
}

// ResumeSubscription transitions a subscription from Paused to Active.
func (s *Standard) ResumeSubscription(ctx context.Context, userID domain.UserID, subID domain.SubscriptionID) (retErr error) {
	if userID == "" {
		return fmt.Errorf("ResumeSubscription: %w: userID is required", ErrInvalidArgument)
	}
	defer func() {
		s.audit(ctx, userID, actionSubscriptionResume, string(subID), retErr)
	}()
	now := s.clock.Now()
	if err := s.store.UpdateSubscriptionStatus(ctx, userID, subID, domain.SubscriptionActive, nil, nil, now); err != nil {
		return fmt.Errorf("ResumeSubscription: %w", mapStoreNotFound(err))
	}
	return nil
}

func newSubscriptionID() (domain.SubscriptionID, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("newSubscriptionID: %w", err)
	}
	return domain.SubscriptionID("sub_" + hex.EncodeToString(buf[:])), nil
}

func toServiceSubscription(row SubscriptionRow) (Subscription, error) {
	goal, err := unmarshalGoalJSON(row.GoalJSON)
	if err != nil {
		return Subscription{}, fmt.Errorf("decode goal: %w", err)
	}
	var compromise *domain.CompromisePolicy
	if row.CompromiseJSON != "" {
		var cp domain.CompromisePolicy
		if err := json.Unmarshal([]byte(row.CompromiseJSON), &cp); err != nil {
			return Subscription{}, fmt.Errorf("decode compromise: %w", err)
		}
		compromise = &cp
	}
	return Subscription{
		ID:           row.ID,
		UserID:       row.UserID,
		Goal:         goal,
		Status:       row.Status,
		CreatedAt:    row.CreatedAt,
		ExpiresAt:    row.ExpiresAt,
		FulfilledBy:  row.FulfilledBy,
		Compromise:   compromise,
		PollInterval: row.PollInterval,
		NextPollAt:   row.NextPollAt,
	}, nil
}
