// Package integration exercises the database schema itself.
//
// These tests talk to PostgreSQL directly rather than through the repository
// layer, deliberately. The point is to prove that the guarantees live in the
// database and not merely in Go — a constraint that only the application
// enforces is a constraint a psql session, a migration, or next year's
// developer walks straight past.
//
// Run with a database available:
//
//	DB_NAME=flowed_test make db-create migrate-up && go test ./test/integration/
//
// Skipped automatically under -short.
package integration

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var pool *pgxpool.Pool

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

	var err error
	pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration tests: cannot create pool: %v\n", err)
		os.Exit(1)
	}
	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr,
			"integration tests: cannot reach the database (%v).\n"+
				"Run `make db-create migrate-up`, or use -short to skip.\n", err)
		os.Exit(1)
	}

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

// fixture holds the identifiers a test needs, all created inside one
// transaction that is rolled back afterwards so tests never see each other's
// rows.
type fixture struct {
	tx           pgx.Tx
	collegeID    string
	departmentID string
	yearID       string
	studentID    string
	morningID    string
	eveningID    string
	regularID    string
	repeatID     string
}

const (
	studyTypeMorning = "a1000000-0000-4000-8000-000000000001"
	studyTypeEvening = "a1000000-0000-4000-8000-000000000002"
	categoryRegular  = "a2000000-0000-4000-8000-000000000001"
	categoryRepeat   = "a2000000-0000-4000-8000-000000000002"
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning fixture transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Logf("rolling back fixture: %v", err)
		}
	})

	f := &fixture{
		tx:        tx,
		morningID: studyTypeMorning,
		eveningID: studyTypeEvening,
		regularID: categoryRegular,
		repeatID:  categoryRepeat,
	}

	suffix := time.Now().UnixNano()

	mustQueryRow(t, tx, &f.collegeID, `
		INSERT INTO college (id, code, name_ar) VALUES (gen_random_uuid(), $1, 'كلية اختبار')
		RETURNING id`, fmt.Sprintf("T%d", suffix%100000))

	mustQueryRow(t, tx, &f.departmentID, `
		INSERT INTO department (id, college_id, code, name_ar, stage_count)
		VALUES (gen_random_uuid(), $1, $2, 'قسم اختبار', 4)
		RETURNING id`, f.collegeID, fmt.Sprintf("D%d", suffix%100000))

	mustQueryRow(t, tx, &f.yearID, `
		INSERT INTO academic_year (id, code, start_date, end_date, status)
		VALUES (gen_random_uuid(), $1, '2025-09-01', '2026-07-01', 'open')
		RETURNING id`, fmt.Sprintf("2%03d-2%03d", suffix%1000, (suffix%1000)+1))

	mustQueryRow(t, tx, &f.studentID, `
		INSERT INTO student (id, student_no, full_name, mother_name)
		VALUES (gen_random_uuid(), $1, 'علي محمد حسن', 'زينب')
		RETURNING id`, fmt.Sprintf("S%d", suffix))

	return f
}

func (f *fixture) insertEnrollment(t *testing.T, sequenceNo int, status, result string, studyTypeID string) (string, error) {
	t.Helper()
	var id string
	err := f.tx.QueryRow(context.Background(), `
		INSERT INTO enrollment (
			id, student_id, academic_year_id, sequence_no,
			college_id, department_id, study_type_id, student_category_id,
			stage, enrollment_status, academic_result,
			result_recorded_at
		) VALUES (
			gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, 1, $8, $9,
			CASE WHEN $9 IN ('pending','not_applicable') THEN NULL ELSE now() END
		) RETURNING id`,
		f.studentID, f.yearID, sequenceNo,
		f.collegeID, f.departmentID, studyTypeID, f.regularID,
		status, result,
	).Scan(&id)
	return id, err
}

