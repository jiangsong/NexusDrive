package tianyi

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
)

func ctx() context.Context { return context.Background() }

// TestSignRequestCanonicalString pins the signature against digests computed
// outside Go. A different canonical string (different field order, a lowercase
// method, a path with the query attached) produces a different digest, so this
// is the test that catches a broken signer before the server does.
func TestSignRequestCanonicalString(t *testing.T) {
	const (
		secret = "secret"
		key    = "sk"
		uri    = "/open/file/listFiles.action"
		date   = "Mon, 02 Jan 2006 15:04:05 GMT"
	)
	if got, want := signRequest(secret, key, "GET", uri, date, ""), "E5843DCB91C6F2B28F527D92D2E1807A2FF10CEA"; got != want {
		t.Errorf("signRequest = %s, want %s", got, want)
	}
	if got, want := signRequest(secret, key, "GET", uri, date, "ABC"), "5A736403299FA7965CF4164E3A39466171190B52"; got != want {
		t.Errorf("signRequest with params = %s, want %s", got, want)
	}
	// The method is upper-cased before signing, so a lowercase caller signs
	// the same string.
	if signRequest(secret, key, "get", uri, date, "") != signRequest(secret, key, "GET", uri, date, "") {
		t.Error("method case changed the signature")
	}
}

