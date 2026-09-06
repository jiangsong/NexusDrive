# CloudFS 挂载盘系统设计方案

版本 0.3 · 2026-09-02 · 状态：M0–M6 全部完成，九个后端已接入

---

## 0. 一页摘要

CloudFS 是一个单机守护进程，把国内外主流网盘挂载为本地目录（Linux / macOS，FUSE），并通过 MCP 让 Claude Code、Codex 等 agent 直接读写。它由五层组成：

| 层 | 职责 | 关键决策 |
|---|---|---|
| 接入层 | FUSE 挂载、MCP 服务、CLI、控制 API | go-fuse v2 直接讲 FUSE 协议；MCP 直连 VFS 不经过 FUSE |
| VFS 核心 | inode 树、句柄、一致性模式 | close-to-open 语义；目录级 `writeback / strict / readonly` |
| 缓存与日志 | 元数据 SQLite、4 MiB 块缓存、写日志与上传队列 | 元数据持久化；块缓存可限额可校验；写入先落本地日志再异步上传 |
| Provider | 统一接口 + 能力矩阵 | 国外与通用协议复用 rclone 后端；国内六家按官方 Open API 自研 |
| 网络层 | 代理规则路由、限流、熔断、重试分类 | Clash 风格域名规则；(网盘, 账号, 请求类别) 三维 token bucket + AIMD |

设计目标按优先级：数据不丢 > 不被封号 > 查找与写入快 > 功能覆盖广。

---

## 1. 需求与约束

### 1.1 功能需求

1. 挂载国内网盘（阿里云盘、百度网盘、115、夸克、天翼云盘、123 云盘）、国外网盘（Google Drive、OneDrive、Dropbox、Box）与通用协议（S3、WebDAV、SMB、SFTP）。
2. 国内环境访问国外网盘走代理，代理可按网盘、按域名配置，出口可故障转移。
3. 统一缓存机制，在各平台 API 质量参差的前提下保证读写可靠：限速、风控、下载链接过期、无变更通知接口、上传中断都不能导致数据丢失或挂载不可用。
4. 文件查找与写入速度接近本地；指定目录可持续高可靠读写。
5. 提供 MCP 接口，agent 可以列目录、读、写、编辑、搜索，响应大小可控。

### 1.2 非功能需求与约束

| 项目 | 决策 |
|---|---|
| 部署形态 | 单机守护进程（个人电脑 / NAS / 开发机） |
| 语言与生态 | Go；rclone 作为库依赖（MIT） |
| 操作系统 | Linux（内核 5.x+，6.9+ 可用 passthrough）、macOS（macFUSE 5）；Windows 预留 WinFsp 接口 |
| 许可 | 项目自身许可待定；不移植 AGPL 代码（OpenList / AList） |
| 一致性 | close-to-open，不承诺跨客户端强一致 |

### 1.3 非目标（一期）

- 多用户多租户网关、Web 管理界面。
- 端到端加密叠加层（可后续用 rclone crypt 语义实现）。
- macOS File Provider / Windows Cloud Files 原生集成。
- 离线下载（磁力、BT）。

---

## 2. 参考产品分析

| 产品 | 机制要点 | 借鉴 | 不照搬的原因 |
|---|---|---|---|
| rclone（v1.75） | VFS 四级缓存模式（off/minimal/writes/full）；full 模式用稀疏文件记录已下载区间；`vfs-read-chunk-size 128M` 递增；`vfs-write-back 5s` 后上传，失败指数重试至 1 min；`dir-cache-time 5m` + `poll-interval`（仅有 ChangeNotify 的后端）；union / chunker / crypt 叠加层；v1.73+ 支持 per-remote `override.http_proxy` | `fs.Fs` 后端抽象、Range 读、重试分级、per-remote 代理 | 稀疏文件缓存难以限额与校验，在 exFAT 等文件系统上失效；cgofuse 路径无 readdirplus；上游没有 115 / 夸克 / 百度 / 阿里 / 123 后端 |
| OpenList / AList | 国内驱动最全；WebDAV 三种策略（302 直链 / 代理地址 / 本地代理）；目录缓存内存 TTL 可按 glob 覆盖；阿里 Open 下载链接默认 15 min 过期；百度 >20 MB 下载需 `User-Agent: pan.baidu.com`；115 Open 同账号仅两个有效 refresh token | 各国内网盘协议细节、按目录 TTL、下载代理模式 | 是 HTTP 网关不是文件系统；缓存不持久；AGPL-3.0 |
| CloudDrive2 | WinFsp / FUSE 挂载为本地盘；写入先在内存算 hash 尝试秒传，再落缓存目录上传；目录缓存默认 40 s，Pro 版可持久化；115 推荐 `MaxQueriesPerSecond=1` | 先算 hash 再落盘、目录缓存持久化、115 保守 QPS | 缓存无背压，会把磁盘写满；挂载容量标称云端容量误导用户；闭源 |
| JuiceFS | 元数据引擎（Redis / TiKV / SQL / SQLite）与对象存储分离；文件 → 64 MiB chunk → slice → 4 MiB block；`--writeback` 本地落盘即返回；`--prefetch`、`--verify-cache-checksum`；企业版 cache group 一致性哈希 | 元数据 / 数据分离、4 MiB 固定块、writeback、块校验 | 假设后端是强一致对象存储；网盘无此保证 |
| macOS File Provider / Windows Cloud Files API | 系统持有占位符，打开时 hydrate，`Keep Offline` 钉住，磁盘压力下自动 dataless | 占位符心智模型、pin 语义 | 平台私有 API，二期再做 |
| MCP 官方 filesystem server | 14 个工具（`read_text_file` head/tail、`edit_file` dryRun、`search_files`、`directory_tree` 等）；`validatePath` 解析符号链接后校验白名单；支持 Roots 与 ToolAnnotations | 工具语义、白名单校验、只读 / 破坏性注解 | 无分页、无字节范围读、无索引搜索；Claude Code 默认拒绝 >25k tokens 的工具结果 |

结论：**复用 rclone 的后端，不复用它的 VFS**；自研 VFS 让国内自研驱动与 rclone 后端共享同一套缓存与可靠性保障。

---

## 3. 总体架构

```
┌──────────────────────────────────────────────────────────────────────┐
│  接入层                                                              │
│  FUSE (go-fuse v2, Linux / macFUSE)      MCP (stdio + Streamable HTTP)│
│  CLI (cloudfs mount / pin / uploads / doctor)   控制 API (/metrics …) │
├──────────────────────────────────────────────────────────────────────┤
│  VFS 核心 (internal/vfs)                                              │
│   inode 树 / 文件句柄 / 一致性模式 / 挂载布局 (多个 remote 拼一个挂载点)│
├──────────────┬──────────────────┬────────────────────────────────────┤
│ MetaStore    │ BlockCache       │ Journal + UploadQueue              │
│ SQLite (WAL) │ 4 MiB 块         │ staging → journal → uploader        │
│ TTL / delta  │ readahead / 2Q   │ 秒传 / 分片续传 / 重试 / 死信 / 冲突 │
│ 负缓存 / FTS │ pin / hydrate    │                                    │
├──────────────┴──────────────────┴────────────────────────────────────┤
│  Provider 抽象 (internal/provider)   能力矩阵 Caps                    │
│   rclone: GDrive / OneDrive / Dropbox / Box / S3 / WebDAV / SMB / SFTP │
│   自研:   aliyun / baidu / pan115 / quark / tianyi / pan123           │
│   兜底:   openlist-bridge (WebDAV)                                    │
├──────────────────────────────────────────────────────────────────────┤
│  网络层 (internal/net)                                               │
│   代理规则引擎 (DOMAIN-SUFFIX / GEOIP → 出口)  出口组健康检查 / 故障转移│
│   限流 token bucket + AIMD   熔断   连接池   错误分类与重试            │
└──────────────────────────────────────────────────────────────────────┘
```

### 3.1 读路径

```mermaid
sequenceDiagram
    participant K as 内核/应用
    participant F as FUSE
    participant V as VFS
    participant B as BlockCache
    participant P as Provider
    K->>F: read(fd, off, len)
    F->>V: Read(ino, off, len)
    V->>B: Get(fileKey, blockIdx)
    alt 命中
        B-->>V: 块数据
    else 未命中
        B->>P: ReadRange(id, version, blockOff, 4MiB)
        P-->>B: 字节流（经限流器与代理）
        B->>B: 落盘 + crc32 + 更新位图
        B-->>V: 块数据
        B->>P: readahead 预取后续块（低优先级）
    end
    V-->>F: 数据
    F-->>K: 数据（进入内核页缓存）
```

