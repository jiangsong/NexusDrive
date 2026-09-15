# Agent 工作底座二期（T-43、T-38、T-39、T-40、T-41、T-42 后半）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 三条互不阻塞的线并行交付二期：线 C（T-43 并存验证 → T-38 会话快照与回滚）、线 D（T-39 嵌入与 hybrid → T-40 记忆库）、线 E（T-41 触发器 → T-42 "运行"按钮）。每条后端能力都带同期控制台界面与同一验收。一期（`feat/agent-phase1`，HEAD `09e0014`）已全部完成，本计划只写二期。

**Architecture:** 不改 meta v12 / journal v13 语义。`internal/agent` 升到 agent.db v2（`session_ops`、`trigger_deliveries` 两表，P0 统一加）；`internal/index` 升到 index.db v2（`vectors`、`embed_pending`，只加表）；新包 `internal/embed`（嵌入端点客户端）、`internal/memory`（记忆库：VFS + index 的薄封装，MCP 与控制面共用）、`internal/trigger`（投递引擎 + exec/webhook 执行器，仅 owner 进程）。`internal/vfs` 唯一改动是 `Change.Kind/Origin`（E0）。mcpsrv 加 `rollback_session` 与 5 个 `memory_*` 工具；控制面路由全部进 `metrics.go` 的 `routes()`；控制台加 `#/triggers` 屏、Agent 屏记忆标签、索引屏嵌入面板、会话详情操作表与回滚浮层。

**Tech Stack:** 同一期（Go 1.27、`modernc.org/sqlite`、go-sdk v1.7.0、原生 ES module 控制台 + Node v22 `node --test`、CDP 驱动的 headless Chromium 冒烟 `CLOUDFS_BROWSER=1`）。**不引入新 Go 依赖**：glob 复用 `internal/index/glob.go`（`**`），HMAC 用标准库，向量检索用暴力 cosine。

**Spec:**
- 验收真相源：`TODO.md` P4 节 T-38 / T-39 / T-40 / T-41 / T-42（后半）/ T-43。设计若与 TODO 验收冲突，以 TODO 为准。
- 界面清单：`docs/ui-plan.md` 阶段 F 的 F5–F10；`docs/agent-roadmap.md` §6.2 路由表、§6.3 SSE、§6.4 安全边界。
- 详细设计：`docs/agent-roadmap.md` §3.5–3.7（嵌入与检索）、§3.11（记忆库）、§4.3（agent.db v2 两表）、§4.5（stdio 拓扑）、§4.8（回滚）、§5（触发器与发送给 Agent）。

## Global Constraints（沿用一期，摘要）

- 工具链只用 `./gow`；`./gow build ./... && ./gow vet ./...` 基线干净。本机 Linux 有 `/dev/fuse`，`./gow test ./...` 不排除任何包；skip 不等于通过。
- 每个任务 GREEN 后对改动包跑 `-race`；计时类用例经 `internal/testx.RaceEnabled` 在 race 下跳过（不是删掉断言）。
- 控制台：文案经 `t()`，`i18n.js` 两表键集合一致；非 `i18n.js` 文件无汉字；来自文件/远端/审计/投递输出的文本一律走文本节点，永不进 `html:`；图标名必须在 `icons.js`；`confirmDelete` 只用对象签名；单文件 < 800 行（`i18n.js` 例外，当前 756 行——**本期 i18n 键多，先把 `i18n.js` 拆成 `i18n.js`（加载器）+ `i18n_zh.js` + `i18n_en.js`，见 P0**）。
- 控制面：新路由登记 `routes()`；handler 首行 `privateRequest(w, r)` + `allowMethod`；破坏性动作 `confirmed(w, r, q.Confirm, "confirm.<key>", args...)`，文案键进 `internal/i18n/catalog_zh.go` 与 `catalog_en.go`；**不新增任何写配置的路由**（触发器、agents、嵌入端点、`memory.root` 只在配置文件改）。
- 分层：`internal/agent`、`internal/index`、`internal/embed`、`internal/memory`、`internal/trigger` 不被 `vfs` 引用；mcpsrv/control 保持薄适配。
- 永不按网盘名字分支；本机无法验证处加 `// UNVERIFIED: <要验证什么>`。
- 新 SQLite 表加进既有库（agent.db / index.db），DSN 与迁移套路照 `internal/agent/db.go`、`internal/index/store.go`；比本构建新的 schema 拒绝打开。
- 代码与注释英文；`docs/`、`TODO.md`、`README.md` 中文。
- 每个任务一个 conventional commit；结尾 `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>` 与 `Claude-Session: https://claude.ai/code/session_01WSCzccKwVPZeA3S1xkd5ti`。永不 `git add -A`（`gow` 是 skip-worktree 的本机改动）。
- UI 任务与后端任务同属一个 TODO 条目，UI 不落地不关条目；只有各线收口任务（C3、D7、E6）改 `TODO.md` 状态。

---

## 分支与合并卫生

- 起点：`feat/agent-phase1`（`09e0014`）。P0 直接在 `feat/agent-phase1` 上提交，**之后**再开三个 worktree：
  ```sh
  git worktree add ../fs-line-c -b feat/agent-line-c feat/agent-phase1
  git worktree add ../fs-line-d -b feat/agent-line-d feat/agent-phase1
  git worktree add ../fs-line-e -b feat/agent-line-e feat/agent-phase1
  ```
  每个 worktree 复制 `gow`（`SP=` 指向本会话 scratchpad）并 `git update-index --skip-worktree gow`。
- 合入顺序：**C → E → D**（改动面从小到大；D 的 `server.go`/`index` 改动最多，最后 rebase）。每线合入前 rebase 到最新 `feat/agent-phase1`，ff 合并。
- 同一 worktree 内**必须等上一个 subagent 的完成通知再派下一个**（一期教训：并行两个 agent 共享工作树导致 stash 互相干扰）。
- **交叉热点与协议**：

  | 文件 | 线 C | 线 D | 线 E | 协议 |
  |---|---|---|---|---|
  | `internal/agent/db.go` schema | — | — | — | **P0 已升 v2 加两表**，三线不再改 schema；线 C 只加 `session_ops` 的 DAO（`ops.go`），线 E 只加 `trigger_deliveries` 的 DAO（`deliveries.go`） |
  | `internal/mcpsrv/server.go` | C1 `rollback_session` 注册 + 写工具前像钩子（`preimage.go` 新文件，`server.go` 只在 6 个写工具各插一行调用） | D4 `registerMemoryTools`（`memory_tools.go` 新文件，`server.go` 只加 `Options.Memory` 与一处注册） | — | 新代码放新文件；`server.go` 相邻行冲突保留两边 |
  | `internal/mcpsrv/scope_tools_test.go` `pathless` 表 | `rollback_session` | `memory_list/get/put/delete/search`（走 `checkPath`，进"带路径表"） | — | 各自追加一行 |
  | `internal/control/metrics.go` `routes()` | `/sessions/{id}/rollback`（已有 `/sessions/` 前缀 handler，内部分发） | `/index/embedding`、`/index/embedding/check`、`/memory/agents`、`/memory/` | `/triggers`、`/triggers/deliveries`、`/triggers/deliveries/`、`/triggers/test`、`/triggers/retry`、`/agent/endpoints`、`/agent/invoke` | 各自追加表尾，冲突保留两边 |
  | `internal/control/status.go` `Collector` | — | `Memory *memory.Store` | `Trigger *trigger.Engine` | 追加字段 |
  | `internal/control/events.go` | — | — | `trigger` 事件 case | 只 E 改 |
  | `internal/control/doctor.go` | C0 `checkAgent`（agent.db 可开、schema、stdio 并存） | D2 `checkEmbedding` | E3 `checkTriggers`（配置 warning） | 各加一个函数，`Run` 里相邻行保留两边 |
  | `internal/config/config.go` | C1 `MCPSession.Retain`、`MaxPreimageBytes` | D0 `Index.Embedding`、`Index.MaxChunks`；D4 `Config.Memory` | E1 `Config.Triggers`、`Config.Agents`、`Config.Warnings` | 不同结构体/字段；`Validate` 相邻行保留两边 |
  | `internal/daemon/daemon.go` | C1 `agent.Recover` 孤儿 blob 清理 + 前像 GC goroutine | D1 embed worker 装配；D4 `d.Memory` | E2 `d.Trigger`（owner 才启动） | 位置不同 |
  | `cmd/cloudfs/main.go` / `agent.go` / `index.go` | C1 `sessions rollback` | D2 `index auth`、`index embedding`；D5 `memory` | E3 `triggers` 分发 | `mcpsrv.Options` 两处（stdio 与 HTTP）D 加 `Memory` |
  | `web/i18n_zh.js` / `web/i18n_en.js` | `rollback.*` | `embedding.*`、`memory.*` | `triggers.*`、`agent.run.*` | 各自追加表尾 |
  | `web/screens/agents.js` | C2 会话标签回滚入口（`agents_sessions.js`） | D6 记忆标签（`agents_memory.js` 新文件，`agents.js` 只加一个标签） | — | 追加式 |
  | `web/screens/main.js` | C2 检查器"被 Agent 修改"（新模块 `agent_touch.js`，`main.js` 一行接线） | — | E5 `send_to_agent.js` 内部加运行按钮（不动 `main.js`） | — |
  | `web/router.js`、`web/icons.js`、`web/api.js` | — | — | E4 `#/triggers`、`onTrigger` | **图标 `undo`/`bolt` 与 `api.js` 的 `onTrigger` 在 P0 统一加**，三线不再改这三个文件的共享行 |
  | `test/e2e/*` | `coexist_e2e_test.go`、`rollback_e2e_test.go` | `memory_e2e_test.go` | `trigger_e2e_test.go` | 各自新文件；浏览器冒烟复用 `browser_helper_test.go`，**不改它** |

