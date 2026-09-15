# CloudFS MCP 接口

CloudFS 通过 Model Context Protocol 把挂载的网盘暴露给 agent。MCP 服务直连 VFS 核心，不经过内核，所以**即使没有挂载也能用**——这在容器里或没有 FUSE 权限时很有用。

> **一期已落地**：会话与作用域、访问令牌与 HTTP 接入、交付箱（`begin_session`/`finish_session`/`list_sessions`）、持久审计，见下文"[会话与作用域](#会话与作用域)"。**仍在规划中**：会话回滚、内容索引与 `semantic_search`、Agent 记忆库，设计见 [Agent 工作底座路线图](agent-roadmap.md)（TODO.md T-37 ~ T-43）。工具表只列出已实现的工具。

## 注册

### Claude Code

```sh
cloudfs mcp install --client claude
```

把输出写入项目根的 `.mcp.json`，或直接用：

```sh
claude mcp add --transport stdio cloudfs -- cloudfs mcp --stdio --allow /work
```

### Codex

```sh
cloudfs mcp install --client codex
```

把输出追加到 `~/.codex/config.toml`。

### HTTP 传输

```sh
cloudfs mcp --http 127.0.0.1:8765
```

默认使用回环地址。绑定其它地址必须提供 bearer token，因为这个服务能读写用户的网盘。
空主机地址（如 `:8765`）实际绑定所有网卡，同样必须认证。HTTP 启用跨源请求保护，
包括旧版 GET 通知流的 Origin 校验；原 SDK 的本地 Host 防护仍保留。

bearer token 有两种：`CLOUDFS_MCP_TOKEN` 环境变量（旧方式，全权，保留兼容）和
`cloudfs mcp token create` 签发的**作用域令牌**（推荐，见"[访问令牌与 HTTP 接入](#访问令牌与-http-接入)"）。
回环监听在签发第一个令牌之前保持免认证（本机进程直接连），一旦存在有效令牌就只认令牌。

```sh
cloudfs mcp install --client claude --transport http   # 打印带 Authorization 头的 HTTP 注册片段
```

同一端点按协议版本选择传输：`2026-07-28` 使用无会话 POST；旧版继续使用
initialize/session/GET。旧版会话 5 分钟没有 POST 会过期，长期订阅客户端需发送
keepalive ping；新版长连接随其 HTTP 请求断开而释放，不需要后续文件变更触发清理。

## 安全边界

| 机制 | 说明 |
|---|---|
| `--allow <path>` | 允许列表。路径先规范化再校验，`..` 穿越无效。列表为空表示整个挂载可见。它是 stdio、环境变量 token 与回环免认证调用的默认作用域；签发的访问令牌带自己的作用域（见下） |
| `--read-only` | 拒绝所有修改类工具 |
| 会话作用域 | 每个连接都有一个会话，读写分开检查；`sandbox` 会话只能写自己的目录。见"会话与作用域" |
| 审计 | 每次工具调用（含拒绝）落到 `agent.db`，参数脱敏；`cloudfs audit` 与控制台「Agent」屏可查 |
| `delete` 需要 `confirm=true` | 删除同时作用于远端且不可撤销 |
| 响应上限 | 每个工具都有 `limit` / `max_bytes`，超出返回 `truncated: true` 与续读游标 |
| 不暴露凭据 | token、cookie 永不出现在任何工具的返回里 |

## 工具

### 读