### 3.2 写路径

```mermaid
sequenceDiagram
    participant K as 内核/应用
    participant V as VFS
    participant J as Journal
    participant U as UploadQueue
    participant P as Provider
    K->>V: open(O_WRONLY)
    V->>J: 创建 staging/<uuid>.part
    K->>V: write × N
    V->>J: 追加写 + 流式 hash
    K->>V: close()
    V->>J: fsync(file) → INSERT uploads(state=pending) → rename objects/<hash> → fsync(dir)
    J-->>V: 提交完成
    V-->>K: close 返回（writeback 模式）
    U->>P: BeginUpload(hashes) 尝试秒传
    alt 秒传成功
        P-->>U: Entry(version)
    else 分片上传
        loop 每片
            U->>P: UploadPart(idx)
            P-->>U: PartToken → 落表 upload_parts
        end
        U->>P: CompleteUpload
        P-->>U: Entry(version)
    end
    U->>J: state=done，更新 nodes.version/hash
    U->>V: staging blob 登记为读缓存块
```

### 3.3 元数据路径

lookup / getattr / readdir 全部先查 SQLite。目录 `complete=1` 且未过期时零远端调用。后台 refresher 对有 delta 的 remote 按游标增量更新；对无 delta 的 remote 按目录 TTL 轮询。任何本地写操作即时更新 SQLite 并向内核发送 `notify_inval_entry / notify_inval_inode`。

---

## 4. 模块设计

### 4.1 Provider 抽象与能力矩阵

```go
// internal/provider/provider.go
type Provider interface {
    Name() string
    Capabilities() Caps

    List(ctx context.Context, dirID, cursor string) (entries []Entry, next string, err error)
    Stat(ctx context.Context, id string) (Entry, error)
    ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error)
    DownloadURL(ctx context.Context, id string) (Link, error)

    BeginUpload(ctx context.Context, parentID, name string, size int64, h Hashes) (UploadSession, error)
    UploadPart(ctx context.Context, s UploadSession, idx int, r io.Reader, n int64) (PartToken, error)
    CompleteUpload(ctx context.Context, s UploadSession, parts []PartToken) (Entry, error)

    Mkdir(ctx context.Context, parentID, name string) (Entry, error)
    Rename(ctx context.Context, id, newName string) (Entry, error)
    Move(ctx context.Context, id, newParentID string) (Entry, error)
    Delete(ctx context.Context, id string) error
}

// 可选接口，用类型断言探测
type ChangeLister interface {
    Changes(ctx context.Context, cursor string) (events []Change, next string, err error)
}
type ServerCopier interface {
    Copy(ctx context.Context, id, newParentID, newName string) (Entry, error)
}
```

能力矩阵 `Caps`：

| 字段 | 含义 | 用途 |
|---|---|---|
| `HashTypes` | md5 / sha1 / sha256 / none | 秒传 key、上传后校验、指纹方式 |
| `RapidUpload` | 秒传所需 hash 组合（如百度 md5+slice_md5，阿里 sha1+proof） | Journal 流式算哪些 hash |
| `RangeRead` | 是否支持 HTTP Range | 不支持则整文件拉取后切块 |
| `PartSize`, `MaxParts`, `UploadParallel` | 分片参数 | UploadQueue 切片 |
| `ServerMove`, `ServerRename`, `ServerCopy` | 服务端操作 | 否则退化为下载 + 上传 |
| `Delta` | 是否有变更通知 | MetaStore 刷新策略 |
| `LinkTTL` | 下载链接有效期 | 过期前刷新 |
| `LinkHeaders` | 下载必需的 UA / Referer | 直链模式下透传给 agent |
| `LinkShareable` | 直链是否可被第三方进程使用 | MCP `get_download_url` 是否可用 |
| `QPS`（meta / download / upload） | 推荐初始速率 | 限流器初值 |
| `Tier` | official / unofficial | 默认限流保守度、状态页提示 |

驱动来源与关键事实：

| Provider | 实现 | 关键事实 |
|---|---|---|
| Dropbox | 自研，官方 HTTP API v2，经共享 `httpx` | `path_lower` 身份、`rev` 条件核对、recursive changes/reset、可恢复 upload session、4 h temporary link |
| OneDrive | 自研，Microsoft Graph v1.0，经共享 `httpx` | stable item ID、`cTag`、delta/reset、预认证 Range、可恢复 upload session |
| Google Drive / Box / SMB | 后续官方 SDK 或精简 `rcloneProvider` | Range、变更游标/通知与上传能力按后端映射，避免为已自研后端重复引入依赖 |
| S3 / 兼容对象存储 | MinIO Go SDK v7，经 `httpx.RawTransport` | delimiter 流式目录、条件 Range、持久 multipart、预签名；rename 为 copy + delete |
| 阿里云盘 | 自研，官方 Open API | 秒传 pre_hash（前 1 KiB SHA1）→ content_hash（SHA1）+ proof_code；下载链接默认 15 min（最长 4 h）；429 `TooManyRequests` 视为风控；三方权益包影响第三方速度 |
| 百度网盘 | 自研，官方开放平台 | 秒传 content-md5 + slice-md5（前 256 KiB）；dlink 约 8 h；>20 MB 下载必须 `User-Agent: pan.baidu.com`；上传分片需 30 s 内完成；非 SVIP 限速 |
| 115 | 自研，115 Open（2025-01 开放） | SHA1 秒传；同账号同 App 仅两个有效 refresh token；第三方 cookie 客户端被主动封禁，>3 req/s 触发风控；默认 QPS=1、大 page size、目录缓存持久化 |
| 123 云盘 | 自研，官方 Open API | MD5 秒传 |
| 天翼云盘 | 自研 | 参考公开协议自行实现 |
| 夸克 | 自研，cookie 接口 | 官方开放平台 beta 无公开文档；`Tier=unofficial` |
| openlist-bridge | WebDAV 到本机 OpenList | `HashTypes=none`、`Delta=false`；作为过渡和兜底 |

### 4.2 网络层：代理与限流

**代理规则引擎**（`internal/net/proxy`）

- 出口 `outbound`：`direct` / `http` / `socks5`（含用户名密码）。
- 出口组 `group`：`fallback`（按顺序取第一个健康的）/ `url-test`（取延迟最低的），周期健康检查（`check_url`、`interval`、`timeout`、`lazy`）。
- 规则从上到下匹配：`DOMAIN` / `DOMAIN-SUFFIX` / `DOMAIN-KEYWORD` / `IP-CIDR` / `GEOIP` / `FINAL`。内置 GeoIP 库（MaxMind 格式，可更新）。
- 每个 remote 可用 `proxy: <出口或组名>` 整体覆盖；也可在规则里对 API 域与 CDN 域分别路由（例如海外用户让阿里 API 走国内出口而 CDN 直连）。
- 实现方式：一个 `http.Transport.Proxy func(*http.Request) (*url.URL, error)` 加自定义 `DialContext`（SOCKS5 走 `golang.org/x/net/proxy`），所有 Provider（包括 rclone 后端，通过 `fshttp` 注入）共用。

默认规则（`proxy.DefaultRules`，覆盖全部已注册的境外驱动：Drive、OneDrive/SharePoint、
Dropbox、Box、S3）：

```
DOMAIN-SUFFIX,googleapis.com,proxy
DOMAIN-SUFFIX,googleusercontent.com,proxy
DOMAIN-SUFFIX,google.com,proxy
DOMAIN-SUFFIX,graph.microsoft.com,proxy
DOMAIN-SUFFIX,microsoftonline.com,proxy
DOMAIN-SUFFIX,sharepoint.com,proxy
DOMAIN-SUFFIX,1drv.com,proxy
DOMAIN-SUFFIX,live.com,proxy
DOMAIN-SUFFIX,dropboxapi.com,proxy
DOMAIN-SUFFIX,dropboxusercontent.com,proxy
DOMAIN-SUFFIX,dropbox.com,proxy
DOMAIN-SUFFIX,box.com,proxy
DOMAIN-SUFFIX,boxcloud.com,proxy
DOMAIN-SUFFIX,amazonaws.com,proxy
GEOIP,CN,direct
FINAL,direct
```

