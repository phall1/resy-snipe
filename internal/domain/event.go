package domain

// EventType enumerates the records that land on a SnipeState's
// append-only audit log. The string values are persisted; do not
// rename them once they are in the wild.
type EventType string

const (
	EventSubmitted     EventType = "submitted"
	EventScheduled     EventType = "scheduled"
	EventDiscovered    EventType = "discovered"
	EventReleased      EventType = "released"
	EventFound         EventType = "found"
	EventBookAttempted EventType = "book_attempted"
	EventBooked        EventType = "booked"
	EventFailed        EventType = "failed"
	EventCanceled      EventType = "canceled"
	EventExpired       EventType = "expired"
)

// AllEventTypes returns every defined event type.
func AllEventTypes() []EventType {
	return []EventType{
		EventSubmitted,
		EventScheduled,
		EventDiscovered,
		EventReleased,
		EventFound,
		EventBookAttempted,
		EventBooked,
		EventFailed,
		EventCanceled,
		EventExpired,
	}
}
