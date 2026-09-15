# CloudFS 桌面界面 —— 开发方案与计划（2026-09-06）

对应设计稿：https://claude.ai/code/artifact/2c5d82b9-8ca9-4f8b-a4f7-02158cd95153（8 张画板：总览、主窗口、添加网盘、连接设置、代理出口、传输队列、缓存与固定、诊断与服务）。

## 当前状态（2026-09-08 复核）

下面的清单按代码逐条核对过一次，勾选状态已与仓库对齐（此前 72 项里有 71 项是做完但没打勾）。
标记含义：`[x]` 已完成并有测试；`[~]` 主体完成但留有缺口，缺口写在条目末尾；`[ ]` 未开始。
Context 一节是 2026-09-06 立项时的事实快照，保留作为决策依据，不再更新。

阶段 0–5、3a、4、A（除 A10 的浏览器冒烟）、B1–B5、C1、D1/D3–D7 已完成。
阶段 E（界面没接上的后端能力）已于 2026-09-08 全部完成。剩余的真实缺口只有两处：
**桌面壳的托盘与打包**（B6/B8）、**需要真实硬件或账号的验收**（C2-3/C2-4/C2-5，与 TODO.md 的
T-11/T-13/T-14/T-15 同源），另有三条小尾巴：`internal/pathsafe` 未抽（P1-1）、
无头浏览器冒烟未写（A10-3）、CI 缺 macOS 桌面壳构建（D2）。

## Context

CloudFS 现在所有操作都在 CLI 里，唯一的图形面是 `internal/control/web/index.html`（210 行、单文件、只读状态页 + 无凭据加账号）。设计稿要求**每一个 CLI 能做的操作都能在界面上完成**，并且**在 Linux / macOS / Windows 上风格一致**。

用户已定三件事：
1. 交付形态：**原生窗口壳 + 同一套 Web UI**（浏览器也能直接打开，Docker/NAS 用户照旧）。
2. 前端：**纯 Go 嵌入，不引入 Node**——`go build` 仍是唯一构建。
3. Windows：**本期同时启动 WinFsp 挂载适配**（T-21 提前）。本机没有 Windows，只能写、交叉编译、vet，不能跑。

核对代码后的关键事实（决定了方案的形状）：
- 控制面 HTTP（`internal/control`）已有 `/status /metrics /cache/* /uploads/* /copy /copies/* /accounts`，守卫是 `privateRequest`（Host 回环 + Origin 同源 + 非 GET 需 `X-CloudFS-Control: 1`）。**`/cache/drop` 漏了守卫**（可被表单 CSRF）。
- 界面需要但**没有 HTTP 端点**的：目录列举/stat/读/建目录/改名/删除（`vfs.FS` 有 API，`mcpsrv` 已做过路径→inode 翻译）、搜索、doctor、代理探测/解释、`provider.Caps`、账号详情/编辑/删除/校验。
- **完全没有 API** 的：删除远端、改 `remotes.<name>.proxy/qps/upload_workers`、编辑代理段、运行时加/卸挂载、配置热加载（全仓库无 SIGHUP/watcher，所有改动都写"重启守护进程"）、服务安装（`cmd/cloudfs/service.go` 在 package main）。
- 凭据边界：`POST /accounts` 拒绝任何 `config.IsSecretField` 键（`accounts.go:196`），凭据只走终端。**必须保持**。
- 现有 UI 的 CSP 是 `script-src 'unsafe-inline'`、只服务 `/`，`ui_test.go:62-66` 断言其它路径 404——拆成多文件要改这条契约（并顺手收紧 CSP 到 `'self'`）。
- **Windows 今天编不过**：`internal/fusefs` 只有 linux/darwin 平台文件且依赖 go-fuse（无 windows 构建）；`cmd/cloudfs/main.go:28` 无条件 import fusefs；`x/sys/unix` 的 `Flock` 用在 `journal/lock.go`、`config/secrets.go`、`config/edit.go`、`control/listeners.go`；`vfs.go:1035` 用 `syscall.Sync`。其它依赖（go-smb2、pkg/sftp、minio、modernc sqlite、go-keyring）都可移植。
- 发布只有 linux/darwin × amd64/arm64，`CGO_ENABLED=0`；CI 矩阵 ubuntu/macos。
- 顺带发现两个 bug：`vfs.attrOf` 从不设置 `Attr.Pinned`（字段存在但永远 false）；`proxy.Manager` 的 `router/outbounds/groups` 无锁读（今天没事是因为构造后不再写）。

## 总体架构决策

```
cmd/cloudfs          守护进程 + CLI，保持 CGO_ENABLED=0 静态二进制（Docker/NAS 的生命线）
  └ internal/control/web/   嵌入的 Web UI（多文件 ES modules，embed.FS）
  └ internal/control/*.go   扩展后的控制面 HTTP API（所有操作的唯一入口）
cmd/cloudfs-desktop  原生壳，独立二进制，cgo（webview_go），只是一个"指向回环 URL 的浏览器 + 托盘"
  └ 不内嵌第二份守护进程逻辑，不自己服务静态文件：起/连 cloudfs，打开它的控制面 URL
cmd/cloudfs (windows) 两个产物：静态版（无 mount，其余全能）；`-tags winfsp` cgo 版（internal/winfs）
```

**三平台风格一致靠什么保证**：同一份 HTML/CSS/JS 字节、同一个 `app.css` 令牌文件、显式字体栈 + `font-size-adjust` + `tabular-nums`、`appearance: none` 手绘所有表单控件、统一滚动条与 `:focus-visible`、`color-scheme: dark`。三个 WebView（WebKitGTK / WKWebView / WebView2）和任何常青浏览器渲染的是同一套东西；不一致只可能来自字体度量，见 A7。

**壳的选型（用户已确认）**：`webview/webview_go`（薄 cgo 绑定，无注入 JS 桥，无构建假设）而不是 Wails——Wails 的资产管线默认假设 Vite/npm，和"不引入 Node"相抵；托盘/单实例/自启需自己补，见 B。若后续打包需求膨胀，再评估 Wails v2。

**字体策略（用户已确认）**：先显式系统字体栈，零字节；三平台真机截图对比后差异不可接受再走 A7 的兜底路径（离线子集 woff2 签入仓库，不进构建链）。

**热加载**：只做代理段（`Manager.Reload`，因为 `ruleTransport` 每个请求重新解析出口，只需换 Manager 内部状态）。远端/挂载/其它一律走**优雅重启端点**，界面明确显示"需要重启，现在重启？"。不做逐子系统热更新——`FS.mounts`、`d.Providers`、journal flock、FUSE 生命周期都是在 `daemon.Open` 冻结的结构，逐个拆开风险大于收益。

**凭据**：浏览器与控制面 API **永远不承载任何秘密值**。守护进程自己跑 OAuth/扫码流程（它绑回调监听、它收 token、它调 `config.SaveCredentials...`），界面只拿到授权 URL 或二维码内容字符串。
- A 类（守护进程可驾驭）：aliyun、baidu（OAuth，`internal/auth.Authorize`）、pan115（扫码，`auth.Authorize115`，`Show` 回调给的是内容字符串，界面自己画二维码）。
- B 类（保持终端）：webdav/openlist/sftp/smb/tianyi 密码、quark cookie、pan123 client_secret、dropbox/gdrive/box 外部获得的 token——界面只把 `next_command` 显示出来。

---

## 工作流（按依赖顺序；S<1d M 1–3d L 3–7d XL 1–3w）

### 阶段 0 —— 地基与已知漏洞（S）

- [x] **P0-1** `/cache/drop` 加 `privateRequest`（`internal/control/metrics.go:80`）。回归：`cache_test.go` 守卫表加 cross-site 一行，修前失败。
- [x] **P0-2** 抽公共件：`writeJSON`（从 `accounts.go` 的 `writeAccountJSON` 提出，no-store/nosniff）、`requireConfirm(confirm bool, reason string)`、`rejectSecretFields(map[string]string)`（把 `buildAccount` 里的 `IsSecretField` 检查提出来，PATCH 与 POST 共用一处）。把 `safeFieldName` 移到 `config` 包导出，`control` 与新的 `config.SetRemoteField` 共用一个定义。
- [x] **P0-3** 修 `vfs.attrOf` 不设 `Pinned`（`internal/vfs/vfs.go:384`，用 `pathPinned` 查）。回归 `TestAttrOfReportsPinned`：写文件→Pin→列父目录→断言 Pinned，修前失败。
- [x] **P0-4** 全路由守卫测试 `internal/control/security_all_routes_test.go`：枚举 mux 上每条路由，非 GET 无头 → 403，跨源 → 403。这是防止下一个人再漏一条的闸门。
- [x] **P0-5** 决定并写下注释：`/status /metrics /healthz /readyz` 保持无守卫（只读、无秘密、回环进程本就能读 /proc）。

### 阶段 1 —— 只读 + 低风险写：`/fs/*` `/search` `/doctor` `/events`（M/L）→ 解锁主窗口、详情、诊断

新文件 `internal/control/fs.go`、`search.go`、`events.go`；`doctor.go` 加路由。

