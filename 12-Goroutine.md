## 第12章 Goroutine（重点）

> 本章从工程语义进入 Go 调度器，再解释 G、M、P、运行队列、抢占、系统调用和 netpoll。公共行为以 Go 1.26 为准，Runtime 快照以 Go 1.26.4 为准。私有字段、队列长度、时间阈值和查找顺序都不是 Go 1 兼容性承诺。

### Goroutine 的语义

`go f()` 启动一个与调用者并发执行的函数。它不返回 future，不自动传递返回值、error 或 panic，也不建立父子生命周期：

```go
go f(x, y)
```

调用 `f` 所需的函数值和实参会在新 goroutine 开始前完成求值。新 goroutine 何时真正运行由调度器决定；主 goroutine 返回会结束进程，不会等待其他 goroutine。

需要记住四条边界：

1. **并发不等于并行**：多个 goroutine 可以交错执行；同一时刻能执行普通 Go 代码的数量受 `GOMAXPROCS` 限制。
2. **goroutine 没有 join 方法**：用 `sync.WaitGroup`、channel 或更高层任务组等待。
3. **panic 不跨 goroutine 恢复**：未恢复的 panic 会终止进程；`recover` 只能在同一 goroutine 的 deferred 函数中生效。
4. **退出责任必须明确**：启动方应知道 goroutine 何时停止、由谁取消、谁等待，以及结果如何回收。

#### 结构化生命周期

Go 1.25 的 `WaitGroup.Go` 可以表达只需要等待完成的任务；传入函数必须不 panic：

```go
var wg sync.WaitGroup

for _, item := range items {
    item := item
    wg.Go(func() {
        process(item)
    })
}
wg.Wait()
```

需要取消和错误传播时，应把它们显式加入协议。下面的固定 worker 数避免了为每个输入无限制创建 goroutine：

```go
func processAll(ctx context.Context, jobs []Job) error {
    ctx, cancel := context.WithCancel(ctx)
    defer cancel()

    jobsCh := make(chan Job)
    errCh := make(chan error, 1)

    workers := min(8, len(jobs))
    var wg sync.WaitGroup
    for range workers {
        wg.Go(func() {
            for {
                select {
                case <-ctx.Done():
                    return
                case job, ok := <-jobsCh:
                    if !ok {
                        return
                    }
                    if err := process(ctx, job); err != nil {
                        select {
                        case errCh <- err:
                            cancel()
                        default:
                        }
                        return
                    }
                }
            }
        })
    }

send:
    for _, job := range jobs {
        select {
        case jobsCh <- job:
        case <-ctx.Done():
            break send
        }
    }
    close(jobsCh)
    wg.Wait()

    select {
    case err := <-errCh:
        return err
    default:
        return ctx.Err()
    }
}
```

示例的重点是所有 worker 都在函数返回前退出。生产代码还应决定是否收集全部错误、是否允许部分成功，以及 `process` 对取消的响应速度。

### GMP 模型

Go Runtime 使用用户态 M:N 调度器。三个核心实体分别是：

| 实体 | 含义 | 主要职责 |
|------|------|----------|
| G | goroutine 的 Runtime 表示 | 栈、调度现场、状态、等待原因 |
| M | machine，对应 OS 线程 | 执行 Go 代码、Runtime 代码或系统调用 |
| P | processor，执行普通 Go 代码所需的逻辑资源 | 本地运行队列、分配缓存、timer、GC work |

关系可以概括为：

```text
                    sched
          global run queue / idle P / idle M
                         |
       +-----------------+-----------------+
       |                 |                 |
      P0                P1                P2
 local runq         local runq         local runq
   timer              timer              timer
   mcache             mcache             mcache
       |                 |                 |
      M0                M1                M2       OS threads
       |                 |                 |
      G7                G3                G9       running goroutines
```

普通用户 G 需要 M 持有 P 才能执行。M 在系统调用、cgo、Runtime 系统栈或线程休眠期间可以没有 P。P 的数量是 `GOMAXPROCS`；M 的数量按阻塞和调度需求动态变化，因此线程数可能大于 P 数。

