// Package store provides the SQLite-backed persistence layer (modernc.org/
// sqlite — no CGO), schema migrations, and the Store interface used by the
// engine. JSON is allowed only at this persistence boundary; in-memory
// types stay typed.
package store
