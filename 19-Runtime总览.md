## 第19章 Runtime 总览

> 本章是 Runtime 部分的地图，内部实现快照基于 Go 1.26.4。后续[第20章 内存管理](./20-内存管理.md)、[第21章 GC](./21-GC.md) 与[第12章 Goroutine](./12-Goroutine.md) 分别展开子系统。Runtime 私有字段、常量和调度启发式不属于 Go 1 兼容性承诺，阅读源码时必须固定到具体 tag。

### Runtime 做什么

Go runtime 通常与用户代码一起链接到可执行文件中，不是需要独立安装的 JVM 式虚拟机。cgo、plugin、`-buildmode=shared` 等构建方式可引入动态链接，因此“Go 永远是完全静态单文件”也不是语言保证。

Runtime 主要负责：

- goroutine 调度、抢占与栈扩缩；
- 堆分配、垃圾回收与物理页归还；
- network poller、timer、部分系统调用和信号处理；
- channel、map、slice、interface 与 reflect 所需的底层支持；
- race/asan/msan、pprof、trace、metrics 等运行时观测能力；
- 调用用户 `init` 和 `main`，管理进程启动与结束。

编译器与 Runtime 是一套协同系统。编译器会根据代码生成或插入：

- `runtime.mallocgc` 等分配入口；
- 函数序言中的栈边界/抢占检查；
- 堆与全局指针写入的写屏障；
- stack map、GC bitmap、类型元数据和安全点；
- map、channel、interface 等操作的 Runtime/ABI 调用。

这也是为什么某些实现变更必须同时修改 `cmd/compile`、`runtime` 和 `internal/abi`。

#### Go 1.26.4 源码地图

```text
src/runtime/
|-- runtime2.go           G/M/P 与全局 Runtime 核心类型
|-- proc.go               启动、调度、sysmon、GOMAXPROCS
|-- stack.go              goroutine 栈分配、复制和缩小
|-- chan.go / select.go   channel 与 select
|-- time.go               timer 与每 P timer heap
|-- netpoll*.go           平台 network poller
|-- malloc.go             mallocgc 与 tiny/small/large 分流
|-- mcache.go             每 P 分配缓存
|-- mcentral.go           每 span class 的共享 span 集合
|-- mheap.go              mspan、mheap 与 arena 元数据
|-- mpagealloc.go         page bitmap 与 radix summary 页分配器
|-- mgc.go                GC 周期与阶段转换
|-- mgcpacer.go           heap goal、trigger、assist、memory limit
|-- mgcmark*.go           根扫描、标记与 Green Tea 路径
|-- mgcwork.go            workbuf 和 span queue
|-- mbarrier.go           混合写屏障说明与批量入口
|-- mgcsweep.go           span 清扫
`-- mgcscavenge.go        空闲物理页归还

src/internal/runtime/gc/sizeclasses.go  当前 size class 表
src/internal/runtime/maps/             Go 1.24+ Swiss Table map 主体
src/cmd/compile/internal/escape/        逃逸分析
```

### 启动流程

不同 OS/架构的汇编入口名称不同，但主线类似：

```text
OS loader
  -> 平台 rt0_* 汇编入口
  -> runtime.rt0_go
  -> osinit / schedinit
  -> 创建 runtime.main goroutine
  -> mstart / schedule
  -> runtime.main
  -> 包初始化
  -> main.main
  -> exit hooks / process exit
