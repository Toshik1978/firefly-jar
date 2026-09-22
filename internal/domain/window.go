package domain

import "github.com/Toshik1978/firefly-jar/internal/civil"

// Window is the inclusive reconcile window [From, To], derived once per run from today's date and
// a window length in days.
type Window struct {
	From civil.Date
	To   civil.Date
}

// NewWindow returns the window of the given length ending on today, inclusive of both ends: From
// is today minus (days-1) and To is today (FR-004).
func NewWindow(today civil.Date, days int) Window {
	return Window{From: today.AddDays(-(days - 1)), To: today}
}

// Contains reports whether d falls within the window, inclusive of both From and To.
func (w Window) Contains(d civil.Date) bool {
	return !d.Before(w.From) && !w.To.Before(d)
}

// IsFirstDay reports whether d is the first day of the window (FR-026).
func (w Window) IsFirstDay(d civil.Date) bool {
	return d == w.From
}
