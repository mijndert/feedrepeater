-- Records when a feed's content last actually changed, as opposed to when it
-- was last fetched.
--
-- Polling every feed on a fixed interval means a blog that posts twice a year
-- is fetched as often as one that posts hourly. This column is what lets the
-- interval widen while a feed stays quiet and snap back the moment it moves.
--
-- Existing rows get the time of the migration rather than NULL: treating a
-- known-good feed as having been quiet forever would push it straight to the
-- slowest tier.
ALTER TABLE feeds ADD COLUMN changed_at INTEGER;

UPDATE feeds SET changed_at = unixepoch() WHERE changed_at IS NULL;
