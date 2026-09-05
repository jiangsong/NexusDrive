package tianyi

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider/httpx"
)

// The fixtures below mirror the shapes public documentation and community
// clients describe for the 189 PC protocol. Where a shape is not verifiable
// against a live account the driver marks it UNVERIFIED; the fixtures follow
// the driver's reading so the tests exercise the code, not so they certify the
// protocol.

const (
	testSessionSecret = "sess-secret-1"
	testAccessToken   = "tok-abc"
	testRootID        = "-11"
)

// frec is one file or folder in the fake's tree.
type frec struct {
	id     string
	name   string
	parent string
	size   int64
	md5    string
	folder bool
	mtime  string
}

// recorded is one request the fake saw.
type recorded struct {
	Method string
	Path   string
	Query  url.Values
	Form   url.Values
	Header http.Header
	Body   []byte
}

// fake189 is a stateful stand-in for cloud.189.cn: the API host, the login
// box, the upload host and the download CDN all on one httptest.Server.
type fake189 struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	reqs     []recorded
	files    map[string]*frec
	override map[string]http.HandlerFunc

	priv   *rsa.PrivateKey
	pubB64 string

	sessionCalls int
	// loginUser and loginPass hold what the login box decrypted, proving the
	// client really RSA-encrypted the credentials.
	loginUser, loginPass string

	// listPages is served by pageNum; the last one must be empty.
	listPages []string

	rapidUpload bool
	initSize    int64
	partBodies  map[int][]byte
	partHeaders map[int]http.Header

	blob []byte
	// cdnFailNext makes the next CDN GET answer 403, as an expired link does.
	cdnFailNext bool
	cdnRanges   []string
	cdnAgents   []string

	taskChecks map[string]int
}

// testKey is generated once and shared: every fake serves the same login-box
// key, which keeps the suite from spending a fresh 2048-bit keygen per test.
var testKey = sync.OnceValues(func() (*rsa.PrivateKey, string) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		panic(err)
	}
	return priv, base64.StdEncoding.EncodeToString(der)
})

func newFake189(t *testing.T) *fake189 {
	t.Helper()
	priv, pub := testKey()
	f := &fake189{
		t:      t,
		priv:   priv,
		pubB64: pub,
		files: map[string]*frec{
			"100": {id: "100", name: "docs", parent: testRootID, folder: true, mtime: "2024-03-01 12:00:00"},
			"201": {id: "201", name: "a.txt", parent: testRootID, size: 10, md5: "0CC175B9C0F1B6A831C399E269772661", mtime: "2024-03-02 08:30:00"},
			"202": {id: "202", name: "b.bin", parent: "100", size: 2048, md5: "900150983CD24FB0D6963F7D28E17F72", mtime: "2024-03-03 10:00:00"},
		},
		override:    map[string]http.HandlerFunc{},
		partBodies:  map[int][]byte{},
		partHeaders: map[int]http.Header{},
		taskChecks:  map[string]int{},
		blob:        []byte("0123456789abcdefghijklmnopqrstuvwxyz"),
		listPages:   []string{listPage1, listPage2, listPageEmpty},
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake189) setOverride(path string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.override[path] = h
}

func (f *fake189) requests(path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.reqs {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fake189) count(path string) int { return len(f.requests(path)) }

func (f *fake189) last(path string) recorded {
	f.t.Helper()
	rs := f.requests(path)
	if len(rs) == 0 {
		f.t.Fatalf("no request was made to %s", path)
	}
	return rs[len(rs)-1]
}

func (f *fake189) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	// Put the body back so handlers can read it too.
	r.Body = io.NopCloser(bytes.NewReader(body))
	rec := recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		rec.Form, _ = url.ParseQuery(string(body))
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	h := f.override[r.URL.Path]
	f.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}

	// Every signed API call must carry a signature this fake can reproduce.
	if strings.HasPrefix(r.URL.Path, "/open/") && !f.signatureOK(r) {
		writeJSON(w, `{"res_code":"InvalidSessionKey","res_message":"InvalidSessionKey"}`)
		return
	}

	switch r.URL.Path {
	case "/getSessionForPC.action":
		f.handleSession(w, r)
	case "/api/portal/unifyLoginForPC.action":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, loginPageJS)
	case "/api/logbox/config/encryptConf.do":
		writeJSON(w, fmt.Sprintf(`{"result":"0","msg":"","data":{"pre":"{NRP}","pubKey":%q}}`, f.pubB64))
	case "/api/logbox/oauth2/loginSubmit.do":
		f.handleLoginSubmit(w, rec)
	case "/open/file/listFiles.action":
		f.handleList(w, r)
	case "/open/file/getFileInfo.action":
		f.handleFileInfo(w, r)
	case "/open/file/getFileDownloadUrl.action":
		f.handleDownloadURL(w, r)
	case "/open/file/createFolder.action":
		f.handleCreateFolder(w, rec)
	case "/open/file/renameFile.action", "/open/file/renameFolder.action":
		f.handleRename(w, rec)
	case "/open/batch/createBatchTask.action":
		f.handleBatchTask(w, rec)
	case "/open/batch/checkBatchTask.action":
		f.handleCheckTask(w, rec)
	case "/person/initMultiUpload":
		f.handleInitUpload(w, r)
	case "/person/getMultiUploadUrls":
		f.handleUploadURLs(w, r)
	case "/person/commitMultiUploadFile":
		f.handleCommit(w, r)
	default:
		switch {
		case strings.HasPrefix(r.URL.Path, "/cdn/"):
			f.handleCDN(w, r)
		case strings.HasPrefix(r.URL.Path, "/oss/part/"):
			f.handlePartPUT(w, r)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}
}