- [~] **P1-1** `canonicalPath`：与 `mcpsrv.checkPath`（`server.go:139`）同一套规则。建议抽成 `internal/pathsafe` 让两边共用；至少共用同一张测试向量表。
- [x] **P1-2** `GET /fs/list?path&cursor&limit&count` → `FS.ReadDirPagePath`，游标编码**照抄** `internal/mcpsrv/directory_page.go`。响应 `FSEntry{name,is_dir,size,mtime,cached,pinned,local_only}`。
- [x] **P1-3** `GET /fs/stat`、`GET /fs/preview?path&offset&length`（硬上限 1 MiB，`application/octet-stream` + `Content-Range`）、`GET /fs/download-url`（**唯一允许返回签名 URL 的端点**，no-store、不落日志，注释写明是有意例外）。
- [x] **P1-4** `POST /fs/mkdir`（单层，不做 mkdir -p）、`POST /fs/rename`、`POST /fs/delete`（`confirm` 必填，拒绝 `/`）——路径→inode 翻译照抄 `mcpsrv.mkdirAll/move/deletePath`（`server.go:783-905`）。
- [x] **P1-5** `GET /search?q&path&limit` → `FS.Meta().SearchReport`，响应带 `complete`，界面显示"可能还有更多"。
- [x] **P1-6** `POST /doctor/run`（有轻微写：探针文件、vacuum，所以是 POST 但不需 confirm）、`POST /doctor/fix`（confirm 必填）。`Collector` 加 `Doctor *control.Doctor`，`Daemon.Collector()` 用 `d.Doctor(fuseSupported)` 装配（现在只有 `cmdDoctor` 装）。
- [x] **P1-7** `GET /events` SSE：`FS.WatchChanges()` 的变更事件 + 每 2s 一次 `Collector.Collect` 作为 `status` 事件。测试模式照 `internal/mcpsrv/subscriptions_http_test.go`。先看 `changes.go` 有没有订阅者上限，参考 `mcpsrv/subscriptions.go` 的慢客户端隔离。仪表类数据允许 `/status` 轮询兜底，目录变更必须走 SSE。
- [x] **P1-8** 测试：`fs_test.go` 用 `cacheControl(t)`（`cache_test.go:13`，真 vfs + fakeprovider）派生 `fsControl(t)`；每条路由 × {无头, 跨源, 非回环 Host} → 403；穿越向量（`..`、NUL、空）→ 400；`test/e2e` 加 `TestUIAPIEndToEnd`：mkdir→list→rename→delete 经控制面，断言**挂载点**上可见。

### 阶段 2 —— 配置变更库（M，`internal/config/edit.go`）

全部走现有 `editConfig`（flock + yaml.Node 手术 + 重新 Parse/Validate + 原子改名，保留注释）。

- [x] **P2-1** `RemoveRemote(path, name)`：有挂载布局引用时拒绝。
- [x] **P2-2** `SetRemoteField(path, name, SetRemoteFieldOptions{Fields map[string]*string, Proxy *string, QPS *QPS, UploadWorkers *int})`：`Fields` 每个键过 `IsSecretField` 与 `SafeExtraFieldName`；`Proxy` 必须是已有出口/组名或空。
- [x] **P2-3** `SetProxy(path, Proxy)`：整段替换（规则引用组引用出口，局部 PATCH 会做出"各自合法但不是本意"的状态）；先在内存构造 Config 跑 `Validate` 给更好的报错。
- [x] **P2-4** `AddMount/RemoveMount/SetLayout`：把 `AddRemote` 里的挂载创建分支重构为 `upsertMountLayout` 让两边共用。
- [x] **P2-5** 测试：每个函数 happy path、拒秘密键、拒不存在的代理目标、**注释存活**（`# 我的备注` 经 `SetRemoteField` 后仍在——这正是 node 手术而非整文件重编的意义）。

### 阶段 3 —— 账号 / 代理 / 挂载端点（L）→ 解锁连接设置、代理出口、添加网盘

- [x] **P3-1** `Collector` 加 `Providers map[string]provider.Provider`、`CheckAccount func`、`ReloadProxy func`（`daemon.go:330` 装配）。
- [x] **P3-2** `GET /accounts/{name}`：`AccountDetail{name,type,proxy,qps,upload_workers,fields(剔除秘密键——legacy 内联秘密可能还在 Extra 里，必须过滤而不是假设没有),caps,has_credentials(bool，永不带值)}`。`caps` 来自 `Providers[name].Capabilities()`。
- [x] **P3-3** `PATCH /accounts/{name}` → `rejectSecretFields` → `SetRemoteField`；`DELETE /accounts/{name}?confirm=true` → `RemoveRemote`，响应 `restart_required: true`；`POST /accounts/{name}/check` → `daemon.CheckAccount`，错误经现有脱敏（`checkConfiguredAccount` 那套，provider 错误里有签名 URL/cookie）。
- [x] **P3-4** `internal/control/proxy.go`：`GET /proxy/explain?host`（`Router().Explain` + `OutboundFor`）、`POST /proxy/check`（`CheckNow`）、`GET /proxy/config`（**先读 `Outbound.URL()` 确认是否支持 `user:pass@host`，支持则脱掉 userinfo**）、`PUT /proxy/config` → `SetProxy`；有 P3a 前响应 `restart_required: true`。
- [x] **P3-5** `internal/control/mounts.go`：`GET/POST /mounts`、`DELETE /mounts/{path}?prefix&confirm` —— **本期只改配置**，永远 `restart_required: true`。运行时加卸挂载明确推后（结构性冻结，不是缺管道）。
- [x] **P3-6** 测试：`accounts_test.go` 扩 PATCH/DELETE/check，新 `proxy_test.go`、`mounts_test.go`，守卫表 + 秘密泄漏断言（`json.Marshal` 响应扫秘密键名与 token 形状）。

### 阶段 3a —— 代理热加载（M，可晚于 3 一个版本）

- [x] **P3a-1** `internal/net/proxy/dialer.go` 加 `(*Manager).Reload(ManagerOptions) error`：把 `NewManager` 里出口/组构造抽成 `buildOutboundsAndGroups`；`Reload` 换 `router/outbounds/groups`，清掉已不存在的组的 `selected`。**顺带修**：`Resolve/OutboundFor/checkGroup/ruleTransport.RoundTrip` 读这三个字段全部改到 `m.mu.RLock()` 下（现在是无锁读）。
- [x] **P3a-2** `daemon.buildProxy` 的转换抽成 `toManagerOptions(cfg.Proxy)`，冷启动与热加载共用；`Collector.ReloadProxy` 接上。`PUT /proxy/config` = `SetProxy`（落盘）+ `ReloadProxy`（生效）；落盘成功而生效失败要回 `{"applied":false,"restart_required":true,"warning":...}`，绝不让调用方以为两件都成了。
- [x] **P3a-3** 回归 `TestReloadAppliesToInFlightRuleTransport`：两个假 HTTP 服务，Reload 后**下一个**请求到新目标；`-race` 下 Reload 与 OutboundFor/RoundTrip 并发循环。

### 阶段 4 —— 凭据流程（M/L，可与 3 并行）→ 解锁添加网盘的授权步骤

- [x] **P4-1** 把 `cmd/cloudfs/config_manage.go:432-461, 487-541` 的核心抽到 `internal/daemon/auth.go`：`StartOAuthFlow(ctx,cfg,name) (url, wait, err)`、`StartDevice115Flow(ctx,cfg,name) (qrContent, scanned, wait, err)`。不打印、不开浏览器；CLI 与控制面都调它（vfs/fusefs/mcpsrv 同一种"单一实现、薄适配"形状）。
- [x] **P4-2** `internal/control/auth.go`：`POST /accounts/{name}/auth/start` → `{kind:"url"|"qr", value, session}`；`GET .../auth/status?session` → `pending|scanned|done|denied|error`；`POST .../auth/cancel`。内存会话表，session id 加密随机，同一账号同时只允许一个流程（第二个 → 409）。完成时守护进程侧调 `config.SaveCredentialsForRemote`，**响应里永不出现 fields**。
- [x] **P4-3** 测试：`internal/daemon/auth_test.go` 假 OAuth/假 115 服务（照 `internal/auth/*_test.go`）；`internal/control/auth_test.go` 断言响应体不含 token 形状；双飞断言（第二个 start 冲突）。

### 阶段 5 —— 生命周期：重启与服务管理（M/L，最后做，爆炸半径最大）

