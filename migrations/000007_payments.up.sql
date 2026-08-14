-- Payments, refunds, voids, and the cashier desk.
--
-- Everything in this migration is append-only. A posted payment is never
-- edited and never deleted; a mistake becomes a void, and returned money
-- becomes a refund. The receipt the student is holding always corresponds to a
-- row that still says what it said when it printed.

-- ---------------------------------------------------------------------------
-- Receipt numbering
-- ---------------------------------------------------------------------------

-- Cashier desks. Receipt series run per year per desk, because Iraqi financial
-- oversight reconciles against a sequential paper book held at one window.
CREATE TABLE cashier_desk (
    id          UUID        PRIMARY KEY,
    code        TEXT        NOT NULL,
    name_ar     TEXT        NOT NULL,
    college_id  UUID        REFERENCES college (id),
    is_active   BOOLEAN     NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_cashier_desk_code CHECK (code ~ '^[A-Z0-9_]{1,16}$')
);

CREATE UNIQUE INDEX uq_cashier_desk_code ON cashier_desk (code);

-- The counter behind receipt numbers. Incremented under a row lock inside the
-- posting transaction, so a rolled-back payment returns its number rather than
-- burning it — the sequence is gapless on success.
--
-- It is not void-free: a voided payment keeps its receipt number and is
-- reported as voided. That is the correct trade. An auditor scanning a receipt
-- book wants to see a cancelled receipt in place, not a missing number they
-- have to go and explain.
CREATE TABLE number_series (
    id                  UUID        PRIMARY KEY,
    series_kind         TEXT        NOT NULL,
    academic_year_id    UUID        NOT NULL REFERENCES academic_year (id) ON DELETE RESTRICT,
    cashier_desk_id     UUID        REFERENCES cashier_desk (id) ON DELETE RESTRICT,
    prefix              TEXT        NOT NULL,
    next_number         BIGINT      NOT NULL DEFAULT 1,
    padding             SMALLINT    NOT NULL DEFAULT 6,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_number_series_kind CHECK (series_kind IN ('payment', 'refund')),
    CONSTRAINT ck_number_series_next CHECK (next_number >= 1),
    CONSTRAINT ck_number_series_padding CHECK (padding BETWEEN 1 AND 12)
);

CREATE UNIQUE INDEX uq_number_series_scope
    ON number_series (series_kind, academic_year_id, cashier_desk_id)
    NULLS NOT DISTINCT;

-- ---------------------------------------------------------------------------
-- Cashier sessions
-- ---------------------------------------------------------------------------

-- A shift at a physical window. Cash payments require an open session, and the
-- session defines the window inside which a mistaken payment can still be
-- voided rather than refunded. Two doctypes' worth of work for the primary
-- control against cash disappearing between the desk and the safe.
CREATE TABLE cashier_session (
    id                  UUID        PRIMARY KEY,
    cashier_user_id     UUID        NOT NULL REFERENCES app_user (id) ON DELETE RESTRICT,
    cashier_desk_id     UUID        NOT NULL REFERENCES cashier_desk (id) ON DELETE RESTRICT,
    academic_year_id    UUID        NOT NULL REFERENCES academic_year (id) ON DELETE RESTRICT,

    opened_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    opening_float       BIGINT      NOT NULL DEFAULT 0,
    closed_at           TIMESTAMPTZ,
    -- What the drawer should hold: opening float plus cash in, less cash
    -- refunded and cash returned on same-session voids. Computed at close from
    -- the transaction rows, never typed.
    expected_cash       BIGINT,
    -- What the cashier actually counted.
    counted_cash        BIGINT,
    variance            BIGINT,
    variance_reason     TEXT,

    status              TEXT        NOT NULL DEFAULT 'open',
    approved_by         UUID        REFERENCES app_user (id),
    approved_at         TIMESTAMPTZ,
    notes               TEXT,

    CONSTRAINT ck_session_status CHECK (status IN ('open', 'closed', 'approved')),
    CONSTRAINT ck_session_opening_float CHECK (opening_float >= 0),
    CONSTRAINT ck_session_closed_stamp CHECK (
        (status IN ('closed', 'approved')) <= (
            closed_at IS NOT NULL AND expected_cash IS NOT NULL AND counted_cash IS NOT NULL
        )
    ),
    CONSTRAINT ck_session_variance_identity CHECK (
        variance IS NULL OR counted_cash IS NULL OR expected_cash IS NULL
        OR variance = counted_cash - expected_cash
    ),
    -- A drawer that does not balance needs an explanation before the shift can
    -- be signed off.
    CONSTRAINT ck_session_variance_reason CHECK (
        variance IS NULL OR variance = 0 OR variance_reason IS NOT NULL
    ),
    CONSTRAINT ck_session_approval_four_eyes CHECK (
        approved_by IS NULL OR approved_by <> cashier_user_id
    )
);

