DROP TABLE IF EXISTS import_row;
DROP TABLE IF EXISTS import_batch;
DROP TABLE IF EXISTS idempotency_record;
DROP FUNCTION IF EXISTS verify_audit_chain(BIGINT);
DROP TABLE IF EXISTS audit_log;
DROP FUNCTION IF EXISTS audit_log_chain();
