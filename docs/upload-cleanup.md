# 上传清理：持久化基础与接线边界

## 当前状态

Journal schema v11 已实现可恢复的上传清理意图、不可恢复上传的 purging 状态和
最小丢弃凭据。状态查看、metrics、doctor、Flush、普通改名/删除拒绝及 FUSE/MCP
错误映射已识别该状态。**VFS DiscardUpload、daemon 启动续清理及在线/离线
CLI/control 入口已接入**。旧管理命令直接 DropPending 的路径已移除；当前接口
清理已停止且仍选中的本地版本，不等于任意历史记录/远端结果管理都已完成。

```sh
cloudfs uploads cancel <upload-id>
cloudfs uploads list --json                # 等待状态从 cancelling 变为 cancelled
cloudfs uploads drop <upload-id> --confirm
```

drop 是不可恢复的本地删除，不能代替 cancel：pending/uploading/dead/done 直接
drop 返回错误，dead 可先取消；已取消但本地版本已替换/缺失同样拒绝首次清理。
必须先确认不再需要该版本，并关闭对应文件的所有读写句柄。purging 可以再次
drop 继续此前确认的清理；清理已完成后再 drop 返回不存在，不制造一次新的成功。

HTTP 为 `POST /uploads/drop`，唯一请求体为 `{"id":"...","confirm":true}`。
要求本地原生 Host、无浏览器来源、`X-CloudFS-Control: 1`、`application/json`；
拒绝查询参数、重复字段、大小写变体、其他选择器、超大请求和尾随 JSON。
成功返回 `discarded` ID 与风险说明，不用 done 状态暗示远端成功；句柄/状态/
身份冲突返回 409，只读挂载 403，不存在 404，内部失败不输出私有存储错误。

CLI 优先调用运行中 daemon。建立连接后的中断/超时/响应丢失/服务器错误均不
转离线重放。只在控制端点尚未连接且明确不存在/拒绝连接时走离线：先确认已有
数据库并获得 journal owner，再使用相同 VFS 协调器打开原元数据库与缓存。
离线不初始化 provider、凭据或代理，不创建挂载节点，不运行 Journal.Recover、
本地发布恢复或其他上传；自动搜索索引维护（包括 Close 时刷新）也关闭。正常
存储打开仍可按 owner 规则迁移旧 schema，并读取缓存索引，不等于完全无数据库写入。

元数据层新增 `Store.RemoveLocalVersion`：在原数据库的 FULL 事务中，核对调用方
确认的全部本地文件别名快照，原子删除该版本、清理搜索记录并推进父目录刷新代次。
即使重试时已无节点，也通过真实写入同步此前删除。VFS DiscardUpload 已调用
该方法，并在同一清理流程处理句柄、挂载策略、缓存引用与最终 Journal 收尾。

## 存储契约

1. 先通过 Uploader.Cancel 停止传输，等待 cancelled。cancelling 还不是 worker
   已退出的证明，不能直接清理；tombstone 删除补偿同样拒绝。
2. `BeginUploadCleanup(id, metaIdentity)` 在一个事务中记录规范化的私有 payload
   路径、元数据库身份，并将 cancelled 变为 purging。原上传行、session、parts
   和恢复历史仍保留。重复 Begin 要求身份仍相同，不允许重新进入 pending。
3. VFS 必须检查挂载/路径策略和打开句柄，持久化移除**该上传对应的本地版本**，
   在原元数据库做真实 FULL 屏障，确认所有相关元数据引用消失；随后执行有错误
   返回的缓存 unlink/fsync。不能只把内存中的“没有引用”当作持久化证明。
4. `FinishUploadCleanup(id, metaIdentity)` 只能在上述条件满足后调用。它验证
   原身份和路径，保护其他上传与未入队的 staging 引用，删除无引用的私有名称，
   power 模式同步所在目录，最后在一个事务中删除上传及其私有会话/分片/取消/
   恢复历史，保存不可重用此 upload ID 的最小凭据。

第 3 步已接入 VFS 协调器；Journal 不依赖 meta/cache，不能自行证明 VFS
引用已消失；API 的 metaIdentity 参数是调用方契约，不是可绕过第 3 步的授权令牌。
不得将两个 Journal 方法直接接到 HTTP/CLI/MCP。

