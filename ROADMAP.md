# Roadmap

Considered and deliberately not built yet. Ordered within each group by what
would be picked up first.

## The one real unlock

**More than one feed per account.** The limit is a single `UNIQUE` constraint on
`feeds.user_id`; `feed_destinations` was built for many feeds already, so the
schema work is done. The cost is interface: the dashboard becomes a list, and
routing becomes feeds against services rather than a column of ticks.

## Changes what the product can do

**Filters.** Post only entries matching or excluding keywords, an author, or a
category. The most-asked-for feature in anything that reposts a feed, and
entirely internal: no third party is involved.

**Images.** Attach an entry's enclosure or `og:image` to Mastodon and Bluesky
posts, which is most of what makes a posted entry look like a person wrote it.
Real work: fetching, size and type limits, alt text, and one more untrusted URL,
though that shape is already covered by `internal/safehttp`.

**Digest mode.** One post a day or a week covering several entries, for feeds
that publish in bursts. Fits the existing template system.

**Threading rather than truncating.** A long entry becomes a short thread on
Mastodon and Bluesky instead of being cut. Mastodon needs a reply id, Bluesky
needs root and parent refs; both are within reach of the existing senders.

## Cheap, because the parts exist

**Tell people when something breaks.** A feed paused after two weeks of
failures, or a destination failing repeatedly, is only visible to someone who
visits. A post to the account's own destinations, or an email, closes the loop.

**Opt-in backfill.** At connect time, offer to post the most recent one to five
existing entries. Nothing is backfilled today, which is the right default but
makes the first day look broken. Cap it hard.

**Link cleaning.** Strip tracking parameters, or add a per-destination campaign
parameter. Self-contained, roughly fifty lines.

## Operational, better before growth than after

**Invite codes.** Named in the README as the next abuse control: one setting and
one column, and the only thing that actually gates who gets in.

**Data export.** Feed, destinations, and delivery history as JSON. Cheap, and it
makes "your content stays yours" in the terms concrete.

**A restore drill.** `task litestream:restore` exists; nothing exercises it on a
schedule. A backup nobody has restored is a hypothesis.

## Looked at and declined

**X, Threads, LinkedIn, Facebook, Instagram.** App review, per-app rate limits,
terms that restrict automated posting, and in X's case a paid tier. Weeks of
process rather than days of code.

**More than one destination per service.** The dashboard is a row per service
and the database enforces one of each. Wanting two Slack channels is a real
thing to want, but it undoes that shape.
