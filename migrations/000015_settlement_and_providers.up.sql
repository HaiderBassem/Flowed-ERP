-- Bank and POS reconciliation, and the boundary electronic providers post
-- through.
--
-- The system could record that a payment arrived "by bank transfer, reference
-- 8814772" and nothing more. Nothing checked that the reference was real,
-- nothing noticed the same slip entered twice, and nothing compared what the
-- system believed it had collected against what the bank said it had received.
-- For a university whose non-cash collection is growing, that is the largest
-- remaining way to lose money without anyone noticing — every other path
-- (void, refund, adjustment) is now hard to abuse, and this one was open.
--
-- Two halves, deliberately separate:
--
--   * Settlement. A statement is imported, its lines are matched against
--     posted payments, and what does not match is worked by a human. The
--     statement is evidence; the payments are the ledger; matching them is the
--     control.
--
--   * Providers. Qi Card, ZainCash, FastPay and branch collection all initiate
--     a payment elsewhere and confirm it later. The confirmation is
--     authoritative, never the client's word for it, and the same confirmation
--     arriving twice must produce one payment.

-- ---------------------------------------------------------------------------
-- Duplicate external references
-- ---------------------------------------------------------------------------

ALTER TABLE payment
    -- Set when an operator confirms that a repeated external reference is
    -- deliberate. One bank transfer covering a family of three students is a
    -- real thing; the same slip entered three times by mistake is a more
    -- common one, and the two are indistinguishable without asking.
    ADD COLUMN reference_duplicate_ack BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN payment.reference_duplicate_ack IS
    'A repeated external reference was confirmed deliberate by the operator who posted it. '
    'Unset, the partial unique index below refuses the second use.';

-- Collections that already share a reference predate this control. They are
-- marked acknowledged rather than left to fail the migration: the decision they
-- represent was taken before the system could ask about it, and nobody can
-- re-take it retrospectively. The earliest of each group keeps its unmarked
-- state, so the reference still reads as belonging to that collection.
--
-- The count this updates is worth looking at after applying: on a real
-- database it is the number of duplicate bank slips nobody had noticed.
--
-- The append-only trigger has to stand aside for this one statement, and the
-- flag is deliberately NOT added to its permitted-column list: a column that
-- could be set later is a control somebody could switch off after the fact,
-- and the whole point of the flag is that it records a decision taken at the
-- moment of posting. A migration runs as the table owner inside one
-- transaction and re-enables the trigger before it commits; the application
-- role can never do this.
ALTER TABLE payment DISABLE TRIGGER trg_payment_immutable;

WITH ranked AS (
    SELECT id, row_number() OVER (
               PARTITION BY payment_method_id, method_reference
               ORDER BY posted_at, id
           ) AS position
    FROM payment
    WHERE method_reference IS NOT NULL
      AND status <> 'voided'
)
UPDATE payment SET reference_duplicate_ack = TRUE
WHERE id IN (SELECT id FROM ranked WHERE position > 1);

ALTER TABLE payment ENABLE TRIGGER trg_payment_immutable;

-- The control: one external reference per method, unless somebody says
-- otherwise in writing. Voided payments are excluded — a slip entered wrongly,
-- voided, and entered again correctly is exactly the sequence this must permit.
CREATE UNIQUE INDEX uq_payment_external_reference
    ON payment (payment_method_id, method_reference)
    WHERE method_reference IS NOT NULL
      AND NOT reference_duplicate_ack
      AND status <> 'voided';

-- ---------------------------------------------------------------------------
-- Settlement batches
-- ---------------------------------------------------------------------------

CREATE TABLE settlement_batch (
    id             UUID        PRIMARY KEY,
    -- Which institution or terminal produced the statement. Free text against
    -- a code table would be over-modelling: the university adds a bank by
    -- uploading its statement, not by asking for a schema change.
    source_code    TEXT        NOT NULL,
    source_name    TEXT,
    -- The file as uploaded, for the audit: what was actually parsed.
    filename       TEXT        NOT NULL,
    -- Digest of the uploaded bytes. Re-uploading the same statement is a
    -- routine mistake at a desk, and matching twice would produce two
    -- reconciliations of one day's money.
    content_sha256 TEXT        NOT NULL,

    statement_from DATE,
    statement_to   DATE,

    status         TEXT        NOT NULL DEFAULT 'uploaded',
    line_count     INTEGER     NOT NULL DEFAULT 0,
    matched_count  INTEGER     NOT NULL DEFAULT 0,
    -- Totals as the statement states them, in whole dinars like everything
    -- else. Stored rather than summed on demand so a partially worked batch
    -- still knows what it started as.
    total_amount   BIGINT      NOT NULL DEFAULT 0,
    matched_amount BIGINT      NOT NULL DEFAULT 0,

    notes          TEXT,
    uploaded_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    uploaded_by    UUID        REFERENCES app_user (id),
    reconciled_at  TIMESTAMPTZ,
    reconciled_by  UUID        REFERENCES app_user (id),

    CONSTRAINT ck_settlement_batch_status CHECK (
        status IN ('uploaded', 'matching', 'needs_review', 'reconciled', 'failed', 'cancelled')
    ),
    CONSTRAINT ck_settlement_batch_counts CHECK (
        line_count >= 0 AND matched_count >= 0 AND matched_count <= line_count
    )
);

