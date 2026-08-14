package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// ReportRepository reads the reporting model: the ten views migration 000009
// installs, and the transaction rows behind them.
//
// Nothing here writes, locks, or opens a transaction. Every method resolves its
// executor with db.Conn, so a caller that needs several reports to agree on one
// snapshot can wrap them in a read transaction and a caller printing one report
// pays for no boundary at all.
type ReportRepository struct{ db *pg.DB }

// NewReportRepository builds the reporting store over a connection pool.
func NewReportRepository(db *pg.DB) *ReportRepository { return &ReportRepository{db: db} }

var _ port.ReportRepository = (*ReportRepository)(nil)

// ---------------------------------------------------------------------------
// Parameter validation
// ---------------------------------------------------------------------------

// requireYear refuses a report that was never bounded to an academic year.
//
// The bound is not a convenience. Unfiltered, an aggregate scans every account
// the university has ever opened, and the number it returns adds up two years
// whose prices have nothing to do with each other.
func requireYear(year *shared.ID, report string) error {
	if year != nil && !shared.IsNil(*year) {
		return nil
	}
	return shared.Validation("report.missing_academic_year",
		"%s requires an academic_year_id; the year filter is what bounds the scan and "+
			"figures from two years are not comparable anyway", report).
		WithDetail("report", report).
		WithDetail("missing_parameter", "academic_year_id")
}

// requireDateRange refuses a cash report with no window. A cashier's day
// belongs to a shift rather than to an academic year, so the date range is what
// bounds it.
func requireDateRange(from, to *shared.Date, report string) error {
	if from != nil && to != nil {
		return nil
	}
	return shared.Validation("report.missing_date_range",
		"%s requires both from and to dates; a cashier's day belongs to a shift, "+
			"not to an academic year, so the range is what bounds the scan", report).
		WithDetail("report", report).
		WithDetail("missing_parameter", "from,to")
}

// exclusiveEnd turns an inclusive end date into the instant just past it.
//
// A register filters on a timestamp, and a caller asking for events "to the
// 30th" means the whole of the 30th. Comparing against midnight would drop the
// day's work, and casting the column to a date in SQL would make the answer
// depend on the session's time zone.
func exclusiveEnd(d *shared.Date) *time.Time {
	if d == nil {
		return nil
	}
	next := d.AddDays(1)
	return timeOrNil(&next)
}

// ---------------------------------------------------------------------------
// Shared aggregate block
// ---------------------------------------------------------------------------

// summaryAggregates is the money block every aggregate report selects.
//
// It reads v_year_department_summary, which is where the counts-versus-money
// rule lives: student_count already excludes superseded enrollments, while the
// money columns sum every non-cancelled account of the year. Re-deriving either
// side here would put a second, divergent copy of that rule in Go.
const summaryAggregates = `
	    coalesce(sum(s.student_count), 0)::bigint        AS student_count,
	    coalesce(sum(s.account_count), 0)::bigint        AS account_count,
	    coalesce(sum(s.gross_total), 0)::bigint          AS gross_total,
	    coalesce(sum(s.discount_total), 0)::bigint       AS discount_total,
	    coalesce(sum(s.effective_net_total), 0)::bigint  AS effective_net,
	    coalesce(sum(s.paid_total), 0)::bigint           AS paid_total,
	    coalesce(sum(s.refunded_total), 0)::bigint       AS refunded_total,
	    coalesce(sum(s.remaining_total), 0)::bigint      AS remaining,
	    CASE WHEN sum(s.effective_net_total) > 0
	         THEN round(100.0 * sum(s.paid_total) / sum(s.effective_net_total), 2)::float8
	    END                                              AS collection_rate_pct`

// summaryColumns re-selects the aggregate block from a subquery that already
// computed it.
const summaryColumns = `
	    base.student_count, base.account_count, base.gross_total, base.discount_total,
	    base.effective_net, base.paid_total, base.refunded_total, base.remaining,
	    base.collection_rate_pct`

// totalsTargets lists the scan targets for the aggregate block, in the order
// both constants above select them.
func totalsTargets(t *port.SummaryTotals) []any {
	return []any{
		&t.StudentCount, &t.AccountCount, &t.GrossTotal, &t.DiscountTotal,
		&t.EffectiveNet, &t.PaidTotal, &t.Refunded, &t.Remaining, &t.CollectionRatePct,
	}
}

// summaryPredicates builds the filter shared by every aggregate report. The
// column names are identical on v_year_department_summary and
// financial_account, so one builder serves both by taking the alias.
func summaryPredicates(f port.SummaryFilter, alias string, args *argList) []string {
	where := []string{alias + ".academic_year_id = " + args.next(*f.AcademicYearID)}
	if f.CollegeID != nil {
		where = append(where, alias+".college_id = "+args.next(*f.CollegeID))
	}
	if f.DepartmentID != nil {
		where = append(where, alias+".department_id = "+args.next(*f.DepartmentID))
	}

	// Organisational scope, enforced in the query rather than after it.
	if predicate := reportScope(f.Scope, alias, args); predicate != "" {
		where = append(where, predicate)
	}
	if f.StudyTypeID != nil {
		where = append(where, alias+".study_type_id = "+args.next(*f.StudyTypeID))
	}
	if f.Stage != nil {
		where = append(where, alias+".stage = "+args.next(*f.Stage))
	}
	return where
}

func joinWhere(where []string) string { return strings.Join(where, "\n\t\t  AND ") }

// ---------------------------------------------------------------------------
// Department, study type and stage summaries
// ---------------------------------------------------------------------------

// DepartmentSummary aggregates a year, grouped college then department.
func (r *ReportRepository) DepartmentSummary(ctx context.Context, f port.SummaryFilter) ([]port.CollegeSummary, error) {
	if err := requireYear(f.AcademicYearID, "the department summary"); err != nil {
		return nil, err
	}

	args := &argList{}
	// The grouping set produces each college's subtotal in the same pass as its
	// departments, so the two agree by construction instead of by a second query
	// that could be filtered differently.
	query := `
		SELECT
		    s.college_id, c.code, c.name_ar,
		    s.department_id, d.code, d.name_ar,` + summaryAggregates + `
		FROM v_year_department_summary s
		JOIN college c ON c.id = s.college_id
		JOIN department d ON d.id = s.department_id
		WHERE ` + joinWhere(summaryPredicates(f, "s", args)) + `
		GROUP BY GROUPING SETS (
		    (s.college_id, c.code, c.name_ar, s.department_id, d.code, d.name_ar),
		    (s.college_id, c.code, c.name_ar)
		)
		ORDER BY c.name_ar, (s.department_id IS NOT NULL), d.name_ar`

	rows, err := r.db.Conn(ctx).Query(ctx, query, args.all()...)
	if err != nil {
		return nil, pg.WrapQuery("report.DepartmentSummary", err)
	}
	defer rows.Close()

	var colleges []port.CollegeSummary
	for rows.Next() {
		var (
			collegeID      shared.ID
			collegeCode    string
			collegeName    string
			departmentID   *shared.ID
			departmentCode *string
			departmentName *string
			totals         port.SummaryTotals
		)
		targets := append([]any{
			&collegeID, &collegeCode, &collegeName,
			&departmentID, &departmentCode, &departmentName,
		}, totalsTargets(&totals)...)
		if err := rows.Scan(targets...); err != nil {
			return nil, pg.WrapQuery("report.DepartmentSummary", err)
		}

		if departmentID == nil {
			colleges = append(colleges, port.CollegeSummary{
				CollegeID:     collegeID,
				CollegeCode:   collegeCode,
				CollegeName:   collegeName,
				SummaryTotals: totals,
			})
			continue
		}
		if len(colleges) == 0 {
			continue
		}
		last := &colleges[len(colleges)-1]
		last.Departments = append(last.Departments, port.DepartmentSummary{
			DepartmentID:   *departmentID,
			DepartmentCode: derefString(departmentCode),
			DepartmentName: derefString(departmentName),
			SummaryTotals:  totals,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, pg.WrapQuery("report.DepartmentSummary", err)
	}
	return colleges, nil
}

// StudyTypeSummary regroups the same aggregate by mode of study, which is where
// the price list differs most: an evening seat and a morning seat in the same
// department are two different fees.
func (r *ReportRepository) StudyTypeSummary(ctx context.Context, f port.SummaryFilter) ([]port.StudyTypeSummary, error) {
	if err := requireYear(f.AcademicYearID, "the study type summary"); err != nil {
		return nil, err
	}

	args := &argList{}
	query := `
		SELECT s.study_type_id, t.code, t.name_ar,` + summaryAggregates + `
		FROM v_year_department_summary s
		JOIN study_type t ON t.id = s.study_type_id
		WHERE ` + joinWhere(summaryPredicates(f, "s", args)) + `
		GROUP BY s.study_type_id, t.code, t.name_ar, t.sort_order
		ORDER BY t.sort_order, t.name_ar`

	rows, err := r.db.Conn(ctx).Query(ctx, query, args.all()...)
	if err != nil {
		return nil, pg.WrapQuery("report.StudyTypeSummary", err)
	}
	summaries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.StudyTypeSummary, error) {
		var out port.StudyTypeSummary
		targets := append([]any{&out.StudyTypeID, &out.StudyTypeCode, &out.StudyTypeName},
			totalsTargets(&out.SummaryTotals)...)
		if err := row.Scan(targets...); err != nil {
			return port.StudyTypeSummary{}, err
		}
		return out, nil
	})
	if err != nil {
		return nil, pg.WrapQuery("report.StudyTypeSummary", err)
	}
	return summaries, nil
}

