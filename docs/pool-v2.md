# 存储池 v2：带宽融合、导出、CRUSH-lite 放置

> 状态：**设计稿，未实现**。对应 `TODO.md` 的 T-29 ~ T-33。v1（容量融合、N 副本、修复、多机）见 [pool.md](pool.md)，架构摘要见 [DESIGN.md](DESIGN.md) §4.11。

## 0. 一页摘要

v1 把多个网盘融合成**一块盘**：系统决定文件放在哪、保 N 份副本、坏了自动重建。它解决的是「容量」与「不丢」。

v2 解决「融合后的**带宽**也要融合」。今天一个 3 副本的文件永远从声明顺序第一个成员读（`internal/pool/read.go` 的 `sortReplicas` 平局按顺序），一个视频的下载速率等于一个网盘的速率；`cp -r` 一万个小文件是一万次串行往返；把一棵目录复制到移动硬盘没有专用通路。Ceph 客户端并行读多个 OSD、rclone 的 `--transfers`/`--multi-thread-streams`、JuiceFS 的跨文件预取，都是同一件事：**让并发落到所有持有者上**。

三条主线、一个原则：

| 主线 | 做什么 | 主要部件 |
|---|---|---|
| 小文件 | 按目录读序预取兄弟文件，整文件直接落 hydrated | `vfs` dir readahead、`cache.PutWhole`、`FOPEN_KEEP_CACHE` |
| 视频 / 大文件 | 合并请求、按读者速率封顶窗口、CDN 字节流独立限流、副本块级扇出、消除二次 hydration | coalescing、`Transfer` 令牌类、`pool.pickReplica`、稀疏整文件布局 |
| 复制到移动硬盘 | 透明 `cp` 变快 + 专用 `cloudfs export` 作业（多文件多流、续传、拔盘暂停、校验） | 内核选项、`internal/export` |
| 放置 v2 | 按路径规则、成员 class、故障域、配额满换盘、rebalance/backfill、`min_replicas` 诚实化 | `pool/placement.go`、`pool/rebalance.go` |

**原则：所有优化都在本地**（缓存、预取、并发、内核选项、作业）。后端仍是 1:1 的真实文件，官方 App 可见，v1 的镜像树 / scrub / op-log / 多机语义一个不改。不做打包 bundle，不做条带与纠删码（§1 说明原因）。

优先级不变：数据不丢 > 不被封号 > 快。每一处扇出与预取都经过既有的 `(remote, account, class)` 限流器；`Tier=unofficial` 的网盘默认单流。

## 1. 参考产品的设计思想与取舍

| 产品 | 机制 | 借鉴 | 不照搬的原因 |
|---|---|---|---|
| Ceph | CRUSH 按权重与故障域放置，规则按 pool；OSD 加入触发 backfill，`osd reweight`；客户端并行读多个 OSD；RBD 条带 | 按路径的放置规则（CRUSH-lite）、故障域 = 账号 / 网盘类型、加盘 backfill、目标偏斜 rebalance、副本并行读 | 无 monitor / MDS：成员是共享真相，本机索引可重建，这是 v1 的既定选择。**不做条带 / 纠删码**：网盘不保证对象级强一致与原子多对象提交；条带后官方 App 不可读；任一成员丢失即整文件丢失，正好与「掉一个盘不影响」相反 |
| JuiceFS | chunk / slice / block；`--prefetch N`；`juicefs warmup --threads`；小文件整文件缓存 | 整文件缓存阈值、跨文件预取、并发 warmup（= export 的并发模型） | 独立元数据引擎：单机 SQLite 已够 |
| Alluxio | 目录级预取、`distributedLoad`、分层存储 | 目录读序检测 → 兄弟文件预取 | 分布式 worker |
| mergerfs / rclone union | create policy（mfs / lfs / epff）、`minfreespace`、`moveonenospc`、从最快分支读 | `moveonenospc`（配额满换盘重放）、按剩余空间放置（v1 已有）、读最快副本 | 只有一份数据、无修复 |
| rclone | `--transfers` / `--checkers`、`--multi-thread-streams`、`copy --checksum` 断点续传、`--vfs-read-ahead` | export 作业的 transfers / streams / 校验 / 续传语义；大文件多流 range | 稀疏文件缓存（DESIGN §2 已述） |
| SeaweedFS | haystack 小文件打包；volume 按 tag 放置 | 成员 `class` 标签 → 规则 `prefer / require / avoid` | **打包 bundle**：官方 App 看不到原始文件、与镜像树和 scrub 冲突、需要 GC 与 compaction。已明确否决 |
| CloudDrive2 / OpenList | 每 remote 并发槽、大文件多线程下载 | 每 remote 的下载槽数由 QPS 与 `MaxConnsPerHost` 推出 | 无背压 |

## 2. 目标架构

分层不变（DESIGN §3），新增的部件都在现有层内：

```
fusefs ──┐                                   ┌─ export.Manager   作业：走 provider 直读，不经 FS.Read
mcpsrv ──┴─ vfs ─┬─ read.go   + dirAhead      目录读序 → 整文件预取
                 │            + coalescing    窗口内连续块合成一次 range
                 │            + rate window   按读者速率封顶，全局在途预算
                 ├─ cache     + PutWhole      预取的小文件直接落 hydrated/
                 │            + 稀疏整文件布局 ≥ 64 MiB 直接按偏移写 hydrated/<fh>.part
                 └─ pool      + pickReplica   按在途数 / 延迟选副本 → 块级扇出
                              + placement v2  rules / class / failure_domain / markFull / rebalance
                 httpx/ratelimit + Transfer 令牌类   CDN 字节流与 API 分桶
                 net/proxy      + MaxConnsPerHost    真正接进 http.Transport
```

## 3. 小文件：目录读序预取

### 3.1 现状与瓶颈

- 读路径没有跨文件预取。`cp -r` 一个有一万个小文件的目录 = 一万次串行 open → read → close，每次等一个 RTT 加一次远端 GET，受该 provider 的 Meta / Download QPS 约束（消费级网盘 1–12/s，`docs/providers.md`）。
- 只读 open 返回 flags `0`（`internal/fusefs/fs.go` `Open`），没有 `FOPEN_KEEP_CACHE`，内核每次 open 都丢掉这个文件的页缓存；第二遍缩略图、第二次复制仍要进守护进程。
- 一个 ≤ 1 块的小文件被前台读到后先写成块文件，10 s 空闲后 hydration 再拷一遍成整文件。

### 3.2 设计

**触发点：句柄的第一次 `Read`**。不是 open（`find` / `git status` 会 open 但不读），不是 readdir（`ls` 不 open）。一次真实的读证明内容被需要。

