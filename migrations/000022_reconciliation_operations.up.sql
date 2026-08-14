-- Reconciliation as an operation, not a log line.
--
-- The nightly check already existed and already found drift correctly. What it
-- did with a finding was write it to the log: which means nobody can answer
-- "has this been looked at", two people investigate the same account, the same
-- drift is rediscovered every night with no sign it is the same one, and a
-- finding that stops appearing looks identical to one that was fixed.
--
-- These two tables give a finding a life: it is seen, it recurs, somebody takes
-- it, somebody closes it with a reason. And a run is recorded whether or not it
-- found anything, because the absence of runs is itself the thing an operator
-- most needs to see — a reconciliation that silently stopped looks exactly like
-- a system with no drift.
--
-- Note what these are not. They are operational records, not financial ones:
-- nothing here is money, and a finding may be updated as its state moves. The
-- accounts they point at are untouched. Fixing drift is never an UPDATE to a
-- cached total — it is finding the command that failed to maintain it.

CREATE TABLE reconciliation_run (
    id            UUID        PRIMARY KEY,
    -- Which invariant this pass checked. Separate runs rather than one big
    -- one: the account check and the audit chain fail for unrelated reasons
    -- and at unrelated frequencies.
    kind          TEXT        NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'running',
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ,
    rows_checked  BIGINT      NOT NULL DEFAULT 0,
    findings      INTEGER     NOT NULL DEFAULT 0,
    new_findings  INTEGER     NOT NULL DEFAULT 0,
    -- The failure text when a run could not complete. A run that errored is
    -- not a clean run, and the distinction has to survive in the table.
    error         TEXT,
    -- Null when the scheduler started it; set when an operator did.
    triggered_by  UUID        REFERENCES app_user (id),

    CONSTRAINT ck_reconciliation_kind CHECK (kind IN (
        'accounts', 'installments', 'refunds', 'audit_chain'
    )),
    CONSTRAINT ck_reconciliation_status CHECK (status IN (
        'running', 'clean', 'drift', 'failed'
    )),
    CONSTRAINT ck_reconciliation_finished CHECK (
        (status = 'running') = (finished_at IS NULL)
    )
);

CREATE INDEX ix_reconciliation_run_time ON reconciliation_run (started_at DESC);
CREATE INDEX ix_reconciliation_run_kind ON reconciliation_run (kind, started_at DESC);

COMMENT ON TABLE reconciliation_run IS
    'One pass of one invariant check. Recorded even when clean: a check that '
    'stopped running looks exactly like a system with nothing wrong.';

CREATE TABLE reconciliation_finding (
    id            UUID        PRIMARY KEY,
    -- The run that first saw it. Later sightings update last_seen_at rather
    -- than inserting again, so one problem is one row however many nights it
    -- survives.
    first_run_id  UUID        NOT NULL REFERENCES reconciliation_run (id),
    last_run_id   UUID        NOT NULL REFERENCES reconciliation_run (id),
    kind          TEXT        NOT NULL,
    subject_type  TEXT        NOT NULL,
    subject_id    UUID        NOT NULL,
    -- The numbers that disagree, as they were when last seen. Read by whoever
    -- picks the finding up, so it does not have to be reproduced first.
    detail        JSONB       NOT NULL,

    severity      TEXT        NOT NULL DEFAULT 'warning',
    state         TEXT        NOT NULL DEFAULT 'open',
    seen_count    INTEGER     NOT NULL DEFAULT 1,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    acknowledged_at     TIMESTAMPTZ,
    acknowledged_by     UUID REFERENCES app_user (id),
    acknowledged_reason TEXT,
    resolved_at         TIMESTAMPTZ,
    resolved_by         UUID REFERENCES app_user (id),
    resolution          TEXT,

    CONSTRAINT ck_finding_severity CHECK (severity IN ('warning', 'critical')),
    CONSTRAINT ck_finding_state CHECK (state IN ('open', 'acknowledged', 'resolved')),
    -- A resolution has to say what was done. "Resolved" with no text is how a
    -- finding gets closed because it was inconvenient.
    CONSTRAINT ck_finding_resolution CHECK (
        (state = 'resolved') = (resolved_at IS NOT NULL AND resolution IS NOT NULL)
    ),
    CONSTRAINT ck_finding_acknowledgement CHECK (
        acknowledged_at IS NULL OR acknowledged_reason IS NOT NULL
    )
);

-- One live finding per subject per kind. Without this the same drifting
-- account produces a new row every night and the queue becomes unreadable
-- within a week — which is how a real finding gets lost among its own copies.
CREATE UNIQUE INDEX uq_finding_live_subject
    ON reconciliation_finding (subject_type, subject_id, kind)
    WHERE state <> 'resolved';

CREATE INDEX ix_finding_state ON reconciliation_finding (state, severity, last_seen_at DESC);
CREATE INDEX ix_finding_subject ON reconciliation_finding (subject_type, subject_id);

COMMENT ON TABLE reconciliation_finding IS
    'One invariant violation, from the night it appeared to the day somebody '
    'closed it with a reason. Updated as its state moves: this is operational '
    'record-keeping, not financial history.';

COMMENT ON COLUMN reconciliation_finding.seen_count IS
    'Nights this has survived. Escalation is a function of this: drift nobody '
    'has explained after several passes is not a transient.';

-- The queue an operator works from: what is open, worst first, oldest first
-- within that. A finding acknowledged but not resolved is still on it — taking
-- something is not the same as fixing it.
CREATE VIEW v_reconciliation_queue AS
SELECT
    f.id,
    f.kind,
    f.subject_type,
    f.subject_id,
    f.severity,
    f.state,
    f.seen_count,
    f.first_seen_at,
    f.last_seen_at,
    f.detail,
    f.acknowledged_by,
    f.acknowledged_reason,
    u.username AS acknowledged_by_username
FROM reconciliation_finding f
LEFT JOIN app_user u ON u.id = f.acknowledged_by
WHERE f.state <> 'resolved'
ORDER BY
    CASE f.severity WHEN 'critical' THEN 0 ELSE 1 END,
    f.first_seen_at;

COMMENT ON VIEW v_reconciliation_queue IS
    'Open invariant violations, worst and oldest first.';
