package index

import (
	"reflect"
	"testing"

	"cloudfs/internal/config"
)

func TestSecretsAreExcludedEvenInsideARule(t *testing.T) {
	m := NewMatcher([]Rule{{Path: "/work", Include: []string{"**/*"}, MaxFileSize: 1 << 20}}, config.DefaultIndexExclude)
	for _, p := range []string{"/work/.env", "/work/keys/server.pem", "/work/.ssh/id_rsa", "/work/.git/HEAD", "/work/node_modules/x/index.js"} {
		if _, ok := m.Match(p, 10); ok {
			t.Errorf("%s matched", p)
		}
		if !m.Excluded(p) {
			t.Errorf("%s not reported excluded", p)
		}
	}
	if _, ok := m.Match("/work/notes/a.md", 10); !ok {
		t.Error("ordinary file not matched")
	}
	// Directories under a global exclude are excluded too, so a walk can
	// prune them.
	for _, d := range []string{"/work/.git", "/work/node_modules", "/work/a/b/node_modules"} {
		if !m.Excluded(d) {
			t.Errorf("directory %s not excluded", d)
		}
	}
	if m.Excluded("/work/.github") || m.Excluded("/work/environment.md") {
		t.Error("a near miss of an exclude pattern was excluded")
	}
}

func TestRuleSizeLimitAndExactFileRule(t *testing.T) {
	m := NewMatcher([]Rule{{Path: "/work", Include: []string{"**/*.md"}, MaxFileSize: 100}, {Path: "/big/report.pdf", MaxFileSize: 1 << 30}}, nil)
	if _, ok := m.Match("/work/a.md", 101); ok {
		t.Error("oversized file matched")
	}
	if r, ok := m.Match("/work/a.md", 100); !ok || r.Path != "/work" {
		t.Errorf("a file at the size limit was refused: %+v %v", r, ok)
	}
	if r, ok := m.Match("/big/report.pdf", 5<<20); !ok || r.Path != "/big/report.pdf" {
		t.Errorf("an exact file rule must cover that file: %+v %v", r, ok)
	}
	if _, ok := m.Match("/big/report.pdf", 2<<30); ok {
		t.Error("an exact file rule ignored its size limit")
	}
	if _, ok := m.Match("/big/other.pdf", 10); ok {
		t.Error("an exact file rule covered a sibling")
	}
}

func TestDeepestRuleWins(t *testing.T) {
	m := NewMatcher([]Rule{
		{Path: "/work", Include: []string{"**/*.md"}, MaxFileSize: 1 << 20},
		{Path: "/work/code", Include: []string{"**/*.go"}, MaxFileSize: 1 << 20},
	}, nil)
	for _, tc := range []struct {
		p    string
		rule string
		ok   bool
	}{
		{"/work/code/a.go", "/work/code", true},
		{"/work/code/deep/a.go", "/work/code", true},
		{"/work/code/a.md", "", false}, // the deeper rule is the only one consulted
		{"/work/a.md", "/work", true},
		{"/work/a.go", "", false},
		{"/work/codex/a.go", "", false}, // "/work/code" is not an ancestor of "/work/codex"
		{"/other/a.md", "", false},
	} {
		r, ok := m.Match(tc.p, 10)
		if ok != tc.ok || r.Path != tc.rule {
			t.Errorf("Match(%s) = %q %v, want %q %v", tc.p, r.Path, ok, tc.rule, tc.ok)
		}
	}
	// Input order does not decide: the deeper rule wins either way.
	m = NewMatcher([]Rule{
		{Path: "/work/code", Include: []string{"**/*.go"}},
		{Path: "/work", Include: []string{"**/*.md"}},
	}, nil)
	if r, ok := m.Match("/work/code/a.go", 10); !ok || r.Path != "/work/code" {
		t.Errorf("deeper rule listed first lost: %+v %v", r, ok)
	}
	if _, ok := m.Match("/work/code/a.md", 10); ok {
		t.Error("outer rule matched a file the inner rule refused")
	}
}

func TestMatchDecidesRelativeToTheRulePath(t *testing.T) {
	m := NewMatcher([]Rule{
		{Path: "/work", Include: []string{"docs/*.md", "*.txt"}, Exclude: []string{"docs/draft-*"}},
		{Path: "/", Include: []string{"**/*.rst"}, Exclude: []string{"tmp/**"}},
	}, nil)
	for _, tc := range []struct {
		p    string
		rule string
		ok   bool
	}{
		{"/work/docs/a.md", "/work", true},
		{"/work/docs/draft-a.md", "", false},  // rule exclude
		{"/work/sub/docs/a.md", "", false},    // include anchored at the rule path
		{"/work/notes.txt", "/work", true},    // "*.txt" means directly under the rule
		{"/work/sub/notes.txt", "", false},    // not below it
		{"/work/a.rst", "", false},            // "/work" shadows "/"
		{"/tmp/a.rst", "", false},             // excluded relative to "/"
		{"/x/tmp/a.rst", "/", true},           // "tmp/**" is anchored at "/"
		{"/a.rst", "/", true},                 // the root rule
		{"/some/where/deep/a.rst", "/", true}, // and it recurses
		{"/some/where/deep/a.txt", "", false}, // but only for its own patterns
	} {
		r, ok := m.Match(tc.p, 10)
		if ok != tc.ok || r.Path != tc.rule {
			t.Errorf("Match(%s) = %q %v, want %q %v", tc.p, r.Path, ok, tc.rule, tc.ok)
		}
	}
}

