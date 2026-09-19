// Integration tests for the reporting repository.
//
// These run against a real PostgreSQL instance, deliberately. Every report in
// this package is a hand-written query over ten views, and the thing that goes
// wrong with such a query is never a type error in Go — it is a column that
// does not exist, a grouping set that nests wrongly, or a join that counts a
// superseded student twice. None of that is visible without a database.
//
// Run with one available:
//
//	DB_NAME=flowed_dev go test ./internal/adapter/postgres/
//
// Skipped automatically under -short.
package postgres

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

var reportDB *pg.DB

func TestMain(m *testing.M) {
	// testing.Short reads a flag, so the flags must be parsed before it is
	// consulted from TestMain.
	flag.Parse()
	if testing.Short() {
		os.Exit(0)
	}

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
			envOr("DB_USER", os.Getenv("USER")),
			os.Getenv("DB_PASSWORD"),
			envOr("DB_HOST", "localhost"),
			envOr("DB_PORT", "5432"),
			envOr("DB_NAME", "flowed_dev"),
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "repository tests: cannot create pool: %v\n", err)
		os.Exit(1)
	}
	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr,
			"repository tests: cannot reach the database (%v).\n"+
				"Run `make db-create migrate-up`, or use -short to skip.\n", err)
		os.Exit(1)
	}

	reportDB = pg.NewDB(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// errRollbackFixture ends every fixture transaction. Returning an error is how
// a caller of WithTx asks for a rollback, and rolling back is what keeps one
// test's students from turning up in another test's head count.
var errRollbackFixture = errors.New("rolling back the fixture")

// Reference data the migrations seed. Tests reuse it rather than inserting
// their own, because the codes are unique and a test that minted a second
// MORNING would collide with every other test doing the same.
const (
	studyTypeMorning = "a1000000-0000-4000-8000-000000000001"
	studyTypeEvening = "a1000000-0000-4000-8000-000000000002"
	categoryRegular  = "a2000000-0000-4000-8000-000000000001"
	categoryRepeat   = "a2000000-0000-4000-8000-000000000002"
	methodCash       = "a3000000-0000-4000-8000-000000000001"
)

type fixture struct {
	t   *testing.T
	ctx context.Context

	collegeID    shared.ID
	departmentID shared.ID
	yearID       shared.ID
	studentID    shared.ID
	cashierID    shared.ID
	approverID   shared.ID

	morning  shared.ID
	evening  shared.ID
	regular  shared.ID
	repeated shared.ID
	cash     shared.ID
}

// withFixture runs the body against seeded rows inside a transaction that is
// always rolled back, however the body ends.
func withFixture(t *testing.T, body func(f *fixture)) {
	t.Helper()
	err := reportDB.WithTx(context.Background(), pg.DefaultTxOptions(), func(ctx context.Context) error {
		// The body runs on its own goroutine because t.Fatalf unwinds by
		// terminating the goroutine that called it. On the test's own goroutine
		// that would skip this closure's return, leaving the transaction open
		// and its connection checked out until the pool blocked on shutdown.
		done := make(chan struct{})
		go func() {
			defer close(done)
			body(newFixture(t, ctx))
		}()
		<-done
		return errRollbackFixture
	})
	if err != nil && !errors.Is(err, errRollbackFixture) {
		t.Fatalf("fixture transaction: %v", err)
	}
}

func newFixture(t *testing.T, ctx context.Context) *fixture {
	t.Helper()
	f := &fixture{
		t:        t,
		ctx:      ctx,
		morning:  mustID(t, studyTypeMorning),
		evening:  mustID(t, studyTypeEvening),
		regular:  mustID(t, categoryRegular),
		repeated: mustID(t, categoryRepeat),
		cash:     mustID(t, methodCash),
	}

	suffix := time.Now().UnixNano()

	f.collegeID = f.scan(`
		INSERT INTO college (id, code, name_ar) VALUES (gen_random_uuid(), $1, 'كلية التقارير')
		RETURNING id`, fmt.Sprintf("RC%d", suffix%1000000))
	f.departmentID = f.scan(`
		INSERT INTO department (id, college_id, code, name_ar, stage_count)
		VALUES (gen_random_uuid(), $1, $2, 'قسم التقارير', 4)
		RETURNING id`, f.collegeID, fmt.Sprintf("RD%d", suffix%1000000))
	f.yearID = f.scan(`
		INSERT INTO academic_year (id, code, start_date, end_date, status)
		VALUES (gen_random_uuid(), $1, '2025-09-01', '2026-07-01', 'open')
		RETURNING id`, f.nextYearCode())
	f.studentID = f.scan(`
		INSERT INTO student (id, student_no, full_name, mother_name, phone)
		VALUES (gen_random_uuid(), $1, 'علي محمد حسن', 'زينب', '07701234567')
		RETURNING id`, fmt.Sprintf("RS%d", suffix))
	f.cashierID = f.scan(`
		INSERT INTO app_user (id, username, full_name, password_hash)
		VALUES (gen_random_uuid(), $1, 'صراف الاختبار', 'x') RETURNING id`,
		fmt.Sprintf("rcashier%d", suffix))
	f.approverID = f.scan(`
		INSERT INTO app_user (id, username, full_name, password_hash)
		VALUES (gen_random_uuid(), $1, 'مدير مالي', 'x') RETURNING id`,
		fmt.Sprintf("rfinance%d", suffix))

	return f
}

// nextYearCode picks a year code nobody has taken.
//
// From the codes already in the database rather than from the clock: the rows a
// previous run created are still there, and on darwin UnixNano always ends in
// three zeros, so a code derived from the timestamp was the same string every
// run. One allocator over every four-digit code rather than a per-package
// family, because a family runs out — at 4999 the next code was 5000, which was
// another package's.
func (f *fixture) nextYearCode() string {
	var highest int
	err := reportDB.Conn(f.ctx).QueryRow(f.ctx, `
		SELECT coalesce(max(left(code, 4)::int), 2999)
		FROM academic_year WHERE code ~ '^[0-9]{4}-[0-9]{4}$'`).Scan(&highest)
	if err != nil || highest < 2999 {
		highest = 2999
	}
	return fmt.Sprintf("%d-%d", highest+1, highest+2)
}

func (f *fixture) scan(sql string, args ...any) shared.ID {
	f.t.Helper()
	var id shared.ID
	if err := reportDB.Conn(f.ctx).QueryRow(f.ctx, sql, args...).Scan(&id); err != nil {
		f.t.Fatalf("fixture insert failed: %v\nSQL: %s", err, sql)
	}
	return id
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := reportDB.Conn(f.ctx).Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("fixture statement failed: %v\nSQL: %s", err, sql)
	}
}

