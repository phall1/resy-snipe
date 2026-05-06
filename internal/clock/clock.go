package clock

import "time"

// Clock abstracts wall-clock access so production code can be tested
// deterministically. Production code outside this package must depend on
// Clock, not on the time package.
type Clock interface {
	// Now returns the clock's current time.
	Now() time.Time
	// After returns a channel that receives the clock's current time once
	// the duration has elapsed. The channel has capacity one so callers
	// that abandon it (e.g. via a context cancellation) do not block the
	// firing path.
	After(d time.Duration) <-chan time.Time
	// AfterFunc schedules f to run once after d has elapsed. The returned
	// Timer can be used to cancel the call before it fires.
	AfterFunc(d time.Duration, f func()) Timer
	// Sleep blocks for d as measured by this clock.
	Sleep(d time.Duration)
}

// Timer is a single scheduled callback.
type Timer interface {
	// Stop cancels the timer if it has not yet fired. It returns true if
	// the call prevented the timer from firing, and false if it had
	// already fired or had previously been stopped.
	Stop() bool
}
