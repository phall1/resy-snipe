package service

import (
	"context"

	"resy-snipe/internal/domain"
)

// Stub is a Service implementation whose every method returns
// ErrNotImplemented. It exists so consumers (transports, cmd/) can
// compile against the Service interface before the real composition
// lands. Production wiring constructs the real Service, not Stub.
type Stub struct{}

// Compile-time interface check (Law 6).
var _ Service = (*Stub)(nil)

// ResolveVenue implements Service.
func (Stub) ResolveVenue(_ context.Context, _ domain.UserID, _ domain.VenueQuery) (domain.Venue, error) {
	return domain.Venue{}, ErrNotImplemented
}

// PlanQuest implements Service.
func (Stub) PlanQuest(_ context.Context, _ domain.UserID, _ domain.Goal) (domain.Plan, error) {
	return domain.Plan{}, ErrNotImplemented
}

// CreateQuest implements Service.
func (Stub) CreateQuest(_ context.Context, _ domain.UserID, _ domain.Goal, _ CreateOpts) (domain.QuestID, error) {
	return "", ErrNotImplemented
}

// GetQuest implements Service.
func (Stub) GetQuest(_ context.Context, _ domain.UserID, _ domain.QuestID) (QuestState, error) {
	return QuestState{}, ErrNotImplemented
}

// ListQuests implements Service.
func (Stub) ListQuests(_ context.Context, _ domain.UserID, _ ListFilter) ([]QuestSummary, error) {
	return nil, ErrNotImplemented
}

// CancelQuest implements Service.
func (Stub) CancelQuest(_ context.Context, _ domain.UserID, _ domain.QuestID, _ CancelOpts) error {
	return ErrNotImplemented
}

// SubscribeQuest implements Service.
func (Stub) SubscribeQuest(_ context.Context, _ domain.UserID, _ domain.QuestID, _ func(domain.Event)) error {
	return ErrNotImplemented
}

// Login implements Service.
func (Stub) Login(_ context.Context, _ domain.UserID, _, _ string) (domain.AccountID, error) {
	return "", ErrNotImplemented
}

// ListAccounts implements Service.
func (Stub) ListAccounts(_ context.Context, _ domain.UserID) ([]Account, error) {
	return nil, ErrNotImplemented
}

// InviteUser implements Service.
func (Stub) InviteUser(_ context.Context, _ domain.UserID, _, _ string) (Invite, error) {
	return Invite{}, ErrNotImplemented
}

// AcceptInvite implements Service.
func (Stub) AcceptInvite(_ context.Context, _, _, _ string) (domain.UserID, BearerToken, error) {
	return "", BearerToken{}, ErrNotImplemented
}

// IssueToken implements Service.
func (Stub) IssueToken(_ context.Context, _ domain.UserID, _, _ string) (BearerToken, error) {
	return BearerToken{}, ErrNotImplemented
}

// RevokeToken implements Service.
func (Stub) RevokeToken(_ context.Context, _ domain.UserID, _ string) error {
	return ErrNotImplemented
}

// ListTokens implements Service.
func (Stub) ListTokens(_ context.Context, _ domain.UserID) ([]Token, error) {
	return nil, ErrNotImplemented
}

// ListUsers implements Service.
func (Stub) ListUsers(_ context.Context, _ domain.UserID) ([]User, error) {
	return nil, ErrNotImplemented
}
