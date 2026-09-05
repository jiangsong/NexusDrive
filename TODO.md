# CloudFS 待办清单

对照 `docs/DESIGN.md` 与最初的实施计划逐条核对代码后得出的差距清单。
首次核对时间：2026-09-02；2026-09-05 持续更新。历史条目与补充混排时，以
`docs/IMPLEMENTATION.md` 顶部工作清单及各条最新进展为准，不直接统计历史勾选比例。

**历史基线（2026-09-02，不代表当前环境验收）**：28 个包，345 个测试函数全绿，`go vet` 与 `gofmt` 干净，
`go test -race ./...` 全库无告警，真实 FUSE 挂载通过端到端验收脚本。
本文件只记录**尚未完成**的部分。

图例：`[ ]` 未开始 · `[~]` 部分完成 · `[x]` 完成
每条的**验收**一栏就是这条做完之后应当成立的断言，写测试时直接照抄。

---

## 已完成（2026-09-02 这一轮）

搭建 SFTP 真实挂载做 IO 压力测试时暴露并修掉的问题，都已固化成回归测试：

- **[x] 热缓存读被块大小主导。** `cache.Get` 每次返回整块，一次 4 KiB 读要读满并分配
  4 MiB。新增 `Cache.ReadAt`（只读请求的字节）与 `Cache.Has`（纯存在性判断，不做 IO），
  读路径改为先走 `ReadAt`。热态顺序读 5.2 MB/s → 783 MB/s。
  见 `internal/cache/blockcache.go` 的 `ReadAt` / `Has` / `locate` / `readBlock`。
- **[x] `close(2)` 随机返回 EINTR。** 内核在调用线程收到信号时会取消 FUSE 请求，
  而 Go 用 SIGURG 做抢占，于是提交操作被随机中断；`close(2)` 不可重试，调用方
  无从恢复而数据还没提交。FLUSH / FSYNC / RELEASE 改用与中断解耦的 context
  （`internal/fusefs/fs.go` 的 `durable`）。回归测试：
  `TestFlushSurvivesAnInterruptedRequest`。
- **[x] SFTP 驱动**（`internal/provider/sftp/`）：分片续传、staging 原子改名、
  短写检测、递归删除、断线自动重连、known_hosts 校验。
- **[x] 代理层原始 TCP 拨号**（`internal/net/proxy/dial.go`）：direct / socks5 /
  HTTP CONNECT，让非 HTTP 后端也受同一套规则约束。
- **[x] known_hosts 算法协商。** Go 客户端按自己的偏好选主机密钥算法，
  若 known_hosts 里只有 ed25519 记录就会误报 key mismatch。改为从 known_hosts
  反推可用算法（`knownHostAlgos`）。
- **[x] 只读命令会破坏运行中的上传队列。** 每个 `cloudfs` 子命令都构建同一套栈，
  于是 `cloudfs uploads` 这类查看命令会对正在使用的 journal 跑一遍恢复流程：
  把正在上传的行改回待传（错误写成「interrupted by restart」）、删掉活动写入正在
  填充的 staging 文件、并删除死信条目的数据。journal 改为在打开时取一把
  `flock` 排他锁，只有持锁进程才能跑恢复（`internal/journal/lock.go`）。
  同时恢复流程的「存活对象」集合从「仅 pending/uploading」改为「表里所有行」，
  否则重启会把死信的数据删掉——而那正是 `cloudfs uploads retry` 要重发的东西。
  回归测试：`TestRecoverOnlyRunsForTheQueueOwner`、`TestRecoverKeepsDeadLetterBlobs`。
- **[x] 内容寻址的暂存对象被误删。** 两次写入相同内容共用一个 blob（这是去重的
  本意），但 `Drop` 无条件删除该文件，于是先完成的那一行会把另一行还要上传的数据
  一起删掉。改为仅当没有其他行引用时才删除。回归测试：
  `TestDropKeepsBlobsAnotherRowStillNeeds`。
- **[x] 并发写后立即读回偶发 ENOENT。** 目录重新列举（`meta.PutDir`）会把不在
  后端列表里的子项当作已删除清掉，而上传还在队列里的文件本来就不在后端列表里，
  于是刚写完的文件被删、紧接着的读回失败。`PutDir` 增加 `protect` 谓词，
  只存在于本地的条目既不被删除也不被列举结果覆盖。回归测试：
  `TestPutDirKeepsProtectedChildren`、`TestConcurrentWritesSurviveDirectoryRefresh`
  （回退修复后后者报 50 处失败）。
- **[x] 限流器忽略能力矩阵。** `buildLimiters` 的注释写着「回退到驱动自己的建议」，
  实际只用了配置覆盖加一个硬编码默认值（meta 4 / 下载 8 / 上传 2 每秒），
  `Caps.QPS` 是死字段。局域网 SFTP 后端因此被按公网网盘的速率限流：
  冷遍历 2000 个文件的 9.4 秒里几乎全是限流等待。改为
  「配置 > 能力矩阵 > 内置默认」三级优先级后降到 1.4 秒。回归测试：
  `TestLimiterSeedsFromTheCapabilityMatrix`。
- **[x] 后端调用计数器**（`internal/provider/instrument.go`）：按 remote × 操作
  统计请求数与字节数，经 `/metrics` 暴露为 `cloudfs_remote_calls_total`。
  "热遍历零远端调用"从此是可测量的断言而不是说法。
- **[x] 被中断的 CREATE 会返回 ENOENT。** FUSE 调试轨迹显示 `INTERRUPT` 先于 `CREATE`
  到达，处理器在已取消的 context 上跑，`MountForIno` 把任何 `Path` 错误一律报成
  「不存在」，于是 Go 程序（SIGURG 抢占）创建文件时随机得到 ENOENT——并且内核会把这个
  ENOENT 缓存成负项。两处修复：所有改树的操作（create / mkdir / unlink / rmdir /
  rename / setattr / 写打开）与 FLUSH 一样改用 `durable(ctx)`；`MountForIno` 只把
  `meta.ErrNotFound` 映射为不存在，其它错误原样上抛（映射成 EINTR）。回归测试：
  `TestMutationsSurviveAnInterruptedRequest`、`TestCancelledContextIsNotReportedAsNotFound`。
- **[x] 过期的目录列举会删掉列举期间新建的条目。** 后台预取先向后端发出 `List`，
  本地 `mkdir` 在此期间落地，随后这份不含新目录的列举被 `PutDir` 应用，新目录被当作
  「后端已删除」清掉，接下来在其中创建文件报 ENOENT（基准工具建数据集时稳定复现）。
  `fetchDir` 现在记录列举开始时刻，`fetched_at` 不早于该时刻的条目与本地未上传条目一样
  受保护。回归测试：`TestStaleListingDoesNotDeleteEntriesCreatedMeanwhile`。
- **[x] SFTP 默认 QPS 定得过低。** 原值 64/64/32 是照网盘驱动的量级拍的，
  但 SSH 既没有服务端配额也没有风控，真正的约束是连接本身。实测 500 个小文件的
  队列排空时间因此从 5 秒被拖到 74 秒。改为 256/256/128，可用 remote 的 `qps` 覆盖。

---

## 本轮优化进展（2026-09-02，IO 测试升级）

- **[x] `cloudfs bench`**（`internal/bench`）：仿 `juicefs bench` 的内置基准，每项前后
  读 `/metrics` 取后端调用差值；`--cold` 经 `POST /cache/drop` 冷启动（同时清 VFS 与内核
  缓存，`Mount.DropKernelCaches`）；`--repeat` 取中位数。`scripts/bench/run-matrix.sh`
  用 `cmd/netem`（延迟/抖动/丢包停顿/重置/限带宽/断链的 TCP 代理）跑 lan/wan/bad 三档。
- **[x] 4.3 元数据**：`PutDir` 改为批量（一次扫描、预编译 UPDATE、多行 INSERT），
  FTS 写入延后到 `name_index_pending` 由后台合并，`Search` 联合两表；FUSE 侧
  `OpendirHandle` 返回 `FOPEN_CACHE_DIR`，内核缓存目录流；`SetInvalidate` 把内核失效
  真正接上（此前 `InvalidateFunc` 从未被调用）。
- **[x] 4.4 passthrough**：已接入，非 root 因 `CAP_SYS_ADMIN` 被内核拒绝，详见 T-08。
- **[x] 4.1 子块拉取**：块缓存支持部分在场（64 KiB 子块 + sidecar 位图），随机读未命中
  只取覆盖请求的子块；同一句柄同一块 4 次子块未命中后后台补全整块；顺序读仍整块。
- **[x] 加固测试**（`test/e2e/hardening_test.go`）：8 个 Go 写者 1000 次
  写+改名零 EINTR；4 写者 × 250 次读回校验 + 每 3 ms 强制目录刷新零失败；
  上传期间 20 次并行打开只读栈零死信、零「interrupted by restart」；
  WebDAV（内容寻址路径）300 个相同内容文件全部落地、零死信。
  这组测试又抓到一个漏洞：**刚创建、尚未提交的文件（无 remote id）不受列举保护**，
  高频刷新下会被当作已删除清掉，`close()` 报 EIO。`localOnlyNode` 现在也保护
  「无 remote id 或 dirty 的文件」。回归测试：`TestListingDoesNotRemoveAFileBeingCreated`。
- **[x] 删除与上传竞争（T-00）**：journal 增加 `tombstone`，`Remove` 对
  `uploading` 状态的行打墓碑而不是丢弃，uploader 完成后删除后端文件并丢行。
  回归测试：`TestTombstonedUploadIsDeletedAfterLanding`。
- **[x] 自己的上一次上传被当成冲突**：`Hooks.RemoteVersion` 让 uploader 用节点当前的
  `RemoteVersion`（被我们自己的上一次上传更新过）做比较；连续两次保存不再生成
  conflict 副本。回归测试：`TestOwnEarlierUploadIsNotAConflict`。
- **[x] 首轮三档实测暴露并修掉的三处**：(1) 子块「补全整块」的阈值是固定 4 次，随机负载下
  每个块都很快达到，后台把整个文件拉了下来——改为按块内子块数的四分之一取阈值；
  (2) 组提交对单线程写者每次都等满窗口，改为只在一批里已有多于一条时才短暂等待；
  (3) `OnInvalidate` 钩子在挂载后才安装、与正在服务的 goroutine 竞争读写（race detector
  报出），改为原子指针。
- **[x] 4.2 写路径**：journal 组提交（≤5 ms / 64 行一次事务 + 一次目录 fsync）；
  `Caps.SinglePutMax` + `SinglePutter`，SFTP 小文件单次上传；上传并发取
  `Caps.UploadParallel`，`remotes.<name>.upload_workers` 可覆盖。
- **[x] 干净矩阵首轮（lan）暴露并修掉的六处**——每处都有回退即失败的回归测试：
  1. **创建文件是 O(N) 的**：`lookupNode` 未命中时调用 `readDirRefresh` 只为确认列举
     是新鲜的，却把整个目录读进内存；一个目录里连续建 2000 个空文件花了 39 s
     （每个 20 ms，且随 N 增长）。改为 `freshenDir`：新鲜的列举不再加载。
     回归测试：`TestCreatesDoNotRescanTheDirectory`（`meta.Store.ChildrenScans` 计数）。
  2. **随机读误触发预读**：预读只看「落在上一块或下一块」，256 MiB 文件上 500 次随机
     4 KiB 读有 ~15 次碰巧相邻，每次拉 2 个整块，共拉了 116 MB。改为要求连续 3 次
     读起点落在上一次读结束的 1 MiB 内（`seqArm` / `seqSlack`）。
     回归测试：`TestRandomReadsDoNotArmReadAhead`（回退后 500 次读拉 2.3 MB，超预算 4 倍）。
  3. **旁边跑 `cloudfs status` 时 `close()` 报 EIO**：两个库的写事务都是 deferred
     BEGIN，先 SELECT 后 UPDATE；另一个进程在中间提交就得到 `SQLITE_BUSY_SNAPSHOT`，
     busy handler 不会重试它。meta 与 journal 的 DSN 加 `_txlock=immediate`，写事务
     开头就排队拿锁。回归测试：`TestWritesWaitForAnotherProcessInsteadOfFailing`。
  4. **提交中途失败后句柄报废**：staging 改名到对象目录之后再出错（上面那个 BUSY），
     下一次 FLUSH 又去 fsync 已关闭的 staging 文件（「file already closed」），数据被
     困在对象目录里、节点却没有指向它。`commitWrite` 把改名之后的每一步记在
     `writeState.pending`，重试从断点继续。回归测试：
     `TestFlushResumesACommitThatFailedHalfway`（注入故障钩子 `FS.commitFault`）。
  5. **每个 create/write 的 `getxattr` 都查库**：内核会探测 `security.*`，现在非
     `user.cloudfs.*` 直接 ENODATA。`Upsert` 更新分支不再重建 FTS 行（按名字找到的行
     名字没变），插入分支改写 `name_index_pending`；热点点查（Get/Lookup/IsAbsent/
     DirState）改用预编译语句（SQL 解析曾占 create 路径 CPU 的五分之一）。
     fake 后端上 write+close 从 6.3 ms 降到 4.0 ms，空文件 create 从 7.1 ms 降到 3.4 ms。
  6. **SFTP 变更类 QPS 128 把 unlink 卡在 170/s**：2000 次 unlink 用了 11 s，服务器本身
     能到 1000/s。默认改为 1024/1024/512。子目录预取从逐个串行改为 8 路并发
     （`prefetchFanout`），冷遍历 40 个子目录不再是 40 次串行往返。
  另：`CLOUDFS_DEBUG_ERRNO=1` 会把所有落到 EIO 的原始错误打进日志，这一轮就是靠它定位的。

