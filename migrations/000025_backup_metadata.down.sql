-- Reverting loses the record of which backups were taken, verified and
-- restored. The files themselves are untouched by this — they live in
-- BACKUP_DIR, not in the database — so nothing on disk is destroyed, only the
-- index of it.

DROP TABLE IF EXISTS backup_schedule;
DROP TABLE IF EXISTS restore_run;
DROP TABLE IF EXISTS backup_run;