// enroll writes one registration. The caller supplies the sequence number
// because a supersede pair occupies two of them.
func (f *fixture) enroll(sequenceNo, stage, attempt int16, studyType shared.ID, supersedes *shared.ID) shared.ID {
	f.t.Helper()
	category := f.regular
	if attempt > 1 {
		category = f.repeated
	}
	return f.scan(`
		INSERT INTO enrollment (
			id, student_id, academic_year_id, sequence_no,
			college_id, department_id, study_type_id, student_category_id,
			stage, attempt_number, enrollment_status, academic_result, supersedes_id)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, 'active', 'pending', $10)
		RETURNING id`,
		f.studentID, f.yearID, sequenceNo,
		f.collegeID, f.departmentID, studyType, category,
		stage, attempt, supersedes)
}

func (f *fixture) supersede(enrollmentID shared.ID) {
	f.t.Helper()
	f.exec(`
		UPDATE enrollment
		SET enrollment_status = 'superseded', academic_result = 'not_applicable',
		    supersede_reason = 'changed study type', supersede_date = current_date
		WHERE id = $1`, enrollmentID)
}

// account opens the financial position for an enrollment, copying the
// enrollment's own dimensions onto it as account generation does.
func (f *fixture) account(enrollmentID, studyType shared.ID, gross, discount int64) shared.ID {
	f.t.Helper()
	return f.scan(`
		INSERT INTO financial_account (
			id, enrollment_id, academic_year_id, student_id,
			college_id, department_id, study_type_id, stage,
			gross_total, discountable_base, discount_total, net_total, status)
		SELECT gen_random_uuid(), e.id, e.academic_year_id, e.student_id,
		       e.college_id, e.department_id, $2, e.stage,
		       $3::bigint, $3::bigint, $4::bigint, $3::bigint - $4::bigint, 'active'
		FROM enrollment e WHERE e.id = $1
		RETURNING id`, enrollmentID, studyType, gross, discount)
}

