# CloudFS 桌面界面 —— 开发方案与计划（2026-09-06）

对应设计稿：https://claude.ai/code/artifact/2c5d82b9-8ca9-4f8b-a4f7-02158cd95153（8 张画板：总览、主窗口、添加网盘、连接设置、代理出口、传输队列、缓存与固定、诊断与服务）。

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

- [ ] **P0-1** `/cache/drop` 加 `privateRequest`（`internal/control/metrics.go:80`）。回归：`cache_test.go` 守卫表加 cross-site 一行，修前失败。
- [ ] **P0-2** 抽公共件：`writeJSON`（从 `accounts.go` 的 `writeAccountJSON` 提出，no-store/nosniff）、`requireConfirm(confirm bool, reason string)`、`rejectSecretFields(map[string]string)`（把 `buildAccount` 里的 `IsSecretField` 检查提出来，PATCH 与 POST 共用一处）。把 `safeFieldName` 移到 `config` 包导出，`control` 与新的 `config.SetRemoteField` 共用一个定义。
- [ ] **P0-3** 修 `vfs.attrOf` 不设 `Pinned`（`internal/vfs/vfs.go:384`，用 `pathPinned` 查）。回归 `TestAttrOfReportsPinned`：写文件→Pin→列父目录→断言 Pinned，修前失败。
- [ ] **P0-4** 全路由守卫测试 `internal/control/security_all_routes_test.go`：枚举 mux 上每条路由，非 GET 无头 → 403，跨源 → 403。这是防止下一个人再漏一条的闸门。
- [ ] **P0-5** 决定并写下注释：`/status /metrics /healthz /readyz` 保持无守卫（只读、无秘密、回环进程本就能读 /proc）。

### 阶段 1 —— 只读 + 低风险写：`/fs/*` `/search` `/doctor` `/events`（M/L）→ 解锁主窗口、详情、诊断

新文件 `internal/control/fs.go`、`search.go`、`events.go`；`doctor.go` 加路由。

- [ ] **P1-1** `canonicalPath`：与 `mcpsrv.checkPath`（`server.go:139`）同一套规则。建议抽成 `internal/pathsafe` 让两边共用；至少共用同一张测试向量表。
- [ ] **P1-2** `GET /fs/list?path&cursor&limit&count` → `FS.ReadDirPagePath`，游标编码**照抄** `internal/mcpsrv/directory_page.go`。响应 `FSEntry{name,is_dir,size,mtime,cached,pinned,local_only}`。
- [ ] **P1-3** `GET /fs/stat`、`GET /fs/preview?path&offset&length`（硬上限 1 MiB，`application/octet-stream` + `Content-Range`）、`GET /fs/download-url`（**唯一允许返回签名 URL 的端点**，no-store、不落日志，注释写明是有意例外）。
- [ ] **P1-4** `POST /fs/mkdir`（单层，不做 mkdir -p）、`POST /fs/rename`、`POST /fs/delete`（`confirm` 必填，拒绝 `/`）——路径→inode 翻译照抄 `mcpsrv.mkdirAll/move/deletePath`（`server.go:783-905`）。
- [ ] **P1-5** `GET /search?q&path&limit` → `FS.Meta().SearchReport`，响应带 `complete`，界面显示"可能还有更多"。
- [ ] **P1-6** `POST /doctor/run`（有轻微写：探针文件、vacuum，所以是 POST 但不需 confirm）、`POST /doctor/fix`（confirm 必填）。`Collector` 加 `Doctor *control.Doctor`，`Daemon.Collector()` 用 `d.Doctor(fuseSupported)` 装配（现在只有 `cmdDoctor` 装）。
- [ ] **P1-7** `GET /events` SSE：`FS.WatchChanges()` 的变更事件 + 每 2s 一次 `Collector.Collect` 作为 `status` 事件。测试模式照 `internal/mcpsrv/subscriptions_http_test.go`。先看 `changes.go` 有没有订阅者上限，参考 `mcpsrv/subscriptions.go` 的慢客户端隔离。仪表类数据允许 `/status` 轮询兜底，目录变更必须走 SSE。
- [ ] **P1-8** 测试：`fs_test.go` 用 `cacheControl(t)`（`cache_test.go:13`，真 vfs + fakeprovider）派生 `fsControl(t)`；每条路由 × {无头, 跨源, 非回环 Host} → 403；穿越向量（`..`、NUL、空）→ 400；`test/e2e` 加 `TestUIAPIEndToEnd`：mkdir→list→rename→delete 经控制面，断言**挂载点**上可见。