---

## 代码锚点（HEAD `09e0014`，2026-09-15 核对）

| 锚点 | 实际 |
|---|---|
| agent.db | `internal/agent/db.go` `schemaVersion = 1`，`migrate()` 在事务里执行 `schemaSQL` 并写 `meta(k,v)` + `PRAGMA user_version`；`Store.Watch()` 发布 `Event{Kind: audit\|session}` |
| 会话 | `internal/agent/session.go` `Sessions.Begin/Finish/FinishWith/Get/List/WriteCounts/Summary`，`Session.State` 为 `active\|finished\|expired`（**加 `rolled_back`**）；`WithSession/FromContext` |
| mcpsrv 写工具 | `server.go` `writeFile` 792、`editFile` 826、`mkdir` 885、`move` 926、`copyFile` 951、`deletePath` 972；全部先 `s.checkPath(ctx, p, true)` 再调 `FS.WriteFile/Mkdir/Rename/Remove/Copy` |
| 工具遍历测试 | `internal/mcpsrv/scope_tools_test.go` `TestEveryToolChecksItsPaths`，`pathless` 表在 160 行 |
| VFS 前像所需 API | `FS.ReadFileRange(ctx, p, 0, 0)` 读满；`FS.Meta().Resolve(ctx, p)` 得 `meta.Node{Remote, RemoteID, Version, Size}`；`FS.Cache().HydratedPath(cache.FileKey{Remote, RemoteID, Version})` 得已水合文件路径；`FS.Cache().ReserveDisk(dir, n)` 记账；`FS.StatPath` 得 `Attr{IsDir, Size, Version, LocalOnly}` |
| VFS 变更流 | `internal/vfs/changes.go` `Change{Paths, Subtree, Rescan}`；helper `changedNode`（write.go 417/1027/1060/1148、vfs.go 909、copy_jobs.go 271、publication.go 130、refresh.go 250/289）、`changedEntry`（write.go 617/708/788/1212、copy_reconcile.go 117、refresh.go 220/287、upload_cleanup.go 117）、`changedRename`（write.go 801）、`changedListing`（vfs.go 920）；`fromKernel(ctx)` vfs.go 1020 |
| index.db | `internal/index/schema.go` `schemaVersion = 1`；`search.go` `SearchQuery.Mode`（`""/keyword` 跑，`hybrid/vector` 置 `Degraded = DegradedNoEmbedding`）、`Store.Search(ctx, q, current)`、`SearchResult{Hits, ModeUsed, Degraded, Truncated}`；`service.go` `Indexer.Search/Status/Rules/Rebuild`；`worker.go` 抽取 worker |
| 控制面 | `metrics.go` `routes()` 73–140；`shared.go` `writeJSON` 18、`confirmed` 33、`allowMethod` 60；`uploads.go` `privateRequest` 167；`doctor.go` `Doctor` 53、`Run` 93（无 agent.db 检查，C0 加）；`status.go` `Collector`（`Config`、`FS`、`Agent`、`Index`…）；`events.go` SSE 已有 `audit/session/index` |
| 配置 | `config.go` `MCP` 180（`Audit`、`Session{Idle}`、`Workspace`）、`Index` 351、`Config` 496；`secrets.go` `IsSecretField` 39（**加 `api_key`、`secret`**）、`SecretStore.Put(key, value)` 108 |
| daemon | `daemon.go` `Daemon{Agent, Sessions, Index, Export, Refresher, Journal, FS}`；agent.db 在 336 打开（非 owner 早退之前）；index 在 296 打开 |
| CLI | `main.go` 分发 `doctor` 104、`mcp` 116、`audit` 140、`sessions` 142、`index` 144；`cmdMCP` 639，非 owner 警告 705–707；`cmd/cloudfs/agent.go` `runSessions` 131；`cmd/cloudfs/index.go` `runIndex` 66 |
| e2e | `test/e2e/e2e_test.go` `newStack(t, mode)`（真实挂载 + in-memory MCP）、`settle`、`callTool`；`agent_e2e_test.go`、`index_e2e_test.go` `withIndex`；`browser_helper_test.go`（CDP，`CLOUDFS_BROWSER=1`） |
| 控制台 | `router.js` `routes`/`navItems`（`#/agents` bot、`#/index` layers）；`api.js` `events({onStatus,onChange,onExport,onAudit,onSession,onIndex})`；`screens/agents.js` 标签容器（`agents_sessions.js`、`agents_audit.js`、`agents_tokens.js`）；`session_panel.js` 会话详情；`send_to_agent.js` 47 行；`screens/index.js` 310 行；`screens/main.js` 480 行 |
| 已知 http-legacy 幽灵会话、stdio 会话不 finish | 一期遗留（TODO T-34/T-35"遗留"），C0 顺手修 stdio：进程退出时 `Finish` 当前会话 |

---

## 文件结构总览