-- One open drawer per cashier at a time.
CREATE UNIQUE INDEX uq_cashier_session_open
    ON cashier_session (cashier_user_id)
    WHERE status = 'open';

CREATE INDEX ix_session_desk ON cashier_session (cashier_desk_id, opened_at DESC);
CREATE INDEX ix_session_status ON cashier_session (status);

-- ---------------------------------------------------------------------------
-- Payments
-- ---------------------------------------------------------------------------

CREATE TABLE payment_method (
    id          UUID        PRIMARY KEY,
    code        TEXT        NOT NULL,
    name_ar     TEXT        NOT NULL,
    is_cash     BOOLEAN     NOT NULL DEFAULT false,
    -- Whether a reference number from the bank or terminal is mandatory.
    requires_reference BOOLEAN NOT NULL DEFAULT false,
    is_active   BOOLEAN     NOT NULL DEFAULT true,
    sort_order  SMALLINT    NOT NULL DEFAULT 0,
    CONSTRAINT ck_payment_method_code CHECK (code ~ '^[A-Z0-9_]{2,32}$')
);

CREATE UNIQUE INDEX uq_payment_method_code ON payment_method (code);

CREATE TABLE payment (
    id                  UUID        PRIMARY KEY,
    receipt_no          TEXT,
    number_series_id    UUID        REFERENCES number_series (id),

    account_id          UUID        NOT NULL REFERENCES financial_account (id) ON DELETE RESTRICT,
    student_id          UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    enrollment_id       UUID        NOT NULL REFERENCES enrollment (id) ON DELETE RESTRICT,
    -- The year whose books this collection belongs to. For debt collected on a
    -- closed year's account this is the current open year, not the account's
    -- year: closing a year freezes its records, it does not stop the
    -- university collecting what it is owed.
    posting_year_id     UUID        NOT NULL REFERENCES academic_year (id) ON DELETE RESTRICT,

    amount              BIGINT      NOT NULL,
    payment_method_id   UUID        NOT NULL REFERENCES payment_method (id) ON DELETE RESTRICT,
    method_reference    TEXT,

    cashier_user_id     UUID        NOT NULL REFERENCES app_user (id) ON DELETE RESTRICT,
    cashier_session_id  UUID        REFERENCES cashier_session (id) ON DELETE RESTRICT,

    paid_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_at           TIMESTAMPTZ,
    status              TEXT        NOT NULL DEFAULT 'draft',

    -- Supplied by the terminal, unique across the table. A network retry
    -- carrying the same key returns the original receipt instead of taking the
    -- money twice.
    idempotency_key     TEXT,
    -- A hash of the meaningful request fields. A client that reuses a key with
    -- a different amount or a different student has a bug, and replaying the
    -- original receipt would hand over a receipt for the wrong collection —
    -- so a key collision with a different payload is an error, not a replay.
    payload_hash        TEXT,

    payer_name          TEXT,
    notes               TEXT,

    voided_at           TIMESTAMPTZ,
    voided_by           UUID        REFERENCES app_user (id),
    void_reason         TEXT,
    void_request_id     UUID,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_payment_amount CHECK (amount > 0),
    CONSTRAINT ck_payment_status CHECK (status IN ('draft', 'posted', 'voided')),
    -- A receipt number exists exactly when the payment has been posted. Draft
    -- rows must not consume numbers.
    CONSTRAINT ck_payment_receipt_on_post CHECK (
        (status IN ('posted', 'voided')) = (receipt_no IS NOT NULL AND posted_at IS NOT NULL)
    ),
    CONSTRAINT ck_payment_void_stamp CHECK (
        (status = 'voided') = (voided_at IS NOT NULL AND void_reason IS NOT NULL)
    ),
    CONSTRAINT ck_payment_idempotency_pair CHECK (
        (idempotency_key IS NULL) = (payload_hash IS NULL)
    )
);

