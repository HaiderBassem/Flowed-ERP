-- The financial account and everything frozen into it.
--
-- One account per enrollment. Not per student: a student is a person who may
-- study for six years, and each of those years has its own prices, its own
-- discounts, and its own debt. Rolling them into one balance is what makes it
-- impossible to answer "what does this student still owe for 2024-2025" three
-- years later.
--
-- The account freezes its inputs at creation. Fee components are copied in as
-- snapshot lines; discounts are materialised as applications carrying the
-- amount they computed to and the definition version they computed from. The
-- configuration those came from may change tomorrow, and this account will not
-- notice.

CREATE TABLE financial_account (
    id                      UUID        PRIMARY KEY,
    enrollment_id           UUID        NOT NULL REFERENCES enrollment (id) ON DELETE RESTRICT,

    -- Denormalised context. Every summary report groups by year, department,
    -- stage and study type; carrying them here removes two joins from queries
    -- that will run over hundreds of thousands of rows. They are safe to
    -- denormalise precisely because they are snapshot-stable: an enrollment
    -- whose department changes is superseded, and the successor gets its own
    -- account rather than mutating this one.
    academic_year_id        UUID        NOT NULL REFERENCES academic_year (id) ON DELETE RESTRICT,
    student_id              UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    college_id              UUID        NOT NULL REFERENCES college (id) ON DELETE RESTRICT,
    department_id           UUID        NOT NULL REFERENCES department (id) ON DELETE RESTRICT,
    study_type_id           UUID        NOT NULL REFERENCES study_type (id) ON DELETE RESTRICT,
    stage                   SMALLINT    NOT NULL,

    -- Which policy produced the snapshot, and how specific it was. Kept for
    -- audit: when a dean asks why this student was charged this amount, the
    -- answer is a row, not a reconstruction.
    fee_policy_id           UUID        REFERENCES fee_policy_version (id),
    fee_policy_specificity  INTEGER,
    installment_template_id UUID        REFERENCES installment_template (id),

    -- Frozen at creation. These are not caches; nothing recomputes them.
    gross_total             BIGINT      NOT NULL DEFAULT 0,
    discountable_base       BIGINT      NOT NULL DEFAULT 0,
    discount_total          BIGINT      NOT NULL DEFAULT 0,
    net_total               BIGINT      NOT NULL DEFAULT 0,

    -- Every later change to what is owed — a retroactive discount, a transfer
    -- credit from a superseded enrollment, a post-close correction — is an
    -- adjustment row, and this is their running sum. The amount actually owed
    -- is net_total + adjustment_total, never a rewritten net_total. That is
    -- what keeps "the fee was 2,000,000 and here is every reason it is now
    -- 1,000,000" answerable.
    adjustment_total        BIGINT      NOT NULL DEFAULT 0,

    -- Caches, maintained inside the same transaction as the payment or refund
    -- that changes them, under a lock on this row. A nightly job recomputes
    -- them from the transaction rows and reports any drift; drift is a bug,
    -- not an expected rounding artefact.
    paid_total              BIGINT      NOT NULL DEFAULT 0,
    refunded_total          BIGINT      NOT NULL DEFAULT 0,
    credit_balance          BIGINT      NOT NULL DEFAULT 0,

    status                  TEXT        NOT NULL DEFAULT 'pending',
    -- Set when the enrollment behind this account was superseded, so the
    -- balance transfer can be traced in both directions.
    superseded_by_account_id UUID       REFERENCES financial_account (id),

    generated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    generated_by            UUID        REFERENCES app_user (id),
    activated_at            TIMESTAMPTZ,
    settled_at              TIMESTAMPTZ,
    cancelled_at            TIMESTAMPTZ,
    cancellation_reason     TEXT,

    last_reconciled_at      TIMESTAMPTZ,
    notes                   TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_account_status CHECK (status IN (
        'pending', 'active', 'settled', 'cancelled', 'exempt'
    )),
    CONSTRAINT ck_account_amounts_non_negative CHECK (
        gross_total >= 0 AND discountable_base >= 0 AND discount_total >= 0
        AND net_total >= 0 AND paid_total >= 0 AND refunded_total >= 0 AND credit_balance >= 0
    ),
    CONSTRAINT ck_account_discount_within_base CHECK (discount_total <= discountable_base),
    CONSTRAINT ck_account_net_identity CHECK (net_total = gross_total - discount_total),
    CONSTRAINT ck_account_base_within_gross CHECK (discountable_base <= gross_total),
    CONSTRAINT ck_account_refund_within_paid CHECK (refunded_total <= paid_total),
    CONSTRAINT ck_account_cancelled_stamp CHECK (
        (status = 'cancelled') <= (cancelled_at IS NOT NULL AND cancellation_reason IS NOT NULL)
    ),
    CONSTRAINT ck_account_no_self_supersede CHECK (
        superseded_by_account_id IS NULL OR superseded_by_account_id <> id
    )
);

