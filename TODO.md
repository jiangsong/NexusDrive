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

## 上手路径（2026-09-09）

### [x] T-27 第一次使用走不通：无配置进不了控制台，Dropbox 没有浏览器授权（2026-09-09）

四个网盘账号合成一个池、挂到一个目录，这条路上的门槛逐条清掉。分两期做完，全部带测试。

**一期 — 让路径可行**

- **Dropbox 浏览器授权。** `daemon.OAuthProfile` 加 `PKCE` 字段并在 `Apply` 透传；新增
  dropbox profile（`token_access_type=offline`，PKCE 公共客户端，scope 按驱动实际调用取）。
  `internal/auth/oauth.go` 本来就实现了 PKCE，只是 profile 没有透出这个字段。
  `SupportsDaemonAuth("dropbox")` 由 profile 推导，Web UI 的浏览器流随之打开。
  退掉 `config_manage.go` 单字段回退表里所有有 profile 的类型 —— dropbox 那条收的是
  几小时就过期的 access token，正是要修的缺陷本身。
- **三处只有无密钥 profile 才会暴露的 guard。** `oauthOptions` 与 `browserAuthorize` 在
  secret 为空时报错/提示；更要命的是两处都无条件把 `client_secret` 放进要保存的 map，
  而 `saveCredentials` 拒绝空值（`internal/config/edit.go:218`）—— PKCE 授权会先对着
  Dropbox 成功，再死在保存这一步，报一个用户从没填过的字段。
  回归测试 `TestAPublicClientAuthorizationSavesOnlyTheRefreshToken` 专抓这条。
- **内置 OAuth 应用 + 用户覆盖。** `internal/daemon/oauth_apps.go`：`BuiltinOAuthAppFor` 与
  `ResolveOAuthClient`，账号自己的 `client_id` 完胜内置；内置的 id 与 secret 成对使用。
  **表先留空**，填值是维护者一行编辑，空表时行为与之前完全一致（有测试断言）。
  新建账号时把内置 id 落进 YAML（`FillBuiltinClientID`），因为 `client_id` 在
  `EffectiveAccountBinding` 的哈希里，事后再填会移动 binding、栅栏在途上传。
- **Dropbox 配额。** 驱动实现 `provider.Quotaer`（`/2/users/get_space_usage`），team 空间
  形状读不了就报未知、不猜。混合池里 Dropbox 成员不再因为"空间未知"而在放置时垫底。
- **加账号与入池合成一个事务。** `AddRemoteOptions.Pool` + 抽出的 `appendPoolMember`，
  一次 `editConfig` 写完；`internal/control/accounts.go` 的两段式与那句
  `remote written, but joining the pool failed` 一并删掉。CLI 补上 `config add --pool`。
- **授权端口全局守卫。** `authRegistry` 只按 remote 记会话，两个不同账号同时授权撞
  53682 端口，压成一个莫名其妙的 502。加 `inFlight` 与 `err.auth_port_busy`（中英）。
- **副本诚实性。** `docs/pool.md` 三处把 `min_replicas` 说成 `close()` 前必须落地的份数 ——
  代码里没有任何写路径消费它。改文档，`pool.js` 无条件渲染 `pool.protect.async`：
  新文件先写到一个网盘就算保存成功，其余由后台补齐。**行为不变，是措辞在撒谎。**

**二期 — 引导流程**

- **`cloudfs setup`**（`cmd/cloudfs/setup.go`）。真正的第一堵墙是 `loadConfig`
  （`main.go:269`）在没有配置时让人去读 `docs/DESIGN.md` 第 6 节 —— 而所有引导都在
  Web UI 里，Web UI 要守护进程先跑，守护进程要配置先存在。setup 写一份起步配置
  （`config.WriteStarter`，刻意不放占位 mount），起一个**只有控制面**的 server
  （不挂 FUSE、不占 journal 锁），打开浏览器；重启时 re-exec 成 `cloudfs mount`。
- **服务端四个字段**：`AccountSummary.has_credentials`（续做时知道还差哪个）、
  `AccountType.browser_auth`（哪些能点浏览器，由守护进程回答而不是页面里存一份名单）、
  `AccountCheckResponse` 的 `total/used/free`（授权完当场显示剩余空间）、
  `PoolCreateRequest.member_capacity`（不报配额的成员当场给个容量）。
- **前端**：`auth_step.js` 把授权那段从 `add_drive.js` 抽出共用；`setup_plan.js`
  （零 import 纯逻辑，20 个 node 测试）从服务端状态推断该停在第几步 ——
  localStorage 只存意图，真相一律来自 `/accounts`、`/pool/status`、`/mounts`，
  所以"关了浏览器"、"守护进程重启了"、"有人在终端 config add"是同一种情况；
  `screens/setup.js` 五步界面，**一次只offer一个网盘授权**（端口是进程级的锁）。
- **文档**：新增 `docs/getting-started.md`（向导版 + 命令行版）与 `docs/README.md` 索引，
  从 `README.md` 与 `docs/pool.md` 链过去。

验收：`./gow test` 全绿（排除三个需要 macFUSE 的包），`./gow vet` 干净，
改动包 `-race` 无告警，`cloudfs setup` 手工冒烟走通（写配置、起控制面、`/accounts` 与
三个新前端模块都是 200）。

**已知遗留**：内置应用表是空的 —— 注册 Dropbox 应用（Full Dropbox、PKCE、回调
`http://127.0.0.1:53682/callback`、申请 Production）是维护者的活；gdrive 的
`.../auth/drive` 是 restricted scope，发布要 OAuth 品牌验证加每年一次的第三方 CASA
评估，值不值得投由人来定。另外 Dropbox 的 App Console 是否接受字面 IP 回调
（`internal/auth/oauth.go:100` 明确拒绝 `localhost` 这个名字）必须在真实控制台上验一次，
它卡着整条 Dropbox 授权路径。

## P0 — 正确性

### [x] T-00h delta 批量与并发目录列举：软/硬围栏拆分完成（2026-09-06，2026-09-07 收尾）

`-race` 下跑全量时发现：1000 部影片的媒体库扫描偶尔只找到 998 部，**没有报错**——
丢的总是相邻的一对（如 `Film 0947`/`Film 0948`），事后再读那两个目录又是完整的。
关掉守护进程的后台工作即不复现，定位到是 **delta 轮询首次应用积压** 与 **冷目录列举**
的交互。

- **围栏本身是对的**：`meta.ErrListingChanged`（"directory listing target changed or a
  newer refresh started"）拒绝发布一份在更新的刷新之前开始、因而可能过期的列举。
  加了标记后确认命中的是**生成代次**分支，且一次列举期间代次会连跳 3 次——delta 批量
  在同一目录上多次推进。
- **调用方的处理是错的**：`dirListing` 把这个拒绝直接当成读失败返回。现在改为**带退避
  的有限重试**（5 次，5ms 起倍增，总计约 0.25s，尊重 ctx），每次重试重新读取目录节点
  （围栏也可能因目录被替换而触发）。隔离复现的压力测试里错误数 72 → 8。
- **做过一次错误的修法并撤销**：曾加过"若目录此刻已 complete 就直接返回"的捷径。
  它会把一个**早于本次请求**的完整标记当作本次答案，等于把一份没人核对过的列举
  （可能是空的）交出去——正是要修的那个症状。已删除，并在代码里写明为什么不能这么做。
- **现在保证的是**：`internal/vfs/refresh_listing_race_test.go` 的
  `TestADeltaPollNeverMakesADirectoryLookShort` 在 1ms 轮询风暴下断言
  **列举可以失败，但绝不能少给条目**——被拒绝的调用方会重试或报错，拿到少一半条目的
  调用方无从知道。实测 720 次列举中 8 次被拒、0 次答错。
- **围栏拆成硬／软两个计数器（当日第二轮，这条的主体修复）**：把上面那个"仍开放"往下挖了
  一层，发现根因不是"需要互斥"，而是**一个计数器承担了两件语义不同的事**。
  - `generation`（硬）：目录下有名字被删除／移动，或目录对象被替换。更早的快照里还有那个
    名字，发布就是复活它——必须拒绝。
  - `stale_generation`（软）：只是"这个目录过期了"。delta 提到一个我们从没列举过的条目
    （`internal/vfs/refresh.go:235` 的 `store.Invalidate`），或上传落地时后端没描述结果
    （`internal/vfs/write.go` 三处）。**没有名字被移除**，快照仍然真实。
  全仓库只有 4 处调 `Store.Invalidate`，**全部是软语义**——其中 write.go:1169 那处前面已经
  有 `meta.Remove` 自己打过硬围栏了。软围栏命中时现在**照常发布快照，但把
  `dir_state.complete` 留在 0**：调用方拿到它要的目录内容，陈旧标记也没丢，下次读重新列举。
  1000 影片扫描撞上首批 delta 积压时的被拒次数 **72 →（加重试）8 → 0**。
  回归：`internal/meta/listing_fence_test.go` 的
  `TestDirListingPublishesThroughAStaleMarkButStaysIncomplete`（把 `Invalidate` 改回
  `fenceDirListingTx` 立刻失败）；`internal/vfs/refresh_listing_race_test.go` 由"允许被拒"
  收紧为**断言零拒绝**。schema v9 → v10（`ALTER TABLE ... ADD COLUMN stale_generation`）。

- **远端纯属性更新已改软围栏（2026-09-07）**：`ApplyRemoteNode` 只在"拿走了一个名字"时
  （`next == nil` 的删除，或 `directoryReplacement`）推硬围栏；纯属性更新推
  `stale_generation`，父目录的并发列举照常发布、`complete` 留 0。
  之前不敢改的理由——"列举不覆盖 delta 刚写的属性"会退化成依赖调用方的 `protect`——
  已经消除：schema **v10 → v11** 给 `nodes` 加了 `applied_gen`，变更流写完节点后把父目录
  当时的 `generation` 记上（`stampAppliedGenerationTx`）；`DirListing` 在 `BeginDirListing`
  取的正是同一个计数器，于是 `applied_gen >= 本次列举的 generation` 精确等价于"这次写发生在
  本次列举开始之后"。`mergeStaged`、`removeMissing` 与收尾的批量 `fetched_at` 刷新都跳过
  这样的条目，`protect == nil` 也成立。保护会自己过期：下一次列举拿到更高的 generation。
  用的是每目录的既有计数器，不是时钟，所以不受 `fetched_at` 秒级分辨率的影响。
  回归：`internal/meta/remote_attribute_fence_test.go` 三条（并发列举照常发布且属性不被写回、
  之后开始的列举重新拥有该条目、v10 库升级后老节点 `applied_gen=0` 仍可被列举更新）；
  去掉 `applied_gen` 判断第一条立刻失败。`TestRemoteNodeChangeFencesParentAndDirectoryListings`
  收窄为只覆盖 delete 与 replace。
  **仍未真实网盘验证**：生产轮询 60 秒，窗口本来就窄，收益要到真实 delta 流上才能测量。
- **顺带**：媒体库成本断言改为在 `NoBackground` 下测量——后台的 delta 轮询与预取本身
  也会产生 provider 调用，把它们算进"遍历的成本"是在量错东西。


### [x] T-00g 连续两次重写会退回上一次的内容（2026-09-06）

跑全量测试时 `internal/fusefs` 偶发失败（约 5 次里 1 次），症状是
`os.WriteFile` 连写两次之后 `stat` 报的是**第一次**的大小。先确认基线也复现——不是
新改动引入的——然后查到两个各自独立的原因，都是真实的数据回退：

1. **路径式 truncate 会另开一个写句柄。** `Setattr` 拿不到 fh 时会
   `Open→Truncate→Release`，于是同一个 inode 上出现两份互相竞争的暂存快照：一份是
   truncate 产生的（空文件），一份是应用写入的内容。两份都会提交、都会上传，**谁后落地
   谁赢**。新增 `FS.TruncatePath`：优先作用在该 inode 上已经打开的写句柄，没有才回退到
   开临时句柄。回归：`TestAPathTruncateUsesTheOpenHandleInsteadOfMakingAnother`
   （断言只产生 1 条上传）与 `TestAPathTruncateWithNoOpenHandleStillApplies`。

2. **上传完成与新提交是两个读-改-写序列，会互相覆盖。** `UploadHooks.OnSuccess` 先
   `meta.Get` 读节点、再决定写回什么；close(2) 可以在这两步之间提交更新的版本。
   旧代码无条件 `UpdateByIno`，于是**已被取代的上传把自己的 size / remote id / version
   写回了一个早已前进的文件**——FUSE 调试轨迹里节点最终是 `size=65536 remote=n2
   dirty=false`，而 4096 那条上传还排在队列里。改为 `meta.AdoptByIno` 做
   compare-and-set（`WHERE ino=? AND remote_id=?`，条件是这次上传发布时的身份），
   条件不成立就只用 `meta.SetRemoteVersion` 记下远端版本——那是下一次上传的冲突检查要比
   对的东西，但不能把读到的整行旧值一起写回去。回归：
   `TestACompletingUploadCannotRevertANewerWrite`（通过 `publishFault` 的
   `upload-result-read` 阶段确定性地插入第二次写）与
   `TestASupersededUploadOnlyRecordsTheRemoteVersion`。

**验证**：把任一处改回原样，对应回归立刻失败（"the completing upload put the previous
content's size back"）。`internal/fusefs` 连续 6 次 `-count=3` 全绿，基线在同样的
命令下约 1/5 失败。这条不需要真实网盘：它完全在本地元数据与队列之间。


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
  - **浏览器 OAuth 向导已补齐（2026-09-07）**：gdrive 与 box 现在和 aliyun / baidu 走同一条
    路径。`cloudfs config auth <account>` 起本地回环回调、开浏览器、自己换 token、自己写进
    安全存储；控制面 / Web UI 的 `POST /accounts/<name>/auth/start` 因为按
    `daemon.SupportsDaemonAuth` 分派，自动跟着支持。
    - 端点表从 CLI 与 daemon 两份重复实现收成一份：`daemon.OAuthProfileFor(type)` 返回
      `OAuthProfile{AuthorizeURL, TokenURL, Scope, JSONToken, FormToken, AuthParams}`，
      `Apply` 再让账号自己的 `oauth_authorize_url` / `oauth_token_url` / `oauth_scope`
      覆盖端点与 scope。换 token 的报文形状是协议属性，不给账号覆盖。
    - 新增 `auth.OAuthOptions.FormToken`：RFC 6749 的表单 POST。Google 与 Box 只接受这种，
      而此前只有 aliyun 的 JSON POST 和 baidu 的 GET query——GET 会把授权码和 client secret
      放进 URL，代理和服务端日志都会留下。
    - gdrive 的授权请求带 `access_type=offline` 与 `prompt=consent`：少任何一个，Google 都
      不下发 refresh token，账号一小时后就停摆。scope 用 `https://www.googleapis.com/auth/drive`，
      box 用 `root_readwrite`。
    - `gdrive` / `box` 的 `provider.Credentials.Note` 相应改写：`config add` 之后提示的是
      "浏览器会替你完成"，不再叫用户自己去别处铸一个 refresh token。
    - 回归：`cmd/cloudfs/config_oauth_gdrive_test.go`（两家各跑一遍完整授权：授权 URL 的参数、
      表单 POST 的字段、凭据落到安全存储、输出不含任何 `private-` 值；以及"凡有 OAuth profile
      的类型，提示必须指向浏览器"）、`internal/auth/form_token_test.go`（表单 POST，且 URL 里
      不出现授权码与 secret）、`internal/daemon/oauth_profile_test.go`（四家 profile 齐全、
      gdrive 的 offline/consent、box 走表单；pan115 与 smb 明确没有 profile）。
  - **仍缺**：gdrive / box 没有真实账号验收（授权流程只在本地 httptest 服务器上跑通）；
    **smb 没有在任何真实 SMB 服务器上跑过**，内存共享测试不等于真机验收；smb 的凭据是密码，
    没有授权服务器可谈，仍走 `config auth --stdin`。因此 T-02 保持部分完成。
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

### [~] T-03 MCP Resources：慢客户端已完全隔离，SDK 侧空容器与真实规模仍开放

- **2026-09-06 慢客户端隔离**：SDK 的 `ResourceUpdated` 逐个通知订阅者且自带 10 秒超时，
  一个传输卡住的客户端会占住这次调用。原来它在合并循环里内联执行——那段时间**服务器完全
  不再感知任何变更**，一个读不动的客户端冻结所有人的视图。现在发送在独立 goroutine 上，
  中间是有界队列（256）：满了不丢通知（watch 仍 dirty，下一个 tick 重投），同一 URI 不堆
  重复项。3 个回归：队列填满后仍接收并记录新变更、重复 tick 不堆积、12 轮会话反复连断后
  `sessions`/`streams`/`count`/队列全部归零。把入队改回内联，前两个立刻失败。
- **2026-09-07 按会话投递**：上一条"投递仍串行"已修。投递从"一条队列一个 goroutine"改成
  **每个会话一条队列、一个发送 goroutine**。SDK 仍然没有按会话投递的入口，定向靠一个
  `resourceWatch.claimed` 标记：发送者在调用 `ResourceUpdated` 之前先在自己那条 watch 上
  claim，发送中间件只放行被 claim 的那条，其余会话在碰到各自传输之前就被丢弃，由它们自己的
  发送者投递。于是一次广播只写进一个传输，卡住的客户端只占住自己那条队列。
  回归 `TestOneStalledSubscriberDoesNotDelayAnother`：一个会话的传输永久阻塞时，另一个会话
  仍在 5 秒内收到自己的通知；把所有会话改回共用一个发送者立刻失败。
  会话churn 回归相应从"投递队列归零"改为"`senders` 归零"。
- **仍开放（本项目侧，已知边界）**：权限只在**订阅时**校验。`reserve` 逐个 URI 走
  `parseResource`（允许列表 + 挂载归属），但 `run`/`deliver`/`send` 都不再调 `checkPath`，
  所以订阅期间改 `--allow` 或改挂载不会被重新验证——投递只检查"这个 URI 是不是这个会话
  注册过的"。另外 `Options.Allow` 是**服务器全局**的，包里没有任何按会话/按身份的权限模型，
  因此"这个会话能看到的路径"等价于"这个会话注册过的 URI"。多租户场景要靠一个进程一份
  allowlist 来隔离，不能靠订阅。
- **仍开放（上游）**：`go-sdk v1.7.0` 的 `Server.disconnect` 只删内层 map 里的会话，
  **不删随之变空的外层 URI 条目**，长期运行下按历史订阅过的 URI 数量增长；这在 SDK 内部，
  cloudfs 只能把自己这侧证明干净，不假装修好了它。TEMP 空间预算、大事务写锁延迟与真实规模
  性能不变。

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
  删除、复制可读版本及远端 delta/目录刷新，队列有界且溢出转重查提示。见 `docs/vfs-changes.md`。
  （原文接着写"会话订阅、取消/断连、权限过滤和通知合并尚未完成"——那已经是历史状态，
  四项都在 `internal/mcpsrv/subscriptions.go` 里实现并有回归：取消见 `receive` 的
  `resources/unsubscribe` 分支与 `release`，断连见 `session.Wait()` 的清理 goroutine，
  权限见 `reserve` 里逐个 URI 的 `parseResource`，合并见 per-watch `dirty` + 25ms tick +
  `queued`。2026-09-07 删掉该句，避免再被当成待办读。）

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

### [~] T-04 Copy：服务端复制的远端结果对账已完成，跨端原子性与真实账号仍缺

- **远端结果对账（2026-09-06 完成）**：这是 T-04 里最实质的一条，做完了。
  服务端复制的请求与答案都过网络，超时/重置/5xx **不能**说明对象是否已创建；
  provider 成功之后、本地元数据写入之前崩溃是同一个问题。原实现在这两种情况下都只是
  返回错误——重试可能留下第二份对象，放弃则在账号上留下本地无人知晓的文件。

  1. **journal schema v12 `server_copies`**：发请求前写下持久意图，并对
     「remote + 目标父对象 + 名字」加唯一约束。同一目的地不允许两个未结清意图，
     否则哪一个产生了对象永远说不清。
  2. **只有「动手前就拒绝」才算确定没发生**（`ErrUnsupported`/`ErrNotFound`/
     `ErrExists`/`ErrAuth`）；其余一律 `ErrCopyUnresolved`，意图保留、目的地占用。
  3. **`FS.ReconcileServerCopies` 去看**：枚举目标父目录找那个名字。找到是文件→
     本地没有就收编再结清；找不到→请求没生效，结清并释放目的地；其他情况保留并记录原因。
  4. **daemon 启动跑一遍**，且每次向某目的地发起新复制前先结清该目的地。
     对账收编后新的复制直接 `ErrExists`，不会再复制一份。
  5. **身份围栏**：元数据库身份/挂载/根对象/账号绑定不匹配则不收编也不删除——
     那是关于另一个账号的证据。
  6. **未结清的进 `status` 告警**，不是只写日志。
  9 组回归（含「答案丢失后重试不产生第二个对象」——写的时候第一版确实产生了第二个，
  测试抓到后才补上「对账收编之后立即返回 ErrExists」这一步）。详见 `docs/copy.md`。
- **跨端原子不覆盖仍缺**：上面的占用只在本进程 journal 内。本地存在性检查与远端复制
  仍不是同一事务，绝大多数网盘 API 也不提供条件创建，所以**不能**声称全局无覆盖；
  能声称的是本进程不会因一次答案丢失而产生重复对象。

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

### [~] T-06 账号命令与初始授权：交互式向导已补齐，真实账号待验收

- **2026-09-06 交互式向导**：`config add` 在终端下逐项询问——先选后端类型，再问该驱动需要
  的字段。**问题来自驱动自己**：新增 `provider.RegisterFields(typ, fields, creds)`，
  15 个已注册驱动全部声明了自己的公开字段与凭据键。CLI 里没有按网盘名写死的问题表——
  那张表会在驱动改动时过时，新驱动也进不去（有回归 `TestEveryRegisteredDriverDescribesItself`
  盯着这一点：新加驱动不声明字段就会失败）。
  - 已经用参数/`--set` 给过的字段不再重复询问。
  - **不在终端时行为完全不变**：不提问、不阻塞，缺必填字段直接报出缺哪几个。
  - `Required` 只标在驱动自己确实拒绝为空的字段上（写的时候第一版把 sftp 的 `user`
    标成必填，立刻被既有测试抓到——该驱动留空时用当前 OS 用户）。另有回归禁止
    「既必填又有默认值」这种自相矛盾的声明。
  - 添加完成后会用驱动声明的凭据说明告诉用户下一步 `config auth` 会要什么。
  - 6 个新测试：按声明提问并写入、已给的不再问、无终端不提问且报缺什么、未注册类型被拒、
    每个驱动都声明了自己、必填声明与驱动实际校验一致。

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
- **2026-09-11 补充**：writeback_cache 只影响写路径，对读负载不适用；go-fuse v2.11 没有协商它的开关，
  且内核在 writeback_cache 开启时会关闭 passthrough。读侧的内核选项（`FOPEN_KEEP_CACHE`、`MaxReadAhead`、
  `MaxBackground`、splice）另立 T-29。建议本条只保留 splice 与「是否值得为写吞吐放弃 passthrough」的决策，
  见 `docs/pool-v2.md` §4.6。

---

## 验证缺口（需要外部资源或长时间运行）

### [ ] T-11 92 处 `UNVERIFIED` 待真实账号核对

按协议资料推断、未在真实账号上跑通的细节。2026-09-07 重新计数：
`grep -rn UNVERIFIED --include='*.go' internal cmd` 命中 **100 处**（2026-09-15 二期合并后重数，严格 `UNVERIFIED:` 为 92：线 D 加了 3 处——
`internal/embed/openai.go` 的 openai 线上格式与 `dimensions`、`internal/embed/ollama.go` 的 ollama 批量接口返回顺序、
`internal/index/embed_worker.go` 的 64 条批是否超真实端点上限；线 E 加了 1 处——`internal/trigger/exec_windows.go` Windows 无 `Setpgid`
的进程组终止；此前 96 处：一期加了 4 处——PDF 中文抽取质量、
爬取器与索引 worker 的 quark 风控映射、Codex HTTP 配置键；再此前为 92 处 / 31 个文件，2026-09-12 重数；
其中 5 处是 T-33 配额哨兵新加的驱动映射，其余差额来自此前未计入的测试与工具文件），
不是此前记的 56——差额主要是 `internal/winfs`（10 处）与 `cmd/cloudfs-desktop`（1 处）
从来没有进过这张表，驱动侧的计数也偏低。下表按当前实测重列。不能在实际核验前笼统
断言这些未确认行为只影响可用性、绝不影响数据正确性。