```
internal/agent/db.go            schema v2：session_ops、trigger_deliveries（P0）
internal/agent/ops.go           session_ops DAO：RecordOp/CompleteOp/OpsOf/OpsByPath（C1）
internal/agent/preimage.go      前像捕获、blob 目录、Recover 孤儿清理、GC（C1）
internal/agent/rollback.go      Rollback(ctx, fs, id, dryRun) → Plan（C1）
internal/agent/deliveries.go    trigger_deliveries DAO：Enqueue/Claim/Done/Fail/Retry/List/Get/ResetRunning（E2）
internal/mcpsrv/preimage.go     写工具前像钩子 + rollback_session 工具（C1）
internal/mcpsrv/memory_tools.go 5 个 memory_* 工具（D4）
internal/embed/{embed.go,openai.go,ollama.go,fake.go}   Embedder + 限流/熔断（D0）
internal/index/{schema.go v2, vectors.go, embed_worker.go, hybrid.go}（D1）
internal/memory/{memory.go, frontmatter.go, conflicts.go}（D4）
internal/vfs/changes.go         Change.Kind/Origin、WithOrigin（E0）
internal/trigger/{engine.go, match.go, exec.go, webhook.go}（E2）
internal/config/{config.go 各线字段, triggers.go(E1), memory.go(D4), embedding.go(D0)}
internal/control/{sessions_rollback.go(C1), embedding.go(D2), memory.go(D5), triggers.go(E3), agent_invoke.go(E5)}
internal/control/web/i18n.js + i18n_zh.js + i18n_en.js（P0 拆分）
internal/control/web/{rollback_plan.js, agent_touch.js}（C2）、{memory_conflicts.js, memory_panel.js}（D6）、{trigger_view.js, delivery_panel.js}（E4）
internal/control/web/screens/{agents_memory.js(D6), triggers.js(E4)}
test/e2e/{coexist_e2e_test.go(C0), rollback_e2e_test.go(C3), memory_e2e_test.go(D7), trigger_e2e_test.go(E6)}
test/chaos/{rollback_chaos_test.go(C3), trigger_chaos_test.go(E6)}
test/perf/embed_perf_test.go（D1）
```

---

## Task P0：共享前置（在 `feat/agent-phase1` 上，开 worktree 之前）

**Files:**
- Modify: `internal/agent/db.go`（`schemaVersion = 2`，`schemaSQL` 加 `session_ops`、`trigger_deliveries` 两表与索引，照 roadmap §4.3）
- Modify: `internal/agent/db_test.go`
- Modify: `internal/control/web/icons.js`（加 `undo`、`bolt`）、`internal/control/web/api.js`（`events` 加 `onTrigger`，`addEventListener('trigger', …)`）
- Split: `internal/control/web/i18n.js` → `i18n.js`（`t()`、`lang`、导入两表）+ `i18n_zh.js`（`export const zh = {...}`）+ `i18n_en.js`；`ui_i18n_test.go` 与 `_tests/i18n.test.mjs` 改为从两个文件读表（**键集合一致**断言不变；"非 i18n 文件无汉字"的豁免列表加 `i18n_zh.js`）
- Modify: `docs/superpowers/plans/2026-09-15-agent-phase2-implementation.md`（本文件已就位）

**Steps:**
- [ ] RED：`TestSchemaV2HasSessionOpsAndDeliveries`（打开新库 `user_version == 2`，两表可插入）、`TestOlderDatabaseMigratesToV2`（v1 库打开后升级，旧 `audit` 行仍在）、`TestNewerSchemaIsRefused` 仍通过
- [ ] GREEN：`./gow test ./internal/agent/ -count=1`
- [ ] 图标与 `onTrigger`：`./gow test ./internal/control/ -run 'TestWebIcons|TestBrowserModule' -count=1`
- [ ] i18n 拆分：`./gow test ./internal/control/ -run 'TestWeb|TestEveryTranslation|TestBrowserModule' -count=1 && node --test internal/control/web/_tests/*.test.mjs`；`wc -l internal/control/web/i18n*.js` 每个 < 800
- [ ] 提交：`feat(agent,ui): agent.db v2 tables and the shared console hooks phase two builds on`

---

# 线 C：T-43 并存验证 → T-38 会话快照与回滚

## Task C0：T-43 — stdio 与 mount 并存的 e2e、doctor 检查、接入面板横幅（F10）

**Files:**
- Create: `test/e2e/coexist_e2e_test.go`
- Modify: `cmd/cloudfs/main.go`（`cmdMCP` 非 owner 时写心跳文件 `<cache>/agent/stdio-<pid>.hb`，每 30 s touch，退出删除；退出前 `Finish` 自己的会话）
- Create: `internal/agent/heartbeat.go`（`WriteHeartbeat(dir, pid)`, `LiveStdioProcesses(dir, now) []int`——mtime 超过 2 min 视为陈旧并删除）
- Modify: `internal/control/doctor.go`（`checkAgent(ctx)`：agent.db 可打开、schema == 本构建、"MCP stdio 进程与挂载并存" warn，`Fix` 文本 `cloudfs mcp install --transport http`），`internal/i18n/catalog_{zh,en}.go` 加 `doctor.agent.*` 键
- Modify: `internal/control/agent.go` `/mcp/connect` 响应加 `stdio_non_owner: bool`
- Modify: `internal/control/web/connect_panel.js`（横幅，`t('connect.stdio_warning')`，链接 `#/diagnostics`）、`ui_agents_test.go`
- Modify: `docs/mcp.md`"与挂载并存"一节；`TODO.md` T-43 写结论（**通过或失败都写**）

**Steps:**
- [ ] RED e2e：`TestStdioBesideMountSharesWrites`——`newStack` 之外再 `daemon.Open` 同一 cacheDir（同进程第二个 fd 拿不到 flock → 非 owner），用 `mcpsrv.New(Options{FS: d2.FS, NonOwner: true, Sessions: d2.Sessions})` in-memory 连接；三个断言：(a) stdio 侧 `write_file /demo/side.txt` 后 owner 侧 `cat mnt/demo/side.txt` 在 5 s 内可见且内容一致；(b) owner 侧 `settle` 后 fake provider 有该文件且 `Calls("Upload") == 1`（无重复上传）；(c) 关闭 d2、重开 owner 后文件不"复活/丢失"。任何一条失败即把现象逐字写进 TODO T-43，测试用 `t.Skip` **不允许**——失败就保留红，作为三期 stdio→HTTP 桥提前的证据，并在计划末尾"结论"处记录
- [ ] RED doctor：`TestDoctorWarnsWhenStdioRunsBesideTheMount`（写一个心跳文件 → warn；无心跳 → ok；陈旧心跳 → ok 且文件被清理）、`TestDoctorReportsAgentDB`
- [ ] RED UI：`TestConnectPanelWarnsAboutStdioNonOwner`（`stdio_non_owner:true` → 横幅渲染且 `href="#/diagnostics"`；false → 无）
- [ ] GREEN：`./gow test ./internal/agent/ ./internal/control/ ./cmd/cloudfs/ -count=1 && ./gow test ./test/e2e/ -run 'TestStdioBesideMount|TestDoctorOnALiveSystem' -count=1 -v`
- [ ] 提交：`feat(agent,control): verify stdio MCP beside a mount and warn about it in doctor and the console`

## Task C1：T-38 后端 — `session_ops`、前像、回滚、MCP/控制面/CLI

