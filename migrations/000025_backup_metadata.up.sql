-- Backup and restore, made a UI operation instead of a terminal one.
--
-- The dump/restore mechanics already existed as scripts/backup.sh and
-- scripts/restore-drill.sh, run from a terminal by whoever had one. This gives
-- them a row: every backup_run is one artifact on disk — a .dump file — and
-- whether it was verified; every restore_run is one attempt to bring one of
-- those artifacts live, whether it passed the same checks restore-drill.sh
-- already ran, and what safety copy guards it. Without this, "which file is
-- safe to restore" is a question only a directory listing plus a manifest
-- file can answer, and a non-technical operator has neither.
--
-- Two tables rather than one, deliberately: a backup_run's own status
-- (verified/failed) is a fact about the file that never changes after the
-- dump completes. Restoring it ten times must not rewrite that fact ten
-- times — reconciliation_run/reconciliation_finding split the same way for
-- the same reason.
--
-- Nothing here is money. These rows never gate a financial invariant; they
-- record what happened to the database as a whole.

CREATE TABLE backup_run (
    id             UUID        PRIMARY KEY,
    -- manual: the button. automatic: the scheduler. safety: taken by the
    -- restore flow itself, of the live database, immediately before it
    -- restores into it. imported: registered from a file the user uploaded
    -- rather than produced on this host.
    kind           TEXT        NOT NULL,
    status         TEXT        NOT NULL DEFAULT 'running',
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at    TIMESTAMPTZ,

    -- Set once the dump exists. Null while running, and after a failure that
    -- never produced a file.
    file_path      TEXT,
    bytes          BIGINT,
    sha256         TEXT,

    -- Recorded at the moment of the dump, so a restore has something to
    -- compare the restored copy against. An empty table passes every
    -- reconciliation view, because nothing in it disagrees with nothing else
    -- — the counts are what catch that.
    schema_version INTEGER,
    server_version TEXT,
    row_counts     JSONB,

    -- The row_counts snapshot is a claim; this is whether the claim was
    -- checked. pg_restore --list succeeding and finding every financial table
    -- is what verified=true means, the same test scripts/backup.sh runs.
    verified       BOOLEAN     NOT NULL DEFAULT false,

    error          TEXT,

    created_by     UUID REFERENCES app_user (id),

    CONSTRAINT ck_backup_kind CHECK (kind IN ('manual', 'automatic', 'safety', 'imported')),
    CONSTRAINT ck_backup_status CHECK (status IN ('running', 'verified', 'failed')),
    CONSTRAINT ck_backup_finished CHECK (
        (status = 'running') = (finished_at IS NULL)
    )
);

CREATE INDEX ix_backup_run_time ON backup_run (started_at DESC);
CREATE INDEX ix_backup_run_status ON backup_run (kind, status);

COMMENT ON TABLE backup_run IS
    'One backup artifact on disk and whether it was verified. Never rewritten '
    'by a later restore — restore_run is the record of using one.';

COMMENT ON COLUMN backup_run.row_counts IS
    'student/financial_account/payment/audit_log counts at dump time. Catches '
    'a restore that silently dropped a table: an empty table reconciles '
    'perfectly, so the count is the only thing that notices.';

-- One attempt to bring a backup live. Both the safety copy and the checks
-- that gate the swap live on this row, so "is it safe to look away yet" has
-- one place to be answered from.
CREATE TABLE restore_run (
    id               UUID        PRIMARY KEY,
    backup_run_id    UUID        NOT NULL REFERENCES backup_run (id),
    -- The safety backup taken of the live database before this attempt
    -- touched anything. Null only in the impossible case where the safety
    -- backup itself failed to even start a row — in every real failure mode
    -- this is set and its own status says whether it succeeded.
    safety_backup_id UUID        REFERENCES backup_run (id),

    -- running: safety backup + scratch restore in progress.
    -- checking: scratch database restored, reconciliation checks running.
    -- swapping: checks passed, live database is being replaced.
    -- restored: swap completed; the app is serving the restored data.
    -- failed: stopped at any stage; the live database was never touched.
    status           TEXT        NOT NULL DEFAULT 'running',
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at      TIMESTAMPTZ,

    -- The four reconciliation views plus the audit chain, as they read
    -- against the scratch database. Recorded whether or not they passed: a
    -- failed restore should say exactly which check refused it.
    checks           JSONB,

    -- Where the live database was renamed to on a successful swap, so it can
    -- be found again rather than only inferred from a timestamp.
    previous_database TEXT,

    error            TEXT,
    created_by       UUID REFERENCES app_user (id),

    CONSTRAINT ck_restore_status CHECK (
        status IN ('running', 'checking', 'swapping', 'restored', 'failed')
    ),
    CONSTRAINT ck_restore_finished CHECK (
        (status IN ('restored', 'failed')) = (finished_at IS NOT NULL)
    )
);

CREATE INDEX ix_restore_run_time ON restore_run (started_at DESC);

COMMENT ON TABLE restore_run IS
    'One attempt to make a backup the live database: its safety copy, the '
    'checks that gated the swap, and whether it reached restored or stopped '
    'with the live database untouched.';

-- One row, holding the automatic-backup policy. A single row rather than a
-- key-value table: there is exactly one schedule, and a table that can only
-- ever hold one row says so structurally rather than by convention.
CREATE TABLE backup_schedule (
    id               BOOLEAN     PRIMARY KEY DEFAULT true,
    enabled          BOOLEAN     NOT NULL DEFAULT false,
    interval_hours   INTEGER     NOT NULL DEFAULT 24,
    retention_count  INTEGER     NOT NULL DEFAULT 30,
    last_run_at      TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by       UUID REFERENCES app_user (id),

    CONSTRAINT ck_backup_schedule_singleton CHECK (id),
    CONSTRAINT ck_backup_schedule_interval CHECK (interval_hours > 0),
    CONSTRAINT ck_backup_schedule_retention CHECK (retention_count >= 1)
);

INSERT INTO backup_schedule (id) VALUES (true);

COMMENT ON TABLE backup_schedule IS
    'The automatic-backup policy: on or off, how often, how many to keep. '
    'Read by the scheduler job every tick rather than at start-up, so a '
    'change the administrator makes in the UI takes effect on the next tick '
    'without a restart.';
