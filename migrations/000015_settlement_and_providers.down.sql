-- Rolling back removes the reconciliation controls and the provider boundary.
-- No payment row is touched: settlement is evidence held beside the ledger, and
-- an intent is a request that either became a payment or did not. What is lost
-- is the ability to notice that the bank and the system disagree.

DROP VIEW IF EXISTS v_unconfirmed_electronic_payments;
DROP VIEW IF EXISTS v_settlement_exceptions;

DROP TRIGGER IF EXISTS trg_provider_event_no_update ON payment_provider_event;
DROP TABLE IF EXISTS payment_provider_event;

DROP TRIGGER IF EXISTS trg_payment_intent_updated_at ON payment_intent;
DROP TABLE IF EXISTS payment_intent;

DROP TABLE IF EXISTS settlement_line;
DROP TABLE IF EXISTS settlement_batch;

DROP INDEX IF EXISTS uq_payment_external_reference;
ALTER TABLE payment DROP COLUMN IF EXISTS reference_duplicate_ack;
