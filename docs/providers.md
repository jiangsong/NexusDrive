# 驱动接入状态

每个驱动都实现同一个 `provider.Provider` 接口并声明能力矩阵 `Caps`，上层不区分具体网盘。本文记录各驱动的实现依据、已知约束，以及需要真实账号才能确认的部分。

## 总览

| 驱动 | 接口来源 | 秒传 | 分片 | 服务端移动/改名 | 直链可外传 | 默认 QPS（meta/下载/上传） | CDN 传输 QPS | 层级 | 分享链接（`Caps.Share`） |
|---|---|---|---|---|---|---|---|---|---|
| `webdav` / `openlist` | 标准 WebDAV | 无 | 单次 PUT | 是 | 否（需本进程凭据） | 8 / 8 / 4 | 共用下载 | official | 否 |
| `aliyun` | 阿里云盘开放平台 | pre_hash + SHA1 + proof | 是 | 是 | 是 | 4 / 4 / 2 | 16 | official | **是**（`share_link`，UNVERIFIED） |
| `baidu` | 百度网盘开放平台 | MD5 + 前 256 KiB MD5 | 是 | 是 | 是（需特定 UA） | 2 / 2 / 1 | 4 | official | **是**（`share?method=set`，四位提取码，UNVERIFIED） |
| `pan115` | 115 开放平台 | SHA1 + 区间签名 | OSS 分片 | 是 | 是（UA 绑定） | 1 / 2 / 1 | 4 | official | 否 |
| `pan123` | 123 云盘开放平台 | MD5 | 是 | 是 | 是 | 4 / 4 / 2 | 共用下载 | official | 否 |
| `quark` | 逆向 cookie 接口 | MD5 + SHA1 | 是 | 是 | 是 | 1 / 2 / 1 | 4 | **unofficial** | 否 |
| `tianyi` | 天翼云盘 | MD5 | 是 | 是 | 是 | 2 / 2 / 1 | 4 | official | 否 |
| `sftp` | SSH 文件传输子系统 | 无 | 是（按偏移写） | 是 | 否（需本进程凭据） | 8192 / 8192 / 4096 | 共用下载 | official | 否 |
| `s3` | S3 API（MinIO Go SDK） | 无 | 是（multipart） | 是（copy + delete） | 是（预签名） | 20 / 16 / 8 | 共用下载 | official | 否 |
| `dropbox` | Dropbox HTTP API v2 | 无 | 是（upload session） | 是 | 是（4 小时临时链接） | 12 / 12 / 6 | 共用下载 | official | **是**（`create_shared_link_with_settings`，UNVERIFIED） |
| `onedrive` | Microsoft Graph v1.0 | SHA-1 校验 | 是（upload session） | 是 | 是（预认证链接，保守 15 分钟） | 12 / 12 / 6 | 共用下载 | official | **是**（`createLink` anonymous，UNVERIFIED） |
| `gdrive` | Google Drive API v3 | 无 | 是（resumable session） | 是 | 否（仅认证请求可读） | 10 / 10 / 4 | 共用下载 | official | **是**（anyone/reader 权限 + `webViewLink`，UNVERIFIED） |
| `box` | Box Content API v2 | 无 | 是（upload session，需整文件 SHA-1） | 是 | 否（302 只对本进程有效） | 8 / 8 / 4 | 共用下载 | official | 否 |
| `smb` | SMB2/3（go-smb2） | 无 | 是（按偏移写） | 是 | 否（需本进程会话） | 4096 / 4096 / 2048 | 共用下载 | official | 否 |

`分享链接` 列是 T-55 的 `provider.Sharer`：`share` MCP 工具与控制台 `POST /share` 只对 `Caps.Share` 的驱动可用，
每个实现都标 `UNVERIFIED` 直到真实账号验收（endpoint、过期与提取码规则、应用权限范围都待核）。

`CDN 传输 QPS` 是 `internal/net/ratelimit.Transfer`：字节流的 ranged GET（走 CDN 直链）单独计
额度，不再与解析直链的元数据调用（`getDownloadUrl` 之类，仍计入下载 QPS）挤同一个令牌桶。
"共用下载"表示该驱动的 `Caps.QPS.Transfer` 为 0（默认值），此时 Transfer 请求直接落在
Download 的令牌桶和 AIMD 状态上，行为与引入这个分类之前完全一致；数值列出的五个国内网盘
驱动已经拆出独立的传输额度，均为 `// UNVERIFIED: CDN request-rate threshold before risk
control` ——真实阈值需要在真机账号上验证。`remotes.<name>.qps.transfer` 可覆盖任何驱动的
默认值，同样遵循"配置 > Caps 推荐 > 内置默认"的优先级。