func (f *fixture) installment(accountID shared.ID, number int16, due time.Time, amount int64) shared.ID {
	f.t.Helper()
	return f.scan(`
		INSERT INTO installment (id, account_id, installment_no, due_date, amount)
		VALUES (gen_random_uuid(), $1, $2, $3, $4)
		RETURNING id`, accountID, number, due, amount)
}

// pay posts a collection. postedAt and voidedAt are separate so the void
// register has a time gap to sort on.
func (f *fixture) pay(accountID, enrollmentID shared.ID, amount int64, postedAt time.Time) shared.ID {
	f.t.Helper()
	return f.scan(`
		INSERT INTO payment (
			id, receipt_no, account_id, student_id, enrollment_id, posting_year_id,
			amount, payment_method_id, cashier_user_id, status, paid_at, posted_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, 'posted', $9, $9)
		RETURNING id`,
		fmt.Sprintf("RCT-%d", time.Now().UnixNano()%100000000),
		accountID, f.studentID, enrollmentID, f.yearID, amount, f.cash, f.cashierID, postedAt)
}

func (f *fixture) allocate(paymentID, installmentID shared.ID, amount int64) {
	f.t.Helper()
	f.exec(`
		INSERT INTO payment_allocation (id, payment_id, installment_id, amount, entry_type)
		VALUES (gen_random_uuid(), $1, $2, $3, 'allocation')`, paymentID, installmentID, amount)
}

func (f *fixture) void(paymentID shared.ID, voidedAt time.Time, reason string) {
	f.t.Helper()
	requestID := f.scan(`
		INSERT INTO void_request (id, payment_id, reason, status, requested_by, requested_at, executed_by, executed_at)
		VALUES (gen_random_uuid(), $1, $2, 'executed', $3, $4, $5, $4)
		RETURNING id`, paymentID, reason, f.cashierID, voidedAt, f.approverID)
	f.exec(`
		UPDATE payment
		SET status = 'voided', voided_at = $2, voided_by = $3, void_reason = $4, void_request_id = $5
		WHERE id = $1`, paymentID, voidedAt, f.approverID, reason, requestID)
}

// grantDiscount writes the definition, its published version, an approved
// assignment and the application frozen onto the account — the whole chain the
// discount report reads, because it reads applications and nothing else.
func (f *fixture) grantDiscount(accountID shared.ID, code string, valueBP int, base, applied int64) shared.ID {
	f.t.Helper()
	definitionID := f.scan(`
		INSERT INTO discount_definition (id, code, name_ar, category, is_full_exemption)
		VALUES (gen_random_uuid(), $1, 'خصم اختبار', 'social', $2)
		RETURNING id`, code, applied >= base)
	versionID := f.scan(`
		INSERT INTO discount_definition_version (
			id, definition_id, version_no, value_type, value_bp, status, published_at, published_by)
		VALUES (gen_random_uuid(), $1, 1, 'percentage', $2, 'published', now(), $3)
		RETURNING id`, definitionID, valueBP, f.approverID)
	assignmentID := f.scan(`
		INSERT INTO discount_assignment (
			id, student_id, definition_id, scope_type, status,
			requested_by, requested_at, approved_by, approved_at)
		VALUES (gen_random_uuid(), $1, $2, 'all_years', 'approved', $3, now(), $4, now())
		RETURNING id`, f.studentID, definitionID, f.cashierID, f.approverID)
	return f.scan(`
		INSERT INTO discount_application (
			id, account_id, assignment_id, definition_version_id,
			frozen_base_amount, computed_amount, applied_amount, status, applied_at, applied_by)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $5, 'applied', now(), $6)
		RETURNING id`, accountID, assignmentID, versionID, base, applied, f.approverID)
}

func mustID(t *testing.T, s string) shared.ID {
	t.Helper()
	id, err := shared.ParseID(s)
	if err != nil {
		t.Fatalf("parsing fixture id %q: %v", s, err)
	}
	return id
}

func daysFromNow(days int) time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, days)
}

// ---------------------------------------------------------------------------
// Debt
// ---------------------------------------------------------------------------