这里的 `proxy` **是一个占位符，不是出口名**。用户不必把自己的出口叫 `proxy`：
`defaultRulesFor` 在建 router 之前把它换成配置里真实存在的东西，顺序是
「名字就叫 `proxy` 的出口或组 → 第一个组 → 第一个非 direct 出口 → `direct`」。
最后那档只在配置里**完全没有**任何代理时才成立——用户没要求代理，这套规则又是我们
内置的，此时直连才是他们表达的意思。

**代理段可以热加载**（`Manager.Reload`，控制面 `PUT /proxy/config` 落盘后即调用）：每个
provider 的 HTTP 客户端每次请求都向 manager 重新解析出口，所以只需整体换掉 manager 内部的
路由状态；在途请求走它已解析的出口，下一次请求走新配置。带悬空目标的 reload 被拒，旧配置
原样生效。远端、挂载等其它配置改动仍需重启。

**用户自己写的规则不参与这个替换**：写了 `,proxy` 而没有定义它，请求就报
`unknown outbound "proxy"` 而不是悄悄直连。用户让走代理的流量绝不能无声地裸奔，
这是规则引擎存在的意义。

**限流与熔断**（`internal/net/ratelimit`）

- Key = (remote, account, class)，class ∈ {meta, download, upload}。
- Token bucket 初值来自 `Caps.QPS`；用户可覆盖。
- AIMD：收到 429 / 风控错误 → 速率减半、进入退避（指数 + 全抖动，尊重 `Retry-After`，上限 64 s）；连续成功 N 次 → 速率加性恢复到初值。
- 熔断：窗口内风控次数超过阈值 → 该 (remote, account) 熔断 M 分钟；挂载点上该 remote 转 `readonly`，读走本地缓存，未缓存的读返回 `EAGAIN`；状态页与 `cloudfs status` 明示原因和恢复时间。
- 连接池：每 host 独立 `http.Transport`，HTTP/2 优先，`MaxConnsPerHost` 来自能力矩阵。

**错误分类**（`internal/net/retry`）

| 类别 | 例子 | 处理 |
|---|---|---|
| 可重试 | 网络错误、5xx、429 | 退避重试，计入 AIMD |
| 认证 | 401、token 过期 | 刷新 token 一次后重试；再失败转 `readonly` 并提示重新授权 |
| 风控 | 阿里 `TooManyRequests`、115 `PermissionDenied` 需 App 验证 | 熔断分支 |
| 链接过期 | 403 / 410 on CDN | 刷新 `DownloadURL` 重试一次 |
| 终止 | 400、404、409（冲突）、413 | 不重试；冲突走冲突副本；其他进入死信 |

### 4.3 元数据层 MetaStore

`internal/meta`，SQLite（`modernc.org/sqlite` 纯 Go，WAL，`synchronous=NORMAL`，定期 `wal_checkpoint(TRUNCATE)`）。

```sql
CREATE TABLE nodes (
  ino        INTEGER PRIMARY KEY,
  parent_ino INTEGER NOT NULL,
  name       TEXT    NOT NULL,
  kind       INTEGER NOT NULL,          -- 0 file, 1 dir
  size       INTEGER NOT NULL DEFAULT 0,
  mtime_ns   INTEGER NOT NULL,
  mode       INTEGER NOT NULL,
  remote     TEXT    NOT NULL,
  remote_id  TEXT    NOT NULL,
  version    TEXT,                      -- ETag / cTag / updated_at 等
  hash_type  TEXT,
  hash       TEXT,
  fetched_at INTEGER NOT NULL,
  ttl_s      INTEGER NOT NULL,
  UNIQUE(parent_ino, name)
);
CREATE INDEX nodes_remote_id ON nodes(remote, remote_id);

CREATE TABLE dir_state (
  ino       INTEGER PRIMARY KEY,
  complete  INTEGER NOT NULL DEFAULT 0,
  listed_at INTEGER,
  cursor    TEXT,
  dirty     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE absent (parent_ino INTEGER, name TEXT, until INTEGER, PRIMARY KEY(parent_ino, name));
CREATE TABLE remote_cursor (remote TEXT PRIMARY KEY, delta_cursor TEXT, updated_at INTEGER);
CREATE TABLE pins (path TEXT PRIMARY KEY, recursive INTEGER, mode TEXT);
CREATE VIRTUAL TABLE name_index USING fts5(name, path UNINDEXED, ino UNINDEXED, tokenize='trigram');
```

策略：

| 机制 | 细节 |
|---|---|
| 本地服务 | `complete=1` 且 `now - listed_at < ttl` 时 readdir / lookup 不出远端；readdirplus 一次带回 attr |
| 预取 | readdir 后对子目录做后台列举，默认深度 2，每 remote 并发 2；`cloudfs warm <path>` 全树爬取；受限流器约束自然减速 |
| 刷新 | 有 delta：GDrive `changes.list`、OneDrive `delta`、Dropbox `list_folder/longpoll`、rclone `ChangeNotify`；无 delta：目录 TTL 默认 5 min，pin 目录 1 min，只读媒体库可设 24 h，也可按 glob 覆盖 |
| 负缓存 | `.git`、`.DS_Store`、`node_modules` 等探测命中 `absent` 表；该目录任何创建操作清空 |
| 写直达 | 本地变更先更新 SQLite 并标 `dirty`，同时 `notify_inval`；内核 `entry_timeout / attr_timeout` 设 30 s |
| 持久化 | 重启后目录树可直接用；避免冷启动刷新风暴 |
| 搜索 | 文件名 trigram FTS，支持子串与 glob 转换；`cloudfs find` 与 MCP `search` 共用 |

### 4.4 数据缓存层 BlockCache

`internal/cache`。

| 机制 | 细节 |
|---|---|
| 布局 | `cache/blocks/aa/bb/<remote>-<remote_id>-<version>-<idx>`；块 4 MiB（可按 Provider 调整）；索引表 `blocks(key, size, crc32, last_access, pinned)`；`file_blocks(file_key, bitmap BLOB)` |
| hydrate | 位图全满 → 合并为 `cache/hydrated/<file_key>`；有 hash 的 Provider 校验整文件 hash；Linux 6.9+ 且具备 `CAP_SYS_ADMIN` 时 open 返回 `FUSE_PASSTHROUGH` |
| readahead | 连续块访问触发，窗口 1→2→4→…→16 块（64 MiB 上限）；预取优先级低于按需读；随机小读异步补全整块；每文件默认 4 并行流 |
| 淘汰 | 2Q（新块先进试用队列，二次命中才进主队列），约束：字节上限、inode 上限、最长年龄、磁盘最低剩余；pin 免疫 |
| 背压 | 剩余空间 < `min_free` 时：先淘汰；仍不足则写返回 `ENOSPC`，读缓存降级为直读（不落盘） |
| 校验 | 块 crc32；过期链接（403 / 410）自动刷新重试一次；hash 不符则丢弃重拉 |
| 内容寻址去重 | `blobs(hash_type, hash → path)`；同一 blob 既是读缓存也可对另一 Provider 秒传，跨网盘拷贝不重新下载 |

### 4.5 写入路径与可靠性

`internal/journal` + `internal/upload`。

流程：

1. `open(O_WRONLY | O_RDWR)`：新文件直接建 staging；已有文件且需随机写时，按需拉块到 staging（Provider 不支持分片合并的必须整文件拉取，能力矩阵决定）。
2. `write`：写 `cache/staging/<uuid>.part`，边写边流式计算该 Provider 秒传所需 hash；`O_APPEND` 与随机写均支持。
3. `close` / `fsync`：`fsync(file)` → 事务 `INSERT uploads(state=pending)` → `rename` 到 `cache/objects/<hash>` → `fsync(dir)` → 返回。默认 `writeback` 模式在此返回。
4. UploadQueue：每 remote 一个 worker pool（并发数来自能力矩阵）。先 `BeginUpload` 尝试秒传；否则分片，每片成功后 `upload_parts` 落表；`CompleteUpload` 幂等（以 upload_id 为 key）。
5. 成功：更新 `nodes.version / hash`；staging blob 切分登记为读缓存块，不重新下载。
6. 失败：退避重试；超过阈值或终止性错误进入 `dead_letter`，blob 保留；可 `cloudfs uploads retry`，或先 cancel 并等待 cancelled，再以 `drop <id> --confirm` 通过 VFS 丢弃当前本地版本。保留期间挂载点仍读本地版本；取消/清理状态不是远端成功证明。
7. 冲突：open 时记录远端 `version`；上传前对比（有 delta 用事件，无 delta 重新 `Stat`）；不一致则上传为 `name (conflict 2026-09-02 host).ext`，状态页告警。

