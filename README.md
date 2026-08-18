# feedrepeater

Posts RSS and Atom entries to Mastodon, Bluesky, Discord, Slack, and webhooks.

Sign in with a Mastodon account on any instance. Add one feed, by its own
address or by the address of the site, whose page is read for the feed it links
to. New entries go out automatically; entries already in the feed when you add
it do not.

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
| `FR_MIN_POLL_INTERVAL` | | `15m` | How often every feed is fetched. Flat during the beta. |
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

## Routing

A feed publishes to the destinations ticked against it on the dashboard. Adding
a feed connects it to every destination you already have, and a new destination
connects to every feed you already have, so the common case needs no decisions;
untick to narrow it.

The link lives in its own `feed_destinations` table rather than being implied by
ownership, which is what makes more than one feed per account a schema-free
change later. The beta limits are two unique constraints and nothing else: one
on `feeds.user_id`, one on `destinations (user_id, kind)`.

## Destinations

**Mastodon** posts as the account you signed in with, using the permission
granted at sign-in. Visibility is configurable per destination.

**Bluesky** needs a handle and an [app
password](https://bsky.app/settings/app-passwords) — not your account password.
Links become rich-text facets so they are clickable.

**Discord** and **Slack** take a channel webhook URL. That URL is the entire
authorisation: anyone holding it can post to the channel, with no account and no
second check. So it is stored encrypted, kept out of the config blob, never
rendered back, and its host is pinned. Without the pin these would be webhooks
with none of the challenge the webhook kind demands, which is to say a way to
aim signed-in traffic at any server. Discord describes a webhook on `GET`, so
connecting proves the token without posting; Slack has no such call, so
connecting posts one line to the channel. Discord posts carry
`allowed_mentions: {parse: []}`, so a feed title containing `@everyone` is text
rather than a klaxon, and its `Retry-After` on a 429 is obeyed when it is longer
than the delivery backoff, capped at an hour. Neither service offers an
idempotency key, so a retry after a timeout that actually arrived can post
twice; retries only happen when no 2xx was seen.

**Webhook** sends a JSON `POST` with an HMAC-SHA256 signature:

```
X-Feedrepeater-Timestamp: 1700000000
X-Feedrepeater-Signature: sha256=<hex>
X-Feedrepeater-Delivery: <stable id, repeated on retries>
```

The signature covers `timestamp + "." + body`. Verify it before trusting the
payload, and reject timestamps that are not recent:

```go
mac := hmac.New(sha256.New, []byte(secret))
mac.Write([]byte(timestamp)); mac.Write([]byte(".")); mac.Write(body)
ok := hmac.Equal(mac.Sum(nil), sig)
```

The secret is shown once when the destination is created. `task db:queue` shows
what has been sent.

## Post text

Each destination has a template with a fixed set of placeholders:

```
{{title}} {{url}} {{summary}} {{author}} {{feed_title}} {{published}}
```

Substitution is a single literal pass — a value that looks like a placeholder is
printed, not expanded. Text is trimmed to each service's limit by shortening the
summary first, then the title. The URL is never truncated.

## Abuse

Anyone can run a Mastodon server, so anyone can mint unlimited accounts to sign
in with. Nothing an instance says about an account — its age, its follower
count, whether it is flagged as a bot — is worth checking, because the same
person controls the server saying it. The controls that work are the ones on
what the service will *do*, not on who is asking.

**Webhooks must prove themselves.** When a webhook is added, the address is
sent `{"event":"verification","challenge":"…"}` and has to echo the challenge
back before the destination is stored. Changing the address re-verifies. Without
this, feedrepeater is an open relay: sign up, point a webhook at someone else's
server, and we deliver requests there on a schedule. The SSRF guard keeps those
requests on the public internet, which protects this network and nobody else's.

**Signup volume is watched, not capped.** There is deliberately no per-instance
rate limit: forty accounts from a large server in an afternoon is what a post
doing well looks like, and it is indistinguishable from a farm until you know
which one it was. Capping it would turn the best day into the worst one. A burst
is logged instead, and the answer to a real flood is `FR_BLOCKED_INSTANCES`,
which shuts out one server, or `FR_MAX_ACCOUNTS`, which closes the door
entirely. Neither affects existing accounts signing in.

**The blast radius is already small.** One feed per account, one destination per
service, at most five entries delivered per poll, six delivery attempts before a
delivery is abandoned, and the poll schedule below. Posting to Mastodon and
Bluesky requires credentials for accounts the abuser must already control, so
the worst case there is spamming their own timeline with our user agent
attached.

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

## Deliveries

A failed delivery can be retried from the dashboard. It goes back in the queue
for the worker rather than being sent from the request, and its attempt count is
reset, since a delivery that spent all six would otherwise be abandoned again on
the first try. Only a failed delivery can be requeued, so pressing twice cannot
disturb one already on its way.

## Polling

Every feed is fetched on one flat interval, `FR_MIN_POLL_INTERVAL`, whatever it
has been doing. There is no widening while a feed is quiet, no slower lane for a
feed that delivers nowhere, and no growing delay after a failure. At 15m that is
96 requests a day per feed, which is the trade being made while the service is
small: predictable behaviour now, an adaptive schedule when the bill argues for
one.

What holds the rate down instead, none of it a ramp:

- **Conditional requests.** ETag and If-Modified-Since on every fetch, so a
  feed that has not changed usually costs a 304 and no body.
- **Per-host spacing.** At most one request every 5 seconds to any one host, so
  several accounts subscribing to the same popular domain do not arrive together.
- **Jitter.** Every scheduled time is spread ±15%, so feeds added on the same
  afternoon do not stay synchronised, and a restart does not produce a spike.
- **The server's own wishes.** `Retry-After` on 429 and 503 is obeyed, capped at
  24 hours. This is the one thing that can push a feed past the interval: it is
  a demand from someone else's server, and ignoring it is how a service gets
  blocked outright. `Cache-Control: max-age` is no longer treated as a minimum,
  because on a flat schedule it would be the ramp coming back through the door.
- **Dead feeds stop.** After two weeks of unbroken failures the feed is paused
  and the dashboard says why. The rule is elapsed time rather than a count of
  attempts, so it means the same thing at any interval.

## Design notes

**One database connection.** The pool is capped at one, which removes every
`SQLITE_BUSY` path. It serialises requests, which is the right trade until it
isn't.

**Every outbound URL is untrusted.** Feed addresses, webhook endpoints, instance
hostnames, and Bluesky PDS endpoints all come from users. `internal/safehttp`
validates the resolved IP inside the dialer, so a name that resolves to a public
address at check time and a private one at connect time is still refused.

**Identity is `(instance host, account id)`.** A hostile instance can report any
username it likes, so usernames are display-only and the host always comes from
the server's record of the sign-in flow, never from a callback parameter.

**One OAuth client per instance, one live token per account.** The client is
registered on first use and cached against the callback it was registered with,
so a change of `FR_BASE_URL` re-registers instead of failing at the instance
forever. Each sign-in issues a new access token; the one it replaces is revoked
once the new one is stored, and deleting an account revokes its token too.
Otherwise an instance accumulates live credentials that can post as the user.

**Secrets are encrypted per column.** Access tokens, app passwords, OAuth client
secrets, and webhook keys are sealed with AES-GCM under purpose-bound keys
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

Beta. One feed per account. No account limits beyond that, no billing.

Proprietary. All rights reserved. Not open source, not for redistribution.
