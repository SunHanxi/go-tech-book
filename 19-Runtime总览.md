## 第19章 Runtime 总览

> 引言：Go 程序看似"原生可执行"，实际上每个二进制里都内嵌了一个庞大的运行时（runtime）。Runtime 负责 goroutine 调度、内存分配、垃圾回收、栈管理、网络 I/O 与系统调用拦截。本章是 Runtime 部分的"地图"，后续 [第20章 内存管理](./20-内存管理.md)、[第21章 GC](./21-GC.md)、[第12章 Goroutine](./12-Goroutine.md) 等都会基于本章的脉络展开。

### Runtime 做什么

**1. 是什么**

Go 的 runtime 是一段与用户代码静态链接在一起的库（不是 JVM 那样的独立虚拟机），它在程序启动时被自动初始化，并在程序整个生命周期里持续运行。它对外提供两类能力：

- 对用户代码透明的"基础设施"：goroutine 调度、栈自动扩缩容、并发垃圾回收、内存分配、网络 poller、系统调用封装、信号处理、time/timer、map/slice/channel 等内置类型的部分实现。
- 显式 API：`runtime`、`runtime/debug`、`runtime/metrics`、`runtime/pprof`、`runtime/trace` 包暴露的函数，例如 `runtime.Gosched()`、`runtime.GC()`、`runtime.LockOSThread()`。

可以把它理解为一个"嵌入式的微内核"：用户写的 `func main()` 其实只是被 runtime 调用的一个普通 goroutine。

**2. 为什么这样设计 / 实现要点**

与 C/C++、Java 相比，Go 选择把 runtime 编译进二进制的几个关键动机：

| 设计选择 | 收益 | 代价 |
| --- | --- | --- |
| 静态链接 runtime | 部署只有一个文件，无依赖 | 二进制偏大（几 MB 起步） |
| 内嵌调度器 | goroutine 可在用户态切换，开销 ~200ns | 不能轻易"绑核"做实时调度 |
| 内嵌 GC | 内存安全，免手动 free | 有 STW 与后台 CPU 占用 |
| 运行时管理栈 | 栈可按需扩缩，goroutine 起步 2KB | 需要 stack copy，对 CGO 不友好 |
| 编译器 + runtime 协作 | 逃逸分析、写屏障、抢占点都由编译器插入 | 源码层面强耦合 `cmd/compile` 与 `runtime` |

runtime 的源码主要在 `src/runtime/` 下，关键文件大致分工如下：

```
runtime/
├── runtime2.go        // G/M/P 核心结构体定义
├── proc.go            // 调度器：schedule(), findRunnable(), sysmon
├── mheap.go           // 全局堆 mheap
├── mcache.go          // 每个 P 的本地缓存
├── mcentral.go        // 每个 span class 的中央缓存
├── mspan.go           // span 结构与操作
├── malloc.go          // mallocgc 入口
├── gc.go / gcwork.go  // GC 状态机
├── mgc.go             // GC 阶段调度
├── netpoll_*.go       // 网络 poller（epoll/kqueue/IOCP）
├── time_*.go          // timer 实现
├── stack.go           // 栈分配与复制
├── signal_*.go        // 信号处理
└── asm_amd64.s        // 汇编入口（rt0_go、switch、gogo）
```

> 关键认识：Go 编译器在生成机器码时，会在很多地方插入对 runtime 的调用——分配对象时插 `runtime.mallocgc`，函数序言插抢占检查 `runtime.morestack`，写指针插写屏障 `runtime.gcWriteBarrier`。没有编译器配合，runtime 没法独立完成这些事。

**3. 工程实践与常见坑**

观察 runtime 行为是排查性能问题的第一步：

```go
package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

func main() {
	// 设置 GOMAXPROCS，默认等于逻辑 CPU 数
	runtime.GOMAXPROCS(runtime.NumCPU())

	// 主动触发 GC，常用于基准测试
	runtime.GC()

	// 打印内存分配统计
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("Alloc=%v MiB, NumGC=%d, PauseNs(total)=%v\n",
		m.Alloc/1024/1024, m.NumGC, m.PauseTotalNs)

	// 设置软内存上限（Go 1.19+），常用于容器
	debug.SetMemoryLimit(1 << 30) // 1 GiB
}
```

