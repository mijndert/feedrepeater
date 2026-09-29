# feedrepeater

Posts RSS and Atom entries to Mastodon.

Sign in with a Mastodon account on any instance and add one feed, by its own
address or by the address of the site that links to it. New entries are posted
to the account you signed in with; entries already in the feed when you add it
are not. There is nothing to connect — the account is the destination.

Single Go binary, SQLite, no JavaScript.

## Running it

Requires Go 1.26 and [go-task](https://taskfile.dev).

```
task dev
```

That writes a `.env` with a generated key on first run and serves
<http://localhost:8080>. `task` on its own lists everything else, and
`task dev:insecure` drops the guard on private addresses so feeds on your own
machine work.

## Configuration

| Variable | Required | Default | |
|---|---|---|---|
| `FR_BASE_URL` | yes | | Public origin. The OAuth redirect URI derives from it, so changing it invalidates registered apps. |
| `FR_SECRET_KEY` | yes | | 32 bytes of hex, from `feedrepeater genkey`. Every stored credential is encrypted under a key derived from it. |
| `FR_ADDR` | | `127.0.0.1:8080` | Listen address. |
| `FR_DB_PATH` | | `feedrepeater.db` | SQLite file. |
| `FR_MIN_POLL_INTERVAL` | | `15m` | Floor on how often a feed is fetched. See [Polling](#polling). |
| `FR_MAX_ITEMS_PER_POLL` | | `5` | Entries delivered per poll. A larger burst is recorded but not posted, so a feed that renumbers its ids cannot flood a timeline. |
| `FR_ALLOW_PRIVATE_NETWORKS` | | | `1` disables the private-address guard. Development only. |
| `FR_MAX_ACCOUNTS` | | unlimited | Stop accepting new accounts past this many. Existing accounts always sign in. |
| `FR_BLOCKED_INSTANCES` | | | Comma-separated hosts refused at sign-in. |
| `FR_CONTACT`, `FR_JURISDICTION` | | | Shown on the terms page. Unset, it omits that section. |

**Losing `FR_SECRET_KEY` means losing every stored credential.** The database is
useless without it, so keep it somewhere other than the backups.

## Deploying

Caddy on the host, feedrepeater in Docker, Litestream streaming the database to
object storage. Create `deploy/.env`, then `task docker:up`:

```
FR_BASE_URL=https://feeds.example.com
FR_SECRET_KEY=<feedrepeater genkey>
LITESTREAM_REPLICA_URL=s3://your-bucket/feedrepeater
LITESTREAM_ACCESS_KEY_ID=...
LITESTREAM_SECRET_ACCESS_KEY=...
```

The container binds to `127.0.0.1:8080` and `deploy/Caddyfile` terminates TLS in
front of it. Point the site block at your own domain. It also rejects anything
not arriving from a Cloudflare address, because the client IP it forwards for
rate limiting is a header anyone can set — if you are not behind Cloudflare,
drop that block and forward `{remote_host}` instead.

On start the entrypoint restores from the replica if the local database is
missing, then runs the binary under `litestream replicate`. `task
litestream:restore` pulls a copy out for inspection.

## How it works

**The destination is the account that signed in.** It is created at sign-in, and
again when a feed is added if that never worked, so a dashboard with a feed on
it always posts somewhere. Visibility and post text are set under Post settings.

The destination holds its own copy of the account's token, sealed under its own
key. Each sign-in issues a new token, re-seals the destination against it and
revokes the old one, which makes signing out and back in the repair for a
destination whose token an instance has revoked. Accounts are matched on
instance host rather than handle, since a handle is display text the instance
can change and a rename must not quietly break that repair.

Bluesky, Discord, Slack, ntfy, linkding and signed webhooks were all here and
were removed in favour of doing one thing. Each took an address from a user and
so needed its own proof that the address belonged to whoever typed it — without
that, this service is a way to aim signed-in traffic at a stranger's server. The
`kind` column and `feed_destinations` stay behind as the seam a second service
returns through; `008_mastodon_only.sql` deleted the rest.

**Post text** is a template over a fixed set of placeholders:

```
{{title}} {{url}} {{summary}} {{author}} {{feed_title}} {{published}}
```

Substitution is a single literal pass, so a value that looks like a placeholder
is printed rather than expanded. Text is trimmed to the instance's own post
length — asked for, not assumed, since it is not 500 everywhere — by shortening
the summary first and then the title, never the URL. `{{published}}` renders in
the account's timezone from Settings, defaulting to UTC; the binary embeds the
IANA database. Settings also holds a default post text, copied into a
destination at creation rather than referenced.

**Adding a feed** accepts the feed address or the site address. A document that
parses as a feed is used as one; one that does not is read as HTML for its
`<link rel="alternate">` tags, and the first linked feed that fetches is stored.
Discovery costs one request per candidate, capped at three. The destination is
created before the subscription is written, because a feed that existed for even
a moment without one would be polled and published nowhere.

**Failed deliveries** can be requeued from the dashboard. The retry goes back to
the worker rather than being sent from the request, with its attempt count reset
so a delivery that spent all six is not abandoned again on the first try.

## Polling

A feed is asked as often as it has recently earned. `FR_MIN_POLL_INTERVAL` is
the floor, every feed starts there, and the first new entry drops a feed
straight back to it.

| Quiet for | Checked every |
|---|---|
| under a day | the floor — 15m by default |
| under a week | 2× the floor |
| under a month | 8× the floor |
| longer | 24× the floor |

Multiples rather than absolute durations, so a one-minute interval set for
development gets a one-minute service. A feed nothing is waiting on — every
subscriber or destination paused — is checked at 4× the floor at best, though
its entries are still recorded so resuming starts from the right place.

Holding the request rate down beyond the schedule: conditional requests
(ETag/If-Modified-Since), body hashing for the many servers that support neither
and return identical bytes with a 200, per-host spacing of 5 seconds, ±15%
jitter so feeds added the same afternoon do not stay synchronised, and
`Retry-After` and `Cache-Control: max-age` honoured up to 24 hours — neither can
pull an interval in, only push it out. After two weeks of unbroken failures a
feed is stopped and the dashboard says why; the rule is elapsed time rather than
a count of attempts, so it means the same thing at any point on the ladder.

**A feed is fetched once, however many accounts follow it.** Feeds are addressed
by URL rather than owned, and a new subscriber joins at the current watermark so
none of the backlog is delivered. `/stats` publishes `feeds.subscriptions`
alongside `feeds.total`; the gap is fetches no longer being made. Two
consequences are invisible from the dashboard:

- **"Check now" pulls the feed forward for every subscriber**, since there is
  only one poll to pull forward. It is rate limited per account.
- **A failing feed's error is the feed's, not yours** — everyone subscribed sees
  the same message, which contains only what the feed's server said. Pausing, by
  contrast, is yours alone.

Adding a feed still fetches it even when the address is already followed for
somebody else. Skipping would be quicker, and visibly so in the response, which
would let anyone with an account learn which feeds this service's users read.

## Design notes

- **One writer, many readers.** Writes go through a single connection, removing
  every `SQLITE_BUSY` path at no cost at this size. Reads have their own pool,
  so WAL lets a `/stats` scan or a slow dashboard run against the last committed
  snapshot instead of queueing behind a write.
- **Every outbound URL is untrusted** — feed addresses, the pages they are
  discovered on, and instance hostnames. `internal/safehttp` validates the
  resolved IP inside the dialer, so a name that resolves publicly at check time
  and privately at connect time is still refused.
- **Identity is `(instance host, account id)`.** A hostile instance can report
  any username, so usernames are display-only and the host always comes from the
  server's record of the sign-in flow, never from a callback parameter.
- **One OAuth client per instance, one live token per account.** The client is
  cached against the callback it was registered with, so changing `FR_BASE_URL`
  re-registers instead of failing at the instance forever. Deleting an account
  revokes its token.
- **Secrets are encrypted per column** with AES-GCM under purpose-bound keys
  derived from `FR_SECRET_KEY`, so a ciphertext cannot be moved between columns.
- **Retries are idempotent.** Posts carry an idempotency key derived from the
  delivery, so a retry that actually arrived does not post twice.

On abuse: anyone can run a Mastodon server, so nothing an instance reports about
an account is worth checking. What is left is limiting what the service will
*do* — no field anywhere names a delivery address, one feed and one destination
per account, five entries per poll, six attempts per delivery, and posting only
with the permission an abuser's own instance granted over their own account.
Signup volume is logged rather than rate limited per instance, because a busy
afternoon from a large server is indistinguishable from a farm until you know
which it was; `FR_BLOCKED_INSTANCES` and `FR_MAX_ACCOUNTS` are the levers, and
neither affects existing accounts signing in.

## Development

```
task check      # format, vet, test
task test
task db         # sqlite shell
task db:feeds   # poll state
task db:queue   # delivery queue
task db:due     # force a poll on the next tick
task db:reset   # delete the dev database
task vuln       # govulncheck
```

`.claude/agents/security-engineer.md` defines a review agent scoped to this
codebase's threat model. Issues and pull requests are welcome; run `task check`
before opening one.

## License

MIT. See [LICENSE](LICENSE).
