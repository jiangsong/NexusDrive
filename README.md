# CloudFS

把国内外主流网盘挂载为本地目录的单机守护进程。统一的元数据缓存、块缓存与可靠写入日志抹平各平台 API 的差异；同时通过 MCP 让 Claude Code、Codex 等 agent 直接读写同一份文件系统。

完整设计见 [docs/DESIGN.md](docs/DESIGN.md)。

## 它解决什么

- **挂载即用**：`ls`、`grep -r`、`git status`、编辑器都能直接工作在网盘目录上，不需要先同步。
- **查找快**：目录树持久化在本地 SQLite，热目录零远端调用；文件名有 trigram 索引。
- **写入可靠**：`close()` 返回时数据已 fsync 并写入日志，之后异步上传。断网、限速、进程被 kill 都不丢数据。
- **不被封号**：按（网盘、账号、请求类别）三维限流，遇 429 或风控自动降速，持续异常则熔断该账号并转只读。
- **代理灵活**：Clash 风格域名规则，API 域与下载 CDN 域可走不同出口，出口组带健康检查与故障转移。
- **Agent 友好**：MCP 工具带分页、字节范围读、响应大小上限、删除确认门禁，不会撑爆 agent 的上下文。

## 快速开始

```sh
go build -o cloudfs ./cmd/cloudfs

# 1. 写配置（示例见下）
mkdir -p ~/.config/cloudfs && $EDITOR ~/.config/cloudfs/config.yaml
./cloudfs config check

# 2. 检查环境
./cloudfs doctor

# 3. 挂载
./cloudfs mount

# 4. 让 agent 用起来
./cloudfs mcp install --client claude   # 打印 .mcp.json 片段
./cloudfs mcp install --client codex    # 打印 ~/.codex/config.toml 片段
```

也可以不安装 Go，直接启动只使用 VFS 的 MCP HTTP 服务（不需要 FUSE 权限）：

```sh
mkdir -p cloudfs-config cloudfs-cache
cp deploy/config.yaml cloudfs-config/config.yaml
# 编辑 cloudfs-config/config.yaml，加入 remotes/layout；示例默认只读且为空。
export CLOUDFS_CONFIG_DIR=./cloudfs-config
export CLOUDFS_CACHE_DIR=./cloudfs-cache
export CLOUDFS_UID="$(id -u)" CLOUDFS_GID="$(id -g)"
export CLOUDFS_MCP_TOKEN="replace-with-a-long-random-token"
docker compose --profile mcp up --build
```

服务只发布到宿主机 `127.0.0.1:8765`，HTTP 客户端须发送
`Authorization: Bearer $CLOUDFS_MCP_TOKEN`。容器内监听非回环地址时 token 是强制项；
它只从环境变量读取，不写入配置。Linux 上的 FUSE Compose profile、镜像直接运行、
发布二进制与权限边界见 [分发与容器](docs/distribution.md)。

## 配置

`~/.config/cloudfs/config.yaml`：