### 阶段 2 —— 配置变更库（M，`internal/config/edit.go`）

全部走现有 `editConfig`（flock + yaml.Node 手术 + 重新 Parse/Validate + 原子改名，保留注释）。

- [ ] **P2-1** `RemoveRemote(path, name)`：有挂载布局引用时拒绝。
- [ ] **P2-2** `SetRemoteField(path, name, SetRemoteFieldOptions{Fields map[string]*string, Proxy *string, QPS *QPS, UploadWorkers *int})`：`Fields` 每个键过 `IsSecretField` 与 `SafeExtraFieldName`；`Proxy` 必须是已有出口/组名或空。
- [ ] **P2-3** `SetProxy(path, Proxy)`：整段替换（规则引用组引用出口，局部 PATCH 会做出"各自合法但不是本意"的状态）；先在内存构造 Config 跑 `Validate` 给更好的报错。
- [ ] **P2-4** `AddMount/RemoveMount/SetLayout`：把 `AddRemote` 里的挂载创建分支重构为 `upsertMountLayout` 让两边共用。
- [ ] **P2-5** 测试：每个函数 happy path、拒秘密键、拒不存在的代理目标、**注释存活**（`# 我的备注` 经 `SetRemoteField` 后仍在——这正是 node 手术而非整文件重编的意义）。

### 阶段 3 —— 账号 / 代理 / 挂载端点（L）→ 解锁连接设置、代理出口、添加网盘

- [ ] **P3-1** `Collector` 加 `Providers map[string]provider.Provider`、`CheckAccount func`、`ReloadProxy func`（`daemon.go:330` 装配）。
- [ ] **P3-2** `GET /accounts/{name}`：`AccountDetail{name,type,proxy,qps,upload_workers,fields(剔除秘密键——legacy 内联秘密可能还在 Extra 里，必须过滤而不是假设没有),caps,has_credentials(bool，永不带值)}`。`caps` 来自 `Providers[name].Capabilities()`。
- [ ] **P3-3** `PATCH /accounts/{name}` → `rejectSecretFields` → `SetRemoteField`；`DELETE /accounts/{name}?confirm=true` → `RemoveRemote`，响应 `restart_required: true`；`POST /accounts/{name}/check` → `daemon.CheckAccount`，错误经现有脱敏（`checkConfiguredAccount` 那套，provider 错误里有签名 URL/cookie）。
- [ ] **P3-4** `internal/control/proxy.go`：`GET /proxy/explain?host`（`Router().Explain` + `OutboundFor`）、`POST /proxy/check`（`CheckNow`）、`GET /proxy/config`（**先读 `Outbound.URL()` 确认是否支持 `user:pass@host`，支持则脱掉 userinfo**）、`PUT /proxy/config` → `SetProxy`；有 P3a 前响应 `restart_required: true`。
- [ ] **P3-5** `internal/control/mounts.go`：`GET/POST /mounts`、`DELETE /mounts/{path}?prefix&confirm` —— **本期只改配置**，永远 `restart_required: true`。运行时加卸挂载明确推后（结构性冻结，不是缺管道）。
- [ ] **P3-6** 测试：`accounts_test.go` 扩 PATCH/DELETE/check，新 `proxy_test.go`、`mounts_test.go`，守卫表 + 秘密泄漏断言（`json.Marshal` 响应扫秘密键名与 token 形状）。

### 阶段 3a —— 代理热加载（M，可晚于 3 一个版本）

- [ ] **P3a-1** `internal/net/proxy/dialer.go` 加 `(*Manager).Reload(ManagerOptions) error`：把 `NewManager` 里出口/组构造抽成 `buildOutboundsAndGroups`；`Reload` 换 `router/outbounds/groups`，清掉已不存在的组的 `selected`。**顺带修**：`Resolve/OutboundFor/checkGroup/ruleTransport.RoundTrip` 读这三个字段全部改到 `m.mu.RLock()` 下（现在是无锁读）。
- [ ] **P3a-2** `daemon.buildProxy` 的转换抽成 `toManagerOptions(cfg.Proxy)`，冷启动与热加载共用；`Collector.ReloadProxy` 接上。`PUT /proxy/config` = `SetProxy`（落盘）+ `ReloadProxy`（生效）；落盘成功而生效失败要回 `{"applied":false,"restart_required":true,"warning":...}`，绝不让调用方以为两件都成了。
- [ ] **P3a-3** 回归 `TestReloadAppliesToInFlightRuleTransport`：两个假 HTTP 服务，Reload 后**下一个**请求到新目标；`-race` 下 Reload 与 OutboundFor/RoundTrip 并发循环。

