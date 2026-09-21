package memory

import (
	"context"
	"errors"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/index"
)

func TestCandidateRequiresReviewAndReviewIsIdempotent(t *testing.T) {
	s, _, _, _ := newStack(t, config.Memory{})
	ctx := context.Background()
	proposed, err := s.Propose(ctx, "codex", "style", "Prefer compact answers.\n", CandidateOptions{
		ID: "c-style", Scope: "cloudfs", SourcePaths: []string{"/work/design.md"}, Replaces: []string{"old-style"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposed.Status != "pending" || proposed.Version == "" {
		t.Fatalf("proposed: %+v", proposed)
	}
	if _, err := s.Get(ctx, "codex", "style"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("candidate became a fact before review: %v", err)
	}
	again, err := s.Propose(ctx, "codex", "style", "Prefer compact answers.\n", CandidateOptions{
		ID: "c-style", Scope: "cloudfs", SourcePaths: []string{"/work/design.md"}, Replaces: []string{"old-style"},
	})
	if err != nil || again.Version != proposed.Version {
		t.Fatalf("idempotent propose: %+v %v", again, err)
	}
	if _, err := s.Propose(ctx, "codex", "style", "different\n", CandidateOptions{ID: "c-style"}); !errors.Is(err, ErrVersionChanged) {
		t.Fatalf("candidate id reused with different content: %v", err)
	}

	reviewed, err := s.Review(ctx, "codex", proposed.ID, "accept", proposed.Version)
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.Decision != "accept" || reviewed.FactVersion == "" || reviewed.ReviewedAt == nil {
		t.Fatalf("reviewed: %+v", reviewed)
	}
	fact, err := s.Get(ctx, "codex", "style")
	if err != nil || fact.Content != proposed.Content || fact.Meta.Scope != "cloudfs" || len(fact.Meta.Replaces) != 1 {
		t.Fatalf("accepted fact: %+v %v", fact, err)
	}
	retry, err := s.Review(ctx, "codex", proposed.ID, "accept", proposed.Version)
	if err != nil || retry.FactVersion != reviewed.FactVersion {
		t.Fatalf("idempotent review: %+v %v", retry, err)
	}
	if _, err := s.Review(ctx, "codex", proposed.ID, "reject", ""); err == nil {
		t.Fatal("opposite review decision was accepted")
	}
	pending, _, _ := s.Candidates(ctx, "codex", "pending", "", 10)
	done, _, _ := s.Candidates(ctx, "codex", "reviewed", "", 10)
	if len(pending) != 0 || len(done) != 1 || done[0].ID != proposed.ID {
		t.Fatalf("pending=%+v reviewed=%+v", pending, done)
	}
}

func TestMemoryIndexExcludesCandidateInbox(t *testing.T) {
	rule := IndexRule("/work/.agent")
	m := index.NewMatcher([]index.Rule{rule}, nil)
	if _, ok := m.Match("/work/.agent/memory/codex/candidates/pending/c-style.md", 100); ok {
		t.Fatal("pending candidate is searchable")
	}
	if _, ok := m.Match("/work/.agent/memory/codex/candidates/reviewed/c-style.md", 100); ok {
		t.Fatal("reviewed candidate is searchable")
	}
	if _, ok := m.Match("/work/.agent/memory/codex/facts/style.md", 100); !ok {
		t.Fatal("durable fact is not searchable")
	}
}

func TestRejectCandidateDoesNotWriteFact(t *testing.T) {
	s, _, _, _ := newStack(t, config.Memory{})
	ctx := context.Background()
	c, err := s.Propose(ctx, "codex", "temporary", "do not retain\n", CandidateOptions{ID: "c-temporary"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Review(ctx, "codex", c.ID, "reject", c.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "codex", "temporary"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected candidate became a fact: %v", err)
	}
}

func TestCandidatesAreBoundedAndPaginated(t *testing.T) {
	s, _, _, _ := newStack(t, config.Memory{})
	ctx := context.Background()
	for _, id := range []string{"c-a", "c-b", "c-c"} {
		if _, err := s.Propose(ctx, "codex", "fact-"+id[2:], id+"\n", CandidateOptions{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	first, next, err := s.Candidates(ctx, "codex", "pending", "", 2)
	if err != nil || len(first) != 2 || next == "" {
		t.Fatalf("first page: %+v next=%q err=%v", first, next, err)
	}
	second, done, err := s.Candidates(ctx, "codex", "pending", next, 2)
	if err != nil || len(second) != 1 || done != "" {
		t.Fatalf("second page: %+v next=%q err=%v", second, done, err)
	}
}

func TestAcceptCandidateDoesNotOverwriteFactChangedSinceProposal(t *testing.T) {
	s, _, _, _ := newStack(t, config.Memory{})
	ctx := context.Background()
	c, err := s.Propose(ctx, "codex", "style", "candidate\n", CandidateOptions{ID: "c-style"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "codex", "style", "newer fact\n", PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Review(ctx, "codex", c.ID, "accept", c.Version); !errors.Is(err, ErrVersionChanged) {
		t.Fatalf("accepted over a newer fact: %v", err)
	}
	fact, err := s.Get(ctx, "codex", "style")
	if err != nil || fact.Content != "newer fact\n" {
		t.Fatalf("newer fact changed: %+v %v", fact, err)
	}
}

func TestAcceptCandidateDoesNotAdoptConcurrentFactWithDifferentMetadata(t *testing.T) {
	s, _, _, _ := newStack(t, config.Memory{})
	ctx := context.Background()
	c, err := s.Propose(ctx, "codex", "style", "same body\n", CandidateOptions{ID: "c-style"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "codex", "style", "same body\n", PutOptions{Scope: "another-project"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Review(ctx, "codex", c.ID, "accept", c.Version); !errors.Is(err, ErrVersionChanged) {
		t.Fatalf("adopted concurrently created fact with different metadata: %v", err)
	}
}
