# CloudFS

把国内外主流网盘挂载为本地目录的单机守护进程。统一的元数据缓存、块缓存与可靠写入日志抹平各平台 API 的差异；同时通过 MCP 让 Claude Code、Codex 等 agent 直接读写同一份文件系统。

完整设计见 [docs/DESIGN.md](docs/DESIGN.md)。

**让 agent 替你装**：把下面两行粘给 Claude Code / Codex / Gemini CLI，它会按 [INSTALL_FOR_AGENTS.md](INSTALL_FOR_AGENTS.md)
一步步来，该由你做的（装 FUSE、浏览器授权）它会停下来等你。

> Follow https://raw.githubusercontent.com/jiangsong/NexusDrive/main/INSTALL_FOR_AGENTS.md to set up CloudFS.
> Ask me which drive to mount and where before running anything.

## 它解决什么

- **挂载即用**：`ls`、`grep -r`、`git status`、编辑器都能直接工作在网盘目录上，不需要先同步。
- **查找快**：目录树持久化在本地 SQLite，热目录零远端调用；文件名有 trigram 索引。
- **写入可靠**：`close()` 返回时数据已 fsync 并写入日志，之后异步上传。断网、限速、进程被 kill 都不丢数据。
- **不被封号**：按（网盘、账号、请求类别）三维限流，遇 429 或风控自动降速，持续异常则熔断该账号并转只读。
- **代理灵活**：Clash 风格域名规则，API 域与下载 CDN 域可走不同出口，出口组带健康检查与故障转移。
- **Agent 友好**：MCP 工具带分页、字节范围读、响应大小上限、删除确认门禁，不会撑爆 agent 的上下文。

## 快速开始

第一次使用，直接跑 `./cloudfs`（不带任何子命令）：没有配置时它会写一份最小配置、在本机
开一个控制台、打开浏览器，让你一步步加网盘、建池、挂载；已经配好了就直接挂载并打开面板。
`./cloudfs setup` 是同一个流程的显式别名。完整走法见 [从零开始](docs/getting-started.md)，
文档索引见 [docs/](docs/README.md)。下面是手写配置的路子。

```sh
go build -o cloudfs ./cmd/cloudfs

# 1. 写配置（示例见下）
mkdir -p ~/.cloudfs && $EDITOR ~/.cloudfs/config.yaml
./cloudfs config check

# 2. 检查环境
./cloudfs doctor

# 3. 挂载
./cloudfs mount

# 4. 让 agent 用起来
./cloudfs mcp install --client claude   # 打印 .mcp.json 片段（stdio）
./cloudfs mcp install --client codex    # 打印 ~/.codex/config.toml 片段
# 挂载已在运行时，给 agent 签一个只能碰 /work 的令牌，走同一进程的 HTTP：
./cloudfs mcp token create --name claude --read /work
./cloudfs mcp install --client claude --transport http --token <上一步打印的令牌>
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

`~/.cloudfs/config.yaml`。配置、`meta.db`、日志（journal）、agent 数据库、凭据回退文件
和控制 socket 都在 `~/.cloudfs/` 下；只有块缓存可以改到别的盘（`cache.dir`，默认
`~/.cloudfs/cache`）。旧版的 `~/.config/cloudfs` + `~/.cache/cloudfs` 布局会在第一次启动时
自动搬到新位置，块缓存留在原地继续用。

```yaml
cache:
  dir: ~/.cloudfs/cache
  max_size: 200GiB      # 缓存内容预算：块与完整文件
  min_free: 10GiB       # 缓存/日志写入的空间准入门槛，详见缓存管理文档
  block_size: 4MiB
  sub_block_size: 16KiB # 随机读未命中时只取这么多（内核一次随机读正好要 16 KiB）
  write_behind: 256MiB  # 冷读拉回的整块先在内存里服务，后台落盘；超过上限退化为同步写
  max_age: 720h
  policy: # 全局缓存策略默认值；每个 layout.<prefix> 可用自己的 cache 覆盖
    preset: none  # none（默认）| media | photos | code，见下表；显式字段总是赢过 preset
    # small_file_whole: false   # 前台读到一个小文件时，一次把整份取下来并装成完整缓存对象，
                                # 而不是按块取。开启后该文件后续任何位置的读都不再产生远端请求。
                                # 只作用于「大于一个 block 且不超过 small_file_threshold」的文件；
                                # 更大的文件仍按块流式读，以免为一次 seek 拉下整部影片。
                                # 与 dir_readahead 的兄弟预取共用同一把 per-inode singleflight，
                                # 两者不会对同一个文件重复取
    # small_file_threshold: 4MiB  # 多大以内算"小文件"；必须是 64KiB 的倍数。
                                  # 这个值管的是 dir_readahead 的兄弟预取
    # small_file_whole_threshold: 0 # 只管前台整取的上限；0 = 沿用 small_file_threshold。
                                  # 两者分开是因为花的是不同的钱：兄弟预取是投机，源码树上
                                  # 1MiB 就够；前台整取只有在超过 block_size 时才有收益
                                  # （不到一个块的文件本来就是一次请求），所以
                                  # small_file_whole 打开时，实际生效的整取上限必须大于
                                  # block_size，否则启动即报错，而不是留一个什么都不做的开关
    # dir_readahead: 32           # readdir 时预取多少个后续文件；0 = 关闭
    # readahead_max: 64MiB        # 顺序预读窗口上限；必须 >= block_size
    # readahead_request: 0        # 一次合并读请求覆盖多少字节；0 = 按 provider 能力自动推算
    # readahead_lead: 8s          # 预读提前量（尚未消费，供后续任务用）

