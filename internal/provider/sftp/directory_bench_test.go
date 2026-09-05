package sftp

import (
	"encoding/binary"
	"fmt"
	"testing"

	"golang.org/x/crypto/ssh"

	"cloudfs/internal/provider"
)

// Includes both ends of a real encrypted loopback SSH connection. The server
// synthesizes one 100-member packet at a time; there is no whole-directory
// fixture. Allocations are cumulative, NOT peak memory or a NAS throughput
// claim. The connection is warmed once to separate handshake from scan cost.
func BenchmarkDirectorySSHEnumeration(b *testing.B) {
	for _, count := range []int{1000, 100000} {
		for _, stream := range []bool{true, false} {
			b.Run(fmt.Sprintf("members=%d/stream=%t", count, stream), func(b *testing.B) {
				s := newDirectorySSHServer(b, "", func(ch ssh.Channel) {
					f := &directoryFixture{}
					page := 0
					for {
						req, err := readDirectoryPacket(ch)
						if err != nil {
							return
						}
						reply := f.reply(req)
						if req[0] == dirRead {
							id := binary.BigEndian.Uint32(req[1:5])
							if page*100 < count {
								members := make([][]byte, 0, 100)
								for i := range 100 {
									members = append(members, directoryTestMember(fmt.Sprintf("file-%08d", page*100+i), directoryTestAttrs(7, 0o100644)))
								}
								reply = directoryTestNames(id, members...)
							} else {
								reply = directoryTestStatus(id, 1)
							}
							page++
						}
						if writeDirectoryPacket(ch, reply) != nil {
							return
						}
					}
				})
				p := s.provider(b)
				if err := p.ListStream(b.Context(), "/", func(provider.Entry) error { return nil }); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					n := 0
					var err error
					if stream {
						err = p.ListStream(b.Context(), "/", func(provider.Entry) error { n++; return nil })
					} else {
						var entries []provider.Entry
						entries, _, err = p.List(b.Context(), "/", "")
						n = len(entries)
					}
					if err != nil || n != count {
						b.Fatalf("members=%d err=%v", n, err)
					}
				}
				b.StopTimer()
				if s.connections.Load() != 1 {
					b.Fatalf("connection was not reused: %d", s.connections.Load())
				}
			})
		}
	}
}
