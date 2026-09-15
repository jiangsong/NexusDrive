# CloudFS Agent 工作底座路线图

2026-09-14 登记 · 状态：**一期、二期已实现（2026-09-15，分支 `feat/agent-phase1`）；三期首项为 stdio→HTTP 桥（T-43）** · 对应 [TODO.md](../TODO.md) P4（T-34 ~ T-43）与
[界面计划](ui-plan.md) 阶段 F。

本文是设计文档：说明每项能力"做成什么样、为什么这样做、界面长什么样"。逐条验收断言以
TODO.md 对应条目为准，这里只引用条目号，不重复抄写。代码锚点在 2026-09-14 按仓库核对过；
行号会随代码漂移，引用时以符号名为准。代码标识符用英文，说明用中文。

---

## 0. 一页摘要

CloudFS 今天是"agent 能挂上的网盘"：MCP 暴露 33 个工具与资源订阅，`--allow`/`--read-only`
做全局权限，写入经日志不丢。但 agent 真正干活时缺的东西一样都没有——没有会话、产物没有
归宿、没人记录"谁动了什么"、写错了撤不回、PDF/docx 读不懂、没有语义检索、记忆换设备就丢、
文件到了也不会叫醒任何人。本路线图把 CloudFS 升级为**个人桌面 Agent（Claude Code / Codex /
OpenClaw）的工作底座与记忆扩展**：单用户单 daemon，权限按会话级 scope 设计，给团队场景留扩展点。

| 维度 | 今天已有 | 要补的 | 条目 | 期 |
|---|---|---|---|---|
| 一、会话与交付箱 | 写工具、`get_download_url`、资源 URI | 会话工作区、`manifest.json`、sandbox 写收窄 | T-36（依赖 T-34） | 一期 |
| 二、内容索引与记忆 | 文件名 FTS5 trigram；`search.content` 只扫缓存前缀 | 文档抽取、分块、FTS 检索、嵌入与 hybrid、记忆库 | T-37 / T-39 / T-40 | 一期 / 二期 / 二期 |
| 三、作用域、令牌、回滚、审计 | `--allow` 全局、`--read-only`、`delete confirm`、单一 env token | 会话 scope（读写分离、过期）、令牌 principal、持久审计、文件级回滚 | T-34 / T-35 / T-38 / T-43 | 一期 / 一期 / 二期 / 二期前 |
| 四、触发器与发送给 Agent | `FS.WatchChanges()`、SSE `/events`、MCP subscribe | 变更种类与来源、exec/webhook 触发、控制台一键交给本机 agent | T-41 / T-42 | 二期 |

**九个关键决策**：

1. **会话、审计、前像、触发器都住在 storage owner 进程**。stdio 进程与 `cloudfs mount` 并存时拿到的是独立 VFS（§4.5），所以推荐拓扑是"一个 owner + HTTP 传输"。
2. **CloudFS 自己定义 principal 与 session**，不把 SDK 的 `*mcp.ServerSession` 当身份：2026-07-28 传输每个 POST 一个 SDK 会话。
3. **两个新数据库**：`agent.db` 存真相数据（审计、令牌哈希、操作与前像），`index.db` 存可整库重建的派生数据；两者都不进 meta/journal（§1.3）。
4. **默认什么都不索引**。`index.pinned` 只处理已完整缓存的文件，零额外下载；主动下载必须显式写 `index.rules`，并受限流、预算与风控熔断约束。
5. **嵌入默认关闭**。开启后端点故障时静默降级为关键词检索；把内容发往非本机端点必须显式 `allow_remote: true`，并在界面常驻提示。
6. **记忆库是约定目录下的纯文件**，不是 KV 表：跨设备同步交给网盘，冲突沿用既有冲突副本机制。
7. **回滚是一组经 VFS 发起的新写入**，不是远端历史版本恢复。只覆盖 MCP 发起的修改；遇到会话之后又被改过的文件报冲突，不覆盖。
8. **触发器至少投递一次、不经 shell**，命令只能在配置文件中定义。浏览器和 MCP 都不能定义要执行的命令。
9. **每一项都是"后端 + 控制台界面"同条目、同期交付、同一验收**。没有界面的条目不算完成。

**分期**：一期两条线并行——线 A 底座（T-34 → T-35 → T-36），线 B 索引 phase 1（T-37）；二期
T-43 → T-38、T-39 → T-40、T-41 → T-42；三期是 stdio→HTTP 桥、递归删除逐文件前像、HNSW 等（§7）。

---

## 1. 总体架构增量

### 1.1 分层位置

新增的四个包都是 vfs 的**消费者**，与 `fusefs`/`mcpsrv` 同级，不进 vfs，也不让 vfs 反向依赖它们。
唯一对 vfs 的改动是二期给 `vfs.Change` 加两个字段（§5.1）。

```
                    ┌──────────────── storage owner 进程 ────────────────┐
终端 ── FUSE ──────→ fusefs ──────────────────────────────┐
Claude/Codex ─HTTP→ mcpsrv ─ middleware(会话→Scope→审计→前像) ─┤
浏览器 ── 回环 ───→ control ─ /sessions /audit /index /triggers ┼─→ vfs ─→ meta / cache / journal / upload ─→ provider
                     │                                    │        ▲ WatchChanges
                     ├─ agent   ── agent.db, preimages/   │        │
                     ├─ index   ── index.db ← textract    ┘────────┤ ReadFileRange / Busy
                     │              └─ embed ─→ httpx ─→ 嵌入端点   │
                     └─ trigger ── trigger_deliveries ─────────────┘ → exec / webhook
                    └────────────────────────────────────────────────────┘
```

`internal/daemon` 仍是唯一知道全部装配关系的地方：打开 agent.db 与 index.db、装配 Indexer 与
trigger 引擎、非 owner 时禁用会话类工具；关闭顺序为 trigger → index → mcpsrv → agent → vfs。

### 1.2 新包与改动面

| 包 | 职责 | 读写的数据 | 依赖 | 条目 |
|---|---|---|---|---|
| `internal/agent`（新） | `Scope`/`Principal`/`Session`、审计写入、前像捕获、回滚、工作区与 manifest、启动恢复 | `<cache.dir>/agent/agent.db`、`<cache.dir>/agent/preimages/` | vfs（经 mcpsrv 调用）、cache（`HydratedPath`/`ReserveDisk`） | T-34 T-35 T-36 T-38 |
| `internal/textract`（新） | 纯函数：格式识别、文本抽取、分块 | 无状态 | 标准库 + `github.com/ledongthuc/pdf` | T-37 |
| `internal/index`（新） | `Store`（index.db）、`Indexer`（对账、增量、让路）、`Search`（bm25/RRF） | `<cache.dir>/index.db` | vfs、meta（新增只读 `WalkSubtree`）、textract、embed | T-37 T-39 T-40 |
| `internal/embed`（新） | `Embedder`：`openai`/`ollama`/`fake` | 无 | `httpx`、`net/ratelimit` | T-39 |
| `internal/trigger`（新） | 规则匹配、去抖入队、串行 worker、exec 与 webhook 执行器 | agent.db 的 `trigger_deliveries` 表 | vfs（`WatchChanges`）、`net/proxy` | T-41 T-42 |

既有包的改动都在"薄适配"范围内：

- `internal/mcpsrv`：`checkPath(ctx, p, write)` 替换 36 个调用点（33 个工具）；receiving middleware 注入会话与审计；新增 `session_tools.go`、`index_tools.go`、`memory_tools.go`；`requireBearer` 查令牌表；`ClientConfig` 多一种 HTTP 输出。
- `internal/vfs`：仅 T-41 给 `Change` 加 `Kind`/`Origin` 并在 9 个 emit 点填写，`Affects` 不变。
- `internal/meta`：新增只读分页遍历 `WalkSubtree(ctx, ino, visit)`，导出 `Store.Identity()`。
- `internal/config`：`MCP.Workspace`、`MCP.Session`、`MCP.Audit`、`Index`、`Memory`、`Triggers`、`Agents` 及 `Validate`；`IsSecretField` 加入 `api_key`。
- `internal/control`：`audit.go`、`sessions.go`、`mcp_tokens.go`、`index.go`、`memory.go`、`triggers.go`、`agent.go`；路由全部登记进 `metrics.go` 路由表；`events.go` 增加 SSE 事件。
- `internal/control/web`：新屏 `screens/agents.js`、`screens/index.js`、`screens/triggers.js`；浮层 `session_panel.js`、`send_to_agent.js`；纯逻辑模块五个（§6.5）。
- `cmd/cloudfs`：`audit`、`sessions`、`index`、`triggers` 子命令，`mcp token`，`mcp install --transport http`。

### 1.3 为什么是两个独立数据库

**不进 meta.db**。meta 是单写者（`writeMu` + `_txlock=immediate`），FUSE 的 FLUSH 在等这把锁。
索引要写 MB 级 TEXT 并触发 FTS 段合并；审计要求每次工具调用一条同步 INSERT。放进 meta 就是
`internal/meta/indexer.go` 顶部注释明确要避免的事：让后台工作拖慢前台 close(2)。

**不进 journal.db**。journal 是正确性关键路径（schema v13，恢复、上传绑定、清理意图都在里面）。
审计写失败必须不影响上传；前像保留策略与上传 blob 生命周期完全不同（§4.8）。

**agent.db 与 index.db 也分开**，因为两者性质相反：

| | agent.db | index.db |
|---|---|---|
| 性质 | 真相数据：审计、令牌哈希、操作记录、前像索引、触发器投递 | 派生数据：抽取文本、分块、向量 |
| 丢失的后果 | 审计与回滚能力丢失，不可重建 | 整库删除后自动重建，只花时间与（rules 模式下）下载 |
| 重建条件 | 从不自动重建 | `meta_store_identity` 不一致、schema 不兼容、用户点"重建" |
| 写入形态 | 小行、高频、同步 | 大 TEXT、批量、后台 |
| 保留 | `mcp.audit.retain` 90 天、`mcp.session.retain` 7 天 | 跟随文件存在与规则范围 |

两者共同的约定：WAL、`busy_timeout`、`synchronous(NORMAL)`，schema 从 v1 起，迁移机制照抄
`internal/meta/store.go` 的写法，不触碰 meta v11 与 journal v13。

**多进程写入**：

- agent.db：owner 进程持有 `<cache.dir>/agent/owner.lock`（flock，仿 `internal/journal/lock.go`），只有持锁者运行 GC、`Purge`、回滚与触发器 worker。审计行由任何进程追加（SQLite WAL 跨进程安全），这样 stdio 非 owner 进程被拒绝的调用也有记录。
- index.db：只由 daemon owner 写，CLI 离线查询用 `?mode=ro` 打开。

### 1.4 配置增量一览

```yaml
mcp:
  allow: [/work]
  workspace: /work/.agent        # T-36；默认 = 第一个 allow 前缀 + /.agent
  session:
    max_preimage_bytes: 32MiB    # T-38
    retain: 168h                 # T-38：会话结束后前像保留 7 天
  audit:
    retain: 2160h                # T-34：审计保留 90 天
index:   { enabled: false, ... } # T-37 / T-39，见 §3.2、§3.5
memory:  { root: /work/.agent, ... }  # T-40，见 §3.11
triggers: [ ... ]                # T-41，见 §5.2
agents:   [ ... ]                # T-42，见 §5.7
```

所有秘密值（嵌入 `api_key`、webhook `secret`）只接受 `keyring:`/`secretfile:` 引用，YAML 明文
在 `Validate()` 拒绝。复用 `internal/config/secrets.go`。

### 1.5 与既有不变量的关系

- **vfs 仍是唯一核心**：会话、索引、触发器都不改变缓存、一致性与上传判断。
- **`Caps` 仍是唯一分支依据**：回滚不需要"按版本取内容"能力位，所以不新增 Caps 字段；索引对 `Tier=unofficial` 的减半预算读的是 Caps，不按网盘名特判。
- **凭据不出 daemon**：浏览器永远看不到远端凭据与嵌入密钥（§6.4）。
- **`test/perf` 调用次数基线不变**：审计、会话解析、`edit_file` 路径上的前像捕获都是 0 次额外远端调用；`pinned` 模式索引也是 0 次。

---

## 2. 维度一：会话与交付箱（T-36）

