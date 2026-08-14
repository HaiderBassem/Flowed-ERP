-- Audit trail, command idempotency, and staged imports.

-- ---------------------------------------------------------------------------
-- Audit log
-- ---------------------------------------------------------------------------

-- Who changed what, when, from what to what, and why.
--
-- The rows form a hash chain: each carries the hash of its predecessor, so
-- removing or altering an entry breaks every hash after it. This matters
-- because the application connects to PostgreSQL as a role that owns its own
-- tables — permissions alone cannot stop a determined insider with database
-- access from editing history. They can still edit it here, but they cannot do
-- it invisibly, and a verification pass over the chain will say exactly where.
CREATE TABLE audit_log (
    id                  UUID        PRIMARY KEY,
    -- Monotonic ordering independent of clock skew, and the chain's spine.
    sequence_no         BIGSERIAL   NOT NULL,

    entity_type         TEXT        NOT NULL,
    entity_id           UUID,
    action              TEXT        NOT NULL,

    actor_user_id       UUID        REFERENCES app_user (id),
    actor_username      TEXT        NOT NULL,
    actor_roles         TEXT[],
    actor_ip            TEXT,
    session_id          TEXT,
    request_id          TEXT,

    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    before_state        JSONB,
    after_state         JSONB,
    -- Free-form context: the amount involved, the receipt number, the
    -- installment touched. Indexed with GIN so an investigation can search on
    -- any of it.
    metadata            JSONB,
    reason              TEXT,

    academic_year_id    UUID        REFERENCES academic_year (id),
    student_id          UUID        REFERENCES student (id),
    account_id          UUID        REFERENCES financial_account (id),

    previous_hash       TEXT,
    entry_hash          TEXT        NOT NULL,

    CONSTRAINT ck_audit_action CHECK (action <> ''),
    CONSTRAINT ck_audit_entity_type CHECK (entity_type <> '')
);

CREATE UNIQUE INDEX uq_audit_sequence ON audit_log (sequence_no);
CREATE INDEX ix_audit_entity ON audit_log (entity_type, entity_id, occurred_at DESC);
CREATE INDEX ix_audit_actor ON audit_log (actor_user_id, occurred_at DESC);
CREATE INDEX ix_audit_occurred ON audit_log (occurred_at DESC);
CREATE INDEX ix_audit_student ON audit_log (student_id, occurred_at DESC) WHERE student_id IS NOT NULL;
CREATE INDEX ix_audit_account ON audit_log (account_id, occurred_at DESC) WHERE account_id IS NOT NULL;
CREATE INDEX ix_audit_year ON audit_log (academic_year_id, occurred_at DESC) WHERE academic_year_id IS NOT NULL;
CREATE INDEX ix_audit_metadata ON audit_log USING gin (metadata jsonb_path_ops);
CREATE INDEX ix_audit_request ON audit_log (request_id) WHERE request_id IS NOT NULL;

