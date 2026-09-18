BEGIN;

ALTER TABLE outbox_events
    ADD COLUMN available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN locked_at TIMESTAMPTZ;

DROP INDEX outbox_events_pending_idx;
CREATE INDEX outbox_events_pending_idx
    ON outbox_events (available_at, created_at, id)
    WHERE processed_at IS NULL;

COMMIT;