| 模块 | 处数 | 集中位置 |
|---|---:|---|
| tianyi | 18 | `tianyi.go` 6 · `upload.go` 3 · `files.go` 3 · `auth.go` 3 · `api.go` 3 |
| quark | 14 | `upload.go` 4 · `files.go` 4 · `api.go` 3 · `quark.go` 3（另 `quark_test.go` 1） |
| pan115 | 11 | `upload.go` 4 · `pan115.go` 4 · `api.go` 3 · `auth.go` 2 · `oss.go` 1 |
| winfs | 10 | `winfs.go` 7 · `fsop.go` 2 · `naming.go` 1 —— 需要 Windows 真机，属于 T-21 |
| aliyun | 6 | `aliyun.go` 6 |
| gdrive | 4 | 修订下载端点、resumable session URI 是否需要 bearer、403 的 reason 取值、orderBy 跨页稳定性 |
| pan123 | 4 | `pan123.go` 4 |
| baidu | 4 | `baidu.go` 4 |
| box | 1 | content 端点的 `version` 查询参数 |
| cloudfs-desktop | 1 | `link_windows.go` |
| embed / index | 3 | `embed/openai.go` 1 · `embed/ollama.go` 1 · `index/embed_worker.go` 1 —— 需要真实 openai / ollama 端点，属于 T-39 遗留 |

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

### [ ] T-45 全量 `-race` 在负载下超时：三项计时测试不适合竞态模式

- **2026-09-15 实测**（Linux 8 核，同时有 3 个 agent 在编译测试，load ≈ 5）：`./gow test ./...` 全绿（两处
  测试自身问题已在 1d721eb 修掉）；`./gow test -race ./...` **无任何 `DATA RACE`**，但三个包因计时超限而红：
  `internal/meta` 在 `TestShortSearchUsesPostingsAt100KNodes`（插入 10 万节点）上耗尽 10 分钟默认超时；
  `test/e2e` 同样 10 分钟超时（`TestSTRMGenerationOverAThousandFilmsHasABoundedBackendCost` 运行中）；
  `test/perf` 的 `TestPoolReadFanoutAddsBandwidth` 断言"3 副本 < 0.60× 单副本时间"在 race 下得 0.88。
  非 race 的 `test/perf` 全绿。
- **做法（2026-09-15 已做前半）**：新增 `internal/testx.RaceEnabled`（`//go:build race` 常量，照标准库
  `internal/race`），`TestShortSearchUsesPostingsAt100KNodes` 与 `TestPoolReadFanoutAddsBandwidth` 在 race 下
  `t.Skip`；e2e 不跳过——它 96 s 的非 race 用时在 race 下本来就要 8～10 分钟，`CLAUDE.md` 的 race 命令改为
  `-timeout 30m`。
- **仍缺**：在空闲机器上跑一次 `./gow test -race -timeout 30m ./... -count=1`，确认除这三处外没有别的超时。
- **2026-09-15 晚补充（非 race，负载下的计时 flake）**：`internal/cache` `TestSparseLayoutWritesTheFileOnce`
  （"wrote 245760 bytes … want 262144"）——根因与早上的 `TestSmallFilesKeepTheBlockLayout` 相同：`HydrateAfter=1ms`
  让管家在最后一块采样前就合并并删掉块文件，少计一块；已改为填充期 `HydrateAfter=time.Hour` + 手动 `hydrateDue()`，
  30 次重跑稳定。`internal/export` `TestExportSpreadsAcrossMembers`（基线 2/12 失败）根因是 fake 延迟 2 ms 在负载下
  抖动超过 `pickReplica` 的 20% 平局带，某成员被判更快拿走多数读；改为 20 ms 后 30 次（含并发负载与 race）稳定。
  仍待处理：`test/e2e` `TestStressWithForcedRefreshKeepsReadYourWrites` 在 race 全包下偶发 `input/output error`，
  与本期改动无关。
- **验收**：空闲机器上述命令全绿；`grep -c 'DATA RACE'` = 0。

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

### [x] T-17 图形控制面：完整 Web 应用已交付（8 屏全操作，凭据仍只走终端），真机视觉验收待补

> 2026-09-06：从只读状态页扩为覆盖所有 CLI 操作的桌面式 Web 应用 + 可选原生壳，详见 T-24/T-25。
>
> 2026-09-08：逐条核对控制面路由与界面调用，补齐界面够不到的后端能力（`docs/ui-plan.md` 阶段 E）：
> 连接删除与连接设置浮层（编辑 proxy/qps/upload_workers/公开字段、测连通性、挂载绑定与解除）、
> 代理编辑（`PUT /proxy/config` 此前零调用者，界面注释却写着会保存生效）、目录与队列分页、
> 文件重命名/预览/下载链接、复制任务屏、批量维护动作（冲刷队列、重试全部死信、释放内核缓存、
> 池重建/按路径校验/加入已有池）、授权会话取消。顺带修一个数据丢失缺口：代理配置 GET 会脱敏
> 出口地址里的密码，界面原样 PUT 回去会把密码从配置里抹掉；现在视图带 `has_credentials`，
> 写入需 `keep_credentials`，脱敏形态原样回传直接 400。
>
> 以下为最初的 M5 只读版验收记录。

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

**Web 加账号（2026-09-06）**：新增 `GET/POST /accounts` 与页面上的表单，`config add`
不再是加一个网盘的唯一入口。

- 表单字段来自各驱动声明的 `provider.Fields`（与 CLI 向导同一来源），页面里没有按网盘
  名写死的表单。
- **凭据不经过这个 API**，这是刻意的边界：POST 里出现任何 `config.IsSecretField` 的键
  一律 400，并告诉调用方去执行 `cloudfs config auth <name>`；响应不回显被拒绝的值，
  配置文件也不会被写入任何东西。网盘凭据是系统里最敏感的值，为省一条命令把它放进
  回环上的浏览器表单——攻击面最大的地方——不划算；而且真正重要的授权流程（浏览器
  OAuth、手机扫码）本来就由终端驱动。回归里逐个尝试 7 种凭据键并检查配置文件未被写。
- 复用既有的 `privateRequest`：跨站表单（无 `X-CloudFS-Control`）、跨源 fetch、
  DNS rebinding 的 Host 全部 403，均有回归。
- 名称/字段名/值都有窄校验（名称只允许字母数字与 `-_`，拒绝结构性键 `type`/`proxy`/
  `qps`/`upload_workers` 与 `_` 前缀，拒绝含换行的值——否则就是往 YAML 里注入）。
  声明为必填的字段缺失即拒绝。
- 另有回归断言**页面上没有 password 类型的输入框、也不出现任何凭据字段名**：服务端会
  拒绝，但一个问你要密码的表单已经教会用户把密码往浏览器里敲了。
- 无配置文件的守护进程（MCP-only 容器）如实报告 `configurable: false`，POST 返回 409。
- **仍缺**：真实浏览器的截图级验收；Web 侧不做也不打算做凭据输入与 OAuth 回调。

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

### [~] T-20 媒体库场景：三条验收断言已补齐，真实播放器待验收

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

**2026-09-06 验收补齐**：原来的三条验收里有两条是可以在本机断言的，现在都断言了
（`test/e2e/strm_media_test.go`，1000 部影片、每部一个目录含影片+海报）：

1. **「Emby 扫描全程的 provider 调用次数有上界」**：冷扫描的后端调用数**恰好等于把
   整棵树枚举一遍**（1501 次，含分页），期望值由 provider 的分页大小推导而不是写死，
   超出即说明遍历在重复列举目录。同时断言扫描期间零 `ReadRange`、零 `DownloadURL`
   ——`.strm` 是指针，为写它去打开影片会把库扫描变成全量镜像。
2. **「媒体目录设 `dir_ttl: 24h` 后重复扫描零远端调用」**：第二次扫描 0 次后端调用、
   0 次写入、1000 个 `unchanged`。
3. 「跳播只下载附近块」在 2026-09-05 已有回归，未重复。
4. 另补 `--prune` 的边界回归：上游消失的片子对应 `.strm` 被删，刮削器自己写在旁边的
   `.nfo` 保留。

**做这条时量出来的一个真实行为**：目录刷新的过期列表保护比较 `fetched_at` 与本次列举
的开始时刻，而这两个时间戳是**秒分辨率**。所以一个刚被列举过的子目录，在同一秒内发起
的父目录刷新中会被保留，即使远端已删除——最多晚一秒才消失。方向是刻意的（宁可留着
也不要误删），但它是可观察的：修剪回归必须等过这一秒。已记入
`docs/directory-refresh.md`。

**`warm` 已经是只列目录不下载内容**（`FS.Warm` 只做 `readDirRefresh`），原目标里的
这一条无需新增。**仍缺**：真实 Emby/Jellyfin/Infuse 的扫描与播放、真实 provider 限流
下的十万级目录耗时、以及自动（非 `--prune`）的陈旧 `.strm` 清理。

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

### [x] T-21 Windows：双产物已交付（静态无挂载 + `-tags winfsp` WinFsp，均 nocgo 交叉），真机验收待补

> 2026-09-06：C1 静态版与 C2 WinFsp 适配均完成并交叉编译通过，见 T-24 阶段 C。运行时行为需 Windows 真机按 `docs/distribution.md` 清单验收。

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

### [x] T-24 桌面界面的控制面 API（2026-09-06 完成阶段 0–5 + A/B/C/D）

设计稿 https://claude.ai/code/artifact/2c5d82b9-8ca9-4f8b-a4f7-02158cd95153 ，开发方案与逐项 TODO 见
**`docs/ui-plan.md`**（阶段 0–5 是后端，A 前端，B 桌面壳，C Windows，D 发布）。这里只记进度。

- **[x] 阶段 0 地基**：`/cache/drop` 补上 `privateRequest`（此前是唯一没守卫的变更路由，
  一个普通 HTML 表单就能让缓存变冷；`cloudfs bench --cold` 的客户端跟着补了头）；
  路由改为从表注册，`security_all_routes_test.go` 遍历每条路由断言"要么守卫、要么在
  `NewServer` 注释里被论证为只读开放"（去掉守卫立刻报 `got 501, want 403`）；抽出
  `writeJSON` / `requireConfirm` / `rejectSecretFields`（凭据边界只此一处）；
  `config.SafeExtraFieldName` 成为"哪些键可由外部请求写入"的唯一定义；
  修了 `vfs.Attr.Pinned` 从不被设置的 bug（`attrAt` 让知道路径的列举零额外查询地报出它，
  回归 `TestAttributesReportPinned`）。
- **[x] 阶段 1** `/fs/*`（list/stat/preview/download-url/mkdir/rename/delete）、`/search`、
  `/doctor/run|fix`、`/events`（SSE：VFS 变更 + 状态快照）。目录分页游标移到
  `vfs.ParseDirectoryCursor/NextDirectoryCursor`，MCP 与控制面发的是同一串字节。
  e2e `TestUIAPIEndToEnd`：经控制面 mkdir→list→rename→delete，逐步在**内核挂载点**上核对。
  它抓到一个既有 bug：**非内核发起的 Remove/Rename 不作废内核 dentry**——只作废了父目录
  inode，内核对那个名字的正向 dentry 会在整个 entry timeout 内继续回答 stat(2)。MCP 的
  delete 一直有这个问题，只是没有 e2e。现在 `invalidateEntryFrom` 在 remove/rename 上对
  旧名（和改名的新名）各发一次 EntryNotify，内核自己做的 unlink/rename 跳过。回归
  `TestOutOfKernelRemoveAndRenameDropTheKernelDentry`；去掉修复后 e2e 在 500ms 后仍能
  stat 到已删目录。
- **[x] 阶段 2** 配置变更库：`RemoveRemote`（有布局引用时拒绝）、`SetRemoteField`（部分更新，
  nil 删键，空值/0 回默认即删键；拒秘密键与非法键名）、`SetProxy`（整段替换，先当完整配置
  `Validate`）、`AddMount/SetLayout/RemoveMount`（与 `AddRemote` 共用 `upsertMountLayout`）。
  测试用带注释的配置文件，断言每个编辑器都不动注释。
- **[x] 阶段 3** `GET/PATCH/DELETE /accounts/{name}`、`POST /accounts/{name}/check`（经
  `daemon.SanitizeAccountError`，CLI 同用）、`/proxy/explain|check|config`、`/mounts`。
  代理地址里的 `user:pass@` 是凭据：GET 脱掉，PUT 拒绝。所有配置改动回 `restart_required`。
- **[x] 阶段 3a** 代理热加载：`proxy.Manager.Reload`。路由状态（rules/outbounds/groups）放进
  `atomic.Pointer[routingState]` 整体替换——顺带修掉了这三个字段此前的无锁读；健康检查
  goroutine 按新分组集合起停；带悬空目标的 reload 被拒且旧配置原样生效。
  `control.ProxyManagerOptions` 是 config→Manager 的唯一转换，冷启动与热加载共用。
  `PUT /proxy/config` 现在 `applied:true, restart_required:false`。回归：下一次请求就走新出口；
  分组换成员后旧成员停止探测、新分组可解析；`-race` 下 50 次 Reload 对 4 路请求。
- **[x] 阶段 4** 守护进程驾驭的授权：`internal/daemon/auth.go` 的 `StartOAuthFlow`/
  `StartDevice115Flow` 从 CLI 抽出、CLI 与控制面共用。守护进程自己绑回调、收 code、换 token、
  存凭据——秘密值从不跨出进程边界给发起方。控制面 `POST/GET/POST
  /accounts/{name}/auth/{start,status,cancel}`：内存会话表，session id 128 位随机，同账号
  只允许一个在飞（第二个 409），终态轮询即回收；A 类（aliyun/baidu OAuth、pan115 扫码）返回
  URL 或二维码内容字符串，B 类（密码/cookie/外部 token）拒绝并指向 `config auth`。失败一律
  报 generic，不带 provider 原文。回归 `auth_test.go`：断言呈现的是 URL/QR、响应体永不含 token、
  双飞 409、取消、终态脱敏。
- **[x] 阶段 A 前端 + A11 `cloudfs ui`**：多文件 ES modules（`internal/control/web/`：
  `index.html`、`app.css` 设计令牌、`store.js`/`api.js`/`router.js`/`i18n.js`/`icons.js`/`ui.js`
  + `screens/{main,transfers,storage,proxy,diagnostics}.js`），`embed.FS` + 白名单静态处理器，
  CSP 收紧到 `script-src 'self'; style-src 'self'`（去 unsafe-inline），每资产按内容哈希带 ETag、
  `If-None-Match` 回 304。字体走显式 CJK 栈 + `font-size-adjust` + `tabular-nums`，控件
  `appearance:none` 手绘、统一滚动条与 `:focus-visible`——三平台同一份字节。SSE + 5s 轮询兜底；
  删除/丢弃走输入标识才启用的确认 sheet（焦点陷阱 + Esc）。`cloudfs ui [--print]` 用 `FetchStatus`
  确认守护进程在线再开浏览器（darwin `open` / windows `rundll32` / 其它 `xdg-open`）。
  回归重写 `ui_test.go`：多文件资产/类型/ETag/304、CSP 收紧、未知路径 404、**全资产字节里
  无 password 输入、无凭据字段名、指向 `config auth`**。README 与用法表已更新。
- **[x] 阶段 C1 Windows 仅编译**：`GOOS=windows go build ./...` 现在干净，产出无 mount 的静态
  `cloudfs.exe`（config/doctor/mcp/webdav/账号管理全可用，`Supported()` 如实报告不支持挂载）。
  拆分：`journal/lock_{unix,windows}.go`（flock ↔ `LockFileEx`+`LOCKFILE_FAIL_IMMEDIATELY`）、
  `config/filelock_{unix,windows}.go`（三处 `unix.Open`+`Flock`+`O_NOFOLLOW` 收敛成一对原语，
  Windows 无 O_NOFOLLOW 已注释承认）、`vfs/sync_{unix,windows}.go`（`syscall.Sync` → 无操作）、
  `control/listeners_{unix,windows}.go`（Unix socket 绑定，Windows 走 TCP 回环并明确报错）、
  `config_manage.go` 的隐藏输入 `unix.Poll` 循环重构为 goroutine + channel + select（去平台化）、
  `internal/fusefs` 依赖 go-fuse 的 5 个文件加 `!windows` 标签 + `fusefs/windows.go` 桩
  （MountFS/Supported/VerifyMountable/Mount 等全套，一律"此构建不支持挂载"）。
  CI 加 `cross` job：Linux runner 上 `GOOS=windows` build + vet 我方包；`release.sh` 加
  windows/amd64（`.exe` 后缀）。native 全量 + `-race` 仍绿。
  **C2 WinFsp 适配（cgo，`-tags winfsp`）与真机验收仍开放**，见 `docs/ui-plan.md` 阶段 C2。
- **[x] 阶段 5 生命周期**：服务管理迁到 `internal/service`（可注入的 `Runtime`，CLI 与控制面
  共用一份；install/uninstall 加非阻塞侧车锁，已装再装回 409，卸载顺序"停服务→拔挂载→删定义"
  仍是唯一不变量并被断言）。`POST /daemon/restart`（confirm）置 draining 标志后原地 re-exec
  （unix execve / windows spawn-then-exit）——先由 `cmdMount` 的 defer 释放 journal 锁与监听，
  保证同一刻只有一个 owner；`POST /service/install|uninstall`、`GET /service/status` 接上。
  回归：draining 中的变更 503、restart/uninstall 的 confirm 门、install 409。诊断页接了这三项。
- **[x] 阶段 B 桌面壳 `cmd/cloudfs-desktop`**：薄 cgo 壳（webview_go，WebKitGTK/WKWebView/
  WebView2），把 WebView 指向守护进程的控制台 URL，不内嵌守护进程、不自服务静态资源。三种解析：
  直连配置的回环 TCP UI；socket-only 守护进程经壳内回环反向代理转接（保留浏览器的回环 Host
  以满足同源守卫）；无守护进程时用 `CLOUDFS_CONTROL_UI` 起一个带 UI 的回环 TCP 面。单实例 flock +
  focus socket 唤起旧窗（Linux `gtk_window_present`）。`-tags desktop` 门控，默认 `go build ./...`
  仍绿；`packaging/pkgconfig` 的 4.0→4.1 shim 绕过 webview_go 硬编码。已验证：对 WebKitGTK 4.1
  编译链接、Xvfb 下开窗、`CLOUDFS_CONTROL_UI` 握手在真实守护进程上服务出控制台。
- **[x] 阶段 C2 WinFsp 适配 `internal/winfs`**：cgofuse 路径式适配，与 fusefs 同形；
  `-tags winfsp` 下 fusefs 变成一文件 façade（类型别名）转发到 winfs。cgofuse v1.6.0 的 no-cgo
  Windows 后端运行时加载 `winfsp-x64.dll`，所以两个 Windows 产物都 `CGO_ENABLED=0` 从 Linux
  交叉编译——比原方案（假设需 cgo 交叉链）更好。两项产品决策在 `naming.go` 里显式给出并注释：
  大小写敏感对齐 POSIX 远端；Windows 无法表示的名字（非法字符/尾点空格/保留设备名）列目录时
  跳过、创建时拒绝，而非静默不可逆改写。已验证到本机极限：`GOOS=windows CGO_ENABLED=0 -tags winfsp`
  交叉编译+type-check、go-fuse 不进任一 Windows 构建图、默认 Linux 构建/vet 不变；每条运行时
  假设标 `UNVERIFIED:`，真机验收清单见 `docs/distribution.md`。
- **[x] 阶段 D 发布/CI/文档**：`ci.yml` 的 `cross` job 加 `-tags winfsp` 交叉 build + `winfs` vet，
  新增 `desktop` job（装 GTK/WebKitGTK + shim，build/vet `-tags desktop`）；`release.sh` 加
  `_mount.exe`（winfsp，nocgo 交叉）——一次产出两个 Windows 产物；`docs/distribution.md` 写清
  两产物与 WinFsp 真机验收清单；README 补 `ui`/桌面壳；`docs/DESIGN.md` §4.8 补全部新控制端点。

### [x] T-25 桌面壳 `cmd/cloudfs-desktop`（webview_go，独立 cgo 二进制）

见 `docs/ui-plan.md` 阶段 B。守护进程保持 `CGO_ENABLED=0`；壳只是指向回环 URL 的窗口 + 托盘，
不内嵌第二份装配逻辑，不自己服务静态文件。

### [x] T-26 存储池：多网盘融合为一个命名空间，N 副本、自动修复、本地热缓存（2026-09-06）

`internal/pool`（`type: pool`）。设计与已知异常见 `docs/pool.md`，架构摘要见 `docs/DESIGN.md` §4.11。
交付：只读镜像命名空间（合并列举、冲突副本、成员失联快照、读故障转移）；写路径（主成员透传、hold、树操作扇出、op-log）；
成员健康状态机（对所有 remote 生效，`/status` 与侧栏圆点）；修复 worker（hold/活副本、out 触发再复制、封顶）；
op-log 幂等重放、drain、scrub、裁剪；命名规则与配额驱动放置、`df` 显示后端容量；delta 聚合、成员标记文件、
多机收敛；控制面 `/pool/*`、`/fs/list` 可用性、存储池界面、`cloudfs pool`；索引重建、hold 对账、
`doctor` 的成员/副本/积压/标记检查。
验收：`internal/pool`、`test/chaos`（成员失联 / 永久丢失 / drain / 带外删除 / delta 回声）、`test/perf`（热遍历 0 调用、
冷目录按持有者计费、修复 N 文件 N 次上传）、`test/e2e/TestPoolEndToEndThroughFUSE`。
真实账号验证缺口见 `docs/pool.md` 末节。

### [x] T-27 路径式 id 的后端改目录名后子孙失联（既有 bug，2026-09-07 修复）

- **原因**：`internal/vfs/write.go` 的 `rename` 丢弃 provider `Move`/`Rename` 返回的 Entry，
  `internal/meta/store.go` 的 `Store.Rename` 只改 `parent_ino/name`、从不重写 `remote_id`，
  更不重写子孙。webdav/sftp/s3/smb 的 id 就是路径，所以目录改名后 `dir_ttl` 内读子孙必然
  拿旧路径去问后端。存储池用不透明稳定 id 绕开了它，普通挂载没有。
- **能力矩阵先说清楚**：新增 `provider.Caps.PathIDs`——"id 就是路径，改目录名会改掉下面
  所有 id"。sftp/webdav/s3/smb 四家宣告它（`openlist` 是 webdav 的别名，跟着继承）。
  上层不按网盘名字特判，这条差异同样走 `Caps`。
- **修法**：`rename` 保留 provider 返回的 id（先 `Move` 后 `Rename`，逐步跟着变），
  与原 id 不同就调用新的 `meta.Store.Retarget(ctx, ino, newID, descendants)`。
  `descendants` 取 `caps.PathIDs`：只有路径式 id 才会把"新 id + `/`"接到子孙上，
  不透明 id 变了只说明它自己变了，说不了子孙。子孙用递归 CTE 限定在子树内，再按
  "旧 id + `/`" 前缀匹配，前缀比较交给 SQLite 的 `substr`/`length` 做（按字符而非字节，
  非 ASCII 名字才不会错位）。因此 `cloudfs-local:` 占位的待上传文件与 `/projector`
  这种只是字符相同的兄弟都不会被改到。
