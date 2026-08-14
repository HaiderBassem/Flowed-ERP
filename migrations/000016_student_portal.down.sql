-- Rolling back removes student access and statement verification. No financial
-- row is touched: a student credential reads, and a verification is a frozen
-- copy of figures that live in the ledger.

DROP TRIGGER IF EXISTS trg_statement_verification_immutable ON statement_verification;
DROP TABLE IF EXISTS statement_verification;

DROP INDEX IF EXISTS uq_app_user_student;
ALTER TABLE app_user DROP COLUMN IF EXISTS student_id;
