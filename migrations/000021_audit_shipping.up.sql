-- Shipping the audit trail off the host that writes it.
--
-- The hash chain in audit_log detects alteration, but only against itself: an
-- operator with the database role can delete the tail of the chain and every
-- remaining entry still verifies, because verification walks what is there.
-- Truncation is the attack the chain cannot see, and it is the likely one — an
-- administrator removing the record of what they did is a far more ordinary
-- story than one forging a sha256.
--
-- The answer is a copy somewhere the database cannot reach. Each shipment
-- records the range it covers, the hash at each end of that range, and the
-- checksum of the bytes that left. Verification then asks three questions the
-- database alone cannot answer:
--
--   * do the shipments cover a contiguous range, with no missing block;
--   * does the archived copy of an entry still match the row in the database;
--   * is the last shipped hash still the hash the database holds at that
--     sequence.
--
-- A tail deleted from audit_log fails the first and third. A row edited in
-- place fails the second.
--
-- The shipment rows themselves are append-only, and deliberately carry no
-- student identifiers: this table says which range went where, never what was
-- in it.

CREATE TABLE audit_shipment (
    id               UUID        PRIMARY KEY,
    -- Where it went: a directory path, a bucket key prefix, an endpoint. Free
    -- text because the destination is an operational choice, and a code table
    -- would need a migration every time the university changed hosts.
    destination      TEXT        NOT NULL,
    -- The artifact within that destination — the file name or object key.
    artifact_ref     TEXT        NOT NULL,

    from_sequence    BIGINT      NOT NULL,
    to_sequence      BIGINT      NOT NULL,
    entry_count      INTEGER     NOT NULL,

    -- The chain endpoints of the shipped block, so a later verification can
    -- check the database still agrees about both ends without reading the
    -- archive at all.
    first_entry_hash TEXT        NOT NULL,
    last_entry_hash  TEXT        NOT NULL,
    -- sha256 of the shipped bytes, so a corrupted or edited archive is
    -- detected before its contents are trusted.
    content_sha256   TEXT        NOT NULL,
    content_bytes    BIGINT      NOT NULL,

    shipped_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    shipped_by       UUID        REFERENCES app_user (id),

    CONSTRAINT ck_audit_shipment_range CHECK (to_sequence >= from_sequence),
    CONSTRAINT ck_audit_shipment_count CHECK (entry_count > 0),
    CONSTRAINT ck_audit_shipment_bytes CHECK (content_bytes > 0)
);

-- One shipment per block per destination. A retry that re-ships the same range
-- is a duplicate, not a second copy, and it would make the coverage check
-- report an overlap it cannot distinguish from a gap.
CREATE UNIQUE INDEX uq_audit_shipment_block
    ON audit_shipment (destination, from_sequence, to_sequence);

CREATE INDEX ix_audit_shipment_time ON audit_shipment (shipped_at DESC);
CREATE INDEX ix_audit_shipment_head ON audit_shipment (destination, to_sequence DESC);

CREATE TRIGGER trg_audit_shipment_immutable
    BEFORE UPDATE OR DELETE ON audit_shipment
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

COMMENT ON TABLE audit_shipment IS
    'One block of audit entries copied off-host. The witness against truncation.';

-- Coverage, as a view an operator can read without knowing the scheme: every
-- shipment beside the one before it, with the gap between them. A row with
-- gap_before > 0 means entries that were never shipped, and the entries in that
-- gap are the ones a truncation would be hiding in.
CREATE VIEW v_audit_shipment_coverage AS
SELECT
    s.destination,
    s.from_sequence,
    s.to_sequence,
    s.entry_count,
    s.shipped_at,
    s.from_sequence - 1 - lag(s.to_sequence) OVER (
        PARTITION BY s.destination ORDER BY s.from_sequence
    ) AS gap_before
FROM audit_shipment s;

COMMENT ON VIEW v_audit_shipment_coverage IS
    'Shipment blocks in order. gap_before > 0 is a range of audit entries that '
    'never left the host.';
