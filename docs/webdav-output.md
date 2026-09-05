# 只读 WebDAV 输出

CloudFS 可以把一棵 VFS 子树作为 WebDAV 输出，供播放器、媒体库或不能安装 FUSE 的客户端
读取。它不是另一个 provider 客户端：PROPFIND、GET、HEAD 和 Range 全部调用同一个 VFS，
因此继续使用元数据缓存、块缓存、代理、限流、熔断和打开句柄生命周期。

## 配置

```yaml
webdav:
  http: 127.0.0.1:8080
  prefix: /dav
  root: /media
  strategy: proxy
```

端点随 `cloudfs mount` 或 `cloudfs mcp` 的 owner 进程启动。`root` 是规范化绝对 VFS 路径；
DAV 客户端看到它为 `/`，不能通过 `..`、双斜杠或反斜杠逃逸。`prefix` 必须是非根的规范
URL path。省略 `webdav.http` 即关闭服务。

回环监听可以不设 token。任何非回环监听都必须通过环境变量提供 token，否则进程在 bind
前失败：

```sh
export CLOUDFS_WEBDAV_TOKEN="replace-with-a-long-random-token"
cloudfs mount --config ~/.config/cloudfs/config.yaml
```

客户端可用 bearer token，或使用 Basic Auth 用户名 `cloudfs`、密码为同一个 token。
内置 server 是明文 HTTP；跨主机使用时应放在有 TLS 的反向代理或可信 VPN 后面，不能把
Basic/bearer token 直接发送到不可信网络。token 不进入 YAML、URL 或 argv。

## 只读契约

当前允许 `OPTIONS`、`PROPFIND`、`GET` 和 `HEAD`。PUT、DELETE、MKCOL、MOVE、COPY、LOCK、
PROPPATCH 等方法明确返回 405，OPTIONS 也只公布上述四种方法。PROPFIND 只接受 Depth 0/1，
拒绝 infinity，并把 XML 请求体限制为 64 KiB，避免一次客户端请求递归扫描整棵云盘。

GET 支持标准 Range 和条件读取。ETag 来自 remote + provider version 的截断 SHA-256，既能
稳定标识版本，也不会把 provider 的签名/账号相关 opaque version 直接泄露给客户端。
目录和文件元数据来自 VFS `Attr`；读取使用持久 VFS handle，响应结束必定 Release。

下载策略：

- `proxy`（默认）：字节始终经过 VFS 和块缓存，provider 所需 UA/Referer 由驱动负责。
- `redirect`：GET/HEAD 获取 provider shareable link 并返回 302；只接受无 userinfo 的
  `http(s)` URL，响应 no-store。链接需要任何额外 header、不可用或不安全时返回 502，
  不把一个必然失败的链接交给播放器。
- `auto`：满足上述安全条件就 302；本地待上传版本、provider 错误、非法 URL，或百度/
  115/夸克这类带 UA/Referer 的 link 自动回落到 proxy。目录请求始终走 DAV handler。

CloudFS 的 bearer/Basic 凭据不会附到 redirect URL，也不会转发给 provider host。

只读是刻意的第一阶段边界：当前 writeback 的 close 成功只表示本地 durable journal 已提交，
而 WebDAV 客户端对覆盖、锁、If-Match 和 MOVE 的预期需要单独定义。未完成这些条件请求和
远端对账契约前，不把现有 VFS 写方法直接拼成一个看似可写的 WebDAV server。

## 容器

Compose 已映射容器 8080 到宿主 `CLOUDFS_WEBDAV_HOST:CLOUDFS_WEBDAV_PORT`，默认仍是
`127.0.0.1:8080`。要从容器外访问，配置中的 `webdav.http` 应为 `0.0.0.0:8080`，同时设置
`CLOUDFS_WEBDAV_TOKEN`；将 `CLOUDFS_WEBDAV_HOST` 改为 `0.0.0.0` 前先准备 TLS/VPN 和宿主
防火墙。MCP-only 模式无需 `/dev/fuse` 即可同时提供 WebDAV。

## 可写模式

默认只读。`webdav.writable: true` 打开 PUT / DELETE / MKCOL / MOVE / COPY /
PROPPATCH / LOCK / UNLOCK，OPTIONS 随之公布这些方法并声明 `DAV: 1, 2`（class 2 是
锁，只在真的提供 LOCK 时才声明——让客户端看到 2 却在 LOCK 上吃 405 比没声明更糟）。
写入走的是 FUSE 用的同一条 VFS 路径：PUT 落到 journal 暂存、关闭句柄时提交、上传队列
异步送到网盘，一致性模式仍由挂载子树的配置决定，只读挂载上的 PUT 被拒绝。

几处不是照抄 `golang.org/x/net/webdav` 默认行为的地方，都是为了不给出错误的答案：

- **COPY 由驱动或本地缓存完成，不经过本进程搬字节。** DAV 库的 COPY 实现是打开源、
  打开目标、逐字节复制；在网盘上这意味着完整下载再完整上传，而 `vfs.Copy` 本来就能
  用服务端 copy 或用已缓存的 blob 直接秒传。所以 COPY 被拦下来交给 `vfs.Copy`。
  目录 COPY 明确返回 403 并说明原因：`vfs.Copy` 是文件原语，在适配层里递归遍历目录
  等于把逻辑下沉到了错误的一层。
- **PUT 不返回 ETag。** DAV 库在写入提交之前就向文件要 ETag，此时节点上的版本描述的
  还是旧内容。返回一个和下一次 GET 对不上的校验符比不返回更糟，所以这里删掉它。
- **只读挂载上的写返回 403 而不是 404/405。** DAV 库把 OpenFile 的任何失败都压成 404、
  把 RemoveAll 的任何失败都压成 405，于是一个能被列出来的目录里的文件会报「不存在」。
  适配层记录「因策略被拒」并在响应写出时改正状态码，处理器本身不变，锁的执行不受影响。
- **MOVE 的目标越界返回 403，跨主机返回 502**，而不是 DAV 库的 404：出问题的是目标，
  不是源。
- **导出根不可删除、不可改名**：它在这个命名空间里没有父目录，删掉它等于端掉整棵导出树。

XML 体（PROPFIND / PROPPATCH / LOCK）限制在 64 KiB；PUT 的体是文件本身，不受此限。

## 尚未验收

- Emby、Jellyfin、Infuse 等真实客户端的兼容性和大文件长播；
- 非回环 TLS reverse proxy、NAS Docker 与断线重连；
- 真实客户端下的写入、锁与配额行为：可写模式有完整的单元与端到端回归，但没有 Finder、
  Explorer、rclone 或媒体客户端实际写入过；上传是异步的，PUT 返回 201 只表示已提交到
  本地日志（writeback）或已上传完成（strict），由挂载子树的一致性模式决定。
- STRM 已可通过 `cloudfs strm` 生成；其真实扫描/播放兼容性，以及 redirect 链接在媒体
  客户端中的过期/重试行为仍未验收，见 `strm.md`。