-- The same file, twice, is refused. A cancelled batch releases its digest so a
-- statement uploaded by mistake can be uploaded again after it is discarded.
CREATE UNIQUE INDEX uq_settlement_batch_content
    ON settlement_batch (content_sha256)
    WHERE status <> 'cancelled';

CREATE INDEX ix_settlement_batch_status ON settlement_batch (status, uploaded_at DESC);

COMMENT ON TABLE settlement_batch IS
    'One imported bank or POS statement. Evidence from outside the system, matched against '
    'the payments inside it.';

CREATE TABLE settlement_line (
    id            UUID        PRIMARY KEY,
    batch_id      UUID        NOT NULL REFERENCES settlement_batch (id) ON DELETE CASCADE,
    line_no       INTEGER     NOT NULL,

    -- The reference the bank prints and the cashier types. This is the join.
    external_ref  TEXT,
    amount        BIGINT      NOT NULL,
    value_date    DATE,
    description   TEXT,
    -- The row exactly as parsed, so a mismatch can be investigated against
    -- what the file actually said rather than against what was extracted.
    raw           JSONB,

    match_status  TEXT        NOT NULL DEFAULT 'unmatched',
    matched_payment_id UUID   REFERENCES payment (id),
    -- The difference when a line matched a payment of a different amount. A
    -- line that matched exactly carries zero; the sign says which way.
    variance      BIGINT      NOT NULL DEFAULT 0,

    reviewed_at   TIMESTAMPTZ,
    reviewed_by   UUID        REFERENCES app_user (id),
    review_note   TEXT,

    CONSTRAINT ck_settlement_line_status CHECK (
        match_status IN (
            'unmatched',   -- no payment carries this reference
            'matched',     -- one payment, same amount
            'variance',    -- one payment, different amount
            'duplicate',   -- more than one payment carries this reference
            'ignored'      -- deliberately set aside, with a reason
        )
    ),
    CONSTRAINT ck_settlement_line_amount CHECK (amount <> 0),
    CONSTRAINT ck_settlement_line_matched CHECK (
        (match_status IN ('matched', 'variance')) = (matched_payment_id IS NOT NULL)
    ),
    CONSTRAINT ck_settlement_line_ignored CHECK (
        match_status <> 'ignored' OR review_note IS NOT NULL
    )
);

CREATE UNIQUE INDEX uq_settlement_line_no ON settlement_line (batch_id, line_no);
CREATE INDEX ix_settlement_line_ref ON settlement_line (external_ref) WHERE external_ref IS NOT NULL;
CREATE INDEX ix_settlement_line_open ON settlement_line (batch_id)
    WHERE match_status IN ('unmatched', 'variance', 'duplicate');
-- One payment cannot settle two statement lines. Without this a duplicated
-- statement row would look reconciled against one collection.
CREATE UNIQUE INDEX uq_settlement_line_payment ON settlement_line (matched_payment_id)
    WHERE matched_payment_id IS NOT NULL;

COMMENT ON TABLE settlement_line IS
    'One row of an imported statement, and what it matched. The rows that never match are '
    'the point: money the bank says arrived that the system never recorded.';

-- ---------------------------------------------------------------------------
-- Electronic payment providers
-- ---------------------------------------------------------------------------

CREATE TABLE payment_intent (
    id              UUID        PRIMARY KEY,
    -- The provider this intent belongs to: QI, ZAINCASH, FASTPAY, BRANCH.
    -- A code rather than a foreign key: providers are wired in configuration
    -- and code, and a table would suggest one can be added by data alone.
    provider_code   TEXT        NOT NULL,

    account_id      UUID        NOT NULL REFERENCES financial_account (id) ON DELETE RESTRICT,
    student_id      UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    academic_year_id UUID       NOT NULL REFERENCES academic_year (id) ON DELETE RESTRICT,

    amount          BIGINT      NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'created',

    -- What the provider calls this transaction. Unique per provider: the
    -- confirmation arrives by this reference, and two intents sharing one
    -- would make the confirmation ambiguous.
    provider_ref    TEXT,
    -- What we told the provider to call it. Sent on initiation so a retried
    -- initiation cannot create a second charge at their end.
    client_ref      TEXT        NOT NULL,
    -- The key the eventual payment is posted under, so a confirmation
    -- delivered twice posts one payment. This is the same idempotency the
    -- cashier desk uses, reached from the other direction.
    payment_idempotency_key TEXT NOT NULL,

    redirect_url    TEXT,
    expires_at      TIMESTAMPTZ,
    failure_code    TEXT,
    failure_message TEXT,

    -- Set when the confirmation was accepted and a payment posted. This is the
    -- only link between the provider world and the ledger.
    payment_id      UUID        REFERENCES payment (id),

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by      UUID        REFERENCES app_user (id),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at    TIMESTAMPTZ,

    CONSTRAINT ck_payment_intent_amount CHECK (amount > 0),
    CONSTRAINT ck_payment_intent_status CHECK (
        status IN ('created', 'pending', 'succeeded', 'failed', 'expired', 'cancelled')
    ),
    -- A succeeded intent must name its payment, and only a succeeded one may.
    -- Without this an intent could report success with nothing in the ledger,
    -- which is the exact shape of "the app said it paid".
    CONSTRAINT ck_payment_intent_settled CHECK ((status = 'succeeded') = (payment_id IS NOT NULL))
);

