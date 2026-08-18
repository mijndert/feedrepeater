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

// Repeated failures no longer slow the schedule down, so the give-up rule has
// to hold at roughly the elapsed time it was written for rather than at a
// count that a flat schedule reaches in an afternoon.
func TestDeadFeedIsJudgedOnElapsedTime(t *testing.T) {
	afternoon := int(5 * time.Hour / base)
	if deadFeed(base, afternoon) {
		t.Errorf("a feed failing for five hours (%d attempts) was given up on", afternoon)
	}
	week := int(7 * 24 * time.Hour / base)
	if deadFeed(base, week) {
		t.Errorf("a feed failing for a week (%d attempts) was given up on", week)
	}
	fortnight := int(deadFeedAfter / base)
	if !deadFeed(base, fortnight) {
		t.Errorf("a feed failing for %s (%d attempts) was still being asked", deadFeedAfter, fortnight)
	}
	// The rule follows the configured interval rather than a fixed count.
	if !deadFeed(time.Hour, int(deadFeedAfter/time.Hour)) {
		t.Error("the give-up rule ignores the configured interval")
	}
	if deadFeed(0, 1_000_000) {
		t.Error("an unconfigured interval gives up immediately")
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
	if got := withJitter(30 * time.Second); got < time.Minute {
		t.Errorf("jitter produced %s, below the one minute floor", got)
	}
}
