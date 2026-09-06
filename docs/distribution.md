# 分发、容器与发布


## Windows

从 Linux runner 交叉编译（`GOOS=windows GOARCH=amd64`，`CGO_ENABLED=0`），产出
`cloudfs_<ver>_windows_amd64.exe`：一个**不含 FUSE 挂载**的静态二进制。它能做 config、
doctor、mcp、webdav 输出、账号管理——除挂载外的一切；`doctor` 如实报告本平台不支持挂载，
`cloudfs mount` 返回清晰错误而不是崩溃。这与 MCP-only 容器同一种"能力缩减、明说"的取法。

内核挂载需要第二个产物 **WinFsp 构建**（`-tags winfsp`）：
`cloudfs_<ver>_windows_amd64_mount.exe`。它把 `internal/winfs`（cgofuse 适配层）链接进来，
其余能力与静态版一致；用户只在想要内核挂载、且已安装 [WinFsp](https://winfsp.dev) 驱动时才装它。
cgofuse v1.6.0 的 no-cgo Windows 后端在运行时动态加载 `winfsp-x64.dll`，因此这个产物同样
`CGO_ENABLED=0`、同样从 Linux 交叉编译，不需要 cgo 工具链——`release.sh` 一次产出两个 `.exe`。
`doctor` 会探测 WinFsp 是否安装并如实报告；未安装时 `cloudfs mount` 给出"请安装 WinFsp"的清晰错误。

**运行时行为无法在非 Windows 机器上验证**。适配层里每条运行时假设都标了 `UNVERIFIED:`，
有 Windows 真机时按下面的验收清单逐项跑通再删除标注：

1. `-tags winfsp` 的 `.exe` 能链接并启动，`doctor` 在装/未装 WinFsp 两态下都报告正确。
2. 用 `type: fake` remote 挂到一个空目录或空闲盘符，`cmd` 与 PowerShell 下 `dir` / `type` /
   `copy` / `del` / `ren` / `mkdir` 行为正确。
3. 写入后立刻读回（验证 Flush 在 WinFsp 的 CLEANUP/CLOSE 派发下确实提交——这是最需要真机确认的
   假设，Linux "FLUSH 每个 fd 触发多次" 的前提在 WinFsp 上未必成立）。
4. 大小写敏感：在远端建 `Report.txt` 与 `report.txt`，确认两者都可见、互不覆盖。
5. 非法名/保留名：远端存在含 `<>:"|?*`、尾部点/空格、或 `CON`/`NUL`/`COM1` 的文件，确认列目录时
   被跳过而非报错崩溃（当前策略），并据此决定是否改为可逆转义。
6. 并发写 + 上传中改名的手工 chaos；Defender 实时保护开启下的大文件读写。
7. 能编就跑 `test/conformance`，有意差异写成显式例外；再跑 WinFsp 上游的 `winfsp-tests`。
8. 桌面壳在干净 Win10 21H2 / Win11 上经 WebView2 加载。

（**fsx-via-WSL 不算验证**——那是另一套文件系统栈。）

## 本地构建

`make build VERSION=0.1.0` 生成无 CGO 的当前平台二进制。`make check` 运行格式、vet
和普通测试，`make race` 单独运行耗时更长的竞态测试。发布版本通过 linker flag 写入，
`cloudfs version` 会显示该版本；普通源码构建保留 `0.1.0` 基线。

`./scripts/release.sh v0.1.0` 在一个不存在的 `dist/` 目录生成以下静态二进制和
`checksums.txt`：

- linux/amd64
- linux/arm64
- darwin/amd64
- darwin/arm64

脚本拒绝复用已有输出目录，避免旧文件混入 checksum。CI 的 tag 发布使用同一个脚本，
不是另一套未经本地验证的构建逻辑。

## MCP-only 容器

MCP 直接使用 VFS、元数据、缓存和可靠上传日志，不要求把 FUSE 暴露给容器：

```sh
mkdir -p cloudfs-config cloudfs-cache
cp deploy/config.yaml cloudfs-config/config.yaml
# 编辑 config.yaml，加入实际 remotes 和 mounts/layout。
export CLOUDFS_CONFIG_DIR=./cloudfs-config
export CLOUDFS_CACHE_DIR=./cloudfs-cache
export CLOUDFS_UID="$(id -u)" CLOUDFS_GID="$(id -g)"
export CLOUDFS_MCP_TOKEN="replace-with-a-long-random-token"
docker compose --profile mcp up --build
```

默认示例的 layout 为空，只用于传输/启动烟测；要读文件必须配置 remote 和 layout。
配置和缓存目录必须可由容器进程写入，因为 file secret backend、provider token 轮换、
元数据与 journal 都会原子落盘。Compose 默认 uid/gid 为 1000；NAS 或多用户主机应像
示例一样显式传入实际宿主 uid/gid，避免产生 root-owned 凭据。

等价的直接运行方式为：

```sh
docker build --build-arg VERSION=dev -t cloudfs:local .
docker run --rm \
  --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:8765:8765 \
  -e CLOUDFS_MCP_TOKEN="$CLOUDFS_MCP_TOKEN" \
  -v "$PWD/cloudfs-config:/config" \
  -v "$PWD/cloudfs-cache:/var/lib/cloudfs" \
  cloudfs:local mcp --http 0.0.0.0:8765
```

镜像自身默认以 uid/gid 65532 运行；bind mount 部署应像上面一样使用宿主 uid/gid。非回环 MCP listener 没有
`CLOUDFS_MCP_TOKEN` 时会拒绝启动；不要把 token 放在 URL、命令行或受版本控制的 YAML。
control API 含本地管理操作，现有实现只允许回环监听，因此 Compose 不把 control 端口
伪装成安全的外部接口。需要诊断可运行：

```sh
docker compose --profile mcp exec cloudfs-mcp cloudfs doctor --json
docker compose --profile mcp exec cloudfs-mcp cloudfs status --json
```

## Linux FUSE 容器

FUSE profile 仅适用于原生 Linux Docker host。先把配置中的 mount path 设为
`/mnt/cloud`，并准备宿主机输出目录：

```sh
mkdir -p mnt
export CLOUDFS_CONFIG_DIR=./cloudfs-config
export CLOUDFS_CACHE_DIR=./cloudfs-cache
export CLOUDFS_UID="$(id -u)" CLOUDFS_GID="$(id -g)"
export CLOUDFS_MOUNT_DIR=./mnt
docker compose --profile fuse up --build cloudfs-mount
```

该 profile 明示授予 `/dev/fuse`、`SYS_ADMIN` 和 `apparmor:unconfined`，并要求 bind mount
使用 `rshared` propagation，才能让容器内 FUSE mount 对宿主机可见。这些都是有安全影响
的宿主能力；不要改成更宽的 `privileged`。宿主 bind parent 也必须允许 shared
propagation，具体设置取决于发行版。Docker Desktop 的 macOS VM 不是此路径的验收平台，
macOS 应使用原生二进制和 `cloudfs service install`。

不要同时启动 `mcp` 与 `fuse` profile 并让它们共享同一个 cache/journal；单写者锁会拒绝
第二个 owner。需要 MCP 与挂载并存时，运行一个 `cloudfs mount` 进程并按配置启用 MCP。

镜像包含 `fuse3`，所以 `cloudfs doctor --json` 会区分“用户态工具已安装但没有
`/dev/fuse`”与可挂载环境，而不是因找不到 fusermount 直接崩溃。

Compose 同时预留 WebDAV 的 8080 端口，默认只发布到宿主回环。配置
`webdav.http: 0.0.0.0:8080` 后必须设置 `CLOUDFS_WEBDAV_TOKEN`；跨主机发布还应使用
TLS/VPN，详见 [只读 WebDAV 输出](webdav-output.md)。

## CI 与 tag release

`.github/workflows/ci.yml` 在 Linux 和 macOS 上执行 `go vet`、全库 race 测试及静态构建，
并在 Linux runner 实际构建镜像和运行 `version`/`doctor`。`.github/workflows/release.yml`
只接受 `vMAJOR.MINOR.PATCH` tag；它重新执行 format/vet/race 门禁，生成四个平台资产与
SHA-256 checksum，先创建 draft GitHub Release，再推送 linux/amd64、linux/arm64 的
GHCR multi-arch 镜像，最后发布 release。这样镜像失败不会留下看似完整的公开 release。

Homebrew tap、NAS 套件等包管理入口要等首个公开仓库、许可证选择和 tag release 成功后
再接；当前不能在没有项目所有者许可决策时伪造可再分发承诺。