// StageSummary regroups the aggregate by year of study and breaks out repeat
// students.
//
// The repeat head count comes from the effective enrollments and the repeat
// money from every non-cancelled account, exactly as the summary view does for
// the whole population — the two halves of the rule applied to a subset, not a
// shortcut around it.
func (r *ReportRepository) StageSummary(ctx context.Context, f port.SummaryFilter) ([]port.StageSummary, error) {
	if err := requireYear(f.AcademicYearID, "the stage summary"); err != nil {
		return nil, err
	}

	args := &argList{}
	base := summaryPredicates(f, "s", args)
	repeat := summaryPredicates(f, "fa", args)

	query := `
		WITH base AS (
		    SELECT s.stage,` + summaryAggregates + `
		    FROM v_year_department_summary s
		    WHERE ` + joinWhere(base) + `
		    GROUP BY s.stage
		),
		repeats AS (
		    SELECT
		        fa.stage,
		        count(DISTINCT CASE WHEN e.enrollment_status <> 'superseded'
		                            THEN fa.student_id END)::bigint AS student_count,
		        coalesce(sum(b.effective_net), 0)::bigint            AS effective_net,
		        coalesce(sum(b.net_paid), 0)::bigint                 AS paid
		    FROM financial_account fa
		    JOIN enrollment e ON e.id = fa.enrollment_id
		    JOIN v_account_balance b ON b.account_id = fa.id
		    WHERE fa.status <> 'cancelled'
		      AND e.attempt_number > 1
		      AND ` + joinWhere(repeat) + `
		    GROUP BY fa.stage
		)
		SELECT base.stage,` + summaryColumns + `,
		       coalesce(r.student_count, 0),
		       coalesce(r.effective_net, 0),
		       coalesce(r.paid, 0)
		FROM base
		LEFT JOIN repeats r ON r.stage = base.stage
		ORDER BY base.stage`

	rows, err := r.db.Conn(ctx).Query(ctx, query, args.all()...)
	if err != nil {
		return nil, pg.WrapQuery("report.StageSummary", err)
	}
	summaries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.StageSummary, error) {
		var out port.StageSummary
		targets := append([]any{&out.Stage}, totalsTargets(&out.SummaryTotals)...)
		targets = append(targets, &out.RepeatStudentCount, &out.RepeatEffectiveNet, &out.RepeatPaid)
		if err := row.Scan(targets...); err != nil {
			return port.StageSummary{}, err
		}
		return out, nil
	})
	if err != nil {
		return nil, pg.WrapQuery("report.StageSummary", err)
	}
	return summaries, nil
}

// ---------------------------------------------------------------------------
// Year summary
// ---------------------------------------------------------------------------

// YearSummary is the year's header, its three breakdowns, and the earlier
// years' debt collected during it.
func (r *ReportRepository) YearSummary(ctx context.Context, yearID shared.ID) (*port.YearSummary, error) {
	f := port.SummaryFilter{AcademicYearID: &yearID}

	summary, err := r.yearHeader(ctx, yearID)
	if err != nil {
		return nil, err
	}
	if summary.Colleges, err = r.yearColleges(ctx, f); err != nil {
		return nil, err
	}
	if summary.StudyTypes, err = r.StudyTypeSummary(ctx, f); err != nil {
		return nil, err
	}
	if summary.Stages, err = r.StageSummary(ctx, f); err != nil {
		return nil, err
	}
	return summary, nil
}

// yearHeader reads the year's identity, its totals, and the prior-year money.
//
// Payments posted into this year against an account of an earlier one are
// counted separately and never added to the year's collection: that cash
// discharges an obligation this year never raised, and blending it would flatter
// the collection rate while leaving the debt it settled apparently outstanding.
func (r *ReportRepository) yearHeader(ctx context.Context, yearID shared.ID) (*port.YearSummary, error) {
	const query = `
		SELECT
		    y.id, y.code, y.status,
		    (SELECT coalesce(sum(s.student_count), 0)::bigint
		       FROM v_year_department_summary s WHERE s.academic_year_id = y.id),
		    (SELECT coalesce(sum(s.account_count), 0)::bigint
		       FROM v_year_department_summary s WHERE s.academic_year_id = y.id),
		    (SELECT coalesce(sum(s.gross_total), 0)::bigint
		       FROM v_year_department_summary s WHERE s.academic_year_id = y.id),
		    (SELECT coalesce(sum(s.discount_total), 0)::bigint
		       FROM v_year_department_summary s WHERE s.academic_year_id = y.id),
		    (SELECT coalesce(sum(s.effective_net_total), 0)::bigint
		       FROM v_year_department_summary s WHERE s.academic_year_id = y.id),
		    (SELECT coalesce(sum(s.paid_total), 0)::bigint
		       FROM v_year_department_summary s WHERE s.academic_year_id = y.id),
		    (SELECT coalesce(sum(s.refunded_total), 0)::bigint
		       FROM v_year_department_summary s WHERE s.academic_year_id = y.id),
		    (SELECT coalesce(sum(s.remaining_total), 0)::bigint
		       FROM v_year_department_summary s WHERE s.academic_year_id = y.id),
		    (SELECT CASE WHEN sum(s.effective_net_total) > 0
		                 THEN round(100.0 * sum(s.paid_total) / sum(s.effective_net_total), 2)::float8
		            END
		       FROM v_year_department_summary s WHERE s.academic_year_id = y.id),
		    (SELECT count(*)::bigint
		       FROM payment p
		       JOIN financial_account fa ON fa.id = p.account_id
		       JOIN academic_year prior ON prior.id = fa.academic_year_id
		      WHERE p.posting_year_id = y.id AND p.status = 'posted'
		        AND prior.start_date < y.start_date),
		    (SELECT coalesce(sum(p.amount), 0)::bigint
		       FROM payment p
		       JOIN financial_account fa ON fa.id = p.account_id
		       JOIN academic_year prior ON prior.id = fa.academic_year_id
		      WHERE p.posting_year_id = y.id AND p.status = 'posted'
		        AND prior.start_date < y.start_date),
		    (SELECT coalesce(sum(rf.amount), 0)::bigint
		       FROM refund rf
		       JOIN financial_account fa ON fa.id = rf.account_id
		       JOIN academic_year prior ON prior.id = fa.academic_year_id
		      WHERE rf.posting_year_id = y.id AND rf.status = 'posted'
		        AND prior.start_date < y.start_date)
		FROM academic_year y
		WHERE y.id = $1`

	var (
		out   port.YearSummary
		prior port.PriorYearCollection
	)
	targets := append([]any{&out.AcademicYearID, &out.AcademicYearCode, &out.Status},
		totalsTargets(&out.Totals)...)
	targets = append(targets, &prior.PaymentCount, &prior.Collected, &prior.Refunded)

	if err := r.db.Conn(ctx).QueryRow(ctx, query, yearID).Scan(targets...); err != nil {
		return nil, pg.WrapQuery("report.YearSummary", err)
	}

	net, err := prior.Collected.Sub(prior.Refunded)
	if err != nil {
		return nil, shared.Internal("report.year_summary_overflow", err,
			"netting prior-year refunds against prior-year collections overflowed")
	}
	prior.NetCollected = net
	out.PriorYearCollection = prior
	return &out, nil
}

// yearColleges is the college breakdown of the year header. It carries no
// department detail: that is the department summary's job, and duplicating it
// here would double the size of a page nobody reads it from.
func (r *ReportRepository) yearColleges(ctx context.Context, f port.SummaryFilter) ([]port.CollegeSummary, error) {
	args := &argList{}
	query := `
		SELECT s.college_id, c.code, c.name_ar,` + summaryAggregates + `
		FROM v_year_department_summary s
		JOIN college c ON c.id = s.college_id
		WHERE ` + joinWhere(summaryPredicates(f, "s", args)) + `
		GROUP BY s.college_id, c.code, c.name_ar
		ORDER BY c.name_ar`

	rows, err := r.db.Conn(ctx).Query(ctx, query, args.all()...)
	if err != nil {
		return nil, pg.WrapQuery("report.YearSummary.colleges", err)
	}
	colleges, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.CollegeSummary, error) {
		var out port.CollegeSummary
		targets := append([]any{&out.CollegeID, &out.CollegeCode, &out.CollegeName},
			totalsTargets(&out.SummaryTotals)...)
		if err := row.Scan(targets...); err != nil {
			return port.CollegeSummary{}, err
		}
		return out, nil
	})
	if err != nil {
		return nil, pg.WrapQuery("report.YearSummary.colleges", err)
	}
	return colleges, nil
}

// ---------------------------------------------------------------------------
// Installments
// ---------------------------------------------------------------------------

