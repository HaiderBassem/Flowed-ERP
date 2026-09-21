-- Reverse of 000030, as far as reversing it can honestly go.
--
-- This script restores the *shapes* the up script removed: the columns, the
-- constraints, the empty tables. It cannot restore their contents, and it says
-- so rather than pretending — a sponsor commitment or a closed shift that was
-- dropped is gone, and the only route back to it is a backup taken before the
-- up script ran. A down script that silently recreated empty tables and let the
-- operator believe the data was back would be worse than one that refuses.
--
-- The down path exists so the migration runner can step backwards in
-- development and so `make migrate-down` is not a lie. It is not a recovery
-- plan; `make restore-drill` is.

DROP VIEW IF EXISTS v_cashier_daily;

-- ---------------------------------------------------------------------------
-- Hosting categories, removed only if nothing points at them. An enrollment
-- priced under one of these must keep resolving to it.
-- ---------------------------------------------------------------------------
DELETE
FROM student_category c
WHERE c.code IN ('HOSTED_EXTERNAL', 'HOSTED_EVENING_TO_MORNING')
  AND NOT EXISTS (SELECT 1 FROM enrollment e WHERE e.student_category_id = c.id)
  AND NOT EXISTS (SELECT 1 FROM fee_policy_version f WHERE f.student_category_id = c.id);

DROP INDEX IF EXISTS ix_student_registered_on;
ALTER TABLE student DROP COLUMN IF EXISTS registered_on;

-- ---------------------------------------------------------------------------
-- Four eyes, restored. Any row already violating them is left alone by NOT
-- VALID: the constraint binds new writes, and refusing to migrate because of a
-- refund somebody issued alone last week would leave the schema half-applied.
-- ---------------------------------------------------------------------------
ALTER TABLE refund
    ADD CONSTRAINT ck_refund_four_eyes
        CHECK (approved_by IS NULL OR approved_by <> requested_by) NOT VALID;
ALTER TABLE void_request
    ADD CONSTRAINT ck_void_request_four_eyes
        CHECK (executed_by IS NULL OR executed_by <> requested_by) NOT VALID;
ALTER TABLE discount_assignment
    ADD CONSTRAINT ck_assignment_four_eyes
        CHECK (approved_by IS NULL OR requested_by IS NULL OR approved_by <> requested_by) NOT VALID;

-- ---------------------------------------------------------------------------
-- The cash window, as an empty shape.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS cashier_desk
(
    id         UUID PRIMARY KEY,
    code       TEXT        NOT NULL UNIQUE,
    name_ar    TEXT        NOT NULL,
    college_id UUID REFERENCES college (id) ON DELETE RESTRICT,
    is_active  BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS cashier_session
(
    id               UUID PRIMARY KEY,
    cashier_user_id  UUID        NOT NULL REFERENCES app_user (id) ON DELETE RESTRICT,
    cashier_desk_id  UUID        NOT NULL REFERENCES cashier_desk (id) ON DELETE RESTRICT,
    academic_year_id UUID        NOT NULL REFERENCES academic_year (id) ON DELETE RESTRICT,
    opened_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    opening_float    BIGINT      NOT NULL DEFAULT 0,
    closed_at        TIMESTAMPTZ,
    expected_cash    BIGINT,
    counted_cash     BIGINT,
    variance         BIGINT,
    variance_reason  TEXT,
    status           TEXT        NOT NULL DEFAULT 'open',
    approved_by      UUID REFERENCES app_user (id),
    approved_at      TIMESTAMPTZ,
    notes            TEXT,
    CONSTRAINT ck_session_status CHECK (status IN ('open', 'closed', 'approved')),
    CONSTRAINT ck_session_variance_reason CHECK (
        variance IS NULL OR variance = 0 OR variance_reason IS NOT NULL),
    CONSTRAINT ck_session_approval_four_eyes CHECK (
        approved_by IS NULL OR approved_by <> cashier_user_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_cashier_session_open
    ON cashier_session (cashier_user_id)
    WHERE status = 'open';

ALTER TABLE payment ADD COLUMN IF NOT EXISTS cashier_session_id UUID REFERENCES cashier_session (id);
ALTER TABLE refund ADD COLUMN IF NOT EXISTS cashier_session_id UUID REFERENCES cashier_session (id);
ALTER TABLE auth_session ADD COLUMN IF NOT EXISTS cashier_desk_id UUID REFERENCES cashier_desk (id);

DROP INDEX IF EXISTS uq_number_series_scope;
ALTER TABLE number_series
    ADD COLUMN IF NOT EXISTS cashier_desk_id UUID REFERENCES cashier_desk (id) ON DELETE RESTRICT;
CREATE UNIQUE INDEX uq_number_series_scope
    ON number_series (series_kind, academic_year_id, cashier_desk_id) NULLS NOT DISTINCT;

CREATE VIEW v_cashier_daily AS
SELECT p.cashier_user_id,
       u.full_name                                                           AS cashier_name,
       p.cashier_session_id,
       (p.posted_at AT TIME ZONE 'UTC')::date                                AS posting_date,
       pm.code                                                               AS method_code,
       pm.is_cash,
       count(*) FILTER (WHERE p.status = 'posted')                           AS payment_count,
       COALESCE(sum(p.amount) FILTER (WHERE p.status = 'posted'), 0)::bigint AS payment_total,
       count(*) FILTER (WHERE p.status = 'voided')                           AS void_count,
       COALESCE(sum(p.amount) FILTER (WHERE p.status = 'voided'), 0)::bigint AS void_total
FROM payment p
         JOIN app_user u ON u.id = p.cashier_user_id
         JOIN payment_method pm ON pm.id = p.payment_method_id
WHERE p.posted_at IS NOT NULL
GROUP BY p.cashier_user_id, u.full_name, p.cashier_session_id,
         ((p.posted_at AT TIME ZONE 'UTC')::date), pm.code, pm.is_cash;

-- Sponsors, settlements, electronic channels, notifications and the portal's
-- verification table are NOT recreated. Their schemas ran to hundreds of lines
-- across four migrations, and an empty copy would restore neither the data nor
-- the triggers and views built on top of it. Restore a backup instead.