**Files:**
- Modify: `internal/config/config.go`（`MCPSession` 加 `Retain time.Duration`（默认 7 d）、`MaxPreimageBytes Size`（默认 32 MiB），`validateAgent` 填默认与拒负值）
- Create: `internal/agent/ops.go`（`Op` 行类型；`RecordOp(ctx, sessionID, auditID, op Op) (seq int64, err)`；`CompleteOp(ctx, seq, postVersion)`；`OpsOf(ctx, sessionID) []Op`；`SessionsTouching(ctx, path, since) []Session`——供 `GET /sessions?path=`）
- Create: `internal/agent/preimage.go`（`Preimages{dir, cache}`：`Capture(ctx, fs, p) Pre`——`StatPath` → absent/dir/file；文件 ≤ max 时 `ReadFileRange(p,0,0)` 读满 → sha256 → 优先 `os.Link(cache.HydratedPath(key), blob)`，失败则把已读字节写入 `blob.tmp` + rename（**读成功就一定有前像**，`not_cached` 只在读失败时）；`ReserveDisk` 记账；`Recover(ctx)` 删除没有任何 `session_ops.pre_blob` 引用的 blob；`GC(ctx, retain)` 删已 finished/expired/rolled_back 且超保留期的会话的 blob 与行）
- Create: `internal/agent/rollback.go`（`Plan{Restored, Skipped, Conflict []PlanItem}`；`Rollback(ctx, fs FSOps, id string, dryRun bool) (Plan, Session, error)`：按 `seq` 逆序，前置检查表照 roadmap §4.8；执行时开新会话 `name="rollback of <id>"` 并对每步同样 `Capture`；结束把原会话 `state=rolled_back`、每行 `rollback_result`）。`FSOps` 是本包定义的最小接口（`StatPath/ReadFileRange/WriteFile/Mkdir/Remove/Rename`），测试用 fake，避免 agent 依赖 vfs 之外的重型装配（**agent 可以 import vfs 的类型**，但接口让单测无需真 VFS）
- Create: `internal/mcpsrv/preimage.go`（`s.beforeWrite(ctx, op, p, to) func(post string)`：会话存在且 `Options.Preimages != nil` 时捕获；6 个写工具各插一行：`done := s.beforeWrite(...)` / 成功后 `done(attr.Version)`；`rollback_session{session_id, confirm, dry_run?}` 工具，`DestructiveHint`，非 owner 返回 `errRequiresOwner`）
- Modify: `internal/mcpsrv/server.go`（`Options.Preimages *agent.Preimages`；6 处调用；注册 `rollback_session`）、`scope_tools_test.go`（`pathless` 加 `rollback_session`）
- Modify: `internal/agent/session.go`（`State` 加 `rolled_back`；`Session` JSON 加 `ops_count`、`rolled_back_at`）
- Create: `internal/control/sessions_rollback.go`（`POST /sessions/{id}/rollback {dry_run|confirm}`，在 `sessionByPath` 内分发；`confirm.rollback` 文案键；`GET /sessions/{id}` 响应加 `ops[]`；`GET /sessions?path=` 走 `SessionsTouching`）
- Modify: `cmd/cloudfs/agent.go`（`sessions rollback <id> --confirm [--dry-run]`，走控制面路由；输出三组清单）
- Modify: `internal/daemon/daemon.go`（owner：`Preimages.Recover` 启动时；GC 每小时；`d.Preimages` 注入两处 `mcpsrv.Options`）
- Modify: `docs/mcp.md`（回滚承诺三句话，逐字自 roadmap §4.8）

**Steps:**
- [ ] RED（agent 包，fake FSOps）：`TestCapturePrefersALinkAndFallsBackToACopy`、`TestCaptureNeverRefusesTheWrite`（too_large → `pre_reason`，行照写）、`TestRecoverDropsOrphanBlobs`、`TestRollbackRestoresInReverseOrder`（create→move→delete 后回滚，路径树与会话前逐项相等）、`TestRollbackSkipsConflictingFiles`（`post_version` ≠ 当前 → conflict，内容未被覆盖）、`TestDryRunWritesNothing`（fake 的写计数为 0）、`TestRollbackOfARollbackRestoresTheEdit`、`TestGCKeepsBlobsInsideRetention`
- [ ] RED（mcpsrv）：`TestWriteToolsRecordOpsWithPreimages`（write/edit/mkdir/move/delete 各一行，`pre_state` 正确）、`TestRollbackSessionNeedsConfirm`、`TestRollbackSessionRestoresContentWithoutRedownload`（fakeprovider `Calls("ReadRange")` 与 `DownloadURL` 增量 0——文件已缓存）、`TestRollbackSessionOnUncachedFileDownloadsOnce`、`TestEveryToolChecksItsPaths` 仍绿
- [ ] RED（control/cmd）：`TestRollbackRouteDryRunThenConfirm`、`TestSessionsByPathListsTouchingSessions`、`TestSessionsRollbackCLI`
- [ ] GREEN：`./gow test ./internal/agent/ ./internal/mcpsrv/ ./internal/control/ ./internal/config/ ./internal/daemon/ ./cmd/cloudfs/ -count=1 && ./gow test -race ./internal/agent/ ./internal/mcpsrv/`
- [ ] 提交：`feat(agent,mcpsrv): session preimages and rollback with dry-run, conflicts and retention`

## Task C2：T-38 界面 — 操作表、回滚预览/确认/结果、检查器"被 Agent 修改"（F5）

**Files:**
- Create: `internal/control/web/rollback_plan.js`（零 import：`groupPlan(plan)` → `{restore[], skip[], conflict[]}`，空计划 → 三组空；`shortID(id)`）+ `_tests/rollback_plan.test.mjs`
- Modify: `internal/control/web/session_panel.js`（操作表：序号 / 操作 / 路径（改名旧→新）/ 前像（`t('rollback.pre.ok|too_large|not_cached|dir')` 点+文字）/ 回滚结果；"回滚此会话"按钮 `undo` → `api.post('/sessions/'+id+'/rollback', {dry_run:true})` → 预览浮层（`openPanel`）三组 + 三句承诺（i18n）→ `confirmDelete` 键入 `shortID` → `{confirm:true}` → 结果浮层 + "回滚这次回滚"）
- Modify: `screens/agents_sessions.js`（状态 `rolled_back` 文案与行内"回滚"入口）
- Create: `internal/control/web/agent_touch.js`（`mountAgentTouch(inspectorEl, path)`：`GET /sessions?path=&since=` 非空时渲染"被 Agent 修改 · client · time"，点击打开会话详情）；`screens/main.js` 检查器一行接线
- Modify: `i18n_zh.js`/`i18n_en.js`（`rollback.*`、`session.ops.*`）
- Create: `internal/control/ui_rollback_test.go`

**Steps:**
- [ ] RED：`TestRollbackButtonPreviewsBeforeConfirming`（源码断言：`dry_run: true` 请求先于 `confirm: true`；不存在直接 `confirm` 的路径）、`TestRollbackPlanGroupsAndPromise`、`TestInspectorShowsAgentTouch`（调用 `/sessions?path=`）、`TestSessionOpsRenderAsText`（路径经文本节点）；`rollback_plan.test.mjs` 分组/空计划
- [ ] GREEN：`node --test internal/control/web/_tests/*.test.mjs && ./gow test ./internal/control/ -run 'TestWeb|TestBrowserModule|TestRollback|TestInspector|TestSession|TestEveryRoute' -count=1`
- [ ] 提交：`feat(ui): session operation table and the preview-then-confirm rollback flow`

## Task C3：线 C 收口 — chaos、e2e、浏览器冒烟、文档、TODO

**Files:**
- Create: `test/chaos/rollback_chaos_test.go`（`TestPreimageLinkThenCrashLeavesNoFalsePreimage`：注入 `os.Link` 后、插行前 kill（用 `agent.Preimages` 的测试钩子 `afterLink`）→ 重启 `Recover` 无假前像、孤儿回收；`TestRollbackInterruptedIsIdempotent`：回滚中途取消 ctx → 重跑结果一致）
- Create: `test/e2e/rollback_e2e_test.go`（`TestRollbackInTheBrowser`：MCP 写 → 终端 `cat` 新内容 → 浏览器点回滚（`CLOUDFS_BROWSER=1`）→ 终端 `cat` 旧内容；无浏览器时走控制面路由的非浏览器版 `TestRollbackRestoresWhatTheShellSees` 必须跑）
- Modify: `TODO.md`（T-38 `[x]` 与 T-43 结论，写法同 T-44：完成 / 验收证明 / 遗留）、`docs/ui-plan.md`（F5、F10 打勾）、`docs/agent-roadmap.md` §7.2 状态、`docs/mcp.md` 工具表加 `rollback_session`
- [ ] GREEN：`./gow test ./test/chaos/ -run Rollback -race -count=1 && ./gow test ./test/e2e/ -run 'Rollback|Coexist|StdioBeside' -count=1 -v && CLOUDFS_BROWSER=1 ./gow test ./test/e2e/ -run 'TestRollbackInTheBrowser|TestAgent' -count=1 -v`
- [ ] 提交：`docs: close T-38 and T-43 with their chaos and e2e evidence`

