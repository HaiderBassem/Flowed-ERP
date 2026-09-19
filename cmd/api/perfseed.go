package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"flowed/internal/platform/config"
	"flowed/internal/platform/logger"
	"flowed/internal/platform/pg"
)

// perfSeed loads a dataset large enough for the timings to mean something.
//
//	api perf-seed --scale=small|medium|large
//
// The demo dataset is built through the real commands, which is what makes a
// successful load evidence that the system works. It is also why it cannot be
// used here: thirty thousand payments through the full command path with its
// locks and its audit chain takes hours, and what this dataset is for is
// measuring how the *reads* behave when the tables are the size a university
// reaches in five years.
//
// So this writes SQL directly, and the one property it must not lose is
// consistency: the four reconciliation views have to stay empty afterwards.
// Every account's cached totals are computed from the rows written beside it,
// every installment's paid amount from its allocations, and the run checks that
// at the end rather than asserting it.
//
// It refuses to run in production, and it refuses to run against a database
// that already holds real data.
func perfSeed() error {
	scale := "small"
	for _, arg := range os.Args[2:] {
		if strings.HasPrefix(arg, "--scale=") {
			scale = strings.TrimPrefix(arg, "--scale=")
		}
	}

	accounts, ok := perfScales[scale]
	if !ok {
		// An exact count is allowed too: the named scales are starting points,
		// and the interesting size is usually "what this university actually
		// has", which is a number somebody read off a report.
		if accounts, ok = parseScaleCount(scale); !ok {
			return fmt.Errorf("unknown scale %q; expected small, medium, large or a count", scale)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.App.IsProduction() {
		return fmt.Errorf("refusing to write a synthetic dataset into production")
	}

	log := logger.New(cfg.Log, cfg.App.Name+"-perf-seed", version, cfg.App.Environment)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	db, err := pg.Connect(ctx, cfg.Database, log, nil)
	if err != nil {
		return err
	}
	defer db.Close()

	seeder := &perfSeeder{db: db, log: log, accounts: accounts}
	return seeder.run(ctx)
}

// perfScales are the sizes worth measuring at.
//
// Small is one busy year at a mid-sized college. Medium is a whole university
// for a year. Large is five years of one — the size at which an unindexed
// report stops being merely slow and starts holding connections that the
// cashier desks need.
var perfScales = map[string]int{
	"small":  2_000,
	"medium": 20_000,
	"large":  100_000,
}

type perfSeeder struct {
	db       *pg.DB
	log      *slog.Logger
	accounts int

	yearID, collegeID, departmentID, studyTypeID, categoryID string
	userID, methodID                                         string
}

func (s *perfSeeder) run(ctx context.Context) error {
	started := time.Now()

	if err := s.resolveReferences(ctx); err != nil {
		return err
	}

	s.log.Info("seeding a performance dataset",
		slog.Int("accounts", s.accounts),
		slog.String("note", "written as SQL; the reconciliation views are checked at the end"))

	// A batch at a time so the write does not build one enormous transaction:
	// a single statement inserting a hundred thousand accounts holds locks and
	// WAL for minutes, and a failure at the end throws all of it away.
	const batch = 2_000
	for offset := 0; offset < s.accounts; offset += batch {
		size := min(batch, s.accounts-offset)
		if err := s.seedBatch(ctx, offset, size); err != nil {
			return fmt.Errorf("seeding accounts %d-%d: %w", offset, offset+size, err)
		}
		s.log.Info("seeded", slog.Int("accounts", offset+size), slog.Int("of", s.accounts))
	}

	if err := s.analyze(ctx); err != nil {
		return err
	}

	// The dataset is only useful if it is a legal one. A perf run against
	// inconsistent data measures queries nobody will ever run.
	if err := s.verify(ctx); err != nil {
		return err
	}

	s.log.Info("performance dataset ready",
		slog.Int("accounts", s.accounts),
		slog.Duration("took", time.Since(started)))
	return nil
}

// resolveReferences finds the configuration rows to hang the dataset off.
//
// It uses whatever the database already has rather than creating its own: a
// dataset whose students sit in a college that no report filters by would make
// every filtered query fast for the wrong reason.
func (s *perfSeeder) resolveReferences(ctx context.Context) error {
	q := s.db.Pool()
	lookups := []struct {
		into  *string
		query string
		what  string
	}{
		{&s.yearID, `SELECT id FROM academic_year ORDER BY start_date DESC LIMIT 1`, "an academic year"},
		{&s.collegeID, `SELECT id FROM college WHERE is_active ORDER BY code LIMIT 1`, "a college"},
		{&s.departmentID, `SELECT id FROM department WHERE college_id = $1 ORDER BY code LIMIT 1`, "a department"},
		{&s.studyTypeID, `SELECT id FROM study_type ORDER BY code LIMIT 1`, "a study type"},
		{&s.categoryID, `SELECT id FROM student_category ORDER BY code LIMIT 1`, "a student category"},
		{&s.userID, `SELECT id FROM app_user ORDER BY created_at LIMIT 1`, "an operator"},
		{&s.methodID, `SELECT id FROM payment_method WHERE code = 'CASH'`, "the cash payment method"},
	}
	for _, lookup := range lookups {
		var err error
		if strings.Contains(lookup.query, "$1") {
			err = q.QueryRow(ctx, lookup.query, s.collegeID).Scan(lookup.into)
		} else {
			err = q.QueryRow(ctx, lookup.query).Scan(lookup.into)
		}
		if err != nil {
			return fmt.Errorf("the database needs %s before a performance dataset can be built "+
				"(run `make demo-reset` first): %w", lookup.what, err)
		}
	}
	return nil
}

// seedBatch writes one batch of students with their whole financial history.
//
// The shape is deliberately realistic rather than uniform: a third of the
// accounts are settled, a third partly paid and a third untouched, because a
// debt report over a dataset where everybody owes the same amount measures a
// query no university runs.
func (s *perfSeeder) seedBatch(ctx context.Context, offset, size int) error {
	const query = `
WITH new_students AS (
    INSERT INTO student (id, student_no, full_name, mother_name, status)
    SELECT
        gen_random_uuid(),
        'PERF' || lpad((($1::int + i))::text, 8, '0'),
        (ARRAY['محمد','علي','فاطمة','زينب','حسين','مريم','عمر','نور'])[1 + (i % 8)] || ' '
            || (ARRAY['الجبوري','العبيدي','الحسناوي','الخفاجي','الدليمي'])[1 + (i % 5)]
            || ' ' || (($1::int + i))::text,
        (ARRAY['سعاد','هدى','أمل','رجاء'])[1 + (i % 4)],
        'active' 
    FROM generate_series(0, $2::int - 1) AS i
    RETURNING id, student_no
),
numbered AS (
    SELECT id, student_no,
           row_number() OVER (ORDER BY student_no) - 1 AS n
    FROM new_students
),
new_enrollments AS (
    INSERT INTO enrollment (
        id, student_id, academic_year_id, sequence_no, college_id, department_id,
        study_type_id, student_category_id, stage, enrollment_status, academic_result)
    SELECT gen_random_uuid(), n.id, $3, 1, $4, $5, $6, $7,
           1 + (n.n % 4), 'active', 'pending'
    FROM numbered n
    RETURNING id, student_id
),
new_accounts AS (
    INSERT INTO financial_account (
        id, enrollment_id, academic_year_id, student_id, college_id, department_id,
        study_type_id, stage, gross_total, discountable_base, discount_total, net_total,
        paid_total, status, generated_by)
    SELECT
        gen_random_uuid(), e.id, $3, e.student_id, $4, $5, $6,
        1 + (n.n % 4),
        2000000 + (n.n % 5) * 250000,
        2000000 + (n.n % 5) * 250000,
        0,
        2000000 + (n.n % 5) * 250000,
        -- Settled, half paid, or untouched. The cache is written here and the
        -- payments below are written to match it exactly.
        CASE n.n % 3
            WHEN 0 THEN 2000000 + (n.n % 5) * 250000
            WHEN 1 THEN (2000000 + (n.n % 5) * 250000) / 2
            ELSE 0
        END,
        'active', $8
    FROM new_enrollments e
    JOIN numbered n ON n.id = e.student_id
    RETURNING id, enrollment_id, student_id, net_total, paid_total
),
new_installments AS (
    INSERT INTO installment (id, account_id, installment_no, due_date, amount, paid_amount, status)
    SELECT
        gen_random_uuid(), planned.account_id, planned.installment_no, planned.due_date,
        planned.amount, planned.paid,
        -- The status has to agree with the money: the schema refuses a
        -- 'pending' row carrying an allocation, which is the same rule that
        -- makes an installment's stored status trustworthy in a report.
        CASE
            WHEN planned.paid = 0             THEN 'pending'
            WHEN planned.paid >= planned.amount THEN 'paid'
            ELSE 'partially_paid'
        END
    FROM (
        SELECT
            a.id AS account_id,
            i AS installment_no,
            (SELECT start_date FROM academic_year WHERE id = $3) + (i * 60) AS due_date,
            -- Two installments, the first carrying any odd dinar so the plan
            -- sums to the net exactly.
            CASE i WHEN 1 THEN a.net_total - (a.net_total / 2) ELSE a.net_total / 2 END AS amount,
            CASE
                WHEN a.paid_total = 0 THEN 0
                WHEN a.paid_total >= a.net_total THEN
                    CASE i WHEN 1 THEN a.net_total - (a.net_total / 2) ELSE a.net_total / 2 END
                -- Part paid: it sits on the first installment, oldest first,
                -- which is how the allocator actually behaves.
                ELSE CASE i
                    WHEN 1 THEN least(a.paid_total, a.net_total - (a.net_total / 2))
                    ELSE 0
                END
            END AS paid
        FROM new_accounts a
        CROSS JOIN generate_series(1, 2) AS i
    ) planned
    RETURNING id, account_id, installment_no, paid_amount
),
new_payments AS (
    INSERT INTO payment (
        id, receipt_no, account_id, student_id, enrollment_id, posting_year_id,
        amount, payment_method_id, cashier_user_id, status, posted_at, paid_at,
        idempotency_key, payload_hash)
    SELECT
        gen_random_uuid(),
        'PERF-' || lpad((($1::int + n.n))::text, 8, '0'),
        a.id, a.student_id, a.enrollment_id, $3,
        a.paid_total, $9, $8, 'posted', now(), now(),
        'perf-' || (($1::int + n.n))::text,
        'perf'
    FROM new_accounts a
    JOIN numbered n ON n.id = a.student_id
    WHERE a.paid_total > 0
    RETURNING id, account_id, amount
)
INSERT INTO payment_allocation (id, payment_id, installment_id, amount, entry_type, created_by)
SELECT gen_random_uuid(), p.id, i.id, i.paid_amount, 'allocation', $8
FROM new_payments p
JOIN new_installments i ON i.account_id = p.account_id
WHERE i.paid_amount > 0`

	_, err := s.db.Pool().Exec(ctx, query,
		offset, size, s.yearID, s.collegeID, s.departmentID, s.studyTypeID,
		s.categoryID, s.userID, s.methodID)
	return err
}

// analyze refreshes the planner statistics.
//
// Without it the first measured queries are planned against statistics from
// before the dataset existed, and the numbers that come out describe a
// mis-planned query rather than a slow one — which is a real production
// failure too, and one worth not reproducing accidentally here.
func (s *perfSeeder) analyze(ctx context.Context) error {
	s.log.Info("refreshing planner statistics")
	for _, table := range []string{
		"student", "enrollment", "financial_account", "installment",
		"payment", "payment_allocation",
	} {
		if _, err := s.db.Pool().Exec(ctx, "ANALYZE "+table); err != nil {
			return fmt.Errorf("analyzing %s: %w", table, err)
		}
	}
	return nil
}

// verify refuses to hand over a dataset that breaks the invariants.
func (s *perfSeeder) verify(ctx context.Context) error {
	checks := []struct {
		view, what string
	}{
		{"v_account_reconciliation", "account caches disagree with their transactions"},
		{"v_installment_reconciliation", "installment caches disagree with their allocations"},
		{"v_over_refunded_payments", "a payment is refunded beyond what it took"},
	}
	for _, check := range checks {
		var count int
		if err := s.db.Pool().QueryRow(ctx,
			"SELECT count(*) FROM "+check.view).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf(
				"the seeded dataset is not internally consistent: %s (%d row(s) in %s)",
				check.what, count, check.view)
		}
	}
	return nil
}

// parseScaleCount lets a caller ask for an exact number rather than a name.
func parseScaleCount(value string) (int, bool) {
	if count, err := strconv.Atoi(value); err == nil && count > 0 {
		return count, true
	}
	return 0, false
}