func mustQueryRow(t *testing.T, tx pgx.Tx, dest *string, sql string, args ...any) {
	t.Helper()
	if err := tx.QueryRow(context.Background(), sql, args...).Scan(dest); err != nil {
		t.Fatalf("fixture query failed: %v\nSQL: %s", err, strings.TrimSpace(sql))
	}
}

func constraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// ---------------------------------------------------------------------------
// Enrollment invariants
// ---------------------------------------------------------------------------

// The rule that prevents two live financial contexts for one student in one
// year. Without it, concurrent registration creates two accounts and the
// student's debt is split across both.
func TestOnlyOneLiveEnrollmentPerStudentPerYear(t *testing.T) {
	f := newFixture(t)

	if _, err := f.insertEnrollment(t, 1, "active", "pending", f.eveningID); err != nil {
		t.Fatalf("first enrollment should insert: %v", err)
	}

	_, err := f.insertEnrollment(t, 2, "active", "pending", f.morningID)
	if err == nil {
		t.Fatal("a second live enrollment in the same year must be rejected")
	}
	if got := constraintName(err); got != "uq_enrollment_one_live_per_year" {
		t.Errorf("violated constraint = %q, want uq_enrollment_one_live_per_year", got)
	}
}

// Every status/result combination outside the legality matrix is refused, so
// no code path can store a state the domain has no meaning for.
func TestEnrollmentStatusResultMatrixIsEnforcedByTheDatabase(t *testing.T) {
	illegal := []struct {
		status, result string
		why            string
	}{
		{"superseded", "passed_r1", "the real result belongs to the successor row"},
		{"completed", "failed", "completion means a pass"},
		{"withdrawn", "passed_r1", "a withdrawn student has no result"},
		{"deferred", "pending", "a deferred year is not applicable, not pending"},
		{"draft", "passed_r1", "a draft has no result yet"},
	}

	for _, tc := range illegal {
		t.Run(tc.status+"/"+tc.result, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.insertEnrollment(t, 1, tc.status, tc.result, f.morningID)
			if err == nil {
				t.Fatalf("%s with %s must be rejected: %s", tc.status, tc.result, tc.why)
			}
			if got := sqlState(err); got != "23514" {
				t.Errorf("SQLSTATE = %q, want 23514 (check violation); error: %v", got, err)
			}
		})
	}
}

// Superseding must write its two rows in the only order the indexes allow, and
// a deferred constraint verifies at commit that the replacement exists.
func TestSupersedeRequiresAReplacementByCommit(t *testing.T) {
	ctx := context.Background()

	// A deferred constraint trigger fires at real COMMIT, not when a savepoint
	// is released, so this test owns a top-level transaction and cleans up its
	// own rows rather than borrowing the shared rollback fixture.
	f, cleanup := newCommittedFixture(t)
	defer cleanup()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var originalID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO enrollment (
			id, student_id, academic_year_id, sequence_no, college_id, department_id,
			study_type_id, student_category_id, stage, enrollment_status, academic_result)
		VALUES (gen_random_uuid(), $1, $2, 1, $3, $4, $5, $6, 1, 'active', 'pending')
		RETURNING id`,
		f.studentID, f.yearID, f.collegeID, f.departmentID, f.eveningID, f.regularID,
	).Scan(&originalID); err != nil {
		t.Fatal(err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE enrollment
		SET enrollment_status = 'superseded', academic_result = 'not_applicable',
		    supersede_reason = 'orphan attempt', supersede_date = '2026-01-15'
		WHERE id = $1`, originalID); err != nil {
		t.Fatalf("marking superseded must succeed inside the transaction, "+
			"since the slot has to be freed before the replacement can be inserted: %v", err)
	}

	err = tx.Commit(ctx)
	if err == nil {
		t.Fatal("superseding with no replacement must be refused at commit: " +
			"it would silently erase a registration somebody may have paid into")
	}
	if !strings.Contains(err.Error(), "no replacement enrollment references it") {
		t.Errorf("unexpected error: %v", err)
	}
}

