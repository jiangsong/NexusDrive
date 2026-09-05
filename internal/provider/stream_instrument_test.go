package provider

import (
	"context"
	"errors"
	"io"
	"testing"
)

type streamStub struct{ Provider }

func (s *streamStub) Capabilities() Caps { return Caps{StreamList: true} }
func (s *streamStub) ListStream(ctx context.Context, id string, visit func(Entry) error) error {
	return visit(Entry{ID: "one", Name: "one"})
}

type streamCopyStub struct{ *streamStub }

func (*streamCopyStub) Copy(context.Context, string, string, string) (Entry, error) {
	return Entry{}, nil
}

type streamChangesStub struct{ *streamStub }

func (*streamChangesStub) Changes(context.Context, string) ([]Change, string, error) {
	return nil, "", nil
}

type streamBothStub struct{ *streamCopyStub }

func (*streamBothStub) Changes(context.Context, string) ([]Change, string, error) {
	return nil, "", nil
}

type streamPutStub struct{ *streamStub }

func (*streamPutStub) PutFile(context.Context, string, string, io.Reader, int64, Hashes) (Entry, error) {
	return Entry{}, nil
}

func TestInstrumentForwardsStreamAcrossEveryWrapper(t *testing.T) {
	base := &streamStub{Provider: &stubProvider{}}
	for _, p := range []Provider{base, &streamCopyStub{base}, &streamChangesStub{base}, &streamBothStub{&streamCopyStub{base}}, &streamPutStub{base}} {
		stats := NewStats()
		wrapped := Instrument(p, stats)
		stream, ok := wrapped.(StreamLister)
		if !ok || !wrapped.Capabilities().StreamList {
			t.Fatalf("%T lost streaming", wrapped)
		}
		stop := errors.New("stop visitor")
		visited := 0
		err := stream.ListStream(context.Background(), "/", func(e Entry) error {
			visited++
			if e.ID != "one" {
				t.Fatal("entry changed by wrapper")
			}
			return stop
		})
		if !errors.Is(err, stop) || visited != 1 || stats.Snapshot()["list"] != 1 {
			t.Fatalf("%T streaming forwarding: visits=%d stats=%v err=%v", wrapped, visited, stats.Snapshot(), err)
		}
		if Unwrap(wrapped) != p {
			t.Fatal("stream wrapper broke driver unwrapping")
		}
	}
}

func TestUnsupportedStreamProbeDoesNotCountRemoteCalls(t *testing.T) {
	stats := NewStats()
	p := Instrument(&stubProvider{}, stats)
	if p.Capabilities().StreamList {
		t.Fatal("wrapper invented streaming capability")
	}
	err := p.(StreamLister).ListStream(context.Background(), "/", func(Entry) error {
		t.Fatal("unsupported probe delivered a member")
		return nil
	})
	if !errors.Is(err, ErrUnsupported) || stats.Total() != 0 {
		t.Fatalf("unsupported stream counted traffic: %v %d", err, stats.Total())
	}
}