| 工具 | 参数 | 说明 |
|---|---|---|
| `list_directory` | `path`, `cursor?`, `limit?` | 分页列目录。返回每项的 `cached`（本地缓存比例）与 `state`（`synced` / `local`） |
| `stat` | `path` | 单个路径的元信息 |
| `stat_many` | `paths[]`（≤100） | 批量。单个路径出错不会让整次调用失败，错误写在该项的 `error` 字段 |
| `read_text` | `path`, `offset?`, `max_bytes?`, `head?`, `tail?` | 文本读。非 UTF-8 会被拒绝并提示改用 `read_range`。截断时返回 `next_offset` |
| `read_range` | `path`, `offset`, `length` | 任意字节范围，base64 返回。用于二进制或大文件分页 |
| `search` | `query`, `path?`, `content?`, `max_results?` | 本地元数据名称／路径片段搜索，权限与子树过滤在限量前执行。`content` 只检查完整缓存文件的有界前缀，限制见下文 |
| `cache_status` | `path` | 该路径的缓存比例、整体命中率、待上传数量 |
| `list_roots` | 无 | 当前可见的挂载点与是否可写 |
| `get_download_url` | `path` | 直链 + 过期时间 + 必需 headers。大文件让 agent 自己拉，不经过本服务 |
| `list_copy_jobs` | `cursor?`, `limit?` | 列出源和目标均获授权的复制准备任务；可能返回空页和续页游标，见下文 |
| `get_copy_job` | `id` | 查询准备状态、长度、检查点及上传交接 ID，不返回内部恢复数据 |
| `list_uploads` | `cursor?`, `limit?` | 分页列出仍活动且路径可见的上传；受限服务不显示无法绑定到当前路径的旧记录 |
| `get_upload` | `id` | 查询状态、路径、大小和重试时间，不返回 blob、哈希、会话、远端版本或原始错误 |

`list_directory` 新返回的游标按最后一个名称续读，删除已读项不会让后续条目因位置
前移而被跳过；不要自行解析或生成游标。旧版的规范十进制偏移仍兼容，但大偏移需要
扫描跳过的索引项。单页不超过配置的 MaxEntries，且硬上限为 1000 项。`total` 保留
为当前目录的独立索引计数，并非与 entries 同一时刻的快照；并发修改时可能不一致，
也不能据此决定是否续页，应使用 truncated/next_cursor。每页计数仍有 O(目录项数)
的索引扫描成本，但不把全部节点加载到内存。

### 写

| 工具 | 参数 | 说明 |
|---|---|---|
| `write_file` | `path`, `content`, `mode?` | `create` / `overwrite` / `append`。返回时数据已在本地持久化；`state` 为 `local` 表示上传仍在队列中 |
| `edit_file` | `path`, `edits[]`, `dry_run?` | 精确字符串替换。每个 `old_text` 必须唯一出现一次，否则拒绝而不是猜 |
| `create_directory` | `path` | 自动创建缺失的父目录，幂等 |
| `move` | `from`, `to` | 同一网盘内重命名或移动 |
| `copy` | `from`, `to` | 复制已提交的单文件内容，可跨网盘；不递归、不提供覆盖模式，writeback 成功表示本地日志提交。恢复及并发限制见 [复制说明](copy.md) |
| `delete` | `path`, `confirm`, `recursive?` | 需要显式确认 |
| `pin` | `path` | 完整下载并钉住。之后该子树的读取与内容搜索都是本地操作 |
| `unpin` | `path` | 移除精确匹配的固定规则，不删除缓存或远端内容；重叠规则仍生效 |
| `retry_copy_job` | `id` | 验证后排队重试失败/取消的准备任务；返回不代表下载或上传完成 |
| `cancel_copy_job` | `id` | 停止准备任务并保留内容；不能取消已交接的上传 |
| `forget_copy_job` | `id`, `confirm` | 必须 confirm=true；仅清理无引用的终止准备内容与记录，不删除本地/远端文件 |
| `retry_upload` | `id` | 仅重试仍绑定到当前授权路径的 dead 上传，不越过退避或活动状态 |
| `cancel_upload` | `id` | 持久停止上传并保留本地内容；不撤销或证明远端结果 |
| `resume_upload` | `id`, `confirm` | 必须 confirm=true；校验当前本地版本并按当前数据库/挂载/授权代次重新绑定后开始全新尝试，接受远端重放风险 |
| `discard_upload` | `id`, `confirm` | 仅无 `--allow` 限制的服务；永久清理已停止的当前本地版本，不撤销远端结果 |
| `flush_uploads` | 无 | 仅无 `--allow` 限制的服务；等待整个当前队列快照，不跳过死信、取消或清理记录 |

