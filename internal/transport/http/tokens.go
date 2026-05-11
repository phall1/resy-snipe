package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// tokensBackend is the slim seam the /v1/auth/tokens handlers depend
// on. Defined-at-the-consumer per Law 5 — the HTTP layer owns the
// contract; the daemon wires it to service.Standard. Pulling the
// methods out of the larger Service interface keeps the handler tests
// from needing a full Service fake.
type tokensBackend interface {
	IssueToken(ctx context.Context, userID domain.UserID, label, scope string) (service.BearerToken, error)
	RevokeToken(ctx context.Context, userID domain.UserID, tokenID string) error
	ListTokens(ctx context.Context, userID domain.UserID) ([]service.Token, error)
}

// MountTokenRoutes registers the /v1/auth/tokens surface against the
// supplied server. Routes:
//
//	POST   /v1/auth/tokens         — issue a fresh bearer (operator)
//	DELETE /v1/auth/tokens/{id}    — revoke by public token id (operator)
//	GET    /v1/auth/tokens         — list tokens for the caller's user
//
// All three routes require an authenticated context (the daemon wraps
// them with AuthMiddleware before mounting). The Service layer
// enforces the operator-scope check via RequireOperatorScope, so a
// user-scope caller surfaces as 403 forbidden.
//
// The handlers read the actor user_id from ctx via
// service.CallerUserID; the URL path never carries a user_id for
// these admin verbs (the caller acts as themselves).
func MountTokenRoutes(s *Server, backend tokensBackend) {
	s.HandleFunc("POST /v1/auth/tokens", newIssueTokenHandler(backend, s.log))
	s.HandleFunc("DELETE /v1/auth/tokens/{id}", newRevokeTokenHandler(backend, s.log))
	s.HandleFunc("GET /v1/auth/tokens", newListTokensHandler(backend, s.log))
}

// issueTokenRequest is the wire shape of POST /v1/auth/tokens. label
// is a free-form operator string (e.g. "phall-laptop"). scope is
// 'user' or 'operator'; the Service rejects other values with 422.
type issueTokenRequest struct {
	Label string `json:"label"`
	Scope string `json:"scope"`
}

// issueTokenResponse carries the plaintext bearer back to the caller.
// This is the ONLY shape that ever includes the plaintext — the
// listing projection (tokenResponse below) omits it permanently.
type issueTokenResponse struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	Label     string `json:"label"`
	Scope     string `json:"scope"`
	CreatedAt string `json:"created_at"` // RFC3339
}

// tokenResponse is the listing projection. created_at and last_seen
// are RFC3339; revoked_at is nil when the token is still live.
type tokenResponse struct {
	ID        string  `json:"id"`
	Scope     string  `json:"scope"`
	Label     string  `json:"label"`
	CreatedAt string  `json:"created_at"`
	LastSeen  *string `json:"last_seen,omitempty"`
	RevokedAt *string `json:"revoked_at,omitempty"`
}

func newIssueTokenHandler(b tokensBackend, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req issueTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			WriteError(w, log, fmt.Errorf("issue_token: %w: %s", service.ErrInvalidArgument, err.Error()))
			return
		}
		uid := service.CallerUserID(r.Context())
		if uid == "" {
			// Should not happen — the route is mounted behind
			// AuthMiddleware. Fail closed if a future refactor breaks
			// that invariant.
			WriteError(w, log, fmt.Errorf("issue_token: %w", service.ErrUnauthorized))
			return
		}
		bt, err := b.IssueToken(r.Context(), uid, req.Label, req.Scope)
		if err != nil {
			WriteError(w, log, err)
			return
		}
		WriteJSON(w, log, http.StatusCreated, issueTokenResponse{
			ID:        bt.ID,
			Token:     bt.Token,
			Label:     bt.Label,
			Scope:     bt.Scope,
			CreatedAt: bt.CreatedAt.Format(time.RFC3339),
		})
	}
}

func newRevokeTokenHandler(b tokensBackend, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokenID := r.PathValue("id")
		if tokenID == "" {
			WriteError(w, log, fmt.Errorf("revoke_token: %w: missing id", service.ErrInvalidArgument))
			return
		}
		uid := service.CallerUserID(r.Context())
		if uid == "" {
			WriteError(w, log, fmt.Errorf("revoke_token: %w", service.ErrUnauthorized))
			return
		}
		if err := b.RevokeToken(r.Context(), uid, tokenID); err != nil {
			WriteErrorDetails(w, log, err, map[string]any{"token_id": tokenID})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func newListTokensHandler(b tokensBackend, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := service.CallerUserID(r.Context())
		if uid == "" {
			WriteError(w, log, fmt.Errorf("list_tokens: %w", service.ErrUnauthorized))
			return
		}
		toks, err := b.ListTokens(r.Context(), uid)
		if err != nil {
			WriteError(w, log, err)
			return
		}
		out := make([]tokenResponse, 0, len(toks))
		for _, t := range toks {
			tr := tokenResponse{
				ID:        t.ID,
				Scope:     t.Scope,
				Label:     t.Label,
				CreatedAt: t.CreatedAt.Format(time.RFC3339),
			}
			if t.LastSeen != nil {
				s := t.LastSeen.Format(time.RFC3339)
				tr.LastSeen = &s
			}
			if t.RevokedAt != nil {
				s := t.RevokedAt.Format(time.RFC3339)
				tr.RevokedAt = &s
			}
			out = append(out, tr)
		}
		WriteJSON(w, log, http.StatusOK, map[string]any{"tokens": out})
	}
}
