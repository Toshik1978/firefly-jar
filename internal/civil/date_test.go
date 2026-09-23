package civil_test

import (
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/civil"
)

// DateSuite covers civil.Date: strict parsing, the calendar date of an instant in a given time
// zone, day arithmetic, ordering, rendering and (un)marshaling.
type DateSuite struct {
	suite.Suite
}

func (s *DateSuite) TestParseDateValid() {
	got, err := civil.ParseDate("2026-09-22")
	s.Require().NoError(err)

	s.Equal(civil.Date{Year: 2026, Month: time.September, Day: 22}, got)
}

func (s *DateSuite) TestParseDateRejectsInvalid() {
	cases := map[string]string{
		"empty string":       "",
		"day out of range":   "2026-02-30",
		"single-digit month": "2026-9-1",
		"single-digit day":   "2026-09-1",
		"wrong separator":    "2026/09/22",
		"not a date":         "abc",
		"trailing garbage":   "2026-09-22x",
		"month out of range": "2026-13-01",
	}

	for name, in := range cases {
		s.Run(name, func() {
			_, err := civil.ParseDate(in)
			s.Require().Error(err)
		})
	}
}

func (s *DateSuite) TestDateOfUsesCalendarDateInLocation() {
	// 2026-09-21T23:30:00Z is still 2026-09-21 in UTC, but Europe/Vilnius is UTC+3 in
	// September, so the same instant is already 2026-09-22 there. DateOf must use the
	// calendar date of the time it is given, so callers convert to the target location with
	// t.In(loc) before calling it.
	instant := time.Date(2026, time.September, 21, 23, 30, 0, 0, time.UTC)

	vilnius, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	s.Equal(civil.Date{Year: 2026, Month: time.September, Day: 21}, civil.DateOf(instant.In(time.UTC)))
	s.Equal(civil.Date{Year: 2026, Month: time.September, Day: 22}, civil.DateOf(instant.In(vilnius)))
}

func (s *DateSuite) TestAddDays() {
	cases := []struct {
		name string
		in   civil.Date
		days int
		want civil.Date
	}{
		{
			name: "across month boundary",
			in:   civil.Date{Year: 2026, Month: time.January, Day: 31},
			days: 1,
			want: civil.Date{Year: 2026, Month: time.February, Day: 1},
		},
		{
			name: "across year boundary",
			in:   civil.Date{Year: 2026, Month: time.December, Day: 31},
			days: 1,
			want: civil.Date{Year: 2027, Month: time.January, Day: 1},
		},
		{
			name: "onto a leap day",
			in:   civil.Date{Year: 2024, Month: time.February, Day: 28},
			days: 1,
			want: civil.Date{Year: 2024, Month: time.February, Day: 29},
		},
		{
			name: "past a leap day in a non-leap year",
			in:   civil.Date{Year: 2026, Month: time.February, Day: 28},
			days: 1,
			want: civil.Date{Year: 2026, Month: time.March, Day: 1},
		},
		{
			name: "negative n moves backward across a month boundary",
			in:   civil.Date{Year: 2026, Month: time.March, Day: 1},
			days: -1,
			want: civil.Date{Year: 2026, Month: time.February, Day: 28},
		},
		{
			name: "zero n is a no-op",
			in:   civil.Date{Year: 2026, Month: time.September, Day: 22},
			days: 0,
			want: civil.Date{Year: 2026, Month: time.September, Day: 22},
		},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			s.Equal(tc.want, tc.in.AddDays(tc.days))
		})
	}
}

func (s *DateSuite) TestDaysSince() {
	from := civil.Date{Year: 2026, Month: time.September, Day: 20}
	to := civil.Date{Year: 2026, Month: time.September, Day: 22}

	// The former hand-rolled Date.DaysBetween(other) meant other minus d; that is upstream's
	// other.DaysSince(d).
	s.Equal(2, to.DaysSince(from))
	s.Equal(-2, from.DaysSince(to))
	s.Equal(0, from.DaysSince(from))
}

func (s *DateSuite) TestCompareBeforeAfter() {
	earlier := civil.Date{Year: 2026, Month: time.September, Day: 20}
	later := civil.Date{Year: 2026, Month: time.September, Day: 22}
	same := civil.Date{Year: 2026, Month: time.September, Day: 20}

	s.Equal(-1, earlier.Compare(later))
	s.Equal(1, later.Compare(earlier))
	s.Equal(0, earlier.Compare(same))

	s.True(earlier.Before(later))
	s.False(later.Before(earlier))
	s.False(earlier.Before(same))

	s.True(later.After(earlier))
	s.False(earlier.After(later))
	s.False(earlier.After(same))
}

func (s *DateSuite) TestString() {
	s.Equal("2026-09-05", civil.Date{Year: 2026, Month: time.September, Day: 5}.String())
	s.Equal("2026-12-31", civil.Date{Year: 2026, Month: time.December, Day: 31}.String())
}

func (s *DateSuite) TestIsValid() {
	s.True(civil.Date{Year: 2026, Month: time.September, Day: 22}.IsValid())
	s.False(civil.Date{Year: 2026, Month: time.February, Day: 30}.IsValid())
}

func (s *DateSuite) TestIsZero() {
	s.True(civil.Date{}.IsZero())
	s.False(civil.Date{Year: 2026, Month: time.September, Day: 22}.IsZero())
}

func (s *DateSuite) TestIn() {
	vilnius, err := time.LoadLocation("Europe/Vilnius")
	s.Require().NoError(err)

	got := civil.Date{Year: 2026, Month: time.September, Day: 22}.In(vilnius)

	s.Equal(time.Date(2026, time.September, 22, 0, 0, 0, 0, vilnius), got)
}

func (s *DateSuite) TestMarshalUnmarshalText() {
	d := civil.Date{Year: 2026, Month: time.September, Day: 22}

	text, err := d.MarshalText()
	s.Require().NoError(err)
	s.Equal("2026-09-22", string(text))

	var got civil.Date
	s.Require().NoError(got.UnmarshalText(text))
	s.Equal(d, got)

	s.Require().Error(got.UnmarshalText([]byte("not-a-date")))
}
