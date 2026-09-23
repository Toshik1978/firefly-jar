// Copyright 2016 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// This file was copied from cloud.google.com/go/civil (v0.123.0, package civil, file civil.go) and
// modified:
//   - removed, because nothing in firefly-jar needs them: the Time and DateTime types, Date's
//     database/sql Value and Scan methods (and the database/sql/driver import), and Date's
//     AddMonths, AddYears and Weekday methods;
//   - the package doc comment was rewritten for this trimmed package and gained a paragraph pointing
//     to Range (range.go), this repository's own addition;
//   - the "2006-01-02" layout and the 86400 seconds-per-day divisor became the named constants
//     dateLayout and secondsPerDay;
//   - ParseDate wraps the time.Parse error with the input, and UnmarshalText wraps ParseDate's
//     error, to satisfy this repository's wrapcheck lint rule;
//   - UnmarshalText leaves *d unchanged when parsing fails (upstream sets it to the zero Date);
//   - IsZero dropped the redundant parentheses around its three comparisons;
//   - Compare was rewritten as a switch returning 1 instead of an if/else chain returning +1,
//     DaysSince lost its named result, and the d2 parameters of Before, After and Compare were
//     renamed other;
//   - doc comments and layout were adjusted to this repository's gofumpt/gci/golines configuration.

// Package civil implements a time-zone-independent civil date: a year, month and day following the
// proleptic Gregorian calendar, with no time-of-day or location attached. Compute "today" and any
// window once per run, in the configured time zone, and pass Date values from there on so day
// arithmetic never depends on a location again.
//
// Range, this repository's own addition in range.go, is the inclusive check window [From, To] of
// such dates, built once per run from today and the configured window length.
package civil

import (
	"fmt"
	"time"
)

// dateLayout is the strict YYYY-MM-DD layout ParseDate accepts and String renders, matching the
// Firefly III split date prefix exactly (research R8).
const dateLayout = "2006-01-02"

// secondsPerDay converts a Unix-time delta in seconds to whole days for DaysSince.
const secondsPerDay = 86400

// A Date represents a date (year, month, day).
//
// This type does not include location information, and therefore does not describe a unique
// 24-hour timespan.
type Date struct {
	Year  int        // Year (e.g., 2014).
	Month time.Month // Month of the year (January = 1, ...).
	Day   int        // Day of the month, starting at 1.
}

// DateOf returns the Date in which t occurs in t's location. Call it as DateOf(t.In(loc)) to get
// the calendar date in a specific time zone.
func DateOf(t time.Time) Date {
	var d Date

	d.Year, d.Month, d.Day = t.Date()

	return d
}

// ParseDate parses a string in RFC3339 full-date format and returns the date value it represents.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return Date{}, fmt.Errorf("parse date %q: %w", s, err)
	}

	return DateOf(t), nil
}

// String returns the date in RFC3339 full-date format.
func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day)
}

// IsValid reports whether the date is valid.
func (d Date) IsValid() bool {
	return DateOf(d.In(time.UTC)) == d
}

// In returns the time corresponding to time 00:00:00 of the date in loc.
//
// In is always consistent with time.Date, even when time.Date returns a time on a different day.
// For example, if loc is America/Indiana/Vincennes, then both
//
//	time.Date(1955, time.May, 1, 0, 0, 0, 0, loc)
//
// and
//
//	civil.Date{Year: 1955, Month: time.May, Day: 1}.In(loc)
//
// return 23:00:00 on April 30, 1955.
//
// In panics if loc is nil.
func (d Date) In(loc *time.Location) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
}

// AddDays returns the date that is n days in the future. n can also be negative to go into the
// past.
func (d Date) AddDays(n int) Date {
	return DateOf(d.In(time.UTC).AddDate(0, 0, n))
}

// DaysSince returns the signed number of days between the date and s, not including the end day.
// This is the inverse operation to AddDays.
func (d Date) DaysSince(s Date) int {
	// Convert to Unix time so we do not have to worry about leap seconds: Unix time increases by
	// exactly 86400 seconds per day.
	deltaUnix := d.In(time.UTC).Unix() - s.In(time.UTC).Unix()

	return int(deltaUnix / secondsPerDay)
}

// Before reports whether d occurs before other.
func (d Date) Before(other Date) bool {
	if d.Year != other.Year {
		return d.Year < other.Year
	}

	if d.Month != other.Month {
		return d.Month < other.Month
	}

	return d.Day < other.Day
}

// After reports whether d occurs after other.
func (d Date) After(other Date) bool {
	return other.Before(d)
}

// Compare compares d and other. If d is before other, it returns -1; if d is after other, it
// returns +1; otherwise it returns 0.
func (d Date) Compare(other Date) int {
	switch {
	case d.Before(other):
		return -1
	case d.After(other):
		return 1
	default:
		return 0
	}
}

// IsZero reports whether the date fields are set to their default value.
func (d Date) IsZero() bool {
	return d.Year == 0 && int(d.Month) == 0 && d.Day == 0
}

// MarshalText implements the encoding.TextMarshaler interface. The output is the result of
// d.String().
func (d Date) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// UnmarshalText implements the encoding.TextUnmarshaler interface. The date is expected to be a
// string in a format accepted by ParseDate.
func (d *Date) UnmarshalText(data []byte) error {
	parsed, err := ParseDate(string(data))
	if err != nil {
		return fmt.Errorf("unmarshal date: %w", err)
	}

	*d = parsed

	return nil
}
