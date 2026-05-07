package store_test

import (
	"strings"
	"testing"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/store"
)

// Compile-time: ResySlotPayload satisfies the sealed SlotPayload
// interface. If a new variant is added without isSlotPayload(), this
// file is the canary.
var _ domain.SlotPayload = domain.ResySlotPayload{}

func TestSlotPayloadRoundTripPreservesType(t *testing.T) {
	t.Parallel()
	original := domain.ResySlotPayload{
		ConfigID:    "rgs://resy/cal/v2/abc/2/2026-06-01/2026-06-01/19:30:00/2/Bar",
		TemplateID:  "tmpl_xyz",
		SeatingType: "Bar",
	}
	data, err := store.MarshalSlotPayload(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"kind":"resy"`) {
		t.Fatalf("envelope missing kind discriminator: %s", data)
	}

	got, err := store.UnmarshalSlotPayload(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	resy, ok := got.(domain.ResySlotPayload)
	if !ok {
		t.Fatalf("round-trip lost concrete type: got %T", got)
	}
	if resy != original {
		t.Fatalf("round-trip lost data: got %+v want %+v", resy, original)
	}
}

func TestSlotPayloadMarshalRejectsNil(t *testing.T) {
	t.Parallel()
	if _, err := store.MarshalSlotPayload(nil); err == nil {
		t.Fatal("nil payload must error")
	}
}

func TestSlotPayloadUnmarshalRejectsMissingKind(t *testing.T) {
	t.Parallel()
	_, err := store.UnmarshalSlotPayload([]byte(`{"payload":{}}`))
	if err == nil {
		t.Fatal("missing kind must error")
	}
}

func TestSlotPayloadUnmarshalRejectsUnknownKind(t *testing.T) {
	t.Parallel()
	_, err := store.UnmarshalSlotPayload([]byte(`{"kind":"opentable","payload":{}}`))
	if err == nil {
		t.Fatal("unknown kind must error")
	}
}

func TestSlotPayloadUnmarshalRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := store.UnmarshalSlotPayload([]byte(`not json`)); err == nil {
		t.Fatal("garbage bytes must error")
	}
}

// Sentinel test: the codec must reject any SlotPayload variant the
// switch hasn't been updated for. Today only ResySlotPayload is
// declared; a future variant added without codec love will trip this
// because Marshal returns an "unknown variant" error.
//
// We can't construct a foreign variant from outside the domain package
// (the marker method is unexported), so we simulate the assertion by
// confirming the only valid variant succeeds.
func TestSlotPayloadCodecExhaustiveOverKnownVariants(t *testing.T) {
	t.Parallel()
	variants := []domain.SlotPayload{
		domain.ResySlotPayload{ConfigID: "x"},
	}
	for _, v := range variants {
		if _, err := store.MarshalSlotPayload(v); err != nil {
			t.Errorf("Marshal(%T) failed: %v", v, err)
		}
	}
}
