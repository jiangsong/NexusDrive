//go:build desktop && linux

package main

/*
#cgo pkg-config: gtk+-3.0
#include <gtk/gtk.h>

static void cloudfs_raise(void *w) {
	if (w != NULL) {
		gtk_window_present(GTK_WINDOW(w));
	}
}
*/
import "C"

import (
	"unsafe"

	webview "github.com/webview/webview_go"
)

// raiseWindow brings the WebKitGTK window to the front. webview.Window() returns
// the GtkWindow* on this platform.
func raiseWindow(w webview.WebView) {
	if ptr := w.Window(); ptr != nil {
		C.cloudfs_raise(unsafe.Pointer(ptr))
	}
}
