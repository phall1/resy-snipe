# OpenTable paper sketch: mapping onto the `Provider` interface

**Status:** design exercise (D6). Goal: stress-test
`internal/providers/provider.go` against a second concrete provider before
the interface ossifies. Output: a list of fits, near-fits, and the one or
two places the interface likely needs to give.

I'm working from training-set knowledge of OpenTable's consumer/mobile API
surface (the only realistic target — the partner Connect API is gated and
not what a sniper would drive). Where I'm not 100% on an endpoint name I
say so; the mapping conclusions don't depend on the exact path.

---

## OpenTable API in one paragraph

OpenTable's consumer flow is a Bearer-token login (email/password against a
mobile-API host, with a strong PerimeterX anti-bot layer) followed by a
search → availability → **lock** → confirm flow. Search and availability
are point-in-time: you ask "what's open for party N on date D" and get a
slot list. There is no public "month calendar with available/sold-out per
day" endpoint analogous to Resy's `/4/find?day=` aggregated view; the
mobile app synthesises calendars by fanning per-date queries (or relying
on a coarse "next available" hint). Booking is explicitly multi-step: a
`lock` call reserves the slot for ~60–90 s and returns a lock token; a
`confirm` call commits using that token. The lock token is the de-facto
idempotency mechanism — there is no documented `Idempotency-Key` header.

---

## Method-by-method mapping

### `ID() ProviderID`

**Fits as-is.** Constant `"opentable"`. No tension.

### `Login(ctx, Credentials) (Session, error)`

