-- Reporting read models.
--
-- Every figure a report prints, an auditor questions, or a receipt shows is
-- derived here from transaction rows — never from the cached totals on
-- financial_account. The caches exist so a list view can render quickly; these
-- views exist so that the number is right. The reconciliation view at the
-- bottom compares the two, and a non-empty result from it is a bug.

-- ---------------------------------------------------------------------------
-- Enrollment, counted correctly
-- ---------------------------------------------------------------------------

-- Enrollments that represent a real seat this year. Superseded rows are
-- excluded, so a student who changed department in March is one student, not
-- two.
--
-- Counts come from here. Money does not: the account behind a superseded
-- enrollment may hold real cash that a student really handed over, and
-- dropping it would leave the year's revenue disagreeing with the cashiers'
-- drawers. Money comes from every account of the year, with the transfer
-- adjustments netting each supersede pair back to one economic position.
CREATE VIEW v_enrollment_effective AS
SELECT
    e.id                    AS enrollment_id,
    e.student_id,
    e.academic_year_id,
    e.college_id,
    e.department_id,
    e.study_type_id,
    e.student_category_id,
    e.stage,
    e.attempt_number,
    e.enrollment_kind,
    e.enrollment_status,
    e.academic_result,
    e.result_by_decision,
    e.sequence_no,
    e.previous_enrollment_id,
    (e.attempt_number > 1)  AS is_repeat,
    (e.enrollment_kind = 'hosted_in') AS is_hosted_in
FROM enrollment e
WHERE e.enrollment_status <> 'superseded';

COMMENT ON VIEW v_enrollment_effective IS
    'One row per real seat: superseded enrollments excluded. Use for head counts. '
    'Do not use as the spine of a money report — the excluded rows may carry '
    'collected cash.';

-- ---------------------------------------------------------------------------
-- Account balance
-- ---------------------------------------------------------------------------

-- The authoritative financial position of one account.
--
-- effective_net is the amount actually owed: the frozen net from the snapshot
-- plus every adjustment posted since. The snapshot never moves, so the reason
-- for any difference between the original fee and today's obligation is always
-- a readable list of adjustment rows.
CREATE VIEW v_account_balance AS
SELECT
    fa.id                                       AS account_id,
    fa.enrollment_id,
    fa.student_id,
    fa.academic_year_id,
    fa.college_id,
    fa.department_id,
    fa.study_type_id,
    fa.stage,
    fa.status                                   AS account_status,

    fa.gross_total,
    fa.discountable_base,
    fa.discount_total,
    fa.net_total                                AS net_snapshot,
    coalesce(adj.total, 0)                      AS adjustment_total,
    fa.net_total + coalesce(adj.total, 0)       AS effective_net,

    coalesce(pay.total, 0)                      AS paid_gross,
    coalesce(ref.total, 0)                      AS refunded_total,
    coalesce(pay.total, 0) - coalesce(ref.total, 0) AS net_paid,

    (fa.net_total + coalesce(adj.total, 0))
        - (coalesce(pay.total, 0) - coalesce(ref.total, 0)) AS remaining,

    coalesce(cred.open_balance, 0)              AS credit_balance,
    coalesce(pay.payment_count, 0)              AS payment_count,
    coalesce(ref.refund_count, 0)               AS refund_count,
    pay.last_payment_at
FROM financial_account fa
LEFT JOIN LATERAL (
    SELECT sum(a.amount) AS total
    FROM account_adjustment a
    WHERE a.account_id = fa.id
) adj ON true
LEFT JOIN LATERAL (
    SELECT
        sum(p.amount)       AS total,
        count(*)            AS payment_count,
        max(p.posted_at)    AS last_payment_at
    FROM payment p
    WHERE p.account_id = fa.id
      AND p.status = 'posted'
) pay ON true
LEFT JOIN LATERAL (
    SELECT sum(r.amount) AS total, count(*) AS refund_count
    FROM refund r
    WHERE r.account_id = fa.id
      AND r.status = 'posted'
) ref ON true
LEFT JOIN LATERAL (
    SELECT sum(c.amount - c.consumed_amount) AS open_balance
    FROM credit_entry c
    WHERE c.account_id = fa.id
      AND c.status IN ('open', 'partially_consumed')
) cred ON true;