```yaml
cache:
  dir: ~/.cache/cloudfs
  max_size: 200GiB      # 缓存内容预算：块与完整文件
  min_free: 10GiB       # 缓存/日志写入的空间准入门槛，详见缓存管理文档
  block_size: 4MiB
  sub_block_size: 16KiB # 随机读未命中时只取这么多（内核一次随机读正好要 16 KiB）
  write_behind: 256MiB  # 冷读拉回的整块先在内存里服务，后台落盘；超过上限退化为同步写
  max_age: 720h

journal:
  durability: power     # power：close() 等本地 fsync 完成才返回（默认）
                        # crash：不 fsync；进程崩溃不丢，掉电可能丢最后几秒并报为死信

proxy:
  outbounds:
    - { name: clash, type: socks5, addr: 127.0.0.1:7890 }
  groups:
    - { name: proxy, type: fallback, members: [clash],
        check_url: https://www.gstatic.com/generate_204, interval: 60s }
  rules:
    - DOMAIN-SUFFIX,googleapis.com,proxy
    - DOMAIN-SUFFIX,graph.microsoft.com,proxy
    - DOMAIN-SUFFIX,dropboxapi.com,proxy
    - GEOIP,CN,direct
    - FINAL,direct

remotes:
  nas:  { type: webdav, url: 'https://nas.local/dav', user: alice } # 用 config auth nas 保存密码
  shell: { type: sftp, host: 192.168.0.20, user: work, root: ~/data }  # 默认文件连接 2 + 目录连接 2
  # SFTP connections / directory_connections 分别控制两个池；packet_size 默认按服务端自动选

  share: { type: smb, host: 192.168.0.30, share: media, user: work, root: /movies }
  # SMB 不走 HTTP，代理与限流由 daemon 直接注入；password 用 config auth share 保存

  archive: { type: s3, endpoint: 'https://s3.example.com', region: us-east-1,
             bucket: backups, prefix: cloudfs, lookup: path, access_key_id: ACCESS_KEY }
  # 用 config auth archive --stdin 保存 secret_access_key / session_token，不把密钥写进 YAML

  dropbox: { type: dropbox, client_id: APP_KEY }
  # 长驻服务导入 refresh_token（及机密 client_secret）；临时测试也可单独导入 access_token

  gdrive: { type: gdrive, client_id: CLIENT_ID }
  # 可选 drive_id 指定共享云端硬盘；同名子项会让该目录报错，需要先在 Drive 里改名

  work: { type: box, client_id: CLIENT_ID }
  # Box 每次刷新都会轮换 refresh_token，务必让 config auth 写进安全存储

  p115: { type: pan115, qps: { meta: 1, download: 2, upload: 1 } }

mounts:
  - path: /mnt/cloud
    layout:
      /work:  { remote: nas,  root: /work,  mode: writeback, dir_ttl: 1m }
      /media: { remote: p115, root: /media, mode: readonly,  dir_ttl: 24h }

mcp:
  http: 127.0.0.1:8765
  allow: [/work]        # MCP 只能碰这些子树
  read_only: false

control:
  metrics: 127.0.0.1:9101
  ui: true               # 同一回环端口的状态页；设 false 可关闭
                         # 页面可以添加网盘账号（公开字段），但凭据只能用 config auth 设置

webdav:
  http: 127.0.0.1:8080   # 可选：随 mount/mcp 进程启动 WebDAV
  prefix: /dav
  root: /media           # 只暴露这一棵 VFS 子树
  strategy: proxy        # proxy | redirect | auto
  writable: false        # 默认只读；true 才开放 PUT/DELETE/MKCOL/MOVE/COPY/LOCK
```

### 凭据与控制面

控制面支持 `control.socket`（默认 `~/.cache/cloudfs/control.sock`），权限为 0600；
`status` 优先读取运行中服务的状态，服务未运行时只读检查本地上传日志。
`control.metrics` 可同时启用，仅允许绑定回环地址。

MCP-only 模式持有日志时也启动同一控制端点，可用 CLI 管理复制准备和上传队列。
`mcp install` 只打印或写客户端注册配置，不建立 daemon、不启动恢复或上传。

`uploads list/retry/cancel/resume/drop/flush` 同样优先连接运行中的 daemon；离线 `list` 只读日志，
不初始化网盘、不启动上传。`retry` 只重试死信，不能把正在上传的任务重新入队。

```sh
cloudfs uploads list --limit 200 --json    # 返回 next_cursor 时可用 --cursor 续查
cloudfs uploads retry <upload-id>          # 不给 id 则重试当前全部死信
cloudfs uploads cancel <upload-id>         # 持久停止后续尝试并保留本地内容，不撤销远端操作
cloudfs uploads resume <upload-id> --confirm # 校验保留内容后发起新尝试，接受远端重放风险
cloudfs uploads drop <upload-id> --confirm # 先 cancel 并等待 cancelled；永久丢弃本地版本，不撤销远端操作
cloudfs uploads flush --timeout 30m        # 等待延迟重试和正在上传的任务结束
```

`flush` 不绕过限流/退避，不提交仍打开的文件句柄；死信仍存在时返回失败。
在线等待被取消不会停止 daemon 的上传工作。离线 flush 被取消会关闭该次启动的上传器，
未完成任务保留在日志中。`drop --confirm` 经 VFS 核对当前本地版本及全部别名，
有打开读写句柄时拒绝；失败保留 `purging`，可显式重试或由重启续清理。离线只打开
本地存储，不构建网盘/凭据/代理，不恢复其他上传。共享内容和缓存租约可能继续占用
磁盘；已替换或缺失版本的历史清理仍有限制。多用户机器若不需要 TCP 指标，建议只启用
有文件权限保护的 Unix socket。

