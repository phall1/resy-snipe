package resy_test

// anti_bot_test.go owns the polling-floor property test described in
// R6's beads spec. The test asserts the *contract* the engine must
// honor: regardless of how aggressively the caller loops, no two
// outbound requests may fire within 100ms of each other.
//
// At Phase 1 the adapter does not yet expose a public polling-floor
// wrapper — that production wrapper lives in E5 (anti-bot floor /
// shutdown) when it lands. So this test ships a small in-test wrapper
// (pollingFloor) that enforces the floor via clock.Fake's After. The
// production wrapper will replace it with the same semantics; the test
// remains as the property contract.
//
// Driving model: the caller runs on its own goroutine, calling gate()
// 50 times in a tight loop. Each gate() call after the first registers
// an After(remaining) event on the fake clock. A coordinator goroutine
// observes the pending-event count via a ticker and advances the fake
// clock by the polling floor whenever an event is waiting. This keeps
// the test deterministic without depending on wall-clock timing.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/resy"
)

// pollingFloor is a test-local wrapper that enforces a minimum interval
// between successive gate() invocations. It is the contract the
// production engine wrapper (E5) will satisfy; this test asserts the
// contract holds when driven by clock.Fake.
type pollingFloor struct {
	mu      sync.Mutex
	clk     *clock.Fake
	floor   time.Duration
	last    time.Time // fake-clock at last fire
	hasRun  bool
	pending atomic.Int64 // # of in-flight gate() waits
}

func newPollingFloor(clk *clock.Fake, floor time.Duration) *pollingFloor {
	return &pollingFloor{clk: clk, floor: floor}
}

// gate blocks until the polling floor permits the next call. It
// returns the fake-clock timestamp at which the call is permitted to
// fire. The first call returns immediately at the current fake-clock
// time; subsequent calls wait until last+floor.
func (p *pollingFloor) gate() time.Time {
	p.mu.Lock()
	if !p.hasRun {
		p.hasRun = true
		p.last = p.clk.Now()
		ts := p.last
		p.mu.Unlock()
		return ts
	}
	earliest := p.last.Add(p.floor)
	p.mu.Unlock()

	for {
		now := p.clk.Now()
		if !now.Before(earliest) {
			break
		}
		wait := earliest.Sub(now)
		p.pending.Add(1)
		<-p.clk.After(wait)
		p.pending.Add(-1)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = p.clk.Now()
	return p.last
}

// TestPollingFloorRespectedUnderPressure fires 50 sequential calls
// behind a 100ms polling floor and asserts every consecutive pair of
// pre-call fake-clock timestamps is spaced by at least 100ms — even
// though the test loop iterates as fast as the goroutine scheduler
// will let it. Each call also goes through an httptest server on the
// real Client so we exercise the production transit path.
//
// This is a property test of the contract a future production wrapper
// will satisfy. The wrapper lives outside this package today (engine
// in internal/engine, blocked on E5), but the property must hold there
// too. When E5 lands, replace pollingFloor with the production type
// and keep this test verbatim — the assertion does not change.
func TestPollingFloorRespectedUnderPressure(t *testing.T) {
	t.Parallel()

	const (
		floor = 100 * time.Millisecond
		calls = 50
	)

	// httptest server that responds 200 with a /4/find body. We only
	// care about request *cadence*; the response shape just has to
	// pass the classifier (the in-test wrapper is what enforces the
	// floor, but firing a real request after each gate() proves the
	// Client tolerates being driven through the wrapper).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"results":{"venues":[{
				"venue":{"id":1,"name":"x"},
				"slots":[{"config":{"id":1,"token":"t","type":"Bar"},"date":{"start":"2026-06-01 19:00:00","end":"2026-06-01 20:00:00"}}]
			}]}
		}`)
	}))
	defer srv.Close()

	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	fake := clock.NewFake(start)
	c := resy.NewClient(slog.New(slog.DiscardHandler), fake,
		resy.WithBaseURL(srv.URL),
		resy.WithAPIKey("test"),
		resy.WithHTTPClient(srv.Client()),
		resy.WithUserAgent("test"),
	)

	gate := newPollingFloor(fake, floor)
	stamps := make([]time.Time, calls)

	// Coordinator: while the caller loop is running, advance the fake
	// clock by `floor` whenever there is at least one pending After()
	// wait. This releases the next gate() call deterministically.
	stop := make(chan struct{})
	coordDone := make(chan struct{})
	go func() {
		defer close(coordDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if gate.pending.Load() > 0 {
				fake.Advance(floor)
			} else {
				// Yield to let the caller schedule its After.
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Caller: 50 iterations, each gates then issues a real Find call.
	for i := range calls {
		stamps[i] = gate.gate()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _, err := resy.GetForTest(c, ctx, "/4/find", url.Values{"venue_id": {"1"}}, "")
		cancel()
		if err != nil {
			close(stop)
			<-coordDone
			t.Fatalf("GetForTest call %d: %v", i, err)
		}
	}
	close(stop)
	<-coordDone

	// First stamp at start; subsequent stamps spaced ≥ floor apart.
	if !stamps[0].Equal(start) {
		t.Errorf("stamp[0] = %v want %v", stamps[0], start)
	}
	for i := 1; i < len(stamps); i++ {
		gap := stamps[i].Sub(stamps[i-1])
		if gap < floor {
			t.Errorf("stamp[%d]-stamp[%d] = %v < %v", i, i-1, gap, floor)
		}
	}
}
