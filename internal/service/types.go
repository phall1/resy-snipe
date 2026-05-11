package service

import (
	"time"

	"resy-snipe/internal/domain"
)

// CreateOpts carries optional knobs for CreateQuest. PlanHash, when
// non-nil, pins the call to a plan previously returned by PlanQuest:
// the Service recomputes the Plan and refuses if the hash does not
// match, preventing TOCTOU between plan-and-commit (ADR-0012).
// IdempotencyKey, when non-nil, makes the call replayable within the
// retention window.
type CreateOpts struct {
	PlanHash       *string
	IdempotencyKey *string
}

// CancelOpts carries optional knobs for CancelQuest. Reason is a
// short free-form annotation stored on the audit row. IdempotencyKey
// behaves identically to CreateOpts.IdempotencyKey.
type CancelOpts struct {
	Reason         string
	IdempotencyKey *string
}

// ListFilter narrows the set of quests returned by ListQuests. Empty
// fields are treated as wildcards. Limit and Cursor are the page
// controls; the transport surfaces them in its own pagination
// envelope.
type ListFilter struct {
	Status    []domain.Status
	AccountID *domain.AccountID
	Since     *time.Time
	Until     *time.Time
	Limit     int
	Cursor    string
}

// QuestSummary is the lightweight projection ListQuests returns. The
// full state (Goal, Plan, Events) is fetched by GetQuest.
type QuestSummary struct {
	ID          domain.QuestID
	UserID      domain.UserID
	AccountID   domain.AccountID
	Status      domain.Status
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
	PlanHash    string
}

// QuestState is the full view GetQuest returns: the summary, the
// original Goal, the most recent Plan, and the bounded event history.
type QuestState struct {
	Summary QuestSummary
	Goal    domain.Goal
	Plan    domain.Plan
	Events  []domain.Event
}

// Account is a Resy account bound to a homelab user. Secrets are not
// exposed at this boundary.
type Account struct {
	ID          domain.AccountID
	UserID      domain.UserID
	Email       string
	DisplayName string
	CreatedAt   time.Time
	DisabledAt  *time.Time
}

// User is a homelab tenant. Role is the access tier ("operator" or
// "tenant"). DisabledAt, when non-nil, marks a soft-deleted account.
type User struct {
	ID         domain.UserID
	Email      string
	Role       string
	CreatedAt  time.Time
	DisabledAt *time.Time
}

// Invite is the artifact returned by InviteUser. Token is the
// plaintext invite-link payload — it is delivered to the invitee out
// of band and is not stored server-side beyond a salted hash.
type Invite struct {
	Token     string
	Email     string
	Role      string
	ExpiresAt time.Time
}

// BearerToken is the credential IssueToken returns. Token is the
// plaintext value, returned exactly once at issuance — only its hash
// is stored. ID is the public ULID handle (the path parameter for
// DELETE /v1/auth/tokens/{id}); Scope is the role tier the daemon
// enforces at the Service layer ('user' or 'operator').
type BearerToken struct {
	ID        string
	Token     string
	Label     string
	Scope     string
	CreatedAt time.Time
}

// Token is the listing projection — what GET /v1/auth/tokens (and the
// CLI's `user list-tokens`) surface. The plaintext bearer is NOT in
// this shape; the daemon stores only its hash. LastSeen is nil until
// the token has been used for authentication at least once;
// RevokedAt is nil while the token is live.
type Token struct {
	ID        string
	UserID    domain.UserID
	Scope     string
	Label     string
	CreatedAt time.Time
	LastSeen  *time.Time
	RevokedAt *time.Time
}