**Fits with adapter glue.** OpenTable's `Credentials` would be email +
password (same shape as Resy's), so `Credentials.Provider()` is the only
contract and it's satisfied. The adapter resolves the Bearer token and
wraps it in a `Session`. Two pieces of friction worth noting, neither of
which require interface changes:

1. The login call frequently trips PerimeterX even on first contact. The
   adapter must classify the `px-captcha` 403 body as
   `ErrAntiBotChallenge` (not `ErrAuthExpired`). The taxonomy already has
   a slot for this.
2. OpenTable consumer login can demand an email-verification round-trip
   on a new device fingerprint. That's MFA-shaped, and `ErrMFARequired`
   already covers it. The provider would need a per-provider
   `CompleteEmailChallenge` path, exactly like the comment in
   `provider.go` anticipates.

### `Ping(ctx, Session) error`

**Fits as-is.** OpenTable has a `/api/me` (or equivalent diner-profile
endpoint) that returns 200 on a live token and 401 on an expired one. The
adapter returns `ErrAuthExpired` on 401. Done.

### `SearchVenues(ctx, Query) ([]Venue, error)`

**Fits with adapter glue.** OpenTable's restaurant search takes a free-text
term plus optional location bias. `Query` currently only carries `Text`,
which is fine for Phase 1; the comment on `Query` already flags location
bias as future. One adapter concern: OpenTable returns timezone as an
IANA string, which feeds cleanly into `Venue.TZ`. No interface change.

### `Calendar(ctx, VenueRef, DateRange) (Calendar, error)`

**Doesn't fit cleanly — but the interface is still the right shape.**

This is the hypothesis the task asked me to test hardest, so I want to
spell out the friction:

- Resy answers a calendar question in **one** call: per-day flags
  (`available` / `sold-out` / `closed`) over a window.
- OpenTable has no equivalent. The adapter has to fan out **N point-in-
  time availability queries** (one per date in the range, possibly per
  party-size) and synthesise the per-day boolean from "did any slot come
  back?".

Three observations follow:

1. **Synthesis is fine for the engine's purposes.** The engine consumes
   `[]CalendarDay{Date, Available}`. Whether the adapter produced that
   from one call or thirty, the engine doesn't care. The
   DiscoveredRelease loop only needs the far edge of the slice.
2. **Cost and rate-limit pressure ride on the adapter.** A 30-day fan-out
   per Calendar call is 30× the request volume of Resy. The adapter must
   parallelise carefully (PerimeterX punishes burstiness) and may want
   to maintain its own short-TTL cache. None of that leaks into the
   interface.
3. **`PartySize` is missing from `DateRange`.** OpenTable's per-day
   availability is party-size-dependent; a table for 2 may be available
   on a date when a table for 6 is not. Resy's calendar is also
   party-aware in practice but the interface doesn't expose it. This is
   the one calendar-shaped change worth considering. Verdict: see
   "Recommended interface adjustments" below.

So `Calendar` survives. It survives because it's a view, not a transport
shape, and synthesis-via-fan-out is a legitimate adapter strategy.

### `Find(ctx, FindRequest) ([]Slot, error)`

**Fits as-is.** OpenTable's `restref/availability?restaurantIds=&dateTime=
&partySize=` returns a list of timeslots. The adapter maps each into a
`Slot` with its `slotHash` going into a new payload variant
(`OpenTableSlotPayload{SlotHash, AvailabilityToken, AttributeTags}`).
`FindRequest.Times` (preferred wall-clock list) is consumed adapter-side
to filter the response.

### `Book(ctx, Slot, Session) (Confirmation, error)`

**This is the load-bearing question.**

Resy's flow inside `Book` is two-call (`/3/details` → `/3/book`); the
`book_token` is internal to the adapter. OpenTable's flow is also
multi-step (`lock` → `confirm`), and likewise the lock token can stay
adapter-internal. So at the interface level: **fits as-is** for both.

But: the `lock` step is **observably stateful** in a way `/3/details` is
not. A successful lock decrements OpenTable's inventory for the lock
window (~60–90 s). If the engine fires three concurrent `Book`s as part
of its race-and-cancel (per `BookingPolicy.MaxConcurrent`), the adapter
will issue three locks against the same restaurant, two of which are
wasted. That's potentially anti-bot-visible behaviour and certainly
inventory-rude.

Two ways out, and I want to flag them rather than commit:

- **Option A (no interface change):** the adapter serialises lock/confirm
  for OpenTable internally and exposes a single-step `Book`. The engine
  thinks it's racing, but for OpenTable the race is effectively a
  serial loop. We lose the speed advantage of racing on OpenTable, but
  we also avoid burning inventory and tripping bot detection.
- **Option B (interface widens):** add `Hold(ctx, Slot, Session)
  (HoldToken, error)` and split `Book` to take a `HoldToken`. The
  engine can now schedule holds and confirms separately, and Resy's
  adapter exposes `Hold` as a thin wrapper over `/3/details`. This is
  the more honest model but it's a real interface change and the engine
  state machine grows a state.

My recommendation is **Option A for now, document Option B as a known
future split if a third provider also has visible-hold semantics**. The
single point I'd change today is making the adapter's serialisation
choice visible — see below.

### Sentinel error taxonomy

Coverage check, going through each:

- `ErrAuthExpired` — yes, OpenTable 401s on expired Bearer tokens.
- `ErrMFARequired` — yes, email-verification challenge maps here.
- `ErrBookTokenExpired` — yes, OpenTable's lock-token expiry maps
  cleanly. The name is slightly Resy-flavoured ("book token") but the
  semantic — short-TTL holding token expired, walk find→book again — is
  exact.
- `ErrSlotTaken` — yes, OpenTable returns a 409-shaped response on
  confirm-after-stolen-inventory.
- `ErrRateLimited` — yes, 429 with `Retry-After`.
- `ErrAntiBotChallenge` — yes, PerimeterX 403 with `px-captcha` body.
- `ErrInventoryEmpty` — yes, an empty timeslot list maps here.

**Taxonomy fits as-is.** I want to call out that `ErrBookTokenExpired`'s
name is provider-flavoured. It's not wrong — the term "book token" is
the engine's name for "short-TTL handle returned by step N-1 of the
booking flow" — but it reads as Resy-specific. Worth a rename to
something like `ErrHoldExpired` if Option B above ever lands, but not
today.

---

## Hypothesis verdicts (the four explicit checks from the task)

1. **Calendar shape:** survives, with caveats. The interface doesn't
   need to change in shape, but it's missing `PartySize` — see below.
2. **Find/Book separation:** survives. Multi-step booking can stay
   inside the adapter. The race-and-cancel design is **not** universally
   valuable; it's specifically a Resy advantage. For OpenTable, racing
   is actively harmful (visible holds), so the adapter serialises. The
   engine doesn't need to know.
3. **Idempotency:** OpenTable does not accept idempotency keys.
   `IdempotencyKey` is currently consumed by Resy's `/3/book` header;
   for OpenTable, the adapter ignores the key and relies on the lock
   token's natural dedupe. That's already how the interface works
   (`IdempotencyKey` lives in `domain`, not in the `Provider`
   contract), so no change.
4. **Sessions:** OpenTable's login response includes an `expires_in`
   (or equivalent), so `Session.ExpiresAt()` is parseable. No change.

---

## Recommended interface adjustments

I want to make exactly one change today and flag two more as proposed
(not implemented).

### Change today

**Add `PartySize int` to `DateRange`.** OpenTable per-day availability is
party-size-dependent. Resy's adapter currently ignores party at the
calendar layer because Resy's calendar response happens to be coarse,
but party will matter the first time we add a second provider. Adding
the field now is cheap; adding it later means revisiting every adapter
and the engine's calendar consumer.

```go
type DateRange struct {
    Start, End domain.Date
    PartySize  int // 0 = adapter chooses a default
}
```

The Resy adapter ignores the field for now (its `/4/find?day=` endpoint
is party-coarse), so behaviour is unchanged.

### Proposed (not implemented)

- **Split `Book` into `Hold` + `Book` if a second hold-visible provider
  appears.** Single-provider speculation is the wrong time to widen the
  interface; OpenTable's adapter can paper over by serialising
  internally. If we onboard a third provider with the same shape, do
  this then.
- **Rename `ErrBookTokenExpired` to `ErrHoldExpired`.** Lower priority,
  pure naming. Wait until at least one non-Resy provider lands; renaming
  one sentinel across one adapter is a 5-minute change and the cost of
  doing it twice is zero. No value to doing it today.

---

## What did NOT change

- `Provider` method set — same seven methods.
- Sentinel error set — same seven sentinels.
- `Credentials`, `Session`, `Query`, `Calendar`, `CalendarDay`,
  `FindRequest`, `Slot`, `Confirmation` — unchanged in shape.

The interface is in surprisingly good shape against a second provider.
The one real find — `DateRange` is missing `PartySize` — is a small
field addition with no code-path impact for the existing Resy adapter.