- **回归**：`internal/vfs/rename_path_ids_test.go`（path-id 后端改名/移动目录后子孙 id
  正确且立刻可读，不透明 id 后端子孙 id 不变）、`internal/meta/retarget_test.go`
  （子树前缀重写、不动待上传占位、`descendants=false` 只改自己）、四个驱动各自
  capabilities 测试断言 `PathIDs`。去掉 `Retarget` 调用后前两条立刻失败。
- **代价**：子孙的 `remote_id` 变了，缓存键（`FileKey{Remote,RemoteID,Version}`）随之改变，
  改名后这些块要重新下载。此前它们是**读不到**的，所以这是净改善；真要保留热缓存需要
  一个缓存改键接口，那是另一件事。

### [x] T-28 `provider.Instrument` 在后端实现 ChangeLister 或 ServerCopier 时丢掉 SinglePutter（既有 bug，2026-09-06）

`internal/provider/instrument.go` 的可选接口组合是硬编码 switch。实际条件是 `!hasChanges && !hasCopy`，所以只要
**任一** 成立就丢——中招的是四个驱动而不是三个：gdrive/onedrive/dropbox（三者都有）以及 box（只有 Copy）。
小文件本该 1 次请求，实际走 3 次会话协议。已改为 put 变体与 changes/copy 变体组合（`countingPut` /
`countingChangesPut` / `countingCopyPut` / `countingBothPut` 共用 `putting`），四种组合都保留全部可选接口。
回归：`TestInstrumentKeepsTheOneRequestUploadBesideTheOthers` 覆盖五种组合，并断言 `Unwrap`、`RangeReaderAt`、
`StreamLister` 仍可达；回退实现即失败。

### [x] T-29 读路径便宜项：内核缓存、请求合并、`Transfer` 令牌类、`MaxConnsPerHost` 接线（`docs/pool-v2.md` 阶段 A）

- **证据**：只读 open 返回 flags `0`（`internal/fusefs/fs.go` `Open`），内核每次 open 丢页缓存；`platform_linux.go`
  `MaxReadAhead = 1MiB`，`MountOptions.MaxBackground` 用 go-fuse 默认 12；`maybeReadAhead` 为窗口内每个块各发一个
  4 MiB 请求；CDN 字节 GET 与取直链 API 记同一个 `Download` 桶（`internal/provider/aliyun/aliyun.go` 的 ranged GET、
  `quark/files.go`），吞吐上限 ≈ QPS × 4 MiB；`net/proxy/dialer.go` 的 `transportFor` 只设 `MaxIdleConnsPerHost: 8`，
  `Caps.MaxConnsPerHost` 除 `pool.go` 求和外无人消费。
- **做法**：`FOPEN_KEEP_CACHE`；`MaxReadAhead 4MiB` + `MaxBackground 64`；macOS `iosize=1048576`；`flight.Reserve` +
  窗口内连续块合并成 `readahead_request`（默认 16 MiB）一次 range；`ratelimit.Transfer` 类（`provider.QPS.Transfer`，
  0 = 回退 Download），aliyun/quark/pan115/tianyi/baidu 的 OSS 流改记它；`Manager.ClientWithLimit` + `remotes.<n>.max_conns`。
- **验收**：
  - `test/perf` 的 `TestSequentialReadUsesWholeBlocks` 改为接受 `wantBlocks / coalesce` 次 `ReadRange`，其余基线不变。
  - `internal/fusefs`：同一文件通过挂载点读两遍，第二遍 `OpStats().opRead` 增量为 0。
  - `internal/net/proxy`：`setConns(3)` 后 transport 的 `MaxConnsPerHost == 3`；`max_conns: 2` 的 remote 经 caps 报告 2。
  - `qps.transfer: 0` 时 aliyun 的字节 GET 仍记入 `Download` 桶（回退路径有测试）。

### [x] T-30 目录读序预取与 `cache.policy` 按前缀预设（`docs/pool-v2.md` §3、§7，阶段 B）

- **证据**：读路径没有跨文件预取；`cp -r` 一万个小文件 = 一万次串行 open/read/close，受 provider QPS 约束；
  `FS.Pin` 是唯一的整子树下载且落缓存不落目标；小文件先写块文件、10 s 后 hydration 再拷一遍。
- **做法**：`internal/vfs/read_dir_ahead.go`——句柄第一次 `Read` 时用 `meta.ChildrenPage` 判定是否按列表顺序读，
  3 次起窗，前方保持 `dir_readahead`（默认 32）个 ≤ `small_file_threshold`（默认 4 MiB）的文件，整文件一次 range →
  新 `cache.PutWhole` 直接落 `hydrated/`；每 remote 槽位 `clamp(round(QPS.Download), 2, MaxConnsPerHost) − 1`，
  与块级 readahead 共用；前台不占槽，`blockFlight.Reserve` 让前台 miss 加入等待；`invalidateListing/dropPaths` 取消。
  `cache.policy{preset, small_file_threshold, dir_readahead, readahead_max, readahead_request, readahead_lead}`
  全局 + `layout.<prefix>.cache` 覆盖，preset `media|photos|code|none`。
- **验收**（`test/perf`，fake 计数）：
  - `TestDirectoryReadaheadMakesSiblingReadsFree`：64 个小文件按名序读 3 个后等待，再读 `file003..010` 时 `ReadRange` 增量 == 0。
  - `TestRandomOrderReadsDoNotTriggerDirectoryReadahead`：乱序读 10 个，`ReadRange == 10`。
  - `TestDirectoryReadaheadIsBoundedByRemoteSlots`：`QPS.Download=2, MaxConnsPerHost=3` 时 `Fake.MaxInflight() ≤ 2`。
  - `TestDirectoryReadaheadSkipsLargeFiles`：超阈值文件的块不被预取。
  - `TestDirectoryReadaheadStopsOnListingChange`：`Refresh` 后不再有超出已认领范围的 `ReadRange`。
  - `internal/config`：preset 解析、显式键覆盖 preset、校验错误；两个不同策略的 mount 得到不同窗口。
- **完成**：`internal/config/cache_policy.go`（preset + 覆盖 + 校验）与 daemon 的 `vfs.CachePolicy` 传递此前已在；
  本次补上 vfs 侧的 `internal/vfs/read_dir_ahead.go`——句柄首读时按 `meta.ChildrenPage` 判定列表顺序，
  连续 3 次起窗，前方 `dir_readahead` 个 ≤ `small_file_threshold` 的文件整文件一次 range → `cache.PutWhole`，
  每 remote 槽位 `clamp(round(QPS.Download), 2, MaxConnsPerHost) − 1`，前台读遇同文件在途预取则等待而不是重发，
  `invalidateListing` / `dropPaths` 清读序。测试 `test/perf/dirahead_test.go` 五条，全部按 provider 调用次数断言。

### [x] T-31 大文件：速率窗口、全局在途预算、池副本扇出、稀疏整文件布局（`docs/pool-v2.md` §4，阶段 C + F）

- **证据**：readahead 窗口固定 16 块 = 64 MiB，不看文件大小与读者速率（`daemon.go` `readAheadBlocks`）；
  `PutAsync` 超 write-behind 预算时静默退化为同步 `Put`；池读在延迟相同时永远打声明顺序第一个副本
  （`internal/pool/read.go` `sortReplicas`），3 副本文件只得到 1 个网盘的速率；≥ 4 MiB 文件冷读本地写两遍（块文件 + hydration）。
- **做法**：`target = clamp(R × readahead_lead, 2 块, readahead_max)` 且只在 stall 时增长；`Σ 在途块 ≤ cache.WriteBehindBudget()`；
  `pool.member` 加 `inflight/served/EWMA`，`pickReplica` 按在途数与延迟选副本，`ReadRange/ReadRangeAt/DownloadURL` 改 pick，
  `pools.<n>.read_fanout: off|auto|all`（unofficial 层默认同文件单流），`resolveFile` 结果 5 s 缓存，`tryReplicas`
  遇 404 先 `Stat` 再标 missing；`cache.Options.WholeLayoutMin`（64 MiB）以上按偏移直接写 `hydrated/<fh>.part` + 位图，
  写满 rename；已 hydrate 文件 `Read` 返回 `fuse.ReadResultFd`；`FileLseeker`；挂载内 `NodeCopyFileRanger`。
- **进度**：池副本扇出已完成——`internal/pool/pick.go`（`pickReplica`：在途数 / 连接预算 + 每 MiB 延迟 EWMA，degraded 成员加罚分，
  unofficial 层在 `read_fanout: auto` 下同文件单流）、`replicacache.go`（`resolveFile` 5 s 缓存，每个索引事务与 `execIndex` 失效）、
  404 先 `Stat` 再标 missing、`pools.<n>.read_fanout` 校验；上面列出的 `internal/pool` 与 `test/perf` 验收测试均已在。
  速率窗口与全局在途预算已完成——`internal/vfs/readahead.go` 的 `nextWindow` 按句柄读速 EWMA × `readahead_lead`
  定窗口（下限 2 块、上限 `readahead_max`，只在前台读 miss 时增长；还没有速率估计时起点取 `MaxConnsPerHost`），
  `launchReadaheadRun` 把在途预取字节数压在 `cache.WriteBehindBudget()` 之下；测试在 `test/perf/dirahead_test.go`。
  稀疏整文件布局也已完成：`internal/cache/sparse.go`——超过 `cache.Options.WholeLayoutMin` 的文件，取回的块
  直接按偏移写进 `hydrated/<hash>.part`（稀疏文件）并维护块位图，写满后 fsync + rename，冷读只写一遍本地盘；
  重启后位图有效就继续用、无效或块粒度不符就丢弃重取。测试 `internal/cache/sparse_test.go`、
  `internal/cache/sparse_disk_test.go`、`test/perf/sparse_test.go`。
- **验收**：
  - `internal/pool`：`TestReadFanoutSpreadsBlocks`（12 块、窗口 8 → 每成员 `ReadRange` 4 ± 1）、`TestReadFanoutSkipsDownMember`、
    `TestReadFanoutPrefersFast`；`test/perf`：`TestPoolReadFanoutAddsBandwidth`（20 ms 延迟、48 MiB、3 成员 < 0.5× 单成员）。
  - 假设读全落成员 0 的旧基线（`TestPoolWriteThenReadIsLocal` 等）按新语义更新且仍断言总调用数。
  - 两个句柄同时顺序读时在途块总数不超过预算；5 MiB/s 的模拟读者窗口稳定在 ≈ 40 MiB 而不是 64 MiB。
  - 冷读一个 256 MiB 文件，本地磁盘写入字节 == 文件大小（不再 2×）；`reload` 后 `.part` 带有效位图可继续读。
  - `internal/fusefs`（Linux）：`cp --sparse=auto` 不再产生 `ENOTSUP`；已 hydrate 文件的读走 `ReadResultFd`。

### [x] T-32 `cloudfs export`：把虚拟路径批量导出到本地/移动硬盘（`docs/pool-v2.md` §5，阶段 D）

- **证据**：没有任何导出/同步/拉取命令。`cloudfs cp` 只支持虚拟路径 → 虚拟路径单文件（`internal/vfs/copy.go`）；
  `cloudfs pin` 落缓存不落目标目录；`cp -r` 走内核单文件串行 READ，无跨文件并行、无续传、拔盘即失败。
- **做法**：新包 `internal/export` + 独立 `<cache.dir>/exports.db`（不复用 `copy_jobs`：那是上传侧语义），
  从 meta 规划（暖态 0 远端调用）、`transfers` × `streams` 并发、直读 `mount.Provider.(RangeReaderAt)`（不经 `FS.Read`）、
  `<rel>.cloudfs-part` + 已完成 range 位图续传、哈希/size+mtime 校验、`--mirror` 需既有标记、拔盘（`st_dev` 变化）与
  `ENOSPC` → `paused(disk)` 自动恢复、`fs.Busy()` 让路；已缓存文件从 `OpenWhole` 拷字节（绝不硬链接缓存对象到用户目录）。
  控制面 `/export`、`/exports*`；CLI `export` / `exports list|show|pause|resume|cancel|forget`；MCP `export`（限 `mcp.export_roots`）；
  UI `#/exports`。
- **验收**（`internal/export`，复用 `test/perf/pool_test.go` 的池 harness）：
  - 30 文件、3 成员、3 副本 → 每成员 `Calls("ReadRange")` 在 `N/3 ± 2`；`ReadBytes()` 总和 == Σ size。
  - 已 pin/hydrate 或 `cloudfs-local:` 的文件 → 每成员 `ReadRange == 0`。
  - k 块后崩溃、重开、恢复 → 成员 `ReadBytes()` 增量 == 剩余块。
  - 注入 `ENOSPC` 或 `st_dev` 变化 → 作业 `paused(disk)`、条目 `pending`，`resume` 后完成。
  - 无标记的 `--mirror` 被拒绝；有标记只删多余项。
  - 哈希不符 → 重试 2 次后 `failed`，作业继续，CLI 退出码 1。
  - 暖态规划：成员 `List == 0`。
  - `test/perf/export_test.go`：20 ms 延迟下 3 成员导出墙钟 ≤ 单成员 0.5×（唯一看时间的断言，断言的是并行度）。
  - 路由守卫、JSON 契约、CLI 与 web i18n 覆盖测试全绿。

### [x] T-33 放置 v2：按路径规则、成员 class、故障域、配额满换盘、修复服务端复制、rebalance（`docs/pool-v2.md` §6，阶段 E）

- **证据**：`replicas` 是池级一个整数，无按路径策略、无故障域（两份副本可能落在同一账号的两个 remote 上）；
  `repair.go` 的 `repairTarget` 是与 `placement.go` `candidates()` 不一致的第二套策略；provider 无配额哨兵，
  网盘 507/403 经 `retry.Classify` 退避到死信而不是换盘；修复只走本机字节拷，同账号也不用 `ServerCopier`；
  无 rebalance/backfill，加盘只影响新写；`min_replicas` 无人消费（T-27 已把文档改诚实）；`TrimOnce` 按声明顺序最后裁；
  `health.go` 串行跑 repair/replay/drain 且忽略 `repair_concurrency`。
- **做法**：`pools.<n>.rules[{prefix, replicas, prefer, avoid, require}]`、`members[].class`、`failure_domain: account|provider|member`，
  `targetFor(path)` 取代 `replicaTarget()` 全部调用点，`repairTarget` 合并进 `candidates()`；`provider.ErrQuotaExceeded` +
  `retry.ClassQuota` + `ErrRestartUpload`，池 `markFull(10m)` 换候选、uploader 清 session 立即重试；新表 `member_usage`；
  `copyReplica` 同域优先 `ServerCopier.Copy`，模糊失败先 `ScrubPath` 再采纳；`write_mode: relaxed`（`min_replicas` = 告警阈值）
  | `strict`（同步补齐、超时仍成功）；`pool/rebalance.go` + `rebalance_queue`，`PlanRebalance(target_skew)`、`RebalanceOnce`
  独立 goroutine + `Pool.SetBusy`、`auto_backfill`；`TrimOnce` 改裁放置分数最差的副本；`dropReplica` 与 drain/trim 共用。
  CLI `pool rebalance [--target-skew] [--dry-run] --confirm`；控制面 `POST /pool/rebalance`；UI 填充条。
- **验收**：
  - `placement_test`：最长前缀规则；`prefer`/`require`/`avoid` 语义；同账号两成员 + 另一账号一成员时第二份落到另一账号；
    配额错误 → 下一候选，`fullUntil` 内 `free()` 不重查。
  - `write_test`：`UploadPart` 返回配额错误 → session 清空、重新放置到另一成员、最终 1 个成功上传（fake 加 `QuotaAfterBytes`）。
  - `repair_test`：同域且 `ServerCopy` 时 `Calls("Copy") == 1` 且 `UploadPart == 0`；模糊失败后先 scrub、不重复拷；配额 → 换成员、不记分歧。
  - `rebalance_test`：3 成员、2 副本、30 文件在 a/b，加 c → 计划 ≈ 1/3，完成后 `skew ≤ target`；先拷后删；busy 为真不推进；裁剪不碰新副本。
  - `test/perf/pool_test.go`：`TestRebalanceCostIsOneUploadPlusOneDeletePerMove`、`TestPlacementSpreadsAcrossDomains`。
  - `test/chaos`：rebalance 中途成员失联 → 队列条目退避、无副本丢失；两机同时 rebalance 同一文件 → 内容相同即良性。
  - `cloudfs doctor` 列出 `below min_replicas` 与 rebalance 积压。
- **进度**：§6.1 的配置与目标口径已落地——`config.PoolRule` / `PoolMember.Class` / `failure_domain` / `write_mode` /
  `min_replicas_timeout` / `rebalance` 解析与校验（`internal/config/pool_rules.go`），编辑器 `AddPoolRule` /
  `RemovePoolRule` / `SetPoolMemberField` 与 `SetPoolField` 白名单扩展，`marker.Settings` 带上 `Rules` / `FailureDomain`，
  daemon 经 `_pool_domains` 注入每个成员的故障域身份，`targetFor(path)` / `wantReplicas(path)` 已取代
  `replicaTarget()` 在 scan / repair / trim / drain / write / `Availability` 的全部按路径调用点
  （`internal/pool/rules.go` + `rules_test.go`、`marker_settings_test.go`、`daemon/pool_domains_test.go`）。
  §6.2 也已落地：`candidates()` 按「已持有 → `require` 过滤 → `prefer` → 故障域不重复 → 不在 `avoid` →
  不是 out → 已知空间 → `free × weight` → weight → 声明顺序」排序，`repairTarget` 收敛成
  `candidates(path) − 已持有活副本`（`internal/pool/placement.go`、`repair.go`）。
  §6.3 配额满换盘已落地：`provider.ErrQuotaExceeded` / `ErrRestartUpload` 哨兵 + `retry.ClassQuota`，
  gdrive / onedrive / webdav / aliyun / sftp / smb 六个驱动映射（各带 `UNVERIFIED:`，总数 78 → 83），
  池 `member.markFull(10m)` + `candidates()` 的 full 档 + `BeginUpload` 换候选、`UploadPart` / `CompleteUpload`
  返回 `ErrRestartUpload`、uploader `case retry.ClassQuota` 清 session 立即重试（无 restart 则死信），
  新表 `member_usage`（触发器维护，`meta.schema_version = 2` 迁移重算）取代放置时的 `SUM(size)`。
  §6.4 修复走服务端复制已落地：同 `Domain` 且 `Caps.ServerCopy` 时 `copyReplica` 先试 `ServerCopier.Copy`，
  `ErrUnsupported` / `ErrNotFound` 回退字节拷，模糊失败标记 `server-copy-unsure` 并在下次尝试前 `ScrubPath`。
  §6.5 `min_replicas` 诚实化已落地：`relaxed`（默认）把它当告警阈值——`ScanOnce` 用 `below min_replicas`
  入队（优先级 2）、`Availability.BelowMin`、`Report.BelowMin`、`pool status` 与 `doctor` 各一行；
  `strict` 在 `finishUpload` 之后（不持索引锁）同步跑 `repairPath`，deadline `min_replicas_timeout`，
  超时仍返回成功。`docs/pool.md` 的表格与写路径描述已同步改写。
  §6.6 rebalance 与 backfill 已落地：`internal/pool/rebalance.go` + `rebalance_queue` 表，
  `PlanRebalance(targetSkew, dryRun)`（最满 → 最空、大文件优先、只搬规则允许的、搬一半差值）、
  `RebalanceOnce`（先拷后删、按索引校验、`pause_between`）、`Pool.SetBusy`（daemon 接 `fsys.Busy`）、
  `backfillIfNeeded`、`dropReplica` 被 drain / trim / rebalance 共用、`TrimOnce` 改裁放置分数最差的副本、
  health.go 里独立 ticker、`Report.Rebalance` + `POST /pool/rebalance` + `cloudfs pool rebalance` +
  doctor 一行。测试：`rebalance_test.go`、`test/perf` 的 `TestRebalanceCostIsOneUploadPlusOneDeletePerMove`
  与 `TestPlacementSpreadsAcrossDomains`、`test/chaos` 的 `TestRebalanceLosesNothingWhenAMemberGoesAwayMidPlan`。
  `rebalance.max_rate` 与 `pause_between` 都已接线（搬完一个文件后按字节数补齐应花的时间再推进下一个），
  UI 池页加了平衡度卡片与「Rebalance」按钮（先 `dry_run` 看计划再确认）。T-33 的代码面至此完成，
  剩下的是需要真实账号才能验证的驱动配额信号（各处 `UNVERIFIED:`）。

## P4 — Agent 工作底座（2026-09-14 登记）

对照 WorkBuddy / 库库AI / OpenClaw 核对出的缺口：P3 说"agent 可安全读写"是全品类空白，
但今天 agent 能做的只是读写文件——没有会话、没有产物归宿、没有"谁动了什么"、没有撤销、
没有语义检索、没有记忆、文件到了也不会叫醒谁。本节把 CloudFS 从"agent 能挂上的网盘"
变成"agent 的工作底座"。完整设计见 `docs/agent-roadmap.md`，界面逐项清单见
`docs/ui-plan.md` 阶段 F。

**纪律**：每一条都是"后端 + 控制台界面"一起交付、一起验收。只有 MCP/CLI 没有界面的
条目不算完成——个人用户看不到的审计与回滚等于没有。

目标场景：个人桌面 Agent（Claude Code / Codex / OpenClaw），单用户单 daemon。
权限模型按会话级 scope 设计，团队场景只需给 principal 加 owner 字段。

分期：一期 = T-34 T-35 T-36（线 A）∥ T-44 T-37（线 B，T-44 前置）；二期 = T-38 T-39 T-40 T-41 T-42；
T-43 是先于二期回滚的验证缺口。T-44 于 2026-09-15 追加。

---

### [x] T-34 Agent 底座：agent.db、会话作用域、审计日志（一期，2026-09-15 完成）

- **2026-09-15 完成**（`feat/agent-phase1`，线 A 提交 e4782fc…59721e8 与 927c0e9、a9e1c44）：
  新包 `internal/agent`（`agent.db` schema v1：`principals`/`sessions`/`audit`，WAL，任何进程可追加，
  `agent.lock` 只决定谁跑保留期清理；`Scope{Read,Write,ReadOnly,ExpiresAt,Sandbox}` 的 `Check/Narrow/Covers`，
  `Narrow` 只收窄，交集为空即刻过期）；`mcpsrv` receiving middleware 解析会话（stdio 一进程一 principal、
  legacy HTTP 按 `Session.ID()`、无状态 HTTP 按 bearer principal + 30 min 空闲轮转），33 个工具 36 处调用点
  改为 `checkPath(ctx, p, write)`，`delete/move/copy` 两端按写检查，订阅注册同一 `Scope.Check`；审计 middleware
  拦 `tools/call` 与 `initialize`/`server/discover`/`resources/subscribe`/`subscriptions/listen`，`args` 脱敏
  ≤ 4 KiB、不含直链与令牌，写失败只计 `cloudfs_audit_write_failures_total`；控制面 `GET /audit`、`GET /sessions`
  （`?state&sandbox&path`）、`GET /sessions/{id}`、`POST /sessions/{id}/finish`，SSE `audit`/`session`；
  CLI `cloudfs audit`、`cloudfs sessions list|show|finish`（离线只读）；界面 `#/agents` 会话/审计标签、
  导航徽标、会话详情浮层、`scope_view.js`。
  与原计划不同之处：`Options.Sessions != nil` 且 ctx 无会话时 scope **fail-closed**（不回退全局 scope，
  否则 middleware 顺序错误不可观测）；go-sdk 1.7 新握手是 `server/discover`，`initialize` 只见于旧客户端。
- **验收证明**：现有 mcpsrv 测试零修改通过（A2 提交 `git diff --stat` 为空）；`TestScopeNarrowNeverWidens`
  （400×400 组合矩阵）；`TestDeniedDeleteIsAuditedWithoutContent`、`TestLargeWriteAuditArgsStayBounded`、
  `TestAuditFailureDoesNotFailTheTool`、`TestAuditWriteFailuresMetric`；`TestEveryRouteIsEitherGuardedOrArguedOpen`
  自动覆盖新路由，`TestFinishSessionWithoutControlHeaderIs403`；界面 `ui_agents_test.go` 全部、
  `_tests/scope_view.test.mjs`、i18n 两表一致；e2e `TestAgentTokenSandboxChainAndAuditInTheBrowser`
  （越界写 → `/audit` denied → 浏览器 `#/agents?tab=audit` 有 `data-result="denied"` 行，真实 Chromium）。
