# 单一状态目录与开箱即用的引导流程

日期：2026-09-18
状态：已评审，待实现

## 背景

产品形态是"启动客户端即开浏览器配置页"，用户不应该需要手写 `config.yaml`。仓库里
这件事已经完成了一半：`cloudfs setup` 会写最小配置、只起控制面、开浏览器，配完
re-exec 成 `cloudfs mount`；内置 OAuth 应用的解析链（账号自带 `client_id` 优先于
内置）也已就位并有测试覆盖。

缺的是三件事：

1. 状态散落在三处（`~/.config/cloudfs/`、`~/.cache/cloudfs/`、系统 keyring），其中
   `journal/` 放在 `~/.cache` 下是一个会丢数据的隐患。
2. 裸 `cloudfs` 只打印用法；桌面壳在全新机器上拉起的是 `cloudfs mount`，而 `mount`
   在没有 mount 项时拒绝启动，于是双击桌面应用只会得到一个"没有守护进程"的占位页。
3. `builtinOAuthApps` 是空表，gdrive 账号必须用户自己准备 OAuth 应用，而产品里没有
   任何地方告诉他怎么准备。

仓库当前无 tag、无 VERSION、无 CHANGELOG，即无外部用户，因此不需要长期兼容层。

## 决策摘要

| 议题 | 决定 | 放弃的选项 |
| --- | --- | --- |
| 配置载体 | 保留 YAML，人不必手写，控制面读写 | 配置进 SQLite；YAML 降为生成物 |
| 目录布局 | 全部进 `~/.cloudfs`，块缓存单列一项可改 | 只搬配置；严守 XDG 分工 |
| Google 内置应用 | 不内置，做 BYO 引导页 | 去做验证 + CASA；内置 Testing 应用 |
| 启动入口 | 裸 `cloudfs` 与桌面壳都走同一状态机 | 只认其中一个 |

## 一、目录布局与路径解析

单一根目录 `~/.cloudfs`：

```
~/.cloudfs/
  config.yaml          # 控制面读写，人可读可改
  meta.db              # 目录树 / TTL / 负缓存 / delta 游标 / trigram 索引
  journal/             # 已写入未上传的数据，不可再生
  agent/               # 会话 / changes / read_heat
  pool/                # 存储池索引（pool-<name>.db）
  control.sock         # Unix socket
  credentials/         # keyring 不可用时的 0600 回退文件
  cache/               # 块缓存，默认在这里，可改到别的盘
```

路径解析优先级不新增机制：`--config` 标志 > `CLOUDFS_CONFIG` 环境变量 >
`~/.cloudfs/config.yaml`。`cache.dir` 独立于这条链，由 config.yaml 决定，默认
`~/.cloudfs/cache`。

### 状态目录与块缓存必须拆开

这是本次唯一的真实架构改动。`internal/daemon/daemon.go` 现在把 `meta.db`、
`blocks/`、`journal/`、`agent/`、`pool/` 全部拼在 `cacheDir` 之下。一旦 `cache.dir`
成为用户可改的设置项，journal 会跟着跑到外置盘上，拔盘即意味着已写入但未上传的数据
不可达。

因此引入两个概念：

- **StateDir**：跟随 `config.yaml` 所在目录，不可配置。承载 `meta.db`、`journal/`、
  `agent/`、`pool/`、`index.db`、`exports.db`、`secrets/`、`control.sock`。
- **cache.dir**：仅承载 `blocks/`。默认 `~/.cloudfs/cache`，控制面设置页可改。

### 涉及的位置

- `cmd/cloudfs/main.go` 的 `defaultConfigPath()`
- `cmd/cloudfs-desktop/main.go` 的 `defaultConfigPath()`
- `internal/config/config.go` 的 `Default()`：`Cache.Dir`、`Control.Socket`
- `internal/daemon/daemon.go` 三处 `~/.cache/cloudfs` 回退
- `internal/daemon/upload_discard.go` 的同一回退

顺带的好处：socket 路径变短，macOS `sun_path` 104 字节限制更宽裕。

### 拆分带来的一个新缺口