// installmentScope is the population every installment-derived report reads.
//
// Superseded lines belong to a replaced plan and waived lines will never be
// collected; leaving either in would inflate the expected column with money
// nobody is waiting for. Cancelled accounts go with them.
const installmentScope = `
	    fa.status <> 'cancelled'
	    AND vi.stored_status NOT IN ('waived', 'superseded')`

// InstallmentReport reports expected against collected by due month, split by
// department.
func (r *ReportRepository) InstallmentReport(ctx context.Context, f port.InstallmentFilter) ([]port.InstallmentMonth, error) {
	if err := requireYear(f.AcademicYearID, "the installment report"); err != nil {
		return nil, err
	}

	args := &argList{}
	where := []string{installmentScope, "vi.academic_year_id = " + args.next(*f.AcademicYearID)}
	if f.CollegeID != nil {
		where = append(where, "fa.college_id = "+args.next(*f.CollegeID))
	}
	if f.DepartmentID != nil {
		where = append(where, "vi.department_id = "+args.next(*f.DepartmentID))
	}

	// Organisational scope, enforced in the query rather than after it.
	if predicate := reportScope(f.Scope, "fa", args); predicate != "" {
		where = append(where, predicate)
	}
	if f.StudyTypeID != nil {
		where = append(where, "vi.study_type_id = "+args.next(*f.StudyTypeID))
	}
	if f.DueFrom != nil {
		where = append(where, "vi.due_date >= "+args.next(timeOrNil(f.DueFrom)))
	}
	if f.DueTo != nil {
		where = append(where, "vi.due_date <= "+args.next(timeOrNil(f.DueTo)))
	}

	query := `
		WITH scheduled AS (
		    SELECT
		        date_trunc('month', vi.due_date)::date AS month_start,
		        vi.department_id,
		        d.name_ar AS department_name,
		        vi.amount, vi.allocated_paid, vi.remaining, vi.is_overdue
		    FROM v_installment_status vi
		    JOIN financial_account fa ON fa.id = vi.account_id
		    JOIN department d ON d.id = vi.department_id
		    WHERE ` + joinWhere(where) + `
		)
		SELECT
		    month_start, department_id, department_name,
		    count(*)::bigint,
		    coalesce(sum(amount), 0)::bigint,
		    coalesce(sum(allocated_paid), 0)::bigint,
		    coalesce(sum(remaining), 0)::bigint,
		    count(*) FILTER (WHERE is_overdue)::bigint,
		    coalesce(sum(remaining) FILTER (WHERE is_overdue), 0)::bigint,
		    CASE WHEN sum(amount) > 0
		         THEN round(100.0 * sum(allocated_paid) / sum(amount), 2)::float8
		    END
		FROM scheduled
		GROUP BY GROUPING SETS ((month_start, department_id, department_name), (month_start))
		ORDER BY month_start, (department_id IS NOT NULL), department_name`

	rows, err := r.db.Conn(ctx).Query(ctx, query, args.all()...)
	if err != nil {
		return nil, pg.WrapQuery("report.InstallmentReport", err)
	}
	defer rows.Close()

	var months []port.InstallmentMonth
	for rows.Next() {
		var (
			monthStart     time.Time
			departmentID   *shared.ID
			departmentName *string
			totals         port.InstallmentTotals
		)
		if err := rows.Scan(
			&monthStart, &departmentID, &departmentName,
			&totals.InstallmentCount, &totals.Expected, &totals.Paid, &totals.Remaining,
			&totals.OverdueCount, &totals.OverdueAmount, &totals.PctCollected,
		); err != nil {
			return nil, pg.WrapQuery("report.InstallmentReport", err)
		}

		// The grouping set emits a month's subtotal ahead of its departments, so
		// one ordered pass builds the nesting.
		if departmentID == nil {
			months = append(months, port.InstallmentMonth{
				Month:             monthStart.Format("2006-01"),
				MonthDate:         shared.DateFromTime(monthStart),
				InstallmentTotals: totals,
			})
			continue
		}
		if len(months) == 0 {
			continue
		}
		last := &months[len(months)-1]
		last.Departments = append(last.Departments, port.InstallmentDepartment{
			DepartmentID:      *departmentID,
			DepartmentName:    derefString(departmentName),
			InstallmentTotals: totals,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, pg.WrapQuery("report.InstallmentReport", err)
	}
	return months, nil
}

// ExpectedCashFlow projects what the remaining due dates should bring in,
// month by month, split by department.
func (r *ReportRepository) ExpectedCashFlow(ctx context.Context, f port.CashFlowFilter) ([]port.CashFlowMonth, error) {
	if err := requireYear(f.AcademicYearID, "the expected cash flow report"); err != nil {
		return nil, err
	}

	args := &argList{}
	where := []string{
		installmentScope,
		"vi.remaining > 0",
		// Anything already due is debt, not forecast; it belongs to the ageing
		// report rather than to a projection of money still to arrive.
		"vi.due_date >= current_date",
		"vi.academic_year_id = " + args.next(*f.AcademicYearID),
	}
	if f.CollegeID != nil {
		where = append(where, "fa.college_id = "+args.next(*f.CollegeID))
	}
	if f.DepartmentID != nil {
		where = append(where, "vi.department_id = "+args.next(*f.DepartmentID))
	}

	// Organisational scope, enforced in the query rather than after it.
	if predicate := reportScope(f.Scope, "vi", args); predicate != "" {
		where = append(where, predicate)
	}
	if f.Through != nil {
		where = append(where, "vi.due_date <= "+args.next(timeOrNil(f.Through)))
	}

	query := `
		WITH upcoming AS (
		    SELECT
		        date_trunc('month', vi.due_date)::date AS month_start,
		        vi.department_id,
		        d.name_ar AS department_name,
		        vi.remaining
		    FROM v_installment_status vi
		    JOIN financial_account fa ON fa.id = vi.account_id
		    JOIN department d ON d.id = vi.department_id
		    WHERE ` + joinWhere(where) + `
		)
		SELECT
		    month_start, department_id, department_name,
		    count(*)::bigint,
		    coalesce(sum(remaining), 0)::bigint
		FROM upcoming
		GROUP BY GROUPING SETS ((month_start, department_id, department_name), (month_start))
		ORDER BY month_start, (department_id IS NOT NULL), department_name`

	rows, err := r.db.Conn(ctx).Query(ctx, query, args.all()...)
	if err != nil {
		return nil, pg.WrapQuery("report.ExpectedCashFlow", err)
	}
	defer rows.Close()

	var months []port.CashFlowMonth
	for rows.Next() {
		var (
			monthStart     time.Time
			departmentID   *shared.ID
			departmentName *string
			count          int64
			expected       money.Amount
		)
		if err := rows.Scan(&monthStart, &departmentID, &departmentName, &count, &expected); err != nil {
			return nil, pg.WrapQuery("report.ExpectedCashFlow", err)
		}
		if departmentID == nil {
			months = append(months, port.CashFlowMonth{
				Month:            monthStart.Format("2006-01"),
				MonthDate:        shared.DateFromTime(monthStart),
				InstallmentCount: count,
				Expected:         expected,
			})
			continue
		}
		if len(months) == 0 {
			continue
		}
		last := &months[len(months)-1]
		last.Departments = append(last.Departments, port.CashFlowDepartment{
			DepartmentID:     *departmentID,
			DepartmentName:   derefString(departmentName),
			InstallmentCount: count,
			Expected:         expected,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, pg.WrapQuery("report.ExpectedCashFlow", err)
	}
	return months, nil
}

// ---------------------------------------------------------------------------
// Debt and ageing
// ---------------------------------------------------------------------------

// DebtReport lists outstanding balances, largest first.
//
// v_debt already excludes cancelled accounts and settled ones, and computes
// overdue against today rather than against a stored flag that a failed nightly
// sweep would silently freeze.
func (r *ReportRepository) DebtReport(ctx context.Context, f port.DebtFilter) ([]port.DebtRow, int, error) {
	if f.AcademicYearID == nil && !f.PriorYearsOnly {
		return nil, 0, shared.Validation("report.missing_academic_year",
			"the debt report requires an academic_year_id, or prior_years_only to bound it "+
				"to the years whose books are already shut").
			WithDetail("report", "the debt report").
			WithDetail("missing_parameter", "academic_year_id")
	}

	predicates := func(args *argList) []string {
		where := []string{"true"}
		if f.AcademicYearID != nil {
			where = append(where, "v.academic_year_id = "+args.next(*f.AcademicYearID))
		}
		if f.PriorYearsOnly {
			where = append(where, "v.is_prior_year")
		}
		if f.CollegeID != nil {
			where = append(where, "v.college_id = "+args.next(*f.CollegeID))
		}
		if f.DepartmentID != nil {
			where = append(where, "v.department_id = "+args.next(*f.DepartmentID))
		}

		// Organisational scope, enforced in the query rather than after it.
		if predicate := reportScope(f.Scope, "v", args); predicate != "" {
			where = append(where, predicate)
		}
		if f.MinimumAmount > 0 {
			where = append(where, "v.remaining >= "+args.next(f.MinimumAmount))
		}
		return where
	}

	args := &argList{}
	where := predicates(args)
	limit := boundedLimit(f.Limit, 50)
	offset := max(f.Offset, 0)

	query := `
		SELECT
		    v.account_id, v.student_id, v.student_no, v.full_name, v.mother_name, v.phone,
		    v.academic_year_id, v.academic_year_code,
		    v.department_id, d.name_ar, v.stage,
		    v.effective_net::bigint, v.net_paid::bigint, v.remaining::bigint,
		    v.is_prior_year, v.oldest_due_date,
		    v.overdue_amount::bigint, v.overdue_installments::bigint,
		    count(*) OVER () AS total_count
		FROM v_debt v
		JOIN department d ON d.id = v.department_id
		WHERE ` + joinWhere(where) + `
		ORDER BY v.remaining DESC, v.student_no
		LIMIT ` + args.next(limit) + ` OFFSET ` + args.next(offset)

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, args.all()...)
	if err != nil {
		return nil, 0, pg.WrapQuery("report.DebtReport", err)
	}

	var total int64
	debts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.DebtRow, error) {
		var (
			out    port.DebtRow
			oldest *time.Time
		)
		if err := row.Scan(
			&out.AccountID, &out.StudentID, &out.StudentNo, &out.FullName, &out.MotherName, &out.Phone,
			&out.AcademicYearID, &out.AcademicYearCode,
			&out.DepartmentID, &out.DepartmentName, &out.Stage,
			&out.EffectiveNet, &out.NetPaid, &out.Remaining,
			&out.IsPriorYear, &oldest,
			&out.OverdueAmount, &out.OverdueCount,
			&total,
		); err != nil {
			return port.DebtRow{}, err
		}
		out.OldestOverdueDue = dateOrNil(oldest)
		return out, nil
	})
	if err != nil {
		return nil, 0, pg.WrapQuery("report.DebtReport", err)
	}

	// An empty page past the end of the result set carries no window count, so a
	// paginator that overshot would otherwise be told there were no debts at all.
	if len(debts) == 0 && offset > 0 {
		countArgs := &argList{}
		countQuery := `SELECT count(*) FROM v_debt v WHERE ` + joinWhere(predicates(countArgs))
		if err := q.QueryRow(ctx, countQuery, countArgs.all()...).Scan(&total); err != nil {
			return nil, 0, pg.WrapQuery("report.DebtReport.count", err)
		}
	}
	return debts, int(total), nil
}

// AgingReport buckets open receivables by how long they have been owed.
//
// A balance on a year whose books are shut lands in the prior-year bucket
// whatever its due date: once a year is closed the age of the individual
// installment stops being the actionable fact and "last year's money" is the
// line the finance office chases.
func (r *ReportRepository) AgingReport(ctx context.Context, f port.AgingFilter) ([]port.AgingRow, error) {
	args := &argList{}
	where := []string{installmentScope, "vi.remaining > 0"}
	if f.AcademicYearID != nil {
		where = append(where, "vi.academic_year_id = "+args.next(*f.AcademicYearID))
	}
	if f.CollegeID != nil {
		where = append(where, "fa.college_id = "+args.next(*f.CollegeID))
	}
	if f.DepartmentID != nil {
		where = append(where, "vi.department_id = "+args.next(*f.DepartmentID))
	}

	// Organisational scope, enforced in the query rather than after it.
	if predicate := reportScope(f.Scope, "fa", args); predicate != "" {
		where = append(where, predicate)
	}

	query := `
		WITH open_items AS (
		    SELECT
		        vi.department_id,
		        d.name_ar AS department_name,
		        vi.remaining,
		        vi.due_date,
		        vi.days_overdue,
		        (ay.status IN ('financially_closed', 'closed')) AS is_prior_year
		    FROM v_installment_status vi
		    JOIN financial_account fa ON fa.id = vi.account_id
		    JOIN academic_year ay ON ay.id = vi.academic_year_id
		    JOIN department d ON d.id = vi.department_id
		    WHERE ` + joinWhere(where) + `
		)
		SELECT
		    department_id, department_name,
		    coalesce(sum(remaining) FILTER (WHERE NOT is_prior_year AND due_date >= current_date), 0)::bigint,
		    coalesce(sum(remaining) FILTER (WHERE NOT is_prior_year AND days_overdue BETWEEN 1 AND 30), 0)::bigint,
		    coalesce(sum(remaining) FILTER (WHERE NOT is_prior_year AND days_overdue BETWEEN 31 AND 90), 0)::bigint,
		    coalesce(sum(remaining) FILTER (WHERE NOT is_prior_year AND days_overdue BETWEEN 91 AND 180), 0)::bigint,
		    coalesce(sum(remaining) FILTER (WHERE NOT is_prior_year AND days_overdue > 180), 0)::bigint,
		    coalesce(sum(remaining) FILTER (WHERE is_prior_year), 0)::bigint,
		    coalesce(sum(remaining) FILTER (WHERE is_prior_year OR due_date < current_date), 0)::bigint,
		    coalesce(sum(remaining), 0)::bigint
		FROM open_items
		GROUP BY department_id, department_name
		ORDER BY department_name`

	rows, err := r.db.Conn(ctx).Query(ctx, query, args.all()...)
	if err != nil {
		return nil, pg.WrapQuery("report.AgingReport", err)
	}
	aging, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.AgingRow, error) {
		var out port.AgingRow
		if err := row.Scan(
			&out.DepartmentID, &out.DepartmentName,
			&out.NotYetDue, &out.Days0To30, &out.Days31To90, &out.Days91To180, &out.Days180Plus,
			&out.PriorYear, &out.TotalOverdue, &out.TotalReceivable,
		); err != nil {
			return port.AgingRow{}, err
		}
		return out, nil
	})
	if err != nil {
		return nil, pg.WrapQuery("report.AgingReport", err)
	}
	return aging, nil
}