## VFS 协调与启动恢复

`FS.DiscardUpload(id, confirm)` 要求显式确认、journal owner、已 cancelled 且非
tombstone 的完整本地发布。首次操作只接受该上传仍是原 inode 的当前本地版本；
已替换/缺失版本不能凭原 inode 猜测删除。v11 上传带有原账号/元数据库绑定，但这
仍不能凭已失效 inode 猜测该删哪个历史版本，也不能替代远端对账；任意终态记录的
历史清理尚未因此完成。

- 核对全部已知别名的版本、长度、名称、父目录与可写挂载；submitted Copy 还
  核对准备记录的原数据库及目标挂载。初始拒绝不写清理意图。
- 读 Open 与改名/删除/复制交接/显式恢复共用 publication gate；清理再排除
  Create 和远端目录发布。检查别名 inode、原 inode 的活动写者，以及读者初次
  打开时的不可变缓存身份。关闭中的写者到提交结束才解除保护；关闭中的读者
  到最后一次在途 Read 结束才解除保护，已关闭句柄的新 Read 返回 EBADF。
- MCP 的 ReadFileRange 和 pin 下载也通过 Open/Release 登记，不能用未登记的
  临时句柄绕过引用检查。打开及 IO 的规模开销仍需性能验收。
- Begin 后元数据删除/缓存删除/最终 Journal 收尾失败均保留 purging；元数据
  删除已提交时立即通知 FUSE/MCP 所有被删除路径，即使后续缓存清理失败。
  临时 upload pin 和该版本的用户缓存标志被释放，用户持久路径规则本身保留。
- 已经取得的独立完整缓存租约继续可读并计量到 Close；清理只删除私有名称，
  不承诺立即释放所有物理字节。其他上传和复制源硬链接仍由 Journal 引用检查保护。
- daemon 在 Journal.Recover 后、本地发布恢复与上传/刷新 worker 前，以有界
  队列分页续清理已有意图，不自动取消新上传。原元数据库身份不匹配或清理失败
  时拒绝继续启动并保留意图，不跳过失败去启动上传。已完成元数据删除的重试
  可以使用零别名快照继续，绝不按同名路径删除替代文件。

此流程不调用 provider，不证明或撤销任何远端结果。在线/离线管理入口与重放
防护已有测试；首次历史版本清理、长期故障/真实环境与完整历史保留仍需继续完善。

## 上传成功之后的回收（2026-09-06）

暂存 blob 与读缓存是**同一个 inode 的两个硬链接**：`OnSuccess` 先把它装进缓存
（`cache.LinkFile`），再由 uploader 调 `Journal.Succeed`。此前 `Succeed` 只把行改成
`done`，队列自己那条链接从不释放，于是：

- 缓存可以淘汰自己那条链接，**字节却永远回收不掉**——journal 仍然指着这个对象；
- `Recover` 的存活集合是"表里所有行"，`done` 行也算，所以重启也不会清；
- 净效果是**用户写过的每一个文件都在 `<journal>/objects/` 留一份完整副本，直到重装**。
  三次上传后 `ls -l` 看到的就是 `links=2`。

现在 `Succeed` 在同一个事务里把 `blob_path` 清空并记 `done_at`，事务提交后再
`removeBlobIfUnreferenced`（内容寻址去重时另一条待传行仍引用则不删，死信的
payload 也不动——那正是 `uploads retry` 要重发的东西）。清空与删除之间崩溃只会留下
一个没有任何行指向的对象，而那恰好是 `Recover` 负责清掉的情况。

终态行本身有界：保留最近 `doneHistory`（200）条且完成时间在 `doneRetention`（5 分钟）
以内的不动，两个条件都越过才回收。留着是因为 `strict` 模式的写入方要靠轮询这一行
看到上传完成；年龄下限意味着要把行从等待者脚下抽走，它得先卡住好几分钟。
journal schema v12 → v13（新增 `done_at`，升级前完成的行为 0，一律保留）。

