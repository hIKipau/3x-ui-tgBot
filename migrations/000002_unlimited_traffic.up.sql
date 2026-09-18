BEGIN;

UPDATE plans
SET quota_bytes = 0, updated_at = now()
WHERE code = 'monthly';

UPDATE subscriptions
SET quota_bytes = 0, updated_at = now()
WHERE plan_code = 'monthly';

COMMIT;
