-- The academic context: years, enrollments, and hosting.
--
-- An enrollment is the unit this whole system turns on. It is the student's
-- context for exactly one academic year, and it is what carries a financial
-- account. Nothing here is ever overwritten to record a change: a student who
-- moves department in March gets a second enrollment row and the first is
-- marked superseded, so the question "what was this student registered as on
-- the day that receipt was printed" always has an answer.

-- ---------------------------------------------------------------------------
-- Academic year
-- ---------------------------------------------------------------------------

CREATE TABLE academic_year (
    id                      UUID        PRIMARY KEY,
    code                    TEXT        NOT NULL,
    start_date              DATE        NOT NULL,
    end_date                DATE        NOT NULL,

    -- Four states, not two. A single "closed" flag cannot express the Iraqi
    -- calendar: second-round (دور ثاني) results arrive in September and
    -- October, weeks after the treasury wants its books shut. So the financial
    -- close and the academic close are separate events, and between them the
    -- year accepts result entries while refusing every peso of movement.
    --
    --   draft             -> being set up, nothing may reference it
    --   open              -> normal operation
    --   financially_closed-> money frozen, academic records still writable
    --   closed            -> everything frozen
    --   adjustment_open   -> time-boxed reopening for an audited correction
    status                  TEXT        NOT NULL DEFAULT 'draft',

    registration_deadline   DATE,
    -- Whether unsettled debt from an earlier year blocks registration in this
    -- one. A policy per year, not a constant: universities change this by
    -- circular, sometimes mid-decade.
    debt_block_policy       TEXT        NOT NULL DEFAULT 'warn',

    financially_closed_at   TIMESTAMPTZ,
    financially_closed_by   UUID        REFERENCES app_user (id),
    closed_at               TIMESTAMPTZ,
    closed_by               UUID        REFERENCES app_user (id),
    -- When status is adjustment_open, the deadline after which it reverts.
    adjustment_window_ends  TIMESTAMPTZ,
    adjustment_reason       TEXT,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_academic_year_code CHECK (code ~ '^\d{4}-\d{4}$'),
    CONSTRAINT ck_academic_year_dates CHECK (end_date > start_date),
    CONSTRAINT ck_academic_year_status CHECK (status IN (
        'draft', 'open', 'financially_closed', 'closed', 'adjustment_open'
    )),
    CONSTRAINT ck_academic_year_debt_policy CHECK (debt_block_policy IN ('ignore', 'warn', 'block')),
    CONSTRAINT ck_academic_year_fin_close_stamp CHECK (
        (status IN ('financially_closed', 'closed', 'adjustment_open'))
            = (financially_closed_at IS NOT NULL)
    ),
    CONSTRAINT ck_academic_year_close_stamp CHECK (
        (status = 'closed') <= (closed_at IS NOT NULL)
    ),
    CONSTRAINT ck_academic_year_adjustment_window CHECK (
        status <> 'adjustment_open'
        OR (adjustment_window_ends IS NOT NULL AND adjustment_reason IS NOT NULL)
    )
);

CREATE UNIQUE INDEX uq_academic_year_code ON academic_year (code);
CREATE INDEX ix_academic_year_status ON academic_year (status);
-- Two years are legitimately open at once every autumn, while second-round
-- results close one year and registration opens the next. Nothing constrains
-- the count of open years; the constraint that matters is one live enrollment
-- per student per year, enforced below.
CREATE INDEX ix_academic_year_dates ON academic_year (start_date, end_date);

CREATE TRIGGER trg_academic_year_updated_at
    BEFORE UPDATE ON academic_year
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Enrollment
-- ---------------------------------------------------------------------------