### 2.1 目标

agent 每次任务的产物落在一个可预期、可分享、互不覆盖的目录。人在终端和控制台都能一眼找到，
agent 不需要自己维护索引。会话本身（身份、scope、审计）由 T-34 提供，本节只讲"交付箱"。

### 2.2 目录约定

- 工作区根 `mcp.workspace`，默认 = 第一个 `allow` 前缀 + `/.agent`。`allow` 为空且未配置 `workspace` 时没有默认值，因为挂载根是布局合成目录，不可写。此时 `begin_session` 报配置错误，不创建任何目录。
- 每会话一个目录 `<workspace>/<client>-<YYYYMMDD>-<sid[:8]>/`。目录名即归档，永不复用。`<client>` 在 stdio 下取 `ClientInfo.name`，在 HTTP 下取令牌 principal 的名称，并规范化为 `[a-z0-9-]`。
- 会话目录内只有 `manifest.json` 一个受管文件。其余文件由 agent 用现有 `write_file`/`edit_file`/`create_directory` 写，**不加 `write_artifact` 工具**。
- **不做共享 `index.json`**：两个会话同时结束会互相覆盖。会话列表由 `list_sessions` 与控制台从 agent.db 读取。
- 工作区根下 `memory/` 与 `skills/` 是保留名，给记忆库使用（§3.11）。会话目录名带日期与会话 ID，不会与它们冲突。

```
/work/.agent/
  claude-code-20260914-3f9a01c2/
    manifest.json
    report.md
    data/summary.csv
  codex-20260914-b71e44d0/
    manifest.json
  memory/          # T-40
  skills/          # T-40，只约定位置
```

### 2.3 manifest.json

```json
{
  "session_id": "3f9a01c2-…",
  "client": "claude-code",
  "principal": "token:codex-laptop",
  "started_at": "2026-09-14T10:02:11Z",
  "finished_at": "2026-09-14T10:31:40Z",
  "scope": { "read": ["/work"], "write": ["/work/.agent/claude-code-20260914-3f9a01c2"], "read_only": false, "expires_at": null, "sandbox": true },
  "summary": "整理 Q3 报表并生成摘要",
  "artifacts": [
    { "path": "/work/.agent/claude-code-20260914-3f9a01c2/report.md", "uri": "cloudfs://ali/work/.agent/…/report.md",
      "size": 18233, "sha256": "…", "state": "synced", "download_url": null, "expires_at": null }
  ]
}
```

`artifacts` 由审计表里该会话的成功写路径生成，不信任 agent 自报。`state` 为 `synced`/`local`。

### 2.4 MCP 工具

| 工具 | 参数 | 行为 |
|---|---|---|
| `begin_session` | `name?`, `sandbox?=false`, `snapshot?=true` | 建会话目录（复用 `mkdirAll`）并写 manifest 骨架；显式会话成为该连接的当前会话。返回 `session_id`、`workspace` 与资源 URI |
| `finish_session` | `session_id?`, `summary?`, `share?=false` | 生成 artifacts；`share=true` 只对已同步文件调 `FS.DownloadURL`，`local` 文件记 `state=local` 且不等待；配置了 `webdav.root` 覆盖时附 DAV URL；会话置 `finished`。**不调用 `flush_uploads`**，受限服务本来就不许 |
| `list_sessions` | `cursor?`, `limit?`, `state?` | 分页，只返回本 principal 可见路径下的会话 |

`snapshot` 控制本会话写操作是否捕获前像（T-38 生效之前该参数被接受但无作用）。

### 2.5 sandbox

`sandbox=true` 时 `Scope.Sandbox` = 会话目录，写权限收窄为该目录，读权限不变。它把"agent 只能写
自己的目录"变成一个参数，而不是要求用户为每个任务新建一个受限令牌。越界写返回 denied，并在审计中
记 `result=denied`。

### 2.6 分享与直链

直链是有过期时间的签名 URL。把它写进网盘上的 manifest 等于持久化签名链接，所以 `share` 默认
false；写入时同时记 `expires_at`，文档说明"过期后用 `get_download_url` 重取"。工作区目录被
`--allow` 排除时，`begin_session` 必须失败，不能静默写到别处。

### 2.7 界面（ui-plan F3，以及 F1 的会话标签）

- **「Agent」屏 · 会话标签**（`screens/agents.js`，T-34 交付骨架）：
  - 顶部三张卡：活动会话、今日写操作、今日拒绝次数。
  - 表格列：客户端、作用域摘要、状态点加文字、开始时间、写操作数、产物数（T-36 增列）。作用域摘要由 `scope_view.js` 生成，例如"读 /work · 写 /work/.agent/… · 30 天后过期"。
  - 过滤条件："仅 sandbox"。分页用 `moreRow`；未翻页时由 SSE `session` 事件刷新。
- **会话详情浮层**（`session_panel.js`，`openPanel`）：
  - 一期内容：作用域、客户端、审计尾巴 50 条、"结束会话"按钮（`POST /sessions/{id}/finish`）。
  - T-36 增加**产物表**，列为路径、大小、状态、动作。状态分已同步与上传中，用点加文字表示，由 SSE `change` 事件刷新。
  - 产物动作有两个。"在文件中打开"跳到主窗口并选中该文件；"复制链接"只在点击时请求 `/fs/download-url`，结果不写入表格 DOM。
  - 另有摘要文本、sandbox 标记和"打开工作区目录"按钮。
- **主窗口**：工作区根与会话目录在文件表名称旁显示 `bot` 小标记，带 `aria-label`。检查器对会话目录内的文件显示"来自会话 <client>-<日期>"链接；点击后用 `GET /sessions?path=` 反查，并打开会话详情浮层。

验收见 TODO.md T-36；界面测试文件为 `ui_sessions_test.go`。

---

## 3. 维度二：内容索引、语义检索与记忆库（T-37、T-39、T-40）

### 3.1 决策

| 决策 | 结论 | 理由 |
|---|---|---|
| 数据放哪 | 独立 `<cache.dir>/index.db` | §1.3 |
| 向量存储 | SQLite BLOB + 内存暴力 cosine，默认 int8 量化 | 零依赖。200k chunk × 512 维 int8 约 100 MB 内存，单次检索 < 50 ms；float32 × 1536 维会到 1.2 GB，不接受 |
| 排序 | FTS5 `bm25()` 与 cosine 各取 top 50，RRF 融合（k=60） | 不需要校准两路分数尺度 |
| 范围 | 默认不索引；`pinned` 零下载；`rules` 才主动下载 | 下载计入 remote 限流桶，风控是真实风险 |
| 嵌入 | `openai`/`ollama`/`none`（默认）；故障静默降级 keyword，响应带 `degraded` | 个人桌面场景 ollama 本地即可 |
| 记忆 | 约定目录纯文件 + 薄工具 | §3.11 |
| ANN 索引 | 不做，除非实测 p95 > 200 ms | 三期候选 `github.com/coder/hnsw` |

### 3.2 范围策略

```yaml
index:
  enabled: true                 # 默认 false；false 时不建 index.db、不注册工具、/index/status 返回 enabled:false
  pinned: true                  # 索引 pin 规则覆盖且 cache.Complete 为真的文件（零额外下载）
  rules:                        # 主动下载的子树，默认空
    - path: /work/notes
      include: ["**/*.md", "**/*.txt", "**/*.pdf", "**/*.docx", "**/*.xlsx", "**/*.pptx"]
      exclude: ["**/.git/**", "**/node_modules/**"]
      max_file_size: 20MiB      # 超过跳过，计入 index_status.skipped
  exclude: ["**/.env", "**/*.pem", "**/id_rsa*", "**/.git/**", "**/node_modules/**"]  # 全局，先于 rules
  max_text_bytes: 2MiB          # 单文档规范化文本上限，超出截断并标 truncated
  max_total_text: 4GiB          # 文本总预算，达到后新文档停在 pending，doctor 告警
  max_chunks: 200000            # 向量上限，达到后嵌入停止，keyword 继续
  fetch_budget: 2GiB/h          # rules 触发的下载预算；Caps.Tier=unofficial 自动减半
  embedding: { provider: none } # 见 §3.5
```

- 默认 `include` 只含文本类扩展名：`.md .txt .rst .csv .json .yaml .toml .go .py .ts .js .rs .java .c .h .sh .sql .html .pdf .docx .xlsx .pptx`。媒体与压缩包永远不进。
- `pinned` 模式不产生 provider 调用：只处理 `cache.Complete(FileKey)` 为真的文件。pin 填充完成后 vfs 已经发 Change，Indexer 收到后检查完整性，未完整则等下次对账。
- `rules` 模式经 `vfs.FS.ReadFileRange` 读取：走块缓存、singleflight、三维限流与熔断，**不另起下载器**。拉下来的块按普通策略可被淘汰；抽取文本已持久化，版本不变就不会再次下载。
- 目录发现只用 meta 已知的树。`rules` 子树里未列举过的目录由 Indexer 调 vfs 目录读取（遵守 `dir_ttl`），这部分计入 meta 调用。
- 通过 MCP `index` 工具或控制台添加的规则持久化在 `index.db.rules`（`source='tool'`）。配置文件里的规则只能改配置移除，语义与 pin 规则一致。
- 非官方接口网盘（如 quark）不建议配置 `rules`，改用 `pinned`；文档与界面表单都写明这一点。

### 3.3 抽取（`internal/textract`）

签名 `Extract(ctx, kind Kind, r io.ReaderAt, size int64, opt Options) (Doc, error)`，
`Doc{Text, Headings []Heading, Truncated bool, ExtractorVer int}`。按扩展名加前 512 字节 sniff 定 `Kind`。

| 类型 | 做法 | 规模 |
|---|---|---|
| txt / md / 代码 / json / csv / yaml | 校验 UTF-8（非 UTF-8 → `failed`），去 NUL，**不改 CRLF**，所以 `start_off/end_off` 就是文件字节偏移，可直接交给 `read_text` | S |
| docx | `archive/zip` + `encoding/xml` 流式读 `word/document.xml`：`w:p` 为段落，`w:t` 拼文本，`w:pStyle` 为 `Heading1..6` 时输出 n 个 `#`，`w:tbl` 行输出 `\| a \| b \|` | 约 200 行 |
| xlsx | 先载入 `xl/sharedStrings.xml`（有上限），逐个 `xl/worksheets/sheet*.xml` 输出 `## <sheet>` 加逐行 CSV，每表最多 5000 行 | 约 150 行 |
| pptx | `ppt/slides/slide*.xml` 按编号排序，取 `a:t`，每页 `## Slide N` | 约 80 行 |
| pdf | `github.com/ledongthuc/pdf` 的 `Page.GetTextByRow()` | 约 100 行胶水 |

- **zip 防炸**：单条目解压上限 64 MiB，总量 256 MiB，条目数 4096，超出即 `failed`。`encoding/xml` 不解析外部实体。
- **PDF 库取舍**：`rsc.io/pdf` 2016 年后无维护；`pdfcpu` 偏页面操作；`unipdf` 是 AGPL/商业授权。选 `ledongthuc/pdf`，它是纯 Go、零传递依赖的维护分支。
- **PDF 的已知问题**：畸形文件会 panic，不支持加密，CID/CJK 字体常乱码，扫描件抽不出文本。对策是每文档 `recover()` 加 30 s 超时，并用乱码启发式判定失败（不可打印或替换字符占比 > 30%）。代码中标注 `UNVERIFIED: 中文 PDF 抽取质量需要真实样本验证`。
- **不做 OCR**，文档明说。
- 同一 `(remote, remote_id, version)` 不重复抽取。版本变了但 `text_hash` 相同时（例如只改了 mtime），跳过重新分块与重新嵌入。

### 3.4 分块（`internal/textract/chunk.go`）

- 窗口 800 **rune**、重叠 100 rune；按 rune 而不是字节计，CJK 字符不会被切成半个。代码文件窗口 1200 rune，按空行块切。
- 先按标题切 section：md 的 `#`、docx 的 Heading、pptx 的 Slide、xlsx 的 sheet。section 超出窗口时，依次按段落、句子（`。！？.!?\n`）再切。
- 每个 chunk 记 `seq, start_off, end_off, heading`，其中 heading 是标题路径，如"第二章 > 2.1 范围"。
- 分块是确定性的。`chunker_ver` 存在 documents 表，升级 chunker 只重新分块，不重新抽取。

