-- Due-date reminders, and the delivery record that makes them accountable.
--
-- The aging report tells the finance office who is late. Nothing tells the
-- student, which means the first they hear of a missed installment is a
-- registration block in September — and the university's collection rate is a
-- direct consequence of that silence.
--
-- Two rules shape the schema. A reminder is recorded before it is sent, so a
-- send that half-succeeded is visible rather than lost; and a reminder is
-- recorded per (installment, kind, window), so a scheduler that runs twice
-- because a deploy restarted it does not send the same message twice. A student
-- who receives three copies of a dunning message stops reading them, which
-- costs more than the message was worth.
--
-- Delivery itself is deliberately outside the financial core. This schema
-- records what should be sent and what happened; the channel — SMS gateway,
-- e-mail, a file for a bulk provider — is an adapter, and a university with no
-- gateway still gets a worklist it can act on by telephone.

CREATE TABLE notification_template (
    id          UUID        PRIMARY KEY,
    code        TEXT        NOT NULL,
    channel     TEXT        NOT NULL,
    -- Arabic is the body a student reads. English is optional and exists for
    -- the operator screens, not for the message.
    subject_ar  TEXT,
    body_ar     TEXT        NOT NULL,
    body_en     TEXT,
    is_active   BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_template_code CHECK (code ~ '^[A-Z0-9_]{2,48}$'),
    CONSTRAINT ck_template_channel CHECK (channel IN ('sms', 'email', 'none')),
    CONSTRAINT ck_template_body CHECK (body_ar <> '')
);

CREATE UNIQUE INDEX uq_template_code ON notification_template (code, channel);

CREATE TRIGGER trg_template_updated_at
    BEFORE UPDATE ON notification_template
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE notification_template IS
    'The wording of each reminder, editable without a deploy. Arabic is the message; the '
    'English body is for the operator screens.';

CREATE TABLE notification (
    id            UUID        PRIMARY KEY,
    -- What this message is about: upcoming_due, overdue, receipt_issued,
    -- clearance_blocked. A code rather than a foreign key to the template,
    -- because the same kind may be sent over two channels.
    kind          TEXT        NOT NULL,
    channel       TEXT        NOT NULL,

    student_id    UUID        NOT NULL REFERENCES student (id) ON DELETE RESTRICT,
    account_id    UUID        REFERENCES financial_account (id) ON DELETE RESTRICT,
    installment_id UUID       REFERENCES installment (id) ON DELETE RESTRICT,
    academic_year_id UUID     REFERENCES academic_year (id) ON DELETE RESTRICT,

    -- The address as it was when the message was queued. Kept rather than
    -- resolved at send time, so a student who changed their number after a
    -- reminder was raised still has a record of where the old one went.
    destination   TEXT,
    -- The rendered message. Frozen for the same reason a receipt is: "what did
    -- you actually tell me" has an answer.
    body          TEXT        NOT NULL,

    status        TEXT        NOT NULL DEFAULT 'pending',
    attempts      SMALLINT    NOT NULL DEFAULT 0,
    last_error    TEXT,

    -- The window this message covers, which together with kind and installment
    -- makes a duplicate detectable. A daily job that runs twice produces one
    -- reminder.
    window_key    TEXT        NOT NULL,

    scheduled_for TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at       TIMESTAMPTZ,
    failed_at     TIMESTAMPTZ,

    CONSTRAINT ck_notification_kind CHECK (
        kind IN ('upcoming_due', 'overdue', 'receipt_issued', 'clearance_blocked', 'sponsor_invoice')
    ),
    CONSTRAINT ck_notification_channel CHECK (channel IN ('sms', 'email', 'none')),
    CONSTRAINT ck_notification_status CHECK (
        status IN ('pending', 'sent', 'failed', 'skipped', 'cancelled')
    ),
    CONSTRAINT ck_notification_attempts CHECK (attempts >= 0)
);

-- The duplicate guard. One message per student, kind and window — and per
-- installment where there is one, so two installments falling due in the same
-- week each get their own reminder.
CREATE UNIQUE INDEX uq_notification_window
    ON notification (student_id, kind, window_key, coalesce(installment_id, '00000000-0000-0000-0000-000000000000'::uuid));

CREATE INDEX ix_notification_pending ON notification (scheduled_for)
    WHERE status = 'pending';
CREATE INDEX ix_notification_student ON notification (student_id, created_at DESC);
CREATE INDEX ix_notification_failed ON notification (failed_at) WHERE status = 'failed';

COMMENT ON TABLE notification IS
    'One message that should reach a student, and what happened to it. Recorded before it is '
    'sent, so a half-completed send is visible rather than lost.';

-- Reminder policy: how many days before a due date to write, and how often
-- afterwards.
--
-- Per academic year, like the debt-block and clearance policies it sits beside,
-- because a university changes its mind about how hard to chase and every past
-- year must keep showing the rule that applied to it.
ALTER TABLE academic_year
    ADD COLUMN reminder_days_before SMALLINT[] NOT NULL DEFAULT ARRAY[7, 1]::SMALLINT[],
    ADD COLUMN reminder_days_after  SMALLINT[] NOT NULL DEFAULT ARRAY[1, 7, 30]::SMALLINT[],
    ADD COLUMN reminders_enabled    BOOLEAN    NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN academic_year.reminders_enabled IS
    'Off by default. A university with no gateway configured should get the worklist without '
    'queueing messages nothing will deliver.';

-- The seed wording. Editable afterwards; here so a fresh installation can send
-- something sensible without anyone writing Arabic into a form first.
INSERT INTO notification_template (id, code, channel, body_ar) VALUES
    ('b1000000-0000-4000-8000-000000000001', 'UPCOMING_DUE', 'sms',
     'عزيزي الطالب {{student_name}}، يستحق القسط بمبلغ {{amount}} دينار بتاريخ {{due_date}}. '
     'الرصيد المتبقي {{outstanding}} دينار. {{university}}'),
    ('b1000000-0000-4000-8000-000000000002', 'OVERDUE', 'sms',
     'عزيزي الطالب {{student_name}}، تأخر قسط بمبلغ {{amount}} دينار المستحق بتاريخ {{due_date}}. '
     'يرجى المراجعة. {{university}}'),
    ('b1000000-0000-4000-8000-000000000003', 'RECEIPT_ISSUED', 'sms',
     'تم استلام مبلغ {{amount}} دينار. وصل رقم {{receipt_no}}. الرصيد المتبقي {{outstanding}} دينار. {{university}}')
ON CONFLICT (code, channel) DO NOTHING;
