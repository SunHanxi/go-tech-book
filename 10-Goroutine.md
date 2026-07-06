## 第10章 Goroutine（重点）

> 引言：Goroutine 是 Go 语言并发模型的灵魂。它以极低的创建与切换成本支撑了"百万并发"的工程神话，而这一神话的底层支柱是 Go Runtime 的 GMP 调度器。本章将从调度器整体设计出发，逐个拆解 M、P、G 三大核心实体，串起 GMP 协同模型，再深入 work stealing、抢占调度、syscall、netpoll 这些让 goroutine "跑得稳、跑得快、跑得不被饿死"的关键机制。理解本章，是理解 [第11章 Channel](./11-Channel.md) 与 [第12章 select](./12-select.md) 的前置基础。

---

### Go Scheduler

#### 1. 是什么

Go Scheduler 是 Go Runtime 内置的 **用户态 M:N 调度器**。它把用户创建的 N 个 goroutine（G）映射到 M 个 OS 线程上执行，并依靠 P（Processor）这一逻辑资源来承载调度上下文与本地运行队列。对用户而言，`go f()` 这一行代码背后发生的事情，全部由 Scheduler 接管：分配栈、入队、被 P 选中、被 M 执行、阻塞时让出、被抢占、被唤醒……

#### 2. 为什么这样设计 / 底层实现要点

历史上 Go 调度器经历了两个阶段：

| 阶段 | 版本 | 模型 | 主要问题 |
|------|------|------|----------|
| 早期 | Go 1.0 | GM 模型（全局队列 + 一把大锁） | 全局锁竞争激烈、缓存局部性差、syscall 阻塞拖累吞吐 |
| 现代 | Go 1.1+ | GMP 模型（引入 P 与本地队列） | 解决锁竞争、改善缓存命中、支撑 work stealing 与抢占 |

设计目标可归纳为：

- **线程创建/切换成本不能传导给用户**：goroutine 切换是用户态寄存器切换 + 栈切换，不陷入内核。
- **充分利用多核**：每个 P 持有本地运行队列，避免全局锁成为瓶颈。
- **syscall 不阻塞调度**：M 进入阻塞系统调用时，P 会与之解绑，被其他 M 复用继续调度。
- **网络 IO 不占用线程**：netpoller 把网络等待转成"就绪事件"，goroutine 在就绪前不占 M。
- **公平性**：work stealing + 抢占 + 全局队列定期轮转，避免 starvation。

全局调度状态保存在 `runtime` 包的 `schedt` 结构（变量名 `sched`）中，简化伪代码如下（基于 Go 1.21，省略大量字段）：

```go
package runtime

// schedt 是全局调度器状态，全局唯一实例为 sched，受 sched.lock 保护
type schedt struct {
	goidgen   uint64 // goroutine id 生成器
	lastpoll  uint64 // 上次 netpoll 的时间戳（nanotime）
	pollUntil uint64 // 下次 netpoll 截止时间

	lock mutex // 保护下面所有字段

	midle        muintptr // 空闲 M 链表头
	nmidle       int32    // 空闲 M 数量
	nmidlelocked int32    // 锁定状态下的空闲 M
	mnext        int64    // 下一个分配的 M id
	maxmcount    int32    // M 数量上限（默认 10000）
	nmsys        int32    // 系统级 M（如 signal M）数量
	nmfreed      int64    // 累计已释放的 M 数量

	pidle      puintptr // 空闲 P 链表头
	npidle     uint32   // 空闲 P 数量
	nmspinning uint32   // 正在 spinning（自旋找活干）的 M 数量

	runq     gQueue  // 全局运行队列
	runqsize int32   // 全局队列长度

	gFree struct { // 全局空闲 g 列表（g 复用池）
		gList
		n int32
	}

	sudoglock  mutex
	sudogcache *sudog // sudog 复用池（channel/sync 用）
	...
}
```

逐字段说明：

- `goidgen`：goroutine 的 `goid` 从这里递增取，避免每个 P 自己造 id 冲突。
- `lastpoll` / `pollUntil`：netpoll 模块用，调度器在 `findRunnable` 里据此决定是否阻塞等待网络事件。
- `lock`：全局大锁，**只保护全局队列与空闲资源池**，不保护 P 本地队列——这是 GMP 把锁打散的关键。
- `midle` / `nmidle`：M 进入空闲后会挂在这里，下次需要新 M 时优先复用，而不是 `newosproc` 创建新线程。
- `maxmcount`：M 数量上限，默认 10000，可通过 `debug.SetMaxThreads` 调整。这是一个经常被忽视的"硬天花板"。
- `pidle` / `npidle`：空闲 P 链表。当所有 P 都在跑、有网络就绪或新 goroutine 涌入时，会从 `pidle` 唤醒 P。
- `nmspinning`：spinning M 的数量。这是 work stealing 协调的核心计数器，防止"没有活却还在自旋"或"有活却没人自旋"。
- `runq` / `runqsize`：全局运行队列。当 P 本地队列满（256）时，一半会溢出到这里；调度器定期从这里取一批回填本地队列。

调度器的核心循环入口是 `runtime.schedule()`，它会按以下优先级选 G 来执行（简化逻辑）：

```go
func schedule() {
	mp := getg().m
	mp.p.ptr().preempt = false

	var gp *g
	// 1) 每 61 次调度，从全局队列取一批，避免全局队列饿死
	if mp.p.ptr().schedtick%61 == 0 && sched.runqsize > 0 {
		lock(&sched.lock)
		gp = globrunqget(mp.p.ptr(), 1)
		unlock(&sched.lock)
	}
	// 2) 本地队列
	if gp == nil {
		gp, _ = runqget(mp.p.ptr())
	}
	// 3) findRunnable：本地、全局、netpoll、work stealing 综合查找
	if gp == nil {
		gp = findRunnable()
	}
	execute(gp)
}
```

> **要点**：`schedtick%61 == 0` 这一行是 Go 调度公平性的"防饿死兜底"。它保证全局队列里的高优先级或被抢占的 goroutine 不会被本地队列无限挤压。

#### 3. 工程实践与常见坑

- **`GOMAXPROCS` 的含义是 P 的数量，不是线程数，也不是 CPU 数。** 默认等于 `runtime.NumCPU()`，可通过 `runtime.GOMAXPROCS(n)` 或环境变量 `GOMAXPROCS` 设置。在容器化场景下，默认值会取宿主机 CPU 数，导致 P 过多引发上下文切换抖动，建议在容器入口显式调用 `runtime.GOMAXPROCS(runtime.NumCPU())` 或使用 `automaxprocs` 库读取 cgroup 限制。
- **`debug.SetMaxThreads(N)` 设置 M 上限。** 默认 10000，对绝大多数应用足够；但若你的程序大量使用阻塞 syscall（如 cgo 调用阻塞库），M 会随 P 解绑而暴涨，可能撞顶，撞顶后新建 goroutine 会 panic `go: runtime: out of threads`。
- **不要在 `init` 里 `go func`。** init 期间 scheduler 可能尚未完全就绪，goroutine 启动时机不确定，容易产生"测试偶现 nil"的问题。
- **goroutine 不是协程。** 用户态协程通常需要显式 `yield`，而 goroutine 由调度器抢占式调度（Go 1.14+），不要假设"同步点"。

