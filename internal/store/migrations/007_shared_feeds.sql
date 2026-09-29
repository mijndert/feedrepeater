-- Feeds become shared, addressed by their URL rather than owned by an account.
--
-- Before this, a feed row belonged to one user, so twenty accounts following the
-- same blog meant twenty rows, twenty fetches every interval, and twenty copies
-- of the same entries. The bytes were never the problem; the requests were. One
-- popular domain saw our user agent once per subscriber per fifteen minutes, and
-- the cost of adding a user was a permanent addition to someone else's traffic.
--
-- The split is: `feeds` is the document (one row per URL, no owner),
-- `feed_state` is what polling knows about it, and `subscriptions` is the part
-- that belongs to an account. Items hang off the feed and are therefore shared
-- too, which is what makes a second subscriber cost one row instead of a fetch.
--
-- What does NOT change here is the product rule that an account has one feed.
-- It moves from a UNIQUE on feeds.user_id to a UNIQUE on subscriptions.user_id,
-- so every handler and the dashboard keep their current meaning. Lifting the
-- beta rule later is dropping that one index, not another migration like this.

-- --- remapping tables ------------------------------------------------------
--
-- The surviving row for a URL is the lowest id that had it. Feeds nobody else
-- duplicated therefore keep their own id, and only genuine duplicates move,
-- which keeps the rewrites below proportional to the overlap rather than to the
-- size of the table.

CREATE TABLE feed_map (
    old_id INTEGER PRIMARY KEY,
    new_id INTEGER NOT NULL
);

INSERT INTO feed_map (old_id, new_id)
SELECT f.id, (SELECT min(g.id) FROM feeds g WHERE g.url = f.url)
FROM feeds f;

-- The same entry fetched by two accounts is two rows with one guid. They fold
-- onto the lowest id, and everything pointing at the losers is repointed first.
CREATE TABLE item_map (
    old_id INTEGER PRIMARY KEY,
    new_id INTEGER NOT NULL
);

INSERT INTO item_map (old_id, new_id)
SELECT i.id, min(j.id)
FROM items i
JOIN feed_map im ON im.old_id = i.feed_id
JOIN feed_map jm ON jm.new_id = im.new_id
JOIN items j ON j.feed_id = jm.old_id AND j.guid = i.guid
GROUP BY i.id;

-- --- fold the duplicates ---------------------------------------------------

-- A destination belongs to exactly one account and an account had exactly one
-- feed, so two deliveries cannot collide on (item, destination) once the items
-- merge. This deletes the collision anyway rather than letting a UNIQUE abort a
-- migration at startup: losing one row of send history is recoverable, a
-- database that refuses to open is not.
DELETE FROM deliveries WHERE id IN (
    SELECT d.id FROM deliveries d
    JOIN item_map m ON m.old_id = d.item_id
    WHERE m.new_id <> d.item_id
      AND EXISTS (
          SELECT 1 FROM deliveries e
          WHERE e.item_id = m.new_id AND e.destination_id = d.destination_id
      )
);

UPDATE deliveries
SET item_id = (SELECT new_id FROM item_map WHERE old_id = deliveries.item_id)
WHERE item_id IN (SELECT old_id FROM item_map WHERE new_id <> old_id);

DELETE FROM items WHERE id IN (SELECT old_id FROM item_map WHERE new_id <> old_id);

UPDATE items
SET feed_id = (SELECT new_id FROM feed_map WHERE old_id = items.feed_id)
WHERE feed_id IN (SELECT old_id FROM feed_map WHERE new_id <> old_id);

UPDATE feed_destinations
SET feed_id = (SELECT new_id FROM feed_map WHERE old_id = feed_destinations.feed_id)
WHERE feed_id IN (SELECT old_id FROM feed_map WHERE new_id <> old_id);

-- --- subscriptions ---------------------------------------------------------

CREATE TABLE subscriptions (
    id      INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    feed_id INTEGER NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    -- Pausing is per subscriber. A feed is only left alone entirely when every
    -- subscription to it is paused, which the poll query works out for itself.
    paused  INTEGER NOT NULL DEFAULT 0,
    -- Items with an id at or below this are never delivered to this subscriber.
    -- It is what replaces the old per-feed `primed` flag: joining a feed someone
    -- else already follows means inheriting their entries as history, and this
    -- watermark is the line between history and news for each account
    -- separately. Priming is therefore free — no fetch, no replay.
    prime_item_id INTEGER NOT NULL DEFAULT 0,
    primed        INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    UNIQUE (user_id, feed_id),
    -- The beta rule, moved rather than dropped. See the header.
    UNIQUE (user_id)
) STRICT;

