-- Restores the wider range. Departments and enrollments that were capped
-- down to 5 by the up migration are not restored to their original stage
-- counts — that data was overwritten, not recoverable here.

ALTER TABLE department
    DROP CONSTRAINT ck_department_stage_count,
    ADD CONSTRAINT ck_department_stage_count CHECK (stage_count BETWEEN 1 AND 8);

ALTER TABLE enrollment
    DROP CONSTRAINT ck_enrollment_stage,
    ADD CONSTRAINT ck_enrollment_stage CHECK (stage BETWEEN 1 AND 8);

ALTER TABLE fee_policy_version
    DROP CONSTRAINT ck_fee_policy_stage,
    ADD CONSTRAINT ck_fee_policy_stage CHECK (stage IS NULL OR stage BETWEEN 1 AND 8);

ALTER TABLE installment_template
    DROP CONSTRAINT ck_installment_template_stage,
    ADD CONSTRAINT ck_installment_template_stage CHECK (stage IS NULL OR stage BETWEEN 1 AND 8);