```sql
CREATE TABLE uploads (
  id               TEXT PRIMARY KEY,
  remote           TEXT NOT NULL,
  remote_parent_id TEXT NOT NULL,
  name             TEXT NOT NULL,
  blob_path        TEXT NOT NULL,
  size             INTEGER NOT NULL,
  hashes           TEXT NOT NULL,   -- JSON
  state            TEXT NOT NULL,   -- pending | uploading | done | dead
  attempt          INTEGER NOT NULL DEFAULT 0,
  next_retry_at    INTEGER,
  last_error       TEXT,
  expected_version TEXT,
  session          TEXT,            -- Provider 的 upload_id 等，JSON
  meta_identity    TEXT NOT NULL,   -- 原元数据库身份
  mount_prefix     TEXT NOT NULL,
  mount_root_id    TEXT NOT NULL,
  account_binding  TEXT NOT NULL,   -- 本地授权代次 + 账号定位配置指纹
  created_at       INTEGER NOT NULL
);
CREATE TABLE upload_parts (upload_id TEXT, idx INTEGER, etag TEXT, state TEXT, PRIMARY KEY(upload_id, idx));
CREATE TABLE dead_letter (upload_id TEXT PRIMARY KEY, reason TEXT, moved_at INTEGER);
```

一致性模式（按挂载或按目录）：

| 模式 | `close()` 返回时机 | `fsync()` | 适用 |
|---|---|---|---|
| `writeback`（默认） | 本地 journal 提交后 | 同 close | 日常编辑、agent 写入、IDE |
| `strict` | 远端上传完成后 | 阻塞到上传完成 | 备份脚本、跨端立刻可见 |
| `readonly` | 拒绝写（`EROFS`） | 无 | 媒体库、被熔断或未授权的账号 |

启动恢复：扫描 `uploads` 中 `pending / uploading`，校验 blob 存在与 hash 一致，重新入队；
`cancelling` 改为 `cancelled`，保留载荷、不重新上传。取消不代表远端撤销；当前仍被元数据
引用的完整取消版本在启动时恢复缓存链接，不创建已消失的目标。`staging` 里无对应记录的 `.part` 删除。
journal v11 的上传在写入时保存元数据库、挂载前缀、根对象和账号绑定；worker 领取后先
核对这些本地字段，再解析 provider。配置显式重新授权、账号定位字段、挂载或数据库改变时
任务进入死信且不产生远端调用。自动 token 刷新和仅迁移密钥存储不换代。旧 schema 的空绑定
记录也默认停住；操作者可在核对当前本地版本后以显式确认恢复事务重新绑定。

### 4.6 FUSE 层

`internal/fuse`，go-fuse v2（直接实现 FUSE 协议，Linux + macFUSE）。

| 选项 | 值 | 原因 |
|---|---|---|
| `readdirplus` | on | 消除 `ls -l` 后的 N 次 lookup |
| `writeback_cache` | on | 内核合并小写；配合 flush / fsync 提交 |
| `splice_read / splice_write / splice_move` | on | 零拷贝 |
| `max_write / max_read` | 1 MiB | 一次请求覆盖更多块 |
| `direct_io` | off | 用内核页缓存 + 主动 inval |
| `entry_timeout / attr_timeout / negative_timeout` | 30 s / 30 s / 5 s | 写直达 + inval 保证安全 |
| passthrough | 探测启用 | hydrated 文件近本地性能 |
| `allow_other` | 可配 | 需 `/etc/fuse.conf` `user_allow_other` |

平台适配在 `platform_linux.go` / `platform_darwin.go`；`Backend` 接口预留 Windows（cgofuse + WinFsp）。macOS 一期用 macFUSE 5 kext，提供 `--backend fskit` 试验开关；Fuse-T 作为无 kext 备选。

### 4.7 MCP 服务

`internal/mcp`，基于官方 Go SDK，支持 stdio 与 Streamable HTTP（`127.0.0.1:<port>`，bearer token）。MCP 直接调用 VFS 核心，不经过 FUSE，因此未挂载也可用。

工具（每个都带 `readOnlyHint / destructiveHint / idempotentHint` 注解，默认响应 < 20k tokens，超出返回 `truncated: true` + `next_cursor`）：

| 工具 | 参数 | 说明 |
|---|---|---|
| `list_directory` | `path, cursor?, limit=200` | 分页；返回 name / kind / size / mtime / cached / upload_state |
| `stat` / `stat_many` | `path` / `paths[]` | 元信息 + 缓存与上传状态 |
| `read_text` | `path, head?, tail?, offset?, max_bytes=262144` | 自动检测编码；二进制文件拒绝并建议 `read_range` |
| `read_range` | `path, offset, length<=4MiB` | base64 |
| `write_file` | `path, content, mode=create\|overwrite\|append` | 同一写路径；返回 `upload_state` |
| `edit_file` | `path, edits[{old_text,new_text}], dry_run?` | 官方语义；返回 diff |
| `mkdir` / `move` / `copy` | 路径 | 幂等 |
| `delete` | `path, confirm, recursive?` | `confirm=true` 缺失即拒绝 |
| `search` | `path, glob?, regex?, content?, max_results=100` | 文件名走 FTS；内容搜索仅在已缓存 / pin 目录内本地 grep，其余返回提示先 `pin` |
| `get_download_url` | `path` | 直链 + 过期时间 + 必需 headers；`LinkShareable=false` 的 Provider 返回错误 |
| `pin` / `cache_status` | `path, recursive?` | agent 主动预热工作目录 |
| `list_uploads` / `get_upload` | `cursor?, limit?` / `id` | 有界查询可见上传，不暴露 blob、session、hash、远端版本和原始错误 |
| `retry_upload` / `cancel_upload` | `id` | 绑定当前授权路径后重试 dead 或持久停止上传 |
| `resume_upload` / `discard_upload` | `id, confirm` | 显式接受远端重放风险或永久本地数据丢失；discard 仅无限制服务 |
| `flush_uploads` | 无 | 仅无限制服务等待全队列快照，不把排空等同于独立远端对账 |
| `list_roots` | 无 | 当前允许根 |

Resources：`cloudfs://<remote>/<path>`，支持 `resources/read`（文本或 blob）与 `resources/subscribe`（来自 MetaStore 变更事件）。

目录分页实现补充：工具新游标按名称定位，旧规范十进制偏移仍兼容；资源继续使用
文件名编码的 next_uri。VFS 刷新完成后使用 `(parent_ino, name)` 索引取有界页，工具
硬上限 1000 项，资源按最多 128 项窗口读取并按 JSON 编码字节预算输出。工具 total
为独立索引计数，不与页面承诺同一快照。冷/过期目录使用 SQLite TEMP 表按 200 项
批次暂存，再以一个元数据事务合并；schema v9 的刷新代次和数据库/目录身份校验
阻止旧结果覆盖新刷新。普通上传的本地账号/元数据库绑定已由 journal v11 补齐；它是
防止队列跨配置发送的 fail-closed 围栏，不是 provider 返回的云端身份凭据。变更通知分批或退化为子树/
全挂载失效，不加载完整变更数组；WebDAV/OpenList 通过能力矩阵选择 StreamLister，
HTTP 多状态响应逐条解析到同一暂存器，完整收尾前不发布。SFTP 的完整枚举、TEMP 空间预算与
真实规模的内存/写锁性能仍待完善。详见 [目录冷刷新](directory-refresh.md)。

删除、改名、显式目录/全局失效及传统目录合并，也在各自元数据事务内推进相关
目录的刷新代次。跨目录移动同时保护源与目标；事务回滚不能留下单独生效的代次
修改。这防止旧 provider 列表重新插入已经删除或改名的旧名称，不替代服务端快照。

目录对象替换补充：同名目录 ID/remote 改变或文件/目录类型互换时，事务内回收
旧子树及索引并分配新 inode；有待上传或活动写句柄后代则保留整个原目录对象。
delta 也在同一事务校验节点快照和后代保护，失败不推进游标，成功推进刷新代次。
活动写句柄按 inode 计数到 close 提交结束；写打开登记与远端发布有短锁互斥，
不跨 provider 列举等待持锁。保留本地父链不代表自动重建远端父目录，详见目录冷刷新文档。

安全：`--allow <path>` 白名单（Roots 作为辅助）；`--read-only`；路径规范化后校验（含符号链接）；每分钟返回字节配额；绝不暴露 token / cookie；HTTP 模式只绑定回环地址。

