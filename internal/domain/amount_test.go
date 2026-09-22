package domain_test

import (
	"strings"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/domain"
)

// AmountSuite covers domain.Amount: exact decimal parsing, normalization and the arithmetic and
// formatting helpers built on it.
type AmountSuite struct {
	suite.Suite
}

func (s *AmountSuite) TestParseAmountGivesValue() {
	got, err := domain.ParseAmount("12.34", "EUR")
	s.Require().NoError(err)

	want, err := decimal.NewFromString("12.34")
	s.Require().NoError(err)

	s.True(want.Equal(got.Value))
	s.Equal("EUR", got.Currency)
}

func (s *AmountSuite) TestParseAmountNegativeIsNegative() {
	got, err := domain.ParseAmount("-12.40", "EUR")
	s.Require().NoError(err)

	s.Equal(-1, got.Sign())
	s.True(got.Value.IsNegative())
}

func (s *AmountSuite) TestParseAmountNormalizesTrailingFractionalZeros() {
	long, err := domain.ParseAmount("12.340000000000", "EUR")
	s.Require().NoError(err)

	short, err := domain.ParseAmount("12.34", "EUR")
	s.Require().NoError(err)

	s.True(long.Equal(short), "normalized values with equal currency must compare equal")
	s.Equal(short.Value.String(), long.Value.String())
}

func (s *AmountSuite) TestParseAmountZeroIsZero() {
	cases := []string{"0", "0.00"}

	for _, in := range cases {
		s.Run(in, func() {
			got, err := domain.ParseAmount(in, "EUR")
			s.Require().NoError(err)
			s.True(got.IsZero())
		})
	}
}

func (s *AmountSuite) TestParseAmountRejectsInvalidInput() {
	tooManySigDigits := strings.Repeat("9", 19)

	cases := map[string]string{
		"empty string":            "",
		"comma decimal separator": "1,23",
		"two decimal points":      "1.2.3",
		"not a number":            "abc",
		"leading whitespace":      " 1",
		"more than 18 sig digits": tooManySigDigits,
		"exponent notation":       "1.5e2",
	}

	for name, in := range cases {
		s.Run(name, func() {
			_, err := domain.ParseAmount(in, "EUR")
			s.Require().Error(err)
		})
	}
}

func (s *AmountSuite) TestEqualFalseWhenCurrencyDiffers() {
	eur, err := domain.ParseAmount("12.34", "EUR")
	s.Require().NoError(err)

	usd, err := domain.ParseAmount("12.34", "USD")
	s.Require().NoError(err)

	s.False(eur.Equal(usd))
}

func (s *AmountSuite) TestNeg() {
	got, err := domain.ParseAmount("12.34", "EUR")
	s.Require().NoError(err)

	neg := got.Neg()

	s.Equal(-1, neg.Sign())
	s.True(got.Value.Equal(neg.Value.Neg()))
	s.Equal(got.Currency, neg.Currency)
}

func (s *AmountSuite) TestAbs() {
	negative, err := domain.ParseAmount("-12.34", "EUR")
	s.Require().NoError(err)

	positive, err := domain.ParseAmount("12.34", "EUR")
	s.Require().NoError(err)

	s.Equal(1, negative.Abs().Sign())
	s.True(negative.Abs().Equal(positive))
	s.Equal(1, positive.Abs().Sign(), "Abs of an already-positive amount stays positive")
}

func (s *AmountSuite) TestKeyIdenticalForEqualValueAndCurrency() {
	long, err := domain.ParseAmount("12.340000000000", "EUR")
	s.Require().NoError(err)

	short, err := domain.ParseAmount("12.34", "EUR")
	s.Require().NoError(err)

	s.Equal(short.Key(), long.Key())
}

func (s *AmountSuite) TestKeyDiffersByCurrency() {
	eur, err := domain.ParseAmount("12.34", "EUR")
	s.Require().NoError(err)

	usd, err := domain.ParseAmount("12.34", "USD")
	s.Require().NoError(err)

	s.NotEqual(eur.Key(), usd.Key())
}

func (s *AmountSuite) TestFormat() {
	negative, err := domain.ParseAmount("-4.5", "EUR")
	s.Require().NoError(err)
	s.Equal("-4.50", negative.Format(2))

	whole, err := domain.ParseAmount("12.00", "EUR")
	s.Require().NoError(err)
	s.Equal("12", whole.Format(0))
}

func (s *AmountSuite) TestFormatPadsFractionWithoutOverflowingMinor() {
	one, err := domain.ParseAmount("1", "EUR")
	s.Require().NoError(err)

	s.Equal("1."+strings.Repeat("0", 20), one.Format(20))
}

func (s *AmountSuite) TestEqualAndKeyNormalizeUnnormalizedValues() {
	scaled := domain.Amount{Value: decimal.New(1200, -2), Currency: "EUR"}
	reduced := domain.Amount{Value: decimal.New(12, 0), Currency: "EUR"}

	s.True(scaled.Equal(reduced), "1200 at scale 2 must equal 12 at scale 0")
	s.Equal(reduced.Key(), scaled.Key())
}

func (s *AmountSuite) TestAddSumsAndNormalizes() {
	a, err := domain.ParseAmount("12.30", "EUR")
	s.Require().NoError(err)

	b, err := domain.ParseAmount("0.70", "EUR")
	s.Require().NoError(err)

	sum, err := a.Add(b)
	s.Require().NoError(err)

	want, err := domain.ParseAmount("13", "EUR")
	s.Require().NoError(err)

	s.True(sum.Equal(want))
	s.Equal("EUR", sum.Currency)
}

func (s *AmountSuite) TestAddRejectsCurrencyMismatch() {
	eur, err := domain.ParseAmount("12.30", "EUR")
	s.Require().NoError(err)

	usd, err := domain.ParseAmount("0.70", "USD")
	s.Require().NoError(err)

	_, err = eur.Add(usd)
	s.Require().Error(err)
}