GMP 的目标不是提供严格优先级或公平时间片，而是兼顾：

- P 本地队列的缓存局部性；
- 全局队列的公平兜底；
- 空闲 P 从其他 P 窃取工作；
- 阻塞 syscall 时让其他 M 使用计算资源；
- 网络等待与 timer 不占住 OS 线程；
- 抢占长期运行的 Go 代码。

### G：goroutine 的 Runtime 表示

当前 `runtime.g` 很大，理解调度只需关注以下概念字段：

```go
type g struct {
    stack       stack
    stackguard0 uintptr

    m     *m
    sched gobuf

    atomicstatus atomic.Uint32
    goid         uint64
    waitsince    int64
    waitreason   waitReason

    preempt       bool
    preemptStop   bool
    asyncSafePoint bool

    schedlink guintptr
    waiting   *sudog
}
```

这不是可导入的定义，只是源码阅读地图。常见状态为：

| 状态 | 含义 |
|------|------|
| `_Grunnable` | 已可运行，正在某个运行队列等待 |
| `_Grunning` | 正在 M 上执行 |
| `_Gwaiting` | 等待 channel、锁、timer、网络事件等 |
| `_Gsyscall` | 正在系统调用或 cgo 路径 |
| `_Gpreempted` | 因抢占停下，等待恢复 |
| `_Gdead` | 不再执行，G 结构可被 Runtime 复用 |

#### 可增长栈

Go 1.26.4 当前为普通新 G 使用 2 KiB 的 `stackMin`，需要时分配更大的连续栈并复制有效内容。编译器生成的栈图帮助 Runtime 找到并修正指针。初始大小和增长策略是实现细节，不应成为容量计算依据。

64 位目标的当前默认单 G 栈上限约 1 GB，32 位目标约 250 MB。深递归仍可能因超过上限而触发不可恢复错误；不要把可增长栈理解成无限栈。

安全的 Go 指针会随 Runtime 协议正确处理。把栈地址转成 `uintptr` 后长期保存会脱离 GC 与栈移动跟踪，属于 `unsafe` 误用。

#### 创建流程

`go f()` 会进入 `runtime.newproc`。当前主线是：

1. 求值函数值与实参，必要时形成闭包环境。
2. 优先从当前 P 的 `gFree` 获取可复用 G。
3. 没有可复用对象时创建 G，并为它准备初始栈。
4. 初始化入口 PC、调用者信息与状态。
5. 通过 `runqput` 放入当前 P 的 `runnext` 或本地队列。
6. 若有空闲 P 且没有足够工作线程，`wakep` 安排 M 执行。

创建 G 可能分配 G、栈或逃逸的闭包环境，不能声称“go 语句零分配”或“成本接近普通函数调用”。用 `go test -benchmem` 和 allocation profile 测量实际路径。

Go 不提供稳定的 goroutine ID 或 goroutine-local storage。不要解析 `runtime.Stack` 获取 goid 作为业务标识；请求信息应显式传参或使用 Context 中的请求级元数据。

### P：并行资源与本地状态

Go 1.26.4 的 `runtime.p` 包含本地运行队列和多个 per-P 缓存。用于理解的子集如下：

```go
type p struct {
    id     int32
    status uint32
    m      muintptr

    mcache *mcache

    runqhead uint32
    runqtail uint32
    runq     [256]guintptr
    runnext  guintptr

    gFree     gList
    sudogcache []*sudog
    timers    timers
    gcw       gcWork
    wbBuf     wbBuf
}
```

关键点：

- `runq` 当前是容量 256 的环形队列。owner P 负责主要生产和消费，其他 P 窃取时通过原子操作推进 head。
- `runnext` 只有一个槽，适合让当前 G 刚唤醒的 G 继承剩余时间片，减少通信接力延迟。它不是优先队列。
- `mcache`、timer heap、GC work 和写屏障缓冲跟随 P，而不是跟随某个固定 M。P handoff 不意味着清空 `mcache`。
- P 缩减后可进入 `_Pdead`；这里的 dead 表示当前不参与调度，不是“该状态已废弃”。

