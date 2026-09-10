# CloudFS 文档

## 上手

- [从零开始](getting-started.md) — 四个网盘合成一个目录：`cloudfs setup`、五步向导，以及同样事情的命令行版。
- [存储池](pool.md) — 配置项、放置规则、副本与修复、drain 与核对、多机使用。

## 使用

- [复制](copy.md) — 服务端复制：网盘内部直接复制，字节不经过本机。
- [缓存管理](cache-management.md) — 本地热缓存的预算、固定与解除。
- [WebDAV 输出](webdav-output.md) — 把一棵子树作为 WebDAV 暴露给播放器或不能装 FUSE 的客户端。
- [STRM 生成](strm.md) — 给 Emby / Jellyfin 生成 `.strm`。
- [MCP](mcp.md) — 把同一份文件系统暴露给 agent 的工具集与客户端注册。
- [分发与部署](distribution.md) — 打包、容器、开机自启。

## 后端

- [驱动现状](providers.md) — 每家网盘支持到什么程度、命名规则、配额能不能读。
- [Dropbox](dropbox.md) · [OneDrive](onedrive.md) · [S3](s3.md) — 各自的注意事项。

## 内部设计

- [设计](DESIGN.md) — 完整设计文档；代码里的包注释大量引用它的小节号。
- [实施记录](IMPLEMENTATION.md) — 逐条实现进展。
- [界面计划](ui-plan.md) — 控制台的界面结构。
- 子系统笔记：[复制准备](copy-preparation.md)、[目录刷新](directory-refresh.md)、
  [FUSE passthrough](fuse-passthrough.md)、[SFTP 目录流](sftp-directory-stream.md)、
  [上传清理](upload-cleanup.md)、[VFS 变更](vfs-changes.md)、[基准](bench.md)。
