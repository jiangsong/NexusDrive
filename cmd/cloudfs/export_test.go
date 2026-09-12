package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/control"
	"cloudfs/internal/export"

	"github.com/google/uuid"
)

// cliExports is a manager that records what the CLI asked for. The command is
// what is under test — its grammar, its refusals and what it prints — and none
// of that needs a destination disk or a provider.
type cliExports struct {
	jobs    map[string]export.Job
	order   []string
	created export.Request
	acted   []string
}

func newCLIExports() *cliExports {
	return &cliExports{jobs: map[string]export.Job{}}
}

func (c *cliExports) put(j export.Job) {
	if _, ok := c.jobs[j.ID]; !ok {
		c.order = append(c.order, j.ID)
	}
	c.jobs[j.ID] = j
}

func (c *cliExports) Create(_ context.Context, req export.Request) (export.Job, error) {
	c.created = req
	j := export.Job{ID: uuid.NewString(), State: export.StateRunning, Sources: req.Sources, Dest: req.Dest,
		Options:    export.JobOptions{Mirror: req.Mirror, Verify: req.Verify},
		FilesTotal: 2, BytesTotal: 2048}
	c.put(j)
	return j, nil
}

func (c *cliExports) Job(_ context.Context, id string) (export.Job, error) {
	j, ok := c.jobs[id]
	if !ok {
		return export.Job{}, export.ErrNotFound
	}
	return j, nil
}

func (c *cliExports) Jobs(_ context.Context, limit int, after string) ([]export.Job, string, error) {
	out := make([]export.Job, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.jobs[id])
	}
	return out, "", nil
}

func (c *cliExports) Items(_ context.Context, id string) ([]export.Item, error) { return nil, nil }

func (c *cliExports) Progress(_ context.Context, id string) (export.Progress, error) {
	j := c.jobs[id]
	return export.Progress{BytesDone: j.BytesDone, BytesTotal: j.BytesTotal}, nil
}

func (c *cliExports) act(action, id string) error {
	if _, ok := c.jobs[id]; !ok {
		return export.ErrNotFound
	}
	c.acted = append(c.acted, action)
	return nil
}

func (c *cliExports) Pause(_ context.Context, id string) error  { return c.act("pause", id) }
func (c *cliExports) Resume(_ context.Context, id string) error { return c.act("resume", id) }
func (c *cliExports) Cancel(_ context.Context, id string) error { return c.act("cancel", id) }
func (c *cliExports) Forget(_ context.Context, id string) error { return c.act("forget", id) }

func exportCLI(t *testing.T) (*cliExports, string) {
	t.Helper()
	cfg, path := uploadCLIConfig(t)
	fake := newCLIExports()
	srv, err := control.NewServer(&control.Collector{Export: fake}).Start(context.Background(), cfg.Control.Socket, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return fake, path
}

// TestExportCLIStartsAndSteersAJob walks the grammar §5.6 names, end to end
// over the real control socket.
func TestExportCLIStartsAndSteersAJob(t *testing.T) {
	fake, path := exportCLI(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runExport(ctx, []string{"/photos", "/docs", "/Volumes/backup", "--verify", "--streams", "2", "--range-size", "8MiB", "--config", path, "--json"}, &out); err != nil {
		t.Fatalf("export: %v (%s)", err, out.String())
	}
	var started control.ExportStartResponse
	if err := json.Unmarshal(out.Bytes(), &started); err != nil || started.ID == "" {
		t.Fatalf("no job id in %s (%v)", out.String(), err)
	}
	if len(fake.created.Sources) != 2 || fake.created.Dest != "/Volumes/backup" {
		t.Fatalf("the last positional argument is the destination; got %+v", fake.created)
	}
	if !fake.created.Verify || fake.created.Streams != 2 || fake.created.RangeSize != 8<<20 {
		t.Fatalf("tunables did not reach the daemon: %+v", fake.created)
	}

	out.Reset()
	if err := runExports(ctx, []string{"list", "--config", path}, &out); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), started.ID) || !strings.Contains(out.String(), "/Volumes/backup") {
		t.Fatalf("list did not show the job: %s", out.String())
	}

	out.Reset()
	if err := runExports(ctx, []string{"show", started.ID, "--config", path}, &out); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out.String(), "destination") || !strings.Contains(out.String(), "/photos") {
		t.Fatalf("show did not describe the job: %s", out.String())
	}

	for _, action := range []string{"pause", "resume", "cancel"} {
		out.Reset()
		if err := runExports(ctx, []string{action, started.ID, "--config", path}, &out); err != nil {
			t.Fatalf("%s: %v (%s)", action, err, out.String())
		}
	}
	out.Reset()
	if err := runExports(ctx, []string{"forget", started.ID, "--confirm", "--config", path}, &out); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if !strings.Contains(out.String(), "forgotten export "+started.ID) {
		t.Fatalf("forget said nothing useful: %s", out.String())
	}
	want := []string{"pause", "resume", "cancel", "forget"}
	if strings.Join(fake.acted, ",") != strings.Join(want, ",") {
		t.Fatalf("the daemon saw %v, want %v", fake.acted, want)
	}
}