CREATE UNIQUE INDEX uq_payment_idempotency_key
    ON payment (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE UNIQUE INDEX uq_payment_receipt
    ON payment (number_series_id, receipt_no)
    WHERE receipt_no IS NOT NULL;

CREATE INDEX ix_payment_account ON payment (account_id, status);
CREATE INDEX ix_payment_student ON payment (student_id, paid_at DESC);
CREATE INDEX ix_payment_posted ON payment (posted_at DESC) WHERE status = 'posted';
CREATE INDEX ix_payment_cashier_day ON payment (cashier_user_id, posted_at DESC);
CREATE INDEX ix_payment_session ON payment (cashier_session_id) WHERE cashier_session_id IS NOT NULL;
CREATE INDEX ix_payment_posting_year ON payment (posting_year_id, posted_at);
CREATE INDEX ix_payment_method_reference ON payment (method_reference) WHERE method_reference IS NOT NULL;
-- Supports the near-duplicate heuristic: same account, same amount, moments
-- apart, from a client that lost its idempotency key across a restart.
CREATE INDEX ix_payment_duplicate_probe ON payment (account_id, amount, paid_at DESC);

-- Posted payments may move to voided and nothing else. Every column carrying
-- money or identity is frozen the moment the row exists.
CREATE TRIGGER trg_payment_immutable
    BEFORE UPDATE OR DELETE ON payment
    FOR EACH ROW EXECUTE FUNCTION forbid_column_mutation(
        'status', 'receipt_no', 'number_series_id', 'posted_at',
        'voided_at', 'voided_by', 'void_reason', 'void_request_id', 'notes'
    );

-- ---------------------------------------------------------------------------
-- Allocations
-- ---------------------------------------------------------------------------

-- Which installments a payment settled. The relationship is many-to-many in
-- both directions: one payment can clear three installments, and one
-- installment can take five payments to clear.
--
-- There is deliberately no unique constraint on (payment, installment).
-- Reversals are rows, not flags, so the same pair legitimately appears more
-- than once: an allocation, its reversal when part of the payment is refunded,
-- and possibly a re-allocation afterwards. A unique pair would make the
-- append-only model unimplementable and force an in-place update — exactly the
-- destructive edit the whole design exists to prevent.
CREATE TABLE payment_allocation (
    id                      UUID        PRIMARY KEY,
    payment_id              UUID        NOT NULL REFERENCES payment (id) ON DELETE RESTRICT,
    installment_id          UUID        NOT NULL REFERENCES installment (id) ON DELETE RESTRICT,
    amount                  BIGINT      NOT NULL,
    entry_type              TEXT        NOT NULL DEFAULT 'allocation',
    -- For a reversal, the allocation being undone. Constrained to belong to
    -- the same payment by the application, which is what stops a refund of
    -- payment A from reversing payment B's funding of an installment and
    -- letting B be voided for a second payout.
    reverses_allocation_id  UUID        REFERENCES payment_allocation (id),
    -- The document that caused a reversal: a refund or a void request.
    caused_by_type          TEXT,
    caused_by_id            UUID,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by              UUID        REFERENCES app_user (id),

    CONSTRAINT ck_allocation_amount CHECK (amount > 0),
    CONSTRAINT ck_allocation_entry_type CHECK (entry_type IN ('allocation', 'reversal')),
    CONSTRAINT ck_allocation_reversal_link CHECK (
        (entry_type = 'reversal') = (reverses_allocation_id IS NOT NULL)
    ),
    CONSTRAINT ck_allocation_no_self_reversal CHECK (
        reverses_allocation_id IS NULL OR reverses_allocation_id <> id
    )
);

-- An allocation may be reversed once. Without this, two concurrent refunds
-- could each reverse the same allocation and the installment would appear
-- unpaid twice over.
CREATE UNIQUE INDEX uq_allocation_reversal
    ON payment_allocation (reverses_allocation_id)
    WHERE reverses_allocation_id IS NOT NULL;

CREATE INDEX ix_allocation_payment ON payment_allocation (payment_id);
CREATE INDEX ix_allocation_installment ON payment_allocation (installment_id);
CREATE INDEX ix_allocation_caused_by ON payment_allocation (caused_by_type, caused_by_id)
    WHERE caused_by_id IS NOT NULL;

CREATE TRIGGER trg_allocation_immutable
    BEFORE UPDATE OR DELETE ON payment_allocation
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- Refunds
-- ---------------------------------------------------------------------------

-- Returning money is its own document. The original payment keeps its amount:
-- a 500,000 payment with a 100,000 refund is not a 400,000 payment, it is two
-- facts, and the net is computed. Editing the payment down would destroy the
-- record of what was collected and what was given back.
CREATE TABLE refund (
    id                  UUID        PRIMARY KEY,
    refund_no           TEXT,
    number_series_id    UUID        REFERENCES number_series (id),

    payment_id          UUID        NOT NULL REFERENCES payment (id) ON DELETE RESTRICT,
    account_id          UUID        NOT NULL REFERENCES financial_account (id) ON DELETE RESTRICT,
    student_id          UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    posting_year_id     UUID        NOT NULL REFERENCES academic_year (id) ON DELETE RESTRICT,

    amount              BIGINT      NOT NULL,
    payment_method_id   UUID        NOT NULL REFERENCES payment_method (id) ON DELETE RESTRICT,
    method_reference    TEXT,
    reason              TEXT        NOT NULL,

    status              TEXT        NOT NULL DEFAULT 'requested',
    requested_by        UUID        NOT NULL REFERENCES app_user (id) ON DELETE RESTRICT,
    requested_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_by         UUID        REFERENCES app_user (id),
    approved_at         TIMESTAMPTZ,
    rejected_by         UUID        REFERENCES app_user (id),
    rejected_at         TIMESTAMPTZ,
    rejection_reason    TEXT,
    posted_at           TIMESTAMPTZ,
    posted_by           UUID        REFERENCES app_user (id),
    cashier_session_id  UUID        REFERENCES cashier_session (id),

    idempotency_key     TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_refund_amount CHECK (amount > 0),
    CONSTRAINT ck_refund_status CHECK (status IN (
        'requested', 'approved', 'rejected', 'cancelled', 'posted'
    )),
    CONSTRAINT ck_refund_approved_stamp CHECK (
        (status IN ('approved', 'posted')) <= (approved_by IS NOT NULL AND approved_at IS NOT NULL)
    ),
    CONSTRAINT ck_refund_posted_stamp CHECK (
        (status = 'posted') = (posted_at IS NOT NULL AND refund_no IS NOT NULL)
    ),
    -- The person who asks for money back cannot be the person who agrees to it.
    CONSTRAINT ck_refund_four_eyes CHECK (approved_by IS NULL OR approved_by <> requested_by),
    CONSTRAINT ck_refund_rejected_stamp CHECK (
        (status = 'rejected') <= (rejected_at IS NOT NULL AND rejection_reason IS NOT NULL)
    )
);

CREATE UNIQUE INDEX uq_refund_idempotency_key
    ON refund (idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX uq_refund_receipt
    ON refund (number_series_id, refund_no) WHERE refund_no IS NOT NULL;

CREATE INDEX ix_refund_payment ON refund (payment_id, status);
CREATE INDEX ix_refund_account ON refund (account_id);
CREATE INDEX ix_refund_status ON refund (status) WHERE status IN ('requested', 'approved');
CREATE INDEX ix_refund_posted ON refund (posted_at DESC) WHERE status = 'posted';
CREATE INDEX ix_refund_posting_year ON refund (posting_year_id, posted_at);

CREATE TRIGGER trg_refund_lifecycle
    BEFORE UPDATE OR DELETE ON refund
    FOR EACH ROW EXECUTE FUNCTION forbid_column_mutation(
        'status', 'refund_no', 'number_series_id', 'approved_by', 'approved_at',
        'rejected_by', 'rejected_at', 'rejection_reason',
        'posted_at', 'posted_by', 'cashier_session_id'
    );

-- Which installments a refund pulled money back out of. The mirror of
-- payment_allocation, and constrained by the application to touch only
-- allocations belonging to the refund's own payment.
CREATE TABLE refund_allocation (
    id                      UUID        PRIMARY KEY,
    refund_id               UUID        NOT NULL REFERENCES refund (id) ON DELETE RESTRICT,
    installment_id          UUID        REFERENCES installment (id) ON DELETE RESTRICT,
    -- The allocation being unwound. NULL when the refund draws on credit
    -- balance rather than on money that reached an installment.
    reverses_allocation_id  UUID        REFERENCES payment_allocation (id),
    credit_entry_id         UUID        REFERENCES credit_entry (id),
    amount                  BIGINT      NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_refund_allocation_amount CHECK (amount > 0),
    CONSTRAINT ck_refund_allocation_target CHECK (
        (installment_id IS NOT NULL AND reverses_allocation_id IS NOT NULL AND credit_entry_id IS NULL)
        OR
        (credit_entry_id IS NOT NULL AND installment_id IS NULL AND reverses_allocation_id IS NULL)
    )
);

CREATE INDEX ix_refund_allocation_refund ON refund_allocation (refund_id);
CREATE INDEX ix_refund_allocation_installment ON refund_allocation (installment_id)
    WHERE installment_id IS NOT NULL;

CREATE TRIGGER trg_refund_allocation_immutable
    BEFORE UPDATE OR DELETE ON refund_allocation
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- Void requests
-- ---------------------------------------------------------------------------

-- A cashier who mis-keys a payment cannot undo it alone. They raise a request;
-- a finance manager executes it. Two signatures on the correction, captured as
-- a document rather than as a permission check that leaves no trace.
CREATE TABLE void_request (
    id                  UUID        PRIMARY KEY,
    payment_id          UUID        NOT NULL REFERENCES payment (id) ON DELETE RESTRICT,
    reason              TEXT        NOT NULL,
    status              TEXT        NOT NULL DEFAULT 'requested',
    requested_by        UUID        NOT NULL REFERENCES app_user (id) ON DELETE RESTRICT,
    requested_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    executed_by         UUID        REFERENCES app_user (id),
    executed_at         TIMESTAMPTZ,
    rejected_by         UUID        REFERENCES app_user (id),
    rejected_at         TIMESTAMPTZ,
    rejection_reason    TEXT,

    CONSTRAINT ck_void_request_status CHECK (status IN ('requested', 'executed', 'rejected', 'cancelled')),
    CONSTRAINT ck_void_request_executed_stamp CHECK (
        (status = 'executed') = (executed_by IS NOT NULL AND executed_at IS NOT NULL)
    ),
    CONSTRAINT ck_void_request_four_eyes CHECK (executed_by IS NULL OR executed_by <> requested_by)
);

-- One live request per payment.
CREATE UNIQUE INDEX uq_void_request_open
    ON void_request (payment_id)
    WHERE status = 'requested';

CREATE INDEX ix_void_request_payment ON void_request (payment_id);
CREATE INDEX ix_void_request_pending ON void_request (status) WHERE status = 'requested';

ALTER TABLE payment
    ADD CONSTRAINT fk_payment_void_request
    FOREIGN KEY (void_request_id) REFERENCES void_request (id);
