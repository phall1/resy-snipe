package http

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// questsBackend is the Service slice the quest verbs need. Defined-
// at-the-consumer (Law 5) — handler tests stub this; production
// passes service.Standard.
type questsBackend interface {
	CreateQuest(ctx context.Context, userID domain.UserID, goal domain.Goal, opts service.CreateOpts) (domain.QuestID, error)
	GetQuest(ctx context.Context, userID domain.UserID, questID domain.QuestID) (service.QuestState, error)
	ListQuests(ctx context.Context, userID domain.UserID, filter service.ListFilter) ([]service.QuestSummary, error)
	CancelQuest(ctx context.Context, userID domain.UserID, questID domain.QuestID, opts service.CancelOpts) error
}

// MountQuestRoutes registers the /v1/quests surface (excluding the SSE
// stream, which lives in sse.go):
//
//	POST   /v1/quests          — CreateQuest
//	GET    /v1/quests          — ListQuests
//	GET    /v1/quests/{id}     — GetQuest
//	DELETE /v1/quests/{id}     — CancelQuest
//
// All four routes require an authenticated caller — the daemon wraps
// this surface with AuthMiddleware before mounting.
func MountQuestRoutes(s *Server, backend questsBackend) {
	s.HandleFunc("POST /v1/quests", newCreateQuestHandler(backend, s.log))
	s.HandleFunc("GET /v1/quests", newListQuestsHandler(backend, s.log))
	s.HandleFunc("GET /v1/quests/{id}", newGetQuestHandler(backend, s.log))
	s.HandleFunc("DELETE /v1/quests/{id}", newCancelQuestHandler(backend, s.log))
}

// createQuestRequest is the POST /v1/quests body. PlanHash and
// IdempotencyKey are optional; PlanHash pins the request to a Plan
// computed earlier (TOCTOU defense, ADR-0012), and IdempotencyKey
// makes the call replayable within the retention window
// (service-layer.md §idempotency).
type createQuestRequest struct {
	Goal           goalWire `json:"goal"`
	PlanHash       *string  `json:"plan_hash,omitempty"`
	IdempotencyKey *string  `json:"idempotency_key,omitempty"`
}

type createQuestResponse struct {
	ID string `json:"id"`
}

func newCreateQuestHandler(b questsBackend, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			WriteError(w, log, fmt.Errorf("create_quest: %w: %s", service.ErrInvalidArgument, err.Error()))
			return
		}
		var req createQuestRequest
		if err := decodeJSON(body, &req); err != nil {
			WriteError(w, log, err)
			return
		}
		goal, err := decodeGoal(req.Goal)
		if err != nil {
			WriteError(w, log, err)
			return
		}
		uid := service.CallerUserID(r.Context())
		if uid == "" {
			WriteError(w, log, fmt.Errorf("create_quest: %w", service.ErrUnauthorized))
			return
		}
		id, err := b.CreateQuest(r.Context(), uid, goal, service.CreateOpts{
			PlanHash:       req.PlanHash,
			IdempotencyKey: req.IdempotencyKey,
		})
		if err != nil {
			WriteError(w, log, err)
			return
		}
		WriteJSON(w, log, http.StatusCreated, createQuestResponse{ID: string(id)})
	}
}

// listQuestsResponse wraps the result so the wire body is a JSON
// object even if quests becomes a paged collection later.
type listQuestsResponse struct {
	Quests []questSummaryWire `json:"quests"`
}

func newListQuestsHandler(b questsBackend, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := service.CallerUserID(r.Context())
		if uid == "" {
			WriteError(w, log, fmt.Errorf("list_quests: %w", service.ErrUnauthorized))
			return
		}
		filter, err := parseListFilter(r)
		if err != nil {
			WriteError(w, log, err)
			return
		}
		rows, err := b.ListQuests(r.Context(), uid, filter)
		if err != nil {
			WriteError(w, log, err)
			return
		}
		out := make([]questSummaryWire, 0, len(rows))
		for _, r := range rows {
			out = append(out, encodeQuestSummary(r))
		}
		WriteJSON(w, log, http.StatusOK, listQuestsResponse{Quests: out})
	}
}

// parseListFilter pulls the query parameters into a service.ListFilter.
// Supported params (all optional):
//   - status=<csv>            comma-separated domain.Status names
//   - account_id=<id>         exact match
//   - since=<rfc3339>         lower bound (inclusive) on created_at
//   - until=<rfc3339>         upper bound (exclusive) on created_at
//   - limit=<int>             page size; ≤0 deferred to Service default
//   - cursor=<str>            opaque pagination handle from a prior page
func parseListFilter(r *http.Request) (service.ListFilter, error) {
	q := r.URL.Query()
	statuses, err := parseStatusList(q.Get("status"))
	if err != nil {
		return service.ListFilter{}, err
	}
	since, err := parseRFC3339(q.Get("since"))
	if err != nil {
		return service.ListFilter{}, fmt.Errorf("list_quests: since: %w", err)
	}
	until, err := parseRFC3339(q.Get("until"))
	if err != nil {
		return service.ListFilter{}, fmt.Errorf("list_quests: until: %w", err)
	}
	limit := 0
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return service.ListFilter{}, fmt.Errorf("%w: limit: %s", service.ErrInvalidArgument, err.Error())
		}
		limit = n
	}
	f := service.ListFilter{
		Status: statuses,
		Limit:  limit,
		Cursor: q.Get("cursor"),
	}
	if id := q.Get("account_id"); id != "" {
		aid := domain.AccountID(id)
		f.AccountID = &aid
	}
	if !since.IsZero() {
		f.Since = &since
	}
	if !until.IsZero() {
		f.Until = &until
	}
	return f, nil
}

func newGetQuestHandler(b questsBackend, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := service.CallerUserID(r.Context())
		if uid == "" {
			WriteError(w, log, fmt.Errorf("get_quest: %w", service.ErrUnauthorized))
			return
		}
		qid := r.PathValue("id")
		if err := ensureNonEmpty("quest_id", qid); err != nil {
			WriteError(w, log, err)
			return
		}
		state, err := b.GetQuest(r.Context(), uid, domain.QuestID(qid))
		if err != nil {
			WriteErrorDetails(w, log, err, map[string]any{"quest_id": qid})
			return
		}
		WriteJSON(w, log, http.StatusOK, encodeQuestState(state))
	}
}

// cancelQuestRequest is the optional DELETE body. Both fields are
// optional; an empty body is also accepted (reason will be blank).
type cancelQuestRequest struct {
	Reason         string  `json:"reason,omitempty"`
	IdempotencyKey *string `json:"idempotency_key,omitempty"`
}

func newCancelQuestHandler(b questsBackend, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := service.CallerUserID(r.Context())
		if uid == "" {
			WriteError(w, log, fmt.Errorf("cancel_quest: %w", service.ErrUnauthorized))
			return
		}
		qid := r.PathValue("id")
		if err := ensureNonEmpty("quest_id", qid); err != nil {
			WriteError(w, log, err)
			return
		}
		opts := service.CancelOpts{}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			WriteError(w, log, fmt.Errorf("cancel_quest: %w: %s", service.ErrInvalidArgument, err.Error()))
			return
		}
		if len(body) > 0 {
			var req cancelQuestRequest
			if err := decodeJSON(body, &req); err != nil {
				WriteError(w, log, err)
				return
			}
			opts.Reason = req.Reason
			opts.IdempotencyKey = req.IdempotencyKey
		}
		if err := b.CancelQuest(r.Context(), uid, domain.QuestID(qid), opts); err != nil {
			WriteErrorDetails(w, log, err, map[string]any{"quest_id": qid})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