---

# 线 D：T-39 嵌入与 hybrid → T-40 记忆库

## Task D0：`internal/embed` 与 `index.embedding` 配置

**Files:**
- Create: `internal/config/embedding.go`（`IndexEmbedding{Provider, BaseURL, Model, APIKey, Dimensions, Batch, Concurrency, QPS, Timeout, Proxy, AllowRemote, Quantize}`；`Index.Embedding`、`Index.MaxChunks`（默认 200 000）；`validate`：provider ∈ none/openai/ollama，openai 必填 `api_key` 且必须是 `keyring:`/`secretfile:` 引用，`allow_remote:false` 且 host 非回环/RFC1918/`.local` → 错误；`IsSecretField` 加 `api_key`）
- Create: `internal/embed/embed.go`（`Embedder` 接口；`Options{Client *httpx.Client, Limiter, Breaker}`；`New(cfg, secrets)`；`Health()`；`ErrRemoteDisabled`）、`openai.go`、`ollama.go`、`fake.go`（hash → 确定性单位向量；`Calls()`、`Batches()`）
- Tests: `internal/embed/*_test.go`（`httptest.Server` 假端点）、`internal/config/embedding_test.go`

**Steps:**
- [ ] RED：`TestOpenAIBatchesAndSendsDimensions`、`TestOllamaUsesTheBatchEndpoint`、`TestDimIsProbedOnceAndPinned`、`TestBreakerOpensAfterFiveServerErrors`（60 s 内 0 请求，`Health().Healthy=false`）、`TestThrottledHonoursRetryAfter`、`TestRemoteEndpointNeedsAllowRemote`（`config.Parse` 报错）、`TestAPIKeyMustBeAReference`
- [ ] GREEN：`./gow test ./internal/embed/ ./internal/config/ -count=1 -race`
- [ ] 提交：`feat(embed): embedding client for openai-compatible and ollama endpoints with breaker and privacy gate`

## Task D1：index.db v2 — 向量、嵌入 worker、hybrid 检索、perf 基线

**Files:**
- Modify: `internal/index/schema.go`（`schemaVersion = 2`，加 `vectors`、`embed_pending`；迁移只加表）
- Create: `internal/index/vectors.go`（`vectorSet`：启动/首查一次性载入 `[]int8` + `[]uint32 chunkID/docID`；`cosineTopK(q, docFilter, k)`；int8 量化 `scale`；`Store.PutVectors/DropVectors/PendingEmbeds/MarkEmbedded`）
- Create: `internal/index/embed_worker.go`（消费 `embed_pending`，`ceil(n/batch)` 次调用；模型/维度与 `index_meta` 不一致 → 清空 `vectors`、全部 chunk 入 `embed_pending`、`chunks_fts` 不动；熔断打开时休眠到 `breaker_open_until`；`max_chunks` 硬上限）
- Create: `internal/index/hybrid.go`（`mode=vector`：cosine top-k；`mode=hybrid`：FTS bm25 与 cosine 各取 top-2k 做 RRF k=60；无 Embedder 或端点不健康 → keyword + `Degraded`；`ModeUsed` 如实）
- Modify: `internal/index/search.go`、`service.go`（`Status.Embedding{Provider, Model, Dim, Remote, Healthy, LastError, BreakerOpenUntil, Embedded, Pending, CharsThisMonth}`、`Status.Vectors`）、`indexer.go`（`Options.Embedder`；文档重抽后其 chunk 进 `embed_pending`）
- Create: `test/perf/embed_perf_test.go`（`TestEmbedCallsEqualCeilChunksOverBatch`、重跑 0 次）

**Steps:**
- [ ] RED：`TestSchemaV2AddsVectorTables`、`TestHybridRRFOrdersByFusedRank`（构造 BM25 与 cosine 结论相反的数据，断言 RRF 顺序）、`TestVectorModeNeedsAnEmbedder`（provider=none → `mode_used: keyword`、`degraded` 非空、无错误）、`TestChangingTheModelReembedsEverything`（`vectors` 清空、`embed_pending == chunks`、`chunks_fts` 行数不变）、`TestEmbedWorkerSleepsWhileTheBreakerIsOpen`、`TestMaxChunksIsAHardCap`
- [ ] GREEN：`./gow test ./internal/index/ -count=1 -race && ./gow test ./test/perf/ -run Embed -count=1`
- [ ] 提交：`feat(index): int8 vectors, an embedding worker and RRF hybrid search that degrade to keyword honestly`

## Task D2：控制面 `/index/embedding`、`check`、doctor、CLI `index auth|embedding`

**Files:**
- Create: `internal/control/embedding.go`（`GET /index/embedding` → `Status.Embedding` + `api_key_configured` + `remote` + 费用估算 `estimate{chars, formula}`；`POST /index/embedding/check {confirm:false 允许}` 真实调一次 `Embed(["cloudfs"])`，返回 dim/latency/error）
- Modify: `internal/control/doctor.go`（`checkEmbedding`：端点可达、维度与 `index_meta` 一致、`remote` 提示）
- Modify: `cmd/cloudfs/index.go`（`index auth`：读 stdin/`--key-file`，`SecretStore.Put("index.embedding", v)` 并把 `keyring:` 引用写进配置；`index embedding` 打印状态）
- Modify: `internal/mcpsrv/index_tools.go`（`index_status.embedding`、`semantic_search.mode` 透传，`TestSemanticSearchReportsModeUsed`）

**Steps:**
- [ ] RED：`TestEmbeddingStatusNeverLeaksTheKey`（响应无 `api_key` 值、无 `Authorization`）、`TestEmbeddingCheckCallsTheEndpointOnce`、`TestDoctorFlagsDimensionMismatch`、`TestIndexAuthWritesAReference`
- [ ] GREEN：`./gow test ./internal/control/ ./cmd/cloudfs/ ./internal/mcpsrv/ -count=1`
- [ ] 提交：`feat(control,cli): embedding endpoint status, a one-shot check and cloudfs index auth`

## Task D3：UI F6 — 嵌入面板、远端横幅、语义模式

**Files:**
- Create: `internal/control/web/embedding_panel.js`（`mountEmbeddingPanel(root)`：provider/模型/维度/地址、健康点+最后错误+熔断恢复、已嵌入/待嵌入、本月字符与"估算"费用；`remote=true` 黄色横幅无关闭按钮；"测试端点"先说明再 `POST /index/embedding/check`；无 key 时显示 `cloudfs index auth` 命令 + `copyBtn`，**无 input**；底部"在配置文件中修改"）；`screens/index.js` 接线 + 概况卡"向量 N / max_chunks"
- Modify: `content_search.js` / `name_search.js` `extraModes`（"文件名 / 关键词 / 语义"，`mode=hybrid`；响应 `degraded` 非空时渲染"已降级为关键词"说明行）
- Modify: `i18n_zh.js`/`i18n_en.js`（`embedding.*`）
- Create: `internal/control/ui_embedding_test.go`

**Steps:**
- [ ] RED：`TestRemoteBannerHasNoCloseButton`、`TestEmbeddingPanelHasNoKeyInput`（源码无 `input` 用于 key、无 `api_key` 字段名）、`TestEndpointCheckOnlyOnClick`、`TestSemanticModeShowsDegradedNote`
- [ ] GREEN：`node --test internal/control/web/_tests/*.test.mjs && ./gow test ./internal/control/ -run 'TestWeb|TestBrowserModule|TestEmbedding|TestSemantic|TestContentSearch' -count=1`
- [ ] 提交：`feat(ui): embedding endpoint panel with the remote-data banner and a semantic search mode`

## Task D4：T-40 后端 — `internal/memory` 与 5 个 MCP 工具

