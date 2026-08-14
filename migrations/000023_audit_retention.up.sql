-- Audit growth: what to do when the trail is a million rows.
--
-- Measured first, because the answer turned out to be less alarming than the
-- question. At 500,000 entries the table is 251 MB including indexes — 526
-- bytes an entry — and a full chain verification takes 662 ms. A university of
-- twenty thousand students writes on the order of 300,000 entries a year, so
-- five years is under a gigabyte and a nightly full verification stays under
-- two seconds. Nothing here needs partitioning, and partitioning would cost
-- something real: the chain trigger reads the previous entry on every insert,
-- and across sixty monthly partitions that becomes sixty index probes on the
-- write path of every money command.
--
-- Two things do need adding, and they are the two that stop being free as the
-- table grows.
--
-- 1. A verification checkpoint. Verifying from zero every night is work that
--    grows forever, and the growth is invisible until the night it does not
--    finish. A checkpoint records "the chain was intact up to sequence N, whose
--    hash was H", so the nightly pass verifies only what has been written
--    since. The prefix is not thereby trusted blindly: the checkpoint stores
--    the hash it stopped at, so a rewritten prefix breaks the join at the
--    checkpoint, and a full pass still runs on a slower schedule.
--
-- 2. Archival. Not deletion — nothing in this system deletes a financial
--    record — but a way to move entries that are old *and* already witnessed
--    off-host into a table the daily queries do not touch. The gate is the
--    shipment record: an entry that has not left this host may not leave the
--    live table either, because the copy that would remain is the one somebody
--    with the database role could remove.

-- ---------------------------------------------------------------------------
-- Verification checkpoints
-- ---------------------------------------------------------------------------

CREATE TABLE audit_verification (
    id            UUID        PRIMARY KEY,
    -- The sequence the chain was verified up to, and the hash at that point.
    -- Both are needed: the sequence says where to resume, and the hash is what
    -- makes resuming safe.
    sequence_no   BIGINT      NOT NULL,
    entry_hash    TEXT        NOT NULL,
    -- Entries covered by this pass. A pass that verified nothing new is still
    -- recorded — it is evidence the check ran.
    entries       BIGINT      NOT NULL DEFAULT 0,
    -- full when the pass started at zero, incremental when it resumed.
    kind          TEXT        NOT NULL,
    problems      INTEGER     NOT NULL DEFAULT 0,
    took_ms       INTEGER,
    verified_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    verified_by   UUID        REFERENCES app_user (id),

    CONSTRAINT ck_audit_verification_kind CHECK (kind IN ('full', 'incremental'))
);

CREATE INDEX ix_audit_verification_time ON audit_verification (verified_at DESC);
CREATE INDEX ix_audit_verification_clean
    ON audit_verification (sequence_no DESC) WHERE problems = 0;

CREATE TRIGGER trg_audit_verification_immutable
    BEFORE UPDATE OR DELETE ON audit_verification
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

COMMENT ON TABLE audit_verification IS
    'Where the chain was last found intact. The nightly check resumes from the '
    'newest clean row rather than walking the whole trail every night.';

-- The resume point: the highest sequence a clean pass reached.
CREATE OR REPLACE FUNCTION audit_verification_checkpoint()
RETURNS TABLE (sequence_no BIGINT, entry_hash TEXT)
LANGUAGE sql
STABLE
AS $$
    SELECT v.sequence_no, v.entry_hash
    FROM audit_verification v
    WHERE v.problems = 0
    ORDER BY v.sequence_no DESC
    LIMIT 1
$$;

COMMENT ON FUNCTION audit_verification_checkpoint() IS
    'The sequence and hash the chain was last verified to, or no row at all.';

-- ---------------------------------------------------------------------------
-- Archival
-- ---------------------------------------------------------------------------

-- Same shape as audit_log, and deliberately not a partition of it: an entry
-- here has left the working set and should not be reachable by a stray query
-- that forgot a date filter. Nothing is lost — the rows are here, and a copy is
-- off-host as well.
CREATE TABLE audit_log_archive (LIKE audit_log INCLUDING DEFAULTS INCLUDING CONSTRAINTS);

ALTER TABLE audit_log_archive ADD PRIMARY KEY (id);
CREATE INDEX ix_audit_archive_sequence ON audit_log_archive (sequence_no);
CREATE INDEX ix_audit_archive_time ON audit_log_archive (occurred_at DESC);
CREATE INDEX ix_audit_archive_entity
    ON audit_log_archive (entity_type, entity_id, occurred_at DESC);
CREATE INDEX ix_audit_archive_student
    ON audit_log_archive (student_id, occurred_at DESC) WHERE student_id IS NOT NULL;

