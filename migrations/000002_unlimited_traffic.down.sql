BEGIN;

UPDATE plans
SET quota_bytes = 53687091200, updated_at = now()
WHERE code = 'monthly' AND quota_bytes = 0;

UPDATE subscriptions
SET quota_bytes = 53687091200, updated_at = now()
WHERE plan_code = 'monthly' AND quota_bytes = 0;

COMMIT;