- [x] **P5-1** `POST /daemon/restart`（confirm）：置 draining 原子标志（`privateRequest` 对非 GET 回 503）→ `Uploader.Flush` 限时 30s → 有 FUSE 则 `Mount.Unmount()` → `Daemon.Close()`（释放 journal flock）→ 写完 200 后关自己的监听 → 若在 systemd/launchd 下运行则 `exit(0)` 交给服务管理器拉起（检查生成的 unit 有没有 `Restart=on-success`，没有就加），否则 `syscall.Exec` 自重启。响应带 `mode: "reexec"|"service_managed_exit"`。**两个 owner 绝不能共存**是这条的唯一原则。
- [x] **P5-2** `cmd/cloudfs/service.go` 逻辑迁到 `internal/service`，CLI 变薄包装。守护进程**可以** `Status`；`Install` 加 flock 且已加载时回 409；`Uninstall` 先读完现有卸载分支（stop unit → unmount → 删文件的顺序）再接——服务管理器在挂载被拔掉后立刻拉起进程是要避免的状态。
- [x] **P5-3** `/service/status`（GET）、`/service/install`、`/service/uninstall`（POST + confirm）。
- [x] **P5-4** 测试迁到 `internal/service/service_test.go`，加"已安装再 install → 409"、卸载调用顺序断言。

### 阶段 A —— 前端（无 Node）

- [x] **A1** 目录：`internal/control/web/{index.html, app.css, app.js, router.js, api.js, store.js, icons.js, i18n.js, components/*.js}`。原生 ES modules + `customElements`，浏览器自己解析 import 图。
- [x] **A2** `ui.go` 改 `//go:embed web` + 显式白名单静态处理器（启动时 walk 进 map，**不用** `http.FileServer` 直出，保留"精确路径否则 404"契约）；CSP 收紧为 `script-src 'self'; style-src 'self'`（去掉 `unsafe-inline`——这是变严不是变松）；`Cache-Control: no-cache` + 启动时算一次 SHA-256 的 `ETag`（二进制整体替换，不需要 `?v=` 打戳机制）。**重写** `ui_test.go:62-66` 的 404 断言而不是绕过。
- [x] **A3** hash 路由（`#/connections` 默认、`#/proxy`、`#/transfers`、`#/storage`、`#/diagnostics`；`#/add-drive`、`#/connect/:remote` 是覆盖在主窗口上的叠层，不是整页切换）。选 hash 而非 history：服务端零改动，且 `ui_test` 的契约不变。
- [x] **A4** `api.js`：统一 `request()`，非 GET 自动带 `X-CloudFS-Control: 1`；非 2xx 抛 `ApiError{status,message}` 由 `toast-host` 统一展示；SSE 订阅 + 指数退避重连（上限 30s）+ 打不开时退回 5s 轮询（今天 `setInterval(refresh, 5000)` 的推广）。
- [x] **A5** `store.js` ~30 行 pub/sub；组件在 `connectedCallback/disconnectedCallback` 订阅/退订。
- [x] **A6** `i18n.js`：所有界面字串走 `t('files.column.size')`；纪律是"组件模板里不写死中文"，由 `internal/control/ui_i18n_test.go` 断言（screens 里出现汉字即失败）。zh-CN 与 en 两张表，键集合必须一致。语言协商在客户端：localStorage 的选择优先，其次浏览器语言；选定的语言随每个请求的 `?lang=` 送到守护进程，由 `internal/i18n` 渲染它自己那部分文案（诊断、字段提示、状态告警、控制 API 错误）。
- [x] **A7** **三平台一致性**（`app.css`）：
  - 字体：`-apple-system, "Segoe UI", "PingFang SC", "Microsoft YaHei", "Noto Sans CJK SC", "Source Han Sans SC", system-ui, sans-serif` 显式列出而不是信任 `system-ui` 的解析；`font-size-adjust` 拉平 x-height；数字列 `font-variant-numeric: tabular-nums`。**不捆绑 CJK 字体**：可用子集要 fonttools（Node/Python 工具链，与约束冲突）或整包 >10 MB（与拒绝 rclone 的理由同构）。在 CSS 里写明这是"零字节 vs 几像素行高差"的有意取舍。
  - 兜底路径（真机截图后如差异不可接受）：把 `i18n.js` 里**实际出现的**汉字机器统计出来（8 屏文案大概几百个码点），用一次性 `fonttools` 命令离线做子集 `.woff2` **提交进仓库**（和 `web/*` 一样是签入源码不是构建产物），`go build` 依然无 Node。
  - 控件：`select, input, button { appearance: none }` 全部按令牌手绘（WebView2 的原生 radio 和 WebKitGTK 的长得完全不一样）；`scrollbar-color` + `::-webkit-scrollbar`；`:root { color-scheme: dark }`；统一 `:focus-visible { outline: 2px solid #6f94bd }`；`-webkit-font-smoothing: antialiased`。
- [x] **A8** 可访问性：`confirm-sheet` 焦点陷阱 + Esc + 焦点归还；`data-table` 方向键行导航；保留原生 `<table>` 语义；对话框 `role="dialog" aria-modal`；每个彩色状态点旁必有文字。
- [x] **A9** 屏幕组件（按设计稿）：先主窗口（`app-shell`/`connection-list`/`file-table`/`inspector-panel`，它锻炼 `data-table`/`confirm-sheet` 两个共用件），再 `add-drive-modal`/`connect-wizard`（叠层），再 `proxy-view`/`transfers-view`/`storage-view`/`doctor-view`。传输队列的"永久丢弃本地版本"用**输入上传 ID**才启用按钮（沿用现有 `window.prompt` 的语义，做成 sheet）。
- [~] **A10** 测试三层：(1) 必做——扩 `ui_test.go`：每个嵌入资产的路径/Content-Type/CSP，未知路径 404，**全部字节里无 `type="password"`、无秘密字段名作为 input name/id**；(2) 不引入 JS 单测框架（那就是要避的 Node），把纯函数（字节格式化、store 合并、路由映射）抽成可导出函数，老实记为缺口；(3) `test/e2e` 加 `CLOUDFS_BROWSER=1` 门控的浏览器冒烟（`os/exec` 起系统 Chromium `--headless --remote-debugging-port`，几百行 Go 写最小 CDP 客户端，无浏览器时 `t.Skip`，与 FUSE 测试缺 `/dev/fuse` 的约定一致）。
- [x] **A11** `cloudfs ui`（或 `open`）命令：用 `control.FetchStatus` 的端点选择逻辑找活着的 URL 打印出来，桌面系统上 `xdg-open/open/rundll32 url.dll,FileProtocolHandler` 打开。不依赖壳，浏览器用户立刻受益。

### 阶段 B —— 桌面壳 `cmd/cloudfs-desktop`（cgo，独立产物）

- [x] **B1** (S) 可行性：`webview/webview_go` 在本机 GTK/WebKitGTK 下能编、能加载 `data:` URL。
- [x] **B2** (M) 骨架：建窗口 → `control.FetchStatus(ctx, socket, tcp)`（`listeners.go:120`，已实现"先 socket 后 TCP，拒绝/不存在视为离线"）→ 导航到回环 URL。
- [x] **B3** (M) 生命周期：不可达时 `os/exec` 起用户已装的 `cloudfs mount`（照 `service.go` 的 `rt.run` 形状）——**壳里绝不内嵌第二份装配逻辑**；未运行/重启中显示本地 `data:` 占位页 + 重试；SSE 断 + FetchStatus 失败 → 同一占位层。
- [x] **B4** (依赖后端) 新增控制面端点"给我一个 UI 用的回环 TCP 地址"（默认配置 `control.metrics` 为空、UI 只在 unix socket 上）——**跨半边的依赖，需后端配合**；壳**不**自己服务 `embed.FS`（那是第二份 CSP/静态服务实现）。
- [x] **B5** (M) 单实例：`listeners.go:61-67` 同款 flock；已在运行则调新增 `POST /focus` 让现有实例前置窗口。
- [ ] **B6** (M，可选并行) 托盘：`getlantern/systray` 之类（Linux 上又一个 cgo/AppIndicator 依赖，无托盘发行版要安全无操作）；菜单：打开窗口 / 重启守护进程 / 退出；三色语义与 Web 令牌一致。
- [~] **B7** (S/M) 开机自启：不做两套——诊断页那个开关就是 `cloudfs service install`（守护进程自启）；壳自身的登录项只是启动 `cloudfs-desktop`，它自己会附着或拉起守护进程。
- [ ] **B8** (L) 打包：Linux `.desktop` + 图标；macOS `.app`（先不签名，签名/公证单列）；Windows 安装器 + WebView2 运行时检测/引导。**macOS/Windows 部分本机无法验证**。
- [~] CI：Linux job 装 `libgtk-3-dev libwebkit2gtk-4.1-dev` 编 `cmd/cloudfs-desktop`；macOS runner 也编一份。

### 阶段 C —— Windows

**C1 仅编译（先交付，CI 可验证，产出静态 `cloudfs.exe`：无 mount，其余全能）**

