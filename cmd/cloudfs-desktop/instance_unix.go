//go:build desktop && (linux || darwin)

package main

import (
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"

	"cloudfs/internal/config"
)

// instance is the single-window guard. The lock file keeps one shell per user;
// the focus socket next to it lets a second launch wake the first instead of
// opening a duplicate window.
type instance struct {
	lock  *os.File
	focus net.Listener
	mu    sync.Mutex
	cb    func()
}

func instancePaths() (lock, sock string) {
	dir := filepath.Dir(config.ExpandHome("~/.config/cloudfs/config.yaml"))
	return filepath.Join(dir, "desktop.lock"), filepath.Join(dir, "desktop.focus")
}

// acquireInstance takes the single-instance lock. A held==true return means
// another shell already owns it and this launch should defer to it.
func acquireInstance() (*instance, bool, error) {
	lockPath, sockPath := instancePaths()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, false, err
	}
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, false, err
	}
	f := os.NewFile(uintptr(fd), lockPath)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, true, nil
	}
	os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		// Single-instance still holds; only cross-launch focus is lost.
		return &instance{lock: f}, false, nil
	}
	inst := &instance{lock: f, focus: ln}
	go inst.serveFocus()
	return inst, false, nil
}

func (i *instance) serveFocus() {
	for {
		c, err := i.focus.Accept()
		if err != nil {
			return
		}
		c.Close()
		i.mu.Lock()
		cb := i.cb
		i.mu.Unlock()
		if cb != nil {
			cb()
		}
	}
}

func (i *instance) onFocus(cb func()) {
	i.mu.Lock()
	i.cb = cb
	i.mu.Unlock()
}

func (i *instance) release() {
	if i.focus != nil {
		i.focus.Close()
	}
	if i.lock != nil {
		unix.Flock(int(i.lock.Fd()), unix.LOCK_UN)
		i.lock.Close()
	}
	_, sockPath := instancePaths()
	os.Remove(sockPath)
}

// signalFocus tells a running shell to come forward.
func signalFocus() {
	_, sockPath := instancePaths()
	if c, err := net.Dial("unix", sockPath); err == nil {
		c.Close()
	}
}
