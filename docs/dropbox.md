# Dropbox 驱动

CloudFS 的 `dropbox` provider 直接使用 Dropbox HTTP API v2，不引入完整 rclone 依赖树。
元数据与内容请求都经过共享 `httpx`，因此继承 remote 级代理路由、三类限流、AIMD 和熔断。
实现依据是 Dropbox 官方的 [HTTP API 定义](https://github.com/dropbox/dropbox-api-spec)
与 [OAuth 指南](https://developers.dropbox.com/oauth-guide)。

## 配置与凭据

临时测试可以导入 access token：

```yaml
remotes:
  db:
    type: dropbox
```

```sh
printf '%s' "$DROPBOX_ACCESS_TOKEN" | \
  cloudfs config auth db --stdin --field access_token --no-check
```

需要守护进程长期后台访问时，应保存 offline OAuth grant 的 refresh token，并把 app key
放在 `client_id`；机密 app secret 仍通过 secret store 导入：

```yaml
remotes:
  db:
    type: dropbox
    client_id: APP_KEY
    refresh_token: secretfile:db/refresh_token
    client_secret: secretfile:db/client_secret  # 公共/PKCE client 可不填
    # part_size: 8MiB
```

```sh
printf '%s\n' 'refresh_token: "..."' 'client_secret: "..."' | \
  cloudfs config auth db --stdin --no-check
```

access token 过期返回 401 时，驱动只刷新一次；并发请求共享同一次刷新结果。Dropbox refresh
token 不按每次 exchange 轮换，短期 access token 只保存在内存，不把它写回配置覆盖 offline
grant。当前 CLI 还没有 Dropbox 浏览器 OAuth 向导，用户须从自己的 OAuth 流程或 App Console
取得 token。生产凭据不要交给命令行参数或明文 YAML。

`api_base`、`content_base` 和 `oauth_url` 仅供兼容网关/测试使用；前两项必须是无 path、userinfo、
query、fragment 的 HTTP(S) origin，OAuth URL 不允许 userinfo、query 或 fragment。

## 文件与目录语义

- Provider ID 使用 Dropbox 返回的规范 `path_lower`，根为 `/`。路径身份让分页结果无需维护
  易失的「父目录 stable-id → 路径」旁表；服务端返回的 direct child、name/path 对应关系会校验。
- `list_folder` / `list_folder/continue` 保留原生 cursor 分页；`Caps.StreamList` 持续取页并逐项
  交付，visitor 一旦失败不再请求下一页。`Caps.Delta` 通过 account-wide recursive cursor
  报告 upsert/delete，单次成功 JSON 响应上限 16 MiB。
- 文件版本使用 `rev`。下载响应的 `Dropbox-API-Result` 必须与请求路径和 rev 一致后才把 body
  交给块缓存；这样即使 Range 请求期间对象被替换，也不会把新内容写进旧版本 cache key。
- Dropbox `content_hash` 是分块组合算法，不是普通文件 SHA-256，因此驱动不会错误声明
  `HashSHA256` 或拿它做通用秒传校验。
- `get_temporary_link` 返回约四小时有效、无需 Authorization header 的可外传 URL；元数据与 URL
  scheme/host 都会先验证。没有可下载表示的在线文档会明确返回不支持。
- `create_folder_v2`、`copy_v2`、`move_v2` 和 `delete_v2` 对应 Mkdir/Copy/Rename/Move/Delete。
  Dropbox 路径不区分大小写，官方 API 不支持只改变大小写的 move；根目录移动/复制/删除拒绝。

## 上传与故障恢复

64 MiB 及以下走 `/files/upload`；更大文件使用顺序 upload session，默认 part 为 8 MiB，单次
请求不超过官方 150 MiB 上限。session id、目标、总大小与 part size 都写进 journal 的 opaque
字段，重启后的新 Provider 实例只补未确认 part。完成前会在副本上排序并验证 part index 从零
连续、每个 token 的长度正确，不修改调用者的 token 切片。

流式 HTTP body 没有回卷函数时，共享 `httpx` 现在强制只尝试一次；这修复了失败后拿已消费
reader 重放为空 body 的数据完整性问题。Dropbox append 若请求已被接收但响应丢失，重试会返回
`incorrect_offset`；只有服务器给出的 correct offset 恰好等于本 part 末端时才确认成功。finish
失败不会仅凭「目标同大小」猜测提交成功，因为同大小旧文件不能证明内容相同。官方 upload
session 最长保留七天，超过后需要重新开始。

## 已验证与未验证

本地状态化 HTTP 回放校验 Bearer、`Dropbox-API-Arg` ASCII JSON、分页 cursor、元数据、Range、
revision 冲突、临时链接、单次/会话上传、跨实例恢复、重复 append、服务端 copy/move/delete、
错误映射、并发 token 刷新、delta/reset 和 VFS 冷读。尚未使用真实 Dropbox 个人/团队/App Folder 账号验收，
共享文件夹 namespace、团队 Select-User、长时间配额/429 行为也未验证。

首次 change poll 用 `get_latest_cursor` 建立基线，同时把持久化目录 freshness 全部作废，避免
停机期间的变化被新基线跳过后长期隐藏。后续 `list_folder/continue` 把 file/folder 映射为
upsert、deleted metadata 映射为路径 delete。服务端返回 reset 时先取得新基线；VFS 保留节点、
待写数据和文件块，只把目录状态设为 stale、保存新 cursor 并发出全局 rescan。共享 namespace
切换和超大 change backlog 仍需真实账号长期验证。
