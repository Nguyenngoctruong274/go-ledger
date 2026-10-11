-- Accounts hold a denormalized balance that is updated in the same
-- transaction as the entries that change it. The CHECK constraint is the
-- last line of defence against overdrafts on customer accounts.
CREATE TABLE accounts (
    id             TEXT        PRIMARY KEY,
    currency       CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    allow_negative BOOLEAN     NOT NULL DEFAULT FALSE,
    balance        BIGINT      NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT accounts_balance_non_negative CHECK (allow_negative OR balance >= 0)
);

CREATE TABLE transfers (
    id         TEXT        PRIMARY KEY,
    currency   CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE entries (
    id          BIGSERIAL   PRIMARY KEY,
    transfer_id TEXT        NOT NULL REFERENCES transfers (id),
    account_id  TEXT        NOT NULL REFERENCES accounts (id),
    amount      BIGINT      NOT NULL CHECK (amount <> 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT entries_one_per_account UNIQUE (transfer_id, account_id)
);

-- Account statements read entries newest-first per account.
CREATE INDEX entries_account_id_id_idx ON entries (account_id, id DESC);

-- Defence in depth for the zero-sum rule enforced in Go: at commit time,
-- every transfer touched by the transaction must balance.
CREATE FUNCTION entries_check_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (SELECT COALESCE(SUM(amount), 0) FROM entries WHERE transfer_id = NEW.transfer_id) <> 0 THEN
        RAISE EXCEPTION 'transfer % does not balance', NEW.transfer_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER entries_balanced
    AFTER INSERT ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION entries_check_balanced();

-- The ledger is append-only: corrections are new transfers, never edits.
CREATE FUNCTION entries_reject_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'entries are append-only' USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER entries_append_only
    BEFORE UPDATE OR DELETE ON entries
    FOR EACH ROW EXECUTE FUNCTION entries_reject_change();
