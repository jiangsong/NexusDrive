package provider

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

type stubProvider struct{ calls int }

func (s *stubProvider) Name() string       { return "stub" }
func (s *stubProvider) Capabilities() Caps { return Caps{} }
func (s *stubProvider) List(context.Context, string, string) ([]Entry, string, error) {
	s.calls++
	return nil, "", nil
}
func (s *stubProvider) Stat(context.Context, string) (Entry, error) { return Entry{}, nil }
func (s *stubProvider) ReadRange(context.Context, string, string, int64, int64) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("0123456789")), nil
}
func (s *stubProvider) DownloadURL(context.Context, string) (Link, error) { return Link{}, nil }
func (s *stubProvider) BeginUpload(context.Context, string, string, int64, Hashes) (UploadSession, error) {
	return UploadSession{}, nil
}
func (s *stubProvider) UploadPart(context.Context, UploadSession, int, io.Reader, int64) (PartToken, error) {
	return PartToken{}, nil
}
func (s *stubProvider) CompleteUpload(context.Context, UploadSession, []PartToken) (Entry, error) {
	return Entry{}, nil
}
func (s *stubProvider) Mkdir(context.Context, string, string) (Entry, error)  { return Entry{}, nil }
func (s *stubProvider) Rename(context.Context, string, string) (Entry, error) { return Entry{}, nil }
func (s *stubProvider) Move(context.Context, string, string) (Entry, error)   { return Entry{}, nil }
func (s *stubProvider) Delete(context.Context, string) error                  { return nil }

type stubWithChanges struct{ *stubProvider }

func (s *stubWithChanges) Changes(context.Context, string) ([]Change, string, error) {
	return nil, "", nil
}

type stubWithCopy struct{ *stubProvider }

func (s *stubWithCopy) Copy(context.Context, string, string, string) (Entry, error) {
	return Entry{}, nil
}

type stubWithBoth struct{ *stubProvider }

func (s *stubWithBoth) Changes(context.Context, string) ([]Change, string, error) {
	return nil, "", nil
}
func (s *stubWithBoth) Copy(context.Context, string, string, string) (Entry, error) {
	return Entry{}, nil
}

func TestInstrumentCountsCalls(t *testing.T) {
	s := NewStats()
	p := Instrument(&stubProvider{}, s)
	ctx := context.Background()
	p.List(ctx, "/", "")
	p.List(ctx, "/", "")
	p.Stat(ctx, "/x")
	p.Delete(ctx, "/x")
	got := s.Snapshot()
	if got["list"] != 2 || got["stat"] != 1 || got["delete"] != 1 {
		t.Fatalf("counts = %v", got)
	}
	if s.Total() != 4 {
		t.Fatalf("total = %d, want 4", s.Total())
	}
}

