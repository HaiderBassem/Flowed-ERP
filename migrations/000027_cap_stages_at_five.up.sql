-- The university has decided five is the ceiling for every department, no
-- exception: First through Fifth Stage. This replaces the earlier 1..8 range
-- (chosen to fit a six-stage medicine track) — any department or enrollment
-- still above five is capped down before the tighter constraint is added, so
-- the migration does not fail against existing data.

UPDATE enrollment SET stage = 5 WHERE stage > 5;
UPDATE department SET stage_count = 5 WHERE stage_count > 5;
UPDATE fee_policy_version SET stage = 5 WHERE stage > 5;
UPDATE installment_template SET stage = 5 WHERE stage > 5;

ALTER TABLE department
    DROP CONSTRAINT ck_department_stage_count,
    ADD CONSTRAINT ck_department_stage_count CHECK (stage_count BETWEEN 1 AND 5);

ALTER TABLE enrollment
    DROP CONSTRAINT ck_enrollment_stage,
    ADD CONSTRAINT ck_enrollment_stage CHECK (stage BETWEEN 1 AND 5);

ALTER TABLE fee_policy_version
    DROP CONSTRAINT ck_fee_policy_stage,
    ADD CONSTRAINT ck_fee_policy_stage CHECK (stage IS NULL OR stage BETWEEN 1 AND 5);

ALTER TABLE installment_template
    DROP CONSTRAINT ck_installment_template_stage,
    ADD CONSTRAINT ck_installment_template_stage CHECK (stage IS NULL OR stage BETWEEN 1 AND 5);
