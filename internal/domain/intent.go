package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
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
// describing the same booking ask must produce the same digest. It is
// reused as the basis for idempotency keys (see D4).
//
// The release portion writes both the variant tag and its typed fields
// in a fixed order so that distinct strategies — or the same strategy
// with different times — produce distinct hashes. Time values are
// serialized in UTC RFC-3339 nanos so identical wall-clock instants
// hash identically across timezones.
func (i Intent) Hash() IntentHash {
	var b strings.Builder
	fmt.Fprintf(&b, "user=%s\n", i.User)
	fmt.Fprintf(&b, "venue=%s\n", i.Venue)
	fmt.Fprintf(&b, "date=%s\n", i.Date)
	fmt.Fprintf(&b, "party=%d\n", i.PartySize)
	for _, p := range i.SlotPrefs {
		fmt.Fprintf(&b, "slot=%s/%s\n", p.Time, p.TableType)
	}
	writeReleaseHash(&b, i.Release)
	sum := sha256.Sum256([]byte(b.String()))
	return IntentHash(hex.EncodeToString(sum[:]))
}

func writeReleaseHash(b *strings.Builder, r ReleaseStrategy) {
	switch v := r.(type) {
	case nil:
		b.WriteString("release=<nil>\n")
	case ExplicitRelease:
		fmt.Fprintf(b, "release=explicit at=%s\n", utcNanos(v.At))
	case DiscoveredRelease:
		fmt.Fprintf(b, "release=discovered from=%s until=%s\n",
			utcNanos(v.ProbeFrom), utcNanos(v.ProbeUntil))
	case ContinuousRelease:
		fmt.Fprintf(b, "release=continuous until=%s\n", utcNanos(v.Until))
	default:
		// A new ReleaseStrategy variant must update this switch and
		// appear in the codec/persistence layer. The default branch
		// is a loud failure so the omission is unmistakable.
		fmt.Fprintf(b, "release=unknown(%T)\n", r)
	}
}

func utcNanos(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
