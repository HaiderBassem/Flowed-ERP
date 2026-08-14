-- The financial half of the academic lifecycle, graduation clearance, plan
-- revisions and student merge.
--
-- Four gaps, one subject: things the design specified and the schema half
-- carried, where the missing piece meant staff had to work around the system.
--
--  1. Deferring, withdrawing, dropping out and transferring out changed the
--     academic status and left the account exactly as it was — full debt, full
--     schedule, no record of a decision. The design is explicit that the
--     financial treatment must be a parameter of the command and never
--     implicit, so this adds the column that records which treatment was
--     chosen and the reason it was.
--
--  2. Graduation clearance (براءة الذمة) was named in the design as a
--     configurable block-or-flag and never built. It is per academic year,
--     beside the debt-block policy it resembles, because a university changes
--     this rule between years and every historical graduation must keep
--     showing the rule that applied to it.
--
--  3. Installment plans could be re-split by the domain and nothing recorded
--     why. A revision row makes "these dates moved, on this date, approved by
--     this person, for this reason" answerable without reading the audit log.
--
--  4. student.merged_into_id existed with no workflow behind it. Duplicates
--     are certain when intake comes off paper, and folding one record into
--     another moves nothing financial: the enrollments and their accounts stay
--     exactly where they are.

-- ---------------------------------------------------------------------------
-- The financial treatment of a status change
-- ---------------------------------------------------------------------------

ALTER TABLE enrollment
    -- Which treatment was applied when this enrollment left active status.
    -- NULL for an enrollment that never left it. The value is recorded even
    -- when it is 'keep', because "we decided to keep charging" and "nobody
    -- thought about it" are different answers to the same question, and only
    -- one of them can be defended to a student's family.
    ADD COLUMN financial_treatment      TEXT,
    ADD COLUMN financial_treatment_at   TIMESTAMPTZ,
    ADD COLUMN financial_treatment_by   UUID REFERENCES app_user (id),

    ADD CONSTRAINT ck_enrollment_financial_treatment CHECK (
        financial_treatment IS NULL OR financial_treatment IN (
            -- The obligation stands. A student who withdraws in June has
            -- consumed the year.
            'keep',
            -- The unpaid remainder is written off by adjustment. The paid part
            -- stays paid: receipts are printed and the money is in the drawer.
            'waive_unpaid',
            -- The whole obligation is reversed by adjustment, and anything
            -- already paid becomes a credit the student may reclaim.
            'waive_all',
            -- Charge a stated proportion and waive the rest, which is what a
            -- pro-rata withdrawal rule amounts to. The proportion is not
            -- stored here: the adjustment carries the amount, and a percentage
            -- in two places is a percentage that disagrees with itself.
            'partial'
        )
    );

COMMENT ON COLUMN enrollment.financial_treatment IS
    'What was done about the money when this enrollment stopped being active. '
    'Never inferred from the status: the design requires an explicit decision, because '
    'a deferral that quietly cancelled a debt and one that quietly kept it are both '
    'defensible policies and the system must record which was chosen.';

-- ---------------------------------------------------------------------------
-- Graduation clearance
-- ---------------------------------------------------------------------------

ALTER TABLE academic_year
    -- Mirrors debt_block_policy in shape deliberately: an operator who has
    -- learned one has learned both.
    ADD COLUMN graduation_clearance_policy TEXT NOT NULL DEFAULT 'warn',
    ADD CONSTRAINT ck_year_clearance_policy
        CHECK (graduation_clearance_policy IN ('ignore', 'warn', 'block'));

COMMENT ON COLUMN academic_year.graduation_clearance_policy IS
    'Whether an outstanding balance blocks graduation (block), is recorded and allowed '
    '(warn), or is not consulted (ignore). Per year, because universities change this '
    'rule and a graduation recorded in 2024 must keep showing the rule of 2024.';

-- Every clearance decision, whichever way it went.
CREATE TABLE graduation_clearance (
    id                 UUID        PRIMARY KEY,
    student_id         UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    enrollment_id      UUID        NOT NULL REFERENCES enrollment (id) ON DELETE RESTRICT,
    academic_year_id   UUID        NOT NULL REFERENCES academic_year (id) ON DELETE RESTRICT,

    -- What was owed across every account of this student at the moment of the
    -- decision. Frozen, like every other financial figure in this system: the
    -- balance moves afterwards and the certificate does not.
    outstanding_total  BIGINT      NOT NULL,
    -- The policy in force when the decision was taken, copied rather than
    -- referenced, for the same reason.
    policy             TEXT        NOT NULL,
    cleared            BOOLEAN     NOT NULL,
    -- Set when somebody cleared a student who owed money. An override with no
    -- name against it is the thing an auditor asks about first.
    override_reason    TEXT,
    override_by        UUID        REFERENCES app_user (id),

    decided_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by         UUID        REFERENCES app_user (id),

    CONSTRAINT ck_clearance_outstanding CHECK (outstanding_total >= 0),
    CONSTRAINT ck_clearance_policy CHECK (policy IN ('ignore', 'warn', 'block')),
    -- Clearing a debtor requires a written reason and a name. Clearing someone
    -- who owes nothing requires neither.
    CONSTRAINT ck_clearance_override CHECK (
        (outstanding_total = 0) OR (NOT cleared) OR (override_reason IS NOT NULL AND override_by IS NOT NULL)
    )
);

