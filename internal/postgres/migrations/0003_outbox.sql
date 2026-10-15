-- Transactional outbox: events are written in the same transaction as the
-- ledger change that caused them, so an event exists if and only if the
-- change committed. A relay process publishes pending rows afterwards.
CREATE TABLE outbox (
    id              BIGSERIAL   PRIMARY KEY,
    topic           TEXT        NOT NULL,
    key             TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT
);

-- The relay scans the oldest unpublished rows in id order.
CREATE INDEX outbox_pending_idx ON outbox (id) WHERE published_at IS NULL;