CREATE TABLE enrollment (
    id                      UUID        PRIMARY KEY,
    student_id              UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    academic_year_id        UUID        NOT NULL REFERENCES academic_year (id) ON DELETE RESTRICT,

    -- Position in the supersede chain within this year. The first registration
    -- is 1; a mid-year department change creates sequence 2 and marks 1
    -- superseded.
    sequence_no             SMALLINT    NOT NULL DEFAULT 1,

    college_id              UUID        NOT NULL REFERENCES college (id) ON DELETE RESTRICT,
    department_id           UUID        NOT NULL REFERENCES department (id) ON DELETE RESTRICT,
    study_type_id           UUID        NOT NULL REFERENCES study_type (id) ON DELETE RESTRICT,
    student_category_id     UUID        NOT NULL REFERENCES student_category (id) ON DELETE RESTRICT,
    stage                   SMALLINT    NOT NULL,

    -- How many counted attempts the student has made at this stage in this
    -- department. Derived from history at creation and validated against it,
    -- never free-typed: it drives the repeat-fee policy row.
    attempt_number          SMALLINT    NOT NULL DEFAULT 1,

    enrollment_kind         TEXT        NOT NULL DEFAULT 'regular',

    -- Three independent dimensions, never merged. Academic result answers "did
    -- they pass"; enrollment status answers "are they still with us"; the
    -- financial account, in a later migration, answers "do they owe us
    -- anything". Collapsing any two of these is how systems end up unable to
    -- describe a student who failed but is still enrolled and still owes money.
    enrollment_status       TEXT        NOT NULL DEFAULT 'draft',
    academic_result         TEXT        NOT NULL DEFAULT 'pending',
    -- ناجح بقرار: passed by committee decision rather than on marks.
    result_by_decision      BOOLEAN     NOT NULL DEFAULT false,
    result_recorded_at      TIMESTAMPTZ,
    result_recorded_by      UUID        REFERENCES app_user (id),

    -- Lineage. previous_enrollment_id links across years, which is what makes
    -- "return after dropout" and attempt validation answerable; supersedes_id
    -- links within a year.
    --
    -- The within-year link points backwards only, from successor to
    -- predecessor. A forward superseded_by_id column would deadlock the
    -- operation it exists to record: marking the old row superseded would
    -- require the replacement to exist already, while inserting the
    -- replacement would require the old row to be superseded first, because of
    -- the one-live-enrollment index below. Nothing could be written in either
    -- order. Pointing backwards removes the cycle — the successor is inserted
    -- knowing its predecessor — and the unique index on supersedes_id keeps
    -- the chain from branching just as effectively.
    previous_enrollment_id  UUID        REFERENCES enrollment (id),
    supersedes_id           UUID        REFERENCES enrollment (id),
    supersede_reason        TEXT,
    supersede_date          DATE,

    deferral_order_ref      TEXT,
    transfer_order_ref      TEXT,
    return_order_ref        TEXT,
    notes                   TEXT,

    registered_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    registered_by           UUID        REFERENCES app_user (id),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_enrollment_sequence CHECK (sequence_no >= 1),
    CONSTRAINT ck_enrollment_stage CHECK (stage BETWEEN 1 AND 8),
    CONSTRAINT ck_enrollment_attempt CHECK (attempt_number >= 1),
    CONSTRAINT ck_enrollment_kind CHECK (enrollment_kind IN ('regular', 'hosted_in', 'transfer_in')),
    CONSTRAINT ck_enrollment_status CHECK (enrollment_status IN (
        'draft', 'active', 'deferred', 'superseded',
        'transferred_out', 'withdrawn', 'dropped_out', 'completed'
    )),
    CONSTRAINT ck_enrollment_result CHECK (academic_result IN (
        'pending', 'passed_r1', 'passed_r2', 'failed', 'no_result', 'not_applicable'
    )),

    -- The legality matrix from the domain design, enforced by the database
    -- rather than by convention. Every combination not listed is rejected at
    -- write time, so no code path can leave an enrollment in a state the
    -- domain has no meaning for — "superseded but passed", say, where the real
    -- result belongs to the successor row.
    CONSTRAINT ck_enrollment_status_result_matrix CHECK (
        CASE enrollment_status
            WHEN 'draft'            THEN academic_result = 'pending'
            WHEN 'active'           THEN academic_result IN ('pending', 'passed_r1', 'passed_r2', 'failed')
            WHEN 'deferred'         THEN academic_result = 'not_applicable'
            WHEN 'superseded'       THEN academic_result = 'not_applicable'
            WHEN 'transferred_out'  THEN academic_result IN ('no_result', 'not_applicable')
            WHEN 'withdrawn'        THEN academic_result = 'no_result'
            WHEN 'dropped_out'      THEN academic_result IN ('no_result', 'failed')
            WHEN 'completed'        THEN academic_result IN ('passed_r1', 'passed_r2')
        END
    ),
    -- A superseded row must say why and when. Whether a successor actually
    -- exists is a cross-row question, checked by the deferred constraint
    -- trigger below rather than here.
    CONSTRAINT ck_enrollment_supersede_reason CHECK (
        (enrollment_status = 'superseded')
            <= (supersede_reason IS NOT NULL AND supersede_date IS NOT NULL)
    ),
    CONSTRAINT ck_enrollment_no_self_reference CHECK (
        (supersedes_id IS NULL OR supersedes_id <> id)
        AND (previous_enrollment_id IS NULL OR previous_enrollment_id <> id)
    ),
    -- A real examination outcome records when it was entered. The three
    -- non-outcomes do not: 'no_result' means the student never sat the exams,
    -- so demanding a timestamp for when that result was recorded asks for a
    -- date that does not exist.
    CONSTRAINT ck_enrollment_result_stamp CHECK (
        academic_result IN ('pending', 'not_applicable', 'no_result')
        OR result_recorded_at IS NOT NULL
    )
);