- **遗留**：~~stdio 会话在进程退出后仍 `active`，只靠 `mcp.session.idle` 过期~~（2026-09-15 C0 修：`cmdMCP`
  在传输结束时调 `Server.FinishStdioSessions`，`Sessions.FinishConn` 按连接键结束）；非 owner stdio 进程写进 `agent.db`
  的审计行不进 owner 的 `Store.Watch`（SSE 看不到，刷新可见，见 T-43）；旧协议客户端 `initialize` 无 `Extra`
  会多出一条 `http-legacy` 幽灵会话（`session_mw.go` 的 `onInitialized` 应先查 `legacyByPrincipal`）；
  `Scope/Principal/Session.ExpiresAt` 的 `omitempty` 对结构体无效（前端已按零值当缺失处理，后端可改 `omitzero`）。

- **证据**：`internal/mcpsrv/server.go:149` 的 `checkPath` 不分读写，`Options.Allow`
  进程全局；`internal/mcpsrv/http.go:65` 新版传输 `Stateless: true`，每个 POST 一个 SDK
  session，SDK 会话不能当身份；`docs/vfs-changes.md` 明写 `vfs.Change` 不是审计。
  全仓库无任何持久操作记录。
- **做法（后端）**：
  - 新包 `internal/agent`：`<cache.dir>/agent/agent.db`（独立 SQLite WAL，不动 meta v11 /
    journal v13），表 `principals`、`sessions`、`audit`。审计行任何进程都可追加；`agent/owner.lock`
    只守护 GC、保留期清理、回滚与触发 worker（见 `docs/agent-roadmap.md` §9.1）。
  - `Scope{Read, Write, ReadOnly, ExpiresAt, Sandbox}`，`Check(p, write)` 取代
    `checkPath + checkWrite`，`Narrow` 只能收窄。`Options.Allow/ReadOnly` 转 `defaultScope`，
    `Options.Sessions == nil` 行为同今天。
  - mcpsrv receiving middleware：解析会话（stdio 一进程一 principal，`InitializedHandler`
    用 `ClientInfo` 建隐式会话；legacy HTTP 用 `Session.ID()`；无状态 HTTP 用 bearer
    principal + 空闲 30 分钟轮转）→ `agent.WithSession(ctx)` → 全部工具（当前 33 个，36 处调用点）改
    `checkPath(ctx, p, write)`（delete/move/copy 两端按写检查）；订阅注册同一 `Scope.Check`。
  - 审计：拦 `tools/call`（+ initialize、订阅注册），`args` 脱敏（`content`/`new_text` →
    `{bytes:n}`，≤ 4 KiB，不含直链/token/cookie），同步单条 INSERT，失败只记日志 +
    `cloudfs_audit_write_failures_total`；保留 `mcp.audit.retain`（默认 90 天）。
  - 控制面 `GET /audit`、`GET /sessions`、`GET /sessions/{id}`、`POST /sessions/{id}/finish`；
    SSE `audit`、`session` 事件；CLI `cloudfs audit`、`cloudfs sessions list|show`（离线 ro）。
- **界面**：
  - 导航新增「Agent」`#/agents`（`screens/agents.js`，图标 `bot`），导航项徽标显示活动会话数。
  - **会话标签**：顶部三张卡（活动会话、今日写操作、今日拒绝次数）；表格列 客户端 / 作用域摘要
    （`scope_view.js`：读 `/work`、写 `/work/.agent/…`、只读、过期时间）/ 状态点+文字 / 开始时间 /
    写操作数；分页 `moreRow`；SSE `session` 事件在未翻页时刷新。行点击打开会话详情浮层
    （`session_panel.js`，一期内容：作用域、客户端、审计尾巴 50 条、"结束会话"按钮）。
  - **审计标签**：过滤条（会话、工具、结果 ok/denied/error、时间 1h/24h/7d），表格列 时间 / 客户端 /
    工具 / 路径（多路径折叠）/ 结果（denied 红底行，同诊断屏配色）/ 字节 / 耗时；`args`
    点击展开为只读 JSON（已脱敏）；SSE `audit` 在未翻页、无过滤时插入顶部。
  - `store.js` 不缓存审计行（大且无共享价值），每屏自己持有。
- **验收**：
  - `--allow /work` 且不启用 sessions 时，现有 `internal/mcpsrv` 测试零修改通过。
  - `Scope.Narrow` 表驱动测试覆盖 父/子/兄弟/根/`..`/尾斜杠，放大必失败。
  - 被拒绝的 `delete` 在 `GET /audit` 有 `result=denied`、`paths` 正确，`args` 不含 `content` 原文、不含 `http`。
  - `write_file` 200 KiB 内容，审计 `args` ≤ 4 KiB，`bytes_in=204800`。
  - agent.db 写失败注入点打开时工具照常成功，metrics +1。
  - `security_all_routes_test` 覆盖新路由；`POST /sessions/{id}/finish` 无头 403。
  - 界面：`ui_agents_test.go` 断言 `agents.js` 被嵌入、`#/agents` 在 `routes` 与 `navItems`、
    会话表调用 `/sessions`、审计表调用 `/audit` 并跟随 `next_cursor`；denied 行有文字标签不只靠颜色；
    `_tests/scope_view.test.mjs` 覆盖作用域摘要；i18n 两表键一致、screens 无汉字。
  - e2e：MCP 调一次越界写 → 浏览器冒烟（`CLOUDFS_BROWSER=1`）打开 `#/agents` 审计标签可见 denied 行。

### [x] T-35 访问令牌与 HTTP 接入（一期，2026-09-15 完成）

- **2026-09-15 完成**（线 A 提交 ede1a74、392de08、ccb0ec0）：`internal/agent/token.go`（`cfs_` 前缀 32 字节随机
  令牌，只存 `sha256` 与 4 位指纹，名字在活令牌中唯一，写前缀必须在读前缀内，过期随 scope 走）；`requireAuth`
  基于 go-sdk `auth.RequireBearerToken`（环境变量令牌保持全权，签发令牌查表，过期/吊销/未知 401），
  `requireBearer(next, token)` 签名保留；吊销 2 s 内关闭该 principal 的有状态会话并把其活动会话置 `expired`；
  CLI `cloudfs mcp token create|list|revoke`（明文只打印一次）；`cloudfs mcp install --client claude|codex
  --transport http [--url --token]`（Codex 片段带 `UNVERIFIED:`）；stdio 非 owner 打警告，会话工具返回
  `requires the storage owner; use the HTTP transport`；控制面 `GET /mcp/connect`（不含令牌）、`GET|POST /mcp/tokens`
  （POST `Cache-Control: no-store`）、`POST /mcp/tokens/{id}/revoke`（confirm）；界面令牌标签、只显示一次的揭示浮层
  （不 import store、无 localStorage）、接入面板、首次设置完成页"连接 Agent"卡片。
- **验收证明**：`TestTwoTokensSeeDisjointTrees`（并发 stat 互斥，订阅拒绝经审计行验证——新客户端的 Subscribe 是
  fire-and-forget）、`TestExpiredTokenInitializeIs401`、`TestRevokeClosesLegacySessionWithin5s`；
  `TestClaudeAddCommandGolden` + 本机真机 `claude` CLI 2.1.272 `claude mcp add …` 接受并 `✔ Connected`；
  `TestCreateTokenResponseIsNoStore`、`TestTokensListNeverCarriesAPlainToken`（形状扫描）；
  `ui_tokens_test.go` 全部、`TestWebAppNeverAsksForACredential`。
- **遗留**：令牌作用域与 `mcp.allow` 不求交（`docs/mcp.md` 已按实际写明），若要求交在签发时 `Scope.Narrow`；
  `control.writeJSON` 改为 `SetEscapeHTML(false)`（为 `<token>` 占位符），是包级改动。

- **证据**：`requireBearer`（`internal/mcpsrv/http.go:165`）只认单一 env token，全权；
  stdio 客户端在 `cloudfs mount` 运行时是非 owner 独立 VFS，看不到内核写、会话与审计不共享；
  `cloudfs mcp install` 只输出 stdio 片段。
- **做法（后端）**：
  - `cloudfs mcp token create --name codex --read /work --write /work/.agent --ttl 720h | list | revoke`；
    `principals` 存 `sha256(token)`，明文只打印一次；`requireBearer` 扩为"env token 全权（兼容）
    或查表"，过期/吊销 401；吊销时关闭该 principal 的 legacy 会话。
  - `cloudfs mcp install --client claude|codex --transport http`（`ClientConfig` 多一种输出）；
    `cmdMCP` 非 owner 时打警告，会话/回滚类工具返回 `requires the storage owner; use the HTTP transport`。
  - 控制面 `GET /mcp/connect`（HTTP 是否监听及地址、当前进程是否 owner、两种客户端片段，不含令牌）、
    `GET /mcp/tokens`、`POST /mcp/tokens`、`POST /mcp/tokens/{id}/revoke`（confirm）。
- **界面**：
  - 「Agent」屏 **访问令牌标签**：表格列 名称 / 指纹（前 4 位）/ 可读 / 可写 / 过期 / 最后使用 / 状态
    （有效、过期、已吊销，点+文字）；"新建令牌"`openForm`：名称、可读路径（多行，每行一个前缀）、
    可写路径、有效期（1 天/7 天/30 天/永不）、只读开关；前端先用 `scope_view.js` 校验"可写必须在可读内"。
  - **令牌揭示浮层**：令牌明文 + `copyBtn` + 醒目文字"关闭后无法再次查看" + Claude Code / Codex 两个
    HTTP 注册片段（含该令牌）各带复制按钮；关闭即丢弃，不进 `store.js`、不进 localStorage。
  - 吊销：`confirmDelete` 键入令牌名称 + `confirm: true`。
  - **接入面板**（Agent 屏顶部可折叠）：HTTP 监听状态点、地址；owner 状态；未启用 HTTP 时给出配置说明；
    stdio 与 mount 并存时黄色横幅"请改用 HTTP 传输"并链到 T-43 诊断项。
  - 首次设置完成页（`screens/setup.js`）加"连接 Agent"卡片，跳 `#/agents` 并展开接入面板。
- **验收**：
  - 同一 HTTP 服务上 token A（`/work`）与 token B（`/gd`）并发：A 对 `/gd/x` 的 `stat` 与
    `resources/subscribe` 都 denied，B 相反；表测试遍历 `tools/list`（当前 33 个工具）证明都过了 `checkPath`。
  - 过期令牌 `initialize` 得 401；吊销后 5 s 内其 legacy 会话被关闭。
  - `mcp install --transport http --client claude` 输出能被 `claude mcp add` 直接接受（快照测试）。
  - `POST /mcp/tokens` 响应头 `Cache-Control: no-store`；`GET /mcp/tokens` 响应体不含任何完整令牌（形状扫描）。
  - 界面：`ui_tokens_test.go` 断言揭示浮层模块不 import `store.js`、源码无 `localStorage`、
    吊销带 `confirm: true`；接入面板调用 `/mcp/connect`；`setup.js` 完成页含 `#/agents` 链接；
    全部嵌入字节无 `type="password"`。

### [x] T-36 交付箱：会话工作区、manifest、sandbox（一期，2026-09-15 完成）

- **2026-09-15 完成**（线 A 提交 9358c46、ec31e45、7c32b1a、0a51cdf）：`config.MCP.Workspace`（默认第一个 allow
  前缀 + `/.agent`）；会话目录 `<workspace>/<client>-<YYYYMMDD>-<sid8>/` 永不复用，唯一受管文件 `manifest.json`
  （`session_id, client, principal, started_at, finished_at, scope, summary, artifacts[]`）；MCP `begin_session
  {name?, sandbox?}` / `finish_session{session_id?, summary?, share?}` / `list_sessions`，产物来自审计表的成功写
  路径，`share=true` 只对已同步文件调 `FS.DownloadURL`（1 s 上限），`sandbox=true` 把 `Scope.Sandbox` 设为会话目录；
  界面：会话详情产物表（状态由 `/fs/stat` 与 SSE `change` 刷新，"复制链接"仅点击时请求且不入 DOM）、
  主窗口工作区与会话目录的 `bot` 标记（`aria-label`）、检查器"来自会话"链接（`GET /sessions?path=`）、
  会话表"产物"列与"仅沙箱"过滤；`?path=`/`?dir=` 深链。
- **验收证明**：`TestConcurrentSessionsKeepSeparateManifests`、`TestSandboxSessionCannotWriteOutsideItsDirectory`
  （denied + 审计 `result=denied` + 目录内写成功 + 读成功）、`TestShareSkipsLocalFilesWithoutALinkCall`
  （`Calls("DownloadURL") == 0`，≤ 1 s）、`TestBeginSessionWithoutWorkspaceIsAConfigError`（`Calls("Mkdir") == 0`）；
  `ui_sessions_test.go` 全部、`_tests/workspace_view.test.mjs`；e2e 真实挂载 `TestAgentSessionManifestMatchesTheMount`
  （manifest 与 `ls` 一致）与浏览器冒烟会话浮层恰两行产物。
- **遗留**：`begin_session` 的 `snapshot?` 参数留给 T-38；`app.css` 顺带修了全局 `appearance:none` 让所有复选框不可见
  的老问题（`input[type=checkbox]{appearance:auto}`）；`Artifact.ExpiresAt` 已改 `omitzero`。

- **证据**：agent 产物散落在调用方自己选的路径，无约定、无清单、无分享；`--allow` 无法表达
  "只能写自己的目录"。
- **做法（后端）**：
  - `config.MCP.Workspace`（默认第一个 allow 前缀 + `/.agent`；allow 为空且未配置时报配置错误）。
  - 每会话目录 `<workspace>/<client>-<YYYYMMDD>-<sid[:8]>/`，永不复用；唯一受管文件 `manifest.json`
    （`session_id, client, principal, started_at, finished_at, scope, summary, artifacts[{path, uri,
    size, sha256?, state, download_url?, expires_at?}]`）。不做共享 index.json。
  - MCP `begin_session{name?, sandbox?}`、`finish_session{session_id?, summary?, share?}`、
    `list_sessions`。产物清单来自审计表的写路径；`share=true` 仅对已同步文件调 `FS.DownloadURL`，
    `local` 记 pending 不等待。不加 `write_artifact`，产物用现有写工具。
  - `sandbox=true`：`Scope.Sandbox` = 会话目录，写收窄，读不变。
- **界面**：
  - 会话详情浮层增加 **产物表**：路径 / 大小 / 状态（已同步、上传中，点+文字，SSE change 事件刷新）/
    动作（"在文件中打开"跳主窗口并选中；"复制链接"按需调 `/fs/download-url`，不渲染进 DOM）；
    摘要文本；sandbox 标记；"打开工作区目录"按钮。
  - 主窗口：工作区根目录与会话目录在文件表名称旁显示 `bot` 小标记；检查器对会话目录内文件显示
    "来自会话 <client>-<日期>"链接，点击打开会话详情浮层（`GET /sessions?path=` 反查）。
  - 「Agent」屏会话表增加"产物数"列与"仅 sandbox"过滤。
- **验收**：
  - 两个并发会话各写同名文件，finish 后两份 manifest 完整、互不引用对方文件。
  - sandbox 会话写 `/work/notes.md` denied 且审计 `result=denied`；会话目录内写成功；读 `/work/notes.md` 成功。
  - `share=true` 且文件仍 local：manifest `state=local` 无 `download_url`，工具 ≤ 1 s 返回，fakeprovider `Calls("DownloadURL")` 为 0。
  - allow 为空且未配 `workspace` 时 `begin_session` 报配置错误，不建目录。
  - 界面：`ui_sessions_test.go` 断言产物表"复制链接"只在点击时请求 `/fs/download-url`、结果不写入表格
    DOM；检查器"来自会话"链接调用 `/sessions?path=`；主窗口工作区标记用 `bot` 图标且带 `aria-label`。
  - e2e：真实挂载 `begin_session` → 写两文件 → `finish_session`，终端 `cat manifest.json` 与 `ls` 一致，
    浏览器冒烟会话详情产物表两行。

### [x] T-37 内容索引 phase 1：抽取 + FTS + semantic_search（一期，2026-09-15 完成）

- **2026-09-15 完成**（`feat/agent-phase1`，线 B 提交 955727e…ce7bc7a）：`internal/textract`（文本类原样保 CRLF、
  docx/xlsx/pptx 经 `archive/zip`+`encoding/xml` 流式解析并有条目数/单条/总量三重上限、PDF 经 `ledongthuc/pdf` +
  recover + 超时 + 乱码启发式并标 `UNVERIFIED: 中文 PDF 抽取质量`、800 rune/100 重叠标题感知分块）；`internal/index`
  （独立 `<cache.dir>/index.db` v1：`index_meta/rules/documents/chunks/chunks_fts(trigram, external content)/index_pending`，
  身份与 meta 对账不符即重建；规则 `**` glob 自写、全局 exclude 默认含 `.env/*.pem/id_rsa*/.git/node_modules`、
  每小时抓取预算且 `Caps.Tier=unofficial` 双倍计费；Indexer 消费 `FS.WatchChanges()`、启动 30 s 与每 10 min 对账、
  轮询 `FS.Busy()` 让路、`ErrRiskControl` 休眠 15 min、pinned 模式零 provider 调用、目录改名走 `RenamePrefix`
  不重抽；检索 FTS `bm25()` + 范围 + `stale` + 字节预算，< 3 rune 词走预算 LIKE 扫描）；MCP `semantic_search`
  （hybrid/vector 降级 keyword 并带 `degraded`）、`index_status`、`index`、`unindex`、`read_extracted_text`，
  每 hit 过 `visible`；控制面 `/index/status|rules|add|remove|rebuild|retry|failed|search|text`（remove/rebuild
  confirm），SSE `index`（1 s 节流），CLI `cloudfs index status|rules|add|rm|rebuild|retry|search`，metrics
  `cloudfs_index_*`，doctor `index_db/index_identity/index_failed/index_text_budget`；界面 `#/index` 屏（未启用时只显示
  说明与配置示例、四张卡、进度条、规则表、失败表、重建）、主窗口"内容"分段（`content_search.js`、`snippet.js`）、
  检查器索引行与三个动作、抽取文本浮层。
- **验收证明**：`TestIndexToolsAbsentWhenDisabled`、`TestIndexDisabledCreatesNoIndexDB`、`TestIndexStatusWhenDisabled`；
  `TestIndexPinnedFilesCostNoReads`（500 pinned 文件 `Calls("ReadRange")` 增量 0）、`TestIndexRulesFetchOncePerVersion`
  （100 → 0 → 1）；`TestDocxHeading1BecomesAMarkdownHeading`、`TestDocxHeadingIsReturnedWithTheHit`、
  `TestMarkdownHitOffsetFeedsReadText`；`TestRenamePrefixKeepsIndexedAt`、`TestRenameKeepsIndexedAtAndUpdatesThePath`；
  `TestSearchNeverLeaksOutsideRoots`、`TestSemanticSearchRespectsAllow`、`TestSemanticSearchRespectsTokenScope`；
  `TestTwoRuneChineseQueryReturnsHitsOrTruncated`、`TestShortQueryScanStopsAtItsBudget`；
  `TestZipWithTooManyEntriesFails` + chaos `TestMaliciousArchiveFailsTheDocumentNotTheDaemon`、
  `TestIndexSurvivesAnUncleanStopMidExtraction`（运行中拷 db/wal/shm 作崩溃镜像，`IntegrityCheck=="ok"`，续跑）；
  `TestBusyForegroundMakesTheWorkerYield`；界面 `ui_index_test.go`、`ui_content_search_test.go`、
  `_tests/index_presets.test.mjs`、`_tests/snippet.test.mjs`（高亮、CJK 不切半字、HTML 当文本）；e2e 真实挂载
  `TestContentSearchFindsAFreshMarkdownFile`（3 s 内命中且 `start_off` 喂 `read_text` 同前缀）、浏览器
  `TestContentSearchInTheBrowser`（真实 Chromium headless shell）。
- **遗留**：pinned 范围由 `FS.PinPolicies()` 推导（unpin 后仍完整缓存的文件不再索引）；`index_status` 无 `path` 时只给
  全局计数且失败列表截 20 条；`SearchQuery.Roots` nil = 不限制、空 = 全拒（同 `meta.SearchWithin`）；
  `Current` 签名 `(path, version, ok)`；`go mod tidy` 把 `webview_go`/`cgofuse`/`x/sync` 提为直接依赖；
  中文 PDF 真实样本与 quark 风控是否以 `ErrRiskControl` 到达 worker 待真机（`UNVERIFIED`）；嵌入/hybrid 见 T-39。

- **证据**：`internal/meta/schema.go` 只有文件名 trigram 索引；`search.content` 只扫完整缓存文件
  前 `MaxBytes`；PDF/docx/xlsx 对 agent 不可读；全仓库无文档解析代码。
- **做法（后端）**：
  - 独立 `<cache.dir>/index.db`（不进 meta：meta 单写者，FLUSH 等它的锁）：`index_meta`、`rules`、
    `documents`（键 `(remote, remote_id)` + `version`、`text`、`text_hash`、`extractor_ver`、`chunker_ver`、
    `state`）、`chunks`、`chunks_fts`（trigram，external content）、`index_pending`。
  - 范围：`index.enabled` 默认 false；`pinned: true` 只处理 `cache.Complete` 为真的文件（零 provider 调用）；
    `rules[]` 经 `vfs.ReadFileRange` 拉取，受三维限流/熔断、`fetch_budget`（unofficial 减半）、
    `ErrRiskControl` 休眠 15 min；全局 exclude 默认含 `.env`、`*.pem`、`id_rsa*`、`.git`、`node_modules`。
  - `internal/textract`：文本类原样（保 CRLF，offset 即文件偏移）；docx/xlsx/pptx 用
    `archive/zip + encoding/xml`（zip 条目/总量/条目数上限）；PDF 用 `github.com/ledongthuc/pdf` +
    recover + 30 s 超时 + 乱码启发式，`UNVERIFIED: 中文 PDF 抽取质量`；不做 OCR。
    分块 800 rune / 重叠 100，标题感知，记 `seq/start_off/end_off/heading`。
  - `internal/index`：订阅 `FS.WatchChanges()`；启动 30 s 与每 10 min 本地对账（新增只读
    `meta.WalkSubtree`）；提取 worker 轮询 `FS.Busy()` 让路；检索 FTS `bm25()` + 范围过滤 + `stale` +
    字节预算，< 3 rune 查询 LIKE 预算扫描。
  - MCP `semantic_search{query, path?, top_k?, mode?, max_snippet_bytes?}`（hybrid/vector 此期降级
    keyword 并带 `degraded`）、`index_status`、`index`、`unindex`、`read_extracted_text`；每 hit 过 `checkPath`。
  - 控制面 `/index/status|rules|add|remove|rebuild|retry|failed|search|text`；SSE `index`；
    CLI `cloudfs index status|add|rm|rebuild|search`；metrics `cloudfs_index_*`；doctor 检查 index.db 与身份。