### 3.5 嵌入（`internal/embed`，T-39）

```go
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Model() string
    Dim() int // 首次调用探测后固定；与 index_meta.embedding_dim 不一致即拒绝启动嵌入
}
```

- `openai`：`POST {base_url}/embeddings`，body `{model, input: [...], encoding_format: "float", dimensions?}`，`Authorization: Bearer`；`dimensions` 默认 512。
- `ollama`：`POST {base_url}/api/embed`，body `{model, input: [...]}`（批量接口；单条的 `/api/embeddings` 不用）。
- `fake`：测试用，hash → 确定性向量，记录调用次数与批大小。
- HTTP client 用 `httpx.New`，所以 `proxy.rules` 对嵌入端点同样生效。限流用独立的 `ratelimit.New` + `ratelimit.NewBreaker`：429 → `Throttled(retryAfter)`，连续失败熔断 → 嵌入 worker 休眠、检索降级。

```yaml
index:
  embedding:
    provider: ollama            # openai | ollama | none（默认）
    base_url: http://127.0.0.1:11434
    model: nomic-embed-text
    api_key: keyring:index.embedding   # openai 必填；由 cloudfs index auth 写入
    dimensions: 512             # 仅 openai
    batch: 64
    concurrency: 2
    qps: 4
    timeout: 30s
    proxy: direct
    allow_remote: false         # base_url 非回环/RFC1918 且为 false → 配置校验失败
    quantize: int8              # int8 | none
```

**隐私与成本要说清楚**。内容会**离开本机**发往 `base_url`；此时 `index_status.embedding.remote=true`，
界面常驻横幅。费用估算公式为 `chunks × ~300 token × 单价`：10k 文档 × 20 chunk 约 60M token，
按 text-embedding-3-small 现价约 1–2 美元。**换模型意味着全量重嵌入**。

### 3.6 数据模型（index.db）

```sql
CREATE TABLE index_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
-- keys: meta_store_identity（拷贝自 meta，不一致 → 整库重建）, extractor_ver, chunker_ver,
--       embedding_model, embedding_dim, quantize

CREATE TABLE rules(path TEXT PRIMARY KEY, include TEXT NOT NULL, exclude TEXT NOT NULL,
                   max_file_size INTEGER NOT NULL, source TEXT NOT NULL);  -- 'tool' | 'config'(只读镜像)

CREATE TABLE documents(
  id INTEGER PRIMARY KEY, remote TEXT NOT NULL, remote_id TEXT NOT NULL,
  version TEXT NOT NULL, path TEXT NOT NULL, ino INTEGER NOT NULL,
  kind TEXT NOT NULL, size INTEGER NOT NULL, mtime_ns INTEGER NOT NULL,
  text TEXT NOT NULL, text_hash BLOB NOT NULL, truncated INTEGER NOT NULL DEFAULT 0,
  extractor_ver INTEGER NOT NULL, chunker_ver INTEGER NOT NULL,
  state INTEGER NOT NULL,            -- 0 ok | 1 dirty | 2 failed
  error TEXT NOT NULL DEFAULT '', indexed_at INTEGER NOT NULL,
  UNIQUE(remote, remote_id));
CREATE INDEX documents_path ON documents(path);
CREATE INDEX documents_state ON documents(state);

CREATE TABLE chunks(
  id INTEGER PRIMARY KEY, doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL, start_off INTEGER NOT NULL, end_off INTEGER NOT NULL,
  heading TEXT NOT NULL DEFAULT '', text TEXT NOT NULL, UNIQUE(doc_id, seq));
CREATE VIRTUAL TABLE chunks_fts USING fts5(text, heading, content='chunks',
  content_rowid='id', tokenize='trigram');   -- external content，不复制文本；标准三触发器同步

CREATE TABLE index_pending(ino INTEGER PRIMARY KEY, path TEXT NOT NULL,
  reason INTEGER NOT NULL, queued_at INTEGER NOT NULL);

-- v2（T-39）只加表，不改表
CREATE TABLE vectors(chunk_id INTEGER PRIMARY KEY REFERENCES chunks(id) ON DELETE CASCADE,
  model TEXT NOT NULL, scale REAL NOT NULL, vec BLOB NOT NULL);  -- int8[dim] 或 float32[dim] LE，L2 归一
CREATE TABLE embed_pending(chunk_id INTEGER PRIMARY KEY, attempts INTEGER NOT NULL DEFAULT 0,
  next_at INTEGER NOT NULL DEFAULT 0);
```

- 文档身份是 `(remote, remote_id, version)`，路径只用于显示与范围过滤。目录改名只需一条 `UPDATE documents SET path = new || substr(path, length(old)+1) WHERE path = old OR path GLOB old||'/*'`，不重新抽取。
- 连接设置 `SetMaxOpenConns(4)`，写事务串行化在 `index.Store.writeMu`。
- meta 的 `store_identity` 变化后 ino 全部失效，需要重建。抽取文本按 `(remote, remote_id, version)` 保留，只重建 ino 与 path。

### 3.7 检索

- **为何用 trigram**：中文没有分词器，unicode61 会把整句当成一个 token。代价是少于 3 个 rune 的查询 FTS 无结果。此时有向量就走 vector-only，否则在范围内用 `chunks.text LIKE` 扫描，行预算 20000，超出置 `truncated`。预算思路沿用 `meta/search.go`，保证"没有匹配"与"停止查找"是两句不同的话。
- **向量内存布局**：启动或首次检索时，把 `vectors` 一次性载入扁平的 `[]int8`，旁边是 `[]uint32 chunkID` 与 `[]uint32 docID`。范围过滤先用 `documents.path` 求 doc_id 集合再打分，避免 top-k 过采样。
- **权限**：每个 hit 过 `checkPath`。范围与 allow 的交集算法直接复用 `internal/mcpsrv/server.go` 中 `search` 工具的实现。
- **`stale`**：hit 的 `documents.version` 与 `meta.Lookup` 当前版本不同时为真，查 `nodes_remote_id` 索引，O(1)。`semantic_search` 永远不承诺"全库最新"。
- **字节预算**：响应总字节按 `Limits.MaxBytes` 累计，超出置 `truncated`，与资源读取的预算做法一致。

### 3.8 MCP 工具（`internal/mcpsrv/index_tools.go`）

| 工具 | 参数 | 返回 |
|---|---|---|
| `semantic_search` | `query`, `path?`, `top_k?`（≤ `Limits.MaxResults`，默认 10）, `mode?`: `hybrid`\|`keyword`\|`vector`（默认 hybrid）, `max_snippet_bytes?`（默认 1 KiB） | `hits[]{path, seq, start_off, end_off, heading, score, snippet, offset_kind: "file"\|"text", stale}`, `mode_used`, `degraded?`, `truncated`, `index{docs, pending}` |
| `index_status` | `path?` | `covered`（被哪条规则覆盖）, `docs{ok,dirty,failed}`, `chunks`, `vectors`, `pending`, `failed[]`（≤20）, `text_bytes`, `embedding{provider, model, dim, remote, healthy, last_error, breaker_open_until}`, `fetch_budget{used,limit}` |
| `index` | `path`, `include?[]`, `max_file_size?` | 规则入库并排队；过 `checkPath`；`--read-only` 下允许（不改远端，与 `pin` 同理） |
| `unindex` | `path` | 只删 `source='tool'` 的精确匹配规则并级联删文档；配置规则拒绝并提示改配置 |
| `read_extracted_text` | `path`, `offset?`, `max_bytes?` | 返回 index.db 里的规范化文本（PDF/docx 也能直接读，不下载、不解析），上限 `Limits.MaxBytes`，带 `next_offset` |

`offset_kind: "file"` 表示偏移是文件字节偏移（文本类），`"text"` 表示是抽取文本偏移（Office/PDF，
配合 `read_extracted_text`）。不做 `cloudfs-index://` 资源。一期（T-37）`hybrid`/`vector` 降级为
keyword 并带 `degraded`；`index.enabled: false` 时 5 个工具都不注册。

### 3.9 增量维护（`internal/index/indexer.go`）

- **订阅** `FS.WatchChanges()`：`Rescan` 触发全量对账；`Paths` 落在规则内则写 `index_pending`；`Subtree` 触发该子树对账。对账只读 meta，经新增的 `meta.WalkSubtree` 分页遍历（`SubtreeRefs` 缺路径与名字，不够用）。
- **定时对账**：启动后 30 s 一次，之后每 10 min 一次。全部是本地 SQL，零远端调用，用来兜住 WatchChanges 队列溢出与 daemon 停机期间的变化。
- **抽取 worker**：单 goroutine，每文档一个事务。每次取文件前先让路：轮询 `FS.Busy()`，最长等 5 s，退避写法照抄 `prefetcher.waitIdle`。遇到 `ErrRiskControl` 或熔断打开时休眠 15 min，并在 `index_status` 报告原因与恢复时间。
- **嵌入 worker**（T-39）：独立 goroutine，按 `batch` 取 `embed_pending`。失败时指数退避，8 次后标 `failed`。换 `embedding.model` 会清空 vectors，全部 chunk 重新入 `embed_pending`，`chunks_fts` 不动。
- **删除与变更**：对账时发现文档已不在树里，或被规则或 exclude 排除，就级联删除。版本变化时置 `state=dirty` 并重新抽取；`text_hash` 相同则只更新 `version`。

### 3.10 指标、doctor、控制面与 CLI

- `/metrics`：`cloudfs_index_documents{state}`、`cloudfs_index_chunks`、`cloudfs_index_vectors`、`cloudfs_index_pending{kind=extract|embed}`、`cloudfs_index_embed_calls_total{result}`、`cloudfs_index_embed_chars_total`、`cloudfs_index_fetch_bytes_total`、`cloudfs_index_search_seconds`（直方图）。
- doctor 的检查项：
  - index.db 可打开，schema 匹配，`meta_store_identity` 一致。
  - 嵌入端点可达：发一次单条 `Embed`，仅在 provider ≠ none 时执行，文案注明会产生一次调用。`Dim()` 须与记录一致。
  - `max_chunks`/`max_total_text` 余量低于 10% 时告警，并报告 failed 文档数。
  - `--fix` 把 failed 文档重置为 dirty。
- 控制面：一期 `/index/status|rules|add|remove|rebuild|retry|failed|search|text` 与 SSE `index`，二期 `/index/embedding`、`/index/embedding/check`，完整路由表见 §6.2。
- CLI `cloudfs index status|add <path>|rm <path>|rebuild|search <query> [--mode]|auth`：优先走控制 socket，离线只允许 `status` 与 `search`，均以只读方式打开。

### 3.11 记忆库（T-40）

**为什么是纯文件而不是 KV 表**：

- 跨设备：网盘同步的是文件，不是本机 SQLite。另一台机器上的 CloudFS 靠 delta/TTL 看到同一份记忆，本地 index.db 各自重建。KV 表需要自己设计同步与冲突，等于重做网盘。
- 生态：Claude Code 的记忆是"一事一文件 + `MEMORY.md` 索引"的 Markdown；Codex 读 `AGENTS.md`；OpenClaw 用 memory 目录。文件形态让用户能用编辑器改、用 git 备份。
- 冲突：两台机器并发写同一记忆文件时，上传结果带 `ConflictName`，既有机制在旁边生成冲突副本并让父目录重新列举。工具只需把副本暴露出来。
- 检索：记忆目录就是一条内置 index 规则，`memory_search` 等于限定范围的 `semantic_search`。

KV 表只在"高频小写入、需要原子计数器"时占优，这不是本场景。

**目录约定**：

```
/<memory.root>/
  memory/
    <agent>/                 # claude-code | codex | openclaw | 自定义
      MEMORY.md              # 每行 "- [name](facts/name.md) — description"
      facts/<name>.md        # 一事一文件，YAML frontmatter: name, description, type, updated_at
    shared/                  # 跨 agent 共享，同结构
  skills/<name>/SKILL.md     # 只约定位置，工具不管
```

```yaml
memory:
  root: /work/.agent       # 默认 = mcp.workspace；必须落在 --allow 内，否则工具注册但一律拒绝并说明
  max_fact_bytes: 64KiB
  max_agent_bytes: 32MiB   # facts/ 子树总和，超过拒绝写入
```