常见坑：

- **`runtime.GOMAXPROCS(1)` 不等于串行**：网络 I/O 与系统调用会让出的 P 仍可被其他 M 拿走跑别的 goroutine。在容器中 `GOMAXPROCS` 默认等于逻辑 CPU 数，但 cgroup 限制并不影响这个值——Go 1.21 仍未自动适配 cgroup，需用 `go.uber.org/automaxprocs` 这类库。
- **`runtime.LockOSThread` 与 goroutine 不对等**：调用后该 goroutine 永远绑在当前 OS 线程上，但 OS 线程在 goroutine 结束前不能复用。忘记 Unlock 会造成线程泄漏。
- **`runtime.Caller` / `runtime.Stack` 有开销**：在高频路径里抓栈会显著拖慢，性能敏感处应使用 `runtime.Callers` 直接拿 PC 数组。
- **`GODEBUG` 调试**：`GODEBUG=schedtrace=1000,scheddetail=1` 可每秒打印调度器状态；`gctrace=1` 打印每次 GC 概要。生产排查必备。

---

### 启动流程

**1. 是什么**

执行 `./mybin` 到 `func main()` 真正运行之间，runtime 要完成大量初始化：建立 TLS、解析命令行参数、初始化调度器、分配栈、启动 sysmon 与 GC、把用户入口包装成 goroutine 并运行。理解启动流程有助于解释一些"反直觉"现象：为什么 `init()` 在 `main()` 之前？为什么 `runtime.main` 不是用户写的 `main`？

**2. 底层实现要点**

以 Linux/amd64 为例（不同平台文件名不同，流程一致）：

1. **入口**：ELF 入口指向 `runtime.rt0_linux_amd64`（汇编），它设置 SP 后跳到 `_rt0_amd64_linux` → `runtime.rt0_go`。
2. **`runtime.rt0_go`**（`asm_amd64.s`）：负责读取 argc/argv/envp，建立 TLS（`m0` 的 `tls` 字段），调用 `runtime.settls`、`runtime.osinit`、`runtime.schedinit`，最后 `mstart` 跑起 `m0`。
3. **`runtime.schedinit`**（`proc.go`）：
   - 解析 `GODEBUG`、`GOMAXPROCS`、`GOGC`、`GOMEMLIMIT` 等环境变量。
   - 初始化全局 `sched` 结构（`schedt`）。
   - 创建 `allp` 数组，给每个 P 分配 `mcache`。
   - 初始化 mheap、defer 池、type allocator、写屏障。
4. **创建主 goroutine**：`runtime.rt0_go` 调 `runtime.mainPC = runtime.main`，并用 `runtime.newproc` 创建第一个 goroutine。
5. **`runtime.mstart0`** → `mstart1` → `schedule()`：调度器从 `m0` 上启动，找到可运行的 G（就是刚创建的主 goroutine），`gogo` 切到它。
6. **`runtime.main`**（`proc.go`）：在主 goroutine 上运行，依次做：
   - `runtime.main_init_done` 信号量初始化；
   - 启动 `sysmon` 后台线程（不是普通 M，是独立的 OS 线程）；
   - 启动 `forcegc` goroutine；
   - 调用 `runtime_init`（编译器生成的函数，包级 `init()` 全部跑在这里）；
   - `main_init_done` 释放信号；
   - 调用 `main_main`（用户 `main`，由 `//go:linkname` 链接到）；
   - 退出整个进程。

简化伪代码：

