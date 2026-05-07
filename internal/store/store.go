package store

import (
	"context"
	"time"

	"resy-snipe/internal/domain"
)

// SessionRow is the persisted view of a provider Session. Adapters
// transform their typed Session into a SessionRow at write time and
// rehydrate from a SessionRow at read time.
type SessionRow struct {
	UserID    domain.UserID
	Provider  domain.ProviderID
	JWT       string
	ExpiresAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ObservedRelease is one cached "venue X opens reservations at this
// local time of day, this many days before the target date" record.
// The engine writes one after a successful DiscoveredRelease run; the
// next snipe for the same venue defaults to ExplicitRelease at this
// time.
type ObservedRelease struct {
	Venue             domain.VenueRef
	ObservedLocalTime domain.WallTime
	DaysOffset        int
	ObservedAt        time.Time
}

// Store is the persistence seam. Every method takes ctx; every method
// either returns a sentinel (ErrNotFound, ErrSessionExpired) or wraps
// one with %w. Engine code branches on errors.Is.
type Store interface {
	// Snipes -----------------------------------------------------
	CreateSnipe(ctx context.Context, s *domain.SnipeState) error
	GetSnipe(ctx context.Context, id domain.SnipeID) (*domain.SnipeState, error)
	UpdateSnipe(ctx context.Context, s *domain.SnipeState) error
	ListSnipesByStatus(ctx context.Context, status domain.Status) ([]*domain.SnipeState, error)

	// Events -----------------------------------------------------
	AppendEvent(ctx context.Context, snipeID domain.SnipeID, e domain.Event) error
	ListEventsBySnipe(ctx context.Context, snipeID domain.SnipeID) ([]domain.Event, error)

	// TransitionSnipe atomically writes the supplied SnipeState (which
	// must already carry the post-transition status and a fresh
	// UpdatedAt) and appends ev to the audit log in a single
	// transaction. Returns ErrNotFound if the snipe row does not
	// exist; on any failure neither write lands.
	TransitionSnipe(ctx context.Context, s *domain.SnipeState, ev domain.Event) error

	// Sessions ---------------------------------------------------
	UpsertSession(ctx context.Context, s SessionRow) error
	// GetSession returns ErrSessionExpired when the row exists but
	// its ExpiresAt has passed at the supplied 'now'. It returns
	// ErrNotFound when there is no row.
	GetSession(ctx context.Context, user domain.UserID, provider domain.ProviderID, now time.Time) (SessionRow, error)
	DeleteSession(ctx context.Context, user domain.UserID, provider domain.ProviderID) error

	// Venues -----------------------------------------------------
	UpsertVenue(ctx context.Context, v domain.Venue) error
	GetVenue(ctx context.Context, ref domain.VenueRef) (domain.Venue, error)

	// Observed release times ------------------------------------
	UpsertObserved(ctx context.Context, o ObservedRelease) error
	GetObserved(ctx context.Context, ref domain.VenueRef) (ObservedRelease, error)
}
