package worker

import (
	"math/rand/v2"
	"time"
)

// Scheduling policy.
//
// A feed is asked as often as it has recently earned. The floor is the
// configured FR_MIN_POLL_INTERVAL and every feed starts there; a feed that goes
// on producing nothing climbs away from it, and the first new entry drops it
// straight back down. Nothing here ramps on failure — a broken feed is retried
// on the same schedule a working one would be, because a server having a bad
// afternoon is not a reason to stop reading a blog for a day.
//
// The flat interval this replaced was honest about its own cost: a blog that
// posts twice a year was fetched ninety-six times a day, thirty-five thousand
// times a year, to be told nothing had happened. Two things already held the
// rate down and both still apply — conditional requests, so an unchanged feed
// usually costs a 304 with no body, and per-host spacing, so several accounts on
// one popular domain do not arrive together. What is new is that the interval
// itself responds to the feed, and that a feed twenty accounts follow is one
// fetch rather than twenty.
const (
	// maxServerHint caps how long a server can push us out, whether it asked
	// with Retry-After or with Cache-Control. Honour the request, but do not let
	// one header park a feed for a week.
	maxServerHint = 24 * time.Hour

	// jitterFraction spreads scheduled times so feeds do not converge into
	// synchronised bursts after a restart or a long outage.
	jitterFraction = 0.15

	// deadFeedAfter is how long a feed may fail without a break before it is
	// stopped. Expressed as elapsed time rather than a count of failures,
	// because the interval a count stands for is no longer fixed.
	deadFeedAfter = 14 * 24 * time.Hour
)

// The ladder, in multiples of the configured floor. At the default fifteen
// minutes that reads 15m, 30m, 2h, 6h.
//
// Multiples rather than absolute durations so an operator who sets a one-minute
// interval for development gets a one-minute service, not one that quietly
// decides half an hour is close enough.
var ladder = []struct {
	quietFor time.Duration
	multiple int
}{
	{24 * time.Hour, 1},
	{7 * 24 * time.Hour, 2},
	{30 * 24 * time.Hour, 8},
	{0, 24}, // anything quieter still
}

// routelessMultiple is the least a feed is slowed by when nothing is listening.
//
// A feed whose subscribers have all unticked their destinations is recorded but
// delivered nowhere, so the only thing a prompt fetch buys is a fresher number
// on a dashboard nobody is looking at. It is still fetched, because the entries
// it records are what the account will start from when it does connect
// something.
const routelessMultiple = 4

// pollInterval decides how long to wait before asking a feed again.
func pollInterval(base, quiet time.Duration, hasRoutes bool, serverHint time.Duration) time.Duration {
	if base <= 0 {
		return base
	}

	multiple := ladder[len(ladder)-1].multiple
	for _, step := range ladder {
		if quiet < step.quietFor {
			multiple = step.multiple
			break
		}
	}
	d := base * time.Duration(multiple)

	if !hasRoutes {
		d = max(d, base*routelessMultiple)
	}

	// A server that says it caches for an hour is telling us not to ask again
	// for an hour, and it is a cheaper thing to obey than a 429 later. It only
	// ever pushes the interval out: a five-minute max-age does not entitle
	// anyone to be polled every five minutes.
	if serverHint > d {
		d = min(serverHint, maxServerHint)
	}
	return d
}

// quietFor reports how long a feed has gone without producing an entry.
//
// A feed that has never produced one is measured from when it was added, so a
// feed added this morning is treated as fresh rather than as silent since the
// epoch — which is the difference between checking it every fifteen minutes and
// checking it twice a day.
func quietFor(now time.Time, changedAt *time.Time, createdAt time.Time) time.Duration {
	since := createdAt
	if changedAt != nil && changedAt.After(since) {
		since = *changedAt
	}
	if d := now.Sub(since); d > 0 {
		return d
	}
	return 0
}

// retryDelay decides how long to wait after a failed fetch. It is the interval
// the feed had earned anyway, unless the server asked for more room: a 429 or a
// 503 with Retry-After is a demand from someone else's server, not a policy of
// ours, and ignoring it is how a service gets blocked outright.
func retryDelay(base, retryAfter time.Duration) time.Duration {
	if retryAfter > base {
		return min(retryAfter, maxServerHint)
	}
	return base
}

// deadFeed reports whether a feed has been failing long enough to give up on.
//
// It reads elapsed time off the start of the failure streak. Inferring it
// instead — multiplying the failure count by the current interval — is wrong the
// moment the interval can vary, because the failures were banked at whatever
// the feed was being polled at when they happened, and a feed climbing the
// ladder mid-outage has its whole history repriced at the new tier. That is not
// a rounding error: at the default floor it fires at seven days rather than
// fourteen, and it fires soonest for exactly the ordinary feeds the rule is
// least meant to catch.
func deadFeed(now time.Time, failingSince *time.Time) bool {
	return failingSince != nil && now.Sub(*failingSince) >= deadFeedAfter
}

// withJitter spreads a duration by ±jitterFraction.
//
// Without it, every feed added on the same afternoon is fetched in the same
// second forever, and a restart re-synchronises the whole set into one spike.
//
// The floor is proportional. A flat one-minute floor silently swallowed every
// duration below it — a thirty-second deferral came back as exactly one minute
// with no spread at all, which is the convergence this exists to prevent, and
// it clipped the lower half of the range at a one-minute poll interval too.
func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := float64(d) * jitterFraction
	offset := (rand.Float64()*2 - 1) * spread
	out := time.Duration(float64(d) + offset)
	if out < d/2 {
		out = d / 2
	}
	return out
}
