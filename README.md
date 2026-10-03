# feedrepeater

Posts RSS and Atom entries to Mastodon, Bluesky, Discord, Slack, ntfy, linkding
and webhooks.

Sign in with a Mastodon account on any instance. Add up to five feeds, each by
its own address or by the address of the site that links to it, and connect up
to ten destinations: the account you signed in with is the first, and further
Mastodon accounts, channels and services are added from the dashboard. Each
feed chooses which destinations it posts to. New entries are posted; entries
already in a feed when you add it are not.

Single Go binary, SQLite, no JavaScript.

## Running it

Requires Go 1.26 and [go-task](https://taskfile.dev).

```
task dev
```

That writes a `.env` with a generated key on first run and serves
<http://localhost:8080>. `task` on its own lists everything else, and
`task dev:insecure` drops the guard on private addresses so feeds and services
on your own machine work.

## Configuration

| Variable | Required | Default | |
|---|---|---|---|
| `FR_BASE_URL` | yes | | Public origin. The OAuth redirect URI derives from it, so changing it invalidates registered apps. |
| `FR_SECRET_KEY` | yes | | 32 bytes of hex, from `feedrepeater genkey`. Every stored credential is encrypted under a key derived from it. |
| `FR_ADDR` | | `127.0.0.1:8080` | Listen address. |
| `FR_DB_PATH` | | `feedrepeater.db` | SQLite file. |
| `FR_POLL_INTERVAL` | | `15m` | How often every feed is fetched. Flat; see [Polling](#polling). `FR_MIN_POLL_INTERVAL` is still read as an alias. |
| `FR_MAX_ITEMS_PER_POLL` | | `5` | Entries delivered per poll. A larger burst is recorded but not posted, so a feed that renumbers its ids cannot flood a timeline. |
| `FR_ALLOW_PRIVATE_NETWORKS` | | | `1` disables the private-address guard. Development only. |
| `FR_MAX_ACCOUNTS` | | unlimited | Stop accepting new accounts past this many. Existing accounts always sign in. |
| `FR_BLOCKED_INSTANCES` | | | Comma-separated hosts refused at sign-in and when connecting a further account. |
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

**Feeds and destinations are separate lists, joined by routing.** An account
follows up to five feeds and holds up to ten destinations, of any mix. The
`feed_destinations` table says which feed posts where, per account. A new feed
starts connected to every destination the account has and a new destination to
every feed, so the common case needs no decisions; each feed's own page has a
checkbox per destination to narrow it. Both limits are counts read inside the
transaction that inserts, on the single writer connection, so two tabs cannot
race past them.

**The account you signed in with is the first destination.** It is created at
sign-in, and again when a feed is added if that never worked. The destination
holds its own copy of the account's token, sealed under its own key. Each
sign-in issues a new token, re-seals the destination against it and revokes
the old one, which makes signing out and back in the repair for a destination
whose token an instance has revoked. Accounts are matched on `(instance host,
account id)`; a handle is display text the instance can change.

**Further Mastodon accounts connect through the same OAuth flow.** Choosing
Mastodon under Connect asks for an instance and sends the browser there to
authorise, exactly as sign-in does. The flow is recorded as a *connect* for the
account that started it, alongside the host, so the callback never learns
either from a parameter — and it refuses to finish under any other session,
revoking the token it was just handed. The authorised account becomes a
destination with its own visibility and post text; connecting an account that
is already connected renews its token and retires the previous one. Removing
the destination revokes its token at the instance. The token the account itself
signed in with is never revoked by a destination action, since the destination
is remade from it the next time a feed is added.

**Post text** is a template over a fixed set of placeholders:

```
{{title}} {{url}} {{summary}} {{author}} {{feed_title}} {{published}}
```

Substitution is a single literal pass, so a value that looks like a placeholder
is printed rather than expanded. Text is trimmed to each service's post length
— asked of a Mastodon instance rather than assumed, since it is not 500
everywhere — by shortening the summary first and then the title, never the URL.
`{{published}}` renders in the account's timezone from Settings, defaulting to
UTC; the binary embeds the IANA database. Settings also holds a default post
text, copied into a destination at creation rather than referenced.

**Adding a feed** accepts the feed address or the site address. A document that
parses as a feed is used as one; one that does not is read as HTML for its
`<link rel="alternate">` tags, and the first linked feed that fetches is stored.
Discovery costs one request per candidate, capped at three.

**Failed deliveries** can be requeued from the dashboard. The retry goes back to
the worker rather than being sent from the request, with its attempt count reset
so a delivery that spent all six is not abandoned again on the first try.

## Destinations

Every destination that takes an address from a user has to prove the address
belongs to whoever typed it. Without that, this service is a way to aim
signed-in traffic at a stranger's server — which is why the connectors were
once removed, and what each one does about it now.

**Mastodon** posts as an account authorised through OAuth, the one signed in
with or one connected afterwards. Nothing is typed but an instance name, and the
instance itself decides whether the person holds the account.

**Bluesky** needs a handle and an [app
password](https://bsky.app/settings/app-passwords) — not the account password.
Connecting opens a session to prove the pair works. Links become rich-text
facets so they are clickable.

**Discord** and **Slack** take a channel webhook URL. That URL is the entire
authorisation, so it is stored encrypted, kept out of the config blob, never
rendered back, and its host is pinned. Discord describes a webhook on `GET`, so
connecting proves the token without posting; Slack has no such call, so
connecting posts one line to the channel. Discord posts carry
`allowed_mentions: {parse: []}`, so a feed title containing `@everyone` is text
rather than a klaxon, and its `Retry-After` on a 429 is obeyed when it is longer
than the delivery backoff, capped at an hour.

**ntfy** pushes a notification to a topic, titled with the feed and opening the
entry when tapped. The server defaults to `ntfy.sh` and can be your own, so
connecting asks the address for `/v1/health` and requires ntfy's own answer, then
publishes one notification because ntfy has no way to say whether a topic and
token will work without using them. Changing the topic, server or token proves
the new one the same way; changing the template or priority does not.

**linkding** saves each entry as a bookmark in a [linkding](https://linkding.link)
of your own: the entry's link and title are their own fields, the post text is
the description, and tags and the unread flag are set per destination. The
address and token are proved together by reading `/api/user/profile/`, which
answers only for a token the server recognises and creates nothing.

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

The secret is shown once when the destination is created. Before it is, the
address is sent `{"event":"verification","challenge":"…"}` and has to echo the
challenge back; changing the address re-verifies.

## Polling

Every feed is fetched every `FR_POLL_INTERVAL`, fifteen minutes by default,
whether or not it has published lately. A ladder that widened the interval
while a feed stayed quiet was here and was removed: the one promise the
dashboard makes about the schedule is now the whole schedule.

What holds the request rate down instead: conditional requests
(ETag/If-Modified-Since), body hashing for the many servers that support
neither and return identical bytes with a 200, per-host spacing of 5 seconds,
±15% jitter so feeds added the same afternoon do not stay synchronised, and
`Retry-After` and `Cache-Control: max-age` honoured up to 24 hours — neither
can pull an interval in, only push it out. After two weeks of unbroken failures
a feed is stopped and the dashboard says why.

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
  discovered on, instance hostnames, webhook URLs, ntfy and linkding servers,
  Bluesky PDS endpoints. `internal/safehttp` validates the resolved IP inside
  the dialer, so a name that resolves publicly at check time and privately at
  connect time is still refused.
- **Identity is `(instance host, account id)`.** A hostile instance can report
  any username, so usernames are display-only and the host always comes from the
  server's record of the sign-in flow, never from a callback parameter. The same
  record says what a flow was for and, for a connect, whose it is.
- **One OAuth client per instance.** The client is cached against the callback
  it was registered with, so changing `FR_BASE_URL` re-registers instead of
  failing at the instance forever. Deleting an account revokes every token it
  holds.
- **Secrets are encrypted per column** with AES-GCM under purpose-bound keys
  derived from `FR_SECRET_KEY`, so a ciphertext cannot be moved between columns.
- **Retries are idempotent where the service allows it.** Mastodon posts carry an
  idempotency key derived from the delivery; linkding keys a bookmark on its URL.
  Discord and Slack offer neither, so a retry after a timeout that actually
  arrived can post twice; retries only happen when no 2xx was seen.

On abuse: anyone can run a Mastodon server, so nothing an instance reports about
an account is worth checking. What is left is limiting what the service will
*do* — every address proves itself before it is stored, five feeds and ten
destinations per account, five entries per poll, six attempts per delivery, and
posting only with the permission an account's own instance granted. Signup
volume is logged rather than rate limited per instance, because a busy
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