服务端不能做的复制走同一条路径：payload 暂存在 `copies/`，`SubmitCopy` 把它交给上传行，
因此上传成功时一并释放，而不是等到下次重启的 `copyRetention` 才清。批量复制不再需要
双倍磁盘直到守护进程重启。

回收终态行时连同按 upload id 建的副表一起删（`upload_parts`、`dead_letter`、
`upload_cancellation`、`upload_resume_history`）——取消后续传、死信后重试都会在那里留行，
只删主表等于把无界增长挪个地方。`upload_discarded` 故意不动：它是阻止已丢弃 upload id
复活的记录，不是历史。带着未完成 cleanup 意图的行整条不参与回收，那是恢复状态。

`cloudfs doctor` 增加 `queue_objects` 检查：报告磁盘上**没有任何队列行指向**的 payload
数量与字节数。正常应为零；崩溃落在"删行"与"删对象"之间会留下几个，下次启动的
`Recover` 会清掉。之所以要单列，是因为这些字节既不在 `cache.max_size` 里，也不在队列的
待传总量里——上面那个泄漏正是靠"没有任何报表会提到它"活了这么久的。

回归：`internal/vfs/upload_reclaim_test.go`（三次成功上传后对象目录必须为空，
且回读零后端请求——证明缓存那条链接还在；另一条断言完成的复制不留 payload，
回退修复后报 "left 1 payloads staged"）、`internal/journal/succeed_reclaim_test.go`
（共享内容、死信 payload、终态历史上下界）。

MCP 的 `discard_upload` 也调用同一 VFS 协调器，并要求 `confirm=true`。它只向没有
`--allow` 路径限制的服务开放：元数据删除后失败会留下无虚拟路径的 purging 意图，
若首次允许受限服务操作，后续就无法再次证明相同路径权限。受限 MCP 仍可在当前
版本路径可验证时 list/get/cancel/retry/resume；全队列 `flush_uploads` 同样仅向
无限制服务开放。任何不确定响应都要求先重新查询，不能自动重放修改请求。

## 故障和竞争保护

- Begin 的 SQL 失败整体回滚，仍为 cancelled。Finish 的 unlink、目录同步窗口
  或最终 SQL 失败均保留 purging 意图；文件已删也可以重试，不能误报完整成功。
- Journal.Recover 保留仍有 purging 行的 payload，不校验缺失文件后把它重新改成
  dead/pending，也不自行跳过 VFS 证明调用 Finish。daemon 已在普通发布前调用
  VFS 续清理；只读非 owner 不运行该流程。
- 状态更新、普通 retry/drop、retarget、tombstone、MarkPublished、重新 Commit
  都不能绕过 purging。迟到的 session/part 回执也拒绝；取消状态仍允许记录刚返回
  的远端信息，以保留原取消契约。清理完成后保留 ID 凭据，防止迟到 Commit 复活
  队列、RecordPart 重建孤立分片记录或 Fail 重建死信。
- 已准备好的恢复请求与 Begin 在 writer 事务上只有一个赢家；状态检查不会被旧的
  恢复校验结果绕过。丢弃后缺失的 payload 不能被另一个 ID 直接领用；真正重新
  staging 的相同内容可以按内容寻址生成新上传，旧 upload ID 仍不可复用。
- 清理遵守 objectMu → writer 锁顺序，涵盖已完成 staging 但尚未入队的预留。
  引用检查逐行读取其他上传，比较规范化路径并保守识别 inode 别名，不把全部
  队列加载进 Go 数组。其他 owner 仍存在时不删除共享内容。
- 普通 Drop/DropPending 的旧 GC 路径也复用该引用检查，修复相对/绝对路径
  混用时漏判其他 owner 并误删内容的问题；符号链接父路径别名用 inode 识别，
  无法读取引用身份时保守保留。旧 GC 仍为 best effort，不因这项修复成为完整
  可恢复清理。检查最坏仍扫描全部上传，并增加文件身份查询开销；未提供大队列
  延迟上限或持久化规范路径索引，相关规模优化保持待办。
- 只接受 objects 的直接子项或本上传自己的 copies/<uuid>.part；记录路径变化、
  外部路径、目录、payload 符号链接或直接父目录符号链接均拒绝。该防护不是对
  能并发修改私有 journal 目录的本机同权限攻击者的完整隔离。