### 阶段 4 —— 凭据流程（M/L，可与 3 并行）→ 解锁添加网盘的授权步骤

- [ ] **P4-1** 把 `cmd/cloudfs/config_manage.go:432-461, 487-541` 的核心抽到 `internal/daemon/auth.go`：`StartOAuthFlow(ctx,cfg,name) (url, wait, err)`、`StartDevice115Flow(ctx,cfg,name) (qrContent, scanned, wait, err)`。不打印、不开浏览器；CLI 与控制面都调它（vfs/fusefs/mcpsrv 同一种"单一实现、薄适配"形状）。
- [ ] **P4-2** `internal/control/auth.go`：`POST /accounts/{name}/auth/start` → `{kind:"url"|"qr", value, session}`；`GET .../auth/status?session` → `pending|scanned|done|denied|error`；`POST .../auth/cancel`。内存会话表，session id 加密随机，同一账号同时只允许一个流程（第二个 → 409）。完成时守护进程侧调 `config.SaveCredentialsForRemote`，**响应里永不出现 fields**。
- [ ] **P4-3** 测试：`internal/daemon/auth_test.go` 假 OAuth/假 115 服务（照 `internal/auth/*_test.go`）；`internal/control/auth_test.go` 断言响应体不含 token 形状；双飞断言（第二个 start 冲突）。

### 阶段 5 —— 生命周期：重启与服务管理（M/L，最后做，爆炸半径最大）

- [ ] **P5-1** `POST /daemon/restart`（confirm）：置 draining 原子标志（`privateRequest` 对非 GET 回 503）→ `Uploader.Flush` 限时 30s → 有 FUSE 则 `Mount.Unmount()` → `Daemon.Close()`（释放 journal flock）→ 写完 200 后关自己的监听 → 若在 systemd/launchd 下运行则 `exit(0)` 交给服务管理器拉起（检查生成的 unit 有没有 `Restart=on-success`，没有就加），否则 `syscall.Exec` 自重启。响应带 `mode: "reexec"|"service_managed_exit"`。**两个 owner 绝不能共存**是这条的唯一原则。
- [ ] **P5-2** `cmd/cloudfs/service.go` 逻辑迁到 `internal/service`，CLI 变薄包装。守护进程**可以** `Status`；`Install` 加 flock 且已加载时回 409；`Uninstall` 先读完现有卸载分支（stop unit → unmount → 删文件的顺序）再接——服务管理器在挂载被拔掉后立刻拉起进程是要避免的状态。
- [ ] **P5-3** `/service/status`（GET）、`/service/install`、`/service/uninstall`（POST + confirm）。
- [ ] **P5-4** 测试迁到 `internal/service/service_test.go`，加"已安装再 install → 409"、卸载调用顺序断言。

### 阶段 A —— 前端（无 Node）

