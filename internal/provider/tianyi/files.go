package tianyi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// beijing is the zone 189 timestamps are in. The API returns wall-clock times
// like "2024-03-01 12:00:00" with no offset, and the servers run on Beijing
// time, so parsing them as UTC would shift every mtime by eight hours.
var beijing = time.FixedZone("CST", 8*60*60)

// timeLayout is the timestamp format used by lastOpTime / createDate.
const timeLayout = "2006-01-02 15:04:05"

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.ParseInLocation(timeLayout, s, beijing); err == nil {
		return t
	}
	// Some endpoints return RFC3339 instead.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// fileItem is one file as listFiles.action reports it.
type fileItem struct {
	ID         looseString `json:"id"`
	FileID     looseString `json:"fileId"`
	Name       string      `json:"name"`
	FileName   string      `json:"fileName"`
	Size       int64       `json:"size"`
	FileSize   int64       `json:"fileSize"`
	MD5        string      `json:"md5"`
	ParentID   looseString `json:"parentId"`
	LastOpTime string      `json:"lastOpTime"`
	CreateDate string      `json:"createDate"`
	Rev        looseString `json:"rev"`
}

// folderItem is one folder as listFiles.action reports it.
type folderItem struct {
	ID         looseString `json:"id"`
	FileID     looseString `json:"fileId"`
	Name       string      `json:"name"`
	FileName   string      `json:"fileName"`
	ParentID   looseString `json:"parentId"`
	LastOpTime string      `json:"lastOpTime"`
	CreateDate string      `json:"createDate"`
	Rev        looseString `json:"rev"`
}

// listResp is the listFiles.action payload.
type listResp struct {
	envelope
	FileListAO struct {
		// Count is the number of entries in this page. Pagination does not
		// depend on it (see List); it is decoded for diagnostics only.
		Count      int          `json:"count"`
		FileList   []fileItem   `json:"fileList"`
		FolderList []folderItem `json:"folderList"`
	} `json:"fileListAO"`
}

func (t *Tianyi) fileEntry(parentID string, f fileItem) provider.Entry {
	e := provider.Entry{
		ID:       firstNonEmpty(f.ID.String(), f.FileID.String()),
		ParentID: firstNonEmpty(f.ParentID.String(), parentID),
		Name:     firstNonEmpty(f.Name, f.FileName),
		Kind:     provider.KindFile,
		Size:     max64(f.Size, f.FileSize),
		ModTime:  parseTime(firstNonEmpty(f.LastOpTime, f.CreateDate)),
	}
	if md5 := strings.ToLower(strings.TrimSpace(f.MD5)); md5 != "" {
		e.Hashes = provider.Hashes{provider.HashMD5: md5}
		e.Version = md5
	} else {
		// Without a hash the only change token 189 offers is the modification
		// time, which is coarse but monotonic for a given file.
		e.Version = firstNonEmpty(f.Rev.String(), f.LastOpTime)
	}
	return e
}

