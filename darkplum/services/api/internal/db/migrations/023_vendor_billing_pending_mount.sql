-- pending_mount: the key is stored (configured=true) but the cluster has not
-- mounted the Secret file into the api pod yet. Idempotent: drop-if-exists
-- then re-add.
ALTER TABLE vendor_billing_status DROP CONSTRAINT IF EXISTS vendor_billing_status_state_check;
ALTER TABLE vendor_billing_status ADD CONSTRAINT vendor_billing_status_state_check
    CHECK (state IN ('not_configured','ok','auth_failed','wrong_key_type','unreachable','pulling','pending_mount'));