- [x] **C1-1** (S) `internal/journal/lock.go` 拆 `lock_unix.go` / `lock_windows.go`（`windows.LockFileEx` + `LOCKFILE_FAIL_IMMEDIATELY` 对应 `LOCK_EX|LOCK_NB`）。
- [x] **C1-2** (M) 抽 `internal/config/filelock_{unix,windows}.go` 一对"打开并锁侧车文件"原语，`secrets.go:216`、`edit.go:35`、`control/listeners.go:61-66` 三处共用（Windows 无 `O_NOFOLLOW`，NTFS 符号链接竞争是另一个更弱的威胁模型，注释里承认）。
- [x] **C1-3** (M) `cmd/cloudfs/config_manage.go:306-321` 的 `unix.Poll/Read`：重构为 goroutine 读 stdin + channel select，去掉裸 Poll，直接变可移植。**先读完那 40 行再定形状**。
- [x] **C1-4** (S) `vfs.go:1035` `syscall.Sync` → `_windows.go` 无操作 + 注释（只是基准用的"让磁盘安静"，`FlushFileBuffers` 要卷句柄，不值当）。
- [x] **C1-5** (S) `internal/fusefs/platform_windows.go` 桩：`checkPlatform` 返回 `(false, "FUSE mounting needs the WinFsp build")`，`PassthroughAvailable` 返回不适用；`MountFS` 在 windows 上返回清晰的"此构建不支持"。`main.go` 无条件调用照常编过。
- [x] **C1-6** (S) `cache/{identity,statfs}_other.go`、`cmd/cloudfs/{service_mount,unmount}_other.go`、`service.go` 的 `default:` 分支——**已是正确的 Windows 行为**，只需确认 `!linux && !darwin` 标签仍选中。
- [x] **C1-7** (S) CI：ubuntu job 加 `GOOS=windows go vet ./...` + `GOOS=windows CGO_ENABLED=0 go build ./cmd/cloudfs`（交叉编译，无需 Windows runner）；`release.sh` 加 `build_one windows amd64`，输出名加 `.exe`。

**C2 WinFsp 适配（XL，写得出、本机验不了；`-tags winfsp`，默认关）**

- [x] **C2-1** 新包 `internal/winfs`（不在 `fusefs` 里用 build tag 分叉——那会让每个文件都要一份 Windows 变体）：与 `fusefs` 同一种薄适配，目标是 `github.com/winfsp/cgofuse` 的回调形状。`checkPlatform` 探 WinFsp 注册表键 / `%ProgramFiles%\WinFsp\bin\winfsp-x64.dll`，与 `platform_darwin.go` 探 `macfuse.fs` 同款。
- [x] **C2-2** 两个 Windows 产物：`cloudfs_<ver>_windows_amd64.exe`（C1 静态）与 `..._windows_amd64_mount.exe`（`CGO_ENABLED=1 -tags winfsp`）。后者需要 cgo 交叉工具链（`zig cc` 作 CC）或真实 Windows builder——**不要假设它像静态版那样能从 Linux 直接交叉**。
- [~] **C2-3** 不变量映射：提交在 FLUSH（cgofuse 有 `Flush(path, fh)`，但 WinFsp 的 CLEANUP/CLOSE 派发频率**必须真机验证**，CLAUDE.md 里"FLUSH 每个 fd 触发多次"的假设不能照搬）；`durable(ctx)` 与 `writeState.lastUploadID` 在 vfs 侧，适配层照调即可；inode 身份——cgofuse 偏路径，需要 path↔ino 表（**最大未知**，先看 `vfs.FS` 的方法是否以 ino 为键）；readdir 用 `FillFunc` 填 stat；xattr 报不支持；`EROFS` 由 cgofuse 转 NTSTATUS，只需冒烟。
- [~] **C2-4** **两个跨越适配层/核心边界的产品决策，需签字不能默默选**：(a) 大小写——`meta` 目录树按 POSIX 大小写敏感，NTFS/WinFsp 默认不敏感但保留大小写，只差大小写的两个远端文件会碰撞（WinFsp 新版支持按卷开大小写敏感，或在 vfs 边界折叠）；(b) 保留名与非法字符（`CON/PRN/NUL/COM1…`、`<>:"|?*`、尾部点/空格）——远端命名空间没这些限制，要么呈现时转义、要么拒绝呈现并明确报错。
- [ ] **C2-5** 真机验收清单（有 Windows 时按序跑）：① `-tags winfsp` 真能链接；② 装/卸 WinFsp 各一次，`doctor` 探测两态都对；③ `fake` remote 挂载，`cmd`/PowerShell 下 `dir/type/copy`；④ `test/conformance` 能编则跑，有意差异写成显式例外；⑤ **`winfsp-tests`**（WinFsp 上游一致性套件，Windows 上 pjdfstest 的对应物）；⑥ 并发写/上传中改名手工 chaos；⑦ Defender 实时保护开着做大文件读写；⑧ 壳在干净 Win10 21H2 / Win11 上 WebView2 加载。**fsx-via-WSL 不算验证**（那是另一套文件系统栈）。

### 阶段 D —— CI / 发布 / 文档

- [x] **D1** `ci.yml`：windows 交叉 vet+build（C1-7）；`-tags winfsp` 单独 job、仅 tag 或手动触发。
- [~] **D2** `ci.yml`：桌面壳 job（Linux 装 GTK/WebKitGTK dev 包；macOS runner 顺带编）。
- [x] **D3** `release.sh`：windows amd64 静态；C2 后加 `_mount.exe`。
- [x] **D4** `docs/distribution.md`：Windows 静态版能力/限制（照 MCP-only 容器那节的口吻"能力缩减、明说"）；C2 后写两产物拆分与 WinFsp 驱动前置。
- [x] **D5** README：`cloudfs ui` 一行；界面章节。
- [x] **D6** `TODO.md`：T-17 → 完整 UI；T-21 翻状态并写清"仅编译 / WinFsp 适配"两段与静态/cgo 双产物策略；新登记 T-24（UI 控制面 API）、T-25（桌面壳）。
- [x] **D7** `docs/DESIGN.md` §4.8 补控制面新端点契约与 SSE；§3.8 CLI 表补 `ui`。

---

### 阶段 E —— 界面没接上的后端能力（2026-09-08 新登记，当日全部完成）

控制面 API 已经能做这些事，界面没有入口。这一段不需要任何新端点，也不需要外部资源。
E1–E8 已全部完成，每条的落地说明见本节末尾。

- [x] **E1** 挂载管理：`POST /mounts`、`PATCH /mounts`、`DELETE /mounts?path&prefix&confirm=true`。
  没有它，界面里删连接会停在"先解除挂载"这一步而无处可去（`RemoveRemote` 有布局引用时拒绝），
  只能 curl 或手改配置文件。挂载编辑改的是配置，响应恒为 `restart_required: true`，界面要说清这一点。
- [x] **E2** 连接设置屏（设计稿第 4 张）：`GET /accounts/{name}` 详情（type、proxy、qps、upload_workers、
  非秘密 fields、caps、has_credentials）、`PATCH /accounts/{name}` 编辑、`POST /accounts/{name}/check` 测连通性。
  侧栏的 `selectRemote(r)` 现在忽略参数只把 `cwd` 归零（`web/screens/main.js`），点任何连接效果相同。
  `action.check`（"测试连通性"）的译文早就在 `i18n.js` 里，没有按钮用它。
- [x] **E3** 代理编辑：`PUT /proxy/config` 从未被调用，`web/screens/proxy.js` 只读。该文件头注释
  写着"改动会保存并生效"，与实现不符——**要么接上要么改注释，不能留着骗人**。`GET /proxy/explain?host=`
  同样没有入口（这是"为什么这个域名走了这条线"唯一的自解释途径）。
- [x] **E4** 分页：`/fs/list`、`/uploads`、`/search` 三处都返回 `next_cursor`，界面全部丢弃——
  目录封顶 500 项、队列封顶 100 行、搜索截断只弹一条 toast。游标编码已经在服务端做好了。
- [x] **E5** 文件动作：`POST /fs/rename`、`GET /fs/preview`、`GET /fs/download-url`、`GET /fs/stat`。
  检查器现在只有固定/预热/删除三个动作。
- [x] **E6** 复制任务整块：`POST /copy`、`GET /copies`、`POST /copies/{retry,cancel,forget}`，界面无对应屏。
- [x] **E7** 批量与维护动作：`POST /uploads/flush`、`POST /uploads/retry {all:true}`、`POST /cache/drop`、
  `GET /cache/stats`；池的 `POST /pool/rebuild`、`POST /pool/join`、按路径 `POST /pool/scrub {path}`。
- [x] **E8** 收尾细节：添加网盘的弹窗关闭时不调 `POST /accounts/{name}/auth/cancel`，把授权会话晾在服务端；
  "完成"按钮只关窗，不刷新账号列表也不 check。`web/screens/storage.js` 里配置来源的固定项渲染成
  灰字提示却没有动作。

已完成（2026-09-08）：
- E3 代理编辑。`web/screens/proxy.js` 重写为可编辑：出口与分组各自增删改（`openForm` 弹窗），
  规则按行编辑（文件里就是这个形状），整段 `PUT /proxy/config`；回复区分 `applied`（热重载成功）
  与 `restart_required`，绝不混为一谈。新增域名解释框（`GET /proxy/explain?host=`）。
  **后端同步改动**：`ProxyOutbound` 增加 `has_credentials`（视图）与 `keep_credentials`（请求）。
  此前 `GET` 会把 `socks5://user:pw@host` 脱敏成 `socks5://host`，页面原样 PUT 回去就会把密码
  从配置里抹掉。现在脱敏形态原样回传会被 400 拒绝并提示用 `keep_credentials`。
  回归 `proxy_credentials_test.go`、`ui_proxy_edit_test.go`。
