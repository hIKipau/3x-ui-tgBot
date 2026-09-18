BEGIN;

CREATE TABLE vpn_accounts (
    user_telegram_id BIGINT PRIMARY KEY REFERENCES users(telegram_id) ON DELETE CASCADE,
    xui_email        TEXT NOT NULL UNIQUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO vpn_accounts (user_telegram_id, xui_email, created_at, updated_at)
SELECT DISTINCT ON (user_telegram_id)
       user_telegram_id, xui_email, created_at, updated_at
FROM subscriptions
WHERE xui_email <> ''
ORDER BY user_telegram_id, updated_at DESC, id DESC;

DROP INDEX subscriptions_xui_email_idx;
ALTER TABLE subscriptions DROP COLUMN xui_email;

CREATE UNIQUE INDEX payments_one_pending_per_user_idx
    ON payments (user_telegram_id, provider)
    WHERE status = 'pending';

COMMIT;