func TestMatcherFillsRuleDefaults(t *testing.T) {
	// A run-time rule arrives with nothing but a path; it gets the
	// built-in include list and file size limit, like a configured rule.
	m := NewMatcher([]Rule{{Path: "/notes", Source: "ui"}}, config.DefaultIndexExclude)
	for _, p := range []string{"/notes/a.md", "/notes/b.docx", "/notes/deep/c.go", "/notes/d.pdf"} {
		if _, ok := m.Match(p, 10); !ok {
			t.Errorf("%s not covered by the default include list", p)
		}
	}
	for _, p := range []string{"/notes/a.bin", "/notes/photo.jpg", "/notes/.env"} {
		if _, ok := m.Match(p, 10); ok {
			t.Errorf("%s covered by the default include list", p)
		}
	}
	if _, ok := m.Match("/notes/a.md", DefaultMaxFileSize+1); ok {
		t.Error("a rule without max_file_size accepted a file over the default limit")
	}
	if r, ok := m.Match("/notes/a.md", DefaultMaxFileSize); !ok || r.MaxFileSize != DefaultMaxFileSize {
		t.Errorf("default limit not applied: %+v %v", r, ok)
	}
	// An explicit empty global exclude list excludes nothing.
	m = NewMatcher([]Rule{{Path: "/notes", Include: []string{"**/*"}}}, []string{})
	if _, ok := m.Match("/notes/.env", 10); !ok {
		t.Error("an empty exclude list still excluded .env")
	}
}

func TestRootsAreTheOutermostRulePaths(t *testing.T) {
	m := NewMatcher([]Rule{
		{Path: "/work/code"},
		{Path: "/big/report.pdf"},
		{Path: "/work"},
		{Path: "/workspace"},
		{Path: "/work/docs/api"},
	}, nil)
	want := []string{"/big/report.pdf", "/work", "/workspace"}
	if got := m.Roots(); !reflect.DeepEqual(got, want) {
		t.Errorf("Roots() = %v, want %v", got, want)
	}
	if got := NewMatcher(nil, nil).Roots(); len(got) != 0 {
		t.Errorf("Roots() of no rules = %v", got)
	}
	if got := NewMatcher([]Rule{{Path: "/"}, {Path: "/work"}}, nil).Roots(); !reflect.DeepEqual(got, []string{"/"}) {
		t.Errorf("a root rule did not swallow the rest: %v", got)
	}
}

func TestRulesFromConfigKeepsEveryFieldAndMarksTheSource(t *testing.T) {
	x := config.Index{Enabled: true, Rules: []config.IndexRule{
		{Path: "/work"},
		{Path: "/big/report.pdf", Include: []string{"*"}, Exclude: []string{"x"}, MaxFileSize: 1 << 30},
	}}
	if err := x.Validate(); err != nil {
		t.Fatal(err)
	}
	rules := RulesFromConfig(x.Rules)
	if len(rules) != 2 {
		t.Fatalf("%d rules", len(rules))
	}
	if rules[0].Path != "/work" || rules[0].Source != "config" || rules[0].MaxFileSize != DefaultMaxFileSize ||
		!reflect.DeepEqual(rules[0].Include, config.DefaultIndexInclude) || rules[0].Exclude != nil {
		t.Errorf("rule 0 = %+v", rules[0])
	}
	if rules[1].Path != "/big/report.pdf" || rules[1].Source != "config" || rules[1].MaxFileSize != 1<<30 ||
		!reflect.DeepEqual(rules[1].Include, []string{"*"}) || !reflect.DeepEqual(rules[1].Exclude, []string{"x"}) {
		t.Errorf("rule 1 = %+v", rules[1])
	}
	// The conversion copies: mutating the result leaves the config alone.
	rules[1].Include[0] = "changed"
	if x.Rules[1].Include[0] != "*" {
		t.Error("RulesFromConfig shares the include slice with the config")
	}
	// The result is what the matcher and the store consume.
	m := NewMatcher(rules, x.Exclude)
	if _, ok := m.Match("/work/a.md", 10); !ok {
		t.Error("configured rule did not match through the matcher")
	}
	if _, ok := m.Match("/work/.env", 10); ok {
		t.Error("the configured default exclude did not apply")
	}
}
