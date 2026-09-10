package control

import (
	"strings"
	"testing"
)

// Four places open a modal and each cleared `app.inert` on the way out. A
// sheet opened from inside another one — unbinding a mount from the
// connection settings, discarding an upload from the queue — therefore
// un-inerted the page while its parent sheet was still up, handing focus and
// clicks back to content behind two layers of scrim. Closing a modal may only
// release the page when it is the last one.
func TestClosingAModalOnlyReleasesThePageWhenItIsTheLastOne(t *testing.T) {
	if !strings.Contains(webSource(t, "web/ui.js"), "export function releasePage") {
		t.Fatal("there is no shared way to release the page, so each modal will invent one")
	}
	// One shell owns the scrim, so one place decides when the page comes
	// back. A screen that clears app.inert itself is a screen that will do
	// it while a sheet underneath is still open.
	entries, err := webFS.ReadDir("web")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			sub, _ := webFS.ReadDir("web/" + e.Name())
			for _, x := range sub {
				files = append(files, "web/"+e.Name()+"/"+x.Name())
			}
			continue
		}
		files = append(files, "web/"+e.Name())
	}
	for _, name := range files {
		if name == "web/ui.js" || !strings.HasSuffix(name, ".js") {
			continue
		}
		b, err := webFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "app.inert") {
			t.Errorf("%s manages the page's inertness itself instead of going through the shared shell", name)
		}
	}
}

// A status tick arrives every couple of seconds. Reloading the queue on each
// one throws away whatever pages the reader had asked for, seconds after they
// asked. Live data is worth having; silently undoing a person's action is not.
func TestTheQueueDoesNotResetItsPagingOnEveryStatusTick(t *testing.T) {
	if strings.Contains(webSource(t, "web/screens/transfers.js"), "subscribe(() => load())") {
		t.Fatal("a status tick reloads the queue from the first page, discarding the pages the reader loaded")
	}
}

// The language table is a plain object, so tables[explicit] is true for
// inherited keys: ?lang=constructor passed the check and became the value
// echoed into document.documentElement.lang and every request's query string.
func TestTheLanguageCheckIgnoresInheritedProperties(t *testing.T) {
	src := webSource(t, "web/i18n.js")
	if strings.Contains(src, "tables[explicit]") || strings.Contains(src, "tables[stored]") {
		t.Error("the language check accepts anything Object.prototype provides")
	}
	if !strings.Contains(src, "supported(") {
		t.Error("there is no single place deciding whether a language code is one we have")
	}
}