func TestDebtReportFindsAnAccountWithABalance(t *testing.T) {
	withFixture(t, func(f *fixture) {
		repo := NewReportRepository(reportDB)

		enrollmentID := f.enroll(1, 1, 1, f.evening, nil)
		accountID := f.account(enrollmentID, f.evening, 1_000_000, 0)
		f.installment(accountID, 1, daysFromNow(-45), 600_000)
		upcoming := f.installment(accountID, 2, daysFromNow(30), 400_000)

		paymentID := f.pay(accountID, enrollmentID, 400_000, time.Now().UTC())
		f.allocate(paymentID, upcoming, 400_000)

		rows, total, err := repo.DebtReport(f.ctx, port.DebtFilter{AcademicYearID: &f.yearID})
		if err != nil {
			t.Fatalf("DebtReport: %v", err)
		}
		if total != 1 || len(rows) != 1 {
			t.Fatalf("DebtReport returned %d row(s), total %d; want exactly the one account still owing", len(rows), total)
		}

		row := rows[0]
		if row.AccountID != accountID {
			t.Errorf("account_id = %s, want %s", row.AccountID, accountID)
		}
		if row.EffectiveNet != money.FromInt64(1_000_000) {
			t.Errorf("effective_net = %d, want 1,000,000", row.EffectiveNet)
		}
		if row.NetPaid != money.FromInt64(400_000) {
			t.Errorf("net_paid = %d, want 400,000", row.NetPaid)
		}
		if row.Remaining != money.FromInt64(600_000) {
			t.Errorf("remaining = %d, want 600,000", row.Remaining)
		}
		// Overdue is derived from today against the due date, never read from a
		// stored flag, so the unpaid first installment must show up unprompted.
		if row.OverdueAmount != money.FromInt64(600_000) || row.OverdueCount != 1 {
			t.Errorf("overdue = %d over %d installment(s), want 600,000 over 1",
				row.OverdueAmount, row.OverdueCount)
		}
		if row.OldestOverdueDue == nil {
			t.Error("oldest overdue due date is missing; the collection list cannot be worked without it")
		} else if got, want := row.OldestOverdueDue.String(), shared.DateFromTime(daysFromNow(-45)).String(); got != want {
			t.Errorf("oldest overdue due date = %s, want %s", got, want)
		}
		if row.MotherName == "" || row.Phone == nil {
			t.Error("the row carries no mother's name or phone; a collection list nobody can act on")
		}

	})
}

func TestDebtReportRefusesToScanEveryYearAtOnce(t *testing.T) {
	withFixture(t, func(f *fixture) {
		repo := NewReportRepository(reportDB)

		_, _, err := repo.DebtReport(f.ctx, port.DebtFilter{})
		if err == nil {
			t.Fatal("an unbounded debt report must be refused")
		}
		if shared.KindOf(err) != shared.KindValidation {
			t.Errorf("error kind = %v, want a validation error naming the missing filter", shared.KindOf(err))
		}
	})
}

// ---------------------------------------------------------------------------
// Counts against money
// ---------------------------------------------------------------------------

// The rule the whole reporting model turns on: a student who was superseded
// mid-year is one head, but both of their accounts still hold money.
func TestDepartmentSummaryCountsASupersededStudentOnceAndKeepsBothAccounts(t *testing.T) {
	withFixture(t, func(f *fixture) {
		repo := NewReportRepository(reportDB)

		first := f.enroll(1, 1, 1, f.evening, nil)
		f.account(first, f.evening, 1_000_000, 0)
		f.supersede(first)

		second := f.enroll(2, 1, 1, f.morning, &first)
		f.account(second, f.morning, 1_200_000, 0)

		colleges, err := repo.DepartmentSummary(f.ctx, port.SummaryFilter{AcademicYearID: &f.yearID})
		if err != nil {
			t.Fatalf("DepartmentSummary: %v", err)
		}
		if len(colleges) != 1 {
			t.Fatalf("got %d college block(s), want 1", len(colleges))
		}
		college := colleges[0]
		if len(college.Departments) != 1 {
			t.Fatalf("got %d department line(s), want 1", len(college.Departments))
		}
		department := college.Departments[0]

		if department.StudentCount != 1 {
			t.Errorf("student_count = %d, want 1: the superseded enrollment is the same person",
				department.StudentCount)
		}
		if department.AccountCount != 2 {
			t.Errorf("account_count = %d, want 2: both accounts exist and one of them may hold cash",
				department.AccountCount)
		}
		if department.GrossTotal != money.FromInt64(2_200_000) {
			t.Errorf("gross = %d, want 2,200,000: money comes from every non-cancelled account, "+
				"including the superseded one", department.GrossTotal)
		}
		if college.StudentCount != department.StudentCount || college.GrossTotal != department.GrossTotal {
			t.Errorf("the college subtotal (%d students, %d gross) disagrees with its only department "+
				"(%d students, %d gross)",
				college.StudentCount, college.GrossTotal, department.StudentCount, department.GrossTotal)
		}
	})
}

