package memory

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"time"

	"cloudfs/internal/vfs"

	"gopkg.in/yaml.v3"
)

// Candidate is a proposed durable fact. Candidates are ordinary Markdown
// files so they sync with the drive, but live outside facts/ and are excluded
// from retrieval until a person explicitly accepts them.
type Candidate struct {
	ID              string     `yaml:"id" json:"id"`
	Name            string     `yaml:"name" json:"name"`
	Agent           string     `yaml:"-" json:"agent"`
	Path            string     `yaml:"-" json:"path"`
	Status          string     `yaml:"status" json:"status"`
	Decision        string     `yaml:"decision,omitempty" json:"decision,omitempty"`
	Description     string     `yaml:"description,omitempty" json:"description,omitempty"`
	Type            string     `yaml:"type,omitempty" json:"type,omitempty"`
	Scope           string     `yaml:"scope,omitempty" json:"scope,omitempty"`
	SourcePaths     []string   `yaml:"source_paths,omitempty" json:"source_paths,omitempty"`
	SourceSession   string     `yaml:"source_session,omitempty" json:"source_session,omitempty"`
	Replaces        []string   `yaml:"replaces,omitempty" json:"replaces,omitempty"`
	ExpiresAt       *time.Time `yaml:"expires_at,omitempty" json:"expires_at,omitempty"`
	ProposedAt      time.Time  `yaml:"proposed_at" json:"proposed_at"`
	ReviewedAt      *time.Time `yaml:"reviewed_at,omitempty" json:"reviewed_at,omitempty"`
	FactVersion     string     `yaml:"fact_version,omitempty" json:"fact_version,omitempty"`
	BaseFactVersion string     `yaml:"base_fact_version,omitempty" json:"base_fact_version,omitempty"`
	BaseFactAbsent  bool       `yaml:"base_fact_absent,omitempty" json:"base_fact_absent,omitempty"`
	BaseFactMeta    *Meta      `yaml:"base_fact_meta,omitempty" json:"base_fact_meta,omitempty"`
	Content         string     `yaml:"-" json:"content"`
	Version         string     `yaml:"-" json:"version"`
}

// CandidateOptions are the metadata copied into a fact when accepted.
type CandidateOptions struct {
	ID            string
	Description   string
	Type          string
	Scope         string
	SourcePaths   []string
	SourceSession string
	Replaces      []string
	ExpiresAt     *time.Time
}

func (s *Store) candidateDir(agent, status string) string {
	return path.Join(s.AgentDir(agent), "candidates", status)
}

func (s *Store) CandidatePath(agent, status, id string) string {
	return path.Join(s.candidateDir(agent, status), id+".md")
}

// Propose creates a pending candidate. Supplying ID makes retries
// idempotent; an existing ID is returned only when the proposal is identical.
func (s *Store) Propose(ctx context.Context, agent, name, content string, opt CandidateOptions) (Candidate, error) {
	if err := s.check(agent, name, true); err != nil {
		return Candidate{}, err
	}
	for _, replaced := range opt.Replaces {
		if !ValidName(replaced) || replaced == name {
			return Candidate{}, fmt.Errorf("replacement %q: %w", replaced, ErrBadName)
		}
	}
	id := opt.ID
	if id == "" {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return Candidate{}, err
		}
		id = "c-" + hex.EncodeToString(raw[:])
	}
	if !ValidName(id) {
		return Candidate{}, fmt.Errorf("candidate id %q: %w", id, ErrBadName)
	}
	now := s.now().UTC()
	c := Candidate{ID: id, Name: name, Agent: agent, Status: "pending", Description: opt.Description,
		Type: opt.Type, Scope: opt.Scope, SourcePaths: append([]string(nil), opt.SourcePaths...),
		SourceSession: opt.SourceSession, Replaces: append([]string(nil), opt.Replaces...),
		ExpiresAt: opt.ExpiresAt, ProposedAt: now, Content: content}
	base, found, err := s.readFact(ctx, agent, name)
	if err != nil {
		return Candidate{}, err
	}
	if found {
		c.BaseFactVersion = Version(base)
		baseFact := s.factOf(agent, name, base)
		baseMeta := baseFact.Meta
		c.BaseFactMeta = &baseMeta
	} else {
		c.BaseFactAbsent = true
	}
	file, err := renderCandidate(c)
	if err != nil {
		return Candidate{}, err
	}
	if int64(len(file)) > int64(s.cfg.MaxFactBytes) {
		return Candidate{}, fmt.Errorf("%w: candidate would be %d bytes, over memory.max_fact_bytes (%d)", ErrTooLarge, len(file), s.cfg.MaxFactBytes)
	}
	s.reviewMu.Lock()
	defer s.reviewMu.Unlock()
	for _, status := range []string{"reviewed", "pending"} {
		existing, found, readErr := s.readCandidate(ctx, agent, status, id)
		if readErr != nil {
			return Candidate{}, readErr
		}
		if found {
			if !sameCandidateProposal(existing, c) {
				return Candidate{}, fmt.Errorf("candidate %s already exists with a different proposal: %w", id, ErrVersionChanged)
			}
			return existing, nil
		}
	}
	if err := s.mkdirAll(ctx, s.candidateDir(agent, "pending")); err != nil {
		return Candidate{}, err
	}
	c.Path = s.CandidatePath(agent, "pending", id)
	if _, err := s.fs.WriteFile(ctx, c.Path, file, false); err != nil {
		return Candidate{}, err
	}
	c.Version = Version(file)
	return c, nil
}