- 新文件 `internal/vfs/read_dir_ahead.go`：
  - `dirRun{dir, mount, lastName, run, cursor, ahead, ctx, cancel, touched}`；`FS.dirAhead` 以 LRU 保存最多 32 个目录的 run。
  - `Handle.dirNoted` 保证每个句柄只记一次；`FS.Read` 在 `activeReads++` 后调用 `noteFileRead`。
  - 「按列表顺序读」的判定：`meta.ChildrenPage(dir, after=lastName, limit=32)`（列表本来就 `ORDER BY name`），当前文件名落在这一页内 → `run++`、`lastName` 前移、`ahead--`；否则重置 run 并取消在途预取。`run >= 3` 起窗（与块级 readahead 的 `seqArm=3` 同源）。
  - `topUp`：保持 `dir_readahead`（默认 32）个文件在读者前方。跳过目录、`cloudfs-local:` 假 id、`Size > small_file_threshold`（默认 4 MiB）、已 `cache.Complete` 的文件。每个文件一次 `readRange` 整文件（≤ 阈值，所以 1 个请求即使跨两块也只花 1 个令牌）→ `cache.PutWhole`。
- **`cache.PutWhole(key, data, size)`**（`internal/cache/whole.go` 新 API）：写临时文件到 `hydrated/`、rename、`attachWholeLocked`、`rememberKey`。从 `installWhole` 抽出尾部为 `installTemp` 共用。预取的小文件立刻成为 passthrough / splice 可用的整文件，没有块文件 → 10 s 后再拷一遍的 hydration。前台单块 fetch 后续也可切到它。
- **并发槽**：每 remote 一个信号量，`downloadSlots = clamp(round(Caps.QPS.Download), 2, Caps.MaxConnsPerHost)`：aliyun 4、baidu / quark / 115 2、gdrive 10、s3 16、sftp = 会话数。后台预取只用 `slots - 1`，永远给前台 miss 留一条；块级 readahead（§4）共用同一个信号量，所以每个 remote 的后台并发只有一个数。
- **让路**：不能复用目录列举预取器的 `waitIdle`（`cp -r` 期间 `fgIO` 几乎永不为 0，那种让路永远等不到）。改为**槽位预留**：前台不占槽；前台 miss 命中正在预取的文件时通过 `blockFlight.Reserve`（§4.1）加入等待而不是重拉；前台 miss 落在没被预取的文件上只在限流器里竞争，与今天的块级 readahead 相同。可选：当 `f.Busy()` 且该 remote 限流器 AIMD 速率跌到配置值一半以下时停止 `topUp`，直到恢复。
- **取消**：`invalidateListing` / `changedListing` / `dropPaths` 调 `cancelDirAhead(dir)`；30 s 未触碰的 run 由空闲扫描清理；`FS.Close` 取消全部。
- **内存与缓存抖动**：在途 ≤ slots × threshold（≤ 16 × 4 MiB）；每目录前方 ≤ 32 × 4 MiB = 128 MiB。预取进来的块进 2Q 的试用队列，没被读到的最先被淘汰，因此不需要额外的「预取」淘汰类。指标 `cloudfs_dir_readahead_files_total{result="hit"|"wasted"}`。
- **内核**：只读 open 返回 `fuse.FOPEN_KEEP_CACHE`。安全性：`ExplicitDataCacheControl=false` 让内核在 size / mtime 变化时自动失效；版本变化时 vfs 已主动 `invalidate(ino)`。

### 3.3 量化与诚实的边界

每文件成本从「RTT + 本地」（串行 ≈ 3 文件/s @ 300 ms）变成「1 / QPS」。但消费级网盘每个文件仍要「取直链 API + GET」两个令牌：aliyun ≈ 2 文件/s、gdrive ≈ 5、s3 ≈ 8、sftp / smb 不限。**消费级网盘的地板是直链 API 的频率**，预取只能把等待并行化，不能突破它；想再快只能调 `remotes.<name>.qps.download`，风控风险自负（`docs/providers.md`）。

## 4. 视频与大文件

### 4.1 请求合并（coalescing）

`maybeReadAhead` 今天为窗口内每个缺失块各发一个 4 MiB 请求。改为把窗口内**缺失且未认领的连续块**合成一次 range：`readahead_request` 默认 16 MiB（当 `Caps.QPS.Download ≤ 16 && Caps.RangeRead`），否则 4 MiB；一个 goroutine 一个 run：`prefetching.claim` 每块、`blockFlight.Reserve(keys)`、一次 `readRange`、切块 `cache.PutAsync`、`resolve`。`shortRead` 在多块 run 上失败 → 退回单块重试，并对该 remote 记 `maxReq = 1`。

需要 `internal/vfs/flight.go` 新增 `Reserve(keys) (resolve, owned)`：为尚未在飞的 key 各登记一个 `flightCall`，等待者阻塞到 `resolve`。§3 的整文件预取也用它。

每令牌吞吐 ×4：aliyun 16 → 64 MiB/s，baidu / quark / 115 8 → 32 MiB/s，gdrive 40 → 160 MiB/s。

### 4.2 `Transfer` 令牌类

今天 CDN 的字节 GET 与「取直链」API 记同一个 `Download` 桶（`internal/provider/aliyun/aliyun.go` 的 ranged GET、`quark/files.go`、`baidu.go` 的 pcs 流）。直链有缓存，稳态每 4 MiB 块花 1 个 `Download` 令牌，「QPS × 4 MiB」这个上限很大程度上是自己加的。

- `internal/net/ratelimit` 新增 `Transfer` 类；`provider.QPS.Transfer`（0 = 回退 `Download`，不改任何现有行为）；`config.QPS.Transfer` YAML 覆盖；`daemon.qpsForClass` 接入。
- 把 CDN 字节 GET 改记 `Transfer`：aliyun / quark / pan115 / tianyi / baidu 的 OSS 流。gdrive / onedrive / dropbox 的 GET 本身就是 API，不改。
- 保守默认：aliyun 16、quark / 115 4、baidu 4。AIMD（429 / 403 减半）与熔断不变。`qps.transfer: 0` 一键恢复旧行为。
- 风险：网盘对 CDN 请求频率的风控阈值未知 → 默认值是猜测，进 T-13 真实账号验证。

### 4.3 按读者速率封顶的窗口与全局预算

窗口保持倍增作为增长机制，加三条约束：