COMMENT ON VIEW v_account_balance IS
    'Authoritative per-account position computed from transaction rows. '
    'effective_net = frozen net + adjustments; net_paid = posted payments - posted refunds.';

-- ---------------------------------------------------------------------------
-- Installments
-- ---------------------------------------------------------------------------

-- Overdue is computed here and stored nowhere. It is a statement about today,
-- and a stored flag would need a nightly sweep whose failure silently
-- misreports the debt.
CREATE VIEW v_installment_status AS
SELECT
    i.id                    AS installment_id,
    i.account_id,
    fa.student_id,
    fa.academic_year_id,
    fa.department_id,
    fa.study_type_id,
    fa.stage,
    i.installment_no,
    i.due_date,
    i.amount,
    coalesce(alloc.allocated, 0)            AS allocated_paid,
    i.amount - coalesce(alloc.allocated, 0) AS remaining,
    i.status                                AS stored_status,
    CASE
        WHEN i.status IN ('waived', 'superseded') THEN i.status
        WHEN coalesce(alloc.allocated, 0) >= i.amount THEN 'paid'
        WHEN i.due_date < current_date THEN 'overdue'
        WHEN coalesce(alloc.allocated, 0) > 0 THEN 'partially_paid'
        ELSE 'pending'
    END                                     AS effective_status,
    (i.status NOT IN ('waived', 'superseded')
        AND coalesce(alloc.allocated, 0) < i.amount
        AND i.due_date < current_date)      AS is_overdue,
    CASE
        WHEN i.due_date < current_date THEN current_date - i.due_date
        ELSE 0
    END                                     AS days_overdue
FROM installment i
JOIN financial_account fa ON fa.id = i.account_id
LEFT JOIN LATERAL (
    -- Allocations net of their reversals. A reversal row carries a positive
    -- amount and a link to what it undoes, so the net is a signed sum rather
    -- than a filter on a flag.
    SELECT
        coalesce(sum(CASE WHEN pa.entry_type = 'allocation' THEN pa.amount ELSE -pa.amount END), 0)
            AS allocated
    FROM payment_allocation pa
    JOIN payment p ON p.id = pa.payment_id
    WHERE pa.installment_id = i.id
      AND p.status IN ('posted', 'voided')
) alloc ON true;

COMMENT ON VIEW v_installment_status IS
    'Per-installment position with overdue derived from due_date and today. '
    'allocated_paid nets reversal rows against allocation rows.';

-- ---------------------------------------------------------------------------
-- Debt
-- ---------------------------------------------------------------------------

-- Outstanding balances, current year and prior. Debt stays on the account of
-- the year that incurred it; a student's total exposure is the sum across
-- their accounts, computed here rather than carried forward into a running
-- balance that loses which year owes what.
CREATE VIEW v_debt AS
SELECT
    b.account_id,
    b.student_id,
    s.student_no,
    s.full_name,
    s.mother_name,
    s.phone,
    b.academic_year_id,
    ay.code                 AS academic_year_code,
    b.college_id,
    b.department_id,
    b.study_type_id,
    b.stage,
    b.effective_net,
    b.net_paid,
    b.remaining,
    b.credit_balance,
    (ay.status IN ('financially_closed', 'closed')) AS is_prior_year,
    overdue.oldest_due_date,
    coalesce(overdue.overdue_amount, 0) AS overdue_amount,
    coalesce(overdue.overdue_count, 0)  AS overdue_installments
