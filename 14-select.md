## 第14章 select（重点）

> 引言：如果说 [channel](./13-Channel.md) 是 goroutine 之间的"管道"，那么 `select` 就是把这些管道汇到一起的"交换机"。它能同时监听多个 channel 的收发，并在其中任意一个就绪时作出响应——这是 Go 实现超时、取消、多路复用、动态分支的核心武器。本章从语法语义一路下探到运行时 `selectgo` 的源码实现，讲清随机选择与公平性的来龙去脉。

---

### select 语法

#### 1. 是什么

`select` 是 Go 内建的控制结构，语法形似 `switch`，但每个 `case` 必须是一个** channel 的发送或接收操作**：

```go
select {
case v := <-ch1:
    fmt.Println("received from ch1:", v)
case ch2 <- 42:
    fmt.Println("sent to ch2")
case v, ok := <-ch3:
    fmt.Println("ch3:", v, ok)
case <-time.After(time.Second):
    fmt.Println("timeout")
default:
    fmt.Println("no channel ready, non-blocking")
}
```

语义要点：

1. **每个 case 是一个 channel 操作**（收/发），不能是任意表达式。
2. **阻塞**：若没有 `default` 且没有任何 case 就绪，当前 goroutine 阻塞，直到至少一个 case 就绪。
3. **单选**：一次只执行**一个** case 的操作 + 其分支体，不会同时跑多个 case。
4. **零值 case**：空 `select{}` 没有任何 case，永久阻塞，常用于阻止 main 退出。

#### 2. 为什么这样设计

`select` 的设计目标是在 CSP 模型下提供"非确定性多路复用"——让程序能**公平地**等待多个事件源，而不是按某个固定顺序轮询。这避免了"先检查 ch1 再检查 ch2"导致的 ch1 饥饿问题。

与 `switch` 的本质区别：

| 维度 | `switch` | `select` |
|------|----------|----------|
| case 类型 | 任意表达式（值匹配） | 必须是 channel 收/发 |
| 求值时机 | 编译期/运行期求值一次 | 运行期反复探测 channel 就绪 |
| 顺序 | 自上而下，第一个匹配即执行 | **随机**选择就绪 case |
| 阻塞 | 不阻塞 | 默认阻塞（除非有 `default`） |
| 编译产物 | 跳转表 / 比较链 | 调用 `runtime.selectgo` |

#### 3. 工程实践与常见坑

- **`select` 不是 `switch`**：不要试图用 `case x > 10:` 这种条件分支——编译直接报错。
- **每个 case 的 channel 表达式都会被求值**：

```go
select {
case v := <-getCh():   // getCh() 每次进入 select 都会被调用！
    use(v)
}
```

若 `getCh()` 有副作用或开销，应在 select 外预先取出 channel 引用。

- **空 `select{}`**：合法且有用，等价于"永久阻塞当前 goroutine"。常用于 main 中等待信号，但需确保有其他 goroutine 推进。

```go
func main() {
    go server()
    select {}   // 阻塞，直到进程被信号杀死
}
```

- **`break` 只跳出 `select` 本身**：与 `switch` 一致，不会跳出外层循环，需用标签。

---

### default

#### 1. 是什么

`default` 分支让 `select` 变成**非阻塞**模式：当没有任何 case 就绪时，立即执行 `default` 而不阻塞。

```go
select {
case v := <-ch:
    fmt.Println("got", v)
default:
    fmt.Println("ch empty, skip")
}
```

#### 2. 底层实现

带 `default` 的 select 在运行时调用 `selectgo` 时传入 `block=false`。`selectgo` 的快速路径是：

```
1. 随机洗牌 case 顺序
2. 依次尝试每个 case（操作 channel）
   - 若任一 case 可立即完成 → 执行并返回
3. 全部未就绪 → 因为 block=false，直接返回 default 分支索引
   （不会进入"在所有 channel 上挂 sudog + gopark"的慢路径）
```