1. `target = clamp(R × readahead_lead, 2 块, readahead_max)`，R = 本 run 已读字节 / 时长（run ≥ 1 s 后生效），`readahead_lead` 默认 8 s。复制拉满带宽 → 长到上限（与今天相同）；5 MiB/s 的播放器停在 ~40 MiB；1 MiB/s 的停在 8 MiB。跳播浪费从 64 MiB 降到几块。
2. 只在 stall（顺序读碰到既未缓存也未认领的块）时增长——Linux readahead 的语义。
3. 全局在途预算：`Σ 在途块 × block_size ≤ cache.WriteBehindBudget()`（`writebehind.go` 新访问器 = `write_behind − subBlockReserve`）。多个句柄、多个播放器（播放器常把同一文件 open 两三次）不能一起超过 write-behind 内存——今天超预算时 `PutAsync` 会静默退化成同步 `Put`，把磁盘写放进读路径。

块级 readahead 的 goroutine 从 §3.2 的每 remote 槽位取，前台读仍豁免。

### 4.4 消除 hydration 的二次写

今天大文件冷读：块文件写一遍，10 s 空闲后 `Hydrate` 再读一遍写成整文件——本地磁盘写两遍。

- 便宜方案（Linux）：`Hydrate` 逐块先试 `unix.IoctlFileCloneRange`（btrfs / xfs / bcachefs 零拷贝；4 MiB 对齐没有文件系统块问题），失败回退读写。macOS APFS 只有整文件 clone，跳过。
- 真正的修复：`cache.Options.WholeLayoutMin`（默认 64 MiB）。达到它的文件由 write-behind worker **按偏移直接写** `hydrated/<fh>.part`（`O_EXCL` 创建、`Truncate(size)` 稀疏）；存在性仍在 `files[fh].present`，每次 flush 后持久化到 `<fh>.part.bitmap`（含每块 crc32）；`readBlock` 加 `.part` 分支（它本就按偏移寻址 hydrated 内容）；写满即 rename `.part → <fh>` 并 `attachWholeLocked`——无拷贝、无 janitor。淘汰单位 = 整文件（`wholeObject`，按 `st_blocks × 512` 计费）；`reload` 时位图有效则重挂，否则删除。passthrough / splice 只在完成后启用。
- 小文件与中等文件保持块文件 + reflink 或拷贝的 hydration（拷贝上限 64 MiB）。

### 4.5 池副本扇出

- `member` 增加 `inflight atomic.Int32`、`served atomic.Int64`（字节）、`maxInflight = Capabilities().MaxConnsPerHost`（默认 4）、按 MiB 归一化的延迟 EWMA（`noteLatency(d, n)`，4 MiB 与 32 MiB 请求可比）。
- `pickReplica(reps)`：可用 ∧ `inflight < maxInflight`（全饱和则忽略此条）；分数 = `inflight/maxInflight + ewmaPerMiB/minEwma`；平局取 `served` 最少。未测量过的成员先给探测机会（保留 v1「每个成员都有机会」的性质），但**不再按声明顺序粘住**——今天 `sortReplicas` 在延迟相同时把一个文件的所有读都送到成员 0。
- `ReadRange` / `ReadRangeAt` / `DownloadURL`：pick → `inflight++` → 调用 → `inflight--`；不可达则从集合剔除重选；`note` / `missing` / `conflict` 处理不变。`tryReplicas` 保留给修复的 `replicaReader`（单流）。
- 并行块请求来自 vfs readahead（每个预取块是独立的 `ReadRangeAt`），加上 picker，一个 3 副本文件自然达到 ~3 × 单成员 QPS。vfs 侧一处调整：mount provider 的 `Caps.MaxConnsPerHost > 1` 时，顺序窗口起点取 `min(ReadAheadBlocks, MaxConnsPerHost)` 而不是从 1 倍增。
- 上限 = Σ 成员 QPS × 请求大小。例：aliyun + baidu，6 × 16 MiB ≈ 96 MiB/s。
- `resolveFile` 每块 2–3 次 SQLite 查询 → 加 5 s TTL 的 `(id, version) → []replicaRow` 缓存，在 `finishUpload` / `relocate` / `Delete` / `upsertReplica` / 标 `missing` 时失效；也消除每块对 `p.mu` 的争用。
- `tryReplicas` 今天对任一 `ErrNotFound` 立刻把副本标 `missing`。多成员并答时，直链刷新期间的瞬时 404 会误伤一份副本——先 `Stat` 确认再降级。
- **风控**：每个成员保留自己的 `(remote, account, class)` 限流器，同账号的两个成员自动共享一个桶，扇出永远不超过任一账号自己的预算。`Tier=unofficial` 的网盘（quark / 115 等）对同一文件的多并发 range 可能触发风控：`pools.<name>.read_fanout: off | auto | all`，默认 `auto` = 对 unofficial 层成员，一个文件最多一条并发流；official 层不限。直链成本：每个成员各自解析一次直链（每成员一次 meta 调用）。
- `Caps.RangeRead` 是「全有或全无」（`pool.go` 汇总能力时任一成员不支持即 false）：此时扇出与 export 的多流都退化为单流。

### 4.6 内核 / FUSE

| 项 | 改动 | 说明 |
|---|---|---|
| `MaxReadAhead` | Linux 1 MiB → 4 MiB（保留 `CLOUDFS_KERNEL_READAHEAD`） | go-fuse 注释「上限 128 KiB」已过时，内核按 `max_readahead` 设 `ra_pages` |
| `MaxBackground` | 64（go-fuse 默认 12） | 让内核保持更多异步 READ 在飞——这才是把 `cp` 的 128 KiB 读变成流水线 1 MiB 请求的开关 |
| macOS | `iosize=1048576`（macFUSE 4.x 验证；Fuse-T 忽略），`MaxWrite` 仍 64 KiB | |
| splice | 已 hydrate 的文件 `Read` 返回 `fuse.ReadResultFd(fd, off, n)` | 非 root 也零用户态拷贝，补 passthrough 的空缺；macOS 上 go-fuse 会读进缓冲，无害 |
| `FileLseeker` | `SEEK_DATA → off`、`SEEK_HOLE → size` | GNU `cp --sparse=auto` 不再每个文件 `ENOTSUP` |
| `copy_file_range` | 实现 `fs.NodeCopyFileRanger`，仅挂载内复制：源已 hydrate → `unix.CopyFileRange` 从缓存 fd 拷进目标写句柄的 staging | **对复制到移动硬盘无帮助**：内核 `fuse_copy_file_range` 对跨 superblock 直接返回 `EXDEV`，`cp` 在调到我们之前就已回退成 read / write |
| `writeback_cache` | 不做 | go-fuse v2.11 没有协商开关；内核开启它时会关闭 passthrough；它只影响写。建议 T-10 结为「对读负载不适用」 |

## 5. 复制到移动硬盘：`cloudfs export`