// signatureOK recomputes the request signature the way the driver built it.
func (f *fake189) signatureOK(r *http.Request) bool {
	key := r.Header.Get("SessionKey")
	if !strings.HasPrefix(key, "sess-key-") {
		return false
	}
	if r.Header.Get("Sign-Type") != "1" || r.Header.Get("X-Request-ID") == "" {
		return false
	}
	want := signRequest(testSessionSecret, key, r.Method, r.URL.EscapedPath(), r.Header.Get("Date"), "")
	return r.Header.Get("Signature") == want
}

func (f *fake189) handleSession(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("accessToken") == "" && q.Get("redirectURL") == "" {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><error><code>InvalidAccessToken</code><message>no credential</message></error>`)
		return
	}
	f.mu.Lock()
	f.sessionCalls++
	n := f.sessionCalls
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<userSession>
  <res_code>0</res_code>
  <res_message>成功</res_message>
  <sessionKey>sess-key-%d</sessionKey>
  <sessionSecret>%s</sessionSecret>
  <familySessionKey>fam-key-%d</familySessionKey>
  <familySessionSecret>fam-secret</familySessionSecret>
  <accessToken>%s</accessToken>
</userSession>`, n, testSessionSecret, n, testAccessToken)
}

func (f *fake189) handleLoginSubmit(w http.ResponseWriter, rec recorded) {
	user, uerr := f.decrypt(rec.Form.Get("userName"))
	pass, perr := f.decrypt(rec.Form.Get("password"))
	if uerr != nil || perr != nil {
		writeJSON(w, `{"result":"-1","msg":"credentials were not encrypted with the published key"}`)
		return
	}
	f.mu.Lock()
	f.loginUser, f.loginPass = user, pass
	f.mu.Unlock()
	writeJSON(w, `{"result":0,"msg":"","toUrl":"https://cloud.189.cn/redirect?ticket=T1"}`)
}

// decrypt reverses the driver's credential encryption.
func (f *fake189) decrypt(v string) (string, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(v, "{NRP}"))
	if err != nil {
		return "", err
	}
	pt, err := rsa.DecryptPKCS1v15(rand.Reader, f.priv, raw)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func (f *fake189) handleList(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("pageNum"))
	f.mu.Lock()
	pages := f.listPages
	f.mu.Unlock()
	if page < 1 || page > len(pages) {
		writeJSON(w, listPageEmpty)
		return
	}
	writeJSON(w, pages[page-1])
}

func (f *fake189) handleFileInfo(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("fileId")
	f.mu.Lock()
	rec, ok := f.files[id]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, `{"res_code":"FileNotFound","res_message":"文件不存在"}`)
		return
	}
	// The fileId/fileName/fileSize spelling; the driver also accepts id/name/size.
	writeJSON(w, fmt.Sprintf(
		`{"res_code":0,"res_message":"成功","fileId":%q,"fileName":%q,"fileSize":%d,"md5":%q,"parentId":%q,"lastOpTime":%q,"isFolder":%t}`,
		rec.id, rec.name, rec.size, rec.md5, rec.parent, rec.mtime, rec.folder))
}

func (f *fake189) handleDownloadURL(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("fileId")
	f.mu.Lock()
	_, ok := f.files[id]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, `{"res_code":"FileNotFound","res_message":"文件不存在"}`)
		return
	}
	writeJSON(w, fmt.Sprintf(`{"res_code":0,"fileDownloadUrl":%q}`, f.srv.URL+"/cdn/"+id))
}

