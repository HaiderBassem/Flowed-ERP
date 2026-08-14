-- Rolling back removes reminders and their delivery history. Nothing financial
-- depends on them: a notification records what was said to a student, never
-- what they owe.

ALTER TABLE academic_year
    DROP COLUMN IF EXISTS reminder_days_before,
    DROP COLUMN IF EXISTS reminder_days_after,
    DROP COLUMN IF EXISTS reminders_enabled;

DROP TABLE IF EXISTS notification;

DROP TRIGGER IF EXISTS trg_template_updated_at ON notification_template;
DROP TABLE IF EXISTS notification_template;
