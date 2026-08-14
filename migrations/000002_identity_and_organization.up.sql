-- Identity and organisational structure.
--
-- The central rule of this schema appears here for the first time: a student
-- row answers only "who is this person". It carries no stage, no department,
-- no study type, and no money. Anything whose value could differ between two
-- academic years lives on an enrollment, created in the next migration.

-- ---------------------------------------------------------------------------
-- Application users
-- ---------------------------------------------------------------------------

CREATE TABLE app_user (
    id              UUID        PRIMARY KEY,
    username        TEXT        NOT NULL,
    full_name       TEXT        NOT NULL,
    password_hash   TEXT        NOT NULL,
    email           TEXT,
    is_active       BOOLEAN     NOT NULL DEFAULT true,
    last_login_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by      UUID,
    CONSTRAINT ck_app_user_username_format CHECK (username ~ '^[a-z0-9._-]{3,64}$')
);

CREATE UNIQUE INDEX uq_user_username ON app_user (username);
CREATE INDEX ix_app_user_active ON app_user (is_active) WHERE is_active;

CREATE TRIGGER trg_app_user_updated_at
    BEFORE UPDATE ON app_user
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Roles are stored as rows rather than as an array column so that granting and
-- revoking authority is itself an auditable insert or delete rather than an
-- opaque rewrite of a list.
CREATE TABLE app_user_role (
    user_id     UUID        NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    role        TEXT        NOT NULL,
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    granted_by  UUID        REFERENCES app_user (id),
    PRIMARY KEY (user_id, role),
    CONSTRAINT ck_app_user_role_value CHECK (role IN (
        'admin', 'finance_manager', 'cashier', 'registrar',
        'academic_officer', 'report_viewer', 'auditor'
    ))
);

CREATE INDEX ix_app_user_role_role ON app_user_role (role);

-- ---------------------------------------------------------------------------
-- Organisational structure
-- ---------------------------------------------------------------------------

CREATE TABLE college (
    id          UUID        PRIMARY KEY,
    code        TEXT        NOT NULL,
    name_ar     TEXT        NOT NULL,
    name_en     TEXT,
    is_active   BOOLEAN     NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_college_code_format CHECK (code ~ '^[A-Z0-9_]{2,32}$')
);

CREATE UNIQUE INDEX uq_college_code ON college (code);

CREATE TRIGGER trg_college_updated_at
    BEFORE UPDATE ON college
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE department (
    id          UUID        PRIMARY KEY,
    college_id  UUID        NOT NULL REFERENCES college (id) ON DELETE RESTRICT,
    code        TEXT        NOT NULL,
    name_ar     TEXT        NOT NULL,
    name_en     TEXT,
    -- Programme length in years. Medicine runs six, engineering four or five;
    -- the enrollment stage is validated against this rather than a global 1..4.
    stage_count SMALLINT    NOT NULL DEFAULT 4,
    is_active   BOOLEAN     NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_department_code_format CHECK (code ~ '^[A-Z0-9_]{2,32}$'),
    CONSTRAINT ck_department_stage_count CHECK (stage_count BETWEEN 1 AND 8)
);

CREATE UNIQUE INDEX uq_department_code ON department (college_id, code);
CREATE INDEX ix_department_college ON department (college_id);

CREATE TRIGGER trg_department_updated_at
    BEFORE UPDATE ON department
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Study type is master data, never a hardcoded enumeration. Morning, evening
-- and parallel are seed rows; a type the ministry introduces in three years is
-- one INSERT, with no code change and no migration.
--
-- Hosting is deliberately absent from this table. A hosted student has both a
-- home study type and the one attended locally, and collapsing that pair into
-- a single "hosting" value would destroy the distinction and force fabricated
-- fee-policy rows for each direction. Hosting is modelled as an overlay on the
-- enrollment in the next migration.
CREATE TABLE study_type (
    id          UUID        PRIMARY KEY,
    code        TEXT        NOT NULL,
    name_ar     TEXT        NOT NULL,
    name_en     TEXT,
    sort_order  SMALLINT    NOT NULL DEFAULT 0,
    is_active   BOOLEAN     NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_study_type_code_format CHECK (code ~ '^[A-Z0-9_]{2,32}$')
);

CREATE UNIQUE INDEX uq_study_type_code ON study_type (code);

CREATE TRIGGER trg_study_type_updated_at
    BEFORE UPDATE ON study_type
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Student category drives fee resolution: a repeating student may owe a
-- different tuition than a first-attempt student in the same seat. Also master
-- data, for the same reason study type is.
CREATE TABLE student_category (
    id          UUID        PRIMARY KEY,
    code        TEXT        NOT NULL,
    name_ar     TEXT        NOT NULL,
    name_en     TEXT,
    is_active   BOOLEAN     NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_student_category_code_format CHECK (code ~ '^[A-Z0-9_]{2,32}$')
);

CREATE UNIQUE INDEX uq_student_category_code ON student_category (code);

CREATE TRIGGER trg_student_category_updated_at
    BEFORE UPDATE ON student_category
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Student identity
-- ---------------------------------------------------------------------------