`Tier` 为 `unofficial` 的驱动默认采用更保守的限流，因为接口随时可能变化，且异常调用模式更容易触发封号。

**S3 / 兼容对象存储**：可选 `prefix` 把所有 key 限制在一个命名空间，`/` 分隔的 key
通过 delimiter 投影为目录；`Caps.StreamList` 使用 SDK 的带 context 流式迭代，visitor
退出时取消并排空 channel。文件读取使用 Range + If-Match ETag，小文件单 PUT，大文件把
multipart upload ID/key/size 持久化进 journal session，重启只补缺失 part。下载直链为
15 分钟 SigV4 预签名 URL，不含 secret key。

S3 没有原子 rename。文件和目录移动遵循先 CopyObject、确认完整目标、再删除源对象；复制
失败回滚已复制目标，删除阶段失败时优先保留数据但可能留下重复对象。目录操作以一百万对象
为硬上限，provider 根拒绝删除。AWS/MinIO/R2 等真服务及版本化 bucket 尚未验收，详见
[S3 驱动](s3.md)。

**Dropbox**：路径 `path_lower` 用作 provider ID，`rev` 用作内容版本；`list_folder/continue`
逐页流式列举，account-wide recursive cursor 提供 upsert/delete delta，reset 会换基线并使目录
freshness 失效。Range body 交付前会核对响应元数据的路径和 revision，临时链接约四小时有效。
小文件单次上传，大文件的顺序 upload session 可跨 journal/进程恢复；服务端报告的
`incorrect_offset` 只有精确落在本 part 末端时才作为幂等确认。Dropbox `content_hash` 不是普通
SHA-256，所以不冒充通用哈希。`cloudfs config auth` 已可用浏览器完成授权；Dropbox 以 PKCE
公共客户端授权，**不需要 client secret**。真实账号验收尚未完成，详见
[Dropbox 驱动](dropbox.md)。

**OneDrive**：Graph `DriveItem.id` 用作稳定身份，文件版本优先取 `cTag`，并接受官方 SHA-1。
children 和 delta 都保留原生 continuation URL，但严格限制在配置的 Graph origin/API 路径内；
delta 过期会换基线并让目录 freshness 全局失效。预认证下载/upload-session URL 不带 Bearer，
下载链接按 item/version 短期内存缓存并在 403/410 后重新核对版本。上传 fragment 顺序发送、
320 KiB 对齐、单请求小于 60 MiB，session 可跨重启恢复；服务端移动/改名/删除可用，但没有
声明同步 ServerCopy。真实 Microsoft/SharePoint 账号和浏览器 OAuth 尚未验收，详见
[OneDrive 驱动](onedrive.md)。

**Google Drive**：文件 id 直接作为 provider ID，内容版本优先取 `headRevisionId`，
`ReadRange` 因此可以直接下载 `/files/{id}/revisions/{rev}?alt=media` 把读钉在某个修订上，
远端并发改写不会把新字节塞进旧的块缓存键。修订被清理时回退到 head，但只在重新确认版本
未变之后才回退。两处 Drive 语义与文件系统不兼容，驱动显式处理而不是留给上层踩：

- **同名子项**：Drive 允许一个目录里有多个同名文件，元数据层按名字索引子项，无法发布这样
  的列表。驱动检测到冲突后让该目录失败并在错误里点名冲突的名字，不静默丢弃或改名。用户需
  要在 Drive 里改掉其中一个。跨页冲突由 `ListStream` 保留的名字集合捕获。
- **Workspace 文档与快捷方式**：Google Docs/Sheets 等没有字节流，只能导出，且导出前大小
  未知。`List`/`Changes` 跳过它们，`Stat` 返回 `ErrUnsupported`。

写入路径会先查同名子项：命中就更新该文件（多段或 resumable 都走 `PATCH`），否则创建，避免
自己制造出上面那种同名冲突。`changes` feed 提供 upsert/delete，页令牌失效换基线。私有内容
只对带凭据的请求可读，所以 `DownloadURL` 返回 `ErrUnsupported`、`Caps.LinkShareable` 为假。
`cloudfs config auth` 已可用浏览器完成授权，真实账号验收尚未完成。

