package domain

import (
	"errors"
	"time"
)

// VenueRef is the (provider, ref) tuple that uniquely identifies a venue
// across the system. It is what an Intent carries; Venue (with name and
// timezone) is the resolved view, populated on first contact.
type VenueRef struct {
	Provider ProviderID
	Ref      string
}

// String returns "<provider>:<ref>".
func (v VenueRef) String() string { return string(v.Provider) + ":" + v.Ref }

// Venue is the resolved view of a venue. TZ is mandatory: every
// wall-clock comparison the engine makes about this venue must go
// through this location.
type Venue struct {
	Provider ProviderID
	Ref      string
	Name     string
	TZ       *time.Location
}

// AsRef returns the VenueRef portion.
func (v Venue) AsRef() VenueRef { return VenueRef{Provider: v.Provider, Ref: v.Ref} }

// ErrVenueMissingTZ is returned by Validate when TZ is nil.
var ErrVenueMissingTZ = errors.New("venue: TZ is required")

// Validate checks the mandatory fields. TZ is the load-bearing one.
func (v Venue) Validate() error {
	if v.Provider == "" {
		return errors.New("venue: Provider is required")
	}
	if v.Ref == "" {
		return errors.New("venue: Ref is required")
	}
	if v.TZ == nil {
		return ErrVenueMissingTZ
	}
	return nil
}

// BookingPolicy parameterises the engine's booking behavior for a
// single snipe. Defaults are picked by the engine; an Intent may
// override.
type BookingPolicy struct {
	// MaxConcurrent caps the number of parallel /3/book attempts the
	// engine will fire after find+details succeed.
	MaxConcurrent int
	// PollFloor is the minimum interval between consecutive polls; the
	// engine refuses to dispatch below this. The empirical Resy floor
	// is 100ms.
	PollFloor time.Duration
	// DetailsSerial forces /3/details fetches to run one at a time in
	// preference order, even when MaxConcurrent allows parallelism on
	// /3/book.
	DetailsSerial bool
}