journal:
  durability: barrier   # barrier（默认）：每次提交一次设备刷新，且刷在写入日志行之后，
                        #   所以这一次同时覆盖暂存数据、rename 和那一行。实测 macOS 上
                        #   约 5.6 ms/小文件。依赖「设备刷新会持久化此前已下发的写」这个
                        #   顺序性质——F_FULLFSYNC 就是这么实现的，但不是 POSIX 保证。
                        # power：暂存文件与 objects 目录各刷一次设备，两次都在写入日志行
                        #   之前，所以不依赖上面那个顺序假设。代价是约 9.72 ms/小文件。
                        #   注意：macOS 上这一档并不比 barrier 更强——SQLite 只在
                        #   PRAGMA fullfsync 打开时才发 F_FULLFSYNC，本仓库没有打开，
                        #   所以日志行本身两档都只走普通 fsync；barrier 那一次设备刷新
                        #   反而覆盖了它。详见 TODO.md T-62。Linux 上 fsync(2) 本就刷
                        #   设备缓存，两档开销相同，只有顺序不同。
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
  # config auth 打开浏览器完成授权；可选 drive_id 指定共享云端硬盘
  # 同名子项会让该目录报错，需要先在 Drive 里改名

  work: { type: box, client_id: CLIENT_ID }
  # config auth 打开浏览器完成授权
  # Box 每次刷新都会轮换 refresh_token，务必让 config auth 写进安全存储

  p115: { type: pan115, qps: { meta: 1, download: 2, upload: 1, transfer: 4 }, max_conns: 2,
          upload_workers: 2 }
  # upload_workers 覆盖该网盘的并行上传数（0..256）；0 或省略表示用驱动自己的
  # Caps.UploadParallel。LAN 上的 SFTP/SMB 可以调高，风控严的网盘调到 1~2
  # max_conns 覆盖该网盘的 Caps.MaxConnsPerHost（同时限制传输层的连接数）；
  # 115 是非官方接口，调低比驱动自带的默认值更保守，能进一步降低风控概率
  # qps.transfer 单独限制 CDN 字节流的 ranged GET，不再挤占 download 桶；
  # 省略或设为 0 表示不单独限流，与 download 共用一个令牌桶（旧行为）

mounts:
  - path: /mnt/cloud
    layout:
      /work:  { remote: nas,  root: /work,  mode: writeback, dir_ttl: 1m }
      /media: { remote: p115, root: /media, mode: readonly,  dir_ttl: 24h,
                cache: { preset: media } }  # 大文件顺序播放：大窗口、大合并请求、关闭目录预取
      # cache.policy 的 preset 可选 none/media/photos/code：
      #   media  → readahead_max 128MiB, readahead_request 16MiB, dir_readahead 0, 未设 dir_ttl 时默认 24h
      #   photos → dir_readahead 64,     small_file_threshold 8MiB, readahead_max 16MiB
      #   code   → dir_readahead 128,    small_file_threshold 1MiB, small_file_whole 开,
      #            small_file_whole_threshold 8MiB（必须大于 block_size 才会真的生效）,
      #            未设 dir_ttl 时默认 1m
      # layout.<prefix>.cache 里的字段覆盖 cache.policy 的全局默认，两边都可以单独指定 preset

mcp:
  http: 127.0.0.1:8765
  allow: [/work]        # MCP 只能碰这些子树；令牌与会话只能在其内收窄
  read_only: false
  workspace: /work/.agent   # agent 会话的交付目录，每会话一个子目录；省略时取第一个 allow 前缀 + /.agent
  audit:
    retain: 2160h       # 工具调用审计（agent.db）的保留期，默认 90 天
  session:
    idle: 30m           # HTTP 令牌会话空闲多久自动结束并轮转，默认 30 分钟
    retain: 720h        # 会话操作记录与变更记录（history / last_writer）的保留期，默认 30 天
    retain_blobs: 168h  # 回滚用前像内容的保留期，默认 7 天；过期后记录仍在，回滚跳过
    max_preimage_bytes: 32MiB   # 超过此大小的文件写前不留前像，无法回滚，默认 32 MiB
    preimage_files: 500 # 一次递归删除最多为多少个已缓存文件保留前像，默认 500
  limits:
    max_tokens: 20000   # 单个工具结果的 token 预算（估算），默认 20000；负数关闭
  install:
    transport: auto     # cloudfs mcp install 的默认传输：auto（守护进程在线选 http）/ stdio / http

hooks:
  context: minimal      # agent 客户端 hook 每轮注入：off / minimal / full（加 MEMORY.md 前 30 行）

