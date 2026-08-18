package worker

import (
	"math/rand/v2"
	"time"
)

// Scheduling policy.
//
// During the beta every feed is checked on one flat interval, the configured
// `FR_MIN_POLL_INTERVAL`. There is no widening while a feed is quiet, no slower
// lane for a feed that delivers nowhere, and no growing delay after a failure:
// a feed is checked every fifteen minutes until it is removed or paused.
//
// This costs more requests than an adaptive schedule, which is the trade being
// made while the service is small enough for the bill not to matter. Two things
// still hold the rate down, and neither is a ramp: conditional requests, so an
// unchanged feed usually costs a 304 with no body, and per-host spacing, so
// several accounts on one popular domain do not arrive together.
const (
	// maxServerHint caps how long a server can push us out with Retry-After.
	// Honour the request, but do not let one header park a feed for a week.
	maxServerHint = 24 * time.Hour

	// jitterFraction spreads scheduled times so feeds do not converge into
	// synchronised bursts after a restart or a long outage.
	jitterFraction = 0.15

	// deadFeedAfter is how long a feed may fail without a break before it is
	// paused. On a flat schedule the failure count is just elapsed time over
	// the interval, so the rule is written as the time it stands for rather
	// than as a count that would mean something different at another interval.
	deadFeedAfter = 14 * 24 * time.Hour
)

// retryDelay decides how long to wait after a failed fetch. It is the same flat
// interval as a success, unless the server asked for more room: a 429 or a 503
// with Retry-After is a demand from someone else's server, not a policy of
// ours, and ignoring it is how a service gets blocked outright.
func retryDelay(base, retryAfter time.Duration) time.Duration {
	if retryAfter > base {
		return min(retryAfter, maxServerHint)
	}
	return base
}

// deadFeed reports whether a feed has been failing long enough to give up on.
// A feed this far gone is not having a bad afternoon.
func deadFeed(base time.Duration, failures int) bool {
	return base > 0 && time.Duration(failures)*base >= deadFeedAfter
}

// withJitter spreads a duration by ±jitterFraction.
//
// Without it, every feed added on the same afternoon is fetched in the same
// second forever, and a restart re-synchronises the whole set into one spike.
func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := float64(d) * jitterFraction
	offset := (rand.Float64()*2 - 1) * spread
	out := time.Duration(float64(d) + offset)
	if out < time.Minute {
		out = time.Minute
	}
	return out
}