CREATE INDEX ix_clearance_student ON graduation_clearance (student_id, decided_at DESC);
CREATE INDEX ix_clearance_year ON graduation_clearance (academic_year_id) WHERE NOT cleared;

-- Append-only: the value of a clearance record is that the decision cannot be
-- revised after the certificate is printed.
CREATE TRIGGER trg_clearance_append_only
    BEFORE UPDATE OR DELETE ON graduation_clearance
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

COMMENT ON TABLE graduation_clearance IS
    'One row per graduation clearance decision (براءة الذمة), including the ones that '
    'refused. Append-only.';

-- ---------------------------------------------------------------------------
-- Installment plan revisions
-- ---------------------------------------------------------------------------

CREATE TABLE installment_plan_revision (
    id               UUID        PRIMARY KEY,
    account_id       UUID        NOT NULL REFERENCES financial_account (id) ON DELETE RESTRICT,
    -- The plan_version the installments carry after this revision.
    plan_version     SMALLINT    NOT NULL,
    kind             TEXT        NOT NULL,
    reason           TEXT        NOT NULL,

    -- What the plan looked like on each side of the change, as counts and
    -- totals. The rows themselves are still there — superseded, never deleted
    -- — and this is the summary that makes the history readable without
    -- reconstructing it.
    installments_before SMALLINT NOT NULL,
    installments_after  SMALLINT NOT NULL,
    unpaid_before        BIGINT  NOT NULL,
    unpaid_after         BIGINT  NOT NULL,

    -- A supervisor's signature, required once the plan has taken money.
    approved_by      UUID        REFERENCES app_user (id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by       UUID        REFERENCES app_user (id),

    CONSTRAINT ck_plan_revision_kind CHECK (kind IN ('reschedule', 'resplit', 'regenerate')),
    CONSTRAINT ck_plan_revision_reason CHECK (reason <> ''),
    CONSTRAINT ck_plan_revision_version CHECK (plan_version >= 1),
    CONSTRAINT ck_plan_revision_totals CHECK (unpaid_before >= 0 AND unpaid_after >= 0)
);

CREATE INDEX ix_plan_revision_account ON installment_plan_revision (account_id, created_at DESC);

CREATE TRIGGER trg_plan_revision_append_only
    BEFORE UPDATE OR DELETE ON installment_plan_revision
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

COMMENT ON TABLE installment_plan_revision IS
    'Why an installment plan changed. The superseded installment rows carry what it was; '
    'this carries who changed it and on what authority.';

-- ---------------------------------------------------------------------------
-- Student merge
-- ---------------------------------------------------------------------------

CREATE TABLE student_merge (
    id             UUID        PRIMARY KEY,
    -- The record folded away. It keeps every row it ever had.
    source_id      UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    -- The record that survives as the person's identity.
    target_id      UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,

    reason         TEXT        NOT NULL,
    -- What moved, so the operation can be understood without diffing two
    -- records afterwards.
    enrollments_moved SMALLINT NOT NULL DEFAULT 0,
    accounts_moved    SMALLINT NOT NULL DEFAULT 0,
    discounts_moved   SMALLINT NOT NULL DEFAULT 0,

    merged_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    merged_by      UUID        REFERENCES app_user (id),

    CONSTRAINT ck_student_merge_distinct CHECK (source_id <> target_id),
    CONSTRAINT ck_student_merge_reason CHECK (reason <> '')
);

-- A record can be merged away exactly once. Merging it twice would leave two
-- different targets both claiming the same history.
CREATE UNIQUE INDEX uq_student_merge_source ON student_merge (source_id);
CREATE INDEX ix_student_merge_target ON student_merge (target_id, merged_at DESC);

CREATE TRIGGER trg_student_merge_append_only
    BEFORE UPDATE OR DELETE ON student_merge
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

COMMENT ON TABLE student_merge IS
    'One row per duplicate folded into a canonical record. The source student row stays, '
    'marked merged and pointing at the target, so a receipt printed under the old '
    'identity still resolves.';

-- A merged record must point at where it went, and a record pointing somewhere
-- must be marked merged. Half of either is a record that disappears from
-- search while still owning payments.
ALTER TABLE student
    ADD CONSTRAINT ck_student_merge_consistent CHECK (
        (status = 'merged') = (merged_into_id IS NOT NULL)
    );
