## 第14章 Timer 与 Ticker

> 引言：`time` 包里的 Timer 与 Ticker 是 Go 里"延时"与"周期触发"的两大原语。它们看似简单（一个 channel + 一个触发时间），底层却共享 Runtime 的**四叉最小堆**定时器系统，并与 netpoller、调度器深度耦合。用不好——`time.After` 不消费会泄漏、`Reset` 时机不对会丢信号、`Stop` 不关 channel 令人困惑。本章沿着 Timer / After / AfterFunc / Ticker / Stop / Reset / Runtime Timer 一路讲到底层实现。

### Timer

**是什么**

`time.Timer` 表示一个**一次性**定时器：到达设定时刻后，Runtime 会向它的 `C` channel 发送当前时间（并关闭？不，不关闭，只发送一次）。`time.NewTimer(d)` 创建并启动一个 Timer。

```go
type Timer struct {
    C <-chan Time      // 只读 channel，到点 Runtime 向它发送 time.Now()
    r runtimeTimer     // Runtime 内部表示，对用户不可见
}

func NewTimer(d Duration) *Timer
func (t *Timer) Stop() bool
func (t *Timer) Reset(d Duration) bool
```

典型用法：

```go
package main

import (
	"fmt"
	"time"
)

func main() {
	t := time.NewTimer(2 * time.Second)
	defer t.Stop() // 习惯性释放

	select {
	case v := <-t.C:
		fmt.Println("fired at", v)
	case <-time.After(3 * time.Second):
		fmt.Println("other branch won")
	}
}
```

**为什么这样设计 / 底层结构**