注册（`cloudfs mcp install --client claude | codex` 自动写入）：

```jsonc
// Claude Code: .mcp.json（项目）或 claude mcp add --transport stdio cloudfs -- cloudfs mcp --stdio --allow /mnt/cloud/work
{ "mcpServers": { "cloudfs": { "command": "cloudfs", "args": ["mcp", "--stdio", "--allow", "/mnt/cloud/work"] } } }
```

```toml
# Codex: ~/.codex/config.toml
[mcp_servers.cloudfs]
command = "cloudfs"
args = ["mcp", "--stdio", "--allow", "/mnt/cloud/work"]
```

### 4.8 控制面与可观测性

- CLI：`config add | auth | list`、`mount | umount`、`service install | uninstall | status`、`ui | open`、`pin | unpin`、`warm`、`uploads list | retry | drop`、`cache stats | gc`、`proxy test`、`status`、`doctor [--fix]`、`mcp --stdio | --http`。
- 控制 API：Unix socket（默认）+ 可选 `127.0.0.1` HTTP；`/healthz`、`/readyz`、`/metrics`（Prometheus）、`/status`（JSON）。
- 图形控制面端点（供内嵌 Web 应用与桌面壳使用，守卫同上，非 GET 需 `X-CloudFS-Control`，只接受回环 Host 与同源 Origin）：
  文件树 `GET /fs/list|stat|preview|download-url`、`POST /fs/mkdir|rename|delete`；`GET /search`；诊断 `POST /doctor/run|fix`；
  变更流 `GET /events`（SSE：目录变更事件 + 每 2s 一次 `status`，断线退避重连并回落 `/status` 轮询）；
  账号 `GET/POST /accounts`、`GET/PATCH/DELETE /accounts/{name}`、`POST /accounts/{name}/check`、账号授权 `POST /accounts/{name}/auth/start|cancel`、`GET .../auth/status`；
  代理 `GET /proxy/explain`、`POST /proxy/check`、`GET/PUT /proxy/config`（代理段热加载，其余改动 `restart_required`）；
  挂载 `GET/POST /mounts`、`DELETE /mounts/{path}`（仅改配置）；
  生命周期 `POST /daemon/restart`（confirm；置 draining 后原地 re-exec，保证同一时刻只有一个 journal/mount owner）、
  服务 `GET /service/status`、`POST /service/install|uninstall`（confirm）。
  凭据边界：任何 `IsSecretField` 键在 `POST /accounts` 与 `PATCH` 上一律 400 并指向 `cloudfs config auth`，秘密值不回显、不落盘、不经浏览器；
  `/fs/download-url` 是唯一有意返回签名 URL 的端点（no-store、不落日志）。桌面壳的 `CLOUDFS_CONTROL_UI=<loopback:port>` 让 `mount` 额外开一个带 UI 的回环 TCP 面（仍受 `Start` 的回环校验约束）。
- 上传管理（已接线）：`GET /uploads?limit=200&cursor=<id>`、`POST /uploads/retry`（JSON `{"id":"..."}` 或 `{"all":true}`）、`POST /uploads/flush`（JSON `{}`）。POST 要求 `Content-Type: application/json` 和 `X-CloudFS-Control: 1`，不提供 CORS；本地原生客户端使用 `Host: cloudfs`。列表不暴露上传 session 和 blob 路径。
- `uploads flush` 与内部 `DrainAll` 语义不同：前者等已提交队列（包括延迟和在途）排空，并对死信、取消及未完成清理报错；后者只处理当前到期任务。在线 flush 不启动额外 worker，连接取消仅结束等待。CLI 默认等待上限 30 分钟，`--timeout` 可调整。
- 上传取消：`uploads cancel <id>` 与 `POST /uploads/cancel`（JSON `{"id":"..."}`）
  先保存停止意图，再中断 worker。pending/dead 可直接进入 cancelled，活动请求先
  cancelling、退出后 cancelled；旧 worker 的 Retry/Defer/Succeed/Fail/重复 Commit
  不能覆盖停止。取消与成功发布串行仲裁，删除补偿和 done 拒绝取消。CLI 优先使用
  原 daemon；离线不启动 provider/worker，已连接后的响应丢失不离线重放。
  取消保留内容及 session/parts，不删除远端，也不证明远端没有生效。flush 对取消
  任务返回未完成；status/metrics/doctor 显示保留状态。普通 retry 不接受取消
  记录，关联本地版本删除／改名返回忙；清理由显式 drop 协调器处理，自动对账仍缺。
- 显式恢复取消上传：`uploads resume <id> --confirm` 与 `POST /uploads/resume`
  （`{"id":"...","confirm":true}`）。VFS 校验当前选中的本地版本、名称、远端父 ID
  和实际可写挂载，复制上传还验证原复制绑定；Journal 在不持写锁时完整校验 CRC。
  重新进入发布锁后再检查目标，事务按取消修订号和状态提交；任何较晚取消、同 inode
  新写入或已恢复操作都会拒绝旧请求。旧会话／分片保存为私有历史，与清空旧会话、
  切回 pending 同事务提交，失败回滚。新请求不是远端对账，可能重复或覆盖内容，
  必须显式确认。journal v11 任务会核对原数据库/挂载/授权代次；更早的空绑定任务
  自动发送会被拒绝，确认恢复时才在同一事务采用当前绑定。
  离线构建 NoBackground 栈，只排队不运行上传。自动对账与完整历史回收仍待补。
- 显式丢弃本地上传：`uploads drop <id> --confirm` 和 `POST /uploads/drop`
  （唯一 JSON `{"id":"...","confirm":true}`），仅接受已停止的当前本地版本或已有
  purging 意图。VFS 核对全部别名/句柄/挂载，Journal 意图 → FULL 元数据删除 →
  checked 缓存删除 → Journal 最终清理；失败保留意图并可重启续清理。在线响应
  丢失不转离线重放；离线仅打开已存在且独占的本地存储，不构建 provider 或恢复
  其他上传。成功不撤销远端动作，也不承诺立即释放共享/租约字节。详细边界及
  生产命令强杀回归见 `docs/upload-cleanup.md`。
- pin/unpin/warm/cache 管理通过控制端点使用现有 VFS；离线先确认队列所有权。pin 规则先落库，缓存用户固定与未上传数据保护分离，启动恢复保护并补缺块。规则按路径匹配，重叠规则相加；具体语义和完整缓存预算的剩余缺口见 `cache-management.md`。
- 指标：块缓存命中率与占用、元数据命中率、负缓存命中、上传队列深度与最老待传时间、死信数、各 remote 请求延迟与错误分类、限流等待时间、熔断状态、代理出口健康、FUSE opcode 延迟直方图。
- `doctor`：FUSE / 内核版本与 passthrough 可用性、`user_allow_other`、缓存目录文件系统类型与剩余空间、SQLite `integrity_check`、各账号 token 有效性、代理连通性、时钟偏差、待重试 / 死信；`--fix` 处理安全项（重新入队、清理孤儿 staging、vacuum）。
- 密钥：token / cookie 存系统 keyring（macOS Keychain、Linux Secret Service），不可用时退化为 `0600` 文件并告警。

### 4.9 WebDAV 输出

- `webdav.http` 随当前 daemon owner 启动，只把 `webdav.root` 投影为 DAV 根；适配器仅调用
  VFS 的 Stat/ReadDir/Open/Read/Release，不能直接解析 provider 或缓存文件。
- 当前契约刻意只读：OPTIONS、Depth 0/1 的 PROPFIND、GET/HEAD 和 Range。所有写方法
  405，Depth infinity 与超过 64 KiB 的属性请求拒绝，避免媒体客户端误配置后递归扫盘。
- ETag 是 remote/provider version 的不可逆摘要，支持条件读取但不公开 provider opaque
  标识。路径统一在规范化后拼到固定 root，`..`、反斜杠不能越界。
- 默认绑定回环；非回环必须从 `CLOUDFS_WEBDAV_TOKEN` 取得至少 16 字节 token，可用 bearer
  或 Basic 用户 `cloudfs`。内建 HTTP 不提供 TLS，跨机发布必须配 TLS reverse proxy/VPN。
- 下载策略为 proxy（默认）、redirect 或 auto；只有 http(s) 且不要求额外 header 的
  shareable provider link 才能 302，auto 对本地版本、失败、非法 URL 和 UA/Referer 约束
  回落到 VFS proxy。可写 DAV 和真实媒体客户端兼容性仍是后续范围，详见
  `docs/webdav-output.md`。

