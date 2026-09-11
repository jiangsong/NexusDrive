# Storage Pool v2 — Implementation Plan

Spec: `docs/pool-v2.md` (binding authority). TODO items: `TODO.md` T-29..T-33.
Branch: `feat/pool-v2`. Language: code and comments in English; user docs in Chinese.

## Global Constraints

- Build/test with `./gow` (never bare `go`): `./gow build ./...`, `./gow vet ./...` (baseline clean — introduce no new warnings), `./gow test ./...`, `./gow test -race ./...`.
- On this macOS host without macFUSE, `./internal/fusefs`, `./test/conformance`, `./test/e2e` hang; exclude them from full runs (`./gow test $(./gow list ./... | grep -v -e internal/fusefs -e test/conformance -e test/e2e)`). FUSE-level tests still must compile (`./gow vet ./internal/fusefs/`) and must `t.Skip` cleanly without `/dev/fuse`.
- Never branch on provider name; new behaviour differences go through `provider.Caps` fields.
- Anything not verifiable without a real account carries an `// UNVERIFIED: <what to verify>` comment.
- `internal/vfs` owns cache/consistency/upload decisions; `fusefs`/`mcpsrv` stay thin adapters.
- Perf tests (`test/perf`) assert provider call counts via `test/fakeprovider`, never wall clock (single exception: `TestPoolReadFanoutAddsBandwidth`/export parallelism tests which assert a ratio).
- TDD: write the failing test first, then implement. Keep files < 800 lines; new files 200–400 lines typical.
- Commit per task with conventional-commit subject (`feat:`/`fix:`/`test:`/`refactor:`), body explains why, and end with `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- Config YAML keys are snake_case; every new key has a default filled in `internal/config` validation and appears in `README.md` config example only once implemented (remove the 【规划中】 marker for what you ship).
- Existing invariants to respect (see `CLAUDE.md`): commit happens at FLUSH; conflict detection uses `RemoteVersion`; `cloudfs-local:` ids; one staging snapshot per inode; `meta.AdoptByIno` CAS.

---

## Task 1: fusefs read-side kernel options (FOPEN_KEEP_CACHE, readahead, MaxBackground, iosize, FileLseeker)

Files: `internal/fusefs/fs.go`, `internal/fusefs/platform_linux.go`, `internal/fusefs/platform_darwin.go`, `internal/fusefs/mount_test.go` (or existing test file).

1. In `(*node).Open` (`fs.go` ~line 432), return `fuse.FOPEN_KEEP_CACHE` in the flags result when the handle was opened read-only (`!write`). Write handles keep flags `0`.
2. Linux (`platform_linux.go`): default `o.MaxReadAhead = 4 << 20` (keep `CLOUDFS_KERNEL_READAHEAD` override); set `o.MaxBackground = 64`. Update the stale comment about a 128 KiB cap.
3. macOS (`platform_darwin.go`): append `iosize=1048576` to mount options; keep `MaxWrite`/`MaxReadAhead` at 64 KiB. Add `// UNVERIFIED: macFUSE 4.x honours iosize; Fuse-T ignores it.`
4. Implement `fs.FileLseeker` on `*file`: `SEEK_DATA` → `off` (if `off < size`, else `ENXIO`), `SEEK_HOLE` → `size`. Use `syscall.Errno` mapping consistent with the file.
5. Tests (must `t.Skip` without `/dev/fuse`/macFUSE, same guard as existing mount tests): (a) read a file twice through the mount; assert `OpStats()` `opRead` delta on the second pass is 0 (page cache served it) — name it `TestReadOnlyOpenKeepsKernelCache`; (b) `TestLseekReportsNoHoles` using `unix.Seek(fd, 0, SEEK_HOLE)` == size. Unit-test the lseek mapping without a mount too (pure function).
6. `./gow vet ./internal/fusefs/` clean. Commit: `feat(fusefs): keep the kernel page cache across read-only opens, widen readahead`.

## Task 2: Wire `Caps.MaxConnsPerHost` into the HTTP transport

Files: `internal/net/proxy/dialer.go`, `internal/net/proxy/dialer_test.go`, `internal/config/config.go` (+ test), `internal/daemon/daemon.go`, `internal/provider/instrument.go` (+ test).