#### 现代 `GOMAXPROCS`

`GOMAXPROCS` 决定 P 的数量，也就是普通 Go 代码的并行上限。它不限制 goroutine 总数，也不等于进程线程数。

Go 1.25 起，在新语言版本和默认兼容设置下，Runtime 会综合：

- 启动时可见的逻辑 CPU 数；
- 进程 CPU affinity；
- Linux cgroup CPU quota。

默认值还可根据 affinity 或 cgroup 变化定期重算。设置 `GOMAXPROCS` 环境变量或调用 `runtime.GOMAXPROCS(n)` 会采用显式值并停止自动更新；`runtime.SetDefaultGOMAXPROCS()` 可恢复当前默认策略。

工程上应先使用 Runtime 默认值。CPU 密集、IO 密集都不存在“固定等于 CPU 数”或“IO 场景设为两倍”的通用公式。显式调整前用 `/sched/gomaxprocs:threads`、CPU profile、调度延迟和代表性负载验证。

### M：OS 线程与系统栈

每个 M 对应一个 OS 线程。当前 `runtime.m` 中与调度相关的概念字段包括：

```go
type m struct {
    g0      *g
    gsignal *g
    curg    *g

    p     puintptr
    nextp puintptr
    oldp  puintptr

    spinning   bool
    blocked    bool
    preemptoff string
    locks      int32
    lockedg    guintptr
}
```

- `g0` 使用系统栈执行调度、栈管理和其他 Runtime 内部路径；`gsignal` 用于信号处理。它们不是用户创建的 goroutine。
- `curg` 是当前用户 G；`p`、`nextp`、`oldp` 记录 P 的当前、待绑定和 syscall 前关系。
- `spinning` 表示 M 暂时没有工作，正在寻找其他 P 的可运行 G 或 timer。
- `preemptoff` 与 `locks` 帮助 Runtime 避免在不安全区间抢占。
- `runtime.LockOSThread` 会把调用 G 与当前 M 绑定，适用于 GUI、线程局部状态、`setns` 等确实要求线程身份的 API。绑定不等于关闭 goroutine 抢占，且必须配对 `UnlockOSThread` 或按文档安排线程终止。

线程栈和每个 M 的总内存成本依 GOOS、是否 cgo、信号栈及 Runtime 实现而变，不应使用“每线程固定 8 MB”估算。大量阻塞 syscall 或 cgo 调用仍会增加 M 和内核资源，应观察：

- `/sched/threads/total:threads`；
- threadcreate profile；
- OS 线程数、虚拟内存与 cgroup PID 限制；
- trace 中的 syscall/cgo 区间。

`debug.SetMaxThreads` 是防止程序无限创建线程的保护阈值，当前默认值为 10000。超过限制会导致程序崩溃，不是可恢复的普通 panic；它不能替代并发限制和根因修复。

### 运行队列与 work stealing

调度器的 `schedule` / `findRunnable` 路径会综合多个工作来源。概念流程是：

```text
本地 runnext / local runq
          |
          v
全局 runq 的公平性检查
          |
          v
timer、GC worker、netpoll、work stealing 等来源
          |
          v
找到 G -> execute
没有工作 -> 释放 P，M 休眠或阻塞等待网络/timer
```

这不是稳定优先级清单。当前源码包含多次快速检查、状态复核和唤醒竞态处理，不能用一张固定流程图预测某个 G 一定先于另一个 G 运行。

几个重要实现点：