control:
  metrics: 127.0.0.1:9101
  ui: true               # 同一回环端口的状态页；设 false 可关闭
                         # 页面可以添加网盘账号（公开字段），但凭据只能用 config auth 设置

search:
  crawl:                 # 文件名索引只覆盖列举过的目录；后台爬取器把没打开过的目录补进来
    enabled: false       # 默认关：不产生任何远端调用。开了以后在前台空闲 idle_after 之后开始
    remotes: [nas]       # 只爬这些 remote；省略表示所有已挂载的 remote
    exclude: ["node_modules", "/work/build/*"]  # path.Match 模式，对虚拟路径和目录名各试一次
    idle_after: 30s      # 前台 IO 安静多久才开始一轮；有前台请求时让路
    rescan: 5m           # 一轮跑完后每隔多久再找被 delta 标为 stale 的目录补列
                         # 非官方接口（如 quark）触发风控后休眠 15 分钟；进度就是元数据库里的
                         # 目录列举状态，重启后续跑，不重复列举已完整的目录

index:                   # 内容索引（PDF / Office / 文本抽取 + 全文检索），给 semantic_search 与界面"内容"搜索用
  enabled: false         # 默认关：不建 index.db，MCP 也不注册 5 个索引工具
  pinned: true           # 已固定且完整缓存的文件直接从缓存抽取，零远端调用；非官方接口的网盘只建议用这一项
  rules:                 # 主动拉取并索引的子树；按小时预算下载，前台 IO 繁忙时让路，风控后休眠 15 分钟
    - path: /work/docs
      include: ["**/*.md", "**/*.pdf", "**/*.docx"]   # 相对 path 的 glob；省略时为文本、代码与 Office 的默认集合
      exclude: ["**/drafts/**"]
      max_file_size: 20MiB                              # 超过的文件不下载、不抽取
  exclude: ["**/.env", "**/*.pem", "**/id_rsa*", "**/.git/**", "**/node_modules/**"]  # 全局排除，省略即这组默认值
  max_text_bytes: 2MiB   # 单个文件最多抽多少文本
  max_total_text: 4GiB   # 整个索引的文本上限，到了就暂停并在 doctor 里提示
  fetch_budget: 2GiB/h   # 规则每小时最多下载多少；Caps.Tier=unofficial 的网盘自动减半
  max_chunks: 200000     # 最多嵌入多少分块；超出的分块仍可关键词检索
  embedding:             # 语义检索的嵌入端点；默认 provider: none，没有任何内容离开本机
    provider: ollama     # none | openai（任何 /embeddings 兼容服务）| ollama（/api/embed）
    base_url: http://127.0.0.1:11434   # 省略时 openai 为 https://api.openai.com/v1，ollama 为本机 11434
    model: nomic-embed-text
    # api_key: keyring:index.embedding   # 只接受 keyring:/secretfile: 引用，由 `cloudfs index auth` 写入；YAML 里写明文会被拒绝
    # dimensions: 512    # openai 的 dimensions 参数（默认 512）；ollama 没有这个参数，保持 0
    # allow_remote: true # 端点不在回环 / 内网 / .local 时必须显式确认：每个被索引的分块都会发到那里
    batch: 64            # 每次请求带多少段文本；concurrency 2、qps 4、timeout 30s、quantize int8 是默认值

memory:                  # Agent 记忆库：<root>/memory/<agent>/{MEMORY.md, facts/<name>.md} 的普通 Markdown
  root: /work/.agent     # 省略时取 mcp.workspace，再退到第一个 mcp.allow 前缀 + /.agent；必须在 allow 内
  max_fact_bytes: 64KiB  # 一条记忆的上限（含 frontmatter）
  max_agent_bytes: 32MiB # 一个 agent 的 facts/ 总量上限

triggers:                # 事件触发器：文件变化 → 本机命令或签名 webhook；只在拥有存储的 mount 进程里运行
  - name: inbox-to-agent # 名字只能是 [a-z0-9-]，是投递记录、控制台与 CLI 里的标识
    paths: ["/work/inbox/**"]        # 虚拟路径 glob（`**` 跨目录，`*` 不跨），与 index.rules 同一套匹配器
    events: [create, write, rename]  # write/create/mkdir/remove/rename/remote/rescan；省略 = 全部
    origins: [kernel, remote]        # kernel/api/remote；排除 api 就不会被 agent 自己的写入触发（否则启动时 warning）
    debounce: 2s                     # 同一路径在窗口内的多次变化合并成一次投递
    on_rescan: ignore                # 队列溢出/路径无法解析时的整树 rescan：deliver（默认，一行 path=""）| ignore
    action:
      exec:
        command: ["/usr/local/bin/summarize", "{path}"]  # 无 shell；{path}/{kind}/{uri} 只能是独立的 argv 元素
        cwd: ~/work
        timeout: 10m                 # 超时杀整个进程组；stdout/stderr 各截 64 KiB 存进投递记录
  - name: notify
    paths: ["/work/reports/**"]
    events: [write]
    action:
      webhook:
        url: https://hooks.example/cloudfs   # 只许 https 或回环 http；其它 http 要 insecure: true
        secret: keyring:cloudfs/hook         # 必须是 keyring:/secretfile: 引用，不能写明文
        timeout: 15s
        include_download_url: false          # true 才把签名直链交给接收方
        proxy: direct                        # 出站走 proxy 的哪个出口；省略按 proxy.rules
