package engine

import (
	"sync"

	"resy-snipe/internal/domain"
)

// Notification is the event envelope delivered to engine subscribers.
// It bundles the new event with the from/to statuses so consumers do
// not have to maintain an external mirror of the state machine to
// render transitions.
//
// From is a pointer so the bootstrap Submit emission (which has no
// prior status) can be expressed as From == nil rather than colliding
// with StatusSubmitted (the zero value of domain.Status).
type Notification struct {
	SnipeID domain.SnipeID
	From    *domain.Status
	To      domain.Status
	Event   domain.Event
}

// Subscriber is the callback shape registered with Engine.Subscribe.
//
// Subscribers are invoked synchronously on the engine's emit path, in
// the order they registered, after the underlying transition has been
// persisted. They MUST NOT block — slow subscribers stall state-machine
// progress for every snipe in flight. CLI/daemon adapters that bridge
// to a slower transport (network, disk) are responsible for buffering
// internally.
//
// A panicking subscriber is contained: emit recovers per-subscriber so
// one buggy consumer cannot unwind the engine.
type Subscriber func(Notification)

// Subscribe registers fn for engine notifications. The returned cancel
// removes the subscription; it is idempotent and safe to call after
// the engine has stopped emitting.
//
// Subscribe is safe to call concurrently with emissions and with other
// Subscribe / cancel calls. The new subscriber will not see events
// emitted before Subscribe returned.
func (e *Engine) Subscribe(fn Subscriber) (cancel func()) {
	if fn == nil {
		// A nil subscriber would silently drop events; fail fast at
		// registration so the bug surfaces at boot.
		panic("engine: nil Subscriber")
	}
	e.subsMu.Lock()
	id := e.nextSubID
	e.nextSubID++
	if e.subs == nil {
		e.subs = make(map[uint64]Subscriber)
	}
	e.subs[id] = fn
	e.subsMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			e.subsMu.Lock()
			delete(e.subs, id)
			e.subsMu.Unlock()
		})
	}
}

// emit fans out a Notification to every current subscriber. The
// subscriber list is snapshotted under the lock and iterated outside
// it, so a subscriber may freely call Subscribe / cancel without
// deadlocking. Panics in subscribers are recovered and discarded —
// engine progress is the priority; a misbehaving notifier is its own
// bug.
func (e *Engine) emit(n Notification) {
	e.subsMu.RLock()
	if len(e.subs) == 0 {
		e.subsMu.RUnlock()
		return
	}
	// Snapshot in deterministic id order so subscriber callbacks see a
	// stable delivery sequence across emissions. Map iteration is
	// randomized in Go; the slice copy fixes the order.
	ids := make([]uint64, 0, len(e.subs))
	for id := range e.subs {
		ids = append(ids, id)
	}
	subs := make([]Subscriber, 0, len(ids))
	// Insertion order == ascending id since nextSubID is monotonic.
	sortUint64(ids)
	for _, id := range ids {
		subs = append(subs, e.subs[id])
	}
	e.subsMu.RUnlock()

	for _, fn := range subs {
		safeDeliver(fn, n)
	}
}

// safeDeliver invokes fn with n, recovering any panic so a single
// misbehaving subscriber cannot unwind the engine's emit path.
func safeDeliver(fn Subscriber, n Notification) {
	defer func() {
		_ = recover()
	}()
	fn(n)
}

// sortUint64 is a tiny insertion sort kept local to engine to avoid
// pulling in slices.Sort (and the comparator allocation that comes
// with it) on a hot path. Subscriber counts are O(1) in practice;
// insertion sort beats every alternative at that scale.
func sortUint64(a []uint64) {
	for i := 1; i < len(a); i++ {
		v := a[i]
		j := i - 1
		for j >= 0 && a[j] > v {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = v
	}
}