**Box**：Box 的文件与文件夹是两套独立编号，同一个数字可以既是文件又是文件夹，且端点不同。
provider ID 因此带类型前缀（`f:12345` / `d:12345`），根是 `d:0`；上层只把它当不透明 ID。
内容版本取 `file_version.id`，读取带 `version` 参数钉住版本。分片上传由服务端决定 part size，
`commit` 必须带整文件 SHA-1，所以能力矩阵声明 `HashSHA1`，缺哈希时直接拒绝开 session；
每个 part 另带自己的 `Digest: sha=`。Box 只接受 20 MB 及以上的 session，20 MB 以下必须走
单次上传，因此 `SinglePutMax` 正好取在这个分界上，中间没有无法上传的区间。同名上传由 409
的 `context_info.conflicts` 给出既有 id，转为该文件的新版本而不是创建第二个同名文件。
commit 返回 202 表示服务端仍在组装，映射为 `ErrTransient` 让上传队列重试（commit 幂等）。
Box 的事件流是账号级 feed 而非目录 delta，`Caps.Delta` 为假，目录按 TTL 刷新。
`cloudfs config auth` 已可用浏览器完成授权，真实账号验收尚未完成。

**SMB**：面向 NAS 与 Windows 共享。与 SFTP 一样没有文件 id、没有内容哈希、没有变更流，
路径即身份，版本回退到 size+mtime 指纹。一处关键差异塑造了写路径：go-smb2 的 `Rename`
发送的 `FileRenameInformation` 里 `ReplaceIfExists` 为 0，**改名不会覆盖已存在的目标**。
所以发布上传时必须先 unlink 目标再改名，中间有一个"名字不存在"的窗口——这是协议在这一层
暴露的操作所能做到的极限，明确记录而不是用重试掩盖。移动（`Move`）则不做这个 unlink：
它必须不能悄悄毁掉目标位置上的无关文件，目标已存在时返回 `ErrExists`。

读路径缓存每个路径一个打开句柄：SMB 打开文件是一次完整的 CREATE 往返，按 64 KiB 子块读时
每次重开会让冷随机读的开销翻三倍。句柄在版本变化、改名、删除、发布上传和连接断开时失效，
且带引用计数——正在读的句柄不会被另一线程的删除关掉。连接断开后整个会话（连同它上面所有
句柄）都失效，因此断链只重试一次并强制重新挂载。SMB 不走 HTTP，无法通过共享 HTTP 客户端
继承代理与限流，daemon 通过 `provider.ConfigDialer` / `ConfigLimiters` 直接注入。

`ServerCopy` 为假：SMB2 有 FSCTL_SRV_COPYCHUNK，但 go-smb2 没有导出它，声明了会让 VFS
选一条跑不通的路径。**尚未在任何真实 SMB 服务器上验收**：当前测试用内存共享复现了
"改名不覆盖""非空目录不可删""短读"这三条服务端行为，这不能替代真机验证。

## 各驱动的具体约束

**WebDAV / OpenList 目录枚举**：`Caps.StreamList` 声明逐条流式 PROPFIND。
VFS 不加载整个 HTTP 响应，解析器按 DAV:response 交付，单元素读取预算 1 MiB
（另有解析缓冲与结构开销）。完整 XML 结束前不发布目录，成员属性全部失败也
不能当成该成员不存在；截断、取消、回调失败关闭响应体且不重放前缀。兼容 List
仍按接口约定返回完整切片，Stat 仍是 Depth:0 全量解码。URL 转义的 #、?、字面
%2F 保留在资源 ID 中，href 必须在配置根路径下。详见 [目录刷新边界](directory-refresh.md)。

**SFTP**：面向自建存储（NAS、工作站、租用主机）。SFTP 没有每文件 id、没有内容哈希、
也没有变更流，所以路径就是身份，能力矩阵如实声明这三项为无，上层因此退回到
「大小 + mtime 指纹」和 TTL 刷新，而不是 delta。

生产目录枚举已走独立 v3 读取器和专用 SSH 池，Caps.StreamList 为 true，逐条
送入 VFS TEMP；报文、EOF/CLOSE、属性补查和取消均有检查。普通文件连接不会因
取消目录列举被关闭。兼容 List 仍累计完整切片，递归删除仍用 pkg/sftp ReadDir。
详见 [SFTP 目录流与验证边界](sftp-directory-stream.md)。