**Files:**
- Create: `internal/config/memory.go`（`Memory{Root, MaxFactBytes(64 KiB), MaxAgentBytes(32 MiB)}`；`Root` 默认 `mcp.workspace`；校验规范路径）
- Create: `internal/memory/memory.go`（`Store{fs, index, cfg}`；`Agents(ctx)`、`List(ctx, agent, cursor)`、`Get(ctx, agent, name) Fact{Content, Version, Path, Conflicts[]}`、`Put(ctx, agent, name, content, mode, expectedVersion)`（大小/预算校验 → `FS.WriteFile` → `MEMORY.md` 唯一匹配替换否则追加）、`Delete(ctx, agent, name)`、`Search(ctx, q, agent, includeShared, topK, mode)` = index 限定范围检索；`NormalizeAgent(clientName)`、`ValidName`）、`frontmatter.go`（前 4 KiB 解析 `name/description/type/updated_at`）、`conflicts.go`（同目录、以 `<name>` 开头且 ≠ `<name>.md` 的兄弟）
- Create: `internal/mcpsrv/memory_tools.go`（`memory_list/get/put/delete/search`；`agent` 默认 = 会话 client name 规范化；每个路径过 `checkPath`；`memory.root` 不在 scope 内 → 五个工具同一说明错误；`--read-only` 下 put/delete 拒绝；`delete` 需 `confirm`）
- Modify: `internal/mcpsrv/server.go`（`Options.Memory`）、`scope_tools_test.go`（5 个工具进带路径表）、`internal/index/rules.go`（内置规则：`memory.root` 子树自动索引 `**/*.md`）
- Modify: `internal/daemon/daemon.go`（`d.Memory`）、`cmd/cloudfs/main.go`（两处 `Options.Memory`）

**Steps:**
- [ ] RED：`TestPutCreatesTheFactAndOneIndexLine`（重复 put 不增行）、`TestStaleExpectedVersionIsRefused`、`TestGetListsConflictCopies`（fakeprovider 注入版本冲突 → 副本出现在 `conflicts`）、`TestBudgetsAreEnforcedWithUsage`、`TestRootOutsideScopeFailsEveryToolTheSameWay`、`TestReadOnlyAllowsReadsOnly`、`TestMemorySearchFindsAFreshFact`（≤ 3 s）、`TestAgentNameIsNormalised`
- [ ] GREEN：`./gow test ./internal/memory/ ./internal/mcpsrv/ ./internal/config/ ./internal/index/ ./internal/daemon/ -count=1 -race`
- [ ] 提交：`feat(memory,mcpsrv): file-backed agent memory with versioned puts, conflict listing and scoped search`

## Task D5：记忆库控制面路由与 CLI

**Files:**
- Create: `internal/control/memory.go`（`GET /memory/agents`、`GET /memory/{agent}?cursor`、`GET|PUT|DELETE /memory/{agent}/{name}`；PUT 带 `expected_version`，409 时返回 `current_version`；DELETE 走 `confirmed`，`confirm.memory.delete` 键；同一 `memory.Store`；`Collector.Memory`）
- Modify: `cmd/cloudfs/main.go` + 新 `cmd/cloudfs/memory.go`（`memory list|get|put|delete [--agent]`）
- Tests: `internal/control/memory_test.go`、`cmd/cloudfs/memory_test.go`

**Steps:**
- [ ] RED：`TestMemoryPutRequiresExpectedVersionWhenGiven`、`TestMemoryDeleteNeedsConfirm`、`TestMemoryRoutesRefuseBadNames`（`..`、`/`、大写）、`TestEveryRouteIsGuarded` 自动覆盖
- [ ] GREEN：`./gow test ./internal/control/ ./cmd/cloudfs/ -count=1`
- [ ] 提交：`feat(control,cli): memory routes and the cloudfs memory command on the same store as the MCP tools`

## Task D6：UI F7 — 记忆标签、编辑浮层、冲突合并浮层

**Files:**
- Create: `internal/control/web/memory_conflicts.js`（零 import：`pairConflicts(entries)`——只按前缀与同目录配对，不猜 provider 命名）+ `_tests/memory_conflicts.test.mjs`
- Create: `internal/control/web/screens/agents_memory.js`（左 agent 列表（条数与占用/上限）、右表格（名称/描述/类型/更新时间/冲突红点+文字）、顶部搜索框走 `/index/search?path=<memory root>`；"新建记忆"`openForm`（名称前端正则校验）；删除 `confirmDelete` 键入名称 → `DELETE` 带 `confirm:true`；未配置 root → 说明 + 配置示例）
- Create: `internal/control/web/memory_panel.js`（编辑浮层：frontmatter（名称只读、描述、类型）+ 正文 `textarea`（按文本填充）+ 字节计数/上限；保存 `PUT` 带 `expected_version`，409 → "已在其他设备修改" + "重新载入"；冲突合并浮层：左右只读并排（`GET /fs/preview` 读副本）、三个动作："保留本体并删除副本"（`POST /fs/delete` + `confirmDelete`）、"用副本覆盖本体"（PUT 带 `expected_version` → 删副本）、"手动合并"（编辑浮层预填两段））
- Modify: `screens/agents.js`（加"记忆"标签）、`i18n_*.js`（`memory.*`）
- Create: `internal/control/ui_memory_test.go`

**Steps:**
- [ ] RED：`TestMemorySaveCarriesExpectedVersion`、`TestMemoryDeleteConfirms`、`TestConflictActionsHitTheirRoutes`（三动作各自路由）、`TestMemoryBodyIsInsertedAsText`、`TestMemoryTabExplainsMissingRoot`；`memory_conflicts.test.mjs` 配对与不配对样例
- [ ] GREEN：`node --test internal/control/web/_tests/*.test.mjs && ./gow test ./internal/control/ -run 'TestWeb|TestBrowserModule|TestMemory|TestConflict' -count=1`
- [ ] 提交：`feat(ui): memory tab with versioned editing and a conflict merge overlay`

## Task D7：线 D 收口 — e2e、文档、TODO

- Create: `test/e2e/memory_e2e_test.go`（`TestMemoryPutIsVisibleInTheMountAndSearchable`：`memory_put` → 终端 `cat facts/<name>.md` 与 `MEMORY.md` 一行 → 3 s 内 `memory_search` 命中；`TestHybridSearchWithAFakeEmbedder`：`withIndex` + `embed.Fake` → `mode_used: hybrid`）
- Modify: `TODO.md`（T-39、T-40 `[x]`：完成/验收证明/遗留；`UNVERIFIED` 计数——本线新增：openai 真实端点 `dimensions` 行为、ollama 批量接口返回顺序）、`docs/ui-plan.md`（F6、F7 打勾）、`docs/mcp.md`（工具表加 5 个 `memory_*`、`semantic_search.mode` 说明与隐私一段）、`docs/agent-roadmap.md` §7.2 状态、`README.md`（`index.embedding` 与 `memory` 配置示例）
- [ ] GREEN：`./gow test ./test/e2e/ -run 'Memory|Hybrid' -count=1 -v && ./gow test ./test/perf/ -count=1`
- [ ] 提交：`docs: close T-39 and T-40 with their e2e evidence`

---

# 线 E：T-41 触发器 → T-42 "运行"按钮

## Task E0：`vfs.Change.Kind/Origin` 与 `WithOrigin`

