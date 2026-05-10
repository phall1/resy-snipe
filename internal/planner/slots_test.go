package planner_test

import (
	"errors"
	"testing"

	"resy-snipe/internal/domain"
	"resy-snipe/internal/planner"
)

// wt is a local shorthand for domain.NewWallTime so the test tables stay
// readable.
func wt(h, m int) domain.WallTime { return domain.NewWallTime(h, m, 0) }

// TestExpandSlots_30MinIntervals exercises the canonical case from the
// design doc: an 18:30..21:00 window with PriorityEarlier emits six
// half-hour candidates in ascending order, with no table-type axis
// (AnyTable=true → single anonymous entry).
func TestExpandSlots_30MinIntervals(t *testing.T) {
	t.Parallel()

	got, err := planner.ExpandSlots(
		domain.TimeWindow{
			Start:    wt(18, 30),
			End:      wt(21, 0),
			Priority: domain.PriorityEarlier,
		},
		domain.Constraints{AnyTable: true},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []domain.SlotPreference{
		{Time: wt(18, 30), TableType: ""},
		{Time: wt(19, 0), TableType: ""},
		{Time: wt(19, 30), TableType: ""},
		{Time: wt(20, 0), TableType: ""},
		{Time: wt(20, 30), TableType: ""},
		{Time: wt(21, 0), TableType: ""},
	}
	assertSlotsEqual(t, got, want)
}

// TestExpandSlots_SinglePointWindow exercises the Start==End edge: one
// candidate at exactly that time.
func TestExpandSlots_SinglePointWindow(t *testing.T) {
	t.Parallel()
	got, err := planner.ExpandSlots(
		domain.TimeWindow{
			Start:    wt(19, 0),
			End:      wt(19, 0),
			Priority: domain.PriorityEarlier,
		},
		domain.Constraints{},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Time != wt(19, 0) || got[0].TableType != "" {
		t.Errorf("got %+v, want {19:00, ''}", got[0])
	}
}

// TestExpandSlots_PriorityLaterDescending pins the descending
// ordering of PriorityLater. The same six candidates as the canonical
// case, just reversed.
func TestExpandSlots_PriorityLaterDescending(t *testing.T) {
	t.Parallel()
	got, err := planner.ExpandSlots(
		domain.TimeWindow{
			Start:    wt(18, 30),
			End:      wt(21, 0),
			Priority: domain.PriorityLater,
		},
		domain.Constraints{AnyTable: true},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []domain.SlotPreference{
		{Time: wt(21, 0), TableType: ""},
		{Time: wt(20, 30), TableType: ""},
		{Time: wt(20, 0), TableType: ""},
		{Time: wt(19, 30), TableType: ""},
		{Time: wt(19, 0), TableType: ""},
		{Time: wt(18, 30), TableType: ""},
	}
	assertSlotsEqual(t, got, want)
}

// TestExpandSlots_PriorityEarlierAscending is the explicit positive
// counterpart to PriorityLater — guards against a future refactor that
// silently flips the default direction.
func TestExpandSlots_PriorityEarlierAscending(t *testing.T) {
	t.Parallel()
	got, err := planner.ExpandSlots(
		domain.TimeWindow{
			Start:    wt(18, 30),
			End:      wt(20, 0),
			Priority: domain.PriorityEarlier,
		},
		domain.Constraints{AnyTable: true},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	wantTimes := []domain.WallTime{wt(18, 30), wt(19, 0), wt(19, 30), wt(20, 0)}
	if len(got) != len(wantTimes) {
		t.Fatalf("len = %d, want %d", len(got), len(wantTimes))
	}
	for i, w := range wantTimes {
		if got[i].Time != w {
			t.Errorf("got[%d].Time = %s, want %s", i, got[i].Time, w)
		}
	}
}

// TestExpandSlots_PriorityNoneAscending exercises the
// "PriorityNone defers to planner default" rule — v1 default is
// ascending.
func TestExpandSlots_PriorityNoneAscending(t *testing.T) {
	t.Parallel()
	got, err := planner.ExpandSlots(
		domain.TimeWindow{
			Start:    wt(18, 30),
			End:      wt(19, 30),
			Priority: domain.PriorityNone,
		},
		domain.Constraints{},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	wantTimes := []domain.WallTime{wt(18, 30), wt(19, 0), wt(19, 30)}
	if len(got) != len(wantTimes) {
		t.Fatalf("len = %d, want %d", len(got), len(wantTimes))
	}
	for i, w := range wantTimes {
		if got[i].Time != w {
			t.Errorf("got[%d].Time = %s, want %s", i, got[i].Time, w)
		}
	}
}

// TestExpandSlots_TableTypeCrossProduct asserts the time × table-type
// fan-out: a 6-time window cross-producted with two table types yields
// 12 SlotPreferences, in (time, then user-supplied table order) order.
func TestExpandSlots_TableTypeCrossProduct(t *testing.T) {
	t.Parallel()
	got, err := planner.ExpandSlots(
		domain.TimeWindow{
			Start:    wt(18, 30),
			End:      wt(21, 0),
			Priority: domain.PriorityEarlier,
		},
		domain.Constraints{TableTypes: []string{"bar", "patio"}},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 12 {
		t.Fatalf("len = %d, want 12 (6 times × 2 tables)", len(got))
	}
	// Spot-check ordering: time-major, table-minor.
	want := []domain.SlotPreference{
		{Time: wt(18, 30), TableType: "bar"},
		{Time: wt(18, 30), TableType: "patio"},
		{Time: wt(19, 0), TableType: "bar"},
		{Time: wt(19, 0), TableType: "patio"},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestExpandSlots_AnyTableOverridesTableTypes asserts that AnyTable=true
// collapses the table-type axis even when TableTypes is non-empty —
// the user's "ignore my prior preferences" knob.
func TestExpandSlots_AnyTableOverridesTableTypes(t *testing.T) {
	t.Parallel()
	got, err := planner.ExpandSlots(
		domain.TimeWindow{
			Start:    wt(19, 0),
			End:      wt(20, 0),
			Priority: domain.PriorityEarlier,
		},
		domain.Constraints{
			TableTypes: []string{"bar", "patio"},
			AnyTable:   true,
		},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (3 times, any-table collapses)", len(got))
	}
	for i, p := range got {
		if p.TableType != "" {
			t.Errorf("got[%d].TableType = %q, want empty (AnyTable forces collapse)", i, p.TableType)
		}
	}
}

// TestExpandSlots_StartAfterEndError pins the inverted-window sentinel.
func TestExpandSlots_StartAfterEndError(t *testing.T) {
	t.Parallel()
	_, err := planner.ExpandSlots(
		domain.TimeWindow{
			Start:    wt(21, 0),
			End:      wt(18, 30),
			Priority: domain.PriorityEarlier,
		},
		domain.Constraints{},
	)
	if !errors.Is(err, planner.ErrInvalidWindow) {
		t.Fatalf("err = %v, want errors.Is %v", err, planner.ErrInvalidWindow)
	}
}

// TestExpandSlots_NonStepAlignedEnd documents v1 truncation behavior
// when (End − Start) is not a multiple of 30 minutes. A 18:30..20:15
// window emits 18:30, 19:00, 19:30, 20:00 — the 20:15 endpoint is not
// itself a 30-minute multiple from Start so it does not appear.
func TestExpandSlots_NonStepAlignedEnd(t *testing.T) {
	t.Parallel()
	got, err := planner.ExpandSlots(
		domain.TimeWindow{
			Start:    wt(18, 30),
			End:      wt(20, 15),
			Priority: domain.PriorityEarlier,
		},
		domain.Constraints{},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	wantTimes := []domain.WallTime{wt(18, 30), wt(19, 0), wt(19, 30), wt(20, 0)}
	if len(got) != len(wantTimes) {
		t.Fatalf("len = %d, want %d", len(got), len(wantTimes))
	}
	for i, w := range wantTimes {
		if got[i].Time != w {
			t.Errorf("got[%d].Time = %s, want %s", i, got[i].Time, w)
		}
	}
}

// TestExpandSlots_PreservesUserTableOrder asserts that the table-type
// axis is emitted in the user-supplied order, not sorted. This is a
// race-ordering contract: the engine walks Intent.SlotPrefs head-to-tail
// per the BookingPolicy.
func TestExpandSlots_PreservesUserTableOrder(t *testing.T) {
	t.Parallel()
	got, err := planner.ExpandSlots(
		domain.TimeWindow{
			Start:    wt(19, 0),
			End:      wt(19, 0),
			Priority: domain.PriorityEarlier,
		},
		domain.Constraints{TableTypes: []string{"patio", "bar", "counter"}},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	wantOrder := []string{"patio", "bar", "counter"}
	if len(got) != len(wantOrder) {
		t.Fatalf("len = %d, want %d", len(got), len(wantOrder))
	}
	for i, w := range wantOrder {
		if got[i].TableType != w {
			t.Errorf("got[%d].TableType = %q, want %q", i, got[i].TableType, w)
		}
	}
}

// assertSlotsEqual asserts deep equality with a friendly diff message.
func assertSlotsEqual(t *testing.T, got, want []domain.SlotPreference) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, len(want)=%d\ngot=%v\nwant=%v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%+v, want=%+v", i, got[i], want[i])
		}
	}
}