- [ ] **A1** 目录：`internal/control/web/{index.html, app.css, app.js, router.js, api.js, store.js, icons.js, i18n.js, components/*.js}`。原生 ES modules + `customElements`，浏览器自己解析 import 图。
- [ ] **A2** `ui.go` 改 `//go:embed web` + 显式白名单静态处理器（启动时 walk 进 map，**不用** `http.FileServer` 直出，保留"精确路径否则 404"契约）；CSP 收紧为 `script-src 'self'; style-src 'self'`（去掉 `unsafe-inline`——这是变严不是变松）；`Cache-Control: no-cache` + 启动时算一次 SHA-256 的 `ETag`（二进制整体替换，不需要 `?v=` 打戳机制）。**重写** `ui_test.go:62-66` 的 404 断言而不是绕过。
- [ ] **A3** hash 路由（`#/connections` 默认、`#/proxy`、`#/transfers`、`#/storage`、`#/diagnostics`；`#/add-drive`、`#/connect/:remote` 是覆盖在主窗口上的叠层，不是整页切换）。选 hash 而非 history：服务端零改动，且 `ui_test` 的契约不变。
- [ ] **A4** `api.js`：统一 `request()`，非 GET 自动带 `X-CloudFS-Control: 1`；非 2xx 抛 `ApiError{status,message}` 由 `toast-host` 统一展示；SSE 订阅 + 指数退避重连（上限 30s）+ 打不开时退回 5s 轮询（今天 `setInterval(refresh, 5000)` 的推广）。
- [ ] **A5** `store.js` ~30 行 pub/sub；组件在 `connectedCallback/disconnectedCallback` 订阅/退订。
- [ ] **A6** `i18n.js`：所有界面字串走 `t('files.column.size')`，只填 zh-CN，不做语言协商；纪律是"组件模板里不写死中文"。
- [ ] **A7** **三平台一致性**（`app.css`）：
  - 字体：`-apple-system, "Segoe UI", "PingFang SC", "Microsoft YaHei", "Noto Sans CJK SC", "Source Han Sans SC", system-ui, sans-serif` 显式列出而不是信任 `system-ui` 的解析；`font-size-adjust` 拉平 x-height；数字列 `font-variant-numeric: tabular-nums`。**不捆绑 CJK 字体**：可用子集要 fonttools（Node/Python 工具链，与约束冲突）或整包 >10 MB（与拒绝 rclone 的理由同构）。在 CSS 里写明这是"零字节 vs 几像素行高差"的有意取舍。
  - 兜底路径（真机截图后如差异不可接受）：把 `i18n.js` 里**实际出现的**汉字机器统计出来（8 屏文案大概几百个码点），用一次性 `fonttools` 命令离线做子集 `.woff2` **提交进仓库**（和 `web/*` 一样是签入源码不是构建产物），`go build` 依然无 Node。
  - 控件：`select, input, button { appearance: none }` 全部按令牌手绘（WebView2 的原生 radio 和 WebKitGTK 的长得完全不一样）；`scrollbar-color` + `::-webkit-scrollbar`；`:root { color-scheme: dark }`；统一 `:focus-visible { outline: 2px solid #6f94bd }`；`-webkit-font-smoothing: antialiased`。
- [ ] **A8** 可访问性：`confirm-sheet` 焦点陷阱 + Esc + 焦点归还；`data-table` 方向键行导航；保留原生 `<table>` 语义；对话框 `role="dialog" aria-modal`；每个彩色状态点旁必有文字。
- [ ] **A9** 屏幕组件（按设计稿）：先主窗口（`app-shell`/`connection-list`/`file-table`/`inspector-panel`，它锻炼 `data-table`/`confirm-sheet` 两个共用件），再 `add-drive-modal`/`connect-wizard`（叠层），再 `proxy-view`/`transfers-view`/`storage-view`/`doctor-view`。传输队列的"永久丢弃本地版本"用**输入上传 ID**才启用按钮（沿用现有 `window.prompt` 的语义，做成 sheet）。
- [ ] **A10** 测试三层：(1) 必做——扩 `ui_test.go`：每个嵌入资产的路径/Content-Type/CSP，未知路径 404，**全部字节里无 `type="password"`、无秘密字段名作为 input name/id**；(2) 不引入 JS 单测框架（那就是要避的 Node），把纯函数（字节格式化、store 合并、路由映射）抽成可导出函数，老实记为缺口；(3) `test/e2e` 加 `CLOUDFS_BROWSER=1` 门控的浏览器冒烟（`os/exec` 起系统 Chromium `--headless --remote-debugging-port`，几百行 Go 写最小 CDP 客户端，无浏览器时 `t.Skip`，与 FUSE 测试缺 `/dev/fuse` 的约定一致）。
- [ ] **A11** `cloudfs ui`（或 `open`）命令：用 `control.FetchStatus` 的端点选择逻辑找活着的 URL 打印出来，桌面系统上 `xdg-open/open/rundll32 url.dll,FileProtocolHandler` 打开。不依赖壳，浏览器用户立刻受益。

### 阶段 B —— 桌面壳 `cmd/cloudfs-desktop`（cgo，独立产物）

