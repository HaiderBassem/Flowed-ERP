-- Fee and installment configuration.
--
-- No amount in this system is decided by code. A fee is a row whose scope
-- columns say which enrollments it covers, and adding next year's prices is
-- data entry, not a deployment. The resolution rule that picks between
-- overlapping rows is arithmetic, not precedence baked into a query.

-- ---------------------------------------------------------------------------
-- Fee policy
-- ---------------------------------------------------------------------------

CREATE TABLE fee_policy_version (
    id                      UUID        PRIMARY KEY,
    policy_code             TEXT        NOT NULL,
    version_no              INTEGER     NOT NULL DEFAULT 1,

    -- Scope. A NULL dimension is a wildcard: "any department", "any stage".
    -- The academic year is never a wildcard — prices belong to a year.
    academic_year_id        UUID        NOT NULL REFERENCES academic_year (id) ON DELETE RESTRICT,
    college_id              UUID        REFERENCES college (id) ON DELETE RESTRICT,
    department_id           UUID        REFERENCES department (id) ON DELETE RESTRICT,
    stage                   SMALLINT,
    study_type_id           UUID        REFERENCES study_type (id) ON DELETE RESTRICT,
    student_category_id     UUID        REFERENCES student_category (id) ON DELETE RESTRICT,

    -- Which of several matching rows wins. The weights are powers of two, so
    -- every distinct combination of specified dimensions produces a distinct
    -- score and two different scopes can never tie. A score computed by
    -- counting non-null dimensions instead would tie {college} against
    -- {department, stage, category} and silently charge different students
    -- different tuition depending on which row the planner returned first.
    --
    -- Generated, so the score cannot disagree with the scope it describes.
    specificity_score       INTEGER GENERATED ALWAYS AS (
        (CASE WHEN college_id          IS NOT NULL THEN 16 ELSE 0 END) +
        (CASE WHEN department_id       IS NOT NULL THEN  8 ELSE 0 END) +
        (CASE WHEN stage               IS NOT NULL THEN  4 ELSE 0 END) +
        (CASE WHEN study_type_id       IS NOT NULL THEN  2 ELSE 0 END) +
        (CASE WHEN student_category_id IS NOT NULL THEN  1 ELSE 0 END)
    ) STORED,

    status                  TEXT        NOT NULL DEFAULT 'draft',
    -- The ceiling on total discount for accounts generated under this policy,
    -- as basis points of the discountable base. Held here rather than on the
    -- discount definitions so that it freezes with the year.
    max_discount_bp         INTEGER     NOT NULL DEFAULT 10000,

    effective_from          DATE,
    description             TEXT,

    published_at            TIMESTAMPTZ,
    published_by            UUID        REFERENCES app_user (id),
    retired_at              TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by              UUID        REFERENCES app_user (id),

    CONSTRAINT ck_fee_policy_version_no CHECK (version_no >= 1),
    CONSTRAINT ck_fee_policy_stage CHECK (stage IS NULL OR stage BETWEEN 1 AND 8),
    CONSTRAINT ck_fee_policy_status CHECK (status IN ('draft', 'published', 'retired')),
    CONSTRAINT ck_fee_policy_max_discount CHECK (max_discount_bp BETWEEN 0 AND 10000),
    CONSTRAINT ck_fee_policy_published_stamp CHECK (
        (status = 'published') <= (published_at IS NOT NULL)
    ),
    -- A department must belong to the college named alongside it. Checked in
    -- the application because SQL cannot join here; the column pair is kept so
    -- resolution stays a single indexed lookup.
    CONSTRAINT ck_fee_policy_dept_needs_college CHECK (
        department_id IS NULL OR college_id IS NOT NULL
    )
);

-- Only one published policy may claim any given scope, which is what makes
-- resolution deterministic.
--
-- NULLS NOT DISTINCT is the load-bearing clause. By default a unique index
-- treats every NULL as different, so two rows both meaning "2025-2026,
-- engineering, any department" would insert cleanly and resolution would find
-- two winners at the same specificity. Systems on databases without this
-- clause work around it with a sentinel value or a coalesced generated column;
-- here the intent is stated directly.
CREATE UNIQUE INDEX uq_fee_policy_scope
    ON fee_policy_version (
        academic_year_id, college_id, department_id, stage, study_type_id, student_category_id
    )
    NULLS NOT DISTINCT
    WHERE status = 'published';

CREATE UNIQUE INDEX uq_fee_policy_code_version ON fee_policy_version (policy_code, version_no);
CREATE INDEX ix_fee_policy_resolution
    ON fee_policy_version (academic_year_id, specificity_score DESC)
    WHERE status = 'published';

