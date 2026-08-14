-- Operator administration, session revocation and organisational scope.
--
-- Three gaps closed together because they are one subject: who may act, on
-- whose data, and how an authority is taken away again.
--
--  1. Users could only be created by a shell command. Disabling a leaver,
--     resetting a forgotten password or narrowing somebody's roles all
--     required a database session, so in practice they were done by whoever
--     held the shell — the opposite of the separation of duties the rest of
--     this system enforces.
--
--  2. Roles travel inside the access token, so revoking them lagged by a full
--     token lifetime and a stolen refresh token stayed valid for weeks. There
--     was no logout at all: signing out cleared the browser and left the token
--     working. Sessions are now rows, and a row can be revoked.
--
--  3. Authority was university-wide. A finance manager appointed for the
--     college of engineering could void a receipt in medicine, and every
--     report showed every college. Scope is now a property of the user.
--
-- Existing users keep university-wide authority (scope_mode 'university'), so
-- this migration changes nobody's access on the day it is applied. Narrowing
-- somebody is a deliberate administrative act afterwards, which is the only
-- safe direction for a change like this to travel.

-- ---------------------------------------------------------------------------
-- Password lifecycle and lockout
-- ---------------------------------------------------------------------------

ALTER TABLE app_user
    -- Set when an administrator resets a password. The temporary credential
    -- authenticates and nothing else: every other route refuses until the
    -- holder chooses their own. Without this, a reset password stays in the
    -- administrator's chat history and works forever.
    ADD COLUMN must_change_password BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN password_changed_at  TIMESTAMPTZ,

    -- Per-account throttling. The IP limiter cannot do this job: it is keyed
    -- by client address, and a university address is a campus NAT shared by a
    -- hall of terminals, so a per-IP budget large enough for the hall is large
    -- enough to guess one cashier's password all afternoon.
    ADD COLUMN failed_login_count   INTEGER     NOT NULL DEFAULT 0,
    ADD COLUMN last_failed_login_at TIMESTAMPTZ,
    ADD COLUMN locked_until         TIMESTAMPTZ,

    -- Disablement is recorded rather than inferred from is_active, because
    -- "who switched this account off, when, and why" is an audit question that
    -- gets asked about the account of somebody who has left.
    ADD COLUMN disabled_at          TIMESTAMPTZ,
    ADD COLUMN disabled_by          UUID REFERENCES app_user (id),
    ADD COLUMN disabled_reason      TEXT,

    -- Organisational scope. 'university' is every college, which is what every
    -- existing user has today; 'scoped' means the grants below are exhaustive.
    -- A scoped user with no grants can reach nothing, which is the correct
    -- direction to fail.
    ADD COLUMN scope_mode           TEXT        NOT NULL DEFAULT 'university',

    ADD CONSTRAINT ck_app_user_scope_mode
        CHECK (scope_mode IN ('university', 'scoped')),
    ADD CONSTRAINT ck_app_user_failed_login_count
        CHECK (failed_login_count >= 0);

COMMENT ON COLUMN app_user.must_change_password IS
    'A reset credential authenticates and nothing else until the holder sets their own.';
COMMENT ON COLUMN app_user.locked_until IS
    'Per-account lockout after repeated failures. Distinct from the IP limiter, which '
    'sees a campus NAT rather than a person.';
COMMENT ON COLUMN app_user.scope_mode IS
    'university = authority over every college (the pre-existing behaviour); '
    'scoped = authority limited to the rows in app_user_scope.';

-- ---------------------------------------------------------------------------
-- Organisational scope grants
-- ---------------------------------------------------------------------------

CREATE TABLE app_user_scope (
    id            UUID        PRIMARY KEY,
    user_id       UUID        NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,

    -- Exactly one of these is set. A college grant covers every department in
    -- it, so a college dean's finance officer does not need a row per
    -- department that opens next year.
    college_id    UUID        REFERENCES college (id) ON DELETE RESTRICT,
    department_id UUID        REFERENCES department (id) ON DELETE RESTRICT,

    granted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    granted_by    UUID        REFERENCES app_user (id),

    CONSTRAINT ck_user_scope_exactly_one CHECK (
        (college_id IS NOT NULL AND department_id IS NULL) OR
        (college_id IS NULL AND department_id IS NOT NULL)
    )
);

