package worker

import (
	"math/rand/v2"
	"time"
)

// Scheduling policy.
//
// Every feed is asked on the same flat interval, the configured
// FR_POLL_INTERVAL, whether or not it has published lately. A ladder that
// widened the interval while a feed stayed quiet was here and was removed: it
// made the schedule a function of the feed's history, which is a second thing
// to explain on a dashboard whose one promise is "checked every fifteen
// minutes", and the request savings it bought are mostly made already by
// conditional requests, body hashing and the feed being fetched once however
// many accounts follow it.
//
// Nothing here ramps on failure either — a broken feed is retried on the same
// schedule a working one would be, because a server having a bad afternoon is
// not a reason to stop reading a blog for a day. What can move the next attempt
// is the server itself: a Retry-After or a Cache-Control max-age pushes it out,
// bounded, and never pulls it in.
const (
	// maxServerHint caps how long a server can push us out, whether it asked
	// with Retry-After or with Cache-Control. Honour the request, but do not let
	// one header park a feed for a week.
	maxServerHint = 24 * time.Hour

	// jitterFraction spreads scheduled times so feeds do not converge into
	// synchronised bursts after a restart or a long outage.
	jitterFraction = 0.15

	// deadFeedAfter is how long a feed may fail without a break before it is
	// stopped. Expressed as elapsed time rather than a count of failures, so it
	// means the same thing whatever the interval is set to.
	deadFeedAfter = 14 * 24 * time.Hour
)

// pollInterval decides how long to wait before asking a feed again: the flat
// interval, unless the server asked for more room.
//
// A server that says it caches for an hour is telling us not to ask again for
// an hour, and it is a cheaper thing to obey than a 429 later. It only ever
// pushes the interval out: a five-minute max-age does not entitle anyone to be
// polled every five minutes.
func pollInterval(base, serverHint time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	if serverHint > base {
		return min(serverHint, maxServerHint)
	}
	return base
}

// retryDelay decides how long to wait after a failed fetch. It is the ordinary
// interval, unless the server asked for more room: a 429 or a 503 with
// Retry-After is a demand from someone else's server, not a policy of ours,
// and ignoring it is how a service gets blocked outright.
func retryDelay(base, retryAfter time.Duration) time.Duration {
	if retryAfter > base {
		return min(retryAfter, maxServerHint)
	}
	return base
}

// deadFeed reports whether a feed has been failing long enough to give up on.
//
// It reads elapsed time off the start of the failure streak rather than
// multiplying a failure count by the interval. The interval is flat now, so the
// two would agree, but the streak is what the rule is actually about and the
// column is already there.
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