### 4.10 STRM 媒体库输出

- `cloudfs strm` 是正在运行的 WebDAV 的有界客户端，不打开 meta/journal/provider，也不与
  daemon owner 竞争。它只用 Depth-1 PROPFIND 递归枚举固定的 `webdav.root` 子树。
- 仅为允许扩展名的普通文件生成同层 `.strm`；URL 指向 WebDAV，实际字节仍遵循
  proxy/redirect/auto 策略。目录深度、媒体数和单次 DAV 响应均有上限。
- 写入使用同目录临时文件、fsync、rename；内容相同则不改写。输出路径逐层拒绝 symlink，
  大小写不敏感的目标名碰撞失败。默认不删除旧文件；显式 prune 只删同源 manifest 记录且
  摘要未变的普通文件，修改项转为非托管，目录项与新 manifest 均 fsync。
- 默认 URL 不包含 token；媒体服务应配置 Basic Auth。把 token 写入 URL 必须显式开启，
  并在 stderr 告警。真实 Emby/Jellyfin/Infuse 扫描和跳播性能仍需环境验收。
- VFS 的顺序段拥有独立可取消的 read-ahead context；跳播在等待新范围前取消旧段，句柄
  close 也取消全部 detached 预读。若前台恰好等待被取消 flight 的同一块，会以自己的
  context 重试；随机目标仍只取子块，连续读再按既有 1→2→…→16 块窗口增长。

---

## 5. 可靠性场景矩阵

| 场景 | 行为 |
|---|---|
| 写入过程中进程被 `kill -9` | 未提交的 `.part` 启动时清理；已提交 journal 的重新入队 |
| 上传中断网 | 分片状态落表，恢复后从未完成片续传 |
| Provider 429 / 风控 | AIMD 降速 + 退避；持续则熔断账号，挂载转只读并提示 |
| 下载链接过期 | 自动刷新 URL 重试一次；再失败按普通错误退避 |
| 远端被其他客户端改动 | delta / TTL 感知后 inval 内核缓存；本地有脏数据则冲突副本 |
| 缓存盘将满 | 淘汰；仍不足则写返回 `ENOSPC`，读降级为直读 |
| token 失效 | 刷新一次；失败则该 remote 只读并要求 `cloudfs config auth` |
| 代理出口挂掉 | 出口组切换备用；全部失败时相关 remote 报错，不影响其他 remote |
| 无 hash 的 Provider | size + mtime 指纹；上传后 `Stat` 对比大小；`Tier` 标低 |
| SQLite 损坏 | `doctor` 检测；`meta.db` 可整体重建（不是真相源）；`journal.db` 独立文件、每次提交 fsync |
| 同名并发写（两个进程） | 句柄级隔离，最后 close 的版本胜出，前一个成为冲突副本 |
| 挂载点被强制卸载 | journal 已提交的上传由守护进程继续；下次挂载恢复视图 |

---

## 6. 代码结构

```
/home/work/workspace/fs
├── go.mod
├── cmd/cloudfs/main.go          # CLI 入口（cobra）
├── internal/
│   ├── vfs/                     # inode 树、句柄、模式策略、挂载布局
│   ├── fuse/                    # go-fuse 适配、platform_linux.go / platform_darwin.go
│   ├── meta/                    # SQLite MetaStore、refresher、prefetch、fts
│   ├── cache/                   # block store、readahead、eviction、pin、hydrate
│   ├── journal/                 # staging、uploads 表、恢复
│   ├── upload/                  # worker pool、秒传、分片、冲突
│   ├── provider/
│   │   ├── provider.go          # 接口 + Caps + Entry
│   │   ├── registry.go          # type → 构造函数
│   │   ├── rclone/              # rcloneProvider
│   │   ├── aliyun/ baidu/ pan115/ pan123/ tianyi/ quark/
│   │   └── openlist/            # WebDAV bridge
│   ├── net/
│   │   ├── proxy/               # 规则引擎、出口组、健康检查
│   │   ├── ratelimit/           # token bucket + AIMD + 熔断
│   │   └── retry/               # 错误分类与退避
│   ├── mcp/                     # 工具 / 资源实现、stdio & http
│   ├── control/                 # 控制 API、metrics、doctor
│   └── config/                  # YAML 配置、密钥存储
├── test/
│   ├── fakeprovider/            # 可注入延迟 / 429 / 断网的模拟 Provider
│   ├── conformance/             # 文件系统语义测试
│   └── e2e/                     # 挂载 + MCP 端到端
└── docs/
    ├── DESIGN.md                # 本文
    ├── providers/               # 各驱动接入说明
    └── mcp.md                   # MCP 注册与工具说明
```

配置示例 `~/.config/cloudfs/config.yaml`：

```yaml
cache:
  dir: ~/.cache/cloudfs
  max_size: 200GiB
  min_free: 10GiB
  block_size: 4MiB
  max_age: 720h
proxy:
  outbounds:
    - { name: clash, type: socks5, addr: 127.0.0.1:7890 }
    - { name: vps,   type: http,   addr: http://user:pass@1.2.3.4:3128 }
  groups:
    - { name: proxy, type: fallback, members: [clash, vps],
        check_url: https://www.gstatic.com/generate_204, interval: 60s }
  rules:
    - DOMAIN-SUFFIX,googleapis.com,proxy
    - DOMAIN-SUFFIX,graph.microsoft.com,proxy
    - DOMAIN-SUFFIX,dropboxapi.com,proxy
    - DOMAIN-SUFFIX,box.com,proxy
    - GEOIP,CN,direct
    - FINAL,direct
remotes:
  gdrive: { type: gdrive, proxy: proxy }
  od:     { type: onedrive, proxy: proxy }
  ali:    { type: aliyun }
  bd:     { type: baidu, download_parallel: 2 }
  p115:   { type: pan115, qps: { meta: 1, download: 2, upload: 1 } }
  nas:    { type: webdav, url: https://nas.local/dav }
mounts:
  - path: /mnt/cloud
    layout:
      /work:  { remote: ali,    root: /work,  mode: writeback, pin: true, dir_ttl: 1m }
      /gd:    { remote: gdrive, root: /,      mode: writeback }
      /media: { remote: p115,   root: /media, mode: readonly,  dir_ttl: 24h }
      /nas:   { remote: nas,    root: /,      mode: strict }
mcp:
  http: 127.0.0.1:8765
  allow: [/mnt/cloud/work, /mnt/cloud/nas]
  read_only: false
control:
  socket: ~/.cache/cloudfs/control.sock
  metrics: 127.0.0.1:9101
  ui: true

webdav:
  http: 127.0.0.1:8080
  prefix: /dav
  root: /media
  strategy: proxy
```

---

## 7. 实施进度

| 阶段 | 交付 | 状态 |
|---|---|---|
| M0 骨架 | Provider 接口与 Caps、故障注入 fakeprovider、配置解析与交叉校验、错误分类、AIMD 限流与熔断、代理规则 | 完成 |
| M1 只读挂载 | SQLite MetaStore（TTL / 负缓存 / 预取 / FTS）、4 MiB 块缓存（2Q / hydrate / 背压）、VFS 读路径与 readahead、go-fuse 挂载 | 完成 |
| M2 写路径 | staging 与流式哈希、写日志、上传队列（秒传 / 分片续传 / 会话恢复）、writeback 与 strict、冲突副本、崩溃恢复 | 完成 |
| M3 网络层与驱动 | 共享 httpx 客户端（代理路由 + 限流 + 熔断 + 分类）、出口组与健康检查、WebDAV / OpenList 驱动 | 完成 |
| M3 国内驱动 | 阿里云盘、百度、115、夸克、天翼、123 | 完成，部分 API 细节待真实账号验证 |
| M4 MCP | 29 个工具、Resources 读取/订阅、允许列表、只读模式、stdio 与 Streamable HTTP、客户端安装命令 | 主体完成，长期负载与规模验收待补 |
| M5 加固 | pin / hydrate、metrics、status、doctor 与 --fix、CLI 全套命令、守护进程装配 | 完成 |
| M6 macOS | macFUSE 平台选项与检测 | 完成，待真机验证 |
| M7 通用协议 | SFTP 驱动、代理层原始 TCP 拨号（direct / socks5 / HTTP CONNECT）、后端调用计数器 | 完成 |
| M8 IO 测试升级与优化 | `cloudfs bench` 内置基准、`netem` 链路损伤代理、三档矩阵、fio 交叉验证；子块拉取、journal 组提交、单次上传、批量元数据写入、内核目录缓存、passthrough 接入 | 完成，方法见 `docs/bench.md` |
| M9 用户服务 | `service install/uninstall/status`，Linux user systemd 与 macOS LaunchAgent，异常退出重启及残留挂载清理 | 代码与隔离测试完成，reboot/kill-9 真机待验收 |
| M9 第二轮：传输层与热路径 | FUSE 请求计数与 READ 尺寸直方图、direct 档；readdirplus 由目录句柄填充、目录句柄惰性列举、`noatime`、列举只在真有变化时失效内核缓存；SFTP 句柄复用 + `ReadAt` 并发 + 255 KiB 包 + 短读守卫；16 KiB 子块；负缓存进内存、`Path` 单查询、create/flush 少查询；`journal.durability: power|crash` 与恢复期 blob 校验 | 完成，数字见 `docs/perf-report.html` |

