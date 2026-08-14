DROP FUNCTION IF EXISTS forbid_column_mutation();
DROP FUNCTION IF EXISTS forbid_mutation();
DROP FUNCTION IF EXISTS set_updated_at();
DROP FUNCTION IF EXISTS reverse_text(text);
DROP FUNCTION IF EXISTS normalize_phone(text);
DROP FUNCTION IF EXISTS normalize_arabic(text);

-- Extensions are left in place deliberately. They are cheap, they may be
-- shared with other schemas in the same database, and dropping pg_trgm would
-- silently invalidate indexes belonging to anything else installed here.
