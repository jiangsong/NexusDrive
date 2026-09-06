//go:build !desktop

// Command cloudfs-desktop is the native window shell for the CloudFS dashboard.
// It is a cgo program that embeds a system WebView (WebKitGTK on Linux,
// WKWebView on macOS, WebView2 on Windows) and points it at the running
// daemon's control UI. Because it needs those platform libraries and cgo, it
// is not part of the default build: build it with `-tags desktop`.
//
// This stub is what the default (no-tag) build compiles, so `go build ./...`
// and `go vet ./...` stay green on a machine without the WebView toolchain.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "cloudfs-desktop was built without desktop support; rebuild with: go build -tags desktop ./cmd/cloudfs-desktop")
	os.Exit(2)
}