-- One live account per enrollment. Cancelled accounts are excluded so that an
-- account generated against the wrong policy can be cancelled and regenerated
-- — a plain unique constraint on enrollment_id would make the second attempt
-- collide with the cancelled row forever.
CREATE UNIQUE INDEX uq_financial_account_live_enrollment
    ON financial_account (enrollment_id)
    WHERE status <> 'cancelled';

CREATE INDEX ix_account_enrollment ON financial_account (enrollment_id);
CREATE INDEX ix_account_student_year ON financial_account (student_id, academic_year_id);
CREATE INDEX ix_account_year_dept ON financial_account (academic_year_id, department_id, stage);
CREATE INDEX ix_account_year_study_type ON financial_account (academic_year_id, study_type_id);
CREATE INDEX ix_account_status ON financial_account (status);
-- Debt scans: accounts still owing money. The predicate keeps the index to the
-- rows the debt report and the registration block actually read.
CREATE INDEX ix_account_outstanding
    ON financial_account (academic_year_id, department_id)
    WHERE status IN ('active', 'pending') AND (net_total + adjustment_total) > paid_total - refunded_total;

CREATE TRIGGER trg_account_updated_at
    BEFORE UPDATE ON financial_account
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Frozen fee snapshot
-- ---------------------------------------------------------------------------

CREATE TABLE fee_snapshot_line (
    id                  UUID        PRIMARY KEY,
    account_id          UUID        NOT NULL REFERENCES financial_account (id) ON DELETE RESTRICT,
    component_code      TEXT        NOT NULL,
    name_ar             TEXT        NOT NULL,
    amount              BIGINT      NOT NULL,
    is_discountable     BOOLEAN     NOT NULL,
    is_refundable       BOOLEAN     NOT NULL,
    sort_order          SMALLINT    NOT NULL DEFAULT 0,
    -- Provenance only; the amount above is authoritative even if the source
    -- component is later edited or deleted.
    source_component_id UUID,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_snapshot_amount CHECK (amount >= 0)
);

CREATE UNIQUE INDEX uq_fee_snapshot_component ON fee_snapshot_line (account_id, component_code);
CREATE INDEX ix_fee_snapshot_account ON fee_snapshot_line (account_id);

CREATE TRIGGER trg_fee_snapshot_immutable
    BEFORE UPDATE OR DELETE ON fee_snapshot_line
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- Discount applications
-- ---------------------------------------------------------------------------