拆分之后 journal 与 blocks 可以落在不同文件系统上，而 `cache.min_free` 的写入准入检查和
doctor 的空间报告都只量块缓存目录所在的盘。journal 所在盘写满时没有前置门禁。拆分前两者
永远同盘，所以这是新出现的，记在 TODO 的遗留项里，不在本次范围内修。

## 二、入口与启动流程

两个入口，同一套状态机，同一个控制面。

状态判定沿用 `cmd/cloudfs/setup.go` 已有的 `stageFor()`：

- `write-starter`：没有配置文件
- `serve`：有配置，但没有任何可挂载的 mount
- `already-usable`：某个 mount 的 layout 指向真实存在的 remote

### 裸 `cloudfs`

无参数启动时走状态机，而不是打印用法：

- `write-starter` → 写 starter、起控制面、开浏览器
- `serve` → 起控制面、开浏览器
- `already-usable` → 挂载并开浏览器（新增；现在这个分支什么都不做）

`cloudfs setup` 保留为显式别名。

### `cloudfs-desktop`

`cmd/cloudfs-desktop/link.go` 现在固定以 `args := []string{"mount"}` 拉起守护进程，
在全新机器上必然失败。改为拉起无参数入口，让守护进程自己走状态机。
`cmd/cloudfs-desktop/main.go` 中 `config.Load` 的错误也不再等同于启动失败——缺配置是
正常起点。

### 参数解析

`parseFlags` 把无法识别的 `--name` 当成"取下一个 token 作为值"，因此前门不能把原样的参数
转发给它选中的处理函数：`cloudfs --force /mnt/drives` 转发给挂载路径时，`--force` 会把挂载点
吃掉当成自己的值，挂载落到配置里的默认位置而没有任何报错。前门为每个处理函数**重建**命令行，
只传它自己声明的标志。

同理，`cloudfs --no-open` 这类"只有标志、没有子命令"的调用必须进入前门而不是 switch，
否则会得到完整用法加 `unknown command "--no-open"`——这正是本设计第一次手工冒烟时的结果。
`--help` / `-h` / `help` 仍然交给 switch，它打印用法是有意的。

### 浏览器只由交互入口打开

`cloudfs mount` 保持沉默：它是 launchd / systemd 使用的路径，服务里弹浏览器是缺陷。
桌面壳也不开外部浏览器，它有自己的 webview。

配置完成后的接力沿用现有机制：`errRestart` + `reexecSelf()` 保持 PID 与控制地址不变，
用户正在看的页面重连到同一个 origin。桌面壳的单实例锁不受影响。

## 三、OAuth 内置应用与 BYO 引导

按"该网盘是否需要受限范围"分类，而不是一刀切。

### 能内置的

Dropbox 使用 PKCE，没有客户端密钥可泄露。Box 的 `root_readwrite` 不触发 Google 那套
年度评估。这些填进 `builtinOAuthApps` 后，用户首次添加账号零额外步骤。

### 不内置的

把整个 Drive 挂成目录需要 `https://www.googleapis.com/auth/drive`，这是受限范围：
应用要通过品牌验证，并且每 12 个月做一次第三方 CASA 安全评估才能保住权限。
`drive.file` 不受限，但只覆盖"本应用创建的文件 + 用户用 Picker 显式挑选的文件"，
挂不出完整目录树。

因此 gdrive 保持空条目。`BuiltinOAuthAppFor` 对空 `ClientID` 返回 `false`，
`ResolveOAuthClient` 自然回落到账号自己的 `client_id`，这条路径已实现且有测试。

### BYO 引导页

添加 Google Drive 账号时，如果没有内置条目，控制面展开分步引导，而不是报错：

1. 建 Google Cloud 项目
2. 启用 Drive API
3. OAuth 客户端类型选 **Desktop app**（桌面类型接受任意 `127.0.0.1` 端口，用户不必
   登记回调地址）
4. 粘贴 client_id
5. 粘贴 client_secret（走 `config auth` 同一条保存路径，进 keyring，不落 YAML）

