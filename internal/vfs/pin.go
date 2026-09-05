package vfs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
)

// PinPolicy is a persistent, path-based retention rule. Renaming a file out
// of a pinned subtree does not move the rule. Overlapping rules are additive.
type PinPolicy struct {
	Path       string `json:"path"`
	Recursive  bool   `json:"recursive"`
	Configured bool   `json:"configured"`
}

func pinPath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\\") || len(p) > 4096 {
		return "", errors.New("vfs: pin path must be an absolute virtual path")
	}
	return path.Clean(p), nil
}

// loadPins restores protection without listing or downloading from a provider.
func (f *FS) loadPins(ctx context.Context) error {
	f.pinMu.Lock()
	defer f.pinMu.Unlock()
	return f.loadPinsLocked(ctx)
}

func (f *FS) loadPinsLocked(ctx context.Context) error {
	pins, err := f.meta.Pins(ctx)
	if err != nil {
		return err
	}
	for _, m := range f.mounts {
		if m.Pin {
			pins = append(pins, meta.Pin{Path: m.Prefix, Recursive: true, Mode: "keep"})
		}
	}
	f.pins = pins
	f.hasPins.Store(len(pins) > 0)
	return f.reconcilePinsLocked(ctx)
}

func (f *FS) coversPinLocked(p string) bool {
	for _, pin := range f.pins {
		if p == pin.Path || pin.Recursive && strings.HasPrefix(p, strings.TrimSuffix(pin.Path, "/")+"/") {
			return true
		}
	}
	return false
}

func (f *FS) pathPinned(p string) bool {
	if !f.hasPins.Load() {
		return false
	}
	f.pinMu.Lock()
	defer f.pinMu.Unlock()
	return f.coversPinLocked(p)
}

// reconcilePinsLocked also releases obsolete identities and paths after a
// rename, replacement or unpin. Temporary upload pins are never touched.
func (f *FS) reconcilePinsLocked(ctx context.Context) error {
	for _, key := range f.cache.Keys() {
		keep, err := f.keyPinnedLocked(ctx, key)
		if err != nil {
			return err
		}
		f.cache.SetUserPin(key, keep)
	}
	return nil
}

func (f *FS) keyPinnedLocked(ctx context.Context, key cache.FileKey) (bool, error) {
	if len(f.pins) == 0 {
		return false, nil
	}
	nodes, err := f.meta.Aliases(ctx, key.Remote, key.RemoteID)
	if err != nil {
		return false, err
	}
	for _, n := range nodes {
		if n.Version != key.Version {
			continue
		}
		p, err := f.meta.Path(ctx, n.Ino)
		if errors.Is(err, meta.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if f.coversPinLocked(p) {
			return true, nil
		}
	}
	return false, nil
}

// protectPinned applies the policy before any block is admitted, including
// new remote versions and files first discovered after a directory was pinned.
func (f *FS) protectPinned(ctx context.Context, n meta.Node) (bool, error) {
	if !f.hasPins.Load() {
		return false, nil
	}
	f.pinMu.Lock()
	defer f.pinMu.Unlock()
	p, err := f.meta.Path(ctx, n.Ino)
	if err != nil {
		return false, err
	}
	keep := f.coversPinLocked(p)
	key := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
	if !keep {
		keep, err = f.keyPinnedLocked(ctx, key)
		if err != nil {
			return false, err
		}
	}
	f.cache.SetUserPin(key, keep)
	return keep, nil
}

// Pin persists intent before downloading. Failure retains the policy and the
// blocks already fetched; retrying resumes missing blocks. It never reports a
// successful pin after silently evicting earlier blocks to make room.
func (f *FS) Pin(ctx context.Context, p string) error {
	p, err := pinPath(p)
	if err != nil {
		return err
	}
	n, err := f.resolve(ctx, p)
	if err != nil {
		return err
	}
	f.pinMu.Lock()
	err = f.meta.AddPin(ctx, meta.Pin{Path: p, Recursive: n.IsDir(), Mode: "keep"})
	if err == nil {
		err = f.loadPinsLocked(ctx)
	}
	f.pinMu.Unlock()
	if err != nil {
		return err
	}
	if err := f.fillPinned(ctx, p); err != nil {
		return fmt.Errorf("pin intent saved for %s, but download is incomplete; retry pin to resume: %w", p, err)
	}
	f.pinMu.Lock()
	covered := f.coversPinLocked(p)
	f.pinMu.Unlock()
	if !covered {
		return errors.New("vfs: pin was removed while downloading")
	}
	return nil
}

// Unpin removes exactly this rule, without deleting cached content. An
// ancestor, child or configuration rule can still retain some of the content.
// No provider call is needed, even if the path was renamed or deleted.
func (f *FS) Unpin(ctx context.Context, p string) error {
	p, err := pinPath(p)
	if err != nil {
		return err
	}
	f.pinMu.Lock()
	defer f.pinMu.Unlock()
	for _, m := range f.mounts {
		if m.Pin && m.Prefix == p {
			return errors.New("vfs: this pin is set by mount configuration; change the configuration and restart")
		}
	}
	if err := f.meta.RemovePin(ctx, p); err != nil {
		return err
	}
	return f.loadPinsLocked(ctx)
}

func (f *FS) PinPolicies() []PinPolicy {
	f.pinMu.Lock()
	defer f.pinMu.Unlock()
	out := make([]PinPolicy, 0, len(f.pins))
	for _, pin := range f.pins {
		p := PinPolicy{Path: pin.Path, Recursive: pin.Recursive}
		for _, m := range f.mounts {
			if m.Pin && m.Prefix == pin.Path {
				p.Configured = true
			}
		}
		out = append(out, p)
	}
	return out
}

func (f *FS) wakePins() {
	if !f.hasPins.Load() {
		return
	}
	select {
	case f.pinWake <- struct{}{}:
	default:
	}
}

// StartPins resumes interrupted pins and follows changes within the normal
// directory TTL/delta policy. It is started by the owner, after journal recovery,
// not by a CLI process inspecting a live daemon's files.
func (f *FS) StartPins(ctx context.Context, interval time.Duration) {
	f.pinMu.Lock()
	defer f.pinMu.Unlock()
	if f.pinCancel != nil || f.pinClosed {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	ctx, f.pinCancel = context.WithCancel(ctx)
	f.pinWG.Add(1)
	go func() {
		defer f.pinWG.Done()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			if err := f.RefreshPins(ctx); err != nil {
				f.pinMu.Lock()
				f.pinError = "pinned content is incomplete; retry pin to inspect the failure"
				f.pinMu.Unlock()
			} else {
				f.pinMu.Lock()
				f.pinError = ""
				f.pinMu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			case <-f.pinWake:
			}
		}
	}()
}

// RefreshPins is also exposed for deterministic recovery and delta tests.
func (f *FS) RefreshPins(ctx context.Context) error {
	if !f.hasPins.Load() {
		return nil
	}
	f.pinMu.Lock()
	err := f.reconcilePinsLocked(ctx)
	f.pinMu.Unlock()
	if err != nil {
		return err
	}
	var first error
	for _, pin := range f.PinPolicies() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.fillPinned(ctx, pin.Path); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (f *FS) PinWarning() string {
	f.pinMu.Lock()
	defer f.pinMu.Unlock()
	return f.pinError
}
