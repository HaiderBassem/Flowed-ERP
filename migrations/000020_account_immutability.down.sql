-- Reverting removes the guard. Nothing in the data has to change: the trigger
-- only ever refused writes, so every existing row already satisfies it.

DROP TRIGGER IF EXISTS trg_account_immutable ON financial_account;
DROP FUNCTION IF EXISTS check_account_immutability();