agents:                  # 控制台"发送给 Agent"浮层里可以"运行"的本机命令；{prompt} 只有这里能用
  - name: claude
    exec: { command: ["claude", "-p", "{prompt}"], cwd: "~", timeout: 30m }   # 选中的路径逐个追加为独立 argv

webdav:
  http: 127.0.0.1:8080   # 可选：随 mount/mcp 进程启动 WebDAV
  prefix: /dav
  root: /media           # 只暴露这一棵 VFS 子树
  strategy: proxy        # proxy | redirect | auto
  writable: false        # 默认只读；true 才开放 PUT/DELETE/MKCOL/MOVE/COPY/LOCK
```

### 凭据与控制面

控制面支持 `control.socket`（默认 `~/.cloudfs/control.sock`），权限为 0600；
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
cloudfs uploads flush --timeout 30m        # 等待延迟重试和正在上传的任务结束（等待期间实时刷新进度行）
cloudfs uploads watch                      # 跟踪队列直到排空；有死信则退出码非零
cloudfs status --watch                     # 先打完整状态，再跟踪上传队列
```

挂载目录支持符号链接，npm 的 `.bin` 和 workspace 链接可以原地创建、执行；远端以
`.rclonelink` 普通文件保存链接目标。该后缀为保留名称，详见[符号链接说明](docs/symlinks.md)。

**往挂载点里拷贝是两段的**：`cp` 返回时数据只是本地持久化（已 fsync 进日志），上传在后台继续。
实测 3434 个文件前台 37 秒、后台约 20 分钟。前台那 37 秒也不是白花的：`journal.durability=power`
下每个文件都要一次 `F_FULLFSYNC`（本机实测 4.07 ms，普通 `fsync(2)` 只要 74 µs），串行拷贝小文件
约 9.72 ms 一个——这是"close() 返回即不怕断电"的价钱，不是开销。进度条跟踪的是慢的那一半，
`cloudfs uploads watch` 就是为这后半段准备的：

```
uploading  42.3%  1.2 GiB/2.8 GiB  431/1024 files  12.4 MiB/s  ETA 2m10s
```

它只读 `/status`，可以同时开多个，中断它不会影响上传。进度按字节算（没有字节的批次按文件数算），
和控制台的进度条同一套算法。注意它跟踪的是**整个上传队列**：同时跑两个拷贝会合成一条进度，
队列里没有区分来源的信息。守护进程重启后重新计数，此时进度行会标注"queued before the restart"。
死信（`Dead > 0`）或整条队列卡在失败的目录创建后面时，watch 会以非零退出码结束，
所以 `cp -r ... && cloudfs uploads watch` 能在脚本里当成真正的完成判定。

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
在首次轮换时转为引用。`config add/auth/list` 已可用：**凡是有 OAuth profile 的后端**
（`daemon.OAuthProfileFor` 那张表，`cloudfs config auth <名字>` 自己据此判断）走浏览器授权，
115 走终端二维码；其他账号可隐藏输入或通过 stdin 导入。授权与系统 keyring 的真实账号验收
仍待完成。

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
`client_id`；以 PKCE 公共客户端授权的后端（当前是 Dropbox）**不需要 `client_secret`**，其余
后端缺 `client_secret` 时由命令询问并写进安全存储。默认回调为
`http://127.0.0.1:53682/callback`，需在应用侧允许。
`--no-browser` 可打印授权链接后等待回调；授权成功后先保存凭据再检查账号，检查失败不会
丢弃已经获取或轮换的 token。重新授权后需重启使用该账号的 daemon；重启后旧授权代次下
尚未完成的上传会停在死信中，不会通过新账号发送，核对后可显式取消/恢复。

### 文件名搜索

```sh
cloudfs find report                     # 名字含 report；多个词做 AND，"my report" 引号包住才含空格
cloudfs find '*.md' --size '>1m'         # 通配 + 大小；k/m/g 二进制单位，a..b 闭区间
cloudfs find --ext go,md --after 2026-09-01 --sort mtime   # 没有词也行：只用过滤条件
cloudfs find 'ext:go size:>1k dm:>2026-09 type:file'      # 同样的过滤写进查询串也可以
cloudfs find --type dir --path /work --limit 50 --json
cloudfs find plan --all                  # 先把索引里还没有的目录都列举一遍再搜
```

索引只覆盖列举过的目录：从没打开过的目录里的文件搜不到。空结果时 stderr 会给出"已列举 /
已知"目录数；`cloudfs warm --all`、界面"索引整棵树"或配置 `search.crawl.enabled: true`
把差距补上。`--sort` 只作用于已收集的那批结果，命中工作预算（stderr 提示）时"最大的 N 个"
不是全局最大的。有守护进程在跑就问它；没有时只读本地索引，不启动上传与刷新，`--all`
则必须有运行中的守护进程或取得存储所有权。语法全表见 `docs/mcp.md`。

