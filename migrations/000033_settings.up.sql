-- Institution details, editable by the office rather than by a deployment.
--
-- The university's name, its college, its address and its logo were environment
-- variables read once at start-up. That is the right home for a database
-- password and the wrong one for a name that appears on every receipt a student
-- carries out of the building: changing it meant editing a file on the server
-- and restarting, which in practice means it never gets changed, and every
-- receipt for three years says "الجامعة".
--
-- A key/value table rather than one row with a column per setting. Settings
-- arrive one at a time, and a column per setting is a migration per setting.
-- The cost is that every value is text, which is acceptable because every value
-- here *is* text: this table holds no money and no identifier the rest of the
-- schema depends on.

CREATE TABLE app_setting
(
    key        TEXT PRIMARY KEY,
    value      TEXT        NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID REFERENCES app_user (id),
    CONSTRAINT ck_app_setting_key CHECK (key ~ '^[a-z][a-z0-9_]{1,63}$')
);

COMMENT ON TABLE app_setting IS
    'Institution details shown on receipts and reports. Editable from the '
    'settings screen: a name that needs a server restart to change is a name '
    'that never gets changed.';

-- Seeded with every key the receipt renderer asks for, so a fresh installation
-- has a complete row set and the renderer never has to tell "not set" apart
-- from "set to empty". Empty is a real answer — a university with no second
-- line on its receipts leaves the college blank.
INSERT INTO app_setting (key, value)
VALUES ('university_name_ar', 'الجامعة'),
       ('college_name_ar', ''),
       ('address', ''),
       ('phone', ''),
       ('logo_data_uri', ''),
       ('receipt_footer_ar', ''),
       ('currency_name_ar', 'دينار عراقي')
ON CONFLICT (key) DO NOTHING;

CREATE TRIGGER trg_app_setting_updated_at
    BEFORE UPDATE
    ON app_setting
    FOR EACH ROW
EXECUTE FUNCTION set_updated_at();