// ---------------------------------------------------------------------------
// Discounts and exemptions
// ---------------------------------------------------------------------------

// DiscountReport reports what discounts actually cost, per definition and
// version.
//
// It reads v_discount_usage, which sums the applications frozen onto accounts.
// Recomputing the cost from the definitions would answer a different question —
// what those grants would be worth under today's rules — and a definition
// revised twice since would make last year's report change every time it is
// printed.
func (r *ReportRepository) DiscountReport(ctx context.Context, f port.DiscountFilter) ([]port.DiscountUsageRow, error) {
	if err := requireYear(f.AcademicYearID, "the discount report"); err != nil {
		return nil, err
	}

	args := &argList{}
	year := args.next(*f.AcademicYearID)
	where := []string{"u.academic_year_id = " + year}
	if f.DefinitionID != nil {
		where = append(where, "u.definition_id = "+args.next(*f.DefinitionID))
	}
	if f.Category != nil {
		where = append(where, "u.category = "+args.next(*f.Category))
	}

	query := `
		WITH year_gross AS (
		    SELECT coalesce(sum(s.gross_total), 0) AS gross_total
		    FROM v_year_department_summary s
		    WHERE s.academic_year_id = ` + year + `
		)
		SELECT
		    u.definition_id, u.definition_code, u.definition_name, u.category,
		    u.version_no, u.value_type, u.value_bp, u.value_amount,
		    u.student_count::bigint, u.application_count::bigint,
		    u.total_discount::bigint, u.truncated_count::bigint,
		    CASE WHEN g.gross_total > 0
		         THEN round(100.0 * u.total_discount / g.gross_total, 2)::float8
		    END
		FROM v_discount_usage u
		CROSS JOIN year_gross g
		WHERE ` + joinWhere(where) + `
		ORDER BY u.total_discount DESC, u.definition_code, u.version_no`

	rows, err := r.db.Conn(ctx).Query(ctx, query, args.all()...)
	if err != nil {
		return nil, pg.WrapQuery("report.DiscountReport", err)
	}
	usage, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.DiscountUsageRow, error) {
		var out port.DiscountUsageRow
		if err := row.Scan(
			&out.DefinitionID, &out.DefinitionCode, &out.DefinitionName, &out.Category,
			&out.VersionNo, &out.ValueType, &out.ValueBP, &out.ValueAmount,
			&out.StudentCount, &out.ApplicationCount, &out.TotalDiscount, &out.TruncatedCount,
			&out.PctOfGross,
		); err != nil {
			return port.DiscountUsageRow{}, err
		}
		return out, nil
	})
	if err != nil {
		return nil, pg.WrapQuery("report.DiscountReport", err)
	}
	return usage, nil
}

