# Agent 工作底座一期（T-34 ~ T-37）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 两条互不阻塞的线并行交付一期：线 A（T-34 审计与会话作用域 → T-35 访问令牌与 HTTP 接入 → T-36 交付箱），线 B（T-37 内容抽取 + FTS + `semantic_search`）；每条后端能力都带同期控制台界面与同一验收。

**Architecture:** 新增 `internal/agent`（独立 `<cache.dir>/agent/agent.db`）与 `internal/textract` + `internal/index`（独立 `<cache.dir>/index.db`），二者都是 `vfs` 的**消费者**，不改 meta v11 / journal v13，不改 `vfs` 语义。`mcpsrv` 只加 receiving middleware（会话解析 + 审计）与 `checkPath(ctx, p, write)`；控制面加路由进 `metrics.go` 路由表；控制台加 `#/agents`、`#/index` 两屏与主窗口搜索/检查器扩展，纯逻辑抽成零 import 模块配 `node --test`。

**Tech Stack:** Go 1.27（模块 `cloudfs`）、`modernc.org/sqlite v1.57.0`（FTS5 trigram）、`github.com/modelcontextprotocol/go-sdk v1.7.0`（`mcp` + `auth`）、新依赖 `github.com/ledongthuc/pdf v0.0.0-20220302134840-0c2507a12d80`（仅 B3）、原生 ES module 控制台（`internal/control/web`）+ Node v24 `node --test`、headless Chrome `--dump-dom` 冒烟（新建门控 `CLOUDFS_BROWSER=1`）。

**Spec:**
- 验收真相源：`TODO.md` P4 节 T-34 / T-35 / T-36 / T-37（若执行时 P4 尚未写入 `TODO.md`，以 `/Users/nava/.claude/plans/fs-workbuddy-encapsulated-frog.md` 的"TODO.md 插入稿"为准，两者文字一致）；T-42 仅"复制提示词"前半。
- 界面清单：`docs/ui-plan.md` 阶段 F（F1–F4、F9 前半）与 `docs/agent-roadmap.md` §UI（屏幕地图、路由表、安全边界五条）。
- 详细设计：`/private/tmp/claude-502/-Users-nava-work-lefs/61c92a79-0409-45f8-972a-30de6e0bae4a/scratchpad/plan-workbase.md`（§0、§1、§A、§B1、§B3）与 `.../scratchpad/plan-rag.md`（§A.1–A.3、A.5–A.11，phase 1 部分）。设计若与 TODO 验收冲突，以 TODO 为准。

## Global Constraints

- 工具链只用 `./gow`：`./gow build ./...`、`./gow vet ./...`（基线干净，不引入新告警）。
- 本机（macOS 无 macFUSE）全量测试：`./gow test $(./gow list ./... | grep -v -e internal/fusefs -e test/conformance -e test/e2e)`；`test/e2e` 只用 `-run` 精确选择**不挂载**的用例运行，需要挂载的用例在 Linux `/dev/fuse` 上跑，skip 不等于通过。
- 每个任务 GREEN 之后对改动包跑 `-race`：`./gow test -race ./internal/agent/ ./internal/mcpsrv/ ./internal/control/`（按任务替换包名）。
- 浏览器纯模块测试：`node --test internal/control/web/_tests/*.test.mjs`；Go 侧 `./gow test ./internal/control/ -run 'TestBrowserModuleBehaviour|TestBrowserModulesParse'`。`TestBrowserModuleBehaviour` 自动 glob `web/_tests/*.test.mjs`，`TestBrowserModulesParse` 自动解析 `web/*.js` 与 `web/screens/*.js`——**无需登记**；纯模块放 `web/` 顶层（同 `paged.js`），测试里 `import { x } from '../x.js'`。`Makefile` 没有 node 目标，不要去加。
- 分层：`internal/agent`、`internal/index`、`internal/textract` 不被 `vfs` 引用；`mcpsrv`/`control` 保持薄适配。一期**不改** `internal/vfs` 任何文件（`Change.Kind/Origin` 是 T-41 二期）。
- 永不按网盘名字分支；行为差异走 `provider.Caps`（例如 `Caps.Tier == "unofficial"` 预算减半）。无法在本机验证的点加 `// UNVERIFIED: <要验证什么>`。
- 新 SQLite 库一律照 `internal/export/store.go:39-48` 的 DSN：`?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_txlock=immediate`，`SetMaxOpenConns(4)`，`meta(k,v)` 表 + `PRAGMA user_version`，比本构建新的 schema 拒绝打开；flock 照 `internal/export/lock.go` / `lock_unix.go` / `lock_windows.go` 复制一份到本包（未导出 helper 不跨包共享）。
- 控制面：新路由**全部**登记在 `internal/control/metrics.go` 的 `routes()`（`security_all_routes_test.go` 自动覆盖守卫）；handler 首行 `privateRequest(w, r)` + `allowMethod`；破坏性动作走 `confirmed(w, r, q.Confirm, "confirm.<key>", args...)`（`shared.go:29`），确认文案键加进 `internal/i18n/catalog_zh.go` 与 `catalog_en.go`；远端凭据永不经过控制面（`rejectSecretFields`）。
- 控制台：所有文案经 `t()`，`web/i18n.js` 的 `zh` 与 `en` 两表键集合一致（`ui_i18n_test.go` `TestWebCatalogsHaveTheSameKeys`），除 `i18n.js` 外任何 `.js` 不得出现汉字与全角标点（`TestWebScreensHoldNoUntranslatedText`），`t('literal')` 必须存在于表中（`TestEveryTranslationKeyUsedByTheAppExists`）；占位符是 `%s`。
- 控制台 DOM：用 `fill()` 不用 `replaceChildren`；来自文件/远端/审计的文本一律作为 `el()` 的**子节点字符串**插入（`append` 走 `createTextNode`），**永不**放进 `html:` 属性；图标名必须在 `icons.js` 定义（`ui_icons_test.go` 用正则 `iconEl\('([a-z]+)'\)` 扫描）；`confirmDelete` 只用对象签名；全部嵌入字节不得出现 `type="password"`、`refresh_token`、`client_secret`、`access_token`、`name="password"`、`id="secret"`、`prompt(`（`ui_test.go` `TestWebAppNeverAsksForACredential`）。
- 文件体量：单文件 < 800 行；`screens/main.js` 当前 376 行，新增逻辑放独立模块（`content_search.js`、`index_inspector.js`、`workspace_view.js` 等），`main.js` 只接线。
- 代码与注释英文；`docs/`、`TODO.md`、`README.md` 中文。
- 每个任务一个提交，conventional commit（`feat:`/`fix:`/`test:`/`docs:`），正文写为什么，结尾 `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`。
- 条目纪律：UI 任务与其后端任务同属一个 TODO 条目，UI 不落地不关条目（线 A 的 A11、线 B 的 B13 才更新 `TODO.md` 状态）。

---

## 决策点：MCP 访问令牌在界面"只显示一次"

**默认（本计划按此编写）**：接受。`POST /mcp/tokens` 返回明文一次（`Cache-Control: no-store`、不落日志），前端只在揭示浮层局部变量里持有，关闭即丢，不进 `store.js`、不进 localStorage；`GET /mcp/tokens` 只返回 4 位指纹。

**回退（用户选"更严"时）**：界面只显示 `cloudfs mcp token create ...` 命令。受影响位置已在任务内以 **【决策点】** 标注：
- **A7**：不登记 `POST /mcp/tokens`（`/mcp/tokens` 只允许 GET，POST 返回 405）；`MCPView` 去掉 `CreateToken`；删除 `TestCreateTokenResponseIsNoStore`，改为 `TestTokensRouteRefusesCreate`。
- **A8**：删除 `web/token_reveal.js` 与"新建令牌"`openForm`；令牌标签顶部改为命令构造器（名称/可读/可写/有效期 → 拼出 CLI 命令 + `copyBtn`，零网络请求）；`ui_tokens_test.go` 改为断言源码**不含** `api.post('/mcp/tokens'`。
- **A11**：e2e 链路第一步改用 `agent.Store.CreateToken`（与 CLI 同一实现），浏览器冒烟不覆盖揭示浮层。
- `docs/agent-roadmap.md` §UI 安全边界第 2 条相应改写（A11 文档步骤）。

---

## 分支与合并卫生

- **起点**：必须等 `feat/pool-v2`（HEAD `41f976d` + 当前大量未提交改动）合入 `main` 之后再开工，否则下列重叠文件必然冲突。开工：`git switch main && git pull && git switch -c feat/agent-phase1`；两条线各自用 worktree（superpowers:using-git-worktrees）：`feat/agent-line-a`、`feat/agent-line-b`，都从 `feat/agent-phase1` 拉出，完成后分别 rebase 回 `feat/agent-phase1`。
- **与当前工作区已修改文件重叠（合并风险，开工前确认 pool-v2 已落地）**：
  `cmd/cloudfs/main.go`、`internal/config/config.go`、`internal/control/events.go`、`internal/control/events_test.go`、`internal/control/metrics.go`、`internal/control/ui_test.go`（仅阅读其断言，本计划不改）、`internal/control/web/api.js`、`internal/control/web/app.js`、`internal/control/web/i18n.js`、`internal/daemon/daemon.go`、`internal/mcpsrv/export_jobs.go`（A2 改 `checkPath/checkWrite` 调用点）、`internal/mcpsrv/export_jobs_test.go`（只跑不改）。
  本计划引用但**不修改**的已修改文件：`internal/vfs/changes.go`、`internal/vfs/read.go`、`internal/vfs/vfs.go`——行号以合入后为准，执行前 `grep -n` 复核。
- **两线交叉热点与协议**（两线都会改，谁后合谁 rebase 解决）：
  | 文件 | 线 A | 线 B | 协议 |
  |---|---|---|---|
  | `internal/mcpsrv/server.go` | A2 `checkPath(ctx,p,write)` 全量替换、`Options.Sessions/Scope` | B9 `Options.Index`、`registerIndexTools` | B9 若先于 A2 合入，调用旧签名 `s.checkPath(p)`；A2 合入后 rebase 一次性改成 `s.checkPath(ctx, p, false)`。A2 的 `TestEveryToolChecksItsPaths` 遍历 `tools/list`，会**强制**把 5 个索引工具登记进表，漏改即红 |
  | `internal/control/metrics.go` `routes()` | A4/A7 | B10/B14 | 各自追加在表尾，冲突只在相邻行，保留两边 |
  | `internal/control/status.go` `Collector`/`Status` | `Agent`、`MCP` 字段 | `Index` 字段 | 同上 |
  | `internal/control/events.go` | `audit`/`session` 事件 | `index` 事件 | 各加一个 `case`，冲突保留两边 |
  | `internal/daemon/daemon.go` | 打开 agent.db（在 `if !opt.SkipWrite` 之前） | 打开 index.db（在 export store 之后） | 位置不同，通常无冲突 |
  | `cmd/cloudfs/main.go` | `mcp token`、`audit`、`sessions` 分发，`mcpsrv.Options.Sessions` | `index` 分发，`mcpsrv.Options.Index` | 两处 `mcpsrv.New(mcpsrv.Options{...})`（stdio 与 `serveMCPHTTPWith`）都要带上两边字段 |
  | `internal/config/config.go` | `MCP.Audit/Session/Workspace` | `Config.Index` | 不同结构体 |
  | `web/router.js`、`web/app.js`、`web/api.js`、`web/i18n.js`、`web/icons.js` | `#/agents`、`bot`、`onAudit/onSession` | `#/index`、`layers`、`onIndex` | 追加式；`i18n.js` 两表各自追加在表尾 |
  | `web/screens/main.js` | A10 工作区标记 + "来自会话" | B12 搜索切换 + 索引检查器，B14 发送给 Agent | 只在 `renderInspector` 按钮行与搜索监听处接线；逻辑都在独立模块 |
  | `test/e2e/browser_helper_test.go` | A11 新建 | B13 新建 | **两线内容逐字相同**（本计划给出全文），git 对内容相同的 add/add 自动合并 |
  | `web/icons.js` `bot` | A0 | B14（仅当 A 未合入） | B14 写与 A0 **完全相同的一行**；合并时保留一份 |

---

## 代码锚点核对（HEAD `41f976d` + 当前工作区，2026-09-14）

| 规划文档中的锚点 | 实际 | 调整 |
|---|---|---|
| `mcpsrv.New` 已挂 receiving middleware `server.go:111-113` | `New` 在 `server.go:86-121`，`AddReceivingMiddleware(privateResourceResponses)` 112、`(s.subscriptions.receive)` 113 | 无 |
| `checkPath` `server.go:149`、`checkWrite` 162、`register` 397 | 一致 | 无 |
| "29 个工具" | **33 个** `mcp.AddTool`：`server.go` 17、`copy_jobs.go` 5、`upload_jobs.go` 7、`export_jobs.go` 4（`Options.Export != nil` 才注册）；`checkPath/checkWrite` 调用点 **36 处**：`server.go` 26、`resources.go` 2（137、266）、`copy_jobs.go` 2、`export_jobs.go` 3、`upload_jobs.go` 3 | 测试不写死数量，遍历 `tools/list` 并要求每个工具进"带路径表"或"无路径豁免表" |
| `http.go:65 Stateless: true`、`requireBearer` 165、`ClientConfig` 196 | 一致；令牌来源 `cmd/cloudfs/main.go:711` `mcpHTTPToken()` 读 `CLOUDFS_MCP_TOKEN` | 现有测试直接调用 `requireBearer(next, token)`（`http_test.go:35`、`copy_jobs_test.go:272`、`resources_test.go:339`、`subscriptions_test.go:84`）→ **保留其签名**，新增 `requireAuth` |
| SDK 身份 | `ServerOptions.InitializedHandler`（sdk `server.go:75`）、`ServerSession.ID()`（1531）、`InitializeParams()`（1959）、`Request.GetSession()`、`Request.GetExtra() *RequestExtra`（`TokenInfo`、`Header`）；`auth.RequireBearerToken` 把 `TokenInfo` 放进请求 ctx，streamable 传输转存到 `RequestExtra.TokenInfo`（sdk `streamable.go:1554`） | 令牌 principal 用 `TokenInfo.UserID` 传递 |
| `daemon.go:314-321` 非 owner 独立 VFS | `if !j.Owner()` 在 314，`return d, nil` 在 320 | agent.db 必须在 `if !opt.SkipWrite {`（约 285）之前打开，否则非 owner 提前返回拿不到 |
| `cmd/cloudfs/main.go:639 cmdMCP` | 工作区中 `cmdMCP` 在 **613**；`mcpInstall` 804；stdio `mcpsrv.New` 672；`serveMCPHTTPWith` 696；`cmdMount` 内 MCP HTTP 启动 563-570 | 以函数名定位 |
| `vfs.FS.ReadFileRange` read.go:502 / `DownloadURL` 543 / `Busy` vfs.go:1173 / `WriteFile` write.go:623 | 一致；另 `StatPath` vfs.go:576、`Mounts()` 367、`Cache()` 370、`Meta()` 373、`IsLocalOnly` write.go:928、`fromKernel` 1017、`prefetcher.waitIdle` read.go:723 | `vfs.Attr` 无 `RemoteID`，有 `Pinned`/`LocalOnly`；索引身份取 `meta.Node` |
| `internal/meta` 需新增 `Store.Identity()` | **已存在** `meta/copy.go:16` | B0 不新增，只新增 `WalkSubtree`（`ChildrenPage` 在 `children_page.go:13`） |
| `meta/store.go:236` 迁移、`schema.go` trigram | `migrate()` 236；`name_index` trigram `schema.go:59` | 无 |
| `journal/lock.go` flock | 一致；`internal/export/lock.go` 为同形副本 | agent/index 各复制 |
| `control/metrics.go:73` 路由表、`shared.go:29 confirmed()` | 一致 | 无 |
| `control/events.go` | `events` 35，现发 `status`/`change`/`export` | 加 `audit`/`session`/`index` |
| `i18n_contract_test.go` 覆盖 zh/en 键一致 | 实为服务端确认文案本地化测试；键一致性在 `ui_i18n_test.go:89` | 引用改为 `ui_i18n_test.go` |
| `browser_modules_test` 需登记新模块 | 自动 glob | 不登记 |
| `test/e2e` 的 `CLOUDFS_BROWSER=1` 冒烟 | **仓库内不存在** | A11/B13 新建 `test/e2e/browser_helper_test.go`：本机 Chrome `--headless=new --dump-dom` 渲染哈希路由，零新 Go 依赖 |
| e2e 挂载 | `test/e2e/e2e_test.go:62 newStack` 需 FUSE，`ui_api_test.go:19 uiCall` 走 httptest | 浏览器冒烟与 MCP 链路用**不挂载**的 `newAgentStack`；真实挂载断言另起用例 |
| 前端 `main.js` 检查器 238-243、搜索框 336-356 | `renderInspector` 226-245（按钮行 238-244）；搜索监听 336-356（`/search` 结果 `complete`） | 无 |
| `setup.js` 完成页 | `renderFinish` 588-613，两个分支都有 `href: '#/pool'` 按钮 | 两个分支都加卡片 |
| `app.js` 导航徽标 | `nav()` 68-78 无徽标支持；`events({...})` 129；`.nav a` 样式 `app.css:91-98` | A5 新增徽标渲染与 `.nav a .badge` 样式 |
| `cache.Complete` blockcache.go:831、`HydratedPath` 952、`FileKey{Remote,RemoteID,Version}` 25 | 一致 | 无 |
| `config.Config` 293、`MCP` 179、`Validate` 364、`IsSecretField` secrets.go:39 | 一致 | 一期不改 `IsSecretField`（`api_key` 属 T-39） |
| fakeprovider 计数"Get"/"Link" | 实际操作名 `ReadRange`（598）、`DownloadURL`（628）、`List`、`Stat`；`NewPathIDs` 可造 `Caps.PathIDs=true` | 验收里的 `Get` → `Calls("ReadRange")`，`Link` → `Calls("DownloadURL")` |
| doublestar glob | `go.mod` 无 doublestar | B6 自写 `internal/index/glob.go`（`**` 段 + `path.Match`），不加依赖 |
| `ledongthuc/pdf` | module download cache 只有 `.info/.mod`，无源码 zip | B3 需联网 `./gow get`；离线回退见 B3 |
| chaos "kill -9" | `test/chaos/chaos_test.go:167 TestKillDuringWriteLosesNothing` 以"不优雅关闭 + 同目录重开"模拟 | B13 照此写法 |
| Chrome / Node | `/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`；`node v24.16.0` | 无 |

---

## 文件结构总览

**线 A（新建）**
- `internal/agent/`：`db.go`（Open/迁移/Watch）、`lock.go` `lock_unix.go` `lock_windows.go`、`types.go`（Principal/Session/AuditRow/Event/Summary）、`scope.go`、`session.go`、`audit.go`、`redact.go`、`token.go`、`workspace.go`，各配 `_test.go`，`export_test.go`（包内测试钩子）。
- `internal/mcpsrv/`：`session_mw.go`、`audit_mw.go`、`session_tools.go`、`agent_env_test.go`、`scope_tools_test.go`、`audit_mw_test.go`、`token_http_test.go`、`client_config_test.go`、`session_tools_test.go`、`testdata/claude_http.golden`、`testdata/codex_http.golden`。
- `internal/control/`：`agent.go`（/audit /sessions）、`agent_client.go`、`mcp_tokens.go`（/mcp/connect /mcp/tokens）、`agent_test.go`、`mcp_tokens_test.go`、`ui_agents_test.go`、`ui_tokens_test.go`、`ui_sessions_test.go`。
- `internal/daemon/agent_view.go`（`control.AgentView`/`control.MCPView` 实现）+ `agent_test.go`。
- `cmd/cloudfs/agent.go`（`audit`、`sessions`）、`cmd/cloudfs/mcp_token.go`，各配测试。
- `internal/control/web/`：`scope_view.js`、`workspace_view.js`、`session_panel.js`、`token_reveal.js`、`connect_panel.js`、`screens/agents.js`、`screens/agents_sessions.js`、`screens/agents_audit.js`、`screens/agents_tokens.js`；`_tests/scope_view.test.mjs`、`_tests/workspace_view.test.mjs`。
- `test/e2e/browser_helper_test.go`、`test/e2e/agent_e2e_test.go`。

**线 A（修改）**：`internal/config/config.go`(+test)、`internal/mcpsrv/{server.go,resources.go,subscriptions.go,copy_jobs.go,export_jobs.go,upload_jobs.go,http.go}`、`internal/control/{metrics.go,status.go,events.go,events_test.go}`、`internal/i18n/{catalog_zh.go,catalog_en.go}`、`internal/daemon/daemon.go`、`cmd/cloudfs/main.go`、`internal/control/web/{icons.js,router.js,app.js,api.js,i18n.js,app.css,screens/main.js,screens/setup.js}`、`docs/mcp.md`、`docs/vfs-changes.md`、`README.md`、`TODO.md`。

**线 B（新建）**
- `internal/textract/`：`textract.go`（Kind/Doc/Options/Extract/KindOf）、`text.go`、`office.go`、`zipguard.go`、`pdf.go`、`chunk.go`，各配 `_test.go`，`pdf_fixture_test.go`（手写最小 PDF）。
- `internal/index/`：`store.go`、`schema.go`、`lock.go` `lock_unix.go` `lock_windows.go`、`glob.go`、`rules.go`、`budget.go`、`indexer.go`、`search.go`、`service.go`（`mcpsrv.IndexService`/`control.IndexControl` 实现），各配 `_test.go`。
- `internal/mcpsrv/index_tools.go` + `index_tools_test.go`。
- `internal/control/index.go`、`index_client.go`、`index_test.go`、`ui_index_test.go`、`ui_content_search_test.go`；可选 `agent_prompt.go` + `agent_prompt_test.go`、`ui_send_to_agent_test.go`。
- `cmd/cloudfs/index.go` + `index_test.go`。
- `internal/control/web/`：`snippet.js`、`index_presets.js`、`content_search.js`、`extracted_text.js`、`index_inspector.js`、`screens/index.js`；可选 `send_to_agent.js`；`_tests/snippet.test.mjs`、`_tests/index_presets.test.mjs`。
- `test/perf/index_test.go`、`test/chaos/index_test.go`、`test/e2e/index_e2e_test.go`、`test/e2e/browser_helper_test.go`（与线 A 逐字相同）。

**线 B（修改）**：`go.mod`/`go.sum`、`internal/config/config.go`(+test)、`internal/meta/children_page.go`（加 `WalkSubtree`）+ `walk_subtree_test.go`、`internal/mcpsrv/server.go`、`internal/control/{metrics.go,status.go,events.go,doctor.go}`、`internal/i18n/{catalog_zh.go,catalog_en.go}`、`internal/daemon/daemon.go`、`cmd/cloudfs/main.go`、`internal/control/web/{icons.js,router.js,app.js,api.js,i18n.js,screens/main.js}`、`docs/mcp.md`、`docs/DESIGN.md`、`README.md`、`TODO.md`。

---

# 线 A：T-34 → T-35 → T-36

## Task A0：agent.db 脚手架、共享类型、配置键、`bot` 图标

**Files:**
- Create: `internal/agent/db.go`、`internal/agent/types.go`、`internal/agent/lock.go`、`internal/agent/lock_unix.go`、`internal/agent/lock_windows.go`（照 `internal/export/lock*.go` 复制，`lockName = "agent.lock"`，错误前缀 `agent:`）
- Create: `internal/agent/db_test.go`
- Modify: `internal/config/config.go`（`MCP` 结构体 179 行附近；`Validate` 364 行附近调用 `c.MCP.validateAgent()`）、`internal/config/config_test.go`
- Modify: `internal/control/web/icons.js`（加 `bot`）

**Interfaces:**
- Consumes: 无。
- Produces:
```go
package agent

const schemaVersion = 1

type Store struct { /* db *sql.DB; lock *os.File; owner bool; readOnly bool; now func() time.Time;
                       watchMu sync.Mutex; watchers map[chan Event]struct{};
                       auditFailures atomic.Int64; appendFault func() error */ }

func Open(dir string) (*Store, error)          // dir = <cache.dir>/agent；MkdirAll 0700；取 flock，失败=非 owner 仍可读写（WAL 跨进程）
func OpenReadOnly(dir string) (*Store, error)  // ?mode=ro；agent.db 不存在返回 os.ErrNotExist
func (s *Store) Owner() bool
func (s *Store) Close() error
func (s *Store) Watch() (<-chan Event, func()) // 缓冲 64，满则丢最旧，不阻塞写路径
func (s *Store) publish(e Event)

// types.go
type Principal struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"` // stdio | env | loopback | token | console
	Name        string    `json:"name"`
	Scope       Scope     `json:"scope"`
	TokenPrefix string    `json:"fingerprint,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	RevokedAt   time.Time `json:"revoked_at,omitempty"`
	LastUsedAt  time.Time `json:"last_used_at,omitempty"`
}
type Session struct {
	ID            string    `json:"id"`
	PrincipalID   string    `json:"principal_id"`
	ConnKey       string    `json:"-"`
	ClientName    string    `json:"client"`
	ClientVersion string    `json:"client_version,omitempty"`
	Transport     string    `json:"transport"` // stdio | http-legacy | http-token | http-loopback
	Scope         Scope     `json:"scope"`
	Workspace     string    `json:"workspace,omitempty"`
	Sandbox       bool      `json:"sandbox"`
	State         string    `json:"state"` // active | finished | expired
	StartedAt     time.Time `json:"started_at"`
	LastSeenAt    time.Time `json:"last_seen_at"`
	FinishedAt    time.Time `json:"finished_at,omitempty"`
	Summary       string    `json:"summary,omitempty"`
	Writes        int       `json:"writes"`
	Artifacts     []Artifact `json:"artifacts,omitempty"`
}
type Artifact struct {
	Path        string    `json:"path"`
	URI         string    `json:"uri"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256,omitempty"`
	State       string    `json:"state"` // synced | local
	DownloadURL string    `json:"download_url,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
}
type AuditRow struct {
	ID          int64           `json:"id"`
	TS          time.Time       `json:"ts"`
	PrincipalID string          `json:"principal_id"`
	SessionID   string          `json:"session_id"`
	Transport   string          `json:"transport"`
	Tool        string          `json:"tool"`
	Paths       []string        `json:"paths"`
	Args        json.RawMessage `json:"args"`
	BytesIn     int64           `json:"bytes_in"`
	BytesOut    int64           `json:"bytes_out"`
	Result      string          `json:"result"` // ok | denied | error
	Error       string          `json:"error,omitempty"`
	DurationMS  int64           `json:"duration_ms"`
}
type Event struct {
	Kind        string    `json:"kind"` // audit | session
	Audit       *AuditRow `json:"audit,omitempty"`
	Session     *Session  `json:"session,omitempty"`
}
type Summary struct {
	Active      int `json:"active"`
	WritesToday int `json:"writes_today"`
	DeniedToday int `json:"denied_today"`
}
```
```go
// internal/config/config.go
type MCPAudit struct {
	Retain time.Duration `yaml:"retain"` // default 90 days
}
type MCPSession struct {
	Idle time.Duration `yaml:"idle"` // default 30m; token sessions rotate after this
}
// MCP gains: Audit MCPAudit `yaml:"audit"`; Session MCPSession `yaml:"session"`
```

- [ ] **Step 1: 写失败测试**

```go
// internal/agent/db_test.go
package agent

func TestOpenCreatesSchemaV1(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil { t.Fatal(err) }
	defer s.Close()
	if !s.Owner() { t.Fatal("the first opener must own agent.db") }
	for _, table := range []string{"principals", "sessions", "audit", "meta"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
	var v int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != schemaVersion { t.Fatalf("user_version = %d", v) }
}

func TestSecondOpenIsNotOwnerButCanWrite(t *testing.T) {
	dir := t.TempDir()
	a, _ := Open(dir); defer a.Close()
	b, err := Open(dir)
	if err != nil { t.Fatal(err) }
	defer b.Close()
	if b.Owner() { t.Fatal("two owners") }
	if _, err := b.db.Exec(`INSERT INTO audit(ts, tool, paths, args, result) VALUES (1, 'stat', '[]', '{}', 'ok')`); err != nil {
		t.Fatalf("a non-owner stdio process must still append audit rows: %v", err)
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.db.Exec(`PRAGMA user_version = 99`)
	s.Close()
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("want a newer-schema refusal, got %v", err)
	}
}

func TestOpenReadOnlyMissingIsNotExist(t *testing.T) {
	if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "agent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v", err)
	}
}

func TestWatchDoesNotBlockWhenNobodyReads(t *testing.T) {
	s, _ := Open(t.TempDir()); defer s.Close()
	_, stop := s.Watch(); defer stop()
	done := make(chan struct{})
	go func() { for i := 0; i < 1000; i++ { s.publish(Event{Kind: "audit"}) }; close(done) }()
	select { case <-done: case <-time.After(2 * time.Second): t.Fatal("publish blocked on a slow watcher") }
}
```
```go
// internal/config/config_test.go
func TestMCPAgentDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimalConfigYAML)) // reuse the file's existing minimal fixture
	if err != nil { t.Fatal(err) }
	if cfg.MCP.Audit.Retain != 90*24*time.Hour { t.Fatalf("retain = %v", cfg.MCP.Audit.Retain) }
	if cfg.MCP.Session.Idle != 30*time.Minute { t.Fatalf("idle = %v", cfg.MCP.Session.Idle) }
}
func TestMCPAgentNegativeDurationsAreRejected(t *testing.T) {
	_, err := Parse([]byte(minimalConfigYAML + "\nmcp:\n  audit:\n    retain: -1h\n"))
	if err == nil { t.Fatal("negative retain accepted") }
}
```
（若 `config_test.go` 没有名为 `minimalConfigYAML` 的夹具，先在文件内找现有最小 YAML 常量复用，名字以实际为准。）

- [ ] **Step 2: 运行确认失败**

Run: `./gow test ./internal/agent/ ./internal/config/ -run 'TestOpen|TestSecondOpen|TestNewerSchema|TestWatch|TestMCPAgent' -v`
Expected: FAIL（`agent` 包不存在 / `MCP.Audit` 未定义）

- [ ] **Step 3: 最小实现**

schema v1（`db.go` 中 `const schemaV1`，一次事务执行，写 `meta('schema_version','1')` 与 `PRAGMA user_version = 1`）：
```sql
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE principals (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL,
  token_hash TEXT NOT NULL DEFAULT '', token_prefix TEXT NOT NULL DEFAULT '',
  scope TEXT NOT NULL, created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0, revoked_at INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX principals_live_token_name ON principals(name) WHERE kind='token' AND revoked_at=0;
CREATE UNIQUE INDEX principals_kind_name ON principals(kind, name) WHERE kind IN ('stdio','env','loopback','console');
CREATE INDEX principals_token_hash ON principals(token_hash) WHERE token_hash != '';
CREATE TABLE sessions (
  id TEXT PRIMARY KEY, principal_id TEXT NOT NULL, conn_key TEXT NOT NULL,
  client_name TEXT NOT NULL DEFAULT '', client_version TEXT NOT NULL DEFAULT '',
  transport TEXT NOT NULL, scope TEXT NOT NULL, workspace TEXT NOT NULL DEFAULT '',
  sandbox INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL,
  started_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0, summary TEXT NOT NULL DEFAULT '',
  artifacts TEXT NOT NULL DEFAULT '[]');
CREATE INDEX sessions_conn_active ON sessions(conn_key) WHERE state='active';
CREATE INDEX sessions_started ON sessions(started_at DESC, id);
CREATE INDEX sessions_workspace ON sessions(workspace) WHERE workspace != '';
CREATE TABLE audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
  principal_id TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '',
  transport TEXT NOT NULL DEFAULT '', tool TEXT NOT NULL,
  paths TEXT NOT NULL, args TEXT NOT NULL,
  bytes_in INTEGER NOT NULL DEFAULT 0, bytes_out INTEGER NOT NULL DEFAULT 0,
  result TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', duration_ms INTEGER NOT NULL DEFAULT 0);
CREATE INDEX audit_ts ON audit(ts);
CREATE INDEX audit_session ON audit(session_id, id);
```
时间列一律 Unix 纳秒。`Open` 结构照 `export.OpenStore`（`internal/export/store.go:39-150`）。`Validate`：`Retain == 0 → 90*24h`、`Idle == 0 → 30m`，负值报 `config: mcp.audit.retain must not be negative`。`icons.js` 在 `download` 行后加：
```js
  bot: svg('<rect x="4" y="8" width="16" height="11" rx="2"/><path d="M12 4v4"/><circle cx="9" cy="13.5" r="1"/><circle cx="15" cy="13.5" r="1"/><path d="M9.5 17h5"/>'),
```

- [ ] **Step 4: 运行确认通过**

Run: `./gow test ./internal/agent/ ./internal/config/ -v && ./gow test ./internal/control/ -run TestIconsAreSizedAndDefined && ./gow vet ./internal/agent/`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/agent internal/config/config.go internal/config/config_test.go internal/control/web/icons.js
git commit -m "feat(agent): agent.db store, shared types and mcp.audit/session config"
```

---

## Task A1：`Scope.Check` / `Scope.Narrow`（T-34 验收"Narrow 表驱动"）

**Files:**
- Create: `internal/agent/scope.go`、`internal/agent/scope_test.go`

**Interfaces:**
- Consumes: 无。
- Produces:
```go
type Scope struct {
	Read      []string  `json:"read,omitempty"`  // empty = whole mount
	Write     []string  `json:"write"`           // nil = same as Read; empty non-nil = none
	ReadOnly  bool      `json:"read_only,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Sandbox   string    `json:"sandbox,omitempty"` // non-empty: write narrowed to this directory
}
var (
	ErrDenied   = errors.New("path is outside the allowed directories") // identical text to mcpsrv today
	ErrReadOnly = errors.New("this cloudfs MCP server is read-only")      // identical text to checkWrite today
	ErrExpired  = errors.New("this cloudfs access has expired")
)
func Normalise(p string) string                                  // path.Clean("/"+TrimPrefix(p,"/")), "" → "/"
func Under(p, prefix string) bool                                // prefix "/" covers all
func (s Scope) Check(p string, write bool) (string, error)        // CheckAt(p, write, time.Now())
func (s Scope) CheckAt(p string, write bool, now time.Time) (string, error)
func (s Scope) CheckWriteAllowed(now time.Time) error             // ReadOnly / expired only, no path
func (s Scope) Narrow(child Scope) Scope
func (s Scope) Covers(o Scope) bool                               // every read/write prefix of o is under s
func (s Scope) EffectiveRead() []string
func (s Scope) EffectiveWrite() []string                          // applies ReadOnly (→ empty) and Sandbox
```

- [ ] **Step 1: 写失败测试**

```go
// internal/agent/scope_test.go
func TestScopeCheck(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	work := Scope{Read: []string{"/work"}, Write: []string{"/work/.agent"}}
	for _, tc := range []struct {
		name  string
		s     Scope
		p     string
		write bool
		want  string
		err   error
	}{
		{"empty read is the whole mount", Scope{}, "/gd/x", false, "/gd/x", nil},
		{"child of read", work, "/work/a.md", false, "/work/a.md", nil},
		{"the prefix itself", work, "/work", false, "/work", nil},
		{"sibling with shared prefix", work, "/workshop/a", false, "", ErrDenied},
		{"dotdot escapes are cleaned first", work, "/work/../gd/x", false, "", ErrDenied},
		{"trailing slash", work, "/work/", false, "/work", nil},
		{"write inside write", work, "/work/.agent/s1/out.md", true, "/work/.agent/s1/out.md", nil},
		{"write outside write but inside read", work, "/work/notes.md", true, "", ErrDenied},
		{"nil write means same as read", Scope{Read: []string{"/work"}}, "/work/notes.md", true, "/work/notes.md", nil},
		{"empty write means none", Scope{Read: []string{"/work"}, Write: []string{}}, "/work/n", true, "", ErrDenied},
		{"read-only refuses writes with the old message", Scope{ReadOnly: true}, "/a", true, "", ErrReadOnly},
		{"sandbox narrows write", Scope{Read: []string{"/work"}, Sandbox: "/work/.agent/s1"}, "/work/notes.md", true, "", ErrDenied},
		{"sandbox keeps read", Scope{Read: []string{"/work"}, Sandbox: "/work/.agent/s1"}, "/work/notes.md", false, "/work/notes.md", nil},
		{"expired", Scope{ExpiresAt: now.Add(-time.Second)}, "/a", false, "", ErrExpired},
		{"root read", Scope{Read: []string{"/"}}, "/anything", false, "/anything", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.s.CheckAt(tc.p, tc.write, now)
			if !errors.Is(err, tc.err) || (tc.err == nil && got != tc.want) {
				t.Fatalf("CheckAt(%q, %v) = %q, %v; want %q, %v", tc.p, tc.write, got, err, tc.want, tc.err)
			}
		})
	}
}

func TestScopeNarrowNeverWidens(t *testing.T) {
	prefixes := [][]string{nil, {"/"}, {"/work"}, {"/work/"}, {"/work/a"}, {"/workshop"}, {"/gd"}, {"/work/../gd"}, {"/work", "/gd"}, {}}
	var scopes []Scope
	for _, r := range prefixes {
		for _, w := range prefixes {
			for _, ro := range []bool{false, true} {
				scopes = append(scopes, Scope{Read: r, Write: w, ReadOnly: ro}, Scope{Read: r, Write: w, Sandbox: "/work/a"})
			}
		}
	}
	for _, parent := range scopes {
		for _, child := range scopes {
			got := parent.Narrow(child)
			if !parent.Covers(got) {
				t.Fatalf("Narrow widened: parent %+v child %+v got %+v", parent, child, got)
			}
			for _, probe := range []string{"/", "/work", "/work/a/b", "/workshop/x", "/gd/x"} {
				for _, write := range []bool{false, true} {
					if _, err := got.Check(probe, write); err == nil {
						if _, perr := parent.Check(probe, write); perr != nil {
							t.Fatalf("narrowed scope allows %q write=%v that parent %+v denies", probe, write, parent)
						}
					}
				}
			}
		}
	}
}

func TestNarrowTakesTheEarlierExpiry(t *testing.T) {
	a := time.Unix(100, 0); b := time.Unix(200, 0)
	if got := (Scope{ExpiresAt: b}).Narrow(Scope{ExpiresAt: a}); !got.ExpiresAt.Equal(a) { t.Fatal(got.ExpiresAt) }
	if got := (Scope{ExpiresAt: a}).Narrow(Scope{}); !got.ExpiresAt.Equal(a) { t.Fatal(got.ExpiresAt) }
}
```

- [ ] **Step 2: 运行确认失败**

Run: `./gow test ./internal/agent/ -run 'TestScope|TestNarrow' -v`
Expected: FAIL（`Scope` 未定义）

- [ ] **Step 3: 最小实现**

```go
// internal/agent/scope.go
func Normalise(p string) string {
	if p == "" { return "/" }
	return path.Clean("/" + strings.TrimPrefix(p, "/"))
}
func Under(p, prefix string) bool {
	return prefix == "/" || p == prefix || strings.HasPrefix(p, prefix+"/")
}
func clean(list []string) []string {
	if list == nil { return nil }
	out := make([]string, 0, len(list))
	for _, p := range list { out = append(out, Normalise(p)) }
	return out
}
func (s Scope) EffectiveRead() []string {
	if len(s.Read) == 0 { return []string{"/"} }
	return clean(s.Read)
}
func (s Scope) EffectiveWrite() []string {
	if s.ReadOnly { return []string{} }
	w := s.EffectiveRead()
	if s.Write != nil { w = clean(s.Write) }
	if s.Sandbox != "" { w = intersect(w, []string{Normalise(s.Sandbox)}) }
	return w
}
// intersect keeps, for every pair, the deeper of two nested prefixes.
func intersect(a, b []string) []string {
	out := []string{}
	for _, x := range a {
		for _, y := range b {
			switch {
			case Under(y, x): out = append(out, y)
			case Under(x, y): out = append(out, x)
			}
		}
	}
	return dedupe(out)
}
func (s Scope) CheckAt(p string, write bool, now time.Time) (string, error) {
	if !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt) { return "", ErrExpired }
	c := Normalise(p)
	if write {
		if s.ReadOnly { return "", ErrReadOnly }
		for _, w := range s.EffectiveWrite() { if Under(c, w) { return c, nil } }
		return "", fmt.Errorf("%w: %s", ErrDenied, c)
	}
	for _, r := range s.EffectiveRead() { if Under(c, r) { return c, nil } }
	return "", fmt.Errorf("%w: %s", ErrDenied, c)
}
func (s Scope) Check(p string, write bool) (string, error) { return s.CheckAt(p, write, time.Now()) }
func (s Scope) CheckWriteAllowed(now time.Time) error {
	if !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt) { return ErrExpired }
	if s.ReadOnly { return ErrReadOnly }
	return nil
}
func (s Scope) Narrow(c Scope) Scope {
	out := Scope{
		Read:     intersect(s.EffectiveRead(), c.EffectiveRead()),
		Write:    intersect(s.EffectiveWrite(), c.EffectiveWrite()),
		ReadOnly: s.ReadOnly || c.ReadOnly,
		ExpiresAt: earliest(s.ExpiresAt, c.ExpiresAt),
	}
	if len(out.Read) == 0 {
		// Nothing readable. An empty Read means "whole mount", so return a
		// scope that is read-only and already expired: every check refuses.
		return Scope{Read: []string{}, Write: []string{}, ReadOnly: true, ExpiresAt: time.Unix(1, 0)}
	}
	return out
}
```
注意：`EffectiveRead` 把空 `Read` 当作整个挂载，所以"交集为空"不能用空切片表达。实现取：`Narrow` 结果为空时返回 `Scope{Read: []string{}, Write: []string{}, ReadOnly: true, ExpiresAt: time.Unix(1, 0)}`（立即过期 = 全拒），并在注释写明原因；`TestScopeNarrowNeverWidens` 覆盖这一分支（`{"/work"}` 与 `{"/gd"}` 相交）。`Covers` 对"已过期"的 o 视为被任何 s 覆盖。

- [ ] **Step 4: 运行确认通过**

Run: `./gow test -race ./internal/agent/ -run 'TestScope|TestNarrow' -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/agent/scope.go internal/agent/scope_test.go
git commit -m "feat(agent): read/write scope with narrow-only composition"
```

---

## Task A2：会话解析 middleware 与 `checkPath(ctx, p, write)`（T-34 做法第 2 点）

**Files:**
- Create: `internal/agent/session.go`、`internal/agent/session_test.go`
- Create: `internal/mcpsrv/session_mw.go`、`internal/mcpsrv/agent_env_test.go`、`internal/mcpsrv/scope_tools_test.go`
- Modify: `internal/mcpsrv/server.go`（`Options` 加字段；`New` 86-121；`checkPath` 149；`checkWrite` 162；26 个调用点）、`internal/mcpsrv/resources.go`（137、266）、`internal/mcpsrv/subscriptions.go`（`reserve` 134 → 带 ctx）、`internal/mcpsrv/copy_jobs.go`（103、209）、`internal/mcpsrv/export_jobs.go`（229、239、316）、`internal/mcpsrv/upload_jobs.go`（116、218、290）

**Interfaces:**
- Consumes: A0 `Store`/`Session`/`Principal`；A1 `Scope`、`ErrDenied`、`ErrReadOnly`。
- Produces:
```go
// internal/agent/session.go
type SessionOptions struct {
	Idle time.Duration      // token sessions rotate after this idle gap
	Now  func() time.Time
}
type ConnInfo struct {
	Key           string // "stdio:%p" | "legacy:<sdk id>" | "token:<principal id>" | "loopback:%p"
	Transport     string // stdio | http-legacy | http-token | http-loopback
	PrincipalID   string
	ClientName    string
	ClientVersion string
}
type ListQuery struct {
	Cursor  string
	Limit   int    // default 50, max 200
	State   string // "" | active | finished | expired
	Sandbox bool
	Path    string // A9: sessions whose workspace contains this path
}
type Sessions struct{ /* store *Store; opt SessionOptions; mu sync.Mutex; principals map[string]Principal */ }

func NewSessions(st *Store, opt SessionOptions) *Sessions
func (m *Sessions) Store() *Store
func (m *Sessions) EnsurePrincipal(ctx context.Context, kind, name string, sc Scope) (Principal, error) // idempotent on (kind,name); updates scope
func (m *Sessions) Principal(ctx context.Context, id string) (Principal, error)
func (m *Sessions) Resolve(ctx context.Context, c ConnInfo) (Session, error)
func (m *Sessions) Get(ctx context.Context, id string) (Session, error)
func (m *Sessions) List(ctx context.Context, q ListQuery) ([]Session, string, error)
func (m *Sessions) Finish(ctx context.Context, id, summary string) (Session, error)
func (m *Sessions) Summary(ctx context.Context, since time.Time) (Summary, error)
var ErrSessionNotFound = errors.New("agent: session not found")

func WithSession(ctx context.Context, s Session) context.Context
func FromContext(ctx context.Context) (Session, bool)
```
```go
// internal/mcpsrv/server.go
type Options struct {
	// ...existing fields...
	// Sessions, when set, gives every call a CloudFS session and scope. nil
	// keeps today's behaviour: one process-wide scope from Allow/ReadOnly.
	Sessions *agent.Sessions
	// Scope overrides the scope derived from Allow/ReadOnly. nil derives it.
	Scope *agent.Scope
}
func (s *Server) checkPath(ctx context.Context, p string, write bool) (string, error)
func (s *Server) checkWrite(ctx context.Context) error
func (s *Server) scopeOf(ctx context.Context) agent.Scope
var ErrDenied = agent.ErrDenied // same message as before
```

- [ ] **Step 1: 写失败测试（agent）**

```go
// internal/agent/session_test.go
func newSessions(t *testing.T, now *time.Time) *Sessions {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { st.Close() })
	return NewSessions(st, SessionOptions{Idle: 30 * time.Minute, Now: func() time.Time { return *now }})
}

func TestResolveReusesTheActiveSessionForAConnection(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	p, _ := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{Read: []string{"/work"}})
	c := ConnInfo{Key: "stdio:0x1", Transport: "stdio", PrincipalID: p.ID, ClientName: "claude-code"}
	a, err := m.Resolve(context.Background(), c)
	if err != nil { t.Fatal(err) }
	b, _ := m.Resolve(context.Background(), c)
	if a.ID != b.ID || a.State != "active" || a.ClientName != "claude-code" { t.Fatalf("%+v %+v", a, b) }
	if len(a.Scope.EffectiveRead()) != 1 || a.Scope.EffectiveRead()[0] != "/work" { t.Fatalf("scope %+v", a.Scope) }
}

func TestTokenSessionRotatesAfterIdle(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	p, _ := m.EnsurePrincipal(context.Background(), "env", "env", Scope{})
	c := ConnInfo{Key: "token:" + p.ID, Transport: "http-token", PrincipalID: p.ID}
	a, _ := m.Resolve(context.Background(), c)
	now = now.Add(31 * time.Minute)
	b, _ := m.Resolve(context.Background(), c)
	if a.ID == b.ID { t.Fatal("an idle token session must rotate") }
	old, _ := m.Get(context.Background(), a.ID)
	if old.State != "expired" { t.Fatalf("old state %q", old.State) }
}

func TestFinishIsTerminalAndTheNextCallStartsANewSession(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	p, _ := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{})
	c := ConnInfo{Key: "stdio:0x2", Transport: "stdio", PrincipalID: p.ID}
	a, _ := m.Resolve(context.Background(), c)
	if _, err := m.Finish(context.Background(), a.ID, "done"); err != nil { t.Fatal(err) }
	b, _ := m.Resolve(context.Background(), c)
	if b.ID == a.ID { t.Fatal("a finished session was reused") }
}

func TestListSessionsPagesByCursor(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	p, _ := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{})
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		m.Resolve(context.Background(), ConnInfo{Key: fmt.Sprintf("stdio:%d", i), Transport: "stdio", PrincipalID: p.ID})
	}
	page1, cur, _ := m.List(context.Background(), ListQuery{Limit: 2})
	page2, _, _ := m.List(context.Background(), ListQuery{Limit: 2, Cursor: cur})
	if len(page1) != 2 || len(page2) != 2 || cur == "" || page1[0].ID == page2[0].ID { t.Fatalf("%v %v %q", page1, page2, cur) }
}
```

- [ ] **Step 2: 写失败测试（mcpsrv）**

```go
// internal/mcpsrv/agent_env_test.go
package mcpsrv

// newAgentEnv is newEnv with a CloudFS session layer in front of the tools.
func newAgentEnv(t *testing.T, opt Options, sc agent.Scope) (*env, *agent.Store) {
	t.Helper()
	st, err := agent.Open(t.TempDir())
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { st.Close() })
	opt.Sessions = agent.NewSessions(st, agent.SessionOptions{Idle: 30 * time.Minute})
	opt.Scope = &sc
	return newEnv(t, opt), st
}
```
```go
// internal/mcpsrv/scope_tools_test.go
func TestStdioInitializeRecordsOneSession(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	_ = e
	m := agent.NewSessions(st, agent.SessionOptions{})
	deadline := time.Now().Add(2 * time.Second)
	for {
		list, _, _ := m.List(context.Background(), agent.ListQuery{State: "active"})
		if len(list) == 1 && list[0].ClientName == "test" && list[0].Transport == "stdio" { return }
		if time.Now().After(deadline) { t.Fatalf("sessions after initialize: %+v", list) }
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWriteScopeIsSeparateFromReadScope(t *testing.T) {
	e, _ := newAgentEnv(t, Options{}, agent.Scope{Read: []string{"/"}, Write: []string{"/work"}})
	e.fake.Seed("other/a.txt", []byte("hello"))
	if res := e.call(t, "read_text", map[string]any{"path": "/other/a.txt"}, nil); res.IsError {
		t.Fatalf("read outside write scope refused: %s", errText(res))
	}
	res := e.call(t, "write_file", map[string]any{"path": "/other/b.txt", "content": "x"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
		t.Fatalf("write outside write scope: %v %s", res.IsError, errText(res))
	}
}

// TestEveryToolChecksItsPaths walks tools/list so that a tool added later has
// to be classified here: either it takes a path and that path is checked, or it
// is listed as path-less with a reason.
func TestEveryToolChecksItsPaths(t *testing.T) {
	e, _ := newAgentEnv(t, Options{Export: nil}, agent.Scope{Read: []string{"/work"}})
	e.fake.Seed("gd/x.txt", []byte("secret"))
	outside := map[string]map[string]any{
		"list_directory":   {"path": "/gd"},
		"stat":             {"path": "/gd/x.txt"},
		"stat_many":        {"paths": []string{"/gd/x.txt"}},
		"read_text":        {"path": "/gd/x.txt"},
		"read_range":       {"path": "/gd/x.txt", "offset": 0, "length": 4},
		"search":           {"query": "x", "path": "/gd"},
		"cache_status":     {"path": "/gd/x.txt"},
		"get_download_url": {"path": "/gd/x.txt"},
		"write_file":       {"path": "/gd/y.txt", "content": "x"},
		"edit_file":        {"path": "/gd/x.txt", "edits": []map[string]any{{"old_text": "secret", "new_text": "s"}}},
		"create_directory": {"path": "/gd/new"},
		"copy":             {"from": "/gd/x.txt", "to": "/work/x.txt"},
		"move":             {"from": "/work/nope.txt", "to": "/gd/y.txt"},
		"delete":           {"path": "/gd/x.txt", "confirm": true},
		"pin":              {"path": "/gd/x.txt"},
		"unpin":            {"path": "/gd/x.txt"},
	}
	// Path-less tools: they take ids or nothing, and filter their results by
	// scope internally (list_roots, upload/copy job listings).
	pathless := map[string]string{
		"list_roots": "filters mounts by scope", "flush_uploads": "no path",
		"list_uploads": "filters by scope", "get_upload": "filters by scope", "retry_upload": "write gate only",
		"cancel_upload": "write gate only", "resume_upload": "write gate only", "discard_upload": "write gate only",
		"list_copy_jobs": "filters by scope", "get_copy_job": "filters by scope", "retry_copy_job": "write gate only",
		"cancel_copy_job": "write gate only", "forget_copy_job": "write gate only",
	}
	tools, err := e.session.ListTools(context.Background(), nil)
	if err != nil { t.Fatal(err) }
	for _, tool := range tools.Tools {
		args, ok := outside[tool.Name]
		if !ok {
			if _, exempt := pathless[tool.Name]; !exempt {
				t.Errorf("tool %q is in neither table: classify it", tool.Name)
			}
			continue
		}
		res := e.call(t, tool.Name, args, nil)
		if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
			t.Errorf("%s reached outside /work: IsError=%v %q", tool.Name, res.IsError, errText(res))
		}
	}
}

func TestSubscribeOutsideScopeIsDenied(t *testing.T) {
	// Mirror the subscription setup of subscriptions_test.go (resources/subscribe
	// on a cloudfs:// URI) with Scope{Read: ["/work"]}; subscribing to /gd/x.txt
	// must fail with the same "outside the allowed" error, /work/a.txt must succeed.
}
```
`TestSubscribeOutsideScopeIsDenied` 的具体请求照 `internal/mcpsrv/subscriptions_test.go` 现有订阅用例的请求构造逐行复制，只换 Options 与 URI；不得留空函数体提交。`outside` 表的键名以各 `*Input` 结构体 json tag 为准（已核对：`listInput.path`、`statManyInput.paths`、`readRangeInput.offset/length`、`writeInput.content`、`editInput.edits`、`moveInput.from/to`、`deleteInput.confirm`、`searchInput.query/path`、`pinInput.path`）；`editSpec` 字段名执行前 `grep -n "type editSpec" -A 4 internal/mcpsrv/server.go` 复核。若 `cache_status` 实际无 path 参数，移到 `pathless` 并写原因。

- [ ] **Step 3: 运行确认失败**

Run: `./gow test ./internal/agent/ -run 'TestResolve|TestTokenSession|TestFinishIs|TestListSessions' -v; ./gow test ./internal/mcpsrv/ -run 'TestStdioInitialize|TestWriteScope|TestEveryTool|TestSubscribeOutside' -v`
Expected: FAIL（`NewSessions`、`Options.Sessions` 未定义）

- [ ] **Step 4: 实现 agent.Sessions**

- `Resolve`：`SELECT ... FROM sessions WHERE conn_key=? AND state='active'`；命中且 `Transport` 为 `http-token` 或 `http-loopback` 且 `now-last_seen > Idle` → `UPDATE state='expired'` 后新建；命中其余 → `last_seen_at` 距上次 ≥ 30 s 才 UPDATE（减少写放大）；未命中 → 取 principal scope 新建（`id = uuid.NewString()`，`github.com/google/uuid` 已是依赖），`publish(Event{Kind:"session"})`。
- `List` 游标：`base64(started_at_ns + ":" + id)`，按 `(started_at DESC, id)` 翻页，与 `/uploads` 的 `next_cursor` 语义一致。
- `Summary(since)`：`active = count(state='active')`；`writes_today = count(audit WHERE ts>=since AND result='ok' AND tool IN writeTools)`；`denied_today = count(audit WHERE ts>=since AND result='denied')`。`writeTools` 定义为包级 `var WriteTools = map[string]bool{"write_file":true,"edit_file":true,"create_directory":true,"copy":true,"move":true,"delete":true}`（A3 同用）。

- [ ] **Step 5: 实现 mcpsrv 接线**

```go
// internal/mcpsrv/session_mw.go
func (s *Server) sessionMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if s.opt.Sessions == nil || method == "initialize" {
			return next(ctx, method, req)
		}
		sess, err := s.resolveSession(ctx, req)
		if err != nil {
			return nil, err
		}
		return next(agent.WithSession(ctx, sess), method, req)
	}
}

func (s *Server) resolveSession(ctx context.Context, req mcp.Request) (agent.Session, error) {
	c := agent.ConnInfo{}
	ss := req.GetSession()
	if p := serverSessionParams(ss); p != nil && p.ClientInfo != nil {
		c.ClientName, c.ClientVersion = p.ClientInfo.Name, p.ClientInfo.Version
	}
	extra := req.GetExtra()
	switch {
	case extra != nil && extra.TokenInfo != nil && extra.TokenInfo.UserID != "":
		c.Key, c.Transport, c.PrincipalID = "token:"+extra.TokenInfo.UserID, "http-token", extra.TokenInfo.UserID
	case ss != nil && sessionID(ss) != "":
		c.Key, c.Transport, c.PrincipalID = "legacy:"+sessionID(ss), "http-legacy", s.defaultPrincipal.ID
	case extra != nil && extra.Header != nil:
		// Stateless HTTP without a bearer token (loopback, no token issued yet):
		// every POST is a fresh SDK session, so the connection key is the
		// principal and the session rotates on idle like a token session.
		c.Key, c.Transport, c.PrincipalID = "http:"+s.defaultPrincipal.ID, "http-loopback", s.defaultPrincipal.ID
	default:
		c.Key, c.Transport, c.PrincipalID = fmt.Sprintf("stdio:%p", ss), "stdio", s.defaultPrincipal.ID
	}
	return s.opt.Sessions.Resolve(ctx, c)
}
```
`serverSessionParams`/`sessionID` 用类型断言 `ss.(*mcp.ServerSession)` 取 `InitializeParams()`/`ID()`。`New` 中：
```go
s.defaultScope = agent.Scope{Read: opt.Allow, ReadOnly: opt.ReadOnly}
if opt.Scope != nil { s.defaultScope = *opt.Scope }
if opt.Sessions != nil {
	p, err := opt.Sessions.EnsurePrincipal(context.Background(), "stdio", "local", s.defaultScope)
	if err != nil { return nil, fmt.Errorf("mcpsrv: agent principal: %w", err) }
	s.defaultPrincipal = p
}
// ServerOptions gains InitializedHandler: s.onInitialized (resolves once so the
// session is visible before the first tool call).
s.mcp.AddReceivingMiddleware(s.sessionMiddleware) // must run before privateResourceResponses/subscriptions.receive
```
中间件顺序：把 `s.sessionMiddleware` 放在现有两次 `AddReceivingMiddleware` **之前**添加；用 `TestSubscribeOutsideScopeIsDenied`（订阅走 `subscriptions.receive`，需要 ctx 里已有会话）锁定顺序，不凭记忆判断 SDK 包装方向——若测试显示订阅拿不到会话，就调换添加顺序。

`checkPath`：
```go
func (s *Server) scopeOf(ctx context.Context) agent.Scope {
	if sess, ok := agent.FromContext(ctx); ok { return sess.Scope }
	return s.defaultScope
}
func (s *Server) checkPath(ctx context.Context, p string, write bool) (string, error) {
	return s.scopeOf(ctx).Check(p, write)
}
func (s *Server) checkWrite(ctx context.Context) error {
	return s.scopeOf(ctx).CheckWriteAllowed(time.Now())
}
```
机械替换规则：
- 形如 `if err := s.checkWrite(); err != nil {...}` 紧跟 `p, err := s.checkPath(in.Path)` → 删除前者，后者改 `s.checkPath(ctx, in.Path, true)`（server.go 693/697、731/735、794/798、893/897）。
- `move`/`copy`（839/843/848、871/875/880）：删 `checkWrite`，`from` 与 `to` **都**用 `write=true`（move 源端被删除，copy 目的端被写；copy 源端按设计也按写检查，保持"两端按写"）。
- 其余读工具 → `write=false`；`list_roots`（1081）的过滤 → `s.checkPath(ctx, m.Prefix, false)`。
- `search` 的 allow 交集（server.go 943-952）改为基于 `s.scopeOf(ctx).EffectiveRead()` 计算，抽成 `func (s *Server) readRoots(ctx context.Context, root string) []string`（B8 复用）。
- 独立 `checkWrite()`（copy_jobs 209、export_jobs 229/316、upload_jobs 218/290）→ `s.checkWrite(ctx)`。
- `resources.go` 137/266：`parseResource` 与其调用链加 `ctx context.Context` 首参；`subscriptions.go` `reserve(token, uris)` → `reserve(ctx, token, uris)`，调用处传 hook 的 ctx。

- [ ] **Step 6: 运行确认通过（含现有测试零修改）**

Run: `./gow test -race ./internal/agent/ ./internal/mcpsrv/ -count=1`
Expected: PASS，且 `git diff --stat -- internal/mcpsrv/*_test.go` 只显示本任务新增的 `agent_env_test.go`、`scope_tools_test.go`（验收"`--allow /work` 且不启用 sessions 时现有测试零修改通过"）。

- [ ] **Step 7: 提交**

```bash
git add internal/agent/session.go internal/agent/session_test.go internal/mcpsrv
git commit -m "feat(mcpsrv): per-session scope with separate read and write checks"
```

---

## Task A3：审计 middleware、脱敏、失败计数、保留期清理（T-34 做法第 3 点）

**Files:**
- Create: `internal/agent/audit.go`、`internal/agent/audit_test.go`、`internal/agent/redact.go`、`internal/agent/redact_test.go`、`internal/agent/export_test.go`
- Create: `internal/mcpsrv/audit_mw.go`、`internal/mcpsrv/audit_mw_test.go`
- Modify: `internal/mcpsrv/server.go`（`New` 注册 middleware；`checkPath` 记录路径）

**Interfaces:**
- Consumes: A0 `AuditRow`、`Event`；A2 `Sessions`、`FromContext`、`WriteTools`。
- Produces:
```go
// internal/agent/audit.go
type AuditQuery struct {
	Cursor  string
	Limit   int       // default 200, max 1000
	Session string
	Tool    string
	Result  string    // ok | denied | error
	Since   time.Time
}
func (s *Store) AppendAudit(ctx context.Context, row AuditRow) (int64, error) // publishes Event{Kind:"audit"}
func (s *Store) Audit(ctx context.Context, q AuditQuery) ([]AuditRow, string, error) // id DESC; cursor = last id
func (s *Store) AuditForSession(ctx context.Context, sessionID string, limit int) ([]AuditRow, error)
func (s *Store) PurgeAudit(ctx context.Context, before time.Time) (int64, error)
func (s *Store) AuditWriteFailures() int64
const MaxAuditArgs = 4 << 10

// internal/agent/redact.go
func RedactArgs(raw json.RawMessage) (args json.RawMessage, bytesIn int64)

// internal/agent/export_test.go (package agent)
func (s *Store) setAppendFault(f func() error) { s.appendFault = f }
```
```go
// internal/mcpsrv/server.go
type AuditWriter interface {
	AppendAudit(ctx context.Context, row agent.AuditRow) (int64, error)
}
// Options gains: Audit AuditWriter // nil = Sessions.Store(); both nil = no audit
```

- [ ] **Step 1: 写失败测试（agent）**

```go
func TestRedactArgsKeepsLargeContentOutAndBounded(t *testing.T) {
	big := strings.Repeat("a", 204800)
	raw, _ := json.Marshal(map[string]any{"path": "/work/a.md", "content": big + " see https://cdn.example/x?sig=1"})
	args, n := RedactArgs(raw)
	if n != int64(len(big)+len(" see https://cdn.example/x?sig=1")) { t.Fatalf("bytes_in = %d", n) }
	if len(args) > MaxAuditArgs { t.Fatalf("args %d bytes", len(args)) }
	if bytes.Contains(args, []byte("aaaa")) || bytes.Contains(args, []byte("http")) { t.Fatalf("leaked: %s", args) }
	if !bytes.Contains(args, []byte(`"/work/a.md"`)) { t.Fatalf("path dropped: %s", args) }
}

func TestRedactArgsRedactsEditsTokensAndURLs(t *testing.T) {
	raw := []byte(`{"path":"/w","edits":[{"old_text":"secret-old","new_text":"secret-new"}],"token":"t","cookie":"c","note":"http://x"}`)
	args, _ := RedactArgs(raw)
	for _, leak := range []string{"secret-old", "secret-new", `"t"`, `"c"`, "http"} {
		if bytes.Contains(args, []byte(leak)) { t.Fatalf("leaked %q: %s", leak, args) }
	}
}

func TestRedactArgsTruncatesAWideObject(t *testing.T) {
	m := map[string]any{}
	for i := 0; i < 500; i++ { m[fmt.Sprintf("k%03d", i)] = strings.Repeat("v", 30) }
	raw, _ := json.Marshal(m)
	args, _ := RedactArgs(raw)
	if len(args) > MaxAuditArgs || !bytes.Contains(args, []byte(`"truncated":true`)) { t.Fatalf("%d %s", len(args), args[:80]) }
}

func TestAuditWriteFailureIsCountedNotReturnedToTheTool(t *testing.T) {
	s, _ := Open(t.TempDir()); defer s.Close()
	s.setAppendFault(func() error { return errors.New("disk full") })
	if _, err := s.AppendAudit(context.Background(), AuditRow{Tool: "stat", Result: "ok"}); err == nil { t.Fatal("fault not surfaced to caller") }
	if s.AuditWriteFailures() != 1 { t.Fatalf("failures = %d", s.AuditWriteFailures()) }
}

func TestAuditPagesByIDAndFilters(t *testing.T) { /* append 5 rows (2 denied), Audit{Limit:2} then cursor; Audit{Result:"denied"} returns 2 */ }
func TestPurgeAuditDropsOldRows(t *testing.T) { /* rows at ts 100 and now; PurgeAudit(before=200) removes 1 */ }
```
后两个测试按注释写完整断言再提交（插入用 `AppendAudit` 带显式 `TS`）。

- [ ] **Step 2: 写失败测试（mcpsrv）**

```go
// internal/mcpsrv/audit_mw_test.go
func auditRows(t *testing.T, st *agent.Store) []agent.AuditRow {
	t.Helper()
	rows, _, err := st.Audit(context.Background(), agent.AuditQuery{Limit: 100})
	if err != nil { t.Fatal(err) }
	return rows
}

func TestDeniedDeleteIsAuditedWithoutContent(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{Read: []string{"/work"}})
	e.fake.Seed("gd/x.txt", []byte("secret"))
	e.call(t, "delete", map[string]any{"path": "/gd/x.txt", "confirm": true}, nil)
	e.call(t, "write_file", map[string]any{"path": "/gd/y.txt", "content": "see https://signed.example/a?sig=1"}, nil)
	rows := auditRows(t, st)
	var del, wr *agent.AuditRow
	for i := range rows {
		switch rows[i].Tool { case "delete": del = &rows[i]; case "write_file": wr = &rows[i] }
	}
	if del == nil || del.Result != "denied" || len(del.Paths) != 1 || del.Paths[0] != "/gd/x.txt" { t.Fatalf("delete row %+v", del) }
	if wr == nil || wr.Result != "denied" || bytes.Contains(wr.Args, []byte("signed.example")) || bytes.Contains(wr.Args, []byte("http")) {
		t.Fatalf("write row %+v", wr)
	}
	if del.SessionID == "" { t.Fatal("the audit row has no session: middleware order is wrong") }
}

func TestLargeWriteAuditArgsStayBounded(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	e.call(t, "write_file", map[string]any{"path": "/a.txt", "content": strings.Repeat("z", 204800)}, nil)
	for _, r := range auditRows(t, st) {
		if r.Tool == "write_file" {
			if len(r.Args) > agent.MaxAuditArgs || r.BytesIn != 204800 || r.Result != "ok" { t.Fatalf("%d %d %s", len(r.Args), r.BytesIn, r.Result) }
			return
		}
	}
	t.Fatal("no write_file audit row")
}

type failingAudit struct{ n atomic.Int64 }
func (f *failingAudit) AppendAudit(context.Context, agent.AuditRow) (int64, error) { f.n.Add(1); return 0, errors.New("agent.db is read-only") }

func TestAuditFailureDoesNotFailTheTool(t *testing.T) {
	fa := &failingAudit{}
	e, _ := newAgentEnv(t, Options{Audit: fa}, agent.Scope{})
	if res := e.call(t, "write_file", map[string]any{"path": "/a.txt", "content": "x"}, nil); res.IsError {
		t.Fatalf("tool failed because audit failed: %s", errText(res))
	}
	if fa.n.Load() == 0 { t.Fatal("audit was never attempted") }
}

func TestInitializeAndSubscribeAreAudited(t *testing.T) { /* after newAgentEnv: a row tool="initialize"; after a resources/subscribe (copy request from subscriptions_test.go) a row tool="resources/subscribe" */ }
```

- [ ] **Step 3: 运行确认失败**

Run: `./gow test ./internal/agent/ -run 'TestRedact|TestAudit|TestPurge' -v; ./gow test ./internal/mcpsrv/ -run 'TestDeniedDelete|TestLargeWrite|TestAuditFailure|TestInitializeAndSubscribe' -v`
Expected: FAIL

- [ ] **Step 4: 实现**

```go
// internal/mcpsrv/audit_mw.go
type callNote struct {
	mu     sync.Mutex
	paths  []string
	denied bool
}
type callNoteKey struct{}

func noteFrom(ctx context.Context) *callNote { n, _ := ctx.Value(callNoteKey{}).(*callNote); return n }

func (s *Server) auditMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		w := s.auditWriter()
		audited := method == "tools/call" || method == "initialize" || method == "resources/subscribe" || method == "subscriptions/listen"
		if w == nil || !audited {
			return next(ctx, method, req)
		}
		note := &callNote{}
		start := time.Now()
		res, err := next(context.WithValue(ctx, callNoteKey{}, note), method, req)
		row := agent.AuditRow{TS: start, Tool: method, DurationMS: time.Since(start).Milliseconds()}
		if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil {
			row.Tool = call.Params.Name
			raw, _ := json.Marshal(call.Params.Arguments)
			row.Args, row.BytesIn = agent.RedactArgs(raw)
		}
		if sess, ok := agent.FromContext(ctx); ok {
			row.SessionID, row.PrincipalID, row.Transport = sess.ID, sess.PrincipalID, sess.Transport
		}
		note.mu.Lock()
		row.Paths, row.Result = append([]string{}, note.paths...), "ok"
		denied := note.denied
		note.mu.Unlock()
		switch r, _ := res.(*mcp.CallToolResult); {
		case denied:
			row.Result = "denied"
		case err != nil:
			row.Result, row.Error = "error", err.Error()
		case r != nil && r.IsError:
			row.Result, row.Error = "error", firstText(r)
		}
		if r, ok := res.(*mcp.CallToolResult); ok && r != nil {
			row.BytesOut = contentBytes(r)
		}
		if _, werr := w.AppendAudit(context.WithoutCancel(ctx), row); werr != nil {
			slog.Warn("mcp audit write failed", "tool", row.Tool, "err", werr)
		}
		return res, err
	}
}
```
`checkPath`/`checkWrite` 在返回前调用 `recordCheck(ctx, clean, p, err)`：有 note 时追加 `clean`（被拒时追加 `agent.Normalise(p)`），`errors.Is(err, agent.ErrDenied) || errors.Is(err, agent.ErrReadOnly) || errors.Is(err, agent.ErrExpired)` 时置 `denied=true`。`auditMiddleware` 添加在 `sessionMiddleware` 的**内层**（ctx 中已有会话；`TestDeniedDeleteIsAuditedWithoutContent` 断言 `SessionID != ""` 锁定顺序）。`RedactArgs`：递归遍历 JSON；键 `content`/`old_text`/`new_text` 的字符串 → `{"bytes":n}` 且计入 `bytesIn`；键名（小写）含 `token`/`cookie`/`authorization`/`password`/`secret` → 删除；任何以 `http://`/`https://` 开头或包含 `://` 的字符串 → `"[url]"`；结果 > `MaxAuditArgs` → `{"truncated":true,"keys":[顶层键名，按字典序，累计不超 3 KiB]}`。`AppendAudit`：`appendFault` 非 nil 且返回错误 → `auditFailures.Add(1)` 并返回；SQL 失败同样计数。

- [ ] **Step 5: 运行确认通过**

Run: `./gow test -race ./internal/agent/ ./internal/mcpsrv/ -count=1`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/agent internal/mcpsrv
git commit -m "feat(mcpsrv): durable, redacted audit of every tool call"
```

---

## Task A4：控制面 `/audit`、`/sessions`、SSE、CLI、daemon 装配（T-34 做法第 4 点）

**Files:**
- Create: `internal/control/agent.go`、`internal/control/agent_test.go`、`internal/control/agent_client.go`
- Create: `internal/daemon/agent_view.go`、`internal/daemon/agent_test.go`
- Create: `cmd/cloudfs/agent.go`、`cmd/cloudfs/agent_test.go`
- Modify: `internal/control/metrics.go`（`routes()` 表尾加 3 行；指标输出加 2 个）、`internal/control/status.go`（`Collector.Agent`、`Status.Agent`、`Collect` 填充）、`internal/control/events.go`（`audit`/`session` 事件）、`internal/control/events_test.go`
- Modify: `internal/daemon/daemon.go`（`Daemon.Agent`、`Daemon.Sessions`；在 `if !opt.SkipWrite {` 之前打开；owner 启动时与每 24 h `PurgeAudit`；`Collector()` 填 `Agent`）
- Modify: `cmd/cloudfs/main.go`（分发 `audit`、`sessions`；帮助表；两处 `mcpsrv.New` 带 `Sessions: d.Sessions`）
- Modify: `internal/i18n/catalog_zh.go`、`catalog_en.go`（`err.agent_unavailable`、`err.session_not_found`、`cli.sessions.*` 若 CLI 输出走目录）

**Interfaces:**
- Consumes: A0–A3。
- Produces:
```go
// internal/control/agent.go
type AgentView interface {
	Audit(ctx context.Context, q agent.AuditQuery) ([]agent.AuditRow, string, error)
	Sessions(ctx context.Context, q agent.ListQuery) ([]agent.Session, string, error)
	Session(ctx context.Context, id string) (agent.Session, []agent.AuditRow, error)
	FinishSession(ctx context.Context, id, summary string) (agent.Session, error)
	Summary(ctx context.Context) (agent.Summary, error)
	Watch() (<-chan agent.Event, func())
	AuditWriteFailures() int64
	Workspace() string // A9 fills; "" until then
}
type AuditView struct {
	ID int64 `json:"id"`; TS time.Time `json:"ts"`; Client string `json:"client"`; SessionID string `json:"session_id"`
	Tool string `json:"tool"`; Paths []string `json:"paths"`; Args json.RawMessage `json:"args"`
	BytesIn int64 `json:"bytes_in"`; BytesOut int64 `json:"bytes_out"`; Result string `json:"result"`
	Error string `json:"error,omitempty"`; DurationMS int64 `json:"duration_ms"`
}
type AuditResponse struct { Rows []AuditView `json:"rows"`; NextCursor string `json:"next_cursor,omitempty"` }
type SessionView struct {
	ID string `json:"id"`; Client string `json:"client"`; ClientVersion string `json:"client_version,omitempty"`
	Transport string `json:"transport"`; State string `json:"state"`; Scope agent.Scope `json:"scope"`
	Workspace string `json:"workspace,omitempty"`; Sandbox bool `json:"sandbox"`
	StartedAt time.Time `json:"started_at"`; LastSeenAt time.Time `json:"last_seen_at"`; FinishedAt *time.Time `json:"finished_at,omitempty"`
	Writes int `json:"writes"`; ArtifactCount int `json:"artifacts"`; Summary string `json:"summary,omitempty"`
}
type SessionsResponse struct { Sessions []SessionView `json:"sessions"`; NextCursor string `json:"next_cursor,omitempty"`; Summary agent.Summary `json:"summary"` }
type SessionDetail struct { Session SessionView `json:"session"`; Audit []AuditView `json:"audit"`; Artifacts []agent.Artifact `json:"artifacts"` }
type SessionFinishRequest struct { Summary string `json:"summary,omitempty"` }
type AgentStatus struct { ActiveSessions int `json:"active_sessions"`; Workspace string `json:"workspace,omitempty"` }
// Collector gains: Agent AgentView ; Status gains: Agent *AgentStatus `json:"agent,omitempty"`

// internal/control/agent_client.go
func CallAgent(ctx context.Context, socket, tcp, method, target string, body, out any) (online bool, err error)
```
```go
// internal/daemon
// Daemon gains: Agent *agent.Store; Sessions *agent.Sessions
func (d *Daemon) agentView() control.AgentView // agent_view.go
```

- [ ] **Step 1: 写失败测试（control）**

```go
// internal/control/agent_test.go
func agentFixture(t *testing.T) (*fixture, *agent.Store, *agent.Sessions) {
	t.Helper()
	f := newFixture(t)
	st, err := agent.Open(filepath.Join(f.dir, "agent"))
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { st.Close() })
	m := agent.NewSessions(st, agent.SessionOptions{})
	f.coll.Agent = testAgentView{st: st, m: m}
	return f, st, m
}
// testAgentView adapts store+sessions exactly like internal/daemon/agent_view.go.

func TestAuditRouteFollowsCursorAndFilters(t *testing.T) {
	f, st, _ := agentFixture(t)
	for i := 0; i < 5; i++ {
		res := "ok"; if i%2 == 0 { res = "denied" }
		st.AppendAudit(context.Background(), agent.AuditRow{TS: time.Now(), Tool: "stat", Paths: []string{"/a"}, Args: json.RawMessage(`{}`), Result: res})
	}
	h := NewServer(f.coll).Handler()
	w := uiCallControl(t, h, "GET", "/audit?limit=2", "")
	var page AuditResponse
	json.Unmarshal(w.Body.Bytes(), &page)
	if w.Code != 200 || len(page.Rows) != 2 || page.NextCursor == "" { t.Fatalf("%d %s", w.Code, w.Body) }
	w = uiCallControl(t, h, "GET", "/audit?limit=10&cursor="+url.QueryEscape(page.NextCursor), "")
	json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Rows) != 3 { t.Fatalf("second page %d", len(page.Rows)) }
	w = uiCallControl(t, h, "GET", "/audit?result=denied", "")
	json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Rows) != 3 { t.Fatalf("denied filter %d", len(page.Rows)) }
}

func TestSessionsRouteListsShowsAndFinishes(t *testing.T) {
	f, _, m := agentFixture(t)
	p, _ := m.EnsurePrincipal(context.Background(), "stdio", "local", agent.Scope{Read: []string{"/work"}})
	s, _ := m.Resolve(context.Background(), agent.ConnInfo{Key: "stdio:1", Transport: "stdio", PrincipalID: p.ID, ClientName: "codex"})
	h := NewServer(f.coll).Handler()
	if w := uiCallControl(t, h, "GET", "/sessions", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"client":"codex"`) { t.Fatalf("%d %s", w.Code, w.Body) }
	if w := uiCallControl(t, h, "GET", "/sessions/"+s.ID, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"audit":`) { t.Fatalf("%d %s", w.Code, w.Body) }
	if w := uiCallControl(t, h, "POST", "/sessions/"+s.ID+"/finish", `{"summary":"ok"}`); w.Code != 200 { t.Fatalf("%d %s", w.Code, w.Body) }
	if w := uiCallControl(t, h, "GET", "/sessions/nope", ""); w.Code != 404 { t.Fatalf("unknown id %d", w.Code) }
}

func TestFinishSessionWithoutControlHeaderIs403(t *testing.T) {
	f, _, _ := agentFixture(t)
	req := httptest.NewRequest("POST", "http://cloudfs/sessions/x/finish", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	NewServer(f.coll).Handler().ServeHTTP(w, req)
	if w.Code != 403 { t.Fatalf("got %d", w.Code) }
}

func TestAgentRoutesAnswer503WithoutAStore(t *testing.T) {
	f := newFixture(t)
	for _, target := range []string{"/audit", "/sessions"} {
		if w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", target, ""); w.Code != 503 { t.Fatalf("%s %d", target, w.Code) }
	}
}

func TestStatusReportsActiveSessions(t *testing.T) { /* one Resolve → GET /status has "agent":{"active_sessions":1 */ }
func TestAuditWriteFailuresMetric(t *testing.T) { /* GET /metrics contains "cloudfs_audit_write_failures_total 0" */ }
```
`uiCallControl` 若包内没有等价 helper，在 `agent_test.go` 内照 `test/e2e/ui_api_test.go:19 uiCall` 写一个（Host `cloudfs`、非 GET 带 `X-CloudFS-Control: 1`）。
```go
// internal/control/events_test.go （追加）
func TestEventsStreamCarriesAuditAndSession(t *testing.T) {
	// Follow the existing events_test.go pattern for reading the SSE stream.
	// After connecting, AppendAudit one row and Resolve one session; the stream
	// must yield "event: audit" with the row's tool and "event: session".
}
```
（按注释写完整：沿用该文件现有 SSE 读取 helper。）

- [ ] **Step 2: 写失败测试（daemon、cmd）**

```go
// internal/daemon/agent_test.go
func TestOpenWiresTheAgentStoreForOwnerAndNonOwner(t *testing.T) {
	// Build a fake-remote config the way the existing daemon tests do, Open twice
	// on the same cache dir: both Daemons have non-nil Agent and Sessions;
	// exactly one has Agent.Owner() == true; Collector().Agent is non-nil.
}
```
```go
// cmd/cloudfs/agent_test.go
func TestAuditFlagsBuildTheQuery(t *testing.T) {
	q, err := auditQueryFromArgs([]string{"--session", "s1", "--tool", "delete", "--result", "denied", "--since", "1h", "--limit", "20"}, time.Unix(7200, 0))
	if err != nil { t.Fatal(err) }
	if q.Session != "s1" || q.Tool != "delete" || q.Result != "denied" || q.Limit != 20 || q.Since.Unix() != 3600 { t.Fatalf("%+v", q) }
}
func TestSessionsFinishRequiresTheDaemon(t *testing.T) { /* offline finish returns an error mentioning "requires the running daemon" */ }
```

- [ ] **Step 3: 运行确认失败**

Run: `./gow test ./internal/control/ -run 'TestAuditRoute|TestSessionsRoute|TestFinishSession|TestAgentRoutes|TestStatusReportsActive|TestAuditWriteFailuresMetric|TestEventsStreamCarriesAudit|TestEveryRouteIsEitherGuardedOrArguedOpen' -v; ./gow test ./internal/daemon/ -run TestOpenWiresTheAgentStore -v; ./gow test ./cmd/cloudfs/ -run 'TestAuditFlags|TestSessionsFinish' -v`
Expected: FAIL

- [ ] **Step 4: 实现**

- 路由（`metrics.go` 表尾）：
```go
		{pattern: "/audit", handler: s.audit},
		{pattern: "/sessions", handler: s.sessions},
		{pattern: "/sessions/", handler: s.sessionByPath, probe: "/sessions/x/finish"},
```
- `s.audit`：`privateRequest` → `allowMethod(GET)` → `collector.Agent == nil` 时 `httpErrorT(w, r, 503, "err.agent_unavailable")` → 解析 `cursor/limit/session/tool/result/since`（`since` 接 RFC3339）→ `Audit` → `AuditView`（`Client` 由 session id 批量查 `Sessions` 得 `ClientName`，缓存本请求内）。
- `s.sessionByPath`：`/sessions/<id>` GET → `SessionDetail`（`Audit` = 最近 50 条）；`/sessions/<id>/finish` POST → `decodeMutation` 读 `SessionFinishRequest` → `FinishSession`；`ErrSessionNotFound` → 404。
- `status.go`：`Collect` 中 `if c.Agent != nil { sum, _ := c.Agent.Summary(ctx); st.Agent = &AgentStatus{ActiveSessions: sum.Active, Workspace: c.Agent.Workspace()} }`。
- `events.go`：在取 `changes` 之后：
```go
	var agentEvents <-chan agent.Event
	if s.collector.Agent != nil {
		ch, stop := s.collector.Agent.Watch()
		defer stop()
		agentEvents = ch
	}
	// select gains:
		case ev, ok := <-agentEvents:
			if !ok { agentEvents = nil; continue }
			switch {
			case ev.Audit != nil:
				if !send("audit", s.auditView(r.Context(), *ev.Audit)) { return }
			case ev.Session != nil:
				if !send("session", sessionView(*ev.Session)) { return }
			}
```
- 指标：`cloudfs_audit_write_failures_total`（counter）、`cloudfs_agent_sessions_active`（gauge），写法照 `metrics.go` 现有 counter 输出。
- `daemon.go`：在 `d.Refresher = vfs.NewRefresher(...)` 之后、`if !opt.SkipWrite {` 之前：
```go
	agentStore, err := agent.Open(filepath.Join(cacheDir, "agent"))
	if err != nil { d.Close(); return nil, fmt.Errorf("daemon: agent store: %w", err) }
	d.Agent = agentStore
	d.Sessions = agent.NewSessions(agentStore, agent.SessionOptions{Idle: cfg.MCP.Session.Idle})
	d.closers = append(d.closers, agentStore.Close)
	if agentStore.Owner() {
		retain := cfg.MCP.Audit.Retain
		purge := func() { _, _ = agentStore.PurgeAudit(context.Background(), time.Now().Add(-retain)) }
		purge()
		stopPurge := make(chan struct{})
		go func() { t := time.NewTicker(24 * time.Hour); defer t.Stop(); for { select { case <-t.C: purge(); case <-stopPurge: return } } }()
		d.closers = append(d.closers, func() error { close(stopPurge); return nil })
	}
```
`Collector()`：`col.Agent = d.agentView()`（`d.Agent == nil` 时保持 nil 接口，同 `Export` 写法）。
- `cmd/cloudfs/main.go`：`case "audit": return cmdAudit(ctx, args[1:])`、`case "sessions": return cmdSessions(ctx, args[1:])`；帮助表加 `audit [--session ID] [--tool T] [--result ok|denied|error] [--since 1h] [--json]` 与 `sessions list|show <id>|finish <id>`；`cmdMCP` 的 stdio `mcpsrv.New`（672）与 `serveMCPHTTPWith`（697）都加 `Sessions: d.Sessions`。
- `cmd/cloudfs/agent.go`：在线 `control.CallAgent`；离线 `agent.OpenReadOnly(filepath.Join(cfg.Cache.Dir, "agent"))` 只支持 `audit`、`sessions list/show`，`finish` 离线返回 `sessions finish requires the running daemon`。输出沿用 `uploads.go` 的表格/`--json` 约定。

- [ ] **Step 5: 运行确认通过**

Run: `./gow build ./... && ./gow test -race ./internal/control/ ./internal/daemon/ ./cmd/cloudfs/ -count=1 && ./gow vet ./...`
Expected: PASS（`TestEveryRouteIsEitherGuardedOrArguedOpen` 自动覆盖 3 条新路由）

- [ ] **Step 6: 提交**

```bash
git add internal/control internal/daemon cmd/cloudfs internal/i18n
git commit -m "feat(control): audit and session endpoints, events and CLI"
```

---

## Task A5：UI F1 —「Agent」屏骨架 + 会话/审计标签（T-34 界面）

**Files:**
- Create: `internal/control/web/scope_view.js`、`internal/control/web/_tests/scope_view.test.mjs`
- Create: `internal/control/web/screens/agents.js`、`screens/agents_sessions.js`、`screens/agents_audit.js`、`internal/control/web/session_panel.js`
- Create: `internal/control/ui_agents_test.go`
- Modify: `internal/control/web/router.js`、`app.js`（import、`screens` 映射、导航徽标、`onAgentEvent`）、`api.js`（`events` 加 `onAudit`/`onSession`）、`i18n.js`、`app.css`（`.nav a .badge`、`tr.denied`）

**Interfaces:**
- Consumes: A4 路由 `GET /sessions?cursor&limit&state`、`GET /sessions/{id}`、`POST /sessions/{id}/finish`、`GET /audit?cursor&limit&session&tool&result&since`、`/status` 的 `agent.active_sessions`、SSE `audit`/`session`。
- Produces（JS）:
```js
// scope_view.js — zero imports
export function cleanPath(p)                         // '/work/../gd/' -> '/gd'
export function under(p, prefix)                     // prefix '/' covers all
export function parsePrefixes(text)                  // textarea -> cleaned, non-empty, deduped
export function scopeParts(scope)                    // -> [{ key, args }] for t(); keys: scope.all, scope.read, scope.write, scope.readonly, scope.expires, scope.sandbox
export function validateTokenScope({ read, write })  // -> '' | 'tokens.err.relative' | 'tokens.err.write_outside_read'
// session_panel.js
export function openSessionPanel(id)                 // GET /sessions/<id>, openPanel
// app.js
export function onAgentEvent(fn)                     // fn({ kind: 'audit'|'session', data })
// screens/agents.js
export function renderAgents(host)                   // returns dispose
```

- [ ] **Step 1: 写失败测试（mjs）**

```js
// internal/control/web/_tests/scope_view.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { cleanPath, under, parsePrefixes, scopeParts, validateTokenScope } from '../scope_view.js';

test('an empty read list is the whole mount', () => {
  assert.deepEqual(scopeParts({}), [{ key: 'scope.all', args: [] }]);
});
test('write null means the same as read and is not repeated', () => {
  assert.deepEqual(scopeParts({ read: ['/work'] }), [{ key: 'scope.read', args: ['/work'] }]);
});
test('an empty write list reads as read only', () => {
  assert.deepEqual(scopeParts({ read: ['/work'], write: [] }).map((p) => p.key), ['scope.read', 'scope.readonly']);
});
test('separate write, sandbox and expiry each get a part', () => {
  const parts = scopeParts({ read: ['/work'], write: ['/work/.agent'], sandbox: '/work/.agent/s1', expires_at: '2026-10-01T00:00:00Z' });
  assert.deepEqual(parts.map((p) => p.key), ['scope.read', 'scope.write', 'scope.sandbox', 'scope.expires']);
});
test('dotdot and trailing slashes are cleaned before comparing', () => {
  assert.equal(cleanPath('/work/../gd/'), '/gd');
  assert.equal(under('/workshop/a', '/work'), false);
  assert.equal(under('/work/a', '/work'), true);
});
test('prefix text is split per line, cleaned and deduped', () => {
  assert.deepEqual(parsePrefixes(' /work \n\n/work/\n/gd'), ['/work', '/gd']);
});
test('write must be inside read', () => {
  assert.equal(validateTokenScope({ read: ['/work'], write: ['/gd'] }), 'tokens.err.write_outside_read');
  assert.equal(validateTokenScope({ read: ['/work'], write: ['/work/.agent'] }), '');
  assert.equal(validateTokenScope({ read: [], write: ['/gd'] }), '');
  assert.equal(validateTokenScope({ read: ['work'], write: [] }), 'tokens.err.relative');
});
```

- [ ] **Step 2: 写失败测试（Go）**

```go
// internal/control/ui_agents_test.go
package control

func TestAgentsScreenIsRoutedAndInTheNav(t *testing.T) {
	router := webSource(t, "web/router.js")
	app := webSource(t, "web/app.js")
	for _, want := range []string{"'#/agents': 'agents-view'", "hash: '#/agents', icon: 'bot', key: 'nav.agents'"} {
		if !strings.Contains(router, want) { t.Errorf("router.js lacks %s", want) }
	}
	if !strings.Contains(app, "renderAgents") || !strings.Contains(app, "active_sessions") {
		t.Error("the shell never mounts the agents screen or never shows the active-session badge")
	}
}

func TestAgentsScreenReachesTheSessionAndAuditRoutes(t *testing.T) {
	sessions := webSource(t, "web/screens/agents_sessions.js")
	audit := webSource(t, "web/screens/agents_audit.js")
	panel := webSource(t, "web/session_panel.js")
	for _, want := range []string{"api.get('/sessions?", "next_cursor", "moreRow(", "scopeParts("} {
		if !strings.Contains(sessions, want) { t.Errorf("sessions tab lacks %s", want) }
	}
	for _, want := range []string{"api.get('/audit?", "next_cursor", "moreRow(", "'data-result'", "t('audit.result.' + "} {
		if !strings.Contains(audit, want) { t.Errorf("audit tab lacks %s", want) }
	}
	for _, want := range []string{"api.get('/sessions/' + encodeURIComponent(", "+ '/finish'", "openPanel("} {
		if !strings.Contains(panel, want) { t.Errorf("session panel lacks %s", want) }
	}
}

// A denied row must say so in words; colour alone is not an accessible signal.
func TestDeniedAuditRowsCarryATextLabel(t *testing.T) {
	audit := webSource(t, "web/screens/agents_audit.js")
	if !strings.Contains(audit, "class: row.result === 'denied' ? 'denied' : ''") || !strings.Contains(audit, "t('audit.result.' + row.result)") {
		t.Error("denied rows must carry both the row class and the translated result label")
	}
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, k := range []string{"audit.result.ok", "audit.result.denied", "audit.result.error"} {
		if !zh[k] { t.Errorf("missing %s", k) }
	}
}

func TestAuditArgsRenderAsTextAndAreNotCached(t *testing.T) {
	audit := webSource(t, "web/screens/agents_audit.js")
	if strings.Contains(audit, "html:") { t.Error("audit tab uses the html attribute; args are untrusted") }
	if !strings.Contains(audit, "JSON.stringify(row.args, null, 2)") { t.Error("args are not shown as formatted text") }
	if strings.Contains(audit, "store.js") { t.Error("audit rows must not be cached in the shared store") }
}

func TestWebCatalogCoversSessionStates(t *testing.T) {
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, s := range []string{"active", "finished", "expired"} {
		if !zh["session.state."+s] { t.Errorf("missing session.state.%s", s) }
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `node --test internal/control/web/_tests/scope_view.test.mjs; ./gow test ./internal/control/ -run 'TestAgentsScreen|TestDeniedAuditRows|TestAuditArgsRender|TestWebCatalogCoversSessionStates' -v`
Expected: FAIL

- [ ] **Step 4: 实现**

`router.js`：`routes` 加 `'#/agents': 'agents-view',`；`navItems` 在 `#/exports` 之后加 `{ hash: '#/agents', icon: 'bot', key: 'nav.agents', badge: 'agents' },`。

`app.js`：
```js
import { renderAgents } from '/ui/screens/agents.js';
// screens map gains: 'agents-view': renderAgents,
function navBadge(item, status) {
  if (item.badge !== 'agents') return null;
  const n = status && status.agent ? status.agent.active_sessions : 0;
  return n > 0 ? el('span', { class: 'badge', 'aria-label': t('nav.agents.active', n) }, String(n)) : null;
}
// in nav(): el('a', { href: item.hash }, iconEl(item.icon), el('span', {}, t(item.key)), navBadge(item, get().status))
const agentHandlers = new Set();
export function onAgentEvent(fn) { agentHandlers.add(fn); return () => agentHandlers.delete(fn); }
// events({... onAudit: (d) => { for (const fn of agentHandlers) fn({ kind: 'audit', data: d }); },
//            onSession: (d) => { for (const fn of agentHandlers) fn({ kind: 'session', data: d }); } })
```
导航在 status tick 时不重绘（`refreshTitlebar` 只换标题栏），因此徽标数更新要在 `refreshTitlebar` 里同步替换 `.nav` 中 `#/agents` 链接的 `.badge`：查询 `document.querySelector('.nav a[href="#/agents"]')`，移除旧 `.badge` 后追加新徽标。

`api.js`：`events({ onStatus, onChange, onExport, onAudit, onSession })`，`connect` 内加两个 `addEventListener`，写法同 `export`。

`screens/agents.js`：
```js
import { el, fill } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { renderSessionsTab } from '/ui/screens/agents_sessions.js';
import { renderAuditTab } from '/ui/screens/agents_audit.js';

const TABS = ['sessions', 'audit'];   // A8 appends 'tokens'
export function renderAgents(host) {
  const params = new URLSearchParams((location.hash.split('?')[1]) || '');
  let tab = TABS.includes(params.get('tab')) ? params.get('tab') : 'sessions';
  let dispose = null;
  const body = el('div', {});
  const bar = el('div', { class: 'row', role: 'tablist' });
  function show(name) {
    tab = name;
    if (dispose) dispose();
    fill(bar, ...TABS.map((n) => el('button', { role: 'tab', 'aria-selected': String(n === tab), class: n === tab ? 'primary' : '', onclick: () => show(n) }, t('agents.tab.' + n))));
    dispose = name === 'audit' ? renderAuditTab(body) : renderSessionsTab(body);
  }
  fill(host, el('div', { class: 'eyebrow' }, t('agents.eyebrow')), el('h2', {}, t('agents.title')), bar, body);
  show(tab);
  return () => { if (dispose) dispose(); };
}
```
（`t('agents.tab.' + n)` 是拼接键，不被字面扫描覆盖；三个键必须手工加进两表。）

`screens/agents_sessions.js`：三张卡片（`summary.active`、`summary.writes_today`、`summary.denied_today`）；表格列 `session.col.client/scope/state/started/writes`；作用域单元格 `scopeParts(s.scope).map((p) => t(p.key, ...p.args)).join(' · ')`；状态单元格 `el('span', {}, el('span', { class: 'dot ' + (s.state === 'active' ? 'ok' : '') }), ' ' + t('session.state.' + s.state))`；`load(cursor)` 调 `api.get('/sessions?limit=50' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''))`，`pageCursor`/`pageFailureMode` 同 `exports.js`；有 `next_cursor` 时 `moreRow(5, () => load(r.next_cursor))`；`onAgentEvent` 收到 `session` 且当前未翻页（`!pagedBeyondFirst`）时重载第一页；行点击 `openSessionPanel(s.id)`；URL 带 `?session=<id>` 时挂载后立即 `openSessionPanel`（供 A11 冒烟深链）。

`screens/agents_audit.js`：过滤条（`input` 会话 ID、`input` 工具、`select` 结果 `''/ok/denied/error`、`select` 时间 `1h/24h/7d/''` → `since = new Date(Date.now() - ms).toISOString()`）；行：
```js
el('tr', { class: row.result === 'denied' ? 'denied' : '', 'data-result': row.result, onclick: () => showArgs(row) },
  el('td', { class: 'dim' }, new Date(row.ts).toLocaleString(locale())),
  el('td', {}, row.client || '-'),
  el('td', {}, row.tool),
  el('td', { class: 'detail' }, row.paths.length > 1 ? row.paths[0] + ' ' + t('audit.paths.more', row.paths.length - 1) : (row.paths[0] || '')),
  el('td', {}, t('audit.result.' + row.result)),
  el('td', { class: 'num' }, bytes(row.bytes_in + row.bytes_out)),
  el('td', { class: 'num dim' }, row.duration_ms + ' ms'))
function showArgs(row) {
  showPanel({ title: t('audit.args'), content: el('pre', { class: 'detail' }, JSON.stringify(row.args, null, 2)), closeLabel: t('close') });
}
```
SSE：`onAgentEvent` 收到 `audit` 且无过滤、未翻页 → `rows.prepend(rowEl(data))`。`closeLabel` 用已有关闭键（执行时 `grep -n "'close'" web/i18n.js` 复核键名，没有则新加 `common.close`）。

`session_panel.js`：
```js
import { api } from '/ui/api.js';
import { el, openPanel, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { scopeParts } from '/ui/scope_view.js';
export async function openSessionPanel(id) {
  let d;
  try { d = await api.get('/sessions/' + encodeURIComponent(id)); } catch (err) { toast(err.message, 'bad'); return; }
  const s = d.session;
  const finish = s.state === 'active' ? el('button', { onclick: async () => {
    try { await api.post('/sessions/' + encodeURIComponent(id) + '/finish', {}); toast(t('session.finished.toast')); close(); }
    catch (err) { toast(err.message, 'bad'); }
  } }, t('session.finish')) : null;
  const content = el('div', {},
    el('div', { class: 'detail' }, s.client + (s.client_version ? ' ' + s.client_version : '')),
    el('div', { class: 'dim' }, scopeParts(s.scope).map((p) => t(p.key, ...p.args)).join(' · ')),
    el('div', { class: 'eyebrow', style: 'margin-top:14px' }, t('session.audit.tail')),
    el('table', {}, el('tbody', {}, ...(d.audit || []).map((r) => el('tr', { 'data-result': r.result },
      el('td', {}, r.tool), el('td', { class: 'detail' }, (r.paths || []).join(', ')), el('td', {}, t('audit.result.' + r.result)))))));
  const close = openPanel({ title: t('session.panel.title', s.client), content, footer: finish });
}
```
（`openPanel` 的返回值与 `footer` 形态以 `ui.js:165` 实现为准；若返回的不是关闭函数，按其实际 API 调整。）

`i18n.js`（`zh` 表尾 / `en` 表尾各追加，键集合一致）：
| key | zh | en |
|---|---|---|
| `nav.agents` | Agent | Agents |
| `nav.agents.active` | %s 个活动会话 | %s active sessions |
| `agents.eyebrow` | Agent 工作底座 | Agent workbase |
| `agents.title` | 会话与审计 | Sessions and audit |
| `agents.tab.sessions` | 会话 | Sessions |
| `agents.tab.audit` | 审计 | Audit |
| `agents.card.active` | 活动会话 | Active sessions |
| `agents.card.writes` | 今日写操作 | Writes today |
| `agents.card.denied` | 今日拒绝 | Denied today |
| `session.col.client` | 客户端 | Client |
| `session.col.scope` | 作用域 | Scope |
| `session.col.state` | 状态 | State |
| `session.col.started` | 开始时间 | Started |
| `session.col.writes` | 写操作 | Writes |
| `session.state.active` | 活动 | Active |
| `session.state.finished` | 已结束 | Finished |
| `session.state.expired` | 已过期 | Expired |
| `session.empty` | 还没有 Agent 会话 | No agent sessions yet |
| `session.panel.title` | 会话 · %s | Session · %s |
| `session.audit.tail` | 最近 50 条操作 | Last 50 operations |
| `session.finish` | 结束会话 | Finish session |
| `session.finished.toast` | 会话已结束 | Session finished |
| `scope.all` | 整个挂载 | Whole mount |
| `scope.read` | 读 %s | Read %s |
| `scope.write` | 写 %s | Write %s |
| `scope.readonly` | 只读 | Read only |
| `scope.expires` | %s 过期 | Expires %s |
| `scope.sandbox` | 沙箱 %s | Sandbox %s |
| `audit.filter.session` | 会话 ID | Session ID |
| `audit.filter.tool` | 工具 | Tool |
| `audit.filter.result` | 结果 | Result |
| `audit.filter.since` | 时间范围 | Time range |
| `audit.since.1h` | 1 小时内 | Last hour |
| `audit.since.24h` | 24 小时内 | Last 24 hours |
| `audit.since.7d` | 7 天内 | Last 7 days |
| `audit.result.ok` | 成功 | OK |
| `audit.result.denied` | 已拒绝 | Denied |
| `audit.result.error` | 失败 | Error |
| `audit.col.time` | 时间 | Time |
| `audit.col.path` | 路径 | Path |
| `audit.col.bytes` | 字节 | Bytes |
| `audit.col.duration` | 耗时 | Duration |
| `audit.args` | 参数（已脱敏） | Arguments (redacted) |
| `audit.paths.more` | 等 %s 个路径 | and %s more |
| `audit.empty` | 没有审计记录 | No audit records |

`app.css`：
```css
.nav a .badge { margin-left: auto; min-width: 18px; padding: 0 6px; border-radius: 9px; background: var(--accent, #2f6fed); color: #fff; font-size: 11px; line-height: 18px; text-align: center; }
tr.denied td { background: rgba(220, 70, 70, 0.12); }
```
（颜色变量以 `app.css` 现有诊断屏配色为准，执行时 `grep -n "bad\|danger" web/app.css` 对齐。）

- [ ] **Step 5: 运行确认通过**

Run: `node --test internal/control/web/_tests/*.test.mjs && ./gow test ./internal/control/ -run 'TestAgents|TestDeniedAudit|TestAuditArgs|TestWebCatalog|TestWebScreens|TestEveryTranslationKey|TestIconsAreSizedAndDefined|TestBrowserModule|TestWebAppNeverAsksForACredential|TestScreensDoNotStringifyASkippedChild' -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/control/web internal/control/ui_agents_test.go
git commit -m "feat(ui): agents screen with sessions and audit tabs"
```

---

## Task A6：访问令牌存储、`requireAuth`、吊销关会话、`cloudfs mcp token`（T-35 做法第 1 点）

**Files:**
- Create: `internal/agent/token.go`、`internal/agent/token_test.go`
- Create: `internal/mcpsrv/token_http_test.go`
- Create: `cmd/cloudfs/mcp_token.go`、`cmd/cloudfs/mcp_token_test.go`
- Modify: `internal/mcpsrv/http.go`（新增 `HTTPAuth`、`NewHTTPHandler`、`ServeHTTPWithAuth`、`requireAuth`；**保留** `requireBearer(next, token)` 与 `ServeHTTPWithToken` 原签名）
- Modify: `internal/mcpsrv/session_mw.go`（legacy 会话 → principal 映射；吊销轮询关闭）
- Modify: `cmd/cloudfs/main.go`（`cmdMCP` 分发 `token` 子命令；`serveMCPHTTPWith` 改用 `ServeHTTPWithAuth`；帮助表）

**Interfaces:**
- Consumes: A0 `Store`/`Principal`；A1 `Scope`、`Under`；A2 `Sessions.EnsurePrincipal/Principal`、`ConnInfo`。
- Produces:
```go
// internal/agent/token.go
type TokenSpec struct {
	Name     string
	Read     []string
	Write    []string // nil = same as Read
	ReadOnly bool
	TTL      time.Duration // 0 = never expires
}
var (
	ErrTokenUnknown = errors.New("agent: unknown token")
	ErrTokenExpired = errors.New("agent: token expired")
	ErrTokenRevoked = errors.New("agent: token revoked")
	ErrTokenName    = errors.New("agent: token name must match ^[a-z0-9][a-z0-9-]{0,63}$ and be unique among live tokens")
	ErrTokenScope   = errors.New("agent: every write prefix must be inside a read prefix")
)
const TokenPlainPrefix = "cfs_"
func (s *Store) CreateToken(ctx context.Context, spec TokenSpec) (plain string, p Principal, err error)
func (s *Store) Tokens(ctx context.Context) ([]Principal, error)
func (s *Store) RevokeToken(ctx context.Context, idOrName string) (Principal, error)
func (s *Store) VerifyToken(ctx context.Context, plain string) (Principal, error) // bumps last_used_at at most once a minute
func (s *Store) HasLiveTokens(ctx context.Context) (bool, error)
func (s *Store) RevokedSince(ctx context.Context, since time.Time) ([]string, error)
func TokenState(p Principal, now time.Time) string // active | expired | revoked
```
```go
// internal/mcpsrv/http.go
type HTTPAuth struct {
	// Token is the legacy CLOUDFS_MCP_TOKEN: full access, kept for compatibility.
	Token string
	// Verify resolves an issued token to its principal. nil = no issued tokens.
	Verify func(ctx context.Context, plain string) (agent.Principal, error)
	// Open reports whether a request with no Authorization header may pass as
	// the local default principal. Only consulted on loopback listeners.
	Open func(ctx context.Context) bool
}
func NewHTTPHandler(s *Server, a HTTPAuth) http.Handler
func ServeHTTPWithAuth(ctx context.Context, s *Server, addr string, a HTTPAuth) error
// Server gains: func (s *Server) CloseSessionsOf(principalID string) int
```

- [ ] **Step 1: 写失败测试（agent）**

```go
// internal/agent/token_test.go
func TestCreateTokenStoresOnlyTheHash(t *testing.T) {
	s, _ := Open(t.TempDir()); defer s.Close()
	plain, p, err := s.CreateToken(context.Background(), TokenSpec{Name: "codex", Read: []string{"/work"}, Write: []string{"/work/.agent"}, TTL: 720 * time.Hour})
	if err != nil { t.Fatal(err) }
	if !strings.HasPrefix(plain, TokenPlainPrefix) || len(plain) < 40 { t.Fatalf("plain %q", plain) }
	if p.TokenPrefix != plain[4:8] { t.Fatalf("fingerprint %q", p.TokenPrefix) }
	rows, _ := s.db.Query(`SELECT id, kind, name, token_hash, token_prefix, scope FROM principals`)
	defer rows.Close()
	for rows.Next() {
		var cols [6]string
		rows.Scan(&cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5])
		for _, c := range cols { if strings.Contains(c, plain[4:]) { t.Fatalf("plain token material stored: %q", c) } }
	}
	sum := sha256.Sum256([]byte(plain))
	var hash string
	s.db.QueryRow(`SELECT token_hash FROM principals WHERE id=?`, p.ID).Scan(&hash)
	if hash != hex.EncodeToString(sum[:]) { t.Fatal("token_hash is not sha256(plain)") }
}

func TestVerifyTokenRejectsExpiredAndRevoked(t *testing.T) {
	s, _ := Open(t.TempDir()); defer s.Close()
	ctx := context.Background()
	short, _, _ := s.CreateToken(ctx, TokenSpec{Name: "short", TTL: time.Millisecond})
	live, p, _ := s.CreateToken(ctx, TokenSpec{Name: "live"})
	time.Sleep(5 * time.Millisecond)
	if _, err := s.VerifyToken(ctx, short); !errors.Is(err, ErrTokenExpired) { t.Fatalf("expired: %v", err) }
	if got, err := s.VerifyToken(ctx, live); err != nil || got.ID != p.ID { t.Fatalf("live: %v", err) }
	if _, err := s.RevokeToken(ctx, "live"); err != nil { t.Fatal(err) }
	if _, err := s.VerifyToken(ctx, live); !errors.Is(err, ErrTokenRevoked) { t.Fatalf("revoked: %v", err) }
	if _, err := s.VerifyToken(ctx, "cfs_nope"); !errors.Is(err, ErrTokenUnknown) { t.Fatalf("unknown: %v", err) }
}

func TestTokenWriteMustBeInsideRead(t *testing.T) {
	s, _ := Open(t.TempDir()); defer s.Close()
	if _, _, err := s.CreateToken(context.Background(), TokenSpec{Name: "bad", Read: []string{"/work"}, Write: []string{"/gd"}}); !errors.Is(err, ErrTokenScope) {
		t.Fatalf("got %v", err)
	}
}

func TestLiveTokenNamesAreUnique(t *testing.T) {
	s, _ := Open(t.TempDir()); defer s.Close()
	ctx := context.Background()
	s.CreateToken(ctx, TokenSpec{Name: "codex"})
	if _, _, err := s.CreateToken(ctx, TokenSpec{Name: "codex"}); !errors.Is(err, ErrTokenName) { t.Fatalf("dup: %v", err) }
	s.RevokeToken(ctx, "codex")
	if _, _, err := s.CreateToken(ctx, TokenSpec{Name: "codex"}); err != nil { t.Fatalf("reuse after revoke: %v", err) }
	if _, _, err := s.CreateToken(ctx, TokenSpec{Name: "Bad Name"}); !errors.Is(err, ErrTokenName) { t.Fatalf("name: %v", err) }
}

func TestRevokedSinceListsRevokedPrincipals(t *testing.T) {
	s, _ := Open(t.TempDir()); defer s.Close()
	ctx := context.Background()
	_, p, _ := s.CreateToken(ctx, TokenSpec{Name: "a"})
	before := time.Now()
	s.RevokeToken(ctx, p.ID)
	ids, _ := s.RevokedSince(ctx, before.Add(-time.Millisecond))
	if len(ids) != 1 || ids[0] != p.ID { t.Fatalf("%v", ids) }
}
```

- [ ] **Step 2: 写失败测试（mcpsrv）**

```go
// internal/mcpsrv/token_http_test.go
type bearerRT struct{ token string }
func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context()); r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func tokenServer(t *testing.T, e *env, a HTTPAuth) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(NewHTTPHandler(e.server, a))
	t.Cleanup(ts.Close)
	return ts
}

func connectWith(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "token-test", Version: "0"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearerRT{token}}}, nil)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTwoTokensSeeDisjointTrees(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	e.fake.Seed("work/a.txt", []byte("a")); e.fake.Seed("gd/x.txt", []byte("x"))
	ctx := context.Background()
	ta, _, _ := st.CreateToken(ctx, agent.TokenSpec{Name: "a", Read: []string{"/work"}})
	tb, _, _ := st.CreateToken(ctx, agent.TokenSpec{Name: "b", Read: []string{"/gd"}})
	ts := tokenServer(t, e, HTTPAuth{Verify: st.VerifyToken})
	var wg sync.WaitGroup
	check := func(token, allowed, denied string) {
		defer wg.Done()
		s := connectWith(t, ts.URL, token)
		for i := 0; i < 10; i++ {
			ok, _ := s.CallTool(ctx, &mcp.CallToolParams{Name: "stat", Arguments: map[string]any{"path": allowed}})
			no, _ := s.CallTool(ctx, &mcp.CallToolParams{Name: "stat", Arguments: map[string]any{"path": denied}})
			if ok.IsError || !no.IsError { t.Errorf("token %s: allowed err=%v denied err=%v", token[:8], ok.IsError, no.IsError) }
		}
		if err := s.Subscribe(ctx, &mcp.SubscribeParams{URI: resourceURIForTest(denied)}); err == nil {
			t.Errorf("token %s subscribed outside its scope", token[:8])
		}
	}
	wg.Add(2)
	go check(ta, "/work/a.txt", "/gd/x.txt")
	go check(tb, "/gd/x.txt", "/work/a.txt")
	wg.Wait()
}

func TestExpiredTokenInitializeIs401(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	tok, _, _ := st.CreateToken(context.Background(), agent.TokenSpec{Name: "short", TTL: time.Millisecond})
	time.Sleep(5 * time.Millisecond)
	ts := tokenServer(t, e, HTTPAuth{Verify: st.VerifyToken})
	req, _ := http.NewRequest("POST", ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}`))
	req.Header.Set("Content-Type", "application/json"); req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { t.Fatal(err) }
	if resp.StatusCode != 401 { t.Fatalf("status %d", resp.StatusCode) }
}

func TestRevokeClosesLegacySessionWithin5s(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	tok, p, _ := st.CreateToken(context.Background(), agent.TokenSpec{Name: "legacy"})
	inject := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tok)
		NewHTTPHandler(e.server, HTTPAuth{Verify: st.VerifyToken}).ServeHTTP(w, r)
	})
	ts := httptest.NewServer(inject); defer ts.Close()
	resp, _ := subscriptionRPC(t, ts.URL, "2025-06-18", "", "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "legacy", "version": "0"}})
	id := resp.Header.Get("Mcp-Session-Id")
	if id == "" { t.Fatal("no legacy session id") }
	subscriptionRPC(t, ts.URL, "2025-06-18", id, "tools/list", map[string]any{}) // binds the session to the principal
	st.RevokeToken(context.Background(), p.ID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for ss := range e.server.MCP().Sessions() { if ss.ID() == id { alive = true } }
		if !alive { return }
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the revoked token's legacy session is still open after 5s")
}

func TestEnvTokenKeepsFullAccess(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	e.fake.Seed("gd/x.txt", []byte("x"))
	ts := tokenServer(t, e, HTTPAuth{Token: "env-token", Verify: st.VerifyToken})
	s := connectWith(t, ts.URL, "env-token")
	if res, _ := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "stat", Arguments: map[string]any{"path": "/gd/x.txt"}}); res.IsError {
		t.Fatal("the legacy env token lost access")
	}
}

func TestLoopbackStaysOpenUntilTheFirstToken(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	open := func(ctx context.Context) bool { live, _ := st.HasLiveTokens(ctx); return !live }
	ts := tokenServer(t, e, HTTPAuth{Verify: st.VerifyToken, Open: open})
	if code := rawInitialize(t, ts.URL, ""); code != 200 { t.Fatalf("before any token: %d", code) }
	st.CreateToken(context.Background(), agent.TokenSpec{Name: "first"})
	if code := rawInitialize(t, ts.URL, ""); code != 401 { t.Fatalf("after the first token: %d", code) }
}
```
`resourceURIForTest` 调 `resources.go` 中把虚拟路径编码成 `cloudfs://` URI 的现有函数（执行前 `grep -n "cloudfs://" internal/mcpsrv/resources.go` 找名字）；`rawInitialize(t, url, token) int` 在本文件内按 `TestExpiredTokenInitializeIs401` 的请求写法抽出。`ClientSession.Subscribe` 的方法名/参数以 go-sdk v1.7.0 `mcp/client.go` 为准。

- [ ] **Step 3: 写失败测试（cmd）**

```go
// cmd/cloudfs/mcp_token_test.go
func TestMCPTokenCreatePrintsThePlainTokenOnce(t *testing.T) {
	cfgPath := writeTestConfig(t) // reuse the helper the uploads/cache CLI tests use
	var out bytes.Buffer
	if err := runMCPToken(context.Background(), &out, []string{"create", "--config", cfgPath, "--name", "codex", "--read", "/work", "--write", "/work/.agent", "--ttl", "720h"}); err != nil {
		t.Fatal(err)
	}
	plain := regexp.MustCompile(`cfs_[A-Za-z0-9_-]{20,}`).FindString(out.String())
	if plain == "" { t.Fatalf("no token printed: %s", out.String()) }
	out.Reset()
	runMCPToken(context.Background(), &out, []string{"list", "--config", cfgPath})
	if strings.Contains(out.String(), plain) || !strings.Contains(out.String(), plain[4:8]) {
		t.Fatalf("list must show the fingerprint and never the token: %s", out.String())
	}
}
func TestMCPTokenRevokeNeedsConfirm(t *testing.T) { /* revoke without --confirm returns an error naming --confirm; with it the list shows "revoked" */ }
```
（`writeTestConfig` 以 `cmd/cloudfs/uploads_test.go` / `cache_test.go` 实际 helper 名为准。）

- [ ] **Step 4: 运行确认失败**

Run: `./gow test ./internal/agent/ -run 'TestCreateToken|TestVerifyToken|TestTokenWrite|TestLiveTokenNames|TestRevokedSince' -v; ./gow test ./internal/mcpsrv/ -run 'TestTwoTokens|TestExpiredToken|TestRevokeCloses|TestEnvToken|TestLoopbackStaysOpen' -v; ./gow test ./cmd/cloudfs/ -run TestMCPToken -v`
Expected: FAIL

- [ ] **Step 5: 实现**

- `CreateToken`：校验名字正则；`Read`/`Write` 经 `Normalise`；`Write != nil` 时每个前缀必须 `Under` 某个 `EffectiveRead()` 前缀；随机 32 字节 → `TokenPlainPrefix + base64.RawURLEncoding`；`token_hash = hex(sha256(plain))`；`token_prefix = plain[4:8]`；`expires_at = now+TTL`（TTL 0 → 0）；唯一索引冲突映射 `ErrTokenName`。
- `VerifyToken`：只按 `token_hash` 查（常量时间比较不需要，因为比较的是哈希索引命中与否）；`revoked_at != 0 → ErrTokenRevoked`；过期 → `ErrTokenExpired`。
- `requireAuth`：
```go
func requireAuth(next http.Handler, a HTTPAuth) http.Handler {
	verifier := func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		far := time.Now().Add(100 * 365 * 24 * time.Hour)
		if a.Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.Token)) == 1 {
			return &auth.TokenInfo{UserID: "env", Expiration: far}, nil
		}
		if a.Verify == nil {
			return nil, auth.ErrInvalidToken
		}
		p, err := a.Verify(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
		}
		exp := p.ExpiresAt
		if exp.IsZero() { exp = far }
		return &auth.TokenInfo{UserID: p.ID, Expiration: exp}, nil
	}
	guarded := auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{})(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && a.Open != nil && a.Open(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
}
```
（`auth.ErrInvalidToken` 的确切哨兵名执行前 `grep -n "ErrInvalidToken" $(./gow env GOMODCACHE)/github.com/modelcontextprotocol/go-sdk@v1.7.0/auth/auth.go` 确认。）`NewHTTPHandler(s, a)`：`h := newMCPHTTPHandler(s)`；`a.Token == "" && a.Verify == nil` → 返回 `h`；否则 `requireAuth(h, a)`。`ServeHTTPWithAuth`：非回环地址时强制 `a.Open = nil`，且 `a.Token == "" && a.Verify == nil` 仍按今天的文案拒绝；`ServeHTTPWithToken(ctx, s, addr, token)` 改为 `ServeHTTPWithAuth(ctx, s, addr, HTTPAuth{Token: token})`。
- `resolveSession`（A2）的 token 分支里：`extra.TokenInfo.UserID == "env"` → principal 用 `EnsurePrincipal("env", "env", s.defaultScope)`；否则 `Sessions.Principal(id)`；若同时 `sessionID(ss) != ""`，记 `s.legacyByPrincipal[pid][ssID] = ss`（`sync.Mutex` 保护）。
- 吊销轮询：`New` 中 `Sessions != nil` 时起 goroutine，每 2 s `RevokedSince(lastCheck)`，对每个 id 调 `s.CloseSessionsOf(id)`；`Close()` 停止该 goroutine（加入现有 `Close` 流程）。
- `cmd/cloudfs/main.go` `serveMCPHTTPWith`：
```go
	auth := mcpsrv.HTTPAuth{Token: mcpHTTPToken()}
	if d.Agent != nil {
		auth.Verify = d.Agent.VerifyToken
		env := auth.Token
		auth.Open = func(ctx context.Context) bool { live, err := d.Agent.HasLiveTokens(ctx); return env == "" && err == nil && !live }
	}
	return mcpsrv.ServeHTTPWithAuth(ctx, srv, addr, auth)
```
- `cmdMCP`：在 `loadConfig` 之后、`install` 判断旁加 `if f.arg(0) == "token" { return runMCPToken(ctx, os.Stdout, args[1:]) }`；`runMCPToken(ctx, out io.Writer, args []string) error` 实现 `create|list|revoke`，直接 `agent.Open(filepath.Join(cfg.Cache.Dir, "agent"))`（跨进程 WAL 可写；运行中的服务端靠 `RevokedSince` 轮询在 ≤ 2 s 内关闭会话）；`create` 把明文**只**写一次到 `out`，stderr 提示 `store it now: this token cannot be shown again`；`--read/--write` 逗号分隔（同 `--allow`）；`--json` 输出 `{token, principal}`。
- 帮助表加：`mcp token create --name N [--read P,..] [--write P,..] [--read-only] [--ttl 720h]`、`mcp token list`、`mcp token revoke <name|id> --confirm`。

- [ ] **Step 6: 运行确认通过**

Run: `./gow build ./... && ./gow test -race ./internal/agent/ ./internal/mcpsrv/ ./cmd/cloudfs/ -count=1`
Expected: PASS（含 `http_test.go`、`copy_jobs_test.go`、`resources_test.go`、`subscriptions_test.go` 对 `requireBearer` 的既有用例不改动通过）

- [ ] **Step 7: 提交**

```bash
git add internal/agent internal/mcpsrv cmd/cloudfs
git commit -m "feat(mcp): scoped access tokens for the HTTP transport"
```

---

## Task A7：`mcp install --transport http`、非 owner 警告、`/mcp/connect`、`/mcp/tokens`（T-35 做法第 2–3 点）

**Files:**
- Create: `internal/mcpsrv/client_config_test.go`、`internal/mcpsrv/testdata/claude_http.golden`、`internal/mcpsrv/testdata/codex_http.golden`、`internal/mcpsrv/testdata/claude_http_add.golden`
- Create: `internal/control/mcp_tokens.go`、`internal/control/mcp_tokens_test.go`
- Modify: `internal/mcpsrv/http.go`（`ClientOptions`、`ClientConfigFor`、`ClientAddCommand`；`ClientConfig` 变包装）、`internal/mcpsrv/server.go`（`Options.NonOwner`、`errRequiresOwner`）
- Modify: `internal/control/metrics.go`（3 条路由）、`internal/control/status.go`（`Collector.MCP`）
- Modify: `internal/daemon/agent_view.go`（`mcpView`、`Daemon.SetMCPHTTP`）、`internal/daemon/daemon.go`（`Collector()` 填 `MCP`）
- Modify: `cmd/cloudfs/main.go`（`mcpInstall` 支持 `--transport http --url --token`；`cmdMCP` 非 owner 警告与 `NonOwner: true`；`cmdMount` 563 与 `serveMCPHTTPWith` 调 `d.SetMCPHTTP(addr)`）
- Modify: `internal/i18n/catalog_zh.go`、`catalog_en.go`（`confirm.token_revoke`、`err.mcp_unavailable`、`err.token_invalid`）

**Interfaces:**
- Consumes: A6 `Store.CreateToken/Tokens/RevokeToken`、`TokenState`、`TokenSpec`。
- Produces:
```go
// internal/mcpsrv/http.go
type ClientOptions struct {
	Client    string   // claude | codex
	Binary    string
	Allow     []string
	ReadOnly  bool
	Transport string   // stdio (default) | http
	URL       string   // http: e.g. http://127.0.0.1:8765/
	Token     string   // http: "" renders the literal placeholder <token>
}
func ClientConfigFor(o ClientOptions) (string, error)
func ClientAddCommand(o ClientOptions) string // claude http: `claude mcp add --transport http cloudfs <url> --header "Authorization: Bearer <token>"`; others ""
// Options gains: NonOwner bool
var errRequiresOwner = errors.New("requires the storage owner; use the HTTP transport")
```
```go
// internal/control/mcp_tokens.go
type MCPView interface {
	Connect(ctx context.Context) MCPConnect
	Tokens(ctx context.Context) ([]agent.Principal, error)
	CreateToken(ctx context.Context, spec agent.TokenSpec) (string, agent.Principal, error) // 【决策点】回退时删除
	RevokeToken(ctx context.Context, id string) (agent.Principal, error)
}
type MCPConnect struct {
	HTTPListening bool              `json:"http_listening"`
	HTTPAddr      string            `json:"http_addr,omitempty"`
	URL           string            `json:"url,omitempty"`
	Owner         bool              `json:"owner"`
	StdioNonOwner bool              `json:"stdio_non_owner"` // always false in phase 1; T-43 fills it
	AuthRequired  bool              `json:"auth_required"`
	Snippets      map[string]string `json:"snippets"`     // claude, codex; token rendered as <token>
	AddCommands   map[string]string `json:"add_commands"` // claude
}
type TokenView struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Fingerprint string     `json:"fingerprint"`
	Read        []string   `json:"read"`
	Write       []string   `json:"write"`
	ReadOnly    bool       `json:"read_only"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	State       string     `json:"state"` // active | expired | revoked
}
type TokensResponse struct{ Tokens []TokenView `json:"tokens"` }
type TokenCreateRequest struct {
	Name       string   `json:"name"`
	Read       []string `json:"read"`
	Write      []string `json:"write"`
	ReadOnly   bool     `json:"read_only"`
	TTLSeconds int64    `json:"ttl_seconds"`
}
type TokenCreateResponse struct {
	Token     string            `json:"token"`
	Principal TokenView         `json:"principal"`
	Snippets  map[string]string `json:"snippets"` // with the real token substituted
}
type TokenRevokeRequest struct{ Confirm bool `json:"confirm"` }
// Collector gains: MCP MCPView
```
```go
// internal/daemon/agent_view.go
func (d *Daemon) SetMCPHTTP(addr string, envToken bool)       // envToken: CLOUDFS_MCP_TOKEN is set
func (d *Daemon) SetMCPSnippets(render func(url string) map[string]string) // injected by cmd/cloudfs so daemon does not import mcpsrv
```

- [ ] **Step 1: 写失败测试（mcpsrv 快照）**

```go
// internal/mcpsrv/client_config_test.go
func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if os.Getenv("CLOUDFS_UPDATE_GOLDEN") == "1" { os.WriteFile(p, []byte(got), 0o644) }
	want, err := os.ReadFile(p)
	if err != nil { t.Fatal(err) }
	if got != string(want) { t.Fatalf("%s mismatch:\n--- got\n%s\n--- want\n%s", name, got, want) }
}
func TestClaudeHTTPSnippetGolden(t *testing.T) {
	got, err := ClientConfigFor(ClientOptions{Client: "claude", Transport: "http", URL: "http://127.0.0.1:8765/"})
	if err != nil { t.Fatal(err) }
	golden(t, "claude_http.golden", got)
	var v map[string]any
	if err := json.Unmarshal([]byte(got), &v); err != nil { t.Fatalf("not JSON: %v", err) }
}
func TestClaudeAddCommandGolden(t *testing.T) {
	golden(t, "claude_http_add.golden", ClientAddCommand(ClientOptions{Client: "claude", Transport: "http", URL: "http://127.0.0.1:8765/"})+"\n")
}
func TestCodexHTTPSnippetGolden(t *testing.T) {
	got, err := ClientConfigFor(ClientOptions{Client: "codex", Transport: "http", URL: "http://127.0.0.1:8765/"})
	if err != nil { t.Fatal(err) }
	golden(t, "codex_http.golden", got)
}
func TestStdioSnippetIsUnchanged(t *testing.T) {
	old, _ := ClientConfig("claude", "/usr/local/bin/cloudfs", []string{"/work"}, true)
	now, _ := ClientConfigFor(ClientOptions{Client: "claude", Binary: "/usr/local/bin/cloudfs", Allow: []string{"/work"}, ReadOnly: true})
	if old != now { t.Fatal("the stdio snippet changed") }
}
func TestNonOwnerOptionIsCarried(t *testing.T) { /* New(Options{FS:..., NonOwner:true}).opt.NonOwner == true (session tools consume it in A9) */ }
```
`testdata/claude_http.golden`：
```json
{
  "mcpServers": {
    "cloudfs": {
      "type": "http",
      "url": "http://127.0.0.1:8765/",
      "headers": {
        "Authorization": "Bearer <token>"
      }
    }
  }
}
```
`testdata/claude_http_add.golden`：
```
claude mcp add --transport http cloudfs http://127.0.0.1:8765/ --header "Authorization: Bearer <token>"
```
`testdata/codex_http.golden`：
```toml
[mcp_servers.cloudfs]
url = "http://127.0.0.1:8765/"
http_headers = { "Authorization" = "Bearer <token>" }
```
Codex 分支代码处加 `// UNVERIFIED: Codex streamable HTTP keys (url, http_headers) against the current Codex config reference`。

- [ ] **Step 2: 写失败测试（control）**

```go
// internal/control/mcp_tokens_test.go
var plainTokenShape = regexp.MustCompile(`cfs_[A-Za-z0-9_-]{20,}`)

func tokensFixture(t *testing.T) (*fixture, *agent.Store) {
	f, st, _ := agentFixture(t) // from agent_test.go
	f.coll.MCP = testMCPView{st: st, addr: "127.0.0.1:8765"}
	return f, st
}

func TestMCPConnectCarriesNoToken(t *testing.T) {
	f, st := tokensFixture(t)
	plain, _, _ := st.CreateToken(context.Background(), agent.TokenSpec{Name: "x"})
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/mcp/connect", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), plain) || plainTokenShape.MatchString(w.Body.String()) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `<token>`) { t.Fatal("snippets must carry the placeholder") }
}

func TestCreateTokenResponseIsNoStore(t *testing.T) { // 【决策点】回退时改为 TestTokensRouteRefusesCreate（POST → 405）
	f, _ := tokensFixture(t)
	w := uiCallControl(t, NewServer(f.coll).Handler(), "POST", "/mcp/tokens", `{"name":"codex","read":["/work"],"write":["/work/.agent"],"ttl_seconds":86400}`)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" { t.Fatalf("%d %q", w.Code, w.Header().Get("Cache-Control")) }
	var r TokenCreateResponse
	json.Unmarshal(w.Body.Bytes(), &r)
	if !plainTokenShape.MatchString(r.Token) || !strings.Contains(r.Snippets["claude"], r.Token) { t.Fatalf("%+v", r) }
}

func TestTokensListNeverCarriesAPlainToken(t *testing.T) {
	f, st := tokensFixture(t)
	plain, _, _ := st.CreateToken(context.Background(), agent.TokenSpec{Name: "a"})
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/mcp/tokens", "")
	if w.Code != 200 || plainTokenShape.MatchString(w.Body.String()) || strings.Contains(w.Body.String(), plain) { t.Fatalf("%s", w.Body) }
	if !strings.Contains(w.Body.String(), `"fingerprint":"`+plain[4:8]+`"`) { t.Fatal("fingerprint missing") }
}

func TestRevokeTokenNeedsConfirm(t *testing.T) {
	f, st := tokensFixture(t)
	_, p, _ := st.CreateToken(context.Background(), agent.TokenSpec{Name: "a"})
	h := NewServer(f.coll).Handler()
	if w := uiCallControl(t, h, "POST", "/mcp/tokens/"+p.ID+"/revoke", `{}`); w.Code != 400 { t.Fatalf("no confirm: %d", w.Code) }
	if w := uiCallControl(t, h, "POST", "/mcp/tokens/"+p.ID+"/revoke", `{"confirm":true}`); w.Code != 200 { t.Fatalf("confirm: %d %s", w.Code, w.Body) }
	w := uiCallControl(t, h, "GET", "/mcp/tokens", "")
	if !strings.Contains(w.Body.String(), `"state":"revoked"`) { t.Fatalf("%s", w.Body) }
}

func TestTokenRoutesAnswer503WithoutMCP(t *testing.T) {
	f := newFixture(t)
	for _, target := range []string{"/mcp/connect", "/mcp/tokens"} {
		if w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", target, ""); w.Code != 503 { t.Fatalf("%s %d", target, w.Code) }
	}
}
```
`testMCPView` 在测试文件中实现，逻辑与 `internal/daemon/agent_view.go` 的 `mcpView` 一致。

- [ ] **Step 3: 运行确认失败**

Run: `./gow test ./internal/mcpsrv/ -run 'TestClaudeHTTP|TestClaudeAdd|TestCodexHTTP|TestStdioSnippet|TestNonOwnerOption' -v; ./gow test ./internal/control/ -run 'TestMCPConnect|TestCreateTokenResponse|TestTokensList|TestRevokeToken|TestTokenRoutes|TestEveryRouteIsEitherGuardedOrArguedOpen' -v`
Expected: FAIL

- [ ] **Step 4: 实现**

- 路由：
```go
		{pattern: "/mcp/connect", handler: s.mcpConnect},
		{pattern: "/mcp/tokens", handler: s.mcpTokens},       // GET list; POST create 【决策点】
		{pattern: "/mcp/tokens/", handler: s.mcpTokenByPath, probe: "/mcp/tokens/x/revoke"},
```
- `mcpTokens` POST：`decodeMutation` → `TokenCreateRequest` → `agent.TokenSpec{TTL: time.Duration(q.TTLSeconds) * time.Second, ...}` → `CreateToken`；**先** `w.Header().Set("Cache-Control", "no-store")` 再 `writeJSON`；`agent.ErrTokenName/ErrTokenScope` → 400（文本为错误本身，不含令牌）；handler 内不调用任何日志函数输出请求/响应体。`Snippets` 由 `Connect(ctx).Snippets` 把 `<token>` 替换为明文。
- `mcpTokenByPath`：只接受 `/mcp/tokens/<id>/revoke` POST；`confirmed(w, r, q.Confirm, "confirm.token_revoke", name)`。
- `mcpView.Connect`：`HTTPListening = addr != ""`；`URL = "http://" + addr + "/"`；`Owner = d.Journal != nil && d.Journal.Owner()`；`AuthRequired = mcpHTTPToken != "" || HasLiveTokens`（daemon 不读 env，env 是否设置由 `SetMCPHTTP` 同时传入：签名改为 `SetMCPHTTP(addr string, envToken bool)`，并同步更新 Interfaces 与调用处）；`Snippets` 调 `mcpsrv.ClientConfigFor`——`internal/daemon` 已依赖 `mcpsrv`？执行前 `grep -n "cloudfs/internal/mcpsrv" internal/daemon/*.go`；若无依赖，改在 `cmd/cloudfs` 构造一个 `func(url string) map[string]string` 注入 `d.SetMCPSnippets`，避免 daemon 反向引入适配层。
- `mcpInstall(f, allow, readOnly, cfg *config.Config)`：`--transport http` 时 `url := f.str("url", "")`，为空则 `"http://" + firstNonEmpty(cfg.MCP.HTTP, "127.0.0.1:8765") + "/"`；打印 `ClientConfigFor(...)`；claude 时 stderr 打印 `ClientAddCommand(...)`；`--token` 为空时 stderr 提示 `create one with: cloudfs mcp token create --name <client> --read <prefix>`。
- `cmdMCP` stdio 分支：`nonOwner := d.Journal != nil && !d.Journal.Owner()`；为真时 stderr：`cloudfs: another process owns this cache (is "cloudfs mount" running?). This stdio server has its own view; use the HTTP transport: cloudfs mcp install --transport http`；`mcpsrv.Options{..., NonOwner: nonOwner}`。
- `cmdMount` 563 行 goroutine 前与 `serveMCPHTTPWith` 内：`d.SetMCPHTTP(addr, mcpHTTPToken() != "")`。

- [ ] **Step 5: 运行确认通过**

Run: `./gow build ./... && ./gow test -race ./internal/mcpsrv/ ./internal/control/ ./internal/daemon/ ./cmd/cloudfs/ -count=1 && ./gow vet ./...`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/mcpsrv internal/control internal/daemon cmd/cloudfs internal/i18n
git commit -m "feat(mcp): HTTP client registration and token management endpoints"
```

---

## Task A8：UI F2 — 访问令牌标签、揭示浮层、接入面板、设置完成页卡片（T-35 界面）

**Files:**
- Create: `internal/control/web/screens/agents_tokens.js`、`internal/control/web/token_reveal.js`（【决策点】回退时删除）、`internal/control/web/connect_panel.js`
- Create: `internal/control/ui_tokens_test.go`
- Modify: `internal/control/web/screens/agents.js`（`TABS` 加 `'tokens'`；顶部挂接入面板）、`internal/control/web/screens/setup.js`（`renderFinish` 两个分支加卡片，588-613）、`internal/control/web/i18n.js`

**Interfaces:**
- Consumes: A7 `GET /mcp/connect`、`GET /mcp/tokens`、`POST /mcp/tokens`、`POST /mcp/tokens/{id}/revoke`；A5 `scope_view.js` 的 `parsePrefixes`、`validateTokenScope`、`scopeParts`。
- Produces（JS）:
```js
// token_reveal.js — must not import store.js, must not touch any Web Storage
export function openTokenReveal({ token, snippets }) // openPanel; the token lives only in this call's closure
// connect_panel.js
export function renderConnectPanel(host, { open })  // GET /mcp/connect; returns dispose
// screens/agents_tokens.js
export function renderTokensTab(host)               // returns dispose
```

- [ ] **Step 1: 写失败测试**

```go
// internal/control/ui_tokens_test.go
package control

func TestTokenRevealKeepsTheTokenOutOfStorage(t *testing.T) {
	src := webSource(t, "web/token_reveal.js")
	for _, banned := range []string{"store.js", "localStorage", "sessionStorage", "indexedDB", "html:"} {
		if strings.Contains(src, banned) { t.Errorf("token_reveal.js mentions %s", banned) }
	}
	for _, want := range []string{"openPanel(", "copyBtn(token)", "t('tokens.reveal.once')", "snippets.claude", "snippets.codex"} {
		if !strings.Contains(src, want) { t.Errorf("token_reveal.js lacks %s", want) }
	}
	if strings.Contains(src, "let token") || strings.Contains(src, "var token") { t.Error("the token must not be held in a module-level variable") }
}

func TestTokensTabReachesTheTokenRoutes(t *testing.T) {
	src := webSource(t, "web/screens/agents_tokens.js")
	for _, want := range []string{
		"api.get('/mcp/tokens'", "api.post('/mcp/tokens'", // 【决策点】回退时改为断言不含 api.post('/mcp/tokens'
		"+ '/revoke'", "confirm: true", "confirmToken: tok.name", "validateTokenScope(", "openTokenReveal(",
		"t('tokens.state.' + tok.state)",
	} {
		if !strings.Contains(src, want) { t.Errorf("tokens tab lacks %s", want) }
	}
	if strings.Contains(src, "store.js") { t.Error("the tokens tab must not route a created token through the shared store") }
}

func TestConnectPanelCallsConnectAndLinksDiagnostics(t *testing.T) {
	src := webSource(t, "web/connect_panel.js")
	for _, want := range []string{"api.get('/mcp/connect'", "stdio_non_owner", "href: '#/diagnostics'", "http_listening"} {
		if !strings.Contains(src, want) { t.Errorf("connect panel lacks %s", want) }
	}
}

func TestSetupFinishLinksToAgentConnect(t *testing.T) {
	if n := strings.Count(webSource(t, "web/screens/setup.js"), "href: '#/agents?connect=1'"); n < 2 {
		t.Fatalf("both finish branches must offer the agent card, found %d", n)
	}
}

func TestTokenScreensHaveNoPasswordInput(t *testing.T) {
	for _, f := range []string{"web/token_reveal.js", "web/screens/agents_tokens.js", "web/connect_panel.js"} {
		src := webSource(t, f)
		if strings.Contains(src, "type: 'password'") || strings.Contains(src, `type="password"`) { t.Errorf("%s renders a password input", f) }
	}
}

func TestWebCatalogCoversTokenStates(t *testing.T) {
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, s := range []string{"active", "expired", "revoked"} {
		if !zh["tokens.state."+s] { t.Errorf("missing tokens.state.%s", s) }
	}
	for _, k := range []string{"agents.tab.tokens", "tokens.err.write_outside_read", "tokens.err.relative"} {
		if !zh[k] { t.Errorf("missing %s", k) }
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `./gow test ./internal/control/ -run 'TestTokenReveal|TestTokensTab|TestConnectPanel|TestSetupFinishLinks|TestTokenScreens|TestWebCatalogCoversTokenStates' -v`
Expected: FAIL

- [ ] **Step 3: 实现**

`token_reveal.js`：
```js
import { el, openPanel, copyBtn } from '/ui/ui.js';
import { t } from '/ui/i18n.js';

// The only place a plain access token is ever shown. It is a parameter of this
// call and nothing else keeps it: closing the panel drops the last reference.
export function openTokenReveal({ token, snippets }) {
  const block = (label, text) => el('div', { style: 'margin-top:14px' },
    el('div', { class: 'eyebrow' }, label),
    el('pre', { class: 'detail', style: 'white-space:pre-wrap;overflow-wrap:anywhere' }, text),
    copyBtn(text));
  const content = el('div', {},
    el('p', { class: 'warn-text', role: 'alert' }, t('tokens.reveal.once')),
    el('code', { class: 'detail', style: 'overflow-wrap:anywhere' }, token), ' ', copyBtn(token),
    snippets.claude ? block(t('tokens.reveal.claude'), snippets.claude) : null,
    snippets.codex ? block(t('tokens.reveal.codex'), snippets.codex) : null);
  openPanel({ title: t('tokens.reveal.title'), content });
}
```
`screens/agents_tokens.js`：表格列 `tokens.col.name/fingerprint/read/write/expires/lastused/state`；状态单元 `el('span', {}, el('span', { class: 'dot ' + (tok.state === 'active' ? 'ok' : 'bad') }), ' ' + t('tokens.state.' + tok.state))`；"新建令牌"按钮 → `openForm`（rows 形态照 `screens/exports.js` 现有 `openForm` 调用；字段：名称 input、可读 textarea、可写 textarea、有效期 select `86400/604800/2592000/0`、只读 checkbox），`validate` 回调：`const key = validateTokenScope({ read: parsePrefixes(readText), write: parsePrefixes(writeText) }); return key ? t(key) : ''`；提交：
```js
const r = await api.post('/mcp/tokens', { name, read, write: writeText.trim() ? write : null, read_only: readOnly, ttl_seconds: ttl });
openTokenReveal({ token: r.token, snippets: r.snippets || {} });
load();
```
吊销（仅 `tok.state === 'active'` 行）：
```js
const ok = await confirmDelete({ title: t('tokens.revoke.title', tok.name), body: t('tokens.revoke.body'), confirmToken: tok.name, confirmLabel: t('tokens.revoke') });
if (!ok) return;
await api.post('/mcp/tokens/' + encodeURIComponent(tok.id) + '/revoke', { confirm: true });
```
`connect_panel.js`：`<details>`（`open` 取 URL `connect=1`）；摘要行 `el('span', { class: 'dot ' + (c.http_listening ? 'ok' : 'warn') })` + `t(c.http_listening ? 'connect.http.on' : 'connect.http.off', c.http_addr || '')`；`t(c.owner ? 'connect.owner.yes' : 'connect.owner.no')`；未监听时 `el('pre', {}, 'mcp:\n  http: 127.0.0.1:8765')` + `t('connect.http.hint')`；`c.stdio_non_owner` 为真时黄色横幅 `el('div', { class: 'banner warn' }, t('connect.stdio.banner'), ' ', el('a', { href: '#/diagnostics' }, t('connect.stdio.link')))`；监听时展示 `c.add_commands.claude` + `copyBtn`。
`screens/agents.js`：`TABS = ['sessions', 'audit', 'tokens']`，`show()` 增加 `name === 'tokens' ? renderTokensTab(body)`；标题下方 `renderConnectPanel(panelHost, { open: params.get('connect') === '1' })`。
`screens/setup.js` 两个分支 panel 内追加：
```js
el('div', { class: 'card', style: 'margin-top:14px' },
  el('h4', { style: 'margin:0' }, t('setup.finish.agent.title')),
  el('p', { class: 'detail' }, t('setup.finish.agent.body')),
  el('a', { class: 'btn', href: '#/agents?connect=1' }, iconEl('bot'), t('setup.finish.agent.open'))),
```
（`setup.js` 若未 import `iconEl`，补 import。）

i18n 键（zh / en）：
| key | zh | en |
|---|---|---|
| `agents.tab.tokens` | 访问令牌 | Access tokens |
| `tokens.col.name` | 名称 | Name |
| `tokens.col.fingerprint` | 指纹 | Fingerprint |
| `tokens.col.read` | 可读 | Read |
| `tokens.col.write` | 可写 | Write |
| `tokens.col.expires` | 过期 | Expires |
| `tokens.col.lastused` | 最后使用 | Last used |
| `tokens.col.state` | 状态 | State |
| `tokens.state.active` | 有效 | Active |
| `tokens.state.expired` | 已过期 | Expired |
| `tokens.state.revoked` | 已吊销 | Revoked |
| `tokens.new` | 新建令牌 | New token |
| `tokens.form.name` | 名称（小写字母、数字、连字符） | Name (lowercase letters, digits, hyphens) |
| `tokens.form.read` | 可读路径（每行一个） | Readable paths (one per line) |
| `tokens.form.write` | 可写路径（每行一个，留空同可读） | Writable paths (one per line; empty = same as readable) |
| `tokens.form.ttl` | 有效期 | Valid for |
| `tokens.ttl.1d` | 1 天 | 1 day |
| `tokens.ttl.7d` | 7 天 | 7 days |
| `tokens.ttl.30d` | 30 天 | 30 days |
| `tokens.ttl.never` | 永不过期 | Never |
| `tokens.form.readonly` | 只读 | Read only |
| `tokens.create` | 创建 | Create |
| `tokens.err.write_outside_read` | 可写路径必须在可读路径之内 | Writable paths must be inside readable paths |
| `tokens.err.relative` | 路径必须以 / 开头 | Paths must start with / |
| `tokens.reveal.title` | 新令牌 | New token |
| `tokens.reveal.once` | 关闭后无法再次查看，请现在复制保存 | This token is shown once. Copy it now; it cannot be shown again |
| `tokens.reveal.claude` | Claude Code 注册 | Claude Code registration |
| `tokens.reveal.codex` | Codex 注册 | Codex registration |
| `tokens.revoke` | 吊销 | Revoke |
| `tokens.revoke.title` | 吊销令牌 %s | Revoke token %s |
| `tokens.revoke.body` | 使用该令牌的 Agent 会立即失去访问，已打开的会话会被关闭。 | Agents using this token lose access immediately and their open sessions are closed. |
| `tokens.empty` | 还没有访问令牌 | No access tokens yet |
| `connect.title` | 接入 Agent | Connect an agent |
| `connect.http.on` | HTTP 传输监听于 %s | HTTP transport listening on %s |
| `connect.http.off` | HTTP 传输未启用 | HTTP transport is off |
| `connect.http.hint` | 在配置文件中加入以下内容后重启 cloudfs | Add this to the configuration file and restart cloudfs |
| `connect.owner.yes` | 当前进程持有存储 | This process owns the storage |
| `connect.owner.no` | 另一个进程持有存储 | Another process owns the storage |
| `connect.stdio.banner` | 检测到 stdio MCP 与挂载并存，请改用 HTTP 传输 | A stdio MCP server is running beside the mount; switch to the HTTP transport |
| `connect.stdio.link` | 查看诊断 | Open diagnostics |
| `setup.finish.agent.title` | 连接 Agent | Connect an agent |
| `setup.finish.agent.body` | 让 Claude Code 或 Codex 通过 HTTP 访问这些网盘，并按令牌限定路径。 | Let Claude Code or Codex reach these drives over HTTP, limited to the paths a token allows. |
| `setup.finish.agent.open` | 前往接入 | Set up access |

- [ ] **Step 4: 运行确认通过**

Run: `node --test internal/control/web/_tests/*.test.mjs && ./gow test ./internal/control/ -run 'TestToken|TestConnectPanel|TestSetupFinish|TestWebCatalog|TestWebScreens|TestEveryTranslationKey|TestIconsAreSizedAndDefined|TestBrowserModule|TestWebAppNeverAsksForACredential|TestScreensDoNotStringifyASkippedChild' -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/control/web internal/control/ui_tokens_test.go
git commit -m "feat(ui): access tokens tab, one-time reveal and agent connect panel"
```

---

## Task A9：交付箱 — `MCP.Workspace`、`begin/finish/list_sessions`、manifest、sandbox（T-36 后端）

**Files:**
- Create: `internal/agent/workspace.go`、`internal/agent/workspace_test.go`
- Create: `internal/mcpsrv/session_tools.go`、`internal/mcpsrv/session_tools_test.go`
- Modify: `internal/agent/session.go`（`Begin`、`FinishWith`、`ListQuery.Path/PrincipalID`）、`internal/config/config.go`（`MCP.Workspace` + 校验）、`config_test.go`
- Modify: `internal/mcpsrv/server.go`（`Options.Workspace`；`New` 调 `s.registerSessionTools()`）
- Modify: `internal/control/agent.go`（`/sessions?path=&sandbox=1`；`SessionDetail.Artifacts`；`ArtifactCount`）、`agent_test.go`
- Modify: `internal/daemon/agent_view.go`（`Workspace()` 返回配置值或推导值）、`cmd/cloudfs/main.go`（两处 `mcpsrv.Options` 加 `Workspace: cfg.MCP.Workspace`）

**Interfaces:**
- Consumes: A2 `Sessions`、`ListQuery`；A3 `AuditForSession`、`WriteTools`；A7 `Options.NonOwner`、`errRequiresOwner`。
- Produces:
```go
// internal/agent/workspace.go
var ErrWorkspaceUnset = errors.New("mcp.workspace is not configured and there is no --allow prefix to derive it from")
func DefaultWorkspace(configured string, sc Scope) (string, error) // configured wins; else first EffectiveRead prefix != "/" joined with ".agent"
func SessionDirName(client string, started time.Time, id string) string // "<client [a-z0-9-], default agent>-<YYYYMMDD>-<id[:8]>"
func ArtifactPaths(rows []AuditRow, sessionDir string) []string // ok rows of WriteTools (except delete); last checked path of each row; unique; sorted; excludes <sessionDir>/manifest.json
type Manifest struct {
	SessionID  string     `json:"session_id"`
	Client     string     `json:"client"`
	Principal  string     `json:"principal"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Scope      Scope      `json:"scope"`
	Summary    string     `json:"summary,omitempty"`
	Artifacts  []Artifact `json:"artifacts"`
}
// internal/agent/session.go
type BeginOptions struct {
	Name      string
	Sandbox   bool
	Workspace string // root; Begin appends SessionDirName
}
func (m *Sessions) Begin(ctx context.Context, current Session, opt BeginOptions) (Session, error)
func (m *Sessions) FinishWith(ctx context.Context, id, summary string, arts []Artifact) (Session, error)
// ListQuery gains: PrincipalID string
```
```go
// internal/mcpsrv
// Options gains: Workspace string
type beginSessionInput struct {
	Name    string `json:"name,omitempty" jsonschema:"Short label for this task"`
	Sandbox bool   `json:"sandbox,omitempty" jsonschema:"Limit writes to this session's directory"`
}
type beginSessionOutput struct {
	SessionID string `json:"session_id"`
	Workspace string `json:"workspace"`
	URI       string `json:"uri"`
}
type finishSessionInput struct {
	SessionID string `json:"session_id,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Share     bool   `json:"share,omitempty" jsonschema:"Attach download links for files already uploaded"`
}
type finishSessionOutput struct {
	SessionID string           `json:"session_id"`
	Manifest  string           `json:"manifest"`
	Artifacts []agent.Artifact `json:"artifacts"`
}
type listSessionsInput struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	State  string `json:"state,omitempty"`
}
```

- [ ] **Step 1: 写失败测试（agent、config）**

```go
// internal/agent/workspace_test.go
func TestSessionDirNameIsStableAndSafe(t *testing.T) {
	got := SessionDirName("Claude Code/1.0", time.Date(2026, 9, 14, 23, 0, 0, 0, time.UTC), "ab12cd34-ef56")
	if got != "claude-code-1-0-20260914-ab12cd34" { t.Fatalf("%q", got) }
	if SessionDirName("", time.Unix(0, 0).UTC(), "12345678") != "agent-19700101-12345678" { t.Fatal("empty client") }
}
func TestDefaultWorkspace(t *testing.T) {
	if ws, _ := DefaultWorkspace("/x/.box", Scope{}); ws != "/x/.box" { t.Fatal(ws) }
	if ws, _ := DefaultWorkspace("", Scope{Read: []string{"/work", "/gd"}}); ws != "/work/.agent" { t.Fatal(ws) }
	if _, err := DefaultWorkspace("", Scope{}); !errors.Is(err, ErrWorkspaceUnset) { t.Fatal(err) }
}
func TestArtifactPathsComeFromOkWritesOnly(t *testing.T) {
	rows := []AuditRow{
		{Tool: "write_file", Result: "ok", Paths: []string{"/w/s1/a.md"}},
		{Tool: "write_file", Result: "denied", Paths: []string{"/w/notes.md"}},
		{Tool: "move", Result: "ok", Paths: []string{"/w/s1/tmp.md", "/w/s1/b.md"}},
		{Tool: "delete", Result: "ok", Paths: []string{"/w/s1/old.md"}},
		{Tool: "read_text", Result: "ok", Paths: []string{"/w/other.md"}},
		{Tool: "write_file", Result: "ok", Paths: []string{"/w/s1/a.md"}},
	}
	got := ArtifactPaths(rows, "/w/s1")
	if strings.Join(got, ",") != "/w/s1/a.md,/w/s1/b.md" { t.Fatalf("%v", got) }
}
func TestBeginSandboxNarrowsWriteOnly(t *testing.T) { /* Begin(current with Scope{Read:["/work"]}, BeginOptions{Sandbox:true, Workspace:"/work/.agent"}) → s.Scope.Sandbox == s.Workspace; Check("/work/n.md", true) denied; Check(..., false) ok; previous active session for the conn is finished */ }
```
```go
// internal/config/config_test.go
func TestMCPWorkspaceMustBeCanonical(t *testing.T) {
	for _, bad := range []string{"work/.agent", "/work/../x", "/"} {
		if _, err := Parse([]byte(minimalConfigYAML + "\nmcp:\n  workspace: " + bad + "\n")); err == nil { t.Errorf("%q accepted", bad) }
	}
}
```

- [ ] **Step 2: 写失败测试（mcpsrv）**

```go
// internal/mcpsrv/session_tools_test.go
func begin(t *testing.T, e *env, args map[string]any) beginSessionOutput {
	t.Helper()
	var out beginSessionOutput
	if res := e.call(t, "begin_session", args, &out); res.IsError { t.Fatalf("begin_session: %s", errText(res)) }
	return out
}

func TestConcurrentSessionsKeepSeparateManifests(t *testing.T) {
	e1, st := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	e2 := secondClient(t, e1) // another in-memory client connected to e1.server
	b1 := begin(t, e1, map[string]any{}); b2 := begin(t, e2, map[string]any{})
	if b1.Workspace == b2.Workspace { t.Fatal("two sessions share a directory") }
	e1.call(t, "write_file", map[string]any{"path": b1.Workspace + "/out.md", "content": "one"}, nil)
	e2.call(t, "write_file", map[string]any{"path": b2.Workspace + "/out.md", "content": "two"}, nil)
	var f1, f2 finishSessionOutput
	e1.call(t, "finish_session", map[string]any{}, &f1)
	e2.call(t, "finish_session", map[string]any{}, &f2)
	if len(f1.Artifacts) != 1 || len(f2.Artifacts) != 1 || f1.Artifacts[0].Path == f2.Artifacts[0].Path { t.Fatalf("%+v %+v", f1, f2) }
	for _, f := range []finishSessionOutput{f1, f2} {
		var m agent.Manifest
		var rt readTextOutput
		e1.call(t, "read_text", map[string]any{"path": f.Manifest}, &rt)
		if err := json.Unmarshal([]byte(rt.Text), &m); err != nil || len(m.Artifacts) != 1 || !strings.HasPrefix(m.Artifacts[0].Path, path.Dir(f.Manifest)+"/") {
			t.Fatalf("manifest %s: %v %+v", f.Manifest, err, m)
		}
	}
	_ = st
}

func TestSandboxSessionCannotWriteOutsideItsDirectory(t *testing.T) {
	e, st := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	e.fake.Seed("work/notes.md", []byte("keep"))
	b := begin(t, e, map[string]any{"sandbox": true})
	if res := e.call(t, "write_file", map[string]any{"path": "/work/notes.md", "content": "x"}, nil); !res.IsError { t.Fatal("sandbox write escaped") }
	if res := e.call(t, "write_file", map[string]any{"path": b.Workspace + "/ok.md", "content": "x"}, nil); res.IsError { t.Fatal(errText(res)) }
	if res := e.call(t, "read_text", map[string]any{"path": "/work/notes.md"}, nil); res.IsError { t.Fatal("sandbox lost read access") }
	rows, _, _ := st.Audit(context.Background(), agent.AuditQuery{Result: "denied"})
	if len(rows) != 1 || rows[0].Paths[0] != "/work/notes.md" { t.Fatalf("%+v", rows) }
}

func TestShareSkipsLocalFilesWithoutALinkCall(t *testing.T) {
	e, _ := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	b := begin(t, e, map[string]any{})
	e.call(t, "write_file", map[string]any{"path": b.Workspace + "/pending.md", "content": "not uploaded"}, nil) // uploader is never drained
	start := time.Now()
	var f finishSessionOutput
	if res := e.call(t, "finish_session", map[string]any{"share": true}, &f); res.IsError { t.Fatal(errText(res)) }
	if time.Since(start) > time.Second { t.Fatalf("finish_session took %v", time.Since(start)) }
	if len(f.Artifacts) != 1 || f.Artifacts[0].State != "local" || f.Artifacts[0].DownloadURL != "" { t.Fatalf("%+v", f.Artifacts) }
	if n := e.fake.Calls("DownloadURL"); n != 0 { t.Fatalf("DownloadURL calls = %d", n) }
}

func TestShareLinksUploadedFiles(t *testing.T) { /* same as above but e.up.DrainAll(ctx) before finish → State "synced", DownloadURL != "", Calls("DownloadURL") == 1 */ }

func TestBeginSessionWithoutWorkspaceIsAConfigError(t *testing.T) {
	e, _ := newAgentEnv(t, Options{}, agent.Scope{})
	res := e.call(t, "begin_session", map[string]any{}, nil)
	if !res.IsError || !strings.Contains(errText(res), "mcp.workspace") { t.Fatalf("%v %s", res.IsError, errText(res)) }
	if n := e.fake.Calls("Mkdir"); n != 0 { t.Fatalf("Mkdir calls = %d", n) }
}

func TestNonOwnerRefusesSessionTools(t *testing.T) {
	e, _ := newAgentEnv(t, Options{Workspace: "/work/.agent", NonOwner: true}, agent.Scope{Read: []string{"/work"}})
	for _, tool := range []string{"begin_session", "finish_session", "list_sessions"} {
		if res := e.call(t, tool, map[string]any{}, nil); !res.IsError || !strings.Contains(errText(res), "requires the storage owner") { t.Errorf("%s: %s", tool, errText(res)) }
	}
}
```
`secondClient(t, e)` 在 `agent_env_test.go` 中加：对 `e.server` 再建一对 `mcp.NewInMemoryTransports()` 并 `Run`/`Connect`，返回共享 `fs/fake/up/j` 但 `session` 不同的 `*env`。A2 的 `TestEveryToolChecksItsPaths` 需把 `begin_session`、`finish_session`、`list_sessions` 登记进 `pathless`（原因：路径由服务端推导，内部走 `checkPath`）。

- [ ] **Step 3: 写失败测试（control）**

```go
func TestSessionsByPathFindsTheSession(t *testing.T) { /* Begin with workspace /work/.agent → GET /sessions?path=/work/.agent/<dir>/a.md returns exactly that session */ }
func TestSessionsSandboxFilter(t *testing.T)       { /* one sandbox + one normal → GET /sessions?sandbox=1 returns 1 */ }
func TestSessionDetailListsArtifacts(t *testing.T) { /* FinishWith 2 artifacts → GET /sessions/<id> "artifacts" length 2 and SessionView.artifacts == 2 */ }
```

- [ ] **Step 4: 运行确认失败**

Run: `./gow test ./internal/agent/ ./internal/config/ -run 'TestSessionDirName|TestDefaultWorkspace|TestArtifactPaths|TestBeginSandbox|TestMCPWorkspace' -v; ./gow test ./internal/mcpsrv/ -run 'TestConcurrentSessions|TestSandboxSession|TestShare|TestBeginSession|TestNonOwnerRefuses|TestEveryToolChecksItsPaths' -v; ./gow test ./internal/control/ -run 'TestSessionsByPath|TestSessionsSandbox|TestSessionDetail' -v`
Expected: FAIL

- [ ] **Step 5: 实现**

- `begin_session`：`NonOwner` → `fail(errRequiresOwner)`；`cur, _ := agent.FromContext(ctx)`；`ws, err := agent.DefaultWorkspace(s.opt.Workspace, s.scopeOf(ctx))` → 失败 `fail(err)`（文本含 `mcp.workspace`，**不**创建目录）；`if _, err := s.checkPath(ctx, ws, true); err != nil { fail }`；`sess, err := s.opt.Sessions.Begin(ctx, cur, agent.BeginOptions{Name: in.Name, Sandbox: in.Sandbox, Workspace: ws})`；用 `server.go` 中 `create_directory` 已在用的"逐级建目录"helper（执行前 `grep -n "func (s \*Server) mkdirAll\|func mkdirAll" internal/mcpsrv/server.go` 确认名字）创建 `sess.Workspace`；`FS.WriteFile(ctx, sess.Workspace+"/manifest.json", manifestJSON(skeleton), false)`；返回 `session_id`、`workspace`、`uri`（`resources.go` 现有 URI 编码函数）。
- `finish_session`：`id := in.SessionID`，空则当前会话；目标会话 `PrincipalID` 必须等于调用方会话的 `PrincipalID`，否则 `fail(agent.ErrDenied)`；`rows := AuditForSession(id, 10000)`；`paths := agent.ArtifactPaths(rows, sess.Workspace)`；对每个 `StatPath`：目录/不存在跳过；`State = "local"` 当 `attr.LocalOnly`，否则 `"synced"`；`in.Share && State == "synced"` → `lctx, cancel := context.WithTimeout(ctx, time.Second)`，`FS.DownloadURL(lctx, p)` 成功填 `DownloadURL/ExpiresAt`，失败忽略；`URI` 同上；写 `manifest.json`（覆盖）；`FinishWith(id, in.Summary, arts)`。
- `list_sessions`：`List(ListQuery{PrincipalID: cur.PrincipalID, ...})`。
- 三个工具注册条件 `s.opt.Sessions != nil`；`finish_session` 标注 `IdempotentHint: false`，`list_sessions` `ReadOnlyHint: true`。
- `Sessions.Begin`：当前连接的旧 active 会话置 `finished`（`summary` 为空）；新会话继承 `current.PrincipalID/ConnKey/Transport/Client*`；`Scope = current.Scope`，`Sandbox` 时 `Scope.Sandbox = Workspace`；`workspace` 列写入。
- `ListQuery.Path`：`WHERE workspace != '' AND (? = workspace OR substr(?, 1, length(workspace)+1) = workspace || '/')`。
- `config.Validate`：`Workspace != ""` 时要求 `strings.HasPrefix("/")`、`path.Clean(w) == w`、`w != "/"`。

- [ ] **Step 6: 运行确认通过**

Run: `./gow build ./... && ./gow test -race ./internal/agent/ ./internal/config/ ./internal/mcpsrv/ ./internal/control/ ./internal/daemon/ -count=1`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add internal/agent internal/config internal/mcpsrv internal/control internal/daemon cmd/cloudfs
git commit -m "feat(mcp): per-session workspaces with manifests and sandboxed writes"
```

---

## Task A10：UI F3 — 会话详情产物表、主窗口工作区标记、检查器"来自会话"（T-36 界面）

**Files:**
- Create: `internal/control/web/workspace_view.js`、`internal/control/web/_tests/workspace_view.test.mjs`
- Create: `internal/control/ui_sessions_test.go`
- Modify: `internal/control/web/session_panel.js`（产物表、摘要、sandbox 标记、打开工作区、`onFsChange` 刷新、`data-artifact`）
- Modify: `internal/control/web/screens/main.js`（名称单元格标记；`renderInspector` 按钮行加"来自会话"；`#/connections?path=` 深链）
- Modify: `internal/control/web/screens/agents_sessions.js`（"产物"列、"仅 sandbox"过滤）、`internal/control/web/i18n.js`

**Interfaces:**
- Consumes: A9 `GET /sessions?path=&limit=1`、`GET /sessions?sandbox=1`、`SessionDetail.artifacts`、`/status` 的 `agent.workspace`；现有 `GET /fs/stat?path=`、`GET /fs/download-url?path=`；`app.js` `onFsChange`。
- Produces（JS）:
```js
// workspace_view.js — zero imports
export function workspaceMark(path, workspace)  // 'root' | 'session' | ''   (session = direct child directory of the workspace)
export function sessionDirOf(path, workspace)   // '/work/.agent/codex-20260914-ab12cd34' for anything inside it, else ''
```

- [ ] **Step 1: 写失败测试（mjs）**

```js
// internal/control/web/_tests/workspace_view.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { workspaceMark, sessionDirOf } from '../workspace_view.js';

const ws = '/work/.agent';
test('the workspace root is marked as root', () => { assert.equal(workspaceMark('/work/.agent', ws), 'root'); });
test('a direct child directory is a session directory', () => { assert.equal(workspaceMark('/work/.agent/codex-20260914-ab12cd34', ws), 'session'); });
test('deeper paths and unrelated paths carry no mark', () => {
  assert.equal(workspaceMark('/work/.agent/codex-20260914-ab12cd34/out.md', ws), '');
  assert.equal(workspaceMark('/work/.agentx', ws), '');
  assert.equal(workspaceMark('/work/.agent', ''), '');
});
test('anything inside a session directory resolves to that directory', () => {
  assert.equal(sessionDirOf('/work/.agent/codex-20260914-ab12cd34/a/b.md', ws), '/work/.agent/codex-20260914-ab12cd34');
  assert.equal(sessionDirOf('/work/.agent', ws), '');
  assert.equal(sessionDirOf('/work/notes.md', ws), '');
});
```

- [ ] **Step 2: 写失败测试（Go）**

```go
// internal/control/ui_sessions_test.go
package control

// A signed link is fetched when someone asks for it and goes to the clipboard;
// it never becomes part of a table that stays on screen.
func TestArtifactLinkIsFetchedOnlyOnClickAndNeverRendered(t *testing.T) {
	src := webSource(t, "web/session_panel.js")
	i := strings.Index(src, "api.get('/fs/download-url?path=")
	if i < 0 { t.Fatal("the artifact table cannot copy a link") }
	if !strings.Contains(src[max(0, i-200):i], "onclick") { t.Error("the link is requested outside a click handler") }
	if strings.Count(src, "r.url") != 1 || !strings.Contains(src, "clipboard.writeText(r.url)") {
		t.Error("the signed URL must appear only in the clipboard call")
	}
	if strings.Contains(src, "copyBtn(r.url") { t.Error("copyBtn falls back to a toast that would print the URL") }
}

func TestArtifactTableShowsLiveStateAndActions(t *testing.T) {
	src := webSource(t, "web/session_panel.js")
	for _, want := range []string{"api.get('/fs/stat?path=", "'data-artifact'", "t('artifact.state.' + ", "onFsChange(", "'#/connections?path='", "t('session.sandbox')", "t('session.openworkspace')"} {
		if !strings.Contains(src, want) { t.Errorf("session panel lacks %s", want) }
	}
}

func TestInspectorLinksBackToTheSession(t *testing.T) {
	src := webSource(t, "web/screens/main.js")
	for _, want := range []string{"api.get('/sessions?path=' + encodeURIComponent(", "openSessionPanel(", "sessionDirOf("} {
		if !strings.Contains(src, want) { t.Errorf("main.js lacks %s", want) }
	}
}

func TestWorkspaceMarkUsesBotIconWithALabel(t *testing.T) {
	src := webSource(t, "web/screens/main.js")
	if !strings.Contains(src, "iconEl('bot')") || !strings.Contains(src, "'aria-label': t('workspace.mark.' + mark)") {
		t.Error("the workspace mark needs the bot icon and a translated aria-label")
	}
}

func TestSessionsTableHasArtifactsColumnAndSandboxFilter(t *testing.T) {
	src := webSource(t, "web/screens/agents_sessions.js")
	for _, want := range []string{"t('session.col.artifacts')", "'&sandbox=1'", "t('session.filter.sandbox')"} {
		if !strings.Contains(src, want) { t.Errorf("sessions tab lacks %s", want) }
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `node --test internal/control/web/_tests/workspace_view.test.mjs; ./gow test ./internal/control/ -run 'TestArtifactLink|TestArtifactTable|TestInspectorLinksBack|TestWorkspaceMark|TestSessionsTableHas' -v`
Expected: FAIL

- [ ] **Step 4: 实现**

`workspace_view.js`：
```js
function clean(p) { const parts = []; for (const s of String(p || '').split('/')) { if (!s || s === '.') continue; if (s === '..') parts.pop(); else parts.push(s); } return '/' + parts.join('/'); }
export function workspaceMark(path, workspace) {
  if (!workspace) return '';
  const p = clean(path), w = clean(workspace);
  if (p === w) return 'root';
  if (!p.startsWith(w + '/')) return '';
  return p.slice(w.length + 1).includes('/') ? '' : 'session';
}
export function sessionDirOf(path, workspace) {
  if (!workspace) return '';
  const p = clean(path), w = clean(workspace);
  if (!p.startsWith(w + '/')) return '';
  return w + '/' + p.slice(w.length + 1).split('/')[0];
}
```
注意 `workspaceMark` 对"会话目录内的直接文件"（如 `/work/.agent/x.md`）也会返回 `session`；主窗口只在 `entry.is_dir` 时渲染标记。

`session_panel.js` 产物表（接在审计尾巴之前）：
```js
const artRows = el('tbody', {});
function artifactRow(a) {
  const state = el('span', {}, '…');
  api.get('/fs/stat?path=' + encodeURIComponent(a.path))
    .then((st) => fill(state, el('span', { class: 'dot ' + (st.local_only ? 'warn' : 'ok') }), ' ' + t('artifact.state.' + (st.local_only ? 'local' : 'synced'))))
    .catch(() => fill(state, t('artifact.state.missing')));
  return el('tr', { 'data-artifact': a.path },
    el('td', { class: 'detail' }, a.path), el('td', { class: 'num' }, bytes(a.size)), el('td', {}, state),
    el('td', {},
      el('button', { onclick: () => { location.hash = '#/connections?path=' + encodeURIComponent(a.path); } }, t('artifact.open')),
      el('button', { onclick: async () => {
        try { const r = await api.get('/fs/download-url?path=' + encodeURIComponent(a.path)); await navigator.clipboard.writeText(r.url); toast(t('artifact.copied')); }
        catch (_) { toast(t('artifact.copyfailed'), 'bad'); }
      } }, t('artifact.copylink'))));
}
fill(artRows, ...(d.artifacts || []).map(artifactRow));
const stopFs = onFsChange((c) => { if (c.rescan || (d.artifacts || []).some((a) => (c.paths || []).some((p) => a.path === p || a.path.startsWith(p + '/')))) fill(artRows, ...(d.artifacts || []).map(artifactRow)); });
```
`/fs/stat` 响应字段名（`local_only`）执行前对照 `internal/control/fs.go` 的 stat 响应结构体复核。面板关闭时调用 `stopFs()`（挂在 `openPanel` 的关闭回调/`onEscape`，以 `ui.js:165` 实现为准）。面板头部：`s.sandbox ? el('span', { class: 'chip' }, iconEl('bot'), t('session.sandbox')) : null`、`s.summary ? el('p', { class: 'detail' }, s.summary) : null`、`s.workspace ? el('button', { onclick: () => { location.hash = '#/connections?path=' + encodeURIComponent(s.workspace); } }, t('session.openworkspace')) : null`。进行中会话 `d.artifacts` 为空时显示 `t('artifact.empty')`。

`screens/main.js`：
```js
import { workspaceMark, sessionDirOf } from '/ui/workspace_view.js';
import { openSessionPanel } from '/ui/session_panel.js';
function workspaceRoot() { const s = get().status; return s && s.agent ? s.agent.workspace || '' : ''; }
// name cell, after the name text, for directories only:
const mark = e.is_dir ? workspaceMark(e.path, workspaceRoot()) : '';
mark ? el('span', { class: 'dim', title: t('workspace.mark.' + mark), 'aria-label': t('workspace.mark.' + mark) }, iconEl('bot')) : null
// renderInspector button row gains:
sessionDirOf(e.path, workspaceRoot()) ? el('button', { onclick: () => fromSession(e) }, iconEl('bot'), t('inspector.fromsession')) : null,
async function fromSession(e) {
  try {
    const r = await api.get('/sessions?path=' + encodeURIComponent(e.path) + '&limit=1');
    const s = (r.sessions || [])[0];
    if (s) openSessionPanel(s.id); else toast(t('inspector.fromsession.none'));
  } catch (err) { toast(err.message, 'bad'); }
}
```
深链：`renderMain` 挂载时读 `new URLSearchParams(location.hash.split('?')[1] || '').get('path')`，若存在则 `cwd = 父目录`、首次 `load()` 完成后选中同名条目（找到对应 `tr` 调 `select`）。

`screens/agents_sessions.js`：列加 `t('session.col.artifacts')` → `s.artifacts`；表头上方 `el('label', {}, el('input', { type: 'checkbox', onchange: (ev) => { sandboxOnly = ev.target.checked; load(); } }), ' ' + t('session.filter.sandbox'))`；`load` 的 URL 追加 `(sandboxOnly ? '&sandbox=1' : '')`。

i18n 键（zh / en）：
| key | zh | en |
|---|---|---|
| `session.col.artifacts` | 产物 | Artifacts |
| `session.filter.sandbox` | 仅沙箱会话 | Sandboxed only |
| `session.sandbox` | 沙箱 | Sandboxed |
| `session.openworkspace` | 打开工作区目录 | Open workspace folder |
| `session.artifacts` | 产物 | Artifacts |
| `artifact.state.synced` | 已同步 | Synced |
| `artifact.state.local` | 上传中 | Uploading |
| `artifact.state.missing` | 已不存在 | Gone |
| `artifact.open` | 在文件中打开 | Show in files |
| `artifact.copylink` | 复制链接 | Copy link |
| `artifact.copied` | 链接已复制 | Link copied |
| `artifact.copyfailed` | 无法复制链接 | Could not copy the link |
| `artifact.empty` | 会话结束后在这里列出产物 | Artifacts are listed here when the session finishes |
| `workspace.mark.root` | Agent 工作区 | Agent workspace |
| `workspace.mark.session` | Agent 会话目录 | Agent session folder |
| `inspector.fromsession` | 来自会话 | From session |
| `inspector.fromsession.none` | 没有找到对应会话 | No matching session |

- [ ] **Step 5: 运行确认通过**

Run: `node --test internal/control/web/_tests/*.test.mjs && ./gow test ./internal/control/ -run 'TestArtifact|TestInspector|TestWorkspaceMark|TestSessionsTable|TestWebCatalog|TestWebScreens|TestEveryTranslationKey|TestIconsAreSizedAndDefined|TestBrowserModule|TestScreensDoNotStringifyASkippedChild|TestWebAppNeverAsksForACredential' -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/control/web internal/control/ui_sessions_test.go
git commit -m "feat(ui): session artifacts, workspace marks and the inspector's session link"
```

---

## Task A11：线 A 集成 — e2e 链路、浏览器冒烟门控、文档、TODO 收口

**Files:**
- Create: `test/e2e/browser_helper_test.go`（**与 B13 逐字相同**）
- Create: `test/e2e/agent_e2e_test.go`
- Modify: `docs/mcp.md`、`docs/vfs-changes.md`（顶部一行）、`docs/agent-roadmap.md`（一期线 A 状态）、`README.md`（配置键与命令表）、`TODO.md`（T-34/T-35/T-36 状态）

**Interfaces:**
- Consumes: A0–A10 全部。
- Produces（测试 helper，线 B 复用）:
```go
type stackOptions struct {
	extraYAML string                                     // appended to the e2e config template (top-level keys only; never mcp:)
	mcp       func(d *daemon.Daemon, o *mcpsrv.Options)  // adjust the in-memory MCP server's options
}
func newStackWith(t *testing.T, mode string, o stackOptions) *stack // FUSE mount; skips without FUSE
func newUnmountedStack(t *testing.T, o stackOptions) *stack         // no kernel mount; never skips for FUSE
func requireBrowser(t *testing.T) string
func startControlUI(t *testing.T, col *control.Collector) string
func renderedDOM(t *testing.T, chrome, url string) string
```

- [ ] **Step 1: 写 helper 文件（线 B 的 B13 Step 1 逐字复制本文件）**

```go
// test/e2e/browser_helper_test.go
package e2e

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/fusefs"
	"cloudfs/internal/mcpsrv"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stackOptions lets a test extend the shared e2e stack without editing
// newStack: extra top-level configuration, and a hook on the MCP options once
// the daemon exists.
type stackOptions struct {
	extraYAML string
	mcp       func(d *daemon.Daemon, o *mcpsrv.Options)
}

func newStackWith(t *testing.T, mode string, o stackOptions) *stack {
	t.Helper()
	if ok, why := fusefs.Supported(); !ok {
		t.Skipf("FUSE unavailable: %s", why)
	}
	return buildStack(t, mode, o, true)
}

func newUnmountedStack(t *testing.T, o stackOptions) *stack {
	t.Helper()
	return buildStack(t, "writeback", o, false)
}

func buildStack(t *testing.T, mode string, o stackOptions, mount bool) *stack {
	t.Helper()
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	mountDir := filepath.Join(base, "mnt")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(base, "config.yaml")
	body := fmt.Sprintf(configTemplate, cacheDir, mountDir, mode) + o.extraYAML
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: "e2e"})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	fake, _ := provider.Unwrap(d.Providers["demo"]).(*fakeprovider.Fake)
	if fake == nil {
		cancel()
		d.Close()
		t.Fatal("the demo remote did not resolve to the fake provider")
	}
	var m *fusefs.Mount
	if mount {
		m, err = fusefs.MountFS(fusefs.MountOptions{
			Options: fusefs.Options{FS: d.FS, AttrTimeout: time.Second, EntryTimeout: time.Second},
			Path:    mountDir,
		})
		if err != nil {
			cancel()
			d.Close()
			t.Fatalf("mount: %v", err)
		}
	}
	opts := mcpsrv.Options{FS: d.FS, Version: "e2e"}
	if o.mcp != nil {
		o.mcp(d, &opts)
	}
	srv, err := mcpsrv.New(opts)
	if err != nil {
		if m != nil {
			m.Unmount()
		}
		cancel()
		d.Close()
		t.Fatal(err)
	}
	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		if m != nil {
			m.Unmount()
		}
		cancel()
		d.Close()
		t.Fatal(err)
	}
	s := &stack{d: d, mount: m, dir: mountDir, fake: fake, session: session, cfg: cfg}
	t.Cleanup(func() {
		session.Close()
		if m != nil {
			m.Unmount()
		}
		cancel()
		d.Close()
	})
	return s
}

// requireBrowser skips unless CLOUDFS_BROWSER=1 and a Chrome or Chromium
// binary can be found. CLOUDFS_CHROME names one explicitly.
func requireBrowser(t *testing.T) string {
	t.Helper()
	if os.Getenv("CLOUDFS_BROWSER") != "1" {
		t.Skip("set CLOUDFS_BROWSER=1 to run the headless browser smoke")
	}
	for _, c := range []string{
		os.Getenv("CLOUDFS_CHROME"),
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser",
	} {
		if c == "" {
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	t.Skip("CLOUDFS_BROWSER=1 but no Chrome or Chromium was found; set CLOUDFS_CHROME")
	return ""
}

// startControlUI serves the embedded console on a loopback port, the same
// handler the daemon mounts.
func startControlUI(t *testing.T, col *control.Collector) string {
	t.Helper()
	srv := control.NewServer(col)
	srv.EnableUI()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// renderedDOM loads url in headless Chrome, lets the page's modules and first
// API calls settle, and returns the serialized DOM.
func renderedDOM(t *testing.T, chrome, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, chrome,
		"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--user-data-dir="+t.TempDir(), "--virtual-time-budget=10000", "--timeout=20000",
		"--dump-dom", url).Output()
	if err != nil {
		t.Fatalf("headless chrome %s: %v", url, err)
	}
	return string(out)
}
```
（若 `/events` 长连接导致 `--dump-dom` 在 `--timeout` 之前不返回，先确认 `--virtual-time-budget` 已生效；仍不返回时在测试中改用 `?lang=en` 之外再加 `CLOUDFS_CHROME` 指向 Chromium 新版，而**不**修改产品代码。）

- [ ] **Step 2: 写 e2e 用例**

```go
// test/e2e/agent_e2e_test.go
package e2e

type bearer struct{ token string }
func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context()); r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// The whole of phase one line A, from a token to a denied row a person can see.
func TestAgentTokenSandboxChainAndAuditInTheBrowser(t *testing.T) {
	s := newUnmountedStack(t, stackOptions{})
	ctx := context.Background()
	s.fake.Seed("work/notes.md", []byte("keep me"))
	plain, _, err := s.d.Agent.CreateToken(ctx, agent.TokenSpec{Name: "e2e", Read: []string{"/work"}}) // 【决策点】两种方案都走 Store，与 CLI 同一实现
	if err != nil { t.Fatal(err) }
	srv, err := mcpsrv.New(mcpsrv.Options{FS: s.d.FS, Sessions: s.d.Sessions, Workspace: "/work/.agent", Version: "e2e"})
	if err != nil { t.Fatal(err) }
	ts := httptest.NewServer(mcpsrv.NewHTTPHandler(srv, mcpsrv.HTTPAuth{Verify: s.d.Agent.VerifyToken}))
	defer ts.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "codex", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: &http.Client{Transport: bearer{plain}}}, nil)
	if err != nil { t.Fatal(err) }
	defer cs.Close()

	call := func(name string, args map[string]any, out any) *mcp.CallToolResult {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil { t.Fatalf("%s: %v", name, err) }
		if out != nil && res.StructuredContent != nil { b, _ := json.Marshal(res.StructuredContent); json.Unmarshal(b, out) }
		return res
	}
	var begun struct{ SessionID, Workspace string }
	var b map[string]string
	call("begin_session", map[string]any{"sandbox": true}, &b)
	begun.SessionID, begun.Workspace = b["session_id"], b["workspace"]
	for _, name := range []string{"a.md", "b.md"} {
		if res := call("write_file", map[string]any{"path": begun.Workspace + "/" + name, "content": name}, nil); res.IsError { t.Fatalf("write %s", name) }
	}
	if res := call("write_file", map[string]any{"path": "/work/notes.md", "content": "overwrite"}, nil); !res.IsError { t.Fatal("sandbox escaped") }
	var fin struct{ Artifacts []agent.Artifact `json:"artifacts"` }
	call("finish_session", map[string]any{"summary": "e2e"}, &fin)
	if len(fin.Artifacts) != 2 { t.Fatalf("artifacts %+v", fin.Artifacts) }

	h := control.NewServer(s.d.Collector()).Handler()
	if w := uiCall(t, h, "GET", "/audit?result=denied", ""); !strings.Contains(w.Body.String(), `"/work/notes.md"`) { t.Fatalf("audit: %s", w.Body) }
	var detail control.SessionDetail
	w := uiCall(t, h, "GET", "/sessions/"+begun.SessionID, "")
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || len(detail.Artifacts) != 2 || !detail.Session.Sandbox {
		t.Fatalf("session detail: %v %s", err, w.Body)
	}

	chrome := requireBrowser(t)
	base := startControlUI(t, s.d.Collector())
	if dom := renderedDOM(t, chrome, base+"/?lang=en#/agents?tab=audit"); !strings.Contains(dom, `data-result="denied"`) {
		t.Fatalf("the audit tab shows no denied row:\n%s", dom)
	}
	if dom := renderedDOM(t, chrome, base+"/?lang=en#/agents?session="+begun.SessionID); strings.Count(dom, "data-artifact=") != 2 {
		t.Fatalf("the session panel does not list two artifacts:\n%s", dom)
	}
}

// Needs a kernel mount: run on Linux with /dev/fuse.
func TestAgentSessionManifestMatchesTheMount(t *testing.T) {
	s := newStackWith(t, "writeback", stackOptions{mcp: func(d *daemon.Daemon, o *mcpsrv.Options) {
		o.Sessions, o.Workspace = d.Sessions, "/.agent"
	}})
	var b map[string]string
	s.callTool(t, "begin_session", map[string]any{}, &b)
	for _, name := range []string{"one.md", "two.md"} {
		s.callTool(t, "write_file", map[string]any{"path": b["workspace"] + "/" + name, "content": name}, nil)
	}
	s.callTool(t, "finish_session", map[string]any{}, nil)
	s.settle(t)
	dir := filepath.Join(s.dir, strings.TrimPrefix(b["workspace"], "/"))
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil { t.Fatal(err) }
	var m agent.Manifest
	if err := json.Unmarshal(raw, &m); err != nil { t.Fatal(err) }
	entries, _ := os.ReadDir(dir)
	var listed []string
	for _, e := range entries { if e.Name() != "manifest.json" { listed = append(listed, b["workspace"]+"/"+e.Name()) } }
	var inManifest []string
	for _, a := range m.Artifacts { inManifest = append(inManifest, a.Path) }
	sort.Strings(listed); sort.Strings(inManifest)
	if strings.Join(listed, ",") != strings.Join(inManifest, ",") { t.Fatalf("ls %v manifest %v", listed, inManifest) }
}
```
（`begin_session` 输出解码用 `map[string]string` 是因为三个字段都是字符串；若 A9 输出含非字符串字段，改为 `beginSessionOutput` 的本地镜像结构体。）

- [ ] **Step 3: 运行**

Run: `./gow test ./test/e2e/ -run 'TestAgentTokenSandboxChain' -v -count=1`
Expected: PASS（浏览器部分 SKIP：未设 `CLOUDFS_BROWSER`）
Run: `CLOUDFS_BROWSER=1 ./gow test ./test/e2e/ -run 'TestAgentTokenSandboxChain' -v -count=1`
Expected: PASS（本机 Chrome）
Run（Linux + `/dev/fuse`）: `./gow test ./test/e2e/ -run TestAgentSessionManifestMatchesTheMount -v -count=1`
Expected: PASS；macOS 无 macFUSE 为 SKIP，TODO 中标 `[~]` 并写明缺口。

- [ ] **Step 4: 手工验收 `claude mcp add`（有 Claude CLI 时）**

```bash
./gow build -o /tmp/cloudfs ./cmd/cloudfs
/tmp/cloudfs mcp install --client claude --transport http --url http://127.0.0.1:8765/ 2>&1 | tail -1 > /tmp/add.sh
HOME=$(mktemp -d) sh -c "$(sed 's/<token>/cfs_dummy/' /tmp/add.sh)"; echo "exit=$?"
```
Expected: `exit=0`。无 Claude CLI 时 TODO 对应验收行标 `[~]`，写"快照测试已覆盖，CLI 接受性待真机"。

- [ ] **Step 5: 文档**

- `docs/mcp.md`：新增"会话与作用域""访问令牌与 HTTP 接入""交付箱（begin/finish/list_sessions）""审计"四节；写明：无状态 HTTP 下同一令牌空闲 30 分钟轮转会话；回环地址在签发第一个令牌前保持今天的免认证行为；stdio 与 mount 并存时请用 HTTP；`share` 只对已同步文件给直链且直链会过期。工具表加 3 个会话工具；删除已实现条目的"规划中"标记。
- `docs/vfs-changes.md` 顶部加一行："`vfs.Change` 是内存、有损、无主体的刷新提示；持久的"谁做了什么"见 agent.db 审计（`docs/mcp.md` 审计节）。"
- `README.md`：配置示例加 `mcp.audit.retain`、`mcp.session.idle`、`mcp.workspace`；命令表加 `cloudfs audit`、`cloudfs sessions`、`cloudfs mcp token`、`cloudfs mcp install --transport http`。
- `docs/agent-roadmap.md`：一期线 A 状态；若【决策点】选了回退，改写 §UI 安全边界第 2 条。
- `TODO.md`：T-34、T-35、T-36 改 `[x]`（全部验收通过）或 `[~]`（逐条列出未满足的验收与原因，例如需要 FUSE 的 manifest 用例、`claude mcp add` 真机），每条验收后附证明它的测试名。

- [ ] **Step 6: 线 A 全量验证**

```bash
./gow build ./...
./gow vet ./...
./gow test $(./gow list ./... | grep -v -e internal/fusefs -e test/conformance -e test/e2e)
./gow test -race ./internal/agent/ ./internal/mcpsrv/ ./internal/control/ ./internal/daemon/ ./internal/config/ ./cmd/cloudfs/
node --test internal/control/web/_tests/*.test.mjs
./gow test ./test/perf/ -count=1
CLOUDFS_BROWSER=1 ./gow test ./test/e2e/ -run 'TestAgent' -v -count=1
```
Expected: 全部 PASS；`test/perf` 调用次数基线不变（审计与会话在工具路径上零额外 provider 调用）。

- [ ] **Step 7: 提交**

```bash
git add test/e2e docs README.md TODO.md
git commit -m "test(e2e): agent token, sandbox session and audit chain; docs for phase one line A"
```

---

# 线 B：T-37 内容索引 phase 1

线 B 的 B1–B8 是无用户界面的内部库（抽取、分块、存储、规则、索引器、检索）；第一个面向用户的后端任务是 B9（MCP 工具）与 B10（控制面/CLI），其后**紧跟** UI 任务 B11、B12。

## Task B0：`config.Index`、`meta.WalkSubtree`、`layers` 图标

**Files:**
- Modify: `internal/config/config.go`（`Config` 293 行加 `Index Index \`yaml:"index"\``；`Validate` 364 行附近调用 `c.Index.Validate()`）、`internal/config/config_test.go`
- Create: `internal/meta/walk_subtree.go`、`internal/meta/walk_subtree_test.go`
- Modify: `internal/control/web/icons.js`（加 `layers`）

**Interfaces:**
- Consumes: 现有 `config.Size`、`config.ParseSize`（config.go:30）、`meta.Store.ChildrenPage`（children_page.go:13）。
- Produces:
```go
// internal/config/config.go
type Index struct {
	Enabled            bool        `yaml:"enabled"`
	Pinned             bool        `yaml:"pinned"`
	Rules              []IndexRule `yaml:"rules"`
	Exclude            []string    `yaml:"exclude"`        // nil = DefaultIndexExclude
	MaxTextBytes       Size        `yaml:"max_text_bytes"` // default 2MiB
	MaxTotalText       Size        `yaml:"max_total_text"` // default 4GiB
	FetchBudget        string      `yaml:"fetch_budget"`   // default "2GiB/h"
	FetchBudgetPerHour int64       `yaml:"-"`
}
type IndexRule struct {
	Path        string   `yaml:"path"`
	Include     []string `yaml:"include"`       // nil = DefaultIndexInclude
	Exclude     []string `yaml:"exclude"`
	MaxFileSize Size     `yaml:"max_file_size"` // default 20MiB
}
var DefaultIndexInclude = []string{"**/*.md", "**/*.txt", "**/*.rst", "**/*.csv", "**/*.json", "**/*.yaml", "**/*.yml", "**/*.toml",
	"**/*.go", "**/*.py", "**/*.ts", "**/*.js", "**/*.rs", "**/*.java", "**/*.c", "**/*.h", "**/*.sh", "**/*.sql", "**/*.html",
	"**/*.pdf", "**/*.docx", "**/*.xlsx", "**/*.pptx"}
var DefaultIndexExclude = []string{"**/.env", "**/*.pem", "**/id_rsa*", "**/.git/**", "**/node_modules/**"}
func (x *Index) Validate() error
```
```go
// internal/meta/walk_subtree.go
var SkipDir = errors.New("meta: skip this directory")
// WalkSubtree visits every descendant of ino depth-first in name order, reading
// children 500 at a time so a large tree never sits in memory. base is the
// virtual path of ino; visit receives each node with its virtual path. Returning
// SkipDir from a directory's visit skips its children.
func (s *Store) WalkSubtree(ctx context.Context, ino uint64, base string, visit func(n Node, p string) error) error
```

- [ ] **Step 1: 写失败测试**

```go
// internal/config/config_test.go
func TestIndexIsOffByDefaultWithSafeDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimalConfigYAML))
	if err != nil { t.Fatal(err) }
	x := cfg.Index
	if x.Enabled || x.Pinned || len(x.Rules) != 0 { t.Fatalf("%+v", x) }
	if x.MaxTextBytes != 2<<20 || x.MaxTotalText != 4<<30 || x.FetchBudgetPerHour != 2<<30 { t.Fatalf("%+v", x) }
	if len(x.Exclude) != len(DefaultIndexExclude) { t.Fatalf("exclude %v", x.Exclude) }
}
func TestIndexRulesAreValidated(t *testing.T) {
	for name, yaml := range map[string]string{
		"relative":   "index:\n  rules:\n    - path: work\n",
		"dotdot":     "index:\n  rules:\n    - path: /work/../x\n",
		"duplicate":  "index:\n  rules:\n    - path: /work\n    - path: /work\n",
		"bad budget": "index:\n  fetch_budget: lots\n",
		"bad glob":   "index:\n  rules:\n    - path: /w\n      include: [\"[\"]\n",
	} {
		if _, err := Parse([]byte(minimalConfigYAML + "\n" + yaml)); err == nil { t.Errorf("%s accepted", name) }
	}
}
func TestIndexRuleDefaults(t *testing.T) {
	cfg, _ := Parse([]byte(minimalConfigYAML + "\nindex:\n  enabled: true\n  fetch_budget: 512MiB/h\n  rules:\n    - path: /work/notes\n"))
	r := cfg.Index.Rules[0]
	if r.MaxFileSize != 20<<20 || len(r.Include) != len(DefaultIndexInclude) || cfg.Index.FetchBudgetPerHour != 512<<20 { t.Fatalf("%+v %d", r, cfg.Index.FetchBudgetPerHour) }
}
```
```go
// internal/meta/walk_subtree_test.go
func TestWalkSubtreeVisitsEveryDescendantWithItsPath(t *testing.T) {
	// Build /a, /a/b.txt, /a/c, /a/c/d.txt, /e.txt with the node-insert helpers
	// store_test.go already uses, then WalkSubtree(root, "/", …).
	// want paths, in order: /a, /a/b.txt, /a/c, /a/c/d.txt, /e.txt
}
func TestWalkSubtreeSkipDir(t *testing.T) { /* returning SkipDir at /a omits /a/b.txt, /a/c, /a/c/d.txt */ }
func TestWalkSubtreePagesLargeDirectories(t *testing.T) { /* 1200 children in one dir → 1200 visits, no duplicates */ }
```
（三个 meta 测试按注释写全：节点插入 helper 以 `internal/meta/store_test.go` / `insert_test.go` 现有函数为准。）

- [ ] **Step 2: 运行确认失败**

Run: `./gow test ./internal/config/ -run 'TestIndex' -v; ./gow test ./internal/meta/ -run TestWalkSubtree -v`
Expected: FAIL

- [ ] **Step 3: 实现**

- `Index.Validate`：填默认值；`FetchBudget` 必须形如 `<size>/h`，`ParseSize` 解析；规则路径 `strings.HasPrefix("/")` 且 `path.Clean(p) == p`，重复拒绝；每个 include/exclude 模式对每个 `/` 分段调用 `path.Match(seg, "")` 检查 `path.ErrBadPattern`（`**` 段跳过）；`MaxTextBytes > 0`、`MaxTotalText >= MaxTextBytes`。
- `WalkSubtree`：显式栈 `[]frame{ino, base}`；对每个目录循环 `ChildrenPage(ctx, dir, after, 0, 500)`，`after = 本页最后一个 Name`，页不足 500 结束；子节点路径 `path.Join(base, n.Name)`；`visit` 返回 `SkipDir` 且节点是目录时不入栈；其他错误立即返回。为保持"深度优先名序"，一个目录的子项读完一页就逐个 visit，遇到目录递归（函数内递归即可，深度 = 目录层级）。
- `icons.js` 加：
```js
  layers: svg('<path d="m12 3 9 5-9 5-9-5z"/><path d="m3 13 9 5 9-5"/>'),
```

- [ ] **Step 4: 运行确认通过**

Run: `./gow test -race ./internal/config/ ./internal/meta/ -count=1 && ./gow test ./internal/control/ -run TestIconsAreSizedAndDefined`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/config internal/meta/walk_subtree.go internal/meta/walk_subtree_test.go internal/control/web/icons.js
git commit -m "feat(config,meta): index configuration and a paged subtree walk"
```

---

## Task B1：`internal/textract` 文本类抽取与类型识别

**Files:**
- Create: `internal/textract/textract.go`、`internal/textract/text.go`、`internal/textract/textract_test.go`、`internal/textract/text_test.go`

**Interfaces:**
- Consumes: 无。
- Produces:
```go
package textract

type Kind string
const (
	KindText        Kind = "text"
	KindMarkdown    Kind = "markdown"
	KindCode        Kind = "code"
	KindDocx        Kind = "docx"
	KindXlsx        Kind = "xlsx"
	KindPptx        Kind = "pptx"
	KindPDF         Kind = "pdf"
	KindUnsupported Kind = ""
)
const ExtractorVer = 1

type Heading struct {
	Level  int
	Title  string
	Offset int64 // byte offset of the heading line in Doc.Text
}
type Doc struct {
	Text       string
	Headings   []Heading
	Truncated  bool
	OffsetKind string // "file": offsets equal file offsets (text kinds); "text": offsets into Text only
}
type Options struct {
	MaxTextBytes     int64
	Timeout          time.Duration
	MaxZipEntries    int
	MaxZipEntryBytes int64
	MaxZipTotalBytes int64
	MaxSheetRows     int
}
func DefaultOptions() Options // 2MiB, 30s, 4096, 64MiB, 256MiB, 5000
var (
	ErrNotUTF8     = errors.New("textract: not valid UTF-8")
	ErrUnsupported = errors.New("textract: unsupported file type")
	ErrZipBomb     = errors.New("textract: archive exceeds extraction limits")
	ErrNoText      = errors.New("textract: no extractable text")
	ErrGarbled     = errors.New("textract: extracted text is unreadable")
)
func KindOf(name string, head []byte) Kind
func Extract(ctx context.Context, kind Kind, r io.ReaderAt, size int64, opt Options) (Doc, error)
```

- [ ] **Step 1: 写失败测试**

```go
// internal/textract/text_test.go
func extractString(t *testing.T, kind Kind, s string, opt Options) (Doc, error) {
	t.Helper()
	return Extract(context.Background(), kind, strings.NewReader(s), int64(len(s)), opt)
}

func TestTextKeepsCRLFSoOffsetsAreFileOffsets(t *testing.T) {
	src := "line one\r\nline two\r\n"
	d, err := extractString(t, KindText, src, DefaultOptions())
	if err != nil { t.Fatal(err) }
	if d.Text != src || d.OffsetKind != "file" { t.Fatalf("%q %q", d.Text, d.OffsetKind) }
}

func TestNULBytesBecomeSpacesWithoutShiftingOffsets(t *testing.T) {
	d, _ := extractString(t, KindText, "a\x00b", DefaultOptions())
	if d.Text != "a b" { t.Fatalf("%q", d.Text) }
}

func TestNonUTF8Fails(t *testing.T) {
	if _, err := extractString(t, KindText, "caf\xe9", DefaultOptions()); !errors.Is(err, ErrNotUTF8) { t.Fatal(err) }
}

func TestMarkdownHeadingsCarryOffsets(t *testing.T) {
	src := "intro\n# 第一章\nbody\n## 1.1 范围\nmore\n"
	d, _ := extractString(t, KindMarkdown, src, DefaultOptions())
	if len(d.Headings) != 2 || d.Headings[0].Title != "第一章" || d.Headings[1].Level != 2 { t.Fatalf("%+v", d.Headings) }
	if !strings.HasPrefix(src[d.Headings[1].Offset:], "## 1.1 范围") { t.Fatalf("offset %d", d.Headings[1].Offset) }
}

func TestTextIsTruncatedOnARuneBoundary(t *testing.T) {
	opt := DefaultOptions(); opt.MaxTextBytes = 7
	d, _ := extractString(t, KindText, "中文字符串", opt) // 3 bytes per rune
	if !d.Truncated || d.Text != "中文" { t.Fatalf("%q %v", d.Text, d.Truncated) }
}

// internal/textract/textract_test.go
func TestKindOfUsesExtensionThenSniffs(t *testing.T) {
	for _, tc := range []struct{ name string; head []byte; want Kind }{
		{"a.md", []byte("# x"), KindMarkdown},
		{"a.go", []byte("package x"), KindCode},
		{"a.txt", []byte("hello"), KindText},
		{"a.docx", []byte("PK\x03\x04"), KindDocx},
		{"a.pdf", []byte("%PDF-1.4"), KindPDF},
		{"a.txt", []byte("\x89PNG\r\n\x1a\n"), KindUnsupported},
		{"a.mkv", []byte("\x1aE\xdf\xa3"), KindUnsupported},
		{"noext", []byte("plain words"), KindText},
	} {
		if got := KindOf(tc.name, tc.head); got != tc.want { t.Errorf("KindOf(%q) = %q want %q", tc.name, got, tc.want) }
	}
}

func TestExtractHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background()); cancel()
	if _, err := Extract(ctx, KindText, strings.NewReader("x"), 1, DefaultOptions()); !errors.Is(err, context.Canceled) { t.Fatal(err) }
}
```

- [ ] **Step 2: 运行确认失败**

Run: `./gow test ./internal/textract/ -v`
Expected: FAIL（包不存在）

- [ ] **Step 3: 实现**

- `KindOf`：小写扩展名映射：`.md/.markdown` → markdown；`.txt/.rst/.csv/.json/.yaml/.yml/.toml/.html/.htm` → text；`.go/.py/.ts/.js/.rs/.java/.c/.h/.sh/.sql` → code；`.docx/.xlsx/.pptx/.pdf` 各自（office 还要求 `head` 以 `PK\x03\x04` 开头，pdf 要求 `%PDF-`，否则 unsupported）；文本类若 `head` 含 NUL 比例 > 1% 或以常见二进制魔数开头（PNG、JPEG `\xff\xd8\xff`、ZIP、Matroska `\x1aE\xdf\xa3`、GZIP `\x1f\x8b`）→ unsupported；无扩展名且 `utf8.Valid(head)` 且无 NUL → text。
- `Extract`：先 `ctx.Err()`；分派：text/markdown/code → `extractText`；其余在 B2/B3 实现，此前返回 `ErrUnsupported`。
- `extractText`：`io.NewSectionReader(r, 0, min(size, opt.MaxTextBytes))` 读满；若 `size > MaxTextBytes` 置 `Truncated` 并回退到最后一个完整 rune（`utf8.RuneStart` 往回找）；`utf8.Valid` 否则 `ErrNotUTF8`；NUL → 空格（`bytes.ReplaceAll`，长度不变）；markdown 扫描行首 `#{1,6} `（跳过 fenced code 块内）记 `Heading{Level, Title: strings.TrimSpace, Offset}`；`OffsetKind = "file"`。

- [ ] **Step 4: 运行确认通过**

Run: `./gow test -race ./internal/textract/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/textract
git commit -m "feat(textract): text extraction that keeps file offsets"
```

---

## Task B2：docx / xlsx / pptx 抽取与 zip 上限（T-37 验收"docx Heading1"、"恶意 zip"）

**Files:**
- Create: `internal/textract/office.go`、`internal/textract/zipguard.go`、`internal/textract/office_test.go`
- Modify: `internal/textract/textract.go`（分派）

**Interfaces:**
- Consumes: B1 `Doc`、`Options`、`ErrZipBomb`、`ErrNoText`。
- Produces:
```go
// zipguard.go
func openZip(r io.ReaderAt, size int64, opt Options) (*guardedZip, error) // entry-count check up front
type guardedZip struct{ /* zr *zip.Reader; opt Options; total int64 */ }
func (g *guardedZip) Open(name string) (io.ReadCloser, error) // enforces per-entry and running total limits while reading
func (g *guardedZip) Names(prefix string) []string
// office.go
func extractDocx(ctx context.Context, g *guardedZip, opt Options) (Doc, error)
func extractXlsx(ctx context.Context, g *guardedZip, opt Options) (Doc, error)
func extractPptx(ctx context.Context, g *guardedZip, opt Options) (Doc, error)
```

- [ ] **Step 1: 写失败测试**

```go
// internal/textract/office_test.go
func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files { names = append(names, n) }
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil { t.Fatal(err) }
		io.WriteString(w, files[n])
	}
	zw.Close()
	return buf.Bytes()
}
func extractBytes(t *testing.T, kind Kind, b []byte) (Doc, error) {
	t.Helper()
	return Extract(context.Background(), kind, bytes.NewReader(b), int64(len(b)), DefaultOptions())
}

const docxBody = `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>
<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>第二章 设计</w:t></w:r></w:p>
<w:p><w:r><w:t>正文第一段</w:t></w:r><w:r><w:t xml:space="preserve"> 继续</w:t></w:r></w:p>
<w:tbl><w:tr><w:tc><w:p><w:r><w:t>a</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>b</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
</w:body></w:document>`

func TestDocxHeading1BecomesAMarkdownHeading(t *testing.T) {
	d, err := extractBytes(t, KindDocx, buildZip(t, map[string]string{"word/document.xml": docxBody}))
	if err != nil { t.Fatal(err) }
	if !strings.Contains(d.Text, "# 第二章 设计\n") || !strings.Contains(d.Text, "正文第一段 继续") { t.Fatalf("%q", d.Text) }
	if len(d.Headings) != 1 || d.Headings[0].Title != "第二章 设计" || d.Headings[0].Level != 1 { t.Fatalf("%+v", d.Headings) }
	if !strings.Contains(d.Text, "| a | b |") { t.Fatalf("table row missing: %q", d.Text) }
	if d.OffsetKind != "text" { t.Fatal(d.OffsetKind) }
}

func TestXlsxSheetsAndSharedStrings(t *testing.T) {
	z := buildZip(t, map[string]string{
		"xl/workbook.xml":          `<workbook><sheets><sheet name="预算" sheetId="1" r:id="rId1" xmlns:r="r"/></sheets></workbook>`,
		"xl/sharedStrings.xml":     `<sst><si><t>项目</t></si><si><t>金额</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c t="s"><v>0</v></c><c t="s"><v>1</v></c></row><row><c t="inlineStr"><is><t>服务器</t></is></c><c><v>1200</v></c></row></sheetData></worksheet>`,
	})
	d, err := extractBytes(t, KindXlsx, z)
	if err != nil { t.Fatal(err) }
	if !strings.Contains(d.Text, "## 预算\n") || !strings.Contains(d.Text, "项目,金额\n") || !strings.Contains(d.Text, "服务器,1200\n") { t.Fatalf("%q", d.Text) }
}

func TestPptxSlidesInNumericOrder(t *testing.T) {
	z := buildZip(t, map[string]string{
		"ppt/slides/slide10.xml": `<p:sld xmlns:p="p" xmlns:a="a"><a:t>ten</a:t></p:sld>`,
		"ppt/slides/slide2.xml":  `<p:sld xmlns:p="p" xmlns:a="a"><a:t>two</a:t></p:sld>`,
	})
	d, _ := extractBytes(t, KindPptx, z)
	if strings.Index(d.Text, "## Slide 2") > strings.Index(d.Text, "## Slide 10") { t.Fatalf("%q", d.Text) }
}

func TestZipWithTooManyEntriesFails(t *testing.T) {
	files := map[string]string{"word/document.xml": docxBody}
	for i := 0; i < 4096; i++ { files[fmt.Sprintf("junk/%05d", i)] = "" }
	if _, err := extractBytes(t, KindDocx, buildZip(t, files)); !errors.Is(err, ErrZipBomb) { t.Fatalf("got %v", err) }
}

func TestZipEntryOverTheLimitFails(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	io.WriteString(w, `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>`)
	w.Write(bytes.Repeat([]byte("A"), 70<<20)) // compresses to almost nothing
	io.WriteString(w, `</w:t></w:r></w:p></w:body></w:document>`)
	zw.Close()
	if _, err := extractBytes(t, KindDocx, buf.Bytes()); !errors.Is(err, ErrZipBomb) { t.Fatalf("got %v", err) }
}

func TestDocxWithoutTextIsNoText(t *testing.T) {
	z := buildZip(t, map[string]string{"word/document.xml": `<w:document xmlns:w="w"><w:body/></w:document>`})
	if _, err := extractBytes(t, KindDocx, z); !errors.Is(err, ErrNoText) { t.Fatalf("got %v", err) }
}
```

- [ ] **Step 2: 运行确认失败**

Run: `./gow test ./internal/textract/ -run 'TestDocx|TestXlsx|TestPptx|TestZip' -v`
Expected: FAIL

- [ ] **Step 3: 实现**

- `openZip`：`zip.NewReader(r, size)`；`len(zr.File) > opt.MaxZipEntries` → `ErrZipBomb`。`Open(name)` 返回包装 reader：读取时累计该条目字节，超过 `MaxZipEntryBytes` 或全局累计超过 `MaxZipTotalBytes` → 返回 `ErrZipBomb`（`fmt.Errorf("%w: %s", ErrZipBomb, name)`）。
- docx：`xml.NewDecoder` 流式读 `word/document.xml`；状态机：`w:p` 开始清空段落缓冲；`w:pStyle@w:val` 匹配 `^Heading([1-6])$` 记级别；`w:t` 字符数据拼接；`w:tc` 结束追加 `" | "` 分隔；`w:tr` 结束输出 `"| " + cells + " |\n"`；段落结束：有级别输出 `strings.Repeat("#", n) + " " + text + "\n"` 并记 `Heading{Offset: 当前 Text 长度}`，否则 `text + "\n"`；每 1000 个 token 检查 `ctx.Err()`。
- xlsx：`xl/sharedStrings.xml` 读入 `[]string`（条数上限 1,000,000，超出 `ErrZipBomb`）；`xl/workbook.xml` 取 sheet 名（按出现顺序），工作表文件 `xl/worksheets/sheet<N>.xml` 按数字序；每表输出 `## <name>\n`，每行把单元格值用 `,` 连接（`t="s"` 查共享串、`t="inlineStr"` 取 `is/t`、其余取 `v`），行数达 `MaxSheetRows` 截止并置 `Truncated`。
- pptx：`ppt/slides/slide(\d+)\.xml` 按数字排序；每页 `## Slide N\n` + 所有 `a:t` 用空格连接 + `\n`。
- 三者：结果 `strings.TrimSpace` 后为空 → `ErrNoText`；`OffsetKind = "text"`；`len(Text) > MaxTextBytes` 按 rune 边界截断置 `Truncated`。
- `encoding/xml` 不解析外部实体，无需额外处理；在 `office.go` 包注释写明。

- [ ] **Step 4: 运行确认通过**

Run: `./gow test -race ./internal/textract/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/textract
git commit -m "feat(textract): docx, xlsx and pptx extraction with archive limits"
```

---

## Task B3：PDF 抽取（新依赖、recover、超时、乱码启发式）

**Files:**
- Modify: `go.mod`、`go.sum`
- Create: `internal/textract/pdf.go`、`internal/textract/pdf_test.go`、`internal/textract/pdf_fixture_test.go`
- Modify: `internal/textract/textract.go`（分派）

**Interfaces:**
- Consumes: B1 `Doc`、`Options`、`ErrNoText`、`ErrGarbled`。
- Produces:
```go
func extractPDF(ctx context.Context, r io.ReaderAt, size int64, opt Options) (doc Doc, err error)
func garbled(s string) bool // > 30% of runes are U+FFFD, C0/C1 controls (except \t \n \r) or private-use
```

- [ ] **Step 1: 获取依赖（需要网络）**

Run: `./gow get github.com/ledongthuc/pdf@v0.0.0-20220302134840-0c2507a12d80 && ./gow mod tidy`
Expected: `go.mod` 出现该 require。
**离线回退**：若无法下载，`extractPDF` 返回 `fmt.Errorf("%w: pdf support is not built in", ErrUnsupported)`，保留 Step 2 的 `garbled`/fixture 测试，`TestMinimalPDFExtractsText` 用 `t.Skip("UNVERIFIED: ledongthuc/pdf not available offline")`，并在 B13 的 TODO 更新中把"PDF 抽取"标 `[~]`。

- [ ] **Step 2: 写失败测试**

```go
// internal/textract/pdf_fixture_test.go
// minimalPDF builds a one-page PDF whose content stream shows text with a
// standard Type1 font, computing the xref offsets so no fixture file is needed.
func minimalPDF(text string) []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	obj("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>")
	stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
	obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}
```
```go
// internal/textract/pdf_test.go
func TestMinimalPDFExtractsText(t *testing.T) {
	d, err := extractBytes(t, KindPDF, minimalPDF("Quarterly cloud storage report"))
	if err != nil { t.Fatal(err) }
	if !strings.Contains(d.Text, "Quarterly cloud storage report") || d.OffsetKind != "text" { t.Fatalf("%q", d.Text) }
}

func TestMalformedPDFDoesNotPanic(t *testing.T) {
	good := minimalPDF("x")
	for _, b := range [][]byte{[]byte("%PDF-1.4\ngarbage"), good[:len(good)/2], bytes.Repeat([]byte{0xff}, 4096)} {
		func() {
			defer func() { if r := recover(); r != nil { t.Fatalf("panic escaped: %v", r) } }()
			if _, err := extractBytes(t, KindPDF, b); err == nil { t.Fatal("malformed PDF extracted without error") }
		}()
	}
}

func TestGarbledTextIsDetected(t *testing.T) {
	if garbled("正常的中文文本和 English words") { t.Fatal("clean text flagged") }
	if !garbled(strings.Repeat("�", 40) + "ok") { t.Fatal("replacement characters not flagged") }
	if !garbled(strings.Repeat("\x01\x02", 30) + "ok") { t.Fatal("control characters not flagged") }
}

type slowReaderAt struct{ b []byte }
func (s slowReaderAt) ReadAt(p []byte, off int64) (int, error) { time.Sleep(50 * time.Millisecond); return bytes.NewReader(s.b).ReadAt(p, off) }

func TestPDFTimeoutIsHonoured(t *testing.T) {
	opt := DefaultOptions(); opt.Timeout = 20 * time.Millisecond
	b := minimalPDF(strings.Repeat("slow ", 100))
	start := time.Now()
	_, err := Extract(context.Background(), KindPDF, slowReaderAt{b}, int64(len(b)), opt)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second { t.Fatalf("%v after %v", err, time.Since(start)) }
}
```

- [ ] **Step 3: 运行确认失败**

Run: `./gow test ./internal/textract/ -run 'TestMinimalPDF|TestMalformedPDF|TestGarbled|TestPDFTimeout' -v`
Expected: FAIL

- [ ] **Step 4: 实现**

```go
// internal/textract/pdf.go
// UNVERIFIED: CJK PDFs (CID fonts) often extract as mojibake with this library;
// verify quality on real Chinese documents before promising it.
func extractPDF(ctx context.Context, r io.ReaderAt, size int64, opt Options) (Doc, error) {
	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	type result struct { text string; err error }
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil { done <- result{err: fmt.Errorf("textract: pdf parser panicked: %v", p)} }
		}()
		rd, err := pdf.NewReader(r, size)
		if err != nil { done <- result{err: err}; return }
		plain, err := rd.GetPlainText()
		if err != nil { done <- result{err: err}; return }
		var buf bytes.Buffer
		_, err = io.Copy(&buf, io.LimitReader(plain, opt.MaxTextBytes+1))
		done <- result{text: buf.String(), err: err}
	}()
	select {
	case <-ctx.Done():
		return Doc{}, ctx.Err() // the parser goroutine is abandoned; it holds only the ReaderAt
	case res := <-done:
		if res.err != nil { return Doc{}, res.err }
		text := strings.TrimSpace(res.text)
		if text == "" { return Doc{}, ErrNoText }
		if garbled(text) { return Doc{}, ErrGarbled }
		d := Doc{Text: text, OffsetKind: "text"}
		if int64(len(d.Text)) > opt.MaxTextBytes { d.Text, d.Truncated = truncateRunes(d.Text, opt.MaxTextBytes), true }
		return d, nil
	}
}
```
`pdf.NewReader`/`GetPlainText` 以该版本实际 API 为准（执行前 `ls $(./gow env GOMODCACHE)/github.com/ledongthuc/pdf@*/` 查看 `read.go`/`page.go`）。`truncateRunes` 与 B1 的截断共用一处实现。注释写明：超时后解析 goroutine 可能继续运行到自然结束，内存上限由 `LimitReader` 与单文档 `max_file_size` 共同约束。

- [ ] **Step 5: 运行确认通过**

Run: `./gow test -race ./internal/textract/ -v && ./gow vet ./internal/textract/`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add go.mod go.sum internal/textract
git commit -m "feat(textract): PDF text extraction behind recover, timeout and a garble check"
```

---

## Task B4：分块 `chunk.go`

**Files:**
- Create: `internal/textract/chunk.go`、`internal/textract/chunk_test.go`

**Interfaces:**
- Consumes: B1 `Doc`、`Heading`、`Kind`。
- Produces:
```go
const ChunkerVer = 1
type Chunk struct {
	Seq      int
	StartOff int64  // byte offset into Doc.Text (== file offset when Doc.OffsetKind == "file")
	EndOff   int64  // exclusive
	Heading  string // "第二章 > 2.1 范围"
	Text     string // == Doc.Text[StartOff:EndOff]
}
type ChunkOptions struct {
	Window     int // runes, default 800
	Overlap    int // runes, default 100
	CodeWindow int // runes, default 1200
}
func DefaultChunkOptions() ChunkOptions
func ChunkDoc(d Doc, kind Kind, opt ChunkOptions) []Chunk
```

- [ ] **Step 1: 写失败测试**

```go
func TestChunksNeverSplitARuneAndMatchTheirOffsets(t *testing.T) {
	d := Doc{Text: strings.Repeat("云端文件系统。", 700), OffsetKind: "file"}
	chunks := ChunkDoc(d, KindText, DefaultChunkOptions())
	if len(chunks) < 5 { t.Fatalf("%d chunks", len(chunks)) }
	for i, c := range chunks {
		if !utf8.ValidString(c.Text) || c.Text != d.Text[c.StartOff:c.EndOff] { t.Fatalf("chunk %d offsets do not match its text", i) }
		if n := utf8.RuneCountInString(c.Text); n > 800 { t.Fatalf("chunk %d has %d runes", i, n) }
		if c.Seq != i { t.Fatalf("seq %d at %d", c.Seq, i) }
	}
}

func TestConsecutiveChunksOverlapByAboutAHundredRunes(t *testing.T) {
	d := Doc{Text: strings.Repeat("a", 3000)}
	c := ChunkDoc(d, KindText, DefaultChunkOptions())
	for i := 1; i < len(c); i++ {
		overlap := c[i-1].EndOff - c[i].StartOff
		if overlap < 80 || overlap > 120 { t.Fatalf("overlap %d between %d and %d", overlap, i-1, i) }
	}
}

func TestSentenceBoundariesArePreferred(t *testing.T) {
	d := Doc{Text: strings.Repeat("短句。", 200) + strings.Repeat("长", 900)}
	c := ChunkDoc(d, KindText, DefaultChunkOptions())
	if !strings.HasSuffix(c[0].Text, "。") { t.Fatalf("first chunk ends mid-sentence: %q", c[0].Text[len(c[0].Text)-9:]) }
}

func TestHeadingPathFollowsSections(t *testing.T) {
	src := "# 第二章\n" + strings.Repeat("x", 50) + "\n## 2.1 范围\n" + strings.Repeat("y", 50) + "\n"
	d, _ := Extract(context.Background(), KindMarkdown, strings.NewReader(src), int64(len(src)), DefaultOptions())
	c := ChunkDoc(d, KindMarkdown, DefaultChunkOptions())
	if len(c) != 2 || c[0].Heading != "第二章" || c[1].Heading != "第二章 > 2.1 范围" { t.Fatalf("%+v", c) }
}

func TestChunkingIsDeterministic(t *testing.T) {
	d := Doc{Text: strings.Repeat("abc def。", 400)}
	a, b := ChunkDoc(d, KindText, DefaultChunkOptions()), ChunkDoc(d, KindText, DefaultChunkOptions())
	if !reflect.DeepEqual(a, b) { t.Fatal("chunking is not deterministic") }
}

func TestEmptyDocHasNoChunks(t *testing.T) {
	if n := len(ChunkDoc(Doc{}, KindText, DefaultChunkOptions())); n != 0 { t.Fatal(n) }
}
```

- [ ] **Step 2: 运行确认失败**

Run: `./gow test ./internal/textract/ -run 'TestChunk|TestConsecutive|TestSentence|TestHeadingPath|TestEmptyDoc' -v`
Expected: FAIL

- [ ] **Step 3: 实现**

1. **Section 切分**：按 `d.Headings` 的 `Offset` 把 `Text` 切成 section（首个标题前的内容为无标题 section）；维护标题栈（遇到 level ≤ 栈顶的标题则弹栈）得到 `"A > B"` 路径。
2. **窗口**：`window = opt.Window`（`KindCode` 用 `CodeWindow`）。section 不超过窗口 → 一个 chunk。
3. **超窗 section**：从 `start` 起向后数 `window` 个 rune 得到候选 `end`；在 `[start + window/2, end]` 范围内从后向前找最后一个句末（`。！？.!?` 后一位，或 `\n\n` 后一位；代码文件只找空行），找到则用它作 `end`；下一块 `start = end` 回退 `overlap` 个 rune（按 rune 回退，保证不切半字）；`start >= sectionEnd` 结束。
4. 所有偏移是字节偏移；`Text = d.Text[StartOff:EndOff]`；`Seq` 全文档连续。

- [ ] **Step 4: 运行确认通过**

Run: `./gow test -race ./internal/textract/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/textract/chunk.go internal/textract/chunk_test.go
git commit -m "feat(textract): heading-aware rune-window chunking with overlap"
```

---

## Task B5：`internal/index` 存储 — index.db schema v1

**Files:**
- Create: `internal/index/store.go`、`internal/index/schema.go`、`internal/index/lock.go`、`internal/index/lock_unix.go`、`internal/index/lock_windows.go`（照 `internal/export/lock*.go`，`lockName = "index.lock"`）、`internal/index/store_test.go`

**Interfaces:**
- Consumes: B4 `textract.Chunk`。
- Produces:
```go
package index

const schemaVersion = 1

type DocState int
const (
	DocOK DocState = iota
	DocDirty
	DocFailed
)
type Document struct {
	ID        int64
	Remote    string
	RemoteID  string
	Version   string
	Path      string
	Ino       uint64
	Kind      string
	Size      int64
	MTimeNS   int64
	TextHash  []byte
	Truncated bool
	State     DocState
	Error     string
	IndexedAt time.Time
}
type Rule struct {
	Path        string   `json:"path"`
	Include     []string `json:"include"`
	Exclude     []string `json:"exclude"`
	MaxFileSize int64    `json:"max_file_size"`
	Source      string   `json:"source"` // config | ui | tool
}
type PendingReason int
const (
	PendingChange PendingReason = iota
	PendingReconcile
	PendingRetry
)
type Stats struct {
	DocsOK, DocsDirty, DocsFailed int
	Chunks                        int
	TextBytes                     int64
	Pending                       int
}
type FailedDoc struct {
	Path      string    `json:"path"`
	Kind      string    `json:"kind"`
	Error     string    `json:"error"`
	IndexedAt time.Time `json:"indexed_at"`
}
type TextPage struct {
	Path       string `json:"path"`
	Text       string `json:"text"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	EOF        bool   `json:"eof"`
	Kind       string `json:"kind"`
}
var (
	ErrConfigRule = errors.New("index: this rule comes from the configuration file; remove it there")
	ErrNotIndexed = errors.New("index: this file is not indexed")
)

type Store struct{ /* db *sql.DB; lock *os.File; owner bool; writeMu sync.Mutex */ }
func OpenStore(dir string) (*Store, error)          // <dir>/index.db
func OpenStoreReadOnly(dir string) (*Store, error)  // os.ErrNotExist when missing
func (s *Store) Owner() bool
func (s *Store) Close() error
func (s *Store) EnsureIdentity(ctx context.Context, metaIdentity string) (reset bool, err error)
func (s *Store) UpsertDocument(ctx context.Context, d Document, text string, chunks []textract.Chunk) (int64, error)
func (s *Store) TouchVersion(ctx context.Context, id int64, version string) error // same text_hash: no re-chunk
func (s *Store) MarkFailed(ctx context.Context, d Document, cause error) error
func (s *Store) DocumentByRemote(ctx context.Context, remote, remoteID string) (Document, bool, error)
func (s *Store) DocumentByPath(ctx context.Context, p string) (Document, bool, error)
func (s *Store) SetPath(ctx context.Context, id int64, p string) error
func (s *Store) RenamePrefix(ctx context.Context, oldPrefix, newPrefix string) (int64, error)
func (s *Store) DeleteDocument(ctx context.Context, id int64) error
func (s *Store) DeleteMissing(ctx context.Context, seen map[int64]bool) (int64, error)
func (s *Store) Rules(ctx context.Context) ([]Rule, error)
func (s *Store) SyncConfigRules(ctx context.Context, rules []Rule) error
func (s *Store) AddRule(ctx context.Context, r Rule) error
func (s *Store) RemoveRule(ctx context.Context, p string) error
func (s *Store) Enqueue(ctx context.Context, ino uint64, p string, why PendingReason) error
func (s *Store) Pending(ctx context.Context, limit int) ([]PendingItem, error)
func (s *Store) Dequeue(ctx context.Context, ino uint64) error
func (s *Store) Stats(ctx context.Context) (Stats, error)
func (s *Store) Failed(ctx context.Context, cursor string, limit int) ([]FailedDoc, string, error)
func (s *Store) RetryFailed(ctx context.Context, p string) (int64, error) // "" = all; failed → dirty
func (s *Store) Text(ctx context.Context, p string, offset int64, max int) (TextPage, error)
func (s *Store) Reset(ctx context.Context) error
type PendingItem struct { Ino uint64; Path string; Reason PendingReason; QueuedAt time.Time }
```

- [ ] **Step 1: 写失败测试**

```go
// internal/index/store_test.go
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir())
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { s.Close() })
	return s
}
func chunksOf(text string) []textract.Chunk {
	return textract.ChunkDoc(textract.Doc{Text: text, OffsetKind: "file"}, textract.KindText, textract.DefaultChunkOptions())
}

func TestIndexSchemaV1AndFTSTriggers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, err := s.UpsertDocument(ctx, Document{Remote: "demo", RemoteID: "r1", Version: "v1", Path: "/work/a.md", Kind: "markdown"}, "cloudfs mounts drives", chunksOf("cloudfs mounts drives"))
	if err != nil || id == 0 { t.Fatal(err) }
	var n int
	s.db.QueryRow(`SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH '"mounts"'`).Scan(&n)
	if n != 1 { t.Fatalf("fts rows %d", n) }
	s.UpsertDocument(ctx, Document{Remote: "demo", RemoteID: "r1", Version: "v2", Path: "/work/a.md", Kind: "markdown"}, "replaced body", chunksOf("replaced body"))
	s.db.QueryRow(`SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH '"mounts"'`).Scan(&n)
	if n != 0 { t.Fatal("old chunks survived an upsert") }
}

func TestRenamePrefixKeepsIndexedAt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "1", Version: "v", Path: "/old/x/a.md"}, "t", chunksOf("t"))
	s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "2", Version: "v", Path: "/oldish/b.md"}, "t", chunksOf("t"))
	before, _, _ := s.DocumentByRemote(ctx, "d", "1")
	time.Sleep(2 * time.Millisecond)
	if n, _ := s.RenamePrefix(ctx, "/old", "/new"); n != 1 { t.Fatalf("renamed %d", n) }
	after, _, _ := s.DocumentByRemote(ctx, "d", "1")
	other, _, _ := s.DocumentByRemote(ctx, "d", "2")
	if after.Path != "/new/x/a.md" || !after.IndexedAt.Equal(before.IndexedAt) || other.Path != "/oldish/b.md" { t.Fatalf("%+v %+v", after, other) }
}

func TestIdentityChangeResetsTheIndex(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	s.EnsureIdentity(ctx, "meta-A")
	s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "1", Version: "v", Path: "/a"}, "t", chunksOf("t"))
	if reset, _ := s.EnsureIdentity(ctx, "meta-A"); reset { t.Fatal("same identity reset") }
	if reset, _ := s.EnsureIdentity(ctx, "meta-B"); !reset { t.Fatal("new identity kept stale inodes") }
	if st, _ := s.Stats(ctx); st.DocsOK != 0 { t.Fatalf("%+v", st) }
}

func TestConfigRulesCannotBeRemoved(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	s.SyncConfigRules(ctx, []Rule{{Path: "/work", Source: "config"}})
	s.AddRule(ctx, Rule{Path: "/notes", Source: "ui"})
	if err := s.RemoveRule(ctx, "/work"); !errors.Is(err, ErrConfigRule) { t.Fatal(err) }
	if err := s.RemoveRule(ctx, "/notes"); err != nil { t.Fatal(err) }
}

func TestTextPagesThroughTheExtractedText(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	body := strings.Repeat("0123456789", 100)
	s.UpsertDocument(ctx, Document{Remote: "d", RemoteID: "1", Version: "v", Path: "/a.txt", Kind: "text"}, body, chunksOf(body))
	p1, _ := s.Text(ctx, "/a.txt", 0, 600)
	p2, _ := s.Text(ctx, "/a.txt", p1.NextOffset, 600)
	if p1.Text+p2.Text != body || !p2.EOF || p1.EOF { t.Fatalf("%d %d %v", len(p1.Text), len(p2.Text), p2.EOF) }
}

func TestFailedDocumentsPageAndRetry(t *testing.T) { /* MarkFailed 3 docs; Failed(limit 2) + cursor → 3 total; RetryFailed("") returns 3 and Stats.DocsDirty == 3 */ }
func TestNewerIndexSchemaIsRefused(t *testing.T) { /* PRAGMA user_version = 99 then reopen → error mentions "newer" */ }
```

- [ ] **Step 2: 运行确认失败**

Run: `./gow test ./internal/index/ -v`
Expected: FAIL（包不存在）

- [ ] **Step 3: 实现**

schema v1（`schema.go`）：
```sql
CREATE TABLE IF NOT EXISTS index_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE rules(path TEXT PRIMARY KEY, include TEXT NOT NULL, exclude TEXT NOT NULL,
                   max_file_size INTEGER NOT NULL, source TEXT NOT NULL);
CREATE TABLE documents(
  id INTEGER PRIMARY KEY, remote TEXT NOT NULL, remote_id TEXT NOT NULL,
  version TEXT NOT NULL, path TEXT NOT NULL, ino INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL DEFAULT '', size INTEGER NOT NULL DEFAULT 0, mtime_ns INTEGER NOT NULL DEFAULT 0,
  text TEXT NOT NULL DEFAULT '', text_hash BLOB NOT NULL DEFAULT x'', truncated INTEGER NOT NULL DEFAULT 0,
  extractor_ver INTEGER NOT NULL DEFAULT 0, chunker_ver INTEGER NOT NULL DEFAULT 0,
  state INTEGER NOT NULL, error TEXT NOT NULL DEFAULT '', indexed_at INTEGER NOT NULL,
  UNIQUE(remote, remote_id));
CREATE INDEX documents_path ON documents(path);
CREATE INDEX documents_state ON documents(state);
CREATE TABLE chunks(
  id INTEGER PRIMARY KEY, doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL, start_off INTEGER NOT NULL, end_off INTEGER NOT NULL,
  heading TEXT NOT NULL DEFAULT '', text TEXT NOT NULL, UNIQUE(doc_id, seq));
CREATE VIRTUAL TABLE chunks_fts USING fts5(text, heading, content='chunks', content_rowid='id', tokenize='trigram');
CREATE TRIGGER chunks_ai AFTER INSERT ON chunks BEGIN
  INSERT INTO chunks_fts(rowid, text, heading) VALUES (new.id, new.text, new.heading);
END;
CREATE TRIGGER chunks_ad AFTER DELETE ON chunks BEGIN
  INSERT INTO chunks_fts(chunks_fts, rowid, text, heading) VALUES ('delete', old.id, old.text, old.heading);
END;
CREATE TRIGGER chunks_au AFTER UPDATE ON chunks BEGIN
  INSERT INTO chunks_fts(chunks_fts, rowid, text, heading) VALUES ('delete', old.id, old.text, old.heading);
  INSERT INTO chunks_fts(rowid, text, heading) VALUES (new.id, new.text, new.heading);
END;
CREATE TABLE index_pending(ino INTEGER PRIMARY KEY, path TEXT NOT NULL, reason INTEGER NOT NULL, queued_at INTEGER NOT NULL);
```
- DSN 在 Global Constraints 的基础上追加 `&_pragma=foreign_keys(1)`（级联删除依赖它）。
- `UpsertDocument`：一个事务：`INSERT ... ON CONFLICT(remote, remote_id) DO UPDATE SET ...`（`indexed_at = now`、`state = 0`、`error = ''`、`extractor_ver = textract.ExtractorVer`、`chunker_ver = textract.ChunkerVer`），`DELETE FROM chunks WHERE doc_id=?`，逐条插入 chunk；`text_hash = sha256(text)`。
- `RenamePrefix`：`UPDATE documents SET path = ? || substr(path, length(?) + 1) WHERE path = ? OR substr(path, 1, length(?) + 1) = ? || '/'`（不用 `GLOB`，避免路径中的 `*?[` 被当成通配符），**不**改 `indexed_at`。
- `EnsureIdentity`：`index_meta.meta_store_identity` 为空 → 写入返回 false；不同 → `Reset`（删 documents/chunks/pending，保留 rules）后写入返回 true。
- `Text`：`documents.text` 按字节切 `[offset, offset+max)`，右边界回退到 rune 起点；`NextOffset = offset + len(page)`，`EOF = NextOffset >= len(text)`；未找到文档 `ErrNotIndexed`。
- `Failed` 游标：`base64(indexed_at_ns:id)`，按 `(indexed_at DESC, id DESC)`。

- [ ] **Step 4: 运行确认通过**

Run: `./gow test -race ./internal/index/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/index
git commit -m "feat(index): index.db with external-content trigram FTS"
```

---

## Task B6：规则匹配、glob、抓取预算（纯逻辑）

**Files:**
- Create: `internal/index/glob.go`、`internal/index/rules.go`、`internal/index/budget.go`、`internal/index/glob_test.go`、`internal/index/rules_test.go`、`internal/index/budget_test.go`

**Interfaces:**
- Consumes: B0 `config.IndexRule`、`config.DefaultIndexExclude`；B5 `Rule`。
- Produces:
```go
// glob.go — "**" matches zero or more path segments; other segments use path.Match
func MatchGlob(pattern, p string) bool

// rules.go
type Matcher struct{ /* rules []Rule; global []string */ }
func NewMatcher(rules []Rule, globalExclude []string) *Matcher
// Match reports the covering rule for a file path and size. A rule whose Path
// equals p covers exactly that file regardless of Include.
func (m *Matcher) Match(p string, size int64) (Rule, bool)
func (m *Matcher) Excluded(p string) bool
func (m *Matcher) Roots() []string // rule paths, for walking
func RulesFromConfig(c []config.IndexRule) []Rule

// budget.go
type Budget struct{ /* perHour int64; now func() time.Time; windowStart time.Time; used int64 */ }
func NewBudget(perHour int64, now func() time.Time) *Budget
func (b *Budget) Take(n int64, unofficial bool) (ok bool, resumeAt time.Time) // unofficial remotes count double
func (b *Budget) Used() (used, limit int64)
```

- [ ] **Step 1: 写失败测试**

```go
// internal/index/glob_test.go
func TestMatchGlob(t *testing.T) {
	for _, tc := range []struct{ pat, p string; want bool }{
		{"**/*.md", "/work/a.md", true},
		{"**/*.md", "/a.md", true},
		{"**/*.md", "/work/a.mdx", false},
		{"**/.git/**", "/work/.git/config", true},
		{"**/.git/**", "/work/.github/x", false},
		{"**/id_rsa*", "/home/.ssh/id_rsa.pub", true},
		{"**/.env", "/app/.env", true},
		{"**/.env", "/app/.envrc", false},
		{"**/node_modules/**", "/w/node_modules/a/b.js", true},
		{"docs/*.txt", "/docs/a.txt", true},
		{"docs/*.txt", "/x/docs/a.txt", false},
	} {
		if got := MatchGlob(tc.pat, tc.p); got != tc.want { t.Errorf("MatchGlob(%q, %q) = %v", tc.pat, tc.p, got) }
	}
}
// internal/index/rules_test.go
func TestSecretsAreExcludedEvenInsideARule(t *testing.T) {
	m := NewMatcher([]Rule{{Path: "/work", Include: []string{"**/*"}, MaxFileSize: 1 << 20}}, config.DefaultIndexExclude)
	for _, p := range []string{"/work/.env", "/work/keys/server.pem", "/work/.ssh/id_rsa", "/work/.git/HEAD", "/work/node_modules/x/index.js"} {
		if _, ok := m.Match(p, 10); ok { t.Errorf("%s matched", p) }
	}
	if _, ok := m.Match("/work/notes/a.md", 10); !ok { t.Error("ordinary file not matched") }
}
func TestRuleSizeLimitAndExactFileRule(t *testing.T) {
	m := NewMatcher([]Rule{{Path: "/work", Include: []string{"**/*.md"}, MaxFileSize: 100}, {Path: "/big/report.pdf", MaxFileSize: 1 << 30}}, nil)
	if _, ok := m.Match("/work/a.md", 101); ok { t.Error("oversized file matched") }
	if _, ok := m.Match("/big/report.pdf", 5<<20); !ok { t.Error("an exact file rule must cover that file") }
	if _, ok := m.Match("/big/other.pdf", 10); ok { t.Error("an exact file rule covered a sibling") }
}
func TestDeepestRuleWins(t *testing.T) { /* rules /work (include *.md) and /work/code (include *.go): /work/code/a.go matched by /work/code; /work/code/a.md not matched */ }
// internal/index/budget_test.go
func TestBudgetStopsAtTheLimitAndResetsHourly(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := NewBudget(100, func() time.Time { return now })
	if ok, _ := b.Take(60, false); !ok { t.Fatal() }
	if ok, resume := b.Take(50, false); ok || !resume.Equal(now.Add(time.Hour)) { t.Fatalf("%v %v", ok, resume) }
	now = now.Add(time.Hour)
	if ok, _ := b.Take(50, false); !ok { t.Fatal("window did not reset") }
}
func TestUnofficialRemotesSpendDouble(t *testing.T) {
	b := NewBudget(100, time.Now)
	if ok, _ := b.Take(40, true); !ok { t.Fatal() }
	if ok, _ := b.Take(15, true); ok { t.Fatal("unofficial remote got the full budget") }
}
```

- [ ] **Step 2: 运行确认失败**

Run: `./gow test ./internal/index/ -run 'TestMatchGlob|TestSecrets|TestRuleSize|TestDeepestRule|TestBudget|TestUnofficial' -v`
Expected: FAIL

- [ ] **Step 3: 实现**

- `MatchGlob`：`pattern` 与 `p` 都按 `/` 分段（去掉前导 `/`）；递归：`**` 段可吞 0..n 段；其余 `path.Match(seg, part)`；模式不以 `**` 开头时从路径首段对齐（`docs/*.txt` 只匹配 `/docs/a.txt`）。规则的 include/exclude 相对**规则路径**匹配（`p` 取 `strings.TrimPrefix(p, rule.Path)`），全局 exclude 相对挂载根匹配。
- `Matcher.Match`：`Excluded(p)` 为真直接 false；候选规则 = `Path == p`（精确文件规则）或 目录前缀覆盖（`p == rule.Path || strings.HasPrefix(p, rule.Path+"/")`）；取最深的一条；精确文件规则只查 `MaxFileSize`；目录规则要求命中任一 include、不命中该规则 exclude、`size <= MaxFileSize`。
- `Budget.Take`：`now - windowStart >= 1h` 重置；`cost = n`（`unofficial` 时 `2n`）；`used + cost > perHour` → `false, windowStart + 1h`；`perHour <= 0` 视为无限。

- [ ] **Step 4: 运行确认通过**

Run: `./gow test -race ./internal/index/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/index/glob.go internal/index/rules.go internal/index/budget.go internal/index/*_test.go
git commit -m "feat(index): rule matching with ** globs and an hourly fetch budget"
```

---

## Task B7：Indexer — pinned/rules/WatchChanges/对账/让路/风控休眠 + `test/perf` 调用次数

**Files:**
- Create: `internal/index/indexer.go`、`internal/index/indexer_test.go`、`internal/index/harness_test.go`
- Create: `test/perf/index_test.go`

**Interfaces:**
- Consumes: B0 `meta.WalkSubtree`、`config.Index`；B1–B4 `textract`；B5 `Store`；B6 `Matcher`、`Budget`；现有 `vfs.FS.WatchChanges/Busy/StatPath/ReadFileRange/Meta/Cache/Mounts`、`cache.Complete`、`cache.HydratedPath`、`meta.Store.Identity`、`provider.ErrRiskControl`。
- Produces:
```go
type Options struct {
	FS             *vfs.FS
	Store          *Store
	Config         config.Index
	Unofficial     func(remote string) bool // Caps.Tier == "unofficial"
	Now            func() time.Time
	StartDelay     time.Duration // default 30s
	ReconcileEvery time.Duration // default 10m
	YieldMax       time.Duration // default 5s
	RiskSleep      time.Duration // default 15m
}
type Progress struct {
	Extracting int       `json:"extracting"`
	Pending    int       `json:"pending"`
	Paused     string    `json:"paused,omitempty"` // "" | busy | risk_control | budget | text_budget
	ResumeAt   time.Time `json:"resume_at,omitempty"`
}
type ReconcileReport struct {
	Walked, Queued, Extracted, Skipped, Failed, Deleted, Renamed int
}
type Indexer struct{ /* ... yields atomic.Int64; progress atomic.Pointer[Progress]; watchers ... */ }

func New(opt Options) (*Indexer, error) // SyncConfigRules + EnsureIdentity(meta identity)
func (x *Indexer) Start(ctx context.Context)  // change consumer + worker + periodic reconcile
func (x *Indexer) Close() error
func (x *Indexer) ReconcileNow(ctx context.Context) (ReconcileReport, error) // synchronous: walk, queue, drain the queue
func (x *Indexer) Progress() Progress
func (x *Indexer) Watch() (<-chan Progress, func())
func (x *Indexer) Yields() int64
func (x *Indexer) Store() *Store
```

- [ ] **Step 1: 写测试 harness**

```go
// internal/index/harness_test.go
// newHarness builds a real vfs.FS over a fake provider (the same wiring as
// mcpsrv's newEnv), an index store, and an Indexer with tiny delays.
type harness struct {
	fs    *vfs.FS
	fake  *fakeprovider.Fake
	store *Store
	x     *Indexer
}
func newHarness(t *testing.T, cfg config.Index) *harness
```
实现逐行照 `internal/mcpsrv/server_test.go:34-100` 的 `newEnv`（meta/cache/journal/fake/vfs/uploader），`fakeprovider.New("ali")`（`Caps.PathIDs` 为 false，保证改名不改 remote_id），再 `OpenStore(t.TempDir())`、`New(Options{FS: fsys, Store: st, Config: cfg, StartDelay: time.Millisecond, ReconcileEvery: time.Hour, YieldMax: 200 * time.Millisecond, RiskSleep: time.Second})`；`cfg` 先调 `cfg.Validate()` 填默认值。

- [ ] **Step 2: 写失败测试（index）**

```go
// internal/index/indexer_test.go
func rulesAll(paths ...string) config.Index {
	x := config.Index{Enabled: true}
	for _, p := range paths { x.Rules = append(x.Rules, config.IndexRule{Path: p}) }
	return x
}

func TestReconcileIndexesMatchingFilesOnly(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	h.fake.Seed("work/a.md", []byte("# 标题\n正文"))
	h.fake.Seed("work/.env", []byte("SECRET=1"))
	h.fake.Seed("other/b.md", []byte("outside"))
	h.fs.ReadDirPath(context.Background(), "/work") // warm listing so meta knows the tree
	rep, err := h.x.ReconcileNow(context.Background())
	if err != nil { t.Fatal(err) }
	if rep.Extracted != 1 { t.Fatalf("%+v", rep) }
	if _, ok, _ := h.store.DocumentByPath(context.Background(), "/work/.env"); ok { t.Fatal(".env was indexed") }
}

func TestChangeEventIndexesAFreshFile(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx, cancel := context.WithCancel(context.Background()); defer cancel()
	h.x.Start(ctx)
	if _, err := h.fs.WriteFile(ctx, "/work/new.md", []byte("fresh marker"), false); err != nil { t.Fatal(err) }
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if d, ok, _ := h.store.DocumentByPath(ctx, "/work/new.md"); ok && d.State == DocOK { return }
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a written file was not indexed within 3s")
}

func TestRenameKeepsIndexedAtAndUpdatesThePath(t *testing.T) {
	h := newHarness(t, rulesAll("/"))
	ctx := context.Background()
	h.fake.Seed("dir/a.md", []byte("body"))
	h.fs.ReadDirPath(ctx, "/dir")
	h.x.ReconcileNow(ctx)
	before, _, _ := h.store.DocumentByPath(ctx, "/dir/a.md")
	if err := h.fs.RenamePath(ctx, "/dir", "/moved"); err != nil { t.Fatal(err) }
	h.x.ReconcileNow(ctx)
	after, ok, _ := h.store.DocumentByPath(ctx, "/moved/a.md")
	if !ok || !after.IndexedAt.Equal(before.IndexedAt) { t.Fatalf("%+v %+v", before, after) }
}

func TestSameTextNewVersionDoesNotRechunk(t *testing.T) { /* re-seed identical bytes with a new version; ReconcileNow → Extracted 0, TouchVersion path taken (Skipped 1), chunks row ids unchanged */ }

func TestBusyForegroundMakesTheWorkerYield(t *testing.T) {
	h := newHarness(t, rulesAll("/work"))
	ctx, cancel := context.WithCancel(context.Background()); defer cancel()
	for i := 0; i < 20; i++ { h.fake.Seed(fmt.Sprintf("work/f%02d.md", i), []byte("x")) }
	h.fs.ReadDirPath(ctx, "/work")
	stop := holdForeground(h.fs) // opens a read handle and keeps a Read blocked on a slow fake range, so fs.Busy() is true
	defer stop()
	h.x.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.x.Yields() > 0 { return }
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the extraction worker never yielded to foreground IO within 5s")
}

func TestRiskControlPausesTheWorker(t *testing.T) { /* fake Faults risk-control on ReadRange → after ReconcileNow, Progress().Paused == "risk_control" and ResumeAt ≈ now + RiskSleep; no further ReadRange calls during the pause */ }
func TestFetchBudgetPausesRules(t *testing.T) { /* Config.FetchBudgetPerHour = 10 bytes, two 8-byte files → one extracted, Paused == "budget" */ }
func TestPinnedModeReadsOnlyCompleteCachedFiles(t *testing.T) { /* Pinned: true, one pinned+hydrated file, one pinned-but-cold file → only the hydrated one indexed; Calls("ReadRange") delta 0 */ }
```
`holdForeground` 的写法：用 `fakeprovider` 的 `Faults` 给 `ReadRange` 加 `Latency`，在 goroutine 里对一个未缓存文件调用 `h.fs.ReadFileRange`（前台读计入 `fgIO`；若 `ReadFileRange` 不计入 `fgIO`，改为 `h.fs.Open` + `h.fs.Read`，以 `vfs.go` 中 `fgIO.Add` 的实际位置为准）。`RenamePath` 以 `vfs` 实际导出的改名方法名为准（`grep -n "func (f \*FS) Rename" internal/vfs/*.go`）。4 个注释测试按注释写全。

- [ ] **Step 3: 写失败测试（perf）**

```go
// test/perf/index_test.go
// Content indexing must not become a second downloader. Pinned mode reads what
// the cache already holds; rules mode downloads each file once per version.
func TestIndexPinnedFilesCostNoReads(t *testing.T) {
	// Use this package's existing harness constructor (see perf_test.go).
	// Seed 500 small files under /pinned, pin the directory and wait for the
	// pin fill to complete (cache.Complete for every key). Reset the fake's
	// counters. Build an index.Indexer with Config{Enabled: true, Pinned: true}
	// and ReconcileNow. Assert Calls("ReadRange") == 0 and 500 documents ok.
}
func TestIndexRulesFetchOncePerVersion(t *testing.T) {
	// Seed 100 files under /work and list /work once. Reset counters.
	// Rules [/work]: ReconcileNow → Calls("ReadRange") == 100 (files ≤ one block).
	// Reset; ReconcileNow → 0. Re-seed one file with new content (new version),
	// refresh the listing, reset; ReconcileNow → 1.
}
```
（两个 perf 用例按注释写全，构造器以 `test/perf/perf_test.go` 现有 harness 为准；小文件 ≤ 块大小，所以"每文件 1 次 ReadRange"成立，若 harness 块大小更小则用 `ceil(size/block)` 求和作为期望值。）

- [ ] **Step 4: 运行确认失败**

Run: `./gow test ./internal/index/ -run 'TestReconcile|TestChangeEvent|TestRename|TestSameText|TestBusy|TestRiskControl|TestFetchBudget|TestPinnedMode' -v; ./gow test ./test/perf/ -run 'TestIndex' -v`
Expected: FAIL

- [ ] **Step 5: 实现**

- **New**：`id, _ := opt.FS.Meta().Identity(ctx)`；`Store.EnsureIdentity(id)`；`Store.SyncConfigRules(RulesFromConfig(cfg.Rules))`（`source = "config"`）；`matcher = NewMatcher(store.Rules(), cfg.Exclude)`（规则增删后重建）。
- **变更消费者**：`ch, stop := FS.WatchChanges()`；`Rescan` → 标记需要全量对账；每个 `Paths` 元素：`Subtree` 为真 → 对该路径子树做"局部对账"（下一条）；否则 `StatPath(p)`：文件且 `matcher.Match` → `Store.Enqueue(ino, p, PendingChange)`，唤醒 worker；不存在 → 若 `DocumentByPath` 命中则删除。
- **对账**（全量与局部同一函数 `reconcile(ctx, roots)`）：根集合 = rules 模式用 `matcher.Roots()`，pinned 模式用挂载前缀（`FS.Mounts()` 的 `Prefix`）；对每个根 `StatPath` 取 `Ino` → `meta.WalkSubtree(ctx, ino, root, visit)`：
  - 目录：被全局 exclude 命中则 `SkipDir`。
  - 文件：pinned 模式要求 `attr.Pinned`（`StatPath` 只读 meta）且 `cache.Complete(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version})`；rules 模式要求 `matcher.Match(p, n.Size)`。
  - `DocumentByRemote(n.Remote, n.RemoteID)`：命中且 `Version` 相同 → 若 `Path != p` 则 `SetPath`（`Renamed++`，**不**改 `indexed_at`），`seen[id] = true`，跳过；命中但版本不同或未命中 → `Enqueue(n.Ino, p, PendingReconcile)`。
  - 走完全量对账后 `DeleteMissing(seen)`（只在全量时执行）。
- **worker**（单 goroutine）：循环取 `Pending(8)`；每项：
  1. `waitIdle()`：`FS.Busy()` 为真时每 50 ms 轮询，最长 `YieldMax`，只要进入过等待就 `yields.Add(1)` 并置 `Paused = "busy"`；
  2. 若处于风控/预算暂停且 `now < ResumeAt` → 睡到 `ResumeAt` 或 ctx 结束；
  3. `Stats().TextBytes >= MaxTotalText` → `Paused = "text_budget"`，保留 pending，不再抽取（doctor 在 B10 告警）；
  4. 取字节：pinned 模式 `cache.HydratedPath(key)` → `os.Open` → `io.ReaderAt`（零 provider 调用）；rules 模式先 `Budget.Take(size, Unofficial(remote))`，不足 → `Paused = "budget"`、`ResumeAt`；再用 `FS.ReadFileRange(ctx, p, off, 4<<20)` 按 4 MiB 分段读满到内存（`size <= MaxFileSize` 已由规则保证）；`errors.Is(err, provider.ErrRiskControl)` → `Paused = "risk_control"`、`ResumeAt = now + RiskSleep`，项留在 pending；
  5. `kind := textract.KindOf(p, head)`；unsupported → `MarkFailed(ErrUnsupported)`；`doc, err := textract.Extract(...)`；失败 → `MarkFailed`；
  6. 已有文档且 `sha256(doc.Text) == TextHash` → `TouchVersion`（`Skipped++`）；否则 `UpsertDocument(d, doc.Text, textract.ChunkDoc(doc, kind, DefaultChunkOptions()))`；
  7. `Dequeue(ino)`；更新 `Progress` 并广播（`Watch` 通道满则丢，不阻塞）。
- **Start**：`time.AfterFunc(StartDelay, 全量对账)`；`time.NewTicker(ReconcileEvery)`；`Close` 取消 ctx、等 goroutine 退出、`stop()` 变更订阅。
- `ReconcileNow`：同步执行全量对账，然后在当前 goroutine 里把 pending 排空（跳过 `waitIdle` 的等待上限不变），返回报告。

- [ ] **Step 6: 运行确认通过**

Run: `./gow test -race ./internal/index/ -count=1 -v && ./gow test ./test/perf/ -run TestIndex -v -count=1`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add internal/index test/perf/index_test.go
git commit -m "feat(index): incremental indexer that yields to foreground IO and respects risk control"
```

---

## Task B8：检索 `search.go` — bm25、范围、stale、字节预算、短查询

**Files:**
- Create: `internal/index/search.go`、`internal/index/search_test.go`

**Interfaces:**
- Consumes: B5 `Store`。
- Produces:
```go
type SearchQuery struct {
	Query           string
	Roots           []string // already intersected with the caller's read scope; empty = nothing
	TopK            int      // default 10
	Mode            string   // hybrid | keyword | vector; phase 1 always runs keyword
	MaxSnippetBytes int      // default 1024
	MaxBytes        int64    // response budget; 0 = unlimited
}
type Hit struct {
	Path       string  `json:"path"`
	Seq        int     `json:"seq"`
	StartOff   int64   `json:"start_off"`
	EndOff     int64   `json:"end_off"`
	Heading    string  `json:"heading,omitempty"`
	Score      float64 `json:"score"`
	Snippet    string  `json:"snippet"`
	OffsetKind string  `json:"offset_kind"` // file | text
	Stale      bool    `json:"stale"`
}
type SearchResult struct {
	Hits      []Hit  `json:"hits"`
	ModeUsed  string `json:"mode_used"`
	Degraded  string `json:"degraded,omitempty"`
	Truncated bool   `json:"truncated"`
	Docs      int    `json:"docs"`
	Pending   int    `json:"pending"`
}
// Current reports the version meta holds now for (remote, remoteID).
type Current func(remote, remoteID string) (version string, ok bool)
func (s *Store) Search(ctx context.Context, q SearchQuery, current Current) (SearchResult, error)
const shortQueryRowBudget = 20000
const DegradedNoEmbedding = "semantic search is not configured; results use keyword matching"
```

- [ ] **Step 1: 写失败测试**

```go
// internal/index/search_test.go
func seedDoc(t *testing.T, s *Store, remoteID, p, text string) {
	t.Helper()
	d := textract.Doc{Text: text, OffsetKind: "file"}
	if _, err := s.UpsertDocument(context.Background(), Document{Remote: "d", RemoteID: remoteID, Version: "v1", Path: p, Kind: "markdown"}, text, textract.ChunkDoc(d, textract.KindMarkdown, textract.DefaultChunkOptions())); err != nil { t.Fatal(err) }
}
func same(string, string) (string, bool) { return "v1", true }

func TestSearchRanksByBM25(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "1", "/w/once.md", "cloudfs appears once among many other words here")
	seedDoc(t, s, "2", "/w/many.md", "cloudfs cloudfs cloudfs cloudfs")
	r, err := s.Search(context.Background(), SearchQuery{Query: "cloudfs", Roots: []string{"/"}}, same)
	if err != nil { t.Fatal(err) }
	if len(r.Hits) != 2 || r.Hits[0].Path != "/w/many.md" || r.ModeUsed != "keyword" { t.Fatalf("%+v", r) }
}

func TestSearchNeverLeaksOutsideRoots(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "1", "/work/a.md", "shared secret phrase")
	seedDoc(t, s, "2", "/private/b.md", "shared secret phrase")
	seedDoc(t, s, "3", "/workshop/c.md", "shared secret phrase")
	r, _ := s.Search(context.Background(), SearchQuery{Query: "secret phrase", Roots: []string{"/work"}, TopK: 50}, same)
	if len(r.Hits) != 1 || r.Hits[0].Path != "/work/a.md" { t.Fatalf("%+v", r.Hits) }
	if r, _ := s.Search(context.Background(), SearchQuery{Query: "secret", Roots: nil}, same); len(r.Hits) != 0 { t.Fatal("empty roots returned hits") }
}

func TestTwoRuneChineseQueryReturnsHitsOrTruncated(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "1", "/w/a.md", "这是关于网盘挂载的说明")
	r, err := s.Search(context.Background(), SearchQuery{Query: "网盘", Roots: []string{"/"}}, same)
	if err != nil { t.Fatal(err) }
	if len(r.Hits) == 0 && !r.Truncated { t.Fatal("a two-rune query answered 'no match' without scanning") }
	if len(r.Hits) == 1 && !strings.Contains(r.Hits[0].Snippet, "网盘") { t.Fatalf("%+v", r.Hits[0]) }
}

func TestShortQueryScanStopsAtItsBudget(t *testing.T) {
	s := openTestStore(t)
	for i := 0; i < 30; i++ { seedDoc(t, s, fmt.Sprint(i), fmt.Sprintf("/w/%02d.md", i), "无关内容") }
	r, _ := searchWithRowBudget(s, SearchQuery{Query: "网盘", Roots: []string{"/"}}, same, 10)
	if !r.Truncated || len(r.Hits) != 0 { t.Fatalf("%+v", r) }
}

func TestHybridDegradesToKeyword(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "1", "/w/a.md", "cloudfs index")
	r, _ := s.Search(context.Background(), SearchQuery{Query: "index", Roots: []string{"/"}, Mode: "hybrid"}, same)
	if r.ModeUsed != "keyword" || r.Degraded != DegradedNoEmbedding || len(r.Hits) != 1 { t.Fatalf("%+v", r) }
}

func TestStaleWhenMetaHasMovedOn(t *testing.T) {
	s := openTestStore(t)
	seedDoc(t, s, "1", "/w/a.md", "cloudfs")
	r, _ := s.Search(context.Background(), SearchQuery{Query: "cloudfs", Roots: []string{"/"}}, func(string, string) (string, bool) { return "v2", true })
	if !r.Hits[0].Stale { t.Fatal("hit not marked stale") }
}

func TestSnippetBudgetIsRuneSafeAndResponseBudgetTruncates(t *testing.T) {
	s := openTestStore(t)
	for i := 0; i < 20; i++ { seedDoc(t, s, fmt.Sprint(i), fmt.Sprintf("/w/%02d.md", i), strings.Repeat("云端", 300)+" 关键词 "+strings.Repeat("文件", 300)) }
	r, _ := s.Search(context.Background(), SearchQuery{Query: "关键词", Roots: []string{"/"}, TopK: 20, MaxSnippetBytes: 100, MaxBytes: 800}, same)
	for _, h := range r.Hits { if len(h.Snippet) > 100 || !utf8.ValidString(h.Snippet) || !strings.Contains(h.Snippet, "关键词") { t.Fatalf("%q", h.Snippet) } }
	if !r.Truncated || len(r.Hits) >= 20 { t.Fatalf("budget ignored: %d hits truncated=%v", len(r.Hits), r.Truncated) }
}
```
`searchWithRowBudget` 是 `search.go` 中未导出的 `func searchWithRowBudget(s *Store, q SearchQuery, cur Current, budget int) (SearchResult, error)`，`Search` 以 `shortQueryRowBudget` 调它。

- [ ] **Step 2: 运行确认失败**

Run: `./gow test ./internal/index/ -run 'TestSearch|TestTwoRune|TestShortQuery|TestHybrid|TestStale|TestSnippetBudget' -v`
Expected: FAIL

- [ ] **Step 3: 实现**

- 范围 SQL 片段（每个 root 一组，OR 连接）：`(d.path = ? OR substr(d.path, 1, length(?) + 1) = ? || '/')`；root `"/"` 直接不加条件；`Roots` 为空直接返回空结果。
- `utf8.RuneCountInString(query) >= 3`：FTS 查询字符串 = 以空白切词、每词 `"` 转义为 `""` 后包双引号、用空格连接（隐式 AND）；
```sql
SELECT c.id, c.seq, c.start_off, c.end_off, c.heading, c.text, d.path, d.remote, d.remote_id, d.version, d.kind, bm25(chunks_fts) AS score
FROM chunks_fts JOIN chunks c ON c.id = chunks_fts.rowid JOIN documents d ON d.id = c.doc_id
WHERE chunks_fts MATCH ? AND d.state = 0 AND (<roots>)
ORDER BY score LIMIT ?
```
  `Score = -bm25`（越大越相关）。
- `< 3` rune：`SELECT ... FROM chunks c JOIN documents d ... WHERE d.state = 0 AND (<roots>) ORDER BY d.path, c.seq LIMIT ?`（`LIMIT budget`），在 Go 里对每行 `strings.Contains(strings.ToLower(text), strings.ToLower(query))`；扫满预算仍未凑够 `TopK` 且还有更多行 → `Truncated = true`；`Score = 1`。
- 片段：在 `text` 中找首个命中（大小写不敏感）的字节位置，窗口居中取 `MaxSnippetBytes`，左右边界向内对齐到 rune 起点。
- `OffsetKind`：`kind` 为 docx/xlsx/pptx/pdf → `"text"`，否则 `"file"`。
- `Stale`：`current(remote, remoteID)` 返回的版本与 `d.version` 不同，或 `ok == false`。
- `MaxBytes`：逐 hit 累加 `len(path)+len(heading)+len(snippet)+64`，超过即停止追加并置 `Truncated`。
- `Mode` 为 `hybrid`/`vector` → `Degraded = DegradedNoEmbedding`；`ModeUsed` 恒为 `"keyword"`。
- `Docs`/`Pending` 取 `Stats`。

- [ ] **Step 4: 运行确认通过**

Run: `./gow test -race ./internal/index/ -count=1 -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/index/search.go internal/index/search_test.go
git commit -m "feat(index): scoped keyword search with stale marks and byte budgets"
```

---

## Task B9：MCP 5 个索引工具 + daemon 装配（T-37 做法：MCP 与装配）

**Files:**
- Create: `internal/index/service.go`、`internal/index/service_test.go`
- Create: `internal/mcpsrv/index_tools.go`、`internal/mcpsrv/index_tools_test.go`
- Modify: `internal/mcpsrv/server.go`（`Options.Index`；`New` 调 `s.registerIndexTools()`）
- Modify: `internal/daemon/daemon.go`（`Daemon.Index`；export store 块之后装配）、`internal/daemon/index_test.go`（新建）
- Modify: `cmd/cloudfs/main.go`（两处 `mcpsrv.Options` 加 `Index: indexOf(d)`）

**Interfaces:**
- Consumes: B5 `Store`、`Rule`、`TextPage`；B7 `Indexer`、`Progress`；B8 `SearchQuery`、`SearchResult`、`Current`。A2 若已合入则用 `s.checkPath(ctx, p, false)` 与 `s.readRoots(ctx, root)`；否则见"分支与合并卫生"协议。
- Produces:
```go
// internal/index/service.go
type Status struct {
	Enabled      bool        `json:"enabled"`
	Covered      string      `json:"covered,omitempty"`     // covering rule path for the queried path
	RuleSource   string      `json:"rule_source,omitempty"` // config | ui | tool
	State        string      `json:"state,omitempty"`       // ok | dirty | failed | pending | uncovered (per path)
	Chunks       int         `json:"chunks,omitempty"`      // per path
	Error        string      `json:"error,omitempty"`       // per path
	Docs         DocCounts   `json:"docs"`
	ChunksTotal  int         `json:"chunks_total"`
	Pending      int         `json:"pending"`
	Failed       []FailedDoc `json:"failed,omitempty"` // ≤ 20
	TextBytes    int64       `json:"text_bytes"`
	MaxTotalText int64       `json:"max_total_text"`
	FetchBudget  BudgetUse   `json:"fetch_budget"`
	FetchBytesTotal int64    `json:"fetch_bytes_total"` // since process start, for cloudfs_index_fetch_bytes_total
	Progress     Progress    `json:"progress"`
}
type DocCounts struct {
	OK     int `json:"ok"`
	Dirty  int `json:"dirty"`
	Failed int `json:"failed"`
}
type BudgetUse struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
}
func (x *Indexer) Search(ctx context.Context, q SearchQuery) (SearchResult, error) // Current = meta lookup by remote id
func (x *Indexer) Status(ctx context.Context, p string) (Status, error)
func (x *Indexer) AddRule(ctx context.Context, r Rule) error    // source defaults to "tool"; Enqueue a reconcile of r.Path
func (x *Indexer) RemoveRule(ctx context.Context, p string) error // ErrConfigRule for config rules; deletes docs no longer covered
func (x *Indexer) Text(ctx context.Context, p string, off int64, max int) (TextPage, error)
func (x *Indexer) Rules(ctx context.Context) ([]RuleView, error)
func (x *Indexer) Rebuild(ctx context.Context) error
func (x *Indexer) Retry(ctx context.Context, p string) (int64, error)
func (x *Indexer) Failed(ctx context.Context, cursor string, limit int) ([]FailedDoc, string, error)
type RuleView struct { Rule; Documents int `json:"documents"` }
```
```go
// internal/mcpsrv/server.go
type IndexService interface {
	Search(ctx context.Context, q index.SearchQuery) (index.SearchResult, error)
	Status(ctx context.Context, p string) (index.Status, error)
	AddRule(ctx context.Context, r index.Rule) error
	RemoveRule(ctx context.Context, p string) error
	Text(ctx context.Context, p string, off int64, max int) (index.TextPage, error)
}
// Options gains: Index IndexService // nil = the five index tools are not registered
```
```go
// internal/daemon
// Daemon gains: Index *index.Indexer (nil unless index.enabled)
```

- [ ] **Step 1: 写失败测试（mcpsrv）**

```go
// internal/mcpsrv/index_tools_test.go
var indexToolNames = []string{"semantic_search", "index_status", "index", "unindex", "read_extracted_text"}

func newIndexEnv(t *testing.T, opt Options, cfg config.Index) (*env, *index.Indexer) {
	t.Helper()
	e := newEnv(t, opt) // tools registered without Index first; rebuild the server with it below
	if err := cfg.Validate(); err != nil { t.Fatal(err) }
	st, err := index.OpenStore(t.TempDir())
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { st.Close() })
	x, err := index.New(index.Options{FS: e.fs, Store: st, Config: cfg, StartDelay: time.Hour, ReconcileEvery: time.Hour})
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { x.Close() })
	opt.Index = x
	return reconnect(t, e, opt), x // reconnect: New(opt with FS=e.fs) + fresh in-memory client, sharing fs/fake/up/j
}

func TestIndexToolsAbsentWhenDisabled(t *testing.T) {
	e := newEnv(t, Options{})
	tools, _ := e.session.ListTools(context.Background(), nil)
	for _, tool := range tools.Tools {
		for _, n := range indexToolNames { if tool.Name == n { t.Fatalf("%s registered without an index", n) } }
	}
}

func TestSemanticSearchRespectsAllow(t *testing.T) {
	e, x := newIndexEnv(t, Options{Allow: []string{"/work"}}, config.Index{Enabled: true, Rules: []config.IndexRule{{Path: "/"}}})
	e.fake.Seed("work/a.md", []byte("shared marker phrase"))
	e.fake.Seed("private/b.md", []byte("shared marker phrase"))
	e.fs.ReadDirPath(context.Background(), "/work"); e.fs.ReadDirPath(context.Background(), "/private")
	x.ReconcileNow(context.Background())
	var out struct{ Hits []index.Hit `json:"hits"` }
	if res := e.call(t, "semantic_search", map[string]any{"query": "marker phrase", "top_k": 10}, &out); res.IsError { t.Fatal(errText(res)) }
	for _, h := range out.Hits { if !strings.HasPrefix(h.Path, "/work/") { t.Fatalf("leaked %s", h.Path) } }
	if len(out.Hits) != 1 { t.Fatalf("%+v", out.Hits) }
}

func TestMarkdownHitOffsetFeedsReadText(t *testing.T) {
	e, x := newIndexEnv(t, Options{}, config.Index{Enabled: true, Rules: []config.IndexRule{{Path: "/notes"}}})
	body := "# Plan\r\n\r\nintro line\r\n## Scope\r\nthe needle sentence lives here\r\n"
	if res := e.call(t, "write_file", map[string]any{"path": "/notes/plan.md", "content": body}, nil); res.IsError { t.Fatal(errText(res)) }
	x.ReconcileNow(context.Background())
	var out struct{ Hits []index.Hit `json:"hits"` }
	e.call(t, "semantic_search", map[string]any{"query": "needle sentence"}, &out)
	if len(out.Hits) != 1 || out.Hits[0].OffsetKind != "file" { t.Fatalf("%+v", out.Hits) }
	var rt readTextOutput
	e.call(t, "read_text", map[string]any{"path": "/notes/plan.md", "offset": out.Hits[0].StartOff, "max_bytes": 16}, &rt)
	if !strings.HasPrefix(body[out.Hits[0].StartOff:], rt.Text) || rt.Text == "" { t.Fatalf("read_text at start_off gave %q", rt.Text) }
}

func TestDocxHeadingIsReturnedWithTheHit(t *testing.T) { /* seed a docx built like textract's office_test buildZip with Heading1 "第二章 设计" and body "needle"; ReconcileNow; hit.Heading == "第二章 设计" */ }

func TestReadExtractedTextPages(t *testing.T) { /* index a 3 KiB text file; read_extracted_text max_bytes 1024 → next_offset 1024; second call continues; eof true on the last page */ }

func TestUnindexRefusesConfigRules(t *testing.T) {
	e, _ := newIndexEnv(t, Options{}, config.Index{Enabled: true, Rules: []config.IndexRule{{Path: "/work"}}})
	res := e.call(t, "unindex", map[string]any{"path": "/work"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "configuration file") { t.Fatalf("%s", errText(res)) }
	if res := e.call(t, "index", map[string]any{"path": "/notes"}, nil); res.IsError { t.Fatal(errText(res)) }
	if res := e.call(t, "unindex", map[string]any{"path": "/notes"}, nil); res.IsError { t.Fatal(errText(res)) }
}

func TestIndexToolIsAllowedOnAReadOnlyServer(t *testing.T) {
	e, _ := newIndexEnv(t, Options{ReadOnly: true}, config.Index{Enabled: true})
	if res := e.call(t, "index", map[string]any{"path": "/notes"}, nil); res.IsError { t.Fatalf("index refused on read-only: %s", errText(res)) }
}

func TestHybridModeReportsDegraded(t *testing.T) { /* semantic_search mode "hybrid" → mode_used "keyword", degraded non-empty, not an error */ }
```
`reconnect(t, e, opt)` 加在 `agent_env_test.go` 之外的新 helper 文件 `index_env_test.go`（避免与线 A 同文件冲突），逻辑：`opt.FS = e.fs; srv, _ := New(opt)`，新建 in-memory 传输并 `Connect`，返回 `&env{server: srv, fs: e.fs, fake: e.fake, up: e.up, j: e.j, session: newSession}`。线 A 合入后，A2 的 `TestEveryToolChecksItsPaths` 在带 `Index` 的 env 下需把 5 个工具登记进 `outside` 表（`semantic_search{query, path}`、`index_status{path}`、`index{path}`、`unindex{path}`、`read_extracted_text{path}`）——该测试默认 env 不带 Index，所以 rebase 时另加一个带 Index 的子测试。

- [ ] **Step 2: 写失败测试（daemon）**

```go
// internal/daemon/index_test.go
func TestIndexDisabledCreatesNoIndexDB(t *testing.T) {
	// Open a daemon with the fake-remote config used by the other daemon tests
	// (index section absent). Assert d.Index == nil and
	// <cache.dir>/index.db does not exist after Close.
}
func TestIndexEnabledRunsTheIndexerOnlyInTheOwner(t *testing.T) {
	// Same config plus "index:\n  enabled: true\n". Open twice on one cache dir:
	// the first has d.Index != nil with Store().Owner() == true; the second has
	// d.Index != nil (search works) but its Store().Owner() == false and it
	// never started a worker (Progress().Pending == 0 and no writes).
}
```
（两个 daemon 测试按注释写全；配置构造照 `internal/daemon` 现有测试的 helper。）

- [ ] **Step 3: 运行确认失败**

Run: `./gow test ./internal/mcpsrv/ -run 'TestIndexTools|TestSemanticSearch|TestMarkdownHit|TestDocxHeading|TestReadExtracted|TestUnindex|TestIndexTool|TestHybridMode' -v; ./gow test ./internal/daemon/ -run TestIndex -v`
Expected: FAIL

- [ ] **Step 4: 实现**

- `index_tools.go`（仅 `s.opt.Index != nil` 注册）：
  - `semantic_search{query, path?, top_k?, mode?, max_snippet_bytes?}`：`root := "/"`；`in.Path != ""` → `root, err = s.checkPath(ctx, in.Path, false)`；`roots := s.readRoots(ctx, root)`（A2 抽出的 allow 交集函数；A2 未合入时直接复用 `search` 工具中 server.go:943-952 的交集逻辑，合入后替换）；`TopK` 上限 `s.opt.Limits.MaxResults`；`MaxBytes = s.opt.Limits.MaxBytes`；结果每个 hit 再过 `checkPath(ctx, hit.Path, false)`，被拒的丢弃；输出 `index.SearchResult`。
  - `index_status{path?}`：`path` 过 `checkPath`；`Status(ctx, p)`；`Failed` 列表逐项过 `checkPath`，被拒的丢弃。
  - `index{path, include?, max_file_size?}`：只做读检查 `checkPath(ctx, p, false)`（与 `pin` 同理，不改远端，只读服务也允许）；`AddRule(Rule{Path: p, Include: in.Include, MaxFileSize: in.MaxFileSize, Source: "tool"})`。
  - `unindex{path}`：读检查；`RemoveRule`；`ErrConfigRule` → `fail(err)`。
  - `read_extracted_text{path, offset?, max_bytes?}`：读检查；`max_bytes` 上限 `Limits.MaxBytes`；`ErrNotIndexed` → `fail`，文本 `this file is not indexed; call index_status to see coverage`。
  - 注解：`semantic_search`/`index_status`/`read_extracted_text` `ReadOnlyHint: true`；`index`/`unindex` `IdempotentHint: true`。
- `service.go`：`Current` 实现 = `meta` 中按 `(remote, remote_id)` 查节点版本（执行前 `grep -n "func (s \*Store) .*RemoteID\|nodes_remote_id" internal/meta/*.go` 找现有查找函数；没有就在 `internal/meta/walk_subtree.go` 同文件加 `func (s *Store) VersionByRemoteID(ctx, remote, remoteID string) (string, bool, error)`，用 `nodes_remote_id` 索引，并补一条 meta 测试）。`Status(p)`：`p == ""` 只填全局；否则 `StatPath` → `matcher.Match` 得 `Covered/RuleSource`，`DocumentByPath` 得 `State/Chunks/Error`，排队中返回 `pending`，不覆盖返回 `uncovered`。`RemoveRule` 后重建 matcher 并删除不再被任何规则覆盖的文档（pinned 模式下被 pin 覆盖的保留）。`Rebuild` = `Store.Reset` + 全量对账排队。
- `daemon.go`（export store 块之后）：
```go
	if cfg.Index.Enabled {
		st, err := index.OpenStore(cacheDir)
		if err != nil { d.Close(); return nil, fmt.Errorf("daemon: index store: %w", err) }
		x, err := index.New(index.Options{
			FS: fsys, Store: st, Config: cfg.Index,
			Unofficial: func(remote string) bool { p, ok := d.Providers[remote]; return ok && p.Capabilities().Tier == "unofficial" },
		})
		if err != nil { st.Close(); d.Close(); return nil, err }
		d.Index = x
		if st.Owner() { x.Start(ctx) }
		d.closers = append(d.closers, x.Close, st.Close)
	}
```
  `Caps.Tier` 的实际类型/常量以 `internal/provider/provider.go` 为准（字符串或具名类型常量）。关闭顺序：`closers` 逆序执行时 indexer 必须先于 vfs 关闭——确认 `fsys.Close` 在 `closers` 中位于更早位置（export 同样依赖此顺序）。
- `cmd/cloudfs/main.go`：
```go
// indexOf keeps a nil indexer a nil interface: mcpsrv registers the index tools on that alone.
func indexOf(d *daemon.Daemon) mcpsrv.IndexService {
	if d.Index == nil { return nil }
	return d.Index
}
```
  stdio `mcpsrv.New` 与 `serveMCPHTTPWith` 都加 `Index: indexOf(d)`。

- [ ] **Step 5: 运行确认通过**

Run: `./gow build ./... && ./gow test -race ./internal/index/ ./internal/mcpsrv/ ./internal/daemon/ ./cmd/cloudfs/ -count=1 && ./gow vet ./...`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/index internal/mcpsrv internal/daemon cmd/cloudfs internal/meta
git commit -m "feat(mcp): semantic_search and index tools backed by the daemon's indexer"
```

---

## Task B10：控制面 `/index/*`、SSE `index`、CLI `cloudfs index`、metrics、doctor（T-37 做法：控制面）

**Files:**
- Create: `internal/control/index.go`、`internal/control/index_test.go`、`internal/control/index_client.go`
- Create: `cmd/cloudfs/index.go`、`cmd/cloudfs/index_test.go`
- Modify: `internal/control/metrics.go`（9 条路由 + 指标）、`internal/control/status.go`（`Collector.Index`、`Status.Index`）、`internal/control/events.go`（`index` 事件，1 s 节流）、`internal/control/events_test.go`
- Modify: `internal/control/doctor.go`（`Doctor.Index`；`Run` 加 `d.checkIndex(ctx)`）、`internal/control/doctor_http_test.go` 或新建 `doctor_index_test.go`
- Modify: `internal/daemon/daemon.go`（`Collector()` 与 Doctor 构造处填 `Index`）
- Modify: `cmd/cloudfs/main.go`（分发 `index`、帮助表）
- Modify: `internal/i18n/catalog_zh.go`、`catalog_en.go`（`confirm.index_remove`、`confirm.index_rebuild`、`err.index_disabled`、`doctor.index.*`）

**Interfaces:**
- Consumes: B9 `Indexer` 方法集、`Status`、`RuleView`。
- Produces:
```go
// internal/control/index.go
type IndexControl interface {
	Status(ctx context.Context, p string) (index.Status, error)
	Rules(ctx context.Context) ([]index.RuleView, error)
	AddRule(ctx context.Context, r index.Rule) error
	RemoveRule(ctx context.Context, p string) error
	Rebuild(ctx context.Context) error
	Retry(ctx context.Context, p string) (int64, error)
	Failed(ctx context.Context, cursor string, limit int) ([]index.FailedDoc, string, error)
	Search(ctx context.Context, q index.SearchQuery) (index.SearchResult, error)
	Text(ctx context.Context, p string, off int64, max int) (index.TextPage, error)
	Watch() (<-chan index.Progress, func())
}
type IndexRulesResponse struct{ Rules []index.RuleView `json:"rules"` }
type IndexAddRequest struct {
	Path        string   `json:"path"`
	Include     []string `json:"include,omitempty"`
	MaxFileSize int64    `json:"max_file_size,omitempty"`
}
type IndexRemoveRequest struct {
	Path    string `json:"path"`
	Confirm bool   `json:"confirm"`
}
type IndexRebuildRequest struct{ Confirm bool `json:"confirm"` }
type IndexRetryRequest struct{ Path string `json:"path,omitempty"` }
type IndexFailedResponse struct {
	Documents  []index.FailedDoc `json:"documents"`
	NextCursor string            `json:"next_cursor,omitempty"`
}
type IndexStatusBrief struct{ Enabled bool `json:"enabled"` }
// Collector gains: Index IndexControl ; Status gains: Index *IndexStatusBrief `json:"index,omitempty"`
// Doctor gains: Index IndexControl

// internal/control/index_client.go
func CallIndex(ctx context.Context, socket, tcp, method, target string, body, out any) (online bool, err error)
```

- [ ] **Step 1: 写失败测试（control）**

```go
// internal/control/index_test.go
type fakeIndex struct{ rules []index.RuleView; removed, rebuilt int; searches []index.SearchQuery; progress chan index.Progress }
// fakeIndex implements IndexControl with in-memory data (Status returns Enabled:true, Docs{OK: 3}).

func TestIndexStatusWhenDisabled(t *testing.T) {
	f := newFixture(t)
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/index/status", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"enabled":false}` { t.Fatalf("%d %s", w.Code, w.Body) }
	if w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/index/rules", ""); w.Code != 404 { t.Fatalf("rules while disabled: %d", w.Code) }
}

func TestIndexRemoveAndRebuildNeedConfirm(t *testing.T) {
	f := newFixture(t); fi := &fakeIndex{}; f.coll.Index = fi
	h := NewServer(f.coll).Handler()
	if w := uiCallControl(t, h, "POST", "/index/remove", `{"path":"/notes"}`); w.Code != 400 || fi.removed != 0 { t.Fatalf("remove without confirm: %d", w.Code) }
	if w := uiCallControl(t, h, "POST", "/index/remove", `{"path":"/notes","confirm":true}`); w.Code != 200 || fi.removed != 1 { t.Fatalf("%d", w.Code) }
	if w := uiCallControl(t, h, "POST", "/index/rebuild", `{}`); w.Code != 400 || fi.rebuilt != 0 { t.Fatalf("rebuild without confirm: %d", w.Code) }
	if w := uiCallControl(t, h, "POST", "/index/rebuild", `{"confirm":true}`); w.Code != 200 || fi.rebuilt != 1 { t.Fatalf("%d", w.Code) }
}

func TestIndexRemoveOfAConfigRuleIs409(t *testing.T) { /* fakeIndex.RemoveRule returns index.ErrConfigRule → 409 with that message */ }

func TestIndexSearchRoutePassesQuery(t *testing.T) {
	f := newFixture(t); fi := &fakeIndex{}; f.coll.Index = fi
	uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/index/search?q=%E7%BD%91%E7%9B%98&path=/work&mode=hybrid&limit=5", "")
	if len(fi.searches) != 1 || fi.searches[0].Query != "网盘" || fi.searches[0].Roots[0] != "/work" || fi.searches[0].TopK != 5 || fi.searches[0].Mode != "hybrid" { t.Fatalf("%+v", fi.searches) }
}

func TestIndexTextAndFailedRoutesPage(t *testing.T) { /* /index/text?path=/a&offset=10&max_bytes=20 reaches Text with those values; /index/failed?limit=2 returns next_cursor */ }
func TestIndexAddRouteMarksUISource(t *testing.T) { /* POST /index/add {"path":"/notes"} → AddRule received Source "ui" */ }
func TestStatusCarriesIndexEnabled(t *testing.T) { /* with fakeIndex: GET /status contains "index":{"enabled":true}; without: no "index" key */ }
func TestIndexMetricsAreExported(t *testing.T) { /* GET /metrics contains cloudfs_index_documents{state="ok"} 3 and cloudfs_index_pending */ }
```
```go
// internal/control/events_test.go (append)
func TestEventsStreamThrottlesIndexProgress(t *testing.T) {
	// fakeIndex.progress is a buffered channel; push 50 Progress values quickly.
	// Reading the SSE stream for 1.5s must yield at most 3 "event: index" frames,
	// and the last frame carries the last pushed Pending value.
}
```
```go
// internal/control/doctor_index_test.go
func TestDoctorReportsIndexHealth(t *testing.T) {
	// Doctor{Index: fakeIndex with Status{Enabled:true, Docs{Failed:2}, TextBytes: 95, MaxTotalText: 100}}
	// Run → a check named "index_db" at LevelOK, "index_failed" at LevelWarn and Fixable,
	// and "index_text_budget" at LevelWarn.
}
```

- [ ] **Step 2: 写失败测试（cmd）**

```go
// cmd/cloudfs/index_test.go
func TestIndexSearchFlags(t *testing.T) {
	q, err := indexSearchFromArgs([]string{"网盘", "--path", "/work", "--mode", "keyword", "--limit", "7"})
	if err != nil || q.Query != "网盘" || q.Roots[0] != "/work" || q.Mode != "keyword" || q.TopK != 7 { t.Fatalf("%+v %v", q, err) }
}
func TestIndexRebuildNeedsConfirm(t *testing.T) { /* runIndex(ctx, out, []string{"rebuild"}) returns an error naming --confirm */ }
```

- [ ] **Step 3: 运行确认失败**

Run: `./gow test ./internal/control/ -run 'TestIndex|TestStatusCarriesIndex|TestEventsStreamThrottles|TestDoctorReportsIndex|TestEveryRouteIsEitherGuardedOrArguedOpen' -v; ./gow test ./cmd/cloudfs/ -run TestIndex -v`
Expected: FAIL

- [ ] **Step 4: 实现**

- 路由（表尾）：
```go
		{pattern: "/index/status", handler: s.indexStatus},
		{pattern: "/index/rules", handler: s.indexRules},
		{pattern: "/index/add", handler: s.indexAdd},
		{pattern: "/index/remove", handler: s.indexRemove},
		{pattern: "/index/rebuild", handler: s.indexRebuild},
		{pattern: "/index/retry", handler: s.indexRetry},
		{pattern: "/index/failed", handler: s.indexFailed},
		{pattern: "/index/search", handler: s.indexSearch},
		{pattern: "/index/text", handler: s.indexText},
```
- 每个 handler：`privateRequest` → `allowMethod`；`collector.Index == nil`：`/index/status` 返回 `{"enabled":false}`，其余 `httpErrorT(w, r, 404, "err.index_disabled")`；`/index/remove` `confirmed(w, r, q.Confirm, "confirm.index_remove", q.Path)`，`ErrConfigRule` → 409；`/index/rebuild` `confirmed(..., "confirm.index_rebuild")`；`/index/add` 固定 `Source: "ui"`；`/index/search` `path` 默认 `/`，`limit` 默认 20、上限 100；`/index/text` `max_bytes` 默认 65536、上限 1 MiB。控制面没有 `--allow`，与 `/fs/*` 同一信任边界（本机控制台）。
- `status.go`：`if c.Index != nil { st.Index = &IndexStatusBrief{Enabled: true} }`。
- `events.go`：
```go
	var indexEvents <-chan index.Progress
	if s.collector.Index != nil {
		ch, stop := s.collector.Index.Watch()
		defer stop()
		indexEvents = ch
	}
	var lastIndex time.Time
	var pendingIndex *index.Progress
	indexFlush := time.NewTicker(time.Second)
	defer indexFlush.Stop()
	// select gains:
		case p, ok := <-indexEvents:
			if !ok { indexEvents = nil; continue }
			pendingIndex = &p
			if time.Since(lastIndex) >= time.Second {
				if !send("index", *pendingIndex) { return }
				lastIndex, pendingIndex = time.Now(), nil
			}
		case <-indexFlush.C:
			if pendingIndex != nil {
				if !send("index", *pendingIndex) { return }
				lastIndex, pendingIndex = time.Now(), nil
			}
```
- 指标：`cloudfs_index_documents{state="ok|dirty|failed"}`、`cloudfs_index_chunks`、`cloudfs_index_pending`、`cloudfs_index_text_bytes`、`cloudfs_index_fetch_bytes_total`（`Status.FetchBudget.Used` 为本小时窗口值；counter 取 B9 `Status.FetchBytesTotal`，由 Indexer 在每次成功读取后累加）。
- `doctor.go`：`checkIndex`：`d.Index == nil` → 无检查；`Status("")` 出错 → `index_db` `LevelFail`；否则 `index_db` `LevelOK`（detail `doctor.index.ok`：文档数、分块数）；`Docs.Failed > 0` → `index_failed` `LevelWarn`、`Fixable: true`、fix `doctor.index.failed.fix`；`MaxTotalText > 0 && TextBytes*10 >= MaxTotalText*9` → `index_text_budget` `LevelWarn`。`Fix` 中对 `index_failed` 调 `Retry(ctx, "")`。
- i18n catalog（zh / en）：`confirm.index_remove`「移出后该路径下的抽取文本与分块会被删除：%s」/「Removing it deletes the extracted text and chunks under %s」；`confirm.index_rebuild`「重建会清空索引并按限流重新下载规则覆盖的文件」/「Rebuilding clears the index and downloads rule-covered files again, rate-limited」；`err.index_disabled`「内容索引未启用（index.enabled: false）」/「Content indexing is off (index.enabled: false)」；`doctor.index.ok`「索引正常：%d 个文档，%d 个分块」/「Index healthy: %d documents, %d chunks」；`doctor.index.failed`「%d 个文档抽取失败」/「%d documents failed to extract」；`doctor.index.failed.fix`「cloudfs doctor --fix 会把失败文档重新排队」/「cloudfs doctor --fix requeues failed documents」；`doctor.index.budget`「索引文本已用 %s / %s」/「Index text uses %s of %s」。
- `cmd/cloudfs/index.go`：`cloudfs index status [--json] | add <path> [--include g1,g2] [--max-file-size 20MiB] | rm <path> --confirm | rebuild --confirm | retry [<path>] | search <query> [--path P] [--mode keyword|hybrid] [--limit N] [--json]`；在线 `control.CallIndex`；离线仅 `status`/`search`：`index.OpenStoreReadOnly(cfg.Cache.Dir)`（`search` 离线时 `Current` 传"总是非 stale"，并在输出头注明 `offline: stale marks unavailable`）；其余离线返回 `requires the running daemon`。

- [ ] **Step 5: 运行确认通过**

Run: `./gow build ./... && ./gow test -race ./internal/control/ ./internal/daemon/ ./cmd/cloudfs/ ./internal/index/ -count=1 && ./gow vet ./...`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/control internal/daemon cmd/cloudfs internal/i18n internal/index
git commit -m "feat(control): index endpoints, progress events, CLI, metrics and doctor checks"
```

---

## Task B11：UI F4-a —「索引」屏（T-37 界面：概况卡、规则表、失败表、重建）

**Files:**
- Create: `internal/control/web/index_presets.js`、`internal/control/web/_tests/index_presets.test.mjs`
- Create: `internal/control/web/screens/index.js`
- Create: `internal/control/ui_index_test.go`
- Modify: `internal/control/web/router.js`、`app.js`（import、`screens` 映射、`onIndexChange`）、`api.js`（`events` 加 `onIndex`）、`i18n.js`

**Interfaces:**
- Consumes: B10 `/index/status`、`/index/rules`、`/index/add`、`/index/remove`、`/index/rebuild`、`/index/retry`、`/index/failed`、SSE `index`；B0 `layers` 图标。
- Produces（JS）:
```js
// index_presets.js — zero imports
export const PRESETS // { docs: [...], code: [...], text: [...] }
export function includeFor(preset)   // copy of the list, [] for unknown
export function parseSize(text)      // '20MiB' -> 20971520; '' -> 0; invalid -> NaN
// app.js
export function onIndexChange(fn)
// screens/index.js
export function renderIndex(host)    // returns dispose
```

- [ ] **Step 1: 写失败测试（mjs）**

```js
// internal/control/web/_tests/index_presets.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { PRESETS, includeFor, parseSize } from '../index_presets.js';

test('the documents preset covers office and pdf files', () => {
  for (const g of ['**/*.md', '**/*.pdf', '**/*.docx', '**/*.xlsx', '**/*.pptx']) assert.ok(PRESETS.docs.includes(g), g);
});
test('includeFor returns a copy so a form cannot edit the preset', () => {
  const list = includeFor('code'); list.push('x');
  assert.ok(!PRESETS.code.includes('x'));
  assert.deepEqual(includeFor('nope'), []);
});
test('sizes parse with binary units', () => {
  assert.equal(parseSize('20MiB'), 20 * 1024 * 1024);
  assert.equal(parseSize('512 KiB'), 512 * 1024);
  assert.equal(parseSize(''), 0);
  assert.ok(Number.isNaN(parseSize('lots')));
});
```

- [ ] **Step 2: 写失败测试（Go）**

```go
// internal/control/ui_index_test.go
package control

func TestIndexScreenIsRoutedAndInTheNav(t *testing.T) {
	router := webSource(t, "web/router.js")
	for _, want := range []string{"'#/index': 'index-view'", "hash: '#/index', icon: 'layers', key: 'nav.index'"} {
		if !strings.Contains(router, want) { t.Errorf("router.js lacks %s", want) }
	}
	if !strings.Contains(webSource(t, "web/app.js"), "renderIndex") { t.Error("the shell never mounts the index screen") }
}

// A disabled index renders an explanation, and must not go on to ask for rules
// the daemon will refuse.
func TestIndexScreenSkipsRulesWhenDisabled(t *testing.T) {
	src := webSource(t, "web/screens/index.js")
	gate := strings.Index(src, "if (!st.enabled)")
	rules := strings.Index(src, "api.get('/index/rules'")
	if gate < 0 || rules < 0 || gate > rules { t.Fatalf("the enabled check (%d) must come before the rules request (%d)", gate, rules) }
	if !strings.Contains(src[gate:rules], "return") { t.Error("the disabled branch does not return before loading rules") }
}

func TestIndexMutationsCarryConfirm(t *testing.T) {
	src := webSource(t, "web/screens/index.js")
	for _, want := range []string{"api.post('/index/remove', { path: rule.path, confirm: true })", "api.post('/index/rebuild', { confirm: true })", "confirmToken: rule.path", "confirmToken: 'rebuild'", "api.post('/index/add'", "api.post('/index/retry'"} {
		if !strings.Contains(src, want) { t.Errorf("index screen lacks %s", want) }
	}
}

func TestConfigRulesHaveNoRemoveButton(t *testing.T) {
	src := webSource(t, "web/screens/index.js")
	if !strings.Contains(src, "rule.source === 'config'") || !strings.Contains(src, "t('index.rule.inconfig')") {
		t.Error("configuration rules must render the in-config note instead of a remove action")
	}
}

func TestIndexScreenPagesFailedDocsAndFollowsProgress(t *testing.T) {
	src := webSource(t, "web/screens/index.js")
	for _, want := range []string{"api.get('/index/failed?", "next_cursor", "moreRow(", "onIndexChange(", "t('index.paused.' + "} {
		if !strings.Contains(src, want) { t.Errorf("index screen lacks %s", want) }
	}
}

func TestWebCatalogCoversIndexPauseReasons(t *testing.T) {
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, r := range []string{"busy", "risk_control", "budget", "text_budget"} {
		if !zh["index.paused."+r] { t.Errorf("missing index.paused.%s", r) }
	}
	for _, s := range []string{"config", "ui", "tool"} {
		if !zh["index.rule.source."+s] { t.Errorf("missing index.rule.source.%s", s) }
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `node --test internal/control/web/_tests/index_presets.test.mjs; ./gow test ./internal/control/ -run 'TestIndexScreen|TestIndexMutations|TestConfigRules|TestWebCatalogCoversIndex' -v`
Expected: FAIL

- [ ] **Step 4: 实现**

`router.js`：`routes` 加 `'#/index': 'index-view',`；`navItems` 在 `#/storage` 之前加 `{ hash: '#/index', icon: 'layers', key: 'nav.index' },`。
`api.js`：`events` 解构加 `onIndex`，`es.addEventListener('index', ...)`。
`app.js`：`import { renderIndex } from '/ui/screens/index.js';`、`'index-view': renderIndex`、
```js
const indexHandlers = new Set();
export function onIndexChange(fn) { indexHandlers.add(fn); return () => indexHandlers.delete(fn); }
// events({... onIndex: (p) => { for (const fn of indexHandlers) fn(p); } })
```
`index_presets.js`：
```js
export const PRESETS = Object.freeze({
  docs: Object.freeze(['**/*.md', '**/*.txt', '**/*.pdf', '**/*.docx', '**/*.xlsx', '**/*.pptx']),
  code: Object.freeze(['**/*.go', '**/*.py', '**/*.ts', '**/*.js', '**/*.rs', '**/*.java', '**/*.c', '**/*.h', '**/*.sh', '**/*.sql']),
  text: Object.freeze(['**/*.md', '**/*.txt', '**/*.rst', '**/*.csv', '**/*.json', '**/*.yaml', '**/*.yml', '**/*.toml', '**/*.html']),
});
export function includeFor(preset) { return PRESETS[preset] ? [...PRESETS[preset]] : []; }
const UNITS = { '': 1, b: 1, kib: 1024, mib: 1024 ** 2, gib: 1024 ** 3, kb: 1000, mb: 1000 ** 2, gb: 1000 ** 3 };
export function parseSize(text) {
  const s = String(text || '').trim();
  if (!s) return 0;
  const m = /^(\d+(?:\.\d+)?)\s*([a-zA-Z]*)$/.exec(s);
  if (!m || !(m[2].toLowerCase() in UNITS)) return NaN;
  return Math.round(Number(m[1]) * UNITS[m[2].toLowerCase()]);
}
```
`screens/index.js` 结构：
```js
import { api } from '/ui/api.js';
import { el, fill, bytes, toast, confirmDelete, openForm, moreRow, iconEl } from '/ui/ui.js';
import { t, locale } from '/ui/i18n.js';
import { pageCursor, pageFailureMode } from '/ui/paged.js';
import { onIndexChange } from '/ui/app.js';
import { includeFor, parseSize } from '/ui/index_presets.js';

const DISABLED_EXAMPLE = 'index:\n  enabled: true\n  pinned: true\n  rules:\n    - path: /work/notes\n      max_file_size: 20MiB\n';

export function renderIndex(host) {
  let disposed = false;
  const stops = [];
  (async () => {
    let st;
    try { st = await api.get('/index/status'); } catch (err) { toast(err.message, 'bad'); return; }
    if (disposed) return;
    if (!st.enabled) {
      fill(host, el('div', { class: 'eyebrow' }, t('index.eyebrow')), el('h2', {}, t('index.title')),
        el('p', { class: 'detail' }, t('index.disabled.body')), el('pre', { class: 'detail' }, DISABLED_EXAMPLE));
      return;
    }
    const cards = el('div', { class: 'row' });
    const progress = el('div', {});
    const rulesBody = el('tbody', {});
    const failedBody = el('tbody', {});
    fill(host, el('div', { class: 'eyebrow' }, t('index.eyebrow')), el('h2', {}, t('index.title')), cards, progress,
      el('div', { class: 'row' }, el('button', { onclick: addRule }, iconEl('plus'), t('index.rule.add')),
        el('button', { class: 'danger', onclick: rebuild }, t('index.rebuild'))),
      el('h3', {}, t('index.rules')), el('table', {}, el('tbody', {}), rulesBody),
      el('h3', {}, t('index.failed')), el('table', {}, failedBody));
    renderCards(st); renderProgress(st.progress);
    await loadRules(); await loadFailed('');
    stops.push(onIndexChange((p) => renderProgress(p)));
    // renderCards, renderProgress, loadRules, loadFailed, addRule, rebuild defined below
  })();
  return () => { disposed = true; for (const s of stops) s(); };
}
```
- `renderCards(st)`：四张卡（照 `screens/storage.js` 卡片 DOM）：文档 `t('index.card.docs.detail', st.docs.ok, st.docs.dirty, st.docs.failed)`（json tag 见 B9 `DocCounts`/`BudgetUse`）、分块 `st.chunks_total`、文本 `bytes(st.text_bytes) + ' / ' + bytes(st.max_total_text)`、本小时下载 `bytes(st.fetch_budget.used) + ' / ' + bytes(st.fetch_budget.limit)`。
- `renderProgress(p)`：`p.extracting || p.pending` → 进度条 + `t('index.progress', p.extracting, p.pending)`；`p.paused` → `t('index.paused.' + p.paused)` + `p.resume_at` 时 `t('index.resume', new Date(p.resume_at).toLocaleTimeString(locale()))`。
- `loadRules()`：`api.get('/index/rules')`；每行：路径 / `rule.include.join(', ')` / `(rule.exclude || []).join(', ')` / `bytes(rule.max_file_size)` / `t('index.rule.source.' + rule.source)` / `rule.documents` / 动作：`rule.source === 'config' ? el('span', { class: 'dim' }, t('index.rule.inconfig')) : el('button', { class: 'danger', onclick: () => removeRule(rule) }, t('index.rule.remove'))`。
- `removeRule(rule)`：`confirmDelete({ title: t('index.rule.remove.title'), body: t('index.rule.remove.body', rule.path), confirmToken: rule.path, confirmLabel: t('index.rule.remove') })` → `api.post('/index/remove', { path: rule.path, confirm: true })` → `loadRules()`。
- `addRule()`：`openForm`（rows 形态照 `screens/exports.js`）：路径 input、预设 select（`docs/code/text`，值 → `includeFor`）、单文件上限 input（`parseSize`，`NaN` 时 `validate` 返回 `t('index.rule.badsize')`）；`note: t('index.rule.note')`；提交 `api.post('/index/add', { path, include, max_file_size })`。
- `loadFailed(cursor)`：`api.get('/index/failed?limit=50' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''))`；行：路径 / 类型 / 错误 / 时间 / `el('button', { onclick: () => api.post('/index/retry', { path: d.path }).then(() => loadFailed('')) }, t('index.retry'))`；`next_cursor` → `moreRow(5, () => loadFailed(r.next_cursor))`；失败处理照 `exports.js` 的 `pageFailureMode`。
- `rebuild()`：`confirmDelete({ title: t('index.rebuild.title'), body: t('index.rebuild.body'), confirmToken: 'rebuild', confirmLabel: t('index.rebuild') })` → `api.post('/index/rebuild', { confirm: true })`。

i18n 键（zh / en）：
| key | zh | en |
|---|---|---|
| `nav.index` | 索引 | Index |
| `index.eyebrow` | 内容索引 | Content index |
| `index.title` | 抽取与检索 | Extraction and search |
| `index.disabled.body` | 内容索引未启用。在配置文件中加入下面的片段并重启；未配置规则时只索引已固定到本地的文件，不会额外下载。 | Content indexing is off. Add the snippet below to the configuration and restart; without rules only files already pinned locally are indexed, with no extra downloads. |
| `index.card.docs` | 文档 | Documents |
| `index.card.docs.detail` | 正常 %s · 待处理 %s · 失败 %s | %s ok · %s pending · %s failed |
| `index.card.chunks` | 分块 | Chunks |
| `index.card.text` | 文本占用 | Text stored |
| `index.card.fetch` | 本小时下载 | Downloaded this hour |
| `index.progress` | 正在抽取 %s · 待处理 %s | Extracting %s · %s pending |
| `index.paused.busy` | 前台读写繁忙，已让路 | Yielding to foreground IO |
| `index.paused.risk_control` | 网盘触发风控，暂停抽取 | Paused: the drive signalled risk control |
| `index.paused.budget` | 本小时下载预算已用完 | Paused: this hour's download budget is spent |
| `index.paused.text_budget` | 索引文本已达上限 | Paused: the index text limit is reached |
| `index.resume` | 预计 %s 恢复 | Resumes around %s |
| `index.rules` | 规则 | Rules |
| `index.rule.col.path` | 路径 | Path |
| `index.rule.col.include` | 包含 | Include |
| `index.rule.col.exclude` | 排除 | Exclude |
| `index.rule.col.maxsize` | 单文件上限 | Max file size |
| `index.rule.col.source` | 来源 | Source |
| `index.rule.col.documents` | 已覆盖文档 | Documents |
| `index.rule.source.config` | 配置文件 | Configuration |
| `index.rule.source.ui` | 控制台 | Console |
| `index.rule.source.tool` | Agent 工具 | Agent tool |
| `index.rule.inconfig` | 在配置文件中修改 | Change it in the configuration file |
| `index.rule.add` | 添加规则 | Add rule |
| `index.rule.preset` | 文件类型 | File types |
| `index.preset.docs` | 文档 | Documents |
| `index.preset.code` | 代码 | Code |
| `index.preset.text` | 全部文本 | All text |
| `index.rule.badsize` | 大小格式不对，例如 20MiB | Size not understood; for example 20MiB |
| `index.rule.note` | 会按限流下载该目录下匹配的文件；非官方接口网盘建议改用固定。 | Matching files under this folder are downloaded, rate-limited. For drives on unofficial APIs, pin them instead. |
| `index.rule.remove` | 移除 | Remove |
| `index.rule.remove.title` | 移除索引规则 | Remove index rule |
| `index.rule.remove.body` | 输入路径确认。%s 下的抽取文本会被删除。 | Type the path to confirm. Extracted text under %s is deleted. |
| `index.failed` | 失败文档 | Failed documents |
| `index.failed.col.kind` | 类型 | Type |
| `index.failed.col.error` | 错误 | Error |
| `index.retry` | 重试 | Retry |
| `index.rebuild` | 重建索引 | Rebuild index |
| `index.rebuild.title` | 重建整个索引 | Rebuild the whole index |
| `index.rebuild.body` | 输入 rebuild 确认。索引会被清空，规则覆盖的文件会按限流重新下载。 | Type rebuild to confirm. The index is cleared and rule-covered files are downloaded again, rate-limited. |

- [ ] **Step 5: 运行确认通过**

Run: `node --test internal/control/web/_tests/*.test.mjs && ./gow test ./internal/control/ -run 'TestIndexScreen|TestIndexMutations|TestConfigRules|TestWebCatalog|TestWebScreens|TestEveryTranslationKey|TestIconsAreSizedAndDefined|TestBrowserModule|TestScreensDoNotStringifyASkippedChild|TestWebAppNeverAsksForACredential' -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/control/web internal/control/ui_index_test.go internal/index/service.go
git commit -m "feat(ui): index screen with coverage cards, rules and failed documents"
```

---

## Task B12：UI F4-b — 主窗口内容搜索、`snippet.js`、检查器索引行与抽取文本浮层（T-37 界面）

**Files:**
- Create: `internal/control/web/snippet.js`、`internal/control/web/_tests/snippet.test.mjs`
- Create: `internal/control/web/content_search.js`、`internal/control/web/extracted_text.js`、`internal/control/web/index_inspector.js`
- Create: `internal/control/ui_content_search_test.go`
- Modify: `internal/control/web/screens/main.js`（搜索框旁分段切换、搜索监听分支、检查器接线、`?q=&mode=content` 深链）、`internal/control/web/i18n.js`

**Interfaces:**
- Consumes: B10 `/index/search`、`/index/status?path=`、`/index/add`、`/index/remove`、`/index/text`；`/status` 的 `index.enabled`。
- Produces（JS）:
```js
// snippet.js — zero imports
export function truncateRunes(text, max)        // by code points; appends '…' when cut; never splits a surrogate pair
export function highlightParts(text, query)     // -> [{ text, hit }] case-insensitive; no HTML anywhere
export function snippetParts(text, query, max = 240)
// content_search.js
export const SEARCH_MODE_KEY = 'cloudfs.search.mode';
export function readSearchMode()                // 'name' | 'content'; storage failures -> 'name'
export function writeSearchMode(mode)
export async function runContentSearch({ rows, query, cwd, onOpen })
// extracted_text.js
export async function openExtractedText(path, startOff)
// index_inspector.js
export function renderIndexInfo(entry, host)     // GET /index/status?path=, fills an info row and actions
```

- [ ] **Step 1: 写失败测试（mjs）**

```js
// internal/control/web/_tests/snippet.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { truncateRunes, highlightParts, snippetParts } from '../snippet.js';

test('highlighting is case-insensitive and keeps the original casing', () => {
  assert.deepEqual(highlightParts('CloudFS mounts cloudfs', 'cloudfs'),
    [{ text: 'CloudFS', hit: true }, { text: ' mounts ', hit: false }, { text: 'cloudfs', hit: true }]);
});
test('CJK text is truncated by character, never by byte', () => {
  const s = '云端文件系统'.repeat(100);
  const out = truncateRunes(s, 7);
  assert.equal(out, '云端文件系统云…');
});
test('a surrogate pair is never split', () => {
  const out = truncateRunes('a😀b😀c', 2);
  assert.equal(out, 'a😀…');
});
test('markup in file content stays plain text parts', () => {
  const parts = snippetParts('<img src=x onerror=alert(1)> needle', 'needle', 240);
  assert.equal(parts[0].text, '<img src=x onerror=alert(1)> ');
  assert.equal(parts[0].hit, false);
  assert.ok(parts.every((p) => typeof p.text === 'string' && !('html' in p)));
});
test('the snippet is centred on the first hit and bounded', () => {
  const text = 'x'.repeat(1000) + 'needle' + 'y'.repeat(1000);
  const parts = snippetParts(text, 'needle', 240);
  const joined = parts.map((p) => p.text).join('');
  assert.ok([...joined].length <= 242, String([...joined].length));
  assert.ok(parts.some((p) => p.hit && p.text === 'needle'));
});
test('an empty query yields one plain part', () => {
  assert.deepEqual(highlightParts('abc', ''), [{ text: 'abc', hit: false }]);
});
```

- [ ] **Step 2: 写失败测试（Go）**

```go
// internal/control/ui_content_search_test.go
package control

func TestContentSearchToggleOnlyWhenIndexEnabled(t *testing.T) {
	src := webSource(t, "web/screens/main.js")
	for _, want := range []string{"function indexEnabled()", "status.index && status.index.enabled", "readSearchMode()", "writeSearchMode("} {
		if !strings.Contains(src, want) { t.Errorf("main.js lacks %s", want) }
	}
	i := strings.Index(src, "t('search.mode.content')")
	if i < 0 || !strings.Contains(src[max(0, i-400):i], "indexEnabled()") { t.Error("the content toggle is rendered without checking index.enabled") }
}

func TestContentSearchCallsIndexSearchAndKeepsItsNotes(t *testing.T) {
	src := webSource(t, "web/content_search.js")
	for _, want := range []string{"api.get('/index/search?q=' + encodeURIComponent(query)", "r.degraded", "r.truncated", "t('search.content.degraded')", "t('search.content.truncated')", "hit.stale", "t('search.content.stale')", "hit.heading"} {
		if !strings.Contains(src, want) { t.Errorf("content_search.js lacks %s", want) }
	}
}

func TestSnippetIsInsertedAsText(t *testing.T) {
	for _, f := range []string{"web/content_search.js", "web/extracted_text.js", "web/snippet.js"} {
		if strings.Contains(webSource(t, f), "html:") { t.Errorf("%s uses the html attribute on file content", f) }
	}
	if !strings.Contains(webSource(t, "web/content_search.js"), "snippetParts(") { t.Error("results do not go through snippetParts") }
}

func TestSearchModeSurvivesUnavailableStorage(t *testing.T) {
	src := webSource(t, "web/content_search.js")
	if strings.Count(src, "try {") < 2 || !strings.Contains(src, "localStorage.getItem(SEARCH_MODE_KEY)") { t.Error("search mode storage is not guarded") }
}

func TestInspectorIndexActionsReachTheirRoutes(t *testing.T) {
	ins := webSource(t, "web/index_inspector.js")
	for _, want := range []string{"api.get('/index/status?path=' + encodeURIComponent(entry.path)", "api.post('/index/add', { path: entry.path })", "api.post('/index/remove', { path: entry.path, confirm: true })", "st.rule_source === 'ui'", "openExtractedText(entry.path, 0)"} {
		if !strings.Contains(ins, want) { t.Errorf("index_inspector.js lacks %s", want) }
	}
	txt := webSource(t, "web/extracted_text.js")
	for _, want := range []string{"api.get('/index/text?path=' + encodeURIComponent(path)", "next_offset", "t('index.text.more')"} {
		if !strings.Contains(txt, want) { t.Errorf("extracted_text.js lacks %s", want) }
	}
	if !strings.Contains(webSource(t, "web/screens/main.js"), "renderIndexInfo(") { t.Error("the inspector never shows index state") }
}
```

- [ ] **Step 3: 运行确认失败**

Run: `node --test internal/control/web/_tests/snippet.test.mjs; ./gow test ./internal/control/ -run 'TestContentSearch|TestSnippetIsInserted|TestSearchModeSurvives|TestInspectorIndexActions' -v`
Expected: FAIL

- [ ] **Step 4: 实现**

`snippet.js`：
```js
export function truncateRunes(text, max) {
  const cps = Array.from(String(text));
  return cps.length <= max ? cps.join('') : cps.slice(0, max).join('') + '…';
}
export function highlightParts(text, query) {
  const s = String(text), q = String(query || '').trim();
  if (!q) return [{ text: s, hit: false }];
  const lower = s.toLowerCase(), lq = q.toLowerCase();
  const parts = [];
  let i = 0;
  for (let j = lower.indexOf(lq); j >= 0; j = lower.indexOf(lq, i)) {
    if (j > i) parts.push({ text: s.slice(i, j), hit: false });
    parts.push({ text: s.slice(j, j + q.length), hit: true });
    i = j + q.length;
  }
  if (i < s.length) parts.push({ text: s.slice(i), hit: false });
  return parts;
}
export function snippetParts(text, query, max = 240) {
  const cps = Array.from(String(text));
  const q = String(query || '').trim().toLowerCase();
  let start = 0;
  if (q) {
    const at = String(text).toLowerCase().indexOf(q);
    if (at >= 0) start = Math.max(0, Array.from(String(text).slice(0, at)).length - Math.floor((max - Array.from(q).length) / 2));
  }
  const window = cps.slice(start, start + max).join('');
  const lead = start > 0 ? '…' : '';
  const tail = start + max < cps.length ? '…' : '';
  return highlightParts(lead + window + tail, query);
}
```
（`toLowerCase` 对个别字符会改变长度；片段只用于显示，`highlightParts` 用小写串定位、用原串切片，对 ASCII 与 CJK 一致。测试若因特殊字母失败，改为逐 code point 比较。）

`content_search.js`：
```js
import { api } from '/ui/api.js';
import { el, fill, iconEl, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { snippetParts } from '/ui/snippet.js';

export const SEARCH_MODE_KEY = 'cloudfs.search.mode';
export function readSearchMode() {
  try { return localStorage.getItem(SEARCH_MODE_KEY) === 'content' ? 'content' : 'name'; } catch (_) { return 'name'; }
}
export function writeSearchMode(mode) {
  try { localStorage.setItem(SEARCH_MODE_KEY, mode); } catch (_) { /* storage may be unavailable; the mode just is not remembered */ }
}
function note(text) { return el('tr', {}, el('td', { colspan: '4', class: 'dim', style: 'text-align:center;padding:12px' }, text)); }
export async function runContentSearch({ rows, query, cwd, onOpen }) {
  let r;
  try { r = await api.get('/index/search?q=' + encodeURIComponent(query) + '&path=' + encodeURIComponent(cwd) + '&limit=50'); }
  catch (err) { toast(err.message, 'bad'); return; }
  const hits = r.hits || [];
  fill(rows, ...hits.map((hit) => el('tr', { onclick: () => onOpen(hit), 'data-hit': hit.path },
    el('td', {}, el('span', { style: 'display:flex;align-items:center;gap:10px' }, iconEl('file'), hit.path.split('/').pop())),
    el('td', { class: 'dim' }, hit.heading || ''),
    el('td', { class: 'detail' }, ...snippetParts(hit.snippet, query).map((p) => (p.hit ? el('mark', {}, p.text) : p.text))),
    el('td', {}, hit.stale ? el('span', { class: 'chip' }, t('search.content.stale')) : null))));
  if (!hits.length) rows.append(note(t('empty')));
  // Both notes belong to this result set, so they stay with the rows.
  if (r.degraded) rows.append(note(t('search.content.degraded')));
  if (r.truncated) rows.append(note(t('search.content.truncated')));
}
```
`extracted_text.js`：
```js
import { api } from '/ui/api.js';
import { el, showPanel, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
const PAGE = 65536;
export async function openExtractedText(path, startOff) {
  const body = el('pre', { class: 'detail', style: 'white-space:pre-wrap;max-height:60vh;overflow:auto' });
  const more = el('button', {}, t('index.text.more'));
  let offset = Math.max(0, (startOff || 0) - 1024);
  async function load() {
    try {
      const r = await api.get('/index/text?path=' + encodeURIComponent(path) + '&offset=' + offset + '&max_bytes=' + PAGE);
      body.append(document.createTextNode(r.text));
      offset = r.next_offset;
      more.hidden = r.eof;
    } catch (err) { toast(err.message, 'bad'); }
  }
  more.addEventListener('click', load);
  showPanel({ title: t('index.text.title', path.split('/').pop()), content: el('div', {}, offset > 0 ? el('div', { class: 'dim' }, t('index.text.from', offset)) : null, body, more) });
  await load();
}
```
`index_inspector.js`：
```js
import { api } from '/ui/api.js';
import { el, fill, toast, confirmDelete } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { openExtractedText } from '/ui/extracted_text.js';
export async function renderIndexInfo(entry, host) {
  let st;
  try { st = await api.get('/index/status?path=' + encodeURIComponent(entry.path)); } catch (_) { return; }
  const label = st.state === 'ok' ? t('inspector.index.ok', st.chunks)
    : st.state === 'failed' ? t('inspector.index.failed', st.error || '')
    : st.state === 'pending' || st.state === 'dirty' ? t('inspector.index.pending') : t('inspector.index.uncovered');
  fill(host,
    el('div', { style: 'display:flex;justify-content:space-between;gap:12px;font-size:13px;margin-bottom:10px' },
      el('span', { class: 'muted' }, t('inspector.index')), el('span', { class: 'detail' }, label)),
    el('div', { class: 'row', style: 'flex-wrap:wrap' },
      st.state === 'uncovered' ? el('button', { onclick: async () => {
        try { await api.post('/index/add', { path: entry.path }); toast(t('index.added')); renderIndexInfo(entry, host); } catch (err) { toast(err.message, 'bad'); }
      } }, t('action.index.add')) : null,
      st.rule_source === 'ui' && st.covered === entry.path ? el('button', { class: 'danger', onclick: async () => {
        const ok = await confirmDelete({ title: t('index.rule.remove.title'), body: t('index.rule.remove.body', entry.path), confirmToken: entry.name, confirmLabel: t('action.index.remove') });
        if (!ok) return;
        try { await api.post('/index/remove', { path: entry.path, confirm: true }); renderIndexInfo(entry, host); } catch (err) { toast(err.message, 'bad'); }
      } }, t('action.index.remove')) : null,
      !entry.is_dir && st.state === 'ok' ? el('button', { onclick: () => openExtractedText(entry.path, 0) }, t('action.index.text')) : null));
}
```
`screens/main.js` 接线：
```js
import { readSearchMode, writeSearchMode, runContentSearch } from '/ui/content_search.js';
import { openExtractedText } from '/ui/extracted_text.js';
import { renderIndexInfo } from '/ui/index_inspector.js';
function indexEnabled() { const status = get().status; return !!(status && status.index && status.index.enabled); }
let searchMode = readSearchMode();
const modeHost = el('div', { class: 'row', role: 'group', 'aria-label': t('search.mode') });
function renderModeToggle() {
  if (!indexEnabled()) { fill(modeHost); searchMode = 'name'; return; }
  fill(modeHost, ...['name', 'content'].map((m) => el('button', {
    'aria-pressed': String(searchMode === m), class: searchMode === m ? 'primary' : '',
    onclick: () => { searchMode = m; writeSearchMode(m); renderModeToggle(); searchBox.dispatchEvent(new Event('input')); },
  }, m === 'content' ? t('search.mode.content') : t('search.mode.name'))));
}
// subscribe(() => renderModeToggle()) with its unsubscribe added to the screen's dispose; header: crumb, grow, modeHost, searchBox, ...
// search listener, before the /search call:
//   if (searchMode === 'content' && indexEnabled()) { await runContentSearch({ rows, query: q, cwd, onOpen: (hit) => openExtractedText(hit.path, hit.start_off) }); return; }
// renderInspector: after the replicas row, append `const indexHost = el('div', {})` and, when indexEnabled(), renderIndexInfo(e, indexHost)
// deep link on mount: const hp = new URLSearchParams(location.hash.split('?')[1] || '');
//   if (hp.get('mode') === 'content') { searchMode = 'content'; } if (hp.get('q')) { searchBox.value = hp.get('q'); once status says index.enabled, dispatch 'input' }
```
（`renderModeToggle` 里 `t('search.mode.content')` 前 400 字节内有 `indexEnabled()`，满足 `TestContentSearchToggleOnlyWhenIndexEnabled`。）

i18n 键（zh / en）：
| key | zh | en |
|---|---|---|
| `search.mode` | 搜索范围 | Search in |
| `search.mode.name` | 文件名 | Names |
| `search.mode.content` | 内容 | Content |
| `search.content.degraded` | 未配置语义检索，结果按关键词匹配 | Semantic search is not configured; results use keyword matching |
| `search.content.truncated` | 结果不完整：只检查了部分文本 | Incomplete: only part of the text was checked |
| `search.content.stale` | 可能已过期 | May be outdated |
| `inspector.index` | 索引 | Index |
| `inspector.index.ok` | 已索引 · %s 块 | Indexed · %s chunks |
| `inspector.index.pending` | 待处理 | Pending |
| `inspector.index.failed` | 失败：%s | Failed: %s |
| `inspector.index.uncovered` | 未覆盖 | Not covered |
| `action.index.add` | 加入索引 | Add to index |
| `action.index.remove` | 移出索引 | Remove from index |
| `action.index.text` | 查看抽取文本 | View extracted text |
| `index.added` | 已加入索引，稍后生效 | Added; indexing starts shortly |
| `index.text.title` | 抽取文本 · %s | Extracted text · %s |
| `index.text.more` | 加载更多 | Load more |
| `index.text.from` | 从第 %s 字节开始 | Starting at byte %s |

- [ ] **Step 5: 运行确认通过**

Run: `node --test internal/control/web/_tests/*.test.mjs && ./gow test ./internal/control/ -run 'TestContentSearch|TestSnippet|TestSearchMode|TestInspector|TestWebCatalog|TestWebScreens|TestEveryTranslationKey|TestIconsAreSizedAndDefined|TestBrowserModule|TestScreensDoNotStringifyASkippedChild|TestWebLanguageSwitchSurvivesUnavailableStorage' -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/control/web internal/control/ui_content_search_test.go
git commit -m "feat(ui): content search in the file browser and index actions in the inspector"
```

---

## Task B13：线 B 集成 — chaos、e2e、浏览器冒烟、文档、TODO 收口

**Files:**
- Create: `test/e2e/browser_helper_test.go`（**逐字复制 A11 Step 1**；若线 A 已合入则文件已存在、不改）
- Create: `test/chaos/index_test.go`、`test/e2e/index_e2e_test.go`
- Modify: `docs/mcp.md`、`docs/DESIGN.md`（新增 §4.12 内容索引）、`docs/agent-roadmap.md`（一期线 B 状态）、`README.md`、`TODO.md`（T-37 状态）

**Interfaces:**
- Consumes: B0–B12；A11 的 `stackOptions`/`newStackWith`/`newUnmountedStack`/`requireBrowser`/`startControlUI`/`renderedDOM`；`test/e2e/ui_api_test.go:19 uiCall`；`test/chaos` 的 `newRig`。
- Produces: 无（收口任务）。

- [ ] **Step 1: 写 chaos 用例**

```go
// test/chaos/index_test.go
// TestIndexSurvivesAnUncleanStopMidExtraction mirrors TestKillDuringWriteLosesNothing:
// a crash is modelled by copying the database files while the writer is busy
// and opening the copy, which is what a restart after kill -9 sees.
func TestIndexSurvivesAnUncleanStopMidExtraction(t *testing.T) {
	r := newRig(t, rigOpt{dir: t.TempDir()})
	for i := 0; i < 50; i++ { r.fake.Seed(fmt.Sprintf("work/%02d.md", i), []byte(strings.Repeat("content ", 200))) }
	// warm the listing through r.fs, give ReadRange a 20ms latency fault, build
	// index.OpenStore(dirA) + index.New(rules [/work]) and Start it.
	// Poll until Stats().DocsOK >= 10, then copy index.db, index.db-wal and
	// index.db-shm from dirA to dirB (the crash image) while it is still running.
	// Open dirB: PRAGMA integrity_check == "ok", DocsOK + Pending covers at least
	// the files seen so far, and a new Indexer over dirB (fault removed) with
	// ReconcileNow ends with DocsOK == 50 and Pending == 0.
}

func TestMaliciousArchiveFailsTheDocumentNotTheDaemon(t *testing.T) {
	// Seed work/bomb.docx as a zip with 4097 entries (build it like textract's
	// office_test buildZip) and work/good.md. ReconcileNow must return without
	// panicking; bomb.docx is DocFailed with an error mentioning the archive
	// limit; good.md is DocOK.
}
```
（两个用例按注释写全，`newRig`/`rigOpt` 以 `test/chaos/chaos_test.go` 实际定义为准；`PRAGMA integrity_check` 通过 `index.OpenStoreReadOnly` 暴露的 `DB()` 或在 `internal/index` 加导出的 `func (s *Store) IntegrityCheck(ctx) (string, error)`——选后者并在 B5 的 `store.go` 补上与一条单测。）

- [ ] **Step 2: 写 e2e 用例**

```go
// test/e2e/index_e2e_test.go
const indexYAML = "index:\n  enabled: true\n  rules:\n    - path: /\n"

func withIndex(d *daemon.Daemon, o *mcpsrv.Options) {
	if d.Index != nil { o.Index = d.Index }
}

// Needs a kernel mount: run on Linux with /dev/fuse.
func TestContentSearchFindsAFreshMarkdownFile(t *testing.T) {
	s := newStackWith(t, "writeback", stackOptions{extraYAML: indexYAML, mcp: withIndex})
	if err := os.WriteFile(filepath.Join(s.dir, "plan.md"), []byte("# Plan\nthe e2e needle 51c9\n"), 0o644); err != nil { t.Fatal(err) }
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var out struct{ Hits []struct{ Path string `json:"path"` } `json:"hits"` }
		s.callTool(t, "semantic_search", map[string]any{"query": "needle 51c9"}, &out)
		if len(out.Hits) == 1 && out.Hits[0].Path == "/plan.md" { return }
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("a file written through the mount was not searchable within 3s")
}

func TestContentSearchInTheBrowser(t *testing.T) {
	s := newUnmountedStack(t, stackOptions{extraYAML: indexYAML, mcp: withIndex})
	ctx := context.Background()
	if _, err := s.d.FS.WriteFile(ctx, "/notes/plan.md", []byte("browser smoke marker 7f3a"), false); err != nil { t.Fatal(err) }
	h := control.NewServer(s.d.Collector()).Handler()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if w := uiCall(t, h, "GET", "/index/search?q=marker%207f3a", ""); strings.Contains(w.Body.String(), "/notes/plan.md") { break }
		if time.Now().After(deadline) { t.Fatal("not indexed within 5s") }
		time.Sleep(100 * time.Millisecond)
	}
	chrome := requireBrowser(t)
	base := startControlUI(t, s.d.Collector())
	dom := renderedDOM(t, chrome, base+"/?lang=en#/connections?q=marker%207f3a&mode=content")
	if !strings.Contains(dom, `data-hit="/notes/plan.md"`) || !strings.Contains(dom, "<mark>marker 7f3a</mark>") {
		t.Fatalf("the file browser's content search did not show the hit:\n%s", dom)
	}
	if dom := renderedDOM(t, chrome, base+"/?lang=en#/index"); !strings.Contains(dom, "Rebuild index") {
		t.Fatalf("the index screen did not render:\n%s", dom)
	}
}
```

- [ ] **Step 3: 运行**

Run: `./gow test ./test/chaos/ -run 'TestIndexSurvives|TestMaliciousArchive' -race -v -count=1`
Expected: PASS
Run: `./gow test ./test/e2e/ -run TestContentSearchInTheBrowser -v -count=1`
Expected: PASS（浏览器部分 SKIP）
Run: `CLOUDFS_BROWSER=1 ./gow test ./test/e2e/ -run TestContentSearchInTheBrowser -v -count=1`
Expected: PASS
Run（Linux + `/dev/fuse`）: `./gow test ./test/e2e/ -run TestContentSearchFindsAFreshMarkdownFile -v -count=1`
Expected: PASS；macOS 无 macFUSE 为 SKIP，TODO 标 `[~]`。

- [ ] **Step 4: 文档**

- `docs/mcp.md`：索引工具表（5 个）与"Agent 使用建议"追加：`semantic_search` 只覆盖索引范围，先看 `index_status.covered`；hit 的 `offset_kind=file` 时 `start_off` 可直接传 `read_text.offset`，`text` 时用 `read_extracted_text`；`degraded` 表示此期只有关键词；`quark` 等 `unofficial` 网盘不建议配 `rules`，用 `pinned`；不做 OCR；`UNVERIFIED` 中文 PDF 质量。
- `docs/DESIGN.md` 新增 §4.12 内容索引：index.db 独立于 meta 的理由、身份 `(remote, remote_id, version)`、让路与风控、分块参数、与 `search.content` 的区别。
- `README.md`：`index:` 配置示例（`enabled`、`pinned`、`rules`、`exclude`、`max_text_bytes`、`max_total_text`、`fetch_budget`），命令表加 `cloudfs index ...`。
- `docs/agent-roadmap.md`：一期线 B 状态。
- `TODO.md`：T-37 改 `[x]` 或 `[~]`，每条验收后附测试名；未满足项（FUSE e2e、中文 PDF 真实样本）写明缺口。

- [ ] **Step 5: 线 B 全量验证**

```bash
./gow build ./...
./gow vet ./...
./gow test $(./gow list ./... | grep -v -e internal/fusefs -e test/conformance -e test/e2e)
./gow test -race ./internal/textract/ ./internal/index/ ./internal/mcpsrv/ ./internal/control/ ./internal/daemon/ ./internal/config/ ./internal/meta/ ./cmd/cloudfs/
node --test internal/control/web/_tests/*.test.mjs
./gow test ./test/perf/ -count=1
./gow test ./test/chaos/ -run TestIndex -race -count=1
CLOUDFS_BROWSER=1 ./gow test ./test/e2e/ -run TestContentSearchInTheBrowser -v -count=1
```
Expected: 全部 PASS；`test/perf` 既有基线不变，新增 `TestIndex*` 通过。

- [ ] **Step 6: 提交**

```bash
git add test/chaos test/e2e docs README.md TODO.md internal/index
git commit -m "test(chaos,e2e): index crash image, archive bomb and browser content search; docs for T-37"
```

---

## Task B14（一期末可选）：UI F9 前半 — 检查器"发送给 Agent"仅复制提示词（T-42 前半）

**Files:**
- Create: `internal/control/agent_prompt.go`、`internal/control/agent_prompt_test.go`
- Create: `internal/control/web/send_to_agent.js`、`internal/control/ui_send_to_agent_test.go`
- Modify: `internal/control/metrics.go`（`/agent/prompt`）、`internal/i18n/catalog_zh.go`、`catalog_en.go`（`agent.prompt.*` 服务端提示词文案）
- Modify: `internal/control/web/screens/main.js`（检查器按钮）、`internal/control/web/content_search.js`（结果行按钮）、`internal/control/web/i18n.js`、`internal/control/web/icons.js`（仅当线 A 未合入：加与 A0 **完全相同**的 `bot` 行）

**Interfaces:**
- Consumes: 现有 `Collector.FS.StatPath`；`Collector.Index`（B10）与 `Collector.Agent`（A4，可能为 nil）只用于决定提示词里提哪些工具。
- Produces:
```go
type AgentPromptResponse struct {
	Path   string `json:"path"`
	URI    string `json:"uri"`
	Prompt string `json:"prompt"`
}
```
```js
// send_to_agent.js
export async function openSendToAgent({ path, heading })
```

- [ ] **Step 1: 写失败测试**

```go
// internal/control/agent_prompt_test.go
func TestAgentPromptForAFile(t *testing.T) {
	f := newFixtureWithFS(t) // the fs_test.go fixture that serves /fs/* over a fake remote
	f.fake.Seed("work/plan.md", []byte("x"))
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/agent/prompt?path=/work/plan.md", "")
	var r AgentPromptResponse
	json.Unmarshal(w.Body.Bytes(), &r)
	if w.Code != 200 || r.Path != "/work/plan.md" || !strings.HasPrefix(r.URI, "cloudfs://") || !strings.Contains(r.Prompt, "read_text") { t.Fatalf("%d %+v", w.Code, r) }
	if strings.Contains(r.Prompt, "read_extracted_text") { t.Error("mentions an index tool while indexing is off") }
	if strings.Contains(r.Prompt, "begin_session") && f.coll.Agent == nil { t.Error("mentions session tools the daemon does not offer") }
}
func TestAgentPromptMissingPathIs404WithoutInternals(t *testing.T) {
	f := newFixtureWithFS(t)
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/agent/prompt?path=/nope/x.md", "")
	if w.Code != 404 || strings.Contains(w.Body.String(), f.dir) || strings.Contains(w.Body.String(), "remote") { t.Fatalf("%d %s", w.Code, w.Body) }
}
```
（`newFixtureWithFS` 以 `internal/control/fs_test.go` 现有带 VFS 的 fixture 名为准。）
```go
// internal/control/ui_send_to_agent_test.go
func TestSendToAgentOnlyReadsAndCopies(t *testing.T) {
	src := webSource(t, "web/send_to_agent.js")
	if !strings.Contains(src, "api.get('/agent/prompt?path=' + encodeURIComponent(path)") || strings.Contains(src, "api.post(") {
		t.Error("the send panel must fetch the prompt and never write")
	}
	if !strings.Contains(src, "textarea.value = ") || strings.Contains(src, "html:") { t.Error("the prompt must be set as a value, not as markup") }
	if !strings.Contains(src, "clipboard.writeText(textarea.value)") { t.Error("copy must take the edited text") }
}
func TestSendToAgentHasBothEntrances(t *testing.T) {
	if !strings.Contains(webSource(t, "web/screens/main.js"), "openSendToAgent({ path: e.path })") { t.Error("inspector entrance missing") }
	if !strings.Contains(webSource(t, "web/content_search.js"), "openSendToAgent({ path: hit.path, heading: hit.heading })") { t.Error("search result entrance missing") }
}
```

- [ ] **Step 2: 运行确认失败**

Run: `./gow test ./internal/control/ -run 'TestAgentPrompt|TestSendToAgent|TestEveryRouteIsEitherGuardedOrArguedOpen' -v`
Expected: FAIL

- [ ] **Step 3: 实现**

- 路由 `{pattern: "/agent/prompt", handler: s.agentPrompt}`；GET；`s.fsPath(w, path)` 规范化；`StatPath` 出错 → `http.Error(w, "not found", 404)`（不透传错误文本）。`URI`：与 `internal/mcpsrv/resources.go` 的 `cloudfs://` 编码规则一致（控制面不 import `mcpsrv`；在 `agent_prompt.go` 写同样规则的小函数并加注释指向 resources.go，测试断言前缀）。
- 提示词由 `i18n.T(LangFrom(r), key, args...)` 拼接：`agent.prompt.intro`（"请处理 CloudFS 挂载中的 %s（%s）"）、`agent.prompt.read`（"用 read_text 读取，必要时 edit_file 修改"）、`Collector.Index != nil` 时加 `agent.prompt.extracted`（"PDF/Office 文档可用 read_extracted_text 读取抽取文本"）、`Collector.Agent != nil` 时加 `agent.prompt.session`（"开始前调用 begin_session，结束时 finish_session 并写 summary"）、目录时加 `agent.prompt.dir`（"这是一个目录，先 list_directory"）；`heading` 查询参数非空时加 `agent.prompt.heading`。
- `send_to_agent.js`：
```js
import { api } from '/ui/api.js';
import { el, openPanel, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
export async function openSendToAgent({ path, heading }) {
  let r;
  try { r = await api.get('/agent/prompt?path=' + encodeURIComponent(path) + (heading ? '&heading=' + encodeURIComponent(heading) : '')); }
  catch (err) { toast(err.message, 'bad'); return; }
  const textarea = el('textarea', { rows: '12', style: 'width:100%' });
  textarea.value = r.prompt;
  const copy = el('button', { class: 'primary', onclick: async () => {
    try { await navigator.clipboard.writeText(textarea.value); toast(t('agent.prompt.copied')); }
    catch (_) { toast(t('agent.prompt.copyfailed'), 'bad'); }
  } }, t('agent.prompt.copy'));
  openPanel({ title: t('agent.prompt.title'), content: el('div', {}, el('p', { class: 'detail' }, t('agent.prompt.body')), textarea), footer: copy });
}
```
- `main.js` 检查器按钮行：`el('button', { onclick: () => openSendToAgent({ path: e.path }) }, iconEl('bot'), t('action.sendtoagent'))`；`content_search.js` 结果行末列加 `el('button', { onclick: (ev) => { ev.stopPropagation(); openSendToAgent({ path: hit.path, heading: hit.heading }); } }, iconEl('bot'))`（带 `'aria-label': t('action.sendtoagent')`）。
- i18n（web，zh / en）：`action.sendtoagent` 发送给 Agent / Send to agent；`agent.prompt.title` 发送给 Agent / Send to agent；`agent.prompt.body` 复制下面的提示词，粘贴到 Claude Code 或 Codex。可以先修改。 / Copy this prompt into Claude Code or Codex. You can edit it first.；`agent.prompt.copy` 复制提示词 / Copy prompt；`agent.prompt.copied` 已复制 / Copied；`agent.prompt.copyfailed` 无法访问剪贴板 / Could not reach the clipboard。

- [ ] **Step 4: 运行确认通过**

Run: `node --test internal/control/web/_tests/*.test.mjs && ./gow test -race ./internal/control/ -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/control internal/i18n
git commit -m "feat(ui): copy an MCP-ready prompt for a file from the inspector and search results"
```

---

# 一期总验证（两条线都合入 `feat/agent-phase1` 之后）

```bash
./gow build ./...
./gow vet ./...
./gow test $(./gow list ./... | grep -v -e internal/fusefs -e test/conformance -e test/e2e)
./gow test -race ./internal/agent/ ./internal/textract/ ./internal/index/ ./internal/mcpsrv/ ./internal/control/ ./internal/daemon/ ./internal/config/ ./internal/meta/ ./cmd/cloudfs/
node --test internal/control/web/_tests/*.test.mjs
./gow test ./test/perf/ -count=1
./gow test ./test/chaos/ -race -count=1
CLOUDFS_BROWSER=1 ./gow test ./test/e2e/ -run 'TestAgent|TestContentSearchInTheBrowser' -v -count=1
./gow vet ./internal/fusefs/ ./test/conformance/ ./test/e2e/   # must compile even where they cannot run
```
合入后必做的交叉检查：
- A2 `TestEveryToolChecksItsPaths` 增加带 `Index` 的子测试并登记 5 个索引工具（见 B9）。
- `semantic_search` 的 roots 改用 `s.readRoots(ctx, root)`，确认令牌作用域（A6）下 `/private` 命中不出现：在 `internal/mcpsrv/index_tools_test.go` 加 `TestSemanticSearchRespectsTokenScope`（`newAgentEnv` + Index，Scope `Read: ["/work"]`）。
- `cmd/cloudfs/main.go` 两处 `mcpsrv.Options` 同时带 `Sessions`、`Workspace`、`NonOwner`（仅 stdio）、`Index`。
- 控制台导航 10 项顺序与 `docs/ui-plan.md` 阶段 F 一致；`#/agents` 徽标与 `#/index` 图标可见（`CLOUDFS_BROWSER=1` 冒烟截取 `nav` DOM 断言两者都在）。

---

# 验收映射（TODO 验收行 → 任务 / 测试）

| 条目 | 验收行（摘要） | 任务 | 证明 |
|---|---|---|---|
| T-34 | `--allow /work` 不启用 sessions 时现有 mcpsrv 测试零修改 | A2 | Step 6 `git diff --stat` + 全包测试 |
| T-34 | `Scope.Narrow` 表驱动覆盖父/子/兄弟/根/`..`/尾斜杠，放大必失败 | A1 | `TestScopeCheck`、`TestScopeNarrowNeverWidens` |
| T-34 | 被拒 `delete` 在 `/audit` 有 `result=denied`、`paths` 正确、`args` 无 content/http | A3、A4 | `TestDeniedDeleteIsAuditedWithoutContent`、`TestAuditRouteFollowsCursorAndFilters` |
| T-34 | `write_file` 200 KiB，`args` ≤ 4 KiB，`bytes_in=204800` | A3 | `TestLargeWriteAuditArgsStayBounded`、`TestRedactArgsKeepsLargeContentOutAndBounded` |
| T-34 | agent.db 写失败时工具照常成功，metrics +1 | A3、A4 | `TestAuditFailureDoesNotFailTheTool`、`TestAuditWriteFailureIsCountedNotReturnedToTheTool`、`TestAuditWriteFailuresMetric` |
| T-34 | 新路由进守卫测试；`POST /sessions/{id}/finish` 无头 403 | A4 | `TestEveryRouteIsEitherGuardedOrArguedOpen`、`TestFinishSessionWithoutControlHeaderIs403` |
| T-34 | 界面：`agents.js` 嵌入、路由与导航、`/sessions`、`/audit` 跟随 `next_cursor`、denied 有文字、`scope_view.test.mjs`、i18n | A5 | `ui_agents_test.go` 全部、`scope_view.test.mjs`、`TestWebCatalogsHaveTheSameKeys`、`TestWebScreensHoldNoUntranslatedText` |
| T-34 | e2e：越界写 → 浏览器 `#/agents` 审计可见 denied 行 | A11 | `TestAgentTokenSandboxChainAndAuditInTheBrowser` |
| T-35 | token A/B 并发，`stat` 与 `resources/subscribe` 互斥；遍历全部工具证明过 `checkPath` | A6、A2 | `TestTwoTokensSeeDisjointTrees`、`TestEveryToolChecksItsPaths` |
| T-35 | 过期令牌 `initialize` 401；吊销 5 s 内 legacy 会话关闭 | A6 | `TestExpiredTokenInitializeIs401`、`TestRevokeClosesLegacySessionWithin5s` |
| T-35 | `mcp install --transport http --client claude` 能被 `claude mcp add` 接受（快照） | A7、A11 | `TestClaudeHTTPSnippetGolden`、`TestClaudeAddCommandGolden`；A11 Step 4 真机 |
| T-35 | `POST /mcp/tokens` `no-store`；`GET /mcp/tokens` 无完整令牌 | A7 | `TestCreateTokenResponseIsNoStore`【决策点】、`TestTokensListNeverCarriesAPlainToken`、`TestMCPConnectCarriesNoToken` |
| T-35 | 界面：揭示浮层不 import store、无 localStorage、吊销 `confirm: true`、接入面板调 `/mcp/connect`、setup 完成页链 `#/agents`、无 password | A8 | `ui_tokens_test.go` 全部、`TestWebAppNeverAsksForACredential` |
| T-36 | 两并发会话同名文件，manifest 完整互不引用 | A9 | `TestConcurrentSessionsKeepSeparateManifests` |
| T-36 | sandbox 写 `/work/notes.md` denied 且审计 denied；会话目录写成功；读成功 | A9 | `TestSandboxSessionCannotWriteOutsideItsDirectory` |
| T-36 | `share=true` 且 local：`state=local` 无链接，≤ 1 s，0 次链接调用 | A9 | `TestShareSkipsLocalFilesWithoutALinkCall`（`Calls("DownloadURL") == 0`） |
| T-36 | allow 空且未配 workspace：配置错误、不建目录 | A9 | `TestBeginSessionWithoutWorkspaceIsAConfigError` |
| T-36 | 界面：复制链接只在点击时请求且不入 DOM；检查器调 `/sessions?path=`；工作区标记 `bot` + `aria-label` | A10 | `ui_sessions_test.go` 全部、`workspace_view.test.mjs` |
| T-36 | e2e：真实挂载 manifest 与 `ls` 一致；浏览器会话详情两行产物 | A11 | `TestAgentSessionManifestMatchesTheMount`（Linux FUSE）、`TestAgentTokenSandboxChainAndAuditInTheBrowser` |
| T-37 | `index.enabled: false`：无 5 个工具、无 index.db、`/index/status` `enabled:false` | B9、B10 | `TestIndexToolsAbsentWhenDisabled`、`TestIndexDisabledCreatesNoIndexDB`、`TestIndexStatusWhenDisabled` |
| T-37 | pinned 500 文件 provider 读增量 0 | B7 | `TestIndexPinnedFilesCostNoReads`（`Calls("ReadRange")`） |
| T-37 | rules 100 文件：首轮 100、次轮 0、改 1 个版本后 1 | B7 | `TestIndexRulesFetchOncePerVersion` |
| T-37 | docx `Heading1` 进 `heading`；`.md` hit `start_off` → `read_text` 同前缀 | B2、B9 | `TestDocxHeading1BecomesAMarkdownHeading`、`TestDocxHeadingIsReturnedWithTheHit`、`TestMarkdownHitOffsetFeedsReadText` |
| T-37 | 目录改名后 hit 路径即新路径、`indexed_at` 不变 | B5、B7 | `TestRenamePrefixKeepsIndexedAt`、`TestRenameKeepsIndexedAtAndUpdatesThePath` |
| T-37 | `--allow /work` 时 `/private` chunk 永不出现 | B8、B9 | `TestSearchNeverLeaksOutsideRoots`、`TestSemanticSearchRespectsAllow`（合入后 `TestSemanticSearchRespectsTokenScope`） |
| T-37 | 2 字中文查询返回结果或 `truncated` | B8 | `TestTwoRuneChineseQueryReturnsHitsOrTruncated`、`TestShortQueryScanStopsAtItsBudget` |
| T-37 | 恶意 zip → failed 不 panic；`kill -9` 后 index.db 完整、pending 续跑 | B2、B13 | `TestZipWithTooManyEntriesFails`、`TestMaliciousArchiveFailsTheDocumentNotTheDaemon`、`TestIndexSurvivesAnUncleanStopMidExtraction` |
| T-37 | `FS.Busy()` 持续为真时 worker 5 s 内至少让路一次 | B7 | `TestBusyForegroundMakesTheWorkerYield` |
| T-37 | 界面：`#/index` 路由导航、未启用不请求 rules、移除/重建 `confirm: true`、配置规则无移除；搜索切换只在 enabled、内容模式调 `/index/search` 并渲染 degraded/truncated；检查器三动作；`snippet.test.mjs` 高亮/CJK/转义 | B11、B12 | `ui_index_test.go`、`ui_content_search_test.go`、`index_presets.test.mjs`、`snippet.test.mjs` |
| T-37 | e2e：写 md 3 s 内命中；浏览器主窗口内容搜索命中 | B13 | `TestContentSearchFindsAFreshMarkdownFile`（Linux FUSE）、`TestContentSearchInTheBrowser` |
| T-42（前半） | 复制提示词无写操作；两个入口；不存在路径 404 不泄露内部路径 | B14 | `TestSendToAgentOnlyReadsAndCopies`、`TestSendToAgentHasBothEntrances`、`TestAgentPromptMissingPathIs404WithoutInternals` |

---

# 执行方式

计划可按两种方式执行：
1. **Subagent-Driven（推荐）**：每个任务派一个新的 subagent（superpowers:subagent-driven-development），任务之间做两段式评审；线 A 与线 B 各开一个 worktree 并行推进。
2. **Inline**：在单个会话中按 superpowers:executing-plans 分批执行，每批结束设检查点。
