package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// PlanHashVersionV1 is the canonicalization rule version. Bump when a
// canonicalization change would alter previously-computed hashes; the
// daemon compares versions before comparing hashes (see ADR-0012).
const PlanHashVersionV1 = 1

// Plan is the planner's output and the user-approvable artifact
// described in ADR-0012. Its hash is content-addressable over the
// fields the engine actually consumes; Notes and ComputedAt are
// volatile and excluded from the hash.
type Plan struct {
	Goal            Goal            `json:"goal"`
	Venue           Venue           `json:"venue"`
	DropMoment      time.Time       `json:"drop_moment"`
	Strategy        ReleaseStrategy `json:"strategy"`
	FireSchedule    []time.Time     `json:"fire_schedule,omitempty"`
	SigningRequired bool            `json:"signing_required"`
	Hash            string          `json:"hash,omitempty"`
	Notes           []string        `json:"notes,omitempty"`
	PlanHashVersion int             `json:"plan_hash_version"`
	ComputedAt      time.Time       `json:"computed_at"`
}

// ErrPlanStrategyUnknown is returned when CanonicalBytes encounters a
// ReleaseStrategy variant the canonicalizer cannot serialize. New
// variants must update both the canonicalizer and the engine.
var ErrPlanStrategyUnknown = errors.New("plan: unknown release strategy variant")

// CanonicalBytes returns the JCS-style canonical JSON used to compute
// Hash. It drops volatile fields (Hash, Notes, ComputedAt) and sorts
// object keys recursively so identical Plans serialize byte-identically
// regardless of map iteration order or marshaller. See planner.md
// §plan-hash-canonicalization for the exact algorithm.
func (p Plan) CanonicalBytes() ([]byte, error) {
	payload, err := canonicalPayload(p)
	if err != nil {
		return nil, err
	}
	envelope := map[string]any{
		"plan_hash_version": p.planHashVersion(),
		"payload":           payload,
	}
	return canonicalMarshal(envelope)
}

// ComputeHash returns the SHA-256 of CanonicalBytes encoded as a
// "sha256:<hex>" string. The prefix names the algorithm so a future
// migration to a stronger hash is detectable.
func (p Plan) ComputeHash() (string, error) {
	buf, err := p.CanonicalBytes()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// WithHash returns a copy of p with Hash set to ComputeHash's result.
// Callers persist the returned value; the Service layer pins the hash
// when storing a Quest so a later CreateQuest can detect drift.
func (p Plan) WithHash() (Plan, error) {
	h, err := p.ComputeHash()
	if err != nil {
		return Plan{}, err
	}
	p.Hash = h
	return p, nil
}

func (p Plan) planHashVersion() int {
	if p.PlanHashVersion <= 0 {
		return PlanHashVersionV1
	}
	return p.PlanHashVersion
}

// kindKey is the discriminator field name used inside every canonical
// union payload (VenueQuery, ReleaseStrategy). Keeping it as a const
// prevents drift across the helpers below.
const kindKey = "kind"

// canonicalPayload builds the typed payload that goes inside the
// versioned envelope. Field order in the resulting map is irrelevant
// because canonicalMarshal sorts keys.
func canonicalPayload(p Plan) (map[string]any, error) {
	strategy, err := canonicalStrategy(p.Strategy)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"goal":             canonicalGoal(p.Goal),
		"venue":            canonicalVenue(p.Venue),
		"drop_moment":      utcNanos(p.DropMoment),
		"strategy":         strategy,
		"fire_schedule":    canonicalTimes(p.FireSchedule),
		"signing_required": p.SigningRequired,
	}, nil
}

func canonicalGoal(g Goal) map[string]any {
	return map[string]any{
		"venue_query": canonicalVenueQuery(g.VenueQuery),
		"date":        g.Date.String(),
		"party":       g.Party,
		"time_prefs": map[string]any{
			"start":    g.TimePrefs.Start.String(),
			"end":      g.TimePrefs.End.String(),
			"priority": g.TimePrefs.Priority.String(),
		},
		"account_id":  string(g.AccountID),
		"constraints": canonicalConstraints(g.Constraints),
	}
}

func canonicalVenueQuery(q VenueQuery) map[string]any {
	switch v := q.(type) {
	case VenueQueryURL:
		return map[string]any{kindKey: "url", "url": v.URL}
	case VenueQuerySlug:
		return map[string]any{kindKey: "slug", "slug": v.Slug, "city": v.City}
	case VenueQueryName:
		return map[string]any{kindKey: "name", "name": v.Name}
	case nil:
		return map[string]any{kindKey: "nil"}
	default:
		return map[string]any{kindKey: fmt.Sprintf("unknown(%T)", v)}
	}
}

func canonicalConstraints(c Constraints) map[string]any {
	tables := append([]string(nil), c.TableTypes...)
	sort.Strings(tables)
	return map[string]any{
		"table_types": tables,
		"any_table":   c.AnyTable,
	}
}

func canonicalVenue(v Venue) map[string]any {
	tz := ""
	if v.TZ != nil {
		tz = v.TZ.String()
	}
	return map[string]any{
		"provider": string(v.Provider),
		"ref":      v.Ref,
		"tz":       tz,
	}
}

func canonicalStrategy(r ReleaseStrategy) (map[string]any, error) {
	switch v := r.(type) {
	case nil:
		return map[string]any{"tag": "nil"}, nil
	case ExplicitRelease:
		return map[string]any{
			"tag": "explicit",
			"at":  utcNanos(v.At),
		}, nil
	case DiscoveredRelease:
		return map[string]any{
			"tag":         "discovered",
			"probe_from":  utcNanos(v.ProbeFrom),
			"probe_until": utcNanos(v.ProbeUntil),
		}, nil
	case ContinuousRelease:
		return map[string]any{
			"tag":   "continuous",
			"until": utcNanos(v.Until),
		}, nil
	default:
		return nil, fmt.Errorf("%w: %T", ErrPlanStrategyUnknown, r)
	}
}

func canonicalTimes(ts []time.Time) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = utcNanos(t)
	}
	return out
}

// canonicalMarshal emits JSON with keys sorted lexicographically at
// every level. encoding/json does not sort map keys for non-string
// keys nor preserve a sorted view across nested maps; this walker
// guarantees both.
func canonicalMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case map[string]any:
		buf.WriteByte('{')
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case []any:
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case []string:
		buf.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			sb, err := json.Marshal(s)
			if err != nil {
				return err
			}
			buf.Write(sb)
		}
		buf.WriteByte(']')
		return nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(b)
		return nil
	}
}