func (t *Tianyi) folderEntry(parentID string, f folderItem) provider.Entry {
	return provider.Entry{
		ID:       firstNonEmpty(f.ID.String(), f.FileID.String()),
		ParentID: firstNonEmpty(f.ParentID.String(), parentID),
		Name:     firstNonEmpty(f.Name, f.FileName),
		Kind:     provider.KindDir,
		ModTime:  parseTime(firstNonEmpty(f.LastOpTime, f.CreateDate)),
		Version:  firstNonEmpty(f.Rev.String(), f.LastOpTime),
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// parseCursor decodes the "<page>|<seen>" cursor List hands out. A bare page
// number is accepted too, so cursors persisted by an older build still work.
func parseCursor(cursor string) (page, seen int, err error) {
	if cursor == "" {
		return 1, 0, nil
	}
	p, s, ok := strings.Cut(cursor, "|")
	page, err = strconv.Atoi(p)
	if err != nil || page < 1 {
		return 0, 0, fmt.Errorf("tianyi: bad list cursor %q", cursor)
	}
	if ok {
		seen, err = strconv.Atoi(s)
		if err != nil || seen < 0 {
			return 0, 0, fmt.Errorf("tianyi: bad list cursor %q", cursor)
		}
	}
	return page, seen, nil
}

// List returns one page of dirID's children. The cursor is opaque to callers;
// it encodes the next page number and how many entries have been seen so far.
//
// 189 returns folders and files in two separate arrays of one page, so a page
// is the concatenation of both.
//
// The listing ends at the first empty page, not at the first short one. The
// server's `count` field is the number of entries in *this* page rather than
// the folder total (the reference client stops paging when it reaches zero,
// which would loop forever if the field were a total), and a short page is not
// documented to mean "last page" when folders and files are paged together.
// Paging until empty costs one extra request per directory and is the only
// reading that cannot silently truncate a listing.
//
// UNVERIFIED: whether a short page is always the last page. If it is, the
// extra request can be dropped by stopping when len(entries) < listPageSize.
func (t *Tianyi) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	if dirID == "" {
		dirID = t.rootID
	}
	page, seen, err := parseCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	q := url.Values{}
	q.Set("folderId", dirID)
	q.Set("mediaType", "0")
	q.Set("orderBy", "lastOpTime")
	q.Set("descending", "true")
	q.Set("pageNum", strconv.Itoa(page))
	q.Set("pageSize", strconv.Itoa(listPageSize))

	var out listResp
	if err := t.getJSON(ctx, apiURL(t.base, "/open/file/listFiles.action", q), ratelimit.Meta, &out); err != nil {
		return nil, "", err
	}
	entries := make([]provider.Entry, 0, len(out.FileListAO.FolderList)+len(out.FileListAO.FileList))
	for _, f := range out.FileListAO.FolderList {
		entries = append(entries, t.folderEntry(dirID, f))
	}
	for _, f := range out.FileListAO.FileList {
		entries = append(entries, t.fileEntry(dirID, f))
	}
	t.rememberKinds(entries)

	seen += len(entries)
	next := ""
	if len(entries) > 0 {
		next = fmt.Sprintf("%d|%d", page+1, seen)
	}
	return entries, next, nil
}

// statResp is the getFileInfo.action payload. Both field spellings seen in the
// wild are decoded and the non-empty one wins.
//
// UNVERIFIED: which of id/fileId and name/fileName the live API uses for a
// personal-cloud file; accepting both costs nothing and avoids a silent empty
// entry if the spelling differs from the one assumed here.
type statResp struct {
	envelope
	ID         looseString `json:"id"`
	FileID     looseString `json:"fileId"`
	Name       string      `json:"name"`
	FileName   string      `json:"fileName"`
	Size       int64       `json:"size"`
	FileSize   int64       `json:"fileSize"`
	MD5        string      `json:"md5"`
	ParentID   looseString `json:"parentId"`
	LastOpTime string      `json:"lastOpTime"`
	CreateDate string      `json:"createDate"`
	IsFolder   *bool       `json:"isFolder"`
}

// Stat returns one entry by id.
func (t *Tianyi) Stat(ctx context.Context, id string) (provider.Entry, error) {
	if id == "" {
		return provider.Entry{}, fmt.Errorf("tianyi: Stat needs a file id")
	}
	q := url.Values{}
	q.Set("fileId", id)
	var out statResp
	if err := t.getJSON(ctx, apiURL(t.base, "/open/file/getFileInfo.action", q), ratelimit.Meta, &out); err != nil {
		return provider.Entry{}, err
	}
	gotID := firstNonEmpty(out.ID.String(), out.FileID.String(), id)
	kind := provider.KindFile
	switch {
	case out.IsFolder != nil && *out.IsFolder:
		kind = provider.KindDir
	case out.IsFolder == nil:
		// The field was absent: fall back to what listing taught us rather
		// than guessing from the size, which is 0 for empty files too.
		if k, ok := t.kindOf(gotID); ok {
			kind = k
		}
	}
	e := provider.Entry{
		ID:       gotID,
		ParentID: out.ParentID.String(),
		Name:     firstNonEmpty(out.Name, out.FileName),
		Kind:     kind,
		Size:     max64(out.Size, out.FileSize),
		ModTime:  parseTime(firstNonEmpty(out.LastOpTime, out.CreateDate)),
	}
	if kind == provider.KindFile {
		if md5 := strings.ToLower(strings.TrimSpace(out.MD5)); md5 != "" {
			e.Hashes = provider.Hashes{provider.HashMD5: md5}
			e.Version = md5
		} else {
			e.Version = out.LastOpTime
		}
	} else {
		e.Version = out.LastOpTime
	}
	t.rememberKinds([]provider.Entry{e})
	return e, nil
}

func (t *Tianyi) rememberKinds(entries []provider.Entry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range entries {
		if e.ID != "" {
			t.kinds[e.ID] = e.Kind
		}
	}
}

func (t *Tianyi) kindOf(id string) (provider.Kind, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k, ok := t.kinds[id]
	return k, ok
}

// downloadResp is the getFileDownloadUrl.action payload.
type downloadResp struct {
	envelope
	FileDownloadURL string `json:"fileDownloadUrl"`
}

// DownloadURL resolves a direct link. The URL is signed by 189 and carries its
// own credentials, so it is usable by any process (Caps.LinkShareable).
func (t *Tianyi) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	u, exp, err := t.resolveLink(ctx, id)
	if err != nil {
		return provider.Link{}, err
	}
	return provider.Link{URL: u, ExpiresAt: exp, Headers: t.caps.LinkHeaders}, nil
}

