package domain

// ResySlotPayload is the Resy-specific data the engine carries from
// /4/find through /3/details into /3/book. It implements the sealed
// SlotPayload union; the marker method isSlotPayload() is unexported,
// so no package outside domain can introduce a new variant.
//
// The struct is treated as a value, not a pointer. Engine and provider
// pass it by value so a copy made on goroutine fan-out cannot share
// state with its sibling.
//
// BookToken and PaymentMethodID are populated by the adapter's
// PrepareSlot step (which calls /3/details). They are zero on a slot
// returned directly from Find; ConfirmSlot requires both to be set.
type ResySlotPayload struct {
	ConfigID        string
	TemplateID      string
	SeatingType     string
	BookToken       string
	PaymentMethodID int64
}

func (ResySlotPayload) isSlotPayload() {}