- E4 分页。目录列表与上传队列都跟随 `next_cursor`，表格末行是"加载更多"；搜索截断改为留在
  结果里的一行说明而不是会消失的 toast。回归 `ui_paging_test.go`。
- E5 文件动作。检查器新增重命名（`POST /fs/rename`）、预览（`GET /fs/preview`，二进制内容明说
  不做文本显示）、下载链接（`GET /fs/download-url`，签名链接按需复制而不是渲染进长期停留的页面）。
  回归 `ui_file_actions_test.go`。
- E6 复制任务屏 `web/screens/copies.js` + `#/copies` 路由与导航项：新建复制（`POST /copy`）、
  列表分页、重试/取消（`POST /copies/{retry,cancel}`）、丢弃记录（`POST /copies/forget`，键入
  任务 ID 确认并带 `confirm: true`）。回归 `ui_copies_test.go`。
- E7 批量与维护动作。队列屏加"立即冲刷"与"重试全部死信"（`all: true`）；缓存屏加"释放内核缓存"
  （`POST /cache/drop`）；池屏加重建索引（键入池名确认 + `confirm: true`）、按路径校验、加入已有池
  （`POST /pool/join`）。回归 `ui_maintenance_test.go`。
- E8 收尾。添加网盘弹窗关闭时调 `POST /accounts/{name}/auth/cancel` 释放服务端授权会话，完成后
  回调 `onDone` 刷新调用方列表；配置来源的固定项补上"去哪改、为什么界面删不掉"的说明。
  回归 `ui_loose_ends_test.go`。
- 顺带把三处各自实现的模态框（焦点陷阱、Esc、焦点归还）收敛为 `ui.js` 的 `openForm`/`showPanel`/
  `openPanel`，新屏不再复制这段。
- 连接删除。侧栏每行 hover 出删除按钮 → 读 `/mounts` 挡住仍被挂载的连接 → 键入名字确认 →
  `DELETE /accounts/{name}?confirm=true`。回归 `internal/control/ui_connections_test.go`。
- E1 + E2 连接设置浮层 `web/connection.js`（点侧栏任一连接打开）：代理出口、三档 QPS、上传并发、
  非秘密后端字段的编辑（`PATCH /accounts/{name}`，清空输入框发 JSON `null` 即删除该键）、
  `POST /accounts/{name}/check` 就地测连通性、按连接列出挂载布局并可绑定（`POST /mounts`）
  与解除（`DELETE /mounts?path&prefix&confirm=true`，键入前缀确认）、后端能力只读表。
  浮层不渲染任何凭据输入框。回归 `internal/control/ui_connection_settings_test.go`、
  `account_detail_test.go` 的 `TestAccountPatchWithANullFieldRemovesIt`。

## 关键复用（不要重写的东西）

| 需要 | 已有 |
|---|---|
| 路径→inode、mkdir/move/delete | `internal/mcpsrv/server.go:783-905` |
| 目录分页游标 | `internal/mcpsrv/directory_page.go:14-33` |
| 路径安全校验 | `mcpsrv.checkPath` `server.go:139` |
| 配置原子编辑 | `config.editConfig` `edit.go:21`（`setNode/removeNode/mappingValue/scalar`） |
| 守卫与守卫表测试 | `privateRequest` `uploads.go:163`；`uploads_test.go:122-152` |
| 真 vfs 测试夹具 | `cacheControl(t)` `cache_test.go:13` |
| SSE 测试模式 | `internal/mcpsrv/subscriptions_http_test.go` |
| OAuth / 扫码 | `internal/auth.Authorize`、`Authorize115`（都是回调式，本就无终端依赖） |
| 账号校验脱敏 | `checkConfiguredAccount` `config_manage.go:~465` |
| 端点发现 | `control.FetchStatus` `listeners.go:120` |
| 关闭顺序 | `Daemon.Close` `daemon.go:315-323` |

## 明确不在本期

- 运行时加/卸挂载（配置改动 + 重启端点覆盖需求）。
- 远端/挂载的热加载（只做代理）。
- B 类凭据（密码/cookie/外部 token）进界面——永远不做。
- macOS 签名/公证、Homebrew/NAS 包（等 T-16 许可证决策）。
- Windows 服务（Windows Service / 任务计划）——`service.go` 的 `default:` 分支保持"不支持"。

## 本机无法验证、必须写明的

- Windows 全部运行时行为（C2 整段、B8 的 Windows 部分、WebView2）。
- macOS 壳（WKWebView）、`.app` 打包。
- 三平台字体度量的真实差异（A7 的兜底路径是否要触发）。
- 桌面壳的 GTK 版本兼容（本机只有一个）。

## 验证

- 每一步：`./gow vet ./... && ./gow test ./... && ./gow test -race ./...`，`gofmt -l .` 为空；每个修复带"回退即失败"的回归。
- 阶段 0/1 结束：`test/e2e` 的 `TestUIAPIEndToEnd` 通过（控制面操作在挂载点可见）；`security_all_routes_test` 通过。
- 阶段 A 结束：`ui_test.go` 全绿（资产/CSP/无密码框）；手工：`cloudfs mount` 后 `cloudfs ui` 打开，8 屏可达，非法路径 404，无 `X-CloudFS-Control` 的 POST 403。
- 阶段 C1 结束：CI 里 `GOOS=windows` vet + build 绿；`release.sh` 产出 `.exe`。
- 阶段 B：Linux 真机打开窗口、附着已运行守护进程、杀掉守护进程后显示占位并自动拉起。
- 阶段 C2：只能到 vet/交叉编译；真机清单留给有 Windows 的那天。

---

### 阶段 F —— Agent 底座界面（2026-09-14 登记）

对应 [TODO.md](../TODO.md) 的 P4 节（T-34 ~ T-44），设计与安全边界见 [Agent 工作底座路线图](agent-roadmap.md)
§6。本段只列界面侧逐项清单；证据、后端做法与验收断言以 TODO.md 为准。**纪律**：F 条目与对应 T 条目
同期交付、同一验收，界面不落地不关 T 条目。F1–F4 与 F11 属一期（F11 前置于 F4-b，F9 的"复制提示词"可提前到一期末），
F5–F10 属二期。

共用约定（每条都适用，不再逐条重复）：新屏与浮层只用 `openForm`/`openPanel`/`showPanel`/`confirmDelete`，
不复制模态框逻辑；表格分页用 `moreRow` + `paged.js`；新文案进 `i18n.js` 两张表且 screens 内无汉字；
纯逻辑模块零 import，放 `web/` 根，测试放 `web/_tests/*.test.mjs`；来自文件或子进程的文本一律按文本插入；
破坏性动作 `confirmDelete` 键入确认 + 请求体 `confirm: true`。

**F1 —— `#/agents` 屏骨架 + 会话 / 审计标签（T-34 · 一期）**

- [x] **F1-1** `icons.js` 加 `bot`；`router.js` 的 `routes` 与 `navItems` 加 `#/agents`（位于 `#/exports` 之后），`app.js` 导航项徽标显示活动会话数。
- [x] **F1-2** `screens/agents.js`：标签容器（会话 / 审计 / 访问令牌 / 记忆，后两个由 F2、F7 填充）；标签选择仅存 URL hash 参数。
- [x] **F1-3** 会话标签：三张卡（活动会话、今日写操作、今日拒绝次数）；表格 客户端 / 作用域摘要 / 状态点+文字 / 开始时间 / 写操作数，调 `GET /sessions` 并跟随 `next_cursor`。
- [x] **F1-4** `session_panel.js`（`openPanel`）一期内容：作用域、客户端、审计尾巴 50 条、"结束会话"（`POST /sessions/{id}/finish`）。
- [x] **F1-5** 审计标签：过滤条（会话、工具、结果 ok/denied/error、时间 1h/24h/7d）；表格 时间 / 客户端 / 工具 / 路径（多路径折叠）/ 结果 / 字节 / 耗时；denied 行红底且带文字标签；`args` 点击展开只读 JSON。
- [x] **F1-6** `api.js` 的 `events()` 增加 `onAudit`、`onSession`；未翻页、无过滤时审计新行插顶部，会话表刷新；`store.js` 不缓存审计行。
- [x] **F1-7** `scope_view.js`（Scope → 可读摘要）+ `_tests/scope_view.test.mjs`。
- [x] **F1-8** i18n 键 `agents.*`、`sessions.*`、`audit.*`；`ui_agents_test.go`：模块嵌入、`#/agents` 在 `routes` 与 `navItems`、调用 `/sessions` 与 `/audit` 并跟随游标、denied 行有文字标签。

**F2 —— 访问令牌标签 + 揭示浮层 + 接入面板 + 设置完成页卡片（T-35 · 一期）**

