//go:build desktop && !linux

package main

import webview "github.com/webview/webview_go"

// raiseWindow is a no-op where bringing the window forward needs platform
// window APIs this build does not carry (macOS, Windows). The single-instance
// lock still prevents a duplicate window on platforms that implement it.
func raiseWindow(webview.WebView) {}