## 第二轮优化（2026-09-02 晚，针对未达标指标）

按审阅后的方案实施，全部有「回退即失败」的测试；实测数字见 `docs/perf-report.html`，原始数据在
`docs/perf-raw-round2/`。门禁结果（同一拓扑、3 次中位数；测量期间 4 核机 load 9–10）：

**最终门禁（第五轮，2026-09-03 13:10 的矩阵，缓存目录清空后跑；12 项中 11 项达标）**

| 项 | 修复前干净矩阵 | 第二轮 | 第五轮 | 门禁 |
|---|---|---|---|---|
| 冷随机 4 KiB × 500 拉取 | 111 MB | 9.6 MB | 9.6 MB | ≤ 16 ✓ |
| 冷随机 4 KiB × 500 IOPS | 200 | 877 | 1331（直连）/ 1534（经代理） | ≥ 1500 ✗ 直连 / ✓ 经代理 |
| 冷顺序读 256 MiB | 125 MB/s | 212 | 279（直连）/ 246（经代理） | 直连 ≥ 250 ✓，经代理 ≥ 170 ✓ |
| 500 小文件 | 80 个/s | 168 / 212 | 262（power）/ 685（crash） | ≥ 250 ✓ / ≥ 400 ✓ |
| 小文件排空 | 15.7 s | 2.4 s | 2.4 s，四档死信全 0 | ≤ 6 ✓ |
| 冷遍历 2041 项 | 1239 ms | 761 ms | 299 ms（逐条 lookup 0） | ≤ 400 ✓ |
| 热遍历 2041 项 | 78 ms | 17 ms | 23 ms，FUSE 只剩 41 opendir + 41 getattr | ≤ 40 ✓ |
| wan 冷遍历 | — | 5.0 s | 1.66 s | ≤ 3 ✓ |
| wan 256 MiB 顺序读 | — | 2.3 MB/s | 4.5 MB/s | ≥ 2.0 ✓ |
| 正确性 | — | — | fsx 90 万次 A-OK、fio 交叉验证、四档零死信、30 包套件与 27 包 race 全绿 | ✓ |

**唯一未达标项的拆解**：直连冷随机读每次 IO 里我们自己的代码稳定占 ~150 µs，SFTP 调用内部
446–980 µs（`remote_ms_by_op` 直接给出）。1500 IOPS 相当于每次 667 µs，所以后端每次调用
≤515 µs 时达标（隔离实测 1534 / 1725 / 1855 / 1974），≥600 µs 时不达标（1265 / 1331）。
测量期间后端主机（4 核）上有与本项目无关的 `snap-store` 常驻占用 2.6 核。

- **[x] 尺子**：`cloudfs_fuse_ops_total{op}` / READ 尺寸直方图进 `/metrics` 与 bench 输出；
  矩阵加 `direct` 档。回归测试：`TestWalkCostsOneRequestPerDirectoryAndThenNone`。
- **[x] readdirplus 不再逐条查库**：目录句柄实现 `fs.FileLookuper`，冷遍历 2041 项从 1239 ms
  降到 149 ms（direct 档）。顺带三处：`OpendirHandle` 惰性列举（内核缓存命中时每个
  opendir 曾花 1.2 ms 建列举）、挂载 `noatime`（readdir 后内核作废目录属性再 getattr）、
  首次列举不再向内核发失效（否则内核正在填充的目录缓存被扔掉，热遍历重读目录）。
  回归测试：`TestListingInvalidatesOnlyWhatChanged`。
- **[x] SFTP 传输层**：句柄缓存 + `ReadAt`（`handles.go`）、按 banner 选 255 KiB 包、改名/
  删除前 evict、断线 purge、`Delete` 先 REMOVE 后判目录。同时修了一个**独立缺陷**：
  服务端截短的 READ 会被当作 EOF 并把短块当完整块入缓存（`vfs.shortRead` 守卫，
  驱动侧追读）。回归测试：`TestRandomReadsShareOneHandle`（回退后 100 次读 100 次 OPEN）、
  `TestMutationsEvictCachedHandles`、`TestReadRangeFollowsUpAShortAnswer`、
  `TestShortReadIsNeverCached`。
- **[x] 子块 16 KiB**：READ 尺寸直方图显示内核随机读一次要 16 KiB；`cache.sub_block_size`
  可配置；重载时丢弃粒度不一致的 sidecar（**独立缺陷**：改粒度后旧位图会错位）。
  回归测试：`TestSidecarAtAnotherGranularityIsDropped`、
  `TestRandomReadsThroughTheKernelStayWithinBudget`（内核路径 500 次随机读 ≤ 16 MB）。
- **[x] SQL 精简**：负缓存进内存（`absent` 表 v4 迁移删除）、`Path` 递归 CTE、Lookup 先查
  命中再查负缓存、create 后不再 Stat、flush 后不再 Get。fake 后端上 write+close 从
  4.0 ms 降到 3.9 ms、空文件 create 从 3.4 ms 降到 2.95 ms（剩下的是 fsync 与 SQLite 写事务）。
  回归测试：`TestMetadataStatementBudget`（每个操作的 SQL 预算）。
- **[x] SFTP 限流上限再放开**：direct 档冷随机读卡在 921 IOPS，正好是 Download 类 1024/s 的
  令牌间隔——每次 16 KiB 未命中一个令牌。SFTP 没有服务端配额，改为 8192/8192/4096，
  AIMD 只在服务端真的拒绝时才回退。unlink 仍 ~160/s：瓶颈换成 journal（删在途文件要打
  墓碑，一次 FULL 同步事务，同时 uploader 每个上传写三次同步事务）；无门禁，见 P2 异步删除。
- **[x] `journal.durability: power | crash`**：daemon 级；crash 跳过三次 fsync，journal 用
  `synchronous=NORMAL`。附带两个**独立缺陷**：`Recover` 只检查 blob 存在（现在核 size，
  crash 模式再核 crc32c），死信后的节点仍指向已丢失的本地 key（`RepairLost` 摘掉节点、
  作废父目录）。`doctor`/`status` 显示当前模式。回归测试：
  `TestRecoverDeadLettersBlobsThatDoNotMatchTheirRow`、`TestLostUploadIsRepairedInTheTree`、
  `TestKillDuringWriteLosesNothingInCrashMode`。
- **[x] 一条 40 次里挂两次的测试（`TestMutationsSurviveAnInterruptedRequest`）挖出三处竞争**，都与
  uploader 和前台操作并发有关：(1) `unlink` 在「看队列」与「丢行」之间，uploader 可能刚好认领
  了这一行——丢掉它，上传照样完成，下次列举文件复活。`journal.DropPending` 只删仍在 pending 的
  行，已认领的返回 `ErrInFlight` 由调用方打墓碑。(2) 提交与上传完成钩子都按（父目录，名字）
  `Upsert`，中间发生过改名就会在旧名字下插入第二个节点，而改名后的节点指向的本地 key 随即
  被释放——读它永远 EIO。改为 `meta.UpdateByIno`；打开后被 unlink 的文件在 close 时不再复活
  （journal 行丢弃或打墓碑）。(3) 上传落地会释放 local-only 缓存项，而之前打开的读句柄还带着旧
  身份，下一次读报「missing from the cache」。读路径发现 local-only key 已不在缓存时重新取节点
  身份。回归测试：`TestDropPendingLeavesAClaimedRowForTheTombstone`、
  `TestWriteAfterUnlinkDoesNotResurrectTheFile`、`TestCommitAfterRenameKeepsTheNewName`、
  `TestReadHandleSurvivesUploadCompletion`；原测试 200 次循环零失败。
- **[x] 每次目录刷新都重写整个目录的 FTS 索引**：`PutDir` 把所有子项重新排队，indexer 对每一项
  `DELETE FROM name_index WHERE ino=?`（`ino UNINDEXED`，等于全表扫描）再插入；e2e 的强制刷新
  压力测试在 race 检测下因此跑不完（goroutine dump 停在 FTS5 crisis merge）。改为只排队索引里
  没有或名字变了的项（`name_indexed` 镜像表，schema v5），FTS 行以 rowid = ino 定位。
  回归测试：`TestRefreshQueuesOnlyChangedNamesForTheIndex`。修完后 e2e 全包在 race 下通过
  （顺带把 fake provider 的 Faults/Caps 改为加锁读写，此前是测试侧的数据竞争）。
- **[x] 第三轮（2026-09-03 凌晨，目标：全部门禁达标）**，每项都在真实直连挂载上单独验证过：
  1. 小文件 168 → 486 个/s：FLUSH 后不再立刻重建 staging（惰性，绝大多数文件 close 就走）；
     父目录路径与 remote id 按 ino 缓存（改名/删除时整体作废）；写事务用预编译语句；
     内核自己发起的变更不再向内核发 InodeNotify（它自己的缓存是一致的，白白多一次系统调用并
     丢掉刚写的页缓存）；lookup 未命中先看 DirState 再读目录行。
  2. 冷遍历 761 → 111 ms：未变化的目录刷新不再逐行重写（只用一条语句刷新 fetched_at）。
  3. 冷随机读 877 → 1625 IOPS：makeRoom 的过期扫描与 statfs 节流；sidecar 每 8 个子块写一次；
     partial 块的 fd 常驻；子块直接从刚取回的字节返回，不再读回缓存文件；≤256 KiB 的读固定走
     第一条 SSH 会话（轮流会话让 IOPS 掉三分之一）；GC 百分比 400；子块写入也改为 write-behind
     （矩阵里随机读紧跟在 256 MiB 顺序读之后，磁盘还在吸收后者的落盘，同步 pwrite 会被 writeback
     节流卡住），整块最多用到上限减 32 MiB，给子块留内存；`/cache/drop` 顺带 `sync`，让每个冷测
     从安静的磁盘开始。
  4. 冷顺序读 212 → 298 MB/s：整块写缓存改为 write-behind（内存先服务、后台落盘，上限 256 MiB）；
     hydrate 推迟到文件空闲 2 s 后（否则读完立刻再拷一遍整个文件）；`RangeReaderAt` 让驱动直接
     填调用方缓冲（少一次 4 MiB 分配与拷贝，而且发现 `countingPut` 包装层曾把它藏掉）；块缓冲池化；
     预读对已在飞的块不再每个 READ 都起一个等待 goroutine；SSH 会话池（默认 2）。
  5. wan 冷遍历 5.0 → 1.6 s（探针）：以上元数据与列举并发的改动共同作用。
  6. 后台工作让路给前台读（第四轮补充，CPU profile 直接指认）：随机读期间 35% 的 CPU 花在
     目录预取的 `PutDirChanged` + FTS 索引上，hydrate 又会在读完 256 MiB 后立刻把整份文件再
     拷一遍。现在 `vfs.FS.fgReads` 统计内核正在等待的读，预取任务在每次列举前让路（上限 5 s，
     否则忙挂载永远不预热），hydrate 开始前与每 8 个块检查一次，忙就放弃本次拷贝改约下次。
     直连探针：冷随机读 1182 → 1578 IOPS，冷顺序读 255 → 290 MB/s。
  7. hydrate 的判据从「空闲 2 s」改为「空闲 10 s」，并且每拷一个块都问一次前台是否在读：
     两秒不算安静（一次 bench 阶段切换、一次构建、一次目录遍历的间隙都不止两秒），于是整份
     文件的拷贝正好压在下一波读上面。矩阵顺序的直连探针：冷随机读 1310 → 1768 IOPS。
  8. 每个操作的耗时进 `/metrics`（`cloudfs_remote_seconds_total{op}`，bench 输出 `remote_ms_by_op`）：
     调用次数说明「改动带来多少流量」，耗时才说明「剩下的是往返本身还是我们的代码」。实测冷随机读
     586 µs/次里有 441 µs 在 SFTP 往返内，我们自己只占 145 µs——这条线的上限是链路，不是代码。
     `CLOUDFS_PPROF=1` 时控制端口暴露 pprof（默认关闭），第四轮的两处热点都是这样找到的。
  9. 测量条件（写进报告「保留意见」）：后端主机 192.168.0.20 是 4 核，测量期间有一个与本项目
     无关的 `snap-store` 进程常驻吃掉 2.6 个核，sftp-server 与它抢 CPU；直连冷顺序读的中位数
     因此在 130–320 MB/s 之间摆动，`remote_ms_by_op` 显示这段时间确实在 SFTP 调用内部。
     没有去动别人机器上的进程。
  10. 第四轮的自审（reuse / 简化 / 效率 / 分层四个角度）又挖出两处**自己引入的正确性缺陷**：
      写回整块时在新块落盘前就删了旧 claim（崩溃窗口里稀疏块被当完整块）；`dropPartial` 删 claim
      却留下块文件（永久留一个会被当完整块的稀疏文件）。根因都是「创建/删除的顺序散在调用点」，
      现在只有 `openPartialBlock` / `removePartialBlock` 一对函数能建和删 partial 块。
      同一轮还修了：claim sweep 每次 flush 扫全表（改为滞后计数器 + 缓存自己的定时器，不再挂在
      写回循环尾巴）、hydrate 每文件一个定时器无限重排（改为单一 janitor + 复用缓冲区）、
      后端耗时只覆盖 14 个操作里的 3 个且两条读路径量的区间不同（现在全覆盖、每次一把锁）、
      pprof 改为只在回环地址上挂载、「前台在忙」把写也算进去。
  11. **重写一个变小的文件会保留旧尾巴（数据损坏，P0，第四轮矩阵跑挂时发现）**：内核不把
      O_TRUNC 随 open 一起发过来，而是先 open、再用**另一个句柄**做 truncate。此前
      `FS.Open(write)` 在 open 当场就把整份旧内容下载进 staging，于是 truncate 作用在另一个
      staging 上，新写的 4 KiB 落进那份满尺寸的 staging，提交时把旧尾巴一起传回后端——本地和
      远端都是错的，而且悄无声息。矩阵在 wan 档换数据集大小时读到 0 字节的 big.bin 才暴露出来
      （`randread` 因此 panic）。修法：staging 改为第一次写时才建（顺带省掉「为写而打开大文件」
      的整份下载）；另外 `UploadHooks.OnSuccess` 不再把「节点已经往前走了」的旧上传结果写回节点。
      回归测试：`TestRewritingAFileSmallerDoesNotKeepTheOldTail`（真实内核路径）、
      `TestTruncateThroughAnotherHandleIsNotUndone`（vfs 层），两条都在回退修改后失败。
  12. **重写文件会被判成冲突、内容落到 conflict 副本里（P0，同一次矩阵暴露）**：重写是两次上传
      ——内核先做的 truncate，然后是内容。第二次上传声明的「我从哪个远端版本开始改的」用的是
      节点里的 RemoteVersion，而第一次上传（我们自己的）已经把远端版本推进了。修 T-11 时加的
      「跳过已被取代的上传结果」把版本更新也一起跳过了，于是第二次上传对着旧版本，被判成别人
      的改动，内容被写进 `big (conflict …).bin`，原文件停在 0 字节。修法：被取代的完成只取
      RemoteVersion，不取尺寸和 id。回归测试：`TestRewritingAFileDoesNotLandAsAConflictCopy`。
  13. 同一个文件的两次上传彼此赛跑（上一条的完整修法）：先提交的那次跑完之前，后一次的冲突
      检查读到的是它正要改的版本，于是自己的重写被判成别人的改动。现在上传开始前做两件事：
      有更新的提交就直接跳过（`Journal.Superseded`），有更早的同文件上传还在飞就让路
      （`Journal.OlderInFlight` + 不计入重试次数的 `Defer`）。最后落地的一定是最新那次提交。
      `-race` 下 fusefs 连跑八轮干净，此前两轮必挂一次。
  14. 删除后仍在队列里的上传不再进死信：文件（或它所在的目录）在本地已经没了，上传失败于
      `not found` 时直接丢弃而不是报错——否则每次 metadata 压测都会给排空门禁留几条死信，
      而那几条对应的文件早已不存在。回归测试：`TestUploadOfADeletedFileIsNotDeadLettered`。
  15. 截断到 0 不再先把旧内容下载下来：内核把 O_TRUNC 实现成 truncate，而 staging 是按需
      创建并从当前内容播种的，于是重写一个 1 GiB 的文件会先把它下载一遍。现在 truncate 到 0
      直接建空 staging。回归测试：`TestRewritingDoesNotDownloadWhatItReplaces`。
  16. 重写只排一次上传：取代（supersede）改为按 inode 而不是按句柄——内核用自己的句柄做
      truncate，写内容的那个句柄根本看不见 truncate 排的那条上传，于是每次重写都白发一次
      空文件上传，还要靠上传侧的「跳过/让路」去善后。回归测试：
      `TestRewritingQueuesOneUploadNotTwo`。实测 64 MiB 重写 241–297 MB/s。
  17. partial 块的 sidecar 顺序修正（正确性）：重载时「有块文件、没 sidecar」会被当成整块，
     而此前是先写数据再写 sidecar——中间崩溃就会把没取过的字节当命中返回。现在建块文件前先写
     一份空 claim，之后的 claim 允许滞后（只会少claim，不会多claim），队列一空由
     `sweepClaims` 补齐，`Cache.Close` 也补一次。顺带把随机读里「每次 flush 写一次 sidecar」
     降到每 8 个子块或 2 s 一次。回归测试：`TestPartialBlockFileNeverExistsWithoutItsClaim`、
     `TestClaimsCatchUpAfterTheWritesStop`、`TestHydrationWaitsForForegroundReads`、
     `TestPrefetchStandsAsideForReads`。
