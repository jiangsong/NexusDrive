# OneDrive 驱动

CloudFS 的 `onedrive` provider 直接调用 Microsoft Graph v1.0，不引入完整 rclone 依赖树。
Graph 元数据请求经过共享 `httpx`，因此继承 remote 级代理、分类限流、AIMD 与熔断；Graph
返回的预认证下载和上传 URL 则不携带 OAuth `Authorization` header。

## 配置与凭据

已有短期 access token 时可直接导入：

```yaml
remotes:
  od:
    type: onedrive
```

```sh
printf '%s' "$ONEDRIVE_ACCESS_TOKEN" | \
  cloudfs config auth od --stdin --field access_token --no-check
```

长期运行应配置 Azure 应用的 offline refresh token：

```yaml
remotes:
  od:
    type: onedrive
    tenant: common                 # 也可用 organizations、consumers 或 tenant ID
    client_id: APPLICATION_ID
    refresh_token: secretfile:od/refresh_token
    client_secret: secretfile:od/client_secret  # 公共/PKCE 客户端可不填
    # scope: "Files.ReadWrite offline_access"
    # drive_id: "..."              # 不填时使用 /me/drive
    # part_size: 10MiB              # 必须是 320 KiB 的整数倍且小于 60 MiB
```

```sh
printf '%s\n' 'refresh_token: "..."' 'client_secret: "..."' | \
  cloudfs config auth od --stdin --no-check
```

刷新响应若轮换 refresh token，会先通过通用 secret persistence 持久化，再使用新 access
token；并发刷新合并。当前 CLI 尚无 Microsoft 浏览器 OAuth/PKCE 向导，token 需从已有 OAuth
流程取得。`graph_base`、`token_url` 只用于兼容网关和测试；生产默认分别为 Microsoft Graph
v1.0 与 v2 token endpoint。

## 身份、列举与读取

- `DriveItem.id` 是稳定的 provider ID，CloudFS 的根 ID 固定为 `root`，实际 Graph root ID
  延迟解析。服务端返回的 ID、名称、父目录、时间、大小和类型在进入 VFS 前校验。
- children 使用 Graph 原生 `@odata.nextLink` 分页和流式 visitor；continuation URL 必须与配置的
  Graph origin 和 API 基路径相同，最多 10,000 页并拒绝重复游标。
- 文件版本优先使用 `cTag`，退回 `eTag`；Graph 提供合法 SHA-1 时写入 `Entry.Hashes`。
- Range 下载先核对版本，再访问 `@microsoft.graph.downloadUrl`。预认证 URL 绝不附带 Bearer；
  403/410 后只刷新一次元数据并再次核对版本。链接按 item/version 在内存缓存 10 分钟，避免
  VFS 每个块都请求一次 Graph；不写进元数据库、journal 或日志。对外能力保守声明 15 分钟。

## 上传与文件操作

不超过 64 MiB 的文件走单次 `PUT ...:/content`。更大文件创建 upload session；session URL、
父 ID、名称、总大小和 part size 都保存在 journal opaque 字段，因此新进程可续传。fragment
按 Graph 要求顺序发送，非末片大小必须是 320 KiB 的整数倍，每个请求严格小于 60 MiB，文件
最大 250 GB。每次响应必须确认下一偏移，末片返回的 item ID 放入不可伪造为完成状态的 part
token；`CompleteUpload` 验证连续唯一的 token 后重新 Stat 已提交对象。

Mkdir、Rename、Move、Delete 使用 Graph 原生操作，根目录的危险操作拒绝。Graph 没有通用的
同步同盘 Copy 原语，因此 `Caps.ServerCopy=false`，上层使用下载加上传的持久复制流程。

## changes 与 reset

首次 poll 用 `root/delta?token=latest` 建立最新基线，并通过通用 `CursorResetError` 让 VFS 先
使所有持久目录 listing 失效，避免停机变化被新基线永久跳过。后续 delta 用 stable item ID
映射 upsert/delete；Graph 返回 410 或 `resyncRequired` 时取得新基线，保留节点、待写数据和
文件块，只把目录 freshness 设为 stale 并触发全局 rescan。delta/next link 同样做同源约束。

## 已验证与未验证

状态化 Graph/CDN 回放覆盖 Bearer 隔离、私有 query 不进入错误文本、分页及 visitor 停止、
Stat/SHA-1、Range/版本冲突/链接失效刷新与缓存、Unicode 单次上传、跨 Provider upload session
恢复、移动改名删除、OAuth 轮换持久化、delta baseline/upsert/delete/reset，以及 Factory 到
VFS 的冷读与增量刷新。尚未用真实 Microsoft 个人、工作/学校、SharePoint drive 验收；共享
项目、权限差异、国家云、长时间 throttling、OAuth 浏览器流程和 250 GB 边界也尚未验证。