---

### M

#### 1. 是什么

M（Machine）是 Go Runtime 对 **OS 线程** 的抽象。每一个 M 都对应一个真正的内核线程（`pthread`/`clone`）。M 才是真正"跑代码"的实体——P 提供"待办事项列表"，G 提供"待办事项本身"，而 M 是"执行者"。

#### 2. 底层数据结构与 Runtime 实现要点

`m` 结构体简化定义（基于 Go 1.21）：

```go
package runtime

type m struct {
	g0          *g       // M 专用的调度栈 goroutine，负责调度切换与系统调用
	morebuf     gobuf    // 栈增长时保存的现场
	div         uint32   // div 的 modulo 帮助字段（调试用）

	// goroutine 相关
	curg        *g       // 当前 M 正在执行的用户 G（非 g0 时）
	caughtsig   guintptr // 处理信号时被打断的 g
	p           puintptr // 当前绑定的 P
	nextp       puintptr // 下一个要绑定的 P（用于 syscall 退出后）
	oldp        puintptr // syscall 之前绑定的 P（用于退出时回绑判断）

	id          int64    // M 的唯一 id
	mallocing   int32    // >0 表示正在分配内存（防止递归）
	throwing    int32    // >0 表示正在 throw
	preemptoff  string   // 禁止抢占的原因字符串（"" 表示允许抢占）
	locksHeld   int32    // 持锁计数（>0 时禁止抢占）

	// 调度状态
	spinning    bool     // true 表示此 M 正在自旋找活干
	blockeded   bool     // true 表示 M 阻塞在系统调用（即将被释放或复用）
	newSigsetup bool     // 标记是否需要重新初始化信号栈

	// 系统线程相关
	ts          tls      // thread-local storage
	mstartfn    func()   // M 启动时执行的函数
	crelsema    uint32   // M 创建信号量

	// 性能采样
	syscalltick uint32   // syscall 计数，用于 GC
	...
}
```

逐字段说明：

- `g0`：**M 的"调度专用 goroutine"**。每个 M 都有自己的 `g0`，栈较大（默认 8MB，可增长），专门用于执行调度逻辑、栈扩缩容、`cgocall`、信号处理等"运行时内部"工作。`g0` 不是用户代码创建的，它在 `mstart` 时通过 `mstart1` 初始化。理解 `g0` 是理解调度切换的关键——M 在调度时"切到 g0"，在执行用户代码时"切到 curg"。
- `curg`：当前 M 正在运行的用户 goroutine。调度器执行用户 G 时，`getg().m.curg` 就是该 G；执行调度代码时，`getg()` 就是 `g0`。
- `p` / `nextp` / `oldp`：M 与 P 的绑定关系三连。`p` 是当前绑定；`nextp` 用于"等待绑定的 P"（如 syscall handoff 时新 M 被指定接手 P）；`oldp` 在 syscall 返回后判断是否还能回到原来的 P。
- `id`：M 的递增 id，由 `sched.mnext` 分配。
- `spinning`：**work stealing 协调的核心标志**。当 M 找不到 G 但又不甘心休眠时，会进入 spinning 状态自旋一段时间去偷别人的 G。`sched.nmspinning` 是所有 spinning M 的总和。
- `blocked`：M 进入阻塞 syscall 后置 true，`exitsyscall` 会据此判断是否要 handoff P。
- `preemptoff` / `locksHeld`：抢占控制。当持锁或正在执行不可中断的运行时操作时，会写 `preemptoff` 或加 `locksHeld`，抢占信号会被忽略。这是 async preemption 安全性的兜底。

M 的生命周期由 `mstart` → `mstart1` → `schedule()` 循环驱动：

```
newosproc 创建线程
   |
   v
mstart()         // 设置 mstartfn、tls、信号栈
   |
   v
mstart1()        // 初始化 g0.sched，调用 mstartfn
   |
   v
schedule()       // 进入主调度循环，永不返回
   |
   v
mexit()          // M 退出（罕见，仅在 maxmcount 收缩时）
```

> **要点**：M 是"重"资源——创建一个 M 等于创建一个 OS 线程，需要内核分配栈、TLS、调度实体。所以 Runtime 会尽量复用 M，而不是频繁创建/销毁。

#### 3. 工程实践与常见坑

- **M 数量可以远大于 P 数量**：当大量 goroutine 阻塞在 syscall（尤其是 cgo 调用）时，M 会持续被创建，每个阻塞的 M 占一份内核资源（默认栈 8MB）。1000 个阻塞 syscall 理论上吃 8GB 内存——这就是 `maxmcount` 存在的原因。
- **cgo 是 M 杀手**：cgo 调用进入 C 代码时，Go 调度器无法抢占该 M，P 会被 handoff 出去，M 阻塞在 C 里。高频 cgo + 大量 goroutine 会触发 M 暴涨，监控上表现为 `go_sched_goroutines` 与线程数同时飙升。
- **`runtime.NumGOMAXPROCS() != runtime.NumCPU()` 的容器陷阱**：见 Scheduler 节，此处不重复，但根因是 P 数与 M 数脱钩后，M 行为受 cgroup 影响。
- **调试 M 状态**：`runtime/pprof` 的 threadcreate profile 可以看线程创建；`/debug/pprof/threadcreate?debug=1` 输出每个 M 的栈。线上偶现"线程数暴涨"时是第一手段。

---

### P

#### 1. 是什么

P（Processor）是 **逻辑处理器**，是 GMP 模型在 Go 1.1 引入的关键抽象。它持有"调度上下文"：本地运行队列、mcache（内存分配缓存）、defer 池、sudog 池等。P 的数量 = `GOMAXPROCS`，决定了"同时执行 Go 代码的并行度上限"。

> 一句话理解：**M 是腿，P 是工作台，G 是任务。** 没有工作台，腿跑得再快也无处干活；没有腿，工作台再好也是死的。

#### 2. 底层数据结构与 Runtime 实现要点

`p` 结构体简化定义（基于 Go 1.21）：

```go
package runtime

const (
	_Pidle     = 0 // 空闲，挂在 sched.pidle 链表
	_Prunning   = 1 // 绑定了 M，正在执行
	_Psyscall   = 2 // 绑定的 M 进入 syscall（可能 handoff）
	_PgcStop    = 3 // STW 中暂停
	_Pdead      = 4 // 已废弃（GOMAXPROCS 缩小后）
)

type p struct {
	id          int32
	status      uint32 // 取值为上面的 _Pidle/_Prunning/...
	link        puintptr // 空闲链表 next 指针

	// 与 M 的关系
	m           muintptr // 当前绑定的 M（0 表示未绑定）

	// 内存分配缓存
	mcache      *mcache  // 每个 P 独享的 mcache，无锁分配小对象

	// 本地运行队列（固定大小环形数组）
	runqhead    uint32
	runqtail    uint32
	runq        [256]guintptr // 容量 256 的环形队列
	runnext     guintptr     // 下一个要执行的 G（"插队"槽，最优先）

	// 缓慢的可运行队列（用于抢占恢复，1.14 后较少使用）
	gFree struct {
		gList
		n int32
	}

	deferpool    [5][]*_defer   // defer 复用池，按 sizeclass 分桶
	deferpoolbuf [5][512]*_defer

	sudogcache []*sudog // sudog 复用池（channel/sync.WaitGroup 等）
	sudogbuf   [128]*sudog

	// 调度统计
	schedtick    uint32 // 调度计数（schedule() 被调用次数）
	syscalltick  uint32 // syscall 计数
	syscallTick  uint32 // 同上（兼容字段）

	// GC 相关
	gcAssistTime         int64 // 本 P 在 GC assist 中花费的时间
	gcFractionalMarkTime int64
	gcBgMarkWorker       guintptr
	gcMarkWorkerMode     gcMarkWorkerMode

	// 抢占
	preempt    bool   // 是否被请求抢占
	pad        cpu.CacheLinePad // 缓存行填充，避免 false sharing
}
```