关键：**`default` 让 select 完全在用户栈上完成，不涉及 goroutine 挂起/唤醒**，开销很低。这就是非阻塞收发的标准做法。

对比三种"非阻塞接收"写法：

| 写法 | 评价 |
|------|------|
| `if len(ch) > 0 { v := <-ch }` | 错误。len 是瞬时快照，存在 TOCTOU 竞争 |
| `v, ok := <-ch` 配合判断 | 旧式，可行但语义不直观 |
| `select { case v := <-ch: ...; default: ... }` | 推荐，原子且清晰 |

#### 3. 工程实践与常见坑

- **典型用途：非阻塞发送 / 接收**

```go
// 丢弃最新无法处理的请求（背压降级）
select {
case jobs <- job:
    // 入队成功
default:
    drop(job)   // 队列满，直接丢弃
}
```

- **典型用途：心跳 tick 的非阻塞消费**

```go
for {
    select {
    case v := <-work:
        process(v)
    case <-tick:
        heartbeat()
    default:
        // 没活干也不阻塞，可以做别的或让出 CPU
        runtime.Gosched()
    }
}
```

- **坑：忙等待（busy loop）**：在 `for { select { case ...: ; default: } }` 中如果 default 分支不主动让出 CPU，会烧满一个核。要么去掉 `default` 让 select 阻塞，要么在 default 里 `runtime.Gosched()` 或 `time.Sleep`。

- **坑：`default` 掩盖死锁**：开发期排查问题时，临时去掉 `default` 让死锁显式抛出 `all goroutines are asleep` 更易定位。

---

### 随机选择

#### 1. 是什么

当 `select` 中**多个 case 同时就绪**时，Go 不会按 case 的书写顺序选择，而是**伪随机**地挑一个执行。看这个经典例子：

```go
package main

import "fmt"

func main() {
    ch1 := make(chan int, 1)
    ch2 := make(chan int, 1)
    ch1 <- 1
    ch2 <- 2

    count := map[int]int{}
    for i := 0; i < 100000; i++ {
        ch1 <- 1   // 重新填满
        ch2 <- 2
        select {
        case <-ch1:
            count[1]++
        case <-ch2:
            count[2]++
        }
    }
    fmt.Println(count)   // 大致 {1: 50000, 2: 50000}，而非总是 1
}
```

输出大约各占 50%，而不是"总是选 ch1"。

#### 2. 为什么这样设计：避免饥饿

如果按书写顺序选，第一个就绪的 case 会"霸占"select，后续 case 可能长期得不到服务——这就是**饥饿（starvation）**。例如：

```go
// 假设按顺序选择
for {
    select {
    case <-fastTicker:   // 频繁就绪，总是被选
        handleFast()
    case <-slowTicker:   // 几乎饿死
        handleSlow()
    }
}
```

随机化让每个就绪 case 在长期统计上有相等的执行机会，是**公平性**的工程化实现。

#### 3. 底层实现：fastrand + Fisher-Yates 洗牌

`runtime.selectgo` 在尝试 case 之前，会用 `fastrandn` 生成随机数，对 case 数组做一次**轻量洗牌**，得到一个随机遍历顺序：

```go
// 简化自 runtime/select.go
func selectgo(c0 *scase, block bool) (int, bool) {
    ncases := ...                       // case 总数
    order := make([]uint16, 2*ncases)   // 洗牌用的索引数组

    // 用 fastrand 产生随机数，对 order[0:ncases] 做 Fisher-Yates 洗牌
    norder := 0
    for i := 1; i < ncases; i++ {
        j := fastrandn(uint32(i + 1))   // 0..i 之间的随机下标
        order[norder] = order[j]
        order[j] = uint16(i)
        norder++
    }

    // 第一轮：按洗牌后的顺序逐个尝试 case
    for _, idx := range order[:norder] {
        c := &cases[idx]
        switch c.kind {
        case caseRecv:
            if v, ok := chanrecv(c.c, c.elem, false /*非阻塞*/); ok {
                return idx, v   // 命中，立即返回
            }
        case caseSend:
            if chansend(c.c, c.elem, false) {
                return idx, true
            }
        }
    }
    // ... 全部未就绪 → 走 default 或挂起
}
```