func (f *fake189) handleCreateFolder(w http.ResponseWriter, rec recorded) {
	parent := rec.Form.Get("parentFolderId")
	name := rec.Form.Get("folderName")
	f.mu.Lock()
	for _, r := range f.files {
		if r.parent == parent && r.name == name {
			f.mu.Unlock()
			writeJSON(w, `{"res_code":"FileAlreadyExists","res_message":"同名文件夹已存在"}`)
			return
		}
	}
	id := strconv.Itoa(300 + len(f.files))
	f.files[id] = &frec{id: id, name: name, parent: parent, folder: true, mtime: "2024-03-04 11:00:00"}
	f.mu.Unlock()
	writeJSON(w, fmt.Sprintf(`{"res_code":0,"id":%s,"name":%q,"parentId":%q,"createDate":"2024-03-04 11:00:00"}`, id, name, parent))
}

func (f *fake189) handleRename(w http.ResponseWriter, rec recorded) {
	id := rec.Form.Get("fileId")
	name := rec.Form.Get("destFileName")
	if id == "" {
		id, name = rec.Form.Get("folderId"), rec.Form.Get("destFolderName")
	}
	f.mu.Lock()
	r, ok := f.files[id]
	if ok {
		r.name = name
	}
	f.mu.Unlock()
	if !ok {
		writeJSON(w, `{"res_code":"FileNotFound","res_message":"文件不存在"}`)
		return
	}
	writeJSON(w, `{"res_code":0,"res_message":"成功"}`)
}

func (f *fake189) handleBatchTask(w http.ResponseWriter, rec recorded) {
	var infos []struct {
		FileID   string `json:"fileId"`
		FileName string `json:"fileName"`
		IsFolder int    `json:"isFolder"`
	}
	if err := json.Unmarshal([]byte(rec.Form.Get("taskInfos")), &infos); err != nil {
		writeJSON(w, `{"res_code":"InvalidParameter","res_message":"bad taskInfos"}`)
		return
	}
	f.mu.Lock()
	for _, in := range infos {
		r, ok := f.files[in.FileID]
		if !ok {
			continue
		}
		switch rec.Form.Get("type") {
		case "MOVE":
			r.parent = rec.Form.Get("targetFolderId")
		case "DELETE":
			delete(f.files, in.FileID)
		}
	}
	f.mu.Unlock()
	writeJSON(w, `{"res_code":0,"taskId":"task-1"}`)
}

func (f *fake189) handleCheckTask(w http.ResponseWriter, rec recorded) {
	id := rec.Form.Get("taskId")
	f.mu.Lock()
	f.taskChecks[id]++
	n := f.taskChecks[id]
	f.mu.Unlock()
	// The first poll reports "still running" so the wait loop is exercised.
	status := 1
	if n > 1 {
		status = taskStatusDone
	}
	writeJSON(w, fmt.Sprintf(`{"res_code":0,"taskId":%q,"taskStatus":%d}`, id, status))
}

func (f *fake189) handleCDN(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.cdnRanges = append(f.cdnRanges, r.Header.Get("Range"))
	f.cdnAgents = append(f.cdnAgents, r.Header.Get("User-Agent"))
	fail := f.cdnFailNext
	f.cdnFailNext = false
	blob := f.blob
	f.mu.Unlock()
	if fail {
		http.Error(w, "link expired", http.StatusForbidden)
		return
	}
	start, end := 0, len(blob)-1
	if rh := r.Header.Get("Range"); strings.HasPrefix(rh, "bytes=") {
		lo, hi, _ := strings.Cut(strings.TrimPrefix(rh, "bytes="), "-")
		start, _ = strconv.Atoi(lo)
		if hi != "" {
			end, _ = strconv.Atoi(hi)
		}
		if end >= len(blob) {
			end = len(blob) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(blob)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(blob[start : end+1])
		return
	}
	w.Write(blob)
}

func (f *fake189) handleInitUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	exists := 0
	size, _ := strconv.ParseInt(q.Get("fileSize"), 10, 64)
	f.mu.Lock()
	f.initSize = size
	if f.rapidUpload && q.Get("fileMd5") != "" {
		exists = 1
	}
	f.mu.Unlock()
	writeXML(w, fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<result>
  <code>SUCCESS</code>
  <data>
    <uploadType>1</uploadType>
    <uploadHost>%s</uploadHost>
    <uploadFileId>77001</uploadFileId>
    <fileDataExists>%d</fileDataExists>
  </data>
</result>`, xmlEsc(f.srv.URL), exists))
}

func (f *fake189) handleUploadURLs(w http.ResponseWriter, r *http.Request) {
	info := r.URL.Query().Get("partInfo")
	num, _, _ := strings.Cut(info, "-")
	n, _ := strconv.Atoi(num)
	if n < 1 {
		writeXML(w, `<?xml version="1.0" encoding="UTF-8"?><result><code>InvalidParameter</code><message>bad partInfo</message></result>`)
		return
	}
	header := "Content-Type=application/octet-stream&Authorization=OSS ak-1:sig-" + strconv.Itoa(n) + "&Date=Mon, 04 Mar 2024 11:00:00 GMT"
	writeXML(w, fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<result>
  <code>SUCCESS</code>
  <uploadUrls>
    <partNumber_%d>
      <requestURL>%s</requestURL>
      <requestHeader>%s</requestHeader>
    </partNumber_%d>
  </uploadUrls>
</result>`, n, xmlEsc(fmt.Sprintf("%s/oss/part/%d/77001", f.srv.URL, n)), xmlEsc(header), n))
}