- **[ ] 探针里 `find` 比内置 walk 慢一倍（wan 档 3.7–4.6 s 对 1.6 s）**：find 对每个条目多做 fstatat；
  readdirplus 已带属性，应当由内核 dentry/attr 命中，需要看 fuse_ops 里 getattr 是否为零。：41 个目录、每次列举 ≥3 次往返（opendir/readdir/
  close），8 路预取理论上 ~1.5 s；探针显示同一棵树两次冷遍历分别 3.5 s 与 15.8 s（后者 52 次
  列举几乎串行）。需要给每次 List 加时延与并发度统计再定位；候选原因：预取任务与 walker 在
  `dirFlight` 上的等待顺序、pkg/sftp 单会话上多个 ReadDir 的排队。
- **未做（有意）**：SFTP 会话池（BDP 不需要）、异步 unlink（无门禁，P2）、Store 级内存节点
  缓存（`cloudfs warm/pin` 跨进程写库会让它过期，先靠 SQL 精简）、cgo SQLite。

## P0 — 正确性

### [x] T-00f 目录替换和 delta 删除丢失本地后代（2026-09-05）

- 目录列表合并在同名目录 ID/remote 改变或文件/目录类型互换时，原实现复用
  inode 并留下旧子项。现在无保护内容时事务内替换为新 inode；有受保护后代则
  保留旧目录及其远端对象身份。回滚、搜索索引、旧子目录刷新、通知预算均有回归。
- delta 原删除分支只检查节点自身，远端递归删除会丢失未上传后代；类型更新也会
  在原 inode 上改 Kind。新增事务内快照校验、VFS 保护和递归替换，成功推进刷新
  代次，失败不推进 delta 游标并保守通知。测试先复现 ENOENT/错误 inode，再修复。
- 另复现已有文件在未提交写入期间未标脏而被远端更新/删除/目录替换覆盖。VFS
  按 inode 保留活动写句柄引用直到 close 提交完成，多句柄及发布窗口都有回归。
- 这是本地保留与发布一致性修复，不代表远端父目录重建、跨客户端快照或真机
  FUSE 验收完成。大目录/临时空间/全树并发边界继续见 `docs/directory-refresh.md`。

### [x] T-00e 旧目录列表撤销已提交的删除/改名（2026-09-05）

- 先发出的 provider List 在本地删除/改名之后返回，原实现仍接受其提交，导致旧
  名称重新出现；跨目录移动的旧目标列表也可能覆盖移动后的内容。修复前存储层
  及 VFS 确定性交错回归均失败，VFS 明确复现 old listing resurrected。
- Remove/Rename 与刷新代次更新共用事务，跨目录同时使两边旧列表失效；Invalidate/
  InvalidateAll 及传统 PutDir 也参与。失败整体回滚，无关目录和同名同位置改名
  不受影响。待上传文件改名后本地读回、journal 目标以及排空后读回已有验证。
- 这不证明 provider 跨客户端分页快照或所有目录类型/对象替换语义；仍见
  `docs/directory-refresh.md` 的未完成边界。

### [x] T-00d 远端父目录消失时保护未上传后代（2026-09-05）

- 原 PutDir 只检查直接子项，干净远端目录内的本地未上传文件可能随整个子树一起
  从元数据中删除。传统与新暂存合并现在都检查后代保护条件，保留其可读本地路径。
- 反向验证：停用新保护后，两个存储分支均报 local descendant lost，VFS 读回报
  no such file；恢复保护后通过。正常无保护内容的子树仍删除，搜索索引也清理。
- 这不重建已删除的远端父目录，也不解决所有跨客户端并发或远端结果对账问题。
  冷刷新暂存及其限制见 `docs/directory-refresh.md`。

### [x] T-00c 上传删除竞争与未交接对象保护（2026-09-05）

- 用确定性失败回归复现 `DropSuperseded` 在候选快照后误删被领取行，改为事务内
  重新检查 pending 状态；已领取／终态保留，不再无条件 Drop。其他数据库错误不吞掉。
- `DropPending` 把状态检查、行与附属记录删除放进同一事务，消除死信检查后被
  requeue／claim、却仍被删除的窗口。失败回滚保留 blob／parts／dead letter。
- 用失败回归复现相同内容的新 staging 已改名、尚未 Commit 时被旧任务 drop 删除。
  新增按 staging ID 的临时对象引用，成功交接才释放；同内容多写者、重复提交和
  提交失败重试分别覆盖。引用检查与 unlink 同事务，并与 staging rename 互斥。
- 此项只修复底层删除安全，不表示在线 `uploads drop`、活动上传取消或本地版本
  持久化清理已完成；这几个入口与恢复闭环继续保留在原目标中。

### [x] T-00 上传进行中删除文件会让它复活（已修，见上）

- **证据**：`internal/vfs/write.go` 的 `Remove`：本地未上传文件只丢弃 `pending` 状态的
  journal 行；若该行已被 uploader 领走（`uploading`），上传会照常完成并在后端重新创建
  这个已被删除的文件，下次目录刷新后它又出现。`internal/fusefs` 的
  `TestKernelDirCacheIsInvalidatedOnChange` 在「改名后立刻删除」时复现过一次
  （列表里多出已删除的 `c.txt`）。
- **做法**：删除时若存在 in-flight 上传，记一条「完成后删除」的补偿（journal 行加
  `tombstone` 标记，uploader 在 `CompleteUpload` 成功后立即调用 `Delete`），或在
  uploader 的 hook 里检查节点是否仍存在再决定是否上报成功。
- **验收**：写入→改名→立刻删除，排空队列后后端不存在该文件；chaos 用例覆盖
  "删除发生在 UploadPart 与 CompleteUpload 之间"。

### [x] T-00b 上传完成瞬间读取偶发 EIO（2026-09-06）

- **2026-09-06 完成**：验收补齐，且先证明了这个窗口确实存在而不是靠推测。
  - **确定性回归**：`internal/vfs/publish_ordering_test.go` 的
    `TestBlobIsCachedUnderTheRemoteKeyBeforeTheNodeMoves`。在 `meta.UpdateByIno` 之后加了一个
    发布缝（`publishFault`，与既有 `uploadCleanupFault` 同形，生产为 nil），测试在这一点整读该
    文件并断言 provider 下载次数为 0。把 `cache.LinkFile` 挪到这个缝之后，测试立刻报
    「1 backend reads while publishing」——**这是先复现后修复的证据，不是事后补的空断言**。
  - **压力验收**：`test/e2e/hardening_test.go` 的
    `TestReadsStraddlingUploadCompletionStayLocal`。真实 FUSE 挂载上 1000 次
    写→改名，4 个读者持续读最近发布的文件，全程零失败、零后端下载，排空后再整轮读回。
  - **实测到的边界**：自然时序下这个窗口小于 1 毫秒。把它人为放宽 2 ms 并回退修复后，
    1000 次里也只命中 1 次，所以**压力测试单独不足以捕获这个顺序回归**，确定性回归才是。
    这一点写在这里，免得以后有人以为压力测试能替代它。

- **2026-09-05 核对**：当前 `UploadHooks.OnSuccess` 已先 `cache.LinkFile` 再 `meta.UpdateByIno`，并有旧读句柄跨上传完成的回归测试。以下为历史问题记录。

- **证据**：全量测试中 `TestMutationsSurviveAnInterruptedRequest` 出现过一次
  「改名后立刻读取 → input/output error」，单独重跑 5 次不复现。时序上处于
  「上传刚完成、节点已切换到远端 id/版本、缓存尚未在新键下登记」的窗口：
  读路径按新键查缓存未命中，转而向后端取，而此时后端/版本对不上。
- **做法**：`UploadHooks.OnSuccess` 里先把 blob 登记到新键（`cache.LinkFile`）再更新
  节点，或让读路径在新键未命中时回退查旧的本地键。
- **验收**：chaos 用例「写入→改名→上传完成瞬间循环读取」1000 次零 EIO。

## P0 — 配置项与实现不一致

用户按文档配置后不会报错、也不会生效。这类问题比"功能没做"更需要优先处理，
因为它会让人误以为功能在工作。

### [x] T-01 控制面 Unix socket 监听与 status 接入

- **2026-09-05 完成**：`internal/control/listeners.go` 实现 Unix/TCP 双监听，Unix socket 权限 0600，锁保护陈旧 socket 恢复，拒绝覆盖普通文件/活动 socket；CLI mount 同步绑定、status 优先走 socket，离线只读 journal。生命周期、冲突保护、TCP 回退与只读日志测试通过。以下为历史差距与验收要求。

- **证据**：`internal/config/config.go:169` 设了默认值 `~/.cache/cloudfs/control.sock`，
  `internal/config/config.go:191` 还对它做了 `~` 展开；但全仓库消费 `cfg.Control`
  的地方只有 `cmd/cloudfs/main.go:392`，用的是 `cfg.Control.Metrics`（TCP）。
  `Control.Socket` 没有任何读取方。
- **文档依据**：DESIGN §3.8「控制 API：Unix socket + 可选 `127.0.0.1` HTTP」。
- **影响**：写了 `control.socket` 的用户得不到任何反馈；同时 TCP 端口是目前唯一入口，
  在多用户机器上比 socket 的文件权限模型更宽松。
