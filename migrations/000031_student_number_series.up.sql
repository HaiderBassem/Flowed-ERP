-- The university number, issued by the system rather than typed in.
--
-- It was a required field the office filled from a paper ledger, which is two
-- ways to get it wrong: a number reused because the ledger was a page behind,
-- and a number skipped because somebody guessed high. The reused one is
-- expensive — the unique index refuses the second student, at the counter, with
-- the first student's fees already collected under that number.
--
-- Keyed by calendar year of registration, not academic year: registration
-- happens before a year is opened as often as after, and a number that could not
-- be issued until somebody opened a year would block the first intake of every
-- September.

CREATE TABLE student_number_series
(
    series_year INTEGER PRIMARY KEY,
    next_number BIGINT      NOT NULL DEFAULT 1,
    padding     SMALLINT    NOT NULL DEFAULT 4,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_student_series_next CHECK (next_number >= 1),
    CONSTRAINT ck_student_series_padding CHECK (padding BETWEEN 1 AND 12)
);

COMMENT ON TABLE student_number_series IS
    'One counter per calendar year behind next_student_no(). Deliberately not '
    'UNLOGGED: losing it would restart numbering at 1 and collide with every '
    'number already issued this year.';

-- Seeded from what is already on file, so the first number this issues cannot
-- collide with one the office typed in by hand before today. Only numbers in the
-- YYYY-NNNN shape are considered — anything else was never part of this sequence
-- and must not move it.
INSERT INTO student_number_series (series_year, next_number)
SELECT substring(student_no FROM 1 FOR 4)::int,
       max(substring(student_no FROM 6)::bigint) + 1
FROM student
WHERE student_no ~ '^[0-9]{4}-[0-9]+$'
GROUP BY 1
ON CONFLICT (series_year) DO NOTHING;

-- next_student_no hands out the next number for a year, creating the counter on
-- first use.
--
-- The increment is a single UPDATE, which takes the row lock, reads the counter
-- and advances it in one atomic step: there is no window in which two callers
-- read the same value. It runs in the caller's transaction, so a registration
-- that rolls back returns its number rather than burning it — the same property
-- that makes the printed receipt sequence gapless.
CREATE FUNCTION next_student_no(p_year INTEGER)
    RETURNS TEXT
    LANGUAGE plpgsql
AS
$$
DECLARE
    v_number  BIGINT;
    v_padding SMALLINT;
BEGIN
    INSERT INTO student_number_series (series_year)
    VALUES (p_year)
    ON CONFLICT (series_year) DO NOTHING;

    UPDATE student_number_series
    SET next_number = next_number + 1,
        updated_at  = now()
    WHERE series_year = p_year
    RETURNING next_number - 1, padding INTO v_number, v_padding;

    RETURN p_year::text || '-' || lpad(v_number::text, v_padding, '0');
END;
$$;

COMMENT ON FUNCTION next_student_no(INTEGER) IS
    'The next university number for a calendar year, e.g. 2026-0001. Must be '
    'called inside the registering transaction so a rollback returns the number.';