### 5.1 四个跨切决定

1. **副本选择在 pool 内**（§4.5）。export 只决定「保持多少 range 在途」，所以普通的 gdrive 挂载同样能用，而「N 个文件跨 3 个成员 → 每成员 ≈ N/3 次下载」由 pool 免费给出。
2. **独立的 `<cache.dir>/exports.db`，不复用 `copy_jobs`**。`copy_jobs` 是上传准备：目标唯一索引 `(target_remote, target_parent, target_name)`、`Submit` 交接给 `uploads`、一个 inode 一份暂存快照、`AdoptByIno` CAS、journal v11 目标绑定——全是上传侧语义；为一个既不需要 journal 所有权也不需要 blob 的功能把 journal 升到 v14，会让旧守护进程打不开。只复用**模式**：`StartCopies` / `resumeCopyJobs` 的 wake-channel 循环、`trackCopy` 逐作业取消、`CopySpec` 的来源身份（meta identity、mount prefix / rootID、account binding）——换绑账号之后不会把作业续进另一份内容。
3. **绝不把缓存对象硬链接到用户的目标目录**。`cache.LinkFile` / `AdoptFile` 是往缓存里装；`whole.go` 的 `wholeObject` 引用计数假设它独占所有名字。用户在移动硬盘上原地编辑导出文件会污染不可变的缓存对象。export 从 `OpenWhole` 拷字节，或用 `clonefile` / `FICLONE`（写时复制，安全）。「零重下载」仍成立。
4. **不经 `FS.Read`**。它会 `fgIO++`（让路逻辑把 export 当成前台）并填充块缓存（抖动）。export 像 `ResumeCopy` 那样直接调 `mount.Provider.(RangeReaderAt)`。

### 5.2 存储

新包 `internal/export`（store / planner / runner / manager）。`exports.db`：WAL、`busy_timeout`、`SetMaxOpenConns(4)`、`meta(k, v)` 记 `schema_version`，只由守护进程打开（`flock`，同 `journal/lock.go`）。

```sql
CREATE TABLE export_jobs (
  id TEXT PRIMARY KEY,
  state TEXT NOT NULL CHECK(state IN ('planning','running','paused','done','failed','cancelled','purging')),
  pause_reason TEXT NOT NULL DEFAULT '',        -- user | disk | unavailable | risk_control | auth
  sources TEXT NOT NULL,                        -- json [vpath...]
  dest TEXT NOT NULL,                           -- 绝对本地目录
  dest_dev INTEGER NOT NULL DEFAULT 0,          -- 规划时的 st_dev：识别拔盘
  options TEXT NOT NULL,                        -- json {mirror,verify,preserve_mtime,transfers,streams,range_size}
  meta_identity TEXT NOT NULL,
  bindings TEXT NOT NULL,                       -- json [{prefix,remote,root_id,account_binding}]
  files_total INTEGER NOT NULL DEFAULT 0, files_done INTEGER NOT NULL DEFAULT 0,
  files_skipped INTEGER NOT NULL DEFAULT 0, files_failed INTEGER NOT NULL DEFAULT 0,
  bytes_total INTEGER NOT NULL DEFAULT 0, bytes_done INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 0,          -- pause/resume/cancel 竞争的 CAS（copy_jobs 模式）
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE export_items (
  job_id TEXT NOT NULL, rel TEXT NOT NULL,      -- dest 下的相对路径
  vpath TEXT NOT NULL, kind INTEGER NOT NULL,
  size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL,
  remote TEXT NOT NULL, remote_id TEXT NOT NULL, version TEXT NOT NULL,
  hash_type TEXT NOT NULL DEFAULT '', hash TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK(state IN ('pending','active','done','skipped','failed')),
  ranges TEXT NOT NULL DEFAULT '',              -- 已完成 range_size 块的位图
  done_bytes INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (job_id, rel)
);
CREATE INDEX export_items_state ON export_items(job_id, state, next_at);
CREATE TABLE export_extras (job_id TEXT NOT NULL, rel TEXT NOT NULL, kind INTEGER NOT NULL, PRIMARY KEY (job_id, rel)); -- --mirror 的删除计划
```

### 5.3 规划器（暖态 0 次远端调用）

1. 对每个来源：`fs.StatPath`；目录则 `fs.ReadDirPath` 递归（`DirState.Fresh` 时由 `meta.Children` 直接服务；池的列表本就来自索引，`TestPoolWarmTraversalIsFree`）。每 500 条一个事务写入 `export_items`；记录 `meta.Identity()` 与每个来源 mount 的 `{Prefix, RootID, AccountBinding}`。
2. 目标预检：`MkdirAll(dest)`、记 `st_dev`、写或刷新标记 `<dest>/.cloudfs-export.json {sources, job_id, meta_identity}`。**`--mirror` 没有同来源的既有标记就拒绝**（rclone 式的防误删：不能把任意目录当镜像目标清空）。
3. 跳过判定：`<dest>/<rel>` 已存在且 size 相同、mtime 相差 ≤ 2 s（对齐 `pool/ctoken.go` 与 exFAT 的 2 s 精度）→ `skipped`；`--verify` 且 provider 提供哈希时再对本地文件算一次哈希（纯本地 IO）。
4. 顺序：`size ≥ multi_range_min` 的先排（它们吃多流），其余按路径序；目录按需创建，目录 mtime 最后后序补。

### 5.4 执行器

- 一个 Manager goroutine（`StartCopies` 的形状），`wake chan struct{}`；守护进程启动时恢复非终态作业；同时运行的作业数 `jobs_parallel` 默认 1——两个作业抢同一批成员只会更慢。
- 调度：取到期的 `pending` 条目（`next_at ≤ now`）→ 全局 `transfers` 信号量（默认 `min(config, Σ 涉及 mount 的 MaxConnsPerHost, 8)`）→ `exportFile`。
- `exportFile`：
  1. `key := cache.FileKey{remote, remote_id, version}`。`cache.OpenWhole(key)` 命中 → 本地拷贝（先试 reflink，再 `io.Copy` 1 MiB 缓冲）；`vfs.IsLocalOnly(remote_id)` → 只读 `journal.Get(id).BlobPath`；部分块在缓存 → `cache.ReadAt` 供已有块，只拉缺失 range。**已缓存文件的导出 = 成员 0 次 `ReadRange`。**
  2. 否则打开 `<dest>/<rel>.cloudfs-part`（`O_CREATE|O_WRONLY`，不截断；`Truncate(size)` 一次），按 `range_size`（默认 32 MiB：绕过缓存，吞吐 ≈ QPS × 请求大小）切块，跳过 `ranges` 已完成的，每文件 `streams` 并发（默认 4；`size < multi_range_min` 用 1）：`ReadRangeAt(ctx, id, version, off, buf)` 写进池化的 `range_size` 缓冲 → `pwrite`；不支持时回退 `ReadRange + io.ReadFull`；`shortRead` 语义同 `vfs/read.go`。
  3. 检查点：每块后 `UPDATE ranges / done_bytes`（每秒几次）；每 256 MiB 与 rename 前 `fsync`。
  4. 校验：边写边算与 `hash_type` 匹配的哈希（sha1 / md5 / sha256）；不符 → 删 part、`attempts++`、重试 ≤ 2 → `failed`。无哈希则校 size 并设 mtime。`--verify` 在 rename 后从盘重读一次（抓移动硬盘的写错误）。
  5. `os.Rename(part, final)` → `os.Chtimes` → `state='done'`、`bytes_done += size`。