1. `transportFor(o Outbound, conns int)`: set `MaxConnsPerHost: conns` and `MaxIdleConnsPerHost: conns` (conns ≤ 0 → keep today's 8 idle / unlimited). `ruleTransport` caches transports keyed by `(outbound, conns)`; when the limit changes, build a fresh transport and `CloseIdleConnections()` on the old one.
2. `func (m *Manager) ClientWithLimit(override string, timeout time.Duration) (*http.Client, func(conns int))`; existing `Client` becomes a wrapper calling it and discarding the setter.
3. Config: `Remote.MaxConns int` (`yaml:"max_conns"`), validated ≥ 0.
4. Daemon: where providers are built, use `ClientWithLimit`; after `provider.New` succeeds, `setConns(effective)` where `effective = rc.MaxConns` if > 0, else `p.Capabilities().MaxConnsPerHost` if > 0, else 8. Apply the override to the caps too: add `provider.WithMaxConns(p Provider, n int) Provider` in `instrument.go` that returns a wrapper whose `Capabilities()` reports `n` and which preserves every optional interface (follow the existing combinatorial wrapper pattern and extend `TestInstrumentKeepsTheOneRequestUploadBesideTheOthers`-style test so no optional interface is lost).
5. Tests: `dialer_test.go` — after `setConns(3)` the transport used by the client has `MaxConnsPerHost == 3`; changing to 5 yields a distinct transport. `daemon_test.go` (or config test) — a remote with `max_conns: 2` reports 2 via caps.
6. Commit: `feat(net): bound HTTP connections per remote by Caps.MaxConnsPerHost`.

## Task 3: `Transfer` rate-limit class for CDN byte streams

Files: `internal/net/ratelimit/ratelimit.go` (+ test), `internal/provider/provider.go` (`QPS` struct), `internal/config/config.go` (`QPS` override), `internal/daemon/daemon.go` (`qpsForClass`), `internal/provider/httpx/*` if Class is enumerated there, providers: `internal/provider/aliyun/aliyun.go` (ranged GET ~line 661), `internal/provider/quark/files.go` (ranged GET), `internal/provider/pan115/*`, `internal/provider/tianyi/*`, `internal/provider/baidu/baidu.go` (pcs byte stream) — only the byte-stream GET sites, never the link/API calls.

1. Add `ratelimit.Transfer` class. Provider `QPS` gains `Transfer float64`; **0 means "share the Download bucket"** — implement by resolving `Transfer` to the Download limiter when the effective rate is 0, so no existing behaviour changes.
2. Config `remotes.<n>.qps.transfer` override, same precedence as the others (config > Caps.QPS > default).
3. Provider defaults (`Caps.QPS.Transfer`): aliyun 16, quark 4, pan115 4, baidu 4, tianyi 4; each with `// UNVERIFIED: CDN request-rate threshold before risk control`. gdrive/onedrive/dropbox/box/s3/webdav/sftp/smb: leave 0.
4. Switch the byte-stream GET request sites to `Class: ratelimit.Transfer`. AIMD throttle handling and circuit breaker remain shared per account as today (verify the breaker keys do not become per-class; write a test if the limiter structure makes that ambiguous).
5. Tests: `ratelimit_test.go` — Transfer rate 0 draws from Download bucket (exhaust Download, Transfer blocks); Transfer rate > 0 has its own bucket. Provider test for aliyun (existing httptest harness): a ranged GET consumes a Transfer token, not a Download token (use the instrumented limiter or a fake limiter hook — follow whatever the aliyun tests already use for classes).
6. Update `docs/providers.md` QPS table with the new column (Chinese). Commit: `feat(ratelimit): meter CDN byte streams in their own Transfer class`.

## Task 4: `flight.Reserve` and readahead request coalescing

Files: `internal/vfs/flight.go` (+ `flight_test.go`), `internal/vfs/read.go`, `internal/vfs/vfs.go` (Options), `internal/daemon/daemon.go` (env), `test/perf/perf_test.go`.

1. `flight.go`: `func (f *flight[K, V]) Reserve(keys []K) (owned []K, resolve func(vals map[K]V, err error))` — registers a call for each key not already in flight; waiters on those keys block until `resolve`; keys already in flight are excluded from `owned`. Unit tests: waiter joins reserved key; resolve with error propagates; double reserve excludes.
2. `read.go` `maybeReadAhead`: group the window's missing, unclaimed blocks into contiguous runs of up to `coalesce = clamp(ReadaheadRequest/BlockSize, 1, 4)` blocks; one goroutine per run: claim each block in `prefetching`, `blockFlight.Reserve(keys)`, one `readRange` of `count*bs` (respect file end), split into blocks, `cache.PutAsync` each, `resolve`. On `shortRead` for a multi-block run: retry the run as single blocks and set a per-remote `maxReq` (sync.Map remote → 1 block) so that remote never coalesces again this process.
3. `Options.ReadaheadRequest int64` default: 16 MiB when the mount provider's `Caps.QPS.Download <= 16 && Caps.RangeRead`, else `BlockSize`. Env `CLOUDFS_READAHEAD_REQUEST` (bytes or size string) overrides globally. Validation: multiple of block size.
4. Perf: update `TestSequentialReadUsesWholeBlocks` to accept `[ceil(wantBlocks/coalesce), ceil(wantBlocks/coalesce)+3]` fetches with the harness block size (make the harness set `ReadaheadRequest` explicitly so the assertion is deterministic); add `TestReadaheadCoalescesContiguousBlocks`: 64 KiB blocks, `ReadaheadRequest = 256 KiB`, sequential read of 2 MiB → `ReadRange` calls ≤ 2 MiB / 256 KiB + 3. Add `TestShortRangeDisablesCoalescingForRemote` using fake `Faults` that truncates ranges > 1 block.
5. Commit: `feat(vfs): coalesce contiguous readahead blocks into one range request`.

## Task 5: `cache.policy` presets and `vfs.Mount.Policy`

Files: `internal/config/config.go` (+ `config_test.go`), `internal/vfs/vfs.go`, `internal/vfs/read.go` (consume `ReadaheadMax`), `internal/daemon/daemon.go` (`buildMounts`), `README.md`.

1. Config types:
   ```go
   type CachePolicy struct {
       Preset             string        `yaml:"preset"`               // media | photos | code | none
       SmallFileWhole     *bool         `yaml:"small_file_whole"`
       SmallFileThreshold Size          `yaml:"small_file_threshold"` // default 4MiB
       DirReadahead       *int          `yaml:"dir_readahead"`        // files ahead; default 32; 0 = off
       ReadaheadMax       Size          `yaml:"readahead_max"`        // default 64MiB
       ReadaheadRequest   Size          `yaml:"readahead_request"`    // default per Task 4
       ReadaheadLead      time.Duration `yaml:"readahead_lead"`       // default 8s
   }
   ```
   `Cache.Policy CachePolicy` (global defaults) and `Layout.Cache *CachePolicy` (per-prefix override). Presets: `media` = readahead_max 128MiB, readahead_request 16MiB, dir_readahead 0, dir_ttl 24h if unset; `photos` = dir_readahead 64, small_file_threshold 8MiB, readahead_max 16MiB; `code` = dir_readahead 128, small_file_threshold 1MiB, dir_ttl 1m if unset; `none` = defaults. Explicit keys win over the preset. Validation: threshold multiple of 64 KiB; `readahead_request` multiple of `block_size`; known preset names; `readahead_max ≥ block_size`.
2. `func ResolveCachePolicy(global CachePolicy, layout *CachePolicy, blockSize int64) vfs.CachePolicy` (values, no pointers) — pure, unit-tested (preset → global → layout precedence, and each validation error).
3. `vfs.CachePolicy{SmallFileWhole bool; SmallFileThreshold, ReadaheadMax, ReadaheadRequest int64; DirReadahead int; ReadaheadLead time.Duration}`; `Mount.Policy CachePolicy`. `maybeReadAhead` takes `ReadAheadBlocks` from `h.Mount.Policy.ReadaheadMax / BlockSize` (fall back to `Options.ReadAheadBlocks` when the mount policy is zero — keep `CLOUDFS_READAHEAD_BLOCKS` working as a global override). Task 4's `ReadaheadRequest` moves to the policy too.
4. vfs test: two mounts with different `ReadaheadMax` on the same fake → after 3 sequential reads the media mount issued more `ReadRange` than the code mount.
5. README: document `cache.policy` and `layout.<prefix>.cache` (Chinese, concise). Commit: `feat(config): per-prefix cache policy presets`.

## Task 6: `cache.PutWhole`

Files: `internal/cache/whole.go` (+ test), `internal/cache/blockcache.go` (if `installWhole` tail must be factored).

1. `func (c *Cache) PutWhole(k FileKey, r io.Reader, size int64) error`: stream into a temp file under `hydrated/` (`.tmp-<rand>`), fsync not required (cache is rebuildable), rename to the hydrated path, then perform exactly what `installWhole` does after the file exists (`attachWholeLocked`, `rememberKey`, presence bitmap = all present, accounting). Factor the shared tail into `installTemp(k, path, size)`. Respect `MinFree`/`MaxBytes` via the existing reservation path (`makeRoom`), returning `ErrNoSpace` the same way `Put` does. A concurrent `Put` of a block for the same key must not corrupt: take the same per-file lock `Put` takes.
2. Tests: PutWhole then `Has`/`ReadAt` all blocks → present with no block files under `blocks/`; `Complete(k)` true; eviction accounts the bytes once; PutWhole over an existing partial block set replaces them (blocks removed, no double charge); `ErrNoSpace` when over budget.
3. Commit: `feat(cache): PutWhole installs a fetched file directly as a hydrated object`.

## Task 7: Directory read-order prefetch (dir readahead)

Files: new `internal/vfs/read_dir_ahead.go` (+ `read_dir_ahead_test.go`), `internal/vfs/read.go` (hook + shared slots), `internal/vfs/vfs.go` (fields, cancel hooks in `invalidateListing`/`changedListing`/`dropPaths`/`Close`), `internal/control/metrics.go` (counter), `test/fakeprovider/fake.go` (`MaxInflight()`), `test/perf/perf_test.go`.

1. Types: `dirRun{dir uint64; mount *Mount; lastName string; run int; cursor string; ahead int; ctx context.Context; cancel func(); touched time.Time}`; `dirAhead{mu sync.Mutex; runs map[uint64]*dirRun (LRU capped at 32); slots map[string]chan struct{}}` on `FS`. `Handle.dirNoted bool`.
2. `noteFileRead(ctx, h)` called once per handle from `FS.Read` right after `activeReads++`. Only when `h.Mount.Policy.DirReadahead > 0`. First read in a dir → create run (`lastName = h.Node.Name`, `run = 1`), no query. Otherwise `meta.ChildrenPage(dir, after=lastName, 0, 32)`: name present → `run++`, `lastName = name`, `ahead--`; absent → reset run and cancel outstanding prefetch. `run >= 3` → `topUp(r)`.
3. `topUp`: page `ChildrenPage(after=cursor, limit = DirReadahead-ahead)`; skip dirs, `IsLocalOnly` ids, `Size > SmallFileThreshold`, `cache.Complete(key)`; spawn `prefetchWhole` for the rest; advance cursor; maintain `ahead`.
4. `prefetchWhole`: acquire per-remote slot (capacity `max(1, downloadSlots(caps)-1)` where `downloadSlots = clamp(int(math.Round(caps.QPS.Download)), 2, max(caps.MaxConnsPerHost, 2))`); `blockFlight.Reserve` all block keys of the file; one `readRange(0, size)`; `cache.PutWhole`; `resolve`; release. Context: `context.WithoutCancel(read ctx)` + 2 min timeout, cancelled by the run's `cancel`. Foreground never takes a slot. Optional gate: skip topUp while `f.Busy()` and the remote limiter's current rate < half its configured rate — implement only if `ratelimit` already exposes the current rate; otherwise leave a TODO comment referencing spec §3.2.
5. Cancel: `cancelDirAhead(dir)` from `invalidateListing`, `changedListing`, `dropPaths`; idle sweep (run untouched 30 s) piggybacks on an existing janitor/ticker if one exists in vfs, else a lazy check on next `noteFileRead`; `FS.Close` cancels all.
6. Block-level readahead goroutines (Task 4) must draw from the same per-remote slot semaphore (foreground exempt).
7. Metric `cloudfs_dir_readahead_files_total{result="hit"|"wasted"}`: hit = prefetched file later read through a handle; wasted = evicted before read (hook cache eviction callback if one exists; else count only hits and document).
8. `fakeprovider.Fake.MaxInflight() int` — high-water mark of concurrent calls (increment in `enter`, decrement on leave).
9. Perf tests (harness gets `DirReadahead: 8, SmallFileThreshold: 4096`): `TestDirectoryReadaheadMakesSiblingReadsFree` (64 × 100-byte files; read file000..002 in name order; wait until `Calls("ReadRange") >= 11` or 3 s; then reading file003..010 adds 0 `ReadRange`); `TestRandomOrderReadsDoNotTriggerDirectoryReadahead` (10 shuffled reads → exactly 10); `TestDirectoryReadaheadIsBoundedByRemoteSlots` (`QPS.Download=2, MaxConnsPerHost=3, Latency=20ms` → `MaxInflight() <= 2`); `TestDirectoryReadaheadSkipsLargeFiles` (one 64 KiB file among small ones stays absent); `TestDirectoryReadaheadStopsOnListingChange` (seed new file + `Refresh` → no `ReadRange` beyond already-claimed files).
10. Commit: `feat(vfs): prefetch sibling files when a directory is read in listing order`.

## Task 8: `internal/export` store and planner

Files: new package `internal/export/{store.go,schema.go,planner.go,types.go}` + tests; `internal/config/config.go` (`Export` section, `mcp.export_roots`).

1. Config `Export{Transfers int (4), Streams int (4), RangeSize Size (32MiB), MultiRangeMin Size (64MiB), YieldToForeground bool (true), DiskProbeInterval time.Duration (30s), JobsParallel int (1)}` with defaults + validation (all > 0; `RangeSize` ≥ 1 MiB). `MCP.ExportRoots []string`.
2. `Store` over `<cache.dir>/exports.db` (modernc sqlite, WAL, `busy_timeout=5000`, `SetMaxOpenConns(4)`, `meta(k,v)` with `schema_version=1`, flock like `journal/lock.go`). Schema exactly as spec §5.2 (`export_jobs`, `export_items`, `export_extras`). Methods: `CreateJob`, `GetJob`, `ListJobs(limit, cursor)`, `UpdateJobState(id, from revision, to state, reason)` (CAS on `revision`), `InsertItems(batch)`, `ClaimDue(jobID, n, now)` (pending→active), `UpdateItemProgress(job, rel, ranges, doneBytes)`, `FinishItem(job, rel, state, err)`, `ResetActive(jobID)` (active→pending, for restart), `Counts(jobID)`, `InsertExtras`, `ListExtras`, `Forget(jobID)`.
3. `Options{Mirror, Verify, PreserveMTime bool; Transfers, Streams int; RangeSize int64}` JSON in `options`; `Binding{Prefix, Remote, RootID, AccountBinding string}` JSON in `bindings`.
4. Planner `Plan(ctx, fs *vfs.FS, job)`: for each source `fs.StatPath`; dirs walked with `fs.ReadDirPath` (breadth-first); items inserted in batches of 500 per tx; record `meta identity` (use whatever `vfs`/`meta` exposes for `CopySpec`) and per-mount binding. Dest pre-check: `MkdirAll`, `st_dev` recorded, marker `<dest>/.cloudfs-export.json {sources, job_id, meta_identity}` written/refreshed; `--mirror` refused (`ErrMirrorNeedsMarker`) unless a marker with the same sources exists. Skip detection: existing final file with same size and `|mtime diff| <= 2s` → `skipped`; with `Verify` and a known provider hash, also hash the local file. Order: items with `size >= MultiRangeMin` first, then path order. `extras` computed for mirror (files under dest not in the plan, excluding the marker and `*.cloudfs-part`).
5. Tests (fake provider + real temp dirs): schema created; CAS state transition rejects stale revision; plan of a warm tree makes 0 provider `List` calls (list once to warm, reset counters, plan); skip detection incl. the 2 s tolerance; mirror refusal; extras listing; restart resets active items.
6. Commit: `feat(export): durable export job store and planner`.

## Task 9: `internal/export` runner and manager

Files: `internal/export/{runner.go,manager.go,source.go,errors.go}` + tests; `internal/daemon/daemon.go` (start manager, `SetBusy`); `test/perf/export_test.go`.

1. `Manager` (pattern: `vfs/copy_worker.go` `StartCopies`): one goroutine, `wake chan struct{}`, resumes non-terminal jobs at start, at most `JobsParallel` runners. `SetBusy(func() bool)`; `Submit(spec) (id, error)`; `Pause/Resume/Cancel/Forget(id)`; `Progress(id)` (`BytesDone, BytesTotal, Rate, ETA, Files…, PerMember`). Progress EWMA over 10 s; publish via the daemon's SSE bus as `export.progress` once per second while running (find the existing events bus used by `/events`).
2. Runner: dispatcher pulls due items → global `transfers` semaphore (`min(cfg.Transfers, Σ MaxConnsPerHost of involved mounts, 8)`) → `exportFile`.
3. `source.go`: pick the source for an item in this order: (a) `cache.OpenWhole(key)` → local copy (try `clonefile`/`FICLONE` reflink via a small `reflink(dst, src)` helper with platform files, fall back to `io.Copy` 1 MiB buffer); (b) `vfs.IsLocalOnly(remoteID)` → read the journal blob read-only (expose the minimal accessor on `vfs.FS` if needed: `LocalBlobPath(id)`); (c) partially cached → `cache.ReadAt` for present blocks, provider range for missing; (d) provider `RangeReaderAt`/`ReadRange` directly via `mount.Provider` (NEVER `FS.Read`).
4. Network path: `<dest>/<rel>.cloudfs-part` opened `O_CREATE|O_WRONLY` (no truncate), `Truncate(size)` once; chunks of `RangeSize`; skip chunks present in `ranges` bitmap; per-file `streams` (1 when `size < MultiRangeMin`); pooled buffers; `pwrite`; checkpoint `ranges` after each chunk; `fsync` every 256 MiB and before rename. Hash streamed matching `hash_type` when known (compute over the whole file after all chunks land by re-reading the part when streams > 1; single-stream files hash inline). Mismatch → delete part, `attempts++`, retry ≤ 2 → `failed`. `Verify` → re-read after rename and compare. Then `Rename`, `Chtimes(mtime)`, `done`. Directory mtimes post-order at job end.
5. Error → state mapping exactly per spec §5.5: disk errors / `st_dev` change → items `pending`, job `paused(disk)`, probe every `DiskProbeInterval` and auto-resume; unavailable → item backoff 30 s·2^n capped 1 h without consuming attempts, all blocked → `paused(unavailable)`; risk control → `paused(risk_control)` until breaker open-until (use `retry.Classify`); auth → `paused(auth)`; not-found/conflict/binding mismatch → item `failed`, job continues; hash mismatch → retry then failed; cancel → `cancelled`, parts kept until `Forget` (which deletes parts and rows); mirror deletion runs after all items when `Mirror` and results in `done` even if deletions fail (warning in `last_error`).
6. Yield: before each chunk, if `YieldToForeground && busy()` wait up to 5 s in 100 ms steps.
7. Tests (reuse `test/perf/pool_test.go`'s pool harness — export a helper `newPoolHarness` from a shared `test/poolharness` package or copy minimal setup): the seven cases in spec §5.7 (spread ≈ N/3±2 per member; cached/pinned → 0 `ReadRange`; crash resume only remaining chunks; ENOSPC & st_dev → paused(disk) then resume; mirror marker; hash mismatch → failed after 2 retries; warm plan `List == 0`). `test/perf/export_test.go`: `Faults.Latency=20ms`, 3 members vs 1 member wall time ratio ≤ 0.5 (allow 0.6 on CI via constant).
8. Commit: `feat(export): parallel, resumable export of virtual paths to a local directory`.

## Task 10: Export control API and CLI

Files: `internal/control/exports.go` (+ test), `internal/control/metrics.go` (`routes()` registration), `internal/control/export_client.go`, `cmd/cloudfs/export.go`, `cmd/cloudfs/exports.go`, `cmd/cloudfs/main.go` dispatch + help table, i18n catalog for CLI if one exists.

1. Routes: `POST /export {sources[], dest, mirror, verify, confirm, transfers, streams, range_size}` → `{id}` (mirror requires `confirm:true`); `GET /exports?limit&cursor`; `GET /exports/<id>` (job + progress + per-member load); `POST /exports/pause|resume|cancel|forget {id, confirm}` (forget requires confirm). Register in `routes()` so `security_all_routes_test.go` covers them; follow the auth/guard pattern of `/copies*`.
2. CLI: `cloudfs export <vpath>... <dest-dir> [--mirror --confirm] [--verify] [--transfers N] [--streams N] [--range-size SIZE] [--wait] [--json]` (daemon required; `--wait` polls and prints one progress line, exit code 1 when `files_failed > 0`); `cloudfs exports list|show|pause|resume|cancel|forget <id> [--confirm] [--json] [--limit --cursor]`. Mirror the `copies` commands' structure and JSON output conventions.
3. Tests: route contract tests in the style of `copies_test.go`; CLI arg parsing tests; i18n coverage test passes (add keys in both languages).
4. README: replace the 【规划中，T-32】 lines with real entries. Commit: `feat(cli): cloudfs export and exports commands`.

## Task 11: Export MCP tools and web UI screen

Files: `internal/mcpsrv/export_jobs.go` (+ test), `internal/mcpsrv/server.go` (register), `internal/control/web/screens/exports.js`, router/nav registration, `internal/control/web/i18n.js` (both languages), web tests under `internal/control/web/_tests`, `docs/mcp.md`.

1. MCP tools: `export {paths[], dest, mirror?, verify?}` — `dest` must resolve under one of `mcp.export_roots` (reject otherwise; read-only server refuses); `list_export_jobs {limit?, cursor?}` (AEAD cursor like `copy_jobs.go`); `get_export_job {id}`; `cancel_export_job {id}`. Tool annotations: export/cancel destructive=false but non-idempotent; list/get read-only.
2. UI screen `#/exports`: table (progress bar, rate, ETA, state colour, pause/resume/cancel/forget with typed confirmation for forget and mirror), "New export" form (source picker using the existing `/fs/list` browser component, dest text input + desktop-bridge directory picker when available, mirror/verify toggles), detail panel with per-member in-flight/throughput. SSE `export.progress` updates rows live.
3. Tests: mcpsrv tool tests (roots enforcement, read-only refusal, cursor round-trip); `node --test internal/control/web/_tests` incl. i18n coverage.
4. `docs/mcp.md`: document the four tools (Chinese). Commit: `feat(ui,mcp): export jobs screen and MCP export tools`.

## Task 12: Pool read fan-out (`pickReplica`)

Files: `internal/pool/members.go`, `internal/pool/read.go`, `internal/pool/pool.go` (caps summary), `internal/pool/db.go`/`resolve` (replica cache), `internal/config/config.go` (`read_fanout`), new `internal/pool/read_test.go`, `test/perf/pool_test.go`.

1. `member` gains `inflight atomic.Int32`, `served atomic.Int64`, `maxInflight int` (= `Capabilities().MaxConnsPerHost`, default 4), EWMA latency per MiB (`noteLatency(d time.Duration, n int64)`), plus `measured bool`.
2. `pickReplica(reps []replicaRow) (*member, replicaRow, bool)`: usable ∧ (`inflight < maxInflight` unless every usable member is saturated); score = `inflight/maxInflight + ewmaPerMiB/minEwma` (unmeasured members score as if best latency, so they get probed); tie → least `served`; no declaration-order stickiness. `Tier == unofficial` members (`Caps.Tier`) under `read_fanout: auto` allow at most one concurrent stream per file: track `(member, fileID)` inflight in a small map; `off` = today's ordered behaviour; `all` = no tier restriction.
3. `ReadRange`/`ReadRangeAt`/`DownloadURL`: loop pick → `inflight++` → call → `inflight--`, `noteLatency`; unreachable → remove from candidate set and re-pick; `ErrNotFound` → `Stat` the replica once before marking `missing` (transient 404 during link refresh must not drop a replica). `tryReplicas` stays for repair's `replicaReader`.
4. `resolveFile` cache: `(id, version) → []replicaRow` with 5 s TTL, invalidated in `finishUpload`, `relocate`, `Delete`, `upsertReplica`, and when a replica is marked `missing`.
5. Config `pools.<n>.read_fanout: off|auto|all` (default `auto`), validated.
6. Tests: `TestReadFanoutSpreadsBlocks` (12 blocks, 8 concurrent `ReadRange` → each of 3 members `Calls("ReadRange")` 4±1), `TestReadFanoutSkipsDownMember`, `TestReadFanoutPrefersFast` (`Faults.Latency` on one member → fewer calls there), `TestUnofficialMemberGetsOneStreamPerFile`, `TestTransient404DoesNotDropReplica`. Update perf baselines that assumed member 0 (`TestPoolWriteThenReadIsLocal` etc. — keep total-count assertions). `test/perf`: `TestPoolReadFanoutAddsBandwidth` (Latency 20 ms, 48 MiB, 3 members < 0.5× single; ratio constant tolerant to 0.6).
7. Commit: `feat(pool): spread block reads across replicas by load and latency`.

## Task 13: Rate-aware readahead window and global in-flight budget

Files: `internal/vfs/read.go`, `internal/cache/writebehind.go` (`WriteBehindBudget()`), `internal/vfs/vfs.go`, vfs tests, `test/perf/perf_test.go`.

1. `Handle` gains `runStart time.Time; runBytes int64`. Window growth: keep doubling but (a) cap `target = clamp(R × ReadaheadLead, 2 blocks, ReadaheadMax)` where `R = runBytes/elapsed` once the run is ≥ 1 s old; (b) grow only on a stall (sequential read whose block is neither cached nor claimed); (c) global budget: total claimed prefetch blocks × BlockSize ≤ `cache.WriteBehindBudget()` (= write_behind − subBlockReserve); when exhausted, stop spawning (do not block the foreground). (d) when the mount provider's `Caps.MaxConnsPerHost > 1`, the initial window is `min(ReadaheadMax/BlockSize, MaxConnsPerHost)` blocks instead of 1.
2. Tests (fake with latency, deterministic clock injection if the vfs has one; otherwise sleep-based with generous margins but asserting counts): slow reader (1 MiB/s simulated via paced reads) settles at a window ≤ 8 MiB (count blocks fetched ahead of the read position); two handles reading concurrently never exceed the budget (fake `MaxInflight() × block ≤ budget`); `TestRandomReadsDoNotArmReadAhead` unchanged and still passing; initial window uses `MaxConnsPerHost`.
3. Commit: `feat(vfs): size the readahead window by reader rate and a global in-flight budget`.

## Task 14: Sparse whole-file cache layout for large files

Files: `internal/cache/blockcache.go`, `internal/cache/whole.go`, `internal/cache/writebehind.go`, new `internal/cache/sparse.go` (+ tests), `internal/cache/reflink_linux.go`/`reflink_other.go`, `internal/config/config.go` (`cache.whole_layout_min`, default 64MiB).

1. Files with `size >= WholeLayoutMin`: `put`/write-behind write blocks at offset into `hydrated/<fh>.part` (`O_EXCL` create + `Truncate(size)`), presence tracked in `files[fh].present` and persisted to `<fh>.part.bitmap` (bitmap + per-block crc32) after each flush; `readBlock` gains the `.part` branch; when all blocks present → rename `.part → <fh>`, `attachWholeLocked` (no copy). Eviction unit = whole file (`wholeObject`, charge `st_blocks*512`); `reload` reattaches `.part` with a valid bitmap, deletes it otherwise. Passthrough/`OpenWhole` only after completion.
2. `Hydrate` for files below the threshold on Linux: try `unix.IoctlFileCloneRange` per block first (`reflink_linux.go`), fall back to copy; other platforms copy.
3. Tests: cold read of a 256 MiB (use 8 MiB with a 64 KiB block and `WholeLayoutMin = 1 MiB` in tests) file writes exactly `size` bytes to the cache dir (measure via `du`-style walk of written bytes or count write syscalls through an injected writer); `reload` with a valid bitmap keeps blocks readable; corrupt bitmap → part deleted; eviction charges once.
4. Commit: `feat(cache): write large files straight into a sparse hydrated object`.

## Task 15: fusefs splice read and in-mount copy_file_range

Files: `internal/fusefs/fs.go`, `internal/fusefs/platform_*.go`, `internal/vfs/vfs.go` (`OpenLocal` lease accessor if missing), tests.

1. `file.Read`: when `FS.OpenLocal(handle)` returns a hydrated fd lease (hold it on the `file`, release in `Release`), return `fuse.ReadResultFd(fd, off, n)`; else current path.
2. Implement `fs.NodeCopyFileRanger` on `node` for in-mount copies only: source hydrated → `unix.CopyFileRange` from the cache fd into the destination write handle's staging file via a new `vfs.FS.WriteFromFD(ctx, h, srcFD, srcOff, dstOff, n)` (goes through the normal write path accounting: `pendingSize`, hashes); otherwise return `ENOTSUP` so the kernel falls back. Document (comment) that cross-superblock copies never reach this hook.
3. Tests skip without FUSE; unit-test `WriteFromFD` in vfs (bytes land in staging, hashes consistent with `Write`).
4. Commit: `feat(fusefs): splice hydrated reads and copy_file_range inside the mount`.

## Task 16: Pool config v2 — rules, classes, failure domain, write mode, rebalance settings

Files: `internal/config/config.go` (+ tests), `internal/config/edit_pools.go` (+ tests), `internal/pool/marker.go` (+ test), `internal/daemon/daemon.go` (`buildPool` passes `Classes`/`Domain`), `cmd/cloudfs/pool.go` (`pool rule add|remove`, `pool member set --class`), `docs/pool.md`.

1. Types: `PoolMember.Class []string`; `PoolRule{Prefix string; Replicas int; Prefer, Avoid, Require []string}`; `Pool.Rules []PoolRule; FailureDomain string (account|provider|member, default account); WriteMode string (relaxed|strict, default relaxed); MinReplicasTimeout time.Duration (2m); Rebalance PoolRebalance{TargetSkew float64 (0.10), AutoBackfill bool (true), MaxRate Size (30MiB), PauseBetween time.Duration (500ms)}`. Validation: prefixes cleaned, absolute, unique; `replicas ≤ len(members)`; class names referenced by rules exist on some member; enums.
2. Editors: `SetPoolField` whitelist += `write_mode`, `failure_domain`, `min_replicas_timeout`; `AddPoolRule`, `RemovePoolRule`, `SetPoolMemberField(pool, remote, "class", value)` — comment-preserving YAML surgery under the file lock like the existing editors; tests round-trip comments.
3. `pool.Member` gains `Classes []string`, `Domain string`; daemon computes `Domain` per `failure_domain` (`account` → the effective account binding used by journal v11; `provider` → remote type; `member` → member name).
4. Marker `Settings` adds `Rules`, `FailureDomain` (digest changes → document the one-time epoch notice in `docs/pool.md`).
5. CLI: `cloudfs pool rule add <pool> --prefix /x [--replicas N] [--prefer a,b] [--avoid c] [--require d]`, `pool rule remove <pool> --prefix /x`, `pool member set <pool> <remote> --class a,b`.
6. Commit: `feat(pool): placement rules, member classes and failure domains in config`.

## Task 17: Placement v2 and quota-full relocation

Files: `internal/pool/placement.go` (+ `placement_test.go`), `internal/pool/repair.go` (`repairTarget` removal), `internal/pool/write.go`, `internal/pool/db.go` (`member_usage`, schema version), `internal/pool/status.go`, `internal/pool/scrub.go`, `internal/pool/drain.go`, `internal/provider/provider.go` (`ErrQuotaExceeded`, `ErrRestartUpload`), `internal/net/retry/classify.go` (`ClassQuota`), `internal/upload/uploader.go`, driver mappings (gdrive 403 `storageQuotaExceeded`, onedrive/webdav 507, aliyun `QuotaExhausted`, sftp/smb `ENOSPC`) each `// UNVERIFIED`, `test/fakeprovider` (`Faults.QuotaAfterBytes`), `internal/upload` tests, `test/perf/pool_test.go`.

1. `ruleFor(path) PoolRule` (longest prefix; default = pool settings); `targetFor(path) (int, capped bool)` replaces every `replicaTarget()` call (`repair.go`, `scrub.go` TrimOnce, `status.go`, `drain.go`, `write.go` hold/queue gating, `Quota()` default rule).
2. `candidates(ctx, path)` ordering: holds path → passes `require` (hard) → prefer match → domain not shared with a current holder (soft demotion) → not `avoid` (soft) → `fullUntil` not in future → `free×weight` → weight → declaration order. Known space before unknown. Delete `repairTarget`; repair uses `candidates(path)` minus current holders.
3. Quota: `provider.ErrQuotaExceeded`, `provider.ErrRestartUpload`; `retry.ClassQuota`; driver mappings. Pool: `BeginUpload` on quota → `markFull(member, 10m)` (`space.quota.Free=0`, `fetched=now`, `fullUntil`) and continue; `UploadPart`/`CompleteUpload` on quota → `markFull` and return `fmt.Errorf("%w: %w", ErrQuotaExceeded, ErrRestartUpload)`; `copyReplica` on quota → `markFull`, no divergence, next candidate. Uploader: `case retry.ClassQuota` → if `errors.Is(err, ErrRestartUpload)` → `Journal.SetSession(nil)` (drop parts, as the expired-session path) and `Retry` with zero delay; else dead-letter with reason `quota`. Status: member badge `full`.
4. `member_usage(member PRIMARY KEY, bytes, files)` maintained in `upsertReplica` and replica deletes; `free()` uses it instead of `SUM(size)`; pool db `meta.schema_version` introduced (=2) with migration creating the table and backfilling from `replicas`.
5. Tests per spec §6.7 placement/write/repair(quota) items + `TestPlacementSpreadsAcrossDomains` in `test/perf/pool_test.go`; uploader test for the restart path.
6. Commit: `feat(pool): rule-driven placement with failure domains and quota-full relocation`.

## Task 18: Repair via server-side copy, `min_replicas` honesty, trim ordering

Files: `internal/pool/repair.go` (+ tests), `internal/pool/write.go` (`finishUpload` strict path), `internal/pool/status.go`, `internal/pool/scrub.go` (`TrimOnce`), `internal/pool/drain.go` (`dropReplica` helper), `internal/control/doctor_pool.go`, `test/fakeprovider` (`Copy` support), `docs/pool.md`.

1. `copyReplica`: before `openSource`, if `dst.p.Capabilities().ServerCopy`, `dst.p` implements `provider.ServerCopier`, and a live replica exists on a member with the same `Domain` → `Copy(ctx, src.remoteID, dstDirID, name)`; `ErrUnsupported`/`ErrNotFound` → byte copy; ambiguous failure (timeout/5xx) → `enqueueRepair` with backoff and set a flag so the next attempt runs `ScrubPath(path)` first (adopt a landed copy through listing) — never a blind second copy.
2. `write_mode: relaxed`: `min_replicas` = alarm threshold — files with `live < min` get repair priority 2, `Availability.State = degraded`, `reason = "below min_replicas"`, `Report.BelowMin`, doctor line. `strict`: `finishUpload` commits the index then synchronously runs `repairPath` from the hold with deadline `min_replicas_timeout`; on timeout return success and leave the file queued.
3. `dropReplica(member, path)` helper shared by `DrainOnce`, `TrimOnce`, and Task 19's rebalance. `TrimOnce` trims the replica with the worst placement score (fullest / avoided / duplicate domain), not the last-declared.
4. Tests: server copy used when same domain (`Calls("Copy") == 1`, `UploadPart == 0`); ambiguous failure → scrub then adopt, no duplicate copy; relaxed alarm surfaces in `Report`/`Availability`; strict waits then succeeds on timeout; trim picks the worst-scored replica.
5. `docs/pool.md`: rewrite the `min_replicas` row and the 写 paragraph honestly (Chinese). Commit: `feat(pool): server-side repair copies, honest min_replicas, score-based trimming`.

## Task 19: Rebalance and backfill

Files: new `internal/pool/rebalance.go` (+ `rebalance_test.go`), `internal/pool/db.go` (`rebalance_queue`, `movePaths`), `internal/pool/health.go` (independent ticker goroutine, `SetBusy`), `internal/pool/status.go` (`Report.Rebalance`), `internal/control/pool_http.go` (`POST /pool/rebalance` + `routes()`), `internal/control/pool_client.go`, `cmd/cloudfs/pool.go` (`pool rebalance`), web pool screen (fill bars + button), `internal/daemon/daemon.go` (`SetBusy` wiring), `test/perf/pool_test.go`, `test/chaos` case.

1. Table `rebalance_queue(path PK, from_member, to_member, size, state pending|copied|done|failed, attempts, next_at, last_error, plan_id, created_at)` + index; `movePaths` includes it; schema version 3.
2. `PlanRebalance(ctx, targetSkew) (Plan, error)`: fill ratio per in-service member from `memberQuota` or `capacity − member_usage`; `skew = max − min`; while `skew > target`: fullest F, emptiest E; candidates = replicas on F not on E whose rule allows E (`require`/`avoid`/`canHold`/domain), largest first, until moved ≈ `(fillF − fillE)/2 × min(totalF,totalE)`; insert rows with `plan_id`. `DryRun` returns the plan without inserting.
3. `RebalanceOnce(ctx)`: only when `!busy()`; one move; `PauseBetween`; `MaxRate` byte cap; steps copy (reuse `copyReplica` with dst=E) → verify `replicas` row ctoken → `dropReplica(F)`; delete failure leaves surplus to `TrimOnce`. Runs on its own ticker goroutine started by `Start` (never inside the serial repair/replay/drain loop). Backfill on `Start`: `AutoBackfill` and some member has `files == 0` while others do not → `PlanRebalance(TargetSkew)`.
4. Status/control/CLI/UI: `Report.Rebalance{Queued, Done, Failed, BytesMoved, Skew}`; `POST /pool/rebalance {pool, target_skew, dry_run, confirm}`; `cloudfs pool rebalance <pool> [--target-skew 10%] [--dry-run] --confirm`; pool screen per-member fill bars + Rebalance button (i18n both languages).
5. Tests: 3 members, 2 replicas, 30 files on a/b, add c → plan ≈ 1/3 of files, after run `skew ≤ target`; copy-before-delete order; busy → no progress; trim never removes the fresh replica; perf `TestRebalanceCostIsOneUploadPlusOneDeletePerMove`; chaos: member goes down mid-rebalance → rows back off, no replica lost.
6. Commit: `feat(pool): rebalance replicas toward a target skew and backfill new members`.

## Task 20: Docs and TODO closure

Files: `TODO.md`, `docs/pool.md`, `docs/pool-v2.md` (status line), `docs/DESIGN.md` §4.11/§7, `README.md`.

1. Mark T-29..T-33 `[x]` with dates and the test names that prove each acceptance line; anything not achievable in this environment (real accounts, Linux-only FUSE checks, macFUSE) stays `[~]` with the exact remaining gap.
2. `docs/pool-v2.md` header: change status from 设计稿 to 已实现 with the per-phase state; add a short "实测" note if numbers exist.
3. README: remove all remaining 【规划中】 markers for shipped items; keep any that are not.
4. Commit: `docs: close out storage pool v2`.