FROM v_account_balance b
JOIN student s ON s.id = b.student_id
JOIN academic_year ay ON ay.id = b.academic_year_id
LEFT JOIN LATERAL (
    SELECT
        min(vi.due_date)    AS oldest_due_date,
        sum(vi.remaining)   AS overdue_amount,
        count(*)            AS overdue_count
    FROM v_installment_status vi
    WHERE vi.account_id = b.account_id
      AND vi.is_overdue
) overdue ON true
WHERE b.remaining > 0
  AND b.account_status <> 'cancelled';

-- ---------------------------------------------------------------------------
-- Aggregates
-- ---------------------------------------------------------------------------

-- The spine of the department, stage, and study-type summaries.
--
-- Head counts come from effective enrollments; money comes from every
-- non-cancelled account. Those are deliberately different populations, which
-- is why they are separate subqueries rather than one join: a single join
-- would either double-count a superseded student's fees or lose the cash
-- collected on their first account.
CREATE VIEW v_year_department_summary AS
SELECT
    fa.academic_year_id,
    fa.college_id,
    fa.department_id,
    fa.study_type_id,
    fa.stage,
    count(DISTINCT CASE WHEN e.enrollment_status <> 'superseded' THEN fa.student_id END) AS student_count,
    count(*)                                        AS account_count,
    sum(fa.gross_total)                             AS gross_total,
    sum(fa.discount_total)                          AS discount_total,
    sum(fa.net_total)                               AS net_snapshot_total,
    sum(b.adjustment_total)                         AS adjustment_total,
    sum(b.effective_net)                            AS effective_net_total,
    sum(b.net_paid)                                 AS paid_total,
    sum(b.refunded_total)                           AS refunded_total,
    sum(b.remaining)                                AS remaining_total,
    CASE
        WHEN sum(b.effective_net) > 0
        THEN round(100.0 * sum(b.net_paid) / sum(b.effective_net), 2)
        ELSE NULL
    END                                             AS collection_rate_pct
FROM financial_account fa
JOIN enrollment e ON e.id = fa.enrollment_id
JOIN v_account_balance b ON b.account_id = fa.id
WHERE fa.status <> 'cancelled'
GROUP BY fa.academic_year_id, fa.college_id, fa.department_id, fa.study_type_id, fa.stage;

-- Discount cost, read from applications rather than recomputed from
-- definitions. A definition may have been revised three times since; the
-- frozen application amount is what was actually forgone.
CREATE VIEW v_discount_usage AS
SELECT
    fa.academic_year_id,
    dd.id                       AS definition_id,
    dd.code                     AS definition_code,
    dd.name_ar                  AS definition_name,
    dd.category,
    ddv.version_no,
    ddv.value_type,
    ddv.value_bp,
    ddv.value_amount,
    count(DISTINCT fa.student_id)   AS student_count,
    count(*)                        AS application_count,
    sum(da.applied_amount)          AS total_discount,
    sum(CASE WHEN da.truncation_reason IS NOT NULL THEN 1 ELSE 0 END) AS truncated_count
FROM discount_application da
JOIN financial_account fa ON fa.id = da.account_id
JOIN discount_definition_version ddv ON ddv.id = da.definition_version_id
JOIN discount_definition dd ON dd.id = ddv.definition_id
WHERE da.status = 'applied'
  AND fa.status <> 'cancelled'
GROUP BY fa.academic_year_id, dd.id, dd.code, dd.name_ar, dd.category,
         ddv.version_no, ddv.value_type, ddv.value_bp, ddv.value_amount;

-- Cash movement by cashier and day, for shift reconciliation. Voided payments
-- appear in both the payment list and the void list and net to zero, which is
-- what the drawer should show.
CREATE VIEW v_cashier_daily AS
SELECT
    p.cashier_user_id,
    u.full_name             AS cashier_name,
    p.cashier_session_id,
    (p.posted_at AT TIME ZONE 'UTC')::date AS posting_date,
    pm.code                 AS method_code,
    pm.is_cash,
    count(*) FILTER (WHERE p.status = 'posted')     AS payment_count,
    coalesce(sum(p.amount) FILTER (WHERE p.status = 'posted'), 0)  AS payment_total,
    count(*) FILTER (WHERE p.status = 'voided')     AS void_count,
    coalesce(sum(p.amount) FILTER (WHERE p.status = 'voided'), 0)  AS void_total
