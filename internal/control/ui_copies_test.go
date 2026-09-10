package control

import (
	"strings"
	"testing"
)

// Server-side copy is the feature the design calls the cheapest one there is:
// the bytes never leave the remote. The daemon has had the whole surface for
// it — start, list, retry, cancel, forget — with no screen anywhere. A copy
// that fails halfway leaves a journal row a person has to be able to see and
// act on, so the screen is not decoration.
func TestCopiesScreenReachesTheCopyRoutes(t *testing.T) {
	b, err := webFS.ReadFile("web/screens/copies.js")
	if err != nil {
		t.Fatalf("there is no copies screen: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		"api.get('/copies",
		"api.post('/copy'",             // start one
		"api.post('/copies/' + action", // retry and cancel share one call
		"'retry'",                      // resume a stalled one
		"'cancel'",                     // stop one in flight
		"api.post('/copies/forget'",    // drop the record, confirm required
		"confirm: true",                // ...which the daemon refuses without
		"next_cursor",                  // the list pages like every other list
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the copies screen has no %s", want)
		}
	}
}

// A screen nobody can navigate to is a screen that does not exist.
func TestCopiesScreenIsRoutedAndInTheNav(t *testing.T) {
	router := []byte(webSource(t, "web/router.js"))
	app := []byte(webSource(t, "web/app.js"))
	if !strings.Contains(string(router), "#/copies") || !strings.Contains(string(router), "copies-view") {
		t.Error("the router has no route for the copies screen")
	}
	if !strings.Contains(string(app), "renderCopies") {
		t.Error("the shell never mounts the copies screen")
	}
}