Timer 对外暴露 channel，对内通过 `runtimeTimer`（详见 [Runtime Timer](#runtime-timer) 一节）注册到 Runtime 的定时器堆。设计要点：
1. **channel 而非回调**：与 Go 的 CSP 风格一致，定时器到点"发一个值"，由用户在 select 里消费，避免回调地狱。
2. **一次性**：Timer 触发后不会自动重置，需手动 `Reset` 才能再次使用。
3. **`C` 是带缓冲大小为 1 的 channel**：保证 Runtime 即使在用户尚未 select 时发送也不会阻塞，发送后 channel 里留一个值等用户取。

```go
// NewTimer 简化逻辑
func NewTimer(d Duration) *Timer {
    c := make(chan Time, 1) // 关键：缓冲为 1
    t := &Timer{
        C: c,
        r: runtimeTimer{
            when:   when(d),
            f:      sendTime, // 到点调用 sendTime(c, now)
            arg:    c,
        },
    }
    startTimer(&t.r) // 注册到 Runtime timer 堆
    return t
}

// sendTime 非阻塞发送，保证 Runtime 不会卡住
func sendTime(c any, seq any) {
    select {
    case c.(chan Time) <- seq.(Time):
    default:
        // 用户还没消费，丢弃本次（实际上 buffered=1 不会走到这里除非已满）
    }
}
```

> 注意 `sendTime` 用 `select default` 非阻塞发送——这是 Runtime 触发定时器的关键，确保 timerproc 不会因为用户 goroutine 慢而被卡死。

**工程实践与常见坑**

1. **用完 `Stop`**：虽然 Timer 触发后会留在堆外，但显式 `Stop` 能提前从堆中摘除，减少 GC 压力与误触发。
2. **`t.C` 只接收一次**：Timer 是一次性的，第二次 `<-t.C` 会永久阻塞（除非 Reset）。
3. **不要共享一个 Timer 给多个接收者**：`C` 是单 channel，多 goroutine 接收只有一个能拿到值。

### After

**是什么**

`time.After(d)` 返回一个 `<-chan Time`，在 `d` 之后该 channel 收到一个时间值。它是 `NewTimer(d).C` 的简写，但**不暴露 Timer 句柄**，因此无法 `Stop`。

```go
func After(d Duration) <-chan Time
```

典型用法（select 超时分支）：

```go
select {
case res := <-doWork():
    handle(res)
case <-time.After(time.Second):
    return errors.New("timeout")
}
```

**底层实现**

```go
func After(d Duration) <-chan Time {
    return NewTimer(d).C
}
```

即 `After` 就是 `NewTimer`，但丢弃了 `*Timer` 引用，于是**无法 Stop**。

**工程实践与常见坑——这是高频面试题与真实事故源**

1. **泄漏陷阱**：在 select 里用 `time.After`，如果**别的分支先命中**退出函数，这个 Timer 仍然留在 Runtime 堆里直到 `d` 到期，期间 channel 和 timer 对象都无法 GC。在紧密循环里每次 select 都 `time.After`，会堆积大量未到期 timer，内存与 CPU 都会被吃掉。

   ```go
   // 反例：紧密循环里的 time.After 泄漏
   for {
       select {
       case v := <-ch:
           handle(v)
       case <-time.After(time.Minute): // 每次 for 都新建一个 1 分钟 timer，泄漏！
           return
       }
   }
   ```

   正解：复用一个 `NewTimer`，或用 `NewTimer` + `Reset`：

   ```go
   t := time.NewTimer(time.Minute)
   defer t.Stop()
   for {
       t.Reset(time.Minute) // 复用
       select {
       case v := <-ch:
           handle(v)
       case <-t.C:
           return
       }
   }
   ```

2. **短超时用 After 无妨**：`time.After(100*time.Millisecond)` 在轻量场景下泄漏窗口很短，可读性优先时可用。但长超时（秒级以上）务必用 `NewTimer`。

| 写法 | 是否泄漏 | 可控性 |
|------|----------|--------|
| `time.After(d)` | 命中其他分支时泄漏至 d 到期 | 无法 Stop |
| `NewTimer(d)` + `defer Stop()` | 不泄漏 | 可 Stop/Reset |

### AfterFunc

**是什么**

`time.AfterFunc(d, f)` 在 `d` 之后**在自己的 goroutine 里**调用函数 `f`，不经过 channel。它返回一个 `*Timer`，可 `Stop` / `Reset`。

```go
func AfterFunc(d Duration, f func()) *Timer
```

典型用法（延迟执行清理、心跳、退避重试）：

```go
package main

import (
	"fmt"
	"time"
)

func main() {
	t := time.AfterFunc(2*time.Second, func() {
		fmt.Println("fired in", "another goroutine")
	})
	_ = t
	time.Sleep(3 * time.Second)
}
```

**底层实现**

`AfterFunc` 与 `NewTimer` 共用 `runtimeTimer`，只是 `f` 字段不同：

```go
func AfterFunc(d Duration, f func()) *Timer {
    t := &Timer{
        r: runtimeTimer{
            when: when(d),
            f:    goFunc, // 到点调用 goFunc，它启动一个 goroutine 执行 f
            arg:  f,
        },
    }
    startTimer(&t.r)
    return t
}

func goFunc(arg any, seq any) {
    go arg.(func())() // 关键：新起 goroutine，不阻塞 timerproc
}
```

要点：
- `f` 在**新 goroutine** 中执行，避免阻塞 Runtime 的 timerproc（timerproc 是单线程，阻塞它会让所有定时器延迟）。
- 因此 `f` 内 panic 不会被你的主 goroutine recover，需在 `f` 内自保 recover。

**工程实践与常见坑**

1. **`f` 必须自保 panic**：因为跑在独立 goroutine，panic 会直接 crash 进程。

   ```go
   time.AfterFunc(d, func() {
       defer func() { _ = recover() }()
       // ...
   })
   ```

2. **`Stop` 返回值**：`AfterFunc` 的 Timer 没有对外 channel，`Stop()` 返回 true 表示尚未触发、成功阻止；false 表示已触发或已 Stop。
3. **`Reset` 对 AfterFunc 同样适用**：可用于心跳场景。
4. **不要在 `f` 里做长阻塞**：虽然 `f` 在独立 goroutine，但若它持锁会拖慢业务；长任务应再起 goroutine 或用 worker。

### Ticker

**是什么**

`time.Ticker` 是**周期性**触发器：按固定间隔向 `C` channel 发送当前时间。`time.NewTicker(d)` 创建并启动。

```go
type Ticker struct {
    C <-chan Time       // 只读 channel，每 d 收到一个值
    r runtimeTimer      // 内部表示
}

func NewTicker(d Duration) *Ticker
func (t *Ticker) Stop()
func (t *Ticker) Reset(d Duration) // Go 1.15+
```

典型用法：

```go
package main

import (
	"fmt"
	"time"
)

func main() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	done := make(chan struct{})
	go func() {
		time.Sleep(3500 * time.Millisecond)
		close(done)
	}()

	for {
		select {
		case <-done:
			fmt.Println("done")
			return
		case t := <-ticker.C:
			fmt.Println("tick at", t)
		}
	}
}
```

**底层结构与 Runtime 实现要点**

```go
func NewTicker(d Duration) *Ticker {
    if d <= 0 {
        panic("non-positive interval for NewTicker")
    }
    c := make(chan Time, 1) // 缓冲为 1
    t := &Ticker{
        C: c,
        r: runtimeTimer{
            when:   when(d),
            period: int64(d), // 关键：period 非 0 表示周期性
            f:      sendTime,
            arg:    c,
        },
    }
    startTimer(&t.r)
    return t
}
```

要点：
- `runtimeTimer.period` 非 0 时，Runtime 在每次触发后自动把 `when += period` 重新入堆，形成周期触发。
- `C` 缓冲为 1：如果用户来不及消费，Runtime 用 `sendTime` 的 `select default` 丢弃本次，**不会积压**。即 Ticker 是"尽力而为"——慢消费者会丢 tick。
- Go 1.15 前 Ticker 的 `C` 即使在 Stop 后仍可能有一个残留值；Go 1.15+ 已优化但语义仍是"Stop 后不再发新值，旧值可能还在 channel"。

**工程实践与常见坑**

1. **必须 `Stop`**：Ticker 不 Stop 会持续占用 timer 堆与 goroutine 唤醒，是经典泄漏源。`defer ticker.Stop()`。
2. **不要假设每秒正好 N tick**：系统调度、GC 暂停会让 tick 延后；若用户消费慢还会丢 tick。Ticker 不保证"补发"。
3. **`time.Tick` 是泄漏陷阱**：`time.Tick(d)` 返回一个**无法 Stop** 的 Ticker channel，只该用在 main 的 forever loop。库代码禁用。

   ```go
   // 反例（库代码）
   for range time.Tick(time.Second) { ... } // 永远无法 Stop

   // 正例
   t := time.NewTicker(time.Second)
   defer t.Stop()
   for range t.C { ... }
   ```

4. **`d <= 0` 会 panic**：`NewTicker(0)` 或负数直接 panic。

### Stop()

**是什么**

`Timer.Stop()` 与 `Ticker.Stop()` 把定时器从 Runtime 堆中移除，阻止后续触发。返回值（仅 Timer）：
- `true`：成功阻止（定时器尚未触发）。
- `false`：已触发、已 Stop，或已被 Reset。

```go
func (t *Timer) Stop() bool
func (t *Ticker) Stop() // 无返回值
```

**底层实现**

`Stop` 调用 `stopTimer(&t.r)`，它从 Runtime 的 timer 堆里删除该条目。若定时器已在"待执行"队列中（已弹出但未执行），则无法真正删除，返回值会反映这一点。`runtimeTimer` 的状态机：

| 状态 | Stop 行为 | 返回值（Timer） |
|------|-----------|-----------------|
| 在堆中等待 | 从堆删除 | true |
| 已触发（channel 已发送） | 无操作 | false |
| 已 Stop 过 | 无操作 | false |
| 已 Reset 重新入堆 | 删除新条目 | 视新条目是否已触发 |

**工程实践与常见坑**

1. **`defer Stop()` 是好习惯**：即使定时器已触发，Stop 也是幂等安全的。
2. **Stop 后 channel 可能仍有残留值**：若 Stop 前定时器已触发，`t.C` 里可能有一个值。直接 `<-t.C` 会读到这个"过期"值。官方文档建议在 Stop 返回 false 时排空 channel：

   ```go
   if !t.Stop() {
       select {
       case <-t.C:
       default:
       }
   }
   ```

   > 但这段排空逻辑与 Reset 配合时极易出错，详见 [Reset](#reset) 一节。

3. **Ticker.Stop 后不要再读 C**：语义上 Stop 后不再有新值，但可能有残留；不要在 Stop 后再 select `t.C`，行为未定义（会读到残留或永久阻塞）。
4. **Stop 不能"取消"已派发的回调**：`AfterFunc` 的 `f` 若已被调度执行，Stop 无法中止它。

### Reset()

**是什么**

`Timer.Reset(d)` 把已存在的 Timer 重新设置为 `d` 后触发，复用同一个 `*Timer` 与 channel，避免反复 `NewTimer` 造成的堆操作与泄漏。Go 1.1 引入，Go 1.23 前语义微妙，Go 1.23+ 已大幅简化。返回值（仅 Timer）：
- `true`：成功阻止旧触发并重置（旧定时器尚未触发）。
- `false`：旧定时器已触发或已 Stop，仍会重置。

```go
func (t *Timer) Reset(d Duration) bool
```

**底层实现**

`Reset` 内部先 `Stop`（从堆摘除旧条目），再用新 `when` 重新 `startTimer`。Go 1.23 前，若旧定时器已触发但 channel 未被消费，Reset 后旧值仍留在 channel，下次 `<-t.C` 会读到**旧时刻**而非新触发的值——这是无数 bug 的根源。Go 1.23 改为：Reset 会确保 channel 里没有"陈旧"值。

**工程实践与常见坑——Reset 是定时器最危险的 API**

Go 1.23 前官方推荐的"安全 Reset"模式（务必排空）：

```go
// Go 1.22 及之前的正确写法
if !t.Stop() {
    select {
    case <-t.C: // 排空可能残留的旧值
    default:
    }
}
t.Reset(d)
```

漏掉排空会导致：在紧密 for-select 循环里，Reset 后第一次 `<-t.C` 立即命中（读到旧值），逻辑误以为超时已到。

Go 1.23+ 后，Reset 与 Stop 的语义被简化：channel 不再残留陈旧值，`Reset` 可直接调用而无需手动排空。但为兼容旧版本，许多代码仍保留排空写法（无害）。

```go
// Go 1.23+ 可直接
t.Reset(d) // 旧值自动清理
```

**典型正确用法：复用 Timer 的心跳循环**

```go
func watchdog(ctx context.Context, idle <-chan struct{}) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		t.Reset(time.Minute) // 每次活动后重置看门狗
		select {
		case <-ctx.Done():
			return
		case <-idle:
			// 有活动，继续循环（Reset 在下一轮）
		case <-t.C:
			log.Println("idle timeout, exit")
			return
		}
	}
}
```

| 场景 | Go 1.22 及之前 | Go 1.23+ |
|------|----------------|----------|
| Stop 返回 false 后 Reset | 需先排空 `t.C` | 直接 Reset |
| 并发 Reset / 读 C | 需串行化（同一 goroutine） | 仍建议串行 |
| Ticker.Reset | Go 1.15 引入，语义类似 | 同样简化 |

> 经验法则：Reset 与读取 `t.C` 应在**同一个 goroutine** 内串行，避免数据竞争。跨 goroutine 复用 Timer 是高级用法，需额外同步。

### 为什么 Stop 不关闭 Channel

**是什么**

一个长期困扰 Go 开发者的问题：为什么 `Timer.Stop()` / `Ticker.Stop()` 不直接 `close(t.C)`？这样接收者 `<-t.C` 立即返回零值，岂不更方便？

**为什么不关闭**

1. **避免双重关闭 panic**：Timer 的 channel 由 Runtime（timerproc / sendTime）和用户共享所有权。若 Stop 关闭 channel，而 Runtime 在 Stop 前已经向它发送（或正要发送）——`sendTime` 里再 close 会 panic。Runtime 与用户操作是并发的，无法保证顺序。
2. **避免向已关闭 channel 发送 panic**：`sendTime` 用 `select default` 发送，但如果 channel 已被 Stop 关闭，发送会 panic（向已关闭 channel 发送）。Runtime 必须额外维护"是否已关闭"状态，复杂且易错。
3. **所有权归属**：channel 的所有权属于创建者（Runtime 创建并负责发送），关闭权也应归创建者。让用户通过 Stop 关闭会打破这一原则。
4. **历史兼容**：早期 Go 版本就这样设计，大量代码依赖"Stop 不关 channel"的语义（如排空逻辑）。改为关闭会破坏兼容性。
5. **多次 Stop / Reset 的幂等性**：若 Stop 关闭 channel，第二次 Stop 会 panic（重复关闭）。当前设计 Stop 可安全多次调用。

> 引用官方文档原话：*"Stop does not close the channel, to prevent a read from the channel succeeding incorrectly. ... the channel is not closed."*

**底层视角**

`sendTime` 的发送逻辑：

```go
func sendTime(c any, seq any) {
    select {
    case c.(chan Time) <- seq.(Time):
    default:
    }
}
```

若 channel 被关闭，`c.(chan Time) <- seq.(Time)` 会 panic。Runtime 没有也不愿维护"是否关闭"标志——那需要锁，会让 timerproc（全局定时器调度核心）变慢。因此选择"只发送不关闭"，让用户用 `Stop` + 可选排空来管理生命周期。

**工程实践与常见坑**

1. **不要试图手动 `close(t.C)`**：会与 Runtime 的 sendTime 竞争，产生 panic。
2. **Stop 后用 `select default` 排空**：避免读到残留值阻塞逻辑。
3. **判断"是否已 Stop"用 Stop 返回值，而非 channel 状态**：channel 不会因 Stop 而变化。
4. **Go 1.23+ 后心态放宽**：新版本对 Stop/Reset 语义做了简化，残留值问题基本消失，但"Stop 不关 channel"的契约不变。

### Runtime Timer

**是什么**

`time.Timer` / `Ticker` / `AfterFunc` 对用户是高层 API，底层都注册为 `runtimeTimer`，由 Runtime 的**定时器子系统**统一调度。Go 1.14+ 起，定时器采用**每 P 一个四叉最小堆**（4-ary heap），由调度器直接驱动，告别了早期的全局锁 + 单一 timerproc 瓶颈。

**底层数据结构**

`runtimeTimer`（用户侧可见的简化定义，实际 Runtime 内是 `runtime.timer`）：

```go
// src/time/sleep.go 中的用户可见结构（字段名与 runtime.timer 一一对应）
type runtimeTimer struct {
    pp       uintptr      // 拥有该 timer 的 P 指针（Runtime 内部用）
    when     int64        // 触发的绝对时刻（纳秒，runtime nanotime）
    period   int64        // 周期（纳秒）；0 表示一次性
    f        func(any, any) // 触发回调
    arg      any          // f 的第一个参数
    seq      any          // f 的第二个参数（用于区分）
    next     *runtimeTimer // 4-ary heap 内部指针（Runtime 内维护）
}
```

Runtime 内的真实结构 `runtime.timer`（`src/runtime/time.go`，字段语义对应）：

```go
type timer struct {
    pp       puintptr       // 所在 P
    when     int64          // 触发时刻
    period   int64          // 周期；0 = 一次性
    f        func(any, any) // 回调
    arg      any
    seq      any
    // 堆管理字段
    next     int            // 在 4-ary heap 中的索引（实现细节）
    status   uint32         // 状态机：timerWaiting/timerRunning/timerModified/...
}
```

字段解释：
- `pp`：所属 P。每 P 一个 timer 堆，避免全局锁。
- `when`：绝对触发时刻（纳秒）。`when(d)` = `nanotime() + int64(d)`。
- `period`：非 0 表示周期性（Ticker）；每次触发后 `when += period` 重新入堆。
- `f`：触发回调。`NewTimer` 用 `sendTime`，`AfterFunc` 用 `goFunc`。
- `arg` / `seq`：回调参数。`NewTimer` 的 `arg` 是 channel，`seq` 一般为 nil。
- `status`：状态机，避免并发修改时的竞争（详见下文）。

**调度模型演进**

| Go 版本 | 模型 | 特点 |
|---------|------|------|
| Go 1.9 及之前 | 全局 timerproc + 单一 4-ary 堆 + 全局锁 | 多核竞争激烈 |
| Go 1.10~1.13 | 每 P 一个堆，但仍由独立 timerproc 协助 | 减少锁竞争 |
| Go 1.14+ | 每 P 一个 4-ary 堆，由调度器直接驱动 | 彻底去掉 timerproc，在 `schedule` / `findRunnable` / `checkTimers` 中就近处理 |
| Go 1.23 | channel 不再残留陈旧值 | Stop/Reset 语义简化 |

**Go 1.14+ 的核心机制**

1. **每 P 一个堆**：`P.timer` 字段持有 4-ary min-heap，按 `when` 排序。`startTimer` 把 timer 加入当前 P 的堆，`O(log n)`。
2. **调度器就近检查**：`schedule()` / `findRunnable()` 在选择下一个 goroutine 时，会调用 `checkTimers()` 检查当前 P 堆顶是否有到期的 timer，有则执行回调。
3. **`timeSleep` 与 netpoll 集成**：`time.Sleep` 把 goroutine 挂起，timer 到点由调度器唤醒。netpoller 在没有网络事件时也会 `netpollBreak` 唤醒以检查 timer。
4. **stealing timer**：当某个 P 空闲而其他 P 有堆积 timer 时，空闲 P 可"偷"别 P 的到期 timer 执行（`stealTimers`，Go 1.14+），避免一个 P 的 timer 堆积。
5. **状态机防竞争**：timer 有 `timerWaiting / timerRunning / timerModified / timerModifying / timerRemoving / timerRemoved` 等状态，用 CAS 切换，避免 `Reset` / `Stop` 与触发之间的竞争。

简化伪代码（`checkTimers`）：

```go
func checkTimers(pp *p, now int64) int64 {
    // 遍历堆顶到期的 timer
    for {
        t := pp.timers[0]
        if t.when > now {
            break // 堆顶未到期
        }
        // 状态切换：timerWaiting -> timerRunning
        if !atomic.Cas(&t.status, timerWaiting, timerRunning) {
            continue
        }
        // 弹出堆顶
        delpTimer(pp, 0)
        // 执行回调（sendTime / goFunc）
        runOneTimer(pp, t, now)
        // 周期性 timer 重新入堆
        if t.period > 0 {
            t.when += t.period
            addAdjustedTimer(pp, t)
        }
    }
    return nextWakeup
}
```

> 关键洞察：Go 1.14+ 的 timer 性能远超早期版本——每 P 独立堆消除了全局锁，调度器就近检查消除了独立 timerproc 的唤醒延迟。这也是为什么 Go 1.14 后大量 `time.After` / `time.Sleep` 的基准性能提升数倍。

**与 netpoller 的关系**

- 网络轮询的 `netpoll` 等待有超时参数：`netpoll(block, timeout)`，timeout 来自当前 P 堆顶 timer 的 `when`。
- 当 timer 到期时，`netpoll` 返回（即使没有网络事件），调度器转去执行 timer 回调。
- 因此 `time.Sleep` 的精度与调度器 tick 频率、netpoll 超时设置耦合——通常毫秒级精度足够，但**不保证微秒精度**。

**工程实践与常见坑**

1. **定时器精度有限**：不要依赖 `time.After(1*time.Millisecond)` 做精确时序，GC、调度会让它漂移几毫秒甚至几十毫秒。
2. **海量 timer 的成本**：每 P 堆操作 O(log n)，几十万 timer 时单 P 堆可能成为瓶颈。如需海量定时任务，考虑时间轮（hierarchical timing wheel）或分层 timer。
3. **`GOMAXPROCS=1` 下 timer 仍能工作**：调度器在每次 schedule 时检查，但精度与吞吐下降。
4. **避免在热路径频繁 NewTimer/Stop**：堆操作有开销，热路径复用 Timer + Reset。
5. **`runtime.NumGoroutine` 突增可能是 timer 泄漏**：`time.After` 不消费、Ticker 不 Stop 都会让 timer 堆堆积，间接拖慢调度。

### 本章小结

Timer 与 Ticker 是 Go 时间维度的两大原语，共享 Runtime 的 timer 子系统：

- **Timer**：一次性，`NewTimer` 返回带 `C` 的 `*Timer`；`After` 是其简写但无法 Stop，长超时场景易泄漏，应改用 `NewTimer` + 复用。
- **AfterFunc**：到点在新 goroutine 执行回调，`f` 须自保 panic，勿长阻塞。
- **Ticker**：周期触发，`period` 非 0；`C` 缓冲 1、慢消费者丢 tick；必须 `Stop`，`time.Tick` 库代码禁用。
- **Stop**：从堆摘除，幂等安全；不关闭 channel 是为避免与 Runtime sendTime 的 close 竞争。
- **Reset**：复用 Timer；Go 1.23 前需 Stop 返回 false 时排空 `t.C`，Go 1.23+ 自动清理陈旧值；Reset 与读 C 应同 goroutine 串行。
- **Runtime Timer**：每 P 一个 4-ary min-heap（Go 1.14+），调度器就近 `checkTimers`，状态机防竞争，netpoll 超时与 timer 联动；精度毫秒级，海量 timer 需考虑时间轮。

牢记三条红线：长超时别用 `After`、用完必 `Stop`、Reset 注意排空（Go 1.23 前）。掌握底层 4-ary heap 与状态机后，你能解释"为什么 time.After 会泄漏""为什么 Stop 不关 channel""为什么 Reset 要排空"等经典问题。