// newCommittedFixture creates fixture rows that survive commit, for the tests
// that need a real transaction boundary. It returns a cleanup that removes them.
func newCommittedFixture(t *testing.T) (*fixture, func()) {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	f := &fixture{
		morningID: studyTypeMorning,
		eveningID: studyTypeEvening,
		regularID: categoryRegular,
		repeatID:  categoryRepeat,
	}

	scan := func(dest *string, sql string, args ...any) {
		if err := pool.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
			t.Fatalf("fixture setup failed: %v\nSQL: %s", err, strings.TrimSpace(sql))
		}
	}

	scan(&f.collegeID, `INSERT INTO college (id, code, name_ar)
		VALUES (gen_random_uuid(), $1, 'كلية') RETURNING id`, fmt.Sprintf("K%d", suffix%100000))
	scan(&f.departmentID, `INSERT INTO department (id, college_id, code, name_ar, stage_count)
		VALUES (gen_random_uuid(), $1, $2, 'قسم', 4) RETURNING id`,
		f.collegeID, fmt.Sprintf("Q%d", suffix%100000))
	scan(&f.yearID, `INSERT INTO academic_year (id, code, start_date, end_date, status)
		VALUES (gen_random_uuid(), $1, '2025-09-01', '2026-07-01', 'open') RETURNING id`,
		fmt.Sprintf("4%03d-4%03d", suffix%1000, (suffix%1000)+1))
	scan(&f.studentID, `INSERT INTO student (id, student_no, full_name, mother_name)
		VALUES (gen_random_uuid(), $1, 'طالب', 'أم') RETURNING id`, fmt.Sprintf("Y%d", suffix))

	return f, func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM enrollment WHERE student_id = $1`, f.studentID)
		_, _ = pool.Exec(c, `DELETE FROM student WHERE id = $1`, f.studentID)
		_, _ = pool.Exec(c, `DELETE FROM academic_year WHERE id = $1`, f.yearID)
		_, _ = pool.Exec(c, `DELETE FROM department WHERE id = $1`, f.departmentID)
		_, _ = pool.Exec(c, `DELETE FROM college WHERE id = $1`, f.collegeID)
	}
}

func TestSupersedePairWritesInTheForcedOrder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	originalID, err := f.insertEnrollment(t, 1, "active", "pending", f.eveningID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.tx.Exec(ctx, `
		UPDATE enrollment
		SET enrollment_status = 'superseded', academic_result = 'not_applicable',
		    supersede_reason = 'changed study type to morning', supersede_date = '2026-01-15'
		WHERE id = $1`, originalID); err != nil {
		t.Fatal(err)
	}

	var replacementID string
	if err := f.tx.QueryRow(ctx, `
		INSERT INTO enrollment (
			id, student_id, academic_year_id, sequence_no,
			college_id, department_id, study_type_id, student_category_id,
			stage, enrollment_status, academic_result, supersedes_id
		) VALUES (gen_random_uuid(), $1, $2, 2, $3, $4, $5, $6, 1, 'active', 'pending', $7)
		RETURNING id`,
		f.studentID, f.yearID, f.collegeID, f.departmentID, f.morningID, f.regularID, originalID,
	).Scan(&replacementID); err != nil {
		t.Fatalf("the replacement should insert once the slot is free: %v", err)
	}

	// Head counts see one seat even though two rows exist.
	var seats, rows int
	if err := f.tx.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM v_enrollment_effective WHERE student_id = $1),
			(SELECT count(*) FROM enrollment WHERE student_id = $1)`,
		f.studentID).Scan(&seats, &rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("enrollment rows = %d, want 2 (history is preserved)", rows)
	}
	if seats != 1 {
		t.Errorf("counted seats = %d, want 1 (the superseded row must not be double counted)", seats)
	}
}

