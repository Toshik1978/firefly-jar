package money

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// maxSignificantDigits is the most decimal digits (integer plus fractional, leading and trailing
// zeros stripped) ParseAmount accepts, per R15.
const maxSignificantDigits = 18

// Amount is an exact decimal amount in a single currency, backed by shopspring/decimal so no float
// is ever involved. A zero-value Amount is the zero amount in an empty currency; construct real
// values with ParseAmount.
type Amount struct {
	Value    decimal.Decimal
	Currency string
}

// ParseAmount parses a decimal string (for example "12.34" or "-12.40") into an Amount for
// currency, without ever going through a float. The grammar is stricter than
// decimal.NewFromString alone: only an optional leading sign, digits, and at most one decimal
// point are accepted, so scientific notation, thousands separators and stray whitespace are all
// rejected. More than maxSignificantDigits significant digits is rejected too (R15).
func ParseAmount(s, currency string) (Amount, error) {
	if err := validateGrammar(s); err != nil {
		return Amount{}, fmt.Errorf("parse amount %q: %w", s, err)
	}

	value, err := decimal.NewFromString(s)
	if err != nil {
		return Amount{}, fmt.Errorf("parse amount %q: %w", s, err)
	}

	return Amount{Value: value, Currency: currency}, nil
}

// Equal reports whether a and other are the same value in the same currency, regardless of
// trailing fractional zeros in either operand's underlying representation.
func (a Amount) Equal(other Amount) bool {
	if a.Currency != other.Currency {
		return false
	}

	return a.Value.Equal(other.Value)
}

// Add returns a + other in their shared currency, for summing Firefly split groups (FR-009). It
// errors when the currencies differ, since adding amounts across currencies is never meaningful
// here.
func (a Amount) Add(other Amount) (Amount, error) {
	if a.Currency != other.Currency {
		return Amount{}, fmt.Errorf("add amount: currency mismatch %s vs %s", a.Currency, other.Currency)
	}

	return Amount{Value: a.Value.Add(other.Value), Currency: a.Currency}, nil
}

// IsZero reports whether a is the zero amount, regardless of scale or currency.
func (a Amount) IsZero() bool {
	return a.Value.IsZero()
}

// Neg returns a with its sign flipped.
func (a Amount) Neg() Amount {
	return Amount{Value: a.Value.Neg(), Currency: a.Currency}
}

// Abs returns a with a non-negative sign.
func (a Amount) Abs() Amount {
	return Amount{Value: a.Value.Abs(), Currency: a.Currency}
}

// Sign returns -1, 0 or 1 for a negative, zero or positive amount.
func (a Amount) Sign() int {
	return a.Value.Sign()
}

// Key returns a string that is identical for two Amounts iff Equal reports true for them, for use
// as a map key when grouping bank transactions and Firefly entries by amount and currency.
// decimal.Decimal.String trims trailing fractional zeros, so unnormalized values collapse to the
// same key.
func (a Amount) Key() string {
	return fmt.Sprintf("%s:%s", a.Currency, a.Value.String())
}

// Format renders a as a decimal string with exactly decimalPlaces digits after the decimal point
// (or none, when decimalPlaces is zero), rounding half away from zero if decimalPlaces is smaller
// than the value's stored precision. decimal.Decimal is backed by big.Int, so padding to a large
// decimalPlaces never overflows.
func (a Amount) Format(decimalPlaces uint8) string {
	return a.Value.StringFixed(int32(decimalPlaces))
}

// validateGrammar rejects everything decimal.NewFromString would accept but ParseAmount must not:
// scientific notation, a leading '+' spelled with surrounding junk, thousands separators, stray
// whitespace, and values with more than maxSignificantDigits significant digits. It accepts only
// an optional leading sign followed by digits, optionally followed by a single '.' and more
// digits.
func validateGrammar(s string) error {
	if s == "" {
		return errors.New("empty string")
	}

	rest := s
	if rest[0] == '+' || rest[0] == '-' {
		rest = rest[1:]
	}

	intPart, fracPart, hasFrac := strings.Cut(rest, ".")
	if hasFrac && strings.Contains(fracPart, ".") {
		return errors.New("multiple decimal points")
	}

	if !isDigits(intPart) {
		return fmt.Errorf("invalid integer part %q", intPart)
	}

	if hasFrac && !isDigits(fracPart) {
		return fmt.Errorf("invalid fractional part %q", fracPart)
	}

	return validateSignificantDigits(intPart, fracPart)
}

// validateSignificantDigits rejects values with more than maxSignificantDigits significant digits
// (leading and trailing zeros stripped), per R15.
func validateSignificantDigits(intPart, fracPart string) error {
	combined := intPart + strings.TrimRight(fracPart, "0")

	significant := strings.TrimLeft(combined, "0")
	if len(significant) > maxSignificantDigits {
		return fmt.Errorf("more than %d significant digits", maxSignificantDigits)
	}

	return nil
}

// isDigits reports whether s is non-empty and consists only of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}