- [x] **F2-1** 访问令牌标签：表格 名称 / 指纹（前 4 位）/ 可读 / 可写 / 过期 / 最后使用 / 状态（点+文字），调 `GET /mcp/tokens`。
- [x] **F2-2** "新建令牌"`openForm`：名称、可读路径（多行）、可写路径、有效期（1 天/7 天/30 天/永不）、只读开关；`scope_view.js` 校验"可写必须在可读内"后 `POST /mcp/tokens`。
- [x] **F2-3** 令牌揭示浮层（agents.js 内，`openPanel`）：明文 + `copyBtn` + "关闭后无法再次查看" + Claude Code / Codex HTTP 注册片段各带复制；令牌只在局部变量，关闭即丢，模块不 import `store.js`、源码无 `localStorage`。（2026-09-15 决定：接受"只显示一次"，已按此实现为独立模块 `token_reveal.js`。）
- [x] **F2-4** 吊销：`confirmDelete` 键入令牌名称 → `POST /mcp/tokens/{id}/revoke`，带 `confirm: true`。
- [x] **F2-5** 接入面板（Agent 屏顶部可折叠）：`GET /mcp/connect` 的 HTTP 监听状态点与地址、owner 状态、未启用 HTTP 时的配置说明。
- [x] **F2-6** `screens/setup.js` 完成页加"连接 Agent"卡片，跳 `#/agents` 并展开接入面板。
- [x] **F2-7** i18n 键 `tokens.*`；`_tests/scope_view.test.mjs` 补可写/可读包含校验；`ui_tokens_test.go`：揭示浮层无 `store.js`/`localStorage`、吊销带 `confirm: true`、接入面板调 `/mcp/connect`、`setup.js` 含 `#/agents` 链接、全部嵌入字节无 `type="password"`。

**F3 —— 会话产物表 + 主窗口工作区标记 + 检查器"来自会话"（T-36 · 一期）**

- [x] **F3-1** `session_panel.js` 产物表：路径 / 大小 / 状态（已同步、上传中，点+文字，SSE `change` 刷新）/ 动作；摘要文本、sandbox 标记、"打开工作区目录"。
- [x] **F3-2** 产物动作"在文件中打开"跳主窗口并选中；"复制链接"只在点击时请求 `/fs/download-url`，结果交给剪贴板不写入表格 DOM。
- [x] **F3-3** 会话表加"产物数"列与"仅 sandbox"过滤（`GET /sessions?sandbox=1`）。
- [x] **F3-4** `screens/main.js` 文件表：工作区根与会话目录名称旁 `bot` 小标记，带 `aria-label`。
- [x] **F3-5** `screens/main.js` 检查器：会话目录内文件显示"来自会话 <client>-<日期>"链接，`GET /sessions?path=` 反查后打开会话详情。
- [x] **F3-6** `ui_sessions_test.go`：复制链接仅点击时请求、结果不入表格；检查器调用 `/sessions?path=`；工作区标记用 `bot` 且带 `aria-label`。

**F4 —— `#/index` 屏 + 主窗口内容搜索 + 检查器索引动作（T-37 · 一期）**

- [x] **F4-1** `icons.js` 加 `layers`；`routes`/`navItems` 加 `#/index`（位于 `#/agents` 之后）。
- [x] **F4-2** `screens/index.js` 未启用态：`GET /index/status` 返回 `enabled:false` 时整屏说明与配置示例，不请求 `/index/rules`。
- [x] **F4-3** 概况卡（照缓存屏四卡）：文档 正常/待处理/失败、分块数、文本占用 / `max_total_text`、本小时下载 / `fetch_budget`；`events()` 增加 `onIndex`，进度条与让路/风控休眠原因及恢复时间。
- [x] **F4-4** 规则表：路径 / 包含 / 排除 / 单文件上限 / 来源 / 已覆盖文档数 / 动作；配置来源只显示"在配置文件中修改"；界面来源"移除"`confirmDelete` 键入路径 → `POST /index/remove`。
- [x] **F4-5** "添加规则"`openForm`：路径、包含 glob 预设（文档/代码/全部文本）、单文件上限；常驻风控提示 → `POST /index/add`。
- [x] **F4-6** 失败文档表：路径 / 类型 / 错误 / 时间 / "重试"（`POST /index/retry`），`GET /index/failed` 分页；"重建索引"`confirmDelete` 键入 `rebuild` → `POST /index/rebuild`。
- [x] **F4-7** `screens/main.js` 搜索框：仅 `status.index.enabled` 时渲染"文件名 / 内容"分段切换（偏好存 localStorage）；内容模式调 `/index/search`；结果行 文件图标 / 路径 / 标题路径 / 片段 / "可能已过期"；`degraded`、`truncated` 为结果内常驻说明行。
- [x] **F4-8** `snippet.js`（片段 + 查询 → 高亮分段数据，截断 240 字符不切半字）+ `_tests/snippet.test.mjs`（高亮、CJK 截断、特殊字符按文本处理）。
- [x] **F4-9** 检查器"索引"信息行（已索引 · N 块 / 待处理 / 失败原因 / 未覆盖）；"加入索引"（`POST /index/add`）、"移出索引"（仅界面来源）、"查看抽取文本"（`showPanel` 分页读 `/index/text`，"加载更多"）；点内容搜索结果打开同一浮层并滚到命中段。
- [x] **F4-10** i18n 键 `index.*`；`ui_index_test.go`：路由与导航、未启用不请求 `/index/rules`、移除与重建带 `confirm: true`、配置来源无移除按钮、搜索切换条件渲染、内容模式调 `/index/search` 并渲染两条说明行、检查器三动作路由。

**F5 —— 会话操作表 + 回滚预览 / 确认 / 结果 + 检查器"被 Agent 修改"（T-38 · 二期）**

- [x] **F5-1** `icons.js` 加 `undo`（P0 统一加，`TestPhaseTwoSharedHooksExist`）。
- [x] **F5-2** `session_panel.js` 操作表：序号 / 操作（新建、覆盖、编辑、改名、删除、建目录）/ 路径（改名显示旧、新路径）/ 前像（可恢复、过大、未缓存、目录，点+文字）/ 回滚结果（C2：行 `data-op`，路径走文本节点，`TestSessionOpsRenderAsText`）。
- [x] **F5-3** "回滚此会话"先 `POST /sessions/{id}/rollback {dry_run: true}` → 预览浮层：将恢复 / 将跳过（原因）/ 冲突三组 + 回滚承诺三句话；无预览不能执行（C2：`openRollback` 是唯一调该路由的代码，`TestRollbackButtonPreviewsBeforeConfirming`）。
- [x] **F5-4** 预览确认：`confirmDelete` 键入会话短 ID → `{confirm: true}`；结果浮层同三组并提供"回滚这次回滚"（C2：`TestRollbackConfirmTypesTheShortID`；C3 浏览器冒烟 `TestRollbackInTheBrowser` 真点过整条链）。
- [x] **F5-5** `rollback_plan.js`（dry-run 结果 → 分组，含空计划）+ `_tests/rollback_plan.test.mjs`（C2）。
- [x] **F5-6** 会话表状态新增"已回滚"、行内快捷"回滚"；检查器"被 Agent 修改 · <client> · <时间>"（`GET /sessions?path=`）（C2：`agents_sessions.js`、`agent_touch.js` + `main.js` 一行接线，`TestInspectorShowsAgentTouch`）。
- [x] **F5-7** `ui_rollback_test.go`：先 `dry_run: true` 后 `confirm: true` 的请求顺序；检查器标记调用 `/sessions?path=`（C2，六个用例；C3 的 e2e 证据见 TODO.md T-38"验收证明"）。

**F6 —— 嵌入端点面板 + 远端横幅 + 语义模式（T-39 · 二期，2026-09-15 完成）**

- [x] **F6-1** `screens/index.js` 嵌入端点面板（`GET /index/embedding`）：provider / 模型 / 维度 / 地址、健康点 + 最后错误 + 熔断恢复时间、已嵌入 / 待嵌入、本月字符数与标明"估算"的费用。
- [x] **F6-2** `remote=true` 时黄色横幅"文件内容会发送到 <host>"常驻，无关闭按钮。
- [x] **F6-3** "测试端点"：说明"会产生一次调用"后才 `POST /index/embedding/check`，仅点击时请求。
- [x] **F6-4** 未配置 `api_key`：显示 `cloudfs index auth` + `copyBtn`，无输入框、无秘密字段名；面板底部"在配置文件中修改"。
- [x] **F6-5** 概况卡加"向量 N / max_chunks"；主窗口搜索切换扩为"文件名 / 关键词 / 语义"，降级时显示"已降级为关键词"。
- [x] **F6-6** `ui_embedding_test.go`：横幅条件渲染且无关闭按钮、无 key 输入、测试端点仅点击请求、降级说明行。

**F7 —— 记忆标签 + 编辑浮层 + 冲突合并浮层（T-40 · 二期，2026-09-15 完成）**