### 会话

| 工具 | 参数 | 说明 |
|---|---|---|
| `begin_session` | `name?`, `sandbox?` | 在工作区下建一个本会话专属目录（`<workspace>/<client>-<日期>-<sid前8位>/`，永不复用），写入 `manifest.json` 骨架，返回 `session_id`、`workspace`、`uri`。`sandbox=true` 时本会话的写被收窄到该目录，读不变 |
| `finish_session` | `session_id?`, `summary?`, `share?` | 结束会话（默认当前会话，只能结束自己 principal 的会话）：从审计里取本会话成功写过的路径作为产物，写全 `manifest.json`，返回 `artifacts[]`。`share=true` 只对已同步（`state=synced`）的文件附 `download_url`，仍在本地的不等待、不请求直链 |
| `list_sessions` | `cursor?`, `limit?`, `state?` | 列本 principal 的会话，最新在前，含工作区与产物 |

三个工具只在服务持有 `Sessions`（`cloudfs mount`/`cloudfs mcp` 常规启动都持有）时注册；
在非 owner 的 stdio 进程里它们返回 `requires the storage owner; use the HTTP transport`（见"与挂载并存"）。
产物用现有写工具直接写进会话目录即可，没有单独的 `write_artifact`。

### 复制任务管理

这些工具调用 VFS 管理接口，不绕过日志所有权、上传交接、引用检查与持久化门禁。
`--read-only` 拒绝三种修改操作；查询仍可用。任务的源和目标必须都在 `--allow` 内，
并且元数据库身份、源/目标挂载前缀、remote 和根对象仍与记录相同；被新挂载遮蔽或
来自旧数据库/账号的任务不可见，需要完整权限的 CLI/控制面检查。

列表/单条输出仅有 `id/source/target/state/size/checkpoint/upload_id`，不返回源对象
ID、版本、哈希、元数据库身份、payload 路径或持久化原始错误。越界和不存在的 UUID
均返回同一通用错误。过滤依据是持久化任务的虚拟路径，不通过 provider 查询。

每次列表最多处理 200 条原始任务（另有一条用于判断续页），且受 `MaxEntries` 和
`MaxBytes` 的结构化响应上限约束；隐藏任务较多时会出现空页但 `truncated=true`，
需要继续使用 `next_cursor`。游标经过认证加密，不直接暴露隐藏任务 ID，不可修改或
跨 MCP 服务实例使用，重启后需从头列举。分页不是快照，新建且 ID 排在游标前的任务
要下一轮查看；空页/续页数量不构成任务总数统计。单条超过字节预算时明确报错。

`submitted` 只表示交给上传器，**不代表远端成功**；`upload_id` 用于后续上传检查，
不能把找不到上传行当作成功。取消保留检查点和本地版本，forget 必须显式确认且仅
清理无引用内容。修改工具返回 `accepted=true` 表示该动作已执行/排队，不承诺最终
状态；发生错误或连接丢失后应先查询，不自动重放。完整限制见 [复制说明](copy.md)。

恢复未完成下载时，VFS 会重新核对原源路径的挂载、对象 ID、版本和长度，不跟随已经
改名到其他路径的稳定远端 ID。该路径检查遵循 VFS 目录缓存一致性，不是跨客户端
原子权限快照；已经准备完整的内容不再读取源，不要求源路径继续存在。

### 上传任务管理

上传工具与 CLI/control 共用 Journal、Uploader 和 VFS 协调器。列表每次最多扫描
200 条原始记录，并同时受 `MaxEntries` 和 `MaxBytes` 限制；允许列表过滤后可能返回
空页和 `next_cursor`。游标认证加密，不直接暴露隐藏 ID，服务重启后需从头列举。
原始 backend/SQLite 错误统一改为不含私有状态的提示；结构化结果也不含本机路径、
内容哈希、上传 session、预期远端版本或持久化原始错误。

