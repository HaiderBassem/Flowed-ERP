-- Student access: a person may see their own fees.
--
-- Every balance enquiry currently walks to the finance window, because the only
-- way to learn what a student owes is for an operator to look it up. That is a
-- queue the university pays for in staff time and the student pays for in a
-- morning, and it is the single most common interaction the system has.
--
-- The design of it is narrow on purpose. A student credential is not a small
-- operator account: it authenticates a person to see one student's own record
-- and to begin a payment against it, and nothing else. The link between the
-- credential and the person is a column here rather than a convention, so every
-- ownership check in the application has one thing to compare against.

ALTER TABLE app_user
    -- Set only on a student credential. NULL on every operator account, which
    -- is what the check below enforces both ways: an operator with a student
    -- link, or a student credential without one, would be an actor whose
    -- authority nobody could reason about.
    ADD COLUMN student_id UUID REFERENCES student (id) ON DELETE RESTRICT;

-- One credential per student. A second would split a person's sign-in history
-- and leave two passwords able to see one record.
CREATE UNIQUE INDEX uq_app_user_student ON app_user (student_id)
    WHERE student_id IS NOT NULL;

COMMENT ON COLUMN app_user.student_id IS
    'The student this credential belongs to. NULL for an operator account. Every route a '
    'student can reach compares the row it is about against this.';

-- ---------------------------------------------------------------------------
-- Verifiable statements
-- ---------------------------------------------------------------------------

-- A printed statement a third party can check without being given access.
--
-- The case this exists for: a student takes a statement to a sponsor, a
-- ministry office or a bank, and that office has to decide whether the paper in
-- front of them is real. The alternative in practice is a stamp, and a stamp is
-- copied. A verification code resolves to the figures as they were when the
-- statement printed — not to the current balance, which will have moved — so
-- the paper and the check agree.
CREATE TABLE statement_verification (
    id             UUID        PRIMARY KEY,
    -- Short, unambiguous when read aloud or typed off a printed page. Not
    -- guessable: it is drawn from the same entropy as a session identifier.
    code           TEXT        NOT NULL,

    student_id     UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    academic_year_id UUID      REFERENCES academic_year (id) ON DELETE RESTRICT,

    -- The figures as printed, frozen. A verification that recomputed would
    -- disagree with the paper the moment the student paid anything, and the
    -- office holding it would conclude the paper was forged.
    total_charged  BIGINT      NOT NULL,
    total_paid     BIGINT      NOT NULL,
    outstanding    BIGINT      NOT NULL,
    -- A digest over the rendered statement, so a document altered after
    -- printing fails verification even if its figures are copied correctly.
    content_sha256 TEXT        NOT NULL,

    issued_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    issued_by      UUID        REFERENCES app_user (id),
    -- Statements go stale. A verification that never expired would let a
    -- student present a two-year-old clearance as current.
    expires_at     TIMESTAMPTZ NOT NULL,
    revoked_at     TIMESTAMPTZ,
    revoked_reason TEXT,

    CONSTRAINT ck_statement_verification_code CHECK (length(code) >= 8),
    CONSTRAINT ck_statement_verification_window CHECK (expires_at > issued_at)
);

CREATE UNIQUE INDEX uq_statement_verification_code ON statement_verification (code);
CREATE INDEX ix_statement_verification_student ON statement_verification (student_id, issued_at DESC);

-- Append-only but for revocation: the figures a statement was issued with
-- cannot be edited afterwards, which is the entire value of being able to
-- verify it.
CREATE TRIGGER trg_statement_verification_immutable
    BEFORE UPDATE OR DELETE ON statement_verification
    FOR EACH ROW EXECUTE FUNCTION forbid_column_mutation('revoked_at', 'revoked_reason');

COMMENT ON TABLE statement_verification IS
    'A printed statement a third party can check by its code. The figures are frozen at '
    'printing: a verification that recomputed would disagree with the paper.';