// The stage summary adds the repeat split on top of the same rule.
func TestStageSummaryBreaksOutRepeatStudents(t *testing.T) {
	withFixture(t, func(f *fixture) {
		repo := NewReportRepository(reportDB)

		enrollmentID := f.enroll(1, 2, 2, f.evening, nil)
		f.account(enrollmentID, f.evening, 1_500_000, 0)

		stages, err := repo.StageSummary(f.ctx, port.SummaryFilter{AcademicYearID: &f.yearID})
		if err != nil {
			t.Fatalf("StageSummary: %v", err)
		}
		if len(stages) != 1 {
			t.Fatalf("got %d stage line(s), want 1", len(stages))
		}
		if stages[0].Stage != 2 {
			t.Errorf("stage = %d, want 2", stages[0].Stage)
		}
		if stages[0].RepeatStudentCount != 1 {
			t.Errorf("repeat_student_count = %d, want 1", stages[0].RepeatStudentCount)
		}
		if stages[0].RepeatEffectiveNet != money.FromInt64(1_500_000) {
			t.Errorf("repeat_effective_net = %d, want 1,500,000: repeat fees are priced differently "+
				"and must be visible on their own", stages[0].RepeatEffectiveNet)
		}
	})
}

// ---------------------------------------------------------------------------
// Discounts
// ---------------------------------------------------------------------------

func TestDiscountReportSumsAppliedAmounts(t *testing.T) {
	withFixture(t, func(f *fixture) {
		repo := NewReportRepository(reportDB)

		enrollmentID := f.enroll(1, 1, 1, f.evening, nil)
		accountID := f.account(enrollmentID, f.evening, 1_000_000, 250_000)
		f.grantDiscount(accountID, fmt.Sprintf("RPT%d", time.Now().UnixNano()%1000000), 2500, 1_000_000, 250_000)

		rows, err := repo.DiscountReport(f.ctx, port.DiscountFilter{AcademicYearID: &f.yearID})
		if err != nil {
			t.Fatalf("DiscountReport: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d discount line(s), want 1", len(rows))
		}
		row := rows[0]

		// The applied amount is what was forgone. Recomputing 25% of the base
		// would give the same answer today and a different one after the
		// definition is revised, which is exactly why the report never does it.
		if row.TotalDiscount != money.FromInt64(250_000) {
			t.Errorf("total_discount = %d, want 250,000", row.TotalDiscount)
		}
		if row.StudentCount != 1 || row.ApplicationCount != 1 {
			t.Errorf("counted %d student(s) over %d application(s), want 1 and 1",
				row.StudentCount, row.ApplicationCount)
		}
		if row.VersionNo != 1 {
			t.Errorf("version_no = %d, want 1: the report reports the version that was applied", row.VersionNo)
		}
		if row.PctOfGross == nil || *row.PctOfGross != 25 {
			t.Errorf("pct_of_gross = %v, want 25", row.PctOfGross)
		}
	})
}

// ---------------------------------------------------------------------------
// Void register
// ---------------------------------------------------------------------------

// The register exists to surface the late reversal, so the late one has to come
// first however the rows were written.
func TestVoidRegisterLeadsWithTheWidestTimeGap(t *testing.T) {
	withFixture(t, func(f *fixture) {
		repo := NewReportRepository(reportDB)

		enrollmentID := f.enroll(1, 1, 1, f.evening, nil)
		accountID := f.account(enrollmentID, f.evening, 2_000_000, 0)

		prompt := f.pay(accountID, enrollmentID, 300_000, time.Now().UTC().Add(-2*time.Hour))
		f.void(prompt, time.Now().UTC().Add(-2*time.Hour).Add(10*time.Minute), "wrong installment")

		late := f.pay(accountID, enrollmentID, 500_000, time.Now().UTC().Add(-50*time.Hour))
		f.void(late, time.Now().UTC().Add(-20*time.Hour), "student says they never paid")

		rows, total, err := repo.VoidRegister(f.ctx, port.RegisterFilter{AcademicYearID: &f.yearID})
		if err != nil {
			t.Fatalf("VoidRegister: %v", err)
		}
		if total != 2 || len(rows) != 2 {
			t.Fatalf("got %d void(s) of %d, want 2", len(rows), total)
		}
		if rows[0].PaymentID != late {
			t.Error("the reversal raised thirty hours after the receipt must lead the register; " +
				"it is the one worth looking at")
		}
		if rows[0].GapHours < 29 || rows[0].GapHours > 31 {
			t.Errorf("gap_hours = %v, want about 30", rows[0].GapHours)
		}
		if !rows[0].CrossedDay {
			t.Error("a reversal that crossed a day boundary must be flagged: the drawer was already counted, " +
				"so it should have been a refund")
		}
		if rows[1].CrossedDay {
			t.Error("a same-day correction must not be flagged as a cross-day reversal")
		}
		if rows[0].RequestedBy == nil || rows[0].ExecutedBy == nil {
			t.Error("both signatures must appear; a void with one name on it is not a two-person act")
		}
	})
}