CREATE TABLE discount_application (
    id                      UUID        PRIMARY KEY,
    account_id              UUID        NOT NULL REFERENCES financial_account (id) ON DELETE RESTRICT,
    assignment_id           UUID        NOT NULL REFERENCES discount_assignment (id) ON DELETE RESTRICT,
    -- The exact version used. This single foreign key is the whole
    -- historical-integrity guarantee: it points at a frozen row, so no later
    -- edit to the discount's configuration can reach this calculation.
    definition_version_id   UUID        NOT NULL REFERENCES discount_definition_version (id) ON DELETE RESTRICT,

    -- What it was computed against and what it computed to, both frozen. The
    -- account is reconstructable from these even if every configuration row
    -- were lost.
    frozen_base_amount      BIGINT      NOT NULL,
    computed_amount         BIGINT      NOT NULL,
    -- After the per-application cap, the policy-level total cap, and the floor
    -- at the non-discountable remainder. Differs from computed_amount only
    -- when something truncated it, and then truncation_reason says which.
    applied_amount          BIGINT      NOT NULL,
    truncation_reason       TEXT,

    application_sequence    SMALLINT    NOT NULL DEFAULT 1,
    status                  TEXT        NOT NULL DEFAULT 'pending',

    applied_at              TIMESTAMPTZ,
    applied_by              UUID        REFERENCES app_user (id),
    reversed_at             TIMESTAMPTZ,
    reversed_by             UUID        REFERENCES app_user (id),
    reversal_reason         TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_application_amounts CHECK (
        frozen_base_amount >= 0 AND computed_amount >= 0 AND applied_amount >= 0
        AND applied_amount <= computed_amount
    ),
    CONSTRAINT ck_application_status CHECK (status IN ('pending', 'applied', 'declined', 'reversed')),
    CONSTRAINT ck_application_truncation CHECK (
        truncation_reason IS NULL OR truncation_reason IN (
            'per_application_cap', 'total_cap', 'discountable_floor'
        )
    ),
    CONSTRAINT ck_application_truncation_consistency CHECK (
        (applied_amount < computed_amount) <= (truncation_reason IS NOT NULL)
    ),
    CONSTRAINT ck_application_applied_stamp CHECK (
        (status = 'applied') <= (applied_at IS NOT NULL)
    ),
    CONSTRAINT ck_application_reversed_stamp CHECK (
        (status = 'reversed') <= (reversed_at IS NOT NULL AND reversal_reason IS NOT NULL)
    )
);

-- One live application per assignment per account. Scoped to the non-terminal
-- states so that the reverse-then-reapply correction path can write a second
-- row: a plain unique pair would make an audited correction impossible.
CREATE UNIQUE INDEX uq_discount_application_active
    ON discount_application (account_id, assignment_id)
    WHERE status IN ('pending', 'applied');

CREATE INDEX ix_application_account ON discount_application (account_id);
CREATE INDEX ix_application_assignment ON discount_application (assignment_id);
CREATE INDEX ix_application_version ON discount_application (definition_version_id);
CREATE INDEX ix_application_pending ON discount_application (status) WHERE status = 'pending';

CREATE TRIGGER trg_discount_application_immutable
    BEFORE UPDATE OR DELETE ON discount_application
    FOR EACH ROW EXECUTE FUNCTION forbid_column_mutation(
        'status', 'applied_at', 'applied_by', 'reversed_at', 'reversed_by', 'reversal_reason'
    );

-- ---------------------------------------------------------------------------
-- Adjustments and credits
-- ---------------------------------------------------------------------------

