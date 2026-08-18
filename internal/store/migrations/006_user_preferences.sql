-- Account-level preferences: the timezone dates are read in, and the post text
-- a new destination starts from.
--
-- Both default to empty, and empty means the behaviour that was there before
-- the columns existed: dates render in UTC, and a new destination starts from
-- the built-in template. So this is a pure addition — no row already stored
-- changes meaning, and an account that never opens Settings behaves as it did.
--
-- default_template is read when a destination is created, not when one is sent.
-- Editing it must not silently rewrite what existing destinations post, which
-- is why the text is copied onto the destination rather than referenced from it.
ALTER TABLE users ADD COLUMN timezone TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN default_template TEXT NOT NULL DEFAULT '';
