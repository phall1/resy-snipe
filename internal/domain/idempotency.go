package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// IdempotencyKey returns a deterministic key for a single /3/book
// attempt. The Resy adapter sends this key with every booking call so
// the upstream service can deduplicate retries (for example, a
// connection-reset retry on the same attempt) but cannot mistake a
// legitimate next attempt for a retry of the previous one.
//
// The key is sha256 hex over (intent_hash, canonical slot payload,
// attempt). intent_hash already incorporates user, venue, date, party,
// slot prefs, and release; the slot payload pins the exact slot we are
// booking; attempt makes a deliberate retry distinguishable from an
// accidental loop. A bug-loop that sends the same attempt number twice
// produces the same key and is dropped server-side; a bug-loop that
// genuinely increments attempt without legitimate intent issues a new
// key but only after the prior attempt resolved.
func IdempotencyKey(intentHash IntentHash, slot SlotPayload, attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "intent=%s\n", intentHash)
	writeSlotPayloadCanonical(&b, slot)
	fmt.Fprintf(&b, "attempt=%d\n", attempt)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// writeSlotPayloadCanonical emits a stable string form of a
// SlotPayload for hashing. It is intentionally separate from the
// store-side JSON codec so domain doesn't depend on store; both
// representations describe the same data, but this one is locked to
// the hash and can never change without invalidating every running
// snipe's idempotency keys.
func writeSlotPayloadCanonical(b *strings.Builder, p SlotPayload) {
	switch v := p.(type) {
	case nil:
		b.WriteString("slot=<nil>\n")
	case ResySlotPayload:
		fmt.Fprintf(b, "slot=resy config=%s template=%s seating=%s\n",
			v.ConfigID, v.TemplateID, v.SeatingType)
	default:
		// A new SlotPayload variant must extend this switch and the
		// store-side codec in lockstep. The default branch is here so
		// an omission produces a distinct, distinguishable key (rather
		// than silently hashing two variants together) — and the
		// engine's exhaustive variant tests catch the omission first.
		fmt.Fprintf(b, "slot=unknown(%T)\n", p)
	}
}
