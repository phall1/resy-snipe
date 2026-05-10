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
func (s *Standard) Login(ctx context.Context, userID domain.UserID, accountEmail, password string) (acctID domain.AccountID, retErr error) {
	if userID == "" {
		return "", fmt.Errorf("Login: %w: userID is required", ErrInvalidArgument)
	}
	// Failed auth still logs (design §audit-log-contract). The
	// deferred audit captures every exit path; target_id is the
	// account-email-shape input on failure (we don't know acctID
	// until BindAccountToUser succeeds) and the bound acctID on
	// success.
	defer func() {
		target := accountEmail
		if acctID != "" {
			target = string(acctID)
		}
		s.audit(ctx, userID, actionAccountLogin, target, retErr)
	}()

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

	acctID, err = s.store.BindAccountToUser(ctx, userID, resolvedEmail)
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

	return acctID, nil
}

// ListAccounts returns the Resy accounts bound to userID, stripped
// of secrets. The v2 schema has no display-only "last_used_at" yet,
// so the surface here mirrors only the columns the schema carries.
func (s *Standard) ListAccounts(ctx context.Context, userID domain.UserID) (out []Account, retErr error) {
	if userID == "" {
		return nil, fmt.Errorf("ListAccounts: %w: userID is required", ErrInvalidArgument)
	}
	defer func() {
		s.audit(ctx, userID, actionAccountList, "", retErr)
	}()

	rows, err := s.store.ListAccounts(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("ListAccounts: %w", err)
	}
	out = make([]Account, 0, len(rows))
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
// wires it. The attempt is still audited (failed auth/operator
// actions are recorded per §audit-log-contract).
func (s *Standard) InviteUser(ctx context.Context, userID domain.UserID, email, _ string) (Invite, error) {
	// TODO(operator-admin): generate invite token, hash it, persist to
	// invites table, return plaintext invite payload.
	err := ErrNotImplemented
	s.audit(ctx, userID, actionUserInvite, email, err)
	return Invite{}, err
}

// AcceptInvite is the unauthenticated "redeem an invite token" verb.
// M1-10 returns ErrNotImplemented. Audit row is logged under the
// (still-empty) UserID actor; the audit helper guards the empty-actor
// case by skipping the write so this is effectively a no-op until
// the verb is wired.
func (s *Standard) AcceptInvite(_ context.Context, _, _, _ string) (domain.UserID, BearerToken, error) {
	// TODO(operator-admin): validate token hash, insert users row,
	// mint initial BearerToken, mark invite consumed. On success,
	// audit under the freshly-minted UserID; on failure (expired
	// or consumed invite) the audit row has no valid actor — the
	// operator-admin wave decides how to record those (likely a
	// separate `invites.attempts` table keyed by invite_hash, not
	// audit_events). M1-11 skips the audit until that decision
	// lands.
	return "", BearerToken{}, ErrNotImplemented
}

// RotateToken issues a fresh bearer token for the caller. M1-10
// returns ErrNotImplemented.
func (s *Standard) RotateToken(ctx context.Context, userID domain.UserID, label string) (BearerToken, error) {
	// TODO(operator-admin): mint new token, hash, insert tokens row,
	// return plaintext to caller.
	err := ErrNotImplemented
	s.audit(ctx, userID, actionTokenRotate, label, err)
	return BearerToken{}, err
}

// RevokeToken invalidates a bearer token. M1-10 returns
// ErrNotImplemented.
func (s *Standard) RevokeToken(ctx context.Context, userID domain.UserID, tokenID string) error {
	// TODO(operator-admin): UPDATE tokens SET revoked_at = ? WHERE
	// token_id = ? AND user_id = ?.
	err := ErrNotImplemented
	s.audit(ctx, userID, actionTokenRevoke, tokenID, err)
	return err
}

// ListUsers is the operator-only cross-tenant listing. M1-10 returns
// ErrNotImplemented; the existing CLI continues to call
// store.ListUsers directly for now.
func (s *Standard) ListUsers(ctx context.Context, userID domain.UserID) ([]User, error) {
	// TODO(operator-admin): gate on role='admin', then surface
	// store.ListUsers' rows.
	err := ErrNotImplemented
	s.audit(ctx, userID, actionUserList, "", err)
	return nil, err
}