**Files:**
- Modify: `internal/vfs/changes.go`（`ChangeKind`：`KindWrite/KindCreate/KindMkdir/KindRemove/KindRename/KindRemote/KindRescan`；`Origin`：`OriginKernel/OriginAPI/OriginRemote`；`Change` 加 `Kind`、`Origin`；`WithOrigin(ctx, name string)`（`"mcp"|"control"|"webdav"` → API）；`originOf(ctx)`：`fromKernel` → Kernel，`WithOrigin` → API，否则 Remote）；helper `changedNode/changedEntry/changedRename/changedListing` 加 `kind` 参数（`changedListing` 固定 `KindRemote`，`Rescan` 固定 `KindRescan`）
- Modify: 所有调用点（write.go 9 处按操作填 kind；vfs.go 909/920、copy_jobs.go、copy_reconcile.go、publication.go、refresh.go、upload_cleanup.go 填 `KindRemote` 或对应操作）
- Modify: `internal/mcpsrv/session_mw.go`（middleware 里 `vfs.WithOrigin(ctx, "mcp")`）、`internal/control/fs.go`（`"control"`）、`internal/webdav`（`"webdav"`）
- Test: `internal/vfs/changes_kind_test.go`（**9 个 emit 点各断言 kind/origin**：写句柄 flush → write/kernel（`FromKernel` ctx）；`WriteFile` via MCP ctx → write/api；`Mkdir` → mkdir；`Remove` → remove；`Rename` → rename；delta 刷新 → remote/remote；队列溢出 → rescan）

**Steps:**
- [ ] RED：上述用例 + `TestAffectsIgnoresKindAndOrigin`（旧消费者不受影响）
- [ ] GREEN：`./gow test ./internal/vfs/ -count=1 -race && ./gow test ./internal/mcpsrv/ ./internal/control/ ./internal/webdav/ -count=1`；`./gow test ./test/perf/ -count=1`（调用次数基线不变）
- [ ] 提交：`feat(vfs): tag every change with its kind and origin`

## Task E1：配置 `triggers[]`、`agents[]`、glob 与自激 warning

**Files:**
- Create: `internal/config/triggers.go`（`Trigger{Name, Paths, Events, Origins, Debounce, OnRescan, Action{Exec *ExecAction, Webhook *WebhookAction}}`；`ExecAction{Command []string, Cwd, Timeout}`；`WebhookAction{URL, Secret, Timeout, IncludeDownloadURL, Proxy, Insecure}`；`Agent{Name, Exec ExecAction}`；`Config.Triggers`、`Config.Agents`、`Config.Warnings []string yaml:"-"`；校验：name 唯一且 `^[a-z0-9][a-z0-9-]{0,63}$`，恰一个 action，exec `command` 非空且 argv[0] 不含 `{`，webhook URL 为 https 或回环 http 否则需 `insecure: true`，`secret` 必须 `keyring:`/`secretfile:` 引用（`IsSecretField` 加 `secret`），未排除 `api` 的 exec 规则 → `Warnings` 追加"may be triggered by the agent's own writes"）
- glob：复用 `internal/index/glob.go` 的匹配器——**把它抽到 `internal/pathglob`**（index 与 trigger 都 import；纯搬家 + 一次性改 import，不改逻辑）

**Steps:**
- [ ] RED：`TestTriggerNeedsExactlyOneAction`、`TestWebhookSecretMustBeAReference`、`TestPlainHTTPWebhookNeedsInsecure`、`TestExecRuleWithoutOriginFilterWarns`、`TestAgentsShareTheExecRules`、`pathglob` 包测试原样通过
- [ ] GREEN：`./gow test ./internal/config/ ./internal/pathglob/ ./internal/index/ -count=1`
- [ ] 提交：`feat(config): trigger and agent rules with a shared path glob package`

## Task E2：`internal/trigger` 引擎、exec、webhook、`agent.Deliveries`

**Files:**
- Create: `internal/agent/deliveries.go`（`Delivery` 行类型；`Enqueue(ctx, rule, path, kind, origin, due)`（`INSERT OR IGNORE` 命中 pending 唯一索引 = 去抖合并）、`Claim(ctx, rule, now)`（pending 且 due → running）、`Done/Fail(ctx, id, err, output, nextDue)`、`Dead`、`Retry(ctx, id)`、`ResetRunning(ctx)`、`List(ctx, q)`、`Get(ctx, id)`、`DeadCount`；`Store.Watch` 加 `Event{Kind: "trigger", Delivery}`）
- Create: `internal/trigger/engine.go`（`Engine{fs, rules, deliveries, execs, hooks}`；`Run(ctx)`：一个 goroutine 消费 `FS.WatchChanges()`，按 `Kind/Origin/glob` 匹配（`Subtree` 事件按前缀匹配 glob 根；`Rescan` → 每条规则一行 `path=''` 除非 `on_rescan: ignore`），`Enqueue(due = now + debounce)`；每规则一个串行 worker：`Claim` → 执行 → `Done`/`Fail` 退避 1 s×2ⁿ 封顶 5 min、8 次 → dead；启动 `ResetRunning`；`Test(ctx, rule, path)` 直接入队并立即执行；`Retry(id)`）
- Create: `internal/trigger/exec.go`（`runExec(ctx, a ExecAction, vars)`：`exec.CommandContext(argv[0], argv[1:]...)`；`{path}/{kind}/{uri}/{prompt}` 只整体替换独立 argv 元素；env 仅 `PATH/HOME/LANG` + `CLOUDFS_PATH/KIND/URI`；`SysProcAttr{Setpgid: true}`，超时 `kill(-pgid)`；stdout/stderr 各截 64 KiB）
- Create: `internal/trigger/webhook.go`（body `{rule, path, uri, kind, origin, ts, size?, download_url?}`；`X-CloudFS-Timestamp`、`X-CloudFS-Signature: sha256=hex(HMAC(secret, ts+"."+body))`；出站 `proxy.Manager` Dial；`Verify(secret, r, body, now)` 导出供文档与测试）
- Modify: `internal/daemon/daemon.go`（owner 且规则非空才 `d.Trigger = trigger.New(...)` 并 `Run`）

**Steps:**
- [ ] RED：`TestDebounceCollapsesABurstIntoOneDelivery`（同路径 50 ms 内 20 次 → 1 行，`kind=write`）、`TestExecArgvIsNeverAShell`（`["echo","{path}; rm -rf /"]` → argv[1] 字面等于 `"/work/a.txt; rm -rf /"`，用一个回显 argv 的测试二进制/`os.Args` 自举）、`TestExecEnvironmentIsMinimal`、`TestExecTimeoutKillsTheProcessGroup`（`sh -c 'sleep 60 & wait'` 后无孙进程）、`TestWebhookSignatureVerifies`（错 secret 拒、对的过、ts 偏差 > 5 min 拒）、`TestRunningDeliveriesRestartAsPending`（`attempts=2`）、`TestStormStaysBounded`（1 万事件 → 行数 ≤ 规则×路径，溢出 rescan 只一行）、`TestAPIOriginCanBeExcluded`、`TestBackoffThenDead`
- [ ] GREEN：`./gow test ./internal/agent/ ./internal/trigger/ ./internal/daemon/ -count=1 -race`
- [ ] 提交：`feat(trigger): at-least-once deliveries with debounce, a shell-free exec runner and signed webhooks`

## Task E3：控制面 `/triggers/*`、SSE `trigger`、CLI、doctor

**Files:**
- Create: `internal/control/triggers.go`（`GET /triggers`（规则只读视图：exec 给 argv 数组、webhook 给 URL 与 `secret_configured: true`，**永不返回 secret**）、`GET /triggers/deliveries?cursor&rule&state`、`GET /triggers/deliveries/{id}`（含 output 与 `truncated`）、`POST /triggers/test {name, path, confirm}`（`confirmed`，`confirm.trigger.test`）、`POST /triggers/retry {id}`；`Collector.Trigger`；`/status` 加 `triggers{dead, pending}`）
- Modify: `internal/control/events.go`（`trigger` 事件 `{id, rule, state, attempts}`）、`doctor.go`（`checkTriggers`：`cfg.Warnings` → warn）
- Create: `cmd/cloudfs/triggers.go`（`triggers list|deliveries|test|retry`）

