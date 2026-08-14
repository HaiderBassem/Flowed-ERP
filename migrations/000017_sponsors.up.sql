-- Sponsors and scholarships: money a third party owes on a student's behalf.
--
-- The system models every reduction in what a student owes as a discount, and
-- for a ministry-funded seat or a company-sponsored employee that is wrong in a
-- way that matters. A discount is money the university decided not to charge; a
-- sponsorship is money somebody else agreed to pay. Recording the second as the
-- first erases the receivable — the university has no list of who owes it what,
-- no way to invoice, and no way to notice a sponsor who has stopped paying.
--
-- The design document lists this as a future extension and does not settle how
-- it should behave, so the policy is configured per agreement rather than
-- chosen here. Two settlement modes, and the difference is who carries the risk
-- when the sponsor does not pay:
--
--   * receivable — the student's obligation is unchanged and the sponsor's
--     commitment is an expected inflow. The sponsor pays through an ordinary
--     payment, allocated exactly like any other, so nothing in the financial
--     core changes. If the sponsor never pays, the student still owes it.
--
--   * covers_debt — the student's obligation falls by an adjustment when the
--     account is generated, and the university carries the loss if the sponsor
--     defaults. This is what a ministry-funded seat actually means: the student
--     is not chased, whatever happens between the university and the ministry.
--
-- Neither is a default. An agreement that does not say which it is has not been
-- negotiated yet, and guessing would decide who gets a debt letter.

-- A sponsorship that covers a student's debt reduces what they owe, and every
-- reduction in this system is a signed adjustment. The kind is new, so the
-- constraint that enumerates them has to learn it: an adjustment type the check
-- refuses is a commitment that cannot be recorded at all.
ALTER TABLE account_adjustment
    DROP CONSTRAINT IF EXISTS ck_adjustment_type;

ALTER TABLE account_adjustment
    ADD CONSTRAINT ck_adjustment_type CHECK (adjustment_type IN (
        'transfer_credit_out', 'transfer_credit_in', 'retroactive_discount',
        'discount_reversal', 'late_result_correction', 'closed_year_correction',
        'waiver', 'write_off', 'correction',
        -- A third party covering part of the fees. Distinct from a discount:
        -- the university is still owed the money, by somebody else.
        'sponsorship'
    ));

CREATE TABLE sponsor (
    id           UUID        PRIMARY KEY,
    code         TEXT        NOT NULL,
    name_ar      TEXT        NOT NULL,
    name_en      TEXT,
    -- What kind of body this is. Not a foreign key: the list is short, stable
    -- and only ever used for grouping a report.
    sponsor_type TEXT        NOT NULL DEFAULT 'other',

    contact_name  TEXT,
    contact_phone TEXT,
    contact_email TEXT,
    address       TEXT,
    notes         TEXT,

    is_active    BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   UUID        REFERENCES app_user (id),

    CONSTRAINT ck_sponsor_code CHECK (code ~ '^[A-Z0-9_]{2,32}$'),
    CONSTRAINT ck_sponsor_type CHECK (
        sponsor_type IN ('ministry', 'government', 'company', 'charity', 'individual', 'other')
    )
);

CREATE UNIQUE INDEX uq_sponsor_code ON sponsor (code);
CREATE INDEX ix_sponsor_active ON sponsor (is_active) WHERE is_active;

CREATE TRIGGER trg_sponsor_updated_at
    BEFORE UPDATE ON sponsor
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE sponsor IS
    'A body that pays part of a student''s fees: a ministry, a company, a charity. Distinct '
    'from a discount, which is money the university chose not to charge.';