-- Every change to what an account owes after it was generated. Signed: a
-- negative amount reduces the debt, a positive one increases it.
--
-- This table is why net_total can stay frozen. A retroactive discount does not
-- rewrite the net; it posts an adjustment. A student who moves department
-- mid-year does not have their payments re-pointed at a new account; the old
-- account is settled with a transfer-out adjustment and the new one opens with
-- the matching transfer-in, the pair linked so the movement is visible from
-- either side.
CREATE TABLE account_adjustment (
    id                  UUID        PRIMARY KEY,
    account_id          UUID        NOT NULL REFERENCES financial_account (id) ON DELETE RESTRICT,
    adjustment_type     TEXT        NOT NULL,
    -- Signed, in whole dinars.
    amount              BIGINT      NOT NULL,
    reason              TEXT        NOT NULL,

    -- The other half of a transfer pair.
    paired_adjustment_id UUID       REFERENCES account_adjustment (id),
    -- Whatever document justified this: a discount application, a superseded
    -- enrollment, a ministry letter.
    reference_type      TEXT,
    reference_id        UUID,

    approved_by         UUID        REFERENCES app_user (id),
    approved_at         TIMESTAMPTZ,
    posted_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_by           UUID        REFERENCES app_user (id),
    -- Which year's books this belongs to. For a post-close correction this is
    -- the current open year even though the account belongs to a closed one.
    posting_year_id     UUID        REFERENCES academic_year (id),

    CONSTRAINT ck_adjustment_type CHECK (adjustment_type IN (
        'transfer_credit_out', 'transfer_credit_in', 'retroactive_discount',
        'discount_reversal', 'late_result_correction', 'closed_year_correction',
        'waiver', 'write_off', 'correction'
    )),
    CONSTRAINT ck_adjustment_amount_nonzero CHECK (amount <> 0),
    CONSTRAINT ck_adjustment_no_self_pair CHECK (
        paired_adjustment_id IS NULL OR paired_adjustment_id <> id
    ),
    -- The corrections that reach into a closed year are exactly the ones that
    -- must carry a named approver.
    CONSTRAINT ck_adjustment_approval CHECK (
        adjustment_type NOT IN ('closed_year_correction', 'waiver', 'write_off')
        OR (approved_by IS NOT NULL AND approved_at IS NOT NULL)
    )
);

CREATE INDEX ix_adjustment_account ON account_adjustment (account_id, posted_at);
CREATE INDEX ix_adjustment_type ON account_adjustment (adjustment_type);
CREATE INDEX ix_adjustment_posting_year ON account_adjustment (posting_year_id, posted_at);
CREATE INDEX ix_adjustment_pair ON account_adjustment (paired_adjustment_id)
    WHERE paired_adjustment_id IS NOT NULL;

CREATE TRIGGER trg_adjustment_immutable
    BEFORE UPDATE OR DELETE ON account_adjustment
    FOR EACH ROW EXECUTE FUNCTION forbid_column_mutation('paired_adjustment_id');

-- Money the university holds that the student has not spent yet: an
-- overpayment, or the excess left when a discount is granted after payment.
--
-- It exists as its own lockable row rather than as a number on the account so
-- that two consumers cannot spend it at once. Carrying it forward to next
-- year's account and refunding it in cash are separate code paths that would
-- otherwise lock different rows, both read a balance of 500,000, and both pay
-- it out.
CREATE TABLE credit_entry (
    id                  UUID        PRIMARY KEY,
    account_id          UUID        NOT NULL REFERENCES financial_account (id) ON DELETE RESTRICT,
    student_id          UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    amount              BIGINT      NOT NULL,
    consumed_amount     BIGINT      NOT NULL DEFAULT 0,
    source              TEXT        NOT NULL,
    source_reference_id UUID,
    status              TEXT        NOT NULL DEFAULT 'open',
    reason              TEXT,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by          UUID        REFERENCES app_user (id),
    closed_at           TIMESTAMPTZ,

    CONSTRAINT ck_credit_amount CHECK (amount > 0),
    CONSTRAINT ck_credit_consumed CHECK (consumed_amount >= 0 AND consumed_amount <= amount),
    CONSTRAINT ck_credit_source CHECK (source IN (
        'overpayment', 'retroactive_discount', 'transfer_credit', 'refund_reversal', 'other'
    )),
    CONSTRAINT ck_credit_status CHECK (status IN ('open', 'partially_consumed', 'consumed', 'refunded', 'expired')),
    CONSTRAINT ck_credit_status_consistency CHECK (
        (status = 'consumed') <= (consumed_amount = amount)
    )
);

CREATE INDEX ix_credit_account ON credit_entry (account_id);
CREATE INDEX ix_credit_student_open ON credit_entry (student_id)
    WHERE status IN ('open', 'partially_consumed');

