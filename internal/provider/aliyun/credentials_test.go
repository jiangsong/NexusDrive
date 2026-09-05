package aliyun

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestTokenCannotBeUsedUntilRotationIsSaved(t *testing.T) {
	s, hs := newServer(t)
	s.on(pathToken, func(call) (int, string) {
		return http.StatusOK, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":7200}`
	})
	p := newProvider(t, hs)
	failure := errors.New("storage unavailable")
	fail := true
	p.SetTokenPersister(func(fields map[string]string) error {
		if fields["refresh_token"] != "new-refresh" {
			t.Fatalf("wrong rotated value")
		}
		if fail {
			return failure
		}
		return nil
	})
	if _, err := p.refresh(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("refresh error: %v", err)
	}
	if _, err := p.token(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("unsaved token exposed: %v", err)
	}
	fail = false
	if tok, err := p.token(context.Background()); err != nil || tok != "new-access" {
		t.Fatalf("saved token: %q %v", tok, err)
	}
	if got := len(s.requests()); got != 1 {
		t.Fatalf("save retry rotated token again: %d requests", got)
	}
}