要点：

- **洗牌只发生一次**（每次进入 select 都洗一次），开销 O(ncases)。
- **逐个非阻塞探测**：用 `block=false` 调用 `chanrecv`/`chansend`，不会在探测阶段阻塞。
- **第一个就绪的 case 即胜出**——由于顺序已被随机化，"第一个"在统计上对所有 case 公平。

> 注意：这里的随机性是**伪随机**（基于 `fastrand`，运行时全局 PRNG），不保证密码学安全，但对调度公平性完全够用。

#### 3. 工程实践与常见坑

- **不要依赖"case 顺序"做语义**：例如希望"优先处理 ch1"——select 不会保证。要做优先级，需手动实现：

```go
// 伪"优先级" select：先非阻塞试一次 ch1，再进入完整 select
select {
case v := <-ch1:
    handleHigh(v)              // 优先尝试 ch1
default:
    select {
    case v := <-ch1:
        handleHigh(v)
    case v := <-ch2:
        handleLow(v)
    }
}
```

- **随机性是统计意义上的**：单次运行可能"连续 10 次都选 ch1"，这是正常的，不是 bug。只在大量样本下才接近均匀。

- **随机选择不保证吞吐公平**：如果 ch1 每秒就绪 1000 次、ch2 每秒就绪 1 次，select 仍然以"谁先就绪谁先进"为准，随机性只作用于"同一瞬间多个就绪"的情况。

---

### 公平性

#### 1. 是什么

`select` 的公平性体现在两个层面：

1. **case 间公平**：多个 case 同时就绪时，每个 case 被选中的概率在长期统计上相等（由随机洗牌保证）。
2. **goroutine 间公平**：当多个 goroutine 在同一个 channel 上等待时，被唤醒的顺序由 `sudog` 队列的 FIFO 顺序决定，而非随机。

这两个层面相互配合，避免 select 在多路复用中产生饥饿。

#### 2. 为什么这样设计

**case 间公平靠随机**（见上一节），**goroutine 间公平靠 FIFO 队列**：

`hchan.recvq` / `sendq` 是双向链表，`enqueue` 加到尾部、`dequeue` 从头部取——严格的 FIFO。这意味着：

- 多个接收者等同一个 channel 时，谁先阻塞谁先被满足；
- 多个发送者等同一个满 channel 时，谁先阻塞谁先被满足（或被 close 时一起被唤醒）。

```
channel ch (无缓冲)，3 个 goroutine 在等接收：

recvq:  [g:A] <-> [g:B] <-> [g:C]   (FIFO 链表)

当某个发送方 ch <- v:
   1. dequeue 头部 g:A
   2. 把 v 拷贝到 A 的接收变量
   3. goready(g:A)
   → A 被唤醒，B、C 继续等
```

如果改成 LIFO 或随机，先到的 goroutine 可能长期不被唤醒，造成"请求堆积"或"延迟抖动"。FIFO 保证可预测的延迟上界。

#### 3. select 场景下的公平性细节

当 select 涉及多个 channel 时，公平性有一个微妙之处：**select 不保证"哪个 channel 先被服务"**，但保证"任一就绪的 channel 都有机会被选"。具体：

- **不能饿死某个 channel**：即便 ch1 就绪频率远高于 ch2，只要 ch2 在某次 select 进入时就绪，它就有非零概率被选中。
- **不保证严格轮转**：select 不会记录"上次选了 ch1，这次该选 ch2"。每次都是独立的随机选择。
- **唤醒后的清理**：当 select 因某个 channel 被唤醒时，需要从**其它所有**已挂 sudog 的 channel 上把自己摘除——这步在 `selectgo` 的慢路径里完成，保证不会"赖在别人的队列上"造成幽灵唤醒。

