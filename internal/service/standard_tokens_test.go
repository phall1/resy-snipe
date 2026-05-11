package service_test

import (
	"errors"
	"strings"
	"testing"

	"resy-snipe/internal/service"
)

// TestStandardIssueTokenRoundTrip verifies that IssueToken mints a
// "tok_"-prefixed plaintext, persists a row with the expected scope
// and label, and emits a token.issue audit row.
func TestStandardIssueTokenRoundTrip(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := service.ContextWithCaller(t.Context(), h.uid, "operator")

	bt, err := h.svc.IssueToken(ctx, h.uid, "phall-laptop", "operator")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if !strings.HasPrefix(bt.Token, "tok_") {
		t.Errorf("Token prefix: got %q", bt.Token)
	}
	if !strings.HasPrefix(bt.ID, "tok_") {
		t.Errorf("ID prefix: got %q", bt.ID)
	}
	if bt.Scope != "operator" || bt.Label != "phall-laptop" {
		t.Errorf("BearerToken metadata: %+v", bt)
	}
	if bt.CreatedAt.IsZero() {
		t.Error("CreatedAt: zero, want non-zero")
	}

	// Listing exposes the row but never the plaintext.
	rows, err := h.svc.ListTokens(ctx, h.uid)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListTokens: got %d rows, want 1", len(rows))
	}
	if rows[0].ID != bt.ID || rows[0].Scope != "operator" || rows[0].Label != "phall-laptop" {
		t.Errorf("ListTokens row: %+v", rows[0])
	}

	// Audit: token.issue and token.list rows.
	if rs := listAuditByAction(t, h, h.uid, "token.issue"); len(rs) != 1 || !rs[0].OK || rs[0].TargetID != bt.ID {
		t.Errorf("token.issue audit: %+v", rs)
	}
	if rs := listAuditByAction(t, h, h.uid, "token.list"); len(rs) != 1 || !rs[0].OK {
		t.Errorf("token.list audit: %+v", rs)
	}
}

// TestStandardIssueTokenRejectsBadScope keeps the Service-layer
// belt-and-braces validation honest: HTTP routes already gate scope,
// but a programming error elsewhere should still surface clearly.
func TestStandardIssueTokenRejectsBadScope(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := service.ContextWithCaller(t.Context(), h.uid, "operator")
	if _, err := h.svc.IssueToken(ctx, h.uid, "x", "wheel"); !errors.Is(err, service.ErrInvalidArgument) {
		t.Errorf("IssueToken bad scope: got %v, want ErrInvalidArgument", err)
	}
}

func TestStandardIssueTokenRejectsEmptyLabel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := service.ContextWithCaller(t.Context(), h.uid, "operator")
	if _, err := h.svc.IssueToken(ctx, h.uid, "", "user"); !errors.Is(err, service.ErrInvalidArgument) {
		t.Errorf("IssueToken empty label: got %v, want ErrInvalidArgument", err)
	}
}

// TestStandardRevokeTokenLifecycle issues a token, revokes it, and
// asserts the row stamps RevokedAt and a second revoke returns
// ErrNotFound (the design has no separate "already revoked" sentinel).
func TestStandardRevokeTokenLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := service.ContextWithCaller(t.Context(), h.uid, "operator")

	bt, err := h.svc.IssueToken(ctx, h.uid, "to-revoke", "user")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if err := h.svc.RevokeToken(ctx, h.uid, bt.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	rows, err := h.svc.ListTokens(ctx, h.uid)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(rows) != 1 || rows[0].RevokedAt == nil {
		t.Errorf("revoked row state: %+v", rows)
	}
	if err := h.svc.RevokeToken(ctx, h.uid, bt.ID); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("second revoke: got %v, want ErrNotFound", err)
	}
}

// TestStandardRevokeTokenCrossUserNotFound asserts that a different
// user revoking by id is opaque — the tokenID is rejected as
// not-found, never as forbidden, to avoid leaking cross-tenant
// existence (same pattern as GetQuest).
func TestStandardRevokeTokenCrossUserNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := service.ContextWithCaller(t.Context(), h.uid, "operator")

	bt, err := h.svc.IssueToken(ctx, h.uid, "owned", "user")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if err := h.svc.RevokeToken(ctx, h.otherID, bt.ID); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("cross-user revoke: got %v, want ErrNotFound", err)
	}
}

// TestStandardTokenVerbsRequireOperatorScope asserts the operator-
// gate on IssueToken / RevokeToken / ListTokens. A caller with
// 'user' scope (or no scope at all) gets ErrForbidden — the design
// pins this enforcement in the Service so the authority contract
// lives with the verb, not the transport.
func TestStandardTokenVerbsRequireOperatorScope(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	userCtx := service.ContextWithCaller(t.Context(), h.uid, "user")

	if _, err := h.svc.IssueToken(userCtx, h.uid, "x", "user"); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("IssueToken user-scope: got %v, want ErrForbidden", err)
	}
	if err := h.svc.RevokeToken(userCtx, h.uid, "tok_anything"); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("RevokeToken user-scope: got %v, want ErrForbidden", err)
	}
	if _, err := h.svc.ListTokens(userCtx, h.uid); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("ListTokens user-scope: got %v, want ErrForbidden", err)
	}

	// Empty ctx fails closed too (defense-in-depth against a
	// transport that forgot to stamp the caller).
	bareCtx := t.Context()
	if _, err := h.svc.IssueToken(bareCtx, h.uid, "x", "user"); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("IssueToken bare-ctx: got %v, want ErrForbidden", err)
	}
}

// TestStandardListTokensScopedToUser confirms ListTokens does not
// leak tokens belonging to other tenants.
func TestStandardListTokensScopedToUser(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := service.ContextWithCaller(t.Context(), h.uid, "operator")

	if _, err := h.svc.IssueToken(ctx, h.uid, "mine", "user"); err != nil {
		t.Fatalf("IssueToken mine: %v", err)
	}
	if _, err := h.svc.IssueToken(ctx, h.otherID, "theirs", "user"); err != nil {
		t.Fatalf("IssueToken theirs: %v", err)
	}
	rows, err := h.svc.ListTokens(ctx, h.uid)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListTokens: got %d rows, want 1", len(rows))
	}
	if rows[0].Label != "mine" {
		t.Errorf("ListTokens leaked across tenants: %+v", rows)
	}
}
