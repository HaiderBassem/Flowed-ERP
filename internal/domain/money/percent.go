package money

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// BasisPoints expresses a percentage as hundredths of a percent: 10.00% is
// 1000, and 100.00% is 10000. Storing the rate as an integer keeps discount
// arithmetic exactly reproducible; a float rate of 12.5% would already differ
// in the last bits between machines and Go versions.
type BasisPoints int32

// FullRate is 100.00%.
const FullRate BasisPoints = 10_000

// ErrInvalidRate reports a rate outside the representable 0%..100% range.
var ErrInvalidRate = errors.New("money: invalid percentage rate")

// NewBasisPoints validates and builds a rate. Rates above 100% are rejected:
// the discount pipeline caps every application at the discountable base
// anyway, and a stored rate above 100% is always a data-entry error.
func NewBasisPoints(bp int32) (BasisPoints, error) {
	if bp < 0 || bp > int32(FullRate) {
		return 0, fmt.Errorf("%w: %d basis points is outside 0..10000", ErrInvalidRate, bp)
	}
	return BasisPoints(bp), nil
}

// ParsePercent reads a human percentage such as "12.5" or "12.50%" into basis
// points, rejecting precision finer than one hundredth of a percent.
func ParsePercent(s string) (BasisPoints, error) {
	cleaned := strings.TrimSuffix(strings.TrimSpace(s), "%")
	if cleaned == "" {
		return 0, fmt.Errorf("%w: empty string", ErrInvalidRate)
	}
	whole, frac, hasFrac := strings.Cut(cleaned, ".")
	units, err := strconv.ParseInt(whole, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidRate, s)
	}
	bp := units * 100
	if hasFrac {
		frac = strings.TrimRight(frac, "0")
		if len(frac) > 2 {
			return 0, fmt.Errorf("%w: %q has finer precision than 0.01%%", ErrInvalidRate, s)
		}
		for len(frac) < 2 {
			frac += "0"
		}
		hundredths, err := strconv.ParseInt(frac, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("%w: %q", ErrInvalidRate, s)
		}
		if units < 0 {
			hundredths = -hundredths
		}
		bp += hundredths
	}
	return NewBasisPoints(int32(bp))
}

// Int32 returns the raw basis-point count for storage.
func (bp BasisPoints) Int32() int32 { return int32(bp) }

// IsZero reports whether the rate is exactly zero.
func (bp BasisPoints) IsZero() bool { return bp == 0 }

// IsFull reports whether the rate is exactly 100%, which marks a full
// exemption (اعفاء كامل) in the discount subsystem.
func (bp BasisPoints) IsFull() bool { return bp == FullRate }

// String renders the rate as a human percentage, e.g. "12.5%".
func (bp BasisPoints) String() string {
	whole := int32(bp) / 100
	frac := int32(bp) % 100
	if frac < 0 {
		frac = -frac
	}
	switch {
	case frac == 0:
		return fmt.Sprintf("%d%%", whole)
	case frac%10 == 0:
		return fmt.Sprintf("%d.%d%%", whole, frac/10)
	default:
		return fmt.Sprintf("%d.%02d%%", whole, frac)
	}
}

// Value implements driver.Valuer for INTEGER columns.
func (bp BasisPoints) Value() (driver.Value, error) { return int64(bp), nil }

// Scan implements sql.Scanner for INTEGER columns.
func (bp *BasisPoints) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*bp = 0
	case int64:
		*bp = BasisPoints(v)
	case int32:
		*bp = BasisPoints(v)
	default:
		return fmt.Errorf("money: cannot scan %T into BasisPoints", src)
	}
	return nil
}

// ApplyRate returns base × rate, rounded half-up to whole dinars.
//
// Half-up is the rounding convention Iraqi finance offices apply on paper, and
// it is the only convention this system uses: Go's math.Round is never
// consulted, and banker's rounding — which would silently favour even dinars —
// is deliberately avoided. The computation runs entirely in int64: for a base
// below ~9.2×10^14 dinars the intermediate product cannot overflow, which is
// twelve orders of magnitude above any real tuition.
//
//	ApplyRate(2_000_000, 1000)  = 200_000   // 10% of two million
//	ApplyRate(3_750_000, 1250)  = 468_750   // 12.5%, exact
//	ApplyRate(1_000_005, 50)    = 5_000     // 0.5% of 1,000,005 = 5000.025 -> 5000
//	ApplyRate(1_000_100, 50)    = 5_001     // 5000.5 -> 5001 (half-up, not half-even)
func ApplyRate(base Amount, rate BasisPoints) (Amount, error) {
	if base == 0 || rate == 0 {
		return 0, nil
	}
	negative := base < 0
	magnitude := int64(base.Abs())

	const scale = int64(FullRate)
	if magnitude > (int64(1)<<62)/scale {
		return 0, fmt.Errorf("%w: base %d too large for exact rate arithmetic", ErrOverflow, base)
	}

	product := magnitude * int64(rate)
	quotient := product / scale
	remainder := product % scale
	// Half-up on the magnitude, so that -x rounds as the mirror of +x.
	if remainder*2 >= scale {
		quotient++
	}
	if negative {
		quotient = -quotient
	}
	return Amount(quotient), nil
}

// MustApplyRate is ApplyRate for callers holding already-bounded inputs.
func MustApplyRate(base Amount, rate BasisPoints) Amount {
	v, err := ApplyRate(base, rate)
	if err != nil {
		panic(err)
	}
	return v
}

// SplitByRates divides a total across weighted shares so that the parts sum
// back to exactly the total. Each share is rounded half-up, and the residual
// left by rounding is assigned to the share at remainderIndex — which callers
// set to the lowest-numbered *open* installment rather than always the first,
// so that a re-split never has to touch an installment that is already paid.
//
// The returned slice has the same length as weights. A nil or empty weights
// slice returns nil.
func SplitByRates(total Amount, weights []BasisPoints, remainderIndex int) ([]Amount, error) {
	if len(weights) == 0 {
		return nil, nil
	}
	if remainderIndex < 0 || remainderIndex >= len(weights) {
		return nil, fmt.Errorf("money: remainder index %d out of range for %d shares", remainderIndex, len(weights))
	}

	var weightSum int64
	for _, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("%w: negative weight %d", ErrInvalidRate, w)
		}
		weightSum += int64(w)
	}
	if weightSum != int64(FullRate) {
		return nil, fmt.Errorf("%w: weights sum to %d basis points, expected %d", ErrInvalidRate, weightSum, FullRate)
	}

	parts := make([]Amount, len(weights))
	var allocated Amount
	for i, w := range weights {
		part, err := ApplyRate(total, w)
		if err != nil {
			return nil, fmt.Errorf("computing share %d: %w", i, err)
		}
		parts[i] = part
		next, err := allocated.Add(part)
		if err != nil {
			return nil, fmt.Errorf("accumulating share %d: %w", i, err)
		}
		allocated = next
	}

	residual, err := total.Sub(allocated)
	if err != nil {
		return nil, fmt.Errorf("computing rounding residual: %w", err)
	}
	if residual != 0 {
		adjusted, err := parts[remainderIndex].Add(residual)
		if err != nil {
			return nil, fmt.Errorf("assigning rounding residual: %w", err)
		}
		parts[remainderIndex] = adjusted
	}
	return parts, nil
}