1. **本地优先**：多数新 G 进入当前 P，减少全局锁和 cache miss。
2. **全局公平兜底**：Go 1.26.4 当前每 61 次调度检查一次全局队列；61 是实现常量，不是公平性 SLA。
3. **本地队列溢出**：`runqputslow` 会把一批 G 转到全局队列，而不是无限扩容本地数组。
4. **窃取约一半**：空闲 P 从其他 P 的 runq 批量获取 G，减少频繁跨核操作。当前 `stealWork` 最多尝试若干轮，最后一轮还会考虑 victim 的 `runnext` 和 timer。
5. **spinning 协调**：`sched.nmspinning` 与 `wakep` 在“少唤醒导致延迟”和“多唤醒浪费 CPU”之间平衡。它不保证严格公平。

`runnext`、随机遍历顺序、窃取轮数和队列容量都可能演进。业务代码若要求优先级、配额或公平排队，应在应用层实现明确调度策略。

### 挂起与唤醒

goroutine 阻塞在 Runtime 可管理的同步点时，通常通过 `gopark` 进入 `_Gwaiting`，M 随即执行其他 G。事件完成后 `goready` 把它改为 `_Grunnable` 并放回运行队列。

常见路径：

| 等待原因 | Runtime 协作 |
|----------|--------------|
| channel send/receive | 用 `sudog` 登记到 channel 等待队列 |
| `sync.Mutex` / `RWMutex` | 竞争慢路径最终使用 Runtime semaphore |
| `time.Sleep` / timer | 登记到 per-P timer heap，到期后唤醒 |
| 网络 IO | 登记到 netpoll，fd 就绪后唤醒 |
| `select` | 同时登记候选 case，由一个 case 赢得唤醒竞争 |

阻塞 goroutine 不等于泄漏。只有当它不再有业务价值、却没有任何可达的取消或唤醒路径时，才是生命周期问题。反过来，即使 goroutine 数量稳定，长期持有大对象、锁或连接也可能造成资源泄漏。

`runtime.Gosched()` 只让当前 G 进入可运行状态并重新参与调度，不会释放它持有的应用锁，也不是 sleep 或同步屏障。正确性不能依赖“调用一次 Gosched 后另一个 goroutine 肯定运行”。

### 抢占

Go 同时保留同步安全点和异步抢占：

- 函数栈检查、显式调度点和部分 Runtime 调用可以协作式让出。
- Go 1.14 起，支持的平台可向长期运行 Go 代码所在的 M 请求异步抢占。Unix 系统通常使用预留信号，其他平台机制不同。
- 编译器提供安全点与指针活跃信息；信号落在不适合精确扫描或 Runtime 不可重入的位置时，抢占会推迟。

Go 1.26.4 的 `forcePreemptNS` 当前为 10ms，sysmon 以自适应周期检查。这只是防止长期占用的启发式值，不是 goroutine 时间片、最大调度延迟或服务延迟承诺。OS 调度、不可抢占 Runtime 区间、CPU quota、cgo 和系统负载都会增加实际延迟。

异步抢占解决了旧版本中“无函数调用的纯 Go 循环可能长期占住 P”的问题，但仍不能提供：

- 严格轮转公平；
- 硬实时 deadline；
- 对 C 代码内部的 Go 安全点；
- 对持有应用锁时自动释放锁。

CPU 密集循环仍应按业务边界检查 Context、分块处理，并用 trace 观察 runnable latency。不要为了“帮助调度”在所有循环里机械插入 `Gosched`；先测量，再决定是否需要显式让出。

### 系统调用与 cgo

普通阻塞系统调用会占住当前 M。Runtime 的目标是让 P 能继续服务其他 G：

1. `entersyscall` 把 G 标记为 syscall 状态，并记录 syscall 前的 P 关系。普通路径会乐观保留快速返回机会。
2. 已知会阻塞的 `entersyscallblock` 路径会立即释放并 handoff P。
3. 对普通 syscall，sysmon 可根据 P 的 syscall tick、是否有其他工作和经过时间接管 P。
4. `exitsyscall` 返回时尝试继续使用保留的 P、取回旧 P或获取空闲 P；都失败时把 G 放回可运行队列。

因此不能把“超过 20µs 才 handoff”写成固定规则。当前源码确实以 sysmon tick 和 10ms 兜底窗口参与判断，但是否接管还取决于其他 P 是否空闲、runq 是否有工作及状态竞态。

