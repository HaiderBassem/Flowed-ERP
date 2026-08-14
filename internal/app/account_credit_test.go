package app

import (
	"context"
	"testing"
	"time"

	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/port"
)

// These cover the step that completes a mid-year supersede: money the student
// already handed over must reach the account that replaces the one they left.
// Without it the replacement demands the full fee while the payment sits in a
// credit row nobody touches, and the student is asked to pay twice for one
// year.
//
// The fakes are deliberately narrow — only the repository methods
// applyCarriedCredit actually calls — so the test says what the function
// depends on rather than what the whole service does.

type creditTestAccounts struct {
	// Embedded so the fake satisfies the interface without implementing all of
	// it. Any method the code under test calls but the fake does not define
	// panics with a nil dereference, which names the missing dependency
	// immediately rather than letting a silent zero value pass.
	port.AccountRepository

	accounts      map[shared.ID]*billing.Account
	credits       map[shared.ID]*billing.CreditEntry
	openByStudent map[shared.ID][]*billing.CreditEntry
	adjustments   []*billing.Adjustment
	consumptions  []*billing.CreditConsumption
	lockedOrder   []shared.ID
}

func (f *creditTestAccounts) GetForUpdate(_ context.Context, id shared.ID) (*billing.Account, error) {
	f.lockedOrder = append(f.lockedOrder, id)
	account, ok := f.accounts[id]
	if !ok {
		return nil, shared.NotFound("account.not_found", "no account %s", id)
	}
	return account, nil
}

func (f *creditTestAccounts) GetCreditForUpdate(_ context.Context, id shared.ID) (*billing.CreditEntry, error) {
	credit, ok := f.credits[id]
	if !ok {
		return nil, shared.NotFound("credit.not_found", "no credit %s", id)
	}
	return credit, nil
}

func (f *creditTestAccounts) ListOpenCredits(_ context.Context, studentID shared.ID) ([]*billing.CreditEntry, error) {
	return f.openByStudent[studentID], nil
}

func (f *creditTestAccounts) UpdateCredit(context.Context, *billing.CreditEntry) error { return nil }
func (f *creditTestAccounts) Update(context.Context, *billing.Account) error           { return nil }

func (f *creditTestAccounts) CreateAdjustment(_ context.Context, a *billing.Adjustment) error {
	f.adjustments = append(f.adjustments, a)
	return nil
}

func (f *creditTestAccounts) RecordCreditConsumption(_ context.Context, c *billing.CreditConsumption) error {
	f.consumptions = append(f.consumptions, c)
	return nil
}

type creditTestInstallments struct {
	port.InstallmentRepository
	updated []*billing.Installment
}

func (f *creditTestInstallments) Update(_ context.Context, i *billing.Installment) error {
	f.updated = append(f.updated, i)
	return nil
}

// creditTestFixture wires the narrow fakes into a service. It builds the
// situation a supersede leaves behind: a retired account holding credit, and a
// freshly generated one that owes money.
type creditTestFixture struct {
	service      *AccountService
	accounts     *creditTestAccounts
	installments *creditTestInstallments
	newAccount   *billing.Account
	oldAccount   *billing.Account
	plan         []*billing.Installment
	studentID    shared.ID
}

func newCreditTestFixture(t *testing.T, owed, creditAmount money.Amount) *creditTestFixture {
	t.Helper()

	studentID := shared.NewID()
	oldAccount := &billing.Account{
		ID: shared.NewID(), StudentID: studentID,
		Status: billing.AccountCancelled, CreditBalance: creditAmount,
	}
	newAccount := &billing.Account{
		ID: shared.NewID(), StudentID: studentID, AcademicYearID: shared.NewID(),
		NetTotal: owed, Status: billing.AccountActive,
	}

	credit := &billing.CreditEntry{
		ID: shared.NewID(), AccountID: oldAccount.ID, StudentID: studentID,
		Amount: creditAmount, Source: billing.CreditFromTransfer, Status: billing.CreditOpen,
	}

	accounts := &creditTestAccounts{
		accounts: map[shared.ID]*billing.Account{
			oldAccount.ID: oldAccount,
			newAccount.ID: newAccount,
		},
		credits:       map[shared.ID]*billing.CreditEntry{credit.ID: credit},
		openByStudent: map[shared.ID][]*billing.CreditEntry{studentID: {credit}},
	}
	installments := &creditTestInstallments{}

	// Two equal installments covering the amount owed.
	half := owed / 2
	plan := []*billing.Installment{
		{ID: shared.NewID(), AccountID: newAccount.ID, Number: 1, Amount: half,
			Status: billing.InstallmentPending, DueDate: shared.NewDate(2025, time.September, 1)},
		{ID: shared.NewID(), AccountID: newAccount.ID, Number: 2, Amount: owed - half,
			Status: billing.InstallmentPending, DueDate: shared.NewDate(2025, time.December, 1)},
	}

	service := &AccountService{deps: Deps{
		Accounts:     accounts,
		Installments: installments,
		Clock:        shared.FixedClock{Instant: time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)},
	}}

	return &creditTestFixture{
		service: service, accounts: accounts, installments: installments,
		newAccount: newAccount, oldAccount: oldAccount, plan: plan, studentID: studentID,
	}
}