引导页必须写明**把应用发布到 Production**。停留在 Testing 状态的应用，授权 7 天后
`refresh_token` 失效，此后一律 `invalid_grant`。发布到 Production 不需要通过验证：
同意页会显示"未验证应用"警告，点"高级 → 继续"即可；未验证应用在 Production 下有
100 用户上限，而自建应用只服务用户自己，碰不到这个上限。

### 错误可读性

`internal/provider/gdrive/gdrive.go` 的刷新失败现在统一成 `provider.ErrAuth`。
新增判别：token 端点返回 `invalid_grant` 时，错误信息点名"应用可能停留在 Testing
状态，授权 7 天后失效；把应用发布到 Production 后重新授权"。

## 四、迁移与兼容

零外部用户，不做长期兼容层，只做一次性搬运。

**触发条件**：`~/.cloudfs/config.yaml` 不存在，且 `~/.config/cloudfs/config.yaml`
存在。在守护进程启动之前判断，且控制 socket 无应答（有守护进程在跑就拒绝并提示先停）。
只在默认路径上触发：`--config` 或 `CLOUDFS_CONFIG` 指定的路径是为某一次命令选的，
把用户的状态搬到那底下是意外行为。

**哪些入口触发**：三个会启动守护进程的入口——裸 `cloudfs`、`cloudfs mount`、`cloudfs setup`。
`cloudfs mount` 必须包含在内：它是 launchd / systemd 运行的命令，不带 `--config`，解析到的是
新根目录下的路径；不迁移的话这些用户只会得到"文件不存在"，而真正的配置还躺在旧位置。
只读命令（`cloudfs status` 等）不触发——搬用户的文件不是它们该做的事。

**搬什么**：`config.yaml`、`meta.db`、`index.db`、`exports.db`、`journal/`、`agent/`、
`pool/`、`secrets/`。

**不搬块缓存**：它可能有几十 GiB。同一文件系统内 `os.Rename` 是原子且瞬时的，复制
几十 GiB 不是。改为在搬过来的 config.yaml 里把 `cache.dir` 显式写成
`~/.cache/cloudfs`，`blocks/` 原地继续使用。用户想收编到 `~/.cloudfs/cache` 可在设置
页自行更改，那时是他知情的一次大拷贝。

**中断安全**：逐项搬运，目标已存在则跳过；`config.yaml` 最后搬。它的存在就是"已迁移"
的标志，因此任何中途崩溃在下次启动时从断点续做，不会出现"配置在新家、数据在旧家"的
错位。`os.Rename` 跨文件系统失败（`EXDEV`）时不静默降级为复制，直接报错并打印手工
命令。

**不搬的**：`control.sock` 重建即可。`internal/hooks` 写入的
`$XDG_CONFIG_HOME/cloudfs/mounts` 留在原处——它是已安装到其他 agent 平台中的 shell
片段读取的路径，属于对外契约，不是 cloudfs 自己的状态。

## 五、测试策略

每条先写在当前树上失败的用例，再改代码。

**状态目录与块缓存的拆分**：断言把 `cache.dir` 指到另一个目录后，`journal/`、
`meta.db`、`agent/` 仍落在 config.yaml 所在的根下。这条测试一旦变红，说明丢数据的
隐患又回来了。

**迁移**：旧目录有全套数据时，迁移后新目录齐全、`blocks/` 原地未动、`cache.dir` 指向
旧路径；搬到一半中断后再跑一次能补完；模拟 `EXDEV` 时报错而不是静默复制；控制 socket
有应答时拒绝迁移。

**入口状态机**：三个 stage 各跑一遍，两个入口各一遍。关键负向断言是 `cloudfs mount`
不开浏览器——注入 `OpenURL` 钩子并断言零调用。

**OAuth 策略测试**：遍历 `builtinOAuthApps`，断言 gdrive 没有内置条目，防止将来有人
顺手填一行把受限范围的应用连同密钥发进二进制。另断言 `invalid_grant` 的错误信息提到
Testing 状态。