- **做法**：在 `internal/control` 加一个 `net.Listen("unix", path)` 的监听器，
  复用现有的 `s.mux`；启动前 unlink 陈旧 socket 文件，权限设 0600；
  daemon 停止时清理。`cloudfs status` 优先走 socket，socket 不存在再退回 TCP。
- **验收**：
  - 配置了 `control.socket` 时，该路径出现且 `curl --unix-socket <path> http://x/healthz` 返回 200。
  - socket 文件权限为 0600，属主是当前用户。
  - 进程被 `kill -9` 后重启，陈旧 socket 不会导致 "address already in use"。
  - 未配置 `control.socket` 时行为与现在完全一致。

---

## P1 — 文档承诺了、实现里没有对应代码

### [~] T-02 国外网盘与通用协议：一期后端全部接入，真实账号与真机验收仍缺

- **进展（2026-09-06）**：Google Drive、Box、SMB 三个驱动补齐，`cloudfs providers` 现在
  列出 aliyun / baidu / box / dropbox / fake / gdrive / onedrive / openlist / pan115 /
  pan123 / quark / s3 / sftp / smb / tianyi / webdav，一期范围内的后端类型全部有实现。
  **没有引入 rclone**：三个驱动都按各自官方接口自研，依赖树只增加了 SMB 需要的
  `github.com/hirochachacha/go-smb2`（MIT）及其 BER 解码依赖。
  - **gdrive**：`headRevisionId` 作为内容版本，读取直接钉在该修订上，远端并发改写不会把
    新字节写进旧的块缓存键；修订被清理时先重新核对版本再回退 head。同名子项让目录明确
    报错而不是静默丢弃，Workspace 文档/快捷方式因为没有字节流而跳过。写入前查同名子项，
    命中则更新该文件，避免自己制造同名冲突。21 个测试，含状态化 HTTP 回放。
  - **box**：文件与文件夹是两套编号，provider ID 带类型前缀（`f:` / `d:`），并有专门的
    回归证明混淆两者会删错东西。分片 commit 必须带整文件 SHA-1，缺哈希直接拒绝开 session；
    `SinglePutMax` 取在 Box 的 20 MB session 下限上，中间没有传不上去的区间。同名上传由
    409 的 conflicts id 转为新版本。29 个测试。
  - **smb**：路径即身份，与 SFTP 同构。关键差异是 go-smb2 的 rename 不覆盖已存在目标，
    所以发布上传要先 unlink（窗口已在代码与文档中写明），而 `Move` 不做这个 unlink 以免
    毁掉目标位置的无关文件。读路径带引用计数的句柄缓存，断链只重试一次并强制重新挂载。
    通过 `ConfigDialer` / `ConfigLimiters` 继承代理与限流（SMB 不走 HTTP，拿不到共享 client）。
    38 个测试用内存共享复现了"改名不覆盖""非空目录不可删""短读"三条服务端行为。
  - **仍缺**：三者都没有浏览器 OAuth 向导；gdrive / box 没有真实账号验收；**smb 没有在
    任何真实 SMB 服务器上跑过**，内存共享测试不等于真机验收。因此 T-02 保持部分完成。
- **进展（2026-09-02）**：`internal/provider/sftp/` 已实现并注册，通过真实
  SSH 服务器验证（挂载、读写、改名、递归删除、断线重连）。同时补上了
  `internal/net/proxy/dial.go` 的 `Manager.DialContext`，让不走 HTTP 的后端
  也经过同一套规则路由（direct / socks5 / HTTP CONNECT 隧道）。
- **证据**：`go.mod` 无 rclone 依赖；`cmd/cloudfs/main.go` 注册的是
  aliyun / baidu / pan115 / pan123 / quark / tianyi / webdav / sftp / s3 / dropbox / onedrive
  （`internal/provider/webdav/webdav.go:505` 另把 `openlist` 注册为同一实现的别名）。
  Google Drive / Box / SMB 仍没有对应 provider 注册；没有为剩余后端引入 rclone。
- **文档依据**：DESIGN §3.1 的驱动来源表，一期范围含
  Google Drive / OneDrive / Dropbox / Box / S3 / WebDAV / SMB / SFTP，M1 的核心交付。
- **影响**：代理层（规则路由、GEOIP、出口组、健康检查）是为访问境外网盘写的。
  SFTP 已让非 HTTP 后端走 `DialContext`，Dropbox 则让真实境外 HTTP provider 走共享代理层；
  其余三类账号仍只能通过 WebDAV/SFTP 等桥接方式间接使用。

- **S3 当前实现（2026-09-05）**：`internal/provider/s3` 使用 MinIO Go SDK v7.3.0，支持
  prefix confinement、流式 delimiter 列举、HEAD/ETag、条件 Range、预签名 URL、小文件
  单 PUT、可跨重启 multipart、服务端 copy 和文件/目录 move/rename/delete。SDK 请求通过
  CloudFS 原始响应 transport 继承代理/限流/熔断且关闭内部重试。状态化 SigV4 HTTP 回放
  覆盖 streaming chunk、session 恢复与 VFS 冷读。S3 rename 是 copy 后删源而非原子操作；
  版本化 bucket、Object Lock 和 AWS/MinIO/R2 等真实服务仍未验收，故 T-02 保持部分完成。
- **Dropbox 当前实现（2026-09-05）**：`internal/provider/dropbox` 直接调用官方 HTTP API v2，
  支持 cursor 分页与流式列举、`rev` 核对的 Range、4 小时临时链接、小文件 upload、可跨重启
  的顺序 upload session、服务端 copy/move/rename/delete，以及 access token 401 后单次 refresh。
  状态化 HTTP 回放覆盖 Unicode header、重复 append 的 correct-offset 确认和 VFS 冷读；
  recursive changes cursor、delete/upsert 和 cursor reset 的 VFS 全链路也已接入。尚无浏览器
  OAuth 向导及真实个人/团队/App Folder 账号验收，T-02 仍为部分完成。
- **OneDrive 当前实现（2026-09-05）**：`internal/provider/onedrive` 直接调用 Microsoft Graph
  v1.0，以 stable item ID、`cTag`/`eTag` 和 SHA-1 建模；支持受约束的原生分页、delta/reset、
  版本核对 Range、短期预认证链接缓存、单次上传、可跨重启的顺序 upload session，以及原生
  move/rename/delete。状态化 Graph/CDN 回放覆盖签名 URL 的 Bearer 隔离、错误脱敏、OAuth
  refresh-token 轮换持久化和 VFS 增量读取。尚无浏览器 OAuth 向导，也未用个人、组织或
  SharePoint 真实账号验收，T-02 仍为部分完成。
- **剩余做法**：评估官方 SDK，或新增精简 `internal/provider/rclone/` 并用
  `github.com/rclone/rclone/fs`（MIT）包装 Google Drive / Box / SMB：
  - `ReadRange` → `OpenOptions{RangeOption}`；
  - 上传走 rclone `Put` 流式，能力矩阵按后端填 `RapidUpload: nil`；
  - `ChangeNotify` 映射为 `ChangeLister`，没有的后端把 `Caps.Delta` 置 false；
  - `http.Transport` 注入我们自己的代理路由器（不用 rclone 的 `override.http_proxy`，
    统一出口更可控）；
  - 为每个后端类型调一次 `provider.Register`。
- **注意**：引入 rclone 会显著增大依赖树，先确认体积与构建时间可接受，
  必要时用 build tag 把它切成可选组件。
- **验收**：
  - `cloudfs providers` 列出 gdrive / onedrive / dropbox / box / s3 / smb / sftp。
  - 现有 conformance 与 chaos 套件在 rclone 包装的本地后端（如 `:local:` 或 minio）上全绿。
  - 一个配了 `proxy: proxy` 的境外 remote，其请求确实经过配置的出口
    （用 httptest 假出口断言 CONNECT 到达）。
  - Range 读的字节正确性用与 `httpx.RangeBody` 相同的属性测试覆盖
    （服务端忽略 Range 时不能污染块缓存）。

### [~] T-03 MCP Resources：读取与订阅已接入，长期负载和分页内存待验收

- **2026-09-05 SFTP 生产流式接线**：独立 v3 读取器已接入专用 SSH 连接池、
  认证/主机密钥/代理/限流与 VFS TEMP，StreamList 已宣告。新增
  `directory_connections`（默认额外 2 条，最大 32），取消列举不关闭普通读写
  会话，槽位/限流等待随关闭中断。回环真实 SSH、部分列表不发布、取消/关闭/
  复用/重连/错误回调已有回归；十万项加密回环累计分配基准已有，不代表峰值内存
  或真实 NAS 吞吐。详见 `docs/sftp-directory-stream.md`。以下“生产 SFTP 全量
  枚举”为历史状态；兼容 List、递归删除、TEMP 配额与规模验收继续开放。

- **2026-09-05 驱动流式补充**：增加 Caps.StreamList/StreamLister，计数包装保留
  接口，VFS 按能力矩阵直接送入已有 TEMP 收集器。WebDAV/OpenList 已逐条解码
  HTTP 响应，完整收尾前不发布；单元素读取预算、取消/回调错误、不重放前缀及
  编码资源 ID 有回归。SFTP 当前依赖仍全量返回，不算完成；TEMP 预算、最终大
  事务、慢通知/SDK 回收及真实规模性能继续开放。

- **2026-09-05 冷刷新补充**：VFS 已按 200 项批次将 provider 页暂存至 SQLite TEMP
  表，最终一个事务发布完整目录；旧节点、删除集合与变更不再作为整目录 Go 数组
  加载。元数据库 schema v9 校验刷新代次、数据库及目录身份；重复游标/名称、
  存储失败和中断不能发布半份列表。修复父目录消失时丢掉未上传后代元数据的问题，
  大变更通知按子树合并，FUSE 全挂载失效合并为单 worker。具体证明与剩余边界见
  `docs/directory-refresh.md`。provider 单次完整枚举、TEMP 空间配额、最终大事务的
  写锁延迟、慢客户端通知隔离、SDK 长期回收及真机性能仍开放。以下“VFS 跨页
  全量聚合/无流式暂存”为历史状态。

- **2026-09-05 分页补充**：Resources 与 list_directory 已改用 VFS/Meta 有界页，
  不再每次加载、排序全部缓存子项。新工具游标按名称续读，保留严格的旧数字游标
  兼容；工具上限 1000 项，资源按 128 项窗口和准确 JSON 字节预算输出。新增万项
  索引计划、1200 项完整遍历、删除后续页、非法游标先拒绝及零整目录读取回归。
  冷/过期 provider 刷新仍全量聚合并原子合并；工具 total 仍有独立索引计数成本，
  不与 entries 承诺同一快照。冷目录流式暂存、慢通知连接隔离、SDK 长期回收及
  真实环境验收仍开放。以下“分页每次全量加载”为历史状态。

- **最新协议接线**：新旧资源订阅、确认后通知、权限校验、批次原子拒绝、25ms 合并、
  数量上限及断连/关闭清理已有实现和回归。HTTP 按协议版本分别使用 stateful/stateless
  传输，修复默认降级到旧协议、冗余取消通知使客户端连接失效及目录订阅阻塞服务关闭。
  同时修复 `:端口` 无认证暴露所有网卡的问题，加入跨源保护。具体边界见 `docs/mcp.md`。
  以下“未接入订阅/不声明能力”为历史状态。仍缺慢连接延迟隔离、SDK 旧版空 URI 容器
  长期回收、大目录分页内存优化和真实 FUSE/网盘长期验收；不据本轮协议测试标为全部完成。

- **事件流接线补充**：VFS 已有独立 `WatchChanges`，覆盖本地提交、内核来源、改名/
  删除、复制可读版本及远端 delta/目录刷新，队列有界且溢出转重查提示。会话订阅、
  取消/断连、权限过滤和通知合并尚未完成，不能据此把 T-03 标成完成。见 `docs/vfs-changes.md`。

- **2026-09-05 当前进展**：已注册允许列表内的资源入口和模板，支持 `resources/read`
  的文本/二进制/空文件、有界字节范围及目录分页；URI 包含 remote 和完整虚拟路径，
  校验挂载归属、路径穿越和参数。模拟传输与真实 Streamable HTTP 测试覆盖权限、认证、
  缓存读取和分页。资源返回 private、TTL 0，避免 SDK 默认 public 及陈旧客户端内容。
  变更订阅尚未实现，不宣告 subscribe 能力；T-03 仍未完成。以下为历史差距与原验收。

- **证据**：`internal/mcpsrv/` 全目录 grep `Resource` 零命中。
  `internal/mcpsrv/server.go:366-450` 只注册了 15 个 tool：
  `list_directory stat stat_many read_text read_range search cache_status list_roots
  get_download_url write_file edit_file create_directory move delete pin`。
- **文档依据**：DESIGN §3.7「Resources：`cloudfs://<remote>/<path>` 支持 `resources/read`
  与 `resources/subscribe`（文件变更推送，来自 MetaStore 事件）」。
- **影响**：agent 只能主动轮询，拿不到文件变更推送；
  也无法把云盘路径作为资源引用挂进对话上下文。
- **做法**：
  - `resources/list` 列出各挂载根，URI 用 `cloudfs://<remote>/<path>`；
  - `resources/read` 复用 `read_text` / `read_range` 的同一套字节上限与白名单校验，
    文本返回 text、其余返回 blob；
  - `resources/subscribe` 挂到 refresher 的事件上（`internal/vfs/refresh.go` 已经
    在应用 delta 时知道哪些 ino 变了，加一个订阅者列表即可），
    本地写入路径也要发事件，否则 agent 自己写的文件不会触发通知。
