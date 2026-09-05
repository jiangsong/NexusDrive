package sftp

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type directoryCloseTestConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *directoryCloseTestConn) Close() error {
	c.once.Do(func() { c.Conn.Close(); close(c.done) })
	return nil
}

// A peer which never acknowledges SSH CLOSE, or stops reading TCP entirely.
type directoryCloseTestChannel struct {
	*directoryCloseTestConn
	blockWrite bool
}

func (c *directoryCloseTestChannel) Close() error {
	if c.blockWrite {
		_, err := c.Write([]byte{1})
		return err
	}
	return nil
}
func (*directoryCloseTestChannel) CloseWrite() error                              { return nil }
func (*directoryCloseTestChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (c *directoryCloseTestChannel) Stderr() io.ReadWriter                        { return c.directoryCloseTestConn }

func TestDirectoryChannelCloseBoundsUnresponsivePeersAndDrains(t *testing.T) {
	for _, blockWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-close-ack", true: "blocked-close-write"}[blockWrite], func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			raw := &directoryCloseTestConn{Conn: client, done: make(chan struct{})}
			defer raw.Close()
			requests := make(chan *ssh.Request)
			go func() { <-raw.done; close(requests) }()
			stream := newDirectoryChannel(&directoryCloseTestChannel{directoryCloseTestConn: raw, blockWrite: blockWrite}, requests, raw)
			done := make(chan error, 2)
			// Close is also called concurrently by cancellation and scan cleanup.
			for range 2 {
				go func() { done <- stream.Close() }()
			}
			for range 2 {
				awaitDirectoryResult(t, done)
			}
			for _, drained := range []chan struct{}{raw.done, stream.requests, stream.stderr} {
				select {
				case <-drained:
				default:
					t.Fatal("Close returned before channel drains ended")
				}
			}
			start := time.Now()
			stream.Close()
			if time.Since(start) > time.Second {
				t.Fatal("repeated Close blocked")
			}
		})
	}
}
