package discount_test

import (
	"testing"
	"time"

	"flowed/internal/domain/discount"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

// A discount is money leaving the university, and the rules that govern one are
// in these two state machines rather than in the resolution engine beside them.
// The engine was tested; these were not.

// The four-eyes rule left with the second operator it depended on: the system
// is run from one account, and a rule refusing self-approval refused every
// approval. What has to survive is the record — who granted it and when — since
// that is now the whole of the control.
func TestAnApprovedGrantRecordsWhoGrantedItAndWhen(t *testing.T) {
	requester := shared.NewID()
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	assignment := submitted(t, requester)

	if err := assignment.Approve(requester, at); err != nil {
		t.Fatalf("the requester must be able to approve with one account: %v", err)
	}
	if assignment.Status != discount.AssignmentApproved {
		t.Errorf("status = %s, want approved", assignment.Status)
	}
	if assignment.ApprovedBy == nil || *assignment.ApprovedBy != requester {
		t.Error("an approved grant must record who approved it")
	}
	if assignment.ApprovedAt == nil || !assignment.ApprovedAt.Equal(at) {
		t.Error("an approved grant must record when")
	}

	// The order still binds. An approved grant approved again would stamp a
	// second approver over the first and lose the only record there is.
	if err := assignment.Approve(shared.NewID(), at); err == nil {
		t.Error("an approved grant must not be approved a second time")
	}
}

func TestTheAssignmentLifecycleRefusesEveryOtherOrder(t *testing.T) {
	actor := shared.NewID()
	at := time.Now().UTC()

	t.Run("a draft cannot be approved", func(t *testing.T) {
		assignment := draft(t, shared.NewID())
		if err := assignment.Approve(actor, at); err == nil {
			t.Error("approving a grant nobody submitted skips the request itself")
		}
	})

	t.Run("an approved grant cannot be submitted again", func(t *testing.T) {
		assignment := submitted(t, shared.NewID())
		if err := assignment.Approve(actor, at); err != nil {
			t.Fatal(err)
		}
		if err := assignment.Submit(); err == nil {
			t.Error("re-submitting an approved grant would return it to the queue " +
				"while it is already granting money")
		}
	})

	t.Run("a rejection must say why", func(t *testing.T) {
		assignment := submitted(t, shared.NewID())
		if err := assignment.Reject(actor, "", at); err == nil {
			t.Error("a rejection with no reason is one the student cannot be answered about")
		}
		if err := assignment.Reject(actor, "not eligible this year", at); err != nil {
			t.Fatalf("a reasoned rejection: %v", err)
		}
		if assignment.Status != discount.AssignmentRejected {
			t.Errorf("status = %s, want rejected", assignment.Status)
		}
	})

	t.Run("only an approved grant can be revoked", func(t *testing.T) {
		assignment := submitted(t, shared.NewID())
		if err := assignment.Revoke(actor, "withdrawn", discount.RevokeProspectiveOnly, at); err == nil {
			t.Error("revoking a grant that was never approved unwinds nothing")
		}
	})

	t.Run("a revocation states its reason and its reach", func(t *testing.T) {
		assignment := approved(t, actor)

		if err := assignment.Revoke(actor, "", discount.RevokeProspectiveOnly, at); err == nil {
			t.Error("a revocation with no reason leaves nobody able to explain the change")
		}
		if err := assignment.Revoke(actor, "fraud", "next-tuesday", at); err == nil {
			t.Error("an unrecognised effect must be refused rather than guessed: the two " +
				"answers differ by a year's fees")
		}

		// The default is the conservative one: money already given stays given.
		if err := assignment.Revoke(actor, "no longer eligible", "", at); err != nil {
			t.Fatalf("revoking with the default effect: %v", err)
		}
		if assignment.RevocationEffect == nil ||
			*assignment.RevocationEffect != discount.RevokeProspectiveOnly {
			t.Error("an unstated effect must default to prospective only; taking back a " +
				"discount already applied is a decision somebody has to make explicitly")
		}
	})
}

func TestAGrantCoversOnlyTheYearsItsScopeNames(t *testing.T) {
	from, to := "2025-2026", "2027-2028"

	cases := []struct {
		name     string
		scope    discount.ScopeType
		from, to *string
		year     string
		want     bool
	}{
		{"all years covers anything", discount.ScopeAllYears, nil, nil, "2031-2032", true},
		{"a single year covers itself", discount.ScopeSingleYear, &from, nil, "2025-2026", true},
		{"a single year covers no other", discount.ScopeSingleYear, &from, nil, "2026-2027", false},
		{"a range covers its start", discount.ScopeYearRange, &from, &to, "2025-2026", true},
		{"a range covers its middle", discount.ScopeYearRange, &from, &to, "2026-2027", true},
		{"a range covers its end", discount.ScopeYearRange, &from, &to, "2027-2028", true},
		{"a range covers nothing after it", discount.ScopeYearRange, &from, &to, "2028-2029", false},
		{"a range with no end covers nothing", discount.ScopeYearRange, &from, nil, "2026-2027", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assignment := approved(t, shared.NewID())
			assignment.ScopeType = tc.scope
			assignment.ScopeCodeFrom = tc.from
			assignment.ScopeCodeTo = tc.to

			if got := assignment.CoversYear(tc.year); got != tc.want {
				t.Errorf("CoversYear(%q) = %t, want %t", tc.year, got, tc.want)
			}
		})
	}

	// A grant that is not approved covers nothing, whatever its scope says.
	// Otherwise a draft would reduce a bill.
	pending := draft(t, shared.NewID())
	pending.ScopeType = discount.ScopeAllYears
	if pending.CoversYear("2026-2027") {
		t.Error("a grant nobody approved must cover no year at all")
	}
}

