-- Reverting drops the record of what was shipped. The archives themselves stay
-- where they are: they are files on another host and this migration never
-- touched them.

DROP VIEW IF EXISTS v_audit_shipment_coverage;
DROP TABLE IF EXISTS audit_shipment;
