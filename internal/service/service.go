package service

import (
	"context"

	"resy-snipe/internal/domain"
)

// Service is the in-process API of the system. Every method takes
// userID as its second parameter; tenancy is enforced inside each
// implementation, never trusted from the request body. Read methods
// are idempotent; mutating methods that accept CreateOpts/CancelOpts
// honor an optional idempotency key.
type Service interface {
	// ResolveVenue turns a free-form query (URL, slug+city, or name)
	// into a canonical Venue. Pure: no persistence, no quest creation.
	ResolveVenue(ctx context.Context, userID domain.UserID, query domain.VenueQuery) (domain.Venue, error)

	// PlanQuest is a pure (Goal, Venue) → Plan computation. The
	// returned Plan carries a content hash CreateQuest will recompute
	// and verify when CreateOpts.PlanHash is supplied.
	PlanQuest(ctx context.Context, userID domain.UserID, goal domain.Goal) (domain.Plan, error)

	// CreateQuest persists a quest and schedules it for execution.
	// When opts.PlanHash is non-nil the Service recomputes the Plan
	// and refuses with ErrInvalidPlanHash on mismatch (ADR-0012).
	CreateQuest(ctx context.Context, userID domain.UserID, goal domain.Goal, opts CreateOpts) (domain.QuestID, error)

	// GetQuest returns the full state of a quest the caller owns. A
	// quest owned by another user surfaces as ErrNotFound to avoid
	// leaking cross-tenant existence.
	GetQuest(ctx context.Context, userID domain.UserID, questID domain.QuestID) (QuestState, error)

	// ListQuests returns lightweight summaries for the caller's
	// quests, narrowed by filter.
	ListQuests(ctx context.Context, userID domain.UserID, filter ListFilter) ([]QuestSummary, error)

	// CancelQuest signals the engine to abort a running quest.
	// Canceling an already-terminal quest is not an error.
	CancelQuest(ctx context.Context, userID domain.UserID, questID domain.QuestID, opts CancelOpts) error

	// SubscribeQuest blocks until ctx is canceled or the quest
	// reaches a terminal status, invoking callback once per event in
	// arrival order on the calling goroutine. The callback must not
	// block; the transport is responsible for buffering.
	SubscribeQuest(ctx context.Context, userID domain.UserID, questID domain.QuestID, callback func(domain.Event)) error

	// Login captures a Resy credential pair and persists a session
	// under the caller's user. It returns the AccountID the session
	// was stored under. Operator-only at the transport level.
	Login(ctx context.Context, userID domain.UserID, accountEmail, password string) (domain.AccountID, error)

	// ListAccounts returns the Resy accounts bound to the caller. No
	// secrets are exposed at this boundary.
	ListAccounts(ctx context.Context, userID domain.UserID) ([]Account, error)

	// InviteUser provisions a new homelab tenant and returns a
	// single-use Invite. Operator-only; tenants receive
	// ErrUnauthorized (ADR-0011).
	InviteUser(ctx context.Context, userID domain.UserID, email, role string) (Invite, error)

	// AcceptInvite redeems an Invite token, creates the tenant user,
	// and returns both the new UserID and an initial BearerToken.
	// Unauthenticated: token is the only credential.
	AcceptInvite(ctx context.Context, token, email, password string) (domain.UserID, BearerToken, error)

	// RotateToken issues a fresh bearer token for the caller under
	// the given label. Operator-only at the transport level.
	RotateToken(ctx context.Context, userID domain.UserID, label string) (BearerToken, error)

	// RevokeToken invalidates the bearer token identified by tokenID.
	// Operator-only at the transport level.
	RevokeToken(ctx context.Context, userID domain.UserID, tokenID string) error

	// ListUsers returns every homelab tenant. Operator-only at the
	// transport level.
	ListUsers(ctx context.Context, userID domain.UserID) ([]User, error)
}