- [x] **F7-1** `screens/agents.js` 记忆标签：左侧 agent 列表（`GET /memory/agents`，条数与占用 / 上限）；右侧表格 名称 / 描述 / 类型 / 更新时间 / 冲突标记（红点+"有冲突副本"）；顶部记忆搜索框。
- [x] **F7-2** 记忆编辑浮层：frontmatter（名称只读、描述、类型）+ 正文 `textarea`（按文本填充）+ 字节计数 / 上限；`PUT /memory/{agent}/{name}` 带 `expected_version`，版本冲突提示"已在其他设备修改"并提供"重新载入"。
- [x] **F7-3** `memory_conflicts.js`（同目录文件名 → 本体与副本配对，只按前缀与同目录）+ `_tests/memory_conflicts.test.mjs`。
- [x] **F7-4** 冲突合并浮层：左右只读并排；"保留本体并删除副本"（`POST /fs/delete`，path 取自 `conflicts[]`，`confirmDelete` + `confirm: true`）、"用副本覆盖本体"（`GET /fs/preview` 读副本 → `PUT` 带 `expected_version` → 删副本）、"手动合并"（编辑浮层预填两段）。
- [x] **F7-5** "新建记忆"`openForm`：agent、名称（前端 `^[a-z0-9][a-z0-9-]{0,63}$` 校验）、描述、类型；删除记忆 `confirmDelete` 键入名称 → `DELETE /memory/{agent}/{name}` 带 `confirm: true`。
- [x] **F7-6** 未配置 `memory.root` 或不在 allow 内：标签页显示说明与配置示例。
- [x] **F7-7** i18n 键 `memory.*`；`ui_memory_test.go`：保存带 `expected_version`、删除带 `confirm: true`、冲突浮层三动作各自路由、正文按文本插入。

**F8 —— `#/triggers` 屏 + 投递详情 + 测试投递（T-41 · 二期，2026-09-15 完成）**

- [x] **F8-1** `icons.js` 加 `bolt`；`routes`/`navItems` 加 `#/triggers`（位于 `#/index` 之后）；导航徽标显示 dead 投递数。
- [x] **F8-2** `screens/triggers.js` 规则卡片（`GET /triggers`，只读）：名称 / 路径 glob / 事件 / 来源 / 动作类型；exec argv 逐元素等宽渲染不拼接；webhook 显示 URL 与"签名密钥已配置"；卡片底部"在配置文件中修改"。
- [x] **F8-3** `trigger_view.js`（规则 → 自激风险）+ `_tests/trigger_view.test.mjs`；风险规则黄色标记。
- [x] **F8-4** 投递表（`GET /triggers/deliveries`）：时间 / 规则 / 路径 / 事件 / 来源 / 次数 / 状态（点+文字）/ dead 行"重试"（`POST /triggers/retry`）；规则与状态过滤；`events()` 增加 `onTrigger`，未翻页时刷新。
- [x] **F8-5** 投递详情 `showPanel`（`GET /triggers/deliveries/{id}`）：stdout/stderr 与截断提示，或 webhook 响应码与错误；支持 `#/triggers?delivery=<id>` 直接打开。
- [x] **F8-6** "测试投递"`openForm`（规则 + 路径）→ `confirmDelete` 键入规则名 → `POST /triggers/test` 带 `confirm: true`，完成后打开该投递详情。
- [x] **F8-7** 未配置规则：exec、webhook 两个配置示例与 webhook 校验代码片段。
- [x] **F8-8** i18n 键 `triggers.*`；`ui_triggers_test.go`：无编辑规则的表单与 PUT 请求、argv 逐元素渲染、webhook secret 不出现在 DOM、测试投递带 `confirm: true`、重试调用 `/triggers/retry`。

**F9 —— 发送给 Agent 浮层（检查器 + 搜索结果两入口）（T-42 · 二期，复制部分可提前；2026-09-15 完成）**

- [x] **F9-1** `send_to_agent.js`（`openPanel`）：`GET /agent/prompt?path=` 预填 `textarea`（按文本填充、可编辑）+ "复制"（始终可用，除读取提示词外不发请求）。
- [x] **F9-2** 检查器"发送给 Agent"按钮（图标 `bot`，文件与目录都有）；内容搜索结果行右侧同一入口，预填命中路径与标题。
- [x] **F9-3** `GET /agent/endpoints` 非空时才渲染 agent 下拉 + "运行"；运行前 `confirmDelete` 键入 agent 名称 → `POST /agent/invoke` 带 `confirm: true` → toast"已提交"附"查看投递"跳 `#/triggers?delivery=<id>`。
- [x] **F9-4** i18n 键 `action.sendtoagent`/`agent.prompt.*`（一期前半已用此命名，2026-09-15）；`ui_send_to_agent_test.go`：运行按钮条件渲染、运行前确认并带 `confirm: true`、提示词按文本插入、两个入口都存在。

**F10 —— 诊断项与横幅联动（T-43 · 二期回滚之前）**

- [x] **F10-1** 诊断屏不改 JS：doctor 新检查"MCP stdio 进程与挂载并存"、agent.db、index.db、嵌入端点自动出现；确认 `diagnostics.js` 对新检查项的 detail 与命令文本正常换行（C0/C3：`agent_db`、`agent_stdio` 两项经 `checkRow` 自动渲染，`detail` 是普通文本节点自然换行，`fix` 命令走等宽 `dim` 行；`TestDoctorOnALiveSystem` 断言两项存在。index.db 一期已有，嵌入端点由线 D 的 D2 加）。
- [x] **F10-2** 接入面板在 `/mcp/connect` 返回 `stdio_non_owner: true` 时渲染黄色横幅"请改用 HTTP 传输"，链到 `#/diagnostics`（C0：决策在 `connect_view.js`，`/mcp/connect` 按心跳文件填 `stdio_non_owner`）。
- [x] **F10-3** `ui_agents_test.go` 补断言：横幅条件渲染并链到 `#/diagnostics`（`TestConnectPanelWarnsAboutStdioNonOwner` + `_tests/connect_view.test.mjs`）。
- [x] **F10-4** `test/e2e` 的 `CLOUDFS_BROWSER=1` 冒烟加 `#/agents`、`#/index`、`#/triggers` 可达；`browser_modules_test.go` 自动覆盖新增 `_tests/*.test.mjs`；`ui_icons_test.go` 覆盖 `bot/layers/bolt/undo`（C3：`#/agents` 的浏览器冒烟通过——`TestAgentTokenSandboxChainAndAuditInTheBrowser`（审计标签 + 会话详情）与 `TestRollbackInTheBrowser`（会话详情 → 回滚全流程）；`browser_modules_test.go` 按目录枚举 `_tests/*.test.mjs`，`rollback_plan.test.mjs` 已被覆盖；`TestPhaseTwoSharedHooksExist` 覆盖 `undo`/`bolt`，`bot`/`layers` 一期已覆盖。`#/index` 的可达冒烟是一期的 `TestContentSearchInTheBrowser`，`#/triggers` 属线 E（E6），不在本线勾选范围）。

**F11 —— Everything 式文件名搜索：主窗口全盘搜索、过滤条、覆盖率（T-44 · 一期，前置于 F4-b）**

- [x] **F11-1** `screens/main.js` 搜索框默认**全盘**（请求不带 `path=`），旁边分段切换"全盘 / 当前目录"，选择存 localStorage；`Ctrl/⌘+K` 聚焦，`Esc` 清空并回到目录视图。搜索逻辑抽到 `content_search.js` 之外的独立模块 `name_search.js`，`main.js` 只接线（F4-b 之后同一个模块再加"内容"分段）。
- [x] **F11-2** 结果行填满：名称（命中高亮，片段经文本节点插入，绝不进 `html:`）/ 大小 / 修改时间 / 状态（已缓存点+文字）；表头可点排序 → 改 `sort=` 重新请求；双击结果定位到父目录并选中该行。
- [x] **F11-3** 结果上方常驻一行"N 条 · 覆盖 已列举 X / 已知 Y 目录"（来自 `/search` 响应的 `coverage`）；未全覆盖时附"索引整棵树"按钮：`confirmDelete` 键入 `warm` → `POST /cache/warm {path:"/", depth:-1, confirm:true}`；`complete=false` 的常驻说明行不变。
- [x] **F11-4** 过滤条：搜索框右侧"筛选"展开为 类型 / 扩展名 / 大小范围 / 修改时间；`search_query.js`（零 import）做过滤条 ↔ 查询串双向转换，用户能看到并手改最终查询串；最近 10 次搜索下拉（localStorage）。
- [x] **F11-5** 缓存屏概况卡加"目录覆盖率"卡（已列举 / 已知、最后爬取时间、爬取中进度），SSE `status` 驱动；检查器目录项加"列举整棵子树"（`warm` depth=-1，同一确认门）。
- [x] **F11-6** i18n 键 `search.*` 扩充（scope、coverage、filters、sort）；`ui_search_test.go`：默认请求不带 `path=`、分段切换写 localStorage、"索引整棵树"带 `confirm: true`、表头点击改 `sort=`、高亮经文本节点插入、覆盖率文案来自 i18n；`_tests/search_query.test.mjs`：双向转换与非法输入。

