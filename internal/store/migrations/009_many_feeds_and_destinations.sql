-- More than one feed per account, and more than one destination of a kind.
--
-- Both beta rules were single constraints, placed so that lifting them would be
-- this file and nothing else: the UNIQUE on subscriptions.user_id, and the
-- unique index on destinations (user_id, kind). The product rule that replaces
-- them — five feeds and ten destinations per account — is a count, and a count
-- is enforced inside the transaction that inserts rather than by the schema.
--
-- The first is a table constraint, which SQLite cannot drop in place, so the
-- table is rebuilt. Migrations run with foreign keys off and legacy_alter_table
-- on (see store.migrate), so dropping the old table does not cascade into
-- anything and the rename does not rewrite clauses in other tables. Nothing
-- references subscriptions anyway; the care is for the pattern.
CREATE TABLE subscriptions_new (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    feed_id       INTEGER NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    paused        INTEGER NOT NULL DEFAULT 0,
    prime_item_id INTEGER NOT NULL DEFAULT 0,
    primed        INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    UNIQUE (user_id, feed_id)
) STRICT;

INSERT INTO subscriptions_new (id, user_id, feed_id, paused, prime_item_id, primed, created_at)
SELECT id, user_id, feed_id, paused, prime_item_id, primed, created_at FROM subscriptions;

DROP TABLE subscriptions;
ALTER TABLE subscriptions_new RENAME TO subscriptions;

-- The index went with the old table.
CREATE INDEX subscriptions_feed ON subscriptions (feed_id);
CREATE INDEX subscriptions_user ON subscriptions (user_id);

-- The second rule is an ordinary index and goes in one statement. Two Mastodon
-- accounts, or two Discord channels, are now two rows for one account.
DROP INDEX destinations_user_kind;

-- An OAuth flow used to mean one thing: signing in. It can now also mean
-- connecting a further Mastodon account as a destination, which has to be
-- recorded at the start of the flow — the callback must not learn from a
-- parameter what it was for, any more than it learns the host from one. The
-- account that started a connect flow is recorded with it, so the callback can
-- insist the same session finishes it.
ALTER TABLE oauth_states ADD COLUMN purpose TEXT NOT NULL DEFAULT 'login';
ALTER TABLE oauth_states ADD COLUMN user_id INTEGER REFERENCES users (id) ON DELETE CASCADE;

-- A Mastodon destination now records the account id it posts as, because two
-- of them can be on one instance. Every row written before this was the
-- account's own — one per kind was the rule — so the owner's id is the right
-- value, and with it filled in the match can be exact rather than "same host",
-- which with a second account on that host would be the wrong row.
UPDATE destinations
SET config = json_set(config, '$.remote_id',
    (SELECT u.remote_id FROM users u WHERE u.id = destinations.user_id))
WHERE kind = 'mastodon'
  AND json_valid(config)
  AND json_extract(config, '$.remote_id') IS NULL
  AND json_extract(config, '$.host') = (SELECT u.host FROM users u WHERE u.id = destinations.user_id);
