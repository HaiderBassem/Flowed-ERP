-- Reverse of 000033.
--
-- The institution's details go back to being environment variables, and
-- whatever the office typed into the settings screen is lost. Worth saying out
-- loud: RECEIPT_UNIVERSITY_NAME has to be set on the server again before the
-- next receipt prints, or it prints the default.

DROP TRIGGER IF EXISTS trg_app_setting_updated_at ON app_setting;
DROP TABLE IF EXISTS app_setting;
