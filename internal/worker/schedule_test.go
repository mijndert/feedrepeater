package worker

import (
	"testing"
	"time"
)

const base = 15 * time.Minute

// The beta schedule is flat: a server asking for room gets it, within reason,
// and nothing else moves the next attempt.
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

// The give-up rule is about elapsed time, and it has to stay about elapsed time
// now that the interval between attempts varies. Deriving it from the failure
// count instead — count times the current interval — reprices failures banked at
// a shorter interval, and fires at half the intended window for any feed that
// climbed the ladder during its outage.
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

// The rule has to mean the same span of time however the feed was being polled
// while it broke. This walks the real loop — interval, then failure, then the
// clock forward — for a feed that goes down the day after it last posted, which
// is the ordinary case and the one an inferred-from-count rule got most wrong.
func TestDeadFeedHoldsAcrossTheLadder(t *testing.T) {
	for _, base := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour} {
		created := time.Now().UTC()
		now := created
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
			step := pollInterval(base, quietFor(now, nil, created), true, 0)
			now = now.Add(step)
			elapsed += step
		}
		if !died {
			t.Errorf("base %s: feed never gave up", base)
			continue
		}
		// One poll interval of slack: the check happens on an attempt, and the
		// attempts are as far apart as the ladder has decided by then.
		if elapsed < deadFeedAfter || elapsed > deadFeedAfter+24*time.Hour {
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

// The ladder is what stops a dormant blog being asked ninety-six times a day to
// say nothing. It is expressed in multiples of the configured floor, so the
// same policy holds whatever the operator set.
func TestPollIntervalWidensWhileAFeedIsQuiet(t *testing.T) {
	const base = 15 * time.Minute
	for _, tc := range []struct {
		name  string
		quiet time.Duration
		want  time.Duration
	}{
		{"just posted", time.Minute, base},
		{"quiet most of a day", 20 * time.Hour, base},
		{"quiet two days", 48 * time.Hour, 30 * time.Minute},
		{"quiet a fortnight", 14 * 24 * time.Hour, 2 * time.Hour},
		{"quiet for months", 200 * 24 * time.Hour, 6 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pollInterval(base, tc.quiet, true, 0); got != tc.want {
				t.Errorf("pollInterval(quiet %s) = %s, want %s", tc.quiet, got, tc.want)
			}
		})
	}
}

// A one-minute development interval must stay a one-minute service rather than
// being rounded up to whatever the ladder's absolute durations would have been.
func TestPollIntervalScalesWithTheConfiguredFloor(t *testing.T) {
	if got := pollInterval(time.Minute, time.Hour, true, 0); got != time.Minute {
		t.Errorf("fresh feed at a 1m floor = %s, want 1m", got)
	}
	if got := pollInterval(time.Minute, 48*time.Hour, true, 0); got != 2*time.Minute {
		t.Errorf("quiet feed at a 1m floor = %s, want 2m", got)
	}
}

// Nothing is listening, so there is nothing waiting on a prompt answer.
func TestPollIntervalSlowsAFeedNobodyRoutes(t *testing.T) {
	const base = 15 * time.Minute
	got := pollInterval(base, time.Minute, false, 0)
	if want := base * routelessMultiple; got != want {
		t.Errorf("unrouted feed = %s, want %s", got, want)
	}
	// Never faster than the ladder already decided.
	if got := pollInterval(base, 200*24*time.Hour, false, 0); got != 6*time.Hour {
		t.Errorf("unrouted dormant feed = %s, want 6h", got)
	}
}

// A server that says it caches for an hour is telling us not to ask for an
// hour. It can push the interval out and must never pull it in.
func TestPollIntervalHonoursCacheHints(t *testing.T) {
	const base = 15 * time.Minute
	if got := pollInterval(base, 0, true, time.Hour); got != time.Hour {
		t.Errorf("hint of 1h = %s, want 1h", got)
	}
	if got := pollInterval(base, 0, true, time.Minute); got != base {
		t.Errorf("a short hint shortened the interval to %s", got)
	}
	if got := pollInterval(base, 0, true, 30*24*time.Hour); got != maxServerHint {
		t.Errorf("an outlandish hint = %s, want the cap %s", got, maxServerHint)
	}
}

// A feed added this morning has never produced an entry, and must not be read
// as one that has been silent since the epoch.
func TestQuietForMeasuresFromCreationUntilSomethingHappens(t *testing.T) {
	now := time.Now().UTC()
	created := now.Add(-2 * time.Hour)
	if got := quietFor(now, nil, created); got != 2*time.Hour {
		t.Errorf("never-changed feed = %s, want 2h", got)
	}
	changed := now.Add(-30 * time.Minute)
	if got := quietFor(now, &changed, created); got != 30*time.Minute {
		t.Errorf("changed feed = %s, want 30m", got)
	}
}
