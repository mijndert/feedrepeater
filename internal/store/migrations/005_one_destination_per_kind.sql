-- One destination per kind per account.
--
-- The dashboard is a row per service, connected or not, and the create handler
-- refuses a second of a kind. Neither of those is the authority. A handler that
-- reads the account's destinations, decides the kind is free, then spends a
-- network round trip verifying a webhook before inserting is a race: requests
-- arriving together all pass the check and all insert. The rule has to live
-- where concurrency cannot get underneath it.
--
-- Rows predating this can hold duplicates, because the previous limit was ten
-- destinations of any mix. The oldest of each kind is kept and the rest are
-- deleted, taking their routes and queued deliveries with them by cascade.
-- Posts already sent stay where they are; only the ability to send more from
-- the duplicate goes.
DELETE FROM destinations WHERE id NOT IN (
    SELECT min(id) FROM destinations GROUP BY user_id, kind
);

CREATE UNIQUE INDEX destinations_user_kind ON destinations (user_id, kind);