```go
package main

//go:linkname main_main main.main
func main_main()

func main() {
	g := getg()

	// 设置主 goroutine 标识
	g.m.g0.m = g.m

	// 启动 sysmon（独立 OS 线程，不受 P 数量限制）
	systemstack(func() {
		newm(sysmon, nil, -1)
	})

	// 确保 m0 被绑定
	lockOSThread()

	// 包级 init()：用户包 init 顺序由依赖图决定
	fn := main_init
	fn()

	close(main_init_done)

	// 用户 main
	if IsLibrary || IsArchive {
		return
	}
	fn = main_main
	fn()

	// 退出
	exit(0)
}
```

> 注意：`runtime.main` 末尾不会 return，而是直接 `exit(0)`。这就是为什么 `main` 函数即使没显式 `os.Exit` 也不会"返回到调用者"——调用者是 runtime，进程直接被终结。

**3. 工程实践与常见坑**

- **`init()` 的执行顺序**：依赖图决定，同包内按文件名升序、文件内按声明顺序。跨包：被导入的包先 `init`。可以利用这点做"插件式注册"，但不要在 `init` 里做重活（网络、磁盘 I/O）——它阻塞整个启动。
- **`sync.Once` vs `init`**：能用 `init` 就别用 `Once`，`init` 对编译器和 CPU 友好（无原子开销）。
- **`//go:linkname` 的使用**：访问 runtime 私有函数需要它，但属于"使用未导出 API"，Go 团队不保证兼容，升级版本可能失效。比如 `runtime.nanotime` 等已被移到 `runtime/sys_*`。
- **冷启动延迟排查**：用 `runtime/trace` 看启动期各 `init()` 耗时；`pprof` 的 CPU profile 也可以从启动开始抓。
- **`GODEBUG=inittrace=1`**（Go 1.20+）会打印每个包 `init` 的耗时与分配，是定位启动慢的利器。

---

### Scheduler

**1. 是什么**

Go 调度器负责把 goroutine（G）映射到 OS 线程（M）上执行，并在线程因系统调用阻塞时把 M 与 P 解绑、让别的 M 接手 P 继续跑其他 G。其核心是 **G-M-P 模型**：

- **G**（goroutine）：用户级协程，包含栈、状态机、调度信息。
- **M**（machine）：OS 线程，由 runtime 创建/回收，真正执行 G 的载体。
- **P**（processor）：逻辑处理器，持有一组可运行 G 的本地队列和本地 mcache。`GOMAXPROCS` 决定 P 的数量。M 必须绑定一个 P 才能执行 G。

调度器策略：本地队列优先、全局队列兜底、work stealing、网络 poller、sysmon 抢占。

**2. 底层实现要点**

G/M/P 在 `runtime2.go` 中定义（Go 1.21+，简化后保留关键字段）：

```go
package runtime

type g struct {
	stack       stack       // 当前栈的 [lo, hi)
	stackguard0 uintptr     // 栈溢出检查哨兵；序言里比较 SP 与之
	m           *m          // 当前绑定的 M
	sched       gobuf       // 上下文：PC、SP、g 自己
	atomicstatus uint32     // _Gidle/_Grunnable/_Grunning/_Gsyscall/_Gwaiting...
	goid        int64       // goroutine id
	waitsince   int64       // 阻塞起始时间（用于 trace)
	lockedm     muintptr    // LockOSThread 后绑定的 M
	preemptrun  uint8       // 异步抢占请求
	// ...
}

type m struct {
	g0          *g          // 调度栈专用 g，runtime 代码运行在它上面
	curg        *g          // 当前在跑的用户 g
	p           puintptr    // 绑定的 P
	nextp       puintptr    // 解绑时下一个 P
	oldp        puintptr    // 系统调用前的 P
	mstartfn    func()      // m 启动函数（如 sysmon）
	spinning    bool        // 正在自旋找活
	lockedg     *g          // LockOSThread 反向引用
	tls         [6]uintptr  // thread-local storage
	// ...
}

type p struct {
	id          int32
	status      uint32      // _Pidle/_Prunning/_Psyscall/_Pgcstop/_Pdead
	m           muintptr    // 绑定的 M
	runqhead    uint32      // 本地队列头
	runqtail    uint32      // 本地队列尾
	runq        [256]guintptr // 本地队列：固定 256 槽，环形
	runnext     guintptr    // 高优先级槽，下次直接跑
	gFree struct {          // 死亡 g 的复用池
		gList
		n int32
	}
	mcache      *mcache     // 本地内存缓存（详见第20章）
	timers      []*timer    // 本地 timer 堆（Go 1.14+ 每 P 一个）
	gcBgMarkWorker guintptr // 后台标记 worker
	gcw         gcWork      // GC 工作缓冲
	// ...
}

type schedt struct {
	gFree struct {          // 全局空闲 g 池
		lock    mutex
		stack   gList
		noStack gList
		n       int32
	}
	midle  muintptr         // 空闲 M 链表
	nmidle int32
	mnext  int64            // 下一个 M 的 id
	pidle  puintptr         // 空闲 P 链表
	npidle int32
	runq     gQueue         // 全局可运行 G 队列
	runqsize int32
	// ...
}
```

