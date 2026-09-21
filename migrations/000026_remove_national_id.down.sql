-- Restores the columns, not the data: the values were dropped with the
-- column and cannot be recovered here, same as every other down script in
-- this project that reverses a DROP COLUMN.

ALTER TABLE student
    ADD COLUMN national_id TEXT;

CREATE UNIQUE INDEX uq_student_national_id ON student (national_id) WHERE national_id IS NOT NULL;

ALTER TABLE student_identity_version
    ADD COLUMN national_id TEXT;
