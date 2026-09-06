package provider

import (
	"context"
	"errors"
	"testing"
)

func TestCheckNameAppliesEachRule(t *testing.T) {
	win := Naming{CaseInsensitive: true, MaxNameBytes: 255, ForbiddenRunes: WindowsForbiddenRunes, ReservedNames: WindowsReservedNames, NoTrailingDotSpace: true}
	for _, ok := range []string{"report.docx", "photo 2026.jpg", "con-artist.txt", "nulled.png"} {
		if err := CheckName(win, ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"a:b.txt", "what?.txt", "trailing.", "trailing ", "CON", "con.txt", "LPT1.log", "", ".", "..", "a/b"} {
		if err := CheckName(win, bad); !errors.Is(err, ErrBadName) || !errors.Is(err, ErrUnsupported) {
			t.Errorf("%q accepted or wrongly classified: %v", bad, err)
		}
	}
	if err := CheckName(Naming{}, "anything: goes? <here>"); err != nil {
		t.Fatalf("the zero rules refuse %v", err)
	}
	if err := CheckName(Naming{NoLeadingDot: true}, ".hidden"); err == nil {
		t.Fatal("leading dot accepted")
	}
	if err := CheckName(Naming{MaxNameBytes: 3}, "abcd"); err == nil {
		t.Fatal("long name accepted")
	}
}

type quotaStub struct {
	Provider
	q Quota
}

func (s *quotaStub) Name() string                                { return "q" }
func (s *quotaStub) Capabilities() Caps                          { return Caps{} }
func (s *quotaStub) Quota(context.Context) (Quota, error)        { return s.q, nil }
func (s *quotaStub) Stat(context.Context, string) (Entry, error) { return Entry{}, nil }

// TestUnwrapKeepsQuotaerReachable: the daemon holds every provider
// instrumented, and the wrapper's optional-interface handling is a fixed
// switch. Quota goes through Unwrap so it is never lost behind it.
func TestUnwrapKeepsQuotaerReachable(t *testing.T) {
	raw := &quotaStub{q: Quota{Total: 100, Used: 40}}
	wrapped := Instrument(raw, NewStats())
	if _, ok := wrapped.(Quotaer); ok {
		t.Log("the wrapper forwards Quotaer itself; QuotaOf must still work")
	}
	q, ok, err := QuotaOf(context.Background(), wrapped)
	if err != nil || !ok || q.Free() != 60 {
		t.Fatalf("quota through instrumentation = %+v ok=%v err=%v", q, ok, err)
	}
	if _, ok, _ := QuotaOf(context.Background(), &failingProvider{}); ok {
		t.Fatal("a backend without Quota reported one")
	}
	if (Quota{}).Free() != -1 || (Quota{Total: 10, Used: 12}).Free() != 0 {
		t.Fatal("Free arithmetic")
	}
}