调度主循环 `schedule()`（`proc.go`，简化伪代码）：

```go
package main

func schedule() {
top:
	mp := getg().m
	pp := mp.p.ptr()

	var gp *g
	// 1) 每 61 次调度看一眼全局队列，避免饥饿
	if pp.schedtick%61 == 0 && sched.runqsize > 0 {
		gp = globrunqget(pp, 1)
	}
	// 2) 本地 runnext
	if gp == nil {
		gp, _ = runqget(pp)
	}
	// 3) findRunnable：阻塞找——本地/全局队列、netpoll、steal、GC、sysmon
	if gp == nil {
		gp = findRunnable()
	}

	execute(gp)
	// execute 内部 gogo 切到 gp，gp 跑完 yield 再回到 schedule
	goto top
}
```

`findRunnable` 是调度器最复杂的函数，按以下顺序找活（简化）：

1. 本地队列、全局队列；
2. 网络 poller（非阻塞模式）；
3. **work stealing**：随机选一个 P，偷它本地队列的一半；
4. 如果都空，去检查 GC 是否需要 worker；
5. 还没有 → 释放 P、把自己挂到 `midle`、`notesleep` 等被唤醒。

**抢占机制**有两类：

- **协作式（Go 1.13 及之前主要靠）**：函数序言里 `cmp SP, g.stackguard0`，若栈不够或被设为 `0xffffffffffff` 触发 `morestack` → 检查 `preempt` 标志 → 让出。**纯计算无函数调用的 goroutine 永不主动让出**，是早期经典坑。
- **异步抢占（Go 1.14+）**：`sysmon` 检测到 G 运行超过 10ms，向该 M 发信号（`SIGURG`），信号处理里强制插入抢占点，安全修改 PC 跳到 `runtime.asyncPreempt`。前提是寄存器上下文可以安全保存（部分 CGO 调用中会跳过）。

调度全景图：

```
              ┌────────────────────────────────────────────┐
              │              sched (全局)                   │
              │   runq (全局 G)  midle  pidle  gFree        │
              └────────────────────────────────────────────┘
                                ▲ steal / 兜底
        ┌───────────────────────┼───────────────────────┐
        │                       │                       │
   ┌────┴────┐             ┌────┴────┐             ┌────┴────┐
   │  P 0    │             │  P 1    │  ...        │  P n    │   GOMAXPROCS
   │ runq[256│             │ runq[256│             │ runq[256│
   │ mcache  │             │ mcache  │             │ mcache  │
   │ timers  │             │ timers  │             │ timers  │
   └────┬────┘             └────┬────┘             └────┬────┘
        │ bind                   │ bind                   │ bind
   ┌────┴────┐             ┌────┴────┐             ┌────┴────┐
   │   M0    │             │   M1    │  ...        │   Mn    │   OS 线程
   │ (OS T)  │             │ (OS T)  │             │ (OS T)  │
   └────┬────┘             └────┬────┘             └────┬────┘
        │ curg                  │ curg                  │ curg
     ┌──┴──┐                 ┌──┴──┐                 ┌──┴──┐
     │ G a │                 │ G c │                 │ G e │
     └─────┘                 └─────┘                 └─────┘
```

