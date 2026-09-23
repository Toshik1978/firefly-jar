package civil_test

import (
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/civil"
)

// RangeSuite covers civil.Range: the inclusive check window derived from today and a window
// length in days.
type RangeSuite struct {
	suite.Suite
}

func (s *RangeSuite) TestNewRangeFromAndTo() {
	today := civil.Date{Year: 2026, Month: time.September, Day: 22}

	got := civil.NewRange(today, 30)

	s.Equal(today, got.To)
	s.Equal(today.AddDays(-29), got.From)
	s.Equal(civil.Date{Year: 2026, Month: time.August, Day: 24}, got.From)
}

func (s *RangeSuite) TestContainsIncludesBothEnds() {
	today := civil.Date{Year: 2026, Month: time.September, Day: 22}
	r := civil.NewRange(today, 30)

	s.True(r.Contains(r.From), "From is included")
	s.True(r.Contains(r.To), "To is included")
	s.True(r.Contains(civil.Date{Year: 2026, Month: time.September, Day: 1}), "a day inside the window")

	s.False(r.Contains(r.From.AddDays(-1)), "the day before From is excluded")
	s.False(r.Contains(r.To.AddDays(1)), "the day after To is excluded")
}

func (s *RangeSuite) TestIsFirstDayTrueOnlyForFrom() {
	today := civil.Date{Year: 2026, Month: time.September, Day: 22}
	r := civil.NewRange(today, 30)

	s.True(r.IsFirstDay(r.From))
	s.False(r.IsFirstDay(r.To))
	s.False(r.IsFirstDay(r.From.AddDays(1)))
}