- **验收**：
  - 用 SDK 的内存 transport 起一个 server，`resources/list` 返回所有挂载根。
  - 订阅某路径后，远端 delta 改动与本地 `write_file` 都能收到一次通知，且不重复推送。
  - 资源读取受 `--allow` 白名单约束，越界返回错误而不是内容。

### [~] T-04 Copy 已接入，完整作业恢复与远端对账尚未完成

- **上传账号绑定补充（journal v11）**：普通写入和复制交接都持久化原元数据库身份、
  挂载前缀、根对象及本地账号绑定。账号绑定由显式授权代次与非密钥账号定位配置组成；
  自动 token 刷新和仅迁移密钥存储保持稳定。Uploader 在解析 provider 前 fail closed，
  漂移或旧 schema 空绑定任务进入死信且零远端调用；确认 resume 可在重新核对当前
  inode/路径/父对象及完整 CRC 后原子采用当前绑定。配置轮换、v10 只读兼容/owner
  迁移、普通写入/Copy 持久化、六类漂移、daemon 重启换账号和显式接纳均有回归。
  这解决队列跨配置误发，不等于云端身份认证、未知远端结果对账或历史版本管理完成。

- **MCP 上传管理补充**：新增 `list_uploads/get_upload/retry_upload/cancel_upload/
  resume_upload/discard_upload/flush_uploads`，与当前 VFS/Journal/Uploader 安全流程
  共用，工具总数增至 29。响应不含私有路径、hash、session、远端版本或原始错误；
  列表原始扫描有界且游标认证加密。受限允许列表只管理仍严格绑定当前授权路径的
  任务，修改时在 publication gate 内复核路径；全队列 flush 以及清理后可能失去
  路径的 discard 仅向无允许列表限制的 MCP 开放。只读/确认/隐藏分页/字节预算、
  路径替换和本地清理均有回归。自动远端对账和历史策略仍保持开放。

- **上传清理最新补充**：修复旧 GC 对相对/绝对路径别名漏判造成的共享内容误删；
  元数据增加全部已授权本地别名的原子删除/FULL 屏障及旧列表防复活。VFS 的
  读写句柄/挂载与缓存协调、daemon 启动续清理已接入，并补 VFS 三阶段强杀恢复。
  CLI/control 的 `drop <id> --confirm` 已使用相同 VFS 流程；离线不构建网盘、
  不恢复其他上传，生产命令强杀/daemon 续清理和不重放已有验证。首次已替换或
  缺失版本的历史清理及完整历史管理仍开放，T-04 不因此完成。
  详见 `docs/upload-cleanup.md` 与第三十至三十二批验收记录。

- **Copy 账号绑定补充**：准备任务的持久 JSON 已记录源/目标账号代次；未完成下载
  在 provider IO 前验证源，重试和发布验证目标，旧空绑定任务不自动接纳且不向受限
  MCP 暴露。已交接上传继续使用 journal v11 目标绑定。源/目标换绑零远端调用回归
  已有；服务端未知结果对账、跨客户端无覆盖及完整历史仍开放。

- **上传清理存储补充（journal v10）**：已实现 purging 意图、私有路径/共享内容
  检查、unlink/同步失败重试、会话历史删除及永久 ID 防复活，状态/metrics/doctor/
  Flush 和普通 namespace 拒绝已识别。Journal 三阶段强杀恢复已有回归；VFS
  与 daemon 后续接线见上述最新补充。历史保留/GC 与
  远端对账仍开放，详见 `docs/upload-cleanup.md`。

- **上传显式恢复补充（journal v9）**：已有 `uploads resume <id> --confirm` 和控制端点。
  全量 CRC、当前本地版本／目标校验后，按取消修订号重新排队；新取消或同 inode
  新提交使旧恢复失效。旧会话与分片保存在私有历史，再开始新尝试，不盲目复用
  可能已经完成的会话。确认接受远端重放风险，不替代自动对账；在线 drop 已由后续
  批次补齐，完整版本清理和历史管理仍未完成。普通上传账号绑定见上方 v11 补充。

- **上传取消补充（2026-09-05，journal v8）**：新增 CLI `uploads cancel <upload_id>`
  与控制端点，支持已交接上传停止。cancelling 表示 worker 尚未退出，cancelled
  不代表远端撤销；内容、会话和分片保留，重启不重新上传。普通 retry/drop 不接受
  取消记录，本地版本删除／改名暂拒绝，专用对账／重试／清理闭环仍缺。以下“完全
  没有已交接上传取消”是历史状态，不能因此把 T-04 或在线 drop 标记完成。

- **MCP 管理补充**：新增列表/单条查询、重试、取消、显式确认清理五个工具，经 VFS
  查询投影和原管理接口执行。双路径允许列表、当前挂载/根对象/元数据库身份检查，
  不返回内部恢复数据；原始扫描有界，游标认证加密。回归覆盖权限/只读、字节预算、
  隐藏分页、真实 HTTP、活动复制取消与检查点续跑、已交接/有引用内容拒绝清理。
  同时复现并修复恢复下载盲目追踪已改名源对象 ID 的问题；ready 内容不再要求源存在。
  以下“MCP 管理入口仍缺”为历史状态；已交接上传取消、完整终态、远端对账、原子
  不覆盖及准备目标改名等剩余范围不变。

- **清理补充（schema v7）**：已有 `copies forget <id> --confirm` 与 `POST /copies/forget`。
  原元数据库确认无本地引用并做 FULL 屏障，再保存 purging 意图；缓存/私有 payload
  删除失败、数据库提交失败或中途强杀可继续清理。上传引用及活动句柄阻止清理，绑定
  墓碑保留，其他硬链接/打开租约不受影响。已有分层与 CLI/HTTP 回归。此命令不删除
  本地/远端文件，不取消上传，也不补全远端终态历史；以下“完全没有清理”属于旧状态。
  本轮另修正恢复测试将 submitted 当作发布完成、过早停止 worker 的时序假设，并补
  确定性取消窗口的恢复测试。实际云盘、内核挂载和物理掉电仍待验收。

- **准备管理补充**：CLI/控制面增加 retry/cancel，schema v6 持久化 cancelled 与 revision，取消阻止检查点/上传交接，重试验证前缀与目标绑定并唤醒 worker，不能覆盖新取消或恢复已删除目标。离线取消不初始化 provider，离线重试不启动后台传输。启动还会恢复失败/取消作业仍被引用的完整缓存及临时保护，不因此重启任务。已交接上传的取消、MCP 管理入口、终态与内容清理、服务端对账等仍缺；以下历史文字中的“没有重试/取消”以本段为准。

- **生产 CLI 与查询入口补充**：现场编译的生产 `cp` 在第二段下载阻塞时被强杀，由独立生产 MCP-only daemon 恢复，HTTP WebDAV 源/目标验收确认只补剩余字节及一次完整上传。新增 `copies list/show`、`GET /copies` 与稳定 ID 分页；离线只读不初始化 provider、不恢复活动任务，旧日志无需迁移。submitted 仅表示交接，不能当作远端完成。MCP 查询、重试/取消、终态回收和真实账号仍待补；以下“真实 CLI 子进程待验收/没有查询入口”属于历史状态。

- **最新接线与验证**：VFS Copy 已使用准备作业，daemon 已接入检查点续跑。元数据库 schema v6 用独立数据库身份及原子复制绑定保护目标，下载恢复检查源版本、挂载与写权限。VFS 子进程下载/目标绑定后强杀恢复、仅补剩余字节、唯一上传交接、删除不复活、源变化拒绝以及 daemon 在线/离线调度测试已有；修复了绑定拒绝时清掉仍被元数据引用的本地缓存问题。仍缺真实 CLI 子进程端到端验收、作业管理/回收、准备目标改名语义、服务端对账及跨端原子不覆盖契约。以下补充中的“尚未接线”和占位节点描述属于历史状态，以本段及 `docs/copy-preparation.md` 为准；T-04 仍未完成。

- **准备日志补充**：schema v5 增加复制意图、已同步前缀检查点、恢复校验/尾部截断、内容所有权及原子上传交接。Journal 层强杀/恢复/哈希/不重复入队回归已有。

- **发布恢复补充**：新增日志发布标记与上传领取门禁，修复 journal 已提交、元数据未更新就开始上传的窗口；daemon 在启动上传/刷新前恢复本地版本，power 模式发布使用 FULL 元数据事务。子进程强杀后的本地读回/继续上传、缓存发布失败和缓存链接重建已有回归。提交日志前的占位节点、下载作业与服务端结果对账仍未完成。

- **2026-09-05 当前代码**：VFS、CLI cp、控制面与 MCP copy 已接入，覆盖单文件服务端复制、跨 remote 完整缓存 inode 复用、目标哈希与秒传上传、strict 等待、离线持久化、权限约束与控制请求不重放。Copy/Journal 新测试的编译与平台链接检查问题已修复。仍缺下载准备阶段的持久化任务、服务端结果不确定时对账、跨端同名创建的原子无覆盖契约、发布失败窗口与真实账号验收；不能标记完成。详见 `docs/copy.md`。以下为历史差距与原验收要求。

- **证据**：`internal/provider/provider.go:165` 定义了 `Copy`，
  `provider.go:124` 有 `Caps.ServerCopy` 标志位；但 `internal/vfs/` 没有 `Copy` 方法
  （见 `grep '^func (f \*FS) [A-Z]'` 的完整列表），CLI 无 copy 子命令，MCP 无 copy 工具。
- **文档依据**：DESIGN §3.1 的接口定义、§3.7 的工具表（`mkdir / move / copy`）、
  以及 §3.4「内容寻址去重：同一份本地 blob 既是读缓存也可以对另一 Provider 发起秒传，
  跨网盘拷贝不必重新下载」。
- **影响**：这条影响面比看起来大。§3.4 那个卖点——**跨网盘拷贝复用本地 blob 直接秒传**——
  底层 blob 表已经在了，但上层没有任何入口能调到它，所以这个能力目前等于不存在。
- **做法**：
  - `FS.Copy(ctx, src, dst)`：同 remote 且 `Caps.ServerCopy` → 走服务端 copy；
  - 跨 remote：查本地 blob / 已 hydrate 的完整文件，有则直接按目标 Provider 所需的
    hash 发起秒传；没有则先 hydrate 再走正常写路径（复用 journal，保证可续传）；
  - 暴露为 `cloudfs cp` 与 MCP 的 `copy` 工具。
- **验收**：
  - 同 remote 内 copy 只产生 1 次远端调用（服务端 copy）。
  - 跨 remote copy 一个已完整缓存的文件，产生 0 次下载调用。
  - 目标端支持秒传时，上传阶段的字节传输量为 0。
  - 中途 kill -9 后重启，copy 能续传完成，不留半个文件。

### [~] T-05 凭据安全存储与轮换持久化

- **2026-09-05 进展**：实现 keyring/0600 文件存储及引用解析、配置原子更新、阿里/百度/115 的 refresh token 轮换保存与失败重试、doctor 提示。daemon 重启回放测试通过。T-06 授权/迁移命令主体已有；真实账号、系统 keyring 和交互验收仍缺。旧明文配置保持兼容，轮换时自动迁移 refresh token。以下为原始需求。

- **证据**：`internal/config/config.go` 中 grep `token|secret|password` 零命中——
  凭据是通过 `Remote.Extra map[string]any`（`config.go:123` 的 inline 字段）
  原样读进来的，也就是明文躺在 YAML 里。
- **文档依据**：DESIGN §5 代码结构中的 `config/ # YAML 配置、密钥存储（keyring）`。
- **影响**：refresh token 与 cookie 明文落盘；配置文件被误提交或备份即等于凭据泄漏。
- **做法**：加一层 `config.Secrets` 抽象，默认实现读写系统 keyring
  （Linux: Secret Service / kwallet；macOS: Keychain），
  YAML 里只留 `token: keyring:<remote>/<field>` 这样的引用。
  无 keyring 的环境（headless NAS）退回到 0600 的独立文件并明确告警，
  `cloudfs doctor` 报告当前用的是哪种。
- **验收**：
  - `cloudfs config auth` 写入后，YAML 中不含任何凭据明文。
  - 删除 keyring 条目后启动，报错信息明确指向重新授权的命令。
  - headless 环境下降级路径可用，且 doctor 会指出这是降级。

### [~] T-06 账号命令与初始授权：主体已实现，真实账号待验收

- **2026-09-05 当前代码**：已有 `config add/auth/list`、隐藏输入/stdin 导入、离线迁移与账号校验；阿里/百度浏览器 OAuth、115 原生 PKCE 扫码流程及模拟 HTTP 测试已实现。凭据写入安全存储、重授权并发配置检查和旧 daemon 轮换保护已接入。`add` 目前使用命令行参数，不是交互式公开字段向导；真实授权、终端扫码与系统 keyring 仍待验收。以下保留历史差距，不能再将整个 T-06 视为未开始。

- **证据**：`cmd/cloudfs/main.go` 的 `cmdConfig` 只接受 `check` 一个子命令，
  其余一律返回 `config: expected 'check [path]'`。
- **文档依据**：DESIGN §3.8「CLI：`config add|auth`」。
- **影响**：这条是目前**最卡进度的一项**。各驱动内部都有 refresh_token 的刷新逻辑
  （`aliyun/aliyun.go`、`baidu/baidu.go`、`pan115/auth.go`、`tianyi/auth.go`），
  但**初始 token 要用户自己想办法弄到再手填进 YAML**。
  没有它，T-13 的真实账号验证就无从谈起。