**3. 工程实践与常见坑**

- **`GOMAXPROCS` 调优**：CPU 密集型任务设为核数即可；如果是混合 I/O，可以略大（如 `*1.25`），但太多会让 P 之间互偷开销变大、cache 命中率下降。容器环境强烈推荐 `go.uber.org/automaxprocs`，否则 Go 1.21 仍会读到宿主机核数。
- **goroutine 泄漏排查**：`runtime.NumGoroutine()` 在监控里埋点；`pprof goroutine` 抓全栈，`debug=2` 模式带 `goroutine state`，能一眼看出 `_Gwaiting` 在哪个 channel。
- **`GODEBUG=schedtrace=1000`**：每秒输出形如 `SCHED 0ms: gomaxprocs=4 idleprocs=0 threads=8 spinningthreads=1 idlethreads=4 runqueue=0 [0 0 0 0]`。`[0 0 0 0]` 是各 P 本地队列长度，长时间不均衡或 spinningthreads 异常高说明负载不均。
- **`runtime.GOMAXPROCS(1)` 不保证并发安全**：它只限制并发执行的 P，不限制 GC、sysmon、网络 poller。把 `GOMAXPROCS(1)` 当作"race detector 替身"是危险的。
- **网络 I/O 不占 P**：epoll 就绪后 netpoller 把 G 重新放回 P 的本地队列，所以高并发 echo server 即使 `GOMAXPROCS=1` 也能扛数千连接。
- **同步阻塞系统调用会让 M 让出 P**：如 `Read(file)`。这是 Go 推 `non-blocking + poller` 设计的根本原因，文件 I/O 无法走 netpoll（Linux 上仍用线程池）。

更多细节见 [第12章 Goroutine](./12-Goroutine.md)。

---

### GC

**1. 是什么**

Go 使用**并发三色标记-清除垃圾回收器**（concurrent tri-color mark-sweep）。它在 Go 1.5 后变为并发，1.8 后 STW 通常 < 1ms，1.14 引入页堆 allocator 的无锁路径，1.19 引入 `GOMEMLIMIT` 软限制，1.21 进一步优化大对象与扫描。其目标不是极限吞吐，而是**低尾延迟**。

**2. 底层实现要点**

三色抽象：

- **白**：未访问，回收候选。
- **灰**：已访问，但其指向的对象还没扫描完。
- **黑**：自身与所有出边都扫描完，本回合安全。

GC 状态机（`runtime/mgc.go` 的 `gcStart`/`gcMarkDone`/`gcSweep`）大致阶段：

| 阶段 | 是否 STW | 做什么 |
| --- | --- | --- |
| Sweep Termination | 极短 STW | 关闭上轮 sweep，准备标记 |
| Mark Setup | STW | 启动所有 P 的写屏障 |
| Mark (并发) | 否 | 后台 worker 与用户代码并发，扫描从根出发的对象图 |
| Mark Termination | STW（亚毫秒） | 清空工作缓冲、关写屏障、统计 |
| Sweep (并发) | 否 | 把未标记的 mspan 还给 heap |

**写屏障**：并发标记期间，用户代码可能修改指针破坏三色不变式（黑→白插边）。Go 用 **Yuasa-style 删除屏障 + Dijkstra 插入屏障** 的混合方案（Go 1.8 起，叫 **hybrid write barrier**）。简化伪代码：

```go
package main

import "unsafe"

//go:nosplit
func gcWriteBarrierptr(slot *unsafe.Pointer, ptr unsafe.Pointer) {
	shade(*slot) // 删除侧：被覆盖的旧值染灰（Yuasa）
	shade(ptr)   // 插入侧：写入的新值染灰（Dijkstra）
	*slot = ptr
}
```

这样即使栈不重扫（栈对象改写不进屏障），也能保证三色安全——大幅压低 Mark Termination 的 STW。

