package control

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The page's decisions about credentials, renames and paging are the kind of
// logic no screenshot checks: which flag is sent, which edit is refused, which
// references follow a rename, what a failed "load more" leaves on screen. They
// live in modules with no DOM and no imports so they can be run directly, and
// this drives them under the same `go test` as everything else.
//
// The suites live under web/_tests: a leading underscore keeps them out of
// //go:embed web, so they are not shipped inside the daemon.
func TestBrowserModuleBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the browser modules are not exercised here")
	}
	suites, err := filepath.Glob(filepath.Join("web", "_tests", "*.test.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(suites) == 0 {
		t.Fatal("no browser module suites were found under web/_tests")
	}
	out, err := exec.Command(node, append([]string{"--test"}, suites...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node --test failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "fail 0") {
		t.Fatalf("node --test did not report a clean run:\n%s", out)
	}
}

// A syntax error in a browser module is invisible to `go build`: the file is
// served as it stands and the screen simply never renders. This parses every
// module the daemon ships, so a broken one fails here instead of in a blank
// page.
func TestBrowserModulesParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the browser modules are not parsed here")
	}
	var modules []string
	for _, pattern := range []string{
		filepath.Join("web", "*.js"),
		filepath.Join("web", "screens", "*.js"),
	} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		modules = append(modules, found...)
	}
	if len(modules) == 0 {
		t.Fatal("no browser modules were found")
	}
	for _, module := range modules {
		source, err := os.ReadFile(module)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(node, "--input-type=module", "--check")
		cmd.Stdin = bytes.NewReader(source)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s", module, err, out)
		}
	}
}
