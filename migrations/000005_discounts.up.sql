-- The discount subsystem, in three layers.
--
--   definition  — what the discount is. Split into a stable header and
--                 immutable versions, because the whole historical-integrity
--                 guarantee rests on an application pointing at a version row
--                 that physically cannot change.
--   assignment  — who was granted it, and for which years.
--   application — how much was actually taken off one account in one year,
--                 frozen at the moment it was computed.
--
-- Raising the teachers'-children discount from twenty to twenty-five percent
-- next year publishes a new version row. Last year's accounts keep pointing at
-- the old one. There is no path from a configuration edit to a historical
-- number, because no historical row holds a reference to "the current value".

-- ---------------------------------------------------------------------------
-- Definitions
-- ---------------------------------------------------------------------------

-- Discounts in the same exclusivity group cannot both apply to one account:
-- a student is either a staff child or a hardship case, not both at full rate.
CREATE TABLE discount_exclusivity_group (
    id          UUID        PRIMARY KEY,
    code        TEXT        NOT NULL,
    name_ar     TEXT        NOT NULL,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_exclusivity_group_code CHECK (code ~ '^[A-Z0-9_]{2,32}$')
);

CREATE UNIQUE INDEX uq_exclusivity_group_code ON discount_exclusivity_group (code);

CREATE TABLE discount_definition (
    id                      UUID        PRIMARY KEY,
    code                    TEXT        NOT NULL,
    name_ar                 TEXT        NOT NULL,
    name_en                 TEXT,
    category                TEXT        NOT NULL DEFAULT 'other',
    exclusivity_group_id    UUID        REFERENCES discount_exclusivity_group (id),
    is_full_exemption       BOOLEAN     NOT NULL DEFAULT false,

    -- Whether eligibility must be re-confirmed each year before the discount
    -- takes effect. A staff benefit runs on; a hardship discount does not,
    -- because the hardship may have ended. This flag is what makes an
    -- all-years grant safe: the application still materialises automatically,
    -- but it waits on an officer's confirmation before it reduces anything.
    annual_reconfirmation   BOOLEAN     NOT NULL DEFAULT true,

    is_active               BOOLEAN     NOT NULL DEFAULT true,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by              UUID        REFERENCES app_user (id),

    CONSTRAINT ck_discount_definition_code CHECK (code ~ '^[A-Z0-9_]{2,48}$'),
    CONSTRAINT ck_discount_definition_category CHECK (category IN (
        'social', 'staff', 'merit', 'exemption', 'sibling', 'martyr', 'other'
    ))
);

CREATE UNIQUE INDEX uq_discount_definition_code ON discount_definition (code);

CREATE TRIGGER trg_discount_definition_updated_at
    BEFORE UPDATE ON discount_definition
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- A version, once published, is frozen by trigger. Only the status and the
-- retirement stamp may move afterwards.
CREATE TABLE discount_definition_version (
    id                      UUID        PRIMARY KEY,
    definition_id           UUID        NOT NULL REFERENCES discount_definition (id) ON DELETE RESTRICT,
    version_no              INTEGER     NOT NULL,

    value_type              TEXT        NOT NULL,
    -- Percentage rates are basis points: 1000 is ten percent. An integer rate
    -- keeps the arithmetic exactly reproducible years later, which a float
    -- rate of 12.5 percent would not be.
    value_bp                INTEGER,
    -- Fixed grants are whole dinars.
    value_amount            BIGINT,

    -- Which fee components the discount may touch. NULL means every
    -- discountable component; a code list narrows it, so an exemption can
    -- cover tuition while leaving the identity-card charge payable.
    applies_to_components   TEXT[],
    per_application_cap     BIGINT,

    stackable               BOOLEAN     NOT NULL DEFAULT true,
    -- Lower runs first. Order matters only when a cap or the floor truncates
    -- the tail, but when it matters, it must be deterministic.
    priority                SMALLINT    NOT NULL DEFAULT 100,

    requires_approval       BOOLEAN     NOT NULL DEFAULT true,
    approval_role           TEXT,
    required_documents      TEXT[],

    valid_from_year_id      UUID        REFERENCES academic_year (id),
    valid_to_year_id        UUID        REFERENCES academic_year (id),

    status                  TEXT        NOT NULL DEFAULT 'draft',
    notes                   TEXT,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by              UUID        REFERENCES app_user (id),
    published_at            TIMESTAMPTZ,
    published_by            UUID        REFERENCES app_user (id),
    retired_at              TIMESTAMPTZ,

    CONSTRAINT ck_discount_version_no CHECK (version_no >= 1),
    CONSTRAINT ck_discount_value_type CHECK (value_type IN ('percentage', 'fixed')),
    CONSTRAINT ck_discount_value_present CHECK (
        (value_type = 'percentage' AND value_bp IS NOT NULL AND value_amount IS NULL)
        OR
        (value_type = 'fixed' AND value_amount IS NOT NULL AND value_bp IS NULL)
    ),
    CONSTRAINT ck_discount_value_bp_range CHECK (value_bp IS NULL OR value_bp BETWEEN 0 AND 10000),
    CONSTRAINT ck_discount_value_amount_range CHECK (value_amount IS NULL OR value_amount >= 0),
    CONSTRAINT ck_discount_cap CHECK (per_application_cap IS NULL OR per_application_cap > 0),
    CONSTRAINT ck_discount_version_status CHECK (status IN ('draft', 'published', 'retired')),
    CONSTRAINT ck_discount_version_published_stamp CHECK (
        (status = 'published') <= (published_at IS NOT NULL AND published_by IS NOT NULL)
    )
);

