# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

CloudFS：把国内外网盘挂载为本地目录的单机守护进程，同时通过 MCP 把同一份文件系统暴露给 agent。
Go 1.27，模块名 `cloudfs`，约 105k 行、41 个包目录、1141 个测试函数。

## 工具链与常用命令

仓库里的 `./gow` 包装脚本先用系统 Go（`command -v go`），没有才回退到会话 scratchpad 里的
工具链。若两者都没有，需要重新下载 Go 并改写 `gow` 顶部的 `SP=` 路径（同时把
`GOMODCACHE`/`GOCACHE` 指到可写目录）。下面所有命令用 `./gow` 代替 `go`。

```sh
./gow build ./...
./gow vet ./...            # 基线是干净的，不要引入新告警
./gow test ./...           # 全量
./gow test -race ./...     # 基线无竞态告警

./gow test ./test/chaos/        # 可靠性矩阵：断网、kill -9、限流、缓存满、冲突
./gow test ./test/conformance/  # 与本地目录逐项比对 POSIX 行为
./gow test ./test/perf/         # 冷/热遍历、读放大、预取的**调用次数**基线
./gow test ./test/e2e/          # 真实挂载 + MCP 端到端

# 单个测试 / 单个包
./gow test ./internal/vfs/ -run TestWriteFlush -v
./gow test ./test/chaos/ -run TestConflict -race -v
```

FUSE 相关测试需要 `/dev/fuse`（Linux）或 macFUSE（macOS）。缺少时测试会 `t.Skip`
而不是失败——所以**看到 skip 要确认是不是环境问题，而不是当成通过**。没装 macFUSE 的
macOS 上，`internal/fusefs` 的 `TestFlushSurvivesAnInterruptedRequest` 与
`test/conformance` 的 `TestCreateReadWriteMatchLocal` 会**挂住直到超时**（不是 skip），
所以在那种机器上跑全量要把 `./internal/fusefs`、`./test/conformance`、`./test/e2e`
排除掉再看结果。

手工冒烟：写一份用 `type: fake` remote 的配置，`./cloudfs mount` 后台跑，再用普通
shell 命令验证。真实内核路径能暴露单元测试发现不了的 FUSE 语义问题。

## 分层架构

调用自上而下，每层只依赖下一层；`internal/daemon` 是唯一知道全部装配关系的地方。

```
cmd/cloudfs ── fusefs (内核) ─┐
            └─ mcpsrv (agent) ┴─→ vfs ─→ meta / cache / journal / upload ─→ provider ─→ httpx ─→ net/{proxy,ratelimit,retry}
```

- **`internal/vfs` 是文件系统核心**，`fusefs` 与 `mcpsrv` 都是它的薄适配器。任何关于缓存、
  一致性、上传的判断都属于 vfs；fusefs 只做 inode 身份、timeout、errno 映射。加功能时
  不要把逻辑写进适配层。
- **`internal/provider` 的 `Caps` 能力矩阵是唯一的分支依据**。上层永远不按网盘名字特判，
  新增行为差异要加 `Caps` 字段而不是 `if name == "quark"`。例如 `Caps.PathIDs` 表示"id 就是
  路径"（sftp/webdav/s3/smb），改目录名后 vfs 据此重写子孙的 `remote_id`。
- **`internal/provider/httpx` 是所有驱动共用的 HTTP 客户端**，代理路由 + 三维限流 + 熔断 +
  错误分类都在这一层。daemon 通过 `provider.ConfigHTTPClient`（`"_http_client"`）这个 cfg key
  把配好的 client 注入驱动工厂；驱动应调用 `httpx.HTTPClientFrom`，只有该 key 缺席
  （单测场景）才自建 client。
- 驱动通过 `init()` 里的 `provider.Register(type, factory)` 注册，**必须在
  `cmd/cloudfs/main.go` 加对应的空导入**才会被链接进二进制。

读路径：`vfs/read.go` 从块缓存取 4 MiB 块，缺块经 `vfs/flight.go`（自实现的 singleflight）
合并并发请求后走 provider；顺序性检测驱动 readahead。
写路径：写入落到 `journal` 的 staging 文件 → FLUSH 时 fsync + 提交日志（`close()` 此时返回）
→ `upload` 队列异步秒传/分片上传。
元数据：`meta` 是 SQLite 目录树 + TTL + 负缓存 + delta 游标 + pin + 文件名 trigram 索引；
热目录零远端调用，`vfs/refresh.go` 用 provider 的 delta feed 保持同步。