```
select 等 3 个 channel，把自己挂到每个 channel 的等待队列：

ch1.recvq: ... -> [me] -> ...
ch2.recvq: ... -> [me] -> ...
ch3.recvq: ... -> [me] -> ...

某时刻 ch2 有数据 → ch2 唤醒 me
me 在 selectgo 中:
   1. 从 ch1.recvq 摘除自己的 sudog
   2. 从 ch3.recvq 摘除自己的 sudog
   3. 处理 ch2 的数据，执行 case 分支
```

#### 4. 工程实践与常见坑

- **公平性 ≠ 优先级**：需要优先级时，select 不直接支持，得用嵌套 select 或独立"高优先级 channel 优先消费"的逻辑。
- **FIFO 在 close 时被打破吗？** 不打破。`closechan` 唤醒所有等待者时**仍然按队列顺序**加入 `glist`，然后统一 `goready`——但被调度的先后由调度器决定，不保证严格 FIFO 唤醒执行。所以"close 后接收者都得到零值"，但谁先 return 是不确定的。
- **饥饿检测**：Go 调度器有 goroutine 饥饿检测（async preemption），但那是针对 CPU 时间，不是 channel。channel 层面的饥饿要靠工程手段（分离 channel、独立 worker、限流）。
- **不要假设 select 是"轮询"**：它是"事件驱动等待"，没有就绪 case 时不消耗 CPU。

---

### select 为什么只能操作 Channel

#### 1. 是什么

Go 的 `select` 严格要求每个 `case` 必须是 channel 的收/发操作，不能是文件 IO、网络 socket、定时器条件、普通布尔表达式：

```go
select {
case x > 10:        // 编译错误：case 必须是 channel 操作
    ...
case f.Read(buf):   // 编译错误：不能是方法调用
    ...
}
```

这与一些语言（如 Unix 的 `select`/`poll`/`epoll`、Java NIO 的 `Selector`）的"通用多路复用"形成对比。

#### 2. 为什么这样设计

**(1) CSP 模型的纯粹性**

Go 的并发原语刻意保持精简：goroutine + channel。`select` 是 channel 的配套机制，专门解决"同时等待多个 channel"的问题。把它做成通用事件复用器会引入大量跨子系统的复杂度（文件系统、网络栈、定时器、信号……），与 Go"少即是多"的设计哲学相悖。

**(2) 类型安全与编译期检查**

每个 case 是 `case v := <-ch` 或 `case ch <- v`，编译器能在编译期完成：

- channel 类型与 case 表达式类型匹配；
- 收/发方向匹配（`chan<-` 不能接收，`<-chan` 不能发送）；
- 变量类型与元素类型匹配。

如果允许任意条件，这些检查都不可能。强类型让 select 几乎不会写错。

**(3) 同步语义的确定性**

channel 操作是**同步原语**——`<-ch` 要么完成（数据到手），要么阻塞。这种"完成即有数据"的语义让 select 的执行模型非常清晰：每个 case 是一次原子探测。文件 IO、网络读是系统调用，语义复杂（部分读、EAGAIN、被信号中断……），不适合塞进 select 的简洁模型。

**(4) 运行时集成的简洁性**

`selectgo` 只需要操作 `hchan`——加锁、查 buf、入 sudog 队列。这套机制对 channel 是天然契合的。如果支持文件/网络，需要把每个 case 接到 netpoller 或 eventfd 上，运行时会变得庞大且与平台耦合。Go 选择把网络 IO 单独走 netpoller（在 `net` 包里隐式 `nonblocking`+`epoll`），让 goroutine 在等待时被 park，但**不通过 select 暴露给用户**——用户只需阻塞读 `conn.Read`，runtime 自动挂起/恢复 goroutine。