CREATE UNIQUE INDEX uq_discount_version ON discount_definition_version (definition_id, version_no);
-- One version of a definition is current at a time. A partial unique index
-- states that without a nullable flag column.
CREATE UNIQUE INDEX uq_discount_version_published
    ON discount_definition_version (definition_id)
    WHERE status = 'published';
CREATE INDEX ix_discount_version_definition ON discount_definition_version (definition_id, version_no DESC);

-- Everything except the lifecycle columns is frozen. Publishing a version is
-- a promise that the numbers behind every application referencing it will
-- still be there, unchanged, at audit time.
CREATE TRIGGER trg_discount_version_immutable
    BEFORE UPDATE OR DELETE ON discount_definition_version
    FOR EACH ROW EXECUTE FUNCTION forbid_column_mutation(
        'status', 'published_at', 'published_by', 'retired_at', 'notes'
    );

-- ---------------------------------------------------------------------------
-- Assignments
-- ---------------------------------------------------------------------------

-- A grant to a student, not to an enrollment. Scope decides which years it can
-- reach; it never reaches back into an account that already exists.
CREATE TABLE discount_assignment (
    id                      UUID        PRIMARY KEY,
    student_id              UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    definition_id           UUID        NOT NULL REFERENCES discount_definition (id) ON DELETE RESTRICT,

    scope_type              TEXT        NOT NULL,
    scope_year_from_id      UUID        REFERENCES academic_year (id),
    scope_year_to_id        UUID        REFERENCES academic_year (id),
    -- Denormalised year codes so overlap checks and scope tests are string
    -- comparisons on '2025-2026' rather than joins through academic_year.
    scope_year_from_code    TEXT,
    scope_year_to_code      TEXT,

    status                  TEXT        NOT NULL DEFAULT 'draft',
    justification           TEXT,
    document_refs           TEXT[],

    requested_by            UUID        REFERENCES app_user (id),
    requested_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_by             UUID        REFERENCES app_user (id),
    approved_at             TIMESTAMPTZ,
    rejection_reason        TEXT,
    revoked_by              UUID        REFERENCES app_user (id),
    revoked_at              TIMESTAMPTZ,
    revocation_reason       TEXT,
    -- Whether revoking also reverses the current year's application or only
    -- stops future ones. Chosen by the approver at revocation time.
    revocation_effect       TEXT,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_assignment_scope_type CHECK (scope_type IN ('single_year', 'year_range', 'all_years')),
    CONSTRAINT ck_assignment_status CHECK (status IN (
        'draft', 'submitted', 'approved', 'rejected', 'revoked', 'expired', 'cancelled'
    )),
    CONSTRAINT ck_assignment_scope_years CHECK (
        CASE scope_type
            WHEN 'single_year' THEN scope_year_from_id IS NOT NULL AND scope_year_to_id IS NULL
            WHEN 'year_range'  THEN scope_year_from_id IS NOT NULL AND scope_year_to_id IS NOT NULL
            WHEN 'all_years'   THEN true
        END
    ),
    CONSTRAINT ck_assignment_range_order CHECK (
        scope_year_from_code IS NULL OR scope_year_to_code IS NULL
        OR scope_year_to_code >= scope_year_from_code
    ),
    -- Approval is a second person's act, and the record must show it.
    CONSTRAINT ck_assignment_approved_stamp CHECK (
        (status = 'approved') <= (approved_by IS NOT NULL AND approved_at IS NOT NULL)
    ),
    CONSTRAINT ck_assignment_four_eyes CHECK (
        approved_by IS NULL OR requested_by IS NULL OR approved_by <> requested_by
    ),
    CONSTRAINT ck_assignment_revoked_stamp CHECK (
        (status = 'revoked') <= (
            revoked_by IS NOT NULL AND revoked_at IS NOT NULL AND revocation_effect IS NOT NULL
        )
    ),
    CONSTRAINT ck_assignment_revocation_effect CHECK (
        revocation_effect IS NULL OR revocation_effect IN ('prospective_only', 'include_current_year')
    )
);

CREATE INDEX ix_assignment_student ON discount_assignment (student_id, status);
CREATE INDEX ix_assignment_definition ON discount_assignment (definition_id);
CREATE INDEX ix_assignment_pending ON discount_assignment (status) WHERE status = 'submitted';
-- Live assignments of one definition to one student, used by the overlap check
-- that runs before approval.
CREATE INDEX ix_assignment_active_scope
    ON discount_assignment (student_id, definition_id, scope_year_from_code, scope_year_to_code)
    WHERE status IN ('draft', 'submitted', 'approved');

CREATE TRIGGER trg_assignment_updated_at
    BEFORE UPDATE ON discount_assignment
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
