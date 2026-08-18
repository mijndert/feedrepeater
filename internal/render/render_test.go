package render

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRenderSubstitutes(t *testing.T) {
	v := Vars{Title: "Hello", URL: "https://example.com/a", Author: "Ada", FeedTitle: "Blog"}
	got := Render("{{title}} by {{author}} on {{feed_title}}\n\n{{url}}", v, 0)
	want := "Hello by Ada on Blog\n\nhttps://example.com/a"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A value that itself looks like a placeholder must be emitted literally,
// which is the property that makes the template language inert.
func TestRenderDoesNotRescanValues(t *testing.T) {
	v := Vars{Title: "{{url}}", URL: "https://example.com/secret"}
	got := Render("{{title}}", v, 0)
	if got != "{{url}}" {
		t.Errorf("substituted value was rescanned: %q", got)
	}
}

func TestRenderUnknownPlaceholderLeftAlone(t *testing.T) {
	if got := Render("{{nope}} {{title}}", Vars{Title: "x"}, 0); got != "{{nope}} x" {
		t.Errorf("got %q", got)
	}
}

func TestRenderTruncatesButKeepsURL(t *testing.T) {
	long := strings.Repeat("word ", 200)
	v := Vars{Title: "A title", Summary: long, URL: "https://example.com/entry"}
	got := Render("{{title}}\n\n{{summary}}\n\n{{url}}", v, 300)

	if n := utf8.RuneCountInString(got); n > 300 {
		t.Errorf("rendered %d runes, limit 300", n)
	}
	if !strings.Contains(got, "https://example.com/entry") {
		t.Errorf("URL was truncated away: %q", got)
	}
	if !strings.Contains(got, "A title") {
		t.Errorf("title was dropped before the summary: %q", got)
	}
}

func TestRenderTruncatesTitleWhenSummaryIsNotEnough(t *testing.T) {
	v := Vars{Title: strings.Repeat("t", 500), URL: "https://example.com/x"}
	got := Render("{{title}} {{url}}", v, 100)
	if n := utf8.RuneCountInString(got); n > 100 {
		t.Errorf("rendered %d runes, limit 100", n)
	}
	if !strings.Contains(got, "https://example.com/x") {
		t.Errorf("URL lost: %q", got)
	}
}

// Truncation must not split a multi-byte character.
func TestRenderTruncationKeepsValidUTF8(t *testing.T) {
	v := Vars{Title: strings.Repeat("héllo wörld ", 60), URL: "https://example.com"}
	got := Render("{{title}} {{url}}", v, 120)
	if !utf8.ValidString(got) {
		t.Errorf("truncation produced invalid UTF-8: %q", got)
	}
}

func TestRenderRepeatedPlaceholder(t *testing.T) {
	v := Vars{Title: strings.Repeat("x", 400)}
	got := Render("{{title}} / {{title}}", v, 100)
	if n := utf8.RuneCountInString(got); n > 100 {
		t.Errorf("rendered %d runes, limit 100", n)
	}
}

func TestRenderTidiesEmptyValues(t *testing.T) {
	got := Render("{{title}}\n\n{{summary}}\n\n{{url}}", Vars{Title: "T", URL: "https://e.com"}, 0)
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("blank run left behind: %q", got)
	}
}

func TestValidateTemplate(t *testing.T) {
	if err := ValidateTemplate("{{title}} {{url}}"); err != nil {
		t.Errorf("valid template rejected: %v", err)
	}
	for _, bad := range []string{"", "   ", "{{secret}}", "{{ password }}", strings.Repeat("a", 2100)} {
		if err := ValidateTemplate(bad); err == nil {
			t.Errorf("ValidateTemplate(%.20q) = nil, want error", bad)
		}
	}
}

func TestStripHTML(t *testing.T) {
	cases := map[string]string{
		"<p>Hello <b>world</b></p>":           "Hello world",
		`<script>alert("x")</script>Safe`:     "Safe",
		"<style>p{color:red}</style>Text":     "Text",
		"A &amp; B &lt;tag&gt;":               "A & B <tag>",
		"<p>One</p><p>Two</p>":                "One Two",
		"plain":                               "plain",
		"<img src=x onerror=alert(1)>caption": "caption",
		"a​b":                                 "ab",
		"before‮after":                        "beforeafter",
	}
	for in, want := range cases {
		if got := StripHTML(in); got != want {
			t.Errorf("StripHTML(%q) = %q, want %q", in, got, want)
		}
	}
}

// The published date is the one placeholder a timezone changes, and it changes
// it exactly where it matters: an entry published late in the evening is the
// next day's date in UTC, so a post would name a day the reader has not reached.
func TestPublishedRendersInTheGivenZone(t *testing.T) {
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skipf("no timezone database available: %v", err)
	}
	// 22:30 UTC on the 17th is 00:30 on the 18th in Amsterdam.
	published := time.Date(2026, 8, 17, 22, 30, 0, 0, time.UTC)

	if got := Render("{{published}}", Vars{Published: published, Location: amsterdam}, 0); got != "2026-08-18" {
		t.Errorf("in Amsterdam = %q, want 2026-08-18", got)
	}
	// A nil location is UTC, which is what an account that has set none gets.
	if got := Render("{{published}}", Vars{Published: published}, 0); got != "2026-08-17" {
		t.Errorf("with no location = %q, want 2026-08-17", got)
	}
	// An entry with no date renders nothing rather than the epoch.
	if got := Render("x{{published}}", Vars{Location: amsterdam}, 0); got != "x" {
		t.Errorf("with no date = %q, want %q", got, "x")
	}
}