**关键复用**：模态与确认用 `internal/control/web/ui.js` 的 `openForm`、`openPanel`、`showPanel`、
`confirmDelete`；表格续页用 `ui.js` 的 `moreRow` 与 `paged.js` 的 `pageCursor`/`pageFailureMode`（续页失败保留
已加载行）；复制用 `ui.js` 的 `copyBtn`；实时刷新扩展 `api.js` 的 `events()`，不另开 EventSource；
新屏登记在 `router.js` 的 `routes` 与 `navItems`；服务端确认门用 `internal/control/shared.go` 的 `confirmed()`。

### 阶段 G —— Agent-first 界面（2026-09-15 登记）

对应 [TODO.md](../TODO.md) 的 P5 节（T-46 ~ T-57），设计与安全边界见 [Agent-first 设计](agent-first-design.md)
§9。本段只列界面侧逐项清单；证据、后端做法与验收断言以 TODO.md 为准。**纪律**与阶段 F 相同：G 条目与对应 T 条目
同期交付、同一验收，界面不落地不关 T 条目。G1 ~ G4 属 P0，G5 ~ G7 属 P1，G8 ~ G9 属 P2。共用约定沿用阶段 F
（`openForm` / `openPanel` / `showPanel` / `confirmDelete`、`moreRow` + `paged.js`、i18n 两表、零 import 模块进
`web/` 根与 `web/_tests/*.test.mjs`、文本按文本插入、破坏性动作键入确认 + `confirm: true`）。

**G1 —— 运行时指引卡（T-46 · P0）**

- [ ] **G1-1** `#/agents` 接入面板加"运行时指引"卡：`GET /agent/prompt?kind=instructions` 的文本（只读、可复制）、token 估算、四个 prompt 名（`onboard` / `search-this-tree` / `write-safely` / `finish`）各带"复制"。
- [ ] **G1-2** `send_to_agent.js` 的预填改为 `kind=onboard`（输出与今天一致）。
- [ ] **G1-3** i18n 键 `agent.instructions.*`；`ui_agents_test.go`：指引卡调用 `?kind=instructions`，文本按文本插入。

**G2 —— 接入面板传输与桥状态（T-49、T-50 · P0）**

- [ ] **G2-1** `connect_view.js`：显示"推荐传输：http（挂载运行中）/ stdio（未检测到挂载）"（来自 `/mcp/connect` 的 `install_transport`）；stdio 非 owner 横幅按 `bridge` 三态渲染：`connected` 绿色"已通过桥连接到 owner"、`disabled` 黄色附原因、`n/a` 不渲染。
- [ ] **G2-2** `_tests/connect_view.test.mjs` 覆盖三态；`ui_agents_test.go` 断言横幅文案来自 i18n。

**G3 —— 设置屏 MCP 段（T-47 · P0）**

- [ ] **G3-1** `#/settings` MCP 段加 `max_tokens`（数字输入，0 = 关闭）与提示"一次 `read_text` 上限约 N token"；`install.transport` 下拉（auto / stdio / http）。
- [ ] **G3-2** `ui_settings_test.go` 断言两个字段经既有配置变更库写入、非法值被拒。

**G4 —— 可逆性与回滚预览（T-48 · P0）**

- [ ] **G4-1** 会话详情浮层操作列表每行可逆性图标（✓ / ⚠ 不可回滚，hover 显示 `preimage_reason` / — 未记录），文字标签不只靠图标。
- [ ] **G4-2** 回滚预览浮层 `skipped` 按 reason 分组，`not_recorded` 单独一行说明"该写入发生时没有会话"；`rollback_plan.js` 纯函数扩展，`_tests/rollback_plan.test.mjs` 覆盖分组。

**G5 —— 来源、历史与变更（T-51、T-52 · P1）**

- [ ] **G5-1** 检查器加"最近修改：来源 · 主体 · 时间"一行（`last_writer`，`console` / `webdav` / `kernel` / `mcp` 四种来源各有图标 + 文字）。
- [ ] **G5-2** 检查器"历史"标签：`GET /changes?path=` 分页，列 时间 / 种类 / 来源 / 主体 / 会话（可点开会话详情）/ 可逆性；`reliable = 0` 行带"可能有遗漏"标签。
- [ ] **G5-3** `#/agents` 审计标签加来源列；新"变更"标签：`GET /changes?prefix=&since=` 分页 + 前缀过滤 + SSE `change` 刷新（未翻页、无过滤时）。
- [ ] **G5-4** `#/agents` 会话详情浮层：内核写标"推断属于本会话"（展示层推断，不入库）。
- [ ] **G5-5** i18n 键 `changes.*` / `origin.*`；`ui_agents_test.go`、`ui_inspector_test.go`：变更表跟随 `next_cursor`、来源图标有文字、`reliable = 0` 有文字标签。

**G6 —— 热度（T-53 · P1）**

- [ ] **G6-1** `web/heat_plot.js`（零 import 纯函数）：输入 `[]{x, y, kind, path}` 与尺寸，输出 SVG 字符串；象限边界取中位数，半径按读取数对数缩放，hover 显示路径与数字，点击派发 `open-inspector`；`_tests/heat_plot.test.mjs`（象限划分、对数半径、空数据、单点）。
- [ ] **G6-2** `#/agents` 新"热度"标签：`GET /agent/heat?prefix=&days=&by=path` 驱动散点；hot-but-stale 清单（右上象限）每行"打开检查器"与"生成建议"；`days` 分段 7 / 30 / 90。
- [ ] **G6-3** 建议浮层（`openPanel`）：`GET /agent/suggestions?prefix=` 的草案列表（pin / index / 热但陈旧 / 可解除 pin），每条"采用"按钮走既有 `/cache/pins`、`/index/rules` 带确认路由，浮层本身不写规则。
- [ ] **G6-4** 主窗口文件列表热度点（30 天读取数，hover 显示 agent / kernel / console 拆分）；检查器"30 天读取"一行；`#/settings` 加 `heat.enabled`、`retention_days`。
- [ ] **G6-5** i18n 键 `heat.*`；`ui_agents_heat_test.go`：热度标签调用 `/agent/heat`、建议浮层调用 `/agent/suggestions` 且不调用写路由、采用按钮带 `confirm: true`、响应中的路径按文本插入；浏览器冒烟：人为拨旧 mtime 后 hot-but-stale 清单出现该文件。

**G7 —— Hooks（T-54 · P1）**

- [ ] **G7-1** 接入面板"Hooks"卡：`GET /agent/hooks` 的每平台安装状态（已安装 / 未安装 / 未验证）、`context` 档位、最近一次 hook 调用时间、要执行的安装 / 卸载命令（`copyBtn`，**不**提供"在浏览器里安装"按钮）。
- [ ] **G7-2** 会话列表 principal 列显示 `hook:claude` 并带图标；会话详情显示 `reads` 计数与 `last_change_seen`。
- [ ] **G7-3** `#/settings` hooks 段：`context`（off / minimal / full）、`changed_max`、`memory_head_lines`。
- [ ] **G7-4** i18n 键 `hooks.*`；`ui_agents_test.go`：Hooks 卡调用 `/agent/hooks`、命令按文本插入、无任何 POST 到 `/agent/hooks`；`TestHooksRouteNeverWritesUserConfig`。

**G8 —— 渲染屏与分享（T-55 · P2）**

- [ ] **G8-1** `router.js` 加 `#/fs/<path>`（不进 `navItems`）；`screens/fs.js`：`GET /fs/render?path=` 渲染（Markdown / 代码 / 图片 / PDF / 其它下载），顶部"最近修改"与"30 天读取"两行，按钮"复制内链"、"创建分享"（`confirmDelete` 键入文件名 → `POST /share` 带 `confirm: true`）、"历史"标签复用 G5-2。
- [ ] **G8-2** 检查器加"复制内链"与"创建分享"两个按钮（同一入口）；分享成功后 toast 附链接与过期时间；驱动 `Caps.Share = false` 时按钮禁用并说明。
- [ ] **G8-3** `#/settings` share 段（`console_links`、`render.enabled`、`render.token_ttl`）；渲染页 token 不进 `store.js`。
- [ ] **G8-4** i18n 键 `share.*` / `fs.*`；`ui_fs_test.go`：渲染内容来自 `/fs/render` 且不含 `<script>`、创建分享前 `confirmDelete`、`Caps.Share = false` 禁用；`_tests/store.test.mjs` 断言 store 键集合不含 `render_token`。

**G9 —— 多人记忆（T-56 · P2）**

- [ ] **G9-1** `#/agents` 记忆标签按 owner 分组（v2 布局），v1 布局时不分组；`#/settings` memory 段 `layout` 只读显示 + "迁移到 v2"按钮（`confirmDelete` 键入 `migrate` → `POST /memory/migrate` 带 `confirm: true`）。
- [ ] **G9-2** 冲突合并浮层：`memory_merge` 结果的三方 diff（零 import `three_way_view.js`）与"采用合并结果"（调 `memory_put` 带 `expected_version` 与 `remote_version`）。
- [ ] **G9-3** i18n 键 `memory.owner.*` / `memory.merge.*`；`ui_agents_memory_test.go`：分组渲染、迁移带 `confirm: true`、采用按钮带两个版本；`_tests/three_way_view.test.mjs`。