逐字段说明：

- `status`：P 的状态机核心。`_Prunning` 时持有 M 在跑；`_Psyscall` 时 M 进入 syscall，调度器可能把 P handoff 给另一个 M；`_PgcStop` 是 STW 暂停；`_Pdead` 是 `GOMAXPROCS` 缩小后被淘汰的 P。
- `m` / `link`：`m` 是当前绑定 M；`link` 在 P 空闲时串成 `sched.pidle` 链表。
- `mcache`：**P 性能的基石之一**。每个 P 一个 mcache，分配小对象时无需加锁。P 被 handoff 时 mcache 会被清空/迁移。这是 GMP 引入后内存分配器吞吐大幅提升的原因。
- `runq`：**容量 256 的环形队列**。`runqhead`/`runqtail` 是无锁的（单 P 内单线程访问）。这是"本地队列"——绝大多数 goroutine 调度都在这里完成，无锁、缓存友好。
- `runnext`：**特殊优先槽**。`runtime.runnext` 持有一个 G，下次 `runqget` 会优先返回它。这是"刚唤醒的 G 立即执行"的优化，减少延迟。但 `runnext` 不是 LIFO 队列，它只有一个槽位。
- `gFree`：已结束的 G 不会立即归还堆，而是挂在 P 本地的 `gFree` 列表，下次 `newproc` 时复用，避免反复分配栈。满了会溢出到 `sched.gFree`。
- `deferpool` / `sudogcache`：defer 与 sudog 的复用池，避免高频 channel/defer 操作产生 GC 压力。
- `preempt`：抢占请求标志。`preemptone` 会设置它，M 在函数序言或被信号打断时检查。
- `pad`：**缓存行填充**，防止相邻 P 的字段落在同一缓存行导致 false sharing。这是性能细节里容易被忽视却至关重要的设计。

P 状态机简化流转：

```
                 newproc/GOMAXPROCS
   _Pdead  <---------------------->  _Pidle
                                       |
                        acquirep()/    |  releasep()
                       绑定 M          |  解绑 M
                                       v
                                   _Prunning
                                       |
                                       | M entersyscall
                                       v
                                   _Psyscall
                                  /         \
                  handoff P      /           \  exitsyscall fast path
                  (新 M 接手)   v             v
                              _Prunning    _Prunning
```

#### 3. 工程实践与常见坑

- **`GOMAXPROCS` 不是越大越好**：P 太多会导致 M 间缓存失效、调度抖动。CPU 密集型场景下 `GOMAXPROCS == CPU 数` 最优；IO 密集型可略高，但通常不超过 2 倍。
- **动态调整 `GOMAXPROCS` 会创建/杀死 P**：增加时新建 P 加入 `pidle`；缩小时多余的 P 标记为 `_Pdead`，其本地队列的 G 转移到全局队列。**不要在热路径里频繁调整**，否则会触发 G 迁移风暴。
- **`GOMAXPROCS=1` 不会死锁**：因为 netpoll、syscall handoff 仍然能让 P 在阻塞时被释放。但纯 CPU 密集 + `GOMAXPROCS=1` 时，无 sysmon 唤醒会导致长时间不调度——Go 1.14 后异步抢占解决了这个问题。
- **`runtime.GOMAXPROCS(0)` 是查询而非设置**：传 0 表示不修改，返回当前值。这是常见的 API 误用坑。
- **监控 P 状态**：`runtime/trace` 或 `pprof` 的 `sched` 信息可看每个 P 的 `schedtick`、`syscalltick`。线上若某 P 的 `syscalltick` 涨势远快于其他 P，多半是该 P 上的 goroutine 频繁阻塞 syscall。

---

### G

#### 1. 是什么

G（Goroutine）是 **用户态轻量级线程**的运行时表示。每个 `go f()` 都会创建一个 G。G 持有自身的栈、状态机、调度现场（`gobuf`）以及 panic/defer 链。G 是"被调度"的对象，它不直接绑定 OS 线程，而是在 P 的本地队列里等待被 M 选中执行。

#### 2. 底层数据结构与 Runtime 实现要点

`g` 结构体简化定义（基于 Go 1.21）：

```go
package runtime

const (
	_Gidle      = iota // 0 刚创建，尚未初始化
	_Grunnable          // 1 可运行，在某个运行队列上等待
	_Grunning           // 2 正在执行（绑定了 M 和 P）
	_Gsyscall           // 3 正在系统调用（绑定了 M，可能没 P）
	_Gwaiting           // 4 等待（channel/lock/timer 等），不在运行队列
	_GmoribundUnused    // 5 历史状态，已废弃
	_Gdead              // 6 已结束，G 结构被复用
	_Genqueue           // 7 枚举兼容（不实际使用）
	_Gcopystack         // 8 正在复制栈（栈扩缩容）
	_Gpreempted         // 9 被异步抢占，等待恢复（Go 1.14+）
)

type gobuf struct {
	sp   uintptr // 栈指针
	pc   uintptr // 程序计数器
	g    guintptr
	ret  uintptr // 返回值
	bp   uintptr // 基址指针
}

type g struct {
	// 栈管理
	stack       stack   // 当前栈的 [lo, hi]
	stackguard0 uintptr // 栈溢出检查阈值（用户代码用）
	stackguard1 uintptr // 栈溢出检查阈值（g0 / signal 用）

	// 调度现场
	m           *m      // 当前绑定的 M
	sched       gobuf   // 切换出去时保存的现场（sp/pc 等）
	syscallsp   uintptr // syscall 时的 sp
	syscallpc   uintptr // syscall 时的 pc
	stktopsp    uintptr // 栈顶 sp，用于栈扫描

	// panic / defer 链
	_panic      *_panic
	_defer      *_defer

	// 状态与标识
	atomicstatus uint32  // G 状态（_Gidle/_Grunnable/...）
	goid         int64   // goroutine id
	waitsince    int64   // 开始等待的时间（用于 starvation 检测）
	waitreason   waitReason // 等待原因（"chan receive"、"select" 等）

	// 抢占
	preempt       bool   // 抢占请求标志
	preemptStop   bool   // 抢占时是否需停在调度点（用于 GC STW）
	preemptShrink bool   // 抢占时同步缩小栈

	// 异步抢占相关
	asyncSafePoint bool  // 是否在安全点
	preemptGen      uint32 // 抢占信号代数

	// 链接
	schedlink    guintptr // 在某个队列（runq/gFree/wait）中时的 next 指针
	...
}

type stack struct {
	lo uintptr
	hi uintptr
}
```