上传行从 journal v11 起保存原元数据库、挂载前缀、根对象和本地账号绑定。worker 在
provider 解析及网络调用前核对；显式重新授权、账号定位配置、挂载或数据库变化会使任务
进入死信，受限 MCP 也不再显示其路径。自动 token 刷新和 `config auth --migrate` 保持
绑定。旧 schema 的无绑定任务不会自动发送；只有显式确认 resume 才按当前目标重新绑定。

受限 MCP 的查询、retry、cancel 和 resume 要求 journal inode 仍严格对应当前本地
版本、文件名、长度、remote、挂载及父目录对象，并在 VFS publication gate 内再次
核对调用时授权路径；替换版本、旧数据库/挂载或移出允许列表后不能只凭 ID 管理。
无法还原路径的 tombstone/purging 记录只对无允许列表限制的服务可见。

`discard_upload` 还要求无允许列表限制：清理可能在删除元数据后失败并留下 purging，
此时已没有路径可供下一次受限调用重新授权。`flush_uploads` 同样只允许无路径限制的
服务，因为它会处理整个队列，包括不可见任务。两者仍受 `--read-only` 拒绝。
修改返回只表示本地动作已经接受；连接丢失或不确定错误后必须先查询，不能自动重放。
上传清理的确认、句柄及持久化边界见 [上传清理说明](upload-cleanup.md)。

## Resources（读取与订阅已接入）

支持 `resources/list`、`resources/templates/list` 和 `resources/read`。列举返回挂载与
`--allow` 相交的可见入口，不扫描远端目录，不暴露被排除的挂载。目录资源返回带子项 URI
的 JSON，文件资源返回文本或 base64 blob，均通过原 VFS 缓存读路径。

URI 示例：`cloudfs://ali/work/readme.md`。`ali` 为 remote 名称，后面是**完整虚拟路径**，
不是 provider 内部路径或本机路径。同一 remote 挂在 `/work` 与 `/archive` 时，URI
分别保留这两个前缀；嵌套挂载必须使用实际归属 remote，不能通过外层 remote 访问它。
remote 名称含空格、中文或 URI 特殊字符时，authority 使用 `r~` 加 UTF-8 字节的小写
十六进制编码；应直接使用列举返回的 URI，不手工拼接。文件名的空格、`#`、`?`、`%`
按 URL 路径转义，仅解码一次。

| 读取方式 | 响应与限制 |
|---|---|
| 文件 URI，无参数 | 默认最多 256 KiB；完整 UTF-8 且不含 NUL 的页返回 text，其余返回 blob |
| `?offset=0&length=1048576` | 显式字节范围返回 blob；offset 非负，length 正数且最多 4 MiB |
| 空文件或 EOF | 返回空 blob，不当作资源不存在 |
| 目录 URI | JSON 的 entries 含 name、uri、kind、size；同时受默认 200 项与 256 KiB JSON 上限约束 |
| 目录 `?cursor=...` | 使用返回的 next_uri 续页；游标按文件名排序，不是数据库/远端对象 ID |

文件页的内容 `_meta` 中包含 `cloudfs/size`、`cloudfs/offset`、`cloudfs/bytes`、
`cloudfs/truncated`，尚有剩余内容时提供 `cloudfs/next_uri`。目录 JSON 中使用
`truncated` 与 `next_uri`。这些上限沿用 `Limits.MaxBytes/MaxRangeBytes/MaxEntries`；
文件上限按原始字节计，base64/JSON 编码存在额外开销。一个目录项连同游标本身超过
JSON 上限时返回错误并提示使用 `list_directory`，不生成无法前进的空页。

所有路径在访问 VFS 前校验允许列表与挂载归属；拒绝 `..`、非规范路径、无效编码、
用户信息/端口、重复或未知参数。资源不存在与越权都不返回内容，底层错误不泄露签名
链接、凭据或本地缓存路径。只读 MCP 允许资源读取，仍拒绝写工具；HTTP 沿用原认证。

