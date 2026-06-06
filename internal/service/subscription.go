package service

import (
	"time"

	"resy-snipe/internal/domain"
)

// Subscription is the consumer-side projection of a subscription row.
type Subscription struct {
	ID           domain.SubscriptionID
	UserID       domain.UserID
	Goal         domain.Goal
	Status       domain.SubscriptionStatus
	CreatedAt    time.Time
	ExpiresAt    *time.Time
	FulfilledBy  *domain.QuestID
	Compromise   *domain.CompromisePolicy
	PollInterval time.Duration
	NextPollAt   time.Time
}

// SubscriptionFilter narrows ListSubscriptions.
type SubscriptionFilter struct {
	Status []domain.SubscriptionStatus
	Limit  int
}

// SubscriptionRow is the on-disk projection the Service hands to its
// StoreBackend. Mirrors store.SubscriptionRow at the field level.
type SubscriptionRow struct {
	ID             domain.SubscriptionID
	UserID         domain.UserID
	GoalJSON       string
	Status         domain.SubscriptionStatus
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ExpiresAt      *time.Time
	FulfilledBy    *domain.QuestID
	CompromiseJSON string
	PollInterval   time.Duration
	NextPollAt     time.Time
}