`cancel` 返回 `cancelling` 表示当前请求尚未退出，`cancelled` 表示任务已停止；两者都
不证明远端文件不存在，也不会回滚已经成功的远端请求。已完成或带删除补偿的任务拒绝
取消。取消记录和完整本地版本会保留，重启不自动重传，`flush` 会报告仍有取消任务。
普通 `retry/drop` 不接受取消记录，相关本地文件的删除／改名也会拒绝。可显式执行
`resume --confirm`：完整校验保留内容的 CRC，确认当前本地版本、可写挂载与目标父 ID
仍匹配，再开始一次新上传；旧会话与分片先存入私有历史，不继续使用不确定的旧会话。
这可能重复或覆盖远端数据，不能代替对账。journal v11 起每条普通上传和复制上传都会
保存原元数据库、挂载前缀、根对象及本地授权代次；任一项变化时，上传器在解析 provider
前将任务转为死信，零远端调用。v10 及更早的无绑定任务同样不会自动发送；确认目标后可
先 cancel，再用 resume --confirm 按当前绑定重新接纳。显式重新授权会换代，自动 token
刷新和 config auth --migrate 保持原代次。该本地围栏不替代云端账号身份核验或远端对账。
离线恢复只重新排队、不启动上传后台，但需要可初始化的当前配置；完整内容校验默认
最多等待 30 分钟，可用 `--timeout` 调整。离线取消仍不初始化 provider 或上传器。
自动远端对账、历史和本地版本清理仍在开发中。

凭据存储支持 `secrets.backend: auto | keyring | file`。`auto` 优先使用系统 keyring，
不可用时采用独立的 0600 文件；`secrets.dir` 可指定文件目录，默认位于配置文件旁的
`secrets/`。配置中使用 `keyring:<key>` 或 `secretfile:<key>` 引用，`doctor` 会提示
明文配置或文件降级。阿里、百度、115 刷新 token 时会保存新值；已有明文 refresh token
在首次轮换时转为引用。`config add/auth/list` 已可用：阿里/百度走浏览器授权，115 走终端
二维码；其他账号可隐藏输入或通过 stdin 导入。授权与系统 keyring 的真实账号验收仍待完成。

```sh
cloudfs config add nas                  # 终端下会逐项询问：先选后端类型，再问该驱动需要的字段
cloudfs config add nas --type webdav --url https://nas.local/dav --user alice   # 或者全部用参数给
cloudfs config auth nas                 # 隐藏输入密码，保存后检查一次根目录
cloudfs config list                     # 不输出凭据值
cloudfs config auth nas --migrate       # 将已有明文凭据迁移到安全存储，不联网
cloudfs config auth nas --check         # 检查已有凭据
```

`config add` 在终端下是交互式的：问题来自各驱动自己声明的字段（`provider.RegisterFields`），
不是 CLI 里按网盘名写死的一张表——那张表会在驱动改动时立刻过时，新驱动也进不去。已经用参数
或 `--set` 给过的字段不会再问一遍。**不在终端时行为完全不变**：不会有任何提问，缺少必填字段
直接报出缺哪几个，脚本不会卡在一个没人回答的问题上。

以上 `add` 示例用于尚未配置的账号，不会覆盖已有同名配置。浏览器授权需要开放平台的
`client_id` / `client_secret`，默认回调为 `http://127.0.0.1:53682/callback`，需在应用侧允许。
`--no-browser` 可打印授权链接后等待回调；授权成功后先保存凭据再检查账号，检查失败不会
丢弃已经获取或轮换的 token。重新授权后需重启使用该账号的 daemon；重启后旧授权代次下
尚未完成的上传会停在死信中，不会通过新账号发送，核对后可显式取消/恢复。

### 缓存固定与解除

```sh
cloudfs pin /work --timeout 30m         # 保存固定意图并完整下载
cloudfs cache pins --json              # 查看路径规则，包括配置中的 pin
cloudfs unpin /work                    # 仅移除该条规则，不删除文件或缓存内容
cloudfs warm /work 2                   # 只预热目录，不下载内容
cloudfs cache stats
cloudfs cache gc
```

这些命令优先管理运行中的服务；离线执行前必须取得存储所有权，不会在另一个服务的
缓存上启动第二套管理栈。pin 在重启时恢复防淘汰保护，并由后台补齐缺失内容；下载失败
保留固定意图和已缓存块，重试只补缺块。空间不足返回错误，不会边淘汰已固定内容边报告成功。
重叠规则共同生效，挂载配置中的 `pin: true` 要通过配置解除。完整语义与限制见
[缓存管理](docs/cache-management.md)。

### 基准测试