GC 触发条件：

1. **堆增长**：下次 GC 目标 = 上次存活堆 × `(1 + GOGC/100)`。`GOGC=100`（默认）即堆翻倍触发。设 `GOGC=off` 关闭。
2. **时间触发**：`runtime.forcegc` goroutine 每 2 分钟强制一次。
3. **手动**：`runtime.GC()`。
4. **GOMEMLIMIT**（1.19+）：达到软上限即使没到 GOGC 目标也触发，类似 Java 的 soft limit。`GOMEMLIMIT=off` 关闭。

控制反馈：runtime 会根据上次 GC 的总耗时（用户 CPU + GC CPU）动态调整触发时机，把 GC CPU 占比收敛到 ~25%（即 `GOGC=100` 时的目标）。

**3. 工程实践与常见坑**

- **`GODEBUG=gctrace=1`**：每次 GC 输出形如 `gc 1 @0.045s 1%: 0.013+0.36+0.022 ms clock, 0.026+0.17+0.40 ms cpu, 4->4->2 MB, 5 MB goal, 0 MB stacks, 0 MB globals, 4 P`。重点关注：
  - 中间百分比（1%）：GC CPU 占比；
  - 后两个堆数字（4->4->2 MB）：GC 前活堆 → GC 后活堆 → 当前活堆；
  - `goal`：下次触发目标。
- **降分配**：每次分配都会让堆更快达到 goal，提前触发 GC。`sync.Pool`、`bytes.Buffer` 复用是首要手段；`pprof allocs` 找热点。
- **大对象直接进堆**：>32KB 走 mheap，没有 mcache 缓冲，频繁分配大对象会让 GC mark/sweep 都吃力。
- **`debug.SetGCPercent(-1)` 不等于关 GC**：仅是"不到堆翻倍就别触发"，但 GOMEMLIMIT、forcegc、显式 `runtime.GC()` 仍会触发。
- **`GOMEMLIMIT` 在容器里的意义**：把它设成 cgroup 内存上限的 ~90%，能避免 OOM Kill 引起整进程崩溃；但太接近真实使用会让 GC 自适应变得激进。
- **不要 `runtime.GC()` 来"清理"**：除非做基准测试或测试 leak。生产里频繁手动 GC 反而打乱自适应反馈。
- **指针 vs 值**：`[]byte` 的 backing array 不含指针，扫描成本低；`[]*Foo` 让 GC 跟踪每个元素。编译器根据类型选择 noscan 的 span class，分配和扫描都更快。

更多细节见 [第21章 GC](./21-GC.md)。

---

### 内存管理

**1. 是什么**

Go 的堆分配器借鉴 TCMalloc，采用 **多级缓存 + 多级中心化** 设计：每个 P 持有 `mcache`（无锁快路径）→ 全局按 size class 持有 `mcentral`（带锁中路径）→ 全局 `mheap` 管理所有 page（慢路径）。再叠加 **Tiny Allocator**（<16B 无指针小对象合并）、**栈分配**（逃逸分析决定）、**mmap arena**（按 64MB 切大块）等机制。

**2. 底层实现要点（速览）**

对象按 size class 分配。Go 1.21 共 67 个 span class（每个 8B 起步），加上 noscan 变体共 136 个：

| Size class | 元素大小 | 一次 span 元素数 | span 页数 |
| --- | --- | --- | --- |
| 1 | 8 B | 8192 | 1 |
| 2 | 16 B | 4096 | 1 |
| 3 | 24 B | 2730 | 1 |
| ... | ... | ... | ... |
| 67 | 32768 B | 8 | 8 |

每页 8KB。`mspan` 把若干连续页打包，按 size class 提供定长对象槽。

多级缓存全景图：