- **界面**：
  - 导航新增「索引」`#/index`（`screens/index.js`，图标 `layers`）。未启用时整屏显示说明与配置示例，
    不渲染空表。
  - **概况卡**（照缓存屏四卡）：文档（正常/待处理/失败）、分块数、文本占用 / `max_total_text`、
    本小时下载 / `fetch_budget`；SSE `index` 事件驱动进度条"正在抽取 N / 待处理 M"，worker 让路或
    风控休眠时显示原因与恢复时间。
  - **规则表**：路径 / 包含 / 排除 / 单文件上限 / 来源（配置、界面）/ 已覆盖文档数 / 动作；配置来源只显示
    "在配置文件中修改"（同缓存屏 pin 做法），界面来源可"移除"（`confirmDelete` 键入路径）。
    "添加规则"`openForm`：路径、包含 glob（预设"文档"/"代码"/"全部文本"）、单文件上限；
    表单下方常驻提示"会按限流下载该目录下匹配的文件；非官方接口网盘建议改用固定"。
  - **失败文档表**：路径 / 类型 / 错误（如"PDF 无可抽取文本"）/ 时间 / "重试"；分页。
  - "重建索引"按钮：`confirmDelete` 键入 `rebuild` + `confirm: true`。
  - **主窗口搜索框**：左侧分段切换"文件名 / 内容"（仅 `status.index.enabled` 时出现，选择存 localStorage）；
    内容模式调 `/index/search`，结果行：文件图标 / 路径 / 标题路径（"第二章 > 2.1"）/ 片段（`snippet.js`
    高亮查询词，截断 240 字符）/ "可能已过期"标记（`stale`）；点击行打开"抽取文本"浮层并滚动到
    命中段；`truncated` 与 `degraded` 为结果内常驻说明行（同 `search.truncated`）。
  - **检查器**：新增"索引"信息行（已索引 · N 块 / 待处理 / 失败原因 / 未覆盖）；动作"加入索引"
    （`POST /index/add` 单文件或目录）、"移出索引"（仅界面来源规则）、"查看抽取文本"（`showPanel`，
    分页读 `/index/text`，PDF/docx 也可读，底部"加载更多"）。
- **验收**：
  - `index.enabled: false`：MCP `tools/list` 无 5 个索引工具，`index.db` 不存在，控制面 `/index/status`
    返回 `enabled:false`。
  - `pinned: true` 索引 500 个已 pin 文件，fakeprovider `Calls("ReadRange")` 增量为 0。
  - `rules` 覆盖 100 文件：首轮对账 `Calls("ReadRange")` 覆盖 100 个文件，第二轮增量 0；改 1 个版本后第三轮只涉及该文件。
  - docx `Heading1` 进 `chunks.heading`；`.md` hit 的 `start_off` 传 `read_text(offset)` 得同前缀。
  - 目录改名后 hit 路径立即为新路径且 `documents.indexed_at` 不变。
  - `--allow /work` 时 `/private` 下 chunk 永不出现在 hits。
  - 2 字中文查询返回结果或 `truncated`，不会空着说"没有匹配"。
  - 恶意 zip（条目 > 4096）→ `failed`，daemon 不 panic；索引中 `kill -9` 重启后 index.db 完整、pending 续跑。
  - `FS.Busy()` 持续为真时提取 worker 5 s 内至少让路一次。
  - 界面：`ui_index_test.go` 断言 `#/index` 路由与导航项、未启用时不请求 `/index/rules`、规则移除与重建
    带 `confirm: true`、配置来源规则无移除按钮；`main.js` 搜索切换只在 `index.enabled` 时渲染、内容模式
    调 `/index/search` 且渲染 `degraded`/`truncated` 行；检查器三动作分别调用对应路由；
    `_tests/snippet.test.mjs` 覆盖高亮、CJK 截断不切半字、HTML 转义（片段来自文件内容，必须当文本插入）。
  - e2e：真实挂载写 md → 3 s 内 `semantic_search` 命中；浏览器冒烟主窗口内容搜索命中同一文件。

### [x] T-38 会话快照与回滚（二期，2026-09-15 完成）

- **2026-09-15 完成**（线 C 提交 0bfea3d、cfe49b6 与本条收口提交）：agent.db v2 的 `session_ops` 表（P0）+
  `internal/agent/ops.go` DAO（`RecordOp/CompleteOp/AbandonOp/OpsOf/LinkOpsToAudit/SessionsTouching`）；
  `internal/agent/preimage.go`：写工具调 VFS **之前** `Capture`（`StatPath` → absent/dir/file；文件 ≤
  `mcp.session.max_preimage_bytes`（默认 32 MiB）经普通读路径读满 → sha256 → 优先 `os.Link` 块缓存的
  hydrated 文件到 `<cache.dir>/agent/preimages/<sha256>`，否则把已读字节经 `ReserveDisk` 记账写副本；
  **链接成功才写行**；留不住只记 `pre_reason=too_large|not_cached`，写照常执行）、`Recover` 清孤儿 blob、
  `GC` 按 `mcp.session.retain`（默认 7 天）回收已结束会话的行与 blob，daemon owner 启动时 `Recover`、每小时 GC；
  `internal/agent/rollback.go`：`Sessions.Rollback(ctx, fs, pre, id, dryRun)` 按 `seq` 逆序、前置检查照
  `docs/agent-roadmap.md` §4.8 的表（覆盖/编辑/追加比对**写入内容的 sha256**（`post_version` 记
  `sha256:<hash>`，上传后版本号变了不算他人改过）、新建内容未变才删、mkdir 只删空目录、改名要求原路径为空、
  删除要求路径为空且有前像）、回滚本身是名为 `rollback of <id>` 的新会话并同样记前像（可再回滚）、`dry_run`
  零写入、已恢复行重跑报 `skipped: already` 且**不覆盖原记录**（C3 chaos 发现并修：重跑曾把已恢复行改写成
  `rolled_back=0`，第三次运行会再次尝试并报 conflict）；`FSOps` 最小接口 + `VFSOps` 适配；
  `internal/mcpsrv/preimage.go`：`beforeWrite` 钩子进 `write_file`/`edit_file`/`create_directory`/`move`/`copy`/
  `delete`（递归删目录记 `dir`）、审计中间件把行链到 `audit_id`、`rollback_session{session_id, confirm, dry_run?}`
  （`DestructiveHint`，非 owner 拒绝，只回滚本 principal 的会话）；控制面 `POST /sessions/{id}/rollback
  {dry_run|confirm}`（`confirmed("confirm.rollback")`）、`GET /sessions/{id}` 带 `ops[]`、`GET /sessions?path=&since=`；
  CLI `cloudfs sessions rollback <id> --dry-run | --confirm [--json]`；`Session.State` 加 `rolled_back` 与
  `rolled_back_at`。界面（F5）：会话详情操作表（序号/操作/路径（改名旧→新）/前像点+文字/回滚结果，路径走文本节点）、
  "回滚此会话"（`undo`）→ 先 `dry_run` 预览浮层（`rollback_plan.js` 三组 + 承诺三句）→ `confirmDelete` 键入
  短 ID → `confirm:true` → 结果浮层 + "回滚这次回滚"；`agents_sessions.js` 状态"已回滚"与行内回滚；主窗口
  检查器 `agent_touch.js`"被 Agent 修改 · client · 时间"（`GET /sessions?path=`）点击打开会话详情。
  `docs/mcp.md`"会话回滚"一节写了承诺三句话与逐条前置检查。
- **验收证明**：
  - 覆盖 1 MiB 已缓存文件后回滚得原内容、fakeprovider 下载 +0：`TestRollbackSessionRestoresContentWithoutRedownload`
    （`ReadRange`/`DownloadURL` 增量 0，排空后网盘内容为原文）；未缓存文件 +1 次且读取字节 ≤ 文件大小、回滚本身 0 次：
    `TestRollbackSessionOnUncachedFileDownloadsOnce`；真实挂载 `TestRollbackRestoresWhatTheShellSees`
    （终端写 1 MiB → MCP 覆盖 → 终端 `cat` 新内容 → 控制面 `dry_run` → `confirm` → 终端 `cat` 原内容，全程
    `ReadRange`/`DownloadURL` 增量 0，排空后网盘为原文，原会话 `rolled_back`，回滚会话自己的 `dry_run` 报 1 条可恢复）。
  - 会话写 → 内核路径再改同文件 → 回滚 conflict 未覆盖、其余 restored：`TestRollbackSkipsConflictingFiles`（agent）、
    `TestRollbackSessionReportsConflictsWithoutOverwriting`（mcpsrv）、e2e `TestRollbackRestoresWhatTheShellSees`
    第三段（终端经内核改 `c1.txt` → `dry_run` 与 `confirm` 都报 `/c1.txt conflict: modified`、`/c2.txt restored`，
    `c1.txt` 仍是终端的内容，行的 `rollback_result` 分别为 `conflict: modified`/`restored`）。
  - `create → move → delete` 后回滚路径树与会话前一致：`TestRollbackRestoresInReverseOrder`（agent，fake 树逐项相等）、
    e2e `TestRollbackRestoresWhatTheShellSees` 第二段（真实挂载 `WalkDir` 的 "d/f 路径 内容" 列表逐项相等，
    排空后网盘也回到 `keep.txt`/`old.txt`）。
  - chaos：`test/chaos/rollback_chaos_test.go` `TestPreimageLinkThenCrashLeavesNoFalsePreimage`（真实 VFS + 块缓存
    hydrated 文件，`HookAfterLink` 在 `os.Link` 之后、插行之前 panic 模拟 kill -9 → 重开 owner `Recover` 恰删 1 个
    孤儿、无行、`SessionsTouching` 为空、缓存 hydrated 文件仍在且再读 0 次 `ReadRange`、之后正常捕获的 blob 不被误删）；
    `TestRollbackInterruptedIsIdempotent`（fake FSOps 在第 2 次写阻塞、取消 ctx → 重跑：树与会话前逐项相等、
    两轮合计 5 次写（每 op 恰一次）、4 行 `skipped: already`、原会话 `rolled_back`、第一轮回滚会话 4 完成 + 1 abandoned、
    第二轮只记 1 行、第三轮 0 写入）。`-race` 通过。
  - `dry_run` 零写入：`TestDryRunWritesNothing`（fake 写计数 0）、`TestRollbackRouteDryRunThenConfirm`、e2e 里 journal
    各状态行数之和与 `BeginUpload` 计数在 `dry_run` 前后不变。
  - 界面：`ui_rollback_test.go` `TestRollbackButtonPreviewsBeforeConfirming`（`dry_run: true` 先于 `confirm: true`，
    无直接 confirm 路径）、`TestRollbackConfirmTypesTheShortID`、`TestRollbackPlanGroupsAndPromise`、`TestSessionOpsRenderAsText`、
    `TestInspectorShowsAgentTouch`（调用 `/sessions?path=`）、`TestRollbackModulesStayShort`；`_tests/rollback_plan.test.mjs`
    分组与空计划。
  - e2e 浏览器：`TestRollbackInTheBrowser`（`CLOUDFS_BROWSER=1`，本机 Playwright Chromium 通过）：MCP 写 → 终端 `cat`
    新内容 → 打开 `#/agents?session=<id>` → 点 `[data-action=rollback]` → 预览浮层含路径与 `execute` → 键入短 ID →
    `.sheet.danger button.danger` → `[data-rollback=result]` 含 `rollback-again` → 终端 `cat` 原内容 → 排空后网盘为原文。
  - 顺带发现并修复（真实挂载才暴露）：`internal/fusefs` 的 `InvalidateFunc`/`InvalidateEntryFunc` 把 VFS ino 当内核
    nodeid 发 `InodeNotify`/`EntryNotify`，而 go-fuse v2.11 的 nodeid 按 lookup 顺序自行编号——任何"内核没 lookup 过的
    inode"（MCP/控制面写的文件、VFS 自己列举的目录）之后两者错位，MCP 改过的目录在内核 `FOPEN_CACHE_DIR` 里一直是旧
    列表（`ls` 看不到 create/move/delete，只有 `CLOUDFS_NO_DIRCACHE=1` 才对）。修法 `internal/fusefs/kernel_nodes.go`：
    ino → 内核持有的 `*fs.Inode` 注册表（`newInode` 登记、`OnForget` 注销、通知时按 `Forgotten()` 剔除 go-fuse 丢弃的
    候选），回调改走 `Inode.NotifyContent/NotifyEntry`；回归用例 `TestInvalidationReachesTheKernelNodeWhenInosDiverge`。
- **遗留**：
  - `rolled_back_at` 存在 agent.db `meta` 表的 `rolled_back_at:<id>` 键（二期冻结 schema，v3 迁移时才进 `sessions` 列）。
  - `post_version` 对内容写入是 `sha256:<hash>` 而非 provider 版本，`copy` 记目的地版本，mkdir/rename/delete 为空。
  - `delete recursive=true` 删目录只记 `pre_state=dir`，回滚报 `skipped: dir`（逐文件前像在三期）；mkdir 的 `dry_run`
    不能预知目录是否为空，按"将尝试"报 restored，真跑非空报 `skipped: not_empty`。
  - `GET /sessions?path=` 同时匹配"工作区包含该路径"的会话（`SessionsTouching` 只按 `session_ops`，检查器用后者）。
  - 回滚中途被打断时，那一轮的回滚会话以 `rollback interrupted: <原因>` 结束（2026-09-15 审查修：之前保持 `active`
    且 stdio/console 传输不轮转，永不过期也永不被 GC），其未落地的一步记 `write_failed`；重跑开新回滚会话。
  - 回滚仍 `active` 的会话（含 agent 自己的当前会话）先像 `finish_session` 一样结束它、再读 ops 行（2026-09-15 审查修：
    之前先读行再回滚，回滚期间连接上的写入会记进正被回滚的会话而逃过撤销）；连接随后的调用进入新会话——这与
    `finish_session` 的语义一致，沙箱本来就只约束一个 `begin_session` 会话而非 principal。
  - 硬链接前像与块缓存共享 inode，不计入 `cache.max_size`（只有拷贝走 `ReserveDisk`）：缓存淘汰了 hydrated 文件后，
    这些字节仍被前像目录占着直到保留期 GC；缓存吃紧时 `du <cache.dir>/agent/preimages` 才看得到。
  - 已执行过的回滚中 conflict 的行重跑不再重试（`skipped: <上次结果>`），要重试需先解决冲突再回滚"回滚会话"。
  - 上传后 `Version` 变化不算冲突是靠内容 hash；追加写在前像留不住时 `post_version` 为空（行本身因无前像报
    `skipped: not_cached`）；记了行但进程在工具返回前退出、没回填 `post_version` 的行报 `conflict: incomplete`。

- **证据**：`journal.Succeed`（`internal/journal/journal.go:864-893`）清空 `blob_path` 释放 blob，
  旧版本只作为可被淘汰的读缓存存在；网盘普遍无"按版本取内容"接口，`Caps` 无此能力位。
  agent 写错今天无法撤销。
- **做法（后端）**：
  - `session_ops` 表；mcpsrv 写工具调 VFS **之前**捕获前像：`StatPath` → 文件且 ≤ `max_preimage_bytes`
    （32 MiB）时读满 → `cache.HydratedPath` → `os.Link` 到 `agent/preimages/<sha256>`（`ReserveDisk`
    记账），链接成功才写行；留不住前像记 `pre_reason` 照常执行，**不因此拒绝写**；递归删目录只记 `dir`。
  - `Rollback` 逆 seq 经 VFS 发起新写入；当前 version ≠ `post_version` 报 conflict 不覆盖；
    回滚本身是新会话可再回滚；`dry_run` 只计算不执行。前像按 `mcp.session.retain`（7 天）GC。
  - MCP `rollback_session{session_id, confirm, dry_run?}`；控制面 `POST /sessions/{id}/rollback`；
    CLI `cloudfs sessions rollback <id> --confirm [--dry-run]`。
  - 承诺写进 `docs/mcp.md`：不是远端历史版本恢复；只覆盖 MCP 发起的修改；本地立即可见、远端最终一致。
- **界面**：
  - 会话详情浮层增加 **操作表**：序号 / 操作（新建、覆盖、编辑、改名、删除、建目录）/ 路径（改名显示 旧 → 新）/
    前像（可恢复、过大、未缓存、目录，点+文字）/ 回滚结果。
  - "回滚此会话"按钮（图标 `undo`）→ 先调 `dry_run` → **预览浮层**（`rollback_plan.js` 分组）：
    将恢复 N（列出）、将跳过 M（原因）、冲突 K（"会话之后此文件又被修改"）；底部说明回滚承诺三句话；
    `confirmDelete` 键入会话短 ID + `confirm: true` 执行；结果浮层同样三组并提供"回滚这次回滚"。
  - 主窗口检查器：文件在保留期内被某会话修改过时显示"被 Agent 修改 · <client> · <时间>"，点击打开会话详情。
  - 「Agent」屏会话表状态新增"已回滚"，行内快捷"回滚"入口。
- **验收**：
  - 覆盖 1 MiB 已缓存文件后回滚，`read_text` 得原内容，fakeprovider 下载 +0；未缓存文件 +1 次且 ≤ 文件大小。
  - 会话写 → 内核路径再改同文件 → 回滚：该文件 conflict 未被覆盖，其余 restored。
  - `create → move → delete` 后回滚，路径树与会话前一致（conformance 风格逐项比对）。
  - chaos：`os.Link` 后、`session_ops` 插入前 `kill -9`，重启无假前像、孤儿 blob 回收；回滚中途 `kill -9`
    重跑幂等。
  - `dry_run` 不产生任何 VFS 写（journal 行数不变）。
  - 界面：`ui_rollback_test.go` 断言回滚按钮先请求 `dry_run: true` 再在确认后请求 `confirm: true`，
    未经预览不能直接执行；`_tests/rollback_plan.test.mjs` 覆盖分组与空计划；检查器"被 Agent 修改"调用
    `/sessions?path=`。
  - e2e：MCP 写 → 终端 `cat` 新内容 → 浏览器冒烟点回滚 → 终端 `cat` 旧内容。

### [x] T-39 嵌入与 hybrid 检索（二期，2026-09-15 完成）

- **2026-09-15 完成**（线 D 提交 cc1d6bc、7f1fd92、d2b50fb、4339aa3，收口提交 e2e 与文档）：`internal/embed`
  （`Embedder{Embed, Model, Dim}`；`openai` 走 `/embeddings` 并带 `dimensions`（默认 512），`ollama` 走 `/api/embed`
  批量；请求经 `httpx` 与代理规则，独立 `ratelimit` 按 `qps` 限速、429 自适应减半并遵守 `Retry-After`，独立
  breaker 5 次 5xx/连接失败后开 60 s；维度首个成功响应探测一次并钉住，之后不一致即报错；`Fake` 导出给 index /
  perf / e2e 计数）；`internal/config/embedding.go`（`index.embedding{provider, base_url, model, api_key, dimensions,
  batch, concurrency, qps, timeout, proxy, allow_remote, quantize}`、`index.max_chunks` 默认 200 000；provider ∈
  none/openai/ollama，`api_key` 进 `IsSecretField` 且只接受 `keyring:`/`secretfile:` 引用，`allow_remote:false`
  时非回环 / RFC1918 / link-local / `.local` 端点 `config.Parse` 报错）；index.db v2 只加 `vectors`（int8 + 每向量
  scale，`quantize: none` 存 float32，随 chunk 级联删除）与 `embed_pending` 两表；`embed_worker.go`（按 `batch`
  取队列、失败按 chunk 指数退避 8 次；换模型或存储形式 → 清空 vectors、全部 chunk 重新入队、`chunks_fts` 不动；
  维度与 `index_meta` 不一致 → 记错误并停 worker 而不是静默重嵌整库；breaker 打开时休眠到关闭；`max_chunks`
  硬上限，超出的 chunk 不入队仍可关键词检索）；`hybrid.go`（`vector` = 内存暴力 cosine top-k，按 generation
  跟随 store；`hybrid` = bm25 top-2k 与 cosine top-2k 做 RRF k=60；无 Embedder / 端点不健康 / 尚无向量 / 向量
  属于另一模型 / 查询嵌入失败 → 一律跑 keyword 并在 `degraded` 说明，`mode_used` 只报真正跑的模式；短查询在有
  向量时走向量）；`Status.Embedding{provider, model, dim, remote, host, healthy, last_error, breaker_open_until,
  embedded, pending, chars_this_month, capped}` 与 `Status.Vectors/MaxChunks`；控制面 `GET /index/embedding`
  （多 `api_key_configured`、裸 host、标明"估算"的费用 `estimate{chars, formula}`，永不带 key 值）与
  `POST /index/embedding/check`（只嵌入一次 `"cloudfs"`，返回 dim / latency / error）；doctor `index_embedding`
  （端点健康、`index_meta` 维度一致）与 `index_embedding_remote` warn；CLI `index auth`（`--key-file` / 隐藏提示 /
  stdin，拒绝命令行参数，写密钥库并把 `keyring:` 引用写进配置）与 `index embedding [--check]`；MCP `index_status.
  embedding` 与 `semantic_search.mode` 透传；界面 `embedding_panel.js` + 零 import 的 `embedding_view.js`（provider /
  模型 / 维度 / 地址、健康点 + 最后错误 + 熔断恢复时间、已嵌入 / 待嵌入、本月字符与估算费用；`remote=true` 黄色
  横幅无关闭按钮；"测试端点"先说明再点击才请求；无 key 时只显示 `cloudfs index auth` + 复制按钮，无输入框；底部
  "在配置文件中修改"），概况卡"向量 N / max_chunks"，主窗口搜索切换"文件名 / 关键词 / 语义"（`mode=hybrid`，
  `degraded` 非空时渲染"已降级为关键词"说明行）。
- **验收证明**：provider=none 时 `mode: hybrid` → `mode_used: keyword`、`degraded` 非空、不报错：
  `TestVectorModeNeedsAnEmbedder`（`internal/index`）、`TestSemanticSearchReportsModeUsed`（`internal/mcpsrv`）；
  连续 5 次 5xx 后 60 s 内无新请求且 `healthy=false`：`TestBreakerOpensAfterFiveServerErrors`（另
  `TestConnectionFailuresFeedTheBreakerAndConcurrencyKeepsOrder`、`TestThrottledHonoursRetryAfter`）；
  `allow_remote: false` + `https://api.openai.com/v1` 解析报错：`TestRemoteEndpointNeedsAllowRemote`（另
  `TestAPIKeyMustBeAReference`、`TestEmbeddingDefaults`）；BM25 与 cosine 结论相反时 RRF 顺序：
  `TestHybridRRFOrdersByFusedRank`（另 `TestShortQueryUsesVectorsWhenPresent`、`TestInt8QuantisationKeepsCosineOrder`）；
  换模型后 `vectors` 清空、`embed_pending` = chunks 数、`chunks_fts` 行数不变：`TestChangingTheModelReembedsEverything`
  （另 `TestSchemaV2AddsVectorTables`、`TestEmbedPendingFollowsTheChunks`、`TestEmbedWorkerSleepsWhileTheBreakerIsOpen`、
  `TestMaxChunksIsAHardCap`、`TestDimensionMismatchStopsTheEmbedWorker`）；perf 嵌入调用次数 = `ceil(chunks/batch)`、
  无变化重跑 0 次：`test/perf` `TestEmbedCallsEqualCeilChunksOverBatch`；端点协议：`TestOpenAIBatchesAndSendsDimensions`、
  `TestOllamaUsesTheBatchEndpoint`、`TestDimIsProbedOnceAndPinned`；控制面 / doctor / CLI：`TestEmbeddingStatusNeverLeaksTheKey`、
  `TestEmbeddingCheckCallsTheEndpointOnce`、`TestStatusCarriesTheEmbeddingLine`、`TestDoctorFlagsDimensionMismatch`、
  `TestDoctorNotesARemoteEndpoint`、`TestIndexAuthWritesAReference`、`TestIndexAuthRefusesAKeyOnTheCommandLine`、
  `TestIndexEmbeddingPrintsTheStatusTable`、`TestIndexStatusCarriesEmbedding`；界面 `ui_embedding_test.go`：
  `TestRemoteBannerHasNoCloseButton`、`TestEmbeddingPanelHasNoKeyInput`、`TestEndpointCheckOnlyOnClick`、
  `TestSemanticModeShowsDegradedNote`、`TestIndexOverviewShowsVectors`、`TestEmbeddingCatalogCoversThePanel`，
  `_tests/embedding_view.test.mjs` 7 例；e2e 真实挂载 `test/e2e/memory_e2e_test.go` `TestHybridSearchWithAFakeEmbedder`
  （`embed.Fake` 接进 `index.Options.Embedder`，挂载点写 md → 抽取 → `EmbedNow` 恰 1 次调用 → `semantic_search{mode:
  hybrid}` 返回 `mode_used: hybrid` 且无 `degraded`，`mode: vector` 同样命中，`index_status.embedding` 报 `model: fake`、
  `dim: 8`、`embedded: 1`）。
