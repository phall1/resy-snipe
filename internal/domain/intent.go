package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// SlotPreference captures one (time-of-day, optional table type) pair
// that the user is willing to book. Engine code iterates these in order
// to choose which slot to /3/details / /3/book first.
type SlotPreference struct {
	Time      WallTime
	TableType string // empty means "any"
}

// Intent is what the user asks for. Once constructed it is treated as
// immutable; the content-addressable Hash() is stable across runs and
// is reused as the basis for idempotency keys (see D4).
type Intent struct {
	User      UserID
	Venue     VenueRef
	Date      Date
	PartySize int
	SlotPrefs []SlotPreference
	Release   ReleaseStrategy
}

// Hash returns a stable hex-encoded SHA-256 of the intent's content.
// The hash is what makes the Intent content-addressable: two intents
// describing the same booking ask must produce the same digest.
//
// D3 will extend ReleaseStrategy with concrete variants and refine the
// release portion of this hash. The current implementation uses the
// dynamic type name as a placeholder so the hash remains stable for
// non-release-bearing intents and so callers can rely on Hash() existing.
func (i Intent) Hash() IntentHash {
	var b strings.Builder
	fmt.Fprintf(&b, "user=%s\n", i.User)
	fmt.Fprintf(&b, "venue=%s\n", i.Venue)
	fmt.Fprintf(&b, "date=%s\n", i.Date)
	fmt.Fprintf(&b, "party=%d\n", i.PartySize)
	for _, p := range i.SlotPrefs {
		fmt.Fprintf(&b, "slot=%s/%s\n", p.Time, p.TableType)
	}
	if i.Release == nil {
		fmt.Fprintf(&b, "release=<nil>\n")
	} else {
		fmt.Fprintf(&b, "release=%T\n", i.Release)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return IntentHash(hex.EncodeToString(sum[:]))
}
