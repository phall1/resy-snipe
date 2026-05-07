package domain

// ReleaseStrategy is a sealed union populated in D3 with three concrete
// variants (Explicit, Discovered, Continuous). The marker method makes
// it impossible for code outside this package to add a new variant —
// any pattern match in the engine stays exhaustive.
type ReleaseStrategy interface {
	isReleaseStrategy()
}

// SlotPayload is a sealed union populated in D2 with provider-specific
// variants (initially ResySlotPayload). Engine code never inspects it
// by JSON; it lives as a typed value in-memory.
type SlotPayload interface {
	isSlotPayload()
}