-- Computes each entry's place in the chain. Done in the database rather than
-- in Go so that a row inserted by any route — application, migration, psql —
-- is chained too. An unchained insert is not possible.
--
-- The transaction-scoped advisory lock serialises appends, which the chain
-- requires: two concurrent inserts reading the same predecessor would produce
-- two rows claiming the same position. The lock is taken at the point of
-- insert, which in this system is the last act of a command before commit, so
-- the serialised window is short.
CREATE OR REPLACE FUNCTION audit_log_chain()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    prev_hash TEXT;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('audit_log_chain'));

    SELECT entry_hash INTO prev_hash
    FROM audit_log
    ORDER BY sequence_no DESC
    LIMIT 1;

    NEW.previous_hash := prev_hash;
    NEW.entry_hash := encode(
        digest(
            coalesce(prev_hash, '') || '|' ||
            NEW.id::text || '|' ||
            NEW.entity_type || '|' ||
            coalesce(NEW.entity_id::text, '') || '|' ||
            NEW.action || '|' ||
            NEW.actor_username || '|' ||
            to_char(NEW.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US') || '|' ||
            coalesce(NEW.before_state::text, '') || '|' ||
            coalesce(NEW.after_state::text, '') || '|' ||
            coalesce(NEW.metadata::text, '') || '|' ||
            coalesce(NEW.reason, ''),
            'sha256'
        ),
        'hex'
    );

    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_audit_log_chain
    BEFORE INSERT ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_chain();

CREATE TRIGGER trg_audit_log_immutable
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Walks the chain and reports the first entry whose stored hash disagrees with
-- a recomputation. An empty result is a clean audit trail.
CREATE OR REPLACE FUNCTION verify_audit_chain(from_sequence BIGINT DEFAULT 0)
RETURNS TABLE (sequence_no BIGINT, id UUID, occurred_at TIMESTAMPTZ, problem TEXT)
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    rec         RECORD;
    expected    TEXT;
    running     TEXT;
    first_row   BOOLEAN := true;
BEGIN
    FOR rec IN
        SELECT * FROM audit_log
        WHERE audit_log.sequence_no > from_sequence
        ORDER BY audit_log.sequence_no
    LOOP
        IF first_row THEN
            running := rec.previous_hash;
            first_row := false;
        ELSIF rec.previous_hash IS DISTINCT FROM running THEN
            sequence_no := rec.sequence_no;
            id := rec.id;
            occurred_at := rec.occurred_at;
            problem := 'previous_hash does not match the preceding entry: the chain is broken here';
            RETURN NEXT;
        END IF;

        expected := encode(
            digest(
                coalesce(rec.previous_hash, '') || '|' ||
                rec.id::text || '|' ||
                rec.entity_type || '|' ||
                coalesce(rec.entity_id::text, '') || '|' ||
                rec.action || '|' ||
                rec.actor_username || '|' ||
                to_char(rec.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US') || '|' ||
                coalesce(rec.before_state::text, '') || '|' ||
                coalesce(rec.after_state::text, '') || '|' ||
                coalesce(rec.metadata::text, '') || '|' ||
                coalesce(rec.reason, ''),
                'sha256'
            ),
            'hex'
        );

        IF expected IS DISTINCT FROM rec.entry_hash THEN
            sequence_no := rec.sequence_no;
            id := rec.id;
            occurred_at := rec.occurred_at;
            problem := 'entry_hash does not match the row contents: this entry was altered after it was written';
            RETURN NEXT;
        END IF;

        running := rec.entry_hash;
    END LOOP;
END;
$$;

COMMENT ON FUNCTION verify_audit_chain(BIGINT) IS
    'Recomputes the audit hash chain and returns any entry that was altered or '
    'removed after it was written. An empty result means the trail is intact.';

-- ---------------------------------------------------------------------------
-- Command idempotency
-- ---------------------------------------------------------------------------

-- The record of commands already executed, so a retried request returns the
-- original outcome instead of performing the work twice.
--
-- The stored response is what makes this honest: replaying a key returns
-- exactly what the first attempt returned, including the receipt number, so a
-- terminal that lost its answer to a timeout prints the same receipt rather
-- than a second one.
CREATE TABLE idempotency_record (
    id                  UUID        PRIMARY KEY,
    idempotency_key     TEXT        NOT NULL,
    command_name        TEXT        NOT NULL,
    -- Hash of the request payload. A key reused with a different payload is a
    -- client bug and must fail loudly rather than replay a mismatched result.
    payload_hash        TEXT        NOT NULL,
    actor_user_id       UUID        REFERENCES app_user (id),

    status              TEXT        NOT NULL DEFAULT 'in_progress',
    response_status     INTEGER,
    response_body       JSONB,
    error_code          TEXT,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at        TIMESTAMPTZ,
    -- Records outlive any plausible retry window, then a scheduled job prunes
    -- them. They are not an audit trail; the audit log is.
    expires_at          TIMESTAMPTZ NOT NULL DEFAULT (now() + INTERVAL '30 days'),

    CONSTRAINT ck_idempotency_status CHECK (status IN ('in_progress', 'succeeded', 'failed'))
);

CREATE UNIQUE INDEX uq_idempotency_key ON idempotency_record (idempotency_key, command_name);
CREATE INDEX ix_idempotency_expiry ON idempotency_record (expires_at);

-- ---------------------------------------------------------------------------
-- Staged imports
-- ---------------------------------------------------------------------------

-- Bulk student loading, staged rather than direct. A spreadsheet is validated
-- into rows, the errors are shown, a human confirms, and only then is anything
-- created. Importing straight from a file is how a typo in a department name
-- becomes four hundred students in a department that does not exist.
CREATE TABLE import_batch (
    id                  UUID        PRIMARY KEY,
    batch_type          TEXT        NOT NULL,
    source_filename     TEXT,
    status              TEXT        NOT NULL DEFAULT 'uploaded',

    academic_year_id    UUID        REFERENCES academic_year (id),
    total_rows          INTEGER     NOT NULL DEFAULT 0,
    valid_rows          INTEGER     NOT NULL DEFAULT 0,
    error_rows          INTEGER     NOT NULL DEFAULT 0,
    created_rows        INTEGER     NOT NULL DEFAULT 0,
    updated_rows        INTEGER     NOT NULL DEFAULT 0,
    skipped_rows        INTEGER     NOT NULL DEFAULT 0,
    failed_rows         INTEGER     NOT NULL DEFAULT 0,

    -- Updated as the worker progresses. A batch whose heartbeat has stopped is
    -- picked up by the reaper: without it, a worker killed mid-import leaves
    -- the batch stuck in "importing" with nothing to notice or resume it.
    heartbeat_at        TIMESTAMPTZ,
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    error_summary       TEXT,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by          UUID        REFERENCES app_user (id),
    confirmed_by        UUID        REFERENCES app_user (id),
    confirmed_at        TIMESTAMPTZ,

    CONSTRAINT ck_import_batch_type CHECK (batch_type IN (
        'students', 'enrollments', 'accounts', 'discounts'
    )),
    CONSTRAINT ck_import_batch_status CHECK (status IN (
        'uploaded', 'validating', 'needs_review', 'confirmed',
        'importing', 'imported', 'completed_with_skips', 'failed', 'cancelled'
    )),
    CONSTRAINT ck_import_batch_confirm_stamp CHECK (
        (status IN ('confirmed', 'importing', 'imported', 'completed_with_skips'))
            <= (confirmed_by IS NOT NULL AND confirmed_at IS NOT NULL)
    )
);

CREATE INDEX ix_import_batch_status ON import_batch (status, created_at DESC);
-- Stalled batches, for the reaper.
CREATE INDEX ix_import_batch_heartbeat ON import_batch (heartbeat_at)
    WHERE status = 'importing';

CREATE TABLE import_row (
    id                  UUID        PRIMARY KEY,
    batch_id            UUID        NOT NULL REFERENCES import_batch (id) ON DELETE CASCADE,
    row_no              INTEGER     NOT NULL,
    raw_data            JSONB       NOT NULL,
    -- Hash of the meaningful fields, so re-staging the same file does not
    -- duplicate rows.
    dedup_hash          TEXT        NOT NULL,

    validation_status   TEXT        NOT NULL DEFAULT 'pending',
    disposition         TEXT        NOT NULL DEFAULT 'create',
    errors              JSONB,
    warnings            JSONB,

    -- What the preview showed. The commit pass recomputes and compares against
    -- this, skipping any row whose outcome changed since a human approved it —
    -- otherwise the review is theatre when configuration moves in between.
    preview_hash        TEXT,
    matched_entity_id   UUID,
    created_entity_id   UUID,
    processed_at        TIMESTAMPTZ,
    error_message       TEXT,

    CONSTRAINT ck_import_row_no CHECK (row_no >= 1),
    CONSTRAINT ck_import_row_validation CHECK (validation_status IN (
        'pending', 'valid', 'warning', 'error', 'processed', 'skipped', 'failed'
    )),
    CONSTRAINT ck_import_row_disposition CHECK (disposition IN ('create', 'update', 'skip', 'error'))
);

CREATE UNIQUE INDEX uq_import_row_no ON import_row (batch_id, row_no);
CREATE UNIQUE INDEX uq_import_row_dedup ON import_row (batch_id, dedup_hash);
CREATE INDEX ix_import_row_status ON import_row (batch_id, validation_status);
-- The worker's queue: rows still to process, in file order.
CREATE INDEX ix_import_row_unprocessed ON import_row (batch_id, row_no)
    WHERE validation_status IN ('valid', 'warning');
