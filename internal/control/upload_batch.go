package control

import (
	"sync"
	"time"
)

// UploadBatch is the console's overall progress: the burst of work the
// queue is currently getting through, from the moment it stopped being
// empty to the moment it is empty again. A person who copied a tree in
// wants one bar for the copy, not the count of rows in a table that
// changes every second.
//
// Totals are recomputed at every reading: rows still queued plus rows
// finished since the batch began, in files and in bytes. A row that leaves
// the queue without going up — dropped, dead-lettered, superseded — shrinks
// the total rather than counting as done.
type UploadBatch struct {
	Active     bool   `json:"active"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	FilesTotal int64  `json:"files_total"`
	FilesDone  int64  `json:"files_done"`
	BytesTotal int64  `json:"bytes_total"`
	BytesDone  int64  `json:"bytes_done"`
}

// uploadBatchTracker turns the uploader's ever-growing totals and the
// queue's current size into UploadBatch readings. Only Collect feeds it,
// once a second.
type uploadBatchTracker struct {
	mu        sync.Mutex
	active    bool
	started   time.Time
	finished  time.Time
	baseFiles int64
	baseBytes int64
	last      UploadBatch
}

// observe folds one reading in. queuedFiles and queuedBytes describe the
// rows still to run; doneFiles and doneBytes are the uploader's totals.
func (b *uploadBatchTracker) observe(now time.Time, queuedFiles int, queuedBytes, doneFiles, doneBytes int64) UploadBatch {
	b.mu.Lock()
	defer b.mu.Unlock()
	if queuedFiles > 0 && !b.active {
		b.active, b.started, b.finished = true, now, time.Time{}
		b.baseFiles, b.baseBytes = doneFiles, doneBytes
	}
	if !b.active {
		return b.last
	}
	done := UploadBatch{
		Active:     true,
		StartedAt:  now.UTC().Format(time.RFC3339),
		FilesDone:  doneFiles - b.baseFiles,
		BytesDone:  doneBytes - b.baseBytes,
		FilesTotal: int64(queuedFiles) + doneFiles - b.baseFiles,
		BytesTotal: queuedBytes + doneBytes - b.baseBytes,
	}
	done.StartedAt = b.started.UTC().Format(time.RFC3339)
	if queuedFiles == 0 {
		b.active = false
		b.finished = now
		done.Active = false
		done.FinishedAt = now.UTC().Format(time.RFC3339)
	}
	b.last = done
	return done
}