```bash
cloudfs bench /mnt/cloud --all --cold --repeat 3 --metrics 127.0.0.1:9101   # 内置基准
scripts/bench/run-matrix.sh /tmp/matrix 192.168.0.20 work '~/test'        # lan/wan/bad 三档
```

`cloudfs bench` 仿 `juicefs bench`：自带数据集，每项前后读 `/metrics` 取后端调用差值，
`--cold` 经 `POST /cache/drop` 从空缓存开始。`cmd/netem` 是链路损伤代理，
`scripts/bench/` 里有 fio 交叉验证。方法、门禁与数据见 `docs/bench.md`、`docs/perf-report.html`。

## 一致性模式

| 模式 | `close()` 返回时机 | 适用 |
|---|---|---|
| `writeback`（默认） | 本地日志提交后 | 日常编辑、agent 写入、IDE |
| `strict` | 远端上传完成后 | 备份脚本、要求跨端立刻可见 |
| `readonly` | 拒绝写（`EROFS`） | 媒体库、被熔断或未授权的账号 |

## 命令

```
mount [path]              挂载并前台运行
umount <path>             卸载
service install|uninstall|status 管理当前用户的 systemd/launchd 挂载服务
mcp --stdio               以 stdio 提供 MCP（Claude Code / Codex 用这个）
mcp --http [addr]         以 Streamable HTTP 提供 MCP（非回环需 CLOUDFS_MCP_TOKEN）
mcp install --client claude|codex [--write <file>]
strm <virtual-path> --out <dir> [--prune] 通过运行中的 WebDAV 生成媒体库 .strm
status [--json]           缓存、上传队列、代理、限流状态
doctor [--fix] [--json]   环境与本地状态诊断
cache stats | gc | pins   查看缓存、回收未固定内容或列出固定规则
uploads list | retry | cancel | resume | drop | flush
find <query>              在本地索引里搜文件名
cp <source> <dest>        复制单个文件，可跨 remote；恢复与竞争限制见 docs/copy.md
copies list | show <id>    查询复制准备状态、检查点及关联上传；list 支持 --limit/--cursor
copies retry|cancel <id>   重试或取消准备任务，保留内容；不能取消已交接的上传
copies forget <id> --confirm 清理无引用的准备内容和记录；不删除本地/远端文件
warm <path> [depth]       预列目录，使后续查找本地化
pin <path>                完整下载并钉住，读取与内容搜索变本地操作
unpin <path>              移除一条固定规则，不删除内容
proxy test <host>         查看某主机走哪个出口
config check [path]       校验配置
config add|auth|list      账号管理、授权、凭据导入与迁移
```

`service install` 读取配置中的第一个 mount，写入当前用户的 systemd unit（Linux）
或 LaunchAgent（macOS），使用当前 cloudfs 可执行文件和配置绝对路径并立即启动。
异常退出会自动重启；正常 SIGTERM 会先走 cloudfs 的卸载路径。Linux user service
默认在用户会话启动，若需无人登录也运行，应由管理员按系统策略启用 linger。
`service uninstall` 先停止 supervisor，检查并清除残留 FUSE 挂载，再删除定义；
不会删除配置、cache、journal 或普通挂载目录。移动二进制或配置后应重新 install。

配置了 `control.metrics` 后，`cloudfs ui` 会在浏览器打开 `http://127.0.0.1:9101/` 的桌面式
控制台（`--print` 只打印 URL）。它是一套嵌进二进制的多文件 Web 应用（ES modules + 一个
设计令牌 CSS，不引入 Node，`go build` 仍是唯一构建），涵盖：连接优先的主窗口与文件浏览、
传输队列、缓存与固定、代理出口、诊断与服务。所有操作走既有和新增的 control 端点
（`/fs/*`、`/search`、`/doctor/*`、`/events` SSE、`/accounts/*`、`/proxy/*`、`/mounts`）。
CSP 收紧为 `script-src 'self'; style-src 'self'`，页面资源按内容哈希带 ETag 版本化。
**凭据永不经过界面**：授权在终端用 `cloudfs config auth` 完成（境外 OAuth/扫码可由守护
进程代跑，秘密值不进浏览器）。`control.ui: false` 只关闭 `/` 与 `/ui/`，不改变 `/status`、
`/metrics` 或 Unix socket。control TCP 仍只接受回环地址。浏览器请求须同源并携带
`X-CloudFS-Control`，跨站 Origin、DNS rebinding Host 与简单表单请求会被拒绝。