```

`schedinit` 的具体顺序是私有实现，但概念上需要先完成：

- m0/g0、TLS 与平台运行环境；
- 命令行、环境变量和 `GODEBUG`；
- 堆、GC controller、调度器全局状态；
- P 列表与每 P `mcache`；
- 随机源、安全、trace 等基础子系统。

`runtime.main` 在主 goroutine 上启动必要的后台 Runtime 任务，执行 Runtime 和用户包初始化，然后调用用户 `main.main`。`main.main` 返回不会等待其他 goroutine 自行结束；需要优雅停机的服务必须在 `main` 返回前完成取消、排水与资源关闭。`os.Exit` 则不会运行当前 goroutine 的 defer。

#### 包初始化顺序

可依赖的原则是：

1. 包按导入依赖顺序初始化，被导入包先完成。
2. 一个包内先初始化包级变量，再按声明顺序调用 `init` 函数。
3. 包初始化在单个 goroutine 中串行进行；`init` 启动的 goroutine 可以并发运行，但不会被初始化机制等待。

语言规范不保证“同包多文件一定按文件名”。构建系统被鼓励按词法文件名顺序向编译器提交文件，但业务正确性不应依赖这一点。跨文件有顺序需求时，用显式函数和数据依赖表达。

启动诊断可用：

```bash
GODEBUG=inittrace=1 ./service
```

`inittrace` 会报告包初始化的时间与分配。避免在 `init` 中做网络 I/O、无上限重试或依赖外部服务的工作，这些失败更适合由可返回 `error` 的显式初始化函数处理。

### Scheduler

#### G-M-P 模型

- **G**（goroutine）：栈、寄存器上下文、状态、等待原因和调度元数据。
- **M**（machine）：OS 线程及其 Runtime 状态，包含用于执行 Runtime 代码的 g0。
- **P**（processor）：执行普通 Go 代码所需的逻辑资源，持有本地 run queue、runnext、`mcache`、timer 与 GC work。

M 通常要持有 P 才能执行用户 Go 代码。M 进入可阻塞系统调用时可与 P 分离，其他 M 接手 P，避免一个 OS 阻塞让所有 goroutine 停顿。

Go 1.26.4 的概念布局：

```go
// 只表示关系，不是可依赖结构定义
type g struct {
    stack       stack
    stackguard0 uintptr
    m           *m
    sched       gobuf
    atomicstatus uint32
    waitreason  waitReason
    lockedm     muintptr
}

type m struct {
    g0       *g
    curg     *g
    p        puintptr
    oldp     puintptr
    spinning bool
    lockedg  guintptr
}

