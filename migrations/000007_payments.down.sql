ALTER TABLE payment DROP CONSTRAINT IF EXISTS fk_payment_void_request;

DROP TABLE IF EXISTS void_request;
DROP TABLE IF EXISTS refund_allocation;
DROP TABLE IF EXISTS refund;
DROP TABLE IF EXISTS payment_allocation;
DROP TABLE IF EXISTS payment;
DROP TABLE IF EXISTS payment_method;
DROP TABLE IF EXISTS cashier_session;
DROP TABLE IF EXISTS number_series;
DROP TABLE IF EXISTS cashier_desk;
