# feedrepeater

Posts RSS and Atom entries to Mastodon.

Sign in with a Mastodon account on any instance. Add one feed, by its own
address or by the address of the site, whose page is read for the feed it links
to. New entries are posted to the account you signed in with; entries already in
the feed when you add it are not. There is nothing to connect: the account is
the destination.

Single Go binary, SQLite, no JavaScript.

## Running it

Requires Go 1.26 and [go-task](https://taskfile.dev).

```
task dev
```

That writes a `.env` with a generated key on first run and starts the server on
<http://localhost:8080>. `task` on its own lists everything else.

To point at feeds on your own machine, use `task dev:insecure` — it disables the
guard that blocks requests to private addresses.

## Configuration

| Variable | Required | Default | |
|---|---|---|---|
| `FR_BASE_URL` | yes | | Public origin. The OAuth redirect URI is derived from it, so changing it invalidates registered apps. |
| `FR_SECRET_KEY` | yes | | 32 bytes of hex. Generate with `feedrepeater genkey`. Every stored credential is encrypted under a key derived from it. |
| `FR_ADDR` | | `127.0.0.1:8080` | Listen address. |
| `FR_DB_PATH` | | `feedrepeater.db` | SQLite file. |
| `FR_MIN_POLL_INTERVAL` | | `15m` | The floor on how often a feed is fetched. A feed that keeps producing entries is checked this often; one that goes quiet is checked less. See [Polling](#polling). |
| `FR_MAX_ITEMS_PER_POLL` | | `5` | Entries delivered per poll. A larger burst is recorded but not posted, so a feed that renumbers its ids cannot flood a timeline. |
| `FR_ALLOW_PRIVATE_NETWORKS` | | | Set to `1` to disable the private-address guard. Development only. |
| `FR_MAX_ACCOUNTS` | | unlimited | Stop accepting new accounts past this many. Existing accounts always sign in. |
| `FR_BLOCKED_INSTANCES` | | | Comma-separated hosts refused at sign-in. |

**Losing `FR_SECRET_KEY` means losing every stored credential.** The database is
useless without it. Keep it somewhere other than the backups.

## Deploying

Caddy on the host, feedrepeater in Docker, Litestream streaming the database to
object storage.

Create `deploy/.env`, then `task docker:up`:

```
FR_BASE_URL=https://feedrepeater.com
FR_SECRET_KEY=<feedrepeater genkey>
LITESTREAM_REPLICA_URL=s3://your-bucket/feedrepeater
LITESTREAM_ACCESS_KEY_ID=...
LITESTREAM_SECRET_ACCESS_KEY=...
```

The container binds to `127.0.0.1:8080`; `deploy/Caddyfile` terminates TLS and
proxies to it. Point the site block at your domain.

On start the entrypoint restores from the replica if the local database is
missing, then runs the binary under `litestream replicate`. `task
litestream:restore` pulls a copy out for inspection.

## Where entries go

There is one destination and nobody chooses it: the Mastodon account used to
sign in. It is created on the way in — at sign-in, and again when a feed is
added if that never worked — so a dashboard with a feed on it is always a
dashboard that posts somewhere. There is no connect step, no list of services,
and no ticking which of them a feed publishes to.

**Mastodon** posts as that account, using the permission granted at sign-in.
Visibility is configurable, along with the post text, under Post settings.

The token the destination posts with is a copy of the account's own, sealed
under its own key. Every sign-in issues a new token and revokes the one it
replaces, so the destination is re-sealed against the new one at the same
moment: signing out and back in is the repair for a destination whose token an
instance has revoked. The account is matched on its instance host, not on its
handle, because a handle is display text the instance can change and a rename
must not quietly stop that repair.

The link between a feed and its destination still lives in its own
`feed_destinations` row rather than being implied by ownership, written inside
the same transaction as the subscription. Nothing edits it. It is what makes
more than one feed per account, or a second service, a change of code rather
than a change of shape.

### The other services

Bluesky, Discord, Slack, ntfy, linkding and signed webhooks were all here, and
were removed in favour of doing one thing. Roughly 3,300 lines went with them,
along with the whole class of problem they carried: every one of them took an
address or a credential from a user, and each needed its own proof that the
address belonged to whoever typed it — an echoed challenge, a pinned host, a
health probe, a profile read — because without it this service is a way to aim
signed-in traffic at a stranger's server.

Nothing that takes an address from a user is left. The remaining untrusted URLs
are feed addresses and instance hostnames, which `internal/safehttp` checks.
Migration `008_mastodon_only.sql` deletes rows of every other kind, and their
credentials with them. The code is in the history; the `kind` column and its
unique index stay behind as the seam a second service comes back through.

## Post text

The post text is a template with a fixed set of placeholders:

```
{{title}} {{url}} {{summary}} {{author}} {{feed_title}} {{published}}
```

Substitution is a single literal pass — a value that looks like a placeholder is
printed, not expanded. Text is trimmed to the instance's own post length —
asked for rather than assumed, since it is not 500 everywhere — by shortening
the summary first, then the title. The URL is never truncated.

`{{published}}` renders in the account's timezone, set in Settings. It is the
one placeholder a zone changes, and it changes it where a reader would notice:
an entry published at 23:00 in Amsterdam is the next day's date in UTC. An
account that has set no zone gets UTC. The binary embeds the IANA database, so a
name that the form accepted resolves the same way wherever it runs.

Settings also holds a default post text, which is what an account's destination
starts from when it is created. It is copied at creation rather than referenced,
so changing it later never rewrites what is already there.

## Abuse

Anyone can run a Mastodon server, so anyone can mint unlimited accounts to sign
in with. Nothing an instance says about an account — its age, its follower
count, whether it is flagged as a bot — is worth checking, because the same
person controls the server saying it. The controls that work are the ones on
what the service will *do*, not on who is asking.

**Nothing here takes an address to deliver to.** A destination is the account
that signed in, so there is no field in which to name somebody else's server,
and no way to have this service deliver requests to one on a schedule. That used
to be the central rule of this section — every kind that accepted an address had
to prove the address belonged to whoever typed it, by an echoed challenge, a
pinned host, a health probe or a profile read, or feedrepeater was an open relay
— and what enforces it now is that the field does not exist. Any future kind
that accepts an address inherits the old rule with it.

**Signup volume is watched, not capped.** There is deliberately no per-instance
rate limit: forty accounts from a large server in an afternoon is what a post
doing well looks like, and it is indistinguishable from a farm until you know
which one it was. Capping it would turn the best day into the worst one. A burst
is logged instead, and the answer to a real flood is `FR_BLOCKED_INSTANCES`,
which shuts out one server, or `FR_MAX_ACCOUNTS`, which closes the door
entirely. Neither affects existing accounts signing in.

**The blast radius is already small.** One feed per account, one destination,
at most five entries delivered per poll, six delivery attempts before a delivery
is abandoned, and the poll schedule below. Posting uses the permission the
abuser's own instance granted over their own account, so the worst case is
spamming their own timeline with our user agent attached.

If abuse becomes real rather than theoretical, the next step is invite codes —
one setting and one column, and the only thing that actually gates who gets in.

## Adding a feed

The address can be the feed or the site. A document that parses as a feed is
used as one; a document that does not is read as HTML for its
`<link rel="alternate">` tags, and the first linked feed that fetches is the one
stored. The page is fetched once either way, so a feed address costs exactly one
request and discovery costs one more per candidate, capped at three.

A linked address is written by whoever wrote the page, so it goes through the
same guard as one typed into the form: `internal/safehttp` validates it, which
is what stops a page from pointing this service at a link-local address.

Adding a feed also makes sure the account has its destination, and does it
before the subscription is written rather than after: a feed that existed for
even a moment without one would be polled and published nowhere. An account
whose stored authorisation cannot be read is told to sign out and back in, and
the feed is not saved — a feed with nowhere to post is a schedule that produces
nothing.

## Deliveries

A failed delivery can be retried from the dashboard. It goes back in the queue
for the worker rather than being sent from the request, and its attempt count is
reset, since a delivery that spent all six would otherwise be abandoned again on
the first try. Only a failed delivery can be requeued, so pressing twice cannot
disturb one already on its way.

## Polling

A feed is asked as often as it has recently earned. `FR_MIN_POLL_INTERVAL` is
the floor and every feed starts there; a feed that goes on producing nothing
climbs away from it, and the first new entry drops it straight back down.

| Quiet for | Checked every |
|---|---|
| under a day | the floor — 15m by default |
| under a week | 2× the floor |
| under a month | 8× the floor |
| longer | 24× the floor |

The ladder is in multiples rather than absolute durations, so setting a
one-minute interval for development gets a one-minute service rather than one
that quietly decides half an hour is close enough. A feed with nothing waiting on it — every
subscriber paused, or their destination paused — is checked at 4× the floor at
best: its entries are still recorded, so resuming starts from the right place,
but nothing is waiting on a prompt answer.

**A feed is fetched once, however many accounts follow it.** Feeds are shared,
addressed by their URL rather than owned. The twentieth account to add a popular
blog costs one row — the entries are already there, and it joins at the current
watermark so none of that backlog is delivered to it. This is why `/stats`
publishes `feeds.subscriptions` alongside `feeds.total`: the gap between the two
is fetches that are no longer being made.

Adding a feed still fetches it once, even when this service already follows the
address for somebody else. Skipping that would be quicker, and the fact that it
was skipped would be visible in the response — which would let anyone with an
account submit addresses and learn which feeds this service's users read. Feed
contents are public; who reads them here is not.

Two consequences of sharing are worth knowing, since neither is visible from the
dashboard:

- **"Check now" pulls the feed forward for every subscriber**, because there is
  only one poll to pull forward. It is one fetch either way, which is why the
  button is rate limited per account.
- **A feed's error state is the feed's, not yours.** If a shared feed starts
  failing, everyone subscribed to it sees the same "Failing" line and the same
  message. Nothing per-account is in it — only what the feed's own server said.
  Pausing, by contrast, is yours alone. A feed stopped by the dead-feed rule is
  revived for everyone the next time anybody subscribes to it — nothing else
  clears it, and with the feed shared, unsubscribing does not get you a fresh
  one while another account still holds it. The reason it stopped stays on the
  dashboard until a fetch actually succeeds, so a revival does not read as a
  recovery that has not happened.

What holds the rate down beyond the schedule itself:

- **Conditional requests.** ETag and If-Modified-Since on every fetch, so a
  feed that has not changed usually costs a 304 and no body.
- **Body hashing.** Plenty of servers support neither header and answer 200 with
  identical bytes forever. The digest of the last document parsed is stored and
  compared, so that case costs the transfer and nothing else — no parse, and
  none of the several hundred no-op inserts behind it.
- **Per-host spacing.** At most one request every 5 seconds to any one host, so
  several accounts subscribing to the same popular domain do not arrive
  together. A feed whose host was just contacted is pushed out rather than
  skipped, so it does not sit at the head of the queue blocking what is behind
  it.
- **Jitter.** Every scheduled time is spread ±15%, so feeds added on the same
  afternoon do not stay synchronised, and a restart does not produce a spike.
- **The server's own wishes.** `Retry-After` on 429 and 503 is obeyed, and
  `Cache-Control: max-age` is honoured as a minimum — a server that says it
  caches for an hour is telling us not to ask again for an hour, which is a
  cheaper thing to obey than a rate limit later. Both are capped at 24 hours,
  and neither can pull an interval in, only push it out.
- **Dead feeds stop.** After two weeks of unbroken failures the feed is stopped
  and the dashboard says why. The rule is elapsed time rather than a count of
  attempts, so it means the same thing at any point on the ladder. Stopping is a
  property of the feed and applies to every subscriber; a subscriber's own pause
  is separate and affects only them.

## Design notes

**One writer, many readers.** Writes go through a single connection, which
removes every `SQLITE_BUSY` path and costs nothing at this size. Reads have
their own pool: WAL lets them run against the last committed snapshot while a
write is in flight, so a public `/stats` scan or one slow dashboard is no longer
a queue for everybody.

**Every outbound URL is untrusted.** Feed addresses, the pages they are
discovered on, and instance hostnames all come from users. `internal/safehttp`
validates the resolved IP inside the dialer, so a name that resolves to a public
address at check time and a private one at connect time is still refused.

**Identity is `(instance host, account id)`.** A hostile instance can report any
username it likes, so usernames are display-only and the host always comes from
the server's record of the sign-in flow, never from a callback parameter.

**One OAuth client per instance, one live token per account.** The client is
registered on first use and cached against the callback it was registered with,
so a change of `FR_BASE_URL` re-registers instead of failing at the instance
forever. Each sign-in issues a new access token; the destination is re-sealed
against it, then the one it replaces is revoked, and deleting an account revokes
its token too. Otherwise an instance accumulates live credentials that can post
as the user.

**Secrets are encrypted per column.** Access tokens, the destination's copy of
one, and OAuth client secrets are sealed with AES-GCM under purpose-bound keys
derived from `FR_SECRET_KEY`, so a ciphertext cannot be moved between columns.

**Retries are idempotent.** Mastodon posts carry an idempotency key derived from
the delivery, so a retry that actually arrived does not post twice.

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
codebase's threat model. In Claude Code: *use the security-engineer agent to
review the changes*.

## Status

Beta. One feed per account, posting to the Mastodon account it signed in with.
No account limits beyond that, no billing.

Proprietary. All rights reserved. Not open source, not for redistribution.
