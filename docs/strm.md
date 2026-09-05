# STRM 媒体库

`cloudfs strm` 把一个 VFS 媒体目录镜像成同层次的 `.strm` 文件。它通过已经运行的
CloudFS WebDAV 做 PROPFIND，不需要 FUSE，也不会再打开一套 journal、provider 或上传器。
播放器打开 `.strm` 中的 URL 时，数据仍按 `webdav.strategy` 走 VFS proxy、302 redirect
或 auto 回落。proxy 路径的连续读取会逐步扩大预读窗口；Range 跳播会取消旧播放位置的
预读，并从目标附近的子块重新开始，不填充两点之间的内容。

## 使用

先在 owner 进程中启用 WebDAV：

```yaml
webdav:
  http: 127.0.0.1:8080
  prefix: /dav
  root: /media
  strategy: auto
```

然后在 owner 运行期间执行：

```sh
export CLOUDFS_WEBDAV_TOKEN="replace-with-the-running-server-token"
cloudfs strm /media/Films --out /srv/emby/Films
```

`/media/Films/Movie.mkv` 会生成 `/srv/emby/Films/Movie.strm`，内容类似：

```text
http://127.0.0.1:8080/dav/Films/Movie.mkv
```

源路径必须位于配置的 `webdav.root`。如果生成器访问 WebDAV 的地址和配置中的监听地址
不同，可传 `--base-url https://media.example/dav`；它表示 `webdav.root` 对应的 DAV URL，
源路径的相对部分会自动追加。

可选参数：

- `--ext mkv,mp4,avi`：替换默认媒体扩展名集合，大小写不敏感；
- `--depth 64`：最大目录深度，范围 1–256；
- `--max-files 100000`：最大媒体文件数，范围 1–1000000；
- `--embed-basic-auth`：把用户 `cloudfs` 和 token 写入每个 URL。
- `--prune`：删除 manifest 证明由上次运行生成、且内容未被修改的陈旧 `.strm`。

## 认证与安全

默认 `.strm` 不含凭据。Emby/Jellyfin 等客户端应为 WebDAV URL 单独配置 Basic Auth，用户
固定为 `cloudfs`，密码为启动 owner 时的 `CLOUDFS_WEBDAV_TOKEN`。只有客户端确实不能保存
认证时才使用 `--embed-basic-auth`；这类 `.strm` 会强制写成 0600，但目录仍包含明文可恢复
的凭据，应限制目录访问、备份和日志采集范围。生成器自己的 PROPFIND 始终从环境变量取
token，不把它放进 argv/YAML。

生成器只接受同源、仍位于起始 DAV 根且确为当前目录直接子项的响应；单个 XML 响应限制
8 MiB。输出目录逐层拒绝符号链接和 `..`，目标名发生大小写不敏感碰撞时停止。文件以临时
文件 fsync 后原子替换；重复执行时内容相同的文件不改写。

默认不自动删除旧 `.strm`：远端临时不可见或部分枚举不应导致本地媒体库被清空。显式
`--prune` 会维护 0600 的 `.cloudfs-strm-manifest.json`；首次运行只建立基线，后续只删除
manifest 记录且 SHA-256 内容仍一致的旧文件。用户修改过的文件会保留，并从新 manifest
移交为非托管；缺失文件忽略。换源、manifest 损坏、路径异常、symlink 或非普通目标都会
在删除前失败。删除目录项和新 manifest 都经过 fsync。生成器不移除目录或任何未登记文件。

## 尚未验收

- Emby、Jellyfin、Infuse 的真实扫描、认证、播放和 302 链接过期重试；
- 十万级目录在真实 provider 限流下的耗时和调用数；
- 不同真实 provider/播放器并发模型下的预读参数调优，以及自动安全清理陈旧 `.strm`。
