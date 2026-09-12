// Package export copies virtual paths out of the mount and onto ordinary
// local storage — a directory, or an external drive (docs/pool-v2.md §5).
//
// It is deliberately not part of the read path. An export never goes through
// vfs.FS.Read: that would count as foreground IO (the yield logic would then
// treat the export as the thing it is supposed to stand aside for) and would
// fill the block cache with bytes nobody is going to read again. It reads the
// mount's provider directly, the way vfs.ResumeCopy does, and it never
// hardlinks or adopts a cache object into the user's directory — the cache's
// whole-object bookkeeping assumes it owns every name it knows, and a file
// edited in place on a removable drive would otherwise corrupt an immutable
// cache entry. Bytes are copied (reflink first, a buffered io.Copy after).
//
// The unit of work is a job: a set of source paths, one destination
// directory, and a durable plan in exports.db. The plan is what makes resume
// exact — a restart re-plans nothing and re-fetches only the ranges the
// per-item bitmap says are still missing.
package export

import (
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"cloudfs/internal/provider"
)

// State is a job's lifecycle state. The terminal ones are done, failed and
// cancelled; purging is the deletion pass that Forget runs.
type State string

const (
	StatePlanning  State = "planning"
	StateRunning   State = "running"
	StatePaused    State = "paused"
	StateDone      State = "done"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
	StatePurging   State = "purging"
)

// Terminal reports whether the manager is finished with a job in this state.
func (s State) Terminal() bool {
	return s == StateDone || s == StateFailed || s == StateCancelled
}

// PauseReason explains a paused job. It is empty in every other state.
type PauseReason string

const (
	PauseNone PauseReason = ""
	// PauseUser: somebody asked. Only a user resume clears it.
	PauseUser PauseReason = "user"
	// PauseDisk: the destination is full, read-only, or simply gone —
	// the case an external drive makes routine. Probed for a return.
	PauseDisk PauseReason = "disk"
	// PauseUnavailable: nothing that holds the data can be reached. Clears
	// itself when the backoff on the first item expires.
	PauseUnavailable PauseReason = "unavailable"
	// PauseRiskControl: the backend signalled a ban risk. Backing off hard
	// is the only safe response.
	PauseRiskControl PauseReason = "risk_control"
	// PauseAuth: credentials no longer work and refreshing did not help.
	PauseAuth PauseReason = "auth"
)

// ItemState is the state of one planned file.
type ItemState string

const (
	ItemPending ItemState = "pending"
	ItemActive  ItemState = "active"
	ItemDone    ItemState = "done"
	ItemSkipped ItemState = "skipped"
	ItemFailed  ItemState = "failed"
)

// Errors callers distinguish.
var (
	ErrNotFound = errors.New("export: no such job")
	// ErrMirrorMarkerMissing refuses a mirror into a directory this export
	// does not already own. A mirror deletes whatever is not in the plan, so
	// without the marker it would be a way to empty any directory on the
	// machine by naming it as a destination.
	ErrMirrorMarkerMissing = errors.New("export: --mirror needs an existing export marker for the same sources in the destination")
	// ErrNotOwner is returned when another process owns exports.db.
	ErrNotOwner = errors.New("export: the export queue is owned by another process")
	// ErrInvalid marks a malformed request.
	ErrInvalid = errors.New("export: invalid request")
)

// JobOptions are the per-job knobs. They are persisted with the job so a resume
// after a config change keeps running the job it was asked for.
type JobOptions struct {
	Mirror bool `json:"mirror"`
	Verify bool `json:"verify"`
	// PreserveMTime stamps the source's modification time on the copy.
	PreserveMTime bool  `json:"preserve_mtime"`
	Transfers     int   `json:"transfers"`
	Streams       int   `json:"streams"`
	RangeSize     int64 `json:"range_size"`
	// MultiRangeMin is the size from which a file gets several streams.
	MultiRangeMin int64 `json:"multi_range_min"`
}

// Binding records the identity of one mount a job's sources came from, so a
// job resumed after the mount was re-pointed or the account rebound does not
// silently continue into different content. It carries no credential.
type Binding struct {
	Prefix         string `json:"prefix"`
	Remote         string `json:"remote"`
	RootID         string `json:"root_id"`
	AccountBinding string `json:"account_binding"`
}

// Job is one export.
type Job struct {
	ID          string
	State       State
	PauseReason PauseReason
	Sources     []string
	Dest        string
	// DestDev is the destination's st_dev at planning time. A different one
	// means a different filesystem is mounted there now — the drive was
	// pulled, or something else was mounted over the path.
	DestDev      uint64
	Options      JobOptions
	MetaIdentity string
	Bindings     []Binding

	FilesTotal   int64
	FilesDone    int64
	FilesSkipped int64
	FilesFailed  int64
	BytesTotal   int64
	BytesDone    int64

	LastError string
	Revision  int64

	CreatedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt time.Time
}

// Item is one planned file or directory.
type Item struct {
	JobID string
	// Rel is the path below Dest, in slash form.
	Rel      string
	VPath    string
	Kind     provider.Kind
	Size     int64
	MTime    time.Time
	Remote   string
	RemoteID string
	Version  string
	HashType string
	Hash     string
	State    ItemState
	// Ranges is the bitmap of completed range_size chunks.
	Ranges    string
	DoneBytes int64
	Attempts  int
	NextAt    time.Time
	LastError string
}

// Extra is one destination entry a mirror plans to delete.
type Extra struct {
	Rel  string
	Kind provider.Kind
}

// Progress is the live view of a running job.
type Progress struct {
	BytesDone  int64
	BytesTotal int64
	// Rate is a 10 s EWMA in bytes per second.
	Rate float64
	ETA  time.Duration
}

// Request is what a caller asks for. Zero-valued tunables fall back to the
// configured defaults.
type Request struct {
	Sources   []string
	Dest      string
	Mirror    bool
	Verify    bool
	Transfers int
	Streams   int
	RangeSize int64
}

// partSuffix names the in-progress copy. A resumed job finds its own bytes
// under this name; nothing reads it as a finished file.
const partSuffix = ".cloudfs-part"

// markerName is the file that proves an export owns a destination directory.
const markerName = ".cloudfs-export.json"

// bitmap marks completed chunks. It is stored as hex so the row stays
// readable in a sqlite shell and survives any text handling on the way.
type bitmap []byte

func parseBitmap(s string, chunks int) bitmap {
	b := make(bitmap, (chunks+7)/8)
	raw, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return b
	}
	copy(b, raw)
	return b
}

func (b bitmap) has(i int) bool {
	byteIdx := i / 8
	if byteIdx < 0 || byteIdx >= len(b) {
		return false
	}
	return b[byteIdx]&(1<<uint(i%8)) != 0
}

func (b bitmap) set(i int) {
	byteIdx := i / 8
	if byteIdx < 0 || byteIdx >= len(b) {
		return
	}
	b[byteIdx] |= 1 << uint(i%8)
}

func (b bitmap) String() string { return hex.EncodeToString(b) }

// chunkCount is how many range_size chunks a file of this size has.
func chunkCount(size, rangeSize int64) int {
	if size <= 0 || rangeSize <= 0 {
		return 0
	}
	return int((size + rangeSize - 1) / rangeSize)
}
