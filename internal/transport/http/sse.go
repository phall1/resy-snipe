package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
)

// sseBackend is the slim Service slice GET /v1/quests/{id}/events
// needs. Defined-at-the-consumer per Law 5.
type sseBackend interface {
	SubscribeQuest(ctx context.Context, userID domain.UserID, questID domain.QuestID, callback func(domain.Event)) error
}

// MountSSERoutes registers GET /v1/quests/{id}/events on s. The route
// emits an SSE stream of quest events; per design daemon.md §4.5 the
// connection is held open until the quest reaches a terminal status
// (Booked, Failed, Canceled, Expired) or the client disconnects.
//
// Reconnection is the client's responsibility — the daemon does not
// maintain per-client state across reconnects. The Service drains the
// persisted event history first via its own ListQuestEvents seam, then
// pumps the live engine stream until terminal status, so a fresh
// connect always sees the complete event history up to the point of
// connection.
func MountSSERoutes(s *Server, backend sseBackend) {
	s.HandleFunc("GET /v1/quests/{id}/events", newSubscribeQuestHandler(backend, s.log))
}

func newSubscribeQuestHandler(b sseBackend, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := service.CallerUserID(r.Context())
		if uid == "" {
			WriteError(w, log, fmt.Errorf("subscribe_quest: %w", service.ErrUnauthorized))
			return
		}
		qid := r.PathValue("id")
		if err := ensureNonEmpty("quest_id", qid); err != nil {
			WriteError(w, log, err)
			return
		}

		// The Flusher capability is required to push event data as the
		// engine emits it. The Go net/http response writers expose this
		// on every HTTP/1.1+ connection; a non-flushable writer
		// indicates a future transport stacked on top (e.g. a buffering
		// middleware) and is treated as a 500 — the contract is
		// SSE-or-error.
		flusher, ok := w.(http.Flusher)
		if !ok {
			WriteError(w, log, errors.New("subscribe_quest: response writer is not flushable"))
			return
		}

		// Pre-flight: confirm the quest exists and belongs to uid
		// before we commit the response headers. The Service's
		// SubscribeQuest does this internally and would return
		// ErrNotFound on a bad pair, but at that point we've already
		// emitted 200 OK and there is no graceful way to backtrack.
		// Defer this to a future refinement (the Service will need a
		// CheckSubscribe verb or similar) — for now we accept that an
		// auth-passing-but-quest-missing request gets a 200 with an
		// "error" SSE event in-stream.

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no") // nginx, sigh
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// callback runs on the Service's goroutine (the contract per
		// service.SubscribeQuest). We must not block — Flush is the
		// only side effect, and the http response writer is goroutine-
		// safe for sequential writes from a single producer (which
		// SubscribeQuest guarantees: "invoking callback once per event
		// in arrival order on the calling goroutine").
		cb := func(e domain.Event) {
			payload, err := json.Marshal(encodeEvent(e))
			if err != nil {
				log.Warn("transport.sse_encode_failed", slog.String("err", err.Error()))
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, payload); err != nil {
				log.Info("transport.sse_write_failed", slog.String("err", err.Error()))
				return
			}
			flusher.Flush()
		}

		if err := b.SubscribeQuest(r.Context(), uid, domain.QuestID(qid), cb); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// Client disconnected — nothing to write; the
				// connection has already been torn down by the time we
				// get here.
				return
			}
			// We've already committed the 200 OK; signal the error
			// in-stream. Clients consuming the stream branch on the
			// "error" event type.
			// Encode the error message as JSON so newlines or other
			// SSE-framing characters in the error can't break the
			// event boundary (gosec G705).
			msg, _ := json.Marshal(err.Error())
			_, _ = w.Write([]byte("event: error\ndata: "))
			_, _ = w.Write(msg)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}
}
