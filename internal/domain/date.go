package domain

import (
	"fmt"
	"time"
)

// Date is a calendar date in some venue's local timezone, with no
// time-of-day component. Keeping it distinct from time.Time prevents
// accidental arithmetic on a naked timestamp without a zone.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// NewDate constructs a Date.
func NewDate(year int, month time.Month, day int) Date {
	return Date{Year: year, Month: month, Day: day}
}

// ParseDate parses a YYYY-MM-DD string.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return Date{}, fmt.Errorf("date %q: %w", s, err)
	}
	return Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
}

// String returns the YYYY-MM-DD form.
func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day)
}

// IsZero reports whether d is the zero value.
func (d Date) IsZero() bool { return d == Date{} }

// In returns the moment at start-of-day in the supplied location.
func (d Date) In(loc *time.Location) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
}

// WallTime is a time-of-day with no date or zone. Used inside
// SlotPreference. The intended interpretation is venue-local.
type WallTime struct {
	Hour, Minute, Second int
}

// NewWallTime constructs a WallTime.
func NewWallTime(h, m, s int) WallTime { return WallTime{Hour: h, Minute: m, Second: s} }

// ParseWallTime accepts HH:MM or HH:MM:SS.
func ParseWallTime(s string) (WallTime, error) {
	if t, err := time.Parse("15:04:05", s); err == nil {
		return WallTime{Hour: t.Hour(), Minute: t.Minute(), Second: t.Second()}, nil
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return WallTime{}, fmt.Errorf("walltime %q: %w", s, err)
	}
	return WallTime{Hour: t.Hour(), Minute: t.Minute(), Second: t.Second()}, nil
}

// String returns HH:MM:SS.
func (w WallTime) String() string {
	return fmt.Sprintf("%02d:%02d:%02d", w.Hour, w.Minute, w.Second)
}