// ---------------------------------------------------------------------------
// Student statement
// ---------------------------------------------------------------------------

func TestStudentStatementIsBuiltFromRawRows(t *testing.T) {
	withFixture(t, func(f *fixture) {
		repo := NewReportRepository(reportDB)

		enrollmentID := f.enroll(1, 1, 1, f.evening, nil)
		accountID := f.account(enrollmentID, f.evening, 1_000_000, 100_000)
		f.exec(`
			INSERT INTO fee_snapshot_line (id, account_id, component_code, name_ar, amount, is_discountable, is_refundable)
			VALUES (gen_random_uuid(), $1, 'TUITION', 'القسط الدراسي', 1000000, true, true)`, accountID)
		f.grantDiscount(accountID, fmt.Sprintf("STM%d", time.Now().UnixNano()%1000000), 1000, 1_000_000, 100_000)
		f.exec(`
			INSERT INTO account_adjustment (id, account_id, adjustment_type, amount, reason, posted_by, posting_year_id)
			VALUES (gen_random_uuid(), $1, 'correction', -50000, 'ministry circular', $2, $3)`,
			accountID, f.approverID, f.yearID)

		first := f.installment(accountID, 1, daysFromNow(-10), 450_000)
		f.installment(accountID, 2, daysFromNow(60), 400_000)

		paid := f.pay(accountID, enrollmentID, 450_000, time.Now().UTC().Add(-time.Hour))
		f.allocate(paid, first, 450_000)

		voided := f.pay(accountID, enrollmentID, 25_000, time.Now().UTC().Add(-3*time.Hour))
		f.void(voided, time.Now().UTC().Add(-2*time.Hour), "keyed twice")

		f.exec(`
			INSERT INTO refund (
				id, refund_no, payment_id, account_id, student_id, posting_year_id,
				amount, payment_method_id, reason, status, requested_by, approved_by, approved_at,
				posted_at, posted_by)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 50000, $6, 'overpaid', 'posted',
			        $7, $8, now(), now(), $8)`,
			fmt.Sprintf("RF-%d", time.Now().UnixNano()%100000000),
			paid, accountID, f.studentID, f.yearID, f.cash, f.cashierID, f.approverID)

		statement, err := repo.StudentStatement(f.ctx, f.studentID, nil)
		if err != nil {
			t.Fatalf("StudentStatement: %v", err)
		}
		if statement.Student.StudentNo == "" {
			t.Error("the statement carries no student number")
		}
		if len(statement.Accounts) != 1 {
			t.Fatalf("got %d account block(s), want 1", len(statement.Accounts))
		}
		account := statement.Accounts[0]

		for name, count := range map[string]int{
			"fee components": len(account.FeeComponents),
			"discounts":      len(account.Discounts),
			"adjustments":    len(account.Adjustments),
			"installments":   len(account.Installments),
			"payments":       len(account.Payments),
			"refunds":        len(account.Refunds),
		} {
			if count == 0 {
				t.Errorf("the %s block is empty; the statement has to explain every figure it prints", name)
			}
		}

		// A voided receipt stays on the statement: the student is holding the
		// paper and the document has to account for it.
		var voidedSeen bool
		for _, p := range account.Payments {
			if p.PaymentID == voided {
				voidedSeen = p.Status == "voided" && p.VoidedAt != nil
			}
		}
		if !voidedSeen {
			t.Error("the voided receipt is missing or unstamped")
		}

		// net 900,000 frozen, less a 50,000 correction, against 450,000 collected
		// and 50,000 returned.
		if account.EffectiveNet != money.FromInt64(850_000) {
			t.Errorf("effective_net = %d, want 850,000 (900,000 frozen less the 50,000 adjustment)",
				account.EffectiveNet)
		}
		if account.NetPaid != money.FromInt64(400_000) {
			t.Errorf("net_paid = %d, want 400,000 (450,000 collected less the 50,000 refunded)", account.NetPaid)
		}
		if account.Remaining != money.FromInt64(450_000) {
			t.Errorf("remaining = %d, want 450,000", account.Remaining)
		}
		if statement.Totals.Remaining != account.Remaining || statement.Totals.AccountCount != 1 {
			t.Errorf("the totals block (%d over %d account(s)) disagrees with the only account block (%d)",
				statement.Totals.Remaining, statement.Totals.AccountCount, account.Remaining)
		}

		// A statement for a year the student never enrolled in is empty, not an
		// error: a clerk browsing years should not be shown a failure.
		other := shared.NewID()
		narrowed, err := repo.StudentStatement(f.ctx, f.studentID, &other)
		if err != nil {
			t.Fatalf("StudentStatement for an unrelated year: %v", err)
		}
		if len(narrowed.Accounts) != 0 {
			t.Errorf("got %d account(s) for an unrelated year, want none", len(narrowed.Accounts))
		}
	})
}