func TestOnlyAnAppliedDiscountTakesAnythingOffTheAccount(t *testing.T) {
	actor := shared.NewID()
	at := time.Now().UTC()

	application := discount.NewApplication(shared.NewID(), discount.Result{
		Candidate:      discount.Candidate{AssignmentID: shared.NewID()},
		FrozenBase:     money.Amount(2_000_000),
		ComputedAmount: money.Amount(500_000),
		AppliedAmount:  money.Amount(500_000),
		Sequence:       1,
	}, discount.ApplicationPending)

	// Pending is recorded and inert. A discount awaiting confirmation that
	// already reduced the bill would be a discount granted by nobody.
	if got := application.EffectiveAmount(); got != 0 {
		t.Errorf("a pending application takes off %s, want nothing", got)
	}

	if err := application.Confirm(actor, at); err != nil {
		t.Fatalf("confirming: %v", err)
	}
	if got := application.EffectiveAmount(); got != money.Amount(500_000) {
		t.Errorf("an applied discount takes off %s, want 500,000", got)
	}
	if err := application.Confirm(actor, at); err == nil {
		t.Error("confirming twice would apply the same discount twice")
	}

	// Reversal keeps the row and stops it counting; the money goes back through
	// a compensating adjustment, not by deleting this.
	if err := application.Reverse(actor, "", at); err == nil {
		t.Error("a reversal with no reason is a discount that vanished unexplained")
	}
	if err := application.Reverse(actor, "granted against the wrong year", at); err != nil {
		t.Fatalf("reversing: %v", err)
	}
	if got := application.EffectiveAmount(); got != 0 {
		t.Errorf("a reversed discount still takes off %s", got)
	}
	if application.ReversalReason == nil {
		t.Error("a reversed application must keep the reason")
	}
}

