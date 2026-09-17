package control

import (
	"html"
	"regexp"
	"strings"
)

// A small Markdown renderer for the render page (docs/agent-first-design.md
// §8.1, T-55): headings, paragraphs, bulleted and numbered lists, fenced
// and indented code, block quotes, horizontal rules, tables, and inline
// code, emphasis, links and images. It never passes markup through —
// every character of the source is HTML-escaped before it is placed, raw
// HTML in the source shows as text, and a link or image URL is kept only
// when its scheme is http, https, mailto or a relative path — so the
// page is safe to render for a file an agent or a stranger wrote, without
// a sanitizer pass whose gaps would be the risk. What it does not do
// (nested lists beyond one level, footnotes, HTML entities) renders as
// plain text, which is the honest failure.

var (
	mdInlineCode = regexp.MustCompile("`([^`]+)`")
	mdImage      = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	mdLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	mdStrong     = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
	mdEm         = regexp.MustCompile(`\*([^*\n]+)\*|\b_([^_\n]+)_\b`)
	mdStrike     = regexp.MustCompile(`~~([^~]+)~~`)
	mdAutoLink   = regexp.MustCompile(`(?:^|[\s(])(https?://[^\s<>()]+)`)
	mdHeading    = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	mdBullet     = regexp.MustCompile(`^\s{0,3}[-*+]\s+(.*)$`)
	mdNumbered   = regexp.MustCompile(`^\s{0,3}\d{1,9}[.)]\s+(.*)$`)
	mdTableSep   = regexp.MustCompile(`^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?\s*$`)
)

// safeURL keeps a URL a page may follow and drops the rest ("" means the
// link is rendered as text).
func safeURL(u string) string {
	u = strings.TrimSpace(u)
	lower := strings.ToLower(u)
	switch {
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "mailto:"):
		return u
	case strings.HasPrefix(lower, "#"), strings.HasPrefix(lower, "/"), strings.HasPrefix(lower, "./"), strings.HasPrefix(lower, "../"):
		return u
	case strings.Contains(lower, ":"):
		return ""
	}
	return u
}

// mdInline renders one line's inline markup over escaped text. Code
// spans are lifted out first so nothing inside them is interpreted.
func mdInline(s string) string {
	var codes []string
	s = mdInlineCode.ReplaceAllStringFunc(s, func(m string) string {
		codes = append(codes, "<code>"+html.EscapeString(m[1:len(m)-1])+"</code>")
		return "\x00" + string(rune('0'+len(codes)-1)) + "\x00"
	})
	s = html.EscapeString(s)
	s = mdImage.ReplaceAllStringFunc(s, func(m string) string {
		sub := mdImage.FindStringSubmatch(m)
		if u := safeURL(html.UnescapeString(sub[2])); u != "" {
			return `<img alt="` + sub[1] + `" src="` + html.EscapeString(u) + `">`
		}
		return sub[1]
	})
	s = mdLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := mdLink.FindStringSubmatch(m)
		if u := safeURL(html.UnescapeString(sub[2])); u != "" {
			return `<a href="` + html.EscapeString(u) + `" rel="noopener noreferrer">` + sub[1] + `</a>`
		}
		return sub[1]
	})
	s = mdAutoLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := mdAutoLink.FindStringSubmatch(m)
		lead := m[:len(m)-len(sub[1])]
		return lead + `<a href="` + sub[1] + `" rel="noopener noreferrer">` + sub[1] + `</a>`
	})
	s = mdStrong.ReplaceAllString(s, "<strong>$1$2</strong>")
	s = mdEm.ReplaceAllString(s, "<em>$1$2</em>")
	s = mdStrike.ReplaceAllString(s, "<del>$1</del>")
	for i, c := range codes {
		s = strings.Replace(s, "\x00"+string(rune('0'+i))+"\x00", c, 1)
	}
	return s
}