type p struct {
    id       int32
    status   uint32
    m        muintptr
    runqhead uint32
    runqtail uint32
    runq     [256]guintptr
    runnext  guintptr
    mcache   *mcache
    timers   timers
    gcw      gcWork
}
```

`GOMAXPROCS` 决定 P 的数量，即同时执行普通 Go 代码的并行上限。它不限制进程的 OS 线程总数，也不保证 goroutine 不交错执行。

#### 可运行 G 从哪里来

`schedule`/`findRunnable` 的完整顺序很长，且会随调度器演进。需要掌握的来源是：

- 当前 P 的 `runnext` 和本地环形 run queue；
- 全局 run queue，当前实现会周期性检查以避免饥饿；
- 从其他 P 偷取的一批 G 和 timer；
- network poller 返回的就绪 G；
- GC mark worker、trace reader 等 Runtime 工作；
- 由 channel、mutex、semaphore、timer 或系统调用唤醒的 G。

`runnext` 是优先槽，适合刚唤醒且与当前 G 有局部性的工作。Runtime 会限制继承时间片的连续使用，避免其他 G 永久饥饿。

#### 抢占和 sysmon

Go 同时使用：

- 函数序言与安全点上的协作式抢占；
- Go 1.14+ 在支持平台上的基于信号的异步抢占；
- sysmon 对长时间运行 G、系统调用 P、timer、netpoll 和强制 GC 等状态的监控。

早期 Go 版本中，不调用函数的紧循环可长时间不让出。现代 Go 的异步抢占显著改善了这个问题，但不能把某个私有时间阈值理解为实时调度 SLA。cgo、信号处理和 Runtime 内部不可抢占区域也需要单独理解。

### 容器感知 `GOMAXPROCS`

Go 1.25 起，当模块语言版本和 `GODEBUG` 允许新默认值且没有显式设置 `GOMAXPROCS` 时，Runtime 会综合：

- 机器逻辑 CPU 数；
- 进程 CPU affinity mask；
- Linux cgroup CPU quota/period 表示的平均 CPU 吞吐上限。

当前通常取三者最小值，cgroup 非整数 quota 向上取整。除非逻辑 CPU 或 affinity 本身小于 2，Runtime 不会因 cgroup quota 把默认值压到 2 以下。默认值可根据 affinity/cgroup 变化自动更新，最快约每秒一次。

以下操作会禁用默认自动更新：

- 把 `GOMAXPROCS` 环境变量设为正整数；
- 调用 `runtime.GOMAXPROCS(n)` 设置自定义值。

Go 1.25 新增的 `runtime.SetDefaultGOMAXPROCS()` 可按当前 CPU/affinity/cgroup 立即恢复并重算默认值。`GODEBUG=containermaxprocs=0` 与 `updatemaxprocs=0` 可禁用相应行为；对 `go 1.24` 及更早语言版本，这两个兼容开关默认为 0。

工程建议：

- 新的 Go 1.25+ 服务先使用 Runtime 默认值，不再无条件设置 `runtime.GOMAXPROCS(runtime.NumCPU())`，否则会覆盖容器感知值并禁用自动更新。
- CPU 密集服务不要套用“核数乘 1.25”之类通用比例。P 过多可增加调度、GC 并行度和 cache 竞争，必须用实际 quota 下的负载测试决定。
- 调用 `runtime.GOMAXPROCS(0)` 可读当前值，`/sched/gomaxprocs:threads` 可用于观测。

### Network Poller 与阻塞系统调用

支持 poll 的网络描述符通常设为非阻塞，goroutine 等待 I/O 时挂到 poll descriptor，不需要一个 OS 线程始终阻塞在每个连接上。epoll/kqueue/IOCP 等返回就绪事件后，Runtime 把相应 G 变为 runnable，它仍需要获得 P 才能继续执行。

不能由 poller 处理的阻塞系统调用通常让 M 进入 syscall 状态并释放 P，调度器可创建或唤醒其他 M 继续使用该 P。Go Runtime 没有一条“所有文件 I/O 都进入固定线程池”的通用规则，具体路径取决于 OS、文件类型和标准库实现。

观测时将以下信号结合：

- `go tool trace` 的 goroutine state 和 network/syscall blocking；
- goroutine profile 中的等待栈；
- `GODEBUG=schedtrace=1000,scheddetail=1`；
- `/sched/goroutines:goroutines`、`/sched/latencies:seconds` 和 Go 1.26 的分状态 goroutine 指标。

Go 1.26 还提供实验性 `goroutineleak` profile，构建时需 `GOEXPERIMENT=goroutineleakprofile`。它利用 GC 可达性找“阻塞在已无可达唤醒路径的同步对象上”的一类泄漏，但无法发现所有逻辑泄漏，也不应取代取消、超时与持续 goroutine 监控。

### GC 总览

Go 1.26 的 GC 是非分代、非移动、并发标记-清扫回收器。Green Tea GC 已默认启用，它在传统对象 workbuf 之外按 span 组织小对象标记与扫描工作，改善局部性和 CPU 扩展性。`GOEXPERIMENT=nogreenteagc` 是 Go 1.26 用于回归定位的临时 opt-out，不是当前默认状态。

一轮周期的主干：

| 阶段 | 全局 STW | 主要工作 |
|---|---|---|
| Sweep termination + mark setup | 是 | 结束上轮清扫，启用写屏障/assist，排队根工作 |
| Concurrent mark | 否 | worker 与 assist 扫描根和堆对象 |
| Mark termination | 是 | 结束标记，刷新本地状态，更新 pacer |
| Concurrent sweep | 否 | 回收 span slot，与后续分配并发 |

根扫描 job 在 concurrent mark 期间执行；扫描某个 goroutine 栈时会暂停该 goroutine，不是把所有栈都塞进起始 STW。

混合写屏障的概念模型：

```go
// 概念伪代码
shade(*slot)
if currentStackIsGrey() {
    shade(newPointer)
}
*slot = newPointer
```

`GOGC` 控制 live heap 与根集之上的增长预算，`GOMEMLIMIT` 是 Runtime 管理内存的软限制。二者都不保证固定 pause，`GOMEMLIMIT` 也不约束 cgo、应用自行 mmap 等非 Runtime 管理内存。

首选观测：

```text
/gc/heap/live:bytes
/gc/heap/goal:bytes
/gc/scan/heap:bytes
/cpu/classes/gc/mark/assist:cpu-seconds
/cpu/classes/gc/total:cpu-seconds
/sched/pauses/total/gc:seconds
```

`GODEBUG=gctrace=1` 适合快速排查，`runtime/metrics` 适合稳定观测，alloc/heap profile 用于定位分配点和持有链。详见[第21章 GC](./21-GC.md)。

### 内存分配总览

编译器先决定对象能否放在 goroutine 栈上。需要堆分配的对象进入 `mallocgc`：

```text
size == 0
  -> 特殊零尺寸地址

size < 16 B 且 noscan
  -> tiny allocator

