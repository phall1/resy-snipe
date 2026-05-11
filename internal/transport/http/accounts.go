package http

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// accountsBackend is the Service slice the /v1/accounts routes need.
// Defined-at-the-consumer (Law 5).
type accountsBackend interface {
	Login(ctx context.Context, userID domain.UserID, accountEmail, password string) (domain.AccountID, error)
	ListAccounts(ctx context.Context, userID domain.UserID) ([]service.Account, error)
}

// MountAccountRoutes registers:
//
//	POST /v1/accounts/login   — operator-only; captures a Resy session
//	GET  /v1/accounts         — lists the caller's Resy-account bindings
//
// Both require an authenticated caller (the daemon wraps the /v1
// surface with AuthMiddleware). Operator-only-ness for Login is
// enforced by the Service layer once it migrates that gate (the
// existing implementation accepts any authenticated user; the
// transport contract reserves the right to refuse a user-scope
// caller).
func MountAccountRoutes(s *Server, backend accountsBackend) {
	s.HandleFunc("POST /v1/accounts/login", newLoginHandler(backend, s.log))
	s.HandleFunc("GET /v1/accounts", newListAccountsHandler(backend, s.log))
}

// loginRequest is the operator-supplied Resy credential pair the
// daemon proxies to the provider. Per design daemon.md §4.2, the
// password is never logged or stored in plaintext — the Service hands
// it to the auth backend in-memory and the resulting session token
// lives in the secrets table sealed.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	AccountID string `json:"account_id"`
}

func newLoginHandler(b accountsBackend, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			WriteError(w, log, fmt.Errorf("login: %w: %s", service.ErrInvalidArgument, err.Error()))
			return
		}
		var req loginRequest
		if err := decodeJSON(body, &req); err != nil {
			WriteError(w, log, err)
			return
		}
		if req.Email == "" || req.Password == "" {
			WriteError(w, log, fmt.Errorf("login: %w: email and password are required", service.ErrInvalidArgument))
			return
		}
		uid := service.CallerUserID(r.Context())
		if uid == "" {
			WriteError(w, log, fmt.Errorf("login: %w", service.ErrUnauthorized))
			return
		}
		aid, err := b.Login(r.Context(), uid, req.Email, req.Password)
		if err != nil {
			WriteError(w, log, err)
			return
		}
		WriteJSON(w, log, http.StatusCreated, loginResponse{AccountID: string(aid)})
	}
}

type listAccountsResponse struct {
	Accounts []accountWire `json:"accounts"`
}

func newListAccountsHandler(b accountsBackend, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := service.CallerUserID(r.Context())
		if uid == "" {
			WriteError(w, log, fmt.Errorf("list_accounts: %w", service.ErrUnauthorized))
			return
		}
		rows, err := b.ListAccounts(r.Context(), uid)
		if err != nil {
			WriteError(w, log, err)
			return
		}
		out := make([]accountWire, 0, len(rows))
		for _, r := range rows {
			out = append(out, encodeAccount(r))
		}
		WriteJSON(w, log, http.StatusOK, listAccountsResponse{Accounts: out})
	}
}