常见工程影响：

- 普通磁盘文件通常不能像 socket 一样交给网络 poller；阻塞文件 IO 会占用 M，Runtime 可另启 M 保持 Go 代码进展。
- pipe、socket、字符设备等是否可 poll 取决于平台、fd 类型和打开方式，不能把所有 `os.File` 归为同一类。
- cgo 调用通常会让出 P，但 C 调用仍占一个 M，且不能在 C 指令中执行 Go 异步抢占。大量慢 cgo 会推高线程数。
- 用 worker 数、连接池或 semaphore 限制阻塞操作并发；不要依赖 `SetMaxThreads` 在最后兜底。

### netpoll 与 deadline

Runtime netpoll 抽象了 Linux epoll、BSD/macOS kqueue、Windows IO completion ports、Solaris event ports 等平台能力。Go 网络栈通常把可轮询 fd 配置为非阻塞：

```text
G 调用 Conn.Read
      |
      v
当前没有数据 -> pollDesc 登记读等待 -> gopark
      |
      v
M 继续执行其他 G
      |
      v
OS 报告 fd 就绪 -> netpoll 收集等待 G -> goready
      |
      v
G 再次被调度，完成 Read
```

`findRunnable` 会做非阻塞 poll；没有其他工作时，某个 M 可以阻塞在 netpoll，等待最早网络事件或 timer。sysmon 还有非阻塞兜底检查。Go 1.26.4 当前在距上次 poll 超过约 10ms 时触发该兜底，这同样不是网络唤醒 SLA。

`SetDeadline` 把 timer 与 poll 等待关联；到期后等待中的 G 被唤醒并得到超时错误。Context 取消是否能中断操作取决于具体 API，网络请求应使用支持 Context 的入口。

生产边界应显式配置超时：

- `http.Server` 的 `ReadTimeout`、`ReadHeaderTimeout`、`WriteTimeout` 和 `IdleTimeout` 零值通常表示没有对应超时；默认 Server 并不会自动给出完整保护。
- `http.Client.Timeout` 是整个请求的上限，不适合所有流式场景；还应按需配置 Dial、TLS handshake、response header 和请求 Context。
- `net.Dialer.Timeout` / `Deadline` 只约束建连，不替代后续读写 deadline。

DNS 可能使用 Go resolver 或 cgo resolver，选择受平台、构建方式、配置文件和 `GODEBUG=netdns` 影响。排查慢 DNS 时先用 `GODEBUG=netdns=1`、trace 和 thread profile 确认实际路径，再决定是否强制 resolver；两种实现的系统集成语义并不完全相同。

### 泄漏与容量治理

常见泄漏模式不是“goroutine 太多”，而是启动后失去退出路径：

```go
func produce(ctx context.Context, out chan<- Item) error {
    for {
        item, err := next(ctx)
        if err != nil {
            return err
        }
        select {
        case out <- item:
        case <-ctx.Done():
            return ctx.Err()
        }
    }
}
```

评审时逐项回答：

1. 谁关闭输入，谁关闭输出？
2. consumer 早退时 producer 如何得知？
3. 阻塞发送、接收、锁和 IO 是否有取消路径？
4. 任务数量是否受限，突发流量在哪里排队？
5. goroutine 返回前谁负责等待？
6. error、panic 和部分结果如何处理？

无限制 `go func()` 会把背压转成 goroutine、栈、连接和下游请求。即使每个 G 很轻，调度、根扫描和持有对象仍有成本。优先使用固定 worker、带容量队列或 semaphore，使并发上限与下游容量一致。

### 诊断工具

#### Runtime metrics

Go 1.26 提供更细的调度状态指标：