CREATE UNIQUE INDEX uq_payment_intent_client_ref ON payment_intent (provider_code, client_ref);
CREATE UNIQUE INDEX uq_payment_intent_provider_ref ON payment_intent (provider_code, provider_ref)
    WHERE provider_ref IS NOT NULL;
CREATE UNIQUE INDEX uq_payment_intent_idempotency ON payment_intent (payment_idempotency_key);
CREATE INDEX ix_payment_intent_account ON payment_intent (account_id, created_at DESC);
CREATE INDEX ix_payment_intent_open ON payment_intent (status, expires_at)
    WHERE status IN ('created', 'pending');

CREATE TRIGGER trg_payment_intent_updated_at
    BEFORE UPDATE ON payment_intent
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE payment_intent IS
    'A collection begun at a provider and confirmed later. Nothing here is money: the money '
    'is the payment row an accepted confirmation creates.';

CREATE TABLE payment_provider_event (
    id            UUID        PRIMARY KEY,
    provider_code TEXT        NOT NULL,
    intent_id     UUID        REFERENCES payment_intent (id) ON DELETE RESTRICT,

    -- The provider's own identifier for this delivery. The duplicate-callback
    -- guard: providers retry, and a retry that posted a second payment would
    -- charge a student twice for one transaction.
    external_event_id TEXT    NOT NULL,
    event_type    TEXT        NOT NULL,
    -- Whether the delivery's signature verified. A callback that fails this is
    -- stored and never acted on: it is evidence of somebody trying.
    signature_ok  BOOLEAN     NOT NULL DEFAULT FALSE,
    payload       JSONB       NOT NULL,

    received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at  TIMESTAMPTZ,
    -- What acting on it did, or why it was refused.
    outcome       TEXT,

    CONSTRAINT ck_provider_event_type CHECK (event_type <> '')
);

-- The duplicate guard itself.
CREATE UNIQUE INDEX uq_provider_event_external
    ON payment_provider_event (provider_code, external_event_id);
CREATE INDEX ix_provider_event_intent ON payment_provider_event (intent_id, received_at DESC);
CREATE INDEX ix_provider_event_unprocessed ON payment_provider_event (received_at)
    WHERE processed_at IS NULL;

-- Provider events are evidence, like login attempts: whoever appears in one
-- must not be able to tidy it afterwards.
CREATE TRIGGER trg_provider_event_no_update
    BEFORE DELETE ON payment_provider_event
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

COMMENT ON TABLE payment_provider_event IS
    'Every callback a provider delivered, verified or not, acted on or not. The unique '
    'external event id is what makes a retried delivery post one payment rather than two.';

-- ---------------------------------------------------------------------------
-- Reconciliation views
-- ---------------------------------------------------------------------------

-- Money the bank says arrived that the system never recorded, and money the
-- system recorded that the bank has not confirmed. Both directions matter: the
-- first is a collection nobody credited to a student, the second is a receipt
-- issued for money that never came.
CREATE VIEW v_settlement_exceptions AS
SELECT
    l.id                AS line_id,
    b.id                AS batch_id,
    b.source_code,
    b.filename,
    l.line_no,
    l.external_ref,
    l.amount,
    l.value_date,
    l.match_status,
    l.variance,
    l.matched_payment_id
FROM settlement_line l
JOIN settlement_batch b ON b.id = l.batch_id
WHERE l.match_status IN ('unmatched', 'variance', 'duplicate')
  AND b.status <> 'cancelled';

COMMENT ON VIEW v_settlement_exceptions IS
    'Statement lines that did not settle cleanly. Expected to be worked to empty after each '
    'import; a line left here is money whose two sides disagree.';

-- Non-cash payments carrying a reference that no imported statement has
-- confirmed. Bounded to posted payments with a reference, because cash has no
-- statement and a voided payment is not owed a confirmation.
CREATE VIEW v_unconfirmed_electronic_payments AS
SELECT
    p.id                AS payment_id,
    p.receipt_no,
    p.posting_year_id,
    p.account_id,
    p.student_id,
    p.amount,
    p.method_reference,
    p.paid_at,
    pm.code             AS method_code
FROM payment p
JOIN payment_method pm ON pm.id = p.payment_method_id
WHERE p.status = 'posted'
  AND NOT pm.is_cash
  AND p.method_reference IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM settlement_line l
      WHERE l.matched_payment_id = p.id
        AND l.match_status IN ('matched', 'variance')
  );

COMMENT ON VIEW v_unconfirmed_electronic_payments IS
    'Posted non-cash collections no statement line has confirmed. A receipt was printed; the '
    'bank has not yet said the money arrived.';
