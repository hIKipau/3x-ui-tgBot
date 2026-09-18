BEGIN;

CREATE TABLE users (
    telegram_id   BIGINT PRIMARY KEY CHECK (telegram_id > 0),
    username      TEXT NOT NULL DEFAULT '',
    first_name    TEXT NOT NULL DEFAULT '',
    last_name     TEXT NOT NULL DEFAULT '',
    language_code TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE plans (
    code          TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    duration_days INTEGER NOT NULL CHECK (duration_days > 0),
    quota_bytes   BIGINT NOT NULL DEFAULT 0 CHECK (quota_bytes >= 0),
    amount_minor  BIGINT NOT NULL CHECK (amount_minor >= 0),
    currency      CHAR(3) NOT NULL CHECK (currency = upper(currency)),
    is_active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO plans (code, name, duration_days, quota_bytes, amount_minor, currency)
VALUES ('monthly', 'VPN на 30 дней', 30, 53687091200, 29900, 'RUB');

CREATE TABLE subscriptions (
    id               BIGSERIAL PRIMARY KEY,
    user_telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    plan_code        TEXT NOT NULL REFERENCES plans(code) ON UPDATE CASCADE ON DELETE RESTRICT,
    status           TEXT NOT NULL CHECK (status IN ('pending', 'active', 'expired', 'cancelled')),
    starts_at        TIMESTAMPTZ,
    expires_at       TIMESTAMPTZ,
    quota_bytes      BIGINT NOT NULL DEFAULT 0 CHECK (quota_bytes >= 0),
    xui_email        TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (starts_at IS NULL OR expires_at IS NULL OR expires_at > starts_at)
);

CREATE INDEX subscriptions_user_created_idx
    ON subscriptions (user_telegram_id, created_at DESC);

CREATE INDEX subscriptions_active_user_idx
    ON subscriptions (user_telegram_id, expires_at DESC)
    WHERE status = 'active';

CREATE UNIQUE INDEX subscriptions_xui_email_idx
    ON subscriptions (xui_email)
    WHERE xui_email <> '';

CREATE TABLE payments (
    id                  BIGSERIAL PRIMARY KEY,
    user_telegram_id    BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE RESTRICT,
    subscription_id     BIGINT REFERENCES subscriptions(id) ON DELETE SET NULL,
    kind                TEXT NOT NULL CHECK (kind IN ('purchase', 'extension')),
    provider            TEXT NOT NULL,
    provider_payment_id TEXT NOT NULL DEFAULT '',
    idempotency_key     TEXT NOT NULL UNIQUE,
    amount_minor        BIGINT NOT NULL CHECK (amount_minor >= 0),
    currency            CHAR(3) NOT NULL CHECK (currency = upper(currency)),
    status              TEXT NOT NULL CHECK (status IN ('pending', 'paid', 'cancelled', 'failed')),
    checkout_url        TEXT NOT NULL DEFAULT '',
    provider_payload    JSONB NOT NULL DEFAULT '{}'::jsonb,
    paid_at             TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX payments_provider_id_idx
    ON payments (provider, provider_payment_id)
    WHERE provider_payment_id <> '';

CREATE INDEX payments_user_created_idx
    ON payments (user_telegram_id, created_at DESC);

CREATE TABLE outbox_events (
    id            BIGSERIAL PRIMARY KEY,
    aggregate_type TEXT NOT NULL,
    aggregate_id  TEXT NOT NULL,
    event_type    TEXT NOT NULL,
    payload       JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at  TIMESTAMPTZ,
    attempts      INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX outbox_events_pending_idx
    ON outbox_events (created_at, id)
    WHERE processed_at IS NULL;

COMMIT;