**(5) 替代方案充足**

Go 通过其它机制覆盖了"通用事件复用"的需求：

| 需求 | Go 方案 |
|------|---------|
| 多个网络连接 | 每个连接一个 goroutine 阻塞读（netpoller 在背后 epoll） |
| 定时器 | `time.After` / `time.Tick` 返回 channel，可纳入 select |
| 信号 | `os/signal.Notify` 返回 channel |
| 文件变化 | 第三方库或 `os` 轮询，结果送入 channel |
| 任意条件 | 用 channel 表达"条件达成事件" |

**统一用 channel 表达事件源**是 Go 的设计选择——所有异步事件都能被"channel 化"，于是 select 只需处理 channel 就足够通用。

#### 3. 工程实践与常见坑

- **"为什么我的网络读不能 select？"**：可以直接 `select { case buf := <-readCh: }`——把 `conn.Read` 放进一个 goroutine，读完送到 channel。这是 Go 的惯用法，比直接 select fd 更清晰。

```go
func readChan(r io.Reader) <-chan []byte {
    out := make(chan []byte)
    go func() {
        defer close(out)
        buf := make([]byte, 1024)
        for {
            n, err := r.Read(buf)
            if n > 0 {
                b := make([]byte, n)
                copy(b, buf[:n])
                out <- b
            }
            if err != nil { return }
        }
    }()
    return out
}
```

- **定时器也是 channel**：`time.After` 内部启动一个 timer，到期后向 channel 发送一个 `Time`。把它放进 select 即可做超时控制。注意 `time.After` 每次调用都会创建 timer，循环中频繁用会泄漏——改用 `time.NewTimer` + `Reset` 复用。

- **`context.Done()` 是 channel**：所以 `select { case <-ctx.Done(): }` 是 cancel 的标准写法。这正体现了"所有事件 channel 化"的设计。

- **想 select 任意条件？** 用一个额外的 channel 当信号：

```go
cond := make(chan struct{})
// 某处: close(cond) 或 cond <- struct{}{}
select {
case <-cond:
    // 条件达成
case <-other:
    // ...
}
```

---

### select 在 Runtime 中如何实现

#### 1. 是什么

`select` 语句在编译期被改写为对 `runtime.selectgo` 的调用。运行时为这次 select 构造一个 `scase` 数组（每个 case 一项），然后由 `selectgo` 完成"探测 → 选择 → 执行或挂起"的完整流程。理解 `selectgo` 是理解 select 性能与正确性的关键。

#### 2. 关键数据结构（Go 1.21+，`runtime/select.go`）

```go
// scase 描述 select 中的一个 case
type scase struct {
    c    *hchan         // 该 case 操作的 channel；nil 表示 CaseNil（无效 case）
    elem unsafe.Pointer // 数据地址
                         //   CaseSend: 指向待发送的值
                         //   CaseRecv: 指向接收变量
    kind uint16         // case 类型，见下方常量
    // ... 其余字段用于排序与 GC
}

const (
    caseNil    = iota // 无效 case（占位，例如 nil channel）
    caseRecv          // 接收: case v := <-ch
    caseSend          // 发送: case ch <- v
    caseDefault       // default 分支
)
```

**逐字段解释：**

| 字段 | 作用 |
|------|------|
| `c` | case 涉及的 channel。若 case 是 `default`，`c` 为 nil；若 case 是 nil channel 的收/发，`c` 也为 nil（视为永不就绪）。 |
| `elem` | 数据缓冲地址。发送 case 指向待发变量；接收 case 指向接收变量；default case 不用。 |
| `kind` | 区分收/发/default。`selectgo` 按 kind 走不同分支。 |

编译器在函数栈上分配 `scase` 数组（每个 case 一个元素），加上一个 `order` 数组用于随机洗牌。然后调用：