func TestCarriedCreditSettlesTheReplacementAccount(t *testing.T) {
	// The student paid 1,800,000 into a seat they left; the replacement costs
	// only 100,000, so the credit covers it outright.
	f := newCreditTestFixture(t, 100_000, 1_800_000)
	actor := shared.Actor{UserID: shared.NewID()}

	carried, err := f.service.applyCarriedCredit(
		context.Background(), actor, f.newAccount, f.plan, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	if carried != 100_000 {
		t.Errorf("carried %s, want 100,000 — only what was owed", carried)
	}
	if got := f.newAccount.Remaining(); got != 0 {
		t.Errorf("the replacement still owes %s; the credit should have settled it", got)
	}
	if f.newAccount.AdjustmentTotal != -100_000 {
		t.Errorf("adjustment total = %s, want -100,000", f.newAccount.AdjustmentTotal)
	}

	// The frozen net is untouched. What changed is recorded beside it, so the
	// account still shows the fee it was charged.
	if f.newAccount.NetTotal != 100_000 {
		t.Errorf("the frozen net was rewritten to %s; it must stay at 100,000", f.newAccount.NetTotal)
	}

	// The source account's cached balance drops by what was spent. Leaving it
	// stale is what makes the nightly reconciliation report drift and blocks
	// the year from closing.
	if f.oldAccount.CreditBalance != 1_700_000 {
		t.Errorf("source credit balance = %s, want 1,700,000 after spending 100,000",
			f.oldAccount.CreditBalance)
	}
}

func TestCarriedCreditTakesOnlyWhatIsOwed(t *testing.T) {
	// A credit larger than the new fee must not overpay the account into a
	// negative obligation.
	f := newCreditTestFixture(t, 400_000, 2_000_000)
	actor := shared.Actor{UserID: shared.NewID()}

	carried, err := f.service.applyCarriedCredit(
		context.Background(), actor, f.newAccount, f.plan, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if carried != 400_000 {
		t.Errorf("carried %s, want exactly the 400,000 owed", carried)
	}
	if f.newAccount.EffectiveNet().IsNegative() {
		t.Errorf("effective net went negative (%s): the university would owe the student for enrolling",
			f.newAccount.EffectiveNet())
	}
	if len(f.accounts.consumptions) != 1 {
		t.Fatalf("expected one credit consumption, got %d", len(f.accounts.consumptions))
	}
	if f.accounts.consumptions[0].Purpose != billing.CreditForCarryForward {
		t.Errorf("consumption purpose = %s, want carry_forward", f.accounts.consumptions[0].Purpose)
	}
}

func TestPartialCreditLeavesTheRestOwed(t *testing.T) {
	// 300,000 of credit against a 1,000,000 fee settles part of it and no more.
	f := newCreditTestFixture(t, 1_000_000, 300_000)
	actor := shared.Actor{UserID: shared.NewID()}

	carried, err := f.service.applyCarriedCredit(
		context.Background(), actor, f.newAccount, f.plan, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if carried != 300_000 {
		t.Errorf("carried %s, want 300,000", carried)
	}
	if got := f.newAccount.Remaining(); got != 700_000 {
		t.Errorf("remaining = %s, want 700,000", got)
	}

	// The plan shrinks from the far end, so the obligations coming up soonest
	// are the last to disappear.
	if len(f.installments.updated) == 0 {
		t.Fatal("the installment plan was not reduced")
	}
	var planTotal money.Amount
	for _, inst := range f.plan {
		if inst.Status != billing.InstallmentWaived {
			planTotal = planTotal.MustAdd(inst.Amount)
		}
	}
	if planTotal != 700_000 {
		t.Errorf("live plan totals %s, want 700,000 to match what is still owed", planTotal)
	}
	if f.plan[0].Status == billing.InstallmentWaived {
		t.Error("the nearest installment was waived first; reduction must start from the far end")
	}
}

func TestNoCreditLeavesTheAccountAlone(t *testing.T) {
	f := newCreditTestFixture(t, 1_000_000, 0)
	f.accounts.openByStudent[f.studentID] = nil
	actor := shared.Actor{UserID: shared.NewID()}

	carried, err := f.service.applyCarriedCredit(
		context.Background(), actor, f.newAccount, f.plan, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if carried != 0 {
		t.Errorf("carried %s with no credit available", carried)
	}
	if f.newAccount.AdjustmentTotal != 0 {
		t.Error("an adjustment was posted for nothing")
	}
	if len(f.accounts.adjustments) != 0 {
		t.Errorf("%d adjustment row(s) written when no credit was spent", len(f.accounts.adjustments))
	}
}

// A student's own overpayment on the account being generated is already part
// of that account's balance; spending it again as "carried" credit would
// discount the fee twice.
func TestCreditOnTheSameAccountIsNotCarried(t *testing.T) {
	f := newCreditTestFixture(t, 500_000, 400_000)
	ownCredit := f.accounts.openByStudent[f.studentID][0]
	ownCredit.AccountID = f.newAccount.ID
	actor := shared.Actor{UserID: shared.NewID()}

	carried, err := f.service.applyCarriedCredit(
		context.Background(), actor, f.newAccount, f.plan, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if carried != 0 {
		t.Errorf("carried %s from the account's own credit; that would discount the fee twice", carried)
	}
}