`<agent>` 在 stdio 下默认取 `ClientInfo.name`，HTTP 下取令牌 principal 名称，规范化为 `[a-z0-9-]`，
可用 `cloudfs mcp --agent <id>` 覆盖。`name` 校验 `^[a-z0-9][a-z0-9-]{0,63}$`，拒绝 `..` 与 `/`。

**工具**（`internal/mcpsrv/memory_tools.go`，全部是 VFS + index 的薄封装）：

| 工具 | 参数 | 行为 |
|---|---|---|
| `memory_list` | `agent?`, `cursor?` | 读 `facts/`（与 `list_directory` 同一分页），每项带 frontmatter 字段（只读前 4 KiB 解析） |
| `memory_get` | `name`, `agent?` | 返回 `content`、`version`、`path`、`conflicts[]`：`conflicts` 是同目录下以 `<name>` 开头、但不等于 `<name>.md` 的兄弟文件。副本命名由 provider 决定，工具不猜格式 |
| `memory_put` | `name`, `content`, `agent?`, `mode?: replace\|append`, `expected_version?` | 先校验大小与预算；`expected_version` 与当前版本不同则拒绝，提示 "memory changed elsewhere; re-read"。写入走 `FS.WriteFile`，再更新 `MEMORY.md` 对应行：唯一匹配则替换，否则追加。`--read-only` 下拒绝 |
| `memory_delete` | `name`, `agent?`, `confirm` | 删除文件与索引行，必须 `confirm=true` |
| `memory_search` | `query`, `agent?`, `include_shared?`, `top_k?`, `mode?` | 等价 `semantic_search{path: memory/<agent>}` ∪ `shared`，无嵌入时 keyword |

冲突处理不加新工具，agent 按以下流程处理：

1. 从 `memory_get.conflicts` 看到冲突副本。
2. 用 `read_text` 读取副本内容。
3. 用 `memory_put` 写入合并结果。
4. 用 `delete` 删除副本。

`expected_version` 的比对与写入不是原子的，窗口在毫秒级且限于单机；跨设备竞争由网盘冲突副本兜底，
文档如实写明。另一台设备的写入在本机可见的延迟等于 delta feed 或 `dir_ttl`；要强制看到最新，先调
`memory_list`。

### 3.12 界面

**「索引」屏 `#/index`**（`screens/index.js`，图标 `layers`，ui-plan F4，T-37）：

- 未启用时整屏显示说明与配置示例，不渲染空表，也不请求 `/index/rules`。
- **概况卡**照缓存屏四卡布局：
  - 四张卡分别是文档（正常/待处理/失败）、分块数、文本占用与 `max_total_text` 之比、本小时下载与 `fetch_budget` 之比。
  - SSE `index` 事件驱动进度条"正在抽取 N / 待处理 M"。
  - worker 让路或风控休眠时，显示原因与恢复时间。
- **规则表**：
  - 列为路径、包含、排除、单文件上限、来源（配置或界面）、已覆盖文档数、动作。
  - 配置来源的规则只显示"在配置文件中修改"，同缓存屏 pin 的做法。界面来源的规则可以"移除"，经 `confirmDelete` 键入路径确认。
  - "添加规则"用 `openForm`，字段为路径、包含 glob（预设"文档"/"代码"/"全部文本"）、单文件上限。表单下方常驻提示"会按限流下载该目录下匹配的文件；非官方接口网盘建议改用固定"。
- **失败文档表**：列为路径、类型、错误（如"PDF 无可抽取文本"）、时间、"重试"动作；分页。
- **重建索引**：`confirmDelete` 键入 `rebuild`，请求带 `confirm: true`。

**主窗口搜索框**（改 `screens/main.js` 现有搜索逻辑）：

- 左侧"文件名 / 内容"分段切换，仅在 `status.index.enabled` 时出现。选择存 localStorage，只是界面偏好。
- 内容模式调 `/index/search`。结果行显示文件图标、路径、标题路径和"可能已过期"标记（`stale`）。
- 片段由 `snippet.js` 高亮查询词，截断到 240 字符，且不切半个字。片段来自文件内容，必须按文本插入，不能当 HTML。
- 点击结果行打开"抽取文本"浮层并滚动到命中段。`truncated` 与 `degraded` 作为结果内的常驻说明行，同现有 `search.truncated` 做法。
- T-39 把切换扩为"文件名 / 关键词 / 语义"。降级时语义按钮旁显示"已降级为关键词"。

**检查器**：

- 新增"索引"信息行，显示已索引 · N 块、待处理、失败原因或未覆盖。
- 动作"加入索引"调 `POST /index/add`，对文件和目录都可用。"移出索引"只对界面来源的规则出现。
- "查看抽取文本"用 `showPanel` 分页读 `/index/text`，底部是"加载更多"。PDF/docx 也能读。

**嵌入端点面板**（「索引」屏内，ui-plan F6，T-39）：

- 面板显示 provider、模型、维度与地址。`remote=true` 时常驻黄色横幅"文件内容会发送到 <host>"，不可关闭。
- 健康状态显示健康点、最后错误和熔断恢复时间。进度显示已嵌入与待嵌入数。
- 显示本月嵌入字符数，并按公式估算费用，文案明说是估算。
- "测试端点"按钮先说明"会产生一次调用"，再请求 `POST /index/embedding/check`。
- 未配置 `api_key` 时，显示 `cloudfs index auth` 命令和复制按钮，**不提供输入框**。
- provider 与 model 的改动需要全量重嵌入，所以面板底部只写"在配置文件中修改"。
- 概况卡增加"向量 N / max_chunks"。

**「Agent」屏 · 记忆标签**（ui-plan F7，T-40）：

- 左侧是 agent 列表（claude-code、codex、openclaw、shared），每项显示条数与"占用 / 上限"。
- 右侧表格列为名称、描述、类型、更新时间、冲突标记（红点加"有冲突副本"文字）。顶部有记忆搜索框。
- **记忆编辑浮层**：
  - 字段为 frontmatter（名称只读、描述、类型）、正文 `textarea`、字节计数与上限。
  - 保存时带 `expected_version`。版本冲突时提示"已在其他设备修改"，并提供"重新载入"。
- **冲突合并浮层**：
  - 由 `memory_conflicts.js` 配对，左右只读并排显示本体与冲突副本。
  - "保留本体并删除副本"：副本删除走既有 `POST /fs/delete`，path 取自 `conflicts[]`，经 `confirmDelete` 并带 `confirm: true`。
  - "用副本覆盖本体"：先读副本，再 `PUT /memory/{agent}/{name}` 带 `expected_version`，最后删除副本。
  - "手动合并"：打开编辑浮层，预填两段内容。
- "新建记忆"用 `openForm`，字段为 agent、名称、描述、类型；名称在前端按同一正则校验。
- 未配置 `memory.root` 或它不在 allow 内时，标签页显示说明与配置示例。

验收见 TODO.md T-37、T-39、T-40；界面测试文件为 `ui_index_test.go`、`ui_embedding_test.go`、`ui_memory_test.go`。

---

## 4. 维度三：作用域、令牌、回滚与审计（T-34、T-35、T-38、T-43）

### 4.1 决定设计走向的代码事实

- **SDK 会话不能当身份**。`internal/mcpsrv/http.go` 的 2026-07-28 传输是 `Stateless: true`，每个 POST 一个 SDK session。`Session.ID()` 只在 legacy HTTP 下非空。
- **权限是进程全局且不分读写**。`Options.Allow` 全进程共用，`checkPath`（`internal/mcpsrv/server.go:149`）不区分读写，`checkWrite` 只看 `ReadOnly`。所有工具处理函数都忽略 `*mcp.CallToolRequest`，但 `New` 已挂 `AddReceivingMiddleware`（`server.go:111-112`），把会话塞进 ctx 的位置是现成的。
- **旧版本内容没人替你留着**。`journal.Succeed` 清空 `blob_path` 并释放 blob；旧版本只作为读缓存 `FileKey{RemoteID, Version}` 存在，可被淘汰。`cache.HydratedPath` 与 `LinkPinnedFile` 是现成的"整文件硬链接"原语。
- **stdio 与 mount 并存时不是同一 VFS**。`cmdMCP`（`cmd/cloudfs/main.go`）走 `daemon.Open`；若 mount 已在跑，stdio 进程不是 journal owner，`internal/daemon/daemon.go` 的 `!j.Owner()` 分支给它独立 VFS 且不启 uploader。它的 `WatchChanges()` 看不到内核写。
- **控制面守卫是现成的**。路由表 `internal/control/metrics.go` 与 `security_all_routes_test.go` 会把新路由自动纳入守卫测试；`confirmed()`（`internal/control/shared.go:29`）是现成的确认门。

### 4.2 Scope、Principal、Session

```go
// internal/agent/scope.go
type Scope struct {
    Read      []string  // 可读前缀；空 = 整个挂载（沿用 Allow 语义）
    Write     []string  // 可写前缀；nil = 同 Read；空切片 = 只读
    ReadOnly  bool
    ExpiresAt time.Time // 零值不过期
    Sandbox   string    // 非空时 Write 收窄为该目录
}
func (s Scope) Check(p string, write bool) (string, error) // 取代 checkPath + checkWrite
func (s Scope) Narrow(child Scope) Scope                    // 只能收窄，不能放大

type Principal struct{ ID, Kind /* stdio|token|console */, Name string; Scope Scope }
type Session   struct{ ID, PrincipalID, ClientName, ClientVersion, Workspace string; Scope Scope; Sandbox bool; State string }
```

- `mcpsrv.Options` 保留 `Allow`/`ReadOnly`，`New` 里转成 `defaultScope`。新增 `Options.Sessions *agent.Sessions`，为 nil 时没有会话能力，行为同今天。
- 33 个工具（36 处调用点）的 `checkPath(p)` 机械替换为 `checkPath(ctx, p, write)`；`delete`/`move`/`copy` 两端都按写检查。资源订阅注册（`reserve → parseResource`）改用同一个 `Scope.Check`。
- `Scope.Narrow` 写错一次就是越权。它是纯函数，用表驱动测试穷举父、子、兄弟、根、`..` 与尾斜杠组合。
- 团队场景以后只需给 principal 加 `owner` 字段，模型不用重来。

### 4.3 agent.db schema（v1）

```sql
CREATE TABLE principals (id TEXT PRIMARY KEY, kind TEXT, name TEXT, token_hash TEXT, scope TEXT,
  created_at INTEGER, expires_at INTEGER, revoked_at INTEGER, last_used_at INTEGER);
CREATE TABLE sessions (id TEXT PRIMARY KEY, principal_id TEXT, client_name TEXT, client_version TEXT,
  scope TEXT, workspace TEXT, sandbox INTEGER,
  state TEXT /* active|finished|rolled_back|expired */,
  started_at INTEGER, last_seen_at INTEGER, finished_at INTEGER, summary TEXT);
CREATE TABLE audit (id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER, principal_id TEXT, session_id TEXT,
  transport TEXT, tool TEXT, paths TEXT, args TEXT, bytes_in INTEGER, bytes_out INTEGER,
  result TEXT /* ok|denied|error */, error TEXT, duration_ms INTEGER);
CREATE INDEX audit_ts ON audit(ts);
CREATE INDEX audit_session ON audit(session_id, id);

-- T-38
CREATE TABLE session_ops (seq INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, audit_id INTEGER,
  op TEXT /* create|overwrite|append|edit|mkdir|rename|delete */, path TEXT, to_path TEXT,
  pre_state TEXT /* absent|file|dir */, pre_remote TEXT, pre_remote_id TEXT, pre_version TEXT,
  pre_size INTEGER, pre_hash TEXT, pre_blob TEXT, pre_reason TEXT /* ''|too_large|not_cached|dir */,
  post_version TEXT, rolled_back INTEGER, rollback_result TEXT);
CREATE INDEX session_ops_path ON session_ops(path);

-- T-41
CREATE TABLE trigger_deliveries (id INTEGER PRIMARY KEY AUTOINCREMENT, rule TEXT, path TEXT, kind TEXT,
  origin TEXT, first_seen INTEGER, due_at INTEGER, attempts INTEGER,
  state TEXT /* pending|running|done|dead */, last_error TEXT, output TEXT, done_at INTEGER);
CREATE UNIQUE INDEX trigger_pending ON trigger_deliveries(rule, path) WHERE state='pending';
```

