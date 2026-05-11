package http_test

import (
	"bufio"
	"context"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
	transport "resy-snipe/internal/transport/http"
)

// fakeSSEBackend produces a fixed sequence of events through the
// callback then returns. The blockingTerminal variant blocks on ctx
// to exercise client-disconnect behavior.
type fakeSSEBackend struct {
	events            []domain.Event
	err               error
	blockUntilCancel  bool
	lastSubscribedUID domain.UserID
	lastSubscribedQID domain.QuestID
}

func (f *fakeSSEBackend) SubscribeQuest(
	ctx context.Context,
	uid domain.UserID,
	qid domain.QuestID,
	cb func(domain.Event),
) error {
	f.lastSubscribedUID = uid
	f.lastSubscribedQID = qid
	for _, e := range f.events {
		cb(e)
	}
	if f.blockUntilCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func newSSEServer(t *testing.T, b *fakeSSEBackend, callerUID domain.UserID) *httptest.Server {
	t.Helper()
	s := transport.NewServer("127.0.0.1:0", "test", nil)
	transport.MountSSERoutes(s, b)
	wrap := stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		ctx := r.Context()
		if callerUID != "" {
			ctx = service.ContextWithCaller(ctx, callerUID, "user")
		}
		s.Handler().ServeHTTP(w, r.WithContext(ctx))
	})
	ts := httptest.NewServer(wrap)
	t.Cleanup(ts.Close)
	return ts
}

func TestSSE_EmitsEventsInOrder(t *testing.T) {
	t.Parallel()
	b := &fakeSSEBackend{events: []domain.Event{
		{Type: domain.EventSubmitted, At: time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)},
		{Type: domain.EventScheduled, At: time.Date(2026, 5, 10, 12, 0, 5, 0, time.UTC)},
		{Type: domain.EventBooked, At: time.Date(2026, 5, 10, 12, 0, 10, 0, time.UTC)},
	}}
	ts := newSSEServer(t, b, "usr_op")
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodGet, ts.URL+"/v1/quests/q_abc/events", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type=%q, want text/event-stream", ct)
	}
	scanner := bufio.NewScanner(resp.Body)
	var events []string
	for scanner.Scan() {
		line := scanner.Text()
		if rest, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, rest)
		}
	}
	wantOrder := []string{"submitted", "scheduled", "booked"}
	if len(events) != len(wantOrder) {
		t.Fatalf("events: %v, want %v", events, wantOrder)
	}
	for i, w := range wantOrder {
		if events[i] != w {
			t.Errorf("events[%d]=%q, want %q", i, events[i], w)
		}
	}
	if b.lastSubscribedQID != "q_abc" {
		t.Errorf("forwarded qid=%q, want q_abc", b.lastSubscribedQID)
	}
}

func TestSSE_NoCallerReturns401(t *testing.T) {
	t.Parallel()
	b := &fakeSSEBackend{}
	ts := newSSEServer(t, b, "")
	req, err := stdhttp.NewRequestWithContext(context.Background(), stdhttp.MethodGet, ts.URL+"/v1/quests/q_abc/events", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != stdhttp.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
}

func TestSSE_ClientDisconnectStopsCleanly(t *testing.T) {
	t.Parallel()
	b := &fakeSSEBackend{
		events:           []domain.Event{{Type: domain.EventSubmitted, At: time.Now()}},
		blockUntilCancel: true,
	}
	ts := newSSEServer(t, b, "usr_op")
	ctx, cancel := context.WithCancel(context.Background())
	req, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, ts.URL+"/v1/quests/q_abc/events", stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	// Consume one event then disconnect.
	buf := make([]byte, 1024)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	cancel()
	_ = resp.Body.Close()
	// Give the server a moment to observe cancellation. If it
	// deadlocks (didn't honor ctx) the test will hit go test's
	// per-test timeout.
	time.Sleep(50 * time.Millisecond)
}