// ExemptionRegister lists the students who ended up owing nothing, and whose
// signature put them there.
//
// Two populations meet the ministry's definition: a grant from a definition
// declared a full exemption, and a grant that happened to consume the entire
// discountable base. Reporting only the first would miss the student who was
// exempted in substance by a stack of partial reliefs.
func (r *ReportRepository) ExemptionRegister(ctx context.Context, f port.RegisterFilter) ([]port.ExemptionRow, int, error) {
	if err := requireYear(f.AcademicYearID, "the exemption register"); err != nil {
		return nil, 0, err
	}

	predicates := func(args *argList) []string {
		where := []string{
			"da.status = 'applied'",
			"fa.status <> 'cancelled'",
			"fa.academic_year_id = " + args.next(*f.AcademicYearID),
			`(dd.is_full_exemption
			      OR (fa.discountable_base > 0 AND da.applied_amount = fa.discountable_base))`,
		}
		if f.From != nil {
			where = append(where, "da.applied_at >= "+args.next(timeOrNil(f.From)))
		}
		if f.To != nil {
			where = append(where, "da.applied_at < "+args.next(exclusiveEnd(f.To)))
		}
		return where
	}

	const from = `
		FROM discount_application da
		JOIN financial_account fa ON fa.id = da.account_id
		JOIN discount_definition_version ddv ON ddv.id = da.definition_version_id
		JOIN discount_definition dd ON dd.id = ddv.definition_id
		JOIN discount_assignment asg ON asg.id = da.assignment_id
		JOIN student st ON st.id = fa.student_id
		JOIN academic_year ay ON ay.id = fa.academic_year_id
		JOIN department d ON d.id = fa.department_id
		LEFT JOIN app_user approver ON approver.id = asg.approved_by`

	args := &argList{}
	where := predicates(args)
	limit := boundedLimit(f.Limit, 50)
	offset := max(f.Offset, 0)

	query := `
		SELECT
		    da.id, st.id, st.student_no, st.full_name, st.mother_name,
		    fa.academic_year_id, ay.code, d.name_ar,
		    dd.code, dd.name_ar, dd.category, dd.is_full_exemption, ddv.version_no,
		    fa.discountable_base, da.applied_amount,
		    approver.full_name, asg.approved_at,
		    count(*) OVER () AS total_count` + from + `
		WHERE ` + joinWhere(where) + `
		ORDER BY st.student_no, dd.code
		LIMIT ` + args.next(limit) + ` OFFSET ` + args.next(offset)

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, args.all()...)
	if err != nil {
		return nil, 0, pg.WrapQuery("report.ExemptionRegister", err)
	}

	var total int64
	exemptions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.ExemptionRow, error) {
		var out port.ExemptionRow
		if err := row.Scan(
			&out.ApplicationID, &out.StudentID, &out.StudentNo, &out.FullName, &out.MotherName,
			&out.AcademicYearID, &out.AcademicYearCode, &out.DepartmentName,
			&out.DefinitionCode, &out.DefinitionName, &out.Category, &out.IsFullExemption, &out.VersionNo,
			&out.DiscountableBase, &out.AppliedAmount,
			&out.ApproverName, &out.ApprovedAt,
			&total,
		); err != nil {
			return port.ExemptionRow{}, err
		}
		return out, nil
	})
	if err != nil {
		return nil, 0, pg.WrapQuery("report.ExemptionRegister", err)
	}

	if len(exemptions) == 0 && offset > 0 {
		countArgs := &argList{}
		countQuery := `SELECT count(*)` + from + ` WHERE ` + joinWhere(predicates(countArgs))
		if err := q.QueryRow(ctx, countQuery, countArgs.all()...).Scan(&total); err != nil {
			return nil, 0, pg.WrapQuery("report.ExemptionRegister.count", err)
		}
	}
	return exemptions, int(total), nil
}

// ---------------------------------------------------------------------------
// Cash
// ---------------------------------------------------------------------------

// CashierDaily reports cash movement per cashier, day and method.
//
// v_cashier_daily counts a voided receipt in its void columns only — the status
// is one value, so the payment total already excludes it. Subtracting the void
// total from it would credit the drawer twice. The rule that makes this the
// right answer is the domain's: a reversal after the drawer has been counted
// must be a refund, never a void, so a same-day void nets to zero and nothing
// crosses a day boundary.
//
// The authoritative figure for reconciling one shift is the cashier session's
// expected cash, which also knows the opening float. This report is the day and
// method view over it.
func (r *ReportRepository) CashierDaily(ctx context.Context, f port.CashierDailyFilter) ([]port.CashierDayRow, error) {
	if err := requireDateRange(f.From, f.To, "the cashier daily report"); err != nil {
		return nil, err
	}

	args := &argList{}
	where := []string{
		"v.posting_date >= " + args.next(timeOrNil(f.From)),
		"v.posting_date <= " + args.next(timeOrNil(f.To)),
	}
	if f.CashierUserID != nil {
		where = append(where, "v.cashier_user_id = "+args.next(*f.CashierUserID))
	}

	// Sessions are summed away: a cashier who opened a second drawer after lunch
	// still worked one day, and the shift-level view belongs to the session
	// reconciliation rather than to this report.
	query := `
		SELECT
		    v.cashier_user_id, v.cashier_name, v.posting_date, v.method_code, v.is_cash,
		    coalesce(sum(v.payment_count), 0)::bigint,
		    coalesce(sum(v.payment_total), 0)::bigint,
		    coalesce(sum(v.void_count), 0)::bigint,
		    coalesce(sum(v.void_total), 0)::bigint,
		    CASE WHEN v.is_cash THEN coalesce(sum(v.payment_total), 0) ELSE 0 END::bigint
		FROM v_cashier_daily v
		WHERE ` + joinWhere(where) + `
		GROUP BY v.cashier_user_id, v.cashier_name, v.posting_date, v.method_code, v.is_cash
		ORDER BY v.posting_date DESC, v.cashier_name, v.method_code`

	rows, err := r.db.Conn(ctx).Query(ctx, query, args.all()...)
	if err != nil {
		return nil, pg.WrapQuery("report.CashierDaily", err)
	}
	days, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.CashierDayRow, error) {
		var (
			out  port.CashierDayRow
			date time.Time
		)
		if err := row.Scan(
			&out.CashierUserID, &out.CashierName, &date, &out.MethodCode, &out.IsCash,
			&out.PaymentCount, &out.PaymentTotal, &out.VoidCount, &out.VoidTotal, &out.ExpectedCash,
		); err != nil {
			return port.CashierDayRow{}, err
		}
		out.Date = shared.DateFromTime(date)
		return out, nil
	})
	if err != nil {
		return nil, pg.WrapQuery("report.CashierDaily", err)
	}
	return days, nil
}

// CollectionTrend reports how a year's money arrived: month by month against
// the obligation the year raised, and across its departments.
//
// Only accounts of the year are counted. Cash taken this year against an
// earlier year's account is the year summary's separate prior-year line, and
// mixing it in here would show a month collecting more than the year ever
// charged.
func (r *ReportRepository) CollectionTrend(ctx context.Context, f port.TrendFilter) (*port.CollectionTrend, error) {
	if err := requireYear(f.AcademicYearID, "the collection trend"); err != nil {
		return nil, err
	}

	summaryFilter := port.SummaryFilter{
		AcademicYearID: f.AcademicYearID,
		CollegeID:      f.CollegeID,
		DepartmentID:   f.DepartmentID,
	}

	trend := port.CollectionTrend{AcademicYearID: *f.AcademicYearID}

	months, net, err := r.trendMonths(ctx, f)
	if err != nil {
		return nil, err
	}
	trend.EffectiveNet = net
	trend.Months = months

	if trend.Departments, err = r.trendDepartments(ctx, summaryFilter); err != nil {
		return nil, err
	}
	return &trend, nil
}