`session_ops_path` 索引支撑 `GET /sessions?path=` 反查：检查器里的"来自会话"与"被 Agent 修改"都靠它。

### 4.4 会话解析顺序

会话在 mcpsrv receiving middleware 里解析，结果写入 `ctx = agent.WithSession(ctx, s)`，工具处理函数签名不动。

1. **stdio**：一个进程对应一个 principal（`kind=stdio`，scope 来自 `--allow`/`--read-only`）。`ServerOptions.InitializedHandler` 触发时，用 `ClientInfo` 建一个隐式会话。`begin_session` 建显式子会话，并成为该连接的当前会话。
2. **legacy HTTP**：用 SDK 的 `Session.ID()` 查当前会话表。
3. **无状态 HTTP**：principal 由 bearer token 确定；当前会话是该 token 最近一个活动会话，空闲 30 分钟自动 `expired`。二期允许用 `_meta["cloudfs/session"]` 显式指定。

Claude Code 的 HTTP 注册只支持静态 header，所以**令牌是身份，会话靠空闲轮转**。审计里同一令牌会出现
多个会话，这是明说的限制。

### 4.5 stdio 拓扑处置（T-35、T-43）

- **一期**：新增 `cloudfs mcp install --client claude|codex --transport http`，只是 `ClientConfig` 多一种输出。文档改为"mount 在跑就用 HTTP"。`cmdMCP` 发现自己不是 owner 时打警告，会话与回滚类工具返回 `requires the storage owner; use the HTTP transport`，审计照写（§1.3）。
- **T-43 结论（2026-09-15）**：e2e 复现失败——非 owner stdio 的 `write_file` 报 `journal: publication requires storage ownership`，但节点已进共享 meta，挂载侧 `cat` 得 EIO，journal 行停在 `needs_publish=1`，owner 重启后复活。桥提前；细节见 TODO.md T-43 与 `docs/mcp.md`"与挂载并存"。
- **T-43 处置（2026-09-15，线 C C0.5/C3）**：桥落地前，非 owner 的 mcpsrv 写工具在 scope 检查之后、碰 VFS/export/index 之前一律返回 `… requires the storage owner; use the HTTP transport: cloudfs mcp install --transport http`（`internal/mcpsrv/owner_fence.go`），`internal/vfs` 的 `ErrNotOwner` 兜底；e2e 改为 `TestStdioBesideMountRefusesWritesCleanly`，把每条观察到的现象变成否定断言。条目保持开放，只等桥。
- **T-43 验证缺口（原文）**：先写 e2e 复现。mount 与 stdio MCP 同时写同一目录，然后读回、排空、重启，确认有没有丢失、复活或延迟可见。journal 行由 owner uploader 领走，但 `needs_publish` 在另一个进程，这一点尚未核实。结论写回 T-43，并据此决定 stdio→HTTP 桥是否提前。该桥用 SDK `StreamableClientTransport` + 原始 schema `AddTool`，约 300 行。
- **修正文档**：`docs/mcp.md`"与挂载并存"一节的"共用同一个 VFS 实例"只对 owner 进程内的 HTTP 传输成立，已补注。

### 4.6 访问令牌（T-35）

- `cloudfs mcp token create --name codex --read /work --write /work/.agent --ttl 720h`：生成 32 字节随机令牌，`principals` 表只存 `sha256(token)`，明文只打印一次。另有 `cloudfs mcp token list|revoke <id>`。
- `requireBearer`（`internal/mcpsrv/http.go:165`）从"单一 env token"扩为两种：env token 全权（兼容今天），或查 principals 表（作用域取该行 scope）。过期或吊销的令牌 401。
- 吊销时 `Server` 主动 `Close()` 该 principal 的 legacy 会话，5 s 内生效。
- 已有资源订阅在注册时校验一次权限，这个边界不变（见 `docs/mcp.md`）；吊销靠关闭会话收回。
- 控制面路由：
  - `GET /mcp/connect`：返回 HTTP 是否监听及地址、本进程是否 owner、是否检测到 stdio 非 owner、两种客户端注册片段。**不含令牌**。
  - `GET /mcp/tokens`：只返回指纹，即令牌前 4 位。
  - `POST /mcp/tokens`：响应头 `Cache-Control: no-store`，不落日志。
  - `POST /mcp/tokens/{id}/revoke`：需要 `confirm`。

### 4.7 审计日志（T-34）

**记什么**：receiving middleware 拦截 `tools/call`，其它方法只记 `initialize` 与订阅注册。每行字段如下。

| 字段 | 规则 |
|---|---|
| `tool`、`transport`、`principal_id`、`session_id` | 来自会话解析 |
| `paths` | 从参数已知键 `path/paths/from/to` 抽取并 `normalise` |
| `args` | 脱敏：`content`、`edits[].new_text` 替换为 `{bytes:n}`；总长 ≤ 4 KiB；绝不含直链 URL、token、cookie |
| `result` | `ok` / `denied`（`ErrDenied` 或 read-only）/ `error` |
| `error` | 经 `mapErr` 后的文本，不含原始 provider 错误 |
| `bytes_in` / `bytes_out` / `duration_ms` | 原始字节数与耗时 |

**写入策略**：同步单条 INSERT（WAL）。写失败只打日志，不让工具失败——审计不能变成拒绝服务的理由；
失败计数进 `cloudfs_audit_write_failures_total`。保留期 `mcp.audit.retain`（默认 90 天），`Purge` 在 owner
启动时和每日执行。

**与 `vfs.Change` 的区别**（写进 `docs/vfs-changes.md` 顶部）：

- Change 是内存中、有损、无主体的"去重读"提示，包含内核与远端来源。
- audit 是持久、无损、有主体（principal/session）的"谁做了什么"。它只包含经 MCP 的操作（二期起含控制台发起的 `agent.invoke`），也包含被拒绝和失败的调用。

**入口**：

- 控制面：`GET /audit?cursor&session&tool&result&since`，游标按 id，与 `/uploads` 同式；`GET /sessions`、`GET /sessions/{id}`（含 ops）。
- SSE：`audit` 事件每条一发，溢出策略同 change 事件。
- CLI：`cloudfs audit [--session] [--since 1h] [--tool] [--json]` 与 `cloudfs sessions list|show|finish`。在线走控制面；离线以 `mode=ro` 打开 agent.db，同 `cloudfs uploads` 的离线路径。

### 4.8 会话快照与回滚（T-38）

**承诺范围**（原文写进 `docs/mcp.md`）：

1. 回滚是**经 VFS 发起的一组新写入**，走 journal → upload → 冲突检测，不是远端历史版本恢复。本地立即可见，远端最终一致。
2. 会话之后又被别人改过的文件**跳过并报 conflict**，不静默覆盖。远端仍有变化时，uploader 会按 `conflictName` 生成冲突副本，这是最后一道网。
3. 只覆盖 MCP 工具发起的修改。内核写、控制台 `/fs/*` 与 WebDAV 不在会话内。

**前像捕获**（`internal/agent/preimage.go`）。mcpsrv 的写工具在调用 `FS.WriteFile/Rename/Remove/Mkdir`
**之前**执行以下步骤，逻辑不进 vfs：

1. `StatPath`。路径不存在记 `pre_state=absent`；是目录记 `pre_state=dir`，不存内容。
2. 若是文件且 `size ≤ mcp.session.max_preimage_bytes`（默认 32 MiB），先经普通读路径把文件读满。`edit_file` 本来就读过，零额外远端调用；`write_file overwrite` 需要一次读取。
3. 然后 `cache.HydratedPath(FileKey{Remote, RemoteID, Version})`，再 `os.Link` 到 `agent/preimages/<sha256>`。两者在同一文件系统，并经 `cache.ReserveDisk` 记账。`cloudfs-local:` 开头的未上传文件走同一条路，因为其内容就在 cache 硬链接里。
4. **链接成功才写 `session_ops` 行**。这个顺序保证崩溃只会留下孤儿 blob，不会留下假前像；启动时 `agent.Recover` 清理孤儿。
5. 文件超过大小上限或没读满时，记 `pre_reason=too_large|not_cached`，写操作照常执行。**不因为留不住前像而拒绝写**。
6. `delete recursive=true` 删目录时，一期只记 `pre_state=dir`，回滚报 `skipped: dir`；逐文件前像在三期。
7. 操作完成后回填 `post_version`。

**回滚**（`agent.Sessions.Rollback(ctx, fs, id, dryRun, force)`，按 `seq` 逆序）：

| op | 动作 | 前置检查 |
|---|---|---|
| overwrite / edit / append | `FS.WriteFile(path, preimage)` | 当前 version == `post_version`，否则 conflict |
| create | `FS.Remove(file)` | 同上 |
| mkdir | 仅当目录为空时 `Remove` | 非空 → skipped |
| rename | `Rename(to → from)` | `from` 现已存在 → conflict |
| delete（文件） | `WriteFile(path, preimage)` | 须有 `pre_blob`；path 现已存在 → conflict |

- 每个 op 的结果写入 `rollback_result`，整体返回 `{restored, skipped, conflict}` 计数与逐项清单。
- `dry_run=true` 只计算这张表，不产生任何 VFS 写，journal 行数不变。
- 回滚本身记为一个新会话（`name="rollback of <id>"`），同样捕获前像，所以**回滚可以再回滚**。它总是被结束：
  跑完是 `rollback of <id>: …` 摘要，中途失败是 `rollback interrupted: <原因>`，不会留下 `active` 的回滚会话。
- 回滚仍 `active` 的会话先像 `finish_session` 一样结束它、再读 ops 行，这样回滚期间连接上的写入进入新会话，
  不会记进正被回滚的会话而逃过撤销；`dry_run` 不改变会话状态。
- 入口：
  - MCP `rollback_session{session_id, confirm, dry_run?}`，带 `DestructiveHint`。
  - 控制面 `POST /sessions/{id}/rollback {dry_run | confirm}`。
  - CLI `cloudfs sessions rollback <id> --confirm [--dry-run]`。
- **为什么不复用 journal blob 或 RemoteVersion**：`Succeed` 后 blob 已释放；网盘普遍没有"按版本号取内容"的 API，`Caps` 里也没有这个能力位。复用读缓存硬链接是唯一不引入新存储格式、不重复下载的办法。代价是"没读满就没有前像"，这一点如实报告。
- **GC**：会话进入 `finished`/`expired` 后，前像按 `mcp.session.retain`（默认 7 天）回收。unlink 只删自己的名字，不影响缓存里的同一 inode。

### 4.9 界面

**「Agent」屏 `#/agents`**（`screens/agents.js`，图标 `bot`，ui-plan F1/F2）。导航项徽标显示活动会话数；
屏幕分四个标签：会话（§2.7）、审计、访问令牌、记忆（§3.12）。

- **审计标签**（T-34）：
  - 过滤条：会话、工具、结果（ok/denied/error）、时间（1h/24h/7d）。
  - 表格列：时间、客户端、工具、路径（多路径折叠）、结果、字节、耗时。denied 行红底，同诊断屏配色，并有文字标签，不只靠颜色。
  - `args` 点击展开为只读 JSON（已脱敏）。
  - SSE `audit` 事件在未翻页且无过滤时插入顶部。`store.js` 不缓存审计行，每屏自己持有。
- **访问令牌标签**（T-35）：
  - 表格列：名称、指纹（前 4 位）、可读、可写、过期、最后使用、状态（有效/过期/已吊销，点加文字）。
  - "新建令牌"用 `openForm`，字段为名称、可读路径（多行，每行一个前缀）、可写路径、有效期（1 天/7 天/30 天/永不）、只读开关。前端先用 `scope_view.js` 校验"可写必须在可读内"。
  - 吊销经 `confirmDelete` 键入令牌名称，请求带 `confirm: true`。
- **令牌揭示浮层**（`openPanel`，在 agents.js 内）：
  - 内容为令牌明文加 `copyBtn`、醒目文字"关闭后无法再次查看"、Claude Code 与 Codex 两个含该令牌的 HTTP 注册片段（各带复制按钮）。
  - 令牌只存在于浮层的局部变量，关闭即丢弃，不进 `store.js`、不进 localStorage。本条待用户确认，见 §6.4。
