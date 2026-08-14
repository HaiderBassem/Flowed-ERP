-- Foundation: extensions, shared domains, and the helper functions the rest of
-- the schema is built on.
--
-- Two conventions established here and followed everywhere after:
--
--   Money is BIGINT holding whole Iraqi Dinars. Never NUMERIC, never DOUBLE
--   PRECISION. IQD has no circulating subunit, so a dinar is the atom, and an
--   integer column makes it impossible for a float to enter the system through
--   a column default or a careless cast. BIGINT rather than INTEGER because a
--   college-year collection total passes 2.1 billion dinars routinely.
--
--   State columns are TEXT with a named CHECK constraint rather than native
--   ENUM types. Adding a study type or a new adjustment reason then costs one
--   ordinary migration instead of an ALTER TYPE that cannot be rolled back,
--   and the constraint name gives the application a precise hook for
--   translating the violation into a domain error.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS btree_gin;

-- ---------------------------------------------------------------------------
-- Arabic text normalisation
-- ---------------------------------------------------------------------------

-- Folds the orthographic variation that makes Arabic name search fail.
--
-- A clerk who types "علي" must find a student registered as "علي" with a
-- hamza, a madda, or a tatweel stretching the letters, and "فاطمه" must find
-- "فاطمة". PostgreSQL collations do not fold these — ة against ه and ى
-- against ي survive every ai_ci collation — so the folding is explicit here
-- and applied identically on write, through a generated column, and on read,
-- through the same function applied to the search term.
--
-- The function is IMMUTABLE because generated columns and expression indexes
-- require it. That is a real commitment: changing the folding rules later
-- means dropping the dependent columns and indexes, rebuilding them, and
-- accepting that previously indexed rows are re-derived. The rules below cover
-- Iraqi civil-registry practice and are not expected to move.
CREATE OR REPLACE FUNCTION normalize_arabic(input text)
RETURNS text
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT nullif(
        btrim(
            regexp_replace(
                translate(
                    -- Strip tashkeel (U+064B..U+0652), superscript alef
                    -- (U+0670), and tatweel (U+0640): all are decorative and
                    -- inconsistently typed.
                    regexp_replace(lower(input), '[ً-ْٰـ]', '', 'g'),
                    -- Fold hamza carriers onto their base letters, ta marbuta
                    -- onto ha, alef maqsura onto ya.
                    'أإآٱؤئةى',
                    'ااااويهي'
                ),
                '\s+', ' ', 'g'
            )
        ),
        ''
    );
$$;

COMMENT ON FUNCTION normalize_arabic(text) IS
    'Folds Arabic orthographic variants for search: strips tashkeel and tatweel, '
    'unifies hamza carriers to alef, ta marbuta to ha, alef maqsura to ya, and '
    'collapses whitespace. Applied on write via generated columns and on read to '
    'the search term, so both sides of a comparison are folded identically.';

-- Canonicalises Iraqi phone numbers so that 07701234567, +964 770 123 4567,
-- 00964-770-123-4567 and the same digits typed in Arabic-Indic numerals all
-- compare equal.
CREATE OR REPLACE FUNCTION normalize_phone(input text)
RETURNS text
LANGUAGE plpgsql
IMMUTABLE
PARALLEL SAFE
AS $$
DECLARE
    digits text;
BEGIN
    IF input IS NULL THEN
        RETURN NULL;
    END IF;

    -- Arabic-Indic (U+0660..) and Extended Arabic-Indic (U+06F0..) numerals
    -- appear in data typed on Arabic keyboards and in spreadsheet imports.
    digits := translate(input, '٠١٢٣٤٥٦٧٨٩۰۱۲۳۴۵۶۷۸۹', '01234567890123456789');
    digits := regexp_replace(digits, '\D', '', 'g');

    IF digits = '' THEN
        RETURN NULL;
    END IF;

    IF left(digits, 5) = '00964' THEN
        digits := '964' || substr(digits, 6);
    ELSIF left(digits, 3) = '964' THEN
        NULL;                                   -- already canonical
    ELSIF left(digits, 1) = '0' THEN
        digits := '964' || substr(digits, 2);   -- national form
    ELSE
        digits := '964' || digits;              -- bare subscriber number
    END IF;

    RETURN digits;
END;
$$;

COMMENT ON FUNCTION normalize_phone(text) IS
    'Canonicalises a phone number to 964XXXXXXXXXX, translating Arabic-Indic '
    'numerals and stripping punctuation, so that every way of writing one Iraqi '
    'mobile number compares equal.';

-- Reverses a string. Cashiers routinely search by the last four digits of a
-- phone number, which a B-tree cannot answer as a suffix match; storing the
-- reversed digits turns that into an indexable prefix scan.
CREATE OR REPLACE FUNCTION reverse_text(input text)
RETURNS text
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT reverse(input);
$$;

-- ---------------------------------------------------------------------------
-- Shared triggers
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- Blocks UPDATE and DELETE on append-only financial tables.
--
-- Application code already refuses to modify a posted payment, but the
-- application is not the only thing that can reach this database: a psql
-- session, a migration, an ORM misconfiguration, or a future developer who has
-- not read the design document all connect as the same role. This trigger is
-- the layer that does not depend on anyone remembering the rule. Corrections
-- happen the way the domain says they do — void, refund, adjustment — each of
-- which inserts a new row.
CREATE OR REPLACE FUNCTION forbid_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION
        'table %I is append-only; % is not permitted on it', TG_TABLE_NAME, TG_OP
        USING
            ERRCODE = 'P0001',
            HINT = 'Correct a posted financial record by inserting a compensating '
                   'document (void, refund, or adjustment), never by editing or '
                   'deleting the original.';
END;
$$;

-- Blocks DELETE while permitting UPDATE of a declared set of columns. Used for
-- documents that have a legitimate state transition — a payment moving to
-- voided — but whose amounts must never change. The permitted columns are
-- passed as trigger arguments.
CREATE OR REPLACE FUNCTION forbid_column_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    permitted   text[] := TG_ARGV;
    col         text;
    old_value   text;
    new_value   text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'rows in %I are permanent; DELETE is not permitted', TG_TABLE_NAME
            USING ERRCODE = 'P0001',
                  HINT = 'Post a compensating document instead of deleting.';
    END IF;

    FOR col IN
        SELECT attname
        FROM pg_attribute
        WHERE attrelid = TG_RELID
          AND attnum > 0
          AND NOT attisdropped
    LOOP
        IF col = ANY (permitted) THEN
            CONTINUE;
        END IF;

        EXECUTE format('SELECT ($1).%I::text', col) INTO old_value USING OLD;
        EXECUTE format('SELECT ($1).%I::text', col) INTO new_value USING NEW;

        IF old_value IS DISTINCT FROM new_value THEN
            RAISE EXCEPTION
                'column %I.%I is immutable once the record exists (attempted % -> %)',
                TG_TABLE_NAME, col, coalesce(old_value, 'NULL'), coalesce(new_value, 'NULL')
                USING ERRCODE = 'P0001',
                      HINT = 'Only the declared state-transition columns may change on this table.';
        END IF;
    END LOOP;

    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION forbid_column_mutation() IS
    'Trigger guard for financial documents: forbids DELETE outright and forbids '
    'UPDATE of every column except those named in the trigger arguments.';
