BEGIN;

ALTER TABLE users
    DROP COLUMN payment_code_blocked_until,
    DROP COLUMN payment_code_failures;

COMMIT;