- **接入面板**（Agent 屏顶部，可折叠，T-35/T-43）：
  - 显示 HTTP 监听状态点与地址、owner 状态。未启用 HTTP 时给出配置说明。
  - `/mcp/connect` 返回 `stdio_non_owner: true` 时显示黄色横幅"请改用 HTTP 传输"，链到 `#/diagnostics` 的对应检查项。
- **首次设置完成页**（改 `screens/setup.js`）：加"连接 Agent"卡片，跳到 `#/agents` 并展开接入面板。
- **会话详情浮层 · 操作表**（T-38，ui-plan F5）：
  - 列为序号、操作（新建/覆盖/编辑/改名/删除/建目录）、路径（改名显示旧、新两个路径）、前像（可恢复/过大/未缓存/目录，点加文字）、回滚结果。
- **回滚流程**（T-38）：
  1. 点"回滚此会话"（图标 `undo`），先请求 `dry_run: true`。
  2. **预览浮层**由 `rollback_plan.js` 分组：将恢复 N（列出）、将跳过 M（附原因）、冲突 K（"会话之后此文件又被修改"）。底部是 §4.8 的三句承诺。
  3. 经 `confirmDelete` 键入会话短 ID，请求带 `confirm: true` 执行。未经预览不能执行。
  4. 结果浮层同样分三组，并提供"回滚这次回滚"。
- **会话表**：状态新增"已回滚"，行内有快捷"回滚"入口。
- **检查器**：文件在保留期内被某会话修改过时，显示"被 Agent 修改 · <client> · <时间>"，数据来自 `GET /sessions?path=`，点击打开会话详情。
- **诊断屏**：JS 不改，doctor 新检查项自动出现，包括 agent.db 可打开、schema 匹配、"MCP stdio 进程与挂载并存"。并存检查在 journal flock 持有者不是自身且存在 stdio 心跳文件时报 warn，detail 给出 `cloudfs mcp install --transport http`。

验收见 TODO.md T-34、T-35、T-38、T-43；界面测试文件为 `ui_agents_test.go`、`ui_tokens_test.go`、`ui_rollback_test.go`。

---

## 5. 维度四：触发器与发送给 Agent（T-41、T-42）

### 5.1 vfs 侧最小改动

```go
// internal/vfs/changes.go —— 只加字段，旧消费者不受影响
type ChangeKind uint8 // KindWrite, KindCreate, KindMkdir, KindRemove, KindRename, KindRemote, KindRescan
type Origin     uint8 // OriginKernel, OriginAPI (mcp/control/webdav), OriginRemote
type Change struct { Paths []string; Subtree, Rescan bool; Kind ChangeKind; Origin Origin }
func WithOrigin(ctx context.Context, name string) context.Context // mcpsrv middleware / control / webdav 各打一次
```

`internal/vfs/write.go` 的 9 个 emit 点各填自己的 kind；`changedListing` 填 `KindRemote`；
`fromKernel(ctx)`（`internal/vfs/vfs.go:1017`）决定 `OriginKernel`。`Affects` 不变。

### 5.2 配置

```yaml
triggers:
  - name: inbox-to-agent
    paths: ["/work/inbox/**"]        # glob（自带匹配器，无新依赖），挂载相对路径
    events: [create, write, rename]  # 默认全部；remote = delta/刷新发现
    origins: [kernel, remote]        # 默认全部；排除 api 可避免 agent 自触发
    debounce: 2s
    on_rescan: ignore                # 默认 deliver
    action:
      exec: { command: ["/usr/local/bin/summarize", "{path}"], cwd: ~/work, timeout: 10m }  # 占位符只能是独立的 argv 元素
  - name: notify
    paths: ["/work/reports/**"]
    events: [write]
    action:
      webhook: { url: https://hooks.example/cloudfs, secret: keyring:cloudfs/hook, timeout: 15s, include_download_url: false, proxy: direct }
agents:
  - name: claude
    exec: { command: ["claude", "-p", "{prompt}"], cwd: "~", timeout: 30m }
```

`Validate()`（`internal/config/triggers.go`）对未排除 `api` 来源的 exec 规则给出 warning（不是错误），
`cloudfs mount` 启动时逐条打到 stderr，文档首例就演示 `origins: [kernel, remote]`。`{path}`/`{kind}`/`{uri}`/`{prompt}`
只能作为独立的 argv 元素出现（`"Summarize {path}"` 会被拒绝，因为替换只按整个元素进行），`{prompt}` 只有 `agents[]` 可用；
webhook 的 `secret` 必须是 `keyring:`/`secretfile:` 引用；glob 匹配器在 `internal/pathglob`，索引规则与触发器共用。

### 5.3 引擎（`internal/trigger`，只在 owner 进程运行）

- **入队**：一个 goroutine 消费 `FS.WatchChanges()`，按规则匹配路径 glob、kind 与 origin；`Subtree` 事件按前缀匹配 glob 根。命中即对 `trigger_deliveries` 执行 `INSERT OR IGNORE`，`due_at = now + debounce`。`(rule, path)` 上有 pending 唯一索引，所以去抖窗口内的重复事件自然合并。
- **Rescan**：每条规则插入一行 `path='' kind=rescan`，除非规则配置了 `on_rescan: ignore`。
- **执行**：每条规则一个串行 worker，到期领取后依次置 `running`、执行、置 `done`。失败退避从 1 s 增长到 5 min，8 次后置 `dead`，控制台可重试。重启时 `running` 改回 `pending`。
- **至少一次投递**：进入变更流的事件不会因为进程死掉而丢失。被队列溢出压成 rescan 的事件只能以 rescan 形式送达，文档写明这一限制。
- 规则数量是个位数，变更流已有 64 条队列，每个事件线性匹配即可，不需要索引。

### 5.4 exec 执行器的安全约束

- `exec.Command(argv[0], argv[1:]...)`，**没有 shell**。
- `{path}`/`{kind}`/`{uri}`/`{prompt}` 只作为独立 argv 元素整体替换，永不拼进字符串。
- 环境变量只保留 `PATH`/`HOME`/`LANG`，外加 `CLOUDFS_PATH`/`CLOUDFS_KIND`/`CLOUDFS_URI`。
- `timeout` 到期杀进程组（`Setpgid`），否则 `claude -p` 的孙进程会活下来。
- stdout 与 stderr 各截断到 64 KiB，存入 `output`。
- 命令白名单就是配置文件本身，配置文件已有 flock 与原子编辑保护。MCP 与控制面都不接受任意命令。

### 5.5 webhook

- `POST` JSON `{rule, path, uri, kind, origin, ts, size?}`。
- 头 `X-CloudFS-Timestamp`（Unix 秒，十进制）与 `X-CloudFS-Signature: sha256=<hex(HMAC-SHA256(secret, ts + "." + body))>`。
- secret 只接受 `keyring:`/`secretfile:` 引用。URL 只允许 https 或回环 http，其它情况须显式 `insecure: true`。
- `include_download_url=true` 才携带直链，文档标注这是把签名 URL 交给第三方。
- 出站走 `proxy.Manager` 的 Dial（可选 `proxy:` 出口名），不走 provider 限流。

接收端校验示例（写进用户文档，界面空状态也展示这段）：

```go
func verify(secret []byte, r *http.Request, body []byte, now time.Time) bool {
	ts := r.Header.Get("X-CloudFS-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Sub(time.Unix(sec, 0)).Abs() > 5*time.Minute {
		return false // 拒绝重放
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(r.Header.Get("X-CloudFS-Signature")))
}
```

### 5.6 与 MCP 的关系

不新增"MCP notification"动作。资源订阅已经覆盖"通知 agent 某路径变了"，而 Claude Code 对自定义
通知不响应。若要"让 agent 主动拉任务"，三期加 `pull_events` 工具从 `trigger_deliveries` 读取，不加推送。

### 5.7 发送给 Agent（T-42）

- `GET /agent/prompt?path=`：返回一段 MCP-ready 提示词，包括虚拟路径、`cloudfs://` URI、`read_text`/`read_extracted_text`/`edit_file` 用法提示，会话可用时附 `begin_session` 建议。零依赖，总是可用；不存在的路径返回 404，不泄露内部路径。
- 配置了 `agents:` 时：
  - `GET /agent/endpoints` 只返回名称，不返回 argv。
  - `POST /agent/invoke {agent, paths[], prompt?, confirm: true}` 复用 T-41 的 exec 执行器，白名单、无 shell、输出截断三条约束相同。`paths` 只作为独立 argv 元素。
  - 投递写入 `trigger_deliveries`（`rule=agent:<name>`），审计记 `principal=console`、`tool=agent.invoke`。
- 未配置 `agents:` 时，`/agent/invoke` 返回 404，且不启动任何进程。
- "复制提示词"不依赖 exec，可以提前到一期末交付；"运行"按钮随 T-41 一起上线。

### 5.8 界面

**「触发器」屏 `#/triggers`**（`screens/triggers.js`，图标 `bolt`，ui-plan F8）。导航徽标显示 dead 投递数。

- **规则卡片列表**（只读）：
  - 每张卡显示名称、路径 glob、事件、来源、动作类型。
  - exec 规则按元素逐个等宽渲染 argv，不拼接成命令行，避免误读成 shell。webhook 规则显示 URL 与"签名密钥已配置"，不显示密钥。
  - `trigger_view.js` 判断自激风险，命中时加黄色标记"此规则可能被 Agent 自身写入触发"。
  - 卡片底部写"在配置文件中修改"。
- **投递表**：
  - 列为时间、规则、路径、事件、来源、次数、状态（待处理/执行中/完成/失败，点加文字）、动作（dead 行有"重试"，调 `/triggers/retry`）。
  - 可按规则与状态过滤；分页；未翻页时由 SSE `trigger` 刷新。
  - 行点击用 `showPanel` 打开详情：exec 显示 stdout/stderr 并提示是否截断，webhook 显示响应码与错误。数据来自 `GET /triggers/deliveries/{id}`。
- **测试投递**：
  1. 用 `openForm` 选择规则并输入路径。
  2. 经 `confirmDelete` 键入规则名确认，因为 exec 会真实执行。
  3. 请求 `POST /triggers/test {name, path, confirm: true}`。
  4. 完成后自动打开该投递的详情。
- **空状态**：未配置任何规则时，整屏显示 exec 与 webhook 两个配置示例和 §5.5 的校验代码。

**发送给 Agent 浮层**（`send_to_agent.js`，ui-plan F9）：

- **入口**有两个：检查器的"发送给 Agent"按钮（图标 `bot`，文件与目录都有），以及内容搜索结果行右侧的同名按钮（预填命中路径与标题）。
- **浮层内容**：预填提示词的 `textarea`（可编辑，内容按文本插入），加上始终可用的"复制"按钮。
- **运行**：`/agent/endpoints` 非空时，浮层下方出现 agent 下拉与"运行"按钮。
  1. 点"运行"后经 `confirmDelete` 键入 agent 名称确认，因为会执行本机命令。
  2. 请求 `POST /agent/invoke`。
  3. 成功后 toast "已提交"，附"查看投递"链接跳 `#/triggers?delivery=<id>`。

验收见 TODO.md T-41、T-42；界面测试文件为 `ui_triggers_test.go`、`ui_send_to_agent_test.go`。

---

## 6. 界面总览

### 6.1 屏幕地图