### 内容索引

```sh
cloudfs index status                      # 文档数、待抽取、失败、文本占用、本小时下载
cloudfs index add /work/docs --include '**/*.pdf,**/*.docx'
cloudfs index search 季度 复盘 --path /work  # 关键词都要出现；命中带标题路径、片段与偏移
cloudfs index status --path /work/docs/plan.pdf   # ok / pending / failed / uncovered
```

`index.enabled: true` 后守护进程把 `index.pinned`（已完整缓存的固定文件，零下载）与 `index.rules`
（按小时预算下载）覆盖的文件抽成文本，写进独立的 `<cache.dir>/index.db`；MCP 多出
`semantic_search`、`index_status`、`index`、`unindex`、`read_extracted_text` 五个工具，控制台多出
「索引」屏，主窗口搜索框多出"文件名 / 内容"切换，PDF 与 Office 文件在检查器里可以"查看抽取文本"。
不做 OCR。抽取器对 zip 条目数、解压量与 PDF 解析都有上限，坏文件只会让自己 `failed`；抽取中断电或
`kill -9`，重启后队列续跑。接口与 agent 使用建议见 `docs/mcp.md`"内容索引"。

### 语义检索与记忆库

```sh
cloudfs index auth --key-file ~/openai.key     # 不带参数时走隐藏提示，也可以 `cat key | cloudfs index auth`；写 keyring: 引用进配置
cloudfs index embedding --check                # provider / 模型 / 维度 / 主机 / 健康 / 已嵌入 / 待嵌入 / 本月字符；--check 真的调一次
cloudfs index search 复盘 季度目标 --mode hybrid   # mode_used 说明真正跑的是 hybrid 还是降级后的 keyword
cloudfs memory agents                          # 谁有记忆、各占多少
cloudfs memory put style --agent claude-code --description "代码风格" <<'EOF'
Go 代码统一 gofmt，注释用英文。
EOF
cloudfs memory get style --agent claude-code   # 正文原样输出
cloudfs memory search gofmt --agent claude-code
```

**语义检索**：配置 `index.embedding` 后，索引 worker 把每个分块发到端点换成向量（int8 存进 `index.db` 的
`vectors` 表），`semantic_search` / `cloudfs index search` / 界面"语义"模式默认按 `hybrid`（bm25 与向量 cosine 做
RRF 融合）检索，同义与跨语言表达也能命中。**默认 `provider: none`，什么都不外发**；一旦配了端点，每个被索引的
分块（包含文件路径/标题与章节标题）与每条语义查询都会发到 `base_url` 所指主机——端点不在本机 / 内网时必须 `allow_remote: true`，控制台「索引」屏会
常驻一条不可关闭的黄色横幅说明"文件内容会发送到 <host>"。本机 ollama 是零外发的选择。端点连续失败会熔断 60 s，
期间以及尚未嵌入完成、换了模型还没重嵌时，检索自动按关键词执行并在 `degraded` 里说明，不报错；换 `model` 会清空
向量并把全部分块重新排队（`chunks_fts` 不动）。`cloudfs doctor` 检查端点可达与维度一致。

**记忆库**：Claude Code / Codex 这类 agent 的记忆本来是本机文件，换台机器就没了。CloudFS 把它约定成网盘上的普通
Markdown：`<memory.root>/memory/<agent>/MEMORY.md` 是索引（每条记忆一行），`facts/<name>.md` 是正文（带
`name / description / type / scope / source / expires / updated_at` frontmatter），v2 的 `memory/<owner>/shared/` 是本人多 agent 共用区，`memory/shared/` 是整盘共用区。MCP 提供
`memory_list / get / put / delete / merge / search / propose / candidates / review`（候选必须显式确认才成为 durable fact；`put` 带 `expected_version` 做乐观并发，`get` 列出网盘留下的冲突副本
`conflicts[]`，`search` 是限定在该 agent 目录下的 `semantic_search`，记忆树由内置索引规则自动覆盖）；控制台「Agent」屏
的"记忆"标签能看、编辑、删除、合并冲突；`cloudfs memory` 在终端做同样的事；你也可以直接 `cat` / 编辑挂载点上的那个
文件。跨设备同步就是网盘同步，两边同时写时输掉的一方以冲突副本形式保留在同目录。`memory.root` 必须在 `mcp.allow` 内，
`--read-only` 的服务只读不写。

### 事件触发器与发送给 Agent

```sh
cloudfs triggers list                            # 配置里的规则（只读；webhook 只显示 URL 与"已配置密钥"）
cloudfs triggers deliveries --rule inbox-to-agent --state dead   # 投递记录：pending / running / done / dead
cloudfs triggers show 42                         # 一条投递的 stdout/stderr 或 webhook 响应
cloudfs triggers test inbox-to-agent /work/inbox/a.md --confirm  # 立刻投递一次（exec 会真的执行）
cloudfs triggers retry 42                        # 重新排队一条 dead 投递
```