// The chain is a chain, not a tree: two replacements cannot claim the same
// predecessor.
func TestSupersedeChainCannotBranch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	originalID, err := f.insertEnrollment(t, 1, "active", "pending", f.eveningID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tx.Exec(ctx, `
		UPDATE enrollment SET enrollment_status = 'superseded', academic_result = 'not_applicable',
			supersede_reason = 'r', supersede_date = '2026-01-15' WHERE id = $1`, originalID); err != nil {
		t.Fatal(err)
	}

	insertSuccessor := func(seq int, status string) error {
		_, err := f.tx.Exec(ctx, `
			INSERT INTO enrollment (
				id, student_id, academic_year_id, sequence_no,
				college_id, department_id, study_type_id, student_category_id,
				stage, enrollment_status, academic_result, supersedes_id
			) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, 1, $8, 'no_result', $9)`,
			f.studentID, f.yearID, seq, f.collegeID, f.departmentID, f.morningID, f.regularID,
			status, originalID)
		return err
	}

	// A withdrawn successor does not occupy the live slot, so only the branch
	// constraint can stop the second one.
	if err := insertSuccessor(2, "withdrawn"); err != nil {
		t.Fatalf("first successor should insert: %v", err)
	}
	err = insertSuccessor(3, "withdrawn")
	if err == nil {
		t.Fatal("two enrollments claiming the same predecessor must be rejected")
	}
	if got := constraintName(err); got != "uq_enrollment_supersedes" {
		t.Errorf("violated constraint = %q, want uq_enrollment_supersedes", got)
	}
}

// ---------------------------------------------------------------------------
// Fee policy resolution
// ---------------------------------------------------------------------------

// NULLS NOT DISTINCT is what makes scope uniqueness actually bite. Without it,
// two rows both meaning "this year, this college, any department" insert
// cleanly and resolution finds two winners at equal specificity.
func TestPublishedFeePolicyScopeIsUniqueAcrossWildcards(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insert := func(code string) error {
		_, err := f.tx.Exec(ctx, `
			INSERT INTO fee_policy_version (
				id, policy_code, version_no, academic_year_id, college_id, status, published_at)
			VALUES (gen_random_uuid(), $1, 1, $2, $3, 'published', now())`, code, f.yearID, f.collegeID)
		return err
	}

	if err := insert("FP-A"); err != nil {
		t.Fatalf("the first policy should insert: %v", err)
	}
	err := insert("FP-B")
	if err == nil {
		t.Fatal("a second published policy with an identical scope must be rejected, " +
			"or fee resolution has two winners and charges students differently by accident")
	}
	if got := constraintName(err); got != "uq_fee_policy_scope" {
		t.Errorf("violated constraint = %q, want uq_fee_policy_scope", got)
	}
}

// Powers of two guarantee that no two distinct scopes tie. A score that merely
// counted specified dimensions would rank {college} against
// {department, stage, category} arbitrarily.
func TestSpecificityScoreIsGeneratedAndTieFree(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	var collegeOnly, deptStageCategory int
	if err := f.tx.QueryRow(ctx, `
		INSERT INTO fee_policy_version (id, policy_code, version_no, academic_year_id, college_id, status)
		VALUES (gen_random_uuid(), 'FP-COLLEGE', 1, $1, $2, 'draft')
		RETURNING specificity_score`, f.yearID, f.collegeID).Scan(&collegeOnly); err != nil {
		t.Fatal(err)
	}
	if err := f.tx.QueryRow(ctx, `
		INSERT INTO fee_policy_version (
			id, policy_code, version_no, academic_year_id, college_id, department_id, stage, student_category_id, status)
		VALUES (gen_random_uuid(), 'FP-NARROW', 1, $1, $2, $3, 1, $4, 'draft')
		RETURNING specificity_score`,
		f.yearID, f.collegeID, f.departmentID, f.repeatID).Scan(&deptStageCategory); err != nil {
		t.Fatal(err)
	}

	if collegeOnly != 16 {
		t.Errorf("college-only score = %d, want 16", collegeOnly)
	}
	if deptStageCategory != 16+8+4+1 {
		t.Errorf("narrow scope score = %d, want 29", deptStageCategory)
	}
	if deptStageCategory <= collegeOnly {
		t.Error("the narrower scope must outrank the broader one")
	}
}

