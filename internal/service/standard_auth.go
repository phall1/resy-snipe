package service

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"

	"golang.org/x/crypto/blake2b"

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

// tokenPlaintextBytes is the entropy budget for a freshly-minted
// bearer. 32 bytes ⇒ 52 base32 chars after the "tok_" prefix; well
// above the 128-bit floor for a credential the daemon never gets to
// re-issue. The hash stored in the database is BLAKE2b-256 of these
// raw bytes (not the base32 form).
const tokenPlaintextBytes = 32

// tokenIDBytes is the random-tail width for the public token id. The
// design pins it as a ULID; we deviate to a hex-encoded random tail
// for parity with newQuestID's id-shape. 8 bytes / 64 bits is
// well-clear of birthday collisions at homelab scale (millions of
// issuances).
const tokenIDBytes = 8

// tokenBase32 is the alphabet used for the on-the-wire plaintext.
// RFC 4648 base32 without padding — case-insensitive on input (the
// auth middleware normalizes upper-case), uppercase on output. The
// Crockford alphabet was tempting but the standard alphabet keeps
// the dependency surface minimal.
var tokenBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// IssueToken mints a fresh bearer for userID under the supplied
// label and scope. The plaintext is returned exactly once on the
// BearerToken; subsequent ListTokens calls expose only the public id,
// scope, label, and timestamps.
//
// Scope must be 'user' or 'operator' — the HTTP layer rejects an
// unauthenticated POST /v1/auth/tokens at the middleware boundary,
// and the route layer's authority check makes sure only operator-
// scoped callers reach this verb. The Service still rejects unknown
// scope values with ErrInvalidArgument as a belt-and-braces.
func (s *Standard) IssueToken(
	ctx context.Context,
	userID domain.UserID,
	label, scope string,
) (out BearerToken, retErr error) {
	defer func() {
		// The audit target is the public token id once minted; on
		// failure we have no id, so we log the label so the operator
		// can still correlate a failed mint with the requesting CLI.
		target := label
		if out.ID != "" {
			target = out.ID
		}
		s.audit(ctx, userID, actionTokenIssue, target, retErr)
	}()

	if err := RequireOperatorScope(ctx); err != nil {
		return BearerToken{}, fmt.Errorf("IssueToken: %w", err)
	}
	if userID == "" {
		return BearerToken{}, fmt.Errorf("IssueToken: %w: userID is required", ErrInvalidArgument)
	}
	if label == "" {
		return BearerToken{}, fmt.Errorf("IssueToken: %w: label is required", ErrInvalidArgument)
	}
	if scope != "user" && scope != "operator" {
		return BearerToken{}, fmt.Errorf("IssueToken: %w: scope must be 'user' or 'operator', got %q", ErrInvalidArgument, scope)
	}

	// Plaintext: 32 raw bytes, base32-encoded as the wire shape. The
	// hash stored in the DB is over the raw bytes — equivalently the
	// authenticator hashes the decoded bytes on lookup. Encoding/
	// decoding is symmetric so this is interchangeable, but raw-byte
	// hashing avoids re-encoding overhead in the hot middleware path.
	raw := make([]byte, tokenPlaintextBytes)
	if _, err := rand.Read(raw); err != nil {
		return BearerToken{}, fmt.Errorf("IssueToken: %w", err)
	}
	plaintext := "tok_" + tokenBase32.EncodeToString(raw)
	hash := blake2b.Sum256([]byte(plaintext))

	// Public id: hex tail mirroring newQuestID conventions. See
	// tokenIDBytes for the entropy rationale.
	idBuf := make([]byte, tokenIDBytes)
	if _, err := rand.Read(idBuf); err != nil {
		return BearerToken{}, fmt.Errorf("IssueToken: %w", err)
	}
	tokenID := "tok_" + hex.EncodeToString(idBuf)

	now := s.clock.Now().UTC()
	rec := TokenRecord{
		ID:        tokenID,
		UserID:    userID,
		Hash:      hash[:],
		Scope:     scope,
		Label:     label,
		CreatedAt: now,
	}
	if err := s.store.InsertToken(ctx, rec); err != nil {
		return BearerToken{}, fmt.Errorf("IssueToken: %w", err)
	}

	out = BearerToken{
		ID:        tokenID,
		Token:     plaintext,
		Label:     label,
		Scope:     scope,
		CreatedAt: now,
	}
	return out, nil
}

// RevokeToken invalidates the bearer token identified by tokenID
// (the public ULID/id handle), provided it belongs to userID. A
// missing token (or one owned by another user, or already revoked)
// surfaces as ErrNotFound — the design has no separate "already
// revoked" sentinel.
func (s *Standard) RevokeToken(ctx context.Context, userID domain.UserID, tokenID string) (retErr error) {
	defer func() {
		s.audit(ctx, userID, actionTokenRevoke, tokenID, retErr)
	}()
	if err := RequireOperatorScope(ctx); err != nil {
		return fmt.Errorf("RevokeToken: %w", err)
	}
	if userID == "" {
		return fmt.Errorf("RevokeToken: %w: userID is required", ErrInvalidArgument)
	}
	if tokenID == "" {
		return fmt.Errorf("RevokeToken: %w: tokenID is required", ErrInvalidArgument)
	}
	now := s.clock.Now().UTC()
	if err := s.store.RevokeToken(ctx, userID, tokenID, now); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("RevokeToken: %w", err)
	}
	return nil
}

// ListTokens returns userID's tokens (live + revoked) for the per-
// user view. Plaintext is never included. Operator listing across
// tenants is not exposed at this surface — a separate path will
// drive that when the operator-admin wave lands.
func (s *Standard) ListTokens(ctx context.Context, userID domain.UserID) (out []Token, retErr error) {
	defer func() {
		s.audit(ctx, userID, actionTokenList, "", retErr)
	}()
	if err := RequireOperatorScope(ctx); err != nil {
		return nil, fmt.Errorf("ListTokens: %w", err)
	}
	if userID == "" {
		return nil, fmt.Errorf("ListTokens: %w: userID is required", ErrInvalidArgument)
	}
	recs, err := s.store.ListTokensForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("ListTokens: %w", err)
	}
	out = make([]Token, 0, len(recs))
	for _, r := range recs {
		out = append(out, Token{
			ID:        r.ID,
			UserID:    r.UserID,
			Scope:     r.Scope,
			Label:     r.Label,
			CreatedAt: r.CreatedAt,
			LastSeen:  r.LastSeen,
			RevokedAt: r.RevokedAt,
		})
	}
	return out, nil
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
