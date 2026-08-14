package money_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/swibit/flowed/internal/domain/money"
)

func TestApplyRateIsExactAndHalfUp(t *testing.T) {
	cases := []struct {
		name string
		base money.Amount
		rate money.BasisPoints
		want money.Amount
	}{
		{"ten percent of two million", 2_000_000, 1000, 200_000},
		{"twelve and a half percent, exact", 3_750_000, 1250, 468_750},
		{"full exemption clears the base", 1_500_000, money.FullRate, 1_500_000},
		{"zero rate takes nothing", 1_500_000, 0, 0},
		{"zero base yields zero", 0, 5000, 0},
		// 1,000,005 x 0.5% = 5000.025, which rounds down.
		{"fraction below half rounds down", 1_000_005, 50, 5_000},
		// 1,000,100 x 0.5% = 5000.5 exactly, which must round UP. Go's
		// math.Round would agree here but strconv-based float paths and
		// banker's rounding would not, which is the whole reason this is
		// integer arithmetic.
		{"exact half rounds up", 1_000_100, 50, 5_001},
		{"one dinar at one basis point rounds to zero", 1, 1, 0},
		{"33.33 percent of a million", 1_000_000, 3333, 333_300},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := money.ApplyRate(tc.base, tc.rate)
			if err != nil {
				t.Fatalf("ApplyRate(%s, %s) returned error: %v", tc.base, tc.rate, err)
			}
			if got != tc.want {
				t.Errorf("ApplyRate(%s, %s) = %s, want %s", tc.base, tc.rate, got, tc.want)
			}
		})
	}
}

func TestApplyRateNeverLosesDinarsAcrossRepeatedCalls(t *testing.T) {
	// A rate applied to the same base must give the same answer every time,
	// including years later during a reconciliation run. Float arithmetic
	// would not guarantee this across platforms.
	const base = money.Amount(1_234_567)
	rate := money.BasisPoints(1733)

	first, err := money.ApplyRate(base, rate)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		again, err := money.ApplyRate(base, rate)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("iteration %d gave %s, first call gave %s", i, again, first)
		}
	}
}

func TestSplitByRatesAlwaysSumsToTheTotal(t *testing.T) {
	// The property that matters: however the shares round, the parts add back
	// to the whole. A plan that does not sum to the net is a reconciliation
	// failure waiting to happen.
	equalQuarters := []money.BasisPoints{2500, 2500, 2500, 2500}
	unevenThirds := []money.BasisPoints{3334, 3333, 3333}
	iraqiPlan := []money.BasisPoints{3334, 2000, 2666, 2000}

	totals := []money.Amount{
		0, 1, 7, 999, 1_000_000, 1_000_001, 1_500_000, 2_000_003, 999_999_999,
	}

	for _, weights := range [][]money.BasisPoints{equalQuarters, unevenThirds, iraqiPlan} {
		for _, total := range totals {
			parts, err := money.SplitByRates(total, weights, 0)
			if err != nil {
				t.Fatalf("SplitByRates(%s, %v) returned error: %v", total, weights, err)
			}
			var sum money.Amount
			for _, p := range parts {
				if p.IsNegative() {
					t.Errorf("SplitByRates(%s, %v) produced a negative share %s", total, weights, p)
				}
				sum = sum.MustAdd(p)
			}
			if sum != total {
				t.Errorf("SplitByRates(%s, %v) parts sum to %s, want %s (parts %v)",
					total, weights, sum, total, parts)
			}
		}
	}
}