func sameCandidateProposal(a, b Candidate) bool {
	if a.Name != b.Name || a.Content != b.Content || a.Description != b.Description || a.Type != b.Type ||
		a.Scope != b.Scope || a.SourceSession != b.SourceSession || !sameStrings(a.SourcePaths, b.SourcePaths) ||
		!sameStrings(a.Replaces, b.Replaces) {
		return false
	}
	if a.ExpiresAt == nil || b.ExpiresAt == nil {
		return a.ExpiresAt == nil && b.ExpiresAt == nil
	}
	return a.ExpiresAt.Equal(*b.ExpiresAt)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Candidates pages through pending or reviewed candidates in stable file-name
// order. next is empty after the last page.
func (s *Store) Candidates(ctx context.Context, agent, status, cursor string, limit int) ([]Candidate, string, error) {
	if err := s.check(agent, "", false); err != nil {
		return nil, "", err
	}
	if status == "" {
		status = "pending"
	}
	if status != "pending" && status != "reviewed" {
		return nil, "", fmt.Errorf("candidate status must be pending or reviewed")
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, vfs.MaxDirectoryPageSize)
	opt, err := vfs.ParseDirectoryCursor(cursor, limit)
	if errors.Is(err, vfs.ErrInvalidCursor) {
		return nil, "", ErrInvalidCursor
	}
	if err != nil {
		return nil, "", err
	}
	opt.Count = false
	page, err := s.fs.ReadDirPagePath(ctx, s.candidateDir(agent, status), opt)
	if errors.Is(err, vfs.ErrNotFound) {
		return []Candidate{}, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	out := []Candidate{}
	for _, entry := range page.Entries {
		id, ok := factName(entry.Name)
		if entry.IsDir || !ok {
			continue
		}
		if status == "pending" {
			if _, reviewed, readErr := s.readCandidate(ctx, agent, "reviewed", id); readErr != nil {
				return nil, "", readErr
			} else if reviewed {
				continue
			}
		}
		c, found, readErr := s.readCandidate(ctx, agent, status, id)
		if readErr != nil {
			return nil, "", readErr
		}
		if found {
			out = append(out, c)
			if len(out) == limit {
				break
			}
		}
	}
	return out, vfs.NextDirectoryCursor(page), nil
}

// Review accepts or rejects a pending candidate. Reviewed files are retained
// so repeating the same decision is idempotent and auditable.
func (s *Store) Review(ctx context.Context, agent, id, decision, expectedVersion string) (Candidate, error) {
	if err := s.check(agent, "", false); err != nil {
		return Candidate{}, err
	}
	if !ValidName(id) {
		return Candidate{}, fmt.Errorf("candidate id %q: %w", id, ErrBadName)
	}
	if decision != "accept" && decision != "reject" {
		return Candidate{}, errors.New("decision must be accept or reject")
	}
	s.reviewMu.Lock()
	defer s.reviewMu.Unlock()
	if reviewed, found, err := s.readCandidate(ctx, agent, "reviewed", id); err != nil {
		return Candidate{}, err
	} else if found {
		if reviewed.Decision != decision {
			return Candidate{}, fmt.Errorf("candidate %s was already %sed", id, reviewed.Decision)
		}
		s.removePendingCandidate(ctx, agent, id)
		return reviewed, nil
	}
	c, found, err := s.readCandidate(ctx, agent, "pending", id)
	if err != nil {
		return Candidate{}, err
	}
	if !found {
		return Candidate{}, fmt.Errorf("%w: candidate %s", ErrNotFound, id)
	}
	if expectedVersion != "" && c.Version != expectedVersion {
		return Candidate{}, fmt.Errorf("%w (current candidate version %q)", ErrVersionChanged, c.Version)
	}
	if decision == "accept" {
		fact, getErr := s.Get(ctx, agent, c.Name)
		if getErr != nil && !errors.Is(getErr, ErrNotFound) {
			return Candidate{}, getErr
		}
		if getErr != nil || !factMatchesCandidate(fact, c) {
			var putErr error
			fact, putErr = s.Put(ctx, agent, c.Name, c.Content, PutOptions{Description: c.Description, Type: c.Type,
				Scope: c.Scope, SourcePaths: c.SourcePaths, SourceSession: c.SourceSession,
				Replaces: c.Replaces, ExpiresAt: c.ExpiresAt, ExpectedVersion: c.BaseFactVersion, ExpectedAbsent: c.BaseFactAbsent})
			if putErr != nil {
				return Candidate{}, putErr
			}
		}
		c.FactVersion = fact.Version
	}
	now := s.now().UTC()
	c.Status, c.Decision, c.ReviewedAt = "reviewed", decision, &now
	file, err := renderCandidate(c)
	if err != nil {
		return Candidate{}, err
	}
	if err := s.mkdirAll(ctx, s.candidateDir(agent, "reviewed")); err != nil {
		return Candidate{}, err
	}
	c.Path = s.CandidatePath(agent, "reviewed", id)
	if _, err := s.fs.WriteFile(ctx, c.Path, file, false); err != nil {
		return Candidate{}, err
	}
	c.Version = Version(file)
	s.removePendingCandidate(ctx, agent, id)
	return c, nil
}

func factMatchesCandidate(f Fact, c Candidate) bool {
	expected := Meta{Name: c.Name}
	if c.BaseFactMeta != nil {
		expected = *c.BaseFactMeta
		expected.Name = c.Name
	}
	if c.Description != "" {
		expected.Description = c.Description
	}
	if c.Type != "" {
		expected.Type = c.Type
	}
	if c.Scope != "" {
		expected.Scope = c.Scope
	}
	if len(c.SourcePaths) > 0 {
		expected.SourcePaths = c.SourcePaths
	}
	if c.SourceSession != "" {
		expected.SourceSession = c.SourceSession
	}
	if len(c.Replaces) > 0 {
		expected.Replaces = c.Replaces
	}
	if c.ExpiresAt != nil {
		expected.ExpiresAt = c.ExpiresAt
	}
	if f.Content != c.Content || f.Meta.Description != expected.Description || f.Meta.Type != expected.Type ||
		f.Meta.Scope != expected.Scope || f.Meta.SourceSession != expected.SourceSession ||
		!sameStrings(f.Meta.SourcePaths, expected.SourcePaths) || !sameStrings(f.Meta.Replaces, expected.Replaces) {
		return false
	}
	if f.Meta.ExpiresAt == nil || expected.ExpiresAt == nil {
		return f.Meta.ExpiresAt == nil && expected.ExpiresAt == nil
	}
	return f.Meta.ExpiresAt.Equal(*expected.ExpiresAt)
}

func (s *Store) removePendingCandidate(ctx context.Context, agent, id string) {
	if dir, err := s.fs.StatPath(ctx, s.candidateDir(agent, "pending")); err == nil {
		_ = s.fs.Remove(ctx, dir.Ino, id+".md", false)
	}
}

func (s *Store) readCandidate(ctx context.Context, agent, status, id string) (Candidate, bool, error) {
	p := s.CandidatePath(agent, status, id)
	file, err := s.fs.ReadFileRange(ctx, p, 0, int64(s.cfg.MaxFactBytes)+1)
	if errors.Is(err, vfs.ErrNotFound) {
		return Candidate{}, false, nil
	}
	if err != nil {
		return Candidate{}, false, err
	}
	if int64(len(file)) > int64(s.cfg.MaxFactBytes) {
		return Candidate{}, false, ErrTooLarge
	}
	c, err := parseCandidate(file)
	if err != nil {
		return Candidate{}, false, err
	}
	c.Agent, c.Path, c.Version = agent, p, Version(file)
	return c, true, nil
}

func renderCandidate(c Candidate) ([]byte, error) {
	meta := c
	meta.Agent, meta.Path, meta.Content, meta.Version = "", "", "", ""
	block, err := yaml.Marshal(meta)
	if err != nil {
		return nil, err
	}
	file := append([]byte("---\n"), block...)
	file = append(file, []byte("---\n")...)
	file = append(file, []byte(c.Content)...)
	return file, nil
}

func parseCandidate(file []byte) (Candidate, error) {
	if !bytes.HasPrefix(file, []byte(fence)) {
		return Candidate{}, errors.New("candidate has no frontmatter")
	}
	rest := file[len(fence):]
	i := bytes.Index(rest, []byte("\n---\n"))
	if i < 0 {
		return Candidate{}, errors.New("candidate frontmatter is incomplete")
	}
	var c Candidate
	if err := yaml.Unmarshal(rest[:i], &c); err != nil {
		return Candidate{}, err
	}
	c.Content = string(rest[i+5:])
	if !ValidName(c.ID) || !ValidName(c.Name) {
		return Candidate{}, ErrBadName
	}
	return c, nil
}