- **做法**：
  - `cloudfs config add <name> --type <t>`：交互式问必填项，写回 YAML；
  - `cloudfs config auth <name>`：起本地回调 server 走 OAuth（阿里、百度、123、115 开放平台），
    cookie 型的（夸克）引导用户粘贴并立即校验一次 `List` 调用；
  - 拿到的凭据交给 T-05 的 Secrets 层，不落 YAML。
- **验收**：
  - 对一个真实账号跑 `config add` + `config auth` 后，`cloudfs doctor` 该 remote 全绿。
  - 授权流程中断（用户关掉浏览器）不留下半个配置项。
  - 与 T-05 联动：授权结果进 keyring，YAML 只有引用。

### [~] T-07 systemd / launchd 用户服务已实现，真机重启待验收

- **2026-09-05 当前代码**：新增 `cloudfs service install|uninstall|status`。
  Linux 原子写入 user systemd unit，包含 network-online 顺序、RequiresMountsFor、
  `Restart=on-failure` 并 enable --now；macOS 原子写入 LaunchAgent，包含
  RunAtLoad 与失败退出 KeepAlive，并用 bootstrap/bootout 管理。定义参数分别按
  systemd 和 XML 规则转义，不经过 shell。uninstall 先停止 supervisor，让 daemon
  在 SIGTERM 路径卸载，再检查并清理残留 FUSE 挂载，最后移除定义并 reload。
  生成器、manager 调用顺序、文件权限/原子替换、特殊路径、失败保留、status 和
  残留挂载均使用临时目录与假 manager 验证，不修改开发机真实 LaunchAgents。
- **仍缺**：本机无 macFUSE/Fuse-T，不能完成 reboot、kill -9 自动重启与真实挂载
  消失验收；Linux user manager/linger 也需目标发行版验证。因此状态保持部分完成。
- **文档依据**：M6 验收项「macFUSE 适配、launchd」。
- **影响**：没有开机自启，守护进程要手工拉起；异常退出后不会恢复，
  这与"持续高可靠读写"的目标直接冲突。
- **做法**：提供 `cloudfs service install|uninstall|status`，
  Linux 生成 user-level systemd unit（`Restart=on-failure`，
  `After=network-online.target`，挂载点用 `RequiresMountsFor`），
  macOS 生成 launchd plist（`KeepAlive`、`RunAtLoad`）。
  注意卸载时要先 umount，避免留下失效挂载点。
- **验收**：
  - install 后 reboot，挂载点自动恢复且上传队列继续消费。
  - `kill -9` 守护进程后服务自动重启，journal 恢复流程被执行。
  - uninstall 后不残留 unit 文件与挂载点。

---

## P2 — 已实现但与文档描述不符

### [~] T-08 FUSE passthrough：已接入，但非 root 下内核拒绝注册

- **2026-09-05 最新状态**：共享 backing 的缓存租约已改为随内核注册释放，并补注册失败、注销失败、连接断开和多句柄并发测试。发现读写交叠及旧/新版本 backing 的内核模式限制尚未解决；为防写入绕过 journal，默认停止自动启用，实验开关仅供隔离验收。**这不是 T-08 完成**，也不能用只读场景缩小原验收范围。具体约束、源码依据和待实现项见 `docs/fuse-passthrough.md`。以下为历史记录。

- **进展（2026-09-02）**：`internal/fusefs` 的 `file` 实现了 `PassthroughFd`，
  只在「只读句柄 + 文件已完整 hydrate + 无待上传本地写」时把缓存文件交给内核。
  实测发现 go-fuse 注册 backing fd 需要 `CAP_SYS_ADMIN`，普通用户下内核拒绝后
  go-fuse 会对整个挂载静默关闭 passthrough。`PassthroughAvailable` 现在同时检查
  内核版本与能力位，doctor 如实报告原因，不再声称已启用。
- **仍缺**：以 root 或 `setcap cap_sys_admin+ep` 运行时的真机验证（本机无 sudo）。
  测试 `TestPassthroughServesHydratedFilesWithoutReadRequests` 在无权限时跳过。

- **证据**：`internal/fusefs/platform_linux.go:41` 的 `PassthroughAvailable()`
  会正确判断内核 ≥ 6.9；`internal/control/doctor.go:117` 据此向用户报告
  "FUSE passthrough is available, so fully cached files read at local-disk speed"。
  但挂载层从未使用它——go-fuse v2.11 通过 `fs.FilePassthroughFder` 接口支持
  （`fs/bridge.go:780` 会检测并调用 `PassthroughFd()`），
  而 `internal/fusefs/fs.go` 的 `file` 类型没有实现这个接口。
- **影响**：hydrated 文件的读取仍然每次穿过 cloudfs 进程，
  速度与承诺不符；**doctor 那句提示目前是不准确的**，属于误导用户。
- **做法**：`file` 实现 `PassthroughFd() (int, bool)`：
  当且仅当该文件已完整 hydrate、当前句柄只读、且平台支持时，
  返回 hydrated 文件的 fd；有待上传的本地写入时必须返回 false，
  否则内核会绕过我们的写路径。macOS 恒返回 false
  （`platform_darwin.go:45` 已经说明 macFUSE 无此模式）。
- **验收**：
  - 内核 ≥6.9 上读一个已 hydrate 的文件，FUSE read opcode 计数为 0。
  - 文件有 pending 写入时不走 passthrough，读到的仍是本地最新内容。
  - 内核 <6.9 与 macOS 上行为与现在完全一致。
  - 在无法启用时，doctor 的措辞不再声称已启用。

### [~] T-09 名称／路径搜索：宽查询与索引积压已限界，真实规模长期吞吐待补

- **2026-09-06 限界与实测**：这条剩下的两项（"大量 pending 的长查询扫描""宽路径候选
  展开与排序"）都做了，且都是先量出来再改的。测量环境：200 个子目录 × 300 个文件
  = 60K 个节点，全部匹配 `wide/`。

  1. **索引积压不再每次查询都扫。** 长查询必须同时查 `name_index_pending`，否则刚
     列出来的文件会搜不到。原来这条 UNION 分支被 SQLite 规划成**全表扫 `nodes`**
     再按 rowid 探测 pending，于是每行都调用一次折叠函数。改了两处：`CROSS JOIN`
     钉住连接顺序（小表驱动），以及积压超过 2048 条时先 `FlushIndex` 再查——把
     "每次查询扫一遍积压"变成"合并一次"。窄锚点查询 46ms → 4.2ms，名字查询
     57ms → 19.6ms。只读库无法写入时回退到原来的扫描，结果仍然正确。
     1／2 字符查询走同步维护的短倒排索引，从不碰 pending，也不会触发合并（有回归）。
  2. **宽路径查询限界。** `expanded` 递归 CTE 原来用 `UNION`（去重），这会让 SQLite
     在外层读第一行之前就把整个展开物化，于是任何预算都拦不住它。改成 `UNION ALL`
     后递归可以惰性消费，预算才真正生效；重复节点由调用方按 ino 丢弃（只读到预算
     为止，代价很小）。预算按 limit 缩放（20×，下限 2000、上限 20000）。
     60K 节点的 `wide/` 查询：无预算 458ms 且随子树线性增长 → 有预算 103ms 且
     与子树大小无关。
  3. **不完整的答案会说自己不完整。** 新增 `SearchReport{Results, Complete}`，
     `count(*) OVER ()` 报告预算内收集到的行数，命中预算即 `Complete=false`。
     MCP 的 `search` 置 `truncated` 并给出说明，CLI `find` 打印一行提示。
     "没有更多匹配"和"我们停止查找了"是两句不同的话，之前分不出来。
  4. 新增 4 个回归：宽查询限界且结果无重复、普通查询与空结果仍报告完整、
     积压被合并且合并后新写入的名字仍可见、短查询不碰也不合并积压。
- **仍缺**：真实目录深度／规模下的长期吞吐，以及 MCP 内容搜索的完整内容索引
  （现在仍是缓存前缀 + 候选预算内的检查）。

- **2026-09-05 当前实现**：补回缺失的 `Store.Search`，增加 `SearchWithin`，MCP
  按权限与子树过滤后再排序限量，不再用超采样猜测可见结果。schema v8 用事务触发器
  维护 1／2 字符倒排索引；升级回填短索引并修复旧名称镜像不一致。目录改名时只改
  本节点，查询按父链在同一 SQLite 快照重建路径，支持 `work/src` 等字面片段。
- **回归范围**：Unicode／标点字面匹配、旧 pending 不覆盖新名称、迁移、事务回滚、
  并发改名快照、权限边界及十万节点短查询计划。FTS 的历史 `path` 列继续留空，
  不再被当成必须物化的目标；短子串匹配不能用词首 prefix 查询替代。
- **仍需验证／优化**：大量 pending 的长查询扫描、宽路径候选展开与排序、真实
  目录深度／规模与长期吞吐。MCP 内容搜索仍是缓存前缀及候选预算内的检查，不是
  完整内容索引。以下为历史差距记录，不表示查询入口仍缺失。

- **证据**：`internal/meta/store.go:455`
  `INSERT INTO name_index (name, path, ino) VALUES (?, '', ?)`——
  `internal/meta/schema.go:59` 的虚拟表定义了 path 列，但从来没有真实值写入。
  另外 `store.go:809-815`：查询串短于 3 个字符时退化为
  `SELECT ... FROM nodes WHERE name LIKE '%q%'`，是全表扫描。
- **影响**：按路径片段搜索（"找 `work/src` 下的所有 `.go`"）无效；
  短查询在大目录树上会明显变慢。
- **做法**：写入时物化完整路径（rename/move 时需要级联更新子树，
  可以只在 FTS 表里重建受影响子树的行，代价可接受）；
  或者改用 ino 链在查询期拼路径，避免级联写放大——**两种方案需要先量一下代价再定**。
  短查询采用独立 1／2 字符倒排索引；FTS 词首前缀不等于任意位置子串。
- **验收**：
  - `search("work/src")` 能命中路径中含该片段的文件。
  - 目录改名后，其子树的搜索结果路径立刻正确。
  - 2 字符查询在 10 万节点的库上不做全表扫描（用 `EXPLAIN QUERY PLAN` 断言）。

### [~] T-10 writeback_cache 与 splice 未启用

- **2026-09-05 核对**：不能把 writeback_cache 与 passthrough 视作可同时打开的独立开关，内核初始化限制这两种能力组合。go-fuse 也会自动探测部分 splice 能力，需检查真正的协商与数据路径，而非仅 grep 挂载选项。此项保持待验收。

- **证据**：`internal/fusefs/fs.go:66-72` 的 `MountOptions` 只设了
  `FsName / Name / Debug / DisableXAttrs / EnableLocks`；
  `platform_linux.go` 补了 `MaxWrite = 1MiB`、`MaxReadAhead = 1MiB`、
  `ExplicitDataCacheControl = false`。文档列的 writeback_cache 与 splice 两项没有开。
- **文档依据**：DESIGN §3.6「启用 readdirplus、writeback_cache、splice、max_write=1MiB」。
- **影响**：小块写会以更多次 FUSE 往返到达用户态，写吞吐低于应有水平。
- **注意**：writeback_cache 会改变内核回写 attr 的时机，
  与我们现在依赖 FLUSH 提交的路径有交互（历史上这里踩过两次坑：
  提交挂在 RELEASE、以及 FLUSH 多次到达）。**改之前先补一组针对性的回归测试**，
  确认 `pendingSize` 与 close-to-open 语义不受影响。
- **验收**：
  - 现有 fusefs、conformance、chaos 三套测试在开启后全绿。
  - 4 KiB 粒度顺序写 1 MiB，FUSE write opcode 次数较现状下降。
  - 写后立即读（read-your-writes）仍然返回正确内容与大小。

---

## 验证缺口（需要外部资源或长时间运行）

### [ ] T-11 51 处 `UNVERIFIED` 待真实账号核对

按协议资料推断、未在真实账号上跑通的细节。2026-09-05 非测试源码计数为 51，
不能在实际核验前笼统断言这些未确认行为只影响可用性、绝不影响数据正确性。

| 驱动 | 处数 | 集中位置 |
|---|---:|---|
| tianyi | 17 | `tianyi.go` 5 · `upload.go` 3 · `files.go` 3 · `auth.go` 3 · `api.go` 3 |
| quark | 12 | `upload.go` 4 · `files.go` 4 · `api.go` 3 · `quark.go` 1 |
| pan115 | 13 | `upload.go` 4 · `pan115.go` 3 · `api.go` 3 · `auth.go` 2 · `oss.go` 1 |
| aliyun | 4 | `aliyun.go` 4 |
| baidu | 2 | `baidu.go` 2 |
| pan123 | 3 | `pan123.go` 3 |

**建议顺序**：从待确认项最少的 baidu(2) 与 pan123(3) 开始，
两者都有官方开放平台文档，核对成本最低；tianyi 与 quark 放到最后。
**前置依赖**：T-06（没有授权流程就拿不到 token）。
**验收**：每核对一处，把 `UNVERIFIED` 注释替换为实际观测到的行为描述，
并补一个基于录制 fixture 的回归测试，使之后不必再连真实账号。

### [~] T-12 pjdfstest / fsx / fio：已在真实 SFTP 挂载上运行，root 项待补