```go
// 简化签名
func selectgo(cases *scase, block bool) (chosen int, recvOK bool)
```

- `cases`：scase 数组首地址；
- `block`：是否有 `default` 分支（有 default → block=false）；
- 返回 `chosen`：被选中的 case 在数组中的下标；`recvOK`：接收 case 是否拿到真值（关闭 channel 返回零值时为 false）。

#### 3. `selectgo` 完整流程（简化伪代码）

```go
func selectgo(cases []scase, block bool) (int, bool) {
    ncases := len(cases)

    // ===== 阶段 0：构造洗牌顺序 =====
    // order 长度 2*ncases：前半用于遍历 case，后半用于"需要挂 sudog 的 case"
    var order [2*ncases]uint16
    for i := 0; i < ncases; i++ {
        order[i] = uint16(i)
    }
    // Fisher-Yates 洗牌，用 fastrandn
    for i := ncases - 1; i > 0; i-- {
        j := fastrandn(uint32(i + 1))
        order[i], order[j] = order[j], order[i]
    }

    // ===== 阶段 1：非阻塞探测（按洗牌顺序） =====
    var casi int
    for _, idx := range order[:ncases] {
        casi = int(idx)
        c := cases[casi].c
        if c == nil { continue }   // nil channel，永不就绪
        switch cases[casi].kind {
        case caseRecv:
            // 用 block=false 调用 chanrecv，不阻塞
            if ok := chanrecv(c, cases[casi].elem, false); ok {
                return casi, true  // 立即返回，命中
            }
        case caseSend:
            if ok := chansend(c, cases[casi].elem, false); ok {
                return casi, false
            }
        }
    }

    // ===== 阶段 2：有 default 且无 case 就绪 → 选 default =====
    if !block {
        // 找到 default case 的下标返回
        for i, c := range cases {
            if c.kind == caseDefault {
                return i, false
            }
        }
        // 理论上不会到这，编译器保证有 default 时 block=false
    }

    // ===== 阶段 3：阻塞模式，准备挂起 =====
    // 为每个非 nil channel 的 case 创建 sudog，挂到对应 channel 的等待队列
    // 关键：按 channel 地址排序加锁，避免多 select 间死锁
    gp := getg()
    var sudogs []*sudog
    for _, idx := range order[:ncases] {
        c := cases[idx].c
        if c == nil { continue }
        sg := acquireSudog()
        sg.g = gp
        sg.elem = cases[idx].elem
        sg.c = c
        sg.isSelect = true       // 标记：这是 select 场景
        c.sendq.enqueue(sg)      // 或 recvq，取决于 kind
        sudogs = append(sudogs, sg)
    }

    // ===== 阶段 4：gopark，等待任一 channel 唤醒 =====
    gp.param = nil
    gopark(selparkcommit, ...)

    // ===== 阶段 5：被唤醒，清理其它 channel 上的 sudog =====
    // 被唤醒后，gp.param 指向"中标"的 sudog
    winner := gp.param.(*sudog)
    chosenIndex := -1
    for _, sg := range sudogs {
        if sg == winner {
            chosenIndex = sgCaseIndex(sg)
            sg.success = true
            continue
        }
        // 从其它 channel 的等待队列摘除自己
        sg.c.sendq.dequeue(sg)   // 或 recvq
        releaseSudog(sg)
    }
    return chosenIndex, winner.success
}
```

> 上述伪代码省略了加锁顺序、`sellock` / `selunlock`、GC 协作等细节，但完整呈现了五阶段结构。真实代码在 `runtime/select.go` 的 `selectgo` 函数，约 400 行。

#### 4. 关键实现要点

**(1) 洗牌保证随机性**

阶段 0 的 Fisher-Yates 洗牌是上一章"随机选择"的根源。洗牌只产生一个随机排列，然后用这个排列遍历 case——开销 O(n)，但保证统计公平。