- 让路：每块前若 `fs.Busy()` 最多等 5 s（`Manager.SetBusy(fsys.Busy)`，同 `cache.SetBusy` 的接法）。成员间公平由 pool 内部保证（§4.5）；普通 remote 由自己的限流器与 `MaxConnsPerHost` 约束。
- 进度：10 s EWMA → `Rate`、`ETA = (bytes_total − bytes_done) / Rate`；运行中每秒发 `/events` SSE `export.progress`。

### 5.5 错误 → 状态

| 条件 | 条目 | 作业 |
|---|---|---|
| 目标 `ENOSPC` / `EDQUOT`、`EIO` / `ENXIO` / `ENODEV` / `EROFS`、目标根 `stat` 失败或 `st_dev ≠ dest_dev`（拔盘） | `pending`（`ranges` 保留） | `paused(disk)`；每 `disk_probe_interval`（30 s）探测目标可写且 `st_dev` 匹配则自动恢复 |
| `provider.ErrUnavailable` / 不可达（`retry.ClassRetryable`、`ClassAuth` 经 `unreachable()`） | `pending`，`next_at` 退避（30 s 起、封顶 1 h），**不消耗尝试**（同 `uploader.go` 的延期语义） | 保持 `running`；所有待办都被阻塞 → `paused(unavailable)`，到期自动恢复 |
| `retry.ClassRiskControl` | `pending` | `paused(risk_control)` 直到熔断器 `OpenUntil` |
| `retry.ClassAuth`（刷新后仍失败） | `pending` | `paused(auth)`；UI 链到重新授权 |
| `ErrNotFound`、`ErrConflict`（来源在导出期间被改）、恢复时绑定或 meta identity 不符 | `failed`（带原因） | 继续；结束为 `done` 且 `files_failed > 0`（CLI 退出码 1） |
| 哈希不符 | 重试 ≤ 2 后 `failed` | 继续 |
| ctx 取消 / 守护进程停止 | `active` 在下次启动时回 `pending` | 不变 |
| 用户 `cancel` | 不动 | `cancelled`；part 文件保留到 `forget` |
| `--mirror` 删除步骤失败 | — | `done` 带警告 |

### 5.6 配置、控制面、CLI、MCP、UI

```yaml
export:
  transfers: 4            # 同时在飞的文件数
  streams: 4              # 每个大文件的并发 range 数
  range_size: 32MiB
  multi_range_min: 64MiB
  yield_to_foreground: true
  disk_probe_interval: 30s
  jobs_parallel: 1
mcp:
  export_roots: [~/Exports]   # MCP 的 export 工具只能写到这些目录之下
```

- 控制面 `internal/control/exports.go`，登记进 `routes()`（`security_all_routes_test.go` 才会守住它们）：`POST /export {sources[], dest, mirror, verify, confirm, transfers, streams}` → `{id}`；`GET /exports?limit&cursor`；`GET /exports/<id>`（作业 + 进度 + 每成员负载）；`POST /exports/pause | resume | cancel | forget {id, confirm}`。客户端 `CallExport` / `CallExports` 仿 `control/copy_admin.go`。
- CLI（`cmd/cloudfs/export.go`、`exports.go`，在 `main.go` 的 `"copies"` 旁分派）：
  `cloudfs export <vpath>... <dest-dir> [--mirror --confirm] [--verify] [--transfers N] [--streams N] [--range-size 32MiB] [--wait] [--json]`；
  `cloudfs exports list | show | pause | resume | cancel | forget <id> [--confirm]`。需要守护进程在跑（长作业，无离线模式）。`--wait` 轮询 `/exports/<id>` 渲染一行进度。
- MCP（`internal/mcpsrv/export_jobs.go`）：`export {paths[], dest, mirror?, verify?}`（dest 必须在 `mcp.export_roots` 下；只读服务器拒绝）、`list_export_jobs`、`get_export_job`、`cancel_export_job`。游标用 `copy_jobs.go` 同款 AEAD。
- UI（`internal/control/web/screens/exports.js`，路由 `#/exports`，导航键 `nav.exports`，`i18n.js` 双语）：仿 `copies.js`——进度条 / 速率 / ETA / 状态色、暂停 / 恢复 / 取消 / forget（forget 与 mirror 要输入确认）；「新建导出」表单：来源用现有 `/fs/list` 浏览器选，目标目录在 `cloudfs-desktop` 存在时用桌面壳的目录选择器；详情面板显示每成员在途数与吞吐（§4.5 的计数器）。

### 5.7 测试

`internal/export/export_test.go`（复用 `test/perf/pool_test.go` 的 `newPoolHarness`，抽成可导出的 helper）：

1. 30 个文件、3 成员、`Replicas: 3`、修复完成 → 导出 → 每成员 `Calls("ReadRange")` 在 `N/3 ± 2` 内；`ReadBytes()` 总和 == Σ size（无重复下载）。
2. 已 pin / hydrate 的文件 → 导出 → 每成员 `Calls("ReadRange") == 0`；`cloudfs-local:`（已写未传）同样 0。
3. 崩溃：k 块后注入故障、重开 store、恢复 → 成员 `ReadBytes()` 增量 == 剩余块。
4. `Runner.writeFault` 注入 `ENOSPC` → 作业 `paused(disk)`、条目 `pending`，`resume` 后完成；`st_dev` 不符同样。
5. 无标记的 `--mirror` 被拒绝；有标记只删多余项。
6. 哈希不符（fake `SetReportHashes` + 篡改 `ReadRange`）→ 重试 2 次后 `failed`。
7. 暖态规划：遍历一次后，规划阶段成员 `List == 0`。

`test/perf/export_test.go`：`Faults.Latency = 20ms`，3 成员导出的墙钟 ≤ 同样内容单成员池的 0.5×（这一条是唯一看时间的，因为它断言的是并行度）。