| 位置 | 形态 | 内容 | 条目 | 期 |
|---|---|---|---|---|
| 导航 `#/agents`「Agent」 | 新屏 `screens/agents.js`，四个标签 | 会话 / 审计 / 访问令牌 / 记忆 | T-34 T-35 T-36 T-38 T-40 | 1（记忆 2） |
| 导航 `#/index`「索引」 | 新屏 `screens/index.js` | 概况卡、规则表、失败文档、嵌入端点面板、重建 | T-37 T-39 | 1（嵌入面板 2） |
| 导航 `#/triggers`「触发器」 | 新屏 `screens/triggers.js` | 规则（只读）、投递记录、测试投递、重试、输出查看 | T-41 | 2 |
| 主窗口搜索框 | 改 `screens/main.js` | "文件名 / 内容"分段切换；内容命中行（路径·标题·片段·过期标记）；`degraded`/`truncated` 常驻说明行 | T-37 T-39 | 1 |
| 主窗口检查器 | 改 `screens/main.js` | 索引状态行 + 加入/移出索引 + 查看抽取文本；"来自会话"链接；"被 Agent 修改"标记；"发送给 Agent"按钮 | T-36 T-37 T-38 T-42 | 1/2 |
| 浮层 `session_panel.js` | `openPanel` | 会话详情：manifest 概要、产物表、审计尾巴、操作表、回滚 | T-36 T-38 | 1（回滚 2） |
| 浮层"令牌揭示"（agents.js 内） | `openPanel` | 新令牌只显示一次 + Claude/Codex HTTP 注册片段 + `copyBtn` | T-35 | 1 |
| 浮层 `send_to_agent.js` | `openPanel` | 预填提示词、复制；已配置 agents 时选择并运行 | T-42 | 2 |
| 导航徽标 | 改 `app.js` | `#/agents` 显示活动会话数；`#/triggers` 显示 dead 投递数 | T-34 T-41 | 1/2 |
| 诊断屏 | 不改 JS | doctor 新检查项自动出现（index.db、嵌入端点、agent.db、stdio 非 owner） | T-37 T-39 T-43 | 1/2 |
| 首次设置完成页 | 改 `screens/setup.js` | "连接 Agent"卡片跳 `#/agents` 接入面板 | T-35 | 1 |

新导航项追加在 `router.js` 的 `routes` 与 `navItems` 中，放在 `#/exports` 之后、`#/storage` 之前：
`#/agents`、`#/index`、`#/triggers`。

### 6.2 新增控制面路由

全部登记进 `internal/control/metrics.go` 路由表，`security_all_routes_test.go` 自动覆盖：非 GET 必须带
`X-CloudFS-Control: 1`，并且只接受回环 Host 与同源 Origin。标"confirm"的路由请求体须带 `confirm: true`，
由 `confirmed()` 校验。

| 期 | 方法与路由 | 说明 | 条目 |
|---|---|---|---|
| 一期 A | `GET /sessions?cursor&state&sandbox&path` | 会话列表；`path` 反查改过该路径的会话 | T-34 T-36 T-38 |
| 一期 A | `GET /sessions/{id}` | 详情，含 artifacts；T-38 起含 ops | T-34 T-36 |
| 一期 A | `POST /sessions/{id}/finish` | 结束会话 | T-34 |
| 一期 A | `GET /audit?cursor&session&tool&result&since` | 审计分页 | T-34 |
| 一期 A | `GET /mcp/connect` | 传输状态、owner、`stdio_non_owner`、注册片段；不含令牌 | T-35 T-43 |
| 一期 A | `GET /mcp/tokens` | 令牌列表，只含指纹 | T-35 |
| 一期 A | `POST /mcp/tokens` | 新建；`Cache-Control: no-store`，不落日志 | T-35 |
| 一期 A | `POST /mcp/tokens/{id}/revoke` | confirm | T-35 |
| 一期 B | `GET /index/status` | 未启用时 `enabled:false` | T-37 |
| 一期 B | `GET /index/rules` | 规则与来源 | T-37 |
| 一期 B | `POST /index/add`、`POST /index/remove` | remove 仅界面来源规则，confirm | T-37 |
| 一期 B | `POST /index/rebuild` | confirm | T-37 |
| 一期 B | `POST /index/retry` | 失败文档重置为 dirty | T-37 |
| 一期 B | `GET /index/failed?cursor` | 失败文档分页 | T-37 |
| 一期 B | `GET /index/search?q&path&mode&limit` | 内容检索 | T-37 T-39 |
| 一期 B | `GET /index/text?path&offset&max_bytes` | 抽取文本分页 | T-37 |
| 二期 | `POST /sessions/{id}/rollback` | `{dry_run: true}` 预览；`{confirm: true}` 执行 | T-38 |
| 二期 | `GET /index/embedding`、`POST /index/embedding/check` | 端点状态；测试调用 | T-39 |
| 二期 | `GET /memory/agents`、`GET /memory/{agent}?cursor` | agent 列表与记忆列表 | T-40 |
| 二期 | `GET /memory/{agent}/{name}`、`PUT /memory/{agent}/{name}`、`DELETE /memory/{agent}/{name}` | PUT 带 `expected_version`；DELETE confirm | T-40 |
| 二期 | `GET /triggers`、`GET /triggers/deliveries?cursor&rule&state`、`GET /triggers/deliveries/{id}` | 规则只读、投递列表、投递详情 | T-41 |
| 二期 | `POST /triggers/test`、`POST /triggers/retry` | test 为 confirm | T-41 |
| 二期（复制可提前） | `GET /agent/prompt?path`、`GET /agent/endpoints`、`POST /agent/invoke` | invoke 为 confirm | T-42 |

复用的既有路由：`/fs/download-url`（产物复制链接）、`/fs/delete`（删除记忆冲突副本）、`/fs/preview`
（读冲突副本内容）、`/doctor/run`（新检查项）。**不新增任何写配置的路由**：触发器、agents、嵌入端点、
`memory.root` 都只在配置文件中修改。

### 6.3 SSE 事件

`/events` 新增四种事件，`internal/control/web/api.js` 的 `events({...})` 相应增加 `onAudit`、`onSession`、
`onIndex`、`onTrigger` 回调，退避重连与轮询回落逻辑不变。

| 事件 | 频率 | 载荷 | 条目 |
|---|---|---|---|
| `audit` | 每条审计一发，溢出同 change | 审计行（已脱敏） | T-34 |
| `session` | 会话状态变化 | `{id, state, writes}` | T-34 |
| `index` | 1 s 节流 | `{extracting, pending, failed, paused_reason?, resume_at?}` | T-37 |
| `trigger` | 投递状态变化 | `{id, rule, state, attempts}` | T-41 |

轮询回落时，Agent 屏与触发器屏各自在可见时每 5 s 刷新第一页，不在 `events()` 里加轮询。

### 6.4 界面安全边界（逐条可测）

1. **远端凭据与嵌入 `api_key` 永不进浏览器**。界面只显示 `next_command`（如 `cloudfs index auth`），沿用 `rejectSecretFields`；`ui_test` 的"嵌入字节中无 `type="password"`"断言不变。
2. **MCP 访问令牌是唯一"只显示一次"的例外**，写法同 `/fs/download-url` 的有意例外。
   - `POST /mcp/tokens` 响应 `Cache-Control: no-store`，不落日志。
   - 前端令牌只存在于揭示浮层的局部变量，关闭即丢，不进 `store.js`、不进 localStorage。
   - 之后 `GET /mcp/tokens` 只返回前 4 位指纹。
   - **待确认**：若用户要求更严，界面改为只显示 `cloudfs mcp token create …` 命令片段，揭示浮层与 `POST /mcp/tokens` 从控制面移除。
3. **触发器 exec 命令与 `agents:` 端点只能在配置文件定义**。界面只读展示 argv，不提供编辑，因为能在浏览器里定义命令就等于远程执行面；webhook secret 只显示"已配置"。
4. **签名直链只按需复制**，不渲染进长期停留的 DOM（沿用 ui-plan E5 做法）。
5. **破坏性动作双重确认**：回滚、令牌吊销、索引重建、索引规则移除、记忆删除、测试投递、运行 agent 都先走 `confirmDelete` 键入确认，服务端再要求 `confirm: true`。

另有两条内容渲染规则：片段、抽取文本、记忆正文、提示词、exec 输出全部来自文件或子进程，一律按
文本插入（`el()` 的 children），不走 `html` 属性；审计 `args` 展开为只读 JSON，服务端已脱敏，前端不再加工。

### 6.5 前端测试约定

- **每屏一个 `internal/control/ui_<feature>_test.go`**，照 `ui_copies_test.go` 的写法：断言模块被嵌入、调用了哪些路由、破坏性请求带 `confirm: true`、没有秘密字段。本路线图新增 `ui_agents_test.go`、`ui_tokens_test.go`、`ui_sessions_test.go`、`ui_index_test.go`、`ui_rollback_test.go`、`ui_embedding_test.go`、`ui_memory_test.go`、`ui_triggers_test.go`、`ui_send_to_agent_test.go`。
- **i18n**：`i18n.js` zh/en 两表键集合一致（`ui_i18n_test.go` 的 `TestWebCatalogsHaveTheSameKeys`），screens 内无汉字（`ui_i18n_test.go`）。新键前缀为 `agents.*`、`tokens.*`、`sessions.*`、`audit.*`、`index.*`、`memory.*`、`triggers.*`、`send.*`。
- **纯逻辑抽成零 import 模块**，放在 `web/` 根，测试放 `web/_tests/*.test.mjs`，由 `browser_modules_test.go` 用 `node --test` 驱动（无 node 时 skip）：

| 模块 | 输入 → 输出 | 条目 |
|---|---|---|
| `scope_view.js` | Scope → 可读摘要；"可写必须在可读内"校验 | T-34 T-35 |
| `rollback_plan.js` | dry-run 结果 → restored/skipped/conflict 分组；空计划 | T-38 |
| `snippet.js` | 片段 + 查询 → 高亮分段（纯数据，不产出 HTML）；CJK 截断不切半字 | T-37 |
| `memory_conflicts.js` | 同目录文件名列表 → 本体与副本配对（只按前缀与同目录，不猜 provider 命名） | T-40 |
| `trigger_view.js` | 规则 → 自激风险判断（exec 且 origins 含 api 且 paths 覆盖工作区或 cwd） | T-41 |

- **图标**：`bot`、`layers`、`bolt`、`undo` 加进 `icons.js`，`ui_icons_test.go` 覆盖。
- **浏览器冒烟**：`test/e2e` 的 `CLOUDFS_BROWSER=1` 冒烟增加 `#/agents`、`#/index`、`#/triggers` 可达，以及各条目 TODO 中列出的端到端链路。

### 6.6 共用件复用

新屏不复制模态框、分页或 SSE 逻辑：

| 需要 | 已有 |
|---|---|
| 表单弹窗、只读面板、自定义浮层 | `internal/control/web/ui.js` 的 `openForm`、`showPanel`、`openPanel` |
| 键入确认 | `ui.js` 的 `confirmDelete` |
| 表格"加载更多"、复制按钮 | `ui.js` 的 `moreRow`、`copyBtn` |
| 分页失败时保留已加载页 | `paged.js` 的 `pageCursor`、`pageFailureMode` |
| SSE 订阅与回落 | `api.js` 的 `events()` |
| 路由与导航 | `router.js` 的 `routes`、`navItems` |
| 服务端确认门、路由守卫 | `internal/control/shared.go` 的 `confirmed()`；`metrics.go` 路由表 |

---

## 7. 分期路线

### 7.1 一期（并行两条线，约 4–5 周，1 人 + agents）

每条后端任务之后紧跟对应界面任务；界面不落地，条目不关。

| 线 | 顺序 | 交付 | 界面 | 理由 | 状态 |
|---|---|---|---|---|---|
| A | T-34 | `internal/agent` 底座、agent.db、`Scope`、隐式会话、审计 middleware、`checkPath(ctx,p,write)` 重构、`/audit`、`/sessions`、CLI | F1 | 审计零风险、立刻有价值；Scope 重构是机械替换，被现有测试兜底 | 一期已实现（2026-09-15，TODO.md 已关） |
| A | T-35 | 令牌 principal、读写分离、过期、`mcp install --transport http`、`/mcp/*` | F2 | HTTP 注册让"单 owner 进程"成为默认拓扑 | 一期已实现（2026-09-15，TODO.md 已关） |
| A | T-36 | `begin/finish/list_sessions`、工作区 manifest、sandbox | F3 | 有了会话与 scope，交付箱几乎只是约定 | 一期已实现（2026-09-15，TODO.md 已关） |
| B | T-44 | Everything 式文件名搜索：后台全树爬取器 + 覆盖率、`SearchResult` 加 size/mtime/kind、`ext:/size:/dm:/type:` 与通配、排序、MCP／控制面／CLI 参数、百万节点基线 | F11 | 2026-09-15 追加。引擎已是 Everything 形态（父链拼路径、trigram、预算），缺的是覆盖率与体验；T-37 的"文件名 / 内容"切换建在它改造过的搜索框上，所以前置 | 一期已实现（2026-09-15） |
| B | T-37 | textract（文本 → Office → PDF → 分块）、index.db v1、Indexer、FTS 检索、5 个 MCP 工具、`/index/*`、CLI、metrics、doctor | F4 | 独立于线 A，只依赖 vfs 与 meta；界面在 T-44 之后 | 一期已实现（2026-09-15） |
| 可选 | T-42 前半 | `GET /agent/prompt` + 检查器"复制提示词" | F9 前半 | 无 exec 依赖 | 未开始（可选） |

