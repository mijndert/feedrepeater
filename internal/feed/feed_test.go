package feed

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const rssSample = `<?xml version="1.0"?>
<rss version="2.0"><channel>
  <title>Example Feed</title>
  <item>
    <title>Second &amp; newest</title>
    <link>/posts/two</link>
    <guid>tag:example.com,2024:2</guid>
    <description>&lt;p&gt;Body &lt;b&gt;two&lt;/b&gt;&lt;/p&gt;</description>
    <pubDate>Tue, 02 Jan 2024 10:00:00 GMT</pubDate>
  </item>
  <item>
    <title>First</title>
    <link>https://example.com/posts/one</link>
    <guid>tag:example.com,2024:1</guid>
    <description>Body one</description>
    <pubDate>Mon, 01 Jan 2024 10:00:00 GMT</pubDate>
  </item>
</channel></rss>`

func TestParseOrdersOldestFirst(t *testing.T) {
	res, err := Parse([]byte(rssSample), "https://example.com/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Example Feed" {
		t.Errorf("title = %q", res.Title)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("got %d entries", len(res.Entries))
	}
	if res.Entries[0].Title != "First" {
		t.Errorf("entries are not oldest first: %q", res.Entries[0].Title)
	}
}

func TestParseResolvesRelativeLinks(t *testing.T) {
	res, _ := Parse([]byte(rssSample), "https://example.com/feed.xml")
	var second Entry
	for _, e := range res.Entries {
		if strings.HasPrefix(e.Title, "Second") {
			second = e
		}
	}
	if second.URL != "https://example.com/posts/two" {
		t.Errorf("relative link resolved to %q", second.URL)
	}
	if second.Title != "Second & newest" {
		t.Errorf("entities not decoded: %q", second.Title)
	}
	if second.Summary != "Body two" {
		t.Errorf("summary not stripped of markup: %q", second.Summary)
	}
}

// A link with a dangerous scheme must be dropped, not carried into post text.
func TestParseRejectsNonHTTPLinks(t *testing.T) {
	doc := `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>
	<item><title>Bad</title><link>javascript:alert(1)</link><guid>x1</guid></item>
	<item><title>Also bad</title><link>data:text/html,hi</link><guid>x2</guid></item>
	</channel></rss>`
	res, err := Parse([]byte(doc), "https://example.com/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Entries {
		if e.URL != "" {
			t.Errorf("kept dangerous link %q", e.URL)
		}
	}
}

func TestParseGUIDFallsBackToContentHash(t *testing.T) {
	doc := `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>
	<item><title>No id and no link</title><description>hello</description></item>
	</channel></rss>`
	res, err := Parse([]byte(doc), "https://example.com/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("got %d entries", len(res.Entries))
	}
	if !strings.HasPrefix(res.Entries[0].GUID, "sha256:") {
		t.Errorf("guid = %q, want a content hash", res.Entries[0].GUID)
	}

	// The same document must produce the same id, or entries repost forever.
	again, _ := Parse([]byte(doc), "https://example.com/feed.xml")
	if again.Entries[0].GUID != res.Entries[0].GUID {
		t.Error("content hash is not stable across parses")
	}
}

func TestParseClampsFutureDates(t *testing.T) {
	doc := `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>
	<item><title>Far future</title><guid>f1</guid><pubDate>Wed, 01 Jan 2200 10:00:00 GMT</pubDate></item>
	</channel></rss>`
	res, _ := Parse([]byte(doc), "https://example.com/feed.xml")
	if len(res.Entries) != 1 || res.Entries[0].Published == nil {
		t.Fatal("entry missing")
	}
	if res.Entries[0].Published.Year() > 2100 {
		t.Errorf("future date not clamped: %v", res.Entries[0].Published)
	}
}

func TestParseCapsEntries(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>`)
	for i := range MaxEntries + 50 {
		b.WriteString(`<item><title>e</title><guid>g`)
		b.WriteString(strings.Repeat("0", 1))
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(string(rune('a' + i/26)))
		b.WriteString(`</guid></item>`)
	}
	b.WriteString(`</channel></rss>`)

	res, err := Parse([]byte(b.String()), "https://example.com/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) > MaxEntries {
		t.Errorf("got %d entries, cap is %d", len(res.Entries), MaxEntries)
	}
}

func TestTrimHeader(t *testing.T) {
	if got := trimHeader(`W/"abc"`, 200); got != `W/"abc"` {
		t.Errorf("got %q", got)
	}
	if got := trimHeader("bad\r\nInjected: 1", 200); got != "" {
		t.Errorf("header injection not stripped: %q", got)
	}
	if got := trimHeader(strings.Repeat("a", 500), 200); got != "" {
		t.Errorf("oversized header kept: %q", got)
	}
}

// A feed written oldest-first with more entries than the cap must lose its
// oldest, not its newest. Truncating the document before sorting dropped
// whichever end the publisher put last — and with the body-hash short-circuit
// an unchanged feed is never re-parsed, so that loss no longer heals.
func TestParseKeepsTheNewestWhenCapping(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<rss><channel><title>Long</title>`)
	for i := range MaxEntries + 50 {
		fmt.Fprintf(&b, `<item><guid>g%03d</guid><title>Entry %03d</title>
			<pubDate>%s</pubDate></item>`,
			i, i, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).
				Add(time.Duration(i)*time.Hour).Format(time.RFC1123Z))
	}
	b.WriteString(`</channel></rss>`)

	res, err := Parse([]byte(b.String()), "https://example.com/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != MaxEntries {
		t.Fatalf("kept %d entries, want %d", len(res.Entries), MaxEntries)
	}
	// Oldest first, so the last one is the newest the document carried.
	newest := res.Entries[len(res.Entries)-1]
	if want := fmt.Sprintf("g%03d", MaxEntries+49); newest.GUID != want {
		t.Errorf("newest kept entry is %s, want %s", newest.GUID, want)
	}
	oldest := res.Entries[0]
	if want := fmt.Sprintf("g%03d", 50); oldest.GUID != want {
		t.Errorf("oldest kept entry is %s, want %s", oldest.GUID, want)
	}
}