## 6. 放置 v2（CRUSH-lite）

```yaml
pools:
  home:
    replicas: 3
    min_replicas: 2
    write_mode: relaxed          # relaxed | strict（§6.5）
    min_replicas_timeout: 2m
    failure_domain: account      # account | provider | member
    read_fanout: auto            # off | auto | all（§4.5）
    members:
      - {remote: ali, class: [fast, cheap]}
      - {remote: gd,  root: /cloudfs, class: [fast]}
      - {remote: nas, root: /pool, capacity: 2TiB, class: [local-nas]}
    rules:
      - {prefix: /photos, replicas: 2, prefer: [local-nas], avoid: [cheap]}
      - {prefix: /video,  replicas: 3, require: [fast]}
    rebalance: {target_skew: 0.10, auto_backfill: true, max_rate: 30MiB/s, pause_between: 500ms}
```

### 6.1 规则与类

- `PoolMember.Class []string`；`PoolRule{Prefix, Replicas, Prefer, Avoid, Require}`；`Pool.Rules`、`FailureDomain`、`WriteMode`、`MinReplicasTimeout`、`Rebalance`。校验：前缀规范化且唯一、`replicas ≤ len(members)`、class 名已声明。编辑器（`config/edit_pools.go`）：`SetPoolField` 白名单加 `write_mode` / `failure_domain` / `min_replicas_timeout`，新增 `AddPoolRule` / `RemovePoolRule` / `SetPoolMemberField(pool, remote, "class", ...)`。
- `ruleFor(path)` 最长前缀；默认规则 = 池设置。`targetFor(path)` 取代 `replicaTarget()` 的全部调用点：`repair.go`（扫描、修复、封顶）、`scrub.go`（`TrimOnce`）、`status.go`（欠副本、`Availability`）、`drain.go`、`write.go`（`Replicas > 1` 决定是否取 hold 与入队）、`Quota()`（用默认规则）。
- 故障域由 daemon 传入 `pool.Member{Classes, Domain}`：`account` = `EffectiveAccountBinding(remote)`、`provider` = remote 类型、`member` = 成员名。
- 标记文件 `marker.go` 的 `Settings` 加 `Rules` / `FailureDomain`：摘要变化会让其他机器收到一次性的 epoch 提示，升级说明里写明。

### 6.2 `candidates()` 排序

已持有该路径 → 通过 `require` → `prefer` 命中 → **故障域不与现有持有者重复**（软条件：降级不排除，「有替代才分散」）→ 不在 `avoid` → 不在 `fullUntil` 内 → `free × weight` → weight → 声明顺序。已知空间永远排在未知之前（v1 规则保留）。返回有序切片，`BeginUpload` 与修复仍取「第一个接受的」。

**`repairTarget` 合并进 `candidates`**：`repair.go` 里那份按声明顺序的第二套放置策略删掉（`repairTarget = candidates(path) − 已持有`），否则修复不认规则与故障域。

### 6.3 配额满换盘（moveonenospc）

今天 provider 没有配额哨兵：网盘返回 507 / 403 / 400 经 `retry.Classify` 到 `ClassTerminal` 或 `s ≥ 500 → ClassRetryable`，退避耗尽进死信，本地 blob 保留，**不会换盘**。

- 新哨兵 `provider.ErrQuotaExceeded`；`retry.Classify` 新 `ClassQuota`。驱动映射：gdrive 403 `storageQuotaExceeded`、onedrive / webdav 507、aliyun `QuotaExhausted`、sftp / smb `ENOSPC`（都带 `UNVERIFIED:` 直到真实账号核对）。
- 池：
  - `BeginUpload` 循环遇到 → `m.markFull(quota_backoff 10m)`（`space.quota.Free = 0`、`fetched = now`、`fullUntil`），换下一个候选。
  - `UploadPart` / `CompleteUpload` 遇到 → `markFull`，返回 `fmt.Errorf("%w: %w", provider.ErrQuotaExceeded, provider.ErrRestartUpload)`（新哨兵）。`uploader.go` 的分类 switch 加 `case retry.ClassQuota`：带 `ErrRestartUpload` 则 `Journal.SetSession(nil)`（丢分片，同 `errSessionExpired` 路径）并立即 `Retry`（延迟 0）——池会重新放置到另一成员；不带则死信（普通 remote 盘满，本来就没处可去）。
  - `copyReplica` 遇到 → `markFull`、不记分歧、换候选（既有的 `failed` 标记循环已能继续）。
- `full` 是成员徽章，不是健康状态；`Availability` / 状态页显示它。
- 新表 `member_usage(member PRIMARY KEY, bytes, files)`，在 `upsertReplica` 与删副本时维护，取代每次放置对 `replicas` 表的 `SUM(size)`；rebalance 的偏斜计算也要它。

### 6.4 修复走服务端复制

`copyReplica` 在 `openSource` 之前：目标 `Caps.ServerCopy` 且实现 `provider.ServerCopier`，且**同 `Domain`**（同账号）的成员上有活副本 → `Copy(ctx, src.remoteID, dstDirID, name)`；`ErrUnsupported` / `ErrNotFound` 回退字节拷；**模糊失败（超时、5xx）不盲目重试**：`enqueueRepair` 退避，并在下次尝试前 `ScrubPath(path)`——已经落地的复制会经正常列举被 `publishGroup` 采纳（镜像路径上的副本就是副本），不需要「服务端复制意图表」。

### 6.5 `min_replicas` 诚实化

把 `CompleteUpload` 阻塞到 N 份不可重试安全：uploader 在分片前就持久化了池的 session，重跑 `CompleteUpload` 会在成员上重复 complete。

- `write_mode: relaxed`（默认）：`min_replicas` 是**告警阈值**——`live < min_replicas` 的文件修复优先级 2、`Availability.State = degraded` 且 `reason = below min_replicas`、`Report.BelowMin` 计数、`doctor` 一行。`pool.md` 相应改写。
- `write_mode: strict`：`finishUpload` 提交索引后同步从 hold 跑 `repairPath`，deadline `min_replicas_timeout`；超时**仍返回成功**，文件留在修复队列（数据在一个成员与本地 hold 上都是持久的）。措辞必须诚实：「close() 最多等 2 min 补齐 min_replicas，永远不会因为它让写失败」。
- 两者都不接受时，从 `config.CreatePool` 与 CLI `--min-replicas` 删掉这个键。

### 6.6 rebalance 与 backfill

`pool/rebalance.go`；新表：