CREATE TABLE student (
    id                  UUID        PRIMARY KEY,
    -- The registrar-issued university number. Never recycled, not even after
    -- graduation or death: a recycled number would silently attach one
    -- person's payment history to another.
    student_no          TEXT        NOT NULL,

    -- Current legal identity, denormalised from the version history below for
    -- search and display. The version rows remain the record of truth.
    full_name           TEXT        NOT NULL,
    mother_name         TEXT        NOT NULL,
    national_id         TEXT,
    birth_date          DATE,
    gender              TEXT,

    -- Contact details are mutable in place: nobody needs to know which phone
    -- number a student had in 2024, only how to reach them now. Changes are
    -- still recorded in the audit log.
    phone               TEXT,
    phone_alt           TEXT,
    email               TEXT,
    address             TEXT,
    guardian_name       TEXT,
    guardian_phone      TEXT,

    first_admission_year TEXT,
    status              TEXT        NOT NULL DEFAULT 'active',
    -- Duplicate identities discovered after the fact are tombstoned into the
    -- surviving row rather than deleted, so that a payment receipt printed
    -- against the losing id still resolves.
    merged_into_id      UUID        REFERENCES student (id),

    notes               TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by          UUID        REFERENCES app_user (id),

    -- Search columns are generated, not application-populated. A generated
    -- column cannot drift from its source: there is no code path that writes a
    -- name without folding it, because the database does the folding.
    full_name_norm      TEXT GENERATED ALWAYS AS (normalize_arabic(full_name)) STORED,
    mother_name_norm    TEXT GENERATED ALWAYS AS (normalize_arabic(mother_name)) STORED,
    phone_norm          TEXT GENERATED ALWAYS AS (normalize_phone(phone)) STORED,
    phone_alt_norm      TEXT GENERATED ALWAYS AS (normalize_phone(phone_alt)) STORED,
    phone_rev           TEXT GENERATED ALWAYS AS (reverse_text(normalize_phone(phone))) STORED,

    CONSTRAINT ck_student_no_format CHECK (student_no ~ '^[A-Za-z0-9/_-]{1,32}$'),
    CONSTRAINT ck_student_status CHECK (status IN (
        'active', 'separated', 'transferred_out', 'graduated', 'deceased', 'merged'
    )),
    CONSTRAINT ck_student_gender CHECK (gender IS NULL OR gender IN ('male', 'female')),
    CONSTRAINT ck_student_merge_consistency CHECK (
        (status = 'merged') = (merged_into_id IS NOT NULL)
    ),
    CONSTRAINT ck_student_no_self_merge CHECK (merged_into_id IS NULL OR merged_into_id <> id)
);

CREATE UNIQUE INDEX uq_student_number ON student (student_no);
CREATE UNIQUE INDEX uq_student_national_id ON student (national_id) WHERE national_id IS NOT NULL;

-- Prefix search on a folded name, the common case when a clerk types the first
-- part of a name. text_pattern_ops makes LIKE 'علي محم%' index-eligible
-- regardless of the database collation.
CREATE INDEX ix_student_name_prefix ON student (full_name_norm text_pattern_ops);
CREATE INDEX ix_student_mother_prefix ON student (mother_name_norm text_pattern_ops);

-- Substring search on a folded name. A B-tree cannot serve LIKE '%محمد%', and
-- in Iraq the distinguishing part of a name is often in the middle. A trigram
-- index answers it directly, which is the main reason this system is on
-- PostgreSQL rather than a database where the fallback is a full scan.
CREATE INDEX ix_student_name_trgm ON student USING gin (full_name_norm gin_trgm_ops);
CREATE INDEX ix_student_mother_trgm ON student USING gin (mother_name_norm gin_trgm_ops);

-- The canonical Iraqi disambiguation query: same name, different mother.
CREATE INDEX ix_student_name_mother ON student (full_name_norm, mother_name_norm);

CREATE INDEX ix_student_phone ON student (phone_norm) WHERE phone_norm IS NOT NULL;
CREATE INDEX ix_student_phone_alt ON student (phone_alt_norm) WHERE phone_alt_norm IS NOT NULL;
-- Suffix search ("the number ending 4567") as an indexable prefix scan.
CREATE INDEX ix_student_phone_rev ON student (phone_rev text_pattern_ops) WHERE phone_rev IS NOT NULL;
CREATE INDEX ix_student_status ON student (status) WHERE status <> 'active';

CREATE TRIGGER trg_student_updated_at
    BEFORE UPDATE ON student
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Legal identity changes in Iraq arrive by court decision, and documents
-- already issued must keep referring to the name that was in force when they
-- were printed. Each version is an insert; none is ever edited.
CREATE TABLE student_identity_version (
    id                  UUID        PRIMARY KEY,
    student_id          UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    version_no          INTEGER     NOT NULL,
    full_name           TEXT        NOT NULL,
    mother_name         TEXT        NOT NULL,
    national_id         TEXT,
    birth_date          DATE,
    birth_place         TEXT,
    gender              TEXT,
    nationality         TEXT,
    effective_from      DATE        NOT NULL,
    court_decision_no   TEXT,
    court_decision_date DATE,
    document_ref        TEXT,
    change_reason       TEXT        NOT NULL,
    recorded_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    recorded_by         UUID        REFERENCES app_user (id),
    CONSTRAINT ck_identity_version_no CHECK (version_no >= 1),
    CONSTRAINT ck_identity_version_gender CHECK (gender IS NULL OR gender IN ('male', 'female'))
);

CREATE UNIQUE INDEX uq_student_identity_version ON student_identity_version (student_id, version_no);
CREATE INDEX ix_student_identity_student ON student_identity_version (student_id, effective_from DESC);

CREATE TRIGGER trg_student_identity_immutable
    BEFORE UPDATE OR DELETE ON student_identity_version
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