逐字段说明：

- `stack`：**G 的栈区间**。初始栈很小（Go 1.4 后默认 2KB），可增长到 1GB（64 位）。栈管理是连续栈（contiguous stack）：满时分配双倍新栈，复制旧栈内容，释放旧栈。这是 goroutine 创建成本远低于线程（线程栈默认 8MB）的根本原因。
- `stackguard0`：**栈溢出检查的关键字段**。函数序言会比较 `SP` 与 `stackguard0`，不足时跳到 `runtime.morestack` 扩栈。Go 1.14 前它也用作"协作式抢占"的标记位——`preemptone` 把 `stackguard0` 设为 `0xfffffade`（`stackPreempt`），导致下次函数调用序言检测到"栈不够"而进入调度器。这就是早期抢占"必须在函数调用点才生效"的原因。
- `sched`：**G 的切换现场**。`gogo(g.sched)` 会从 `sched.sp`/`sched.pc` 恢复寄存器并跳转。`mcall`/`gogo` 这一对汇编函数就是 G 切换的物理实现。
- `atomicstatus`：状态机。常见流转：`_Gidle` → `_Grunnable` → `_Grunning` → `_Gsyscall` → `_Grunning` → ... → `_Gdead`。`_Gpreempted` 是异步抢占新增的中间态。
- `waitsince` / `waitreason`：等待起始时间与原因。`waitreason` 在 `pprof` 与 `trace` 里是诊断"goroutine 卡在哪"的核心信息（如 `"semacquire"`、`"chan receive"`、`"select"`、`"IO wait"`）。
- `preempt` / `preemptStop` / `preemptShrink`：抢占控制三元组。`preempt` 是请求标志；`preemptStop` 表示抢占后必须停（STW 用）；`preemptShrink` 表示抢占时顺便缩栈。
- `schedlink`：G 在某个链表（全局 runq、gFree、wait queue）中时的 next 指针。

G 的状态流转简化图：

```
   newproc()
      |
      v
  _Gidle --gostartcall--> _Grunnable --execute()--> _Grunning
                                 ^                     |
                                 |                     | entersyscall
                                 |                     v
                                 |                  _Gsyscall
                                 |                     |
                                 | exitsyscall        |
                                 +---------------------+
                                 |
                                 | gopark (channel/lock/timer)
                                 v
                              _Gwaiting
                                 | goready
                                 v
                            _Grunnable
                                 |
                                 | goexit
                                 v
                              _Gdead --复用--> _Gidle
```

G 的创建入口 `runtime.newproc`（用户代码 `go f()` 编译后调用）简化逻辑：

```go
package main

import (
	"fmt"
	"runtime"
)

func main() {
	// go 关键字在编译期被展开为 runtime.newproc 调用
	// 等价于：runtime.newproc(siz, f)
	go sayHello()
	runtime.Gosched() // 让出 P，给新 G 执行机会
	fmt.Println("main done")
}

func sayHello() { fmt.Println("hello") }
```

`newproc` 内部简化为：

```go
// 伪代码（不可编译，仅说明逻辑）
func newproc(siz int32, fn *funcval) {
	newg := gfget()           // 从 P.gFree 或 sched.gFree 复用一个 G
	if newg == nil {
		newg = malg(_StackMin) // 没有可复用的，分配新 G + 2KB 栈
	}
	// 设置状态为 _Grunnable，准备入队
	newg.status = _Grunnable
	// 把 fn 的参数从调用者栈复制到 newg 栈
	memmove(...)
	// 入本地队列
	runqput(_p_, newg, true)
	// 若有空闲 P 且无 spinning M，唤醒一个 M 来处理
	if sched.npidle != 0 && atomicload(&sched.nmspinning) == 0 {
		wakep()
	}
}
```

> **要点**：G 的创建是"复用优先"。Runtime 维护 `P.gFree` 与 `sched.gFree` 两级池，绝大多数 `go` 语句命中池，只复制参数到已有栈，**不分配堆内存**。这就是 goroutine 创建成本接近函数调用的关键。

#### 3. 工程实践与常见坑

- **goroutine 泄漏是头号杀手**：常见于 `for { select { case ch <- x: ... } }` 配合 `context` 不当——发送方等接收方，接收方已退出，goroutine 永久阻塞。诊断：`pprof goroutine` 看 `waitreason` 分布，`"chan send"` / `"chan receive"` 持续增长是泄漏信号。
- **`runtime.Gosched()` 主动让出，但不释放锁**：它只是把当前 G 重新放回本地队列尾部。误以为它会"等待"是常见错误——它立刻返回（被调度回来时）。
- **goroutine 没有父进程关系**：父 goroutine 退出不会回收子 goroutine。`go` 出去的 G 与父 G 完全独立，需要用 `sync.WaitGroup` 或 channel 显式同步。
- **`goid` 不是稳定 ID**：`runtime.Stack` 字符串里能拿到 goid，但官方明确不保证唯一/稳定，不要用它做业务标识。需要唯一 ID 自己生成。
- **栈大小有限**：默认上限 1GB（64 位），深递归会 `runtime: goroutine stack exceeds 1000000000-byte limit; created by ...` 然后崩溃。改写为迭代或增大 `debug.SetMaxStack`（慎用）。
- **`runtime.LockOSThread` 把 G 钉在当前 M**：用于必须固定线程的场景（如 cgo 调用 GUI 库、`setns` 等）。被锁的 G 不会被调度到其他 M，且其 M 不会被复用给其他 G——滥用会破坏 GMP 的灵活性。

---

### GMP 模型

#### 1. 是什么

GMP 模型是 Go 调度器把 **G（goroutine）、M（OS 线程）、P（逻辑处理器）** 三者组合起来协同工作的整体架构。它通过 P 的本地队列、P 与 M 的解绑/重绑、work stealing、抢占、netpoll 五大机制，实现了"用户态低开销调度 + 多核并行 + IO 不阻塞"。

#### 2. 整体架构与设计要点

ASCII 全景图：

```
                    +---------------------------------------+
                    |            sched (schedt)             |
                    |  global runq | pidle | midle | gFree  |
                    +-----+---------------------------------+
                          |                ^
            globrunqget   |                | runqput_slow
                          v                |
   +----------------------------------------------------------------+
   |                       P0       P1       P2        ... Pn-1     |
   |                     (runq)  (runq)  (runq)         (runq)      |
   |                  [G1..G256][G..]  [G..]            [G..]        |
   +-------+-------------+--------+--------+------------------------+
           |             |        |        |
           |execute      |execute |        |execute
           v             v        v        v
        +-----+       +-----+  +-----+  +-----+
   M0-->| M0  |   M1->| M1  |  | M2  |  | M3  |   ... OS Threads
        |g0/cu|       |g0/cu|  |g0/cu|  |g0/cu|       (curg = 当前 G)
        +-----+       +-----+  +-----+  +-----+
           |             |        |        |
        syscall/      syscall  netpoll  spinning
        netpoll       block    wait     (work stealing)
        handoff
```