CREATE TRIGGER trg_audit_archive_immutable
    BEFORE UPDATE OR DELETE ON audit_log_archive
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

COMMENT ON TABLE audit_log_archive IS
    'Audit entries moved out of the working set. Old, and already copied '
    'off-host: both conditions, or the move would be the deletion it must '
    'never be.';

-- Everything, live and archived, in one place. A student history query reads
-- this rather than audit_log so an archived entry does not silently vanish
-- from an investigation.
CREATE VIEW v_audit_trail AS
SELECT *, false AS archived FROM audit_log
UNION ALL
SELECT *, true AS archived FROM audit_log_archive;

COMMENT ON VIEW v_audit_trail IS
    'The whole trail, live and archived. Read this when the question is what '
    'happened, rather than what happened recently.';

-- Moves a prefix of the trail into the archive.
--
-- Three conditions, and all three are refusals rather than filters, because a
-- silent partial archive is how a hole appears in a record nobody is looking
-- at:
--
--   * the range must end before the cutoff;
--   * every entry in it must be covered by a recorded shipment, so a copy
--     exists somewhere this database cannot reach;
--   * the chain must verify across the range being moved.
--
-- SECURITY DEFINER because the move needs the immutability trigger suspended
-- for the duration, and this function is the only sanctioned way to do that.
CREATE OR REPLACE FUNCTION archive_audit_entries(
    before_time TIMESTAMPTZ,
    max_entries INTEGER DEFAULT 100000
)
RETURNS TABLE (moved BIGINT, from_sequence BIGINT, to_sequence BIGINT)
LANGUAGE plpgsql
SECURITY DEFINER
AS $$
DECLARE
    shipped_to  BIGINT;
    cutoff_seq  BIGINT;
    first_seq   BIGINT;
    problem_cnt INTEGER;
BEGIN
    SELECT coalesce(max(s.to_sequence), 0) INTO shipped_to FROM audit_shipment s;
    IF shipped_to = 0 THEN
        RAISE EXCEPTION 'nothing has been shipped off-host; archiving would leave one copy'
            USING ERRCODE = 'P0001',
                  HINT = 'Configure AUDIT_ARCHIVE_DIR and run `api audit-ship` first.';
    END IF;

    -- The last entry that is both old enough and already witnessed.
    SELECT max(a.sequence_no) INTO cutoff_seq
    FROM audit_log a
    WHERE a.occurred_at < before_time
      AND a.sequence_no <= shipped_to;

    IF cutoff_seq IS NULL THEN
        moved := 0; from_sequence := 0; to_sequence := 0;
        RETURN NEXT;
        RETURN;
    END IF;

    SELECT min(a.sequence_no) INTO first_seq FROM audit_log a;

    -- Bound the batch so one call cannot lock the table for minutes.
    SELECT min(seq) INTO cutoff_seq FROM (
        SELECT a.sequence_no AS seq
        FROM audit_log a
        WHERE a.sequence_no <= cutoff_seq
        ORDER BY a.sequence_no DESC
        LIMIT 1
        OFFSET greatest(
            (SELECT count(*) FROM audit_log a2 WHERE a2.sequence_no <= cutoff_seq) - max_entries,
            0)
    ) bounded;

    SELECT count(*) INTO problem_cnt
    FROM verify_audit_chain(greatest(first_seq - 1, 0)) v
    WHERE v.sequence_no <= cutoff_seq;

    IF problem_cnt > 0 THEN
        RAISE EXCEPTION 'the chain does not verify across the range being archived (% problem(s))',
            problem_cnt
            USING ERRCODE = 'P0001',
                  HINT = 'Investigate before moving anything: compare against the off-host archive.';
    END IF;

    ALTER TABLE audit_log DISABLE TRIGGER trg_audit_log_immutable;

    INSERT INTO audit_log_archive
    SELECT * FROM audit_log WHERE sequence_no <= cutoff_seq;

    DELETE FROM audit_log WHERE sequence_no <= cutoff_seq;

    ALTER TABLE audit_log ENABLE TRIGGER trg_audit_log_immutable;

    moved := (SELECT count(*) FROM audit_log_archive WHERE sequence_no <= cutoff_seq
                                                       AND sequence_no >= first_seq);
    from_sequence := first_seq;
    to_sequence := cutoff_seq;
    RETURN NEXT;
END;
$$;

COMMENT ON FUNCTION archive_audit_entries(TIMESTAMPTZ, INTEGER) IS
    'Moves old, already-shipped audit entries into audit_log_archive. Refuses '
    'unless a copy exists off-host and the chain verifies across the range.';
