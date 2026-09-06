//go:build !windows

package control

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// bindControlSocket takes the ownership lock, clears a stale socket only when
// nothing is answering on it, and listens on a private 0600 Unix socket. The
// flock serializes this recovery against a second starting process, and the
// non-blocking lock is what makes "someone already owns this socket" a clear
// error rather than two daemons fighting over one path.
func bindControlSocket(socket string) (*os.File, net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return nil, nil, err
	}
	fd, err := unix.Open(socket+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("control: socket lock: %w", err)
	}
	lock := os.NewFile(uintptr(fd), socket+".lock")
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, nil, fmt.Errorf("control: socket %s already owned: %w", socket, err)
	}
	if st, e := os.Lstat(socket); e == nil {
		if st.Mode()&os.ModeSocket == 0 {
			lock.Close()
			return nil, nil, fmt.Errorf("control: refusing to replace non-socket %s", socket)
		}
		conn, e := net.DialTimeout("unix", socket, 200*time.Millisecond)
		if e == nil {
			conn.Close()
			lock.Close()
			return nil, nil, fmt.Errorf("control: socket %s is active", socket)
		}
		if !errors.Is(e, syscall.ECONNREFUSED) && !errors.Is(e, os.ErrNotExist) {
			lock.Close()
			return nil, nil, e
		}
		if e = os.Remove(socket); e != nil {
			lock.Close()
			return nil, nil, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		lock.Close()
		return nil, nil, e
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		lock.Close()
		return nil, nil, err
	}
	if err := os.Chmod(socket, 0600); err != nil {
		l.Close()
		lock.Close()
		return nil, nil, err
	}
	return lock, l, nil
}