上传先写同目录下的 `.cloudfs-upload.<name>.<纳秒>` 暂存文件，每个分片按自己的偏移
`WriteAt`，因此分片可以乱序到达、中断后可以只补没传完的那些；全部就位后再
`PosixRename` 原子改名到目标名，所以读者永远看不到写了一半的文件。改名前会核对
暂存文件的大小与声明大小是否一致——分片写短了却被当成成功，是对文件系统而言最坏的
一类故障。服务端不支持 `posix-rename` 扩展时退回「先删后改名」，这中间存在一个目标名
不存在的窗口，SFTP 协议本身没有办法消除。

读路径复用远端句柄：同一路径同一版本的文件在 45 秒内共享一个已打开的句柄（上限 32 个），
一次未命中就是一次 `READ` 往返，而不是 `OPEN`+`READ`+`CLOSE` 三次；一个 4 MiB 块通过
`ReadAt` 一次发出、由 pkg/sftp 并发拆包。我们自己的改名、删除、上传落地会先关掉该路径的
句柄（POSIX 语义下旧句柄指向旧 inode，Windows/SMB 后端更会拒绝对打开中文件改名）。
`connections`（默认 2）个文件 SSH 连接轮流承担大请求（≤256 KiB 的读固定在第一条会话上，随机读的句柄因此常热）；另有 `directory_connections` 个专用目录连接（默认 2，上限 32），总连接上限为两者之和，均按需创建。一条连接就是服务端一个核上的一条密码流，
局域网上冷顺序读因此被卡在 ~235 MB/s；两条起就能用上更多链路。请求包大小 `packet_size` 默认为 0（自动）：服务端 banner 含 `OpenSSH` 时用 255 KiB
（sftp-server 的上限，rclone 的 `--sftp-chunk-size 255k` 同款），其他服务端保持协议规定的
32 KiB；服务端若把请求截短，驱动会追读剩余部分，绝不返回短数据。`unlink` 直接发
`REMOVE`，失败再判断是否目录，一个文件删除只有一次往返。

主机密钥默认按 `~/.ssh/known_hosts` 校验，并且会**从 known_hosts 里已有的记录反推
要协商的密钥算法**：否则 Go 客户端按自己的偏好选算法，遇到文件里只有 ed25519 记录的
主机就会把「已知主机」误报成 key mismatch。`insecure_host_key: true` 可以关掉校验，
但不是默认——这条链路可能经过代理，不校验等于把会话暴露给中间人。

连接是长驻的：每 30 秒发 `keepalive@openssh.com`，链路断掉时下一次调用自动重连重试
一次（只对看起来是「会话已死」的错误重试，服务端明确拒绝某个操作时不重试）。

不超过一个分片（默认 8 MiB）的文件走单次上传：`Create` 暂存 → 写入 → 原子改名，
一次限流等待完成，而不是 BeginUpload / UploadPart / CompleteUpload 三次。这是小文件
队列排空速度的主要来源。

默认 QPS 比网盘驱动高一个数量级，因为 SSH 没有服务端配额也没有风控，真正的约束是
连接本身。局域网实测：把上传限到 32/s 时，500 个小文件的队列要 74 秒才排空，而链路
本身只需要 5 秒——瓶颈完全在令牌桶。仍然可以用 remote 的 `qps` 覆盖，AIMD 也照常在
服务端真正拒绝时降速。

配置示例：

```yaml
remotes:
  nas:
    type: sftp
    host: 192.168.0.20     # 也接受 host:port
    port: 22               # 可选
    user: work             # 可选，默认当前用户
    key_file: ~/.ssh/id_ed25519   # 可选；不填则试 agent 与 ~/.ssh 下的默认密钥
    root: ~/test           # 可选，默认登录目录；~ 由服务端解析
    concurrency: 4         # 可选，分片并发与每文件在途请求数
    connections: 2         # 普通文件 SSH 连接
    directory_connections: 2 # 额外目录 SSH 连接；默认合计最多 4 条
```

**阿里云盘**：下载链接默认 15 分钟过期（最长 4 小时），驱动缓存链接并在临近过期时刷新，CDN 返回 403 时刷新一次。秒传走官方两段式：先用前 1 KiB 的 SHA1 探测，服务端命中后再提交完整 SHA1 与 proof_code。`TooManyRequests` 映射为风控而非普通限流，因为阿里对重试 429 的账号会直接封禁。

**百度网盘**：超过约 20 MB 的下载必须带 `User-Agent: pan.baidu.com`，否则服务端拒绝；该头同时写进 `Caps.LinkHeaders`，MCP 的 `get_download_url` 会一并交给调用方。非 SVIP 账号有速度限制，这是账号侧约束，系统只能降速适配。`Entry.ID` 是 `fs_id:path` 的复合形式，因为管理类接口按路径寻址而元信息与下载链接按 fs_id。