规则只能在配置文件里改：控制台「触发器」屏（`#/triggers`）、控制面 `/triggers/*` 与 CLI 都是只读视图加"测试投递"
与"重试"两个动作，命令白名单就是配置文件本身。投递**至少一次**：记录先落进 `agent.db` 再执行，进程在执行中被
`kill -9`，重启后同一条会再跑一次（`attempts` 加一），所以命令自身要幂等；变更队列溢出时被压掉的事件只以一条
`path=""` 的 rescan 送达（`on_rescan: ignore` 可关掉）。失败按 1 s→5 min 退避重试 8 次后标 `dead`，控制台导航徽标
显示 dead 数。exec 的子进程环境只有 `PATH`/`HOME`/`LANG` 与 `CLOUDFS_PATH`/`CLOUDFS_KIND`/`CLOUDFS_URI`。

配置了 `agents:` 后，主窗口检查器与内容搜索结果行的"发送给 Agent"浮层多出 agent 下拉与"运行"按钮：键入 agent 名
确认后 `POST /agent/invoke` 把提示词作为 `{prompt}`、选中路径作为独立 argv 交给该命令，投递记在
`rule=agent:<name>` 下，审计里是 `principal=console`、`tool=agent.invoke`；失败不重试，重启后提示词不保留，需重新运行。

webhook 请求体是 `{rule, path, uri, kind, origin, ts, size?, download_url?}`，头 `X-CloudFS-Timestamp`（Unix 秒）与
`X-CloudFS-Signature: sha256=<hex(HMAC-SHA256(secret, ts + "." + body))>`。接收端这样校验（与
`internal/trigger.Verify`、控制台空状态展示的片段一致）：

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

### 缓存固定与解除

```sh
cloudfs pin /work --timeout 30m         # 保存固定意图并完整下载
cloudfs cache pins --json              # 查看路径规则，包括配置中的 pin
cloudfs unpin /work                    # 仅移除该条规则，不删除文件或缓存内容
cloudfs warm /work 2                   # 只预热目录，不下载内容
cloudfs warm --all                     # 列举所有挂载里索引还没有的目录；非官方接口的网盘留意风控
cloudfs cache stats
cloudfs cache gc
```

**要在网盘上跑 `git status`、构建、`grep -r` 这类命令，先 pin 那棵子树。** 它们是乱序读整棵树的，
而目录顺序读才有兄弟预取（`cache.policy.dir_readahead`）；不 pin 的话每个文件都是一次远端请求。

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
| `writeback`（默认） | 本地日志提交后（`mkdir` 也是：目录先在本地生效，后台再到网盘创建） | 日常编辑、agent 写入、IDE |
| `strict` | 远端上传完成后（`mkdir` 等网盘创建完成） | 备份脚本、要求跨端立刻可见 |
| `readonly` | 拒绝写（`EROFS`） | 媒体库、被熔断或未授权的账号 |

## 存储池：把所有网盘融合成一块盘

```yaml
remotes:
  ali:  {type: aliyun}
  gd:   {type: gdrive}
  home: {type: pool, pool: home}
pools:
  home:
    members: [{remote: ali}, {remote: gd}]
    replicas: 3
    read_fanout: auto   # off | auto | all：块读如何分到副本
mounts:
  - path: /mnt/cloud
    layout:
      /: {remote: home}
```

一个目录、N 份副本、任一网盘掉线不影响使用、坏了自动在其它网盘重建、本地只做热缓存；大文件的并行块读分散到各个副本（`read_fanout`）；每个网盘上看到的仍是真实文件。界面「存储池」一屏或 `cloudfs pool …` 管理；第一次使用见 [从零开始](docs/getting-started.md)，配置项与内部机制见 [存储池](docs/pool.md)。

**已实现**：目录读序小文件预取、可恢复的 `cloudfs export`、按路径放置规则及 rebalance。仍待真实账号和各平台运行验收的项目以文档中的 `UNVERIFIED` 标注；完整状态见 [存储池 v2](docs/pool-v2.md)。配置形如：

```yaml
pools:
  home:
    rules:
      - {prefix: /photos, replicas: 2, prefer: [nas]}
    failure_domain: account
export:
  transfers: 4
  streams: 4
  memory_budget: 512MiB
```

## 命令

