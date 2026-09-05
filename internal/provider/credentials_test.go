package provider

import (
	"errors"
	"testing"
)

func TestRotatedCredentialsRetriedAfterSaveFailure(t *testing.T) {
	var p TokenPersistence
	fail := true
	var got string
	p.SetTokenPersister(func(fields map[string]string) error {
		if fail {
			return errors.New("disk unavailable")
		}
		got = fields["refresh_token"]
		fields["refresh_token"] = "must not mutate retained state"
		return nil
	})
	if err := p.SaveTokens(map[string]string{"refresh_token": "rotated"}); err == nil {
		t.Fatal("save failure hidden")
	}
	if err := p.FlushTokens(); err == nil {
		t.Fatal("pending failure hidden")
	}
	fail = false
	if err := p.FlushTokens(); err != nil || got != "rotated" {
		t.Fatalf("retry: %q %v", got, err)
	}
	got = ""
	if err := p.FlushTokens(); err != nil || got != "" {
		t.Fatal("saved credentials written again")
	}
}
