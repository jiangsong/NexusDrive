package aliyun

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloudfs/internal/provider"
)

func TestProofRejectsShortCallbackAndPropagatesCancellation(t *testing.T) {
	_, hs := newServer(t)
	p := newProvider(t, hs)
	for _, delta := range []int{-1, 1} {
		ctx := provider.WithUploadContentReader(context.Background(), func(_ context.Context, off, n int64) ([]byte, error) { return make([]byte, int(n)+delta), nil })
		if code, ok, err := p.proofCode(ctx, "root", "a", 100); err != nil || ok || code != "" {
			t.Fatalf("wrong-length proof accepted: %q, %v, %v", code, ok, err)
		}
	}
	ctx := provider.WithUploadContentReader(context.Background(), func(context.Context, int64, int64) ([]byte, error) { return nil, context.Canceled })
	if _, _, err := p.proofCode(ctx, "root", "a", 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation swallowed: %v", err)
	}
}

func TestQueuedContentTakesPrecedenceOverStandaloneProofHook(t *testing.T) {
	_, hs := newServer(t)
	p := newProvider(t, hs, func(o *Options) {
		o.ProofBytes = func(context.Context, string, string, int64, int64) ([]byte, error) {
			t.Error("standalone callback used for queued content")
			return nil, nil
		}
	})
	content := strings.Repeat("hello-proof-", 100)
	ctx := provider.WithUploadContentReader(context.Background(), provider.ContentRangeReader(strings.NewReader(content), int64(len(content))))
	code, ok, err := p.proofCode(ctx, "root", "a", int64(len(content)))
	off, n := ProofRange("at-1", int64(len(content)))
	if err != nil || !ok || code != ProofCode([]byte(content[off:off+n])) {
		t.Fatalf("proof = %q, %v, %v", code, ok, err)
	}
}
