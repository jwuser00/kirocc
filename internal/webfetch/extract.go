package webfetch

import (
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
)

// minMainContentRunes is the threshold below which an <article>/<main> subtree
// is considered boilerplate (a teaser, a wrapper) and extraction falls back to
// the whole <body>.
const minMainContentRunes = 200

// skippedElements never contribute text: code, styling, chrome, and widgets.
// Navigation and footer elements are dropped because search-result answers
// need the article, not the site menu around it.
var skippedElements = map[atom.Atom]struct{}{
	atom.Script:   {},
	atom.Style:    {},
	atom.Noscript: {},
	atom.Template: {},
	atom.Iframe:   {},
	atom.Svg:      {},
	atom.Canvas:   {},
	atom.Object:   {},
	atom.Nav:      {},
	atom.Header:   {},
	atom.Footer:   {},
	atom.Aside:    {},
	atom.Form:     {},
	atom.Button:   {},
	atom.Select:   {},
	atom.Dialog:   {},
}

// blockElements force a line break around their content so extracted text
// keeps paragraph structure instead of collapsing into one line.
var blockElements = map[atom.Atom]struct{}{
	atom.P:          {},
	atom.Div:        {},
	atom.Section:    {},
	atom.Article:    {},
	atom.Main:       {},
	atom.Li:         {},
	atom.Ul:         {},
	atom.Ol:         {},
	atom.Dl:         {},
	atom.Dt:         {},
	atom.Dd:         {},
	atom.Table:      {},
	atom.Tr:         {},
	atom.H1:         {},
	atom.H2:         {},
	atom.H3:         {},
	atom.H4:         {},
	atom.H5:         {},
	atom.H6:         {},
	atom.Blockquote: {},
	atom.Pre:        {},
	atom.Br:         {},
	atom.Figure:     {},
	atom.Figcaption: {},
}

// ExtractText parses HTML from r (decoding by contentType charset) and returns
// the page title and readable text. It prefers an <article>/<main>/[role=main]
// subtree when one carries enough text, falling back to <body>.
func ExtractText(r io.Reader, contentType string) (title, text string, err error) {
	decoded, err := charset.NewReader(r, contentType)
	if err != nil {
		// Undecodable charset: read as-is rather than failing the page.
		decoded = r
	}
	doc, err := html.Parse(decoded)
	if err != nil {
		return "", "", err
	}

	title = findTitle(doc)

	if main := findMainContent(doc); main != nil {
		text = collectText(main)
		if len([]rune(text)) >= minMainContentRunes {
			return title, text, nil
		}
	}
	if body := findElement(doc, atom.Body); body != nil {
		return title, collectText(body), nil
	}
	return title, collectText(doc), nil
}

// findTitle returns the text of the first <title> element.
func findTitle(doc *html.Node) string {
	t := findElement(doc, atom.Title)
	if t == nil {
		return ""
	}
	var sb strings.Builder
	for c := t.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
		}
	}
	return strings.TrimSpace(sb.String())
}

// findMainContent locates the main content subtree: <article>, then <main>,
// then any element with role="main".
func findMainContent(doc *html.Node) *html.Node {
	if n := findElement(doc, atom.Article); n != nil {
		return n
	}
	if n := findElement(doc, atom.Main); n != nil {
		return n
	}
	return findFunc(doc, func(n *html.Node) bool {
		for _, a := range n.Attr {
			if a.Key == "role" && a.Val == "main" {
				return true
			}
		}
		return false
	})
}

func findElement(doc *html.Node, a atom.Atom) *html.Node {
	return findFunc(doc, func(n *html.Node) bool { return n.DataAtom == a })
}

// findFunc returns the first element node (document order) matching pred.
func findFunc(n *html.Node, pred func(*html.Node) bool) *html.Node {
	if n.Type == html.ElementNode && pred(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findFunc(c, pred); found != nil {
			return found
		}
	}
	return nil
}

// collectText walks the subtree and produces whitespace-collapsed text with
// paragraph structure preserved via newlines.
func collectText(root *html.Node) string {
	var sb strings.Builder
	walkText(root, &sb)
	return collapseWhitespace(sb.String())
}

func walkText(n *html.Node, sb *strings.Builder) {
	if n.Type == html.ElementNode {
		if _, skip := skippedElements[n.DataAtom]; skip {
			return
		}
	}
	if n.Type == html.TextNode {
		sb.WriteString(n.Data)
		return
	}
	_, block := blockElements[n.DataAtom]
	if block {
		sb.WriteByte('\n')
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkText(c, sb)
	}
	if block {
		sb.WriteByte('\n')
	}
}

// collapseWhitespace normalizes runs of spaces/tabs to one space and runs of
// newlines to at most two, trimming each line.
func collapseWhitespace(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	newlines := 2 // treat start-of-text as after a break so leading blanks drop
	space := false
	flushWord := func(r rune) {
		if newlines > 0 {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
				if newlines > 1 {
					sb.WriteByte('\n')
				}
			}
		} else if space {
			sb.WriteByte(' ')
		}
		newlines = 0
		space = false
		sb.WriteRune(r)
	}
	for _, r := range s {
		switch r {
		case '\n', '\r':
			newlines++
		case ' ', '\t', '\f', '\v', ' ':
			space = true
		default:
			flushWord(r)
		}
	}
	return sb.String()
}

// Truncate clamps s to at most maxBytes bytes at a rune boundary, appending a
// truncation marker when content was dropped.
func Truncate(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n[content truncated]"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
