## 第13章 Channel（重点）

> 引言：Channel 是 Go 并发模型的灵魂。它源自 Hoare 提出的 CSP（Communicating Sequential Processes）思想——"不要通过共享内存来通信，而要通过通信来共享内存"。本章从 `hchan` 的运行时结构出发，逐层剖析有缓冲/无缓冲 channel 的收发流程、`close` 与 `range` 的语义，并落到工程中的最佳实践与常见坑。读懂本章，是理解下一章 [select](./14-select.md) 的基础。

---

### Channel 原理

#### 1. 是什么

Channel 是 Go 语言提供的一等公民（first-class citizen）类型，用于在 goroutine 之间传递数据与同步。它的字面量语法是 `chan T`，可以通过 `make` 创建：

```go
ch := make(chan int, 3)   // 有缓冲，容量 3
ch := make(chan int)      // 无缓冲
```

Channel 支持三个核心操作：发送 `ch <- v`、接收 `v := <-ch`、关闭 `close(ch)`。它本身是**并发安全**的——多个 goroutine 可以同时对同一个 channel 收发，不会产生数据竞争，无需额外的锁。

#### 2. 为什么这样设计：CSP 模型与底层数据结构

Go 的并发哲学写在 [Effective Go](https://go.dev/doc/effective_go) 的开篇：

> Do not communicate by sharing memory; instead, share memory by communicating.

这句话对应两种并发风格：

| 风格 | 代表语言 | 同步手段 | Go 中的体现 |
|------|----------|----------|-------------|
| 共享内存 + 锁 | C/C++/Java | `mutex`、`condition variable` | `sync.Mutex`、`sync.WaitGroup` |
| 消息传递 | Erlang/Go | channel、actor mailbox | `chan`、`select` |

Go 不是非此即彼，而是**以 channel 为主、锁为辅**。channel 把"传递数据"和"同步"合二为一：一次 `<-` 操作既传值又隐含 happened-before 关系，比手工加锁更难写错。

底层实现上，每个 channel 都是一个 `hchan` 结构体（位于 `runtime/chan.go`），它包含：

- 一个**互斥锁** `lock`，保证并发安全；
- 一个**环形缓冲区** `buf`（无缓冲时为空）；
- 两个**等待队列** `sendq` / `recvq`，分别保存因发送/接收而阻塞的 goroutine（用 `sudog` 包装）；
- 元素类型、元素大小、关闭标志等元信息。

所有的 `<-` 操作都走 runtime 函数：`runtime.chansend1` / `runtime.chanrecv1`，它们会先加 `hchan.lock`，再根据缓冲区状态决定是直接完成、还是把当前 goroutine 挂起（`gopark`）。被挂起的 goroutine 通过 `sudog` 挂到等待队列上，等对端操作时由 `goready` 唤醒。

整个收发流程可以简化为下图：

```
        ┌──────────────────── hchan ─────────────────────┐
        │  lock                                          │
        │  ┌──── buf (ring buffer) ──────┐               │
        │  │ [0][1][2] ... dataqsiz-1    │               │
        │  │   ↑recvx       ↑sendx       │               │
        │  └─────────────────────────────┘               │
        │  qcount  / dataqsiz / closed                   │
        │  recvq ──► sudog ─► sudog ─► nil  (等待接收)   │
        │  sendq ──► sudog ─► sudog ─► nil  (等待发送)   │
        └────────────────────────────────────────────────┘

send 路径:  chansend -> lock -> [buf 未满? 写 buf : 入 sendq & gopark] -> unlock
recv 路径:  chanrecv -> lock -> [buf 非空? 读 buf : sendq 有? 直接拿 : 入 recvq & gopark] -> unlock
```

#### 3. 工程实践与常见坑

**何时用 channel，何时用锁？**

- 数据在 goroutine 间**流动、传递所有权** → 用 channel（pipeline、fan-out、结果汇总）。
- 保护一段**共享状态的临界区**（如缓存、计数器） → 用 `sync.Mutex`。
- 信号通知、done/cancel → channel（`close(ch)` 广播）或 `context.Context`。

**常见坑：**

1. **goroutine 泄漏**：向一个无人接收的**无缓冲** channel 发送，或向已满 channel 发送而无人接收，goroutine 永久阻塞。建议配合 `context` 或带超时的 `select`。
2. **向已关闭 channel 发送** → panic: `send on closed channel`。关闭责任应交给**唯一的发送方**。
3. **重复关闭** → panic: `close of closed channel`。可用 `sync.Once` 或方向受限 channel 规避。
4. **nil channel 的妙用**：向 nil channel 收发会**永久阻塞**，在 `select` 中用 nil case 可"动态禁用"某个分支（见第14章）。

> 经验法则：channel 的所有者（创建者 / 唯一发送方）负责关闭；接收方永远不要关闭。这条规则能消除 90% 的 channel 关闭 panic。

---

### hchan

#### 1. 是什么

`hchan` 是 channel 在运行时的"真身"。你写的 `chan int` 在编译期是一个 `*hchan` 指针——`make(chan int, n)` 实际上调用了 `runtime.makechan`，分配并初始化一个 `hchan` 结构。所有 `<-` 操作最终都转化为对这块内存的读写。

#### 2. 底层数据结构（Go 1.21+，`runtime/chan.go`）

下面是简化后的关键结构（省略了 GC 相关字段）：

```go
type hchan struct {
    qcount   uint           // 当前 buf 中元素个数
    dataqsiz uint           // buf 的容量（环形数组长度）
    buf      unsafe.Pointer // 指向环形数组首元素
    elemsize uint16         // 单个元素大小（字节）
    closed   uint32         // 是否已关闭，0=未关闭，1=已关闭
    elemtype *_type         // 元素类型指针
    sendx    uint           // 下一次发送写入 buf 的下标
    recvx    uint           // 下一次接收读取 buf 的下标
    recvq    waitq          // 等待接收的 sudog 队列
    sendq    waitq          // 等待发送的 sudog 队列
    lock     mutex          // 保护上述所有字段的互斥锁
}

// waitq 是一个双向链表
type waitq struct {
    first *sudog
    last  *sudog
}

// sudog 是 goroutine 在等待队列中的"票据"，包装了 g 和数据地址
type sudog struct {
    g     *g             // 被阻塞的 goroutine
    next  *sudog         // 链表后继
    prev  *sudog         // 链表前驱
    elem  unsafe.Pointer // 数据地址（发送：源；接收：目标）
    isSelect bool        // 是否处于 select 场景
    success  bool        // 唤醒后是否成功完成操作
    c     *hchan         // 所属 channel
    // ... 其余字段用于 GC 与 debug
}
```

**逐字段解释：**

| 字段 | 作用 |
|------|------|
| `qcount` | buf 里现在有几个元素。`len(ch)` 直接返回它。 |
| `dataqsiz` | buf 容量。`cap(ch)` 返回它。`make(chan T, n)` 的 n 即此处。无缓冲 channel 为 0。 |
| `buf` | 指向 `dataqsiz * elemsize` 大小的数组。无缓冲时为 `nil`。 |
| `elemsize` | 元素字节数，用于 `memmove` 拷贝。`chan struct{}` 时为 0，零拷贝。 |
| `closed` | 关闭标志。原子读，但写入受 `lock` 保护。 |
| `elemtype` | 元素类型，用于拷贝时的边界检查与 GC 扫描。 |
| `sendx` / `recvx` | 环形缓冲的写/读游标，每次操作后对 `dataqsiz` 取模前进。 |
| `recvq` / `sendq` | 因"接收不到"或"发不出去"而阻塞的 goroutine 链表，FIFO。 |
| `lock` | 自旋锁（`runtime.mutex`），保护所有字段。channel 慢就慢在这把锁——高频收发会成为瓶颈。 |

`sudog` 的关键字段：

- `g`：被挂起的 goroutine 指针，`goready(s.g)` 用来唤醒它。
- `elem`：数据缓冲地址。**发送**时指向待发送变量；**接收**时指向接收变量。无缓冲 channel 直接在两个 goroutine 的栈之间通过 `elem` 做 `memmove`，绕过 buf。
- `isSelect`：标识该 sudog 是否参与 `select`。select 唤醒时需要让"未中标"的 channel 把 sudog 从队列摘除。
- `success`：唤醒后用以区分是"真正完成收发"还是"被 select 的另一个分支抢先"。

**内存布局示意：**

```
make(chan int, 3) 产生：

   ch ──► ┌──────── hchan ────────────┐
          │ qcount=0  dataqsiz=3      │
          │ elemsize=8 elemtype=int   │
          │ closed=0                  │
          │ sendx=0  recvx=0          │
          │ recvq={nil,nil}           │
          │ sendq={nil,nil}           │
          │ lock ──┐                  │
          └────────┼──────────────────┘
                   │
   buf ────────────┴──► [ 0 ][ 0 ][ 0 ]   (3 个 int 槽位)
                         ^sendx,recvx 都从 0 开始
```

#### 3. 工程实践与常见坑

- **`chan struct{}` 是零成本信号**：`elemsize=0`，`buf` 不分配，`memmove` 跳过。适合做"事件通知 / done channel"。
- **`len(ch)` / `cap(ch)` 是 O(1)**：直接读 `qcount` / `dataqsiz`，但只是**瞬时快照**，不要用它做同步判断（如 `if len(ch) > 0 { <-ch }` 仍可能有竞争，应直接 `<-ch` 或用 `select`+`default`）。
- **大 channel 是性能杀手**：`dataqsiz` 很大时一次性分配大块内存。若用 channel 做任务队列，考虑用切片 + `sync.Cond` 或第三方 ring buffer。
- **channel 不是免费的锁**：每次收发要加 `hchan.lock` + 可能的 `memmove` + 可能的 goroutine 调度。高频路径上，`sync.Mutex` 保护一个 slice 通常更快。

---

### 有缓冲 Channel

#### 1. 是什么

有缓冲 channel 在创建时指定容量 `n > 0`：

```go
ch := make(chan int, 3)
```

它的语义是**异步**的：发送方在 buf 未满时**不阻塞**，直接把值丢进 buf 就返回；接收方在 buf 非空时也能立即取走。只有当 buf **满了**发送方才阻塞，buf **空了**接收方才阻塞。

#### 2. 底层实现：环形缓冲区

有缓冲 channel 的核心是 `buf` 指向的环形数组，配合 `sendx` / `recvx` 两个游标：

```
dataqsiz = 5, qcount = 3 (buf 中有 3 个元素)

buf:  [ A ][ B ][ C ][ . ][ . ]
        ^                ^
        recvx=0          sendx=3
        (下次从这里读)    (下次从这里写)

接收一次: 读 buf[recvx]=A, recvx=(0+1)%5=1, qcount=2
         buf:  [ A ][ B ][ C ][ . ][ . ]   (A 仍在内存但已"出队")
                   ^recvx=1

发送一次 D: 写 buf[sendx]=D, sendx=(3+1)%5=4, qcount=3
         buf:  [ A ][ B ][ C ][ D ][ . ]
                              ^recvx=1   ^sendx=4

再发送 E: 写 buf[sendx]=E, sendx=(4+1)%5=0, qcount=4
         buf:  [ E ][ B ][ C ][ D ][ . ]   ← sendx 绕回头部
         ^sendx=0    ^recvx=1
```

**`chansend` 的核心逻辑（简化伪代码）：**

```go
func chansend(c *hchan, ep unsafe.Pointer, block bool) bool {
    lock(&c.lock)
    if c.closed != 0 {
        unlock(&c.lock)
        panic("send on closed channel")
    }
    // 1. 优先：有接收者在等 → 直接把数据拷给接收者，绕过 buf
    if sg := c.recvq.dequeue(); sg != nil {
        sendDirect(c.elemtype, sg, ep)
        unlock(&c.lock)
        goready(sg.g)            // 唤醒接收者
        return true
    }
    // 2. buf 未满 → 写 buf
    if c.qcount < c.dataqsiz {
        qp := chanbuf(c, c.sendx)
        typedmemmove(c.elemtype, qp, ep)
        c.sendx++
        if c.sendx == c.dataqsiz { c.sendx = 0 }
        c.qcount++
        unlock(&c.lock)
        return true
    }
    // 3. buf 满了 → 非阻塞模式直接返回 false；阻塞模式入 sendq & gopark
    if !block {
        unlock(&c.lock)
        return false
    }
    gp := getg()
    mysg := acquireSudog()
    mysg.g = gp
    mysg.elem = ep
    mysg.c = c
    c.sendq.enqueue(mysg)
    gopark(chanparkcommit, ...)   // 当前 goroutine 挂起
    // 被唤醒后从这里继续
    releaseSudog(mysg)
    return true
}
```

> 注意第 1 步的优化：**即使 buf 有空间，只要 `recvq` 上有人在等，就跳过 buf 直接把数据递到接收者手上**。这避免了"先写 buf 再从 buf 读"的双重拷贝。

**`chanrecv` 的对称逻辑：**

```go
func chanrecv(c *hchan, ep unsafe.Pointer, block bool) (received bool) {
    lock(&c.lock)
    // 0. channel 已关闭且 buf 空 → 返回零值
    if c.closed != 0 && c.qcount == 0 {
        unlock(&c.lock)
        if ep != nil { typedmemclr(c.elemtype, ep) }
        return false
    }
    // 1. 有发送者在等 → 直接从发送者那里拿（无缓冲）或从 buf 拿并让发送者补位
    if sg := c.sendq.dequeue(); sg != nil {
        recv(c, sg, ep)
        unlock(&c.lock)
        goready(sg.g)
        return true
    }
    // 2. buf 非空 → 读 buf
    if c.qcount > 0 {
        qp := chanbuf(c, c.recvx)
        typedmemmove(c.elemtype, ep, qp)
        c.recvx++
        if c.recvx == c.dataqsiz { c.recvx = 0 }
        c.qcount--
        unlock(&c.lock)
        return true
    }
    // 3. buf 空 → 入 recvq & gopark
    // ... 类似 chansend
}
```

#### 3. 工程实践与常见坑

- **缓冲大小是工程权衡**：太小 → 高频场景容易阻塞；太大 → 内存占用高、且会"延迟"对背压的感知。常见经验值是 `1` 或 `2`，真正需要削峰填谷时再加大。
- **有缓冲 ≠ 解耦**：很多人以为"加了缓冲就不会阻塞"，错。buf 一旦满了发送方照样阻塞。要做真正的解耦，配合 `select`+`default` 或 `context` 主动放弃。
- **FIFO 但不保证强实时**：channel 保证元素**进入 buf 的顺序 = 离开 buf 的顺序**，但不保证"发送返回"和"接收方处理完"的时序——这是异步语义。
- **不要用 `len(ch) == cap(ch)` 做判断**：仍是瞬时快照，多 goroutine 下不可靠。

```go
// 反面教材
if len(ch) < cap(ch) {
    ch <- v   // 仍可能阻塞：别的 goroutine 抢先塞进来了
}

// 正确做法
select {
case ch <- v:
default:
    // 队列满，降级处理
}
```

---

### 无缓冲 Channel

#### 1. 是什么

无缓冲 channel 创建时不带容量：

```go
ch := make(chan int)   // 等价于 make(chan int, 0)
```

它的语义是**同步 rendezvous（会合）**：发送方和接收方必须"同时在场"才能完成这次传递。一次 `ch <- v` 在有接收方准备好之前**绝不返回**；一次 `<-ch` 在有发送方准备好之前也**绝不返回**。可以理解为一次**同步握手**。

#### 2. 底层实现：直接 goroutine-to-goroutine 拷贝

无缓冲 channel 的 `dataqsiz=0`，`buf=nil`，所以**永远不经过 buf**。数据直接从一个 goroutine 的栈拷贝到另一个 goroutine 的栈。

**发送路径（无接收者时）：**

```
goroutine A: ch <- 42
   ┌────────────┐                    ┌────────────┐
   │  g:A       │   buf=nil          │  recvq=nil │
   │  elem=&v   │                    │  sendq=[]  │
   └────────────┘                    └────────────┘
   1. lock; 2. recvq 空、buf 空 → 创建 sudog{g:A, elem:&v}
   3. sendq.enqueue(sudog); 4. gopark(A)  ← A 挂起
```

**接收方出现时：**

```
goroutine B: x := <-ch
   ┌────────────┐   1. lock                           ┌────────────┐
   │  g:B       │   2. 发现 sendq 有 A 的 sudog       │  sendq=[A] │
   │  elem=&x   │   3. memmove(x, A.elem)  ← 直接拷贝  │            │
   └────────────┘   4. goready(A) 唤醒 A               └────────────┘
                    5. B 直接返回，无需 gopark
```

关键点：

- 数据 `42` 从 A 的栈变量 `v` **直接 memmove** 到 B 的栈变量 `x`，**没有中间 buf**。
- B 拿到锁后，A 已经 `gopark`，但 A 的 `sudog.elem=&v` 仍指向 A 栈上的 `v`——只要 A 没被唤醒、栈没销毁，这个地址就有效。这正是 `gopark` 的作用：冻结 goroutine 状态。
- B 唤醒 A 后，A 从 `chansend` 中 `gopark` 之后的那行继续执行，释放 sudog 并返回。

**happened-before 关系：**

无缓冲 channel 建立强同步：`ch <- v` 的"完成"在 `<-ch` 的"完成"之前。因此 `v` 的写入对接收方**完全可见**，无需额外同步。

```
A: v = 42; ch <- v;        // A 写 v 在 send 之前
B: x := <-ch; print(x);    // B 接收在读 x 之前，且能看到 A 对 v 的写
```

#### 3. 工程实践与常见坑

- **无缓冲 channel = 同步原语**：常用于"等对方完成"的握手，如：

```go
done := make(chan struct{})
go func() {
    // 工作...
    done <- struct{}{}   // 等主 goroutine 准备好接收
}()
<-done                    // 阻塞直到 worker 完成
```

- **主 goroutine 直接 `go f()` 后立刻 `<-ch` 会卡住 worker**：如果主 goroutine 还没准备好接收，worker 在 `ch <- v` 处阻塞，看似"无限快"的 worker 也跑不起来。
- **`chan struct{}` 是最优无缓冲信号**：零拷贝、零内存，纯同步语义。
- **不要把无缓冲 channel 当队列用**：它没有"暂存"能力，任何一方先到都得等。需要暂存就用有缓冲。
- **deadlock 经典坑**：

```go
func main() {
    ch := make(chan int)
    ch <- 1          // 主 goroutine 阻塞，没人接收 → fatal error: all goroutines are asleep - deadlock!
    fmt.Println(<-ch)
}
```

修复：先 `go func() { <-ch }()`，或改成 `make(chan int, 1)`。

---

### close()

#### 1. 是什么

`close(ch)` 把 channel 标记为"不再有数据发送"。它有两个直接后果：

1. 后续的所有**接收**会立即返回：先把 buf 里剩余元素按 FIFO 消费完，之后返回**零值**，且 `v, ok := <-ch` 的 `ok` 为 `false`。
2. 后续的任何**发送**都会 panic：`send on closed channel`。

```go
ch := make(chan int, 2)
ch <- 1
ch <- 2
close(ch)
fmt.Println(<-ch)      // 1
fmt.Println(<-ch)      // 2
v, ok := <-ch          // v=0, ok=false   ← buf 空了，返回零值
```

#### 2. 底层实现：`runtime.closechan`

```go
func closechan(c *hchan) {
    if c == nil {
        panic("close of nil channel")     // nil channel 不能 close
    }
    lock(&c.lock)
    if c.closed != 0 {
        unlock(&c.lock)
        panic("close of closed channel")  // 重复 close panic
    }
    c.closed = 1                          // 置位关闭标志

    var glist gList
    // 1. 唤醒所有接收等待者：他们都会得到零值 + ok=false
    for {
        sg := c.recvq.dequeue()
        if sg == nil { break }
        sg.elem = nil                     // 标记：收到的是零值
        gp := sg.g
        gp.param = nil
        glist.push(gp)
    }
    // 2. 唤醒所有发送等待者：他们会被 panic
    for {
        sg := c.sendq.dequeue()
        if sg == nil { break }
        sg.elem = nil
        gp := sg.g
        gp.param = nil
        glist.push(gp)
    }
    unlock(&c.lock)

    // 3. 统一 goready 所有挂起的 goroutine
    for !glist.empty() {
        gp := glist.pop()
        gp.schedlink = 0
        goready(gp)                       // 发送者唤醒后会 panic
    }
}
```

关键设计：

- **置 `closed=1` 在锁内**：与 `chansend` 的 `c.closed != 0` 检查互斥，避免 close 与 send 竞争。
- **批量唤醒**：所有等待者先收集到 `glist`，释放锁后再 `goready`，缩短锁持有时间。
- **发送等待者也会被唤醒**：但它们的 `chansend` 在 `gopark` 返回后会发现 `c.closed != 0`，于是 panic——这就是"向已关闭 channel 发送会 panic"的运行时根源。
- **接收者得到零值**：`close` 时如果 `recvq` 上有人，他们的 `sudog.elem` 被置 nil，唤醒后 `chanrecv` 走零值路径。

#### 3. 工程实践与常见坑

**三大 panic 场景：**

| 操作 | 条件 | 结果 |
|------|------|------|
| `close(ch)` | ch 已关闭 | `close of closed channel` |
| `close(ch)` | ch 是 nil | `close of nil channel` |
| `ch <- v` | ch 已关闭 | `send on closed channel` |

**关闭责任与安全关闭模式：**

原则：**只有发送方关闭，且只关闭一次**。具体落地有三种模式：

**模式 1：方向限制（推荐）**

```go
package main

import "fmt"

// producer 只拿到发送方向，外部拿到接收方向
func producer(out chan<- int) {
    defer close(out)      // 唯一发送方负责关闭
    for i := 0; i < 3; i++ {
        out <- i
    }
}

func main() {
    ch := make(chan int)
    go producer(ch)
    for v := range ch {   // 接收方用 range 自动检测关闭
        fmt.Println(v)
    }
}
```

`chan<- int` 让 producer 内部无法接收、外部无法发送，关闭责任唯一明确。

**模式 2：`sync.Once`**

```go
var once sync.Once
ch := make(chan int)

func shutdown() {
    once.Do(func() { close(ch) })
}
```

适合多个 goroutine 都可能触发关闭的场景。

**模式 3：额外的 done channel / context**

不直接 close 数据 channel，而是用一个独立的 `done` 信号通知所有发送方停止发送：

```go
ctx, cancel := context.WithCancel(context.Background())
go func() {
    for {
        select {
        case <-ctx.Done():
            return
        case ch <- produce():
        }
    }
}()
// 取消时调用 cancel()，发送方自行退出，再由所有者关闭 ch
```

**关闭 nil channel 的妙用**：在 `select` 中把一个 channel 变量置 `nil`，对应 case 会永久阻塞（即"禁用"该分支），常用于"处理完一类事件后不再处理"的状态机。详见第14章。

---

### range

#### 1. 是什么

`for v := range ch` 是遍历 channel 的语法糖。它会**不断接收**直到 channel **被关闭且 buf 排空**才退出循环：

```go
for v := range ch {
    fmt.Println(v)
}
// 等价于
for {
    v, ok := <-ch
    if !ok {        // channel 关闭且 buf 空
        break
    }
    fmt.Println(v)
}
```

#### 2. 底层实现

`for range chan` 在编译期被改写为调用 `runtime.chanrecv2`（带 `ok` 返回值的版本）。每次迭代：

1. 调用 `chanrecv`，传入接收变量地址；
2. 若 `chanrecv` 返回 `false`（channel 已关闭且 buf 空）→ 跳出循环；
3. 若返回 `true` → 执行循环体，回到步骤 1。

注意 `chanrecv` 在 channel 关闭后会**先消费完 buf 里的剩余元素**，每消费一个返回 `true`，buf 空了才返回 `false`。所以 `range` 不会丢数据。

```
channel 状态: closed=1, buf=[A, B, C]

range 第 1 次: chanrecv -> 读 A, 返回 true  → 循环体
range 第 2 次: chanrecv -> 读 B, 返回 true  → 循环体
range 第 3 次: chanrecv -> 读 C, 返回 true  → 循环体
range 第 4 次: chanrecv -> closed && buf 空 -> 返回 false → break
```

#### 3. 工程实践与常见坑

- **必须有发送方关闭**：`for range ch` 在 channel 永不关闭时会**永久阻塞**最后一条 `chanrecv`，导致 goroutine 泄漏。生产者完成后必须 `close(ch)`。
- **不要在接收方 close**：`range` 的接收方不知道发送方何时停，强求关闭会引入竞争。
- **range 不会消费 nil channel**：`for v := range nilCh` 永久阻塞（与 `<-nil` 一致）。
- **break 只跳出 range**：在 `select` 内的 `break` 跳不出外层 `for range`，需要标签：

```go
outer:
    for v := range ch {
        select {
        case <-stop:
            break outer      // 用标签跳出外层
        default:
            process(v)
        }
    }
```

- **range 一个有缓冲 channel 时关闭后仍能读出残留数据**：这是特性不是 bug，确保不丢消息。但要小心：如果发送方在 `close` 前 buf 里还有 N 条未消费，接收方的 `range` 会先消费这 N 条再退出。

---

### select

#### 1. 是什么

`select` 是 Go 中处理**多个 channel 操作**的控制结构，类似 `switch`，但每个 `case` 必须是 channel 的发送或接收：

```go
select {
case v := <-ch1:
    fmt.Println("from ch1:", v)
case ch2 <- 42:
    fmt.Println("sent to ch2")
case <-time.After(time.Second):
    fmt.Println("timeout")
default:
    fmt.Println("no channel ready")
}
```

它的语义：

- **阻塞**直到至少一个 case 就绪（除非有 `default`）；
- 若多个 case 同时就绪，**随机**选一个执行；
- 每个 case 的操作与对应分支体**原子地**完成（其他 case 不会被同时执行）。

#### 2. 为什么这样设计

`select` 是 CSP 模型中"非确定性选择"的实现，它让程序能**公平地**等待多个事件源，而不是轮询。底层由 `runtime.selectgo` 实现，涉及 `scase` 数组、对所有 channel 加锁、随机洗牌等机制——细节留到第14章详述。

这里只需理解一个高层流程：

```
                     ┌─────────────┐
                     │  select { } │
                     └──────┬──────┘
                            │
              ┌─────────────┼─────────────┐
              ▼             ▼             ▼
        case <-ch1    case ch2<-v    case <-ch3
              │             │             │
              └─────────────┼─────────────┘
                            ▼
                   1. 随机洗牌 case 顺序
                   2. 依次尝试每个 case 是否就绪
                   3. 任一就绪 → 执行该 case，返回
                   4. 全部未就绪且有 default → 执行 default
                   5. 全部未就绪且无 default → 在所有 channel 上
                      挂起 sudog，gopark，等待任一 channel 唤醒
                            │
                            ▼
                   被唤醒 → 清理其它 channel 上的 sudog → 执行中奖 case
```

#### 3. 工程实践与常见坑

- **超时控制**：`case <-time.After(d)` 是最常用的模式，避免 goroutine 永久阻塞。
- **非阻塞收发**：`default` 分支让 select 立即返回，常用于"有就处理、没有就跳过"。
- **动态禁用 case**：把 channel 变量置 `nil`，对应 case 永久阻塞，等价于"从 select 中移除"。
- **空 select `select{}`**：永久阻塞，常用于让 main goroutine 等待信号（避免泄漏的 goroutine 退出前 main 退出）。

> `select` 的完整原理（`scase` 结构、`selectgo` 算法、随机性与公平性）见 [第14章 select](./14-select.md)。

---

### Channel 最佳实践

#### 1. 所有权与关闭责任

**原则**：channel 的创建者即唯一发送者，由它负责 `close`；接收者永远不要 close。

落地方式：用**方向受限 channel** 显式表达所有权：

```go
package main

import "fmt"

// 返回只读 channel，调用方只能接收；内部 goroutine 拥有写端并负责关闭
func counter() <-chan int {
    out := make(chan int)
    go func() {
        defer close(out)
        for i := 0; i < 5; i++ {
            out <- i
        }
    }()
    return out
}

func main() {
    for v := range counter() {
        fmt.Println(v)
    }
}
```

这样编译器会阻止接收方误发或误关，把"关闭责任"从约定升级为类型约束。

#### 2. Pipeline 模式

把多个 stage 用 channel 串起来，每个 stage 是一组 goroutine：

```go
package main

import "fmt"

func gen(nums ...int) <-chan int {
    out := make(chan int)
    go func() {
        defer close(out)
        for _, n := range nums {
            out <- n
        }
    }()
    return out
}

func square(in <-chan int) <-chan int {
    out := make(chan int)
    go func() {
        defer close(out)
        for n := range in {
            out <- n * n
        }
    }()
    return out
}

func main() {
    for v := range square(gen(1, 2, 3, 4)) {
        fmt.Println(v)   // 1 4 9 16
    }
}
```

每个 stage 输入 `<-chan int`、输出 `<-chan int`，方向受限、关闭责任清晰，可自由组合。

#### 3. Fan-out / Fan-in

Fan-out：多个 worker 消费同一个 channel，并行处理。Fan-in：把多个 channel 的结果汇入一个。

```go
package main

import (
    "fmt"
    "sync"
)

func worker(id int, in <-chan int, out chan<- int, wg *sync.WaitGroup) {
    defer wg.Done()
    for n := range in {
        out <- n * n
    }
}

func merge(cs ...<-chan int) <-chan int {
    var wg sync.WaitGroup
    out := make(chan int)
    output := func(c <-chan int) {
        defer wg.Done()
        for v := range c {
            out <- v
        }
    }
    for _, c := range cs {
        wg.Add(1)
        go output(c)
    }
    go func() {
        wg.Wait()
        close(out)        // 所有输入消费完才关闭合并 channel
    }()
    return out
}

func main() {
    in := make(chan int)
    out := make(chan int)
    var wg sync.WaitGroup

    // Fan-out: 3 个 worker
    for i := 0; i < 3; i++ {
        wg.Add(1)
        go worker(i, in, out, &wg)
    }
    go func() {
        for i := 0; i < 10; i++ {
            in <- i
        }
        close(in)
    }()

    // 等所有 worker 退出后关闭 out
    go func() { wg.Wait(); close(out) }()

    for v := range out {
        fmt.Println(v)
    }
}
```

#### 4. 超时与取消（done channel / context）

```go
package main

import (
    "context"
    "fmt"
    "time"
)

func worker(ctx context.Context) <-chan int {
    out := make(chan int)
    go func() {
        defer close(out)
        for i := 0; ; i++ {
            select {
            case <-ctx.Done():
                return
            case out <- i:
                time.Sleep(200 * time.Millisecond)
            }
        }
    }()
    return out
}

func main() {
    ctx, cancel := context.WithTimeout(context.Background(), time.Second)
    defer cancel()
    for v := range worker(ctx) {
        fmt.Println(v)
    }
}
```

`ctx.Done()` 本质是一个 `<-chan struct{}`，`cancel()` / 超时会 `close` 它，所有 `select` 立刻就绪。

#### 5. Worker Pool

```go
package main

import (
    "fmt"
    "sync"
)

func main() {
    jobs := make(chan int, 100)
    results := make(chan int, 100)

    // 启动 4 个 worker
    var wg sync.WaitGroup
    for w := 0; w < 4; w++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for j := range jobs {
                results <- j * j
            }
        }()
    }

    // 投递任务
    go func() {
        for i := 0; i < 20; i++ {
            jobs <- i
        }
        close(jobs)
    }()

    // 等所有 worker 退出后关闭 results
    go func() { wg.Wait(); close(results) }()

    for r := range results {
        fmt.Println(r)
    }
}
```

#### 6. 常见反模式

**反模式 1：用 channel 当互斥锁**

```go
// 别这样
ch := make(chan struct{}, 1)
ch <- struct{}{}   // "加锁"
// 临界区
<-ch               // "解锁"
```

channel 比 `sync.Mutex` 慢一个数量级，且语义模糊。需要锁就用 `sync.Mutex`。

**反模式 2：把 channel 当普通集合**

```go
// 别这样：用 channel 存数据再反复 len/遍历
ch := make(chan int, 1000)
// ... 塞一堆数据
// 想随机访问？做不到。
```

channel 是"流"不是"集合"，需要切片/映射就用 slice/map。

**反模式 3：发送方未关闭导致 range 泄漏**

```go
// 反面
go func() {
    ch <- 1
    ch <- 2
    // 忘了 close(ch)
}()
for v := range ch {  // 永久阻塞在第 3 次接收
    fmt.Println(v)
}
```

**反模式 4：在接收方 close**

```go
// 反面
go func() {
    for v := range ch {
        if v == sentinel {
            close(ch)  // 接收方关闭，可能和发送方竞争 → panic
        }
    }
}()
```

用 `context` 或额外的 done channel 通知发送方停止，而不是接收方去 close。

**反模式 5：无缓冲 channel 当缓冲用**

```go
// 反面：以为这样能并发处理 5 个
ch := make(chan int)       // 无缓冲！
for i := 0; i < 5; i++ {
    go func() { ch <- work() }()  // 实际仍串行：必须有人立即接收
}
```

需要并发暂存就用 `make(chan int, n)`。

#### 7. 性能要点

| 关注点 | 建议 |
|--------|------|
| 高频收发 | 优先 `sync.Mutex` + slice；channel 每次有锁+可能调度 |
| 信号通知 | `chan struct{}`，零拷贝零分配 |
| 缓冲大小 | 默认 1；削峰填谷再加大；过大掩盖背压问题 |
| goroutine 泄漏 | 所有发送路径配 `ctx.Done()` 或超时；用 `goleak` 工具检测 |
| 批量传递 | 一次发 `[]T` 而非多次发 `T`，减少锁与调度次数 |

---

### 本章小结

本章从 `hchan` 结构出发，剖析了 channel 的核心实现：

- **`hchan`** 由互斥锁 `lock`、环形缓冲 `buf`、`sendq`/`recvq` 等待队列构成；`len`/`cap` 是 O(1) 快照。
- **有缓冲 channel** 用环形数组 + `sendx`/`recvx` 游标实现 FIFO 队列；满则发送方入 `sendq`，空则接收方入 `recvq`。一个重要优化：只要 `recvq` 有等待者，发送方会绕过 buf 直接把数据递给接收者。
- **无缓冲 channel** 同步会合，`buf=nil`，数据在两个 goroutine 栈间直接 `memmove`，建立强 happened-before 关系。
- **`close()`** 在锁内置位 `closed`，批量唤醒 `recvq`（得零值）和 `sendq`（触发 panic）。三大 panic：重复关闭、关闭 nil、向已关闭 channel 发送。
- **`range`** 是 `chanrecv2` 的语法糖，消费完 buf 中残留后才退出。
- **`select`** 是多路 channel 复用，随机选择就绪分支，底层 `selectgo` 详见下一章。
- **最佳实践**：方向受限表达所有权、发送方负责关闭、pipeline/fan-out/context 配合、避免把 channel 当锁或集合。

掌握 channel 的关键在于理解它"传递数据 + 同步"二合一的本质，以及"所有者关闭、接收方只读"的所有权模型。下一章 [select](./14-select.md) 会深入 `scase` 与 `selectgo`，揭示随机性与公平性的运行时实现。