func TestADeclinedApplicationNeverBecomesApplied(t *testing.T) {
	actor := shared.NewID()
	at := time.Now().UTC()

	application := discount.NewApplication(shared.NewID(), discount.Result{
		Candidate:     discount.Candidate{AssignmentID: shared.NewID()},
		AppliedAmount: money.Amount(250_000),
	}, discount.ApplicationPending)

	if err := application.Decline(actor, at); err != nil {
		t.Fatalf("declining: %v", err)
	}
	if got := application.EffectiveAmount(); got != 0 {
		t.Errorf("a declined application takes off %s", got)
	}
	if err := application.Confirm(actor, at); err == nil {
		t.Error("confirming a declined application would reinstate a discount " +
			"that eligibility was not confirmed for")
	}
	if err := application.Reverse(actor, "changed my mind", at); err == nil {
		t.Error("reversing something that was never applied unwinds money that never moved")
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func draft(t *testing.T, requester shared.ID) *discount.Assignment {
	t.Helper()
	assignment, err := discount.NewAssignment(
		shared.NewID(), shared.NewID(), discount.ScopeAllYears, requester)
	if err != nil {
		t.Fatalf("building an assignment: %v", err)
	}
	return assignment
}

func submitted(t *testing.T, requester shared.ID) *discount.Assignment {
	t.Helper()
	assignment := draft(t, requester)
	if err := assignment.Submit(); err != nil {
		t.Fatalf("submitting: %v", err)
	}
	return assignment
}

func approved(t *testing.T, requester shared.ID) *discount.Assignment {
	t.Helper()
	assignment := submitted(t, requester)
	if err := assignment.Approve(shared.NewID(), time.Now().UTC()); err != nil {
		t.Fatalf("approving: %v", err)
	}
	return assignment
}

func TestADefinitionNeedsACodeAndAnArabicName(t *testing.T) {
	if _, err := discount.NewDefinition("  ", "منحة", discount.Category("merit")); err == nil {
		t.Error("a discount with no code cannot be named on a receipt or in a report")
	}
	if _, err := discount.NewDefinition("MERIT", "   ", discount.Category("merit")); err == nil {
		t.Error("the Arabic name is what the student sees; a blank one is not a discount")
	}

	definition, err := discount.NewDefinition("  merit-1 ", " منحة التفوق ", discount.Category("merit"))
	if err != nil {
		t.Fatalf("building a definition: %v", err)
	}
	// Upper-cased and trimmed, so "merit-1" and "MERIT-1 " are the same code
	// rather than two definitions nobody can tell apart in a report.
	if definition.Code != "MERIT-1" {
		t.Errorf("code = %q, want MERIT-1", definition.Code)
	}
	if definition.NameAr != "منحة التفوق" {
		t.Errorf("name = %q, want it trimmed", definition.NameAr)
	}
	// Both defaults are the cautious direction: a discount is re-confirmed
	// every year unless somebody says otherwise, and the version behind it
	// needs approval.
	if !definition.AnnualReconfirmation {
		t.Error("annual reconfirmation must default on: a grant nobody re-checks " +
			"outlives the eligibility that justified it")
	}
}

func TestAVersionIsFrozenOnceItIsPublished(t *testing.T) {
	actor := shared.NewID()
	at := time.Now().UTC()

	t.Run("a percentage must be a legal rate", func(t *testing.T) {
		if _, err := discount.NewPercentageVersion(shared.NewID(), 1, money.BasisPoints(-1)); err == nil {
			t.Error("a negative rate would add to the bill rather than reduce it")
		}
		if _, err := discount.NewPercentageVersion(shared.NewID(), 1, money.BasisPoints(10_001)); err == nil {
			t.Error("a rate above 100% would pay the student to enrol")
		}
	})

	t.Run("a fixed amount must be positive", func(t *testing.T) {
		if _, err := discount.NewFixedVersion(shared.NewID(), 1, money.Amount(0)); err == nil {
			t.Error("a zero discount is a row that means nothing and reconciles to nothing")
		}
		if _, err := discount.NewFixedVersion(shared.NewID(), 1, money.Amount(-50_000)); err == nil {
			t.Error("a negative fixed discount is a charge wearing a discount's name")
		}
	})

	version, err := discount.NewPercentageVersion(shared.NewID(), 1, money.BasisPoints(2_500))
	if err != nil {
		t.Fatalf("building a version: %v", err)
	}
	if version.Status != discount.VersionDraft {
		t.Errorf("a new version starts %s, want draft", version.Status)
	}
	if !version.RequiresApproval {
		t.Error("a version must require approval by default; the safe direction for " +
			"the absence of a decision is the one that asks for one")
	}

	if err := version.Publish(actor, at); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	if version.PublishedBy == nil || *version.PublishedBy != actor {
		t.Error("a published version must record who published it")
	}

	// Publishing again would move a version that historical applications point
	// at — every application stores the version id it was computed from, and
	// that is what makes a five-year-old discount recomputable.
	if err := version.Publish(actor, at); err == nil {
		t.Error("a published version must not be publishable twice")
	}
	if code := shared.CodeOf(version.Publish(actor, at)); code != "discount.version_not_draft" {
		t.Errorf("code = %q, want discount.version_not_draft", code)
	}
}