// ---------------------------------------------------------------------------
// Every query, against the real schema
// ---------------------------------------------------------------------------

// A report is a hand-written query over views whose column names Go cannot
// check. This exercises all fifteen against a populated year so that a renamed
// column fails here rather than in front of a finance manager.
func TestEveryReportRunsAgainstTheSchema(t *testing.T) {
	withFixture(t, func(f *fixture) {
		repo := NewReportRepository(reportDB)

		enrollmentID := f.enroll(1, 1, 1, f.evening, nil)
		accountID := f.account(enrollmentID, f.evening, 1_000_000, 100_000)
		f.grantDiscount(accountID, fmt.Sprintf("ALL%d", time.Now().UnixNano()%1000000), 10000, 1_000_000, 1_000_000)
		overdue := f.installment(accountID, 1, daysFromNow(-200), 500_000)
		f.installment(accountID, 2, daysFromNow(45), 400_000)
		paid := f.pay(accountID, enrollmentID, 200_000, time.Now().UTC())
		f.allocate(paid, overdue, 200_000)

		voided := f.pay(accountID, enrollmentID, 10_000, time.Now().UTC().Add(-5*time.Hour))
		f.void(voided, time.Now().UTC(), "duplicate")

		f.exec(`
			INSERT INTO refund (
				id, refund_no, payment_id, account_id, student_id, posting_year_id,
				amount, payment_method_id, reason, status, requested_by, approved_by, approved_at,
				posted_at, posted_by)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 20000, $6, 'overpaid', 'posted',
			        $7, $8, now(), now(), $8)`,
			fmt.Sprintf("RF-%d", time.Now().UnixNano()%100000000),
			paid, accountID, f.studentID, f.yearID, f.cash, f.cashierID, f.approverID)

		summary := port.SummaryFilter{AcademicYearID: &f.yearID}
		register := port.RegisterFilter{AcademicYearID: &f.yearID}
		today := shared.DateFromTime(time.Now().UTC())
		weekAgo := today.AddDays(-7)

		checks := map[string]func() error{
			"StudentStatement": func() error {
				_, err := repo.StudentStatement(f.ctx, f.studentID, &f.yearID)
				return err
			},
			"DepartmentSummary": func() error {
				rows, err := repo.DepartmentSummary(f.ctx, summary)
				if err == nil && len(rows) == 0 {
					return errors.New("no college block for a year that has an account")
				}
				return err
			},
			"StudyTypeSummary": func() error { _, err := repo.StudyTypeSummary(f.ctx, summary); return err },
			"StageSummary":     func() error { _, err := repo.StageSummary(f.ctx, summary); return err },
			"YearSummary": func() error {
				year, err := repo.YearSummary(f.ctx, f.yearID)
				if err == nil && year.AcademicYearCode == "" {
					return errors.New("the year header carries no code")
				}
				return err
			},
			"InstallmentReport": func() error {
				months, err := repo.InstallmentReport(f.ctx, port.InstallmentFilter{AcademicYearID: &f.yearID})
				if err == nil && len(months) != 2 {
					return fmt.Errorf("got %d due month(s), want 2", len(months))
				}
				return err
			},
			"DebtReport": func() error {
				_, _, err := repo.DebtReport(f.ctx, port.DebtFilter{AcademicYearID: &f.yearID})
				return err
			},
			"AgingReport": func() error {
				_, err := repo.AgingReport(f.ctx, port.AgingFilter{AcademicYearID: &f.yearID})
				return err
			},
			"DiscountReport": func() error {
				_, err := repo.DiscountReport(f.ctx, port.DiscountFilter{AcademicYearID: &f.yearID})
				return err
			},
			"ExemptionRegister": func() error {
				rows, _, err := repo.ExemptionRegister(f.ctx, register)
				if err == nil && len(rows) == 0 {
					return errors.New("a grant that consumed the whole discountable base is an exemption " +
						"and must appear in the ministry's register")
				}
				return err
			},
			"CashierDaily": func() error {
				_, err := repo.CashierDaily(f.ctx, port.CashierDailyFilter{From: &weekAgo, To: &today})
				return err
			},
			"CollectionTrend": func() error {
				trend, err := repo.CollectionTrend(f.ctx, port.TrendFilter{AcademicYearID: &f.yearID})
				if err != nil {
					return err
				}
				if len(trend.Months) != 1 {
					return fmt.Errorf("got %d month(s) of movement, want 1", len(trend.Months))
				}
				// 200,000 collected less 20,000 returned. The voided receipt never
				// enters either side: it is not a posted payment.
				if got := trend.Months[0].NetCollected; got != money.FromInt64(180_000) {
					return fmt.Errorf("net collected = %d, want 180,000", got)
				}
				if got := trend.Months[0].CumulativeNet; got != money.FromInt64(180_000) {
					return fmt.Errorf("cumulative net = %d, want 180,000", got)
				}
				if trend.EffectiveNet != money.FromInt64(900_000) {
					return fmt.Errorf("effective net = %d, want 900,000", trend.EffectiveNet)
				}
				return nil
			},
			"ExpectedCashFlow": func() error {
				_, err := repo.ExpectedCashFlow(f.ctx, port.CashFlowFilter{AcademicYearID: &f.yearID})
				return err
			},
			"VoidRegister":   func() error { _, _, err := repo.VoidRegister(f.ctx, register); return err },
			"RefundRegister": func() error { _, _, err := repo.RefundRegister(f.ctx, register); return err },
		}

		for name, run := range checks {
			if err := run(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}

// Every aggregate refuses to run unbounded, and says which parameter is
// missing rather than returning a number for the whole institution.
func TestAggregatesRefuseAnUnboundedScan(t *testing.T) {
	withFixture(t, func(f *fixture) {
		repo := NewReportRepository(reportDB)

		refusals := map[string]error{}
		_, refusals["DepartmentSummary"] = repo.DepartmentSummary(f.ctx, port.SummaryFilter{})
		_, refusals["StudyTypeSummary"] = repo.StudyTypeSummary(f.ctx, port.SummaryFilter{})
		_, refusals["StageSummary"] = repo.StageSummary(f.ctx, port.SummaryFilter{})
		_, refusals["InstallmentReport"] = repo.InstallmentReport(f.ctx, port.InstallmentFilter{})
		_, refusals["DiscountReport"] = repo.DiscountReport(f.ctx, port.DiscountFilter{})
		_, refusals["CollectionTrend"] = repo.CollectionTrend(f.ctx, port.TrendFilter{})
		_, refusals["ExpectedCashFlow"] = repo.ExpectedCashFlow(f.ctx, port.CashFlowFilter{})
		_, refusals["CashierDaily"] = repo.CashierDaily(f.ctx, port.CashierDailyFilter{})
		_, _, refusals["VoidRegister"] = repo.VoidRegister(f.ctx, port.RegisterFilter{})
		_, _, refusals["RefundRegister"] = repo.RefundRegister(f.ctx, port.RegisterFilter{})
		_, _, refusals["ExemptionRegister"] = repo.ExemptionRegister(f.ctx, port.RegisterFilter{})

		for name, err := range refusals {
			if err == nil {
				t.Errorf("%s ran without a bound; it would scan every year the university has kept", name)
				continue
			}
			if shared.KindOf(err) != shared.KindValidation {
				t.Errorf("%s refused with kind %v, want a validation error", name, shared.KindOf(err))
			}
		}
	})
}
