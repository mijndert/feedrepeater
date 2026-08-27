# Roadmap

Considered and deliberately not built yet. Ordered within each group by what
would be picked up first.

## The one real unlock

**More than one feed per account.** The limit is a single `UNIQUE` constraint on
`subscriptions.user_id`; `feed_destinations` was built for many feeds already,
so the schema work is done. The cost is interface: the dashboard becomes a list,
and each feed needs its own post text if the templates are not to collapse into
one.

## Changes what the product can do

**Filters.** Post only entries matching or excluding keywords, an author, or a
category. The most-asked-for feature in anything that reposts a feed, and
entirely internal: no third party is involved.

**Images.** Attach an entry's enclosure or `og:image` to the post, which is most
of what makes a posted entry look like a person wrote it.
Real work: fetching, size and type limits, alt text, and one more untrusted URL,
though that shape is already covered by `internal/safehttp`.

**Digest mode.** One post a day or a week covering several entries, for feeds
that publish in bursts. Fits the existing template system.

**Threading rather than truncating.** A long entry becomes a short thread
instead of being cut. Mastodon needs a reply id on each post after the first,
which is within reach of the existing sender.

## Cheap, because the parts exist

**Tell people when something breaks.** A feed stopped after two weeks of
failures, or a token the instance has revoked, is only visible to someone who
visits. A direct message to the account, from the account, closes the loop
without needing an address anybody has to give us.

**Opt-in backfill.** When a feed is added, offer to post the most recent one to
five existing entries. Nothing is backfilled today, which is the right default but
makes the first day look broken. Cap it hard.

**Link cleaning.** Strip tracking parameters, or add a campaign parameter.
Self-contained, roughly fifty lines.

## Operational, better before growth than after

**Invite codes.** Named in the README as the next abuse control: one setting and
one column, and the only thing that actually gates who gets in.

**Data export.** Feed, post settings, and delivery history as JSON. Cheap, and
it makes "your content stays yours" in the terms concrete.

**A restore drill.** `task litestream:restore` exists; nothing exercises it on a
schedule. A backup nobody has restored is a hypothesis.

## Looked at and declined

**X, Threads, LinkedIn, Facebook, Instagram.** App review, per-app rate limits,
terms that restrict automated posting, and in X's case a paid tier. Weeks of
process rather than days of code.

**The other services, again.** Bluesky, Discord, Slack, ntfy, linkding and
signed webhooks were built, shipped, and removed; the code is in the history and
the `kind` column is still there to hang one on. What made them expensive was
not the sending. It was that each took an address or a credential from a user
and so needed its own proof that the address belonged to whoever typed it. Any
of them coming back brings that back with it, and with it the question this
service currently does not have to answer: what happens when somebody points us
at a server that never agreed to hear from us.
