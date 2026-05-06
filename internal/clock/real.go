package clock

import "time"

// Real is the production Clock backed by the standard library.
type Real struct{}

// NewReal returns a Real clock. The zero value is also valid.
func NewReal() Real { return Real{} }

func (Real) Now() time.Time                         { return time.Now() }
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (Real) Sleep(d time.Duration)                  { time.Sleep(d) }

func (Real) AfterFunc(d time.Duration, f func()) Timer {
	return realTimer{t: time.AfterFunc(d, f)}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) Stop() bool { return r.t.Stop() }