-- How a credit was spent. Append-only, so the history of a carried-forward
-- balance is a list of rows rather than a decrementing number.
CREATE TABLE credit_consumption (
    id                  UUID        PRIMARY KEY,
    credit_entry_id     UUID        NOT NULL REFERENCES credit_entry (id) ON DELETE RESTRICT,
    amount              BIGINT      NOT NULL,
    consumed_for        TEXT        NOT NULL,
    target_account_id   UUID        REFERENCES financial_account (id),
    target_reference_id UUID,
    consumed_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    consumed_by         UUID        REFERENCES app_user (id),

    CONSTRAINT ck_credit_consumption_amount CHECK (amount > 0),
    CONSTRAINT ck_credit_consumption_for CHECK (consumed_for IN (
        'installment_offset', 'carry_forward', 'cash_refund', 'write_off'
    ))
);

CREATE INDEX ix_credit_consumption_entry ON credit_consumption (credit_entry_id);
CREATE INDEX ix_credit_consumption_target ON credit_consumption (target_account_id)
    WHERE target_account_id IS NOT NULL;

CREATE TRIGGER trg_credit_consumption_immutable
    BEFORE UPDATE OR DELETE ON credit_consumption
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- Installments
-- ---------------------------------------------------------------------------

-- The schedule of what is owed and when. Amounts are per-row and need not be
-- equal, because Iraqi plans routinely are not.
--
-- There is no overdue column. Overdue is due_date < today with money still
-- outstanding, which is a fact about right now, not a fact about the row.
-- Storing it would require a nightly sweep over every installment in the
-- system whose failure leaves stale flags that quietly misreport the debt.
CREATE TABLE installment (
    id                  UUID        PRIMARY KEY,
    account_id          UUID        NOT NULL REFERENCES financial_account (id) ON DELETE RESTRICT,
    installment_no      SMALLINT    NOT NULL,
    due_date            DATE        NOT NULL,
    amount              BIGINT      NOT NULL,
    -- Cache of the allocations against this row, maintained in the same
    -- transaction as the payment that changes it.
    paid_amount         BIGINT      NOT NULL DEFAULT 0,
    status              TEXT        NOT NULL DEFAULT 'pending',
    label_ar            TEXT,

    -- A re-split keeps the old rows and marks them superseded rather than
    -- editing amounts, so the plan the student agreed to in October is still
    -- readable in March.
    superseded_by_id    UUID        REFERENCES installment (id),
    plan_version        SMALLINT    NOT NULL DEFAULT 1,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_installment_no CHECK (installment_no >= 1),
    CONSTRAINT ck_installment_amount CHECK (amount > 0),
    CONSTRAINT ck_installment_paid_range CHECK (paid_amount >= 0 AND paid_amount <= amount),
    CONSTRAINT ck_installment_status CHECK (status IN (
        'pending', 'partially_paid', 'paid', 'waived', 'superseded'
    )),
    CONSTRAINT ck_installment_status_consistency CHECK (
        CASE status
            WHEN 'pending'        THEN paid_amount = 0
            WHEN 'partially_paid' THEN paid_amount > 0 AND paid_amount < amount
            WHEN 'paid'           THEN paid_amount = amount
            ELSE true
        END
    ),
    CONSTRAINT ck_installment_superseded_stamp CHECK (
        (status = 'superseded') = (superseded_by_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX uq_installment_number
    ON installment (account_id, installment_no)
    WHERE status <> 'superseded';

CREATE INDEX ix_installment_account ON installment (account_id, installment_no);
-- The overdue scan: rows still owing money, ordered by when they came due.
CREATE INDEX ix_installment_due_open
    ON installment (due_date)
    WHERE status IN ('pending', 'partially_paid');
CREATE INDEX ix_installment_status ON installment (status);

CREATE TRIGGER trg_installment_updated_at
    BEFORE UPDATE ON installment
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
