BEGIN;

CREATE TABLE telegram_payment_confirmations (
    update_id        BIGINT PRIMARY KEY CHECK (update_id > 0),
    user_telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE RESTRICT,
    subscription_id  BIGINT NOT NULL REFERENCES subscriptions(id) ON DELETE RESTRICT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX telegram_payment_confirmations_user_idx
    ON telegram_payment_confirmations (user_telegram_id, created_at DESC);

COMMIT;