-- One grant per target. Re-granting is idempotent rather than an accumulation
-- of duplicate rows nobody can read.
CREATE UNIQUE INDEX uq_user_scope_college ON app_user_scope (user_id, college_id)
    WHERE college_id IS NOT NULL;
CREATE UNIQUE INDEX uq_user_scope_department ON app_user_scope (user_id, department_id)
    WHERE department_id IS NOT NULL;
CREATE INDEX ix_user_scope_user ON app_user_scope (user_id);

COMMENT ON TABLE app_user_scope IS
    'Which colleges and departments a scoped user may act on. Read on every request '
    'and carried in the access token, so a narrowed scope takes effect within one '
    'access-token lifetime and immediately on refresh.';

-- ---------------------------------------------------------------------------
-- Sessions, so that a credential can be withdrawn
-- ---------------------------------------------------------------------------

CREATE TABLE auth_session (
    -- The token's jti. Both tokens of a pair carry it, so revoking the session
    -- kills the refresh token and every access token minted from it.
    id             UUID        PRIMARY KEY,
    user_id        UUID        NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,

    issued_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The refresh token's expiry: the outer bound of the session's life.
    expires_at     TIMESTAMPTZ NOT NULL,
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    revoked_at     TIMESTAMPTZ,
    revoked_by     UUID        REFERENCES app_user (id),
    revoked_reason TEXT,

    -- Recorded so an operator listing their sessions can recognise which is
    -- which, and so an investigation can tell a desk from a laptop. Not
    -- student data, and never a metric label.
    ip_address     TEXT,
    user_agent     TEXT,
    -- A cashier signs in at a desk; the session records which, because receipt
    -- series run per desk and a session moved to another desk is a new shift.
    cashier_desk_id UUID       REFERENCES cashier_desk (id),

    CONSTRAINT ck_auth_session_expiry CHECK (expires_at > issued_at)
);

CREATE INDEX ix_auth_session_user ON auth_session (user_id, issued_at DESC);
-- The revocation check runs on every refresh and on every access-token
-- validation that the configuration asks to be strict about, so it must be an
-- index-only hit on the primary key plus this partial index for the sweeper.
CREATE INDEX ix_auth_session_live ON auth_session (expires_at)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE auth_session IS
    'One row per sign-in. Revoking the row is what makes logout, "sign out my other '
    'sessions", and disabling a user take effect before the access token expires.';

-- ---------------------------------------------------------------------------
-- Login attempts, for throttling and for the security trail
-- ---------------------------------------------------------------------------

CREATE TABLE auth_login_attempt (
    id           UUID        PRIMARY KEY,
    -- Text rather than a foreign key: the interesting attempts are the ones
    -- against usernames that do not exist.
    username     TEXT        NOT NULL,
    user_id      UUID        REFERENCES app_user (id) ON DELETE SET NULL,
    succeeded    BOOLEAN     NOT NULL,
    -- A short machine reason: unknown_user, bad_password, locked, disabled.
    -- Never the password, never a hint about which half was wrong.
    failure_code TEXT,
    ip_address   TEXT,
    user_agent   TEXT,
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ix_login_attempt_username ON auth_login_attempt (username, occurred_at DESC);
CREATE INDEX ix_login_attempt_time ON auth_login_attempt (occurred_at);

COMMENT ON TABLE auth_login_attempt IS
    'Every sign-in attempt, successful or not. Feeds the lockout decision and answers '
    '"was this account being guessed before the fraud" during an investigation. '
    'Purged on a retention schedule by the scheduler; it is not the audit log.';

-- Attempts cannot be edited, for the same reason payments cannot: the value of
-- the record is that whoever appears in it cannot tidy it up afterwards.
-- DELETE stays permitted, because retention deletes whole old windows on a
-- schedule and that is a different act from editing one row.
CREATE TRIGGER trg_login_attempt_no_update
    BEFORE UPDATE ON auth_login_attempt
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
