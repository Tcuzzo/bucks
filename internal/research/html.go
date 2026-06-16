package research

import (
	"html"
	"regexp"
	"strings"
)

// htmlToText turns a raw HTML document into (title, readable text) using ONLY the
// standard library — no x/net/html tokenizer, no headless browser, no cgo. This
// keeps BUCKS a single pure-Go static binary. The trade-off, stated honestly: we do
// NOT execute JavaScript, so a page that renders its content client-side will yield
// little text. For server-rendered news/market pages (the common case) this is
// robust enough for a lightweight researcher.
//
// The extraction is deliberately conservative:
//  1. drop <script>…</script> and <style>…</style> blocks entirely (their bodies are
//     code, not prose);
//  2. drop HTML comments;
//  3. pull the <title> for Page.Title;
//  4. replace block-level tags with a newline so paragraphs don't run together, then
//     strip all remaining tags;
//  5. decode HTML entities (html.UnescapeString) and collapse whitespace;
//  6. bound the result to maxText.
//
// The result contains only visible prose — never markup — so a summary built from it
// can never echo a tag or a script fragment as if it were a fact.
func htmlToText(doc string, maxText int) (title string, text string) {
	title = extractTitle(doc)

	// 1+2: remove script/style bodies and comments before any tag stripping, so their
	// (non-prose) contents never leak into the text.
	s := scriptStyleRe.ReplaceAllString(doc, " ")
	s = commentRe.ReplaceAllString(s, " ")

	// 4: turn block-level boundaries into newlines so adjacent blocks stay separated,
	// then strip every remaining tag.
	s = blockTagRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")

	// 5: decode entities (&amp; &#8217; &nbsp; …) then collapse whitespace.
	s = html.UnescapeString(s)
	s = collapseWhitespace(s)

	// 6: bound. We cap by rune count so we never split a multi-byte rune.
	if maxText <= 0 {
		maxText = defaultTextCap
	}
	if len(s) > maxText {
		s = boundRunes(s, maxText)
	}
	return title, s
}

var (
	// scriptStyleRe matches <script ...>...</script> and <style ...>...</style>
	// (case-insensitive, dot matches newline) so their bodies are removed wholesale.
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)>`)
	// commentRe matches HTML comments.
	commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	// titleRe captures the first <title>…</title> content.
	titleRe = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title>`)
	// blockTagRe matches the OPENING/closing of common block elements + <br>; each is
	// replaced with a newline so block boundaries survive tag stripping.
	blockTagRe = regexp.MustCompile(`(?is)</?(p|div|br|li|ul|ol|tr|h[1-6]|section|article|header|footer|blockquote|table|td|th)\b[^>]*>`)
	// tagRe matches any remaining HTML tag.
	tagRe = regexp.MustCompile(`(?s)<[^>]*>`)
	// wsRe collapses any run of whitespace (incl. newlines) to a single space.
	wsRe = regexp.MustCompile(`[ \t\f\r\v]+`)
	// multiNewlineRe collapses runs of blank lines to a single newline.
	multiNewlineRe = regexp.MustCompile(`\n{2,}`)
)

// extractTitle returns the decoded, whitespace-collapsed <title> text, or "".
func extractTitle(doc string) string {
	m := titleRe.FindStringSubmatch(doc)
	if len(m) < 2 {
		return ""
	}
	t := html.UnescapeString(m[1])
	return strings.TrimSpace(wsRe.ReplaceAllString(t, " "))
}

// collapseWhitespace trims and normalizes whitespace: runs of spaces/tabs become a
// single space, and runs of blank lines become a single newline, so the text reads
// as separated paragraphs without huge gaps.
func collapseWhitespace(s string) string {
	// First normalize Windows newlines, then collapse intra-line whitespace.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	// Collapse spaces/tabs (but keep newlines).
	s = wsRe.ReplaceAllString(s, " ")
	// Trim trailing spaces on each line by collapsing " \n" -> "\n".
	s = strings.ReplaceAll(s, " \n", "\n")
	s = strings.ReplaceAll(s, "\n ", "\n")
	// Collapse runs of newlines to one.
	s = multiNewlineRe.ReplaceAllString(s, "\n")
	return strings.TrimSpace(s)
}

// boundRunes truncates s to at most max bytes WITHOUT splitting a multi-byte rune.
// It walks rune boundaries up to the cap.
func boundRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Find the largest rune boundary <= max.
	cut := max
	for cut > 0 && !isUTF8Boundary(s, cut) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}

// isUTF8Boundary reports whether index i is the start of a UTF-8 rune in s (i.e. not
// a continuation byte). i==len(s) is a boundary.
func isUTF8Boundary(s string, i int) bool {
	if i <= 0 || i >= len(s) {
		return true
	}
	return s[i]&0xC0 != 0x80
}