- **遗留**：openai / ollama 的线上格式只对照公开文档写成（`internal/embed/openai.go`、`ollama.go` 各一处
  `UNVERIFIED`：真实 openai 端点的 `dimensions` 行为、ollama 批量接口的返回顺序）；64 条 × 1200 rune 的批是否超过真实
  端点的请求 / token 上限（`embed_worker.go` `UNVERIFIED`）；向量检索只有内存暴力 cosine（HNSW 留三期）；
  `api_key_configured` 提示对任何 provider 都显示（ollama 通常不需要 key）；换 `embedding.model` 会重嵌整库，没有增量
  迁移；e2e 用 `embed.Fake`，守护进程不能从配置注入假端点，所以 e2e 的索引是测试自建的 `Indexer`（同一 `index.New`
  与 MCP 接线，`index.enabled: false` 避免两份索引）。

- **证据**：T-37 只有 keyword；中文同义表达、跨语言检索 FTS 无能为力。
- **做法（后端）**：
  - `internal/embed`：`Embedder{Embed, Model, Dim}`；`openai`（`/embeddings`，`dimensions` 默认 512）、
    `ollama`（`/api/embed` 批量）、`fake`。HTTP 走 `httpx.New`（proxy 规则生效），独立 `ratelimit` + breaker。
  - `api_key` 加入 `config.IsSecretField`，只接受 `keyring:`/`secretfile:`，`cloudfs index auth` 写入。
  - `allow_remote: false` 时非回环/RFC1918 端点配置校验失败。
  - index.db v2：`vectors`（int8 默认，L2 归一）、`embed_pending`；启动载入内存暴力 cosine；
    FTS `bm25` + cosine RRF（k=60）；`max_chunks` 硬上限；换模型清空 vectors 全量重嵌。
  - 端点故障或 provider=none 静默降级 keyword，响应带 `degraded`。
  - 控制面 `GET /index/embedding`、`POST /index/embedding/check`；doctor 端点可达与维度检查。
- **界面**：
  - 「索引」屏 **嵌入端点面板**：provider / 模型 / 维度 / 地址；**远端标识**——`remote=true` 时黄色横幅
    "文件内容会发送到 <host>"常驻，不可关闭；健康点 + 最后错误 + 熔断恢复时间；已嵌入 / 待嵌入进度；
    本月嵌入字符数与按公式的费用估算（明说是估算）。
  - "测试端点"按钮：说明"会产生一次调用" → `POST /index/embedding/check`。
  - 未配置 `api_key` 时显示 `cloudfs index auth` 命令与复制按钮，**不提供输入框**。
  - 配置编辑不在界面做（provider/model 改动需全量重嵌，影响大）：面板底部"在配置文件中修改"。
  - 主窗口内容搜索切换扩为"文件名 / 关键词 / 语义"；降级时语义按钮显示"已降级为关键词"说明行。
  - 概况卡增加"向量 N / max_chunks"。
- **验收**：
  - provider=none 时 `mode: hybrid` 返回 `mode_used: keyword` 且 `degraded` 非空，不报错。
  - 端点连续 5 次 5xx 后 60 s 内无新请求，`healthy=false`。
  - `allow_remote: false` + `base_url: https://api.openai.com/v1` 的配置 `config.Parse` 报错。
  - 构造向量使 BM25 与 cosine 结论相反，断言 RRF 顺序。
  - 换 `embedding.model` 后 `vectors` 清空、`embed_pending` = chunks 数、`chunks_fts` 行数不变。
  - `test/perf`：嵌入调用次数 = `ceil(chunks/batch)`，无变化重跑 0 次。
  - 界面：`ui_embedding_test.go` 断言 remote 横幅在 `remote=true` 时渲染且无关闭按钮；嵌入模块无
    `input` 用于 key、无秘密字段名；"测试端点"仅点击时请求；语义模式降级说明行渲染。

### [x] T-40 Agent 记忆库（二期，2026-09-15 完成）

- **2026-09-15 完成**（线 D 提交 f41c4b3、9ddba86、3893dc0，收口提交 e2e 与文档）：`internal/config/memory.go`
  （`memory.root` 默认 `mcp.workspace`，再退到第一个 allow 前缀 + `/.agent`，与 `begin_session` 同一推导；
  `max_fact_bytes` 64 KiB、`max_agent_bytes` 32 MiB；root 必须规范路径）；`internal/memory`（`Store{fs, index, cfg}`
  是 MCP 工具、控制面与 CLI 共用的唯一实现：`Agents/List/Get/Put/Delete/Search`；`Put` 校验名字
  `^[a-z0-9][a-z0-9-]{0,63}$`、`expected_version`、单条与 agent 预算（拒绝时给当前用量），写 `facts/<name>.md`
  （frontmatter `name/description/type/updated_at` + 正文）后把 `MEMORY.md` 唯一匹配行替换、否则追加；版本取自文件
  字节的 sha256 前 12 字节而不是网盘版本，上传落地不变、内容一变就变；冲突副本 = 同目录、以 `<name>` 开头且不是合法
  fact 文件的兄弟，不猜任何 provider 的命名；`NormalizeAgent` 把 client name 规范成 `[a-z0-9-]`）；MCP
  `memory_list/get/put/delete/search`（agent 默认 = HTTP 令牌名或 stdio client name 规范化，`cloudfs mcp --agent`
  覆盖；每个路径过 `checkPath`，root 不在 scope 内五个工具同一说明；`--read-only` 与非 owner stdio 拒绝 put/delete；
  `delete` 需 `confirm`；`search` = index 限定 `memory/<agent>`（+ `shared`）的检索，靠 `Source: builtin` 的内置规则
  跟随 `memory.root` 自动索引 `**/*.md`，`unindex` 与界面不能删它）；控制面 `GET /memory/agents`（无 store / 无 root
  时 `{enabled:false, reason, example}`）、`GET /memory/{agent}?cursor`、`GET|PUT|DELETE /memory/{agent}/{name}`
  （PUT 带 `expected_version`，过期 409 且 body 带 `current_version`；超限 413 带用量；`..`、`/`、大写 400；DELETE 走
  `confirmed` 键 `confirm.memory.delete`）、`GET /memory/search`（无索引 409）；CLI `cloudfs memory
  agents|list|get|put|delete|search [--agent]`（`get` 原样打印正文可管道，`put` 读 `--file` 或 stdin，`delete`
  需 `--confirm`）；界面 `screens/agents_memory.js`（左 agent 列表：条数、占用 / 上限、有副本时红点；右表 名称 / 描述 /
  类型 / 更新时间 / 冲突标记 `data-conflicts`；顶部搜索框走 `/memory/search`；"新建记忆"`openForm` 前端同规则校验；
  未配置 root 显示原因与配置示例；深链 `#/agents?tab=memory&agent=<a>&memory=<name>` 直接开编辑器）、
  `memory_panel.js`（编辑浮层：名称只读、描述、类型、正文 `textarea` 经 `.value` 填充、字节计数 / 上限，保存 PUT 带读到
  的 `expected_version`，409 → "已在其他设备修改" + "重新载入"；冲突合并浮层：本体与副本只读并排（副本经 `/fs/preview`），
  "保留本体并删除副本"（键入确认 → `POST /fs/delete`）、"用副本覆盖本体"（PUT 带读到的版本 → 删副本）、"手动合并"
  （两段预填进下方编辑器，不写任何东西）；删除 `confirmDelete` 键入名称 → `DELETE` 带 `confirm:true`）、零 import 的
  `memory_conflicts.js`（副本配对只按前缀与同目录、名字正则与 Go 侧逐字符一致、UTF-8 字节计数、frontmatter 拆分）。
  **顺带修的 VFS 缺口**：上传以冲突副本落地后，输掉的本地节点保留 `cloudfs-local:` 身份、缓存项已被钩子释放，而本应
  把远端版本带回来的列举却一直把它当待传写入保护，文件在进程生命周期内不可读——`remote_protection.go` 新增
  `conflictLoser`（本地身份、无缓存项、无写句柄）并在 `fetchDir` 的 `protect` 里放行，下一次列举恢复远端版本
  （`internal/vfs/conflict_restore_test.go` `TestConflictLoserIsRestoredByTheNextListing`）。
- **验收证明**：`memory_put("style")` 后 `facts/style.md` 存在、`MEMORY.md` 恰一行、重复 put 不增行：
  `TestPutCreatesTheFactAndOneIndexLine`（`internal/memory` 与 `internal/mcpsrv` 各一）、`TestIndexLineEditing`、
  e2e `TestMemoryPutIsVisibleInTheMountAndSearchable`（真实挂载：`memory_put` → 终端 `cat` 得 frontmatter + 正文、
  `MEMORY.md` 恰一行 → `memory_search` 3 s 内命中 → 带 `expected_version` 二次 put 仍一行 → 终端 `>>` 追加 → `memory_get`
  返回新正文与新版本）；过期 `expected_version` 被拒、内容不变：`TestStaleExpectedVersionIsRefused`（memory 与 mcpsrv）、
  `TestMemoryPutRequiresExpectedVersionWhenGiven`（控制面 409 带 `current_version`）；fakeprovider 注入版本冲突后
  `memory_get.conflicts` 列出副本：`TestGetListsConflictCopies`、`TestConflictSiblingsArePrefixMatchesThatAreNotFacts`；
  超过 `max_fact_bytes` / `max_agent_bytes` 被拒并给用量：`TestBudgetsAreEnforcedWithUsage`；root 不在 allow 内五个工具
  同一说明：`TestRootOutsideScopeFailsEveryToolTheSameWay`、`TestMemoryToolsRefuseWithoutARoot`、
  `TestMemoryAgentsExplainsAMissingRoot`；`--read-only` 下 get/list/search 可用、put/delete 拒绝：
  `TestReadOnlyAllowsReadsOnly`、`TestMemoryPutAndDeleteNeedTheOwner`；写入 3 s 内 `memory_search` 命中：
  `TestMemorySearchFindsAFreshFact`、`TestMemorySearchScopesTheIndexToTheAgent`、`TestIndexRuleCoversTheMemoryTree`、
  `TestMemoryStoreIsWiredWithTheBuiltinIndexRule`、`TestBuiltinRulesFollowTheConfigurationAndCannotBeRemoved` 与上述 e2e；
  agent 身份：`TestAgentNameIsNormalised`（memory 与 mcpsrv）、`TestMemoryAgentFollowsTheTokenPrincipal`；删除确认：
  `TestMemoryDeleteNeedsConfirm`（mcpsrv 与控制面）、`TestMemoryCLIDeleteNeedsConfirm`；路由名字校验：
  `TestMemoryRoutesRefuseBadNames`、`TestMemoryRoutesWhenNoStore`、`TestMemorySearchWithoutIndexIs409`、
  `TestEveryRouteIsEitherGuardedOrArguedOpen` 与 `TestEveryRouteToleratesTheLanguageParameter` 自动覆盖；CLI：`TestMemoryCLIPutsAndGets`；frontmatter：`TestFrontmatterRoundTrip`、
  `TestFrontmatterOnlyReadsTheHead`；界面 `ui_memory_test.go`：`TestMemorySaveCarriesExpectedVersion`、
  `TestMemoryDeleteConfirms`、`TestConflictActionsHitTheirRoutes`（三动作各自路由）、`TestMemoryBodyIsInsertedAsText`、
  `TestMemoryTabExplainsMissingRoot`、`TestMemoryNameValidatedInTheForm`、`TestMemoryCatalogCoversTheTab`、
  `TestMemoryModulesStayShort`，`_tests/memory_conflicts.test.mjs` 7 例（配对与不配对样例）；浏览器冒烟
  `TestMemoryTabInTheBrowser`（`CLOUDFS_BROWSER=1`：记忆标签列出 MCP 创建的 agent 与 fact，深链打开编辑器、路径在浮层里、
  `textarea.value` 等于正文）；VFS 修复：`TestConflictLoserIsRestoredByTheNextListing`。
- **遗留**：fact 的 `version` 是内容哈希而不是网盘版本——两台设备写出相同字节视为同一版本（对记忆语义无害，但不能
  用它判断"谁先落地"）；`expected_version` 的比对与写入不是原子的（`Store.mu` 只串行化本进程，跨进程 / 跨设备有毫秒级
  窗口，输掉的一方仍会以冲突副本形式保留）；`Put` 的 `description` / `type` 为空表示"保留文件里的"，所以不能通过 put
  清空描述（要清空只能改文件）；CLI `--agent` 默认 `shared` 而 MCP 默认调用方自己，两边默认不同是有意的但要知道；
  名为 `agents` / `search` 的 agent 与 `/memory/agents`、`/memory/search` 路由同名，只能经 MCP 工具访问；冲突副本配对
  只按"同目录 + 前缀"，provider 若把副本放到别处或改名前缀就配不上；带外（MCP）写入后内核属性失效是异步 `InodeNotify`，
  微秒级窗口内终端的 `O_APPEND` 仍按旧 size 定位（e2e 里先等 `stat` 看到新 size；属于 T-43 并存语义，不是记忆库缺陷）；
  `skills/<name>/SKILL.md` 只约定位置，没有工具。

- **证据**：Claude Code / Codex / OpenClaw 的记忆都是本机文件，换设备即丢；CloudFS 已能跨设备同步文件，
  但没有约定、没有工具、冲突副本对 agent 不可见。
- **做法（后端）**：
  - 约定目录（纯文件，不做 KV 表；跨设备同步交给网盘，冲突走既有 conflict-copy）：
    `/<memory.root>/memory/<agent>/{MEMORY.md, facts/<name>.md}`、`memory/shared/`、`skills/<name>/SKILL.md`。
  - `<agent>` 默认取 `clientInfo.name` 规范化为 `[a-z0-9-]`，`--agent` 覆盖；`name` 校验 `^[a-z0-9][a-z0-9-]{0,63}$`。
  - `memory.max_fact_bytes`（64 KiB）、`memory.max_agent_bytes`（32 MiB）；`memory.root` 必须在 `--allow` 内。
  - MCP `memory_list/get/put/delete/search`（`put` 带 `expected_version`；`get` 返回 `conflicts[]`；
    `search` = 限定范围的 `semantic_search`，index 内置规则）。
  - 控制面 `GET /memory/agents`、`GET /memory/{agent}`、`GET|PUT|DELETE /memory/{agent}/{name}`（写走同一实现与校验）。
- **界面**：
  - 「Agent」屏 **记忆标签**：左侧 agent 列表（claude-code、codex、openclaw、shared，显示条数与占用 / 上限）；
    右侧表格 名称 / 描述 / 类型 / 更新时间 / 冲突标记（红点+"有冲突副本"）；顶部记忆搜索框（走 `memory_search`）。
  - 行点击打开 **记忆编辑浮层**：frontmatter 字段（名称只读、描述、类型）+ 正文 `textarea` + 字节计数 /
    上限；保存带 `expected_version`，版本冲突时提示"已在其他设备修改"并提供"重新载入"。
  - **冲突合并浮层**（`memory_conflicts.js` 配对）：左右并排本体与冲突副本（只读），按钮"保留本体并删除副本"、
    "用副本覆盖本体"、"手动合并"（打开编辑浮层预填两段）；删除走 `confirmDelete` 键入名称。
  - "新建记忆"`openForm`：agent、名称（前端同规则校验）、描述、类型。
  - 未配置 `memory.root` 或不在 allow 内：标签页显示说明与配置示例。
- **验收**：
  - `memory_put("style")` 后 `facts/style.md` 存在、`MEMORY.md` 恰一行指向它；重复 put 不增行。
  - 过期 `expected_version` 被拒，内容不变。
  - fakeprovider 注入版本冲突后 `memory_get.conflicts` 列出副本路径。
  - 超过 `max_fact_bytes` / `max_agent_bytes` 被拒并给出当前用量。
  - `memory.root` 不在 allow 内时五个工具返回同一说明错误。
  - `--read-only` 下 get/list/search 可用，put/delete 拒绝。
  - 写入记忆 3 s 内 `memory_search` 命中。
  - 界面：`ui_memory_test.go` 断言保存带 `expected_version`、删除带 `confirm: true`、冲突浮层三个动作分别调用
    对应路由；`_tests/memory_conflicts.test.mjs` 覆盖副本配对（不猜 provider 命名，只按前缀与同目录）；
    编辑浮层正文作为文本插入不作为 HTML。

### [x] T-41 事件触发器：exec / webhook（二期，2026-09-15 完成）

- **2026-09-15 完成**（线 E 提交 c6f476a、94a8bd9、db29d6a、ffd3153、654392f 与本条收口提交）：`vfs.Change` 加
  `Kind`（write/create/mkdir/remove/rename/remote/rescan）与 `Origin`（kernel/api/remote），`WithOrigin(ctx, name)` 由
  mcpsrv 中间件（`"mcp"`）、控制面（`"control"`）、WebDAV（`"webdav"`）各打一次，9 个 emit 点各填自己的 kind，
  `changedListing`/rescan 固定 remote，`Affects` 不变；glob 抽到 `internal/pathglob`（索引与触发器共用）；配置
  `triggers[]{name, paths, events, origins, debounce, on_rescan, action: exec|webhook}`（`internal/config/triggers.go`：
  name 唯一且 `^[a-z0-9][a-z0-9-]{0,63}$`、恰一个 action、占位符只能是独立 argv 元素、webhook 只许 https/回环 http 否则
  `insecure: true`、secret 必须 `keyring:`/`secretfile:` 引用、未排除 `api` 的 exec 规则进 `Config.Warnings` 并由
  `cloudfs mount` 打到 stderr）；`internal/agent/deliveries.go`（`trigger_deliveries` DAO，`(rule,path)` pending 部分唯一
  索引 = 去抖合并，`Claim/Done/Fail/Dead/Retry/ResetRunning/List/Get/Counts`，`Store.Watch` 发 `trigger` 事件）；
  `internal/trigger`（仅 owner 且 `!NoBackground` 才启动：一个 goroutine 消费 `WatchChanges` 按 kind/origin/glob 匹配，
  `Subtree` 事件按 `MayMatchBelow` 命中根在其下的规则，rescan 每规则一行 `path=''` 除非 `on_rescan: ignore`；每规则
  串行 worker，退避 1 s×2ⁿ 封顶 5 min、8 次 dead，启动 `ResetRunning`；exec 无 shell、环境只留 PATH/HOME/LANG +
  `CLOUDFS_PATH/KIND/URI`、`Setpgid` 杀进程组、stdout/stderr 各截 64 KiB；webhook `X-CloudFS-Timestamp` +
  `X-CloudFS-Signature: sha256=HMAC(secret, ts+"."+body)`，`Verify` 导出供文档与界面，出站走 `proxy.Manager`）；
  控制面 `GET /triggers`（永不返回 secret）、`GET /triggers/deliveries?cursor&rule&state`、`GET /triggers/deliveries/{id}`
  （含 `truncated`）、`POST /triggers/test`（`confirm.trigger.test`）、`POST /triggers/retry`，`/status` 带
  `triggers{pending, dead}`，SSE `trigger`，doctor `checkTriggers` 把配置 warning 变成 warn；CLI `cloudfs triggers
  list|deliveries|show|test|retry`（无 daemon 时只读 agent.db）；界面 `#/triggers`（`screens/triggers.js` +
  `delivery_panel.js` + `trigger_view.js`：只读规则卡、argv 逐元素 `<code>`、webhook"签名密钥已配置"、自激黄标
  `data-risk="self-trigger"`、投递表 + 过滤 + 分页 + SSE 刷新、详情 `showPanel` 文本节点、`#/triggers?delivery=<id>`
  深链、测试投递键入规则名确认、空状态两个示例 + 校验片段，导航徽标 = dead 数）。
- **验收证明**（每条验收 → 用例）：
  - 50 ms 内 20 次内核写只 1 行且 `kind=write`：`internal/trigger` `TestDebounceCollapsesABurstIntoOneDelivery`；
    真实挂载下 `echo >` 的 create + FLUSH 合并成 1 行：`test/e2e` `TestKernelWriteFiresAnExecTrigger`（5 s 内 done，
    output 逐行是虚拟路径与 kind）。
  - argv 字面 `"/work/a.txt; rm -rf /"` 无 shell：`TestExecArgvIsNeverAShell`；环境最小：`TestExecEnvironmentIsMinimal`；
    超时杀进程组无孙进程：`TestExecTimeoutKillsTheProcessGroup`；输出截断：`TestExecOutputIsCapped`。
  - webhook 错 secret 拒、对的过、ts 偏差 > 5 min 拒：`TestWebhookSignatureVerifies`；非 2xx 计失败：`TestWebhookNon2xxIsAFailure`。
  - running 时 kill -9，重启同 `(rule,path)` 再投一次且 `attempts=2`：`test/chaos` `TestDeliveryRunningAtCrashIsRedelivered`
    （第一个引擎在子进程阻塞时关闭、行仍 running，同一 agent.db 上第二个引擎 `ResetRunning` 后跑完 done，行数仍 1）；
    单元层 `TestRunningDeliveriesRestartAsPending`、`internal/agent` `TestResetRunningRepends`。
  - 1 万事件风暴行数 ≤ 规则 × 路径、溢出 rescan 只一行：`TestStormStaysBounded`；真实 `vfs.FS` 64 槽队列被 400 个变更
    压溢出后 `deliver` 规则恰 1 行 pending `path=''`、`on_rescan: ignore` 规则 0 行、每路径不重复、rescan 跑完 output
    以 `rescan` 开头：`test/chaos` `TestStormUnderOverflowDeliversOneRescan`。
  - `origins` 排除 api 时 MCP 写入不触发：`TestAPIOriginCanBeExcluded`；真实挂载 + 进程内 MCP `write_file` 2 s 内无投递、
    随后 shell 写恰 1 行：`test/e2e` `TestMCPWriteDoesNotFireWhenAPIIsExcluded`。
  - 9 个 emit 点各断言 kind/origin：`internal/vfs/changes_kind_test.go`（`TestKernelCreateAndFlushAreTaggedKernel`、
    `TestWriteFileViaAPIIsCreateThenWrite`、`TestMkdirRemoveAndRenameCarryTheirKind`、`TestDeltaRefreshIsRemote`、
    `TestListingChangesAreRemoteEvenForAKernelReaddir`、`TestUploadLandingIsRemote`、`TestCopyAnnouncesACreate`、
    `TestQueueOverflowIsARescanFromRemote`、`TestAffectsIgnoresKindAndOrigin`）；三个适配层的打标：
    `TestToolCallsAreTaggedAsAPIChanges`、`TestFSRoutesTagTheirChangesAsControlOrigin`、`TestWriteRequestsCarryTheWebDAVOrigin`；
    `test/perf` 调用次数基线不变。
  - 配置：`TestTriggerNeedsExactlyOneAction`、`TestWebhookSecretMustBeAReference`、`TestPlainHTTPWebhookNeedsInsecure`、
    `TestExecRuleWithoutOriginFilterWarns`、`TestPlaceholdersMustBeWholeArgvElements`、`TestRoadmapTriggerExampleParses`、
    `TestMountPrintsConfigWarnings`；引擎只在 owner 跑：`internal/daemon` `TestTriggerEngineRunsOnlyInTheOwner`、
    `TestNoBackgroundSkipsTheTriggerEngine`。
  - 控制面：`TestTriggersViewNeverContainsTheSecret`、`TestTriggerTestNeedsConfirm`、`TestRetryOnlyDeadDeliveries`、
    `TestDeliveryDetailCarriesOutputAndTruncation`、`TestTriggerEventsReachSSE`、`TestDoctorSurfacesConfigWarnings`、
    `TestStatusCountsDeliveries`、`TestTriggersRoutesWhenNoEngine`、`TestEveryRouteIsGuarded`；CLI `TestTriggersCLIListsAndRetries`。
  - 界面：`ui_triggers_test.go` `TestTriggersScreenHasNoRuleEditor`（无编辑表单、无 PUT）、`TestArgvRendersPerElement`、
    `TestWebhookSecretNeverInDOM`、`TestTestDeliveryConfirms`（`confirm: true`）、`TestDeadRowRetries`（`/triggers/retry`）、
    `TestDeliveryOutputIsText`、`TestTriggersNavAndBadge`；`_tests/trigger_view.test.mjs` 自激判断；浏览器
    `test/e2e` `TestTriggersScreenInTheBrowser`（`CLOUDFS_BROWSER=1`：规则卡 `data-rule`、投递行 `data-delivery`/
    `data-state="done"`、导航在 `#/index` 之后，深链打开详情并以文本显示 printf 的三行输出）。
