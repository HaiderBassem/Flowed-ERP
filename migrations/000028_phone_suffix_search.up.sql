-- normalize_phone() always prepends the 964 country code to whatever digits
-- it is given, so passing it a short fragment like the last four digits of a
-- number ("4567") canonicalises to "9644567" — which never equals a real
-- 13-digit phone_norm. That silently broke the suffix search phone_rev
-- exists for: cashiers search by the last few digits, and the query never
-- ran the comparison that would have found them.
--
-- phone_digits() strips a search term down to plain digits with no country-
-- code assumption, so a fragment can be reversed and matched as a prefix of
-- phone_rev exactly the way phone_rev was built to be searched.
CREATE OR REPLACE FUNCTION phone_digits(input text)
RETURNS text
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT NULLIF(
        regexp_replace(
            translate(input, '٠١٢٣٤٥٦٧٨٩۰۱۲۳۴۵۶۷۸۹', '01234567890123456789'),
            '\D', '', 'g'
        ),
        ''
    );
$$;

COMMENT ON FUNCTION phone_digits(text) IS
    'Digits only, no country-code canonicalisation, for suffix/fragment phone '
    'search against phone_rev. Distinct from normalize_phone(), which always '
    'assumes it is given a whole number.';