// resolveLink returns a cached link when one is still fresh, so a sequential
// read of a large file resolves once instead of once per range request.
func (t *Tianyi) resolveLink(ctx context.Context, id string) (string, time.Time, error) {
	t.mu.Lock()
	if l, ok := t.links[id]; ok && time.Now().Before(l.expires) {
		t.mu.Unlock()
		return l.url, l.expires, nil
	}
	t.mu.Unlock()

	q := url.Values{}
	q.Set("fileId", id)
	var out downloadResp
	if err := t.getJSON(ctx, apiURL(t.base, "/open/file/getFileDownloadUrl.action", q), ratelimit.Meta, &out); err != nil {
		return "", time.Time{}, err
	}
	raw := strings.TrimSpace(out.FileDownloadURL)
	if raw == "" {
		return "", time.Time{}, fmt.Errorf("%w: tianyi: no download url for %s", provider.ErrNotFound, id)
	}
	// 189 sometimes answers with a protocol-relative or path-only URL.
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	} else if strings.HasPrefix(raw, "/") {
		raw = t.base + raw
	}
	exp := time.Now().Add(LinkTTL)
	t.mu.Lock()
	t.links[id] = cachedLink{url: raw, expires: exp}
	t.mu.Unlock()
	return raw, exp, nil
}

func (t *Tianyi) dropLink(id string) {
	t.mu.Lock()
	delete(t.links, id)
	t.mu.Unlock()
}

// rangeReader ties the limited view of a response body to the body's Close.
type rangeReader struct {
	io.Reader
	body io.ReadCloser
}

func (r *rangeReader) Close() error { return r.body.Close() }

// ReadRange returns a reader over n bytes of the file starting at off; n <= 0
// means "to the end".
//
// The version argument is advisory: 189 offers no precondition header, so the
// driver cannot ask the CDN to fail on a changed file. Staleness is instead
// caught by the caller re-Stating, and by the link cache being dropped on the
// first 403/410.
func (t *Tianyi) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	if off < 0 {
		return nil, fmt.Errorf("tianyi: negative read offset %d", off)
	}
	rc, err := t.readRangeOnce(ctx, id, off, n)
	if err == nil {
		return rc, nil
	}
	if !errors.Is(err, provider.ErrLinkExpired) {
		return nil, err
	}
	// The signed URL died early. Drop it and resolve exactly once more.
	t.dropLink(id)
	return t.readRangeOnce(ctx, id, off, n)
}

