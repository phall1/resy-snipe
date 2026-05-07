package domain_test

import (
	"log/slog"
	"regexp"
	"testing"
	"time"

	"resy-snipe/internal/domain"
)

func TestEventTypeStringsAreStable(t *testing.T) {
	t.Parallel()
	want := map[domain.EventType]string{
		domain.EventSubmitted:     "submitted",
		domain.EventScheduled:     "scheduled",
		domain.EventDiscovered:    "discovered",
		domain.EventReleased:      "released",
		domain.EventFound:         "found",
		domain.EventBookAttempted: "book_attempted",
		domain.EventBooked:        "booked",
		domain.EventFailed:        "failed",
		domain.EventCanceled:      "canceled",
		domain.EventExpired:       "expired",
	}
	for et, str := range want {
		if string(et) != str {
			t.Errorf("event type drifted: %v != %q", et, str)
		}
	}
	if got, expected := len(domain.AllEventTypes()), len(want); got != expected {
		t.Fatalf("AllEventTypes len = %d, want %d", got, expected)
	}
}

func TestEventCarriesTypedAttrs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 1, 19, 30, 0, 0, time.UTC)
	e := domain.Event{
		Type: domain.EventBooked,
		At:   now,
		Attrs: []slog.Attr{
			slog.String(domain.LogKeySnipeID, "snp_abc"),
			slog.Int(domain.LogKeyAttempt, 3),
			slog.String(domain.LogKeyResyRequestID, "req_xyz"),
		},
	}
	if e.Type != domain.EventBooked {
		t.Fatalf("Type lost: %s", e.Type)
	}
	if !e.At.Equal(now) {
		t.Fatalf("At lost: got %v want %v", e.At, now)
	}
	if len(e.Attrs) != 3 {
		t.Fatalf("attrs lost: %v", e.Attrs)
	}
}

var hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestIdempotencyKeyDeterministic(t *testing.T) {
	t.Parallel()
	intent := domain.IntentHash("ihash")
	slot := domain.ResySlotPayload{ConfigID: "c", TemplateID: "t", SeatingType: "Bar"}
	a := domain.IdempotencyKey(intent, slot, 1)
	b := domain.IdempotencyKey(intent, slot, 1)
	if a != b {
		t.Fatalf("same inputs produced different keys: %s vs %s", a, b)
	}
	if !hexSHA256.MatchString(a) {
		t.Fatalf("key is not 64 hex chars: %q", a)
	}
}

func TestIdempotencyKeyChangesWithAttempt(t *testing.T) {
	t.Parallel()
	intent := domain.IntentHash("ihash")
	slot := domain.ResySlotPayload{ConfigID: "c"}
	k1 := domain.IdempotencyKey(intent, slot, 1)
	k2 := domain.IdempotencyKey(intent, slot, 2)
	if k1 == k2 {
		t.Fatal("attempt change must produce a different key")
	}
}

func TestIdempotencyKeyChangesWithSlotPayload(t *testing.T) {
	t.Parallel()
	intent := domain.IntentHash("ihash")
	a := domain.IdempotencyKey(intent, domain.ResySlotPayload{ConfigID: "c1"}, 1)
	b := domain.IdempotencyKey(intent, domain.ResySlotPayload{ConfigID: "c2"}, 1)
	if a == b {
		t.Fatal("ConfigID change must produce a different key")
	}

	c := domain.IdempotencyKey(intent, domain.ResySlotPayload{ConfigID: "c1", SeatingType: "Bar"}, 1)
	if a == c {
		t.Fatal("SeatingType change must produce a different key")
	}
}

func TestIdempotencyKeyChangesWithIntentHash(t *testing.T) {
	t.Parallel()
	slot := domain.ResySlotPayload{ConfigID: "c"}
	a := domain.IdempotencyKey("ihash1", slot, 1)
	b := domain.IdempotencyKey("ihash2", slot, 1)
	if a == b {
		t.Fatal("intent hash change must produce a different key")
	}
}

func TestIdempotencyKeyHandlesNilSlot(t *testing.T) {
	t.Parallel()
	// nil is degenerate but must not panic, and must produce a key
	// distinguishable from a real slot.
	nilKey := domain.IdempotencyKey("ihash", nil, 1)
	if !hexSHA256.MatchString(nilKey) {
		t.Fatalf("nil-slot key not hex: %q", nilKey)
	}
	resyKey := domain.IdempotencyKey("ihash", domain.ResySlotPayload{}, 1)
	if nilKey == resyKey {
		t.Fatal("nil slot must hash differently from zero-value Resy slot")
	}
}