核心规则：

1. **P 的数量 = `GOMAXPROCS`**，决定并行度上限。M 数量动态变化（受 `maxmcount` 限制）。
2. **M 必须绑定 P 才能执行用户 G**。M 执行调度代码时跑在 `g0` 上；执行用户代码时切到 `curg`。
3. **G 在 P 的本地队列**，被绑定的 M 取出执行。本地队列无锁、容量 256。
4. **本地队列满，半数 G 溢出到全局队列**（`runqput_slow`）。
5. **本地队列空，依次尝试**：全局队列 → netpoll → work stealing → 休眠。

设计要点表：

| 设计选择 | 解决的问题 | 代价 |
|----------|------------|------|
| 本地 runq（256 容量） | 全局锁竞争 | 单 P 调度局部性 |
| P 与 M 解耦 | syscall 阻塞不拖累调度 | handoff 与回绑的开销 |
| mcache 绑定 P | 小对象分配无锁 | handoff 时需清空 mcache |
| g0 调度栈 | 调度逻辑不污染用户栈 | 切换开销（mcall/gogo） |
| work stealing | P 间负载均衡 | spinning M 的协调复杂度 |
| 异步抢占 | CPU 密集 G 不让出 | 信号开销、安全点约束 |
| netpoll | 网络 IO 不占线程 | 集成复杂、平台差异 |

GMP 调度主循环（`runtime.schedule` → `findRunnable` → `execute`）的简化全景：

```go
// 不可编译，仅说明逻辑
func findRunnable() *g {
	mp := getg().m
top:
	// 1. 本地队列
	if gp, _ := runqget(mp.p.ptr()); gp != nil {
		return gp
	}
	// 2. 全局队列（每 61 次强取）
	if gp := globrunqget(mp.p.ptr(), 0); gp != nil {
		return gp
	}
	// 3. netpoll（非阻塞模式，看有没有就绪 G）
	if netpollinited() && (sched.lastpoll != 0 || listEmpty(&netpollwaiters)) {
		if list := netpoll(0); !list.empty() {
			gp := list.pop()
			injectglist(list)
			casgstatus(gp, _Gwaiting, _Grunnable)
			return gp
		}
	}
	// 4. work stealing：偷其他 P 的 runq
	if mp.spinning || nmspinning.Load() == 0 {
		if !mp.spinning {
			mp.spinning = true
			atomic.Xadd(&sched.nmspinning, 1)
		}
		if gp, _ := runqsteal(mp.p.ptr(), randomP, true); gp != nil {
			return gp
		}
	}
	// 5. 都没有：解除 spinning，尝试再走一遍
	stopm() // M 休眠到 sched.midle
	goto top
}
```

> **要点**：`findRunnable` 是 GMP 协同的"心脏"。它把本地队列、全局队列、netpoll、work stealing 串成一条优先级链。理解这条链，就理解了 GMP 为什么"既快又公平"。

#### 3. 工程实践与常见坑

- **GMP 不会自动均衡长任务**：若一个 goroutine 跑纯 CPU 死循环（Go 1.14 前的 `for {}`），会卡死所在 P，其他 P 救不了它。Go 1.14+ 异步抢占解决了 CPU 死循环，但**不抢占持锁或正在 cgo 的 G**——这些仍是"卡死"高发区。
- **STW 时所有 P 进入 `_PgcStop`**：GC 标记阶段需要 STW 启动时，调度器会 `stopTheWorld`，把所有 P 切到 `_PgcStop`，M 解绑进入 `midle`。若你的程序对 STW 敏感，关注 `GOGC` 与 `GOMEMLIMIT`。
- **`GOMAXPROCS=1` 仍能并发**：因为 IO/syscall/netpoll 会 handoff P。但纯 CPU 并行需要 `GOMAXPROCS>1`。
- **trace 是观察 GMP 的最佳工具**：`go tool trace trace.out` 能可视化每个 P 的时间线、M 的状态、G 的等待原因。线上偶发卡顿排查的第一选择。

---

### work stealing

#### 1. 是什么

Work stealing（工作窃取）是 Go 调度器实现 **P 间负载均衡** 的算法。当某个 P 的本地运行队列空了，它会去"偷"其他 P 队列里一半的 G 过来执行，从而避免"有的 P 闲死、有的 P 累死"。

#### 2. 底层实现要点

Work stealing 的核心函数是 `runtime.runqsteal`，被 `findRunnable` 调用。关键设计：

- **偷一半，不是偷一个**：从被偷 P 的 `runq` 里取 `runqtail - runqhead` 的一半，转移到自己 P 的 `runq`。这是为了减少偷的频率（偷本身有 cache miss 代价）。
- **`runnext` 不会被偷**：`runnext` 是"刚唤醒、最该立刻跑"的槽，偷时优先跳过它（除非 `stealRunNextG=true` 且被偷 P 还很忙）。
- **随机选起点，遍历一圈**：从随机 P 开始，依次尝试所有 P，避免热点 P 被所有空闲 P 同时围攻。
- **spinning 协调**：M 进入 spinning 状态前会原子地增加 `sched.nmspinning`。这保证"全局只要有 spinning M，新 G 就不会没人理"。当 spinning M 偷到活或失败转入休眠时，相应减计数。

简化伪代码：

```go
// 不可编译，仅说明逻辑
func runqsteal(p, p2 *p, stealRunNextG bool) (*g, bool) {
	// 1. 尝试偷 p2.runq 的一半
	t := p2.runqtail
	h := atomic.LoadAcq(&p2.runqhead)
	n := int32(t - h)
	n = n - n/2 // 偷一半
	for i := uint32(0); i < uint32(n); i++ {
		g := p2.runq[(h+i)%uint32(len(p2.runq))]
		// 转移到 p.runq
		runqput(p, g, false)
	}
	// 2. 若没偷到且允许，尝试偷 runnext
	if n == 0 && stealRunNextG {
		if next := p2.runnext; next != 0 {
			return next.ptr(), true
		}
	}
	return nil, false
}

// findRunnable 中的调用
func stealWork(mp *m) *g {
	pp := mp.p.ptr()
	for i := 0; i < 4; i++ { // 4 轮尝试
		// 随机起点，遍历所有 P
		start := fastrand() % uint32(len(allp))
		for enum := 0; enum < len(allp); enum++ {
			p2 := allp[(start+uint32(enum))%uint32(len(allp))]
			if p2 == pp { continue }
			if gp, _ := runqsteal(pp, p2, true); gp != nil {
				return gp
			}
			// 也偷 p2 的 gFree（复用 G 结构）
			if gp := runqsteal_gfree(pp, p2); gp != nil {
				// 转 _Grunnable 返回
			}
		}
	}
	return nil
}
```

spinning 协调的核心：**避免过度自旋也避免欠自旋**。

- 若 `nmspinning == 0` 且有空闲 P：新 G 入队时调用 `wakep()`，启动一个 spinning M。
- spinning M 偷到活后，自己解除 spinning 并继续执行；同时若仍有空闲 P 且全局队列非空，再 `wakep()` 一个。
- spinning M 偷 4 轮失败后，解除 spinning 并 `stopm()` 休眠到 `midle`。

