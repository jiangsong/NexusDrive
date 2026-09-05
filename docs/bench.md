# 基准与验证方法

本文记录 IO 基准的做法、门禁与复现方式。数字本身在 `docs/perf-report.html`。

## 工具

| 工具 | 位置 | 作用 |
|---|---|---|
| `cloudfs bench` | `internal/bench`、`cmd/cloudfs` | 内置基准，仿 `juicefs bench`。自带数据集，一行 JSON 一项，附后端调用差值 |
| `netem` | `internal/netem`、`cmd/netem` | TCP 代理，注入延迟/抖动/丢包停顿/重置/限带宽/断链，HTTP 控制 |
| `run-matrix.sh` | `scripts/bench/` | 在 lan / wan / bad 三档链路上跑全套负载，输出 `results/<tier>.jsonl` |
| fio | `scripts/bench/build-fio.sh`、`scripts/bench/fio/*.fio` | 行业工具交叉验证；`cross-check.py` 并排比对，差异 >25% 标红 |

mdtest 需要 MPI，本机没有；元数据用 fio 的 `filecreate / filestat / filedelete` 引擎代替。

## 负载

| 名称 | 内容 | 看什么 |
|---|---|---|
| walk | 遍历 40 目录 × 50 文件并 stat | 冷态列目录次数 = 目录数；热态 0 |
| statstorm | 逐个 stat 2000 文件 | 内核属性缓存是否生效 |
| seqread1m / seqread4k | 顺序读大文件，1 MiB / 4 KiB 缓冲 | 冷态接近链路上限；4 KiB 读的请求数 = 块数 |
| randread | 500 次随机 4 KiB 读 | 冷态每次未命中只取一个子块 |
| parread | 8 线程 × 50 次 64 KiB 随机读 | 并发下的单飞与去重 |
| seqwrite | 顺序写 64 MiB | 可见速率、`close()` 延迟、排空时间分开报 |
| randwrite | 500 次随机 4 KiB 覆盖写 | 随机写路径 |
| smallfiles | 500 × 4 KiB 创建 | 组提交、单次上传 |
| metadata | create / stat / readdir / unlink 各 N | mdtest 的四项 |
| stress | 4 写者 × 25 × 1 MiB，写完读回逐字节比对 | 正确性，任何失败即缺陷 |

冷态通过 `POST /cache/drop` 达成：清块缓存、把目录列举标记过期、让内核丢弃已缓存的
条目与页——不再靠杀进程删目录，冷态数字里不再混进启动开销。本地未上传的文件与 pin
的文件不会被清。

## 链路三档

| 档 | 单向延迟 | 抖动 | 丢包停顿 | 带宽 | 大文件 |
|---|---|---|---|---|---|
| direct | 不经代理，直连后端 | — | — | — | 256 MiB |
| lan | 0 | 0 | 0 | 不限 | 256 MiB |
| wan | 25 ms（RTT 50） | 3 ms | 1%，每次 200 ms | 20 Mbit | 64 MiB |
| bad | 100 ms（RTT 200） | 20 ms | 5%，每次 400 ms | 5 Mbit | 16 MiB |

丢包不是丢字节：一条 TCP 流丢一个段的表现是重传超时，所以这里用「停顿」模拟。
lan/wan/bad 都经过代理，唯一变量是损伤参数；direct 档把代理自身的开销从数字里剥掉。

每项结果还带 `fuse_ops`（内核发到本进程的请求数，按类型）、`fuse_read_bytes` 与
`fuse_read_sizes`（READ 尺寸直方图）。热遍历「快」是不够的，`fuse_ops` 为零才说明内核
在自己回答；冷随机读每次拉多少，看 READ 尺寸就知道内核一次要多少。

## 门禁

| 项 | 目标 |
|---|---|
| 冷态随机 4 KiB × 500（256 MiB 内） | ≥ 1,500 IOPS，拉取 ≤ 40 MB（子块 16 KiB 时 ≤ 16 MB） |
| 小文件 500 × 4 KiB 可见速率 | ≥ 400 个/s（`durability: crash`）；`power` 如实报数，目标 ≥ 250 |
| 小文件队列排空 | ≤ 6 s |
| 冷遍历 2041 项 | ≤ 400 ms，且 `lookup` 请求为 0（readdirplus 由目录句柄填充） |
| 热遍历 2041 项 | ≤ 40 ms，且除 opendir 外 FUSE 请求为 0 |
| 冷态顺序读 256 MiB | direct ≥ 250 MB/s；经代理 ≥ 170 MB/s |
| wan 档 | 冷遍历 ≤ 3 s；64 MiB 顺序读 ≥ 20 MB/s；断链 30 s 后自愈、零死信 |
| 正确性 | stress 零失败；整树 md5 与远端一致 |

远端调用次数作为硬断言写在 `test/perf`，CI 能跑；上面的时间与吞吐要真实链路。
门禁结果见 `docs/perf-report.html`（及 `TODO.md` 的汇总表），逐轮的取舍与仍然存在的保留意见写在报告的
「保留意见」一节。

## 复现

```bash
# 三档矩阵（会在 <workdir> 下建 mnt / cache / results）
scripts/bench/run-matrix.sh /tmp/cloudfs-matrix 192.168.0.20 work '~/test' lan,wan,bad

# 单独跑一项
cloudfs bench /mnt/lan --tests randread --cold --repeat 3 --metrics 127.0.0.1:9101

# crash 模式小文件（单独挂载，daemon 级配置）
#   journal: { durability: crash }
cloudfs bench /mnt/direct --tests smallfiles,metadata --repeat 3 --metrics 127.0.0.1:9102 --no-prepare

# fio 交叉验证
FIO=$(scripts/bench/build-fio.sh /tmp/fio)
for j in scripts/bench/fio/*.fio; do
  DIR=/mnt/lan $FIO --output-format=json --output=/tmp/fio-out/$(basename $j .fio).json $j
done
scripts/bench/cross-check.py /tmp/cloudfs-matrix/results/lan.jsonl /tmp/fio-out "cloudfs lan warm"
```

## 定位性能问题

守护进程带 `CLOUDFS_PPROF=1` 启动时，控制端口会多出 `/debug/pprof/`（只监听回环，默认关闭）：

```bash
CLOUDFS_PPROF=1 cloudfs mount --config …
curl -o cpu.prof 'http://127.0.0.1:9101/debug/pprof/profile?seconds=12'
go tool pprof -top -cum cloudfs cpu.prof
```

第四轮就是这样定位到「随机读期间 35% CPU 花在目录预取的元数据写入上」「sidecar 每次 flush 都写」
的——两者都不在读路径的代码里，只看代码看不出来。
