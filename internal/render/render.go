// Package render turns a feed entry and a user template into post text.
//
// The template language is deliberately inert: a fixed set of {{name}}
// placeholders replaced by literal values, in one pass. There is no
// expression evaluation, no field access, and no way for a substituted value
// to be re-read as a placeholder, so feed content cannot reach a template
// engine and there is nothing to sandbox.
package render

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Vars are the values available to a template.
type Vars struct {
	Title     string
	URL       string
	Summary   string
	Author    string
	FeedTitle string
	Published time.Time
}

// Variable documents one placeholder for the UI.
type Variable struct {
	Name string
	Desc string
}

// Variables is the complete, closed set of placeholders.
var Variables = []Variable{
	{"title", "Entry title"},
	{"url", "Entry link"},
	{"summary", "Entry summary, HTML removed"},
	{"author", "Entry author"},
	{"feed_title", "Feed title"},
	{"published", "Publication date, YYYY-MM-DD"},
}

// DefaultTemplate is what a new destination starts with.
const DefaultTemplate = "{{title}}\n\n{{url}}"

var placeholder = regexp.MustCompile(`\{\{\s*([a-z_]{1,24})\s*\}\}`)

// ValidateTemplate reports the first unknown placeholder in a template.
func ValidateTemplate(tmpl string) error {
	if strings.TrimSpace(tmpl) == "" {
		return &TemplateError{Msg: "template must not be empty"}
	}
	if utf8.RuneCountInString(tmpl) > 2000 {
		return &TemplateError{Msg: "template is too long"}
	}
	for _, m := range placeholder.FindAllStringSubmatch(tmpl, -1) {
		if !known(m[1]) {
			return &TemplateError{Msg: "unknown placeholder {{" + m[1] + "}}"}
		}
	}
	return nil
}

type TemplateError struct{ Msg string }

func (e *TemplateError) Error() string { return e.Msg }

func known(name string) bool {
	for _, v := range Variables {
		if v.Name == name {
			return true
		}
	}
	return false
}

// Render substitutes the variables and fits the result within limit runes.
//
// A limit of zero means unlimited. When the text is too long, the summary is
// shortened first, then the title; the URL is never touched, because a
// truncated link is worse than no summary.
func Render(tmpl string, v Vars, limit int) string {
	values := v.values()
	out := substitute(tmpl, values)
	if limit <= 0 || utf8.RuneCountInString(out) <= limit {
		return tidy(out)
	}

	for _, name := range []string{"summary", "title", "author"} {
		cur := values[name]
		if cur == "" {
			continue
		}
		over := utf8.RuneCountInString(out) - limit
		// How many times this variable appears decides how much each cut helps.
		uses := strings.Count(substitute(tmpl, marker(values, name)), "\x00")
		if uses == 0 {
			continue
		}
		perUse := (over + uses - 1) / uses
		target := utf8.RuneCountInString(cur) - perUse - 1 // -1 leaves room for the ellipsis
		if target < 0 {
			target = 0
		}
		values[name] = truncate(cur, target)
		out = substitute(tmpl, values)
		if utf8.RuneCountInString(out) <= limit {
			break
		}
	}

	out = tidy(out)
	if utf8.RuneCountInString(out) > limit {
		// Everything shrinkable is gone and the literal text alone still does not
		// fit; cut the whole thing rather than fail the delivery.
		out = truncate(out, limit-1)
	}
	return out
}

func (v Vars) values() map[string]string {
	published := ""
	if !v.Published.IsZero() {
		published = v.Published.UTC().Format("2006-01-02")
	}
	return map[string]string{
		"title":      v.Title,
		"url":        v.URL,
		"summary":    v.Summary,
		"author":     v.Author,
		"feed_title": v.FeedTitle,
		"published":  published,
	}
}

// marker returns the value set with one variable replaced by a sentinel, used
// to count how often that variable appears in the rendered output.
func marker(values map[string]string, name string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	out[name] = "\x00"
	return out
}

// substitute performs a single left-to-right pass. Because replacement values
// are never rescanned, a title containing "{{url}}" is emitted literally.
func substitute(tmpl string, values map[string]string) string {
	return placeholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		sub := placeholder.FindStringSubmatch(m)
		if v, ok := values[sub[1]]; ok {
			return v
		}
		return m
	})
}

// truncate cuts to at most n runes, appending an ellipsis when it cuts, and
// prefers to break on a word boundary.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	cut := string(runes[:n])
	if i := strings.LastIndexAny(cut, " \n\t"); i > len(cut)/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " \n\t.,;:-") + "…"
}

var (
	trailingSpace = regexp.MustCompile(`[ \t]+\n`)
	blankLines    = regexp.MustCompile(`\n{3,}`)
	repeatedSpace = regexp.MustCompile(`  +`)
)

// tidy cleans up the whitespace left behind when a variable renders empty.
func tidy(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = trailingSpace.ReplaceAllString(s, "\n")
	s = blankLines.ReplaceAllString(s, "\n\n")
	s = repeatedSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
