// Package money provides exact, integer-based monetary arithmetic for Iraqi Dinar.
//
// Every amount in this system is a whole number of dinars stored in an int64.
// Floating point is never used: not in arithmetic, not in storage, not on the
// wire. Percentage math runs through basis points with explicit half-up
// rounding so that a discount computed today and recomputed in five years
// yields a bit-identical result.
package money

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Amount is a whole number of Iraqi Dinars. IQD has no subunit in practice —
// fils were withdrawn from circulation — so the smallest representable unit is
// one dinar and no scaling factor is applied.
type Amount int64

// Zero is the additive identity, provided for readability at call sites.
const Zero Amount = 0

var (
	// ErrOverflow reports an arithmetic result outside the int64 range. A
	// financial system must fail loudly here rather than wrap around.
	ErrOverflow = errors.New("money: arithmetic overflow")
	// ErrNegative reports a negative amount where the domain forbids one.
	ErrNegative = errors.New("money: negative amount not allowed")
	// ErrParse reports an unparseable textual amount.
	ErrParse = errors.New("money: cannot parse amount")
)

// FromInt64 builds an Amount from a raw dinar count.
func FromInt64(v int64) Amount { return Amount(v) }

// Int64 returns the raw dinar count, for storage and wire encoding.
func (a Amount) Int64() int64 { return int64(a) }

// IsZero reports whether the amount is exactly zero.
func (a Amount) IsZero() bool { return a == 0 }

// IsPositive reports whether the amount is strictly greater than zero.
func (a Amount) IsPositive() bool { return a > 0 }

// IsNegative reports whether the amount is strictly less than zero.
func (a Amount) IsNegative() bool { return a < 0 }

// Add returns a+b, or ErrOverflow if the sum leaves the int64 range.
func (a Amount) Add(b Amount) (Amount, error) {
	sum := a + b
	// Overflow occurred iff the operands share a sign that the result does not.
	if (a > 0 && b > 0 && sum < 0) || (a < 0 && b < 0 && sum >= 0) {
		return 0, fmt.Errorf("%w: %d + %d", ErrOverflow, a, b)
	}
	return sum, nil
}

// Sub returns a-b, or ErrOverflow if the difference leaves the int64 range.
func (a Amount) Sub(b Amount) (Amount, error) {
	if b == math.MinInt64 {
		return 0, fmt.Errorf("%w: %d - %d", ErrOverflow, a, b)
	}
	return a.Add(-b)
}

// MustAdd returns a+b and panics on overflow. Reserved for sums over values
// already validated as bounded (for example, a handful of fee components).
func (a Amount) MustAdd(b Amount) Amount {
	sum, err := a.Add(b)
	if err != nil {
		panic(err)
	}
	return sum
}

// Neg returns -a.
func (a Amount) Neg() Amount { return -a }

// Abs returns the magnitude of a.
func (a Amount) Abs() Amount {
	if a < 0 {
		return -a
	}
	return a
}

// Min returns the smaller of a and b.
func Min(a, b Amount) Amount {
	if a < b {
		return a
	}
	return b
}

// Max returns the larger of a and b.
func Max(a, b Amount) Amount {
	if a > b {
		return a
	}
	return b
}

// ClampNonNegative returns a, or zero when a is negative. Used wherever the
// domain floors a computed value (net fees, remaining balances).
func (a Amount) ClampNonNegative() Amount {
	if a < 0 {
		return 0
	}
	return a
}

// Sum adds a slice of amounts, reporting overflow rather than wrapping.
func Sum(amounts ...Amount) (Amount, error) {
	var total Amount
	for i, amt := range amounts {
		next, err := total.Add(amt)
		if err != nil {
			return 0, fmt.Errorf("summing element %d: %w", i, err)
		}
		total = next
	}
	return total, nil
}

// String renders the amount in plain digits with no separators, suitable for
// logs and for exact round-tripping. Presentation formatting belongs in the
// HTTP layer, not here.
func (a Amount) String() string { return strconv.FormatInt(int64(a), 10) }

// Format renders the amount with thousands separators for receipts and
// reports, e.g. 1500000 becomes "1,500,000".
func (a Amount) Format() string {
	s := strconv.FormatInt(int64(a.Abs()), 10)
	var b strings.Builder
	if a < 0 {
		b.WriteByte('-')
	}
	for i, digit := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return b.String()
}

// MarshalJSON encodes the amount as a JSON number of whole dinars. Values stay
// well inside the 2^53 range JavaScript can represent exactly, and encoding as
// a number keeps clients from reintroducing float parsing on a string.
func (a Amount) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(int64(a), 10)), nil
}

// UnmarshalJSON accepts either a JSON number or a quoted integer string, and
// rejects anything carrying a fractional part.
func (a *Amount) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "null" {
		*a = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	if strings.ContainsAny(s, ".eE") {
		return fmt.Errorf("%w: %q has a fractional or exponent part; IQD amounts are whole dinars", ErrParse, s)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrParse, s)
	}
	*a = Amount(v)
	return nil
}

// Value implements driver.Valuer so amounts map onto PostgreSQL BIGINT.
func (a Amount) Value() (driver.Value, error) { return int64(a), nil }

// Scan implements sql.Scanner for BIGINT columns.
func (a *Amount) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*a = 0
	case int64:
		*a = Amount(v)
	case int32:
		*a = Amount(v)
	case []byte:
		parsed, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return fmt.Errorf("%w: %q", ErrParse, v)
		}
		*a = Amount(parsed)
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("%w: %q", ErrParse, v)
		}
		*a = Amount(parsed)
	default:
		return fmt.Errorf("money: cannot scan %T into Amount", src)
	}
	return nil
}

// Parse reads a decimal string of whole dinars, tolerating spaces, underscores
// and thousands separators as they appear in imported spreadsheets.
func Parse(s string) (Amount, error) {
	cleaned := strings.NewReplacer(",", "", "_", "", " ", "", " ", "").Replace(strings.TrimSpace(s))
	if cleaned == "" {
		return 0, fmt.Errorf("%w: empty string", ErrParse)
	}
	// Tolerate a trailing ".0" style zero fraction from Excel exports, but
	// reject any fraction that would actually lose money.
	if dot := strings.IndexByte(cleaned, '.'); dot >= 0 {
		frac := strings.TrimRight(cleaned[dot+1:], "0")
		if frac != "" {
			return 0, fmt.Errorf("%w: %q has a fractional part; IQD amounts are whole dinars", ErrParse, s)
		}
		cleaned = cleaned[:dot]
	}
	v, err := strconv.ParseInt(cleaned, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrParse, s)
	}
	return Amount(v), nil
}