配置 `webdav.http` 后，同一个 owner 进程会把 `webdav.root` 作为只读 DAV 根目录输出。
支持有界 PROPFIND、GET/HEAD、Range 和不泄露 provider opaque version 的 ETag；所有读取
仍经过 VFS/cache。非回环监听强制要求 `CLOUDFS_WEBDAV_TOKEN`，客户端可用 bearer 或
Basic 用户 `cloudfs`。内置端点是 HTTP，跨主机应放在 TLS/VPN 后；完整边界见
[只读 WebDAV 输出](docs/webdav-output.md)。
`strategy: proxy` 始终走缓存；`redirect` 只对无额外 header 的安全 http(s) provider link
返回 302；`auto` 在直链需要 UA/Referer、文件尚未上传或链接不可用时自动回落 proxy。

STRM 生成器通过这个正在运行的 WebDAV 做 Depth-1 递归枚举，不另开 daemon，也不绕过
VFS 元数据缓存。例如 `cloudfs strm /media/Films --out /srv/emby/Films`。默认生成的 URL
不含凭据，播放器应配置 Basic 用户 `cloudfs` 和同一个 token；只有客户端无法单独保存认证时，
才显式使用 `--embed-basic-auth`。完整用法和安全边界见 [STRM 媒体库](docs/strm.md)。

上传的 `purging` 表示本地清理尚未结束，不等于远端上传成功；Flush 不会忽略该状态。
在线、离线 `uploads drop <id> --confirm` 已使用同一安全清理流程；确认、重试与历史版本限制见 [上传清理说明](docs/upload-cleanup.md)。

## 状态

| 里程碑 | 内容 | 状态 |
|---|---|---|
| M0 | Provider 接口与能力矩阵、故障注入 fake、配置、错误分类、AIMD 限流、代理规则 | 完成 |
| M1 | SQLite 元数据、4 MiB 块缓存、readahead、预取、VFS 读路径、FUSE 挂载 | 完成 |
| M2 | staging/日志/上传队列、writeback 与 strict、冲突副本、崩溃恢复 | 完成 |
| M3 | 共享 HTTP 层（代理 + 限流 + 熔断）、WebDAV / OpenList 驱动 | 完成 |
| M3 | 阿里云盘 / 百度 / 115 / 夸克 / 天翼 / 123 驱动 | 实现与模拟测试已有，真实账号待验收 |
| M1 | 国外网盘与通用协议：S3 / Dropbox / OneDrive / Google Drive / Box / SFTP / WebDAV / SMB | 全部已接入，真实账号与真实 SMB 服务器待验收 |
| M4 | MCP 服务：29 个工具、资源列举/读取/订阅、允许列表、只读模式、客户端安装 | 复制准备和上传管理已接入；长期负载及上传/Copy 远端对账仍待补，见 docs/mcp.md |
| M5 | pin/hydrate、2Q 淘汰、背压、metrics、doctor、status | 主体已实现，内核及大规模验收待补 |
| M6 | macOS（macFUSE）平台适配、systemd/launchd 用户服务定义 | 实现完成，挂载/重启待真机验证 |

已实现的包：

| 包 | 内容 |
|---|---|
| `internal/provider` | Provider 接口、能力矩阵、注册表、哨兵错误、HTTP 客户端注入契约 |
| `internal/provider/httpx` | 共享 HTTP 客户端：代理路由、限流、熔断、错误分类、Range 读 |
| `internal/provider/webdav` | WebDAV / OpenList 驱动（PROPFIND、Range、MOVE/COPY、ETag 指纹） |
| `internal/provider/sftp` | SFTP 驱动（按偏移分片、原子改名落位、断线重连、known_hosts 校验） |
| `internal/provider/s3` | S3/兼容对象存储（流式 delimiter 列举、Range、multipart 恢复、预签名与服务端 Copy） |
| `internal/provider/dropbox` | Dropbox API v2（分页与 changes、revision Range、临时链接、可恢复 upload session） |
| `internal/provider/onedrive` | Microsoft Graph v1.0（stable ID、delta、预认证 Range、可恢复 upload session） |
| `internal/provider/gdrive` | Google Drive API v3（修订钉住的 Range、changes delta、resumable 上传、同名冲突显式失败） |
| `internal/provider/box` | Box Content API v2（文件/文件夹双命名空间、SHA-1 分片 commit、409 转新版本） |
| `internal/provider/smb` | SMB2/3（go-smb2；句柄缓存、暂存改名发布、断链重挂载） |
| `internal/provider/{aliyun,baidu,pan115,pan123,quark,tianyi}` | 六家国内网盘驱动 |
| `internal/meta` | SQLite 目录树、TTL、负缓存、delta 游标、pin、trigram 文件名索引 |
| `internal/cache` | 4 MiB 块缓存、位图、hydrate、2Q 淘汰、四重限额、内容寻址链接 |
| `internal/journal` | 写日志：staging、流式哈希、崩溃恢复、分片记录、死信 |
| `internal/upload` | 上传队列：秒传、分片续传、会话恢复、冲突副本、AIMD 反馈 |
| `internal/vfs` | inode 树、句柄、读写路径、readahead、后台预取、delta 刷新、一致性模式 |
| `internal/fusefs` | go-fuse 适配、平台选项、errno 映射、xattr 状态、statfs |
| `internal/mcpsrv` | MCP 工具集、允许列表、响应上限、stdio 与 HTTP 传输 |
| `internal/net/{proxy,ratelimit,retry}` | 规则路由与出口组、AIMD 令牌桶与熔断、错误分类与退避 |
| `internal/control` | status、Prometheus 指标、健康端点、doctor 与 --fix |
| `internal/daemon` | 从配置装配整套系统 |
| `test/{fakeprovider,chaos,conformance,perf,e2e}` | 故障注入 provider、可靠性矩阵、POSIX 语义比对、调用次数基线、端到端 |

