package control

import (
	"strings"
	"testing"
)

// The proxy screen's own header comment promised that a change is saved and
// applied, while the screen only ever read the configuration: PUT
// /proxy/config had no caller anywhere in the page. A page that describes an
// ability it does not have is worse than one that omits it, so the screen
// either edits or stops saying it does.
func TestProxyScreenWritesTheConfiguration(t *testing.T) {
	src := webSource(t, "web/screens/proxy.js")
	for _, want := range []string{
		"api.put('/proxy/config'", // the write the header comment promised
		"api.get('/proxy/explain", // why does this host take this route
		"keep_credentials",        // never round-trip a redacted address
		"t('proxy.saved.applied'", // a live reload and a restart are different outcomes
		"t('proxy.saved.restart'", // ...and the page must not conflate them
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the proxy screen has no %s", want)
		}
	}
}