func (f *fake189) handlePartPUT(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	n, _ := strconv.Atoi(parts[len(parts)-2])
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.partBodies[n] = body
	f.partHeaders[n] = r.Header.Clone()
	f.mu.Unlock()
	w.Header().Set("ETag", `"etag-part-`+strconv.Itoa(n)+`"`)
	w.WriteHeader(http.StatusOK)
}

func (f *fake189) handleCommit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("uploadFileId") == "" {
		writeXML(w, `<?xml version="1.0" encoding="UTF-8"?><result><code>InvalidParameter</code><message>no uploadFileId</message></result>`)
		return
	}
	f.mu.Lock()
	size := f.initSize
	f.files["90001"] = &frec{id: "90001", name: "up.bin", parent: testRootID, size: size, md5: q.Get("fileMd5"), mtime: "2024-03-05 10:00:00"}
	f.mu.Unlock()
	writeXML(w, fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<result>
  <code>SUCCESS</code>
  <file>
    <userFileId>90001</userFileId>
    <fileName>up.bin</fileName>
    <fileSize>%d</fileSize>
    <fileMd5>%s</fileMd5>
    <createDate>2024-03-05 10:00:00</createDate>
  </file>
</result>`, size, xmlEsc(q.Get("fileMd5"))))
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	io.WriteString(w, body)
}

func writeXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	io.WriteString(w, body)
}

func xmlEsc(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// newDriver builds a driver wired to the fake, with the fast retry policy and
// poll interval tests need.
func newDriver(t *testing.T, f *fake189, opt Options) *Tianyi {
	t.Helper()
	if opt.Client == nil {
		opt.Client = httpx.New(httpx.Options{
			Remote: "tianyi-test",
			Policy: retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}, MaxAttempts: 3},
		})
	}
	opt.BaseURL, opt.AuthURL, opt.WebURL, opt.UploadURL = f.srv.URL, f.srv.URL, f.srv.URL, f.srv.URL
	if opt.AccessToken == "" && opt.Username == "" {
		opt.AccessToken = testAccessToken
	}
	d, err := NewWithOptions("t189", opt)
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	d.pollInterval = time.Millisecond
	return d
}

const loginPageJS = `<html><script>
  var appId = "8025431004";
  var captchaToken = "CAP-5";
  var lt = "LT-123-abc";
  var reqId = "REQ-9";
  var paramId = "PARAM-77";
  var returnUrl = 'https://m.cloud.189.cn/return';
</script></html>`

// listPage1 spells ids as JSON numbers, which is one of the two spellings 189
// uses; listPage2 spells them as strings.
const listPage1 = `{"res_code":0,"res_message":"成功","fileListAO":{"count":2,
  "folderList":[{"id":100,"name":"docs","parentId":-11,"lastOpTime":"2024-03-01 12:00:00","createDate":"2024-02-01 09:00:00"}],
  "fileList":[{"id":201,"name":"a.txt","size":10,"md5":"0CC175B9C0F1B6A831C399E269772661","parentId":-11,"lastOpTime":"2024-03-02 08:30:00"}]}}`

const listPage2 = `{"res_code":0,"res_message":"成功","fileListAO":{"count":1,
  "folderList":[],
  "fileList":[{"id":"202","name":"b.bin","size":2048,"md5":"900150983CD24FB0D6963F7D28E17F72","parentId":"-11","lastOpTime":"2024-03-03 10:00:00"}]}}`

const listPageEmpty = `{"res_code":0,"res_message":"成功","fileListAO":{"count":0,"folderList":[],"fileList":[]}}`
