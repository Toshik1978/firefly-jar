package civil

// Range is the inclusive check window [From, To], derived once per run from today's date and
// a window length in days.
type Range struct {
	From Date
	To   Date
}

// NewRange returns the window of the given length ending on today, inclusive of both ends: From
// is today minus (days-1) and To is today (FR-004).
func NewRange(today Date, days int) Range {
	return Range{From: today.AddDays(-(days - 1)), To: today}
}

// Contains reports whether d falls within the window, inclusive of both From and To.
func (r Range) Contains(d Date) bool {
	return !d.Before(r.From) && !r.To.Before(d)
}

// IsFirstDay reports whether d is the first day of the window (FR-026).
func (r Range) IsFirstDay(d Date) bool {
	return d == r.From
}