size <= 32 KiB
  -> size class
  -> P.mcache
  -> mcentral（本地 span 用完时）
  -> mheap/pageAlloc（需要新 span 时）

size > 32 KiB
  -> 专用大对象 span
  -> mheap/pageAlloc
```

Go 1.26.4 有 68 个 size-class 索引，其中 0 是保留值，1..67 覆盖 8 B 到 32 KiB。与 scan/noscan 位组合后共 136 个 span class。大对象使用 size class 0，不是“第 67 类”。

`mheap.pages` 的当前页分配器由 page bitmap 和多级 radix summary 组成，用摘要快速定位连续空闲 page。历史文章里的 `mTreap`、`free/busy treap` 不是 Go 1.26 实现。

工程上应区分：

- **分配速率**：用 benchmark `-benchmem` 和 allocs profile。
- **存活堆**：用 in-use heap profile 查持有链。
- **扫描量**：用 `/gc/scan/*` 查指针密度和根集。
- **Runtime 总内存**：用 `/memory/classes/total:bytes - /memory/classes/heap/released:bytes`。
- **RSS/cgroup working set**：用 OS/容器指标，它还包含 cgo、mmap、代码映射等内容。

详见[第20章 内存管理](./20-内存管理.md)。

### 调试与观测清单

| 目标 | 工具 |
|---|---|
| 调度器瞬时状态 | `GODEBUG=schedtrace=1000,scheddetail=1` |
| goroutine 等待栈 | goroutine profile、`go tool pprof` |
| 时序、阻塞、syscall、GC | `go tool trace` |
| CPU 热点 | CPU profile |
| 累计分配 | allocs profile |
| 当前持有 | in-use heap profile |
| 锁竞争 | mutex/block profile（先理解采样率和开销） |
| GC 快速摘要 | `GODEBUG=gctrace=1` |
| 持续 Runtime 数据 | `runtime/metrics` |
| 数据竞争 | `go test -race ./...` |
| 逃逸决策 | `go build -gcflags='-m=2' ./...` |

高频调用 `runtime.Stack`、全量 mutex/block profile 或过长 trace 都会扰动被观测系统。生产排查应使用限时采集、适当采样率，并保存 Go 版本、环境变量和负载上下文。

### 常见误区

- **`GOMAXPROCS(1)` 不是 race detector**：goroutine 仍会交错执行，未同步访问仍违反内存模型；用 `-race` 和正确同步。
- **`runtime.LockOSThread` 不是普通性能优化**：它用于线程局部状态、GUI、特定 cgo/API 约束等场景，必须设计清晰的解锁和退出路径。
- **接口转换不必然堆分配**：是否逃逸取决于数据流、类型和编译器优化，用 `-m=2` 验证。
- **返回局部变量指针是内存安全的**：编译器会在需要时把对象放到堆上；问题是分配成本，不是悬挂指针。
- **`unsafe.Pointer`/`uintptr` 可以隐藏指针语义**：不要把 Go 指针长期存为 `uintptr`；遵循 `unsafe` 和 cgo 规则，必要时用 `runtime.KeepAlive`/`runtime.Pinner`。
- **频繁手动 `runtime.GC()` 不是内存治理**：它会强制周期，不会回收仍可达对象；先查 heap profile、分配速率与保留链。
- **Runtime 指标名不能凭记忆拼接**：用 `runtime/metrics.All()` 发现支持指标与 `ValueKind`。

### 本章小结

- Runtime 与编译器共同实现调度、栈、分配、GC、poller、timer 和多种语言内建操作。
- 启动主线从平台 `rt0_*` 到 `schedinit`、`runtime.main`、包初始化和用户 `main.main`；跨文件 `init` 顺序不应成为业务契约。
- 调度器用 G-M-P、本地/全局队列、work stealing、netpoll 与抢占组织并发执行。
- Go 1.25+ 的默认 `GOMAXPROCS` 可感知 Linux cgroup CPU quota 并自动更新；显式设置会禁用这一行为。
- Go 1.26 默认使用 Green Tea GC；根工作在并发 mark 中执行，暂停时间没有固定保证。
- 分配器主干是 tiny/small/large 分流与 `mcache -> mcentral -> mheap/pageAlloc`；当前页分配器不是历史 treap。
- 任何 Runtime 调优都应从 metrics、profile、trace 和代表性 benchmark 开始，不从固定纳秒、阈值或比例开始。
