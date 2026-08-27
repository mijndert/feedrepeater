-- Mastodon is the only destination.
--
-- Bluesky, Discord, Slack, ntfy, linkding and generic webhooks are gone from
-- the binary. A row of one of those kinds can no longer be built into anything
-- that sends, so it would sit in the queue failing "unknown destination type"
-- six times before being abandoned, and show on a dashboard as a service that
-- cannot be edited or removed. Deleting it is the honest version of what has
-- already happened.
--
-- Routes and deliveries are deleted explicitly rather than left to the cascade
-- that would do it in normal running: migrations are applied with foreign keys
-- off, so ON DELETE CASCADE does not fire here, and the check that runs after
-- them would find the orphans this leaves. Order matters — dependents first,
-- then the rows they point at.
--
-- Delivery history goes with them, which is the one visible loss: an account
-- that was posting to Discord loses the record that it did. Posts already
-- published stay where they were published; what goes is the ability to send
-- more. The credentials go too, and that is the point worth stating out loud:
-- an app password, a channel webhook URL or an API token this service no longer
-- has any use for should not be sitting in a backup either.
--
-- The kind column and its unique index stay. They are the seam a second service
-- comes back through, and a table that has never carried a kind would need a
-- migration to learn one.
DELETE FROM deliveries
WHERE destination_id IN (SELECT id FROM destinations WHERE kind <> 'mastodon');

DELETE FROM feed_destinations
WHERE destination_id IN (SELECT id FROM destinations WHERE kind <> 'mastodon');

DELETE FROM destinations WHERE kind <> 'mastodon';
