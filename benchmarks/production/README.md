# 统一生产工作流对照

关联 [#427](https://github.com/Luo-root/pulse/issues/427)。[English](README_en.md)。这个独立模块固定使用 **Pulse（可靠维度加 Durable）、Eino、Temporal** 三列，根生产模块不新增运行依赖。旧 DAG 对照保留历史结果，不参与本轮排名。

## 共同任务与适配

- chain / wide / diamonds；实际任务数为 1、10、100、1000、10000；每个任务执行相同 uint64 运算，rounds=1 或 256。输入是 `1 + 直接前驱输出之和`，整图结果是终端输出之和。Go 固定值和 Python 大整数 oracle 独立复核。
- 共同 spec/oracle 准备在 adapter setup 计时外；一节点就是一项真实业务，不添加空源、汇总任务，也不把整图塞进单个 Activity。
- Pulse 使用可复用 Graph，每轮独立 Execution。Eino 显式 AllPredecessor、唯一 output key，仅合并直接前驱。Temporal 使用真实 Server/worker，一节点一个常规 Activity，先等前驱再取得 workflow semaphore，MaximumAttempts=1。
- 各 Go 进程（含独立 Server）的 GOMAXPROCS=4，图的业务并发上限=4；这不是所有进程合计四个 CPU 的亲和性限制。Eino 的宿主 semaphore 只限制业务体，并不限制引擎调度 goroutine；宿主在错误返回时取消并等待自身业务体，不承诺 Eino 所有内部 goroutine 已退出。
- plain 保留各框架原生行为；observe 添加公开 Observer/callback/interceptor 的原子计数，不写外部日志。事件种类和数量不同，开销不能当作相同功能的单价。

## 保证与计量范围

| 维度 | Pulse / Durable | Eino | Temporal |
| --- | --- | --- | --- |
| 普通执行 | Pulse 内存执行 | compose Graph 内存执行 | 始终包含原生持久化和服务调用 |
| 可靠记录 | Durable 经 Store 扩展，每完整节点链确认后发布 | 普通成功不逐节点保存；主动 interrupt 时保存 checkpoint | 原生事件历史、Activity 完成确认 |
| 重启场景 | 确认前缀后强杀宿主，再打开 SQLite 恢复 | 主动 checkpoint 后进程退出，新进程读取 blob 恢复 | 无在途 Activity 的确认前缀后强杀 worker；另测 Server 重开 |
| SQLite | 评测专用 Store，不是正式生产后端 | 评测专用 CheckPointStore，原样保存框架 blob | CLI 嵌入的真实 Server SQLite |

SQLite 均配置 WAL / FULL；各自事务布局、写入次数与保存粒度保留。Durable Store 用完整 JSON CAS 保存 RunRecord，运行独占仅借官方 filestore 的系统锁；不提供分布式租约或 fencing。Eino 主动中断不是任意 crash 自动恢复。恢复验收选择已确认且没有在途外部副作用的边界，不能推导任意副作用恰好一次。

本地 CLI `start-dev` 是真实 Temporal Service，适合本轮单机复跑；这不是对生产集群、网络延迟、HA 或生产数据库的测试。普通规模矩阵显式把 `limit.historyCount.error` 提高到 200000、warn 提高到 100000，不能称默认配置成绩。

`server_setup_ns` 单列服务准备；`adapter_setup_ns` 包含建图/Compile，或 Dial/注册/worker.Start，职责不同。`first_ns` 是第一次完整执行尝试（失败时不是完成成绩）；`run_ns` 是随后复用批次的每轮均值。嵌入式引擎批量降低小任务计时噪声，Temporal 默认一轮；每批长度原样保留。中位数是重复批次均值的中位数，不是线上 p50，min–max 不是置信区间。

Go 分配仅覆盖复用批次中的执行进程；Temporal 不含独立 Server 分配，不能据此作全引擎内存排名。RSS/CPU 用另一组 `--iterations 1` 的相同工作量案例：每案例第一次执行加一次复用，包含共同宿主、准备、GC 和实际服务。Windows Job CPU 包含已退出子进程；RSS 是每 25ms 采样的同时进程树 RSS 峰值。共用二进制链接三家依赖，RSS 含共同宿主底座，不代表各框架最小部署占用。文件长度快照含主 DB/WAL/SHM；它是空间占用，不是累计写入字节、活对象大小或每任务成本。

Windows 计时使用 QPC，避免本机 Go time.Now 约 0.5ms 粒度把小图首轮写成零。整案例 context 60s，包含 setup/首次执行；父 watchdog 65s。超时保留首轮完成标志、计划及已完成的复用轮数，未完成整批的均值仍为空；触限后其余重复保留 not_repeated，空值不填零。Python 恢复控制器与 Job CPU 采样在本版面向 Windows；Windows Job 从挂起进程开始包含所有后代。正式采时串行轮换，期间不并跑 build/test/profile/绘图或站点构建。

## 复跑

Go 1.26.0；Python 3.13、psutil 7.2.2。依赖固定公开版本，无 replace。Temporal CLI 1.9.1（Server 1.32.0）单独从官方 release 下载到工作区，先与官方 checksums.txt 核对 SHA256，不需要全局安装。

```powershell
$env:GOWORK = 'off'
$env:GOPROXY = 'https://proxy.golang.org,direct'
go mod download
go test -mod=readonly -race -count=1 -timeout=120s ./...
go build -mod=readonly -o ../results/production-bench.exe .
# 单独起真实 Server 后，启用不能以替身替代的测试：
$env:PULSE_TEMPORAL_ADDRESS = '127.0.0.1:7233'
go test -mod=readonly -race -count=1 -timeout=120s -run '^TestTemporal' -v .
# 设置实际 CLI 路径；输出只能选 ignored results 或仓库外新目录。
python ../tools/run_production.py --binary ../results/production-bench.exe --temporal-bin <CLI路径> --out ../results/production-native
python ../tools/run_production.py --binary ../results/production-bench.exe --temporal-bin <CLI路径> --out ../results/production-resources --iterations 1 --nodes 1,100,1000 --rounds 1 --modes plain --repeats 3
python ../tools/run_production.py --binary ../results/production-bench.exe --temporal-bin <CLI路径> --out ../results/production-checkpoint --engines pulse --modes checkpoint --rounds 1 --repeats 3
python ../tools/run_recovery.py --binary ../results/production-bench.exe --temporal-bin <CLI路径> --out ../results/production-recovery
```

新运行拒绝覆盖旧结果；普通矩阵 `--resume` 必须匹配原身份、二进制与采样器哈希。原始 JSONL、stdout/stderr、DB、诊断和本机清单不提交 Git。入仓仅复跑代码/锁、精简汇总、版本摘要和页面实际引用图。