func (t *Tianyi) readRangeOnce(ctx context.Context, id string, off, n int64) (io.ReadCloser, error) {
	u, _, err := t.resolveLink(ctx, id)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	for k, v := range t.caps.LinkHeaders {
		hdr.Set(k, v)
	}
	hdr.Set("Range", httpx.RangeHeader(off, n))
	resp, err := t.cli.Do(ctx, httpx.Request{
		Method:       http.MethodGet,
		URL:          u,
		Class:        ratelimit.Transfer,
		Header:       hdr,
		Stream:       true,
		ExpectStatus: []int{http.StatusOK, http.StatusPartialContent},
	})
	if err != nil {
		return nil, err
	}
	body := resp.Body
	if resp.Status == http.StatusOK && off > 0 {
		// The node ignored the Range header and sent the whole object. Skip
		// forward rather than handing the caller the wrong bytes.
		if _, err := io.CopyN(io.Discard, body, off); err != nil {
			body.Close()
			return nil, fmt.Errorf("tianyi: skip to offset %d: %w", off, err)
		}
	}
	if n > 0 {
		return &rangeReader{Reader: io.LimitReader(body, n), body: body}, nil
	}
	return body, nil
}

// mkdirResp is the createFolder.action payload.
type mkdirResp struct {
	envelope
	ID       looseString `json:"id"`
	FileID   looseString `json:"fileId"`
	Name     string      `json:"name"`
	FileName string      `json:"fileName"`
	ParentID looseString `json:"parentId"`
	CreateAt string      `json:"createDate"`
}

// Mkdir creates a folder and returns it. The response carries the new id, so
// no follow-up Stat is needed.
func (t *Tianyi) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	if parentID == "" {
		parentID = t.rootID
	}
	var out mkdirResp
	err := t.postJSON(ctx, t.base+"/open/file/createFolder.action", map[string]string{
		"parentFolderId": parentID,
		"folderName":     name,
	}, ratelimit.Meta, &out)
	if err != nil {
		return provider.Entry{}, err
	}
	id := firstNonEmpty(out.ID.String(), out.FileID.String())
	if id == "" {
		return provider.Entry{}, fmt.Errorf("tianyi: createFolder returned no folder id")
	}
	e := provider.Entry{
		ID:       id,
		ParentID: firstNonEmpty(out.ParentID.String(), parentID),
		Name:     firstNonEmpty(out.Name, out.FileName, name),
		Kind:     provider.KindDir,
		ModTime:  parseTime(out.CreateAt),
	}
	if e.ModTime.IsZero() {
		e.ModTime = time.Now()
	}
	t.rememberKinds([]provider.Entry{e})
	return e, nil
}

// Rename renames a file or folder in place.
//
// 189 has two endpoints, renameFile.action and renameFolder.action, with
// different parameter names, and the Provider interface hands us only an id.
// The kind comes from the cache that List and Stat fill; when it is not cached
// the driver spends one Stat to learn it rather than guessing, because calling
// the wrong endpoint fails the rename outright.
func (t *Tianyi) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	kind, ok := t.kindOf(id)
	if !ok {
		e, err := t.Stat(ctx, id)
		if err != nil {
			return provider.Entry{}, err
		}
		kind = e.Kind
	}
	path, form := "/open/file/renameFile.action", map[string]string{
		"fileId":       id,
		"destFileName": newName,
	}
	if kind == provider.KindDir {
		path, form = "/open/file/renameFolder.action", map[string]string{
			"folderId":       id,
			"destFolderName": newName,
		}
	}
	var out statResp
	if err := t.postJSON(ctx, t.base+path, form, ratelimit.Meta, &out); err != nil {
		return provider.Entry{}, err
	}
	// The rename response omits the parent id, so the canonical entry is
	// re-fetched instead of being assembled from a partial payload.
	return t.Stat(ctx, id)
}

// batchTaskResp is the createBatchTask.action payload.
type batchTaskResp struct {
	envelope
	TaskID string `json:"taskId"`
}