-- The agreement: who is sponsored, by whom, for how much, on what terms.
CREATE TABLE sponsorship (
    id           UUID        PRIMARY KEY,
    sponsor_id   UUID        NOT NULL REFERENCES sponsor (id) ON DELETE RESTRICT,
    student_id   UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,

    -- Coverage. Exactly one shape is used, which the check below enforces: a
    -- percentage of the discountable base, a fixed amount per year, or the
    -- whole obligation.
    coverage_type TEXT       NOT NULL,
    coverage_bp   INTEGER,
    coverage_amount BIGINT,
    -- An upper bound in dinars per academic year, whatever the shape. Ministry
    -- agreements are routinely "eighty per cent, up to two million".
    annual_cap    BIGINT,

    -- How the commitment behaves. No default: see the header.
    settlement_mode TEXT     NOT NULL,

    -- The window the agreement covers, by academic year code so it survives a
    -- year being recreated.
    from_year_code TEXT      NOT NULL,
    to_year_code   TEXT,

    status        TEXT       NOT NULL DEFAULT 'draft',
    agreement_ref TEXT,
    notes         TEXT,

    approved_at   TIMESTAMPTZ,
    approved_by   UUID       REFERENCES app_user (id),
    revoked_at    TIMESTAMPTZ,
    revoked_by    UUID       REFERENCES app_user (id),
    revoked_reason TEXT,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by    UUID        REFERENCES app_user (id),

    CONSTRAINT ck_sponsorship_coverage_type CHECK (
        coverage_type IN ('percentage', 'fixed_per_year', 'full')
    ),
    CONSTRAINT ck_sponsorship_settlement CHECK (settlement_mode IN ('receivable', 'covers_debt')),
    CONSTRAINT ck_sponsorship_status CHECK (
        status IN ('draft', 'active', 'suspended', 'revoked', 'expired')
    ),
    -- The shape must carry the figure it needs and no other, so a percentage
    -- agreement cannot quietly also hold an amount nobody looks at.
    CONSTRAINT ck_sponsorship_coverage_shape CHECK (
        (coverage_type = 'percentage'     AND coverage_bp IS NOT NULL AND coverage_amount IS NULL) OR
        (coverage_type = 'fixed_per_year' AND coverage_amount IS NOT NULL AND coverage_bp IS NULL) OR
        (coverage_type = 'full'           AND coverage_bp IS NULL AND coverage_amount IS NULL)
    ),
    CONSTRAINT ck_sponsorship_bp CHECK (coverage_bp IS NULL OR (coverage_bp > 0 AND coverage_bp <= 10000)),
    CONSTRAINT ck_sponsorship_amounts CHECK (
        (coverage_amount IS NULL OR coverage_amount > 0) AND (annual_cap IS NULL OR annual_cap > 0)
    ),
    CONSTRAINT ck_sponsorship_years CHECK (to_year_code IS NULL OR to_year_code >= from_year_code)
);

-- One live agreement per (sponsor, student). Two would make "how much does
-- this sponsor owe for this student" ambiguous, which is the question the whole
-- table exists to answer.
CREATE UNIQUE INDEX uq_sponsorship_live ON sponsorship (sponsor_id, student_id)
    WHERE status IN ('draft', 'active', 'suspended');
CREATE INDEX ix_sponsorship_student ON sponsorship (student_id) WHERE status = 'active';
CREATE INDEX ix_sponsorship_sponsor ON sponsorship (sponsor_id, status);

CREATE TRIGGER trg_sponsorship_updated_at
    BEFORE UPDATE ON sponsorship
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON COLUMN sponsorship.settlement_mode IS
    'receivable: the student still owes it and the sponsor''s share is an expected inflow. '
    'covers_debt: the student''s obligation falls by an adjustment and the university carries '
    'the loss if the sponsor defaults. Deliberately without a default — the choice decides who '
    'receives a debt letter.';

