package worker

import (
	"testing"
	"time"
)

const base = 15 * time.Minute

// The schedule is flat: a server asking for room gets it, within reason, and
// nothing else moves the next attempt.
func TestRetryDelayIsFlatUnlessTheServerAsks(t *testing.T) {
	if got := retryDelay(base, 0); got != base {
		t.Errorf("plain failure = %s, want %s", got, base)
	}
	// A Retry-After shorter than the interval must not speed us up.
	if got := retryDelay(base, time.Minute); got != base {
		t.Errorf("short Retry-After sped up polling: %s", got)
	}
	if got := retryDelay(base, 2*time.Hour); got != 2*time.Hour {
		t.Errorf("Retry-After of 2h produced %s", got)
	}
	// An outrageous one is clamped rather than obeyed.
	if got := retryDelay(base, 30*24*time.Hour); got != maxServerHint {
		t.Errorf("Retry-After of 30d produced %s, want %s", got, maxServerHint)
	}
}

// The give-up rule is about elapsed time, read off the start of the streak.
func TestDeadFeedIsJudgedOnElapsedTime(t *testing.T) {
	now := time.Now().UTC()

	if deadFeed(now, nil) {
		t.Error("a feed that is not failing was given up on")
	}

	afternoon := now.Add(-5 * time.Hour)
	if deadFeed(now, &afternoon) {
		t.Error("a feed failing for five hours was given up on")
	}
	week := now.Add(-7 * 24 * time.Hour)
	if deadFeed(now, &week) {
		t.Error("a feed failing for a week was given up on")
	}
	fortnight := now.Add(-deadFeedAfter)
	if !deadFeed(now, &fortnight) {
		t.Errorf("a feed failing for %s was still being asked", deadFeedAfter)
	}
}

// Walking the real loop — interval, failure, clock forward — the rule fires
// after a fortnight at whatever interval the operator set, give or take one
// attempt.
func TestDeadFeedHoldsAtAnyInterval(t *testing.T) {
	for _, base := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour} {
		now := time.Now().UTC()
		var failingSince *time.Time
		elapsed := time.Duration(0)

		var died bool
		for range 100_000 {
			if failingSince == nil {
				at := now
				failingSince = &at
			}
			if deadFeed(now, failingSince) {
				died = true
				break
			}
			step := pollInterval(base, 0)
			now = now.Add(step)
			elapsed += step
		}
		if !died {
			t.Errorf("base %s: feed never gave up", base)
			continue
		}
		if elapsed < deadFeedAfter || elapsed > deadFeedAfter+base {
			t.Errorf("base %s: gave up after %s, want about %s", base, elapsed, deadFeedAfter)
		}
	}
}

// Jitter has to actually vary, stay inside its band, and never produce a
// nonsensical delay.
func TestJitter(t *testing.T) {
	d := time.Hour
	low := time.Duration(float64(d)*(1-jitterFraction)) - time.Second
	high := time.Duration(float64(d)*(1+jitterFraction)) + time.Second

	seen := map[time.Duration]bool{}
	for range 200 {
		got := withJitter(d)
		if got < low || got > high {
			t.Fatalf("jittered %s outside the ±%.0f%% band", got, jitterFraction*100)
		}
		seen[got] = true
	}
	if len(seen) < 50 {
		t.Errorf("jitter produced only %d distinct values; feeds would stay synchronised", len(seen))
	}
	// Small durations have to keep their spread. A flat one-minute floor
	// clamped every one of them to exactly a minute, which is the synchronised
	// convergence jitter exists to prevent — and it silently turned the
	// thirty-second host deferral into a sixty-second one with no variation at
	// all, so a whole deferred batch came back with identical times.
	small := map[time.Duration]bool{}
	for range 200 {
		got := withJitter(30 * time.Second)
		if got < 15*time.Second || got > 35*time.Second {
			t.Fatalf("jittered 30s to %s, outside a sensible band", got)
		}
		small[got] = true
	}
	if len(small) < 50 {
		t.Errorf("jittering 30s produced only %d distinct values", len(small))
	}
	if got := withJitter(0); got != 0 {
		t.Errorf("jittered zero to %s", got)
	}
}

// The interval is flat. A feed that has not posted in months is asked exactly
// as often as one that posted an hour ago, and an operator's one-minute setting
// is a one-minute service.
func TestPollIntervalIsFlat(t *testing.T) {
	for _, b := range []time.Duration{time.Minute, 15 * time.Minute, time.Hour} {
		if got := pollInterval(b, 0); got != b {
			t.Errorf("pollInterval(%s) = %s, want the interval itself", b, got)
		}
	}
	if got := pollInterval(0, time.Hour); got != 0 {
		t.Errorf("a zero interval produced %s", got)
	}
}

// A server that says it caches for an hour is telling us not to ask for an
// hour. It can push the interval out and must never pull it in.
func TestPollIntervalHonoursCacheHints(t *testing.T) {
	if got := pollInterval(base, time.Hour); got != time.Hour {
		t.Errorf("hint of 1h = %s, want 1h", got)
	}
	if got := pollInterval(base, time.Minute); got != base {
		t.Errorf("a short hint shortened the interval to %s", got)
	}
	if got := pollInterval(base, 30*24*time.Hour); got != maxServerHint {
		t.Errorf("an outlandish hint = %s, want the cap %s", got, maxServerHint)
	}
}
