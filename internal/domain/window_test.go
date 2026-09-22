package domain_test

import (
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/civil"
	"github.com/Toshik1978/firefly-jar/internal/domain"
)

// WindowSuite covers domain.Window: the inclusive reconcile window derived from today and a
// window length in days.
type WindowSuite struct {
	suite.Suite
}

func (s *WindowSuite) TestNewWindowFromAndTo() {
	today := civil.Date{Year: 2026, Month: time.September, Day: 22}

	got := domain.NewWindow(today, 30)

	s.Equal(today, got.To)
	s.Equal(today.AddDays(-29), got.From)
	s.Equal(civil.Date{Year: 2026, Month: time.August, Day: 24}, got.From)
}

func (s *WindowSuite) TestContainsIncludesBothEnds() {
	today := civil.Date{Year: 2026, Month: time.September, Day: 22}
	w := domain.NewWindow(today, 30)

	s.True(w.Contains(w.From), "From is included")
	s.True(w.Contains(w.To), "To is included")
	s.True(w.Contains(civil.Date{Year: 2026, Month: time.September, Day: 1}), "a day inside the window")

	s.False(w.Contains(w.From.AddDays(-1)), "the day before From is excluded")
	s.False(w.Contains(w.To.AddDays(1)), "the day after To is excluded")
}

func (s *WindowSuite) TestIsFirstDayTrueOnlyForFrom() {
	today := civil.Date{Year: 2026, Month: time.September, Day: 22}
	w := domain.NewWindow(today, 30)

	s.True(w.IsFirstDay(w.From))
	s.False(w.IsFirstDay(w.To))
	s.False(w.IsFirstDay(w.From.AddDays(1)))
}
