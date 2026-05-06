// Package clock is the single home of wall-clock access. Production code
// outside this package must never call time.Now() directly; instead it
// receives a Clock via constructor injection. The package provides the
// Clock interface, a Real implementation that delegates to time, and a
// Fake implementation that drives time-dependent tests deterministically.
package clock