**(2) 两轮探测的精妙**

为什么是"先非阻塞探测、再挂起"两轮，而不是直接挂起？因为：

- **快速路径**：大多数 select 在第一次探测时就有 case 就绪（channel 频繁活动），无需挂起 goroutine，省下 sudog 分配与队列操作。
- **正确性**：挂起前必须再探测一次，否则可能错过"探测后、挂起前"对端发来的数据。`selectgo` 在挂起前会用 `sellock` 锁定所有 channel，再重新检查每个 case 的就绪状态——这是**避免丢失唤醒**的关键。

**(3) 按 channel 地址排序加锁，避免死锁**

多个 goroutine 同时 select 多个相同 channel 时，如果各自按不同顺序加锁，会形成死锁。`selectgo` 把所有涉及的 channel 按**内存地址排序**后统一加锁，保证全局一致的加锁顺序：

```
goroutine A: select { ch1, ch2 }   地址: ch1=0x100, ch2=0x200 → 锁顺序 ch1, ch2
goroutine B: select { ch2, ch1 }   地址: ch1=0x100, ch2=0x200 → 锁顺序 ch1, ch2  ← 同样顺序
```

这就消除了交叉锁死锁。

**(4) `isSelect` 标记的作用**

挂到 channel 等待队列的 sudog 带有 `isSelect=true`。当某个 channel 唤醒该 sudog 时，会检查这个标记——如果是 select 场景，需要让 `selectgo` 自己清理其它 channel 上的 sudog；而普通收发则不需要这步。这避免了"幽灵唤醒"：一个 goroutine 不能同时被两个 channel 同时唤醒并各自执行 case。

**(5) 清理的复杂性**

被唤醒后，`selectgo` 必须从所有**未中标**的 channel 上摘除自己的 sudog，否则：

- 这些 channel 后续会唤醒一个已经不存在的 goroutine；
- sudog 不释放会内存泄漏。

这一步需要再次按地址顺序加锁，逐个 dequeue。开销随 case 数线性增长——所以 select 的 case 数不宜过多（一般几个到几十个）。

#### 5. 完整流程图

```
                        ┌──────────────────────┐
                        │  进入 selectgo        │
                        └──────────┬───────────┘
                                   │
                       ┌───────────▼────────────┐
                       │ 阶段0: 洗牌 case 顺序  │  fastrandn + Fisher-Yates
                       └───────────┬────────────┘
                                   │
                       ┌───────────▼────────────┐
                       │ 阶段1: 非阻塞逐个探测   │  chanrecv/chansend(block=false)
                       └───────────┬────────────┘
                                   │
                          有 case 就绪? ──是──► 返回选中 case
                                   │ 否
                                   ▼
                          有 default?  ──是──► 返回 default
                                   │ 否
                                   ▼
                       ┌────────────────────────┐
                       │ 阶段3: 分配 sudog,     │  按 channel 地址排序加锁
                       │       挂到各 channel   │  isSelect=true
                       └───────────┬────────────┘
                                   │
                       ┌───────────▼────────────┐
                       │ 阶段4: gopark 挂起      │  等待任一 channel 唤醒
                       └───────────┬────────────┘
                                   │
                       ┌───────────▼────────────┐
                       │ 阶段5: 识别中标的       │
                       │       sudog, 清理其它  │  从其它 channel dequeue
                       └───────────┬────────────┘
                                   │
                                   ▼
                              返回选中 case
```

#### 6. 工程实践与常见坑

- **case 数量影响性能**：每次 select 洗牌 + 探测 + 可能的挂起都是 O(ncases)。case 多到几十上百时考虑重构（拆分多个 select，或用 channel 多路复用器模式）。
- **重复 channel 的坑**：一个 select 里两次 `<-ch` 是合法的，但只会执行其中一个；如果两个 case 都是同一个 channel 的收/发，且 channel 就绪，被选中的是随机一个。这种写法易出错，应避免。