CREATE INDEX subscriptions_feed ON subscriptions (feed_id);

-- prime_item_id is zero for everything already stored. The watermark only ever
-- gates entries at the moment they are inserted, and every entry these accounts
-- can see is already in the table and already recorded as seen, so there is
-- nothing for a non-zero value to hold back.
INSERT INTO subscriptions (user_id, feed_id, paused, prime_item_id, primed, created_at)
SELECT f.user_id, m.new_id, f.paused, 0, f.primed, f.created_at
FROM feeds f
JOIN feed_map m ON m.old_id = f.id;

-- --- fetch state -----------------------------------------------------------
--
-- Split from `feeds` because it is the only part that changes on a poll. A
-- fetch rewrites next_fetch_at whether or not anything happened, so with the
-- columns inline every poll dirtied a page of the wide feeds table holding
-- perhaps a dozen feeds. Narrow rows pack far more feeds per page, so the same
-- poll cycle ships a fraction of the pages to the replica.

CREATE TABLE feed_state (
    feed_id       INTEGER PRIMARY KEY REFERENCES feeds (id) ON DELETE CASCADE,
    etag          TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    -- SHA-256 of the last body actually parsed. A server with no conditional
    -- request support answers 200 with identical bytes forever; comparing the
    -- hash is what stops that costing a parse and a few hundred no-op inserts
    -- every interval.
    body_hash     BLOB,
    next_fetch_at INTEGER NOT NULL,
    last_fetch_at INTEGER,
    changed_at    INTEGER,
    last_error    TEXT NOT NULL DEFAULT '',
    failures      INTEGER NOT NULL DEFAULT 0,
    -- When the current run of failures began, or NULL if the last fetch worked.
    -- The dead-feed rule is about elapsed time, and elapsed time cannot be
    -- recovered from a count once the interval between attempts varies: the
    -- failures are banked at whatever the feed was being polled at when they
    -- happened, and multiplying them by today's interval reprices history.
    failing_since INTEGER,
    -- The dead-feed rule, which is a property of the document and applies to
    -- every subscriber at once. Distinct from subscriptions.paused, which is one
    -- account's choice about a feed that may be perfectly healthy.
    disabled      INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX feed_state_due ON feed_state (next_fetch_at) WHERE disabled = 0;

-- A feed carrying failures into the migration has been failing since roughly
-- now minus what it had banked at the old flat interval. Anything already
-- healthy gets NULL, which reads as "not failing".
INSERT INTO feed_state (feed_id, etag, last_modified, next_fetch_at, last_fetch_at,
        changed_at, last_error, failures, failing_since, disabled)
SELECT f.id, f.etag, f.last_modified, f.next_fetch_at, f.last_fetch_at,
       f.changed_at, f.last_error, f.failures,
       CASE WHEN f.failures > 0 THEN unixepoch() ELSE NULL END, 0
FROM feeds f
WHERE f.id IN (SELECT new_id FROM feed_map);

-- --- the feed itself -------------------------------------------------------

CREATE TABLE feeds_new (
    id         INTEGER PRIMARY KEY,
    url        TEXT NOT NULL UNIQUE,
    title      TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
) STRICT;

INSERT INTO feeds_new (id, url, title, created_at)
SELECT f.id, f.url, f.title, f.created_at
FROM feeds f
WHERE f.id IN (SELECT new_id FROM feed_map);

DROP TABLE feeds;
ALTER TABLE feeds_new RENAME TO feeds;

DROP TABLE feed_map;
DROP TABLE item_map;

-- --- indexes ---------------------------------------------------------------

-- TrimItems asks for the newest n of one feed every hour. Without the id in the
-- index that is a seek on (feed_id, guid) followed by a sort of everything the
-- feed has.
CREATE INDEX items_feed_recent ON items (feed_id, id DESC);

-- Nothing indexed destination_id: UNIQUE (item_id, destination_id) does not
-- have it leftmost, so deleting a destination full-scanned the whole deliveries
-- table to find the rows to cascade.
CREATE INDEX deliveries_destination ON deliveries (destination_id);

-- The sender only ever asks for pending rows, and pending is the small minority
-- once a service has been running: sent rows outnumber them by whatever the
-- retention allows. A partial index holds the queue and nothing else, so it
-- stays cache-resident no matter how much history accumulates.
DROP INDEX deliveries_due;
CREATE INDEX deliveries_due ON deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX deliveries_pending_user ON deliveries (user_id) WHERE status = 'pending';
