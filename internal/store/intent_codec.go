package store

import (
	"encoding/json"
	"fmt"
	"time"

	"resy-snipe/internal/domain"
)

// intentJSON is the on-disk shape of an Intent. Domain types stay
// pure; this struct exists only at the persistence boundary and may
// evolve independently of domain.Intent (with a migration if needed).
type intentJSON struct {
	User      domain.UserID    `json:"user"`
	Venue     domain.VenueRef  `json:"venue"`
	Date      string           `json:"date"`
	PartySize int              `json:"party_size"`
	SlotPrefs []slotPrefJSON   `json:"slot_prefs,omitempty"`
	Release   *releaseJSON     `json:"release,omitempty"`
}

type slotPrefJSON struct {
	Time      string `json:"time"`
	TableType string `json:"table_type,omitempty"`
}

// releaseJSON encodes a ReleaseStrategy with a discriminator so the
// store boundary can roundtrip the sealed union. Only one of the
// time fields is populated for any given Kind.
type releaseJSON struct {
	Kind       string    `json:"kind"`
	At         time.Time `json:"at,omitempty"`
	ProbeFrom  time.Time `json:"probe_from,omitempty"`
	ProbeUntil time.Time `json:"probe_until,omitempty"`
	Until      time.Time `json:"until,omitempty"`
}

const (
	releaseKindExplicit   = "explicit"
	releaseKindDiscovered = "discovered"
	releaseKindContinuous = "continuous"
)

// MarshalIntent encodes a domain.Intent for storage.
func MarshalIntent(i domain.Intent) ([]byte, error) {
	out := intentJSON{
		User:      i.User,
		Venue:     i.Venue,
		Date:      i.Date.String(),
		PartySize: i.PartySize,
	}
	for _, p := range i.SlotPrefs {
		out.SlotPrefs = append(out.SlotPrefs, slotPrefJSON{
			Time:      p.Time.String(),
			TableType: p.TableType,
		})
	}
	rel, err := encodeRelease(i.Release)
	if err != nil {
		return nil, err
	}
	out.Release = rel
	return json.Marshal(out)
}

// UnmarshalIntent decodes wire bytes back into a domain.Intent.
func UnmarshalIntent(data []byte) (domain.Intent, error) {
	var raw intentJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return domain.Intent{}, fmt.Errorf("UnmarshalIntent: %w", err)
	}
	d, err := domain.ParseDate(raw.Date)
	if err != nil {
		return domain.Intent{}, fmt.Errorf("UnmarshalIntent date: %w", err)
	}
	out := domain.Intent{
		User:      raw.User,
		Venue:     raw.Venue,
		Date:      d,
		PartySize: raw.PartySize,
	}
	for _, p := range raw.SlotPrefs {
		w, err := domain.ParseWallTime(p.Time)
		if err != nil {
			return domain.Intent{}, fmt.Errorf("UnmarshalIntent slot time %q: %w", p.Time, err)
		}
		out.SlotPrefs = append(out.SlotPrefs, domain.SlotPreference{
			Time:      w,
			TableType: p.TableType,
		})
	}
	rel, err := decodeRelease(raw.Release)
	if err != nil {
		return domain.Intent{}, err
	}
	out.Release = rel
	return out, nil
}

func encodeRelease(r domain.ReleaseStrategy) (*releaseJSON, error) {
	switch v := r.(type) {
	case nil:
		return nil, nil
	case domain.ExplicitRelease:
		return &releaseJSON{Kind: releaseKindExplicit, At: v.At}, nil
	case domain.DiscoveredRelease:
		return &releaseJSON{
			Kind:       releaseKindDiscovered,
			ProbeFrom:  v.ProbeFrom,
			ProbeUntil: v.ProbeUntil,
		}, nil
	case domain.ContinuousRelease:
		return &releaseJSON{Kind: releaseKindContinuous, Until: v.Until}, nil
	default:
		return nil, fmt.Errorf("encodeRelease: unknown variant %T", r)
	}
}

func decodeRelease(j *releaseJSON) (domain.ReleaseStrategy, error) {
	if j == nil {
		return nil, nil
	}
	switch j.Kind {
	case releaseKindExplicit:
		return domain.ExplicitRelease{At: j.At}, nil
	case releaseKindDiscovered:
		return domain.DiscoveredRelease{ProbeFrom: j.ProbeFrom, ProbeUntil: j.ProbeUntil}, nil
	case releaseKindContinuous:
		return domain.ContinuousRelease{Until: j.Until}, nil
	default:
		return nil, fmt.Errorf("decodeRelease: unknown kind %q", j.Kind)
	}
}

// MarshalResult / UnmarshalResult round-trip the Result struct. Result
// is a flat record so no discriminator is needed.
type resultJSON struct {
	BookedAt       time.Time `json:"booked_at"`
	ConfirmationID string    `json:"confirmation_id"`
}

func MarshalResult(r *domain.Result) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	return json.Marshal(resultJSON{
		BookedAt:       r.BookedAt,
		ConfirmationID: r.ConfirmationID,
	})
}

func UnmarshalResult(data []byte) (*domain.Result, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var raw resultJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("UnmarshalResult: %w", err)
	}
	return &domain.Result{
		BookedAt:       raw.BookedAt,
		ConfirmationID: raw.ConfirmationID,
	}, nil
}