- 已 submitted 的复制上传可以清理其私有链接，但不改写准备任务的 submitted
  历史，也不触碰源文件硬链接。准备记录仍需单独 forget；submitted 不代表远端成功。
- 最小凭据包含 upload ID、meta identity、规范化 blob 路径，保存在私有数据库，
  不包含 session/parts/hash，不对控制面输出。凭据目前不自动 GC；SQL 逻辑删除
  也不是安全擦除，数据库空闲页、WAL 或备份可能仍含旧数据。

## 可观测性

`uploads list` 的有界分页包括 purging。status 的 `uploads.purging` 和
`cloudfs_uploads_purging` 展示未完成清理数量，doctor 给警告且不提供自动重试修复。
RetainedBytes 是停止/清理记录的逻辑长度之和，不是实际磁盘占用；unlink 后 SQL
失败时记录仍存在，不能据该计数推断文件仍完整可读。

Flush 在 purging 存在时返回 ErrCleanupPending，不把“没有 pending/uploading”
等同于上传完成。普通改名/删除不得穿过清理状态，FUSE 返回 EBUSY，MCP 返回
不含私有路径的清理提示。HTTP retry/flush 对相应状态给冲突，不重新上传。

## 已验证和剩余验收

专项覆盖迟到状态写入、prepared resume 竞争、完成后 ID 不复活、会话历史删除、
已提交及未提交的共享内容、相对/绝对别名、复制源硬链接、路径和身份拒绝、只读
v9 查看与 v10 owner 迁移、SQL 回滚、unlink/同步窗口错误、重开与重复完成。
同一测试二进制的子进程在意图已落盘、unlink 后、最终提交前三个位置被强杀，
父进程重新打开存储并续清理。它是 Journal 层进程崩溃测试，不是在线 drop、
生产 CLI、真实账号、FUSE 或物理掉电验收。

元数据专项另覆盖全部别名/搜索索引删除、原数据库身份、快照改变、遗漏或新增
别名、跨 remote 异常引用、目录/文件下异常子节点拒绝、删除及最终屏障失败回滚、
缺失屏障行拒绝、重开重试、独立数据库连接并发改名。临时移除刷新代次保护后，
测试复现旧列表重新发布被删除版本，恢复保护后通过。此处的全量别名快照和
remote_id 扫描仍有规模成本，且 API 调用方必须阻止事务之后的新发布，不能把
事务内核对误当作任意外部写者都无法重新引用的全局保证。

VFS 专项新增读/写/别名打开、路径型 MCP 读取、关闭中的读者和提交中写者、
读打开与清理竞争、各持久化阶段失败、真实缓存 unlink 失败、错误元数据库、
共享上传、固定规则、独立缓存租约、submitted Copy 源及准备历史保护。
VFS 测试子进程在 intent/metadata/cache 三阶段被强杀，父进程重建全部组件
后续清理并验证未重新发布或上传；daemon 测试覆盖三个重启边界的真实装配顺序。
这些是模拟后端的存储/进程测试，不替代 FUSE、账号或物理掉电验收。

生产进程专项另构建并运行真实 cloudfs：通过实际 Unix 控制端点执行 drop，
由另一个 SQLite 写事务阻塞元数据提交，观察 purging 已持久化后强杀 CLI 与
MCP-only daemon，释放事务后启动新生产 daemon 续清理。断言原 inode、journal
payload 和缓存链接均消失且后端请求数不增加；不使用 VFS 故障钩子或测试假驱动。
后端为本地 HTTP WebDAV 服务，此结果仍不是实际网盘账号或物理掉电验证。

后续必须完成：

- 更广泛的历史版本/别名策略及跨客户端并发、真实内核/长期缓存故障验收。
- 扩充生产命令的长期磁盘故障矩阵；历史查询/保留策略/凭据 GC 的完整设计。

普通上传的本地账号/元数据库围栏已由 journal v11 实现；远端未知结果对账、
准备目标改名和跨客户端原子不覆盖仍是独立待办。清理只处理本地数据与管理记录，
不撤销或证明任何远端操作结果。
