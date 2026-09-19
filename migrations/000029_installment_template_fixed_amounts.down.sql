ALTER TABLE installment_template DROP COLUMN retired_at;

ALTER TABLE installment_template_line
    DROP CONSTRAINT ck_template_line_amount,
    DROP COLUMN amount;
