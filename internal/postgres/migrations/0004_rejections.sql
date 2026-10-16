-- A transfer id gets exactly one outcome. Refusals (insufficient funds,
-- unknown account, currency mismatch) are recorded too, so a duplicate of a
-- refused request is refused again instead of being re-evaluated against
-- balances that may have changed in the meantime.
ALTER TABLE transfers
    ADD COLUMN status TEXT NOT NULL DEFAULT 'posted' CHECK (status IN ('posted', 'rejected')),
    ADD COLUMN reject_code TEXT,
    ADD CONSTRAINT transfers_reject_code_matches_status
        CHECK ((status = 'rejected') = (reject_code IS NOT NULL));
