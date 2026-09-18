BEGIN;

DROP INDEX payments_one_pending_per_user_idx;

ALTER TABLE subscriptions
    ADD COLUMN xui_email TEXT NOT NULL DEFAULT '';

UPDATE subscriptions AS subscription
SET xui_email = account.xui_email,
    updated_at = now()
FROM vpn_accounts AS account
WHERE subscription.id = (
    SELECT candidate.id
    FROM subscriptions AS candidate
    WHERE candidate.user_telegram_id = account.user_telegram_id
    ORDER BY candidate.created_at DESC, candidate.id DESC
    LIMIT 1
);

CREATE UNIQUE INDEX subscriptions_xui_email_idx
    ON subscriptions (xui_email)
    WHERE xui_email <> '';

DROP TABLE vpn_accounts;

COMMIT;