**Steps:**
- [ ] RED：`TestTriggersViewNeverContainsTheSecret`、`TestTriggerTestNeedsConfirm`、`TestRetryOnlyDeadDeliveries`、`TestTriggerEventsReachSSE`、`TestDoctorSurfacesConfigWarnings`、`TestEveryRouteIsGuarded` 自动覆盖
- [ ] GREEN：`./gow test ./internal/control/ ./cmd/cloudfs/ -count=1`
- [ ] 提交：`feat(control,cli): read-only trigger rules, delivery listing, test and retry`

## Task E4：UI F8 — `#/triggers` 屏

**Files:**
- Create: `internal/control/web/trigger_view.js`（零 import：`selfTriggerRisk(rule)`：exec 且 origins 含 api（或为空）→ true；`argvCells(cmd)`）+ `_tests/trigger_view.test.mjs`
- Create: `internal/control/web/screens/triggers.js`（规则卡片只读：argv 逐元素等宽 `<code>` 不拼接；webhook 显示 URL 与"签名密钥已配置"；风险黄标；底部"在配置文件中修改"；投递表列 时间/规则/路径/事件/来源/次数/状态（点+文字）/动作（dead 行"重试"）；规则与状态过滤；`moreRow` 分页；`onTrigger` 未翻页时刷新；空状态：两个配置示例 + 校验代码片段（i18n 之外的代码块用 `<pre>` 文本节点））
- Create: `internal/control/web/delivery_panel.js`（`showPanel` 详情：stdout/stderr 文本节点 + 截断提示，或响应码与错误；支持 `#/triggers?delivery=<id>`）
- Modify: `router.js`（`'#/triggers': 'triggers-view'`，`navItems` 在 `#/index` 之后，`icon: 'bolt'`，`badge: 'triggers'` = dead 数）、`app.js`（视图容器与 badge）、`i18n_*.js`（`triggers.*`）
- Create: `internal/control/ui_triggers_test.go`

**Steps:**
- [ ] RED：`TestTriggersScreenHasNoRuleEditor`（无 form 提交 PUT/POST 规则）、`TestArgvRendersPerElement`、`TestWebhookSecretNeverInDOM`、`TestTestDeliveryConfirms`（`confirm: true`）、`TestDeadRowRetries`（`/triggers/retry`）、`TestDeliveryOutputIsText`；`trigger_view.test.mjs` 自激判断
- [ ] GREEN：`node --test internal/control/web/_tests/*.test.mjs && ./gow test ./internal/control/ -run 'TestWeb|TestBrowserModule|TestTrigger|TestEveryRoute|TestNav' -count=1`
- [ ] 提交：`feat(ui): triggers screen with read-only rules, deliveries and a confirmed test run`

## Task E5：T-42 后半 — `/agent/endpoints`、`/agent/invoke`、运行按钮（F9-3/F9-4）

**Files:**
- Create: `internal/control/agent_invoke.go`（`GET /agent/endpoints` → `{agents:[{name}]}` 只有名字；`POST /agent/invoke {agent, paths[], prompt?, confirm}`：未配置 → 404 且不启动进程；`confirmed`（`confirm.agent.invoke`）；组装 `{prompt}` = 用户提示词，`paths` 逐个作为独立 argv 追加；入队 `rule="agent:<name>"` 并立即执行（复用 `trigger.Engine.Invoke`）；审计 `principal=console`、`tool=agent.invoke`（走 `agent.Store` 直接写审计行））
- Modify: `internal/trigger/engine.go`（`Invoke(ctx, a config.Agent, vars) (deliveryID, error)`）
- Modify: `internal/control/web/send_to_agent.js`（`GET /agent/endpoints` 非空时才渲染 agent 下拉 + "运行"；`confirmDelete` 键入 agent 名 → `POST /agent/invoke` 带 `confirm:true` → toast + "查看投递"链接 `#/triggers?delivery=<id>`；复制按钮行为不变）
- Modify: `internal/control/ui_send_to_agent_test.go`、`i18n_*.js`（`agent.run.*`）

**Steps:**
- [ ] RED：`TestInvokeWithoutAgentsIs404AndSpawnsNothing`、`TestInvokePassesPathsAsSeparateArgv`、`TestInvokeIsAudited`、UI：`TestRunButtonOnlyWithEndpoints`、`TestRunConfirmsWithConfirmTrue`、`TestCopyStillMakesNoWrite`
- [ ] GREEN：`./gow test ./internal/control/ ./internal/trigger/ -count=1 && node --test internal/control/web/_tests/*.test.mjs`
- [ ] 提交：`feat(control,ui): run a configured agent on a file from the send-to-agent panel`

## Task E6：线 E 收口 — chaos、e2e、文档、TODO

- Create: `test/chaos/trigger_chaos_test.go`（`TestDeliveryRunningAtCrashIsRedelivered`：running 时关闭引擎（模拟 kill -9：不走 Done）→ 重启 `attempts=2`）
- Create: `test/e2e/trigger_e2e_test.go`（`TestKernelWriteFiresAnExecTrigger`：真实挂载下 `echo > mnt/inbox/a.txt` → exec 规则（`sh` 不用；用 `/bin/cat` 写到 tmp 文件的 argv）在 5 s 内投递 done 且 argv 含虚拟路径；`TestMCPWriteDoesNotFireWhenAPIIsExcluded`；`CLOUDFS_BROWSER=1` 下 `#/triggers` 可达且投递行渲染）
- Modify: `TODO.md`（T-41、T-42 `[x]`；本线 `UNVERIFIED`：Windows 下 `Setpgid` 等价物）、`docs/ui-plan.md`（F8、F9-3/F9-4 打勾）、`docs/agent-roadmap.md` §7.2、`README.md`（`triggers`/`agents` 配置示例 + webhook 校验代码）、`docs/mcp.md`（至少一次投递与溢出 rescan 的限制）
- [ ] GREEN：`./gow test ./test/chaos/ -run Trigger -race -count=1 && ./gow test ./test/e2e/ -run 'Trigger' -count=1 -v && CLOUDFS_BROWSER=1 ./gow test ./test/e2e/ -run 'TestTriggersScreen' -count=1 -v`
- [ ] 提交：`docs: close T-41 and T-42 with their chaos and e2e evidence`

---

## 合入与二期总验证

1. 顺序 C → E → D，各自 `git rebase feat/agent-phase1` 后 ff 合入；每次合入后立刻 `./gow build ./... && ./gow vet ./... && ./gow test ./internal/... -count=1`。
2. 合入后收口（在 `feat/agent-phase1` 上一个提交）：`TestEveryToolChecksItsPaths` 登记齐 `rollback_session` 与 5 个 `memory_*`；`cmd/cloudfs/main.go` 两处 `mcpsrv.Options` 同时带 `Preimages`/`Memory`；导航顺序与 `docs/ui-plan.md` 一致；`docs/agent-roadmap.md` §7.1/§7.2 状态列。
3. 总验证：
   ```sh
   ./gow build ./... && ./gow vet ./...
   ./gow test ./... -count=1
   ./gow test -race ./internal/agent/ ./internal/embed/ ./internal/index/ ./internal/memory/ ./internal/trigger/ ./internal/vfs/ ./internal/mcpsrv/ ./internal/control/ ./internal/daemon/ ./internal/config/ ./cmd/cloudfs/
   node --test internal/control/web/_tests/*.test.mjs
   ./gow test ./test/perf/ -count=1
   ./gow test ./test/chaos/ -race -count=1
   CLOUDFS_BROWSER=1 ./gow test ./test/e2e/ -run 'TestAgent|TestContentSearch|TestNameSearch|TestRollback|TestTriggersScreen' -v -count=1
   ```
4. `grep -c '^### \[x\] T-\(38\|39\|40\|41\|42\|43\)' TODO.md` = 6；`docs/ui-plan.md` F5–F10 全部 `[x]`。

## 结论记录（执行时填写）

- T-43 e2e 结论：
- stdio→HTTP 桥是否提前到三期之前：
