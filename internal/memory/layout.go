package memory

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"cloudfs/internal/vfs"
)

// Layouts (docs/agent-first-design.md §8.3, T-56). v1 keeps one agent
// directory per agent name: memory/<agent>/. v2 keys by the person the
// agent acts for as well: memory/<owner>/<agent>/, with memory/shared/
// unchanged, so two people sharing one drive account do not write into
// each other's memories. Which layout a drive is in is a fact about the
// drive, not about one machine's configuration, so it lives in the drive:
// memory/.layout holds "v2" once Migrate has moved every directory. The
// configuration's memory.layout, when set, overrides the marker (for a
// fresh drive that should start in v2, or a test).

const (
	LayoutV1 = "v1"
	LayoutV2 = "v2"
	// layoutMarker is the file under memory/ that says the drive is v2.
	layoutMarker = ".layout"
	// layoutCache is how long a marker read is trusted before the drive
	// is asked again; another device's migration shows up within it.
	layoutCache = 30 * time.Second
)

// Layout is the layout in force: the configuration's, else the marker's,
// else v1.
func (s *Store) Layout(ctx context.Context) string {
	if s.cfg.Layout != "" {
		return s.cfg.Layout
	}
	if s.cfg.Root == "" {
		return LayoutV1
	}
	s.layoutMu.Lock()
	defer s.layoutMu.Unlock()
	if s.layoutAt.IsZero() || s.now().Sub(s.layoutAt) > layoutCache {
		s.layout = LayoutV1
		if data, err := s.fs.ReadFileRange(ctx, s.LayoutMarkerPath(), 0, 64); err == nil && strings.TrimSpace(string(data)) == LayoutV2 {
			s.layout = LayoutV2
		}
		s.layoutAt = s.now()
	}
	return s.layout
}

// LayoutMarkerPath is <root>/memory/.layout.
func (s *Store) LayoutMarkerPath() string { return path.Join(s.cfg.Root, "memory", layoutMarker) }

// Key names one memory directory: an agent, and in v2 the owner it acts
// for. Top-level shared is drive-wide; <owner>/shared is personal memory
// shared by that owner's agents.
type Key struct {
	Owner string
	Agent string
}

// String is the key as the tools and the console spell it: "agent" in v1,
// top-level "shared", or "owner/agent" in v2.
func (k Key) String() string {
	if k.Owner == "" {
		return k.Agent
	}
	return k.Owner + "/" + k.Agent
}

// ParseKey reads an agent argument: "agent", "owner/agent", "personal" or
// "shared". Personal is the current owner's cross-agent area in v2 and is
// an alias for shared in the single-owner v1 layout.
// In v2 an unqualified agent is the caller's own (defaultOwner); in v1 an
// owner is refused. Names are validated.
func ParseKey(layout, arg, defaultOwner string) (Key, error) {
	return parseKey(layout, arg, defaultOwner, true)
}

// ParseIdentity resolves an agent name derived from the caller rather than an
// explicit tool argument. "personal" remains a valid client name here; only
// an explicit agent=personal uses the cross-agent alias.
func ParseIdentity(layout, arg, defaultOwner string) (Key, error) {
	return parseKey(layout, arg, defaultOwner, false)
}

func parseKey(layout, arg, defaultOwner string, personalAlias bool) (Key, error) {
	if personalAlias && arg == PersonalAgent {
		if layout == LayoutV2 {
			if !ValidName(defaultOwner) || defaultOwner == SharedAgent {
				return Key{}, fmt.Errorf("owner %q: %w", defaultOwner, ErrBadName)
			}
			return Key{Owner: defaultOwner, Agent: SharedAgent}, nil
		}
		return Key{Agent: SharedAgent}, nil
	}
	owner, agent, qualified := strings.Cut(arg, "/")
	if !qualified {
		owner, agent = "", arg
	}
	if !ValidName(agent) {
		return Key{}, fmt.Errorf("agent %q: %w", agent, ErrBadName)
	}
	if agent == SharedAgent && !qualified {
		return Key{Agent: SharedAgent}, nil
	}
	if layout != LayoutV2 {
		if qualified {
			return Key{}, fmt.Errorf("agent %q: memory layout v1 has no owners; run cloudfs memory migrate first", arg)
		}
		return Key{Agent: agent}, nil
	}
	if !qualified {
		owner = defaultOwner
	}
	if !ValidName(owner) || owner == SharedAgent {
		return Key{}, fmt.Errorf("owner %q: %w", owner, ErrBadName)
	}
	return Key{Owner: owner, Agent: agent}, nil
}

// Migrate moves a v1 tree to v2 under owner: every memory/<agent>/ but
// shared becomes memory/<owner>/<agent>/, one rename each, in name order;
// then the marker is written. Every step is idempotent — a directory
// already moved is skipped, a marker already written is left — so a run
// interrupted anywhere is finished by the next. It returns the agents
// moved by this run. A tree already in v2 is a no-op.
func (s *Store) Migrate(ctx context.Context, owner string) ([]string, error) {
	if s.cfg.Root == "" {
		return nil, ErrNoRoot
	}
	if !ValidName(owner) || owner == SharedAgent {
		return nil, fmt.Errorf("owner %q: %w", owner, ErrBadName)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	memDir := path.Join(s.cfg.Root, "memory")
	dirs, err := s.fs.ReadDirPath(ctx, memDir)
	if errors.Is(err, vfs.ErrNotFound) {
		dirs = nil
	} else if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range dirs {
		if d.IsDir && ValidName(d.Name) && d.Name != SharedAgent && d.Name != owner {
			names = append(names, d.Name)
		}
	}
	sort.Strings(names)
	var moved []string
	if len(names) > 0 {
		ownerDir := path.Join(memDir, owner)
		if err := s.mkdirAll(ctx, ownerDir); err != nil {
			return nil, err
		}
		parent, err := s.fs.StatPath(ctx, memDir)
		if err != nil {
			return nil, err
		}
		target, err := s.fs.StatPath(ctx, ownerDir)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if _, err := s.fs.StatPath(ctx, path.Join(ownerDir, name)); err == nil {
				// Moved by an earlier run; the source, if it still exists,
				// is what that run was interrupted before removing — the
				// rename would clobber, so it stays for a person to look at.
				continue
			}
			if err := s.fs.Rename(ctx, parent.Ino, name, target.Ino, name); err != nil {
				return moved, fmt.Errorf("move %s: %w", name, err)
			}
			moved = append(moved, name)
		}
	}
	if data, err := s.fs.ReadFileRange(ctx, s.LayoutMarkerPath(), 0, 64); err != nil || strings.TrimSpace(string(data)) != LayoutV2 {
		if err := s.mkdirAll(ctx, memDir); err != nil {
			return moved, err
		}
		if _, err := s.fs.WriteFile(ctx, s.LayoutMarkerPath(), []byte(LayoutV2+"\n"), false); err != nil {
			return moved, err
		}
	}
	s.layoutMu.Lock()
	s.layout, s.layoutAt = LayoutV2, s.now()
	s.layoutMu.Unlock()
	return moved, nil
}
