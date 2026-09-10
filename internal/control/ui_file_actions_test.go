package control

import (
	"strings"
	"testing"
)

// The inspector could pin, warm and delete. Renaming, looking at the first
// bytes and getting a download link were all routed, tested and unreachable:
// three verbs that exist in the daemon, in the CLI and in the agent surface,
// but not in the page a person actually uses.
func TestInspectorReachesTheFileRoutes(t *testing.T) {
	src := webSource(t, "web/screens/main.js")
	for _, want := range []string{
		"'/fs/rename'",       // move or rename within the namespace
		"'/fs/preview?path=", // the first bytes, capped by the daemon
		"'/fs/download-url?path=",
		"t('action.rename'",
		"t('action.preview'",
		"t('action.link'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the inspector has no %s", want)
		}
	}
	// A download link is a signed URL. Putting it in the DOM of a page that
	// stays open long after the link expires is how a stale link becomes a
	// confusing error; it is handed over as a copy action instead.
	if strings.Contains(src, "window.open(") {
		t.Error("the inspector opens a signed URL in a new window instead of handing it over deliberately")
	}
}

// Preview reads bytes that may not be text at all. The page must say what it
// is showing rather than paint binary into the document.
func TestPreviewSaysWhenTheBytesAreNotText(t *testing.T) {
	if !strings.Contains(webSource(t, "web/screens/main.js"), "t('preview.binary'") {
		t.Fatal("the preview never tells the reader the bytes are not text")
	}
}
