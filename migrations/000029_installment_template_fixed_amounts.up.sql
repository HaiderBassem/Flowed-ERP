-- An installment template line has always been a share of whatever net a
-- specific account turns out to owe (share_bp), which is what lets one
-- template serve every student in its scope regardless of individual
-- discounts. Some plans are simpler than that: "Evening, 2026-2027,
-- 400,000 / 400,000 / 350,000 / 350,000" names literal amounts, not
-- percentages, and forcing an administrator to convert that into basis
-- points by hand is where a plan quietly stops summing to what was meant.
--
-- amount is nullable and additive. share_bp keeps meaning what it always
-- meant and is still required on every row, computed from the literal
-- amounts when they are given (so an existing re-split, which only knows
-- shares, keeps working unchanged). Generation prefers the exact amount when
-- every line on a template carries one — see billing.GeneratePlan.
ALTER TABLE installment_template_line
    ADD COLUMN amount BIGINT,
    ADD CONSTRAINT ck_template_line_amount CHECK (amount IS NULL OR amount >= 0);

COMMENT ON COLUMN installment_template_line.amount IS
    'A literal installment amount, when the template was authored in fixed '
    'amounts rather than percentages. NULL for an ordinary share-based line. '
    'share_bp is always populated regardless, derived from these amounts at '
    'definition time, so re-splitting a partial remainder mid-year still has '
    'a share to work from.';

-- installment_template's status column already accepts 'retired' — nothing
-- ever set it, because there was no command to. A published template's scope
-- occupies uq_installment_template_scope until retired, the same rule
-- fee_policy_version enforces, so changing a plan needs the same retire step
-- fee policies already have.
ALTER TABLE installment_template ADD COLUMN retired_at TIMESTAMPTZ;

