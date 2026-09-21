-- Simplification: one operator, no shifts, no third parties.
--
-- The system was built for a university with a separation-of-duties matrix and
-- a cash window per college. The university that runs it is one office with one
-- person in it, for whom every separation was a door they had to walk through
-- twice — and several of them, four-eyes above all, were doors nobody else was
-- standing behind. What this migration removes is not safety that was working;
-- it is safety that could not work with one operator and was therefore being
-- worked around.
--
-- What stays: the append-only rule, the hash-chained audit trail, the frozen
-- net, the snapshot, the lock order. Those hold with one operator and are the
-- reason a correction is still a new row rather than an edit.

-- ---------------------------------------------------------------------------
-- Views over tables that are about to go.
-- ---------------------------------------------------------------------------
DROP VIEW IF EXISTS v_sponsor_receivable;
DROP VIEW IF EXISTS v_settlement_exceptions;
DROP VIEW IF EXISTS v_unconfirmed_electronic_payments;
DROP VIEW IF EXISTS v_cashier_daily;

-- ---------------------------------------------------------------------------
-- Third parties, electronic channels, the student portal, notifications.
--
-- payment.sponsor_id goes first: it is the one reference into this group from a
-- table that stays, and dropping the parent while a child still points at it
-- fails halfway — which, in a transactional migration, rolls the whole thing
-- back and tells nobody which reference it was.
-- ---------------------------------------------------------------------------
DROP INDEX IF EXISTS ix_payment_sponsor;
ALTER TABLE payment DROP COLUMN IF EXISTS sponsor_id;

DROP TABLE IF EXISTS settlement_line;
DROP TABLE IF EXISTS settlement_batch;
DROP TABLE IF EXISTS payment_provider_event;
DROP TABLE IF EXISTS payment_intent;
DROP TABLE IF EXISTS sponsor_commitment;
DROP TABLE IF EXISTS sponsorship;
DROP TABLE IF EXISTS sponsor;
DROP TABLE IF EXISTS notification;
DROP TABLE IF EXISTS notification_template;
DROP TABLE IF EXISTS statement_verification;

-- ---------------------------------------------------------------------------
-- Shifts and the cash drawer.
--
-- Receipt series were scoped per (year, desk) so a paper receipt book could be
-- reconciled against one window. With one window the desk half of that key is
-- noise, and a receipt number that reads R-2025-2026-000008 is the shorter
-- thing to say down a telephone.
-- ---------------------------------------------------------------------------
ALTER TABLE payment DROP COLUMN IF EXISTS cashier_session_id;
ALTER TABLE refund DROP COLUMN IF EXISTS cashier_session_id;
ALTER TABLE auth_session DROP COLUMN IF EXISTS cashier_desk_id;

DROP INDEX IF EXISTS uq_number_series_scope;
ALTER TABLE number_series DROP COLUMN IF EXISTS cashier_desk_id;
CREATE UNIQUE INDEX uq_number_series_scope
    ON number_series (series_kind, academic_year_id);

DROP TABLE IF EXISTS cashier_session;
DROP TABLE IF EXISTS cashier_desk;

-- ---------------------------------------------------------------------------
-- Four eyes.
--
-- Every one of these refused an action because the same person raised it and
-- carried it out. With one account that is every action, so the constraint does
-- not slow a fraud down — it stops a refund being issued at all. The audit
-- trail still records who did it and the row is still append-only.
-- ---------------------------------------------------------------------------
ALTER TABLE refund DROP CONSTRAINT IF EXISTS ck_refund_four_eyes;
ALTER TABLE void_request DROP CONSTRAINT IF EXISTS ck_void_request_four_eyes;
ALTER TABLE discount_assignment DROP CONSTRAINT IF EXISTS ck_assignment_four_eyes;

-- ---------------------------------------------------------------------------
-- Registration date.
--
-- The office records the day the student presented themselves, which is not the
-- day the row was written: paper intake taken on Sunday is entered on Tuesday,
-- and a "registered this week" report built on created_at answers the wrong
-- question. Backfilled from created_at because that is the only answer anybody
-- can reconstruct for a student already on file.
-- ---------------------------------------------------------------------------
ALTER TABLE student ADD COLUMN IF NOT EXISTS registered_on DATE;
UPDATE student SET registered_on = created_at::date WHERE registered_on IS NULL;
ALTER TABLE student ALTER COLUMN registered_on SET DEFAULT CURRENT_DATE;
ALTER TABLE student ALTER COLUMN registered_on SET NOT NULL;
CREATE INDEX IF NOT EXISTS ix_student_registered_on ON student (registered_on);

COMMENT ON COLUMN student.registered_on IS
    'The day the office registered the student, which is not created_at: paper '
    'intake is entered days later, and a "registered this week" report built on '
    'the row timestamp answers the wrong question.';

-- ---------------------------------------------------------------------------
-- Hosting as a priced student category.
--
-- Two kinds of hosting arrive at this university and each carries its own
-- tuition: a student hosted from a different university, and a student of the
-- evening programme attending the morning one. Both are already expressible —
-- fee_policy_version resolves on student_category_id — so they are categories
-- rather than a new dimension, and the office prices them on the same screen as
-- everything else.
--
-- The existing HOSTED category stays where it is: rows already point at it, and
-- nothing financial may be rewritten to point somewhere else.
-- ---------------------------------------------------------------------------
INSERT INTO student_category (id, code, name_ar, name_en, is_active)
VALUES (gen_random_uuid(), 'HOSTED_EXTERNAL', 'استضافة من جامعة أخرى',
        'Hosted from another university', TRUE),
       (gen_random_uuid(), 'HOSTED_EVENING_TO_MORNING', 'استضافة من المسائي إلى الصباحي',
        'Hosted from evening to morning', TRUE)
ON CONFLICT (code) DO NOTHING;

-- ---------------------------------------------------------------------------
-- The cashier daily sheet, rebuilt without the shift column.
-- ---------------------------------------------------------------------------
CREATE VIEW v_cashier_daily AS
SELECT p.cashier_user_id,
       u.full_name                                                           AS cashier_name,
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
GROUP BY p.cashier_user_id, u.full_name,
         ((p.posted_at AT TIME ZONE 'UTC')::date), pm.code, pm.is_cash;

COMMENT ON VIEW v_cashier_daily IS
    'Collections per operator, day and method. Counts and money both come from '
    'payment, so the sheet and the ledger cannot disagree.';
