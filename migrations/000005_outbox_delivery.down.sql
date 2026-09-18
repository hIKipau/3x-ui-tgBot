BEGIN;

DROP INDEX outbox_events_pending_idx;

ALTER TABLE outbox_events
    DROP COLUMN locked_at,
    DROP COLUMN available_at;

CREATE INDEX outbox_events_pending_idx
    ON outbox_events (created_at, id)
    WHERE processed_at IS NULL;

COMMIT;