```
                   用户 mallocgc(size)
                          │
                ┌─────────┴──────────┐
                │  逃逸？  size<=16B  │
                │  否 → 栈分配         │
                │  是 → 堆分配         │
                └─────────┬──────────┘
                          │
              ┌───────────┴────────────┐
              │ P.mcache (无锁快路径)    │
              │  tiny / alloc[sizeclass]│
              └───────────┬────────────┘
                  miss →  │ refill
              ┌───────────┴────────────┐
              │ mcentral[136] (每 class)│
              │  partial / full 链表    │  ← 锁
              └───────────┬────────────┘
                  miss →  │ grow
              ┌───────────┴────────────┐
              │ mheap (page 分配)        │
              │  arena / treap / busy   │
              └────────────────────────┘
                          │
                  mmap (arena 64MB / 64位)
```

`mheap` 把地址空间切成 **arena**（64 位下 64MB 一块），arena 内按 8KB page 索引。分配大对象（>32KB）直接走 mheap，从 `free` 树里找连续 page。空闲 page 用 **treap**（笛卡尔树，按 page 起始地址与随机优先级组织）维护，方便合并相邻页。

> 关键文件：`src/runtime/mheap.go`、`mcache.go`、`mcentral.go`、`mspan.go`、`malloc.go`、`sizeclasses.go`（67 类定义）、`mheap_*.go`（按平台）。详见 [第20章 内存管理](./20-内存管理.md)。

**3. 工程实践与常见坑**

- **逃逸分析**：`go build -gcflags='-m'` 看哪些变量逃逸。常见逃逸源：返回局部变量指针、闭包捕获、`interface{}` 参数（编译期未知大小）、`[]byte(s)` 转换。少逃逸 = 多栈分配 = 0 GC 压力。
- **栈分配 ≠ 永远不付出代价**：栈太大会 `morestack` 触发栈拷贝（goroutine 栈按需扩展，最大 1GB）。
- **`make([]T, n)` 的代价**：n 较大且 T 含指针时，分配 + 初始化 + 后续 GC 扫描都不便宜；能用 `sync.Pool` 就别每次 make。
- **`unsafe.Sizeof` vs `runtime.KeepAlive`**：让对象看似逃逸但实际栈分配有时会引入 use-after-free 隐患（CGO 场景），靠 `runtime.KeepAlive` 兜底。
- **不要 `unsafe.Pointer` 跨 goroutine 传堆地址给"未逃逸"对象**：逃逸分析是函数级的，跨函数边界后编译器可能错过。这是"看似安全实则悬挂指针"的典型坑。
- **mcache 与 P 强绑定**：被 `LockOSThread` 的 goroutine 仍然在某个 P 上跑，mcache 来自该 P；`GOMAXPROCS=1` 时只有一份 mcache，所有分配都串行化同一缓存——压测时这一点会让某些无锁优化失效。

更多细节见 [第20章 内存管理](./20-内存管理.md)。

---

### 本章小结

- Runtime 是一段与用户代码静态链接的"嵌入式微内核"，覆盖调度、内存、GC、栈、网络、信号、timer 等基础设施，由编译器在每处插入的调用与屏障协同工作。
- 启动从汇编入口 `rt0_go` 出发，经 `schedinit` 初始化全局 sched、mheap、P 池与 mcache，再用 `newproc` 创建主 goroutine，最终在 `runtime.main` 里跑包级 `init` 和用户 `main`。
- 调度器以 G-M-P 模型组织，本地队列优先 + 全局队列兜底 + work stealing + 网络 poller + sysmon 异步抢占，把 goroutine 切换开销压到 ~200ns 级。
- GC 是并发三色标记-清除 + 混合写屏障，靠 `GOGC`/`GOMEMLIMIT` 控制触发，目标是低尾延迟而非极限吞吐。
- 内存分配走 TCMalloc 风格的 mcache → mcentral → mheap 三级路径，小对象走 size class、大对象直接走 mheap，逃逸分析决定栈/堆。

掌握本章的"地图"后，[第20章 内存管理](./20-内存管理.md) 将深入每一级缓存的数据结构，[第21章 GC](./21-GC.md) 与 [第12章 Goroutine](./12-Goroutine.md) 会各自展开实现细节。