func TestNewRejectsIncompleteCredentials(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"nothing", map[string]any{}, `"access_token"`},
		{"username only", map[string]any{"username": "u"}, `"password"`},
		{"password only", map[string]any{"password": "p"}, `"username"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New("t189", tc.cfg)
			if err == nil {
				t.Fatal("expected an error naming the missing key")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}

	d, err := New("t189", map[string]any{"access_token": "tok", "root_id": 4242})
	if err != nil {
		t.Fatalf("access_token alone should be enough: %v", err)
	}
	// YAML hands unquoted ids through as an int; they must survive as strings.
	if d.RootID() != "4242" {
		t.Errorf("root_id = %q, want \"4242\"", d.RootID())
	}
	if _, err := New("t189", map[string]any{"access_token": []string{"nope"}}); err == nil {
		t.Error("a non-scalar config value should be rejected")
	}
}

func TestRegisteredInRegistry(t *testing.T) {
	found := false
	for _, typ := range provider.Types() {
		if typ == "tianyi" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tianyi is not registered: %v", provider.Types())
	}
	if _, err := provider.New("tianyi", "t189", map[string]any{}); err == nil {
		t.Error("the factory should reject a config with no credentials")
	}
	p, err := provider.New("tianyi", "t189", map[string]any{"access_token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "t189" {
		t.Errorf("Name = %q", p.Name())
	}
	caps := p.Capabilities()
	if caps.Delta {
		t.Error("Caps.Delta must be false: 189 has no change feed")
	}
	if _, ok := p.(provider.ChangeLister); ok {
		t.Error("the driver must not implement ChangeLister")
	}
	if caps.Tier != provider.TierOfficial || caps.PartSize != 8<<20 {
		t.Errorf("caps = %+v", caps)
	}
	if caps.QPS != (provider.QPS{Meta: 2, Download: 2, Upload: 1}) {
		t.Errorf("QPS = %+v", caps.QPS)
	}
	if len(caps.RapidUpload) != 1 || caps.RapidUpload[0] != provider.HashMD5 {
		t.Errorf("RapidUpload = %v", caps.RapidUpload)
	}
}

// TestSessionFromAccessTokenDecodesXML covers the first of the two XML call
// sites: getSessionForPC.action answers with a <userSession> document.
func TestSessionFromAccessTokenDecodesXML(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})

	s, err := d.ensureSession(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if s.key != "sess-key-1" || s.secret != testSessionSecret {
		t.Fatalf("session = %+v", s)
	}
	if s.familyKey != "fam-key-1" || s.accessToken != testAccessToken {
		t.Errorf("family/token not decoded: %+v", s)
	}
	q := f.last("/getSessionForPC.action").Query
	if q.Get("accessToken") != testAccessToken || q.Get("appId") != appID || q.Get("clientType") != clientType {
		t.Errorf("session query = %v", q)
	}
	// A second call reuses the session instead of logging in again.
	if _, err := d.ensureSession(ctx()); err != nil {
		t.Fatal(err)
	}
	if n := f.count("/getSessionForPC.action"); n != 1 {
		t.Errorf("logged in %d times, want 1", n)
	}
	if d.Session() != "sess-key-1" {
		t.Errorf("Session() = %q", d.Session())
	}
}

// TestPasswordLoginEncryptsCredentials proves the credentials really are
// RSA-encrypted with the key the login box published: the fake decrypts them
// with the matching private key.
func TestPasswordLoginEncryptsCredentials(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{Username: "13800000000", Password: "s3cr3t!"})

	if _, err := d.ensureSession(ctx()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	user, pass := f.loginUser, f.loginPass
	f.mu.Unlock()
	if user != "13800000000" || pass != "s3cr3t!" {
		t.Fatalf("login box decrypted %q/%q", user, pass)
	}

	submit := f.last("/api/logbox/oauth2/loginSubmit.do")
	if got := submit.Form.Get("userName"); !strings.HasPrefix(got, "{NRP}") || len(got) < 100 {
		t.Errorf("userName was not the prefixed ciphertext: %q", got)
	}
	if submit.Form.Get("userName") == "13800000000" || submit.Form.Get("password") == "s3cr3t!" {
		t.Fatal("credentials were sent in the clear")
	}
	if submit.Form.Get("paramId") != "PARAM-77" || submit.Form.Get("captchaToken") != "CAP-5" {
		t.Errorf("login page params not forwarded: %v", submit.Form)
	}
	if submit.Header.Get("lt") != "LT-123-abc" || submit.Header.Get("REQID") != "REQ-9" {
		t.Errorf("lt/REQID headers = %v", submit.Header)
	}
	// The redirect from the login box is what buys the session.
	if got := f.last("/getSessionForPC.action").Query.Get("redirectURL"); got != "https://cloud.189.cn/redirect?ticket=T1" {
		t.Errorf("redirectURL = %q", got)
	}
}

func TestListPaginates(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})

	var all []provider.Entry
	cursor := ""
	for i := 0; ; i++ {
		if i > 5 {
			t.Fatal("List did not terminate")
		}
		page, next, err := d.List(ctx(), testRootID, cursor)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(all) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(all), all)
	}

	pages := f.requests("/open/file/listFiles.action")
	if len(pages) != 3 {
		t.Fatalf("made %d list requests, want 3", len(pages))
	}
	for i, r := range pages {
		if got, want := r.Query.Get("pageNum"), strconv.Itoa(i+1); got != want {
			t.Errorf("request %d pageNum = %q, want %q", i, got, want)
		}
		if r.Query.Get("folderId") != testRootID || r.Query.Get("pageSize") != strconv.Itoa(listPageSize) {
			t.Errorf("request %d query = %v", i, r.Query)
		}
	}

	dir := all[0]
	if dir.ID != "100" || dir.Name != "docs" || dir.Kind != provider.KindDir {
		t.Errorf("folder entry = %+v", dir)
	}
	file := all[1]
	if file.ID != "201" || file.Kind != provider.KindFile || file.Size != 10 {
		t.Errorf("file entry = %+v", file)
	}
	// Hashes are lowercase hex per the provider contract; 189 returns them
	// uppercase.
	if got := file.Hashes[provider.HashMD5]; got != "0cc175b9c0f1b6a831c399e269772661" {
		t.Errorf("md5 = %q", got)
	}
	if file.Version != file.Hashes[provider.HashMD5] {
		t.Errorf("version = %q, want the md5", file.Version)
	}
	want := time.Date(2024, 3, 2, 8, 30, 0, 0, beijing)
	if !file.ModTime.Equal(want) {
		t.Errorf("mtime = %s, want %s (Beijing wall clock)", file.ModTime, want)
	}
	// The second page spells its ids as strings rather than numbers.
	if all[2].ID != "202" || all[2].Size != 2048 {
		t.Errorf("second page entry = %+v", all[2])
	}
}

func TestListRejectsBadCursor(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})
	if _, _, err := d.List(ctx(), testRootID, "not-a-page"); err == nil {
		t.Fatal("expected an error for a malformed cursor")
	}
}

func TestStat(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})

	e, err := d.Stat(ctx(), "201")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "201" || e.Name != "a.txt" || e.Size != 10 || e.Kind != provider.KindFile {
		t.Errorf("entry = %+v", e)
	}
	if e.ParentID != testRootID {
		t.Errorf("parent = %q", e.ParentID)
	}
	if e.Hashes[provider.HashMD5] != "0cc175b9c0f1b6a831c399e269772661" {
		t.Errorf("hashes = %v", e.Hashes)
	}
	if got := f.last("/open/file/getFileInfo.action").Query.Get("fileId"); got != "201" {
		t.Errorf("fileId query = %q", got)
	}

	folder, err := d.Stat(ctx(), "100")
	if err != nil {
		t.Fatal(err)
	}
	if folder.Kind != provider.KindDir {
		t.Errorf("folder kind = %v", folder.Kind)
	}
	if _, err := d.Stat(ctx(), "999"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("missing id error = %v, want ErrNotFound", err)
	}
}

func TestDownloadURL(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})

	link, err := d.DownloadURL(ctx(), "201")
	if err != nil {
		t.Fatal(err)
	}
	if link.URL != f.srv.URL+"/cdn/201" {
		t.Errorf("url = %q", link.URL)
	}
	if time.Until(link.ExpiresAt) <= 0 || time.Until(link.ExpiresAt) > LinkTTL+time.Second {
		t.Errorf("expiry = %s, want within %s", link.ExpiresAt, LinkTTL)
	}
	if link.Headers["User-Agent"] != DefaultUserAgent {
		t.Errorf("link headers = %v", link.Headers)
	}
	// A second resolve inside the TTL is served from the cache.
	if _, err := d.DownloadURL(ctx(), "201"); err != nil {
		t.Fatal(err)
	}
	if n := f.count("/open/file/getFileDownloadUrl.action"); n != 1 {
		t.Errorf("resolved the link %d times, want 1", n)
	}
}

func TestReadRangeSendsRangeAndAgent(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})

	rc, err := d.ReadRange(ctx(), "201", "", 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "34567" {
		t.Errorf("read %q, want %q", got, "34567")
	}
	f.mu.Lock()
	ranges, agents := f.cdnRanges, f.cdnAgents
	f.mu.Unlock()
	if len(ranges) != 1 || ranges[0] != "bytes=3-7" {
		t.Errorf("Range headers = %v, want [bytes=3-7]", ranges)
	}
	if len(agents) != 1 || agents[0] != DefaultUserAgent {
		t.Errorf("User-Agent headers = %v", agents)
	}

	// n <= 0 means "to the end".
	rc, err = d.ReadRange(ctx(), "201", "", 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "uvwxyz" {
		t.Errorf("open-ended read = %q", got)
	}
	f.mu.Lock()
	last := f.cdnRanges[len(f.cdnRanges)-1]
	f.mu.Unlock()
	if last != "bytes=30-" {
		t.Errorf("open-ended Range = %q", last)
	}
}

func TestReadRangeRefreshesExpiredLink(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})

	if _, err := d.DownloadURL(ctx(), "201"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.cdnFailNext = true // the cached link dies before its nominal TTL
	f.mu.Unlock()

	rc, err := d.ReadRange(ctx(), "201", "", 0, 4)
	if err != nil {
		t.Fatalf("ReadRange should have re-resolved the link: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "0123" {
		t.Errorf("read %q", got)
	}
	if n := f.count("/open/file/getFileDownloadUrl.action"); n != 2 {
		t.Errorf("resolved %d times, want 2 (once cached, once after the 403)", n)
	}
	if n := len(f.requests("/cdn/201")); n != 2 {
		t.Errorf("hit the CDN %d times, want 2", n)
	}
}

func TestMkdir(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})

	e, err := d.Mkdir(ctx(), "100", "sub")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != provider.KindDir || e.Name != "sub" || e.ParentID != "100" || e.ID == "" {
		t.Errorf("entry = %+v", e)
	}
	form := f.last("/open/file/createFolder.action").Form
	if form.Get("parentFolderId") != "100" || form.Get("folderName") != "sub" {
		t.Errorf("form = %v", form)
	}
	if _, err := d.Mkdir(ctx(), "100", "sub"); !errors.Is(err, provider.ErrExists) {
		t.Errorf("duplicate mkdir error = %v, want ErrExists", err)
	}
}

func TestRenameUsesTheRightEndpointPerKind(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})

	// Nothing is cached yet, so the driver spends one Stat to learn the kind
	// rather than guessing which rename endpoint to call.
	e, err := d.Rename(ctx(), "201", "a2.txt")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "a2.txt" || e.ID != "201" {
		t.Errorf("entry = %+v", e)
	}
	form := f.last("/open/file/renameFile.action").Form
	if form.Get("fileId") != "201" || form.Get("destFileName") != "a2.txt" {
		t.Errorf("file rename form = %v", form)
	}
	if n := f.count("/open/file/getFileInfo.action"); n != 2 {
		t.Errorf("made %d Stat calls, want 2 (kind lookup + canonical entry)", n)
	}

	// Listing teaches the driver that 100 is a folder, so the folder endpoint
	// is used without another kind lookup.
	if _, _, err := d.List(ctx(), testRootID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Rename(ctx(), "100", "docs2"); err != nil {
		t.Fatal(err)
	}
	form = f.last("/open/file/renameFolder.action").Form
	if form.Get("folderId") != "100" || form.Get("destFolderName") != "docs2" {
		t.Errorf("folder rename form = %v", form)
	}
	if f.count("/open/file/renameFile.action") != 1 {
		t.Error("the folder rename went to the file endpoint")
	}
}

func TestMoveWaitsForTheBatchTask(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})

	e, err := d.Move(ctx(), "201", "100")
	if err != nil {
		t.Fatal(err)
	}
	if e.ParentID != "100" {
		t.Errorf("moved entry parent = %q, want 100", e.ParentID)
	}
	form := f.last("/open/batch/createBatchTask.action").Form
	if form.Get("type") != "MOVE" || form.Get("targetFolderId") != "100" {
		t.Errorf("batch form = %v", form)
	}
	if got := form.Get("taskInfos"); !strings.Contains(got, `"fileId":"201"`) ||
		!strings.Contains(got, `"fileName":"a.txt"`) || !strings.Contains(got, `"isFolder":0`) {
		t.Errorf("taskInfos = %s", got)
	}
	// The task is asynchronous: the first poll says "running", so Move must
	// have polled twice before returning.
	if n := f.count("/open/batch/checkBatchTask.action"); n != 2 {
		t.Errorf("polled %d times, want 2", n)
	}
}

func TestMoveReportsDestinationConflict(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})
	f.setOverride("/open/batch/checkBatchTask.action", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, fmt.Sprintf(`{"res_code":0,"taskId":"task-1","taskStatus":%d}`, taskStatusConflict))
	})
	if _, err := d.Move(ctx(), "201", "100"); !errors.Is(err, provider.ErrExists) {
		t.Errorf("conflicting move error = %v, want ErrExists", err)
	}
}

func TestDelete(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})

	if err := d.Delete(ctx(), "202"); err != nil {
		t.Fatal(err)
	}
	form := f.last("/open/batch/createBatchTask.action").Form
	if form.Get("type") != "DELETE" {
		t.Errorf("batch form = %v", form)
	}
	if form.Get("targetFolderId") != "" {
		t.Errorf("DELETE must not carry a target folder: %v", form)
	}
	if _, err := d.Stat(ctx(), "202"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("after delete Stat = %v, want ErrNotFound", err)
	}
	if err := d.Delete(ctx(), testRootID); err == nil {
		t.Error("deleting the remote root must be refused")
	}
}

func TestBeginUploadRapidHit(t *testing.T) {
	f := newFake189(t)
	f.rapidUpload = true
	d := newDriver(t, f, Options{})

	const sum = "900150983cd24fb0d6963f7d28e17f72"
	s, err := d.BeginUpload(ctx(), testRootID, "up.bin", 2048, provider.Hashes{provider.HashMD5: sum})
	if err != nil {
		t.Fatal(err)
	}
	if !s.RapidDone || s.Entry == nil {
		t.Fatalf("session = %+v, want a rapid hit with an entry", s)
	}
	if s.Entry.ID != "90001" || s.Entry.Name != "up.bin" || s.Entry.Size != 2048 {
		t.Errorf("entry = %+v", s.Entry)
	}
	if s.Entry.Hashes[provider.HashMD5] != sum {
		t.Errorf("entry hashes = %v", s.Entry.Hashes)
	}

	q := f.last("/person/initMultiUpload").Query
	if q.Get("parentFolderId") != testRootID || q.Get("fileName") != "up.bin" || q.Get("fileSize") != "2048" {
		t.Errorf("init query = %v", q)
	}
	// 189 wants the hash uppercase; the provider contract stores it lowercase.
	if q.Get("fileMd5") != strings.ToUpper(sum) {
		t.Errorf("fileMd5 = %q, want uppercase", q.Get("fileMd5"))
	}
	// A file that fits in one slice can supply sliceMd5 == fileMd5, so the
	// server checks it (lazyCheck 0).
	if q.Get("sliceMd5") != strings.ToUpper(sum) || q.Get("lazyCheck") != "0" {
		t.Errorf("slice hash/lazyCheck = %q/%q", q.Get("sliceMd5"), q.Get("lazyCheck"))
	}
	if q.Get("sliceSize") != strconv.FormatInt(DefaultPartSize, 10) {
		t.Errorf("sliceSize = %q", q.Get("sliceSize"))
	}
	if f.count("/person/commitMultiUploadFile") != 1 {
		t.Error("a rapid hit must still be committed")
	}
	if f.count("/person/getMultiUploadUrls") != 0 {
		t.Error("a rapid hit must not ask for part urls")
	}
}

func TestBeginUploadWithoutHashSkipsRapid(t *testing.T) {
	f := newFake189(t)
	f.rapidUpload = true
	d := newDriver(t, f, Options{})

	s, err := d.BeginUpload(ctx(), testRootID, "up.bin", 2048, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.RapidDone {
		t.Fatal("no hash was supplied, so no rapid upload is possible")
	}
	q := f.last("/person/initMultiUpload").Query
	if q.Get("fileMd5") != "" || q.Get("lazyCheck") != "1" {
		t.Errorf("init query = %v", q)
	}
	if s.Opaque[optUploadFileID] != "77001" || s.ID != "77001" {
		t.Errorf("session = %+v", s)
	}
}

func TestChunkedUpload(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})
	// Shrink the slice size so a 20-byte payload spans three parts.
	d.caps.PartSize = 8

	data := []byte("abcdefghijklmnopqrst")
	s, err := d.BeginUpload(ctx(), testRootID, "up.bin", int64(len(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.PartSize != 8 {
		t.Fatalf("PartSize = %d, want 8", s.PartSize)
	}

	var tokens []provider.PartToken
	for i := 0; i*8 < len(data); i++ {
		lo := i * 8
		hi := lo + 8
		if hi > len(data) {
			hi = len(data)
		}
		// Drive UploadPart from the Opaque map only, the way a journal-resumed
		// session would.
		resumed := provider.UploadSession{ID: s.ID, PartSize: s.PartSize, Opaque: s.Opaque}
		tok, err := d.UploadPart(ctx(), resumed, i, bytes.NewReader(data[lo:hi]), int64(hi-lo))
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		if tok.Index != i {
			t.Errorf("token index = %d, want %d", tok.Index, i)
		}
		if tok.ETag != fmt.Sprintf("etag-part-%d", i+1) {
			t.Errorf("part %d etag = %q", i, tok.ETag)
		}
		tokens = append(tokens, tok)
	}
	if len(tokens) != 3 {
		t.Fatalf("uploaded %d parts, want 3", len(tokens))
	}

	f.mu.Lock()
	bodies, headers := f.partBodies, f.partHeaders
	f.mu.Unlock()
	joined := append(append(append([]byte{}, bodies[1]...), bodies[2]...), bodies[3]...)
	if !bytes.Equal(joined, data) {
		t.Errorf("uploaded bytes = %q, want %q", joined, data)
	}
	if len(bodies[3]) != 4 {
		t.Errorf("last part is %d bytes, want 4", len(bodies[3]))
	}
	// The per-part headers the upload host handed back must be replayed on the
	// PUT, Authorization included.
	if got := headers[1].Get("Authorization"); got != "OSS ak-1:sig-1" {
		t.Errorf("part 1 Authorization = %q", got)
	}
	if got := headers[2].Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("part 2 Content-Type = %q", got)
	}
	// partInfo carries the part's own MD5 so the host can reject a bad slice.
	sum := md5.Sum(data[0:8])
	urls := f.requests("/person/getMultiUploadUrls")
	if got, want := urls[0].Query.Get("partInfo"), "1-"+base64.StdEncoding.EncodeToString(sum[:]); got != want {
		t.Errorf("partInfo = %q, want %q", got, want)
	}
	if urls[0].Query.Get("uploadFileId") != "77001" {
		t.Errorf("uploadFileId = %q", urls[0].Query.Get("uploadFileId"))
	}

	// A short part list must not be committed: that would publish a truncated
	// file.
	if _, err := d.CompleteUpload(ctx(), s, tokens[:2]); err == nil {
		t.Error("CompleteUpload accepted a short part list")
	}

	e, err := d.CompleteUpload(ctx(), s, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "90001" || e.Name != "up.bin" || e.Size != int64(len(data)) {
		t.Errorf("entry = %+v", e)
	}
	if e.ParentID != testRootID || e.Kind != provider.KindFile {
		t.Errorf("entry = %+v", e)
	}
	if want := time.Date(2024, 3, 5, 10, 0, 0, 0, beijing); !e.ModTime.Equal(want) {
		t.Errorf("mtime = %s, want %s", e.ModTime, want)
	}
	commit := f.last("/person/commitMultiUploadFile").Query
	if commit.Get("uploadFileId") != "77001" || commit.Get("opertype") != "3" {
		t.Errorf("commit query = %v", commit)
	}
}

func TestParseUploadHeader(t *testing.T) {
	// An OSS Authorization value contains a colon, so the split must be on the
	// first '=' and not on ':'.
	got := parseUploadHeader("Content-Type=application/octet-stream&Authorization=OSS ak:sig&Date=Mon, 04 Mar 2024 11:00:00 GMT")
	if got["Authorization"] != "OSS ak:sig" {
		t.Errorf("Authorization = %q", got["Authorization"])
	}
	if got["Date"] != "Mon, 04 Mar 2024 11:00:00 GMT" || got["Content-Type"] != "application/octet-stream" {
		t.Errorf("headers = %v", got)
	}
	// A pair with no '=' at all falls back to ':'.
	if got := parseUploadHeader("X-Oss-Date:20240304T110000Z"); got["X-Oss-Date"] != "20240304T110000Z" {
		t.Errorf("colon form = %v", got)
	}
	if len(parseUploadHeader("")) != 0 {
		t.Error("empty header list should decode to nothing")
	}
}

// TestErrorMapping drives every sentinel the driver claims to produce, through
// a real request, including the risk-control path the upper layer uses to
// circuit-break the account.
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		code    string
		message string
		want    error
		class   retry.Class
	}{
		{"session", "InvalidSessionKey", "会话失效", provider.ErrAuth, retry.ClassAuth},
		{"missing", "FileNotFound", "文件不存在", provider.ErrNotFound, retry.ClassTerminal},
		{"duplicate", "FileAlreadyExists", "同名文件已存在", provider.ErrExists, retry.ClassTerminal},
		{"risk", "InfoSecurityErrorCode", "文件涉嫌违规", provider.ErrRiskControl, retry.ClassRiskControl},
		{"flow", "UserDayFlowOverLimited", "超出限制", provider.ErrRateLimited, retry.ClassRetryable},
		{"risk by message", "SomethingNew", "账号异常，请完成安全验证", provider.ErrRiskControl, retry.ClassRiskControl},
		{"rate by message", "SomethingNew", "操作过于频繁", provider.ErrRateLimited, retry.ClassRetryable},
		{"unmapped", "SomethingNew", "请稍后重试", nil, retry.ClassTerminal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake189(t)
			d := newDriver(t, f, Options{})
			f.setOverride("/open/file/getFileInfo.action", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, fmt.Sprintf(`{"res_code":%q,"res_message":%q}`, tc.code, tc.message))
			})
			_, err := d.Stat(ctx(), "201")
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("error %v does not match %v", err, tc.want)
			}
			if got := retry.Classify(err); got != tc.class {
				t.Errorf("classified as %v, want %v", got, tc.class)
			}
			// The server's own code and message survive into the message so an
			// operator can see what 189 actually said.
			if !strings.Contains(err.Error(), tc.code) || !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q lost the server's code/message", err)
			}
		})
	}
}

// TestAuthFailureRetriesLoginExactlyOnce covers the recovery path: a session
// the server has forgotten is re-established once, and a second rejection is
// surfaced rather than retried forever.
func TestAuthFailureRetriesLoginExactlyOnce(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})

	var calls int
	f.setOverride("/open/file/getFileInfo.action", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			writeJSON(w, `{"res_code":"InvalidSessionKey","res_message":"会话失效"}`)
			return
		}
		writeJSON(w, `{"res_code":0,"fileId":"201","fileName":"a.txt","fileSize":10,"md5":"0CC175B9C0F1B6A831C399E269772661","parentId":"-11","lastOpTime":"2024-03-02 08:30:00","isFolder":false}`)
	})

	e, err := d.Stat(ctx(), "201")
	if err != nil {
		t.Fatalf("the driver should have re-logged in and retried: %v", err)
	}
	if e.Name != "a.txt" {
		t.Errorf("entry = %+v", e)
	}
	if n := f.count("/getSessionForPC.action"); n != 2 {
		t.Errorf("logged in %d times, want 2", n)
	}
	if d.Session() != "sess-key-2" {
		t.Errorf("session after recovery = %q, want the new one", d.Session())
	}

	// A server that never accepts the session must not loop: exactly one extra
	// login, then the error.
	f2 := newFake189(t)
	d2 := newDriver(t, f2, Options{})
	f2.setOverride("/open/file/getFileInfo.action", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"res_code":"InvalidSessionKey","res_message":"会话失效"}`)
	})
	if _, err := d2.Stat(ctx(), "201"); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("error = %v, want ErrAuth", err)
	}
	if n := f2.count("/open/file/getFileInfo.action"); n != 2 {
		t.Errorf("made %d attempts, want 2", n)
	}
	if n := f2.count("/getSessionForPC.action"); n != 2 {
		t.Errorf("logged in %d times, want 2", n)
	}
}

