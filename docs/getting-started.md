# 从零开始：四个网盘，一个目录

把几个网盘账号合成一个本地文件夹：文件放在哪个网盘由系统决定，每份文件保存多份副本，
任何一个网盘掉线都不影响其它文件。这一页是从空配置走到挂载好的完整流程。

配置文件长什么样、每个设置什么含义，见 [存储池](pool.md)。

## 你需要什么

- 两个以上的网盘账号。可以混用厂商，比如两个 Google Drive 加两个 Dropbox。
- 每家网盘的一个 OAuth 应用（下一节），用来授权。四个同厂商的账号可以共用同一个应用。
- macOS 装 macFUSE，Linux 需要 `/dev/fuse`，Windows 装 WinFsp。

## 先建一个 OAuth 应用

网盘不允许一个程序直接拿你的账号密码，要走浏览器授权。授权前需要一个"应用"身份：

- **Google Drive**：Google Cloud Console 建一个项目，创建 OAuth 客户端，类型选**桌面应用**，
  拿到 `client_id` 和 `client_secret`。回调地址填 `http://127.0.0.1:53682/callback`。
- **Dropbox**：App Console 建一个应用，access type 选 **Full Dropbox**（选 App folder 的话
  所有文件会被关在 `/Apps/` 下面），拿到 App key。回调地址同样填
  `http://127.0.0.1:53682/callback`。Dropbox 走 PKCE，**不需要 client secret**。

同一个应用可以给同厂商的多个账号用，四个账号不用建四个应用。

## 最省事的走法：`cloudfs setup`

```sh
cloudfs setup
```

它做三件事：写一份最小配置（如果还没有）、在本机 `127.0.0.1:9101` 上启动控制台、打开浏览器。
控制台**只监听本机回环地址**，不会暴露到网络上。加 `--no-open` 只打印地址不开浏览器。

浏览器里是五步：

1. **选网盘**——每行一个，选类型、起个本地名字。至少两个才有冗余。
2. **逐个连接**——点「开始授权」，浏览器跳到网盘的授权页，回来就显示「已连接 · 剩余 87 GB」。
   **一次只能授权一个**，界面会把其余几行的按钮禁用；这不是保守，是授权回调共用本机
   53682 端口，两个一起来会撞。
3. **每个文件保存几份**——默认 2 份。这一步会告诉你可用空间大约是总空间的几分之一，
   以及坏掉几个网盘不丢文件。不报告剩余空间的网盘会单独列出来，让你填个大概容量。
4. **放在哪个文件夹**——默认 `~/CloudFS`。
5. **重启并完成**——所有配置改动都在守护进程下次启动时生效。点一下，页面会自己等它回来。

中途关掉浏览器也没关系：进度是从配置文件推出来的，重跑 `cloudfs setup` 会回到你停下的
那个网盘。在终端里 `cloudfs config add` 加的账号，向导也认。

下面是同样的事情用命令行做一遍。

## 加账号，一个一个授权

先加第一个，同时把池建起来：

```sh
cloudfs config add gd1 --type gdrive --client-id <你的 CLIENT_ID>
cloudfs config auth gd1        # 隐藏输入 client_secret，然后自动打开浏览器
```

`config auth` 会打印一个授权链接并打开浏览器。授权完成后它会回显
`credentials saved for "gd1"`，并做一次根目录列表验证。

**一次只能授权一个账号。** 授权回调共用本机的 53682 端口，两个 `config auth` 同时跑会撞在一起
（另一个会报 `address already in use`）。挨个来。

第一个账号有了，建池并指定挂载点：

```sh
cloudfs pool create home --members gd1 --replicas 2 --mount ~/CloudFS --prefix /
```

`--replicas 2` 表示每份文件保存两份。可用空间约等于总空间除以副本数，换来的是坏掉任意一个
网盘都不丢文件。

剩下三个账号加进来，`--pool` 让加账号和入池一步完成：

```sh
cloudfs config add gd2 --type gdrive --client-id <同一个 CLIENT_ID> --pool home
cloudfs config auth gd2

cloudfs config add db1 --type dropbox --client-id <APP_KEY> --pool home
cloudfs config auth db1        # Dropbox 也是浏览器授权，不问 client secret

cloudfs config add db2 --type dropbox --client-id <同一个 APP_KEY> --pool home
cloudfs config auth db2
```

挂上：

```sh
cloudfs mount                  # 前台运行，Ctrl-C 卸载
cloudfs service install        # 或者装成开机自启的服务
```

## 完成之后

```sh
ls ~/CloudFS
cloudfs pool status
```

**第一次写文件时会看到的事**：保存成功（`close()` 返回）只表示数据已经落到本机的日志里，
这一刻**还没有任何一个网盘收到它**。接着后台的上传队列把它送到一个成员，其余副本再由修复
worker 在随后的几秒到几分钟里补齐。所以刚写完的文件会有一小段时间在网盘官方 App 里看不到，
之后又有一段时间只有一份。这是这一页配出来的默认模式（`writeback`）的正常行为，不是故障。
补齐进度看 `cloudfs pool status` 的欠副本数，或者控制台（`cloudfs mount` 启动时会打印地址）
的存储池那一屏。想让 `close()` 一直等到有一个网盘收下文件再返回，把挂载的 `mode` 从
`writeback` 改成 `strict`，见 [存储池](pool.md)。

`df ~/CloudFS` 看到的是池容量，也就是各成员容量之和除以副本数。

## 出问题了

**浏览器没自动打开。** `config auth` 打印的链接手工粘到浏览器里就行，或者加 `--no-browser`
只打印不打开。

**授权卡住或者报 `address already in use`。** 有另一个授权还开着。先把那个完成或取消
（关掉浏览器里那个标签不够，要让 `config auth` 退出），再开下一个。

**某个网盘不显示剩余空间。** 不能报告配额的成员在放置时排在能报告的后面，会拿到更少的文件。
给它配一个大概的总容量：

```sh
cloudfs pool add home <网盘名> --capacity 2TiB
```

已经在池里的成员，改配置文件里那个成员的 `capacity:` 字段。

**改完配置没生效。** 所有配置改动都在守护进程下次启动时生效。重启 `cloudfs mount`，
或者在控制台上点重启。

**想再加第五个网盘。** 和上面一样：`cloudfs config add <名字> --type <类型>
--client-id <ID> --pool home`，然后 `cloudfs config auth <名字>`，重启守护进程。

## 相关文档

- [存储池](pool.md)：配置项、放置规则、修复与核对、多机使用。
- [驱动现状](providers.md)：每家网盘支持到什么程度、命名规则、配额能不能读。
- [Dropbox](dropbox.md)、[Google Drive 之类](providers.md)：各自的注意事项。