### 实测基线

`test/perf` 把设计目标固化为可回归的调用次数断言：

| 场景 | 结果 |
|---|---|
| 冷遍历 22 目录 / 500 文件 | 271 次 Provider 调用 |
| 热遍历同一棵树 | 0 次调用 |
| 400 文件遍历的 per-file Stat | 0 次（属性随 listing 返回） |
| 1 MiB 文件按 4 KiB 读 256 次 | 16 次 Range 请求（等于块数） |
| 3 次顺序读后 readahead 预取 | 16 块中已缓存 11 块 |
| 预取后遍历 17 个目录 | 0 次调用 |
| 写入后读回刚上传的文件 | 0 次下载（blob 直接成为缓存条目） |
| 已建索引的文件名搜索 | 0 次调用 |

### 局域网实测（SFTP 后端，2026-09-02）

在 192.168.0.30 上挂载 192.168.0.20 的 `~/test`，千兆内网、RTT 0.2 ms，数据集为
2000 个 4 KiB 文件加两个大文件（256 MiB / 64 MiB）。同一组负载分别跑在 cloudfs、
sshfs 与本地磁盘上，数字见 `docs/perf-report.html`。这一轮测试暴露并修复了六个缺陷，
其中三个是正确性问题（`close(2)` 返回 EINTR、并发写后读回 ENOENT、只读命令破坏
上传队列），三个是性能或数据保全问题（热缓存读被块大小主导、限流器忽略能力矩阵、
内容寻址的暂存对象被误删）。

### M8 带来的机制变化

- **读路径**（§4.4 补充）：块缓存支持部分在场——64 KiB 子块加 sidecar 位图。
  非顺序的未命中只取覆盖请求的子块；同一句柄在同一块上第四次子块未命中后，
  剩余部分在后台补全；顺序读仍按整块走预读。已完全 hydrate 且无本地待上传写入的
  只读句柄，在 Linux 6.9+ 且持有 `CAP_SYS_ADMIN` 时交给内核 passthrough。
- **写路径**（§4.5 补充）：`close()` 的日志提交进入组提交队列，≤ 5 ms 或 64 条合并为
  一个事务加一次目录 fsync；`close()` 仍在自己的行落盘后才返回。不超过一个分片的文件
  经 `SinglePutter` 一次请求上传。上传并发按 `Caps.UploadParallel`，配置可覆盖。
  删除与在途上传竞争时给行打墓碑，上传完成后由 uploader 删除后端文件。
  冲突判定以节点当前的 `RemoteVersion` 为准，自己的上一次上传不算冲突。
- **元数据**（§4.3 补充）：`PutDir` 批量写入，FTS 索引延后合并；内核通过
  `FOPEN_CACHE_DIR` 缓存目录流，本地改动由内核自行失效，后端改动经 `OnInvalidate`
  失效。列举不得删除「列举开始之后出现」或「后端尚不知道」的条目。
- **读路径的顺序判定**：预读窗口只在连续 3 次读「落在上一块或下一块、且起点在上一次
  读结束的 1 MiB 内」之后才打开；块相邻本身不算顺序，否则大文件上的随机读会不断误触发
  整块预读。
- **两个 SQLite 库的写事务一律 `BEGIN IMMEDIATE`**（`_txlock=immediate`）：daemon 与
  `cloudfs status` 这类只读命令共用同一文件，deferred 事务在另一进程提交后升级写锁会
  得到不可重试的 `SQLITE_BUSY_SNAPSHOT`。`commitWrite` 在 staging 改名之后的每一步都
  记录在句柄上，失败后的下一次 FLUSH 从断点续做，而不是重新提交已不存在的 staging。
- **元数据热点**：lookup 未命中时只确认列举新鲜、不加载列举；点查用预编译语句；
  内核的 `security.*` xattr 探测不查库。
- **运维**：`POST /cache/drop` 清 VFS 与内核缓存做冷启动；journal 以 `flock` 标记
  队列属主，非属主进程既不跑恢复也不跑上传；`CLOUDFS_DEBUG_ERRNO=1` 把落到 EIO 的
  原始错误写进日志。

### M9 第三轮补充

- **读路径**：整块拉取用 `cache.PutAsync` 写后（内存先服务，两个后台 goroutine 落盘，内存上限
  `cache.write_behind`，默认 256 MiB，超限退化为同步写）；hydrate 推迟到文件空闲 `HydrateAfter`
  （2 s）后；驱动可实现 `provider.RangeReaderAt` 直接填调用方缓冲，块缓冲经 `cache.BlockBuffer`
  池化；预读对每个块只保留一个在飞 goroutine（`inflight`）；partial 块文件在填充期间保持打开，
  子块同样先在内存服务、由后台 worker 落盘（整块最多用到上限减 32 MiB，为子块留出内存），
  sidecar 只记录已落盘的子块；`Cache.Close` 等待后台写完，daemon 退出与测试清理都调用它。
- **SFTP**：`connections`（默认 2）条 SSH 会话轮流承担大请求，≤256 KiB 的读固定在第一条会话上。
  生产流式列举另用 `directory_connections` 条专用 SSH 连接（默认 2，上限 32），总上限
  为两者之和。逐条交付 VFS TEMP，取消不会中断普通读写句柄，详见 `sftp-directory-stream.md`。
- **写路径**：staging 一律按需创建（`ensureStaging`/`openStaging`），open 时不建也不下载——
  内核把 O_TRUNC 实现成「先 open，再用另一个句柄 truncate」，open 当场下载旧内容会让随后的写
  落进一份满尺寸的 staging，把旧尾巴一起提交回去；顺带也省掉了为写而打开大文件的整份下载。
  FLUSH 提交后句柄保持可写；提交与上传
  完成按 inode 更新；内核发起的变更（ctx 带 `vfs.FromKernel`）不向内核发失效。
- **元数据**：`pathOf`/`dirRemoteID` 按 ino 缓存、结构变更整体作废；写事务预编译；未变化的目录
  刷新只刷 fetched_at。
- **后台让路**：`vfs.FS.fgReads` 统计内核正在等待的读。目录预取在每次列举前调用 `waitIdle`
  （上限 `prefetchYield` = 5 s），hydrate 通过 `Cache.SetBusy` 在开始前与每拷一个块时检查，
  忙则放弃并重新排期（整个缓存一个 janitor 定时器，不是每文件一个）；判据也从「空闲 2 s」放宽到 `HydrateAfter` 默认 10 s，
  因为两秒的间隙在任何真实负载里都算不上安静。两者都是「宁可晚点预热，也不给前台读排队」。
- **每个操作的耗时**：`provider.Stats` 除调用次数外记录每个操作累计耗时，经
  `cloudfs_remote_seconds_total{op}` 暴露，`cloudfs bench` 输出 `remote_ms_by_op`。它回答的是
  「剩下的时间是往返本身还是往返之上的代码」——直连冷随机读 586 µs/次里 441 µs 在 SFTP 往返内。
  `CLOUDFS_PPROF=1` 时控制端口额外挂上 `/debug/pprof`（默认关闭，见 `docs/bench.md`）。
- **partial 块的 claim 次序**：重载把「有块文件、无 sidecar」读作整块，所以 partial 块只能由
  `openPartialBlock`（先写空 claim 再建文件）创建、由 `removePartialBlock`（先删文件再删 claim）
  删除——顺序留在调用点上时，三个调用点里有两个写反了。此后 claim 允许滞后于数据（只少不多），按 8 个子块 / 2 s 写一次，
  由缓存自己的定时器（`armClaimSweepLocked`，滞后计数器为零时不扫）补齐，`Cache.Close` 退出前再补一次。崩溃最多让缓存少认几个子块，
  永远不会把没取过的字节当命中。

