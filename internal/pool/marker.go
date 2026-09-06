package pool

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
)

// A marker is the one file of the pool's own that lives on a member:
// <root>/.cloudfs-pool.json. It says which pool the member belongs to and
// with what settings, and it is how a second machine joins the same pool
// from any one member (`cloudfs pool join`). It carries no index — the
// members are the truth, the index is rebuilt by listing — and no
// credential.
const markerName = hiddenPrefix + ".json"

// Marker is what a member's marker file says.
type Marker struct {
	PoolID        string    `json:"pool_id"`
	PoolName      string    `json:"pool_name"`
	CreatedAt     time.Time `json:"created_at"`
	SettingsEpoch int64     `json:"settings_epoch"`
	Settings      Settings  `json:"settings"`
	// Members lists the remotes the writing machine knew, by name, so a
	// joining machine can tell what it is missing. Names only: another
	// machine's credentials are its own.
	Members   []string  `json:"members"`
	WrittenBy string    `json:"written_by"`
	WrittenAt time.Time `json:"written_at"`
}

// Settings is the part of the pool configuration every machine must
// agree on.
type Settings struct {
	Replicas    int           `json:"replicas"`
	MinReplicas int           `json:"min_replicas"`
	GCGrace     time.Duration `json:"gc_grace"`
	TrimGrace   time.Duration `json:"trim_grace"`
}

func settingsOf(c config.Pool) Settings {
	return Settings{Replicas: c.Replicas, MinReplicas: c.MinReplicas, GCGrace: c.GCGrace, TrimGrace: c.TrimGrace}
}

// epochOf derives a settings epoch: the time the settings last changed on
// this machine, kept in the index so it rises only when they change.
func (p *Pool) settingsEpoch(ctx context.Context) int64 {
	want := settingsOf(p.settings)
	b, _ := json.Marshal(want)
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:8])
	stored, _ := p.metaGet(ctx, "settings_digest")
	epochStr, _ := p.metaGet(ctx, "settings_epoch")
	var epoch int64
	fmt.Sscan(epochStr, &epoch)
	if stored != digest {
		epoch = p.now().UnixNano()
		_ = p.metaSet(ctx, "settings_digest", digest)
		_ = p.metaSet(ctx, "settings_epoch", fmt.Sprint(epoch))
	}
	return epoch
}