> **要点**：`nmspinning` 是全局计数器，所有 P 共享。它是 work stealing 的"心跳"——新增 G 时检查它，决定要不要唤醒 M；M 偷活失败时更新它，决定要不要休眠。这个协调机制让 Go 既不过度自旋浪费 CPU，又不让 G 等太久。

#### 3. 工程实践与常见坑

- **死循环 goroutine 会让 work stealing 失效**：Go 1.14 前，纯 CPU 死循环的 G 永不让出 P，其他 P 偷不到它，也偷不到它的"队列"（因为队列空）。这就是为什么早期 Go 程序里 `for {}` 会让程序"看起来卡死"。Go 1.14+ 异步抢占解决了这一点。
- **`GOMAXPROCS=1` 无 work stealing**：只有一个 P，无偷可施。
- **监控 spinning**：`runtime/metrics` 提供 `/sched/goroutines:goroutines` 等指标；pprof 的 sched profile 可看 spinning 比例。spinning 比例持续高说明 P 间不均衡（常见于某些 P 上的 G 长期阻塞）。
- **不要靠 work stealing 保证公平**：偷一半的设计在"短任务 + 长 burst"场景下可能让某些 G 排队较久。强公平需求用 channel 或 `runtime.Gosched` 主动让出。

---

### 抢占调度

#### 1. 是什么

抢占调度（preemption）是调度器在 **goroutine 不主动让出** 时，强制打断它、让 P/M 资源给其他 G 执行的机制。Go 的抢占经历了协作式（cooperative）到异步式（asynchronous）的演进。

#### 2. 演进与底层实现要点

**Go 1.13 及之前：协作式抢占（基于栈检查）**

- 实现方式：`sysmon` 检测到某 G 运行超过 10ms，调用 `preemptone` 设置 `g.stackguard0 = stackPreempt`（`0xfffffade`）。
- 触发时机：被抢占 G 在 **下一次函数调用的序言** 比较 SP 与 stackguard0，发现"栈不够"，跳到 `runtime.morestack`，在那里检查到 `stackPreempt` 标记，转而调用 `runtime.newstack` → `gopreempt_m` → 让出 P。
- 致命缺陷：**没有函数调用的死循环无法被抢占**。`for { x++ }` 这种纯计算且无函数调用的代码会永久卡死所在 P，GC、其他 G 全部饿死。

**Go 1.14+：异步抢占（基于信号 SIGURG）**

- 实现方式：`sysmon` 检测到长运行 G，调用 `preemptM` 向目标 M 发送 `SIGURG` 信号。
- 信号处理：M 的信号处理器 `sigtrampgo` 识别为抢占请求，调用 `doSigPreempt` → `asyncPreempt`（汇编函数），在被打断指令的安全点伪造一个对 `mcall(asyncPreempt)` 的调用，进入 `asyncPreempt2` → `gopreempt_m` → 重新调度。
- 安全约束：并非所有指令点都可抢占。Runtime 持锁、`atomic` 操作、不可重入的运行时内部代码段会标记 `preemptoff` 或在不可抢占区间（`wantAsyncPreempt` 控制）。若信号到达时不在安全点，会延迟到下个安全点。

简化流程：

```
sysmon (独立 M，无 P)
   |
   | 检测到 G 运行 > 10ms
   v
preemptone(mp)        // 设置 mp.curg.preempt=true，g.stackguard0=stackPreempt
   |
   v
preemptM(mp)          // tgkill/mp.sendSignal 发送 SIGURG
   |
   v
目标 M 收到 SIGURG
   |
   v
sigtrampgo -> doSigPreempt
   |
   v
asyncPreempt (汇编)   // 在被打断处伪造 mcall(asyncPreempt)
   |
   v
asyncPreempt2 -> mcall(gopreempt_m)
   |
   v
gopreempt_m           // G 状态 -> _Grunnable，放回全局队列
   |
   v
schedule()            // 调度下一个 G
```

关键源码片段（简化）：

```go
// 不可编译，仅说明逻辑
func preemptone(mp *m) bool {
	pp := mp.p.ptr()
	gp := mp.curg
	if gp == nil || gp == mp.g0 {
		return false
	}
	gp.preempt = true
	// 协作式抢占标记（仍保留，作为兜底）
	atomic.Storeuintptr(&gp.stackguard0, stackPreempt)
	// 异步抢占请求
	if preemptMSupported && mp.signalPending.CompareAndSwap(0, _SigPreempt) {
		signalM(mp, sigPreempt)
	}
	return true
}
```

异步抢占的安全点由编译器与 runtime 共同维护：

- 编译器在每个函数插入"异步抢占是否允许"的元数据（PCRange 表）。
- 运行时在不可抢占段设置 `mp.preemptoff` 字符串（如 `"GC mark assist"`），信号到达时检测到非空则推迟。

> **要点**：异步抢占是 Go 1.14 最重要的 runtime 改进之一。它彻底解决了"CPU 密集 goroutine 卡死调度器"的历史问题，让 GC STW 时间、调度延迟在死循环场景下从"无限"降到毫秒级。代价是信号机制的复杂性与安全点维护的开销。

#### 3. 工程实践与常见坑

- **Go 1.14 之前不要写无函数调用的死循环**。即便你已用 1.21+，仍要警惕 cgo 段不可抢占——cgo 调用进入 C 代码后，Go 完全失去控制，`SIGURG` 无法在 C 内部触发 Go 调度。长时间 cgo 调用仍会卡住 M（虽然 P 已 handoff）。
- **`runtime.GOMAXPROCS(1)` + 死循环在 1.14 前必死，1.14 后可恢复**。但恢复有延迟（sysmon 10ms 巡检 + 信号处理开销），对延迟敏感的场景仍应主动 `runtime.Gosched()`。
- **不可抢占段会导致抢占延迟**：持 `runtime` 内部锁、`reflect` 某些操作、`runtime.LockOSThread` 的 G，异步抢占会推迟。极端情况下会出现"sysmon 已请求抢占但 G 还在跑几秒"——排查方向是 pprof 看该 G 是否在 cgo 或 reflect。
- **`debug.SetGCPercent` 与抢占的关系**：GC STW 依赖抢占把所有 P 停在安全点。若 G 长期不可抢占，STW 时间会拉长，表现为 `gc pause` 飙升。
- **不要依赖抢占实现"时间片轮转"**：抢占的目的是防止饿死，不是公平分片。需要确定性时间片的应用（如仿真、游戏循环）应自己用 channel 或 timer 切片。

---

### syscall

#### 1. 是什么

这里讨论的是 Go 调度器如何处理 **阻塞型系统调用**（read/write/open/select 等真正陷入内核、可能阻塞 M 的调用）。网络 IO 走 netpoll 旁路（见下一节），本节聚焦普通 syscall。syscall 处理是 GMP 模型"IO 不阻塞调度"承诺的关键组成。

#### 2. 底层实现要点

GMP 处理 syscall 的核心是 **P 与 M 解绑**：

