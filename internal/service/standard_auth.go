package service

import (
	"context"
	"errors"
	"fmt"

	"resy-snipe/internal/domain"
)

// Login captures a Resy credential pair and persists a session for
// the supplied homelab userID. The Service delegates the actual
// upstream call to a ResyAuthBackend (the cmd/ layer wires the
// production *resy.Client); on success it claims the legacy NULL-
// user_id accounts row the v1 session bridge created and returns its
// AccountID.
//
// Operator-only at the transport level — the daemon checks role
// before binding. M1-10 does not enforce role here; the daemon's
// route guard does. Tenants reaching this path receive
// ErrUnauthorized at the transport boundary.
//
// TODO(M2): replace the v1 session bridge with a sealed-secrets path
// (design/secrets.md). The current shape stores the Resy JWT
// plaintext in sessions.jwt; M2 wraps it in secrets.ciphertext.
func (s *Standard) Login(ctx context.Context, userID domain.UserID, accountEmail, password string) (domain.AccountID, error) {
	if userID == "" {
		return "", fmt.Errorf("Login: %w: userID is required", ErrInvalidArgument)
	}
	if accountEmail == "" || password == "" {
		return "", fmt.Errorf("Login: %w: email and password are required", ErrInvalidArgument)
	}
	if s.auth == nil {
		return "", ErrNotImplemented
	}

	resolvedEmail, err := s.auth.Login(ctx, accountEmail, password)
	if err != nil {
		// The auth backend already wraps the underlying provider
		// failure; we map the well-known classes here.
		return "", fmt.Errorf("Login: %w", err)
	}

	acctID, err := s.store.BindAccountToUser(ctx, userID, resolvedEmail)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// The auth backend reported success but no legacy
			// account row materialized — this is an integration bug
			// in the adapter, not a tenant-visible state. Surface as
			// upstream-unavailable so the caller retries.
			return "", fmt.Errorf("Login: %w (no account row materialized after auth)", ErrUpstreamUnavailable)
		}
		return "", fmt.Errorf("Login bind account: %w", err)
	}

	// TODO(M1-11): audit_events.write(user=userID, action=login,
	// target=acctID, ok=true).

	return acctID, nil
}

// ListAccounts returns the Resy accounts bound to userID, stripped
// of secrets. The v2 schema has no display-only "last_used_at" yet,
// so the surface here mirrors only the columns the schema carries.
func (s *Standard) ListAccounts(ctx context.Context, userID domain.UserID) ([]Account, error) {
	if userID == "" {
		return nil, fmt.Errorf("ListAccounts: %w: userID is required", ErrInvalidArgument)
	}
	rows, err := s.store.ListAccounts(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("ListAccounts: %w", err)
	}
	out := make([]Account, 0, len(rows))
	for _, r := range rows {
		out = append(out, Account{
			ID:          r.ID,
			UserID:      r.UserID,
			Email:       r.ResyEmail,
			DisplayName: r.DisplayName,
			CreatedAt:   r.CreatedAt,
			DisabledAt:  r.DisabledAt,
		})
	}
	return out, nil
}

// InviteUser is the operator-only "provision a new tenant" verb. M1-10
// returns ErrNotImplemented; a later issue (the operator-admin wave)
// wires it.
func (s *Standard) InviteUser(_ context.Context, _ domain.UserID, _, _ string) (Invite, error) {
	// TODO(operator-admin): generate invite token, hash it, persist to
	// invites table, return plaintext invite payload.
	return Invite{}, ErrNotImplemented
}

// AcceptInvite is the unauthenticated "redeem an invite token" verb.
// M1-10 returns ErrNotImplemented.
func (s *Standard) AcceptInvite(_ context.Context, _, _, _ string) (domain.UserID, BearerToken, error) {
	// TODO(operator-admin): validate token hash, insert users row,
	// mint initial BearerToken, mark invite consumed.
	return "", BearerToken{}, ErrNotImplemented
}

// RotateToken issues a fresh bearer token for the caller. M1-10
// returns ErrNotImplemented.
func (s *Standard) RotateToken(_ context.Context, _ domain.UserID, _ string) (BearerToken, error) {
	// TODO(operator-admin): mint new token, hash, insert tokens row,
	// return plaintext to caller.
	return BearerToken{}, ErrNotImplemented
}

// RevokeToken invalidates a bearer token. M1-10 returns
// ErrNotImplemented.
func (s *Standard) RevokeToken(_ context.Context, _ domain.UserID, _ string) error {
	// TODO(operator-admin): UPDATE tokens SET revoked_at = ? WHERE
	// token_id = ? AND user_id = ?.
	return ErrNotImplemented
}

// ListUsers is the operator-only cross-tenant listing. M1-10 returns
// ErrNotImplemented; the existing CLI continues to call
// store.ListUsers directly for now.
func (s *Standard) ListUsers(_ context.Context, _ domain.UserID) ([]User, error) {
	// TODO(operator-admin): gate on role='admin', then surface
	// store.ListUsers' rows.
	return nil, ErrNotImplemented
}