```sql
CREATE TABLE rebalance_queue (
  path TEXT PRIMARY KEY, from_member TEXT NOT NULL, to_member TEXT NOT NULL, size INTEGER NOT NULL,
  state TEXT NOT NULL DEFAULT 'pending',   -- pending | copied | done | failed
  attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
  plan_id TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE INDEX rebalance_member ON rebalance_queue(from_member, state);
```

（`pool/db.go` 加 `meta.schema_version`，从 2 起；`write.go` 的 `movePaths` 列表加上它。）

- `PlanRebalance(ctx, targetSkew)`：每个在服务中成员的填充率来自 `memberQuota` 或 `capacity − member_usage`；`skew = maxFill − minFill`。当 `skew > target`：取最满 F 与最空 E，候选 = F 有、E 无、规则允许（`require` / `avoid` / `canHold` / 不破坏故障域）的文件，大文件优先，搬到 `(fillF − fillE) / 2 × min(totalF, totalE)` 字节；写入 `rebalance_queue`（`plan_id`）。`--dry-run` 只返回计划。
- `RebalanceOnce(ctx)`：仅在 `!p.busy()`（新 `Pool.SetBusy(func() bool)`，daemon 接 `fsys.Busy`）时推进；一次一个；`pause_between`；`max_rate` 字节速率上限。顺序：(1) `copyReplica(dst = E)`（复用 hold / 活副本，同域走服务端复制）；(2) 校验 `replicas` 行的 `ctoken` 与 entry 一致；(3) `dropReplica(F)`——与 `DrainOnce` / `TrimOnce` 共享的新 helper（`Delete` + `DELETE FROM replicas`）；删失败则留给 `TrimOnce`。
- 在 `health.go` 里**独立的 ticker goroutine** 跑它（今天 repair / replay / drain 严格串行且忽略 `repair_concurrency`），一次慢搬迁不能拖住 `ReplayOnce` / `DrainOnce`。
- backfill：`Start` 时若 `auto_backfill` 且某成员 `member_usage.files == 0` 而其他成员不为 0 → `PlanRebalance(target_skew)`。加盘后逐步填满；新写本来就偏向最空的成员。
- **`TrimOnce` 的顺序必须改**：今天从声明顺序最后的成员裁多余副本，rebalance 之后那可能正是新副本。改为裁放置分数最差的那份（最满、被 `avoid`、故障域重复）。
- 多机：另一台机器只在 `live > target` 且过了 `trim_grace` 才裁，我们的新副本 `seen_at > cutoff` 不会是被裁的那份；旧的可能是——这正是预期结果。
- 状态：`Report.Rebalance{Queued, Done, Failed, BytesMoved, Skew}`；UI 池页每成员填充条 + 「Rebalance」按钮（带目标偏斜）；控制面 `POST /pool/rebalance {pool, target_skew, dry_run, confirm}`（登记进 `routes()`）；CLI `cloudfs pool rebalance <pool> [--target-skew 10%] [--dry-run] --confirm`。

### 6.7 测试

- `placement_test.go`：最长前缀规则；`prefer` 排序；`require` 排除；故障域（a、b 同账号，c 另一账号 → a 之后 c 先于 b）；`avoid` 降级不排除；配额错误 → 下一候选，`fullUntil` 生效，10 min 内 `free()` 不重查。
- `write_test.go`：`UploadPart` 返回 `ErrQuotaExceeded` → session 清空并重新放置到另一成员（`fakeprovider.Faults` 加 `QuotaAfterBytes`）。
- `repair_test.go`：同域且 `ServerCopy` 时走 `Copy`（fake 加 `Copy`）；模糊失败 → 先 scrub 再采纳，不重复拷；配额 → 换成员、不记分歧。
- `rebalance_test.go`：3 成员、2 副本、30 文件在 a / b，加 c → 计划 ≈ 1/3，完成后 `skew ≤ target`；先拷后删的顺序；busy 钩子为真时不推进；裁剪不碰新副本。
- `test/perf/pool_test.go`：`TestRebalanceCostIsOneUploadPlusOneDeletePerMove`、`TestPlacementSpreadsAcrossDomains`。

## 7. `cache.policy`：按挂载前缀的策略

```yaml
cache:
  policy:
    preset: none
    small_file_threshold: 4MiB
    dir_readahead: 32
    readahead_max: 64MiB
    readahead_request: 16MiB
    readahead_lead: 8s
mounts:
  - path: /mnt/cloud
    layout:
      /:       {remote: home}
      /video:  {remote: home, cache: {preset: media}}    # readahead_max 128MiB, request 16MiB, dir_readahead 0, dir_ttl 24h（未设时）
      /photos: {remote: home, cache: {preset: photos}}   # dir_readahead 64, threshold 8MiB, readahead_max 16MiB
      /code:   {remote: home, cache: {preset: code}}     # dir_readahead 128, threshold 1MiB, dir_ttl 1m
```

- `config.CachePolicy{Preset, SmallFileWhole *bool, SmallFileThreshold, DirReadahead, ReadaheadMax, ReadaheadRequest, ReadaheadLead}`；`Cache.Policy` 是全局默认，`Layout.Cache *CachePolicy` 是覆盖（`pin` / `dir_ttl` 留在原处）。显式键覆盖 preset。校验：threshold 为 64 KiB 倍数、`readahead_request` 为 `block_size` 倍数、preset 名已知。
- `vfs.Mount.Policy`（解析后的值，无指针）由 `daemon.buildMounts` 按 preset → 全局 → layout 合并；消费方：`maybeReadAhead`（max / request / lead）、`noteFileRead` / `topUp`（dir_readahead / threshold）、`fetchBlock`（`small_file_whole` → `PutWhole`）。
- 环境变量（仅全局）：`CLOUDFS_READAHEAD_BLOCKS`（保留）、`CLOUDFS_DIR_READAHEAD`、`CLOUDFS_SMALL_FILE_THRESHOLD`、`CLOUDFS_READAHEAD_REQUEST`。
- 测试：`config_test.go` 的 preset 解析与校验错误；`internal/vfs` 里两个不同策略的 mount 得到不同窗口（fake 加延迟，3 次顺序读后计 `ReadRange`：media 窗口 > code 窗口）。

## 8. 把 `Caps.MaxConnsPerHost` 接进 transport

今天 `http.Transport.MaxConnsPerHost` 是 0（不限），只有 `MaxIdleConnsPerHost: 8`（`internal/net/proxy/dialer.go` `transportFor`）；`Caps.MaxConnsPerHost` 唯一的消费者是 `pool.go` 里的求和。

