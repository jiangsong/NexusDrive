//go:build desktop && windows

package main

// Windows single-instance and cross-launch focus are left to the installer and
// the WebView2 host; this build simply opens a window. The daemon's own journal
// lock still prevents two daemons, which is the invariant that matters.
type instance struct{}

func acquireInstance() (*instance, bool, error) { return &instance{}, false, nil }
func (i *instance) onFocus(func())              {}
func (i *instance) release()                    {}
func signalFocus()                              {}
