package service

// standard.go: the Standard composition for service.Service.
//
// Standard is the real Service implementation that composes Resolver,
// Planner, Engine, and Store into the in-process API both transports
// consume (see docs/v2/design/service-layer.md §implementation-sketch
// and ADR-0003). Stub remains in the package as the
// compile-against-while-wiring placeholder for callers that do not
// yet want a full backend.
//
// The deferred verbs (SubscribeQuest, InviteUser, AcceptInvite,
// RotateToken, RevokeToken, ListUsers) still return
// ErrNotImplemented; they are wired in M1-11 / M1-13 / later operator
// admin issues. Every such method carries a TODO comment naming the
// issue that owns it.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/engine"
	"resy-snipe/internal/providers"
	"resy-snipe/internal/resolver"
)

// QuestRow is the on-disk projection the Service hands to its
// StoreBackend. Defined inside the service package per Law 5 — the
// Service owns the contract; an adapter in cmd/ binds it to the
// store package's package-level CRUD functions. Mirrors
// store.QuestRow at the field level; the duplicated definition keeps
// internal/service from importing internal/store.
type QuestRow struct {
	ID          domain.QuestID
	UserID      domain.UserID
	AccountID   domain.AccountID
	GoalJSON    string
	PlanHash    string
	Status      domain.Status
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// AccountRow is the on-disk projection of one accounts row,
// consumer-side per Law 5. Mirrors store.AccountRow.
type AccountRow struct {
	ID          domain.AccountID
	UserID      domain.UserID
	ResyEmail   string
	DisplayName string
	CreatedAt   time.Time
	DisabledAt  *time.Time
}

// EventRow is the consumer-side projection of one quest_events row.
// GetQuest assembles a QuestState's Events from these. M1-10 returns
// an empty list (the engine writes to the v1 events table by
// SnipeID; M1-11 wires the v2 quest_events stream).
type EventRow struct {
	Type  domain.EventType
	At    time.Time
	Attrs []slog.Attr
}

// IdempotencyLookup is the consumer-side projection of an
// idempotency_keys row, returned by StoreBackend.GetIdempotencyResult.
// Found is false when no row matches (userID, scope, plaintextKey);
// PayloadMatch is false when a row exists for the key but its
// payloadHash differs (the Service surfaces ErrIdempotencyConflict
// in that case); Expired is true when the matching row has already
// passed expires_at (callers treat expired==true as not-found).
// TargetID and PrevErr replay the persisted outcome.
type IdempotencyLookup struct {
	Found        bool
	PayloadMatch bool
	Expired      bool
	TargetID     string
	PrevErr      string
}

// StoreBackend is the slim persistence seam the Service depends on,
// defined at the consumer per docs/laws.md Law 5. The internal/store
// package owns the implementation (a small set of package-level
// functions backed by SQLite); the cmd/ wiring layer adapts them to
// this interface. internal/service therefore does not import
// internal/store directly — the dependency arrow points the other way
// at composition time.
//
// Every method takes userID as the second parameter (after ctx) so
// tenancy is enforced at the SQL layer, not at the Service. Filters
// that traverse user-owned rows always include user_id in their WHERE
// clause; cross-tenant queries (operator-only) live elsewhere
// (store.ListUsers, store.SeedOperator).
type StoreBackend interface {
	// CreateQuest persists a freshly-minted quest row. Implementations
	// trust the caller-supplied userID and account_id; the Service is
	// the tenancy boundary.
	CreateQuest(ctx context.Context, q QuestRow) error
	// GetQuest returns the (userID, questID) row, or ErrNotFound if no
	// such row exists or it belongs to a different user. The two
	// states fold into one signal per docs/v2/design/service-layer.md
	// §identity-scoping.
	GetQuest(ctx context.Context, userID domain.UserID, questID domain.QuestID) (QuestRow, error)
	// ListQuests returns userID's rows narrowed by filter, ordered
	// most-recent-first.
	ListQuests(ctx context.Context, userID domain.UserID, filter ListFilter) ([]QuestRow, error)
	// UpdateQuestStatus flips a quest's status (and optionally
	// completed_at) in place. updatedAt is supplied by the caller per
	// Law 7. Wrong-tenant calls produce ErrNotFound.
	UpdateQuestStatus(
		ctx context.Context,
		userID domain.UserID,
		questID domain.QuestID,
		newStatus domain.Status,
		completedAt *time.Time,
		updatedAt time.Time,
	) error

	// ListAccounts returns userID's accounts rows.
	ListAccounts(ctx context.Context, userID domain.UserID) ([]AccountRow, error)
	// GetAccountByEmail returns the (userID, email) accounts row.
	// Wrong-tenant calls produce ErrNotFound.
	GetAccountByEmail(ctx context.Context, userID domain.UserID, email string) (AccountRow, error)
	// BindAccountToUser claims a (currently NULL-user_id) accounts
	// row for userID and returns its accountID. Used by Login to
	// translate the resy adapter's "session under resy_email" output
	// into a v2-tenancy "account under userID" row.
	BindAccountToUser(ctx context.Context, userID domain.UserID, email string) (domain.AccountID, error)

	// ListEvents returns up to 'limit' most-recent quest_events rows
	// for (userID, questID). M1-10 implementations may return an
	// empty slice — quest_events writes land in M1-11.
	ListEvents(ctx context.Context, userID domain.UserID, questID domain.QuestID, limit int) ([]EventRow, error)

	// ListQuestEvents returns the chronological replay of
	// (userID, questID)'s persisted events. It is the history seam
	// SubscribeQuest pumps through its callback before bridging the
	// live engine stream (see docs/v2/design/service-layer.md
	// §streaming). Ordering is oldest-first so the subscriber observes
	// the same sequence a long-lived connection would have witnessed.
	//
	// limit <= 0 means "no limit". M1-13 callers always pass 0 and
	// rely on quest_events being bounded by quest lifetime; future
	// transports that page through history may supply a cap.
	//
	// Until M1-11 wires the quest_events writers this returns an
	// empty slice — that is intentional, not an error condition.
	ListQuestEvents(ctx context.Context, userID domain.UserID, questID domain.QuestID, limit int) ([]domain.Event, error)

	// GetIdempotencyResult looks up a prior CreateQuest/CancelQuest
	// outcome filed under (userID, scope, plaintextKey, payloadHash).
	// See IdempotencyLookup for the field semantics; in summary:
	//   * Found=false means no row exists — proceed fresh.
	//   * Found=true, PayloadMatch=true → return the persisted result.
	//   * Found=true, PayloadMatch=false → surface ErrIdempotencyConflict.
	//   * Expired=true → treat as not-found (proceed fresh).
	GetIdempotencyResult(
		ctx context.Context,
		userID domain.UserID,
		scope, plaintextKey, payloadHash string,
		now time.Time,
	) (IdempotencyLookup, error)

	// PutIdempotencyResult records the outcome of a CreateQuest /
	// CancelQuest call. errVal nil persists a success row (result_code
	// = 'ok'); non-nil persists the error string for replay. The TTL
	// + now pair stamps expires_at without the store reading the wall
	// clock on its own (Law 7).
	PutIdempotencyResult(
		ctx context.Context,
		userID domain.UserID,
		scope, plaintextKey, payloadHash string,
		targetID string,
		errVal error,
		ttl time.Duration,
		now time.Time,
	) error

	// WriteAuditEvent persists one audit_events row for the supplied
	// AuditWrite. Every Service-layer call writes exactly one row
	// before returning (docs/v2/design/service-layer.md
	// §audit-log-contract); failed auth still logs. Implementations
	// must NOT fail the parent call on audit-write errors — the
	// Service's audit helper translates errors to a WARN log and
	// proceeds.
	WriteAuditEvent(ctx context.Context, evt AuditWrite) error

	// WriteQuestEvent persists one quest_events row for (userID,
	// questID). Engine state transitions surface as one Event each
	// (internal/engine emits one Notification per transition); the
	// Service bridges those into this seam so subsequent GetQuest /
	// SubscribeQuest calls can replay history.
	WriteQuestEvent(ctx context.Context, userID domain.UserID, questID domain.QuestID, evt domain.Event) error
}

// AuditWrite is the consumer-side projection of one audit_events row,
// shaped for the Service's audit helper. IP and UserAgent are
// transport concerns (HTTP-side metadata) and stay empty for the M1
// wave — M2 transports populate them via the bearer-token middleware.
// DetailsJSON is bounded to 4KiB at the design-doc level
// (multi-user.md §audit_events); the Service's audit helper enforces
// the bound at write time.
type AuditWrite struct {
	UserID       domain.UserID
	TargetUserID *domain.UserID
	Action       string
	TargetID     string
	OK           bool
	ErrorCode    string
	IP           string
	UserAgent    string
	DetailsJSON  []byte
}

// ResyAuthBackend is the slim auth seam the Service depends on for
// Login. Defined at the consumer per Law 5; cmd/ adapts the real
// *resy.Client to it. The Service does not import internal/resy
// directly so the auth provider is swappable in tests.
type ResyAuthBackend interface {
	// Login performs the credentialed sign-in against the upstream
	// provider and side-effects the persistent session store. On
	// success the returned (email) is the account identity the
	// session was filed under — typically the supplied email
	// verbatim, but adapter-level normalization (lowercasing) may
	// transform it.
	Login(ctx context.Context, email, password string) (string, error)
}

// Standard is the production Service. It composes Resolver, Planner,
// Engine, Store, and the Resy auth backend; it owns no state beyond
// the dependency wiring. All tenancy enforcement happens at the
// StoreBackend layer (user_id in every WHERE/SET); Standard itself
// folds wrong-tenant existence into ErrNotFound at the API boundary.
type Standard struct {
	resolver resolver.Resolver
	provider providers.Provider // for PlanQuest's calendar snapshot
	engine   *engine.Engine
	store    StoreBackend
	auth     ResyAuthBackend
	clock    clock.Clock
	logger   *slog.Logger
	// persistEvents controls whether SubscribeQuest writes engine
	// transitions to the quest_events stream. Defaults to true (M1-11
	// §engine-event → quest_event bridge). Tests that exercise the
	// streaming path without a persistence side-effect can flip it off
	// via WithPersistEvents(false).
	persistEvents bool
}

// Compile-time interface check (Law 6).
var _ Service = (*Standard)(nil)

// Option configures Standard at construction.
type Option func(*Standard)

// WithLogger pins the slog.Logger the Service uses for its own log
// lines. A nil logger falls back to slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(s *Standard) {
		if log != nil {
			s.logger = log
		}
	}
}

