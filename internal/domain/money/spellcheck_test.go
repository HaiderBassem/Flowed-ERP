package money_test

import (
	"testing"

	"flowed/internal/domain/money"
)

// Not an assertion — a readable sample of the spelling across the range a
// receipt actually carries, so the output can be eyeballed by someone who
// reads Arabic.
func TestShowSpelledAmounts(t *testing.T) {
	for _, a := range []money.Amount{
		1, 2, 3, 11, 100, 200, 1_000, 2_000, 200_000, 320_000, 500_000,
		640_000, 1_500_000, 2_200_000, 3_300_000, 12_345_678,
	} {
		t.Logf("%15s  %s", a.Format(), money.SpellArabic(a))
	}
}