// ---------------------------------------------------------------------------
// Financial immutability
// ---------------------------------------------------------------------------

func TestAppendOnlyTablesRefuseUpdateAndDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	var accountID string
	enrollmentID, err := f.insertEnrollment(t, 1, "active", "pending", f.eveningID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.tx.QueryRow(ctx, `
		INSERT INTO financial_account (
			id, enrollment_id, academic_year_id, student_id, college_id, department_id,
			study_type_id, stage, gross_total, discountable_base, discount_total, net_total
		) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, 1, 2000000, 2000000, 0, 2000000)
		RETURNING id`,
		enrollmentID, f.yearID, f.studentID, f.collegeID, f.departmentID, f.eveningID,
	).Scan(&accountID); err != nil {
		t.Fatal(err)
	}

	var lineID string
	if err := f.tx.QueryRow(ctx, `
		INSERT INTO fee_snapshot_line (id, account_id, component_code, name_ar, amount, is_discountable, is_refundable)
		VALUES (gen_random_uuid(), $1, 'TUITION', 'القسط', 2000000, true, true)
		RETURNING id`, accountID).Scan(&lineID); err != nil {
		t.Fatal(err)
	}

	// A snapshot line is the record of what a student was actually charged.
	// Editing one would rewrite history that receipts already reference.
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"update", `UPDATE fee_snapshot_line SET amount = 1 WHERE id = $1`},
		{"delete", `DELETE FROM fee_snapshot_line WHERE id = $1`},
	} {
		t.Run("fee_snapshot_line_"+tc.name, func(t *testing.T) {
			sp, err := f.tx.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sp.Rollback(ctx) }()

			if _, err := sp.Exec(ctx, tc.sql, lineID); err == nil {
				t.Errorf("%s on an append-only table must be refused", tc.name)
			} else if !strings.Contains(err.Error(), "append-only") {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestPaymentAmountIsImmutableWhileStatusMayMove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	accountID, paymentID := f.seedPayment(t, 1_000_000)

	// The amount is frozen: the receipt in the student's hand says what it says.
	sp, err := f.tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Exec(ctx, `UPDATE payment SET amount = 400000 WHERE id = $1`, paymentID); err == nil {
		t.Error("editing a payment's amount must be refused; a refund is the way to return money")
	} else if !strings.Contains(err.Error(), "immutable") {
		t.Errorf("unexpected error: %v", err)
	}
	_ = sp.Rollback(ctx)

	// The status may move to voided, which is the only legitimate transition.
	if _, err := f.tx.Exec(ctx, `
		UPDATE payment SET status = 'voided', voided_at = now(), void_reason = 'wrong student'
		WHERE id = $1`, paymentID); err != nil {
		t.Errorf("voiding a payment should be permitted: %v", err)
	}

	_ = accountID
}

func (f *fixture) seedPayment(t *testing.T, amount int64) (accountID, paymentID string) {
	t.Helper()
	ctx := context.Background()

	enrollmentID, err := f.insertEnrollment(t, 1, "active", "pending", f.eveningID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.tx.QueryRow(ctx, `
		INSERT INTO financial_account (
			id, enrollment_id, academic_year_id, student_id, college_id, department_id,
			study_type_id, stage, gross_total, discountable_base, discount_total, net_total, status
		) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, 1, $7, $7, 0, $7, 'active')
		RETURNING id`,
		enrollmentID, f.yearID, f.studentID, f.collegeID, f.departmentID, f.eveningID, amount,
	).Scan(&accountID); err != nil {
		t.Fatal(err)
	}

	var userID string
	if err := f.tx.QueryRow(ctx, `
		INSERT INTO app_user (id, username, full_name, password_hash)
		VALUES (gen_random_uuid(), $1, 'صراف', 'x') RETURNING id`,
		fmt.Sprintf("cashier%d", time.Now().UnixNano()%1000000)).Scan(&userID); err != nil {
		t.Fatal(err)
	}

	if err := f.tx.QueryRow(ctx, `
		INSERT INTO payment (
			id, receipt_no, account_id, student_id, enrollment_id, posting_year_id,
			amount, payment_method_id, cashier_user_id, status, posted_at,
			idempotency_key, payload_hash
		) VALUES (
			gen_random_uuid(), $1, $2, $3, $4, $5, $6,
			(SELECT id FROM payment_method WHERE code = 'CASH'), $7, 'posted', now(), $8, 'hash'
		) RETURNING id`,
		fmt.Sprintf("R-%d", time.Now().UnixNano()%1000000),
		accountID, f.studentID, enrollmentID, f.yearID, amount, userID,
		fmt.Sprintf("key-%d", time.Now().UnixNano()),
	).Scan(&paymentID); err != nil {
		t.Fatal(err)
	}
	return accountID, paymentID
}

func TestIdempotencyKeyIsUniqueAcrossPayments(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	accountID, _ := f.seedPayment(t, 500_000)

	var userID string
	if err := f.tx.QueryRow(ctx, `SELECT cashier_user_id FROM payment WHERE account_id = $1 LIMIT 1`,
		accountID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	var enrollmentID string
	if err := f.tx.QueryRow(ctx, `SELECT enrollment_id FROM payment WHERE account_id = $1 LIMIT 1`,
		accountID).Scan(&enrollmentID); err != nil {
		t.Fatal(err)
	}
	var existingKey string
	if err := f.tx.QueryRow(ctx, `SELECT idempotency_key FROM payment WHERE account_id = $1 LIMIT 1`,
		accountID).Scan(&existingKey); err != nil {
		t.Fatal(err)
	}

	_, err := f.tx.Exec(ctx, `
		INSERT INTO payment (
			id, receipt_no, account_id, student_id, enrollment_id, posting_year_id,
			amount, payment_method_id, cashier_user_id, status, posted_at,
			idempotency_key, payload_hash
		) VALUES (
			gen_random_uuid(), 'R-DUPLICATE', $1, $2, $3, $4, 500000,
			(SELECT id FROM payment_method WHERE code = 'CASH'), $5, 'posted', now(), $6, 'hash'
		)`, accountID, f.studentID, enrollmentID, f.yearID, userID, existingKey)

	if err == nil {
		t.Fatal("a retried submission carrying the same idempotency key must not collect twice")
	}
	if got := constraintName(err); got != "uq_payment_idempotency_key" {
		t.Errorf("violated constraint = %q, want uq_payment_idempotency_key", got)
	}
}

// Allocation and reversal rows for the same pair must coexist: reversals are
// rows, not flags, and a unique pair would force the destructive in-place
// update the whole design exists to prevent.
func TestAllocationAndItsReversalCanShareAPair(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	accountID, paymentID := f.seedPayment(t, 500_000)

	var installmentID string
	if err := f.tx.QueryRow(ctx, `
		INSERT INTO installment (id, account_id, installment_no, due_date, amount)
		VALUES (gen_random_uuid(), $1, 1, '2025-10-01', 500000)
		RETURNING id`, accountID).Scan(&installmentID); err != nil {
		t.Fatal(err)
	}

	var allocationID string
	if err := f.tx.QueryRow(ctx, `
		INSERT INTO payment_allocation (id, payment_id, installment_id, amount, entry_type)
		VALUES (gen_random_uuid(), $1, $2, 500000, 'allocation')
		RETURNING id`, paymentID, installmentID).Scan(&allocationID); err != nil {
		t.Fatal(err)
	}

	// Two partial reversals against one allocation. Whole-allocation-only
	// reversal would make an ordinary partial refund impossible, so the schema
	// permits several; that their sum stays within the allocation is checked
	// by the application while it holds the account's row lock, which is what
	// actually serialises two refunds against the same money.
	for i, amount := range []int{200_000, 300_000} {
		if _, err := f.tx.Exec(ctx, `
			INSERT INTO payment_allocation (
				id, payment_id, installment_id, amount, entry_type, reverses_allocation_id)
			VALUES (gen_random_uuid(), $1, $2, $3, 'reversal', $4)`,
			paymentID, installmentID, amount, allocationID); err != nil {
			t.Fatalf("partial reversal %d must be insertable: %v", i+1, err)
		}
	}

	var reversed int64
	if err := f.tx.QueryRow(ctx, `
		SELECT coalesce(sum(amount), 0) FROM payment_allocation
		WHERE reverses_allocation_id = $1`, allocationID).Scan(&reversed); err != nil {
		t.Fatal(err)
	}
	if reversed != 500_000 {
		t.Errorf("reversals total %d, want 500,000", reversed)
	}

	// The live-allocation query the refund planner reads from must now exclude
	// this allocation: nothing of it is left to unwind.
	var live int
	if err := f.tx.QueryRow(ctx, `
		SELECT count(*)
		FROM payment_allocation pa
		LEFT JOIN LATERAL (
		    SELECT sum(r.amount) AS reversed
		    FROM payment_allocation r WHERE r.reverses_allocation_id = pa.id
		) rev ON true
		WHERE pa.payment_id = $1
		  AND pa.entry_type = 'allocation'
		  AND pa.amount > coalesce(rev.reversed, 0)`, paymentID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Errorf("%d allocation(s) still look unwindable after being fully reversed", live)
	}
}

// ---------------------------------------------------------------------------
// Audit chain
// ---------------------------------------------------------------------------

func TestAuditEntriesAreChainedAndImmutable(t *testing.T) {
	ctx := context.Background()

	// Audit rows are chained across the whole table, so this test commits and
	// cleans up after itself rather than using the shared fixture rollback.
	var firstID, secondID string
	var firstHash, secondPrev string

	if err := pool.QueryRow(ctx, `
		INSERT INTO audit_log (id, entity_type, action, actor_username, metadata)
		VALUES (gen_random_uuid(), 'test', 'chain.first', 'tester', '{"t":1}'::jsonb)
		RETURNING id, entry_hash`).Scan(&firstID, &firstHash); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO audit_log (id, entity_type, action, actor_username, metadata)
		VALUES (gen_random_uuid(), 'test', 'chain.second', 'tester', '{"t":2}'::jsonb)
		RETURNING id, previous_hash`).Scan(&secondID, &secondPrev); err != nil {
		t.Fatal(err)
	}

	if firstHash == "" {
		t.Error("the trigger must compute an entry hash; an unchained entry is not possible")
	}
	if secondPrev != firstHash {
		t.Errorf("the second entry's previous_hash = %q, want the first's hash %q", secondPrev, firstHash)
	}

	for _, tc := range []struct{ name, sql string }{
		{"update", `UPDATE audit_log SET action = 'tampered' WHERE id = $1`},
		{"delete", `DELETE FROM audit_log WHERE id = $1`},
	} {
		if _, err := pool.Exec(ctx, tc.sql, firstID); err == nil {
			t.Errorf("%s on the audit log must be refused", tc.name)
		}
	}

	var problems int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM verify_audit_chain(0)`).Scan(&problems); err != nil {
		t.Fatal(err)
	}
	if problems != 0 {
		t.Errorf("verify_audit_chain reported %d problem(s) on an untampered chain", problems)
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// Two cashiers taking money from the same student at the same moment must
// serialise on the account row rather than interleaving and losing an update.
func TestConcurrentPaymentsSerialiseOnTheAccountLock(t *testing.T) {
	ctx := context.Background()

	setup, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	suffix := time.Now().UnixNano()
	var collegeID, departmentID, yearID, studentID, enrollmentID, accountID string

	exec := func(dest *string, sql string, args ...any) {
		if err := setup.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
			_ = setup.Rollback(ctx)
			t.Fatalf("setup failed: %v\nSQL: %s", err, strings.TrimSpace(sql))
		}
	}

	exec(&collegeID, `INSERT INTO college (id, code, name_ar) VALUES (gen_random_uuid(), $1, 'ك')
		RETURNING id`, fmt.Sprintf("C%d", suffix%100000))
	exec(&departmentID, `INSERT INTO department (id, college_id, code, name_ar, stage_count)
		VALUES (gen_random_uuid(), $1, $2, 'ق', 4) RETURNING id`, collegeID, fmt.Sprintf("P%d", suffix%100000))
	exec(&yearID, `INSERT INTO academic_year (id, code, start_date, end_date, status)
		VALUES (gen_random_uuid(), $1, '2025-09-01', '2026-07-01', 'open') RETURNING id`,
		fmt.Sprintf("3%03d-3%03d", suffix%1000, (suffix%1000)+1))
	exec(&studentID, `INSERT INTO student (id, student_no, full_name, mother_name)
		VALUES (gen_random_uuid(), $1, 'طالب', 'أم') RETURNING id`, fmt.Sprintf("X%d", suffix))
	exec(&enrollmentID, `INSERT INTO enrollment (
			id, student_id, academic_year_id, sequence_no, college_id, department_id,
			study_type_id, student_category_id, stage, enrollment_status, academic_result)
		VALUES (gen_random_uuid(), $1, $2, 1, $3, $4, $5, $6, 1, 'active', 'pending') RETURNING id`,
		studentID, yearID, collegeID, departmentID, studyTypeEvening, categoryRegular)
	exec(&accountID, `INSERT INTO financial_account (
			id, enrollment_id, academic_year_id, student_id, college_id, department_id,
			study_type_id, stage, gross_total, discountable_base, discount_total, net_total, status)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, 1, 1000000, 1000000, 0, 1000000, 'active')
		RETURNING id`, enrollmentID, yearID, studentID, collegeID, departmentID, studyTypeEvening)

	if err := setup.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = pool.Exec(cleanup, `DELETE FROM financial_account WHERE id = $1`, accountID)
		_, _ = pool.Exec(cleanup, `DELETE FROM enrollment WHERE id = $1`, enrollmentID)
		_, _ = pool.Exec(cleanup, `DELETE FROM student WHERE id = $1`, studentID)
		_, _ = pool.Exec(cleanup, `DELETE FROM academic_year WHERE id = $1`, yearID)
		_, _ = pool.Exec(cleanup, `DELETE FROM department WHERE id = $1`, departmentID)
		_, _ = pool.Exec(cleanup, `DELETE FROM college WHERE id = $1`, collegeID)
	})

	// Ten concurrent collections of 50,000 each. Read-modify-write without the
	// lock would lose updates; with it, the total is exact.
	const workers = 10
	const each = 50_000

	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()

			var paid int64
			if err := tx.QueryRow(ctx,
				`SELECT paid_total FROM financial_account WHERE id = $1 FOR UPDATE`,
				accountID).Scan(&paid); err != nil {
				errs <- err
				return
			}
			if _, err := tx.Exec(ctx,
				`UPDATE financial_account SET paid_total = $1 WHERE id = $2`,
				paid+each, accountID); err != nil {
				errs <- err
				return
			}
			errs <- tx.Commit(ctx)
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent worker failed: %v", err)
		}
	}

	var total int64
	if err := pool.QueryRow(ctx, `SELECT paid_total FROM financial_account WHERE id = $1`,
		accountID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if want := int64(workers * each); total != want {
		t.Errorf("paid_total = %d, want %d — %d dinars were lost to interleaved updates",
			total, want, want-total)
	}
}