func TestInstrumentCountsReadBytes(t *testing.T) {
	s := NewStats()
	p := Instrument(&stubProvider{}, s)
	rc, err := p.ReadRange(context.Background(), "/x", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(rc)
	rc.Close()
	if s.ReadBytes() != 10 {
		t.Fatalf("read bytes = %d, want 10", s.ReadBytes())
	}
}

// TestInstrumentPreservesOptionalInterfaces is the property that matters most:
// the refresher decides whether to poll a remote by type-asserting
// ChangeLister, so a wrapper that adds or drops it silently changes behaviour.
func TestInstrumentPreservesOptionalInterfaces(t *testing.T) {
	cases := []struct {
		name            string
		p               Provider
		changes, copies bool
	}{
		{"plain", &stubProvider{}, false, false},
		{"changes", &stubWithChanges{&stubProvider{}}, true, false},
		{"copy", &stubWithCopy{&stubProvider{}}, false, true},
		{"both", &stubWithBoth{&stubProvider{}}, true, true},
	}
	for _, c := range cases {
		w := Instrument(c.p, NewStats())
		if _, ok := w.(ChangeLister); ok != c.changes {
			t.Errorf("%s: ChangeLister = %v, want %v", c.name, ok, c.changes)
		}
		if _, ok := w.(ServerCopier); ok != c.copies {
			t.Errorf("%s: ServerCopier = %v, want %v", c.name, ok, c.copies)
		}
	}
}

func TestInstrumentReset(t *testing.T) {
	s := NewStats()
	p := Instrument(&stubProvider{}, s)
	p.List(context.Background(), "/", "")
	s.Reset()
	if s.Total() != 0 || s.ReadBytes() != 0 {
		t.Fatal("reset did not clear the counters")
	}
}

func TestInstrumentNilIsSafe(t *testing.T) {
	if got := Instrument(nil, NewStats()); got != nil {
		t.Fatal("wrapping nil should stay nil")
	}
	p := &stubProvider{}
	if got := Instrument(p, nil); got != Provider(p) {
		t.Fatal("without stats the provider should be returned unchanged")
	}
}

func TestUnwrapReturnsTheBackend(t *testing.T) {
	inner := &stubProvider{}
	for _, p := range []Provider{
		Provider(inner),
		&stubWithChanges{inner},
		&stubWithCopy{inner},
		&stubWithBoth{inner},
	} {
		if got := Unwrap(Instrument(p, NewStats())); got != p {
			t.Fatalf("Unwrap returned %T, want %T", got, p)
		}
	}
}

type stubWithPut struct{ *stubProvider }

func (s *stubWithPut) PutFile(context.Context, string, string, io.Reader, int64, Hashes) (Entry, error) {
	return Entry{}, nil
}

func TestInstrumentCountsSinglePuts(t *testing.T) {
	st := NewStats()
	p := Instrument(&stubWithPut{&stubProvider{}}, st)
	sp, ok := p.(SinglePutter)
	if !ok {
		t.Fatal("SinglePutter was not preserved by the wrapper")
	}
	sp.PutFile(context.Background(), "/", "x", strings.NewReader("abcd"), 4, nil)
	if st.Snapshot()["put_file"] != 1 || st.WriteBytes() != 4 {
		t.Fatalf("counts = %v, bytes = %d", st.Snapshot(), st.WriteBytes())
	}
}

// TestInstrumentKeepsTheBufferFillingRead: the counting wrapper for a
// single-put backend embeds the Provider interface, which hid ReadRangeAt
// and sent every block through the copying path.
func TestInstrumentKeepsTheBufferFillingRead(t *testing.T) {
	p := Instrument(&fastPutter{}, &Stats{})
	if _, ok := p.(RangeReaderAt); !ok {
		t.Fatal("the instrumented single-put provider lost ReadRangeAt")
	}
	buf := make([]byte, 4)
	if n, err := p.(RangeReaderAt).ReadRangeAt(context.Background(), "id", "v", 0, buf); err != nil || n != 4 {
		t.Fatalf("ReadRangeAt through the wrapper: %d %v", n, err)
	}
}

type fastPutter struct{ Provider }

func (fastPutter) ReadRangeAt(ctx context.Context, id, version string, off int64, buf []byte) (int, error) {
	return copy(buf, "data"), nil
}
func (fastPutter) PutFile(ctx context.Context, parentID, name string, r io.Reader, size int64, h Hashes) (Entry, error) {
	return Entry{}, nil
}

// TestStatsRecordTimePerOperation: call counts say how much traffic a change
// causes; the time inside those calls is what says whether the round trip or
// the code above it is the floor. Without it every "slow" is a guess.
func TestStatsRecordTimePerOperation(t *testing.T) {
	s := NewStats()
	done := s.timed("list")
	time.Sleep(2 * time.Millisecond)
	done()
	ns := s.Nanos()
	if ns["list"] < int64(time.Millisecond) {
		t.Fatalf("list recorded %d ns, want at least a millisecond", ns["list"])
	}
	if s.Snapshot()["list"] != 1 {
		t.Fatal("timing a call must also count it")
	}
	s.Reset()
	if len(s.Nanos()) != 0 {
		t.Fatal("Reset must clear the timings with the counts")
	}
}

type stubWithPutAndChanges struct{ *stubProvider }

func (s *stubWithPutAndChanges) Changes(context.Context, string) ([]Change, string, error) {
	return nil, "", nil
}
func (s *stubWithPutAndChanges) PutFile(context.Context, string, string, io.Reader, int64, Hashes) (Entry, error) {
	return Entry{}, nil
}

type stubWithPutAndCopy struct{ *stubProvider }

func (s *stubWithPutAndCopy) Copy(context.Context, string, string, string) (Entry, error) {
	return Entry{}, nil
}
func (s *stubWithPutAndCopy) PutFile(context.Context, string, string, io.Reader, int64, Hashes) (Entry, error) {
	return Entry{}, nil
}

type stubWithEverything struct{ *stubProvider }

func (s *stubWithEverything) Changes(context.Context, string) ([]Change, string, error) {
	return nil, "", nil
}
func (s *stubWithEverything) Copy(context.Context, string, string, string) (Entry, error) {
	return Entry{}, nil
}
func (s *stubWithEverything) PutFile(context.Context, string, string, io.Reader, int64, Hashes) (Entry, error) {
	return Entry{}, nil
}

// TestInstrumentKeepsTheOneRequestUploadBesideTheOthers: a backend that
// offers a one-request upload usually offers a change feed or a server-side
// copy too — gdrive, onedrive, dropbox and box all do. The wrapper used to
// choose: any of those and PutFile was dropped, so every small write to
// those four paid for a three-request upload session instead, with nothing
// to show for it. Each combination keeps everything the driver declared.
func TestInstrumentKeepsTheOneRequestUploadBesideTheOthers(t *testing.T) {
	cases := []struct {
		name                  string
		p                     Provider
		changes, copies, puts bool
	}{
		{"put only", &stubWithPut{&stubProvider{}}, false, false, true},
		{"put and changes", &stubWithPutAndChanges{&stubProvider{}}, true, false, true},
		{"put and copy", &stubWithPutAndCopy{&stubProvider{}}, false, true, true},
		{"all three", &stubWithEverything{&stubProvider{}}, true, true, true},
		{"changes only", &stubWithChanges{&stubProvider{}}, true, false, false},
	}
	for _, c := range cases {
		st := NewStats()
		w := Instrument(c.p, st)
		if _, ok := w.(ChangeLister); ok != c.changes {
			t.Errorf("%s: ChangeLister = %v, want %v", c.name, ok, c.changes)
		}
		if _, ok := w.(ServerCopier); ok != c.copies {
			t.Errorf("%s: ServerCopier = %v, want %v", c.name, ok, c.copies)
		}
		sp, ok := w.(SinglePutter)
		if ok != c.puts {
			t.Errorf("%s: SinglePutter = %v, want %v", c.name, ok, c.puts)
			continue
		}
		if !ok {
			continue
		}
		// The put still goes through the counter, whichever variant carries it.
		if _, err := sp.PutFile(context.Background(), "/", "x", strings.NewReader("abcd"), 4, nil); err != nil {
			t.Errorf("%s: PutFile: %v", c.name, err)
		}
		if st.Snapshot()["put_file"] != 1 || st.WriteBytes() != 4 {
			t.Errorf("%s: counts = %v bytes = %d", c.name, st.Snapshot(), st.WriteBytes())
		}
		// And the backend is still reachable through Unwrap.
		if Unwrap(w) != c.p {
			t.Errorf("%s: Unwrap did not reach the backend", c.name)
		}
		if _, ok := w.(RangeReaderAt); !ok {
			t.Errorf("%s: lost ReadRangeAt", c.name)
		}
		if _, ok := w.(StreamLister); !ok {
			t.Errorf("%s: lost ListStream", c.name)
		}
	}
}
