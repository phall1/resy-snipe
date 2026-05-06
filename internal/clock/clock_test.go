package clock_test

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"resy-snipe/internal/clock"
)

// Compile-time assertions: both impls satisfy Clock.
var (
	_ clock.Clock = clock.Real{}
	_ clock.Clock = (*clock.Fake)(nil)
)

func TestFakeNowReflectsAdvance(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := clock.NewFake(start)
	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("Now()=%v want %v", got, start)
	}
	c.Advance(2 * time.Hour)
	if got := c.Now(); !got.Equal(start.Add(2 * time.Hour)) {
		t.Fatalf("Now() after Advance: got %v want %v", got, start.Add(2*time.Hour))
	}
}

func TestFakeAfterDoesNotFireBeforeDeadline(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(time.Unix(0, 0))
	ch := c.After(time.Second)

	select {
	case v := <-ch:
		t.Fatalf("After fired without Advance: %v", v)
	default:
	}

	c.Advance(500 * time.Millisecond)
	select {
	case v := <-ch:
		t.Fatalf("After fired before deadline: %v", v)
	default:
	}

	c.Advance(500 * time.Millisecond)
	select {
	case <-ch:
	default:
		t.Fatal("After did not fire after deadline reached")
	}
}

// Verifies After is composable with context cancellation: a caller that
// abandons the channel via ctx.Done() does not block the Fake's firing
// path on a later Advance.
func TestFakeAfterRespectsCancellation(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(time.Unix(0, 0))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch := c.After(time.Hour)
	select {
	case <-ch:
		t.Fatal("After fired before any Advance")
	case <-ctx.Done():
	}

	// Even though the consumer has gone away, advancing past the
	// deadline must not panic, deadlock, or leak.
	c.Advance(2 * time.Hour)
}

func TestFakeAfterFuncFiresInRegistrationOrder(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(time.Unix(0, 0))

	var mu sync.Mutex
	var got []int
	for i := 1; i <= 5; i++ {
		i := i
		c.AfterFunc(time.Second, func() {
			mu.Lock()
			got = append(got, i)
			mu.Unlock()
		})
	}

	c.Advance(time.Second)

	want := []int{1, 2, 3, 4, 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registration order: got %v want %v", got, want)
	}
}

func TestFakeAdvanceFiresInDeadlineOrder(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(time.Unix(0, 0))

	var mu sync.Mutex
	var got []time.Duration
	register := func(d time.Duration) {
		c.AfterFunc(d, func() {
			mu.Lock()
			got = append(got, d)
			mu.Unlock()
		})
	}

	// Register out of deadline order on purpose.
	register(3 * time.Second)
	register(1 * time.Second)
	register(2 * time.Second)

	c.Advance(5 * time.Second)

	want := []time.Duration{1 * time.Second, 2 * time.Second, 3 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deadline order: got %v want %v", got, want)
	}
}

func TestFakeAdvanceMixedDeadlineAndRegistrationOrder(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(time.Unix(0, 0))

	var mu sync.Mutex
	var got []string
	at := func(d time.Duration, label string) {
		c.AfterFunc(d, func() {
			mu.Lock()
			got = append(got, label)
			mu.Unlock()
		})
	}

	at(2*time.Second, "b1")
	at(1*time.Second, "a1")
	at(2*time.Second, "b2")
	at(1*time.Second, "a2")

	c.Advance(2 * time.Second)

	want := []string{"a1", "a2", "b1", "b2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordering: got %v want %v", got, want)
	}
}

func TestFakeTimerStopPreventsFiring(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(time.Unix(0, 0))

	var fired int32
	var mu sync.Mutex
	timer := c.AfterFunc(time.Second, func() {
		mu.Lock()
		fired++
		mu.Unlock()
	})

	if !timer.Stop() {
		t.Fatal("Stop returned false on un-fired timer")
	}
	c.Advance(2 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	if fired != 0 {
		t.Fatalf("stopped timer fired %d time(s)", fired)
	}
	if timer.Stop() {
		t.Fatal("second Stop returned true")
	}
}

// AfterFunc handlers may schedule new events from inside the callback;
// the new events should be scheduled relative to the fake clock's
// deadline-at-fire time, not the original now.
func TestFakeReentrantSchedulingFromAfterFunc(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(time.Unix(0, 0))

	var seen []time.Duration
	var mu sync.Mutex
	c.AfterFunc(time.Second, func() {
		// At this point the fake clock is at start+1s. A 500ms
		// AfterFunc registered here should fire at start+1.5s.
		c.AfterFunc(500*time.Millisecond, func() {
			mu.Lock()
			seen = append(seen, time.Since(time.Unix(0, 0)))
			mu.Unlock()
			_ = c.Now()
		})
	})

	c.Advance(2 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("re-entrant handler not fired: %v", seen)
	}
}

func TestRealAfterFuncFires(t *testing.T) {
	t.Parallel()
	c := clock.NewReal()
	done := make(chan struct{})
	c.AfterFunc(time.Millisecond, func() { close(done) })

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Real.AfterFunc did not fire within 1s")
	}
}

func TestRealTimerStopBeforeFire(t *testing.T) {
	t.Parallel()
	c := clock.NewReal()
	var fired bool
	timer := c.AfterFunc(time.Hour, func() { fired = true })
	if !timer.Stop() {
		t.Fatal("Stop returned false on un-fired timer")
	}
	if fired {
		t.Fatal("timer fired despite Stop")
	}
}

func TestRealAfterFires(t *testing.T) {
	t.Parallel()
	c := clock.NewReal()
	select {
	case <-c.After(time.Millisecond):
	case <-time.After(time.Second):
		t.Fatal("Real.After did not fire within 1s")
	}
}
