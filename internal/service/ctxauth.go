package service

import (
	"context"
	"fmt"

	"resy-snipe/internal/domain"
)

// Caller-identity context keys. The HTTP middleware (and any future
// transport) is responsible for stamping these onto every Service-
// bound request context after authenticating the bearer token. The
// Service then reads them via the typed helpers in this file rather
// than re-parsing headers — the dependency arrow points from the
// transport down into Service, never the other way.
//
// Per docs/v2/design/daemon.md §4.2, the scope dimension is
// {'user', 'operator'}: 'user' may read/modify only its own user_id's
// resources; 'operator' may reach the admin verbs (/v1/users,
// /v1/auth/tokens). The Service makes the role decision so the
// authority contract lives next to the verb it gates — the transport
// is just a courier.
type ctxKey int

const (
	ctxKeyUserID ctxKey = iota
	ctxKeyScope
)

// ContextWithCaller stamps userID and scope onto ctx. The HTTP auth
// middleware calls this once per authenticated request; tests that
// exercise a Service verb directly can use it to simulate an
// authenticated call. scope must be 'user' or 'operator' (the
// middleware validates this against the tokens.scopes CHECK
// constraint, so a malformed value here is a programming error).
func ContextWithCaller(ctx context.Context, userID domain.UserID, scope string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyUserID, userID)
	ctx = context.WithValue(ctx, ctxKeyScope, scope)
	return ctx
}

// CallerUserID returns the authenticated user id stamped on ctx, or
// the empty UserID if none was set. The Service's per-verb tenancy
// checks should pass the explicit userID parameter rather than read
// this — CallerUserID exists for transports that want to authority-
// check the URL path vs. the bearer (e.g. "the path says userID=X
// but the bearer is for userID=Y, refuse").
func CallerUserID(ctx context.Context) domain.UserID {
	v, _ := ctx.Value(ctxKeyUserID).(domain.UserID)
	return v
}

// CallerScope returns the authenticated scope stamped on ctx, or the
// empty string if none was set. The operator-gated verbs use this
// via RequireOperatorScope; everywhere else the (userID, store
// WHERE-clause) pair is the tenancy boundary and the scope is
// advisory metadata.
func CallerScope(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyScope).(string)
	return v
}

// RequireOperatorScope returns ErrForbidden if the caller's scope is
// not 'operator'. Operator-gated verbs (IssueToken / RevokeToken /
// ListTokens / ListUsers / InviteUser) call this at their first
// validation step so the authority contract is co-located with the
// verb body. A missing scope (e.g. an internal call that forgot to
// stamp ctx) is treated identically to a non-operator scope — fail
// closed.
func RequireOperatorScope(ctx context.Context) error {
	if CallerScope(ctx) == "operator" {
		return nil
	}
	return fmt.Errorf("%w: operator scope required", ErrForbidden)
}