CREATE UNIQUE INDEX uq_enrollment_sequence
    ON enrollment (student_id, academic_year_id, sequence_no);

-- The invariant that prevents two live financial contexts for one student in
-- one year. Draft, active and deferred enrollments all occupy the slot; the
-- terminal states do not, which is exactly what lets a superseded row and its
-- replacement coexist.
--
-- A partial unique index expresses this directly. On a database without them,
-- the same rule needs a nullable flag column and a convention nobody can see
-- from the schema.
CREATE UNIQUE INDEX uq_enrollment_one_live_per_year
    ON enrollment (student_id, academic_year_id)
    WHERE enrollment_status IN ('draft', 'active', 'deferred');

-- The supersede chain is a chain, not a tree: two enrollments may not claim
-- the same predecessor.
CREATE UNIQUE INDEX uq_enrollment_supersedes
    ON enrollment (supersedes_id)
    WHERE supersedes_id IS NOT NULL;

CREATE INDEX ix_enrollment_student ON enrollment (student_id, academic_year_id);
CREATE INDEX ix_enrollment_year_dept_stage
    ON enrollment (academic_year_id, department_id, stage, study_type_id);
CREATE INDEX ix_enrollment_year_study_type ON enrollment (academic_year_id, study_type_id);
CREATE INDEX ix_enrollment_year_college ON enrollment (academic_year_id, college_id);
CREATE INDEX ix_enrollment_status ON enrollment (enrollment_status);
CREATE INDEX ix_enrollment_result ON enrollment (academic_year_id, academic_result);
CREATE INDEX ix_enrollment_previous ON enrollment (previous_enrollment_id)
    WHERE previous_enrollment_id IS NOT NULL;

CREATE TRIGGER trg_enrollment_updated_at
    BEFORE UPDATE ON enrollment
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- A superseded enrollment must have exactly one successor by the end of the
-- transaction that superseded it.
--
-- The check is deferred because the supersede operation is necessarily two
-- statements: free the year's slot by marking the old row superseded, then
-- insert the replacement pointing back at it. Between those statements the
-- invariant is legitimately false. Deferring to commit lets the pair be
-- written in the only order the indexes allow while still refusing to store a
-- student whose year was retired with nothing put in its place — which would
-- silently erase a registration and, with it, an account somebody may already
-- have paid into.
CREATE OR REPLACE FUNCTION check_supersede_has_successor()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    successor_count INTEGER;
    target_id       UUID;