func (r *ReportRepository) trendMonths(ctx context.Context, f port.TrendFilter) ([]port.CollectionMonth, money.Amount, error) {
	args := &argList{}
	year := args.next(*f.AcademicYearID)
	scope := []string{"fa.academic_year_id = " + year, "fa.status <> 'cancelled'"}
	if f.CollegeID != nil {
		scope = append(scope, "fa.college_id = "+args.next(*f.CollegeID))
	}
	if f.DepartmentID != nil {
		scope = append(scope, "fa.department_id = "+args.next(*f.DepartmentID))
	}

	// Organisational scope, enforced in the query rather than after it.
	if predicate := reportScope(f.Scope, "fa", args); predicate != "" {
		scope = append(scope, predicate)
	}
	accountScope := joinWhere(scope)

	// Refunds are netted into the month they were paid out, not the month of the
	// receipt they reverse: the collection curve is a record of cash movement,
	// and rewriting a closed month is exactly what an audit trail forbids.
	query := `
		WITH movement AS (
		    SELECT
		        date_trunc('month', p.posted_at AT TIME ZONE 'UTC')::date AS month_start,
		        1::bigint AS payment_count, p.amount AS collected, 0::bigint AS refunded
		    FROM payment p
		    JOIN financial_account fa ON fa.id = p.account_id
		    WHERE p.status = 'posted' AND ` + accountScope + `
		    UNION ALL
		    SELECT
		        date_trunc('month', rf.posted_at AT TIME ZONE 'UTC')::date,
		        0::bigint, 0::bigint, rf.amount
		    FROM refund rf
		    JOIN financial_account fa ON fa.id = rf.account_id
		    WHERE rf.status = 'posted' AND ` + accountScope + `
		),
		obligation AS (
		    SELECT coalesce(sum(b.effective_net), 0)::bigint AS effective_net
		    FROM financial_account fa
		    JOIN v_account_balance b ON b.account_id = fa.id
		    WHERE ` + accountScope + `
		)
		SELECT
		    m.month_start,
		    sum(m.payment_count)::bigint,
		    coalesce(sum(m.collected), 0)::bigint,
		    coalesce(sum(m.refunded), 0)::bigint,
		    (coalesce(sum(m.collected), 0) - coalesce(sum(m.refunded), 0))::bigint,
		    sum(coalesce(sum(m.collected), 0) - coalesce(sum(m.refunded), 0))
		        OVER (ORDER BY m.month_start)::bigint,
		    CASE WHEN o.effective_net > 0
		         THEN round(100.0 * (coalesce(sum(m.collected), 0) - coalesce(sum(m.refunded), 0))
		                    / o.effective_net, 2)::float8
		    END,
		    o.effective_net
		FROM movement m
		CROSS JOIN obligation o
		GROUP BY m.month_start, o.effective_net
		ORDER BY m.month_start`

	rows, err := r.db.Conn(ctx).Query(ctx, query, args.all()...)
	if err != nil {
		return nil, 0, pg.WrapQuery("report.CollectionTrend.months", err)
	}
	defer rows.Close()

	var (
		months []port.CollectionMonth
		net    money.Amount
	)
	for rows.Next() {
		var (
			month      port.CollectionMonth
			monthStart time.Time
		)
		if err := rows.Scan(
			&monthStart, &month.PaymentCount, &month.Collected, &month.Refunded,
			&month.NetCollected, &month.CumulativeNet, &month.PctOfNet, &net,
		); err != nil {
			return nil, 0, pg.WrapQuery("report.CollectionTrend.months", err)
		}
		month.Month = monthStart.Format("2006-01")
		month.MonthDate = shared.DateFromTime(monthStart)
		months = append(months, month)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, pg.WrapQuery("report.CollectionTrend.months", err)
	}

	// A year with an obligation but no collection yet returns no movement rows,
	// so the net has to be read on its own rather than left at zero.
	if len(months) == 0 {
		netArgs := &argList{}
		netScope := []string{"fa.academic_year_id = " + netArgs.next(*f.AcademicYearID), "fa.status <> 'cancelled'"}
		if f.CollegeID != nil {
			netScope = append(netScope, "fa.college_id = "+netArgs.next(*f.CollegeID))
		}
		if f.DepartmentID != nil {
			netScope = append(netScope, "fa.department_id = "+netArgs.next(*f.DepartmentID))
		}

		// Organisational scope, enforced in the query rather than after it.
		if predicate := reportScope(f.Scope, "fa", netArgs); predicate != "" {
			netScope = append(netScope, predicate)
		}
		netQuery := `
			SELECT coalesce(sum(b.effective_net), 0)::bigint
			FROM financial_account fa
			JOIN v_account_balance b ON b.account_id = fa.id
			WHERE ` + joinWhere(netScope)
		if err := r.db.Conn(ctx).QueryRow(ctx, netQuery, netArgs.all()...).Scan(&net); err != nil {
			return nil, 0, pg.WrapQuery("report.CollectionTrend.net", err)
		}
	}
	return months, net, nil
}

func (r *ReportRepository) trendDepartments(ctx context.Context, f port.SummaryFilter) ([]port.CollectionDepartment, error) {
	args := &argList{}
	query := `
		SELECT
		    s.department_id, d.name_ar,
		    coalesce(sum(s.effective_net_total), 0)::bigint,
		    coalesce(sum(s.paid_total), 0)::bigint,
		    coalesce(sum(s.remaining_total), 0)::bigint,
		    CASE WHEN sum(s.effective_net_total) > 0
		         THEN round(100.0 * sum(s.paid_total) / sum(s.effective_net_total), 2)::float8
		    END
		FROM v_year_department_summary s
		JOIN department d ON d.id = s.department_id
		WHERE ` + joinWhere(summaryPredicates(f, "s", args)) + `
		GROUP BY s.department_id, d.name_ar
		ORDER BY d.name_ar`

	rows, err := r.db.Conn(ctx).Query(ctx, query, args.all()...)
	if err != nil {
		return nil, pg.WrapQuery("report.CollectionTrend.departments", err)
	}
	departments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.CollectionDepartment, error) {
		var out port.CollectionDepartment
		if err := row.Scan(
			&out.DepartmentID, &out.DepartmentName,
			&out.EffectiveNet, &out.Paid, &out.Remaining, &out.PctCollected,
		); err != nil {
			return port.CollectionDepartment{}, err
		}
		return out, nil
	})
	if err != nil {
		return nil, pg.WrapQuery("report.CollectionTrend.departments", err)
	}
	return departments, nil
}

// ---------------------------------------------------------------------------
// Void and refund registers
// ---------------------------------------------------------------------------

// VoidRegister lists every reversed collection, widest time gap first.
//
// The gap between posting a receipt and reversing it is what this report is
// for, and why it leads the sort. A void raised minutes after the receipt is a
// cashier correcting a typo. One raised the next day, against a receipt the
// student is holding and cash the drawer has already been counted for, is the
// shape of money leaving the university with the record erased behind it — and
// the domain's own rule is that such a reversal must be a refund, which is why
// crossing a day boundary is flagged on the row.
func (r *ReportRepository) VoidRegister(ctx context.Context, f port.RegisterFilter) ([]port.VoidRow, int, error) {
	if err := requireYear(f.AcademicYearID, "the void register"); err != nil {
		return nil, 0, err
	}

	predicates := func(args *argList) []string {
		where := []string{
			"p.status = 'voided'",
			"p.posting_year_id = " + args.next(*f.AcademicYearID),
		}
		if f.From != nil {
			where = append(where, "p.voided_at >= "+args.next(timeOrNil(f.From)))
		}
		if f.To != nil {
			where = append(where, "p.voided_at < "+args.next(exclusiveEnd(f.To)))
		}
		return where
	}

	const from = `
		FROM payment p
		JOIN student st ON st.id = p.student_id
		JOIN payment_method pm ON pm.id = p.payment_method_id
		JOIN app_user cashier ON cashier.id = p.cashier_user_id
		LEFT JOIN void_request vr ON vr.id = p.void_request_id
		LEFT JOIN app_user requester ON requester.id = vr.requested_by
		LEFT JOIN app_user executor ON executor.id = coalesce(vr.executed_by, p.voided_by)`

	args := &argList{}
	where := predicates(args)
	limit := boundedLimit(f.Limit, 50)
	offset := max(f.Offset, 0)

	query := `
		SELECT
		    p.id, p.receipt_no, p.student_id, st.student_no, st.full_name,
		    p.amount, pm.code, p.posted_at, p.voided_at,
		    round((extract(epoch FROM (p.voided_at - p.posted_at)) / 3600.0)::numeric, 2)::float8,
		    ((p.voided_at AT TIME ZONE 'UTC')::date > (p.posted_at AT TIME ZONE 'UTC')::date),
		    coalesce(p.void_reason, vr.reason, ''),
		    cashier.full_name, requester.full_name, vr.requested_at, executor.full_name, vr.id,
		    count(*) OVER () AS total_count` + from + `
		WHERE ` + joinWhere(where) + `
		ORDER BY (p.voided_at - p.posted_at) DESC, p.voided_at DESC
		LIMIT ` + args.next(limit) + ` OFFSET ` + args.next(offset)

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, args.all()...)
	if err != nil {
		return nil, 0, pg.WrapQuery("report.VoidRegister", err)
	}

	var total int64
	voids, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.VoidRow, error) {
		var out port.VoidRow
		if err := row.Scan(
			&out.PaymentID, &out.ReceiptNo, &out.StudentID, &out.StudentNo, &out.FullName,
			&out.Amount, &out.MethodCode, &out.PostedAt, &out.VoidedAt,
			&out.GapHours, &out.CrossedDay, &out.Reason,
			&out.CashierName, &out.RequestedBy, &out.RequestedAt, &out.ExecutedBy, &out.VoidRequestID,
			&total,
		); err != nil {
			return port.VoidRow{}, err
		}
		return out, nil
	})
	if err != nil {
		return nil, 0, pg.WrapQuery("report.VoidRegister", err)
	}

	if len(voids) == 0 && offset > 0 {
		countArgs := &argList{}
		countQuery := `SELECT count(*)` + from + ` WHERE ` + joinWhere(predicates(countArgs))
		if err := q.QueryRow(ctx, countQuery, countArgs.all()...).Scan(&total); err != nil {
			return nil, 0, pg.WrapQuery("report.VoidRegister.count", err)
		}
	}
	return voids, int(total), nil
}