| 指标 | 含义 |
|------|------|
| `/sched/goroutines:goroutines` | 当前存活 G 数 |
| `/sched/goroutines/runnable:goroutines` | 近似可运行但尚未执行的 G |
| `/sched/goroutines/running:goroutines` | 近似正在执行的 G |
| `/sched/goroutines/waiting:goroutines` | 近似等待 IO 或同步原语的 G |
| `/sched/goroutines/not-in-go:goroutines` | 近似处于 syscall/cgo 的 G |
| `/sched/goroutines-created:goroutines` | 启动以来创建总数 |
| `/sched/threads/total:threads` | Runtime 当前拥有的线程数 |
| `/sched/latencies:seconds` | G 处于 runnable 后等待运行的分布 |

状态子指标是近似值，文档明确不保证彼此相加等于总数。采集前用 `runtime/metrics.All` 检查目标工具链是否支持。

#### Profile 与 trace

- goroutine profile：看当前栈与等待位置，比较多次快照判断是否持续增长。
- block profile：定位 channel、select、锁等阻塞累计时间。
- mutex profile：定位锁竞争的持有方。
- threadcreate profile：定位导致 OS 线程创建的调用栈。
- execution trace：观察 G/M/P 时间线、runnable 延迟、syscall、网络、GC 和抢占。

```bash
GODEBUG=schedtrace=1000,scheddetail=1 ./service
curl -sS http://127.0.0.1:6060/debug/pprof/goroutine?debug=2
go tool pprof http://127.0.0.1:6060/debug/pprof/block
go tool trace trace.out
```

pprof HTTP 端点会暴露调用栈和运行信息，必须放在受控管理面，不要直接公开到互联网。

Go 1.26 还提供实验性的 `goroutineleak` profile，需在构建时启用：

```bash
GOEXPERIMENT=goroutineleakprofile go build ./cmd/service
```

启用后可通过 `pprof.Lookup("goroutineleak")` 或 `/debug/pprof/goroutineleak` 获取。它借助 GC 找出“阻塞在某同步原语上，且任何可运行链路都无法再到达并唤醒该原语”的 G，能发现一大类永久阻塞，但不能证明所有业务泄漏都不存在。该功能在 Go 1.26 仍是实验 API。

### 源码阅读路线

固定到目标 Go tag 后按以下顺序阅读：

1. `src/runtime/runtime2.go`：`g`、`m`、`p` 与状态常量。
2. `src/runtime/proc.go`：`newproc`、`schedule`、`findRunnable`、`stealWork`、sysmon、syscall 进出。
3. `src/runtime/stack.go`：栈常量、`newstack`、`copystack`。
4. `src/runtime/preempt.go` 与平台信号文件：同步/异步抢占。
5. `src/runtime/netpoll.go` 与 `netpoll_*.go`：pollDesc 和平台 poller。
6. `src/runtime/time.go`：per-P timer heap 与调度器协作。

不要从博客里的私有结构体反推当前实现。最可靠的顺序是先读公开文档和 trace 语义，再读相同补丁版本的源码，最后用小实验验证。

### 本章小结

- G 保存栈、状态和等待信息；M 是 OS 线程；P 持有执行 Go 代码所需的本地调度与 Runtime 资源。
- `GOMAXPROCS` 是 P 数，不是线程数。Go 1.25+ 默认可感知 affinity 与 Linux cgroup quota，并动态更新。
- 新 G 优先进入本地队列，溢出进入全局队列；空闲 P 通过 work stealing、netpoll、timer 和其他来源寻找工作。
- 当前 10ms 抢占目标、61 次全局队列检查、256 槽 runq 等都是实现细节，不是延迟或公平保证。
- 可管理的同步等待会 park G 而释放 M/P 执行其他工作；阻塞 syscall 和 cgo 仍占 M，但 P 可被 handoff。
- netpoll 让可轮询 IO 不长期占用线程，deadline 和 Context 决定业务等待何时结束。
- goroutine 必须有 owner、退出条件、取消路径、等待者和并发上限；数量增长只是症状，根因是生命周期或背压协议。
- 用 Runtime metrics、goroutine/block/mutex/threadcreate profile 和 trace 基于证据诊断，不用固定纳秒与线程栈大小猜测。
