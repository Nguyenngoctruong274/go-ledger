-- Idempotent posting: the transfer id is the client's idempotency key.
-- request_hash is ledger.Transfer.Fingerprint(); a retry with the same id
-- and the same hash replays the original result, a different hash is a
-- conflict. Transfers posted before this migration have no hash and can
-- only be reported as conflicts if their id is reused.
ALTER TABLE transfers ADD COLUMN request_hash BYTEA
    CHECK (request_hash IS NULL OR octet_length(request_hash) = 32);

-- The balance each entry produced, so a replay (and an account statement)
-- can show it without recomputing history.
ALTER TABLE entries ADD COLUMN balance_after BIGINT;

-- Backfill existing rows. Entries are append-only, so the guard trigger is
-- switched off for this one controlled update inside the migration's
-- transaction.
ALTER TABLE entries DISABLE TRIGGER entries_append_only;

UPDATE entries AS e
SET balance_after = r.running
FROM (
    SELECT id, SUM(amount) OVER (PARTITION BY account_id ORDER BY id) AS running
    FROM entries
) AS r
WHERE e.id = r.id;

ALTER TABLE entries ENABLE TRIGGER entries_append_only;

ALTER TABLE entries ALTER COLUMN balance_after SET NOT NULL;