资源响应、根列表和模板均设置 `cacheScope: private`、`ttlMs: 0`，客户端每次读取需
重新请求；服务端仍使用 VFS 的 TTL/缓存，不表示每次联网。此设置在 SDK 默认处理后
应用，避免其默认值覆盖 private。协议依据见 [MCP Resources](https://modelcontextprotocol.io/specification/2026-07-28/server/resources)。

### 资源订阅

已声明 `resources.subscribe` 能力，支持旧版 `resources/subscribe` / `resources/unsubscribe`
及新版 `subscriptions/listen`。新版先发 acknowledged，再发送带对应 subscriptionId 的
`notifications/resources/updated`；确认前发生的变更保留到确认后。每个服务共用一条
[VFS 事件流](vfs-changes.md)，覆盖本地/内核写入、远端 delta/TTL 刷新、复制发布、删除和改名，
不通过额外下载或轮询实现通知。

- URI 在注册前校验允许列表、挂载归属和参数；可以订阅尚不存在的授权路径以等待创建。
  只读模式允许订阅。批次内任一 URI 无效或重复时整体拒绝，不先注册合法的部分。
- 同一路径不同页的 URI 独立订阅，内容改变会通知所订阅的原 URI；目录改名通知新旧
  路径的后代及相关父目录。只发送已授权的订阅 URI，不广播原始内部事件。
- 通知按 25ms 窗口合并，不承诺每版本、持久化或恰好一次交付。客户端应重新读取；
  重启/重连需重新订阅并读取。取消前已发出的通知可能仍在网络中。
- 每 SDK 会话最多 128 个资源 URI，服务合计最多 4096 个；跟踪资源的 SDK 会话最多
  64 个，所有新版 listen 流（含仅订阅目录变化的流）合计最多 4096 个。新版 HTTP
  每条 POST 流对应独立 SDK 会话，不能把这些限制理解为按登录用户计费/隔离的配额。
- 旧版重复订阅幂等；同一 SDK 会话/URI 的重叠新版流明确拒绝，须先取消原流。
  断连释放资源，服务关闭会取消资源及目录变化流，避免退出等待永不结束。

go-sdk v1.7.0 的 HTTP 客户端取消订阅时还会发送旧式 `notifications/cancelled`。
服务端对经过认证/跨源检查且格式有效、最多 64 KiB 的这类冗余通知只返回 202，
不查找其 requestId、不取消其他请求，真正取消仍由原流断开触发。
该 SDK 客户端遇到其他 HTTP 4xx 可能关闭整条连接；需要重新连接，不应把随后出现的
“连接关闭”当作另一个路径通过了权限检查。当前不修改协议错误状态来掩盖此客户端限制。
协议依据见 [Streamable HTTP](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)。

### 仍需验证和优化

文件分页不是跨请求版本快照，目录游标也不是一致性快照；分页期间
内容改变可能跨版本，新建且排序在游标之前的项需下一轮扫描。目录资源按最多 128 项
的小窗口读取，list_directory 按页读取；SQLite 使用已有 parent/name 索引定位，不再
把整个已缓存目录加载、排序后切片。资源 JSON 按准确编码长度累计预算，避免每加一项
都重新序列化整个响应。已有目录分页、删除后续页、万项索引计划和缓存调用数回归。
冷目录或 TTL 过期时，VFS 已将 provider 页按 200 项批次暂存在独立 SQLite TEMP 表，
最后在一个事务里合并目录和刷新时间；不会跨页累积完整 Go 列表。中途失败不发布
半份目录。WebDAV/OpenList 已经逐条解析 HTTP PROPFIND 并接入同一暂存器，
取消/截断/超大单项不发布半份目录；SFTP 已用专用 SSH 连接逐条交付并接入同一
暂存器。兼容全量 List 和其他非流式驱动仍可能返回完整切片；原子合并的写锁延迟、
TEMP 空间预算、工具 total 的计数成本及真实规模性能仍需完善；详见
[目录冷刷新及边界](directory-refresh.md)。这不是整个进程或真实内核路径的内存上限证明。
**慢客户端不再拖住变更检测（2026-09-06）**。SDK 的 `ResourceUpdated` 会逐个通知订阅者，
自带 10 秒超时，所以一个传输卡住的客户端会占住这次调用。原来这个调用在合并循环里内联执行，
于是那段时间**整个服务器都不再感知任何变更**——一个读不动的客户端会冻结其他所有客户端看到的
视图。现在发送交给独立 goroutine，中间是有界队列（256）：队列满时不丢通知，对应的 watch
仍是 dirty，下一个 tick 会再次投递；同一个 URI 不会在队列里堆重复项。回归覆盖「队列填满后
仍能接收并记录新变更」和「重复 tick 不堆积同一 URI」，把入队改回内联即失败。

**慢客户端也不再拖住其他订阅者（2026-09-07）**。上一轮只把变更检测摘了出来：投递仍是一个
goroutine 一条队列，而 SDK 的广播按会话依次发送，于是卡住的客户端虽然不再让服务器失明，却
还是让**别人的通知排在自己后面**。现在**每个会话有自己的队列和自己的发送 goroutine**。
SDK 仍然没有按会话投递的入口，所以定向是这样做到的：发送者在调用广播前，先在自己那条 watch
上打一个 `claimed` 标记；发送中间件只放行被 claim 的那条，其余会话在碰到传输之前就被丢弃，
由它们各自的发送者投递。因此一次广播只会写进一个传输，卡住的只有它自己那条队列。
回归 `TestOneStalledSubscriberDoesNotDelayAnother`：一个会话的传输永久阻塞时，另一个会话
仍在 5 秒内收到自己的通知；把所有会话改回共用一个发送者立刻失败。

**权限的边界（不是缺陷，是模型）**：URI 的允许列表与挂载归属只在**订阅注册时**校验一次
（`reserve` → `parseResource`）。投递路径只确认"这个 URI 是这个会话注册过的"，不再重跑
`checkPath`，所以订阅期间修改 `--allow` 或挂载配置不会对已有订阅重新生效——要收回权限得让
客户端重连或重启守护进程。而且 `Options.Allow` 是服务器全局的，没有按会话或按身份的权限
模型，"这个会话能看到什么"就等于"这个会话注册过什么"。要隔离不同信任级别的 agent，靠的是
一个进程一份 allowlist，不是靠订阅。

**仍未解决（上游）**：SDK 的 `Server.disconnect` 只从每个 URI 的内层 map 里删掉该会话，
**不会删除随之变空的外层 URI 条目**（`go-sdk v1.7.0` `mcp/server.go`），长期运行下
`resourceSubscriptions` 会按历史订阅过的不同 URI 数量增长。cloudfs 侧的簿记已有回归证明
会话反复连断后 `sessions`/`streams`/`count`/`senders` 全部归零，但外层条目在 SDK 内部，
这里不假装修好了它。真实 FUSE/网盘事件仍需验收。

## 会话与作用域

一期 Agent 底座把"谁在调用、能碰什么、做了什么"落成三张表，存在 `<cache.dir>/agent/agent.db`
（独立 SQLite，不动 meta/journal；任何进程都能追加审计行，保留期清理只在 owner 进程跑）。
设计背景见 [Agent 工作底座路线图](agent-roadmap.md)。

### 作用域模型

`Scope{read, write, read_only, expires_at, sandbox}`：`read` 是可见前缀（空 = 整个挂载），`write`
省略时同 `read`、给空列表表示不可写，`sandbox` 非空时写再收窄到该目录。每个工具调用都按
"读还是写"分别检查：`delete`/`move`/`copy` 的两端都按写检查，`resources/subscribe` 按读检查。
`--allow`/`--read-only` 是本进程的**默认作用域**，stdio、`CLOUDFS_MCP_TOKEN` 与回环免认证的调用都用它；
签发的访问令牌是用户显式授予的一份独立作用域（`--write` 必须在 `--read` 内），**不**与 `mcp.allow`
求交，所以只签发你真的想给出去的前缀。会话只能在自己 principal 的作用域内收窄（`sandbox`），
不能放大。越界调用返回 denied 并进审计。

每个 MCP 连接对应一个**会话**（principal、传输、作用域、开始/最后活动时间、写次数、产物）：

| 传输 | 会话身份 |
|---|---|
| stdio | 一进程一会话，principal 是本进程的默认作用域 |
| HTTP 作用域令牌 | 同一令牌的所有请求是一条连接（无状态 HTTP 下每个 POST 都是新的 SDK session，不能当身份）；空闲超过 `mcp.session.idle`（默认 30 分钟）自动结束并轮转新会话 |
| HTTP 旧版有状态协议 | 按 SDK session id |
| HTTP 回环免认证 | 一个本机默认 principal，同样按空闲轮转 |

### 访问令牌与 HTTP 接入

```sh
cloudfs mcp token create --name codex --read /work --write /work/.agent --ttl 720h
cloudfs mcp token list                  # 只显示名称、4 位指纹、作用域、过期与最后使用
cloudfs mcp token revoke codex --confirm
cloudfs mcp install --client claude --transport http [--url http://127.0.0.1:8765/] [--token <token>]
```

令牌明文只在创建时打印一次，库里只存 `sha256`；`list` 与控制面 `GET /mcp/tokens` 只给指纹。
过期或吊销的令牌 `initialize` 得 401，吊销同时关闭该 principal 仍活着的旧版有状态会话
（其它进程吊销的在 2 秒内生效）。`--write` 的每个前缀必须落在某个 `--read` 前缀内。

`mcp install --transport http` 输出带 `Authorization: Bearer <token>` 的注册片段，并在
stderr 给出可直接执行的 `claude mcp add --transport http cloudfs <url> --header "Authorization: Bearer <token>"`；
不传 `--token` 时片段里保留字面 `<token>` 占位，命令本身永远不会替你造一个令牌。
控制台「Agent」屏的"访问令牌"标签能做同样的事（新建时明文只显示一次，关闭浮层即丢弃）。

### 交付箱

`mcp.workspace` 是会话目录的根（省略时取第一个 `--allow` 前缀 + `/.agent`；`allow` 为空且未配置
时 `begin_session` 报配置错误，不建目录）。会话目录里唯一受管的文件是 `manifest.json`：

```json
{
  "session_id": "…", "client": "codex", "principal": "token:codex",
  "started_at": "…", "finished_at": "…",
  "scope": { "read": ["/work"], "write": ["/work/.agent"], "sandbox": "/work/.agent/codex-20260915-0a1b2c3d" },
  "summary": "…",
  "artifacts": [ { "path": "…", "uri": "cloudfs://…", "size": 123, "sha256": "…", "state": "synced" } ]
}
```

产物清单来自审计表里本会话成功的写路径，所以终端里 `ls` 会话目录与 `manifest.json` 的
`artifacts` 一致（`test/e2e` `TestAgentSessionManifestMatchesTheMount` 在真实挂载上验证）。
`share=true` 给出的直链会过期（`expires_at`），只对已同步的文件有；控制台会话浮层的"复制链接"
也是点击时才向 daemon 要一次链接，不写进页面。

### 审计

每次 `tools/call`（加 `initialize` 与订阅注册）同步写一行：时间、principal、会话、传输、工具、
涉及路径、脱敏参数（`content`/`new_text` 只记 `{bytes:n}`，整体 ≤ 4 KiB，不含直链、token、cookie）、
进出字节、结果（`ok`/`denied`/`error`）与耗时。写失败不影响工具本身，只计
`cloudfs_audit_write_failures_total`。保留期 `mcp.audit.retain`（默认 90 天）。

```sh
cloudfs audit --result denied --since 24h        # daemon 未运行时直接只读 agent.db
cloudfs sessions list | show <id> | finish <id> --summary "…"
```

控制面：`GET /audit`、`GET /sessions`、`GET /sessions/{id}`、`POST /sessions/{id}/finish`，SSE 事件
`audit`/`session`；控制台「Agent」屏的"会话"与"审计"标签就是它们的视图（denied 行有文字标签，
不只靠颜色）。

## Agent 使用建议

**搜索只覆盖已知目录树。** `query` 不含 `/` 时匹配名称；含 `/` 时按完整虚拟路径的字面子串匹配（例如 `work/src/`），不会把 `%`、`_` 当通配符。匹配忽略大小写，权限路径仍区分大小写。结果按路径深度、路径排序。目录改名后无需重写子树索引，查询在一个数据库快照内重建路径。1～2 个字符使用独立短字符串索引，从不查询待索引记录；较长查询同时检查尚未完成后台索引的记录，
积压超过阈值时先把它合并进索引，而不是每次查询都扫一遍。宽泛的路径查询有工作预算（随
`max_results` 缩放），达到预算即停止收集并在响应里置 `truncated`——"没有更多匹配"和"我们
停止查找了"是两句不同的话。因此一次查询的耗时不随子树大小无限增长，但也不保证返回的是
全局最浅的那一页。

**先 `pin` 再做内容搜索，但它不是完整 grep。** `content` 只检查完整缓存文件的前 `MaxBytes` 字节，并有最多 `4 × max_results` 个已授权名称候选的预算。预算耗尽时返回 `truncated` 和说明；未缓存文件会跳过并提示先 `pin`。文件后半部分、候选预算外或尚未列举的文件可能不被检查。需要完整内容检索时应逐文件分页读取；当前搜索没有跨请求内容快照。

**用 `stat_many` 而不是循环 `stat`。** 一次调用检查最多 100 个路径，缺失的路径在结果里单独标注，不会中断整批。

**大文件用 `read_range` 分页。** `read_text` 默认上限 256 KiB，`read_range` 上限 4 MiB。真正的大文件用 `get_download_url` 拿直链自己下载。

**`write_file` 返回后数据就安全了。** `state: "local"` 不代表有风险，它只是说上传还在队列里；数据已经 fsync 到本地日志，进程崩溃也不会丢。要确认上传落地就看 `cache_status` 的 `pending_uploads`。

## 与挂载并存

MCP 与 FUSE 挂载共用同一个 VFS 实例，所以两边看到的是同一份文件系统：agent 写的文件，终端里 `cat` 立刻能读到；终端里改的文件，agent 下次 `read_text` 就看到新内容。端到端测试 `test/e2e` 专门验证这一点。

这个"同一实例"只对 storage owner 进程内的 MCP（例如 `cloudfs mount` 同进程启用的 HTTP 传输）成立。挂载已在运行时再单独启动的 `cloudfs mcp --stdio` 不是 journal owner，拿到的是另一份 VFS 实例：写入进共享 journal 但不运行上传器，看不到内核写的变更事件。这种进程现在会：

- 启动时在 stderr 打印 `cloudfs: another process owns this cache (is "cloudfs mount" running?) … use the HTTP transport: cloudfs mcp install --transport http`（stdout 是传输，不能写）；
- 读写工具照常工作，调用照常写进共享的 `agent.db` 审计（agent 库在 owner 判断之前打开）；但 owner 进程的 SSE 只推送自己写的行，控制台要刷新一次才看到这些审计；
- `begin_session`/`finish_session`/`list_sessions` 拒绝并返回 `requires the storage owner; use the HTTP transport`。

其与挂载并存时的一致性（内核写与 stdio 进程视图）仍登记为 TODO.md T-43；与挂载并存时请改用 HTTP 传输，控制台「Agent」屏的接入面板在这种情况下也会提示。