// TestExportCLIRefusesBeforeTouchingTheDaemon: every one of these is a typo a
// person makes at the shell, and each must be answered by the command rather
// than by a request that has already started deleting something.
func TestExportCLIRefusesBeforeTouchingTheDaemon(t *testing.T) {
	fake, path := exportCLI(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"mirror without confirm", []string{"/a", "/tmp/dest", "--mirror"}},
		{"confirm without mirror", []string{"/a", "/tmp/dest", "--confirm"}},
		{"no destination", []string{"/a"}},
		{"relative source", []string{"photos", "/tmp/dest"}},
		{"unknown flag", []string{"/a", "/tmp/dest", "--delete-source", "yes"}},
		{"zero streams", []string{"/a", "/tmp/dest", "--streams", "0"}},
	} {
		var out bytes.Buffer
		if err := runExport(ctx, append(tc.args, "--config", path), &out); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
	if len(fake.created.Sources) != 0 {
		t.Fatalf("a refused command still reached the daemon: %+v", fake.created)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"forget without confirm", []string{"forget", uuid.NewString()}},
		{"pause with confirm", []string{"pause", uuid.NewString(), "--confirm"}},
		{"unknown action", []string{"purge", uuid.NewString()}},
		{"show without an id", []string{"show"}},
		{"malformed id", []string{"show", "not-a-uuid"}},
	} {
		var out bytes.Buffer
		if err := runExports(ctx, append(tc.args, "--config", path), &out); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
	if len(fake.acted) != 0 {
		t.Fatalf("a refused command still reached the daemon: %v", fake.acted)
	}
}

// TestExportCLISaysTheDaemonIsNeeded: an export is a long job the daemon owns,
// so there is no offline mode — and the message has to say so rather than
// failing with a bare connection error.
func TestExportCLISaysTheDaemonIsNeeded(t *testing.T) {
	_, path := uploadCLIConfig(t) // no control server started
	ctx := context.Background()
	var out bytes.Buffer
	err := runExport(ctx, []string{"/a", "/tmp/dest", "--config", path}, &out)
	if err == nil || !strings.Contains(err.Error(), "daemon is not running") {
		t.Fatalf("export without a daemon: %v", err)
	}
	err = runExports(ctx, []string{"list", "--config", path}, &out)
	if err == nil || !strings.Contains(err.Error(), "daemon is not running") {
		t.Fatalf("exports without a daemon: %v", err)
	}
}

// TestExportCLIWaitReportsFailedFiles: --wait polls to the end, and a job that
// finished with failures must not exit zero — a script that copies 998 of 1000
// files and reports success is worse than one that fails.
func TestExportCLIWaitReportsFailedFiles(t *testing.T) {
	fake, path := exportCLI(t)
	ctx := context.Background()
	id := uuid.NewString()
	fake.put(export.Job{ID: id, State: export.StateDone, Sources: []string{"/a"}, Dest: "/tmp/dest",
		FilesTotal: 3, FilesDone: 2, FilesFailed: 1, BytesTotal: 3072, BytesDone: 2048})

	var out bytes.Buffer
	err := waitForExport(ctx, &out, mustSocket(t, path), "", id, false, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "files failed") {
		t.Fatalf("a job with failed files exited with %v", err)
	}
	if !strings.Contains(out.String(), "done") || !strings.Contains(out.String(), "1 failed") {
		t.Fatalf("the progress line says nothing about the failures: %q", out.String())
	}

	ok := uuid.NewString()
	fake.put(export.Job{ID: ok, State: export.StateDone, Dest: "/tmp/dest", FilesTotal: 1, FilesDone: 1, BytesTotal: 1, BytesDone: 1})
	out.Reset()
	if err := waitForExport(ctx, &out, mustSocket(t, path), "", ok, false, 5*time.Second); err != nil {
		t.Fatalf("a clean job exited with %v", err)
	}
}

func mustSocket(t *testing.T, configPath string) string {
	t.Helper()
	cfg, _, err := loadConfig(parseFlags([]string{"--config", configPath}))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Control.Socket
}