**115**：同一账号同一应用只有两个有效 refresh token，第三次登录会静默作废最早的一个。秒传是两步握手：服务端可能要求客户端对指定字节区间做 SHA1 再提交。因为 `BeginUpload` 只拿到哈希拿不到内容，驱动暴露了一个区间哈希回调；未注入时该挑战会明确失败并降级为分片上传，而不是猜一个签名把文件注册到未经校验的内容上。默认 QPS 设为 1，115 对第三方客户端的风控相当激进。

**夸克**：官方开放平台仍在闭门测试且无公开文档，本驱动使用逆向的 cookie 接口。接口可能随时失效，风控响应映射为 `ErrRiskControl` 以触发熔断。账号 cookie 不会发送到 OSS 上传主机。

**天翼云盘**：登录凭据用 RSA 加密后提交；部分接口返回 XML。签名的规范化字符串顺序尚未在真实账号上确认。

**123 云盘**：分片大小由服务端在会话里返回，驱动不写死。`reuse:true` 但 `fileID:0` 的响应不当作秒传命中。

## 待真实账号确认

代码里用 `// UNVERIFIED:` 标注了每一处依据公开文档推断、但未在真实账号上跑通的细节：

| 驱动 | 数量 | 主要集中在 |
|---|---|---|
| tianyi | 17 | 签名规范化字符串、登录页字段、返回码枚举、分片 XML 结构 |
| quark | 12 | 各数字错误码、异步任务状态枚举、OSS 签名的规范化字符串 |
| pan115 | 13 | token 错误码、上传初始化字段名、OSS 区域与回调体 |
| aliyun | 4 | 秒传端到端路径、部分私有 API 错误码、异步移动任务 |
| pan123 | 3 | 链接有效期、分片数上限、重名判定（只能靠消息文本） |
| baidu | 2 | 上传主机发现（locateupload）、precreate 的 block_list 占位 |

用以下命令列出全部条目：

```sh
grep -rn 'UNVERIFIED:' internal/provider/
```

以上合计 51 处非测试源码标记。实际账号核验前不能笼统保证这些未确认路径只影响
可用性而绝不影响数据正确性；模拟测试和错误处理不代替服务端行为验收。

## 新增一个驱动

1. 在 `internal/provider/<name>/` 下实现 `provider.Provider`。
2. `Factory` 里用 `httpx.FromConfig(cfg, fallback)` 取 HTTP 客户端，这样才能继承代理规则、限流与熔断。
3. 实现 `SetTransport(client any)`，供守护进程在构造后注入。
4. 如实填写 `Caps`：上层完全按它决策，声明了却没实现的能力会变成运行时错误。
5. `init()` 里 `provider.Register("<name>", Factory)`，并在 `cmd/cloudfs/main.go` 里加空导入。
6. 用 `httptest` 写测试，回放真实响应形状，断言请求路径、必需头、分页、秒传、分片与错误码映射。

## 配额与命名规则（存储池用）

| 驱动 | 配额（`provider.Quotaer`） | 命名规则（`Caps.Naming`） |
|---|---|---|
| gdrive | About.storageQuota（无上限账号报告为未知） | 无限制 |
| webdav | RFC 4331 quota-available/used-bytes，服务器不支持则未知 | 禁 `\`，255 字节 |
| aliyun | `getSpaceInfo`（UNVERIFIED） | 禁 `\`（UNVERIFIED） |
| baidu / pan115 / pan123 / quark / tianyi | 未实现（放置不按空间优先，可配 `capacity`） | Windows 类禁字符集（UNVERIFIED） |
| onedrive | 未实现 | 大小写不敏感，禁 `<>:"|?*\`，保留名，不能以点/空格结尾 |
| smb | 未实现 | 大小写不敏感，Windows 保留名与禁字符 |
| dropbox | `/2/users/get_space_usage`，individual 配额取 `allocation.allocated`；team 空间形状不同、当前读不了，一律报告为未知（不猜测，UNVERIFIED） | 大小写不敏感，不能以点/空格结尾 |
| box | 未实现 | 大小写不敏感，不能以点/空格结尾 |
| sftp / s3 | 未实现 | 255 字节名 / 1024 字节键 |

驱动没声明的规则，存储池会在成员实际拒绝时学习下来（`member_naming` 表），之后不再往那个成员放同类名字。