FROM payment p
JOIN app_user u ON u.id = p.cashier_user_id
JOIN payment_method pm ON pm.id = p.payment_method_id
WHERE p.posted_at IS NOT NULL
GROUP BY p.cashier_user_id, u.full_name, p.cashier_session_id,
         (p.posted_at AT TIME ZONE 'UTC')::date, pm.code, pm.is_cash;

-- ---------------------------------------------------------------------------
-- Reconciliation
-- ---------------------------------------------------------------------------

-- Cached totals against recomputed ones. Read nightly; an empty result is the
-- expected outcome, and any row is a defect to investigate, not noise to
-- tolerate. Drift here means some code path wrote money outside the
-- transaction that was supposed to own it.
CREATE VIEW v_account_reconciliation AS
SELECT
    fa.id                   AS account_id,
    fa.academic_year_id,
    fa.student_id,
    fa.paid_total           AS cached_paid,
    b.paid_gross            AS computed_paid,
    fa.paid_total - b.paid_gross        AS paid_drift,
    fa.refunded_total       AS cached_refunded,
    b.refunded_total        AS computed_refunded,
    fa.refunded_total - b.refunded_total AS refunded_drift,
    fa.adjustment_total     AS cached_adjustment,
    b.adjustment_total      AS computed_adjustment,
    fa.adjustment_total - b.adjustment_total AS adjustment_drift,
    fa.credit_balance       AS cached_credit,
    b.credit_balance        AS computed_credit,
    fa.credit_balance - b.credit_balance AS credit_drift,
    fa.last_reconciled_at
FROM financial_account fa
JOIN v_account_balance b ON b.account_id = fa.id
WHERE fa.paid_total       IS DISTINCT FROM b.paid_gross
   OR fa.refunded_total   IS DISTINCT FROM b.refunded_total
   OR fa.adjustment_total IS DISTINCT FROM b.adjustment_total
   OR fa.credit_balance   IS DISTINCT FROM b.credit_balance;

COMMENT ON VIEW v_account_reconciliation IS
    'Accounts whose cached totals disagree with the transaction rows. '
    'Expected to be empty; every row is a defect.';

-- Installments whose cached paid_amount disagrees with their allocations.
CREATE VIEW v_installment_reconciliation AS
SELECT
    i.id                AS installment_id,
    i.account_id,
    i.installment_no,
    i.paid_amount       AS cached_paid,
    v.allocated_paid    AS computed_paid,
    i.paid_amount - v.allocated_paid AS drift,
    i.status            AS stored_status,
    v.effective_status
FROM installment i
JOIN v_installment_status v ON v.installment_id = i.id
WHERE i.paid_amount IS DISTINCT FROM v.allocated_paid;

-- Payments refunded beyond what was collected. Structurally impossible if the
-- posting path holds its locks; present as a standing tripwire because the one
-- error this system must never make quietly is paying out more than it took in.
CREATE VIEW v_over_refunded_payments AS
SELECT
    p.id                AS payment_id,
    p.receipt_no,
    p.account_id,
    p.student_id,
    p.amount            AS payment_amount,
    sum(r.amount)       AS refunded_amount,
    sum(r.amount) - p.amount AS excess
FROM payment p
JOIN refund r ON r.payment_id = p.id AND r.status = 'posted'
GROUP BY p.id, p.receipt_no, p.account_id, p.student_id, p.amount
HAVING sum(r.amount) > p.amount;

COMMENT ON VIEW v_over_refunded_payments IS
    'Payments whose posted refunds exceed the amount collected. Must always be '
    'empty; a row here means money left the university that never entered it.';