- **进入 syscall**（`runtime.entersyscall`）：当前 G 状态切到 `_Gsyscall`，M 的 P 状态切到 `_Psyscall`，M 与 P **暂时解绑**。但 P 不会被立即转交——只有当 sysmon 检测到该 syscall 持续超过 20µs（`retake` 阈值）时，才会把 P 强制 handoff 到 `pidle`，让其他 M 接手。
- **退出 syscall**（`runtime.exitsyscall`）：尝试 fast path——若原 P 仍在 `_Psyscall` 且未被他 M 取走，直接重绑原 P 继续。否则 slow path——尝试获取任意空闲 P；都没有则把 G 放回全局队列，M 进入休眠。

简化状态机：

```
   G 在 _Grunning，M 持有 P
            |
            | entersyscall (用户代码 syscall 调用 -> runtime.entersyscall)
            v
   G -> _Gsyscall, P -> _Psyscall, M 与 P "松绑"
            |
            +----< fast path (>20µs 内返回) >----+
            |                                   |
            | exitsyscall                       | sysmon retake 检测超时
            | 原 P 仍 _Psyscall                 | handoffP: P -> _Pidle
            | 直接重绑原 P                       | 唤醒/新建 M2 接手 P
            | G -> _Grunning                    |
            v                                   v
   G 继续在原 M/P 上跑            slow path:
                                  exitsyscall 时原 P 已被抢
                                  尝试获取任意空闲 P：
                                    - 拿到: 绑定新 P，继续跑
                                    - 拿不到: G 入全局队列, M 休眠
```

关键源码简化：

```go
// 不可编译，仅说明逻辑
func entersyscall() {
	mp := getg().m
	gp := mp.curg
	casgstatus(gp, _Grunning, _Gsyscall)
	mp.m.syscalltick = pp.syscalltick
	pp.syscalltick++
	pp.m = 0           // P 与 M 解绑（逻辑上）
	mp.p = 0
	pp.m = 0
	atomic.Store(&pp.status, _Psyscall)
	// 注意：此时并未立刻释放 P 给别人，等 sysmon 判断
}

func exitsyscall() {
	mp := getg().m
	gp := mp.curg
	oldp := mp.oldp.ptr()
	// fast path: 原 P 还在 _Psyscall 且未被抢
	if exitsyscallfast(oldp) {
		casgstatus(gp, _Gsyscall, _Grunning)
		return
	}
	// slow path
	casgstatus(gp, _Gsyscall, _Grunnable)
	// 尝试获取任意空闲 P
	p := pidleget()
	if p != nil {
		acquirep(p)
		execute(gp)
	}
	// 没有 P：G 入全局队列，M 休眠
	globrunqput(gp)
	stopm()
	schedule()
}
```

sysmon 的 `retake` 是 syscall handoff 的触发器：

```go
// 不可编译，仅说明逻辑
func retake(now int64) uint32 {
	for _, pp := range allp {
		if pp.status == _Psyscall && pp.syscalltick == ... {
			// 超过 20µs 还在 syscall
			if int64(pp.syscalltick) != oldTick || now-syscalltime > 10*1000*1000 {
				handoffp(pp) // P -> _Pidle, 唤醒新 M
			}
		}
		if pp.status == _Prunning && preemptone(pp.m) {
			// 长时间运行的 G，请求异步抢占
		}
	}
}
```

> **要点**：syscall 的 "fast path / slow path" 设计是 Go 调度器吞吐的关键。短 syscall 不触发 P 迁移，开销极低；长 syscall 才 handoff，避免 M 被内核阻塞拖累。20µs 是经验阈值——足够区分"真阻塞"与"瞬时陷入"。

#### 3. 工程实践与常见坑

- **文件 IO 是 syscall，不走 netpoll**：`os.File.Read` 在 Linux 上是 `read` 系统调用，M 会真正阻塞在内核（文件 fd 不支持 epoll）。高频文件 IO + 大量 goroutine 会导致 M 暴涨。解决：用线程池封装（如 `aio`、`io_uring`），或限制并发。
- **`time.Sleep` 不占 M**：sleep 由 runtime timer 管理器维护，goroutine 进入 `_Gwaiting`，不阻塞 M。误以为 sleep 占线程是常见误解。
- **cgo 调用走类似 syscall 路径**：`cgocall` 会 `entersyscall`-like 的 handoff P，但**异步抢占对 cgo 段无效**。长时间 cgo 是 M 卡住的高发原因。
- **监控 M 阻塞**：`pprof threadcreate` + `trace` 的 syscall 段。若 `syscalltick` 增长快而 `schedtick` 不动，说明该 P 上的 G 大量时间花在 syscall。
- **`runtime.SetMutexProfileFraction` 与 syscall 无关**：但很多"卡住"问题最后定位是 syscall 阻塞（DNS 解析、文件锁、`syscall.Read` 慢盘），别只盯着 mutex。

---

### netpoll

#### 1. 是什么

netpoll 是 Go Runtime 内置的 **网络事件轮询器**，封装了平台 IO 多路复用（Linux `epoll`、macOS `kqueue`、Windows `IOCP`、Solaris `port`）。它让 goroutine 在等待网络 IO 时**不占用 M**——goroutine 进入 `_Gwaiting`，M 继续跑别的 G；当 fd 就绪时，netpoll 把对应 G 重新放回运行队列。

这是 Go 写出"百万连接"服务端的理论基础：每个连接一个 goroutine，但阻塞的 goroutine 不消耗线程。

#### 2. 底层实现要点

netpoll 的核心数据结构与流程：

- **`pollDesc`**：每个被 netpoll 接管的 fd 关联一个 `pollDesc`，记录等待该 fd 的 G（读写各一个）、fd、状态等。
- **`netpoll` 函数**：调用 `epoll_wait`/`kevent` 取就绪事件，把对应 `pollDesc` 上的 G 收集成链表返回。
- **集成点**：
  - `findRunnable`：M 找不到活时，调用 `netpoll(0)`（非阻塞）取就绪 G 注入运行队列。
  - sysmon：周期性 `netpoll(0)` 把就绪 G 投递到全局队列。
  - `startTheWorld` / GC 结束：调用 `netpoll(0)` 把积压的就绪 G 投递。
  - `pollWork`：在 `runtime.poll` 路径上，若无 P 空闲，调用 `netpollBreak` 唤醒阻塞中的 `netpoll(-1)`。

简化调用链（以 Linux epoll 为例）：

```
G 调 conn.Read
   |
   v
runtime.pollfd → netpollblock(pd, mode, wait)
   |
   | 当前 G 进入 _Gwaiting, waitreason="IO wait"
   | gopark 把 G 移出运行队列，M 跑别的 G
   v
   ... 等数据到达 ...

(epoll 中数据就绪)
   |
   v
sysmon / findRunnable 调用 netpoll(0)
   |
   v
epoll_wait → 取出就绪 fd 的 pollDesc
   |
   v
netpollready → 把 pd.rg/waiting G 状态切 _Grunnable
   |
   v
runqput 注入运行队列
   |
   v
M 调度执行该 G，Read 返回数据
```

`netpoll` 平台实现差异：

