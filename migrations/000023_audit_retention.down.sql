-- Reverting moves any archived entries back into the live table, because the
-- older schema has nowhere else for them and losing them is not an option this
-- migration is allowed to take.

ALTER TABLE audit_log DISABLE TRIGGER trg_audit_log_immutable;
INSERT INTO audit_log SELECT * FROM audit_log_archive
ON CONFLICT (id) DO NOTHING;
ALTER TABLE audit_log ENABLE TRIGGER trg_audit_log_immutable;

DROP VIEW IF EXISTS v_audit_trail;
DROP FUNCTION IF EXISTS archive_audit_entries(TIMESTAMPTZ, INTEGER);
DROP TABLE IF EXISTS audit_log_archive;
DROP FUNCTION IF EXISTS audit_verification_checkpoint();
DROP TABLE IF EXISTS audit_verification;
