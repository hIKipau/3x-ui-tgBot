BEGIN;

ALTER TABLE users
    ADD COLUMN payment_code_failures INTEGER NOT NULL DEFAULT 0 CHECK (payment_code_failures >= 0),
    ADD COLUMN payment_code_blocked_until TIMESTAMPTZ;

COMMIT;
