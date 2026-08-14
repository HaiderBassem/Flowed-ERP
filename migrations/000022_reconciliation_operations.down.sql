-- Reverting loses the record of which findings were investigated and what was
-- concluded. The invariants themselves are unaffected: the views the check
-- reads from are older than this migration and stay.

DROP VIEW IF EXISTS v_reconciliation_queue;
DROP TABLE IF EXISTS reconciliation_finding;
DROP TABLE IF EXISTS reconciliation_run;