func TestSplitByRatesPutsResidualWhereAsked(t *testing.T) {
	// 1,000,001 into thirds rounds each share down and leaves one dinar over.
	// Which share absorbs it is the caller's choice, because after a re-split
	// the first installment may already be paid and must not be touched.
	const total = money.Amount(1_000_001)
	weights := []money.BasisPoints{3333, 3333, 3334}

	baseline, err := money.SplitByRates(total, weights, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, index := range []int{0, 1, 2} {
		parts, err := money.SplitByRates(total, weights, index)
		if err != nil {
			t.Fatalf("SplitByRates with remainder index %d: %v", index, err)
		}

		var sum money.Amount
		for _, p := range parts {
			sum = sum.MustAdd(p)
		}
		if sum != total {
			t.Fatalf("remainder index %d: shares sum to %s, want %s", index, sum, total)
		}

		// Every share except the chosen one matches the un-adjusted split;
		// the chosen one carries the extra dinar.
		for i := range parts {
			want := baseline[i]
			if i == index {
				want = baseline[i]
				if index != 0 {
					want = baseline[i] + 1
				}
			} else if i == 0 && index != 0 {
				want = baseline[0] - 1
			}
			if parts[i] != want {
				t.Errorf("remainder index %d: share %d = %s, want %s (all shares %v)",
					index, i, parts[i], want, parts)
			}
		}
	}
}

func TestSplitByRatesRejectsAnOutOfRangeRemainderIndex(t *testing.T) {
	weights := []money.BasisPoints{5000, 5000}
	for _, index := range []int{-1, 2, 99} {
		if _, err := money.SplitByRates(1_000_000, weights, index); err == nil {
			t.Errorf("SplitByRates accepted out-of-range remainder index %d", index)
		}
	}
}

func TestSplitByRatesRejectsWeightsThatDoNotSumToOneHundredPercent(t *testing.T) {
	for _, weights := range [][]money.BasisPoints{
		{5000, 4000}, // 90%
		{5000, 6000}, // 110%
		{10000, 1},   // just over
	} {
		if _, err := money.SplitByRates(1_000_000, weights, 0); err == nil {
			t.Errorf("SplitByRates accepted weights %v that do not total 100%%", weights)
		}
	}
}

func TestAddDetectsOverflowRatherThanWrapping(t *testing.T) {
	if _, err := money.Amount(math.MaxInt64).Add(1); err == nil {
		t.Error("adding past MaxInt64 must report overflow, not wrap to a negative balance")
	}
	if _, err := money.Amount(math.MinInt64).Sub(1); err == nil {
		t.Error("subtracting past MinInt64 must report overflow")
	}
}

func TestJSONRejectsFractionalAmounts(t *testing.T) {
	// A client sending 1500000.5 has a bug, and silently truncating it would
	// lose half a dinar per request in a direction nobody notices.
	var a money.Amount
	if err := json.Unmarshal([]byte("1500000.5"), &a); err == nil {
		t.Error("unmarshalling a fractional amount must fail")
	}
	if err := json.Unmarshal([]byte("1500000"), &a); err != nil {
		t.Fatalf("unmarshalling a whole amount failed: %v", err)
	}
	if a != 1_500_000 {
		t.Errorf("got %s, want 1,500,000", a)
	}

	encoded, err := json.Marshal(money.Amount(1_500_000))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "1500000" {
		t.Errorf("marshalled to %s, want 1500000", encoded)
	}
}

func TestParseToleratesSpreadsheetFormatting(t *testing.T) {
	cases := map[string]money.Amount{
		"1500000":   1_500_000,
		"1,500,000": 1_500_000,
		"1 500 000": 1_500_000,
		"1500000.0": 1_500_000, // Excel exports whole numbers with a zero fraction
		" 750000 ":  750_000,
	}
	for input, want := range cases {
		got, err := money.Parse(input)
		if err != nil {
			t.Errorf("Parse(%q) returned error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("Parse(%q) = %s, want %s", input, got, want)
		}
	}

	if _, err := money.Parse("1500000.75"); err == nil {
		t.Error("Parse must reject a real fractional amount rather than truncating it")
	}
}

func TestFormatAddsThousandsSeparators(t *testing.T) {
	cases := map[money.Amount]string{
		0:          "0",
		999:        "999",
		1_000:      "1,000",
		1_500_000:  "1,500,000",
		-1_500_000: "-1,500,000",
	}
	for amount, want := range cases {
		if got := amount.Format(); got != want {
			t.Errorf("Amount(%d).Format() = %q, want %q", amount, got, want)
		}
	}
}

func TestParsePercent(t *testing.T) {
	cases := map[string]money.BasisPoints{
		"10":    1000,
		"10%":   1000,
		"12.5":  1250,
		"12.50": 1250,
		"0.01":  1,
		"100":   money.FullRate,
	}
	for input, want := range cases {
		got, err := money.ParsePercent(input)
		if err != nil {
			t.Errorf("ParsePercent(%q) returned error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePercent(%q) = %d, want %d", input, got, want)
		}
	}

	for _, bad := range []string{"101", "-5", "12.345", ""} {
		if _, err := money.ParsePercent(bad); err == nil {
			t.Errorf("ParsePercent(%q) should have failed", bad)
		}
	}
}
