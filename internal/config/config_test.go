package config

import (
	"strings"
	"testing"
	"time"
)

// required sets the two variables without which Load refuses to start, so each
// test below is about the one thing it varies.
func required(t *testing.T) {
	t.Helper()
	t.Setenv("FR_BASE_URL", "https://fr.test")
	t.Setenv("FR_SECRET_KEY", strings.Repeat("ab", 32))
	// Clear both spellings so whatever the developer's shell has set does not
	// leak into the case being tested.
	t.Setenv("FR_POLL_INTERVAL", "")
	t.Setenv("FR_MIN_POLL_INTERVAL", "")
}

// The schedule is flat and the interval is the one number on the dashboard that
// describes it, so the default has to be the quarter hour the page promises.
func TestPollIntervalDefaultsToFifteenMinutes(t *testing.T) {
	required(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != 15*time.Minute {
		t.Errorf("PollInterval = %s, want 15m", c.PollInterval)
	}
}

func TestPollIntervalIsReadFromTheEnvironment(t *testing.T) {
	required(t)
	t.Setenv("FR_POLL_INTERVAL", "5m")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != 5*time.Minute {
		t.Errorf("PollInterval = %s, want 5m", c.PollInterval)
	}
}

// The variable was FR_MIN_POLL_INTERVAL while the interval was a floor. A
// deployment that set it must keep the interval it set across the rename,
// rather than silently reverting to the default.
func TestOldPollIntervalNameIsStillHonoured(t *testing.T) {
	required(t)
	t.Setenv("FR_MIN_POLL_INTERVAL", "30m")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != 30*time.Minute {
		t.Errorf("PollInterval = %s, want 30m from the old name", c.PollInterval)
	}
}

// When both are set the new name wins, whichever order the environment lists
// them in. Otherwise an operator who adds FR_POLL_INTERVAL without removing the
// old line would see no change and have nothing to explain it.
func TestNewPollIntervalNameWinsOverTheOld(t *testing.T) {
	required(t)
	t.Setenv("FR_MIN_POLL_INTERVAL", "30m")
	t.Setenv("FR_POLL_INTERVAL", "2m")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != 2*time.Minute {
		t.Errorf("PollInterval = %s, want FR_POLL_INTERVAL's 2m", c.PollInterval)
	}
}

// One minute is the floor under either name. Below it the service is a way to
// hammer somebody else's server, so the value is refused at startup rather than
// clamped, which an operator would never notice.
func TestPollIntervalBelowAMinuteIsRefused(t *testing.T) {
	for _, name := range []string{"FR_POLL_INTERVAL", "FR_MIN_POLL_INTERVAL"} {
		t.Run(name, func(t *testing.T) {
			required(t)
			t.Setenv(name, "30s")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("30s under %s: err = %v, want a refusal naming the variable", name, err)
			}

			// Exactly a minute is allowed: the floor is inclusive.
			t.Setenv(name, "1m")
			c, err := Load()
			if err != nil {
				t.Fatalf("1m under %s: %v", name, err)
			}
			if c.PollInterval != time.Minute {
				t.Errorf("PollInterval = %s, want 1m", c.PollInterval)
			}
		})
	}
}

// A value that is not a duration at all is a typo in a deployment file, and
// the message has to say which variable so it can be found.
func TestPollIntervalMustBeADuration(t *testing.T) {
	required(t)
	t.Setenv("FR_POLL_INTERVAL", "fifteen")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "FR_POLL_INTERVAL") {
		t.Errorf("err = %v, want a parse failure naming FR_POLL_INTERVAL", err)
	}
}
