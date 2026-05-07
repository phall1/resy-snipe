package clock

import (
	"sort"
	"sync"
	"time"
)

// Fake is a deterministic Clock driven by Advance. Pending events fire in
// deadline order; ties break by registration order. Fake is safe for
// concurrent use, including re-entrant scheduling from inside an
// AfterFunc callback.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	pending []*fakeEvent
	seq     uint64
}

type fakeEvent struct {
	deadline time.Time
	seq      uint64
	fire     func(now time.Time)
	stopped  bool
}

// NewFake returns a Fake clock initialized to start.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	f.schedule(d, func(now time.Time) { ch <- now })
	return ch
}

func (f *Fake) AfterFunc(d time.Duration, fn func()) Timer {
	e := f.schedule(d, func(_ time.Time) { fn() })
	return &fakeTimer{f: f, e: e}
}

func (f *Fake) Sleep(d time.Duration) {
	<-f.After(d)
}

// Advance moves the fake clock forward by d. Every pending event whose
// deadline lies in (now, now+d] fires, in deadline order, with the
// fake clock set to the event's deadline at fire time. After all eligible
// events have fired, the clock is set to now+d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	target := f.now.Add(d)
	for len(f.pending) > 0 && !f.pending[0].deadline.After(target) {
		e := f.pending[0]
		f.pending = f.pending[1:]
		if e.stopped {
			continue
		}
		f.now = e.deadline
		deadline := e.deadline
		fire := e.fire
		f.mu.Unlock()
		fire(deadline)
		f.mu.Lock()
	}
	f.now = target
	f.mu.Unlock()
}

func (f *Fake) schedule(d time.Duration, fire func(time.Time)) *fakeEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	e := &fakeEvent{
		deadline: f.now.Add(d),
		seq:      f.seq,
		fire:     fire,
	}
	f.pending = append(f.pending, e)
	sort.SliceStable(f.pending, func(i, j int) bool {
		a, b := f.pending[i], f.pending[j]
		if a.deadline.Equal(b.deadline) {
			return a.seq < b.seq
		}
		return a.deadline.Before(b.deadline)
	})
	return e
}

func (f *Fake) stop(e *fakeEvent) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.stopped {
		return false
	}
	e.stopped = true
	return true
}

type fakeTimer struct {
	f *Fake
	e *fakeEvent
}

func (t *fakeTimer) Stop() bool { return t.f.stop(t.e) }
