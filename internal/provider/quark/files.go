package quark

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// fileItem is one entry as the sort/info endpoints report it.
//
// UNVERIFIED: md5 and sha1 are present on some responses and absent on
// others; the driver treats them as optional and never fabricates a hash.
type fileItem struct {
	Fid        string `json:"fid"`
	FileName   string `json:"file_name"`
	PdirFid    string `json:"pdir_fid"`
	Size       int64  `json:"size"`
	FileType   int    `json:"file_type"`
	Dir        bool   `json:"dir"`
	File       bool   `json:"file"`
	FormatType string `json:"format_type"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	MD5        string `json:"md5"`
	SHA1       string `json:"sha1"`
	RiskType   int    `json:"risk_type"`
}

// entry converts a Quark item into the provider's representation.
func (it *fileItem) entry() provider.Entry {
	kind := provider.KindFile
	switch {
	case it.Dir:
		kind = provider.KindDir
	case it.File:
		kind = provider.KindFile
	case it.FileType == 0:
		// Quark uses file_type 0 for folders. Only consulted when neither
		// boolean is present, which happens on the leaner responses.
		kind = provider.KindDir
	}
	ts := it.UpdatedAt
	if ts == 0 {
		ts = it.CreatedAt
	}
	e := provider.Entry{
		ID:       it.Fid,
		ParentID: it.PdirFid,
		Name:     it.FileName,
		Kind:     kind,
		Size:     it.Size,
	}
	if ts != 0 {
		// Quark reports epoch milliseconds.
		e.ModTime = time.UnixMilli(ts)
	}
	if kind == provider.KindDir {
		e.Size = 0
	}
	if it.MD5 != "" || it.SHA1 != "" {
		e.Hashes = provider.Hashes{}
		if it.MD5 != "" {
			e.Hashes[provider.HashMD5] = it.MD5
		}
		if it.SHA1 != "" {
			e.Hashes[provider.HashSHA1] = it.SHA1
		}
	}
	// Version must change whenever content changes. The content MD5 is the
	// ideal token; when Quark omits it the update timestamp is the only thing
	// on offer.
	switch {
	case it.MD5 != "":
		e.Version = it.MD5
	case ts != 0:
		e.Version = strconv.FormatInt(ts, 10)
	}
	return e
}

// List returns one page of dirID's children. The cursor is the next page
// number as a decimal string; an empty cursor starts at page 1 and an empty
// next means the listing is complete.
func (q *Quark) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	page := 1
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 1 {
			return nil, "", fmt.Errorf("quark: invalid list cursor %q", cursor)
		}
		page = n
	}
	params := commonParams()
	params.Set("pdir_fid", q.dirOrRoot(dirID))
	params.Set("_page", strconv.Itoa(page))
	params.Set("_size", strconv.Itoa(q.pageSize))
	params.Set("_fetch_total", "1")
	params.Set("_fetch_sub_dirs", "0")
	params.Set("_sort", "file_type:asc,updated_at:desc")

	var data struct {
		List []fileItem `json:"list"`
	}
	env, err := q.getJSON(ctx, q.apiURL("/file/sort", params), ratelimit.Meta, &data)
	if err != nil {
		return nil, "", err
	}
	entries := make([]provider.Entry, 0, len(data.List))
	for i := range data.List {
		entries = append(entries, data.List[i].entry())
	}

	// Prefer the server's total; fall back to "a full page means there may be
	// another" so a response without metadata still paginates instead of
	// silently truncating the directory.
	next := ""
	switch {
	case env.Metadata.Total > 0:
		if page*q.pageSize < env.Metadata.Total {
			next = strconv.Itoa(page + 1)
		}
	case len(data.List) == q.pageSize:
		next = strconv.Itoa(page + 1)
	}
	return entries, next, nil
}

// Stat returns one entry by id.
//
// UNVERIFIED: the /file/info endpoint name and its query parameter. The PC
// client mostly works from listings, so this shape is taken from community
// clients. If it turns out to be wrong the failure is a clean ErrNotFound
// rather than a wrong answer, and callers that hold the parent id can list
// instead.
func (q *Quark) Stat(ctx context.Context, id string) (provider.Entry, error) {
	if id == "" {
		return provider.Entry{}, fmt.Errorf("quark: Stat requires a file id")
	}
	params := commonParams()
	params.Set("fid", id)
	var it fileItem
	if _, err := q.getJSON(ctx, q.apiURL("/file/info", params), ratelimit.Meta, &it); err != nil {
		return provider.Entry{}, err
	}
	if it.Fid == "" {
		// An empty body with a success envelope means the id is unknown.
		return provider.Entry{}, fmt.Errorf("quark: stat %s: %w", id, provider.ErrNotFound)
	}
	return it.entry(), nil
}

// Mkdir creates a directory under parentID.
func (q *Quark) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	body := map[string]any{
		"pdir_fid":      q.dirOrRoot(parentID),
		"file_name":     name,
		"dir_path":      "",
		"dir_init_lock": false,
	}
	var data struct {
		Fid      string `json:"fid"`
		FileName string `json:"file_name"`
	}
	if _, err := q.postJSON(ctx, q.apiURL("/file", nil), ratelimit.Meta, body, &data); err != nil {
		return provider.Entry{}, err
	}
	if data.Fid == "" {
		return provider.Entry{}, fmt.Errorf("quark: mkdir %q: server returned no fid", name)
	}
	return provider.Entry{
		ID:       data.Fid,
		ParentID: q.dirOrRoot(parentID),
		Name:     name,
		Kind:     provider.KindDir,
		ModTime:  time.Now(),
	}, nil
}

// Rename changes an entry's name in place.
//
// The rename endpoint answers with an empty body, so the resulting entry is
// re-read with Stat. Because Stat's shape is unverified, a failure there is
// not propagated: the rename itself already succeeded, and reporting an error
// would make the caller retry a completed operation. In that case a minimal
// entry carrying the new name is returned instead.
func (q *Quark) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	body := map[string]any{"fid": id, "file_name": newName}
	if _, err := q.postJSON(ctx, q.apiURL("/file/rename", nil), ratelimit.Meta, body, nil); err != nil {
		return provider.Entry{}, err
	}
	if e, err := q.Stat(ctx, id); err == nil {
		return e, nil
	}
	e := provider.Entry{ID: id, Name: newName}
	provider.EnsureVersion(&e)
	return e, nil
}

// Move relocates an entry into newParentID. Quark performs moves as an
// asynchronous task, so the returned entry is only produced once the task
// reports completion.
func (q *Quark) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	body := map[string]any{
		"action_type":  1,
		"filelist":     []string{id},
		"to_pdir_fid":  q.dirOrRoot(newParentID),
		"exclude_fids": []string{},
	}
	var data struct {
		TaskID string `json:"task_id"`
	}
	if _, err := q.postJSON(ctx, q.apiURL("/file/move", nil), ratelimit.Meta, body, &data); err != nil {
		return provider.Entry{}, err
	}
	if err := q.waitTask(ctx, data.TaskID); err != nil {
		return provider.Entry{}, err
	}
	// Same reasoning as Rename: the move has committed, so an unverifiable
	// Stat must not turn a success into an error.
	if e, err := q.Stat(ctx, id); err == nil {
		return e, nil
	}
	e := provider.Entry{ID: id, ParentID: q.dirOrRoot(newParentID)}
	provider.EnsureVersion(&e)
	return e, nil
}

// Delete removes an entry (Quark moves it to the recycle bin). Like Move it is
// an asynchronous task.
func (q *Quark) Delete(ctx context.Context, id string) error {
	body := map[string]any{
		"action_type":  2,
		"filelist":     []string{id},
		"exclude_fids": []string{},
	}
	var data struct {
		TaskID string `json:"task_id"`
	}
	if _, err := q.postJSON(ctx, q.apiURL("/file/delete", nil), ratelimit.Meta, body, &data); err != nil {
		return err
	}
	return q.waitTask(ctx, data.TaskID)
}

// taskDone is the task status Quark reports for a finished task.
//
// UNVERIFIED: the status enum. 2 is observed for "finished"; values above it
// are treated as failures rather than as success, so an unknown terminal state
// surfaces as an error instead of a phantom success.
const taskDone = 2

// maxTaskPolls bounds how long Move and Delete wait for their task. At the
// default metadata rate of 1 QPS this is already a generous wall-clock budget.
const maxTaskPolls = 20

// waitTask polls an asynchronous task until it finishes.
func (q *Quark) waitTask(ctx context.Context, taskID string) error {
	if taskID == "" {
		// Some deployments answer synchronously with no task id. Nothing to
		// wait for, and inventing a failure here would break a working call.
		return nil
	}
	for i := 0; i < maxTaskPolls; i++ {
		params := commonParams()
		params.Set("task_id", taskID)
		params.Set("retry_index", strconv.Itoa(i))
		var data struct {
			TaskID string `json:"task_id"`
			Status int    `json:"status"`
			// UNVERIFIED: the field carrying a failed task's reason.
			Message string `json:"message"`
		}
		if _, err := q.getJSON(ctx, q.apiURL("/task", params), ratelimit.Meta, &data); err != nil {
			return err
		}
		switch {
		case data.Status == taskDone:
			return nil
		case data.Status > taskDone:
			return fmt.Errorf("quark: task %s failed with status %d: %s", taskID, data.Status, data.Message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(q.taskPollInterval):
		}
	}
	return fmt.Errorf("quark: task %s did not finish after %d polls: %w", taskID, maxTaskPolls, provider.ErrTransient)
}

// DownloadURL resolves a direct link for id.
//
// The link only works when replayed with the same User-Agent, Referer and
// account Cookie, so all three travel in Link.Headers. This is also why
// Caps.LinkShareable is false: a process without the account's cookie cannot
// use the URL.
func (q *Quark) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	raw, expires, err := q.resolveLink(ctx, id)
	if err != nil {
		return provider.Link{}, err
	}
	return provider.Link{URL: raw, ExpiresAt: expires, Headers: q.linkHeaders()}, nil
}

func (q *Quark) linkHeaders() map[string]string {
	return map[string]string{
		"User-Agent": q.ua,
		"Referer":    DefaultReferer,
		"Cookie":     q.CookieHeader(),
	}
}

// resolveLink returns a cached link when one is still fresh, otherwise asks
// the service for a new one. Caching matters because a sequential read issues
// many ReadRange calls against one file while the metadata budget is 1 QPS.
func (q *Quark) resolveLink(ctx context.Context, id string) (string, time.Time, error) {
	q.mu.Lock()
	if l, ok := q.links[id]; ok && time.Now().Before(l.expires) {
		q.mu.Unlock()
		return l.url, l.expires, nil
	}
	q.mu.Unlock()

	body := map[string]any{"fids": []string{id}}
	var data []struct {
		Fid         string `json:"fid"`
		FileName    string `json:"file_name"`
		DownloadURL string `json:"download_url"`
	}
	if _, err := q.postJSON(ctx, q.apiURL("/file/download", nil), ratelimit.Download, body, &data); err != nil {
		return "", time.Time{}, err
	}
	if len(data) == 0 || data[0].DownloadURL == "" {
		return "", time.Time{}, fmt.Errorf("quark: download %s: server returned no url: %w", id, provider.ErrNotFound)
	}
	expires := time.Now().Add(LinkTTL)
	q.mu.Lock()
	q.links[id] = cachedLink{url: data[0].DownloadURL, expires: expires}
	q.mu.Unlock()
	return data[0].DownloadURL, expires, nil
}

func (q *Quark) invalidateLink(id string) {
	q.mu.Lock()
	delete(q.links, id)
	q.mu.Unlock()
}

// ReadRange streams n bytes of id starting at off. n <= 0 reads to the end.
//
// version is accepted for interface compatibility but not used: a Quark
// download URL has no version selector, so there is nothing to pin the read
// to. Callers detect a changed file by comparing Entry.Version instead.
func (q *Quark) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	_ = version
	raw, _, err := q.resolveLink(ctx, id)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set("Range", httpx.RangeHeader(off, n))
	resp, err := q.cli.Do(ctx, httpx.Request{
		Method:       http.MethodGet,
		URL:          raw,
		Class:        ratelimit.Transfer,
		Header:       q.requestHeader(header),
		Stream:       true,
		ExpectStatus: []int{http.StatusOK, http.StatusPartialContent},
	})
	if err != nil {
		// 403/410 from the CDN means the link died early. Drop it so the next
		// attempt resolves a fresh one, and report the sentinel the retry
		// layer looks for.
		if errors.Is(err, provider.ErrLinkExpired) {
			q.invalidateLink(id)
			return nil, fmt.Errorf("quark: read %s: %w", id, err)
		}
		return nil, err
	}
	// A Quark CDN node that ignores Range would otherwise hand back the start
	// of the file as if it were the requested window.
	return httpx.RangeBody(resp, off, n)
}

// urlJoin appends a path to a base URL that may already carry a path.
func urlJoin(base, path string) string {
	b := base
	for len(b) > 0 && b[len(b)-1] == '/' {
		b = b[:len(b)-1]
	}
	if path == "" {
		return b
	}
	if path[0] != '/' {
		path = "/" + path
	}
	return b + path
}

// escapePath percent-encodes an OSS object key for use in a request URI while
// leaving the path separators intact.
func escapePath(p string) string {
	u := url.URL{Path: p}
	return u.EscapedPath()
}