// TestSignedRequestsCarryTheSignature checks the wiring end to end: the fake
// rejects any /open/ call whose signature it cannot reproduce, so a successful
// List proves the headers were built correctly.
func TestSignedRequestsCarryTheSignature(t *testing.T) {
	f := newFake189(t)
	d := newDriver(t, f, Options{})
	if _, _, err := d.List(ctx(), testRootID, ""); err != nil {
		t.Fatal(err)
	}
	r := f.last("/open/file/listFiles.action")
	if r.Header.Get("SessionKey") != "sess-key-1" || r.Header.Get("Sign-Type") != "1" {
		t.Errorf("headers = %v", r.Header)
	}
	if _, err := http.ParseTime(r.Header.Get("Date")); err != nil {
		t.Errorf("Date %q is not an HTTP date: %v", r.Header.Get("Date"), err)
	}
	want := signRequest(testSessionSecret, "sess-key-1", "GET", "/open/file/listFiles.action", r.Header.Get("Date"), "")
	if r.Header.Get("Signature") != want {
		t.Errorf("Signature = %q, want %q", r.Header.Get("Signature"), want)
	}
	if r.Header.Get("Accept") != "application/json;charset=UTF-8" {
		t.Errorf("Accept = %q; the JSON endpoints must ask for JSON", r.Header.Get("Accept"))
	}
	// The upload host must NOT ask for JSON: it answers XML by default and
	// this driver decodes XML there.
	if _, err := d.BeginUpload(ctx(), testRootID, "up.bin", 4, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.last("/person/initMultiUpload").Header.Get("Accept"); strings.Contains(got, "json") {
		t.Errorf("upload Accept = %q, want no JSON negotiation", got)
	}
}
