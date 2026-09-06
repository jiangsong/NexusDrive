# 分发、容器与发布


## Windows

从 Linux runner 交叉编译（`GOOS=windows GOARCH=amd64`，`CGO_ENABLED=0`），产出
`cloudfs_<ver>_windows_amd64.exe`：一个**不含 FUSE 挂载**的静态二进制。它能做 config、
doctor、mcp、webdav 输出、账号管理——除挂载外的一切；`doctor` 如实报告本平台不支持挂载，
`cloudfs mount` 返回清晰错误而不是崩溃。这与 MCP-only 容器同一种"能力缩减、明说"的取法。

内核挂载需要单独的 **WinFsp 构建**（`-tags winfsp`，cgo，运行时依赖已安装的 WinFsp 驱动），
它是另一个产物、另一条构建线，需要 Windows 或 cgo 交叉工具链，且只能在有 Windows 真机时验收。
本机无 Windows，C2 适配代码与验收清单见 `docs/ui-plan.md` 阶段 C2。

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