// renderMarkdown renders a document to an HTML fragment.
func renderMarkdown(src string) string {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var b strings.Builder
	var para []string
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + mdInline(strings.Join(para, " ")) + "</p>\n")
			para = para[:0]
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			flush()
			fence := trimmed[:3]
			lang := strings.TrimSpace(strings.TrimPrefix(trimmed, fence))
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fence); i++ {
				code = append(code, lines[i])
			}
			cls := ""
			if lang != "" {
				cls = ` class="language-` + html.EscapeString(strings.Fields(lang)[0]) + `"`
			}
			b.WriteString("<pre><code" + cls + ">" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
		case trimmed == "":
			flush()
		case mdHeading.MatchString(trimmed):
			flush()
			sub := mdHeading.FindStringSubmatch(trimmed)
			level := len(sub[1])
			b.WriteString("<h" + string(rune('0'+level)) + ">" + mdInline(sub[2]) + "</h" + string(rune('0'+level)) + ">\n")
		case isRule(trimmed):
			flush()
			b.WriteString("<hr>\n")
		case strings.HasPrefix(trimmed, ">"):
			flush()
			var quote []string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">"); i++ {
				quote = append(quote, strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">"), " "))
			}
			i--
			b.WriteString("<blockquote>" + renderMarkdown(strings.Join(quote, "\n")) + "</blockquote>\n")
		case strings.HasPrefix(trimmed, "|") && i+1 < len(lines) && mdTableSep.MatchString(lines[i+1]):
			flush()
			cells := func(l string) []string {
				l = strings.TrimSpace(l)
				l = strings.TrimPrefix(strings.TrimSuffix(l, "|"), "|")
				out := strings.Split(l, "|")
				for j := range out {
					out[j] = strings.TrimSpace(out[j])
				}
				return out
			}
			b.WriteString("<table><thead><tr>")
			for _, c := range cells(line) {
				b.WriteString("<th>" + mdInline(c) + "</th>")
			}
			b.WriteString("</tr></thead><tbody>\n")
			for i += 2; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				b.WriteString("<tr>")
				for _, c := range cells(lines[i]) {
					b.WriteString("<td>" + mdInline(c) + "</td>")
				}
				b.WriteString("</tr>\n")
			}
			i--
			b.WriteString("</tbody></table>\n")
		case mdBullet.MatchString(line) || mdNumbered.MatchString(line):
			flush()
			ordered := mdNumbered.MatchString(line)
			tag := "ul"
			if ordered {
				tag = "ol"
			}
			b.WriteString("<" + tag + ">\n")
			for ; i < len(lines); i++ {
				var item string
				if ordered && mdNumbered.MatchString(lines[i]) {
					item = mdNumbered.FindStringSubmatch(lines[i])[1]
				} else if !ordered && mdBullet.MatchString(lines[i]) {
					item = mdBullet.FindStringSubmatch(lines[i])[1]
				} else {
					break
				}
				// A continuation line indented under the item joins it.
				for i+1 < len(lines) && strings.HasPrefix(lines[i+1], "  ") && strings.TrimSpace(lines[i+1]) != "" && !mdBullet.MatchString(lines[i+1]) && !mdNumbered.MatchString(lines[i+1]) {
					i++
					item += " " + strings.TrimSpace(lines[i])
				}
				b.WriteString("<li>" + mdInline(item) + "</li>\n")
			}
			i--
			b.WriteString("</" + tag + ">\n")
		case strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t"):
			flush()
			var code []string
			for ; i < len(lines) && (strings.HasPrefix(lines[i], "    ") || strings.HasPrefix(lines[i], "\t") || strings.TrimSpace(lines[i]) == ""); i++ {
				code = append(code, strings.TrimPrefix(strings.TrimPrefix(lines[i], "    "), "\t"))
			}
			i--
			for len(code) > 0 && strings.TrimSpace(code[len(code)-1]) == "" {
				code = code[:len(code)-1]
			}
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
		default:
			para = append(para, trimmed)
		}
	}
	flush()
	return b.String()
}

// isRule says a line is a horizontal rule: three or more of one of - * _
// with nothing but spaces between.
func isRule(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	return strings.Trim(s, string(c)) == ""
}