// ID returns the pool's identity, minted once per index. A joining machine
// adopts the id it finds in a marker instead.
func (p *Pool) ID(ctx context.Context) string {
	if id, _ := p.metaGet(ctx, "pool_id"); id != "" {
		return id
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	id := "pool-" + hex.EncodeToString(b[:])
	_ = p.metaSet(ctx, "pool_id", id)
	return id
}

// MachineID identifies this index for the marker's written_by.
func (p *Pool) MachineID(ctx context.Context) string {
	if id, _ := p.metaGet(ctx, "machine_id"); id != "" {
		return id
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	_ = p.metaSet(ctx, "machine_id", id)
	return id
}

// marker builds what this machine would write.
func (p *Pool) marker(ctx context.Context) Marker {
	m := Marker{PoolID: p.ID(ctx), PoolName: p.name, SettingsEpoch: p.settingsEpoch(ctx), Settings: settingsOf(p.settings),
		Members: p.Members(), WrittenBy: p.MachineID(ctx), WrittenAt: p.now()}
	if created, _ := p.metaGet(ctx, "created_at"); created != "" {
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	} else {
		m.CreatedAt = p.now()
		_ = p.metaSet(ctx, "created_at", m.CreatedAt.Format(time.RFC3339Nano))
	}
	return m
}

// ReadMarker reads the marker on one member, if any.
func ReadMarker(ctx context.Context, p provider.Provider, root string) (Marker, error) {
	rootID, err := resolveRoot(ctx, p, root)
	if err != nil {
		return Marker{}, err
	}
	entries, err := listAllOf(ctx, p, rootID)
	if err != nil {
		return Marker{}, err
	}
	for _, e := range entries {
		if e.Name != markerName || e.Kind != provider.KindFile {
			continue
		}
		rc, err := p.ReadRange(ctx, e.ID, e.Version, 0, e.Size)
		if err != nil {
			return Marker{}, err
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		if err != nil {
			return Marker{}, err
		}
		var m Marker
		if err := json.Unmarshal(b, &m); err != nil {
			return Marker{}, fmt.Errorf("pool: marker on %s: %w", p.Name(), err)
		}
		return m, nil
	}
	return Marker{}, fmt.Errorf("%w: no pool marker on %s", provider.ErrNotFound, p.Name())
}

// resolveRoot walks a configured root path from the provider's own root.
func resolveRoot(ctx context.Context, p provider.Provider, root string) (string, error) {
	id := provider.RootOf(p)
	root, err := cleanPath(root)
	if err != nil {
		return "", err
	}
	if root == "/" {
		return id, nil
	}
	for _, seg := range splitSegments(root) {
		entries, err := listAllOf(ctx, p, id)
		if err != nil {
			return "", err
		}
		next := ""
		for _, e := range entries {
			if e.Name == seg && e.Kind == provider.KindDir {
				next = e.ID
				break
			}
		}
		if next == "" {
			return "", fmt.Errorf("%w: %s on %s", provider.ErrNotFound, root, p.Name())
		}
		id = next
	}
	return id, nil
}

func splitSegments(p string) []string {
	var out []string
	cur := ""
	for _, r := range p {
		if r == '/' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// listAllOf enumerates one directory on any provider.
func listAllOf(ctx context.Context, p provider.Provider, dirID string) ([]provider.Entry, error) {
	var out []provider.Entry
	if sl, ok := p.(provider.StreamLister); ok && p.Capabilities().StreamList {
		delivered := false
		err := sl.ListStream(ctx, dirID, func(e provider.Entry) error { delivered = true; out = append(out, e); return nil })
		if err == nil {
			return out, nil
		}
		if delivered || !errors.Is(err, provider.ErrUnsupported) {
			return nil, err
		}
		out = out[:0]
	}
	cursor := ""
	for {
		entries, next, err := p.List(ctx, dirID, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// WriteMarkers puts this machine's marker on every reachable member whose
// marker is missing or older. A marker with a higher settings epoch than
// ours is left alone and reported as a notice: another machine changed the
// pool's settings, and the person who runs this one decides.
func (p *Pool) WriteMarkers(ctx context.Context) error {
	mine := p.marker(ctx)
	body, err := json.MarshalIndent(mine, "", "  ")
	if err != nil {
		return err
	}
	probe := p.probeInterval()
	var firstErr error
	for _, m := range p.members {
		if st := m.state(); st == provider.HealthDisabled || !m.usable(probe) {
			continue
		}
		existing, err := ReadMarker(ctx, m.p, m.root)
		switch {
		case err == nil && existing.PoolID != "" && existing.PoolID != mine.PoolID:
			p.addNotice(fmt.Sprintf("member %s carries the marker of another pool (%s, %q); it was not overwritten", m.name, existing.PoolID, existing.PoolName))
			continue
		case err == nil && existing.SettingsEpoch > mine.SettingsEpoch:
			p.addNotice(fmt.Sprintf("member %s carries newer pool settings (replicas=%d, min_replicas=%d) written by %s; this machine's config differs", m.name, existing.Settings.Replicas, existing.Settings.MinReplicas, existing.WrittenBy))
			continue
		case err == nil && existing.SettingsEpoch == mine.SettingsEpoch && existing.PoolID == mine.PoolID:
			continue
		case err != nil && !errors.Is(err, provider.ErrNotFound):
			if unreachable(err) {
				m.note(err)
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := p.putMarker(ctx, m, body); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// putMarker writes the marker file on one member.
func (p *Pool) putMarker(ctx context.Context, m *member, body []byte) error {
	rootID, err := p.ensureRoot(ctx, m)
	if err != nil {
		return err
	}
	caps := m.p.Capabilities()
	if sp, ok := m.p.(provider.SinglePutter); ok && caps.SinglePutMax >= int64(len(body)) && caps.SinglePutMax > 0 {
		_, err := sp.PutFile(ctx, rootID, markerName, bytes.NewReader(body), int64(len(body)), nil)
		m.note(err)
		return err
	}
	sess, err := m.p.BeginUpload(ctx, rootID, markerName, int64(len(body)), nil)
	m.note(err)
	if err != nil {
		return err
	}
	if sess.RapidDone {
		return nil
	}
	pt, err := m.p.UploadPart(ctx, sess, 0, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		m.note(err)
		return err
	}
	_, err = m.p.CompleteUpload(ctx, sess, []provider.PartToken{pt})
	m.note(err)
	return err
}

// Notices are things a person should see about the pool's state that are
// not divergences of a path: another machine's settings, a member that
// belongs elsewhere.
func (p *Pool) Notices() []string {
	p.noticeMu.Lock()
	defer p.noticeMu.Unlock()
	return append([]string(nil), p.notices...)
}

func (p *Pool) addNotice(s string) {
	p.noticeMu.Lock()
	defer p.noticeMu.Unlock()
	for _, n := range p.notices {
		if n == s {
			return
		}
	}
	p.notices = append(p.notices, s)
	if len(p.notices) > 50 {
		p.notices = p.notices[len(p.notices)-50:]
	}
}

// AdoptMarker takes the pool id from a marker found on a member, for a
// machine that joins an existing pool: from then on both machines call the
// pool the same thing. It is refused once this index has minted its own id
// and files under it.
func (p *Pool) AdoptMarker(ctx context.Context, m Marker) error {
	if m.PoolID == "" {
		return errors.New("pool: marker has no pool id")
	}
	if id, _ := p.metaGet(ctx, "pool_id"); id != "" && id != m.PoolID {
		var n int
		_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entries`).Scan(&n)
		if n > 0 {
			return fmt.Errorf("pool: this index already belongs to %s with %d entries; rebuild it to join %s", id, n, m.PoolID)
		}
	}
	if err := p.metaSet(ctx, "pool_id", m.PoolID); err != nil {
		return err
	}
	if !m.CreatedAt.IsZero() {
		_ = p.metaSet(ctx, "created_at", m.CreatedAt.Format(time.RFC3339Nano))
	}
	return nil
}
