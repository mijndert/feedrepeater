-- OAuth client credentials, registered lazily once per Mastodon instance.
CREATE TABLE instances (
    host          TEXT PRIMARY KEY,
    client_id     TEXT NOT NULL,
    client_secret BLOB NOT NULL,
    created_at    INTEGER NOT NULL
) STRICT;

-- Identity is (host, remote_id). A hostile instance can control the acct and
-- display name it reports, so those are display-only and never used to match.
CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    host          TEXT NOT NULL,
    remote_id     TEXT NOT NULL,
    acct          TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',
    avatar_url    TEXT NOT NULL DEFAULT '',
    access_token  BLOB NOT NULL,
    created_at    INTEGER NOT NULL,
    last_login_at INTEGER NOT NULL,
    UNIQUE (host, remote_id)
) STRICT;

-- id is the SHA-256 of the cookie value; the cookie itself is never stored.
CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
) STRICT;

CREATE INDEX sessions_user ON sessions (user_id);
CREATE INDEX sessions_expiry ON sessions (expires_at);

-- In-flight OAuth authorisations. The host is recorded here at the start of the
-- flow so the callback never has to trust a parameter for it.
CREATE TABLE oauth_states (
    id            TEXT PRIMARY KEY,
    host          TEXT NOT NULL,
    code_verifier BLOB NOT NULL,
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL
) STRICT;

CREATE INDEX oauth_states_expiry ON oauth_states (expires_at);

-- One feed per user during beta, enforced by the UNIQUE on user_id.
CREATE TABLE feeds (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER NOT NULL UNIQUE REFERENCES users (id) ON DELETE CASCADE,
    url           TEXT NOT NULL,
    title         TEXT NOT NULL DEFAULT '',
    etag          TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    primed        INTEGER NOT NULL DEFAULT 0,
    paused        INTEGER NOT NULL DEFAULT 0,
    next_fetch_at INTEGER NOT NULL,
    last_fetch_at INTEGER,
    last_error    TEXT NOT NULL DEFAULT '',
    failures      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
) STRICT;

CREATE INDEX feeds_due ON feeds (next_fetch_at);

CREATE TABLE items (
    id           INTEGER PRIMARY KEY,
    feed_id      INTEGER NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    guid         TEXT NOT NULL,
    url          TEXT NOT NULL DEFAULT '',
    title        TEXT NOT NULL DEFAULT '',
    summary      TEXT NOT NULL DEFAULT '',
    author       TEXT NOT NULL DEFAULT '',
    published_at INTEGER,
    seen_at      INTEGER NOT NULL,
    UNIQUE (feed_id, guid)
) STRICT;

CREATE TABLE destinations (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    label       TEXT NOT NULL,
    config      TEXT NOT NULL DEFAULT '{}',
    credentials BLOB,
    template    TEXT NOT NULL DEFAULT '',
    paused      INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    last_ok_at  INTEGER,
    last_error  TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX destinations_user ON destinations (user_id);

-- user_id is denormalised so that every read path can scope by owner without a
-- join, which makes an accidental cross-tenant query harder to write.
CREATE TABLE deliveries (
    id              INTEGER PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    item_id         INTEGER NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    destination_id  INTEGER NOT NULL REFERENCES destinations (id) ON DELETE CASCADE,
    status          TEXT NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error      TEXT NOT NULL DEFAULT '',
    remote_url      TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    UNIQUE (item_id, destination_id)
) STRICT;

CREATE INDEX deliveries_due ON deliveries (status, next_attempt_at);
CREATE INDEX deliveries_user ON deliveries (user_id, id DESC);
