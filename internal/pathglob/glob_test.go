package pathglob

import "testing"

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		pat, p string
		want   bool
	}{
		// ** matches zero or more segments.
		{"**/*.md", "/work/a.md", true},
		{"**/*.md", "/a.md", true},
		{"**/*.md", "/work/a.mdx", false},
		{"**/*.md", "/work/deep/er/a.md", true},
		{"**/.git/**", "/work/.git/config", true},
		{"**/.git/**", "/work/.git/objects/ab/cd", true},
		{"**/.git/**", "/work/.git", true},
		{"**/.git/**", "/work/.github/x", false},
		{"**/id_rsa*", "/home/.ssh/id_rsa.pub", true},
		{"**/id_rsa*", "/home/.ssh/id_rsa", true},
		{"**/id_rsa*", "/home/.ssh/id_dsa", false},
		{"**/.env", "/app/.env", true},
		{"**/.env", "/.env", true},
		{"**/.env", "/app/.envrc", false},
		{"**/*.pem", "/work/keys/server.pem", true},
		{"**/node_modules/**", "/w/node_modules/a/b.js", true},
		{"**/node_modules/**", "/w/src/node_modules_backup/a.js", false},
		// Anchored patterns start at the first segment.
		{"docs/*.txt", "/docs/a.txt", true},
		{"docs/*.txt", "/x/docs/a.txt", false},
		{"docs/*.txt", "/docs/sub/a.txt", false},
		{"docs/**/*.txt", "/docs/sub/a.txt", true},
		{"docs/**/*.txt", "/docs/a.txt", true},
		{"docs/**", "/docs", true},
		{"docs/**", "/docs/a/b", true},
		{"docs/**", "/other/docs/a", false},
		// "*" never crosses a "/".
		{"*.md", "/a.md", true},
		{"*.md", "/dir/a.md", false},
		{"*", "/dir/a.md", false},
		{"src/*/main.go", "/src/cmd/main.go", true},
		{"src/*/main.go", "/src/cmd/sub/main.go", false},
		// Character classes and ? come from path.Match.
		{"**/[a-c].txt", "/x/b.txt", true},
		{"**/[a-c].txt", "/x/d.txt", false},
		{"**/a?.go", "/x/ab.go", true},
		{"**/a?.go", "/x/abc.go", false},
		// Matching is case-sensitive.
		{"**/*.MD", "/work/a.md", false},
		{"**/README.md", "/work/readme.md", false},
		// Runs of ** collapse, ** alone matches everything.
		{"**", "/anything/at/all", true},
		{"**", "/", true},
		{"**/**/*.go", "/a.go", true},
		{"**/**/*.go", "/x/y/a.go", true},
		// Leading and doubled slashes do not matter.
		{"/docs/*.txt", "docs/a.txt", true},
		{"docs/*.txt", "//docs//a.txt", true},
		// Malformed segments match nothing rather than panicking.
		{"**/[", "/x/[", false},
		{"", "/x", false},
		{"", "/", true},
	} {
		if got := Match(tc.pat, tc.p); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pat, tc.p, got, tc.want)
		}
	}
}

func TestMatchHandlesRepeatedDoubleStars(t *testing.T) {
	// Several "**" against a long path of identical segments must finish
	// promptly and still answer by the last segment.
	p := "/"
	for i := 0; i < 40; i++ {
		p += "a/"
	}
	p += "b"
	pat := "**/a/**/a/**/a/**/a/**/c"
	if Match(pat, p) {
		t.Fatal("matched a path that ends in b against a pattern that ends in c")
	}
}