- **遗留**：
  - Windows 没有 `Setpgid`：`internal/trigger/exec_windows.go` 只杀直接子进程，超时后孙进程会活下来
    （`UNVERIFIED`：Job object 或 `CREATE_NEW_PROCESS_GROUP` 方案要在 Windows 真机验证）。
  - 改名事件带 `Subtree`，`Subtree` 事件按"glob 根在其下"匹配，所以 `**/*.md` 规则会被非 `.md` 文件的改名触发
    （一次多余投递，不会漏）。
  - 队列溢出压成的 rescan 只以 `path=''` 送达，被压掉的具体路径不可恢复（文档已写明）；至少一次意味着重启后
    同一动作可能再跑一遍，动作自身要幂等。
  - webhook `download_url` 只在提供方能给出可分享直链时出现（其它提供方在 output 里记 `download_url unavailable`）。
  - 不新增 MCP notification 动作；`pull_events` 留三期。

- **证据**：`vfs.Change` 只有 `Paths/Subtree/Rescan`，无种类与来源；9 个 emit 点
  （`internal/vfs/write.go:417/617/708/788/801/1027/1060/1148/1212`）各知道操作；
  `fromKernel(ctx)`（`vfs.go:1017`）可区分内核来源；无任何出站通知。
- **做法（后端）**：
  - `Change` 加 `Kind`（write/create/mkdir/remove/rename/remote/rescan）与 `Origin`（kernel/api/remote），
    `WithOrigin(ctx)`；`Affects` 不变。
  - 配置 `triggers[]{name, paths（自带 glob 匹配，不引入 doublestar 依赖）, events, origins, debounce, on_rescan, action: exec | webhook}`；
    `Validate()` 对未排除 `api` 来源的 exec 规则给 warning（防自激）。
  - `internal/trigger`（仅 owner）：消费 WatchChanges → `trigger_deliveries`（`(rule,path)` pending 唯一索引
    = 去抖合并）→ 每规则串行 worker，退避 1 s→5 min，8 次 dead，重启 running→pending（at-least-once；
    溢出只以 rescan 送达，文档明说）。
  - exec 无 shell，占位符只替换独立 argv 元素，环境只留 PATH/HOME/LANG + `CLOUDFS_*`，`Setpgid` 杀进程组，
    输出截 64 KiB。webhook `X-CloudFS-Signature: sha256=HMAC(secret, ts+"."+body)`，仅 https/回环，
    secret 只接受 `keyring:`/`secretfile:`，出站走 `proxy.Manager`。
  - 控制面 `GET /triggers`、`GET /triggers/deliveries`、`GET /triggers/deliveries/{id}`、`POST /triggers/test|retry`；
    SSE `trigger`；CLI `cloudfs triggers list|deliveries|test|retry`。
- **界面**：
  - 导航新增「触发器」`#/triggers`（`screens/triggers.js`，图标 `bolt`），导航徽标显示 dead 投递数。
  - **规则卡片列表（只读）**：名称 / 路径 glob / 事件 / 来源 / 动作类型；exec 显示 argv 逐元素（等宽，
    不拼接成命令行，避免误读成 shell）；webhook 显示 URL 与"签名密钥已配置"；自激风险（`trigger_view.js`）
    黄色标记"此规则可能被 Agent 自身写入触发"；卡片底部"在配置文件中修改"。
  - **投递表**：时间 / 规则 / 路径 / 事件 / 来源 / 次数 / 状态（待处理、执行中、完成、失败，点+文字）/ 动作
    （dead 行"重试"）；过滤规则与状态；分页；SSE 未翻页时刷新。行点击 `showPanel` 显示 stdout/stderr
    （截断提示）或 webhook 响应码与错误。
  - "测试投递"`openForm`：选择规则 + 输入路径 → `confirmDelete` 键入规则名（exec 会真实执行）→
    `POST /triggers/test`，完成后自动打开该投递详情。
  - 未配置任何规则：整屏显示两个配置示例（exec、webhook）与 webhook 校验代码片段。
- **验收**：
  - 同一路径 50 ms 内 20 次内核写，`debounce: 2s` 规则只产生 1 行 delivery，`kind=write`。
  - exec `["echo", "{path}; rm -rf /"]` 收到的 argv[1] 字面等于 `"/work/a.txt; rm -rf /"`，无 shell 进程。
  - webhook 错误 secret 校验失败、正确成功；ts 偏差 > 5 min 被示例校验器拒绝。
  - 投递 running 时 `kill -9`，重启后同 `(rule,path)` 再投一次且 `attempts=2`。
  - 1 万事件风暴下 deliveries 行数 ≤ 规则 × 路径数，内存有界；溢出 rescan 只投一行。
  - `origins` 排除 api 时 MCP 写入不触发。
  - 9 个 emit 点各断言 kind/origin。
  - 界面：`ui_triggers_test.go` 断言屏幕无任何编辑规则的表单与 PUT 请求；argv 逐元素渲染；webhook secret
    不出现在响应与 DOM；测试投递带 `confirm: true`；dead 行重试调用 `/triggers/retry`；
    `_tests/trigger_view.test.mjs` 覆盖自激判断。

### [x] T-42 发送给 Agent（二期，2026-09-15 完成）

- **2026-09-15 后半完成**（线 E 提交 e2297a1，前半见下）：配置 `agents[]{name, exec{command, cwd, timeout}}`（与
  `triggers[].action.exec` 同一套校验，只有 agents 可用 `{prompt}`）；`internal/control/agent_invoke.go`
  `GET /agent/endpoints` 只返回名字、`POST /agent/invoke {agent, paths[], prompt?, confirm}`：无引擎或无此 agent → 404
  且不启动任何进程，`confirmed`（`confirm.agent.invoke`）后经 `trigger.Engine.Invoke` 入队 `rule="agent:<name>"`
  并立即执行（`{prompt}` 整体替换、`paths` 逐个作为独立 argv 追加、失败直接 dead 不重试、同路径重复调用返回
  `ErrAlreadyQueued`），每次到达引擎的调用写审计行 `principal=console`、`transport=console`、`tool=agent.invoke`
  （参数只记 agent 名与 prompt 长度）；界面 `send_to_agent.js` 在 `/agent/endpoints` 非空时渲染 agent 下拉 + "运行"，
  `confirmDelete` 键入 agent 名 → 带 `confirm: true` 提交 → toast + "查看投递"链到 `#/triggers?delivery=<id>`，
  复制按钮行为不变。
- **验收证明**：
  - 未配置 agents 时 404 且无子进程：`internal/control` `TestInvokeWithoutAgentsIs404AndSpawnsNothing`；执行 argv 与配置
    逐元素相等、`paths` 只作为独立元素：`TestInvokePassesPathsAsSeparateArgv`、`internal/trigger`
    `TestInvokeAppendsPathsAsSeparateArgv`；未确认不执行：`TestInvokeNeedsConfirm`；失败即 dead：`TestInvokeFailureIsDeadAtOnce`。
  - `GET /agent/prompt` 不存在路径 404 不泄露内部路径：`TestAgentPromptMissingPathIs404WithoutInternals`（一期）。
  - 审计 `principal=console`、`tool=agent.invoke`：`TestInvokeIsAudited`；端点只有名字：`TestEndpointsListNamesOnly`。
  - 界面 `ui_send_to_agent_test.go`：`TestCopyStillMakesNoWrite`、`TestRunButtonOnlyWithEndpoints`、
    `TestRunConfirmsWithConfirmTrue`、`TestSendToAgentOnlyReadsAndCopies`（文本插入）、`TestSendToAgentHasBothEntrances`；
    投递详情在浏览器里的渲染由 T-41 的 `TestTriggersScreenInTheBrowser` 覆盖（运行按钮跳的就是同一深链）。
- **遗留**：
  - agent 调用不跨重启：`{prompt}` 与第 2 个起的路径只保存在引擎内存里（表只存首路径），重启后重试会以
    "the prompt of an agent run is not kept across restarts" 直接 dead，需从控制台重新运行。内存里的 prompt 在投递
    `done` 时释放、`dead` 时保留给重试（2026-09-15 审查修：之前只增不删，每次运行的 prompt 常驻到进程退出）。
  - 控制台发起的审计行 `session` 与 client 列为空（没有 MCP 会话在背后），审计屏按 principal `console` 区分。
  - 一次只发送一个文件（多选留三期）；`cwd` 只在配置里指定。

- **2026-09-15 一期前半已完成**（提交 fd9aad2）：`GET /agent/prompt?path=[&heading=]`（`internal/control/agent_prompt.go`）
  返回 `{path, uri, prompt}`：虚拟路径 + 与 `mcpsrv/resources.go` 同规则的 `cloudfs://<remote>/<path>` URI +
  `read_text`/`edit_file`；仅 `Collector.Index` 存在时提 `read_extracted_text`，仅 `Collector.Agent` 存在时提
  `begin_session`/`finish_session`，目录加 `list_directory`；零依赖、总是注册；不存在的路径 404 且不泄露缓存目录/remote 名
  （`TestAgentPromptMissingPathIs404WithoutInternals`、`TestAgentPromptMentionsTheRightTools`）。界面：检查器"发送给
  Agent"（`bot`，文件与目录）与内容搜索结果行同一入口（带命中标题）→ `send_to_agent.js`（`openPanel`，`textarea`
  预填可编辑，"复制"只写剪贴板，除 GET 提示词外无请求；`TestSendToAgentOnlyReadsAndCopies`、
  `TestSendToAgentHasBothEntrances`）。i18n 键为 `action.sendtoagent`/`agent.prompt.*`（非原计划的 `send.*`）。
  **未做**：`agents[]` 配置、`/agent/endpoints`、`/agent/invoke`、运行按钮与确认（随 T-41 二期）。

- **证据**：控制台文件检查器（`internal/control/web/screens/main.js:238-243`）只有固定/预热/改名/预览/
  链接/删除；用户选中文件后无法把任务交给本机 agent，只能自己拼路径与提示词。
- **做法（后端）**：
  - `GET /agent/prompt?path=`：返回 MCP-ready 提示词（虚拟路径、`cloudfs://` URI、`read_text` /
    `read_extracted_text` / `edit_file` 用法、会话建议 `begin_session`），零依赖总是可用。
  - 配置 `agents[]{name, exec{command argv, cwd, timeout}}`；`GET /agent/endpoints`（只返回名称）；
    `POST /agent/invoke {agent, paths[], prompt?}` 复用 T-41 exec 执行器（同白名单、无 shell、输出截断），
    投递进 `trigger_deliveries`（rule=`agent:<name>`），审计 `principal=console`。未配置 → 404 且不执行。
- **界面**：
  - 检查器新增"发送给 Agent"按钮（图标 `bot`，文件与目录都有）→ **发送浮层**（`send_to_agent.js`）：
    预填提示词 `textarea`（可编辑）+ "复制"按钮（始终可用）；已配置 agents 时下方出现 agent 下拉 +
    "运行"按钮 → `confirmDelete` 键入 agent 名称（会执行本机命令）→ toast"已提交"并附"查看投递"链接跳
    `#/triggers?delivery=<id>`。
  - 内容搜索结果行右侧同样有"发送给 Agent"（预填命中路径与标题）。
  - 一期先交付"复制提示词"（无 exec 依赖），运行按钮随 T-41 一起上线。
- **验收**：
  - `POST /agent/invoke` 未配置 agents 时 404 且无子进程；配置后执行命令与配置 argv 逐元素相等，
    `paths` 只作为独立 argv 元素。
  - `GET /agent/prompt` 对 `--allow` 外路径（控制面无 allow，但对不存在路径）返回 404，不泄露内部路径。
  - 审计有 `principal=console`、`tool=agent.invoke` 行。
  - 界面：`ui_send_to_agent_test.go` 断言复制按钮不发网络请求以外的写操作、运行按钮仅在 `/agent/endpoints`
    非空时渲染、运行前确认并带 `confirm: true`、提示词 `textarea` 内容按文本插入；检查器与搜索结果两个入口都存在。

### [ ] T-43 验证缺口：stdio MCP 与 mount 并存（二期回滚之前完成；验证与栅栏已交付，桥待三期提前）

- **状态（2026-09-15 线 C 收口，提交 ab59d2f、19fef9f 与本条收口提交）**：本条的验证目标已达成——
  e2e 复现存在并给出结论（下文 C0 观察），写栅栏落地（C0.5：`internal/mcpsrv/owner_fence.go` + `internal/vfs`
  `ErrNotOwner`，`TestStdioBesideMountRefusesWritesCleanly` 把每条观察到的现象变成否定断言，随 `./gow test ./test/e2e/`
  常跑），doctor `agent_stdio` warn 与接入面板横幅落地（`docs/ui-plan.md` F10-1～F10-4 已勾，`#/agents` 浏览器冒烟
  `TestAgentTokenSandboxChainAndAuditInTheBrowser`、`TestRollbackInTheBrowser` 在 `CLOUDFS_BROWSER=1` 下通过）。
  **未关闭的唯一原因是 stdio→HTTP 桥**：并存拓扑下 stdio 进程现在是"只读 + 明确拒绝写"，不是"写入被转发给 owner"。
  **决定**：桥（SDK `StreamableClientTransport` + 原始 schema `AddTool`，约 300 行，见 `docs/agent-roadmap.md` §4.5/§7.3）
  **排在三期其他条目之前**，落地后把 `TestStdioBesideMountRefusesWritesCleanly` 改回"写入经桥可见且只上传一次"的
  肯定断言并关闭本条。T-38 的回滚不依赖桥（非 owner 的 `rollback_session` 返回 owner 错误，控制面路由只在 owner 进程）。
- **2026-09-15 结论（线 C C0.5，写栅栏已落地，e2e 转绿）**：并存拓扑下 stdio 的写入**一律被拒绝**，且拒绝发生在碰
  meta / journal / 网盘之前。`test/e2e/coexist_e2e_test.go` 改名 `TestStdioBesideMountRefusesWritesCleanly`，
  固定契约：stdio 的 `write_file`/`edit_file`/`create_directory`/`move`/`copy`/`delete` 全部返回
  `… requires the storage owner; use the HTTP transport: cloudfs mcp install --transport http`（不再是
  `journal: publication requires storage ownership`）；随后挂载侧对这些路径 `stat` 得 `ENOENT`（轮询 500 ms），
  journal 行数不变且没有 `pending`/`needs_publish` 行，fake provider 调用数不变、网盘无这些文件；stdio 自己
  `stat` 该文件也是 "does not exist"；终端写的 `shell.txt` 在 stdio 侧 `stat`/`read_text`/`list_directory`/`search`
  与 `edit_file dry_run` 正常；owner 重启后 `/demo` 仍只有 `shell.txt`、`BeginUpload` 计数不变、队列干净。
  下面 C0 观察到的每一条现象都成了这个用例的否定断言。
  - 实现：`internal/mcpsrv/owner_fence.go` `requireOwner`——`Options.NonOwner` 时每个写工具在 scope 检查之后、
    第一次调 VFS/export/index 之前返回 `errNonOwnerWrite`（包装 `errRequiresOwner`，后者的文案补上了
    `cloudfs mcp install --transport http`），审计行记 `denied`；覆盖 `write_file`、`edit_file`（非 dry_run）、
    `create_directory`、`move`、`copy`、`delete`、`pin`、`unpin`、`export`、`cancel_export_job`、四个 `*_upload`、
    `flush_uploads`、三个 `*_copy_job`、`index`、`unindex`（会话三工具沿用原有拒绝）。
    `TestNonOwnerRefusesEveryMutatingToolBeforeTouchingTheFS` 遍历 `tools/list`，未归类的新工具直接报错。
  - 兜底：`internal/vfs` 加 `ErrNotOwner` 与 `FS.requireOwner`（journal 存在且 `!Owner()` 才生效，无 journal 的
    只读装配与测试不受影响），`WriteFile/Create/Open(write)/Mkdir/Remove/Rename/Copy` 在任何 meta 变更之前返回它；
    fusefs/winfs 映射为 `EROFS`（内核挂载永远是 owner，只是别再报 EIO）。`TestWritesNeedTheJournalOwner`
    用同目录第二次 `journal.Open` 复现非 owner，断言 provider 调用数、journal 行、meta 节点数全部不变。
  - stdio→HTTP 桥（让 stdio 进程把写转发给 owner）仍是根治方案：**三期提前候选，见计划结论**。本条剩余
    验收（doctor warn、横幅）C0 已交付，桥落地前本条保持开放。
- **2026-09-15 C0 观察（当时 e2e 复现失败，用例 `TestStdioBesideMountSharesWrites` 保持红色；已被 C0.5 的
  栅栏修掉，保留作为否定断言的依据）**：同进程第二次 `daemon.Open` 同一 cache 目录拿不到 journal/agent flock
  （flock 按打开文件描述计，`d2.Journal.Owner() == false` 已断言），再按 `cmdMCP` 非 owner 分支起
  `mcpsrv.New(Options{FS: d2.FS, NonOwner: true, Sessions: d2.Sessions})`，两个 daemon 共用同一个
  `fakeprovider.Shared` 实例（`remotes.demo: {type: fake, shared: <key>}`，模拟同一账号）。观察到（逐字）：
  - stdio 侧 `write_file /demo/side.txt` 返回工具错误 **`journal: publication requires storage ownership`**
    （`internal/journal/publication.go` `MarkPublished` 非 owner 拒绝；此时 `commitWrite` 已经把节点写进共享
    meta、把本地链接装进 d2 自己的块缓存）。stdio 侧随后 `stat` 得 `/demo/side.txt: file, 6 bytes, local`、
    `read_text` 得 `6 of 6 bytes`——它自己认为写成功了。
  - 挂载侧（owner）：`stat mnt/demo/side.txt` 立刻可见（0.8 ms，6 字节），**`cat` 得 `input/output error`**
    （owner 的 `cache.Cache` 内存索引没有那份本地链接，`blockfetch` 对 `cloudfs-local:` id 返回
    `errLocalOnlyGone`）。journal 行停在 `state=pending needs_publish=true`，owner 上传器按
    `needs_publish = 0` 取行，永远不领它；等了 4.5 s 网盘上仍没有该文件。
  - `create_directory /demo/sub` 正常（直接调 provider，挂载侧与网盘都立刻有）；终端写的 `shell.txt`
    上传落地后 stdio 侧 `read_text` 能读到——**读路径与目录操作没问题，坏的只有文件写入**。
  - stdio 侧退出、owner 重启后：`RecoverPublications` 把那条 `needs_publish=1` 的行发布并上传，
    `/demo/side.txt` 读回 `hello\n`，网盘上出现该文件——**agent 被告知失败的写入在下一次 `cloudfs mount`
    启动时"复活"**。
  - 断言 (a)"5 s 内挂载侧读到"失败（错误信息含上面两条）；(b)(c) 因 (a) `Fatalf` 未执行。
  - **决定**：stdio→HTTP 桥提前（见计划"结论记录"）。桥落地前的栅栏由 C0.5 完成（见上），mcpsrv 与 vfs 各一道。
- **本条已交付（C0）**：`internal/agent/heartbeat.go`（`WriteHeartbeat/RemoveHeartbeat/LiveStdioProcesses`，
  `<cache.dir>/agent/stdio-<pid>.hb`，30 s 一次，超 2 min 陈旧并清理）；`cmdMCP` 非 owner 写心跳、退出删除、
  传输结束时 `Server.FinishStdioSessions` 结束自己的会话（修掉 T-34 遗留"stdio 会话退出后仍 active"）；
  doctor `checkAgent`（`agent_db` schema 版本、`agent_stdio` 心跳 warn，`Fix` 为 `cloudfs mcp install
  --transport http`，`doctor.agent.*` 中英文键）；`GET /mcp/connect` 的 `stdio_non_owner` 按心跳填；
  控制台 `connect_view.js` + 接入面板横幅链 `#/diagnostics`；`docs/mcp.md`"与挂载并存"改写。
  测试：`TestLiveStdioProcessesDropsStaleHeartbeats`、`TestFinishStdioSessionsClosesTheProcessSession`、
  `TestStdioHeartbeatLivesWithTheServer`、`TestDoctorWarnsWhenStdioRunsBesideTheMount`、`TestDoctorReportsAgentDB`、
  `TestMCPConnectReportsAStdioServerBesideTheOwner`、`TestConnectPanelWarnsAboutStdioNonOwner`、
  `_tests/connect_view.test.mjs`、`TestDoctorOnALiveSystem` 加 `agent_db`/`agent_stdio`。
- **证据**：`cmd/cloudfs/main.go` 的 `cmdMCP`（当前约 613 行，`daemon.Open` 在 639 行）走 `daemon.Open`；mount 在跑时 stdio 进程非 owner，
  `internal/daemon/daemon.go:314-321` 给它独立 VFS（写入落共享 journal，无 uploader）。
  `docs/mcp.md` "MCP 与 FUSE 挂载共用同一个 VFS 实例"只对 owner 内 HTTP 成立；journal 行由 owner
  uploader 领走但 `needs_publish` 在另一进程——行为未核实。
- **做法**：先写 e2e 复现（mount + stdio MCP 同时写同一目录、读回、排空、重启），确认是否有丢失/复活/
  延迟可见；据结果决定 stdio→HTTP 桥（SDK `StreamableClientTransport` + 原始 schema `AddTool`，约 300 行）
  是否提前；修正 `docs/mcp.md` 表述。
- **界面**：
  - doctor 新检查"MCP stdio 进程与挂载并存"：检测到非 owner MCP 进程（journal flock 持有者 ≠ 自身且有
    stdio 心跳文件）时 warn，detail 给出 `cloudfs mcp install --transport http` 命令；诊断屏自动显示。
  - 「Agent」屏接入面板横幅（见 T-35）链到诊断屏该项。
- **验收**：
  - e2e 复现用例存在并记录结论（通过或失败都写进本条）。
  - doctor 在"mount + stdio MCP"并存时报 warn，只有 mount 时 ok。
  - 界面：`ui_agents_test.go` 断言横幅在 `/mcp/connect` 返回 `stdio_non_owner: true` 时渲染并链到 `#/diagnostics`。

---

### [x] T-44 Everything 式文件名搜索：覆盖率、过滤与排序、全盘即时搜索（一期，2026-09-15 完成）

- **2026-09-15 完成**（线 B 提交 227d207、0c485dd、b3a00de、be96520、b94d8a7）：`internal/vfs/crawl.go` 后台爬取器
  （每 remote 串行、按 ino 单调扫 `dir_state.complete=0`、`yieldToForeground` 让路、风控休眠 15 min、进度只在
  `dir_state`、默认关闭；`search.crawl{enabled,remotes,exclude,idle_after,rescan}`；`warm --all` 与界面"索引整棵树"
  `{path:"/",depth:-1,all:true,confirm:true}` 同一实现，`depth<0` 必须 confirm）；`meta.Coverage`/`IncompleteDirs`/
  `Stats.LastCrawl`，`/search` 与 `/status` 带 `coverage`；`SearchResult` 加 `Kind/Size/MTime/Remote/RemoteID/Version/Cached`；
  `meta/query.go` 语法 `ext: size: dm: type: path:`、`-` 取反、引号字面量、`*`/`?` → GLOB（最长字面段走 trigram，
  否则短索引）、裸词空白切分 AND；`sort=name|size|mtime|path`（`-` 反转）；MCP `search` 加 `glob/ext/min_size/max_size/
  modified_after/kind/sort` 与 `coverage`；控制面 `/search` 同参数并每行带 `size/mtime/kind/cached`；CLI `find
  --ext --size --after --sort --type --path --json --all`；meta 迁移 v12 加 `nodes_dirs` 部分索引；查询形状改为按候选
  相关子查询重建路径，`bounded` 前进过滤；界面 `name_search.js`（默认全盘、"全盘/当前目录"分段存 localStorage、
  Ctrl/⌘+K、Esc、命中高亮文本节点、大小/时间/状态列、表头排序三态、覆盖率行、"索引整棵树"）、`search_query.js`
  过滤条双向转换、最近 10 次搜索、缓存屏"目录覆盖率"卡、检查器"列举整棵子树"。