BEGIN
    target_id := CASE TG_OP WHEN 'DELETE' THEN OLD.id ELSE NEW.id END;

    IF TG_OP <> 'DELETE' AND NEW.enrollment_status <> 'superseded' THEN
        RETURN NULL;
    END IF;

    -- The row may have been removed or moved on since the statement ran; only
    -- a row still marked superseded needs a successor.
    IF NOT EXISTS (
        SELECT 1 FROM enrollment
        WHERE id = target_id AND enrollment_status = 'superseded'
    ) THEN
        RETURN NULL;
    END IF;

    SELECT count(*) INTO successor_count
    FROM enrollment
    WHERE supersedes_id = target_id;

    IF successor_count = 0 THEN
        RAISE EXCEPTION
            'enrollment % is marked superseded but no replacement enrollment references it', target_id
            USING ERRCODE = 'P0001',
                  HINT = 'Superseding must create the replacement in the same transaction. '
                         'To end a registration without a replacement, use withdrawn, '
                         'transferred_out or dropped_out instead.';
    END IF;

    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_enrollment_supersede_successor
    AFTER INSERT OR UPDATE ON enrollment
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_supersede_has_successor();

-- ---------------------------------------------------------------------------
-- Hosting overlay (استضافة)
-- ---------------------------------------------------------------------------

-- Hosting is an attribute of an enrollment, not a study type.
--
-- A hosted student has two study types at once: the one at their home
-- institution and the one they physically attend. A single "hosting" value in
-- the study_type table would erase that pair, and because fee policy resolves
-- on study type it would force a fabricated policy row for every direction.
-- Worse, it could not represent an outgoing host at all: our student attending
-- elsewhere keeps their ordinary enrollment with us, because their results
-- still come back to us.
--
-- The direction distinguishes the two cases:
--   incoming -> their student sits with us; enrollment.study_type_id is what
--               they attend here, and the home columns record where they came
--               from.
--   outgoing -> our student sits elsewhere; the base enrollment stays active
--               and unchanged, and the host columns record where they went.
CREATE TABLE hosting_record (
    id                      UUID        PRIMARY KEY,
    enrollment_id           UUID        NOT NULL REFERENCES enrollment (id) ON DELETE RESTRICT,
    direction               TEXT        NOT NULL,

    home_university         TEXT,
    home_college            TEXT,
    home_department         TEXT,
    home_study_type_id      UUID        REFERENCES study_type (id),

    host_university         TEXT,
    host_college            TEXT,
    host_department         TEXT,
    host_study_type_id      UUID        REFERENCES study_type (id),

    -- Which institution collects tuition for this student. Iraqi practice
    -- varies by agreement, so it is configured per hosting record rather than
    -- assumed. When the home institution collects, we generate no financial
    -- account for an incoming hosted student.
    fee_collector           TEXT        NOT NULL DEFAULT 'home',

    period_from             DATE,
    period_to               DATE,
    agreement_ref           TEXT,
    notes                   TEXT,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by              UUID        REFERENCES app_user (id),

    CONSTRAINT ck_hosting_direction CHECK (direction IN ('incoming', 'outgoing')),
    CONSTRAINT ck_hosting_fee_collector CHECK (fee_collector IN ('home', 'host', 'split')),
    CONSTRAINT ck_hosting_period CHECK (period_to IS NULL OR period_from IS NULL OR period_to >= period_from),
    CONSTRAINT ck_hosting_incoming_home CHECK (
        direction <> 'incoming' OR home_university IS NOT NULL
    ),
    CONSTRAINT ck_hosting_outgoing_host CHECK (
        direction <> 'outgoing' OR host_university IS NOT NULL
    )
);

CREATE UNIQUE INDEX uq_hosting_enrollment ON hosting_record (enrollment_id);
CREATE INDEX ix_hosting_direction ON hosting_record (direction);

CREATE TRIGGER trg_hosting_updated_at
    BEFORE UPDATE ON hosting_record
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