**一期总验收**：

- 检查命令：`./gow test ./...`（在无 macFUSE 的机器上排除 `./internal/fusefs`、`./test/conformance`、`./test/e2e`）全绿，`./gow vet` 干净，改动包 `-race` 无告警。
- 现有 `internal/mcpsrv`、`internal/control`、`internal/vfs` 测试零修改通过。
- `test/perf` 的 provider 调用次数基线不变。
- `test/e2e` 新增一条完整链路：
  1. 用 HTTP 令牌建会话，`begin_session(sandbox)`。
  2. 在工作区写两个文件；一次越界写被 denied。
  3. `finish_session`。
  4. 浏览器冒烟确认审计 denied 行与产物表两行都可见。
  （已落地：`test/e2e/agent_e2e_test.go` `TestAgentTokenSandboxChainAndAuditInTheBrowser`、`TestAgentSessionManifestMatchesTheMount`）
- 另一条链路：写 md 后 3 s 内 `semantic_search` 命中，浏览器冒烟里主窗口内容搜索命中同一文件。
  （已落地：`test/e2e/index_e2e_test.go` `TestContentSearchFindsAFreshMarkdownFile`（真实 FUSE 挂载）、`TestContentSearchInTheBrowser`（`CLOUDFS_BROWSER=1`）；chaos：`test/chaos/index_test.go` `TestIndexSurvivesAnUncleanStopMidExtraction`、`TestMaliciousArchiveFailsTheDocumentNotTheDaemon`）

### 7.2 二期（约 4–5 周）

| 顺序 | 条目 | 依赖 | 界面 | 状态（2026-09-15） |
|---|---|---|---|---|
| 1 | T-43 核实 stdio 与 mount 并存行为 | 无；结论决定 stdio→HTTP 桥是否提前 | F10 | **验证与栅栏已交付，条目保持开放**：e2e `TestStdioBesideMountRefusesWritesCleanly`（并存下 stdio 写一律在碰 meta/journal 前被拒）、doctor `agent_stdio` warn、接入面板横幅、F10 全勾；结论是**桥提前到三期首位**（§4.5、§7.3） |
| 2 | T-38 快照与回滚 | T-34、T-43 | F5 | **完成**（线 C：0bfea3d 后端、cfe49b6 界面、收口提交 chaos/e2e）：`session_ops` + 前像 + 逆序回滚 + dry_run + 冲突 + 保留期 GC；MCP/控制面/CLI 三入口；F5 全勾；证据见 TODO.md T-38"验收证明"。顺带修了 fusefs 失效通知用 VFS ino 当内核 nodeid 的错位（`kernel_nodes.go`） |
| 3 | T-39 嵌入与 hybrid | T-37 | F6 | **已实现（2026-09-15，线 D）**：`internal/embed`（openai / ollama / fake，限流 + breaker + 远端门）、index.db v2 `vectors` + `embed_pending`、嵌入 worker、RRF hybrid 诚实降级、`/index/embedding` + `check`、doctor、`cloudfs index auth |
| 4 | T-40 记忆库 | T-37（keyword 即可先上），T-39 可选 | F7 | **已实现（2026-09-15，线 D）**：`internal/memory` 一份实现供 5 个 `memory_*` 工具、`/memory/*` 路由与 `cloudfs memory` 共用，内置索引规则跟随 `memory.root`，F7 全勾；顺带修了冲突输家节点被列举一直保护的 VFS 缺口；证据见 TODO.md T-40"验收证明"，e2e `TestMemoryPutIsVisibleInTheMountAndSearchable`、浏览器 `TestMemoryTabInTheBrowser` |
| 5 | T-41 触发器 | 无（vfs 字段 + 引擎） | F8 | **完成**（线 E，TODO.md 已关）：chaos `TestDeliveryRunningAtCrashIsRedelivered`/`TestStormUnderOverflowDeliversOneRescan`，e2e `TestKernelWriteFiresAnExecTrigger`/`TestMCPWriteDoesNotFireWhenAPIIsExcluded`/`TestTriggersScreenInTheBrowser` |
| 6 | T-42 发送给 Agent（运行） | T-41 exec 执行器 | F9 | **完成**（线 E，TODO.md 已关）：`/agent/endpoints`、`/agent/invoke`、运行按钮 |

T-38/T-39/T-41 三条彼此独立，可以并行给不同 agent 做。

### 7.3 三期（按需）

- **stdio→HTTP 桥（T-43 结论：并存有数据风险，C0.5 已用写栅栏封住；桥排在三期其他条目之前，落地后关闭 T-43）**。
- 递归删除目录的逐文件前像，按文件数与字节预算记录。
- 控制台 `/fs/*` 与 WebDAV 操作进审计。
- `_meta["cloudfs/session"]` 显式会话。
- `pull_events` MCP 工具。
- 纯 Go HNSW，仅在实测 `max_chunks` 规模下 p95 检索 > 200 ms 时实施。
- 团队场景：principal 的 `owner` 字段。
- 多选文件发送给 Agent。

---

## 8. 风险

| 风险 | 影响 | 应对 | 条目 |
|---|---|---|---|
| 索引 `rules` 持续下载触发网盘风控 | 账号被限速或封禁 | 共用 remote 三维限流与熔断；`fetch_budget`；`Tier=unofficial` 减半；`ErrRiskControl` 休眠 15 min；默认不索引，推荐 `pinned` | T-37 |
| 嵌入把文件内容发往第三方 | 隐私泄露 | 默认 `provider: none`；`allow_remote` 显式开启；全局 exclude 默认含密钥类文件；状态与界面常驻 `remote=true` 横幅 | T-39 |
| 触发器 exec 写回被监视目录导致自激 | 无限循环、资源耗尽 | 去抖合并；`origins` 可排除 api；`Validate()` warning；界面自激标记；每规则串行 worker | T-41 |
| 触发器 exec 成为执行面 | 本机任意命令执行 | 无 shell、argv 元素替换、命令只能在配置文件定义、界面只读、测试投递键入确认、进程组超时 | T-41 T-42 |
| `Scope.Narrow`/`Check` 实现错误 | 越权读写 | 纯函数表驱动穷举；遍历 `tools/list` 逐一过 `checkPath` 的表测试；`Options.Sessions == nil` 时行为同今天 | T-34 T-35 |
| 无状态 HTTP 会话靠空闲轮转 | 审计中同一令牌出现多个会话，归属不精确 | 文档明说；二期 `_meta` 显式会话 | T-34 |
| stdio 与 mount 并存时是独立 VFS | 会话与审计不共享；上传行为未核实，可能丢失或复活 | 推荐 HTTP；非 owner 禁用会话工具；doctor warn；T-43 先核实再做回滚 | T-35 T-43 |
| 前像留不住（未读满、超大、递归删除） | 回滚不完整 | 如实记 `pre_reason`；预览浮层列出跳过项；回滚按 version 比对，不覆盖他人修改 | T-38 |
| 前像占用磁盘 | 缓存预算被挤占 | `ReserveDisk` 记账；`max_preimage_bytes`；`retain` 7 天 GC | T-38 |
| 令牌明文在浏览器出现一次 | 截图、扩展或共享屏幕泄露 | no-store、不进 store/localStorage、关闭即丢；待确认是否改为只给命令 | T-35 |
| PDF 抽取质量（CJK 乱码、panic、扫描件） | 检索命中差、daemon 崩溃 | recover + 超时 + 乱码启发式；失败文档可见；`UNVERIFIED` 标注 | T-37 |
| 恶意 Office/zip 文档 | 内存或 CPU 耗尽 | 条目、单条与总量上限；超时；失败入表 | T-37 |
| 向量常驻内存 | 大库内存占用高 | int8 默认；`max_chunks` 硬上限；doctor 余量告警 | T-39 |
| 检索结果陈旧 | agent 基于旧内容行动 | hit 带 `stale`；不承诺全库最新 | T-37 |
| meta 重建导致 ino 失效 | 索引全部错位 | `meta_store_identity` 比对，不一致整库重建（保留抽取文本） | T-37 |
| 记忆 `expected_version` 非原子 | 毫秒级窗口内覆盖 | 单机窗口小；跨设备由网盘冲突副本兜底；冲突合并浮层 | T-40 |
| 审计写失败 | 记录缺失 | 不阻塞工具；失败计数指标；doctor 检查 agent.db | T-34 |

---

## 9. 与既有文档的关系

| 文档 | 关系 | 本规划落地时要改什么 |
|---|---|---|
| [TODO.md](../TODO.md) P4 | 逐条证据、做法与**验收断言的权威来源**；本文只引用条目号 | 每完成一条勾选并写落地说明 |
| [界面计划](ui-plan.md) 阶段 F | 界面侧逐项勾选清单，F1–F10 与 T-34 ~ T-43 一一对应 | 界面落地时勾选 |
| [设计](DESIGN.md) §4.7、§4.8、§4.12–§4.14、§9 | 已加交叉引用、规划中小节与风险行 | 实现后把 §4.12–§4.14 由"规划中"改为正式设计，并补 §6 代码结构 |
| [MCP](mcp.md) | 顶部"规划中"说明；"与挂载并存"补注拓扑限制 | 工具实现后才进工具表；补"会话与作用域""回滚承诺""`semantic_search` 只覆盖索引范围，先看 `index_status.covered`" |
| [VFS 变更](vfs-changes.md) | Change 与审计的区别 | T-34 顶部加一行区别；T-41 补 `Kind`/`Origin` |
| [缓存管理](cache-management.md) | pin 规则语义被索引与规则来源复用；前像占用缓存预算 | T-38 补前像记账说明 |
| [上传清理](upload-cleanup.md)、[复制](copy.md) | 回滚产生的写入走同一上传与冲突路径 | 无需修改 |
| [WebDAV 输出](webdav-output.md) | `finish_session` 可附 DAV URL；WebDAV 操作不进会话 | 三期审计扩展时更新 |

### 9.1 本文对草案分歧的取舍

写本文时对两份详细设计草案与已批准规划做了统一，记录如下，避免实现时再争论：

- **回滚放在二期并支持 dry_run**。草案曾排在一期第 3 周、路由 `POST /sessions/rollback`；本文以规划为准：二期，先做 T-43，路由 `POST /sessions/{id}/rollback`，带 `dry_run`。
- **触发器放在二期**，不作为一期第 4 周的弹性项。
- **索引界面在一期交付完整 `#/index` 屏**。草案"一期只加状态卡、页面留到 phase 2 之后"与"每项必须带界面"的纪律冲突，以纪律为准。
- **记忆库放在二期**，不是草案的 phase 3。
- **agent.db 的 flock 只保护 owner 专属工作**。草案一边写"flock 仿 journal"，一边写"非 owner 审计仍写"，两者矛盾。本文规定 `owner.lock` 只保护 GC、回滚与触发器 worker，审计行由任何进程追加。
- **`GET /sessions` 增加 `path` 与 `sandbox` 参数**。规划的路由清单漏了它们，而检查器"来自会话""被 Agent 修改"与"仅 sandbox"过滤都需要。
- **冲突副本的删除与读取复用 `/fs/delete`、`/fs/preview`**。副本文件名不满足记忆 `name` 的正则，`DELETE /memory/{agent}/{name}` 无法表达，所以不新增记忆副本路由。
- **`memory.root` 默认等于 `mcp.workspace`**，只保留一个 `.agent` 目录。草案中"第一个可写 layout 前缀"与"第一个 allow 前缀"两种默认值合并为后者。
- **HTTP 传输下的 `<client>` 与 `<agent>` 取令牌 principal 名称**。无状态传输不能依赖每个 POST 的 `ClientInfo`。
- **webhook 时间戳格式定为 Unix 秒（十进制）**。草案未指定。
