package academic_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/shared"
)

// openYear builds an open year whose dates match its code, since the domain
// requires the two to agree.
func openYear(t *testing.T, code string) *academic.Year {
	t.Helper()
	startYear, err := strconv.Atoi(code[:4])
	if err != nil {
		t.Fatalf("malformed test year code %q: %v", code, err)
	}
	y, err := academic.NewYear(code,
		shared.NewDate(startYear, time.September, 1),
		shared.NewDate(startYear+1, time.July, 1))
	if err != nil {
		t.Fatalf("building year: %v", err)
	}
	if err := y.Open(); err != nil {
		t.Fatalf("opening year: %v", err)
	}
	return y
}

// The two-phase close is the design's answer to the Iraqi calendar: the
// treasury shuts its books weeks before second-round results arrive. A single
// closed flag forces one of the two to be wrong.
func TestFinancialCloseFreezesMoneyButNotResults(t *testing.T) {
	year := openYear(t, "2025-2026")
	actor := shared.NewID()
	now := time.Now().UTC()

	if !year.AcceptsFinancialPosting() || !year.AcceptsAcademicRecording() {
		t.Fatal("an open year accepts both money and results")
	}

	if err := year.CloseFinancially(actor, now); err != nil {
		t.Fatal(err)
	}

	if year.AcceptsFinancialPosting() {
		t.Error("a financially closed year must not accept money")
	}
	if !year.AcceptsAcademicRecording() {
		t.Error("a financially closed year must still accept second-round results")
	}

	if err := year.Close(actor, now); err != nil {
		t.Fatal(err)
	}
	if year.AcceptsAcademicRecording() {
		t.Error("a fully closed year accepts nothing")
	}
}

func TestCloseRequiresFinancialCloseFirst(t *testing.T) {
	year := openYear(t, "2025-2026")
	if err := year.Close(shared.NewID(), time.Now().UTC()); err == nil {
		t.Error("closing a year that has not shut its books must be refused")
	}
}

// The error a cashier sees when a year is closed must point at the way
// forward, because prior-year debt is still collectible and refusing without
// explanation is how staff end up working around the system.
func TestClosedYearErrorExplainsThatDebtIsStillCollectible(t *testing.T) {
	year := openYear(t, "2024-2025")
	if err := year.CloseFinancially(shared.NewID(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	err := year.RequireFinancialPosting("recording a payment")
	if err == nil {
		t.Fatal("posting into a closed year must be refused")
	}
	domainErr, ok := shared.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %T", err)
	}
	if _, present := domainErr.Details["remedy"]; !present {
		t.Error("the refusal must tell the operator what to do instead")
	}
}

func TestAdjustmentWindowIsBoundedAndOneWay(t *testing.T) {
	year := openYear(t, "2024-2025")
	actor := shared.NewID()
	now := time.Now().UTC()

	if err := year.CloseFinancially(actor, now); err != nil {
		t.Fatal(err)
	}
	if err := year.Close(actor, now); err != nil {
		t.Fatal(err)
	}

	if err := year.OpenForAdjustment("", now.Add(time.Hour)); err == nil {
		t.Error("reopening closed books without a written reason must be refused")
	}

	deadline := now.Add(2 * time.Hour)
	if err := year.OpenForAdjustment("ministry correction to a 2024-2025 fee", deadline); err != nil {
		t.Fatal(err)
	}
	if !year.AcceptsFinancialPosting() {
		t.Error("an adjustment window must accept the correction it was opened for")
	}

	if year.AdjustmentWindowExpired(now.Add(time.Hour)) {
		t.Error("the window has not expired yet")
	}
	if !year.AdjustmentWindowExpired(now.Add(3 * time.Hour)) {
		t.Error("the window should be expired past its deadline")
	}

	if err := year.CloseAdjustmentWindow(); err != nil {
		t.Fatal(err)
	}
	if year.AcceptsFinancialPosting() {
		t.Error("closing the window must freeze the year again")
	}
}

func TestYearCodeMustMatchItsDates(t *testing.T) {
	if _, err := academic.NewYear("2026-2027", shared.NewDate(2025, 9, 1), shared.NewDate(2026, 7, 1)); err == nil {
		t.Error("a code disagreeing with the start date must be refused: reports grouped by " +
			"code would otherwise disagree with reports filtered by date")
	}
	for _, bad := range []string{"2025", "25-26", "2025/2026", ""} {
		if _, err := academic.NewYear(bad, shared.NewDate(2025, 9, 1), shared.NewDate(2026, 7, 1)); err == nil {
			t.Errorf("malformed code %q must be refused", bad)
		}
	}
	if _, err := academic.NewYear("2025-2026", shared.NewDate(2026, 7, 1), shared.NewDate(2025, 9, 1)); err == nil {
		t.Error("a year ending before it starts must be refused")
	}
}

// Two years are legitimately open every autumn, while results close one and
// registration opens the next. Nothing in the model forbids it.
func TestTwoYearsMayBeOpenAtOnce(t *testing.T) {
	outgoing := openYear(t, "2024-2025")
	incoming := openYear(t, "2025-2026")

	if !outgoing.AcceptsFinancialPosting() || !incoming.AcceptsFinancialPosting() {
		t.Error("the autumn overlap requires both years to accept money")
	}
}