// WithAuth pins the Resy auth backend. A nil auth backend makes
// Login return ErrNotImplemented — useful in tests that exercise the
// non-Login surface.
func WithAuth(a ResyAuthBackend) Option {
	return func(s *Standard) {
		s.auth = a
	}
}

// WithPersistEvents pins whether SubscribeQuest writes engine
// transitions to the quest_events stream. Defaults to true; tests that
// drive the live path without wanting a persistence side-effect (e.g.
// the M1-13 subscribe tests that pre-seed history via raw SQL) can
// disable it.
func WithPersistEvents(enabled bool) Option {
	return func(s *Standard) {
		s.persistEvents = enabled
	}
}

// WithProvider pins the providers.Provider the Service hands to the
// planner for calendar snapshots. The same provider typically backs
// the engine and the resolver. A nil provider leaves PlanQuest
// running with an empty CalendarSnapshot — the planner falls through
// to Explicit/Discovered strategies.
func WithProvider(p providers.Provider) Option {
	return func(s *Standard) {
		s.provider = p
	}
}

// New constructs a Standard service. resolver, engine, store, and
// clock are required; nil arguments panic at startup rather than
// surface a confusing nil-deref at first call.
//
// The Resy auth backend is optional at construction; Login returns
// ErrNotImplemented if none was wired. Production wiring always
// supplies one via WithAuth.
func New(
	r resolver.Resolver,
	eng *engine.Engine,
	st StoreBackend,
	clk clock.Clock,
	opts ...Option,
) (*Standard, error) {
	if r == nil {
		return nil, errors.New("service.New: resolver is required")
	}
	if eng == nil {
		return nil, errors.New("service.New: engine is required")
	}
	if st == nil {
		return nil, errors.New("service.New: store is required")
	}
	if clk == nil {
		return nil, errors.New("service.New: clock is required")
	}
	s := &Standard{
		resolver:      r,
		engine:        eng,
		store:         st,
		clock:         clk,
		logger:        slog.Default(),
		persistEvents: true,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// newQuestID mints a fresh QuestID of shape "q_<8 hex>". The 8-hex
// width is borrowed from the operator's deriveOperatorID id-shape and
// from the migration 0002 conventions — short enough for CLI output,
// wide enough to avoid collisions at homelab scale.
//
// crypto/rand is the entropy source. A failed read returns a non-nil
// error rather than panicking; callers (CreateQuest) translate that
// to ErrInternal via wrapping.
func newQuestID() (domain.QuestID, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("service.newQuestID: %w", err)
	}
	return domain.QuestID("q_" + hex.EncodeToString(buf[:])), nil
}
