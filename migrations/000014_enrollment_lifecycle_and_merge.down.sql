-- Rolling back loses the record of which financial treatment was applied to a
-- withdrawal, the clearance decisions, the plan revision history and the merge
-- register. The financial rows those decisions produced — adjustments, credits,
-- superseded installments — are untouched: they live in the ledger, which is
-- the point of expressing every one of these as an adjustment rather than as a
-- field somewhere.

ALTER TABLE student DROP CONSTRAINT IF EXISTS ck_student_merge_consistent;

DROP TRIGGER IF EXISTS trg_student_merge_append_only ON student_merge;
DROP TABLE IF EXISTS student_merge;

DROP TRIGGER IF EXISTS trg_plan_revision_append_only ON installment_plan_revision;
DROP TABLE IF EXISTS installment_plan_revision;

DROP TRIGGER IF EXISTS trg_clearance_append_only ON graduation_clearance;
DROP TABLE IF EXISTS graduation_clearance;

ALTER TABLE academic_year
    DROP CONSTRAINT IF EXISTS ck_year_clearance_policy,
    DROP COLUMN IF EXISTS graduation_clearance_policy;

ALTER TABLE enrollment
    DROP CONSTRAINT IF EXISTS ck_enrollment_financial_treatment,
    DROP COLUMN IF EXISTS financial_treatment,
    DROP COLUMN IF EXISTS financial_treatment_at,
    DROP COLUMN IF EXISTS financial_treatment_by;