- [ ] **B1** (S) 可行性：`webview/webview_go` 在本机 GTK/WebKitGTK 下能编、能加载 `data:` URL。
- [ ] **B2** (M) 骨架：建窗口 → `control.FetchStatus(ctx, socket, tcp)`（`listeners.go:120`，已实现"先 socket 后 TCP，拒绝/不存在视为离线"）→ 导航到回环 URL。
- [ ] **B3** (M) 生命周期：不可达时 `os/exec` 起用户已装的 `cloudfs mount`（照 `service.go` 的 `rt.run` 形状）——**壳里绝不内嵌第二份装配逻辑**；未运行/重启中显示本地 `data:` 占位页 + 重试；SSE 断 + FetchStatus 失败 → 同一占位层。
- [ ] **B4** (依赖后端) 新增控制面端点"给我一个 UI 用的回环 TCP 地址"（默认配置 `control.metrics` 为空、UI 只在 unix socket 上）——**跨半边的依赖，需后端配合**；壳**不**自己服务 `embed.FS`（那是第二份 CSP/静态服务实现）。
- [ ] **B5** (M) 单实例：`listeners.go:61-67` 同款 flock；已在运行则调新增 `POST /focus` 让现有实例前置窗口。
- [ ] **B6** (M，可选并行) 托盘：`getlantern/systray` 之类（Linux 上又一个 cgo/AppIndicator 依赖，无托盘发行版要安全无操作）；菜单：打开窗口 / 重启守护进程 / 退出；三色语义与 Web 令牌一致。
- [ ] **B7** (S/M) 开机自启：不做两套——诊断页那个开关就是 `cloudfs service install`（守护进程自启）；壳自身的登录项只是启动 `cloudfs-desktop`，它自己会附着或拉起守护进程。
- [ ] **B8** (L) 打包：Linux `.desktop` + 图标；macOS `.app`（先不签名，签名/公证单列）；Windows 安装器 + WebView2 运行时检测/引导。**macOS/Windows 部分本机无法验证**。
- [ ] CI：Linux job 装 `libgtk-3-dev libwebkit2gtk-4.1-dev` 编 `cmd/cloudfs-desktop`；macOS runner 也编一份。

### 阶段 C —— Windows

**C1 仅编译（先交付，CI 可验证，产出静态 `cloudfs.exe`：无 mount，其余全能）**

- [ ] **C1-1** (S) `internal/journal/lock.go` 拆 `lock_unix.go` / `lock_windows.go`（`windows.LockFileEx` + `LOCKFILE_FAIL_IMMEDIATELY` 对应 `LOCK_EX|LOCK_NB`）。
- [ ] **C1-2** (M) 抽 `internal/config/filelock_{unix,windows}.go` 一对"打开并锁侧车文件"原语，`secrets.go:216`、`edit.go:35`、`control/listeners.go:61-66` 三处共用（Windows 无 `O_NOFOLLOW`，NTFS 符号链接竞争是另一个更弱的威胁模型，注释里承认）。
- [ ] **C1-3** (M) `cmd/cloudfs/config_manage.go:306-321` 的 `unix.Poll/Read`：重构为 goroutine 读 stdin + channel select，去掉裸 Poll，直接变可移植。**先读完那 40 行再定形状**。
- [ ] **C1-4** (S) `vfs.go:1035` `syscall.Sync` → `_windows.go` 无操作 + 注释（只是基准用的"让磁盘安静"，`FlushFileBuffers` 要卷句柄，不值当）。
- [ ] **C1-5** (S) `internal/fusefs/platform_windows.go` 桩：`checkPlatform` 返回 `(false, "FUSE mounting needs the WinFsp build")`，`PassthroughAvailable` 返回不适用；`MountFS` 在 windows 上返回清晰的"此构建不支持"。`main.go` 无条件调用照常编过。
- [ ] **C1-6** (S) `cache/{identity,statfs}_other.go`、`cmd/cloudfs/{service_mount,unmount}_other.go`、`service.go` 的 `default:` 分支——**已是正确的 Windows 行为**，只需确认 `!linux && !darwin` 标签仍选中。
- [ ] **C1-7** (S) CI：ubuntu job 加 `GOOS=windows go vet ./...` + `GOOS=windows CGO_ENABLED=0 go build ./cmd/cloudfs`（交叉编译，无需 Windows runner）；`release.sh` 加 `build_one windows amd64`，输出名加 `.exe`。

