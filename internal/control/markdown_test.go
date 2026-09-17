package control

import (
	"strings"
	"testing"
)

// TestRenderMarkdownEscapesEverythingAndKeepsSafeLinks: block and inline
// forms render; raw HTML in the source is text on the page; a
// javascript: link is text; a code span is not interpreted.
func TestRenderMarkdownEscapesEverythingAndKeepsSafeLinks(t *testing.T) {
	src := "# Title <b>x</b>\n\nA *para* with `code *not em*` and [link](https://example.com) and [bad](javascript:alert(1)).\n\n- one\n- two\n  continued\n\n1. first\n2. second\n\n```go\nfmt.Println(\"<hi>\")\n```\n\n> quoted **bold**\n\n| a | b |\n|---|---|\n| 1 | <i>2</i> |\n\n---\n\n<script>alert(1)</script>\n"
	out := renderMarkdown(src)
	for _, want := range []string{
		"<h1>Title &lt;b&gt;x&lt;/b&gt;</h1>", "<em>para</em>", "<code>code *not em*</code>",
		`<a href="https://example.com" rel="noopener noreferrer">link</a>`, " and bad).", "<ul>", "<li>two continued</li>", "<ol>", "<li>second</li>",
		`<pre><code class="language-go">fmt.Println(&#34;&lt;hi&gt;&#34;)</code></pre>`, "<blockquote><p>quoted <strong>bold</strong></p>", "<th>a</th>", "<td>&lt;i&gt;2&lt;/i&gt;</td>", "<hr>",
		"&lt;script&gt;alert(1)&lt;/script&gt;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %s in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"<script>", "javascript:", "<b>x</b>", "<i>2</i>"} {
		if strings.Contains(out, bad) {
			t.Errorf("passes %q through:\n%s", bad, out)
		}
	}
	if img := renderMarkdown("![alt](https://x/y.png) ![no](data:text/html,x)"); !strings.Contains(img, `<img alt="alt" src="https://x/y.png">`) || strings.Contains(img, "data:") {
		t.Errorf("images: %s", img)
	}
	if auto := renderMarkdown("see https://a.example/p?q=1 now"); !strings.Contains(auto, `href="https://a.example/p?q=1"`) {
		t.Errorf("autolink: %s", auto)
	}
}