- **进展（2026-09-02）**：fio 3.38 从源码编译（`scripts/bench/build-fio.sh`），
  job 文件与内置负载一一对应，`cross-check.py` 并排比对；pjdfstest 子集
  （open/mkdir/rmdir/unlink/rename/truncate/ftruncate/chmod/symlink/link/utimensat，
  180 个文件、6967 个断言）在 SFTP 挂载上跑完：2407 通过、4560 失败。失败按
  `scripts/bench/pjd-summary.py` 分类后，绝大多数是**非 root 下无法切换 uid**（`-u 65534`）
  的用例及其级联（rename/09.t、10.t 两个文件就占 3043 条），其余是文档化的差异
  （无硬链接、无符号链接、无 mkfifo/mknod、无 POSIX 权限位）。密闭复现
  `TestPathThroughAFileIsENOTDIR` 证实「经过普通文件的路径应报 ENOTDIR」在本实现上
  是对的，日志里的该项失败是前置 `-u` 步骤失败的级联。
- **仍缺**：以 root 跑一遍 pjdfstest 得到干净的通过/跳过清单；fsx 100 万次结果见 `docs/bench.md`。

- **现状**：`test/conformance` 是自研的"与本地目录逐项对比"测试，覆盖不到 POSIX 边界。
- **文档依据**：DESIGN §7.2「pjdfstest 子集 + fsx + fio 随机读写在 fakeprovider 挂载上跑」。
- **未覆盖的具体语义**：O_APPEND 并发追加的原子性、稀疏文件与空洞、
  hardlink、mmap 写回、rename 覆盖时的 inode 语义边界。
- **验收**：pjdfstest 子集的通过/跳过清单落到 `docs/` 里，
  每个跳过项写明为什么云盘语义下不适用；fsx 随机读写跑满 100 万次操作无差异。

### [ ] T-13 真实网盘冒烟测试为零

- **文档依据**：DESIGN §7.6「每个 Provider 一个小账号，上传/秒传/断点续传/下载/
  重命名/删除；国内网盘按官方限速跑 30 分钟不触发风控」。
- **前置依赖**：T-06。
- **验收**：每个 provider 一份冒烟记录，含 30 分钟持续操作的 QPS 曲线与
  风控事件计数（应为 0）。

### [ ] T-14 三项真实性能基线未测

- **现状**：现有的 `test/perf` 断言的是 fakeprovider 上的**远端调用次数**
  （冷遍历 500 文件 271 次、热遍历 0 次、1 MiB 按 4 KiB 读 256 次只发 16 个请求）。
  这套指标防回归很有效，但它是代用指标，不是文档要的真实基线。
- **文档依据**：DESIGN §7.4「冷/热 `find` 10 万文件、顺序读 1 GiB、
  `git status` 于 pin 目录；记录远端调用次数与缓存命中率」。
- **验收**：三项各有一条基线数字记录在 `docs/` 中，注明硬件与网络条件。

### [ ] T-15 macOS 未上真机验证

- **现状**：`internal/fusefs/platform_darwin.go` 已按 macFUSE 的限制写好
  （更小的 max_write、volname、noappledouble），但没有在真机上跑过任何一个测试。
- **验收**：完整测试套件在装了 macFUSE 的 macOS 上跑通，
  差异项记录在 `docs/` 中。

---

## P3 — 采用缺口（能力已具备，但用户接触不到）

前面 P0–P2 与"验证缺口"都是**对着设计文档**核对出来的。这一节是**对着竞品**核对出来的：
这些条目不是功能缺陷，代码质量也没问题，但它们决定了这个项目是"零用户"还是"有用户"。
核对时间：2026-09-02。核对方式：竞品调研 + 全仓库 grep。

### 竞品的卖点与盈利点（判断依据，不是待办）

| 产品 | 形态 | 用户为什么用 | 钱从哪来 |
|---|---|---|---|
| rclone | CLI，MIT | 70+ 后端、可脚本化、免费 | 不盈利，靠捐赠；被第三方 GUI 变现 |
| OpenList（AList 分叉） | HTTP 网关，AGPL | 国内驱动最全、WebDAV 输出、在线播放、一键部署 | 不盈利。AList 原作者的变现路径是**把项目卖给商业公司**，社区因此分叉 |
| CloudDrive2 | 闭源客户端 | 真·本地盘、多盘聚合、**跨盘秒传复制**、Emby 直连 | freemium：免费版限 1–2 个挂载，Pro 订阅／终身约 299–499 元 |
| Mountain Duck | 闭源桌面 | Finder/Explorer 原生、Cryptomator 加密 | 一次性 $39，每个大版本再收一次 |
| ExpanDrive | 闭源桌面 | 企业后端齐（SharePoint/S3/Box） | 个人免费引流，团队 $29/月（≤10 人） |
| RaiDrive / NetDrive | 闭源 Windows | 直接映射盘符 | freemium 订阅 |
| MultCloud / odrive | SaaS | 云到云搬运不占本地带宽 | 订阅 + 流量包 |
| JuiceFS | 开源核心 | POSIX + 对象存储 | 开源引流，企业版卖缓存组、多写、SLA |

**结论**：这个品类的付费理由只有四条——省钱（替代买硬盘扩容）、省事（GUI 三步接入）、
不出事（不封号／不丢数据／不写满磁盘）、能播（媒体库直连）。
CloudFS 在"不出事"这一条上**已经明显强于所有竞品**（三维 AIMD 限流 + 熔断、写日志 +
崩溃恢复 + 冲突副本、四重限额 + `ENOSPC` 背压、持久化元数据），
在"agent 可安全读写"这一条上是**全品类空白**。
但这些优势目前全部锁在一个装不上的产品里，所以本节按"能装上 → 敢长用 → 想留下"排序。

---

### [ ] T-16 仓库无版本控制、无许可证声明

- **证据**：仓库根 `ls -a` 无 `.git/`、无 `LICENSE`、无 `COPYING`；README 未提许可。
  `docs/DESIGN.md:40` 写的是"项目自身许可待定"。
- **竞品依据**：AList 作者把项目卖给商业公司后社区分叉出 OpenList，这件事让国内这批用户
  对"会不会变味"极度敏感。**一个干净的、不含 AGPL 血统的实现 + 公开的许可与治理承诺，
  本身就是获客材料**——而这恰好是本项目已经付出成本换来的（国内驱动全部按协议自研）。
- **影响**：没有版本控制就没有 CI、没有 release、没有外部贡献，T-18 无从谈起；
  没有许可证，任何公司和 NAS 厂商都不能用，个人用户也无法判断是否可以二次分发。
- **做法**：
  - `git init` + `.gitignore`（至少排除 `cloudfs` 二进制、`gow` 指向的 scratchpad 路径）；首次提交现有全量代码。
  - 定许可：想最大化采用选 **Apache-2.0**（专利授权条款对企业友好）；
    想防止闭源分叉选 AGPL-3.0（但会挡住企业用户，且与"不移植 AGPL 代码"的初衷无关，别混淆）。
    在 `docs/DESIGN.md:40` 把"待定"改成结论。
  - 写一份 `GOVERNANCE.md` 或在 README 里用三行说明：谁维护、会不会变闭源、驱动为何全自研。
- **验收**：
  - 仓库根存在 `LICENSE`，README 顶部有徽章或一行声明，`docs/DESIGN.md` 不再写"待定"。
  - `git log` 有初始提交，`git status` 干净（构建产物已被忽略）。
  - README 能回答"这个项目会不会像 AList 一样被卖掉"。

---

### [~] T-17 图形控制面

- **证据**：`internal/control/metrics.go:24-27` 只注册了 `/healthz` `/readyz` `/status`
  `/metrics` 四个 JSON/文本端点；全仓库 `grep 'http.FileServer\|embed.FS'` 零命中，
  无 HTML 模板、无静态资源。所有操作只有 CLI（`cmd/cloudfs/main.go:56-86` 的子命令表）。
- **竞品依据**：CloudDrive2 / RaiDrive / Mountain Duck / ExpanDrive 全部是 GUI 优先；
  OpenList 的一键部署 + Web 管理页是它取代 AList 的主要抓手。目标用户是 NAS 玩家，
  不是 Go 开发者。
- **影响**：这是第一道墙。即使 T-06 做完，用户仍要 SSH 进机器编辑 YAML 才能加一个网盘。
- **做法**：不要另起进程。复用 `internal/control` 已有的 `s.mux`，用 `embed.FS` 挂一套
  静态页面到 `/`，数据全部走已有的 `/status` JSON：
  - 只读优先：缓存用量、上传队列与死信、限流与熔断状态、各 remote 的调用计数
    （`daemon.CallStats` 已经在统计）、代理出口健康。
  - 写操作只做两件高频的：加账号（跳 T-06 的授权流）、重试/丢弃某个上传。
  - 绑定与 `control.metrics` 相同的回环约束，非回环地址必须带 token（照抄
    `internal/mcpsrv/http.go:17-19` 已有的判断）。
- **验收**：
  - `cloudfs mount` 后浏览器打开 `http://127.0.0.1:9101/` 能看到挂载状态，无需读日志。
  - 页面数据全部来自现有 `/status`，没有为 UI 新增绕过 VFS 的数据通路。
  - 非回环绑定且未配 token 时拒绝启动，与 MCP HTTP 行为一致。
  - 关闭 UI（配置项）后，`/status` `/metrics` 行为完全不变。

**当前实现（2026-09-05）**：`control.ui` 默认开启，在既有 `control.metrics` 回环监听的
`/` 提供内嵌只读状态页；设为 false 后根路由恢复 404，`/status`、`/metrics` 和 Unix
socket 不变。页面只读取 `/status`，展示缓存、上传、元数据、挂载、远端调用/熔断、代理
与警告，动态值全部通过 `textContent` 写入，并设置 CSP、no-store、nosniff、拒绝 frame
与非 GET/HEAD。上传 dead retry、pending/uploading cancel、cancelled resume/drop 复用既有
control API；resume 有风险确认，drop 必须重新输入完整 ID。浏览器 mutation 只新增精确
same-origin 许可，跨站 Origin、DNS rebinding Host、cross-site fetch 和无专用 header 的
请求仍拒绝。相关回归五轮 race 通过，HTML tidy 无错误；本地浏览器无可用实例，未做
截图级视觉验收。Web 加账号/授权入口尚未实现，因此保持部分完成。

---

### [~] T-18 分发物：Docker 镜像、CI、release、包管理器

- **证据**：仓库根无 `Dockerfile`、无 `.github/`、无 `Makefile`、无 `.goreleaser.yml`。
  README 的安装方式是 `go build -o cloudfs ./cmd/cloudfs`，前提是用户先装 Go。
- **竞品依据**：NAS 用户安装 OpenList / CloudDrive2 的实际方式就是 Docker（群晖套件、
  compose 一行）。缺这一项等于零触达。
- **关联**：T-07（systemd / launchd 服务单元）是同一件事的另一半，建议合并排期。
- **做法**：
  - 多阶段 `Dockerfile`（distroless 或 alpine），容器内挂载需 `--device /dev/fuse`
    `--cap-add SYS_ADMIN` `--security-opt apparmor:unconfined`，在 README 里写清楚；
    同时说明**不挂载也能用**——MCP 直连 VFS（`docs/mcp.md` 已有这个论点，但没人看得到）。
  - `docker-compose.yml` 示例：配置目录、缓存目录、MCP 端口、control 端口。
  - GitHub Actions：`vet` + `test -race` + 三平台交叉编译 + checksum + 附件发布。
  - 有了 release 二进制再谈 Homebrew tap / scoop，不要反过来。
- **验收**：
  - `docker run` 一条命令 + 一个配置文件即可跑起 MCP-only 模式（无 FUSE 权限也能用）。
  - 打了 tag 之后 CI 自动产出 linux/amd64、linux/arm64、darwin/arm64 三个二进制与校验和。
  - CI 里 `go vet` 与 `go test -race ./...` 是必过项（当前基线本来就是干净的，别让它退化）。
  - 镜像内 `cloudfs doctor` 能正确报告 FUSE 是否可用，而不是直接崩。

**当前实现（2026-09-05）**：已增加无 CGO 多阶段 `Dockerfile`、MCP-only 与 Linux
FUSE 两个互斥 Compose profile、Makefile、四平台静态发布脚本、Linux/macOS race CI、
tag 附件/checksum release 及 GHCR 双架构镜像发布。容器非回环 MCP 新增只从
`CLOUDFS_MCP_TOKEN` 读取的 bearer token，缺失时仍 fail closed；修复了 `--http addr`
曾把地址误当位置参数的问题。四平台本地构建和版本注入已验证，Compose 配置已由 Docker
CLI 解析；本机 Docker daemon 未运行，镜像构建/doctor、GitHub-hosted CI、实际 tag 和
GHCR 发布仍需外部环境验收。Homebrew/NAS 包必须等 T-16 的仓库与许可证决策后再做，
因此本项保持部分完成。详见 `docs/distribution.md`。

---

### [~] T-19 WebDAV / 直链输出：可写模式已补齐，真实客户端待验收

- **证据**：`internal/provider/webdav` 是**客户端**；全仓库非驱动代码 `grep PROPFIND` 零命中。
  对外只有两个面：FUSE 挂载，和 MCP（`internal/mcpsrv/http.go` 是 MCP 传输，不是文件服务）。
  直链能力其实已经有了，但只暴露给 agent（`internal/mcpsrv/server.go:415` 的
  `get_download_url`，且已经带上了 `Caps.LinkHeaders`）。
- **竞品依据**：WebDAV 输出是 AList/OpenList 生态的事实互通标准，也是 Emby / Jellyfin /
  Infuse / 各类播放器接入的默认方式。OpenList 的三种策略（302 直链 / 代理地址 / 本地代理）
  在 `docs/DESIGN.md` §2 已经分析过，但没有落成实现。