```go
// 反面：两个 case 操作同一 channel，行为不直观
select {
case v := <-ch:
    doA(v)
case v := <-ch:        // 同一个 ch
    doB(v)
}
```

- **nil channel 禁用 case 的实战**：在状态机中把不关心的 channel 置 nil，让对应 case 永不就绪，是 select 的常用技巧：

```go
package main

import "fmt"

func main() {
    a, b := make(chan int), make(chan int)
    aClosed, bClosed := false, false

    for !aClosed || !bClosed {
        // 动态禁用已关闭的 channel
        var ca, cb <-chan int = a, b
        if aClosed { ca = nil }
        if bClosed { cb = nil }

        select {
        case v, ok := <-ca:
            if !ok { aClosed = true; continue }
            fmt.Println("a:", v)
        case v, ok := <-cb:
            if !ok { bClosed = true; continue }
            fmt.Println("b:", v)
        }
    }
    fmt.Println("both closed")
}
```

`ca = nil` 后 `case v := <-ca` 永久阻塞，等价于从 select 中移除。当两边都关闭后，`ca` 和 `cb` 都是 nil，select 进入"全 nil + 无 default"的死锁——所以外层用 `for !aClosed || !bClosed` 控制，避免真的死锁。

- **select 嵌套的代价**：嵌套 select 每层都走 `selectgo`，开销叠加。能用单个 select 表达的优先合并。
- **不要在 select 内做重活**：select 的 case 分支应尽量短，把耗时操作挪到 select 外，避免阻塞影响其它 case 的响应延迟。

#### 7. 性能与正确性要点汇总

| 关注点 | 说明 |
|--------|------|
| 洗牌开销 | 每次 select O(ncases)，case 多时可见 |
| 加锁顺序 | 按 channel 地址排序，多 select 不会死锁 |
| 快速路径 | 多数 select 命中阶段 1，无需挂起，开销小 |
| 慢路径开销 | 挂起需为每个 channel 分配 sudog + 入队，唤醒后清理 |
| 重复 channel | 合法但易错，避免 |
| nil channel | 等价"禁用 case"，是动态 select 的核心技巧 |
| case 数 | 几个到几十个最佳；上百需重构 |
| `time.After` 泄漏 | 循环中频繁用会泄漏 timer，改用 `time.NewTimer` + `Reset` |

---

### 本章小结

本章从语法到运行时全面剖析了 `select`：

- **语法语义**：每个 case 必须是 channel 收/发；默认阻塞，有 `default` 则非阻塞；一次只执行一个 case。
- **`default`**：让 select 走 `block=false` 快速路径，是非阻塞收发的标准做法；注意避免忙等待。
- **随机选择**：多个 case 同时就绪时，由 `fastrandn` + Fisher-Yates 洗牌产生的随机顺序决定，避免书写顺序导致的饥饿。
- **公平性**：case 间公平靠随机，goroutine 间公平靠 `recvq`/`sendq` 的 FIFO；唤醒后需从其它 channel 摘除 sudog 防止幽灵唤醒。
- **只能操作 channel**：源自 CSP 模型的纯粹性、类型安全、同步语义确定性；定时器/信号/取消等事件都被"channel 化"以纳入 select。
- **`selectgo` 实现**：scase 数组 → 洗牌 → 非阻塞探测 → (default 或挂起) → 唤醒清理，五阶段流程；按 channel 地址排序加锁避免死锁；`isSelect` 标记协调多 channel 唤醒。

掌握 select 的关键在于理解它"事件驱动的非确定性多路复用"本质——它不是 `switch`，不是 `epoll`，而是 CSP 模型下对 channel 的天然补充。配合上一章的 [channel 原理](./13-Channel.md)，你已经具备了 Go 并发编程最核心的两块基石。后续章节将进入 context、sync 包与运行时调度的更深层。