-- What a sponsorship worked out to for one account, frozen when the account was
-- generated.
--
-- The same rule as a discount application and for the same reason: a commitment
-- recomputed from the agreement in three years would answer with today's terms
-- rather than the ones the student was admitted under.
CREATE TABLE sponsor_commitment (
    id             UUID        PRIMARY KEY,
    sponsorship_id UUID        NOT NULL REFERENCES sponsorship (id) ON DELETE RESTRICT,
    sponsor_id     UUID        NOT NULL REFERENCES sponsor (id) ON DELETE RESTRICT,
    account_id     UUID        NOT NULL REFERENCES financial_account (id) ON DELETE RESTRICT,
    student_id     UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    academic_year_id UUID      NOT NULL REFERENCES academic_year (id) ON DELETE RESTRICT,

    -- The base the share was computed against, and the result. Both frozen.
    frozen_base    BIGINT      NOT NULL,
    committed_amount BIGINT    NOT NULL,
    -- What the sponsor has actually paid against this commitment, maintained
    -- inside the same transaction as the payment that changes it — the same
    -- treatment every other cached total in this schema gets.
    paid_amount    BIGINT      NOT NULL DEFAULT 0,

    settlement_mode TEXT       NOT NULL,
    status         TEXT        NOT NULL DEFAULT 'open',
    -- The adjustment that reduced the student's obligation, for a covers_debt
    -- commitment. NULL under receivable, where nothing was adjusted.
    adjustment_id  UUID        REFERENCES account_adjustment (id),

    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by     UUID        REFERENCES app_user (id),

    CONSTRAINT ck_commitment_amounts CHECK (
        frozen_base >= 0 AND committed_amount >= 0 AND paid_amount >= 0
    ),
    CONSTRAINT ck_commitment_status CHECK (status IN ('open', 'settled', 'written_off', 'cancelled')),
    CONSTRAINT ck_commitment_settlement CHECK (settlement_mode IN ('receivable', 'covers_debt')),
    CONSTRAINT ck_commitment_adjustment CHECK (
        settlement_mode = 'covers_debt' OR adjustment_id IS NULL
    )
);

-- One commitment per (sponsorship, account): a second would double the
-- sponsor's obligation for one year.
CREATE UNIQUE INDEX uq_commitment_per_account ON sponsor_commitment (sponsorship_id, account_id);
CREATE INDEX ix_commitment_sponsor_open ON sponsor_commitment (sponsor_id) WHERE status = 'open';
CREATE INDEX ix_commitment_account ON sponsor_commitment (account_id);

CREATE TRIGGER trg_commitment_updated_at
    BEFORE UPDATE ON sponsor_commitment
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The committed and frozen figures cannot move; what a sponsor has paid can.
CREATE TRIGGER trg_commitment_immutable
    BEFORE UPDATE OR DELETE ON sponsor_commitment
    FOR EACH ROW EXECUTE FUNCTION forbid_column_mutation('paid_amount', 'status', 'updated_at');

COMMENT ON TABLE sponsor_commitment IS
    'What a sponsorship worked out to for one account, frozen at generation. The receivable '
    'the university can actually invoice.';

-- A payment made by a sponsor rather than by the student.
--
-- The payment row itself is unchanged — it is an ordinary collection against
-- the student's account, allocated the ordinary way — and this column is what
-- makes it attributable. Without it, "how much has this ministry paid us" is a
-- question answerable only by reading payer names.
ALTER TABLE payment
    ADD COLUMN sponsor_id UUID REFERENCES sponsor (id);

CREATE INDEX ix_payment_sponsor ON payment (sponsor_id) WHERE sponsor_id IS NOT NULL;

COMMENT ON COLUMN payment.sponsor_id IS
    'Set when a sponsor paid rather than the student. The collection is otherwise ordinary: '
    'same receipt series, same allocation, same rules.';

-- What each sponsor owes and has paid, per year.
CREATE VIEW v_sponsor_receivable AS
SELECT
    s.id                              AS sponsor_id,
    s.code                            AS sponsor_code,
    s.name_ar                         AS sponsor_name,
    c.academic_year_id,
    count(*)                          AS commitment_count,
    count(DISTINCT c.student_id)      AS student_count,
    coalesce(sum(c.committed_amount), 0)::bigint AS committed_total,
    coalesce(sum(c.paid_amount), 0)::bigint      AS paid_total,
    coalesce(sum(c.committed_amount - c.paid_amount), 0)::bigint AS outstanding_total
FROM sponsor s
JOIN sponsor_commitment c ON c.sponsor_id = s.id
WHERE c.status = 'open'
GROUP BY s.id, s.code, s.name_ar, c.academic_year_id;

COMMENT ON VIEW v_sponsor_receivable IS
    'What each sponsor has committed, paid and still owes, per academic year. The invoice '
    'list a finance office works from — and the thing modelling a sponsorship as a discount '
    'made impossible to produce.';
