-- Reference rows are removed only if nothing references them. A foreign key
-- violation here is the correct outcome: it means real data depends on this
-- seed, and rolling the migration back would orphan it.

DELETE FROM discount_exclusivity_group WHERE code IN ('EXEMPTION', 'SOCIAL');
DELETE FROM payment_method WHERE code IN ('CASH', 'BANK', 'POS', 'ONLINE', 'TRANSFER', 'OTHER');
DELETE FROM student_category WHERE code IN ('REGULAR', 'REPEAT', 'HOSTED', 'TRANSFER');
DELETE FROM study_type WHERE code IN ('MORNING', 'EVENING', 'PARALLEL');