```
mount [path]              挂载并前台运行
umount <path>             卸载
service install|uninstall|status 管理当前用户的 systemd/launchd 挂载服务
mcp --stdio               以 stdio 提供 MCP（Claude Code / Codex 用这个；挂载已在运行时请改用 HTTP）
mcp --http [addr]         以 Streamable HTTP 提供 MCP（非回环需令牌；回环在签发第一个令牌后也只认令牌）
mcp install --client claude|codex [--transport stdio|http] [--url <url>] [--token <token>] [--write <file>]
                          打印或写入客户端注册片段；--transport http 附带可直接执行的 claude mcp add 命令
mcp token create --name N [--read P,..] [--write P,..] [--read-only] [--ttl 720h]
                          签发作用域访问令牌，明文只打印一次
mcp token list | revoke <name|id> --confirm   列出（只显示指纹）或吊销令牌，吊销同时关闭其会话
audit [--session ID] [--tool T] [--result ok|denied|error] [--since 1h] [--limit N] [--json]
                          查看 MCP 工具调用审计，最新在前；daemon 未运行时直接读 agent.db
sessions list [--state active|finished|expired|rolled_back] | show <id> | finish <id> [--summary text]
                          查看 agent 会话及其作用域、产物；finish 需要运行中的 daemon
sessions rollback <id> --dry-run | --confirm [--json]
                          撤销会话经 MCP 写工具做的修改：--dry-run 只打印将恢复/跳过/冲突三组，--confirm 执行；需要运行中的 daemon
strm <virtual-path> --out <dir> [--prune] 通过运行中的 WebDAV 生成媒体库 .strm
status [--json] [--watch] 缓存、上传队列、代理、限流状态；--watch 打完快照后继续跟踪上传队列直到排空
ui | open [--print]       在浏览器打开桌面式控制台（需 control.metrics）
doctor [--fix] [--json]   环境与本地状态诊断
cache stats | gc | pins   查看缓存、回收未固定内容或列出固定规则
uploads list | watch | retry | cancel | resume | drop | flush
                          watch 跟踪整个队列直到排空；有死信或有卡住的行时退出码非零
find [query] [--ext go,md] [--size >1m] [--after 2026-09-01] [--type dir|file] [--sort name|size|mtime|path] [--path /sub] [--limit N] [--all] [--json]
                          在本地文件名索引里搜索（Everything 式语法，见下文）；--all 先列举整棵树
index status [--path P] [--json]   内容索引概况（文档 / 分块 / 队列 / 预算）；--path 看单个路径是否覆盖、已索引还是失败
index rules [--json]      列出索引规则及来源（配置文件 / 控制台 / Agent 工具）与已覆盖文档数
index add <path> [--include "**/*.md,**/*.pdf"] [--max-file-size 20MiB]
                          运行时加一条规则并立即入队；需要运行中的守护进程
index rm <path> --confirm 移除运行时规则并丢弃只有它覆盖的文本；配置文件里的规则只能改配置
index rebuild --confirm   清空索引并按规则重新下载、抽取
index retry [path]        把失败文档（可限定子树）重新排队
index search <query> [--path P] [--mode keyword|hybrid|vector] [--limit N] [--json]
                          在抽取文本里检索；没有守护进程时只读 index.db。未配置嵌入端点时 hybrid/vector 降级为 keyword 并在 degraded 里说明
index embedding [--check] [--json]
                          嵌入端点状态（provider / 模型 / 维度 / 主机 / 健康 / 已嵌入 / 本月字符与费用估算）；--check 真实调用一次端点
index auth [--key-file F] 把嵌入 API key 存进密钥库并在 index.embedding.api_key 写引用；key 来自 --key-file、隐藏提示或 stdin 管道，绝不接受命令行参数
triggers list | deliveries [--rule R] [--state S] [--limit N] [--cursor C] | show <id> [--json]
                          事件触发器的规则（只读）与投递记录；没有守护进程时只读 agent.db
triggers test <rule> <path> --confirm | retry <id>   立刻投递一次 / 重排一条 dead 投递；需要运行中的守护进程
memory agents             列出有记忆的 agent：条数、占用 / 上限、冲突副本数；需要运行中的守护进程
memory list [--agent A] [--cursor C] [--limit N] [--json]
                          列一个 agent 的记忆（名称 / 描述 / 类型 / 更新时间 / 冲突副本）；--agent 默认 shared
memory get <name> [--agent A] [--json]
                          原样打印一条记忆的正文（可管道）；--json 带 version 与 conflicts
memory put <name> [--agent A] [--file F] [--description D] [--type T] [--append] [--expected-version V]
                          写一条记忆，正文来自 --file 或 stdin；--expected-version 与当前版本不同则拒绝
memory delete <name> [--agent A] --confirm
                          删除记忆文件与 MEMORY.md 里指向它的行（远端同样删除）
memory search <query> [--agent A] [--no-shared] [--mode keyword|hybrid|vector] [--limit N] [--json]
                          在一个 agent 的记忆（默认加 shared）里检索
cp <source> <dest>        复制单个文件，可跨 remote；恢复与竞争限制见 docs/copy.md
copies list | show <id>    查询复制准备状态、检查点及关联上传；list 支持 --limit/--cursor
copies retry|cancel <id>   重试或取消准备任务，保留内容；不能取消已交接的上传
copies forget <id> --confirm 清理无引用的准备内容和记录；不删除本地/远端文件
export <vpath>... <dir>   【规划中，T-32】把虚拟路径批量导出到本地/移动硬盘：多文件多流并发、续传、拔盘暂停、校验
exports list|show|pause|resume|cancel|forget <id>  【规划中，T-32】导出作业管理
pool rebalance <pool>     【规划中，T-33】按目标偏斜在成员间搬迁副本；加盘后 backfill
warm <path> [depth]       预列目录，使后续查找本地化；不给 depth 即整棵子树
warm --all                列举索引里还没有的每个目录（所有挂载），与界面"索引整棵树"同一实现
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
传输队列、缓存与固定、代理出口、诊断与服务。诊断页可安装/卸载开机自启服务，并可一键**重启守护进程**
（改了 remote、mount 或无热更能力的代理段后让改动生效）。所有操作走既有和新增的 control 端点
（`/fs/*`、`/search`、`/doctor/*`、`/events` SSE、`/accounts/*`、`/proxy/*`、`/mounts`、`/service/*`、`/daemon/restart`）。
CSP 收紧为 `script-src 'self'; style-src 'self'`，页面资源按内容哈希带 ETag 版本化。
**凭据永不经过界面**：授权在终端用 `cloudfs config auth` 完成（境外 OAuth/扫码可由守护
进程代跑，秘密值不进浏览器）。`control.ui: false` 只关闭 `/` 与 `/ui/`，不改变 `/status`、
`/metrics` 或 Unix socket。control TCP 仍只接受回环地址。浏览器请求须同源并携带
`X-CloudFS-Control`，跨站 Origin、DNS rebinding Host 与简单表单请求会被拒绝。

界面与守护进程都支持中文和英文。页面右上角的语言选择保存在浏览器里，没选过时按浏览器的
`Accept-Language` 判断；选定后每个请求都带 `?lang=`，所以诊断结论、网盘配置项提示、状态告警
这些由守护进程生成的句子也跟着换语言。命令行与桌面壳读 `CLOUDFS_LANG`（其次 `LC_ALL`、
`LC_MESSAGES`、`LANG`），例如 `CLOUDFS_LANG=en cloudfs doctor`。没有对应译文时回落到中文，
两张表都缺的键会原样显示键名——这是有意的：宁可看见 `doctor.cache.writable` 也不要空白。

想要一个原生窗口而不是浏览器标签，可另外构建桌面壳
`go build -tags desktop ./cmd/cloudfs-desktop`（cgo，依赖系统 WebView：Linux 的 WebKitGTK、
macOS 的 WKWebView、Windows 的 WebView2）。它不内嵌第二份守护进程、也不自己服务静态资源：
找到在跑的守护进程就把窗口指向它的控制台 URL，找不到就用你的配置起一个（通过
`CLOUDFS_CONTROL_UI` 交给它一个回环 TCP 地址），只经 unix socket 暴露的守护进程则由壳内的
回环反向代理转接。单实例用文件锁，第二次启动会唤起已有窗口而不是再开一个。守护进程本身仍是
`CGO_ENABLED=0` 静态二进制，桌面壳只是可选的额外产物。

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
| M4 | MCP 服务：54 个工具（全能力装配时；记忆 / 索引 / 导出 / 会话等按装配条件注册）、资源列举/读取/订阅、允许列表、只读模式、客户端安装 | 复制准备和上传管理已接入；长期负载及上传/Copy 远端对账仍待补，见 docs/mcp.md |
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
  changes cursor/reset 已接入，团队 namespace 未验证。`cloudfs config auth` 会打开浏览器完成
  授权：Dropbox 以 PKCE 公共客户端授权，**不需要 client secret**，配置里填好 client_id
  （App key）即可。
- OneDrive 已通过状态化 Graph/CDN 回放和 VFS 读取/增量刷新，但尚未用个人、组织或 SharePoint
  真实账号验收；CLI 尚无 Microsoft 浏览器 OAuth 向导，详见 [docs/onedrive.md](docs/onedrive.md)。
- FUSE passthrough 默认关闭：共享 backing 的缓存租约已修复，读写混用和版本切换仍未完成。`CLOUDFS_EXPERIMENTAL_PASSTHROUGH=1` 仅用于隔离验收，不应用于正常写入工作负载；见 [passthrough 状态](docs/fuse-passthrough.md)。
- Google Drive 已通过状态化 HTTP 回放验收，但尚未用真实账号验证。`cloudfs config auth` 会打开
  浏览器完成授权（授权请求带 `access_type=offline` 与 `prompt=consent`，否则 Google 不下发
  refresh token）；只需先在配置里填好 client_id，client_secret 由命令询问并写进安全存储。
  Drive 允许一个目录里存在同名文件，文件系统不能表示，遇到时该目录会明确报错而不是隐藏其中一个；
  Google Docs 等 Workspace 文档没有字节流，不会出现在挂载里。详见 [docs/providers.md](docs/providers.md)。
- Box 已通过状态化 HTTP 回放验收，尚未用真实账号验证。`cloudfs config auth` 会打开浏览器完成
  授权，请求 `root_readwrite` 权限；client_secret 由命令询问并写进安全存储。
  Box 没有目录级变更流，所以目录按 TTL 刷新而不是 delta。
- SMB **尚未在任何真实服务器上验收**，当前只有内存共享的行为复现测试。
  SMB 的改名不覆盖已存在的目标，因此发布上传时会先删除目标，中间存在一个"名字暂时不存在"的窗口。
- macOS 走 macFUSE，需要安装并批准系统扩展；FSKit 后端尚未接入。
