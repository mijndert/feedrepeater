package render

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// StripHTML reduces feed markup to plain text.
//
// Feed content is attacker-controlled and gets posted under the user's name, so
// it is stripped rather than escaped: script and style contents are dropped
// entirely, block elements become line breaks, and control characters that
// could hide text or reorder it visually are removed.
func StripHTML(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	z := html.NewTokenizer(strings.NewReader(s))
	skipDepth := 0

	for {
		switch z.Next() {
		case html.ErrorToken:
			return clean(b.String())

		case html.TextToken:
			if skipDepth == 0 {
				b.Write(z.Text())
			}

		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if skipDepth > 0 {
				if isSkipped(tag) {
					skipDepth++
				}
				continue
			}
			if isSkipped(tag) {
				skipDepth = 1
				continue
			}
			if isBlock(tag) {
				b.WriteString("\n")
			}

		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if skipDepth > 0 {
				if isSkipped(tag) {
					skipDepth--
				}
				continue
			}
			if isBlock(tag) {
				b.WriteString("\n")
			}
		}
	}
}

func isSkipped(tag string) bool {
	switch tag {
	case "script", "style", "head", "template", "noscript", "iframe", "object", "svg":
		return true
	}
	return false
}

func isBlock(tag string) bool {
	switch tag {
	case "p", "div", "br", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6",
		"blockquote", "pre", "section", "article", "ul", "ol", "table":
		return true
	}
	return false
}

// clean removes invisible and direction-altering characters, then normalises
// whitespace.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case r == 0xFEFF, r == 0x200B, r == 0x200C, r == 0x200D, r == 0x2060:
			return -1 // zero-width
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return -1 // bidi overrides and isolates
		case r == 0xAD:
			return -1 // soft hyphen
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(repeatedSpace.ReplaceAllString(s, " "))
}