| 平台 | 系统调用 | 文件描述符类型 |
|------|----------|----------------|
| Linux | `epoll_create1` / `epoll_ctl` / `epoll_wait` | 任意 fd（管道、socket、eventfd） |
| macOS / FreeBSD | `kqueue` / `kevent` | socket、pipe（普通文件不支持） |
| Windows | `IOCP` (`GetQueuedCompletionStatus`) | socket（基于 WSAEventSelect） |
| Solaris / illumos | `port_getn` | 任意 fd |

简化伪代码：

```go
// 不可编译，仅说明逻辑
func netpoll(block bool) gList {
	if !netpollinited() { return gList{} }
	// 调用平台特定函数（epoll_wait/kevent）
	events := epoll_wait(epfd, -1 if block else 0)
	var toRun gList
	for _, ev := range events {
		pd := ev.pollDesc
		// 读就绪
		if ev.events&EPOLLIN != 0 {
			if rg := pd.rg; rg != nil {
				toRun.push(rg)        // 把等待读的 G 收集
				pd.rg = nil
			}
		}
		// 写就绪
		if ev.events&EPOLLOUT != 0 {
			if wg := pd.wg; wg != nil {
				toRun.push(wg)
				pd.wg = nil
			}
		}
	}
	return toRun
}
```

`netpollBreak` 用于唤醒阻塞中的 `netpoll(-1)`：

- 每个平台有一个专用的"唤醒 fd"（Linux 是 eventfd，macOS 是 pipe）。
- 当有 G 被唤醒需要被调度（`netpollWake`），Runtime 写这个 fd，让 `epoll_wait` 立即返回，从而 M 能及时处理新就绪事件。

netpoll 与调度器的协作要点：

1. **goroutine 等 IO 时不占 M**：`gopark` 把 G 切到 `_Gwaiting`，`waitreason="IO wait"`，M 立即回到 `schedule()` 跑别的 G。
2. **就绪 G 不会立即跑**：`netpoll` 把就绪 G 注入运行队列后，仍需等 M 调度到它。
3. **netpoll 在 `findRunnable` 中是"二级优先"**：本地队列、全局队列优先；只有它们空了才查 netpoll。这是为了减少 epoll_wait 调用次数。
4. **sysmon 兜底**：即使没有 M 进入 `findRunnable`（所有 M 都在忙），sysmon 也会每 20µs 检查 netpoll，避免就绪 G 等太久。

> **要点**：netpoll 是 Go 网络栈性能的基石。它把"阻塞 IO"从"线程阻塞"降级为"goroutine 等待"，使单进程支撑百万连接成为可能。但 netpoll 只对**网络 fd**有效——普通文件 IO 仍走 syscall 路径阻塞 M。

#### 3. 工程实践与常见坑

- **`os.File` 不走 netpoll**：磁盘文件、命名管道（在 Linux 上普通文件 fd）不支持 epoll。`os.File.Read` 是真阻塞 syscall，会 handoff P。**这是"goroutine 读文件卡住整个服务"问题的根因**。解决：文件 IO 用 `bufio` + 限制并发，或用专用线程池。
- **`net.Conn.SetDeadline` 必须设置**：netpoll 不会自动超时。不设 deadline 的连接若对端永不响应，goroutine 永远 `_Gwaiting`，泄漏。`http.Server` 默认有 read/write timeout，但 `net.Dial` 默认无超时——务必 `net.DialTimeout` 或 `Dialer.Timeout`。
- **`netpoll` 不支持 `poll`/`select` 兼容的所有 fd**：如 Linux 的 inotify、某些字符设备，行为依平台而定。需要时可回退到 `syscall` + 线程池。
- **DNS 解析器选择影响 netpoll**：`net.Resolver` 有 `PreferGo` 选项。Go 原生解析器走 netpoll（不阻塞 M）；cgo 解析器走 `getaddrinfo` syscall（阻塞 M）。容器环境 DNS 慢时，cgo 解析器会拖垮 M 池——建议设 `GODEBUG=netdns=go` 强制走 netpoll。
- **Windows netpoll 限制**：Windows 仅 socket 走 IOCP，pipe 等仍阻塞。Windows 上跑高并发服务要特别小心，生产环境推荐 Linux。
- **观察 netpoll 状态**：`pprof goroutine` 看 `waitreason="IO wait"` 的 G 数量。若持续高，要么流量大（正常），要么有连接不响应（异常，查 deadline 与对端）。

---

### 本章小结

本章系统地拆解了 Go 调度器的核心：

1. **Go Scheduler** 是用户态 M:N 调度器，全局状态在 `schedt`，核心循环 `schedule()` → `findRunnable()` → `execute()`。
2. **M** 是 OS 线程抽象，持有 `g0`（调度栈）与 `curg`（用户 G），通过 `g0` 与 `curg` 的切换实现调度。M 是"重"资源，受 `maxmcount` 限制。
3. **P** 是逻辑处理器，承载本地 runq（256 容量）、mcache、defer/sudog 池。数量 = `GOMAXPROCS`，决定并行度。状态机 `_Pidle`/`_Prunning`/`_Psyscall`/`_PgcStop`/`_Pdead` 是 GMP 协同的骨架。
4. **G** 是 goroutine 的运行时表示，2KB 起步可增长栈、`gobuf` 调度现场、状态机 `_Gidle`/`_Grunnable`/`_Grunning`/`_Gsyscall`/`_Gwaiting`/`_Gdead`。创建走 `gfget` 复用池，成本接近函数调用。
5. **GMP 模型** 把三者组合：M 必须绑 P 才能跑 G；P 本地队列优先，全局队列兜底；netpoll/syscall 让 IO 不阻塞调度。
6. **work stealing** 通过 P 间偷半数 G 实现负载均衡，`nmspinning` 协调自旋与休眠，避免过度自旋也避免 G 等待。
7. **抢占调度** 经历协作式（1.13 前，基于 stackguard0）到异步式（1.14+，基于 SIGURG）的演进，解决了 CPU 密集 G 卡死调度器的问题，但 cgo/持锁段仍不可抢占。
8. **syscall** 通过 P handoff 实现"IO 不阻塞调度"：短 syscall fast path 不迁 P，长 syscall slow path handoff 给新 M，20µs 是判定阈值。文件 IO 不走 netpoll，会真阻塞 M。
9. **netpoll** 封装 epoll/kqueue/IOCP，让网络 IO 的 goroutine 进入 `_Gwaiting` 不占 M，是百万连接的基础。但只对网络 fd 有效，文件 IO 与 cgo DNS 解析仍走阻塞 syscall 路径。

掌握本章后，你应能：

- 读懂 `runtime/schedule.go`、`runtime/proc.go` 的关键路径；
- 解释 `pprof goroutine` 里 `waitreason` 与 `trace` 里 P/M/G 时间线；
- 诊断 M 暴涨、goroutine 泄漏、STW 过长、netpoll 失效等典型问题；
- 合理设置 `GOMAXPROCS`、`maxmcount`、`GOGC`、`netdns=go` 等运行时参数。

下一章 [第11章 Channel](./11-Channel.md) 将进入 goroutine 间通信的世界——channel 的底层 hchan 结构、send/recv 状态机、与调度器的 `gopark`/`goready` 联动，是本章调度机制的直接应用。