## 开发

```sh
go build ./...
go test ./...
go test ./test/chaos/        # 可靠性矩阵：断网、kill -9、限流、缓存满、冲突
go test ./test/conformance/  # 与本地目录逐项比对 POSIX 行为
go test ./test/perf/         # 冷热遍历、读放大、预取的调用次数基线
go test ./test/e2e/          # 挂载 + MCP 端到端
go test -race ./...          # 竞态检测
```

本机没有系统 Go 时，仓库里的 `gow` 脚本会用会话临时目录中的工具链。

FUSE 测试需要 `/dev/fuse`（Linux 装 `fuse3`，macOS 装 macFUSE）；缺少时会自动跳过而不是失败。

## 已知限制

- 国内六家驱动的部分 API 细节尚未在真实账号上验证，代码里用 `UNVERIFIED` 标注，见 [docs/providers.md](docs/providers.md)。
- 夸克与 115 的部分接口为非官方，默认采用更保守的限流；官方 Open API 优先。
- 阿里三方权益包、百度非 SVIP 限速是账号侧限制，系统只能降速适配并在 `status` 中明示。
- S3 已通过 SigV4 HTTP 回放和 VFS 读取，但尚未用 AWS、MinIO、R2、OSS 等真实服务验收；
  目录移动由多次 CopyObject 后删除源对象组成，不具备对象存储本身不存在的原子 rename。
- Dropbox 已通过状态化 HTTP 回放和 VFS 读取，但尚未用个人/团队/App Folder 真实账号验收；
  changes cursor/reset 已接入，CLI 仍没有浏览器 OAuth 向导，团队 namespace 也未验证。
- OneDrive 已通过状态化 Graph/CDN 回放和 VFS 读取/增量刷新，但尚未用个人、组织或 SharePoint
  真实账号验收；CLI 尚无 Microsoft 浏览器 OAuth 向导，详见 [docs/onedrive.md](docs/onedrive.md)。
- FUSE passthrough 默认关闭：共享 backing 的缓存租约已修复，读写混用和版本切换仍未完成。`CLOUDFS_EXPERIMENTAL_PASSTHROUGH=1` 仅用于隔离验收，不应用于正常写入工作负载；见 [passthrough 状态](docs/fuse-passthrough.md)。
- Google Drive 已通过状态化 HTTP 回放验收，但尚未用真实账号验证；CLI 无浏览器 OAuth 向导。
  Drive 允许一个目录里存在同名文件，文件系统不能表示，遇到时该目录会明确报错而不是隐藏其中一个；
  Google Docs 等 Workspace 文档没有字节流，不会出现在挂载里。详见 [docs/providers.md](docs/providers.md)。
- Box 已通过状态化 HTTP 回放验收，尚未用真实账号验证；CLI 无浏览器 OAuth 向导。
  Box 没有目录级变更流，所以目录按 TTL 刷新而不是 delta。
- SMB **尚未在任何真实服务器上验收**，当前只有内存共享的行为复现测试。
  SMB 的改名不覆盖已存在的目标，因此发布上传时会先删除目标，中间存在一个"名字暂时不存在"的窗口。
- macOS 走 macFUSE，需要安装并批准系统扩展；FSKit 后端尚未接入。
