package provider

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestUploadProofRejectsInvalidAndTruncatedContent(t *testing.T) {
	h := ContentRangeHasher(strings.NewReader("0123456789"), 10)
	got, err := h(context.Background(), 2, 5)
	if want := fmt.Sprintf("%x", sha1.Sum([]byte("2345"))); err != nil || got != want {
		t.Fatalf("proof = %q, %v; want %q", got, err, want)
	}
	for _, bounds := range [][2]int64{{-1, 3}, {3, 2}, {0, 10}, {10, 10}, {0, 1<<63 - 1}} {
		if got, err := h(context.Background(), bounds[0], bounds[1]); err == nil || got != "" {
			t.Fatalf("invalid bounds %v produced proof %q, %v", bounds, got, err)
		}
	}
	short := ContentRangeHasher(strings.NewReader("short"), 10)
	if got, err := short(context.Background(), 0, 9); err == nil || got != "" {
		t.Fatalf("truncated source produced proof %q, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h(ctx, 0, 1); err != context.Canceled {
		t.Fatalf("cancelled proof: %v", err)
	}
}

func TestUploadContentReaderBoundsAndShortRead(t *testing.T) {
	read := ContentRangeReader(strings.NewReader("0123456789"), 10)
	if b, err := read(context.Background(), 3, 4); err != nil || string(b) != "3456" {
		t.Fatalf("proof bytes = %q, %v", b, err)
	}
	for _, bounds := range [][2]int64{{-1, 1}, {0, -1}, {10, 1}, {11, 0}, {1, 1<<63 - 1}, {1<<63 - 1, 1}} {
		if b, err := read(context.Background(), bounds[0], bounds[1]); err == nil || b != nil {
			t.Fatalf("invalid %v returned %q, %v", bounds, b, err)
		}
	}
	if b, err := read(context.Background(), 10, 0); err != nil || len(b) != 0 {
		t.Fatalf("empty range = %q, %v", b, err)
	}
	short := ContentRangeReader(strings.NewReader("short"), 10)
	if b, err := short(context.Background(), 0, 10); err == nil || b != nil {
		t.Fatalf("short range = %q, %v", b, err)
	}
	large := ContentRangeReader(strings.NewReader("unused"), 1<<30)
	if _, err := large(context.Background(), 0, MaxUploadProofBytes+1); err == nil {
		t.Fatal("unbounded proof allocation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := read(ctx, 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	if UploadContentReaderFrom(context.Background()) != nil {
		t.Fatal("unexpected source")
	}
	if b, err := UploadContentReaderFrom(WithUploadContentReader(context.Background(), read))(context.Background(), 0, 1); err != nil || string(b) != "0" {
		t.Fatalf("context source = %q, %v", b, err)
	}
}