// RefundRegister lists every posted refund with both signatures on it.
//
// Only posted refunds appear: a request that was rejected returned no money,
// and this register answers what left the university.
func (r *ReportRepository) RefundRegister(ctx context.Context, f port.RegisterFilter) ([]port.RefundRow, int, error) {
	if err := requireYear(f.AcademicYearID, "the refund register"); err != nil {
		return nil, 0, err
	}

	predicates := func(args *argList) []string {
		where := []string{
			"rf.status = 'posted'",
			"rf.posting_year_id = " + args.next(*f.AcademicYearID),
		}
		if f.From != nil {
			where = append(where, "rf.posted_at >= "+args.next(timeOrNil(f.From)))
		}
		if f.To != nil {
			where = append(where, "rf.posted_at < "+args.next(exclusiveEnd(f.To)))
		}
		return where
	}

	const from = `
		FROM refund rf
		JOIN payment p ON p.id = rf.payment_id
		JOIN student st ON st.id = rf.student_id
		JOIN payment_method pm ON pm.id = rf.payment_method_id
		JOIN app_user requester ON requester.id = rf.requested_by
		LEFT JOIN app_user approver ON approver.id = rf.approved_by`

	args := &argList{}
	where := predicates(args)
	limit := boundedLimit(f.Limit, 50)
	offset := max(f.Offset, 0)

	query := `
		SELECT
		    rf.id, rf.refund_no, rf.posted_at, p.receipt_no,
		    rf.student_id, st.student_no, st.full_name,
		    rf.amount, pm.code, rf.reason,
		    requester.full_name, approver.full_name, rf.approved_at,
		    count(*) OVER () AS total_count` + from + `
		WHERE ` + joinWhere(where) + `
		ORDER BY rf.posted_at DESC, rf.refund_no
		LIMIT ` + args.next(limit) + ` OFFSET ` + args.next(offset)

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, args.all()...)
	if err != nil {
		return nil, 0, pg.WrapQuery("report.RefundRegister", err)
	}

	var total int64
	refunds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.RefundRow, error) {
		var out port.RefundRow
		if err := row.Scan(
			&out.RefundID, &out.RefundNo, &out.PostedAt, &out.OriginalReceipt,
			&out.StudentID, &out.StudentNo, &out.FullName,
			&out.Amount, &out.MethodCode, &out.Reason,
			&out.RequestedBy, &out.ApprovedBy, &out.ApprovedAt,
			&total,
		); err != nil {
			return port.RefundRow{}, err
		}
		return out, nil
	})
	if err != nil {
		return nil, 0, pg.WrapQuery("report.RefundRegister", err)
	}

	if len(refunds) == 0 && offset > 0 {
		countArgs := &argList{}
		countQuery := `SELECT count(*)` + from + ` WHERE ` + joinWhere(predicates(countArgs))
		if err := q.QueryRow(ctx, countQuery, countArgs.all()...).Scan(&total); err != nil {
			return nil, 0, pg.WrapQuery("report.RefundRegister.count", err)
		}
	}
	return refunds, int(total), nil
}

// ---------------------------------------------------------------------------
// Student statement
// ---------------------------------------------------------------------------

// statementScope restricts a child query of the statement to the accounts the
// statement covers. Every child query uses it with its own alias, so all six
// read exactly the same set of accounts as the account blocks they fill in.
func statementScope(alias string) string {
	return alias + `account_id IN (
		      SELECT fa.id FROM financial_account fa
		      WHERE fa.student_id = $1
		        AND ($2::uuid IS NULL OR fa.academic_year_id = $2)
		  )`
}

// StudentStatement assembles one student's whole file from transaction rows.
//
// Nothing here reads a cached total. This is the document a student disputing a
// balance is handed and the one an auditor reconstructs a charge from, so every
// figure on it has to be derivable from the rows printed beside it.
func (r *ReportRepository) StudentStatement(
	ctx context.Context, studentID shared.ID, yearID *shared.ID,
) (*port.StudentStatement, error) {
	q := r.db.Conn(ctx)

	var statement port.StudentStatement
	const identity = `
		SELECT s.id, s.student_no, s.full_name, s.mother_name, s.phone, s.status
		FROM student s WHERE s.id = $1`
	if err := q.QueryRow(ctx, identity, studentID).Scan(
		&statement.Student.ID, &statement.Student.StudentNo, &statement.Student.FullName,
		&statement.Student.MotherName, &statement.Student.Phone, &statement.Student.Status,
	); err != nil {
		return nil, pg.WrapQuery("report.StudentStatement", err)
	}

	accounts, err := r.statementAccounts(ctx, studentID, yearID)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		statement.Accounts = []port.StatementAccount{}
		return &statement, nil
	}

	index := make(map[shared.ID]*port.StatementAccount, len(accounts))
	for i := range accounts {
		index[accounts[i].AccountID] = &accounts[i]
	}
	for _, load := range []func(context.Context, shared.ID, *shared.ID, map[shared.ID]*port.StatementAccount) error{
		r.statementComponents,
		r.statementDiscounts,
		r.statementAdjustments,
		r.statementInstallments,
		r.statementPayments,
		r.statementRefunds,
	} {
		if err := load(ctx, studentID, yearID, index); err != nil {
			return nil, err
		}
	}

	totals, err := statementTotals(accounts)
	if err != nil {
		return nil, err
	}
	statement.Accounts = accounts
	statement.Totals = totals
	return &statement, nil
}

// statementAccounts reads the per-year blocks.
//
// Cancelled accounts are included and flagged. A cancelled account is usually
// the reason a second one exists against the same enrollment, and a statement
// that hides it cannot explain why the student sees two sets of fees.
func (r *ReportRepository) statementAccounts(
	ctx context.Context, studentID shared.ID, yearID *shared.ID,
) ([]port.StatementAccount, error) {
	const query = `
		SELECT
		    b.account_id, b.enrollment_id, b.academic_year_id, ay.code,
		    c.name_ar, d.name_ar, t.name_ar,
		    b.stage, e.attempt_number, e.enrollment_kind, e.enrollment_status, b.account_status,
		    b.gross_total, b.discountable_base, b.discount_total, b.net_snapshot,
		    b.adjustment_total::bigint, b.effective_net::bigint,
		    b.paid_gross::bigint, b.refunded_total::bigint, b.net_paid::bigint,
		    b.credit_balance::bigint, b.remaining::bigint
		FROM v_account_balance b
		JOIN financial_account fa ON fa.id = b.account_id
		JOIN enrollment e ON e.id = b.enrollment_id
		JOIN academic_year ay ON ay.id = b.academic_year_id
		JOIN college c ON c.id = b.college_id
		JOIN department d ON d.id = b.department_id
		JOIN study_type t ON t.id = b.study_type_id
		WHERE b.student_id = $1 AND ($2::uuid IS NULL OR b.academic_year_id = $2)
		ORDER BY ay.start_date, fa.generated_at`

	rows, err := r.db.Conn(ctx).Query(ctx, query, studentID, yearID)
	if err != nil {
		return nil, pg.WrapQuery("report.StudentStatement.accounts", err)
	}
	accounts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.StatementAccount, error) {
		var a port.StatementAccount
		if err := row.Scan(
			&a.AccountID, &a.EnrollmentID, &a.AcademicYearID, &a.AcademicYearCode,
			&a.CollegeName, &a.DepartmentName, &a.StudyTypeName,
			&a.Stage, &a.AttemptNumber, &a.EnrollmentKind, &a.EnrollmentStatus, &a.AccountStatus,
			&a.GrossTotal, &a.DiscountableBase, &a.DiscountTotal, &a.NetSnapshot,
			&a.AdjustmentTotal, &a.EffectiveNet,
			&a.PaidGross, &a.RefundedTotal, &a.NetPaid, &a.CreditBalance, &a.Remaining,
		); err != nil {
			return port.StatementAccount{}, err
		}
		// Empty lists must render as [] and not null, for the reason the page
		// envelope gives: a client iterating a field without a nil check is
		// common, and an account with no refunds is not worth a crashed screen.
		a.FeeComponents = []port.StatementFeeComponent{}
		a.Discounts = []port.StatementDiscount{}
		a.Adjustments = []port.StatementAdjustment{}
		a.Installments = []port.StatementInstallment{}
		a.Payments = []port.StatementPayment{}
		a.Refunds = []port.StatementRefund{}
		return a, nil
	})
	if err != nil {
		return nil, pg.WrapQuery("report.StudentStatement.accounts", err)
	}
	return accounts, nil
}

func (r *ReportRepository) statementComponents(
	ctx context.Context, studentID shared.ID, yearID *shared.ID, index map[shared.ID]*port.StatementAccount,
) error {
	query := `
		SELECT account_id, component_code, name_ar, amount, is_discountable, is_refundable
		FROM fee_snapshot_line
		WHERE ` + statementScope("") + `
		ORDER BY account_id, sort_order, component_code`

	rows, err := r.db.Conn(ctx).Query(ctx, query, studentID, yearID)
	if err != nil {
		return pg.WrapQuery("report.StudentStatement.components", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			accountID shared.ID
			line      port.StatementFeeComponent
		)
		if err := rows.Scan(&accountID, &line.ComponentCode, &line.NameAr,
			&line.Amount, &line.IsDiscountable, &line.IsRefundable); err != nil {
			return pg.WrapQuery("report.StudentStatement.components", err)
		}
		if account := index[accountID]; account != nil {
			account.FeeComponents = append(account.FeeComponents, line)
		}
	}
	return pg.WrapQuery("report.StudentStatement.components", rows.Err())
}