// checkTaskResp is the checkBatchTask.action payload. taskStatus 4 means the
// task finished; 2 means it stopped on a conflict at the destination.
//
// UNVERIFIED: the full taskStatus enum. 4 (success) and 2 (conflict) are what
// community clients act on; every other value is treated as "still running".
type checkTaskResp struct {
	envelope
	TaskID     string `json:"taskId"`
	TaskStatus int    `json:"taskStatus"`
}

const (
	taskStatusConflict = 2
	taskStatusDone     = 4
)

// taskInfo is one element of the taskInfos array a batch task takes.
type taskInfo struct {
	FileID   string `json:"fileId"`
	FileName string `json:"fileName"`
	IsFolder int    `json:"isFolder"`
}

// runBatchTask submits a MOVE or DELETE batch task and waits for it to finish.
// 189 performs both asynchronously, so the call is not done when the HTTP
// response arrives.
func (t *Tianyi) runBatchTask(ctx context.Context, taskType string, info taskInfo, targetFolderID string) error {
	infos, err := json.Marshal([]taskInfo{info})
	if err != nil {
		return err
	}
	form := map[string]string{
		"type":      taskType,
		"taskInfos": string(infos),
	}
	if targetFolderID != "" {
		form["targetFolderId"] = targetFolderID
	}
	var out batchTaskResp
	if err := t.postJSON(ctx, t.base+"/open/batch/createBatchTask.action", form, ratelimit.Meta, &out); err != nil {
		return err
	}
	if out.TaskID == "" {
		// Some deployments complete small tasks inline and return no task id.
		return nil
	}
	return t.waitBatchTask(ctx, taskType, out.TaskID)
}

func (t *Tianyi) waitBatchTask(ctx context.Context, taskType, taskID string) error {
	for attempt := 0; attempt < t.pollAttempts; attempt++ {
		var out checkTaskResp
		err := t.postJSON(ctx, t.base+"/open/batch/checkBatchTask.action", map[string]string{
			"type":   taskType,
			"taskId": taskID,
		}, ratelimit.Meta, &out)
		if err != nil {
			return err
		}
		switch out.TaskStatus {
		case taskStatusDone:
			return nil
		case taskStatusConflict:
			return fmt.Errorf("%w: tianyi: %s task %s hit a name conflict at the destination",
				provider.ErrExists, taskType, taskID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(t.pollInterval):
		}
	}
	return fmt.Errorf("%w: tianyi: %s task %s did not finish in %s",
		provider.ErrTransient, taskType, taskID, time.Duration(t.pollAttempts)*t.pollInterval)
}

// taskInfoFor builds the descriptor a batch task needs. It costs a Stat
// because 189 wants the name and the file/folder flag, neither of which the
// Provider interface gives us.
func (t *Tianyi) taskInfoFor(ctx context.Context, id string) (taskInfo, error) {
	e, err := t.Stat(ctx, id)
	if err != nil {
		return taskInfo{}, err
	}
	isFolder := 0
	if e.Kind == provider.KindDir {
		isFolder = 1
	}
	return taskInfo{FileID: id, FileName: e.Name, IsFolder: isFolder}, nil
}

// Move moves an entry into a new parent folder. 189 models this as an
// asynchronous batch task, so Move returns only once the task reports success.
func (t *Tianyi) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	if newParentID == "" {
		newParentID = t.rootID
	}
	info, err := t.taskInfoFor(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	if err := t.runBatchTask(ctx, "MOVE", info, newParentID); err != nil {
		return provider.Entry{}, err
	}
	return t.Stat(ctx, id)
}

// Delete removes a file or folder. Like Move it is an asynchronous batch task;
// deleting a folder deletes its contents.
func (t *Tianyi) Delete(ctx context.Context, id string) error {
	if id == t.rootID {
		return fmt.Errorf("tianyi: refusing to delete the remote root %q", id)
	}
	info, err := t.taskInfoFor(ctx, id)
	if err != nil {
		return err
	}
	if err := t.runBatchTask(ctx, "DELETE", info, ""); err != nil {
		return err
	}
	t.mu.Lock()
	delete(t.kinds, id)
	delete(t.links, id)
	t.mu.Unlock()
	return nil
}
