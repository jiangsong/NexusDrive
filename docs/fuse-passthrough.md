# FUSE passthrough：已修复部分与剩余限制

## 当前状态

共享 backing 的缓存租约生命周期已经修复，但完整的读写模式切换尚未完成。
默认不启用 passthrough；普通挂载继续使用 VFS 读写和 journal 提交。
`CLOUDFS_EXPERIMENTAL_PASSTHROUGH=1` 仅用于隔离测试，不能作为正常写入工作负载的配置。
`CLOUDFS_NO_PASSTHROUGH` 非空时优先关闭。实验开关不绕过平台能力检查。

这道临时安全门不是 T-08 的完成条件，也不是用只读场景替代原本的完整功能。
后续必须解决下述模式问题，并在真实内核上验收，才能恢复默认优化。

## 已完成：内核注册持有缓存租约

go-fuse v2.11.0 按 inode 共享 backing ID，只向第一个句柄调用 `PassthroughFd`。
旧实现把缓存租约放在这个句柄中，首个句柄释放后，其他句柄仍在使用的内核 backing
却可能不再受缓存保护和计量。

现在生命周期为：

1. 生成 fd 时创建 offer；尚未注册的 offer 随其文件句柄释放。
2. 注册成功后，租约转交注册表，文件句柄关闭不释放它。
3. go-fuse 最后一次注销共享 backing ID 成功后，注册表关闭租约。
4. 注销失败保留计量，直到 FUSE 连接断开；不会仅因 ioctl 报错就认定空间已经释放。
5. `OnUnmount` 在 go-fuse 关闭连接后释放剩余注册；`Mount.Wait/Unmount` 等待这一步完成。

此行为与[内核关于 backing 引用的说明](https://docs.kernel.org/filesystems/fuse/fuse-passthrough.html)
一致：用户态关闭原 fd 不代表内核已经不再持有该文件。

适配使用公开的 `fs.Options.ServerCallbacks` 和 `fuse.NewServer`，没有修改或复制
第三方模块。需要注意版本耦合：v2.11.0 的 `rawBridge.Init` 只有覆盖回调接收者这一步，
因此本项目的 `backingFS.Init` 将真实 server 接到代理，并保留构建时注入的代理。
升级 go-fuse 必须重新核对 `Init`、注册/注销和 `Serve/OnUnmount` 的时序。
没有接入注册代理的裸适配器不会提供 backing fd。

## 未完成：同一 inode 的 IO 模式和内容版本

[Linux 6.9 的 IO 模式实现](https://raw.githubusercontent.com/torvalds/linux/v6.9/fs/fuse/iomode.c)
要求同一 inode 的并存打开方式相容，并拒绝不同 backing 对象混用。不能简单地为每个
句柄独立注册 backing，也不能把已经进入 passthrough 的 inode 随意退回普通缓存模式。
`DIRECT_IO | PASSTHROUGH` 仍可能把 mmap 指向 backing，不能据此证明 journal 写入安全。

当前仍需解决：

- 冷读句柄未关闭、文件已 hydrate 时，新打开的热读句柄如何切换模式。
- passthrough 读者仍打开时，后续写者不能继承 backing 并绕过 journal。
- 远端刷新或本地提交产生新版本时，新打开的句柄不能继承旧 backing。
- mmap、截断、原子替换及新旧句柄交叠时，缓存不可变性和提交语义如何保持。

这些需求需要统一的 inode/内容版本和 backing 管理方案，而非单独修改引用计数。
也不能同时简单打开 writeback_cache：[内核初始化代码](https://raw.githubusercontent.com/torvalds/linux/master/fs/fuse/inode.c)
明确限制它与 passthrough 的组合。T-10 必须按真实协商能力重新验收。

## 测试证据与验收边界

`backing_test.go` 使用真实 go-fuse NodeFS 分发层、复制 fd 的 ioctl 替身，覆盖多句柄、
GC、注册失败、注销失败、连接断开、未注册 offer、并发打开/释放，以及默认模式下
已有读者时写入仍进入 journal。此测试不模拟内核 IO 模式，也不证明 mmap 正确。

真实挂载测试 `TestPassthroughKernelLeaseSurvivesFirstCloseAndInvalidation` 验证首个句柄
关闭后，另一个内核读者跨缓存失效继续读取，且没有 FUSE READ 请求；关闭最后一个
句柄后计量释放。它和原有 passthrough 测试显式打开实验开关。
本轮 macOS 环境缺少 macFUSE/Fuse-T，真实挂载测试跳过，不能视为真机验收通过。
