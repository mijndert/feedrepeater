-- Routes a feed to the destinations it publishes to.
--
-- Before this, every destination received every entry. Making the link
-- explicit is what allows one feed to go to a subset of destinations, and it
-- is already shaped for more than one feed per account: nothing here assumes
-- the UNIQUE constraint on feeds.user_id that the beta still carries.
CREATE TABLE feed_destinations (
    feed_id        INTEGER NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    destination_id INTEGER NOT NULL REFERENCES destinations (id) ON DELETE CASCADE,
    user_id        INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at     INTEGER NOT NULL,
    PRIMARY KEY (feed_id, destination_id)
) STRICT;

CREATE INDEX feed_destinations_destination ON feed_destinations (destination_id);
CREATE INDEX feed_destinations_user ON feed_destinations (user_id);

-- Preserve the previous behaviour for anything already stored: every existing
-- destination stays connected to the account's feed.
INSERT INTO feed_destinations (feed_id, destination_id, user_id, created_at)
SELECT f.id, d.id, f.user_id, unixepoch()
FROM feeds f
JOIN destinations d ON d.user_id = f.user_id;
