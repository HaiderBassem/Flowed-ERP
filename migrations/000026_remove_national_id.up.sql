-- National ID is no longer part of the student workflow. The registrar
-- identifies a student by student_no; national_id was carried alongside it
-- but nothing financial keys off it, and it is being dropped rather than
-- merely hidden so a psql session cannot resurrect it into the API by
-- accident.

DROP INDEX IF EXISTS uq_student_national_id;

ALTER TABLE student
    DROP COLUMN IF EXISTS national_id;

-- student_identity_version is an append-only history of legal-identity
-- changes (court decisions on name changes, etc). Its national_id values are
-- historical fact, not live state, but the field is retired everywhere else
-- and a version row that still carries it would be the one place the old
-- shape survives.
ALTER TABLE student_identity_version
    DROP COLUMN IF EXISTS national_id;