// statementDiscounts lists every application, whatever its status.
//
// A reversed or declined grant is part of the story: it explains why a student
// who was told they had a discount is being charged in full, and removing it
// would leave that argument unanswerable.
func (r *ReportRepository) statementDiscounts(
	ctx context.Context, studentID shared.ID, yearID *shared.ID, index map[shared.ID]*port.StatementAccount,
) error {
	query := `
		SELECT
		    da.account_id, da.id, dd.code, dd.name_ar, dd.category,
		    ddv.version_no, ddv.value_type,
		    da.frozen_base_amount, da.computed_amount, da.applied_amount,
		    da.truncation_reason, da.status, da.applied_at
		FROM discount_application da
		JOIN discount_definition_version ddv ON ddv.id = da.definition_version_id
		JOIN discount_definition dd ON dd.id = ddv.definition_id
		WHERE ` + statementScope("da.") + `
		ORDER BY da.account_id, da.application_sequence, dd.code`

	rows, err := r.db.Conn(ctx).Query(ctx, query, studentID, yearID)
	if err != nil {
		return pg.WrapQuery("report.StudentStatement.discounts", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			accountID shared.ID
			d         port.StatementDiscount
		)
		if err := rows.Scan(&accountID, &d.ApplicationID, &d.DefinitionCode, &d.DefinitionName,
			&d.Category, &d.VersionNo, &d.ValueType,
			&d.FrozenBase, &d.ComputedAmount, &d.AppliedAmount,
			&d.TruncationReason, &d.Status, &d.AppliedAt); err != nil {
			return pg.WrapQuery("report.StudentStatement.discounts", err)
		}
		if account := index[accountID]; account != nil {
			account.Discounts = append(account.Discounts, d)
		}
	}
	return pg.WrapQuery("report.StudentStatement.discounts", rows.Err())
}

func (r *ReportRepository) statementAdjustments(
	ctx context.Context, studentID shared.ID, yearID *shared.ID, index map[shared.ID]*port.StatementAccount,
) error {
	query := `
		SELECT account_id, id, adjustment_type, amount, reason, posted_at
		FROM account_adjustment
		WHERE ` + statementScope("") + `
		ORDER BY account_id, posted_at, id`

	rows, err := r.db.Conn(ctx).Query(ctx, query, studentID, yearID)
	if err != nil {
		return pg.WrapQuery("report.StudentStatement.adjustments", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			accountID shared.ID
			adj       port.StatementAdjustment
		)
		if err := rows.Scan(&accountID, &adj.ID, &adj.Type, &adj.Amount, &adj.Reason, &adj.PostedAt); err != nil {
			return pg.WrapQuery("report.StudentStatement.adjustments", err)
		}
		if account := index[accountID]; account != nil {
			account.Adjustments = append(account.Adjustments, adj)
		}
	}
	return pg.WrapQuery("report.StudentStatement.adjustments", rows.Err())
}

func (r *ReportRepository) statementInstallments(
	ctx context.Context, studentID shared.ID, yearID *shared.ID, index map[shared.ID]*port.StatementAccount,
) error {
	query := `
		SELECT
		    vi.account_id, vi.installment_id, vi.installment_no, vi.due_date, vi.amount,
		    vi.allocated_paid::bigint, vi.remaining::bigint,
		    vi.effective_status, vi.is_overdue, vi.days_overdue
		FROM v_installment_status vi
		WHERE ` + statementScope("vi.") + `
		ORDER BY vi.account_id, vi.due_date, vi.installment_no`

	rows, err := r.db.Conn(ctx).Query(ctx, query, studentID, yearID)
	if err != nil {
		return pg.WrapQuery("report.StudentStatement.installments", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			accountID shared.ID
			due       time.Time
			i         port.StatementInstallment
		)
		if err := rows.Scan(&accountID, &i.InstallmentID, &i.Number, &due, &i.Amount,
			&i.AllocatedPaid, &i.Remaining, &i.Status, &i.IsOverdue, &i.DaysOverdue); err != nil {
			return pg.WrapQuery("report.StudentStatement.installments", err)
		}
		i.DueDate = shared.DateFromTime(due)
		if account := index[accountID]; account != nil {
			account.Installments = append(account.Installments, i)
		}
	}
	return pg.WrapQuery("report.StudentStatement.installments", rows.Err())
}

// statementPayments lists posted and voided collections.
//
// A voided receipt stays on the statement with its void stamp: the student is
// holding the paper, and a document that cannot account for it is no use in the
// argument it exists to settle. Drafts are left out — an unposted payment is
// not money.
func (r *ReportRepository) statementPayments(
	ctx context.Context, studentID shared.ID, yearID *shared.ID, index map[shared.ID]*port.StatementAccount,
) error {
	query := `
		SELECT
		    p.account_id, p.id, p.receipt_no, p.amount, pm.code, p.status,
		    p.paid_at, p.posted_at, p.voided_at, p.void_reason, u.full_name, p.payer_name
		FROM payment p
		JOIN payment_method pm ON pm.id = p.payment_method_id
		JOIN app_user u ON u.id = p.cashier_user_id
		WHERE p.status IN ('posted', 'voided')
		  AND ` + statementScope("p.") + `
		ORDER BY p.account_id, p.posted_at, p.paid_at`

	rows, err := r.db.Conn(ctx).Query(ctx, query, studentID, yearID)
	if err != nil {
		return pg.WrapQuery("report.StudentStatement.payments", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			accountID shared.ID
			p         port.StatementPayment
		)
		if err := rows.Scan(&accountID, &p.PaymentID, &p.ReceiptNo, &p.Amount, &p.MethodCode,
			&p.Status, &p.PaidAt, &p.PostedAt, &p.VoidedAt, &p.VoidReason,
			&p.CashierName, &p.PayerName); err != nil {
			return pg.WrapQuery("report.StudentStatement.payments", err)
		}
		if account := index[accountID]; account != nil {
			account.Payments = append(account.Payments, p)
		}
	}
	return pg.WrapQuery("report.StudentStatement.payments", rows.Err())
}

func (r *ReportRepository) statementRefunds(
	ctx context.Context, studentID shared.ID, yearID *shared.ID, index map[shared.ID]*port.StatementAccount,
) error {
	query := `
		SELECT
		    rf.account_id, rf.id, rf.refund_no, p.receipt_no, rf.amount,
		    rf.reason, rf.status, rf.requested_at, rf.posted_at
		FROM refund rf
		JOIN payment p ON p.id = rf.payment_id
		WHERE ` + statementScope("rf.") + `
		ORDER BY rf.account_id, rf.requested_at`

	rows, err := r.db.Conn(ctx).Query(ctx, query, studentID, yearID)
	if err != nil {
		return pg.WrapQuery("report.StudentStatement.refunds", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			accountID shared.ID
			rf        port.StatementRefund
		)
		if err := rows.Scan(&accountID, &rf.RefundID, &rf.RefundNo, &rf.ReceiptNo, &rf.Amount,
			&rf.Reason, &rf.Status, &rf.RequestedAt, &rf.PostedAt); err != nil {
			return pg.WrapQuery("report.StudentStatement.refunds", err)
		}
		if account := index[accountID]; account != nil {
			account.Refunds = append(account.Refunds, rf)
		}
	}
	return pg.WrapQuery("report.StudentStatement.refunds", rows.Err())
}

// statementTotals adds the account blocks up.
//
// Credit is totalled beside the debt and never against it: money held on an
// overpaid year does not settle what another year is owed, and offsetting the
// two here would print a balance the finance office cannot act on.
func statementTotals(accounts []port.StatementAccount) (port.StatementTotals, error) {
	gross := make([]money.Amount, 0, len(accounts))
	discount := make([]money.Amount, 0, len(accounts))
	net := make([]money.Amount, 0, len(accounts))
	paid := make([]money.Amount, 0, len(accounts))
	remaining := make([]money.Amount, 0, len(accounts))
	credit := make([]money.Amount, 0, len(accounts))

	for _, a := range accounts {
		gross = append(gross, a.GrossTotal)
		discount = append(discount, a.DiscountTotal)
		net = append(net, a.EffectiveNet)
		paid = append(paid, a.NetPaid)
		remaining = append(remaining, a.Remaining)
		credit = append(credit, a.CreditBalance)
	}

	totals := port.StatementTotals{AccountCount: len(accounts)}
	for _, sum := range []struct {
		into   *money.Amount
		values []money.Amount
	}{
		{&totals.GrossTotal, gross},
		{&totals.DiscountTotal, discount},
		{&totals.EffectiveNet, net},
		{&totals.NetPaid, paid},
		{&totals.Remaining, remaining},
		{&totals.CreditBalance, credit},
	} {
		value, err := money.Sum(sum.values...)
		if err != nil {
			return port.StatementTotals{}, shared.Internal("report.statement_overflow", err,
				"totalling a student statement overflowed")
		}
		*sum.into = value
	}
	return totals, nil
}

// derefString reads a column that a grouping set left NULL on a subtotal row.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// reportScope renders an organisational scope as a predicate on a report query.
//
// Every aggregate in this file carries college_id and department_id on the row
// or the view it selects from, so one helper serves all of them. An
// unrestricted scope adds nothing; a scoped caller with no grants gets "false",
// which returns an empty report rather than the whole university.
func reportScope(scope shared.ScopeFilter, alias string, args *argList) string {
	if scope.Unrestricted() {
		return ""
	}
	if scope.Empty() {
		return "false"
	}

	var clauses []string
	if len(scope.Colleges) > 0 {
		clauses = append(clauses, alias+".college_id = ANY("+args.next(scope.Colleges)+")")
	}
	if len(scope.Departments) > 0 {
		clauses = append(clauses, alias+".department_id = ANY("+args.next(scope.Departments)+")")
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}