### M9 带来的机制变化

- **内核侧可观测**：fusefs 为每种请求计数并记录 READ 尺寸直方图，经 `/metrics`
  暴露（`cloudfs_fuse_ops_total{op}`、`cloudfs_fuse_read_size_bucket{le}`），`cloudfs bench`
  按项输出差值。它直接回答了两个问题：热遍历的 78 ms 花在哪（每次 opendir 都在建整份
  列举，内核其实自己能答）、随机读一次要多少字节（16 KiB，于是子块就是 16 KiB）。
- **目录句柄**（§4.6 补充）：`OpendirHandle` 返回的句柄实现 `FileLookuper`，readdirplus 的每个
  条目由列举本身填充，不再逐条查库；列举在第一次读时才构建，内核缓存命中时只有一次
  opendir；挂载带 `noatime`，否则每次 readdir 后内核会作废目录属性再来一次 getattr。
  `PutDir` 报告增删改，只有「已缓存的目录真的变了」才向内核发失效——首次列举时的失效
  会把内核正在填充的目录缓存扔掉，这是热遍历重新读目录的根因。
- **SFTP 传输**（§4.1 补充）：远端句柄按（路径，版本）缓存并租借，读走 `ReadAt` 一次
  发出；自己的改名/删除/落地先关句柄；包大小按服务端 banner 选 255 KiB；短读由驱动
  追读、由 VFS 拒绝入缓存（`shortRead`）。
- **元数据热路径**：负缓存只在内存（它本来就是缓存），`MarkAbsent` 不再是写事务；
  `Path` 用递归 CTE 一次查完；create 后不再 Stat、flush 后不再 Get；`Store.QueryStats`
  让 `test/perf` 对每个操作设 SQL 预算。
- **前台操作与 uploader 的并发**：`DropPending` 在同一写事务内检查状态并删除 pending／
  dead／done；已被认领的拒绝删除，由 VFS 打墓碑。`DropSuperseded` 只删除仍为 pending
  的旧版本，候选快照后被领取的行保留给 worker；
  提交与上传完成按 inode 更新节点（`UpdateByIno`），不按名字 upsert，改名不会造出第二个
  节点，打开后被 unlink 的文件在 close 时不复活；上传落地释放 local-only 缓存项后，旧读句柄在
  下一次读时重取节点身份。
- **对象目录交接与回收**：`CommitStaging` 移入内容寻址目录时，按 staging ID 保存
  进程内临时引用；VFS 的 `Upload.StagingID` 在日志行提交成功后释放这一份引用，
  失败重试保留，同内容多个 staging 分别计数，重复提交不释放其他写者的引用。
  staging rename 与垃圾清理共用对象锁；清理在写事务内检查持久化引用并 unlink，
  防止新日志行在检查后插入。只清理 journal 私有 objects／合法 copies 路径。
  临时引用不是新的持久化承诺：未提交写入在进程退出后仍按未确认孤儿处理，
  已确认写入由日志行保护。普通丢行后的 unlink 仍为尽力清理；上传取消已接入停止
  意图，但取消后的对账／重试与本地版本销毁仍需要独立持久化管理流程。
- **持久化强度**：`journal.durability` 为 daemon 级配置。`power`（默认）保持三次 fsync；
  `crash` 一次都不做，`close()` 的承诺降为「进程崩溃不丢」。为此 staging 一律多算一个
  crc32c，`Recover` 对每一行核对 size（所有模式）与 crc（crash 模式），不符的死信并附
  原因；死信的节点由 `RepairLost` 从树上摘掉、父目录列举作废，下一次读回远端版本。

## 8. 验证

| 层次 | 位置 | 覆盖 |
|---|---|---|
| 单元 | 各 `internal/*` 包的 `_test.go` | MetaStore TTL / 负缓存 / 索引、块缓存位图与淘汰、日志状态机、限流 AIMD、代理规则匹配、错误分类 |
| 驱动 | `internal/provider/*/` | 每个驱动用 `httptest` 回放真实响应形状，断言请求路径、必需 header、分页、秒传、分片、错误码映射 |
| 语义 | `test/conformance` | 同一组操作分别跑在 cloudfs 挂载与本地目录上并逐项比对：创建 / 读写 / seek / append / truncate / rename / readdir / ENOENT / 空文件 / 大文件；硬链接、符号链接、chmod 的差异被显式断言 |
| 可靠性 | `test/chaos` | 设计文档第 5 节矩阵：断网重试、kill -9 恢复、429 降速、风控熔断、缓存满降级、冲突副本、死信与重排、链接过期、并发写、只读拒绝 |
| 性能 | `test/perf` | 上表的调用次数基线，外加两个 benchmark |
| 端到端 | `test/e2e` | 配置文件 → 守护进程 → 真实 FUSE 挂载 + 真实 MCP 会话：终端与 agent 互相看得见对方的写入、崩溃恢复、只读双向拒绝、status 与 metrics 反映真实工作、`grep -r` 零远端调用 |

竞态检测（`go test -race`）覆盖 VFS、缓存、元数据、日志、上传与端到端包。

FUSE 相关测试在没有 `/dev/fuse` 或 macFUSE 的机器上自动跳过而不是失败。

### 仍需真实账号验证

- 各国内网盘驱动的秒传握手、分片参数与错误码，标注在代码里的 `UNVERIFIED:` 注释中。
- 长时间运行下的风控行为：按推荐 QPS 跑 30 分钟不触发限制。
- macOS 上的 macFUSE 挂载与 Finder 交互。

## 9. 风险与待定项

| 风险 | 应对 |
|---|---|
| OpenList / AList 为 AGPL-3.0 | 只参考协议细节，按官方文档自行实现；若移植代码则全项目 AGPL |
| 夸克、115 cookie 接口非官方，可能失效或封号 | `Tier=unofficial` 默认更保守；优先官方 Open API；OpenList bridge 作退路 |
| 阿里三方权益包、百度非 SVIP 限速 | 账号侧限制，系统只能降速适配；状态页明示 |
| passthrough 需 Linux 6.9+ 与 `CAP_SYS_ADMIN` | 仅作加速路径，不影响正确性 |
| macFUSE 需 kext，FSKit 后端不完整 | 一期以 macFUSE 为准，FSKit 作试验开关 |
| MCP 规范 2026-07-28 为无状态版本，Roots 已 deprecated | 锁定 SDK 版本；白名单以 `--allow` 为主 |
| SQLite 单写者 | 元数据与 journal 分库；写事务短小；WAL 模式读不阻塞 |

---

## 10. 参考资料

- rclone mount / VFS：https://rclone.org/commands/rclone_mount/ ；FAQ 代理：https://rclone.org/faq/ ；union：https://rclone.org/union/ ；crypt：https://rclone.org/crypt/
- rclone 115 分支：https://github.com/wiserain/rclone/releases
- OpenList 驱动文档：https://openlistteam.github.io/docs/zh/guide/drivers/ ；通用设置：https://doc.oplist.org/guide/drivers/common
- CloudDrive2 缓存分析：https://littlenewton.uk/2025/03/introduction-clouddrive2-buffer-policy/index.html
- JuiceFS 架构：https://juicefs.com/docs/community/architecture/ ；缓存：https://juicefs.com/docs/community/guide/cache/
- FUSE passthrough：https://docs.kernel.org/filesystems/fuse/fuse-passthrough.html ；go-fuse：https://github.com/hanwen/go-fuse ；macFUSE 5：https://macfuse.github.io/2025/05/05/macfuse-5.0.0.html
- Google Drive changes：https://developers.google.com/workspace/drive/api/guides/manage-changes ；Graph throttling：https://learn.microsoft.com/en-us/graph/throttling ；Dropbox 变更检测：https://developers.dropbox.com/detecting-changes-guide
- 阿里云盘开放平台：https://www.yuque.com/aliyundrive/zpfszx ；百度网盘开放平台：https://pan.baidu.com/union/doc/ ；115 开放平台公告：https://115.com/115/T843700.html
- MCP 规范：https://modelcontextprotocol.io/specification/2026-07-28/ ；官方 filesystem server：https://github.com/modelcontextprotocol/servers/blob/main/src/filesystem/README.md
- Claude Code MCP：https://code.claude.com/docs/en/mcp ；Codex MCP：https://learn.chatgpt.com/docs/extend/mcp?surface=cli
- Clash 规则与出口：https://en.clash.wiki/configuration/outbound.html
