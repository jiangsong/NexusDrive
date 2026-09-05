package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/journal"
	"gopkg.in/yaml.v3"
)

// This builds and executes the production binary: no test-only provider,
// VFS fault hook, or inherited in-memory storage participates in recovery.
func TestCopyProductionProcessKillAndDaemonResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "cloudfs")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build production binary: %v\n%s", err, out)
	}
	installCfg, installPath := uploadCLIConfig(t)
	install := exec.CommandContext(ctx, bin, "mcp", "install", "--client", "claude", "--config", installPath)
	if out, err := install.CombinedOutput(); err != nil || !bytes.Contains(out, []byte("mcpServers")) {
		t.Fatalf("registration initialized unusable provider: %v\n%s", err, out)
	}
	if _, err := os.Stat(installCfg.Cache.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registration created daemon storage: %v", err)
	}
	body := bytes.Repeat([]byte("real-cli-checkpoint!"), 120000)
	blocked := make(chan struct{}, 1)
	var resumed atomic.Bool
	var resumedBytes atomic.Int64
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>/source</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>%d</d:getcontentlength><d:getetag>"v1"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, len(body))
		case "GET":
			var start, end int64
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(body)) {
				http.Error(w, "invalid range", 416)
				return
			}
			if !resumed.Load() && start >= 1<<20 {
				select {
				case blocked <- struct{}{}:
				default:
				}
				<-r.Context().Done()
				return
			}
			if resumed.Load() {
				resumedBytes.Add(end - start + 1)
			}
			w.Header().Set("ETag", `"v1"`)
			http.ServeContent(w, r, "source", time.Time{}, bytes.NewReader(body))
		default:
			http.Error(w, "unexpected source mutation", 405)
		}
	}))
	defer source.Close()
	var destMu sync.Mutex
	var uploaded []byte
	var puts int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destMu.Lock()
		defer destMu.Unlock()
		switch r.Method {
		case "PROPFIND":
			if r.URL.Path != "/" && uploaded == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:">`)
			if uploaded != nil {
				fmt.Fprintf(w, `<d:response><d:href>/dest</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>%d</d:getcontentlength><d:getetag>"landed"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, len(uploaded))
			}
			fmt.Fprint(w, `</d:multistatus>`)
		case "PUT":
			var err error
			uploaded, err = io.ReadAll(io.LimitReader(r.Body, int64(len(body))+1))
			if err != nil {
				http.Error(w, "read failed", 500)
				return
			}
			puts++
			w.WriteHeader(201)
		default:
			http.Error(w, "unexpected target request", 405)
		}
	}))
	defer target.Close()
	cfg, p := uploadCLIConfig(t)
	cfg.Cache.BlockSize = 1 << 20
	cfg.Remotes = map[string]config.Remote{
		"source": {Type: "webdav", Extra: map[string]any{"url": source.URL}},
		"target": {Type: "webdav", Extra: map[string]any{"url": target.URL}},
	}
	cfg.Mounts[0].Layout = map[string]config.Layout{
		"/src": {Remote: "source", Root: "/", Mode: config.ModeReadonly},
		"/dst": {Remote: "target", Root: "/", Mode: config.ModeWriteback},
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	copyCmd := exec.CommandContext(ctx, bin, "cp", "/src/source", "/dst/dest", "--config", p)
	var copyLog bytes.Buffer
	copyCmd.Stdout, copyCmd.Stderr = &copyLog, &copyLog
	if err := copyCmd.Start(); err != nil {
		t.Fatal(err)
	}
	copyDone := make(chan error, 1)
	go func() { copyDone <- copyCmd.Wait() }()
	select {
	case <-blocked:
	case err := <-copyDone:
		t.Fatalf("cp exited before second range: %v\n%s", err, copyLog.String())
	case <-ctx.Done():
		copyCmd.Process.Kill()
		<-copyDone
		t.Fatalf("cp never reached checkpoint: %s", copyLog.String())
	}
	if err := copyCmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-copyDone; err == nil {
		t.Fatal("cp was not killed")
	}
	ro, err := journal.OpenReadOnly(filepath.Join(cfg.Cache.Dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	jobs, err := ro.CopyJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].State != journal.CopyPreparing || jobs[0].Checkpoint != 1<<20 {
		t.Fatalf("production cp did not persist prefix: %+v %v", jobs, err)
	}
	resumed.Store(true)
	// MCP-only mode starts the same daemon without requiring a FUSE mount.
	daemonCmd := exec.CommandContext(ctx, bin, "mcp", "--stdio", "--config", p)
	stdin, err := daemonCmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var daemonLog bytes.Buffer
	daemonCmd.Stderr = &daemonLog
	if err := daemonCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		daemonCmd.Process.Kill()
		if err := daemonCmd.Wait(); err != nil && t.Failed() {
			t.Logf("daemon: %v\n%s", err, daemonLog.String())
		}
	}()
	for {
		job, err := ro.GetCopy(ctx, jobs[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		rows, _, err := ro.ListActive(ctx, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == journal.CopySubmitted && len(rows) == 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("daemon recovery stalled: %+v %+v", job, rows)
		}
		time.Sleep(5 * time.Millisecond)
	}
	destMu.Lock()
	defer destMu.Unlock()
	if puts != 1 || !bytes.Equal(uploaded, body) {
		t.Fatalf("remote result: puts=%d size=%d", puts, len(uploaded))
	}
	if got, want := resumedBytes.Load(), int64(len(body))-(1<<20); got != want {
		t.Fatalf("restart fetched %d bytes, want only remaining %d", got, want)
	}
	for {
		_, online, err := control.CallCopies(ctx, cfg.Control.Socket, "", control.CopiesRequest{ID: jobs[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		if online {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("MCP-only daemon has no control endpoint")
		}
		time.Sleep(time.Millisecond)
	}
	if _, online, err := control.CallCopyMutation(ctx, cfg.Control.Socket, "", control.CopyMutationRequest{Action: "cancel", ID: jobs[0].ID}); !online || err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("MCP-only owner management is not wired: online=%v err=%v", online, err)
	}
}