- **影响**：不能挂载 FUSE 的场景（容器无权限、NAS 上的第三方 App、手机播放器）
  目前只能走 MCP，而这些客户端都不说 MCP。整条已经写好的缓存与限流链路对它们不可用。
- **做法**：新增 `internal/davsrv`，与 `mcpsrv` 平级，同样是 VFS 的薄适配器（逻辑不下沉到它）：
  - `PROPFIND` / `GET`（含 Range）/ `PUT` / `MKCOL` / `MOVE` / `DELETE` 映射到 `vfs`；
  - 三种下载策略可配：`proxy`（字节过本进程，走块缓存）、`redirect`（302 到 provider 直链，
    带不了自定义头的客户端不可用，要在文档里点明）、`auto`（能带头就 302，否则代理）；
  - 复用 mcpsrv 的白名单与只读模式语义，不要再实现一套；
  - 认证沿用 control/MCP 的 token 模型，默认只绑回环。
- **验收**：
  - `rclone lsd webdav://127.0.0.1:port` 与 `cadaver` 能正常列目录、断点续传下载。
  - Emby/Jellyfin 添加 WebDAV 媒体库后能扫描并播放，跳播（Range 请求）不触发整文件下载
    ——用 `test/perf` 的调用计数方式断言，不是靠肉眼看。
  - `redirect` 策略下，provider 要求特定 UA 时（百度 >20 MB）自动降级为 `proxy` 而不是给出 403 的链接。
  - 只读模式下所有写方法返回 403，白名单外路径返回 404 而不是 403（不泄露存在性）。

**可写模式补充（2026-09-06）**：`webdav.writable`（默认 false）打开 PUT / DELETE /
MKCOL / MOVE / COPY / PROPPATCH / LOCK / UNLOCK，OPTIONS 公布对应方法并在真的提供锁时
才声明 `DAV: 1, 2`。写入复用 FUSE 的同一条 VFS 路径（Create/Write/Truncate/Release，
提交在 Release），只读挂载上的写被拒绝。四处刻意偏离 `x/net/webdav` 默认行为，都是为了
不给错误答案，且每条都有回归：

1. **COPY 交给 `vfs.Copy`**，不让 DAV 库下载再上传——那正是 §3.4 的跨盘秒传要避免的。
   目录 COPY 明确 403（`vfs.Copy` 是文件原语，递归不该写在适配层）。单测断言
   `copies==1 && streamed==0`；e2e 断言复制一个已缓存文件产生 0 次后端下载。
2. **PUT 不返回 ETag**：DAV 库在提交前就取 ETag，拿到的是旧内容的校验符。回归断言
   PUT 响应无 ETag、提交后的 GET 有 ETag。
3. **只读挂载上的写返回 403**：DAV 库把 OpenFile 的任何错误压成 404、RemoveAll 的任何
   错误压成 405。适配层记录「因策略被拒」并在写响应头时改正，处理器与锁执行不变。
4. **MOVE 目标越界 403、跨主机 502**，而非 404：出问题的是目标不是源。

导出根不可删/不可改名（它在该命名空间里没有父目录）。XML 体限 64 KiB，PUT 体不受此限。
新增 21 个单测 + 1 个不挂 FUSE 的端到端（PUT→队列→网盘→读回、MKCOL、MOVE、COPY、
递归 DELETE）。**仍缺**：Finder / Explorer / rclone / cadaver / 媒体客户端的真实写入
验收，配额与大文件长写，以及 TLS 反代下的行为。因此 T-19 保持部分完成。

**当前实现（2026-09-05）**：新增 `internal/webdavsrv`，把单一规范 VFS 子树投影为 DAV
根目录；PROPFIND Depth 0/1、GET/HEAD、Range、条件 ETag 全部经 VFS 的 Stat/ReadDir/Open/
Read/Release，未旁路 provider/cache。写方法统一 405，OPTIONS 只公布只读方法；拒绝
Depth infinity、64 KiB 以上属性体、路径逃逸和反斜杠。ETag 哈希 remote/version，不泄露
opaque provider 标识。回环可无认证，非回环在 bind 前强制至少 16 字节的
`CLOUDFS_WEBDAV_TOKEN`，支持 bearer 或 Basic 用户 `cloudfs`；token 不进 YAML/argv。
与 mount、MCP-only owner、Docker 8080 映射已接线。HTTP 回放五轮 race 覆盖认证、Range、
PROPFIND、只读、root confinement 和句柄释放；无 FUSE 的真实 daemon→VFS→fake provider
端到端五轮 race 验证首读走 provider、热读零远端调用。proxy/redirect/auto 已接入：只对
无额外 header 的安全 http(s) shareable link 返回 no-store 302，auto 遇 UA/Referer 约束、
本地版本、错误或非法 URL 回落 VFS proxy，强制 redirect 则 502。仍缺真实 rclone/cadaver/
媒体客户端、TLS/NAS、大文件长播和完整可写 DAV，因此保持部分完成。详见
`docs/webdav-output.md`。

---

### [~] T-20 媒体库场景只完成 STRM 入口

- **原始证据**：最初全仓库没有 STRM/Emby/Jellyfin/M3U 入口。
- **竞品依据**：国内这个品类过半的实际用途是影音库（"给 NAS 扩容 100T"就是这个叙事）。
  CloudDrive2 的核心卖点之一是 Emby 直连流畅。
- **影响**：`pin` / `warm` / readahead / hydrate 这些已经做好的能力，
  在最大的实际场景里没有对应的入口和默认策略。
- **做法**（依赖 T-19，先做那个）：
  - STRM 生成：`cloudfs strm <mount-path> --out <dir>`，为每个媒体文件写一个指向
    WebDAV/直链的 `.strm`，让刮削器不必走 FUSE。增量更新复用 `vfs/refresh.go` 的 delta。
  - 媒体友好的预取策略：大文件顺序读时放大 readahead 窗口、跳播时不填充中间块
    （避免把整部电影拉进缓存又被 2Q 立刻淘汰）。
  - 目录层面的 `dir_ttl` 预设（媒体库 24h，见 README 的配置示例），并让 `warm` 支持
    "只预热目录结构不下载内容"。
- **验收**：
  - 对一个 1000 部影片的目录生成 STRM 后，Emby 扫描全程的 provider 调用次数有上界断言。
  - 跳播到文件 80% 位置，只下载该处附近的块，缓存占用与顺序播放同一部片子相比不超过 X%。
  - 媒体目录设 `dir_ttl: 24h` 后，重复扫描零远端调用（`test/perf` 风格断言）。

**当前实现（2026-09-05）**：`cloudfs strm <virtual-path> --out <dir>` 已通过运行中的
只读 WebDAV 做 Depth-1 递归枚举，不打开第二个 daemon/journal/provider。扩展名、深度、
文件数和单响应大小有界；只接受同源、起始根内的直接子项；输出逐层拒绝 symlink，采用
fsync + rename 原子写，重复内容不改写，大小写不敏感的目标碰撞失败。默认 URL 不含 token，
显式 `--embed-basic-auth` 才嵌入并告警。单测覆盖递归、认证、URL 转义、幂等、越根响应、
碰撞、上限和 symlink 逃逸。VFS 顺序段预读现在会在 seek/close 时取消；80% 跳播回归断言
只传输新旧位置附近块，不填充中间区域，前台撞上被取消的同块 flight 会自行重试。
显式 `--prune` 以同源 manifest 和内容摘要为删除凭证，首次只建基线，用户修改项保留并
转为非托管，拒绝 symlink/换源/损坏记录；默认仍零删除。尚未用真实 Emby/Jellyfin/Infuse
或千片真实 provider 验收，因此保持部分完成。详见
`docs/strm.md`。

---

### [ ] T-21 Windows 缺席的采用代价（登记，不改范围）

- **状态**：本条**不是新发现的遗漏**。`internal/fusefs/` 只有 `platform_linux.go` 与
  `platform_darwin.go`，WinFsp 适配在《明确不在当前范围内》里已列为二期，接口已预留。
  这里登记的只是它的**采用代价**，是否提前由排期决定。
- **竞品依据**：RaiDrive / NetDrive 是纯 Windows 产品，CloudDrive2 的主力用户也在 Windows。
  放弃 Windows 等于放弃最大的桌面市场。
- **判断**：如果目标用户是 NAS / 自建服务（Docker + WebDAV 输出），Windows 可以继续押后，
  T-19 反而更重要——Windows 用户可以用 WebDAV 映射盘符。
  如果目标是桌面用户，则 WinFsp 必须提前到 T-18 之后。
- **验收**：无（这是一个排期决策，不是一项实现）。在决定之后把本条改成 `[x]` 并写下结论。

---

## 明确不在当前范围内

以下是设计文档中标注为二期或预留的部分，列在这里是为了避免被误当作遗漏：

- **Windows / WinFsp 适配**：接口已预留（平台相关代码都在 `platform_*.go`），未实现。
- **macOS File Provider / FSKit 原生集成**：一期以 macFUSE 为准，FSKit 仅作试验开关。
- **OpenList 代码移植**：出于 AGPL 许可考虑，国内驱动只参考协议细节自行实现。
  当前 `openlist` 类型是 WebDAV 实现的别名（`internal/provider/webdav/webdav.go:505`），
  作为兜底通路，能力矩阵标记为无 hash / 无 delta。

---

## 推荐推进顺序

### 2026-09-05 缓存管理开发进展

- **[x] pin/unpin 生命周期与入口**：VFS 保存路径规则后下载，空间不足不假报成功，等待缓存后台写入后检查完整性。恢复用户防淘汰标记、补齐中断下载、处理重叠规则/多路径别名/改名/上传落地，且与待上传数据的临时 pin 独立。
- **[x] 在线缓存管理**：CLI pin/unpin/warm/cache stats/gc/pins 使用同一 daemon；MCP 增加 unpin；控制端点拒绝浏览器来源和请求重放。离线调用先检查存储所有权，不启动上传或后台刷新。
- **[~] 完整文件预算与 GC**：已实现统一计量、硬链接去重、完整文件淘汰、用户态打开租约、临时副本预留及重启临时文件回收。并发淘汰后的后台写入不再重新发布旧块，未清理的在途副本继续计量；passthrough 多句柄生命周期与真机验收仍未完成。
- **[x] 日志写入空间准入**：daemon 将 staging 写入/扩容接到共享预留器，并发缓存/日志不能重复使用同一份准入空间；空间检查失败拒绝写入，底层 ENOSPC 保留到 FUSE。覆盖写按字节保守申请，缩小已有 staging 不受阻；部分失败写入同步更新实际长度/哈希。此机制不是文件系统硬配额，不能约束其他进程的写入。
- **[ ] 大规模 pin 性能和真机验证**：规则恢复/改名目前遍历缓存 key 并查本地元数据，需在大缓存、多别名场景测量；macOS/Linux 内核挂载仍待环境验收。详细行为见 `docs/cache-management.md`。

### 原阶段计划（需结合以上进展使用）

按"能装上 → 敢长用 → 想留下 → 扩边界"分阶段。P0–P2 是**对着设计文档**核对出的缺口，
P3 是**对着竞品**核对出的缺口；两者交错推进，因为前者决定质量、后者决定有没有人用。

**阶段 0 — 能装上**（当前所有技术优势都锁在一个装不上的产品里，这一阶段不动核心）

1. **T-16**（版本控制 + 许可证：一切协作、CI、发布的前提）
2. **T-01**（配置谎报，改动最小，顺手清掉）
3. **T-06 → T-05**（授权流程与凭据存储；没有它任何人装不上，也是 T-11 / T-13 的前置）
4. **T-18 + T-07**（Docker 镜像 / CI / release / systemd / launchd，同一件事的两半）
5. **T-17**（最小 Web UI，只读优先）

**阶段 1 — 敢长期用**

6. **T-08 / T-09 / T-10**（承诺与实现不符，纯本地工作，不需要外部资源）
7. **T-11 → T-13**（拿到真实账号之后：核对 `UNVERIFIED`，跑真实网盘冒烟）
8. **T-12 / T-14 / T-15**（pjdfstest / fsx / fio、性能基线、macOS 真机）

**阶段 2 — 想留下**

9. **T-04**（跨盘复制/秒传：底层去重能力已经写好，只差接出来，性价比最高）
10. **T-19**（对外 WebDAV / 直链输出：进入 Emby / Jellyfin / OpenList 生态的唯一通路）
11. **T-20**（媒体库场景：STRM、跳播友好的预取；依赖 T-19）

**阶段 3 — 扩边界**

12. ~~**T-02**（境外网盘）~~ —— 2026-09-06 已按各自官方接口自研补齐 gdrive / box / smb，未引入 rclone，剩下的只是真实账号与真机验收（并入 T-13）。
13. **T-03**（MCP Resources；"agent 可安全读写的云盘"是全品类空白，也是唯一不与
    CloudDrive2 正面拼价格的差异点）
14. **T-21**（Windows / WinFsp：排期决策，取决于目标用户是 NAS 还是桌面）

**不建议做的事**：靠限制挂载数量收费（CloudDrive2 的 freemium 模式）。
本项目的口碑点在可靠性——限流防封号、写日志不丢数据、背压不写满磁盘——
用阉割数量来收费会把最强的那一项浪费掉。若要变现，JuiceFS 式的开源核心
（单机免费，团队/多节点共享缓存、审计、集中授权收费）与现有架构更相容。