**测试自身的卫生**：这批改动全是路径解析，用例必须 `t.Setenv("HOME", t.TempDir())`。
漏一个就会往开发者真实家目录写入 `~/.cloudfs`，且往往要等到别人机器上才暴露。

**跑法**：`./gow test ./...`，另加 `./gow test ./test/e2e/` 覆盖"全新家目录 → 启动 →
配置页可达"。未安装 macFUSE 的 macOS 上，`./internal/fusefs`、`./test/conformance`、
`./test/e2e` 会挂到超时而非 skip，跑全量前先排除。

## 六、应用密钥（client_secret）：浏览器里唯一的例外

### 问题

方案 A（不内置 Google 应用）决定了 gdrive 账号跑在用户自己注册的 OAuth 应用上。
Google 的桌面客户端是机密客户端（非 PKCE），`ResolveOAuthClient` 因此要求
`client_secret`。而控制面 `POST /accounts` 用 `rejectSecretFields` 拒绝一切
`config.IsSecretField` 的键，浏览器授权流程又没有 `promptSecret`（那只有终端有）。

结果是一条死路：页面能填 client_id、能建账号、点「开始授权」必然失败，而且失败原因被
`err.auth_start_failed` 吞成一句「检查账号设置与守护进程日志」。用户只能回终端执行
`cloudfs config auth`——正是本方案要消灭的那一步。box、onedrive、aliyun、baidu、pan123
同理；dropbox 因为走 PKCE 不受影响。

### 决定

开一个**只有应用密钥能通过**的口子：`POST /accounts/{name}/auth/app-secret`。

它和账号凭据的区别是实质性的，不是措辞上的：client_secret 标识的是**应用**，不是人。
用户自己在服务商控制台注册了这个应用，他是这个值的唯一来源；而应用换来的 token 仍然
只在守护进程里交换和保存，一个字节都不经过页面。

边界靠四条守住：

- **只收这一个字段**。请求体只有 `client_secret`，解码器开了
  `DisallowUnknownFields`，`refresh_token` 之类连 400 都过不去。
- **只对用得上的后端开放**。`AuthStarter.AppSecret`（daemon 的 `NeedsAppSecret`：有
  OAuth profile 且非 PKCE）为假时在碰文件之前就 400。
- **存法不变**。走 `SaveCredentialsForRemotePreservingBinding`，进钥匙串或 0600 文件，
  配置里只留 `keyring:` 引用。保留 binding 是因为存注册信息并没有换账号身份，随后的
  授权本来就会轮换它。
- **不算已授权**。新增 `config.IsOAuthAppField`，`has_credentials` 不再把
  client_secret 算成凭据；detail 另给 `app_secret` / `has_app_secret` 两个布尔量。
  否则一个注册了应用但从没登录过的账号会在列表里显示「已授权」，续做的向导会跳过它。

### 错误分级

`ErrClientSecretRequired` 成为哨兵错误，daemon 通过 `AuthStarter.AppSecretMissing`
把识别能力交给 control（control 不能 import daemon）。`authStart` 遇到它返回
**428 Precondition Required** 与 `err.app_secret_required`，其余启动失败仍旧是泛化的
502——那些错误可能带 secrets 路径或代理内部信息。

页面侧 `authFailureMode(428)` 返回 `'app_secret'`，`auth_step.js` 渲染一个
`type="password"` 输入框，存完自动重跑一次授权。这段逻辑只此一份，加网盘弹窗和连接
设置抽屉都复用它。`TestWebAppCollectsOnlyTheOAuthApplicationSecret` 把「密码框与
client_secret 字面量只允许出现在 auth_step.js」钉死，其余模块出现即失败。

### 测试

`internal/control/app_secret_test.go`：密钥不进配置文件、不回显；存了密钥不等于已授权；
非机密客户端的后端拒收；只收这一个字段；428 而不是泛化 502。
`internal/daemon/app_secret_test.go`：gdrive 要密钥、dropbox 不要、sftp 没有应用；
哨兵错误可被 `errors.Is` 识别；`AuthStarterFor` 确实把两个钩子接上。
`web/_tests/auth_policy.test.mjs`：428 是一个步骤，不是失败。
