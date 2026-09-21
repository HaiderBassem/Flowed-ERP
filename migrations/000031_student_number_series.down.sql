-- Reverse of 000031.
--
-- Dropping the counter does not invalidate a number already issued: the numbers
-- live on the student rows and stay unique there. What is lost is the position,
-- so an office stepping back to hand-typed numbers must read the highest one on
-- file before issuing the next. The up script seeds itself from exactly that
-- query, which is what makes stepping forward again safe.

DROP FUNCTION IF EXISTS next_student_no(INTEGER);
DROP TABLE IF EXISTS student_number_series;