CREATE TRIGGER trg_fee_policy_updated_at
    BEFORE UPDATE ON fee_policy_version
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- A fee is several components, not one number. Tuition, registration,
-- laboratory, identity card: they carry different rules. Registration and
-- identity-card charges are typically not discountable and not refundable,
-- which is why a hundred-percent exemption must not zero them.
CREATE TABLE fee_component (
    id                  UUID        PRIMARY KEY,
    fee_policy_id       UUID        NOT NULL REFERENCES fee_policy_version (id) ON DELETE CASCADE,
    component_code      TEXT        NOT NULL,
    name_ar             TEXT        NOT NULL,
    name_en             TEXT,
    amount              BIGINT      NOT NULL,
    is_discountable     BOOLEAN     NOT NULL DEFAULT true,
    is_refundable       BOOLEAN     NOT NULL DEFAULT true,
    is_mandatory        BOOLEAN     NOT NULL DEFAULT true,
    sort_order          SMALLINT    NOT NULL DEFAULT 0,

    CONSTRAINT ck_fee_component_amount CHECK (amount >= 0),
    CONSTRAINT ck_fee_component_code CHECK (component_code ~ '^[A-Z0-9_]{2,32}$')
);

CREATE UNIQUE INDEX uq_fee_component_code ON fee_component (fee_policy_id, component_code);
CREATE INDEX ix_fee_component_policy ON fee_component (fee_policy_id);

-- ---------------------------------------------------------------------------
-- Installment templates
-- ---------------------------------------------------------------------------

-- The template says how a net amount is cut into installments. Shares may be
-- unequal — 500,000 / 300,000 / 400,000 / 300,000 is a perfectly normal Iraqi
-- plan — so a template is a list of weighted lines, not a count to divide by.
CREATE TABLE installment_template (
    id                  UUID        PRIMARY KEY,
    code                TEXT        NOT NULL,
    name_ar             TEXT        NOT NULL,
    name_en             TEXT,

    academic_year_id    UUID        REFERENCES academic_year (id) ON DELETE RESTRICT,
    college_id          UUID        REFERENCES college (id) ON DELETE RESTRICT,
    department_id       UUID        REFERENCES department (id) ON DELETE RESTRICT,
    stage               SMALLINT,
    study_type_id       UUID        REFERENCES study_type (id) ON DELETE RESTRICT,

    specificity_score   INTEGER GENERATED ALWAYS AS (
        (CASE WHEN academic_year_id IS NOT NULL THEN 16 ELSE 0 END) +
        (CASE WHEN college_id       IS NOT NULL THEN  8 ELSE 0 END) +
        (CASE WHEN department_id    IS NOT NULL THEN  4 ELSE 0 END) +
        (CASE WHEN stage            IS NOT NULL THEN  2 ELSE 0 END) +
        (CASE WHEN study_type_id    IS NOT NULL THEN  1 ELSE 0 END)
    ) STORED,

    max_installments    SMALLINT    NOT NULL,
    status              TEXT        NOT NULL DEFAULT 'draft',
    published_at        TIMESTAMPTZ,
    published_by        UUID        REFERENCES app_user (id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_installment_template_code CHECK (code ~ '^[A-Z0-9_]{2,32}$'),
    CONSTRAINT ck_installment_template_max CHECK (max_installments BETWEEN 1 AND 24),
    CONSTRAINT ck_installment_template_status CHECK (status IN ('draft', 'published', 'retired')),
    CONSTRAINT ck_installment_template_stage CHECK (stage IS NULL OR stage BETWEEN 1 AND 8)
);

CREATE UNIQUE INDEX uq_installment_template_code ON installment_template (code);
CREATE UNIQUE INDEX uq_installment_template_scope
    ON installment_template (academic_year_id, college_id, department_id, stage, study_type_id)
    NULLS NOT DISTINCT
    WHERE status = 'published';
CREATE INDEX ix_installment_template_resolution
    ON installment_template (academic_year_id, specificity_score DESC)
    WHERE status = 'published';

CREATE TRIGGER trg_installment_template_updated_at
    BEFORE UPDATE ON installment_template
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE installment_template_line (
    id                  UUID        PRIMARY KEY,
    template_id         UUID        NOT NULL REFERENCES installment_template (id) ON DELETE CASCADE,
    line_no             SMALLINT    NOT NULL,
    -- Share of the net, in basis points. The application validates that the
    -- lines of a template sum to exactly 10000 before the template may be
    -- published, so a plan generated from it always sums to the net rather
    -- than failing per student on the first day of registration.
    share_bp            INTEGER     NOT NULL,
    -- Due date as an offset in days from the academic year's start, so one
    -- template serves every year.
    due_offset_days     INTEGER     NOT NULL,
    label_ar            TEXT,

    CONSTRAINT ck_template_line_no CHECK (line_no >= 1),
    CONSTRAINT ck_template_line_share CHECK (share_bp > 0 AND share_bp <= 10000),
    CONSTRAINT ck_template_line_offset CHECK (due_offset_days >= 0)
);

CREATE UNIQUE INDEX uq_template_line_no ON installment_template_line (template_id, line_no);
CREATE INDEX ix_template_line_template ON installment_template_line (template_id);