- **验收证明**：`TestCrawlListsEveryDirectoryExactlyOnce`（1023 目录 `List` 恰 1023、二轮 0）、`TestCrawlerOffCostsNoCalls`、
  `TestCrawlYieldsToForegroundIO`、`TestCrawlSleepsAfterRiskControl`、`TestCrawlResumesAfterAnUncleanStop`；
  `TestFindFiltersByExtensionAndSize`、`TestFindSortsByModifiedTime`、`TestFindKindDirReturnsOnlyDirectories`、
  `TestGlobAndExtensionAgree`、`TestFilteredQueryStillReportsItsBudget`、`TestLegacySearchSignaturesStillWork`；
  `TestSearchScaleMillionNodeTree`（默认 50 万节点，`CLOUDFS_SCALE_DIRS=200` 跑满 100 万：名字 5.4 ms、2 字符
  35.6 ms、`ext:go size:>1k` sort=mtime 36.5 ms、`*.pdf` sort=size 46.7 ms，EXPLAIN 无 `SCAN nodes`）；
  `TestSearchGlobRespectsAllowlist`、`TestSearchCarriesCoverageAndRowFacts`、`TestFindCLIFiltersAndSorts`；
  界面 `ui_search_test.go` 十个用例、`_tests/search_query.test.mjs`；e2e 真实挂载 `TestNeverOpenedDirectoryBecomesSearchable`
  （0.3～0.5 s 可搜）、浏览器 `TestNameSearchInTheBrowser`（大小列非空）；`docs/DESIGN.md` §4.3 搜索行已改。
- **遗留**：规模基线默认 50 万节点（1M ingest 约 7 min 超包超时，`CLOUDFS_SCALE_DIRS=200` 才是整百万）；无锚点的纯过滤
  查询（如单独 `type:dir`）在 1M 上约 2 s，若过滤条常用需加 `size`/`mtime` 索引；路径子串锚点命中多数名字时（如
  `dir1/file-1`）约 6 s，是既有形状；排序作用于预算内收集集，`Complete=false` 时 top-N 不保证全局；第四列是状态
  而非路径（父路径为第二行），`sort=path` 未暴露；"索引整棵树"按钮分支未在浏览器实测（fake 无法从外部造未列举
  子树），请求体经 curl 与 `TestIndexWholeTreeAsksForConfirmation` 验证。

对照 Everything（voidtools）核对：它的两条原则是"索引等于整个卷"与"输入即结果"，附带按扩展名/大小/
日期过滤与排序。CloudFS 的引擎已经是这个形态——`internal/meta/search.go` 用父链在查询期拼路径
（不物化、目录改名不重写子树，与 Everything 的 string forest 同构）、FTS5 trigram 子串索引、1／2 字符
短倒排、工作预算 + `Complete` 诚实报告，T-09 实测名字查询 5～20 ms。缺的是 Everything 的**体验**，
不是引擎。本条前置于 T-37（T-37 的"文件名 / 内容"切换要建立在这里改造过的搜索框上）。

- **证据**：
  - 覆盖率：只索引列举过的目录。`internal/vfs/refresh.go:228-231` 对落在未列举目录的 delta 事件只把父目录
    标 stale、不补节点，所以 delta feed 不能引导整棵树；`FS.Warm`（`internal/vfs/vfs.go:1135`）手动、阻塞、
    限深度；`cloudfs find` 空结果时只在 stderr 提示"run cloudfs warm"；界面没有任何"覆盖了多少"的提示，
    `i18n.js` 也没有对应文案（`control/search.go:24-26` 的注释说 UI 必须说明，但没做）。
  - 结果太瘦：`meta.SearchResult`（`internal/meta/store.go:1391`）只有 `Ino/Name/Path`；
    `screens/main.js:339-341` 搜索结果行的大小、修改时间两列渲染为空。
  - 无过滤与排序：`mcpsrv.searchInput`（`internal/mcpsrv/server.go:328`）只有 `path/query/content/max_results`；
    `docs/DESIGN.md:340` 写"支持子串与 glob 转换"，实现里没有 glob。控制面 `/search` 同样只有 `q/path/limit`。
  - 默认范围是当前目录：`screens/main.js:336-356` 固定带 `path=cwd`；无命中高亮、结果计数、快捷键、最近搜索。
  - 规模：`TestShortSearchUsesPostingsAt100KNodes` 只到 10 万节点，百万节点无基线；T-09 自己标了
    "真实规模长期吞吐待补"。
- **做法（后端）**：
  - **覆盖**：`internal/vfs/crawl.go` 后台爬取器——每个 remote 一个串行 worker，从根 BFS 列举
    `dir_state.complete=0` 的目录，复用 `readDirRefresh`；照 `prefetcher.waitIdle`（`internal/vfs/read.go:720`）
    在前台有请求时让路；走既有三维限流与熔断，`provider.ErrRiskControl` 后休眠 15 min，`Caps.Tier=unofficial`
    并发固定 1；进度就是 `dir_state`（不加新表），重启续跑。配置新增顶层
    `search.crawl: {enabled: false, remotes: [...], exclude: [glob], idle_after: 30s, rescan: 5m}`，默认关闭
    （`rescan` 是重新扫 `complete=0` 目录的周期，delta 把目录标 stale 后靠它补列）；`cloudfs warm --all` 与界面
    "索引整棵树"（`POST /cache/warm {path:"/", depth:-1, all:true, confirm:true}`）都触发同一实现，`depth<0` 必须带 confirm。`meta.Stats` 暴露 `Dirs`/`CompleteDs`（已有）+ `LastCrawl`，
    控制面 `/search` 响应加 `coverage{listed, known, crawling}`，`status` 加同一组数。
  - **结果与过滤**：`SearchResult` 加 `Size/MTime/Kind/Cached`（`nodes` 已有列，在 `bounded` CTE 直接带出，
    `Cached` 由调用方按 `cache.Complete` 补）。新增 `internal/meta/query.go` 解析查询语法：
    `ext:go size:>1m dm:>2026-09 type:dir path:src` 与 `*`/`?` 通配（通配转 `GLOB`，锚点仍走 trigram）；裸词按空白切分
    后 AND（引号包住才是含空格的字面量）——这是 Everything 的语义，带空格的旧查询行为因此改变，写进 `docs/mcp.md`；
    过滤条件在 `bounded` 之前进 SQL，预算仍作用于匹配行；`sort=name|size|mtime|path`（默认 depth,path 不变）。
    MCP `search` 加 `glob/ext/min_size/max_size/modified_after/kind/sort`；控制面 `/search` 同参数；
    CLI `find` 加 `--ext --size --after --sort --all`。
  - **规模**：`test/perf/search_scale_test.go` 合成 100 万节点（200 目录 × 5000 文件），断言名字查询
    p95 < 100 ms、2 字符查询 < 50 ms、`EXPLAIN QUERY PLAN` 无 `SCAN nodes`；爬取器对 fakeprovider 的
    `Calls("List")` = 目录数（每目录恰一次）。
- **界面**：
  - 主窗口搜索框：默认**全盘**，旁边分段切换"全盘 / 当前目录"（选择存 localStorage）；`Ctrl/⌘+K` 聚焦、
    `Esc` 清空回到目录视图；结果行填满 名称（命中高亮，高亮片段经文本节点插入）/ 大小 / 修改时间 / 状态
    （已缓存点+文字）；表头可点排序（改 `sort=` 重新请求）；结果上方常驻一行"N 条 · 覆盖 已列举 X / 已知 Y
    目录"，未全覆盖时附"索引整棵树"按钮（`POST /cache/warm {path:"/", depth:-1}` 或开启 `search.crawl`，
    `confirmDelete` 键入 `warm`——会产生大量远端调用）；`complete=false` 的常驻说明行不变；双击结果定位到父目录
    并选中该行；最近 10 次搜索下拉（localStorage）。
  - 过滤条：搜索框右侧"筛选"展开为 类型 / 扩展名 / 大小范围 / 修改时间；选择即拼进查询串，用户能看到并手改
    最终查询（`search_query.js` 零 import 模块做双向转换）。
  - 缓存屏概况卡加"目录覆盖率"卡（已列举 / 已知、最后爬取时间、爬取中进度），SSE `status` 驱动。
  - 检查器目录项加"列举整棵子树"（`warm` depth=-1，同一确认门）。
- **验收**：
  - 爬取器：fakeprovider 1000 目录，`search.crawl.enabled` 后 `Calls("List")` 恰 1000；前台读持续时 5 s 内至少
    让路一次；`ErrRiskControl` 后 15 min 内新增 `List` 为 0；不优雅关闭后重开，已 `complete=1` 的目录不再列举。
  - `search("ext:go size:>1k")` 只返回同时满足两条件的文件；`sort=mtime` 顺序正确；`type:dir` 只返回目录；
    通配 `*.md` 与 `ext:md` 结果一致；预算被击中时 `Complete=false`。
  - 100 万节点基线：p95 达标，`EXPLAIN QUERY PLAN` 无全表扫；`test/perf` 既有调用次数基线不变
    （爬取器默认关闭，不产生调用）。
  - MCP `search` 带 `glob:"*.md"` 与 `--allow` 交集正确；控制面 `/search` 响应含 `coverage` 与每行
    `size/mtime/kind/cached`；`cloudfs find --ext go --sort mtime` 输出顺序正确。
  - 界面：`ui_search_test.go` 断言默认请求不带 `path=`（全盘）、分段切换写 localStorage、"索引整棵树"带
    `confirm: true`、表头点击改 `sort=`、高亮经文本节点插入不进 `html:`、覆盖率行文案来自 i18n；
    `_tests/search_query.test.mjs` 覆盖过滤条 ↔ 查询串双向转换与非法输入；i18n 两表一致、screens 无汉字。
  - e2e：真实挂载下，从未打开过的目录里的文件在开启爬取后 ≤ 10 s 可被主窗口搜到；浏览器冒烟结果行大小列非空；
    `docs/DESIGN.md` 搜索一行改为与实现一致。

---

## 明确不在当前范围内

以下是设计文档中标注为二期或预留的部分，列在这里是为了避免被误当作遗漏：

- ~~**Windows / WinFsp 适配**~~：2026-09-06 提前到本期，见 T-21 与 `docs/ui-plan.md` 阶段 C。
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
- **[~] 大规模 pin 性能**：已测量并修掉。**方向反了**——`reconcilePinsLocked` 原先遍历
  `cache.Keys()`，对每个缓存对象查一次 `meta.Aliases` 问"你在哪、被 pin 了吗"，代价随缓存
  大小线性增长。而它由 `dropPaths()` 调用，也就是**每次改名、每次删除都跑一遍**，还带 5 秒
  预算：缓存攒大之后，一次与 pin 毫不相干的改名会花光预算，用户看到
  "pin retention could not be reconciled"。
  改为反过来问"规则覆盖了什么"：新增 `meta.SubtreeRefs`（每条规则一次向下递归 CTE，
  只取文件的 remote/remote_id/version）与 `cache.UserPinnedKeys()`（只枚举当前被 pin 的键，
  纯内存），取消 pin 只需检查当前被 pin 的那些。规则路径解析只走本地元数据
  （`pinRuleNodeLocked`），断网也能恢复 pin。`cache.Known()` 保证"规则覆盖但从没读过"的
  内容不会在这里凭空建缓存记录——那仍由首次入块时的 `protectPinned` 负责。
  实测（`internal/vfs/pin_scale_test.go`）：缓存里 2000 个无关文件时，一次改名后的对账
  **2002 次元数据查询 → 4 次**，0.67s → 0.05s。回归即失败：把遍历改回缓存立刻超预算。
  行为不变由既有的 `TestRenameDoesNotMovePathPin`、`TestPinAliasesAreAdditive`、
  `TestOverlappingPinsAndTemporaryWriteProtection`、`TestPinSurvivesRestartWithoutNetwork...`
  守住，另加 `TestReconcileFollowsContentMovedIntoAndOutOfAPinnedDirectory` 双向验证。
  顺带：`meta.Aliases` 此前不计入 `QueryStats`，所以这条成本在计数器里是隐形的，已补上。
- **[ ] pin 的真机验收**：macOS/Linux 内核挂载仍待环境验收。详细行为见 `docs/cache-management.md`。

### 原阶段计划（需结合以上进展使用）

按"能装上 → 敢长用 → 想留下 → 扩边界"分阶段。P0–P2 是**对着设计文档**核对出的缺口，
P3 是**对着竞品**核对出的缺口；两者交错推进，因为前者决定质量、后者决定有没有人用。

### [x] T-22 上传成功后队列不放开磁盘（2026-09-06，新发现）

查"完整终态历史回收"这条时发现的**真实磁盘泄漏**，不是文档与实现不符，是数据一直在涨。

- **证据**：三次成功上传后 `<journal>/objects/` 里三个对象仍在，`links=2`。暂存 blob 与
  读缓存是同一个 inode 的两条硬链接，`OnSuccess` 先 `cache.LinkFile` 装进缓存，
  uploader 再 `Journal.Succeed`——而 `Succeed` 只改状态，队列自己那条链接从不释放。
- **影响**：缓存可以淘汰自己那条链接，**字节永远回收不掉**；`Recover` 的存活集合是
  "表里所有行"（这条是对的，死信要靠它保住 payload），`done` 行也算，重启也不清。
  净效果：用户写过的每一个文件都在 journal 目录里留一份完整副本，直到重装。
  这不受 `cache.max_size` 约束，也不出现在任何统计里。
- **修法**：`Succeed` 在同一事务里清空 `blob_path` 并记 `done_at`，提交后再
  `removeBlobIfUnreferenced`——内容寻址去重时另一条待传行仍引用则不删，死信 payload 不动。
  清空与删除之间崩溃只留下一个没有行指向的对象，正是 `Recover` 该清的那种。
- **顺带把终态行也定界了**（这就是原来那条"完整终态历史回收"）：保留最近 200 条且完成
  不足 5 分钟的，两个条件都越过才回收。留着是因为 `strict` 模式的写入方靠轮询这一行看到
  完成；年龄下限保证行不会从等待者脚下被抽走。journal schema v12 → v13（`done_at`）。
- **复制也走同一条路径**：服务端做不了的复制把 payload 暂存在 `copies/`，`SubmitCopy`
  交给上传行，因此现在上传成功时一并释放，而不是等下次重启的 `copyRetention`。
  批量复制不再需要双倍磁盘直到守护进程重启。
- **回收要带上副表**：一次上传如果中途被取消再显式续传、或死信后被重试，会在
  `upload_cancellation` / `upload_resume_history` / `dead_letter` / `upload_parts`
  留下按 upload id 建的行。只删主表等于把无界增长挪个地方，所以回收连它们一起删。
  `upload_discarded` 故意不动——那是阻止已丢弃的 upload id 复活的记录，不是历史；
  带着未完成 cleanup 意图的行整条不参与回收，那是恢复状态。
- **补上可观测性**：`cloudfs doctor` 新增 `queue_objects`，报告磁盘上没有任何队列行指向的
  payload 数与字节数（`Journal.OrphanObjects`，只读，非 owner 进程也能问）。正常为零；
  崩溃落在"删行"与"删对象"之间会留几个，下次启动的 `Recover` 清掉。单列是因为这些字节
  既不在 `cache.max_size` 里也不在队列待传总量里——上面那个泄漏正是靠"没有任何报表会
  提到它"活下来的。
- **回归**：`internal/vfs/upload_reclaim_test.go`（三次成功上传后对象目录为空，且回读
  零后端请求——证明缓存那条链接还在；回退修复后报 "3 objects (51 bytes)"。另一条断言
  完成的复制不留 payload，回退后报 "left 1 payloads staged"）、
  `internal/journal/succeed_reclaim_test.go`（共享内容不误删、死信 payload 不动、
  终态历史的上下界）。详见 `docs/upload-cleanup.md`。

### [x] T-23 内置代理规则指名一个从不存在的出口（2026-09-06，新发现）

画 UI 方案时顺手核对「国外网盘是否都支持代理」，结果发现**管道层早就通了、默认配置反而是坏的**。

- **能力本来就在**：`gdrive/box/dropbox/onedrive/s3/webdav` 经 `httpx` 的代理路由，
  `sftp/smb` 经 `provider.DialerFrom` 注入的同一个拨号器（`internal/provider/smb/factory.go:92`）。
  没有绕开规则的路径。
- **但 `DefaultRules` 里的目标是字面量 `proxy`，而 `proxy` 这个出口从来不存在**——
  没有配置时 `NewManager` 只内建一个 `direct`。于是：

  ```
  www.googleapis.com  -> proxy: unknown outbound "proxy"
  api.dropboxapi.com  -> proxy: unknown outbound "proxy"
  graph.microsoft.com -> proxy: unknown outbound "proxy"
  api.box.com         -> proxy: unknown outbound "proxy"
  ```

  这不是边角情形，是两种最常见的配置：**完全没配代理**（用户加个 Google Drive 账号
  什么都不配，账号直接不可用，报错还是我们的内部术语），以及**配了代理但没起名叫
  `proxy`**（hk / auto 这种真实会用的名字）——后者同样全线失败。
- **另外 S3 根本没有内置规则**，会落到 `FINAL,direct`：旁边四家都走代理，唯独它裸奔。
- **修法**：新增 `defaultRulesFor`（`internal/net/proxy/default_target.go`），在建 router
  之前把占位符换成配置里真实存在的东西，顺序是「名字就叫 `proxy` 的出口或组 → 第一个组 →
  第一个非 direct 出口 → `direct`」。最后一档只在配置里完全没有代理时成立——用户没要求
  代理，这套规则又是我们内置的。**用户自己写的规则不参与替换**：写了 `,proxy` 而没定义它
  仍然报 `unknown outbound`，绝不悄悄直连（这正是规则引擎存在的意义）。
  同时补上 `DOMAIN-SUFFIX,amazonaws.com,proxy`（`amazonaws.com.cn` 是另一个后缀，
  仍走 `GEOIP,CN,direct`，这对 AWS 中国账号是对的）。
- **回归**（`internal/net/proxy/default_target_test.go`，逐条验证过回退即失败）：
  没配代理时境外域名解析到 `direct` 且不报错；出口叫任何名字都能被内置规则找到；
  组优先于裸出口；`direct` 类型的出口不被误认成代理；用户手写的未定义出口仍然失败；
  五个境外驱动的端点逐个断言走代理，AWS 中国走直连。
- 文档：`docs/DESIGN.md` §4.2 的默认规则表已补全并写明 `proxy` 是占位符不是出口名。

### 2026-09-06 收尾：本机能做的都做完了

这一轮把**不需要外部资源**的模块全部做完并补齐单测（详见各条 T-xx 下的当日记录）：
T-02（gdrive/box/smb）、T-00b、T-00g（新发现的两个数据回退）、T-19（可写 DAV）、
T-09（搜索限界）、T-20（媒体库成本断言）、T-04（服务端复制远端对账）、
T-03（慢客户端隔离）、T-06（交互式向导）、T-17（Web 加账号）。
全量 `test`、`test -race`、`vet`、`gofmt` 均干净。

**剩下的都卡在本机拿不到的东西上，不是没写代码**：

| 条目 | 卡在哪 |
|---|---|
| T-08 / T-10 passthrough、writeback_cache | 注册 backing fd 需要 `CAP_SYS_ADMIN`，本机无 sudo，内核直接拒绝。无法验证的内核集成重构不应盲写 |
| T-11 / T-13 51 处 UNVERIFIED、真实冒烟 | 需要各网盘的真实账号 |
| T-14 三项性能基线 | 需要真实网盘与真实网络条件 |
| T-15 macOS | 需要装了 macFUSE 的真机 |
| T-12 pjdfstest 干净清单 | 需要 root（切 uid 的用例） |
| T-16 LICENSE | 是项目所有者的法律决定，不是代码 |
| T-21 Windows | 是排期决定，不是代码 |

**阶段 0 — 能装上**（历史排序，保留作为上下文）

1. **T-16**（版本控制 + 许可证：一切协作、CI、发布的前提）—— git 已初始化；LICENSE 仍待所有者决定
2. **T-01**（配置谎报，改动最小，顺手清掉）—— 已完成
3. **T-06 → T-05**（授权流程与凭据存储）—— 向导已补齐，真实账号验收仍缺
4. **T-18 + T-07**（Docker 镜像 / CI / release / systemd / launchd，同一件事的两半）
5. **T-17**（最小 Web UI，只读优先）—— 已完成，另加了不含凭据的加账号入口

**阶段 1 — 敢长期用**

6. **T-08 / T-09 / T-10**（承诺与实现不符，纯本地工作，不需要外部资源）
7. **T-11 → T-13**（拿到真实账号之后：核对 `UNVERIFIED`，跑真实网盘冒烟）
8. **T-12 / T-14 / T-15**（pjdfstest / fsx / fio、性能基线、macOS 真机）

**阶段 2 — 想留下**

9. **T-04**（跨盘复制/秒传：底层去重能力已经写好，只差接出来，性价比最高）
10. **T-19**（对外 WebDAV / 直链输出：进入 Emby / Jellyfin / OpenList 生态的唯一通路）
11. **T-20**（媒体库场景：STRM、跳播友好的预取；依赖 T-19）
12. **T-29 → T-30 → T-32 → T-31 → T-33**（存储池 v2，`docs/pool-v2.md`：先做便宜的内核/合并/限流项，
    再做小文件目录预取，然后导出作业（用户可见价值最高、不碰读路径核心），之后视频扇出，最后放置 v2——
    它改索引 schema 与多机标记，放最后）

**阶段 3 — 扩边界**

12. ~~**T-02**（境外网盘）~~ —— 2026-09-06 已按各自官方接口自研补齐 gdrive / box / smb，未引入 rclone，剩下的只是真实账号与真机验收（并入 T-13）。
13. **T-03**（MCP Resources；"agent 可安全读写的云盘"是全品类空白，也是唯一不与
    CloudDrive2 正面拼价格的差异点）
14. **T-21**（Windows / WinFsp：排期决策，取决于目标用户是 NAS 还是桌面）

**阶段 4 — Agent 工作底座**（2026-09-14 登记，T-44 于 2026-09-15 追加，见 P4 节与 `docs/agent-roadmap.md`）

15. **一期（并行两线）**：线 A T-34 → T-35 → T-36；线 B T-44 → T-37（T-44 改造主窗口搜索框，T-37 在它
    之上加"内容"分段）。每条后端任务后紧跟界面任务，界面不落地不关条目。**2026-09-15 全部完成**，34 个提交在
    `feat/agent-phase1`，一期总验证见各条"验收证明"。
16. **二期**：T-43（先核实拓扑）→ T-38（线 C，2026-09-15 完成：T-38 关闭，T-43 的验证与栅栏交付、桥转三期首位）；
    T-39 → T-40（记忆检索依赖嵌入可选，keyword 即可先上）；
    T-41 → T-42（运行按钮依赖 exec 执行器，复制提示词可提前到一期末）。
17. **三期**：**stdio→HTTP 桥（T-43 结论，排第一）**；递归删除逐文件前像；control/WebDAV 操作进审计；`pull_events`；
    HNSW（仅实测 p95 > 200 ms）；团队 principal owner；多选文件发送给 Agent。

**不建议做的事**：靠限制挂载数量收费（CloudDrive2 的 freemium 模式）。
本项目的口碑点在可靠性——限流防封号、写日志不丢数据、背压不写满磁盘——
用阉割数量来收费会把最强的那一项浪费掉。若要变现，JuiceFS 式的开源核心
（单机免费，团队/多节点共享缓存、审计、集中授权收费）与现有架构更相容。
