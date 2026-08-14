-- Reference data the system cannot function without.
--
-- These are seeded as a migration, with fixed identifiers, rather than left to
-- an operator's first afternoon. Every environment then agrees on what the
-- code MORNING means, and a fee policy exported from staging resolves against
-- the same study type in production.
--
-- Everything here remains ordinary master data: an administrator adds a study
-- type the ministry introduces next year through the application, with no
-- migration and no deployment.

INSERT INTO study_type (id, code, name_ar, name_en, sort_order) VALUES
    ('a1000000-0000-4000-8000-000000000001', 'MORNING',  'صباحي', 'Morning',  10),
    ('a1000000-0000-4000-8000-000000000002', 'EVENING',  'مسائي', 'Evening',  20),
    ('a1000000-0000-4000-8000-000000000003', 'PARALLEL', 'موازي', 'Parallel', 30)
ON CONFLICT (code) DO NOTHING;

-- Categories feed fee resolution. REPEAT is what makes a repeating student's
-- differing tuition a data question rather than a code question.
INSERT INTO student_category (id, code, name_ar, name_en) VALUES
    ('a2000000-0000-4000-8000-000000000001', 'REGULAR',  'اعتيادي',  'Regular'),
    ('a2000000-0000-4000-8000-000000000002', 'REPEAT',   'معيد',     'Repeating'),
    ('a2000000-0000-4000-8000-000000000003', 'HOSTED',   'مستضاف',   'Hosted'),
    ('a2000000-0000-4000-8000-000000000004', 'TRANSFER', 'منتقل',    'Transferred in')
ON CONFLICT (code) DO NOTHING;

INSERT INTO payment_method (id, code, name_ar, is_cash, requires_reference, sort_order) VALUES
    ('a3000000-0000-4000-8000-000000000001', 'CASH',     'نقداً',           true,  false, 10),
    ('a3000000-0000-4000-8000-000000000002', 'BANK',     'حوالة مصرفية',   false, true,  20),
    ('a3000000-0000-4000-8000-000000000003', 'POS',      'بطاقة',          false, true,  30),
    ('a3000000-0000-4000-8000-000000000004', 'ONLINE',   'دفع إلكتروني',   false, true,  40),
    ('a3000000-0000-4000-8000-000000000005', 'TRANSFER', 'تحويل داخلي',    false, true,  50),
    ('a3000000-0000-4000-8000-000000000006', 'OTHER',    'أخرى',           false, false, 60)
ON CONFLICT (code) DO NOTHING;

-- Exclusivity groups. Two discounts in one group cannot both apply: a student
-- is a staff child or a hardship case, and stacking the two full-rate would
-- exceed what any circular intends.
INSERT INTO discount_exclusivity_group (id, code, name_ar, description) VALUES
    ('a4000000-0000-4000-8000-000000000001', 'EXEMPTION', 'الإعفاءات',
        'Full exemptions. Never combined with anything else.'),
    ('a4000000-0000-4000-8000-000000000002', 'SOCIAL', 'الخصومات الاجتماعية',
        'Hardship and social-status discounts; one per student.')
ON CONFLICT (code) DO NOTHING;