- `ruleTransport` 按 `(outbound, conns)` 缓存 transport；`transportFor(o, conns)` 设 `MaxConnsPerHost = MaxIdleConnsPerHost = conns`；上限变化时建新 transport 并关闭旧的空闲连接。`Manager.ClientWithLimit(override, timeout) (*http.Client, setConns func(int))`，`Client` 变成它的包装。
- daemon：`httpClient, setConns := pm.ClientWithLimit(rc.Proxy, 0)`；`provider.New` 返回后 `setConns(rc.MaxConns || p.Capabilities().MaxConnsPerHost || 8)`。新 YAML `remotes.<name>.max_conns`。在 `New` 内部发请求的驱动，头几次调用用默认 transport。
- 同一个数也喂 §3.2 的 `downloadSlots`：覆盖值要写回 `Instrument` 返回的 `Caps` 副本（或加 `provider.WithMaxConns(p, n)` 包装）。
- HTTP/2 下 `ForceAttemptHTTP2` 让一条连接承载多个流，`MaxConnsPerHost` 约束的是连接数不是请求数——请求数仍由限流器与槽位约束。
- 测试：`dialer_test.go` `setConns(3)` 后 transport 的 `MaxConnsPerHost == 3`；`daemon_test.go` `max_conns: 2` 的 remote 经 caps 报告 2。

## 9. 分阶段路线与门禁

门禁沿用 `test/perf` 的约定：**断言 provider 调用次数与状态，不看墙钟**（唯一例外是 §5.7 的并行度测试）。

| 阶段 | 内容 | 门禁 |
|---|---|---|
| A 便宜、立刻可测 | `FOPEN_KEEP_CACHE`；`MaxReadAhead 4 MiB` + `MaxBackground 64`；§8 接线；`flight.Reserve` + coalescing；`Transfer` 令牌类（aliyun 先） | `TestSequentialReadUsesWholeBlocks` 改为接受 `wantBlocks / coalesce` 次；fusefs 同一文件读两遍第二遍 `opRead` 增量 0；transport `MaxConnsPerHost == 3` |
| B 小文件 | §3 dir readahead + §7 policy | `TestDirectoryReadaheadMakesSiblingReadsFree`（按名序读 3 个后等待，再读 `file003..010` 时 `ReadRange` 增量 == 0）；`TestRandomOrderReadsDoNotTriggerDirectoryReadahead`（== 10）；`TestDirectoryReadaheadIsBoundedByRemoteSlots`（`Fake.MaxInflight() ≤ 2`）；`TestDirectoryReadaheadSkipsLargeFiles`；`TestDirectoryReadaheadStopsOnListingChange` |
| C 视频 | §4.3 速率窗口 + 全局预算；§4.5 池扇出 | `TestReadFanoutSpreadsBlocks`（12 块、窗口 8 → 每成员 4 ± 1）；`TestReadFanoutSkipsDownMember`；`TestReadFanoutPrefersFast`；`TestPoolReadFanoutAddsBandwidth`（20 ms 延迟、48 MiB、3 成员 < 0.5× 单成员）；更新所有假设读全落成员 0 的旧基线（`TestPoolWriteThenReadIsLocal` 等） |
| D 导出 | §5 全部 | §5.7 七条 + 路由守卫、`copies_test.go` 式 JSON 契约、`i18n_coverage_test` |
| E 放置 | §6 全部 | §6.7 |
| F 内核补完 | §4.4 稀疏整文件布局；splice；`FileLseeker`；挂载内 `copy_file_range` | 大文件冷读的本地写字节 == 文件大小（不再 2×）；fusefs 测试在 Linux 主机上跑 |

推荐顺序 A → B → D → C → E → F：导出先于视频，用户可见价值最高且不碰读路径核心；放置 v2 最后，因为它改索引 schema 与多机标记。

## 10. 风险与冲突清单

**风控。** 扇出与预取全部走既有 `(remote, account, class)` 限流器，同账号成员共享桶；`unofficial` 层默认单流；`Transfer` 类默认值是猜测，`qps.transfer: 0` 一键回退。真实账号验证并入 T-13。

**索引一致性。** `replicaRow` 5 s 缓存的失效点必须齐全；`markFull` 必须立刻使 `space` 失效，否则 60 s 配额 TTL 内反复撞墙。

**移动硬盘。** exFAT 无 reflink、mtime 精度 2 s（跳过判定 ±2 s 已覆盖）、拔盘用 `st_dev` 识别而不是「路径是否存在」（挂载点目录在拔盘后往往还在）。

**与现有代码的冲突**（实现时逐条处理）：

1. `repair.go` 的 `repairTarget` 是与 `candidates()` 不一致的第二套策略——合并。
2. `replicaTarget()` 全局一个数，8 处调用要换成 `targetFor(path)`；`write.go` 用 `Replicas > 1` 决定取 hold。
3. `sortReplicas` / `rank()` 平局按声明顺序，读粘在成员 0；扇出后计成员 0 读次数的 perf 基线会漂移。
4. `quotaTTL` 60 s 与每次放置的 `SUM(size)`：配额错误要立刻失效 `space`；偏斜计算需要 `member_usage`。
5. `tryReplicas` 对任一 `ErrNotFound` 立刻标 `missing`——先 `Stat` 确认。
6. `copy_jobs` 的目标唯一索引、`Submit` 交接、一 inode 一暂存快照 / `AdoptByIno` CAS、journal v11 绑定全是上传侧——export 用独立 `exports.db`。
7. `cache.LinkFile` / `AdoptFile` 的方向与 `wholeObject` 计费禁止把缓存对象硬链接到用户路径。
8. `FS.Read` 计入前台并填缓存——export 直读 provider。
9. `health.go` 串行跑 repair / replay / drain 且忽略 `repair_concurrency`——rebalance 独立 goroutine + busy 钩子。
10. `TrimOnce` 从声明顺序最后裁——rebalance 后可能裁掉新副本。
11. `marker.go` `Settings` 摘要：加规则会让每个池在升级时 epoch 提示一次。
12. `pool.go` 汇总 `RangeRead` 全有或全无；`Caps.MaxConnsPerHost` 至今无人消费——§4.5 与 export 是第一批。
13. 路由守卫（`security_all_routes_test.go`）、CLI i18n 覆盖测试、web `i18n.js` 目录都要求新路由 / 新键双语登记。

## 11. 本地无法验证、需要真实账号的

各网盘 CDN 字节流的风控阈值（`Transfer` 默认值）；unofficial 层对同文件多并发 range 的反应；配额错误的真实响应码与错误体（§6.3 的映射全部 `UNVERIFIED`）；`ServerCopier` 在同账号跨目录复制的语义与限速；macFUSE `iosize` 是否生效；exFAT / NTFS 移动硬盘上 `st_dev` 与 mtime 行为。
