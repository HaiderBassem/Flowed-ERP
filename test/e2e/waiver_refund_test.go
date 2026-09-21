package e2e

import (
	"context"
	"net/http"
	"testing"
)

// Waiving an obligation and then refunding the payment that produced the credit
// must not pay the same dinar out twice.
//
// The shape of the worry: a withdrawal treated as waive_all raises a credit for
// everything already collected, and that credit's SourceReference is the
// enrollment. A refund unwinds credit whose SourceReference is the payment, so
// it cannot see the waiver credit and unwinds the live allocations instead. If
// the waiver credit is still open afterwards, the university has handed over
// cash and is still holding a promise of the same amount.
func TestWaivingThenRefundingDoesNotPayTwice(t *testing.T) {
	admin := adminClient(t)
	s := setupScenario(t, admin)
	ctx := context.Background()

	paid := s.outstanding
	pay := admin.expect(admin.postMoney("/api/v1/payments", map[string]any{
		"account_id": s.accountID, "amount": paid, "payment_method_id": s.methodID,
		"method_reference": "REF-" + unique(),
	}, unique()), http.StatusCreated, "collecting the whole obligation")
	paymentID, _ := pay.nested("payment")["id"].(string)

	// Withdraw, waiving everything: the money is in the drawer, the obligation
	// is gone, so the whole collection becomes credit owed back to the student.
	wd := admin.expect(admin.post("/api/v1/enrollments/"+s.enrollmentID+"/status", map[string]any{
		"status": "withdrawn", "reason": "اختبار", "financial_treatment": "waive_all",
	}), http.StatusOK, "withdrawing with waive_all")
	treatment := wd.nested("financial_treatment")
	creditRaised := int64(treatment["credit_raised"].(float64))
	t.Logf("waive_all raised credit = %d (paid %d)", creditRaised, paid)

	creditQ := `SELECT coalesce(sum(amount - consumed_amount), 0)
	            FROM credit_entry
	            WHERE student_id = $1 AND status IN ('open','partially_consumed')`

	var openCredit int64
	if err := db.Pool().QueryRow(ctx, creditQ, s.studentID).Scan(&openCredit); err != nil {
		t.Fatalf("reading open credit after the waiver: %v", err)
	}
	if openCredit != creditRaised {
		t.Fatalf("expected %d of open credit after the waiver, found %d", creditRaised, openCredit)
	}

	// Now hand the money back in cash against the original payment.
	refund := admin.postMoney("/api/v1/refunds/issue", map[string]any{
		"payment_id": paymentID, "amount": paid,
		"payment_method_id": s.methodID, "reason": "اختبار الاسترجاع",
	}, unique())
	t.Logf("refund of the full payment returned %d %s", refund.status, refund.errorCode())

	if refund.status != http.StatusOK && refund.status != http.StatusCreated {
		// Refusing is a perfectly good answer — it means the two paths cannot
		// both pay out. Record which refusal, so the manual can quote it.
		t.Logf("REFUSED, so no double payout: %s", refund.errorCode())
		return
	}

	if err := db.Pool().QueryRow(ctx, creditQ, s.studentID).Scan(&openCredit); err != nil {
		t.Fatalf("reading open credit after the refund: %v", err)
	}

	t.Logf("after refunding %d in cash, open credit is still %d", paid, openCredit)
	if openCredit > 0 {
		t.Errorf("DOUBLE PAYOUT: the student was refunded %d in cash and the university "+
			"still holds %d of open credit for the same money", paid, openCredit)
	}
}
