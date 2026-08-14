-- Rolling back returns every user to university-wide authority and removes the
-- ability to revoke a session. Both are losses of control rather than losses of
-- data: no financial row references any of this, and the login-attempt history
-- is forensic rather than accounting. A deployment that rolls back past this
-- point should expect every issued refresh token to work again until it
-- expires, because nothing will be left to check it against.

DROP TRIGGER IF EXISTS trg_login_attempt_no_update ON auth_login_attempt;
DROP TABLE IF EXISTS auth_login_attempt;
DROP TABLE IF EXISTS auth_session;
DROP TABLE IF EXISTS app_user_scope;

ALTER TABLE app_user
    DROP CONSTRAINT IF EXISTS ck_app_user_scope_mode,
    DROP CONSTRAINT IF EXISTS ck_app_user_failed_login_count,
    DROP COLUMN IF EXISTS must_change_password,
    DROP COLUMN IF EXISTS password_changed_at,
    DROP COLUMN IF EXISTS failed_login_count,
    DROP COLUMN IF EXISTS last_failed_login_at,
    DROP COLUMN IF EXISTS locked_until,
    DROP COLUMN IF EXISTS disabled_at,
    DROP COLUMN IF EXISTS disabled_by,
    DROP COLUMN IF EXISTS disabled_reason,
    DROP COLUMN IF EXISTS scope_mode;