**C2 WinFsp 适配（XL，写得出、本机验不了；`-tags winfsp`，默认关）**

- [ ] **C2-1** 新包 `internal/winfs`（不在 `fusefs` 里用 build tag 分叉——那会让每个文件都要一份 Windows 变体）：与 `fusefs` 同一种薄适配，目标是 `github.com/winfsp/cgofuse` 的回调形状。`checkPlatform` 探 WinFsp 注册表键 / `%ProgramFiles%\WinFsp\bin\winfsp-x64.dll`，与 `platform_darwin.go` 探 `macfuse.fs` 同款。
- [ ] **C2-2** 两个 Windows 产物：`cloudfs_<ver>_windows_amd64.exe`（C1 静态）与 `..._windows_amd64_mount.exe`（`CGO_ENABLED=1 -tags winfsp`）。后者需要 cgo 交叉工具链（`zig cc` 作 CC）或真实 Windows builder——**不要假设它像静态版那样能从 Linux 直接交叉**。
- [ ] **C2-3** 不变量映射：提交在 FLUSH（cgofuse 有 `Flush(path, fh)`，但 WinFsp 的 CLEANUP/CLOSE 派发频率**必须真机验证**，CLAUDE.md 里"FLUSH 每个 fd 触发多次"的假设不能照搬）；`durable(ctx)` 与 `writeState.lastUploadID` 在 vfs 侧，适配层照调即可；inode 身份——cgofuse 偏路径，需要 path↔ino 表（**最大未知**，先看 `vfs.FS` 的方法是否以 ino 为键）；readdir 用 `FillFunc` 填 stat；xattr 报不支持；`EROFS` 由 cgofuse 转 NTSTATUS，只需冒烟。
- [ ] **C2-4** **两个跨越适配层/核心边界的产品决策，需签字不能默默选**：(a) 大小写——`meta` 目录树按 POSIX 大小写敏感，NTFS/WinFsp 默认不敏感但保留大小写，只差大小写的两个远端文件会碰撞（WinFsp 新版支持按卷开大小写敏感，或在 vfs 边界折叠）；(b) 保留名与非法字符（`CON/PRN/NUL/COM1…`、`<>:"|?*`、尾部点/空格）——远端命名空间没这些限制，要么呈现时转义、要么拒绝呈现并明确报错。
- [ ] **C2-5** 真机验收清单（有 Windows 时按序跑）：① `-tags winfsp` 真能链接；② 装/卸 WinFsp 各一次，`doctor` 探测两态都对；③ `fake` remote 挂载，`cmd`/PowerShell 下 `dir/type/copy`；④ `test/conformance` 能编则跑，有意差异写成显式例外；⑤ **`winfsp-tests`**（WinFsp 上游一致性套件，Windows 上 pjdfstest 的对应物）；⑥ 并发写/上传中改名手工 chaos；⑦ Defender 实时保护开着做大文件读写；⑧ 壳在干净 Win10 21H2 / Win11 上 WebView2 加载。**fsx-via-WSL 不算验证**（那是另一套文件系统栈）。

### 阶段 D —— CI / 发布 / 文档

- [ ] **D1** `ci.yml`：windows 交叉 vet+build（C1-7）；`-tags winfsp` 单独 job、仅 tag 或手动触发。
- [ ] **D2** `ci.yml`：桌面壳 job（Linux 装 GTK/WebKitGTK dev 包；macOS runner 顺带编）。
- [ ] **D3** `release.sh`：windows amd64 静态；C2 后加 `_mount.exe`。
- [ ] **D4** `docs/distribution.md`：Windows 静态版能力/限制（照 MCP-only 容器那节的口吻"能力缩减、明说"）；C2 后写两产物拆分与 WinFsp 驱动前置。
- [ ] **D5** README：`cloudfs ui` 一行；界面章节。
- [ ] **D6** `TODO.md`：T-17 → 完整 UI；T-21 翻状态并写清"仅编译 / WinFsp 适配"两段与静态/cgo 双产物策略；新登记 T-24（UI 控制面 API）、T-25（桌面壳）。
- [ ] **D7** `docs/DESIGN.md` §4.8 补控制面新端点契约与 SSE；§3.8 CLI 表补 `ui`。

---

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
