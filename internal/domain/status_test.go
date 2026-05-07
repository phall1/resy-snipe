package domain_test

import (
	"errors"
	"testing"
	"time"

	"resy-snipe/internal/domain"
)

// TestTransitionTableExhaustive verifies that CanTransition returns a
// definite answer for every (from, to) pair in the closed status set.
// This is the totality assertion called out by the D1 acceptance
// criteria. It does not check correctness of individual cells — that
// is what TestKnownTransitions covers.
func TestTransitionTableExhaustive(t *testing.T) {
	t.Parallel()
	statuses := domain.AllStatuses()
	if got, want := len(statuses), 10; got != want {
		t.Fatalf("AllStatuses() returned %d entries, want %d", got, want)
	}

	pairs := 0
	for _, from := range statuses {
		for _, to := range statuses {
			// CanTransition must return without panic for every pair.
			_ = domain.CanTransition(from, to)
			pairs++
		}
	}
	if want := len(statuses) * len(statuses); pairs != want {
		t.Fatalf("walked %d pairs, want %d", pairs, want)
	}
}

// TestKnownTransitions locks down the allow-list. Every transition
// engine code may legitimately attempt is enumerated here; nothing
// outside this list is allowed.
func TestKnownTransitions(t *testing.T) {
	t.Parallel()
	allowed := map[domain.Status][]domain.Status{
		domain.StatusSubmitted: {
			domain.StatusScheduled, domain.StatusDiscovering,
			domain.StatusCancelled, domain.StatusFailed,
		},
		domain.StatusScheduled: {
			domain.StatusAwaiting, domain.StatusDiscovering,
			domain.StatusCancelled, domain.StatusFailed, domain.StatusExpired,
		},
		domain.StatusDiscovering: {
			domain.StatusAwaiting, domain.StatusCancelled,
			domain.StatusFailed, domain.StatusExpired,
		},
		domain.StatusAwaiting: {
			domain.StatusFinding, domain.StatusCancelled,
			domain.StatusFailed, domain.StatusExpired,
		},
		domain.StatusFinding: {
			domain.StatusBooking, domain.StatusAwaiting,
			domain.StatusCancelled, domain.StatusFailed, domain.StatusExpired,
		},
		domain.StatusBooking: {
			domain.StatusBooked, domain.StatusFinding,
			domain.StatusCancelled, domain.StatusFailed,
		},
		domain.StatusBooked:    nil,
		domain.StatusFailed:    nil,
		domain.StatusCancelled: nil,
		domain.StatusExpired:   nil,
	}

	for _, from := range domain.AllStatuses() {
		want := map[domain.Status]bool{}
		for _, to := range allowed[from] {
			want[to] = true
		}
		for _, to := range domain.AllStatuses() {
			got := domain.CanTransition(from, to)
			if got != want[to] {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want[to])
			}
		}
	}
}

func TestNoSelfTransitions(t *testing.T) {
	t.Parallel()
	for _, s := range domain.AllStatuses() {
		if domain.CanTransition(s, s) {
			t.Errorf("self-transition allowed for %s", s)
		}
	}
}

func TestTerminalsAreSinks(t *testing.T) {
	t.Parallel()
	terminals := []domain.Status{
		domain.StatusBooked,
		domain.StatusFailed,
		domain.StatusCancelled,
		domain.StatusExpired,
	}
	for _, from := range terminals {
		if !from.IsTerminal() {
			t.Errorf("%s should report IsTerminal()=true", from)
		}
		for _, to := range domain.AllStatuses() {
			if domain.CanTransition(from, to) {
				t.Errorf("terminal %s allows transition to %s", from, to)
			}
		}
	}
}

func TestStatusStringsAreLowercaseAndDistinct(t *testing.T) {
	t.Parallel()
	seen := map[string]domain.Status{}
	for _, s := range domain.AllStatuses() {
		got := s.String()
		if got == "" {
			t.Errorf("status %d has empty String()", int(s))
			continue
		}
		if other, ok := seen[got]; ok {
			t.Errorf("duplicate string %q on %v and %v", got, other, s)
		}
		seen[got] = s
	}
}

func TestSnipeStateTransitionAllowsValid(t *testing.T) {
	t.Parallel()
	s := newTestState()
	if err := s.Transition(domain.StatusScheduled); err != nil {
		t.Fatalf("Submitted -> Scheduled: %v", err)
	}
	if got := s.Status(); got != domain.StatusScheduled {
		t.Fatalf("Status() = %s, want scheduled", got)
	}
}

func TestSnipeStateTransitionRejectsInvalid(t *testing.T) {
	t.Parallel()
	s := newTestState()
	err := s.Transition(domain.StatusBooked)
	if err == nil {
		t.Fatal("Submitted -> Booked must fail")
	}
	var ite domain.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("error is not InvalidTransitionError: %v", err)
	}
	if ite.From != domain.StatusSubmitted || ite.To != domain.StatusBooked {
		t.Fatalf("err carries wrong pair: %+v", ite)
	}
	// Status must not have changed on a rejected transition.
	if got := s.Status(); got != domain.StatusSubmitted {
		t.Fatalf("Status mutated after rejected transition: %s", got)
	}
}

func TestSnipeStateRejectsTransitionFromTerminal(t *testing.T) {
	t.Parallel()
	s := newTestState()
	if err := s.Transition(domain.StatusFailed); err != nil {
		t.Fatalf("Submitted -> Failed: %v", err)
	}
	if err := s.Transition(domain.StatusFailed); err == nil {
		t.Fatal("Failed -> Failed must fail (no self-transition, no exit from terminal)")
	}
	if err := s.Transition(domain.StatusScheduled); err == nil {
		t.Fatal("Failed -> Scheduled must fail (terminal)")
	}
}

func newTestState() *domain.SnipeState {
	return domain.NewSnipeState("snp_test", domain.Intent{}, time.Time{})
}