## 必须知道的实现约束

这些是真实挂载测试暴露出来的，读代码不容易发现，改写路径 / 冲突逻辑前务必先读：

- **提交发生在 FUSE 的 FLUSH，不是 RELEASE**：内核不等待 RELEASE。而 FLUSH 会为每个关闭的
  fd 触发多次，所以 FLUSH 提交后句柄必须保持可写；`writeState.lastUploadID` 用来让后一次
  flush 顶掉前一次排队的上传（否则 shell 重定向会先传空文件再传真内容）。
- **冲突检测比对 `meta.Node.RemoteVersion`（最后已知的远端版本），不是 `Version`**。后者在
  本地写入后会变成待上传标记，用它比对会把自己的连续写误判成他人冲突。
- **未上传的文件用 `cloudfs-local:` 前缀的假 RemoteID**（`internal/vfs/write.go`），读取走
  cache 里的硬链接。重命名/删除这类文件必须改写或取消 journal 里的待传记录，不能去问服务端。
- **一个 inode 同时只能有一份暂存快照**。每个写句柄自带一个 staging 文件，所以路径式
  truncate 绝不能自己开句柄——那会让同一次重写产生两条上传，谁后落地谁赢。走
  `FS.TruncatePath`：优先作用在已打开的写句柄上，没有才回退。
- **上传完成回写节点必须是 compare-and-set**（`meta.AdoptByIno`，条件是本次上传发布时的
  `remote_id`）。`OnSuccess` 先读节点再写回，而 close(2) 可以插在中间提交更新的版本；
  无条件 `UpdateByIno` 会让已被取代的上传把旧的 size/id/version 写回去。条件不成立时
  只能用 `meta.SetRemoteVersion` 记远端版本，不能把读到的整行旧值写回。
- 一致性模式 `writeback`（默认，日志提交即返回）/ `strict`（远端上传完成才返回）/ `readonly`
  （`EROFS`）按挂载子树配置，判断都在 vfs。
- 没有引入 rclone 依赖（会拉入数百个包）；国外网盘按同一 `Provider` 接口自研，共享层已就位。

## 驱动现状与 `UNVERIFIED` 约定

已注册类型：`webdav`、`openlist`（同一驱动）、`aliyun`、`baidu`、`pan115`、`pan123`、
`quark`、`tianyi`、`sftp`、`s3`、`dropbox`、`onedrive`、`gdrive`、`box`、`smb`，
外加测试用的 `fake`。

部分 API 与平台细节尚未在真实账号/真机上验证，代码里用 `UNVERIFIED:` 注释标注
（当前 78 处，含 `internal/winfs` 的 10 处），每处都写清楚要验证什么。**改这些地方时保留或更新标注，验证通过才删除**。`Caps.Tier` 为
`unofficial` 的驱动（quark）默认限流更保守，风控信号映射为 `provider.ErrRiskControl` 以触发
熔断而不是重试进封号。各驱动的具体约束见 `docs/providers.md`。

## 测试策略

- `test/fakeprovider`：内存 Provider + 故障注入（延迟、429、风控、断连、链接过期），并对每个
  调用计数——这让"热目录零远端调用"成为可断言的事实。新的可靠性问题优先用它复现。
- `test/perf` 断言的是**provider 调用次数**而非墙钟时间：静默让远端流量翻倍的回归就是它要抓的。
- `test/conformance` 把 cloudfs 与普通本地目录逐项对比；故意不同的行为（无 POSIX 权限、
  无硬链接、close-to-open 可见性）是显式断言而不是留白。
- `test/chaos` 对应 `docs/DESIGN.md` 第 5 节的可靠性矩阵。

## 文档

- `docs/DESIGN.md` — 完整设计，代码里的包注释大量引用它的小节号（§4.1 等）。改架构要同步。
- `docs/providers.md`、`docs/mcp.md` — 驱动接入状态、MCP 工具与客户端注册。
- `TODO.md` — 逐条核对代码后得出的**文档与实现的差距清单**（P0/P1/P2 + 验证缺口），每条都带
  证据位置和"验收"断言。动手前先查这里，避免重复分析已知缺口；完成一条要更新它的状态。
- `README.md` — 面向用户的配置示例与命令表。

代码与代码注释用英文，独立文档与 TODO 用中文，沿用这个约定。
