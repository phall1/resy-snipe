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

// plansBackend is the Service slice POST /v1/quests/plan needs.
// Defined-at-the-consumer per Law 5.
type plansBackend interface {
	PlanQuest(ctx context.Context, userID domain.UserID, goal domain.Goal) (domain.Plan, error)
}

// MountPlanRoutes registers POST /v1/quests/plan on s. The caller
// supplies a Goal; the Service returns the canonical Plan whose Hash
// can be pinned to a subsequent POST /v1/quests via the
// "plan_hash" field on CreateOpts.
func MountPlanRoutes(s *Server, backend plansBackend) {
	s.HandleFunc("POST /v1/quests/plan", newPlanQuestHandler(backend, s.log))
}

// planQuestRequest is the POST body — a Goal in wire form.
type planQuestRequest struct {
	Goal goalWire `json:"goal"`
}

// planQuestResponse wraps the Plan so the wire body stays a JSON
// object even though there's only one field. Future extensions
// (warnings, deprecation notes) can be added without breaking
// clients.
type planQuestResponse struct {
	Plan planWire `json:"plan"`
}

func newPlanQuestHandler(b plansBackend, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			WriteError(w, log, fmt.Errorf("plan_quest: %w: %s", service.ErrInvalidArgument, err.Error()))
			return
		}
		var req planQuestRequest
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
			WriteError(w, log, fmt.Errorf("plan_quest: %w", service.ErrUnauthorized))
			return
		}
		plan, err := b.PlanQuest(r.Context(), uid, goal)
		if err != nil {
			WriteError(w, log, err)
			return
		}
		WriteJSON(w, log, http.StatusOK, planQuestResponse{Plan: encodePlan(plan)})
	}
}
