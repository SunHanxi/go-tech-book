## 第17章 sync 包

> 引言：`sync` 包是 Go 并发的显式同步工具箱，与 channel 的通信式同步互补。它提供 `Mutex`、`RWMutex`、`Once`、`Cond`、`WaitGroup`、`Pool` 和 `Map`，`sync/atomic` 则提供原子操作。两类工具表达的问题不同，不能用“谁一定更快”选型。本章公共语义基于 Go 1.26；内部结构只用于理解，不是兼容性承诺。

### Mutex

**是什么**

`sync.Mutex` 是 Go 的互斥锁，保证同一时刻只有一个 goroutine 进入临界区。它不可重入（同 goroutine 二次 Lock 会死锁），且**禁止复制**（用 `go vet` 检测）。锁不绑定 goroutine：一个 goroutine 可以 Lock，再安排另一个 goroutine Unlock；这种所有权转移必须有清晰协议。

```go
type Mutex struct {
    // 未导出字段
}

func (m *Mutex) Lock()
func (m *Mutex) TryLock() bool
func (m *Mutex) Unlock()
```

典型用法：

```go
package main

import (
	"fmt"
	"sync"
)

type Counter struct {
	mu sync.Mutex
	n  int
}

func (c *Counter) Add() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func main() {
	c := &Counter{}
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Add()
		}()
	}
	wg.Wait()
	fmt.Println(c.n) // 1000
}
```

**为什么这样设计 / 底层数据结构**

Go 1.26.4 的导出类型 `sync.Mutex` 是 `internal/sync.Mutex` 的薄包装；后者当前用两个字段编码状态。下面的布局用于理解实现，业务代码不能依赖字段、大小或位定义：

```go
type Mutex struct {
    state int32
    sema  uint32
}

const (
    mutexLocked      = 1 << iota // 1: 锁被持有
    mutexWoken                   // 2: 有等待者被唤醒（即将拿到锁）
    mutexStarving                // 4: 饥饿模式
    mutexWaiterShift = iota      // 3: 等待者计数从第 3 位开始
)
```

`state` 字段位布局（int32）：

| 位段 | 含义 |
|------|------|
| bit 0 | `mutexLocked`：1 = 锁已被持有 |
| bit 1 | `mutexWoken`：1 = 有等待者被显式唤醒 |
| bit 2 | `mutexStarving`：1 = 进入饥饿模式 |
| bit 3~31 | 等待者数量（waiter count） |

`sema` 是 Runtime 信号量（`runtime_SemacquireMutex` / `runtime_Semrelease`），用于把等待者挂起/唤醒，本质是 `g` 队列。

**两种模式**

1. **正常模式（Normal）**：新来者有"自旋"机会，可与刚被唤醒的等待者竞争锁。若新来者赢，等待者继续睡。吞吐量高，但等待者可能长期饥饿。
2. **饥饿模式（Starving）**：当一个等待者排队超过当前实现的 1ms 阈值仍未拿到锁，它会推动 Mutex 切到饥饿模式。此模式下锁**直接交给队首等待者**，新来者不抢锁、直接排队。获得锁的等待者若是最后一个等待者，或其等待时间不足阈值，会切回正常模式。这个阈值属于实现细节，不应成为业务时序假设。

**Lock 流程（简化伪代码）**

```go
func (m *Mutex) Lock() {
    if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
        return // 无竞争快路径
    }
    m.lockSlow()
}
```

慢路径会循环读取整个状态字，并根据模式决定短暂自旋、尝试 CAS 取得锁，或增加 waiter 计数后通过 `runtime_SemacquireMutex` 挂起。被唤醒后还要修正 `mutexWoken`、等待者计数和饥饿状态。省略这些状态转换的“几行伪代码”通常会暗示错误的不变量，应直接对照当前 `internal/sync/mutex.go`。

**主动自旋（Spin）**：慢路径会调用 Runtime 的 `runtime_canSpin` 判断当前调度状态是否适合短暂自旋，并通过 `runtime_doSpin` 执行架构相关操作，以避免立刻挂起 goroutine。次数、指令和判定条件都是 Runtime 实现细节；自旋期间可能设置 `mutexWoken`，避免 `Unlock` 再唤醒另一个等待者。

**Unlock 流程**

```go
func (m *Mutex) Unlock() {
    new := atomic.AddInt32(&m.state, -mutexLocked)
    if new != 0 {
        m.unlockSlow(new) // 有等待者，发信号量唤醒队首
    }
}
```

**工程实践与常见坑**

1. **`Lock` 后必须 `Unlock`，配对用 `defer`**：
   ```go
   m.Lock()
   defer m.Unlock()
   ```
2. **不可重入**：同 goroutine 两次 Lock 会死锁。Go 没有"可重入锁"的标准实现，需重构代码避免。
3. **禁止复制**：`sync.Mutex` 含信号量状态，复制会让两个锁共享不同状态，行为未定义。`go vet` 会检测。
4. **`Unlock` 未锁的 Mutex 是不可恢复的运行时错误**：当前实现调用 Runtime `fatal`，不能把它当作可由 `recover` 处理的普通 panic。
5. **谨慎使用 `TryLock`**：失败不会建立任何内存模型上的 synchronizes-before 关系。它适合少数确实允许跳过工作的场景，不适合用轮询代替阻塞锁。
6. **理解可见性**：第 n 次 `Unlock` synchronizes-before 任意更晚成功的 `Lock`；成功的 `TryLock` 等价于 `Lock`，失败则不建立关系。
7. **尽量缩小临界区**：锁内不要做 IO、长计算，避免吞吐坍塌。
8. **避免锁嵌套**：A.Lock 后再 B.Lock 易死锁；确需嵌套时固定加锁顺序。

| 误用 | 后果 |
|------|------|
| 复制 Mutex | 状态错乱，`go vet` 报错 |
| 重复 Lock 同一锁 | 死锁 |
| Unlock 未 Lock | 不可恢复的运行时错误 |
| 锁内阻塞 IO | 吞骤降 |

### RWMutex

**是什么**

`sync.RWMutex` 是读写锁：多个读锁可并发，写锁独占。在读临界区确实能并行且存在锁竞争时，它可能提高吞吐。写锁不可升级（持读锁时再 Lock 写锁会死锁）。

```go
type RWMutex struct {
    w           Mutex        // 写锁互斥（串行化写者）
    writerSem   uint32       // 写者信号量（等待读者退出）
    readerSem   uint32       // 读者信号量（等待写者完成）
    readerCount atomic.Int32 // 当前读者数（含"有写者等待"标记）
    readerWait  atomic.Int32 // 写者到达后，还需退出的读者数
}

func (rw *RWMutex) RLock()
func (rw *RWMutex) TryRLock() bool
func (rw *RWMutex) RUnlock()
func (rw *RWMutex) Lock()    // 写锁
func (rw *RWMutex) TryLock() bool
func (rw *RWMutex) Unlock()  // 写锁
```

**底层结构与状态机**

关键字段 `readerCount` 是一个**双用途计数器**：
- 正常时：当前活跃读者数。
- 有写者等待时：`readerCount -= rwmutexMaxReaders`（`rwmutexMaxReaders = 1 << 30`），既记录"写者已到"，又保留原读者数（通过 `readerCount + rwmutexMaxReaders` 还原）。

`RLock` 流程：
```go
func (rw *RWMutex) RLock() {
    if rw.readerCount.Add(1) < 0 {
        // readerCount < 0 说明有写者持有/等待，本读者阻塞
        runtime_SemacquireRWMutexR(&rw.readerSem, false, 0)
    }
}
```

`Lock`（写锁）流程：
```go
func (rw *RWMutex) Lock() {
    rw.w.Lock()                       // 串行化写者
    r := rw.readerCount.Add(-rwmutexMaxReaders) + rwmutexMaxReaders
    // r 是 Lock 时已有的读者数
    if r != 0 && rw.readerWait.Add(r) != 0 {
        // 还有读者未退出，写者阻塞
        runtime_SemacquireRWMutex(&rw.writerSem, false, 0)
    }
}
```

`RUnlock`：减少 readerCount；若 < 0（有写者等待）且自己是最后一个待退读者，唤醒写者。

**写者优先语义**：当写者到达（Lock 中减 readerCount 后），**新来的读者会阻塞**（readerCount < 0 触发 RLock 阻塞）。这避免写者饥饿，但要小心：高写压力下读者可能长时间拿不到锁。

**工程实践与常见坑**

1. **`RLock` / `RUnlock` 必须配对**：未持有对应锁时 `RUnlock` / `Unlock` 是不可恢复的运行时错误；少释放一次则可能永久阻塞。
2. **不能在读锁内升级写锁**：持读锁时调 `Lock()` 会自死锁（写锁等所有读者退出，而自己是读者）。
   ```go
   rw.RLock()
   rw.Lock() // 死锁
   ```
3. **不要递归获取读锁**：若两次 `RLock` 之间有写者等待，第二次 `RLock` 会阻塞；同一 goroutine 因而无法执行第一次 `RUnlock`，形成死锁。`RWMutex` 不可重入，也不应把读锁升级为写锁。
4. **按争用和临界区实测**：`RWMutex` 允许读并行，但额外状态与读写协调也有成本。读比例、临界区长度、核心数和争用分布都会改变结果，不能只按一个固定读写比例选型。
5. **禁止复制**：同 Mutex。

| 场景 | 推荐 |
|------|------|
| 状态简单、临界区短，或写竞争明显 | 先用 Mutex，再用 benchmark 验证 |
| 读临界区可并行且争用已成为瓶颈 | benchmark 对比 RWMutex |
| 可发布不可变快照 | 考虑 `atomic.Pointer[T]` |

### Once

**是什么**

`sync.Once` 保证某个动作**恰好执行一次**，即使多个 goroutine 并发调用 `Do(f)`。常用于单例初始化、配置加载。

```go
type Once struct {
    done atomic.Bool
    m    Mutex
}

func (o *Once) Do(f func())
```

典型用法：

```go
package main

import (
	"fmt"
	"sync"
)

var (
	once   sync.Once
	config string
)

func LoadConfig() string {
	once.Do(func() {
		fmt.Println("loading...")
		config = "loaded"
	})
	return config
}

func main() {
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = LoadConfig() // 只有一个 goroutine 真正加载
		}()
	}
	wg.Wait()
	fmt.Println(config)
}
```

**底层实现**

```go
func (o *Once) Do(f func()) {
    if !o.done.Load() {
        o.doSlow(f)
    }
}

func (o *Once) doSlow(f func()) {
    o.m.Lock()
    defer o.m.Unlock()
    if !o.done.Load() {
        defer o.done.Store(true) // f 返回或 panic 后才标记
        f()
    }
}
```

要点：
- **双重检查（Double-Checked Locking）**：先 atomic 读 `done`，未执行才加锁；锁内再读一次，防止并发重复执行。
- `done` 用 atomic 保证可见性——不加锁的快速路径也能正确看到"已完成"状态。
- `f()` 在 `done.Store(true)` **之前**执行（defer LIFO），保证观察到 `done==true` 的调用不会在 f 完成前返回。

> 这里展示的是 Go 1.26.4 的当前实现。字段的具体原子类型及布局不是公共契约。

**工程实践与常见坑**

1. **`f` panic 后 Once 仍视为已执行**：`defer o.done.Store(true)` 在 panic 展开栈时仍会执行，首次调用者看到 panic，后续 `Do` 不会再调用 f。`OnceFunc` / `OnceValue` / `OnceValues` 会让后续调用重放同一个 panic，语义不同。
2. **`f` 内不要再调同一个 Once 的 Do**：递归调用死锁（持锁状态下再次 Lock）。
3. **Once 不能 Reset**：每个待执行动作使用新的 `Once` 实例。需要重新加载时，应在锁下设计明确的版本/状态机，或原子发布一份新快照；不要并发替换、清零正在使用的 `Once`。
4. **`OnceFunc` / `OnceValue` / `OnceValues`（Go 1.21）**：封装常见模式，避免手写 Once。

   ```go
   loadConfig := sync.OnceValue(func() string {
       return "loaded"
   })
   fmt.Println(loadConfig()) // 全程只算一次
   ```

### Cond

**是什么**

`sync.Cond` 是条件变量，让 goroutine **等待某个条件成立**后被唤醒。它必须关联一个 `sync.Locker`（通常是 `*Mutex` 或 `*RWMutex`）。`Wait` 原子地"释放锁 + 挂起"，被唤醒后再重新加锁。

```go
type Cond struct {
    noCopy noCopy     // 静态检查：禁止复制
    L     Locker      // 关联的锁
    notify notifyList // 等待者队列（ticket）
    checker copyChecker // 运行时检查：禁止复制
}

func NewCond(l Locker) *Cond
func (c *Cond) Wait()
func (c *Cond) Signal()   // 唤醒一个等待者
func (c *Cond) Broadcast() // 唤醒所有等待者
```

典型用法（生产者-消费者）：

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type Queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []int
}

func NewQueue() *Queue {
	q := &Queue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *Queue) Put(v int) {
	q.mu.Lock()
	q.items = append(q.items, v)
	q.cond.Signal() // 通知一个等待者
	q.mu.Unlock()
}

func (q *Queue) Get() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 {
		q.cond.Wait() // 释放锁、挂起；唤醒后重新加锁
	}
	v := q.items[0]
	q.items = q.items[1:]
	return v
}

func main() {
	q := NewQueue()
	go func() {
		time.Sleep(100 * time.Millisecond)
		q.Put(42)
	}()
	fmt.Println(q.Get()) // 42
}
```

**底层结构与 notifyList**

```go
type notifyList struct {
    wait atomic.Uint32 // 下一个分配的 ticket
    notify uint32      // 下一个要唤醒的 ticket
    lock   uintptr     // Runtime 锁
    head   *sudog      // 等待者链表（runtime 内部）
    tail   *sudog      // 链表尾
}
```

字段解释：
- `wait`：单调递增的 ticket 分配器，每个 `Wait` 调用拿到一个唯一 ticket。
- `notify`：当前已唤醒到哪个 ticket，用它判断哪些等待者该被唤醒。
- `head/tail`：等待者链表（runtime `sudog`，即 goroutine 包装）。

`Wait` 简化逻辑：
```go
func (c *Cond) Wait() {
    c.checker.check()
    t := runtime_notifyListAdd(&c.notify) // 拿 ticket
    c.L.Unlock()                          // 释放锁
    runtime_notifyListWait(&c.notify, t)  // 挂起，直到被 notify
    c.L.Lock()                            // 重新加锁
}
```

`Signal` / `Broadcast` 调用 `runtime_notifyListNotifyOne` / `runtime_notifyListNotifyAll`，按 ticket 顺序唤醒。

**ticket 的作用**：每次 `Wait` 先取得递增 ticket，再释放关联锁并进入 Runtime 等待队列；`Signal` / `Broadcast` 根据计数决定应唤醒的 ticket。这样可以正确处理“已经登记、尚未真正睡眠”与通知并发发生的窗口。具体链表和计数布局属于 Runtime 实现。

**工程实践与常见坑**

1. **`Wait` 必须在 `for` 循环中**：Go 的 `Cond.Wait` 不会无原因返回，但它重新获得 `c.L` 之前，条件可能已被其他 goroutine 消费或改回去，因此返回后仍必须重新检查。
   ```go
   for !condition {
       c.Wait()
   }
   ```
   **不要用 `if`**——这是 Cond 最经典的 bug。
2. **条件本身必须在同一把锁下检查和修改**：`Signal` / `Broadcast` 调用本身允许不持有 `c.L`。常见写法是在锁下修改条件并通知，再释放锁；是否把通知移到解锁后要根据对象生命周期和争用权衡，不能靠通知弥补未受锁保护的条件访问。
3. **`Broadcast` 慎用**：唤醒所有等待者，可能引发"惊群"。多数场景 `Signal` 足够。
4. **禁止复制**：`Cond` 内含 `noCopy` 与 `copyChecker`，复制会 panic。
5. **不要用 Cond 代替 channel**：能用 channel 表达的（如"等一个值"）优先用 channel，更不易错。Cond 适合"条件复杂、需共享锁"的场景。

| 场景 | 选型 |
|------|------|
| 等一个事件 / 一个值 | channel |
| 等待复杂共享状态变化 | Cond |
| 批量唤醒 | Broadcast |

### WaitGroup

**是什么**

`sync.WaitGroup` 等待一组 goroutine 完成。主 goroutine `Add(n)` 增加计数，每个 worker `Done()`（即 `Add(-1)`）减少，`Wait()` 阻塞到计数归零。

```go
type WaitGroup struct {
    noCopy noCopy
    state atomic.Uint64 // 高32位=计数；bit31=synctest；低31位=等待者
    sema  uint32        // 信号量，阻塞 Wait
}

func (wg *WaitGroup) Add(delta int)
func (wg *WaitGroup) Done()
func (wg *WaitGroup) Wait()
func (wg *WaitGroup) Go(f func()) // Go 1.25+
```

典型用法：

```go
package main

import (
	"fmt"
	"sync"
)

func main() {
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			fmt.Println("worker", id)
		}(i)
	}
	wg.Wait()
	fmt.Println("all done")
}
```

**底层结构与状态机**

```go
type WaitGroup struct {
    noCopy noCopy
    state atomic.Uint64 // 高32位=counter，低31位=waiter，中间1位供 synctest
    sema  uint32
}
```

字段解释：
- `state`：高 32 位是计数器，低 31 位是等待者数，剩余一位用于 Go 1.25+ `testing/synctest` 的 bubble 关联。业务代码不应依赖这套布局。
- `sema`：信号量。Wait 把 waiter+1，若 counter>0 则 `runtime_Semacquire` 阻塞；Add 让 counter 归零时 `runtime_Semrelease` 唤醒所有 waiter。
- `noCopy`：禁止复制（`go vet` 检测）。

`Add` 简化逻辑：
```go
func (wg *WaitGroup) Add(delta int) {
    state := wg.state.Add(uint64(delta) << 32)
    v := int32(state >> 32)      // counter
    w := uint32(state & 0x7fffffff) // waiter，排除 synctest 标志
    if v < 0 {
        panic("sync: negative WaitGroup counter")
    }
    if v > 0 || w == 0 {
        return // 还有任务，或没人等
    }
    // counter 归零且有等待者：唤醒所有
    wg.state.Store(0)
    for ; w != 0; w-- {
        runtime_Semrelease(&wg.sema, false, 0)
    }
}
```

`Wait`：把 waiter+1，若 counter>0 则 `runtime_Semacquire` 阻塞。

> 为什么 counter 和 waiter 打包成一个 64 位原子？`Add` 与 `Wait` 需要对“任务是否仍未完成”和“有多少 goroutine 正在等待”进行一致观察与更新。复合状态避免读到两个独立计数器的撕裂组合；当前实现还会检测一部分违规的 `Add` / `Wait` 并发用法。业务代码不应依赖具体位布局。

**工程实践与常见坑**

1. **启动 goroutine 前先 `Add`**：
   ```go
   // 反例：可能 Wait 提前返回
   go func() {
       wg.Add(1) // 竞态：main 可能已 Wait 返回
       defer wg.Done()
   }()

   // 正例
   wg.Add(1)
   go func() {
       defer wg.Done()
   }()
   ```
2. **计数不能为负**：`Add` 负值使 counter<0 会 panic。确保 Add 的总数与 Done 次数匹配。
3. **禁止复制**：复制会分裂状态，`go vet` 报错。
4. **按批次复用**：新的正值 `Add` 必须发生在上一批所有 `Wait` 返回之后；不能在旧一批仍等待时开始下一批。
5. **Go 1.25+ 的 `WaitGroup.Go`**：简化 `Add(1); go f()` 模式，还允许未完成任务继续调用同一 `WaitGroup.Go` 派生任务。传入函数必须不 panic；需要错误传播时使用 `errgroup` 或显式结果 channel。
   ```go
   var wg sync.WaitGroup
   for i := 0; i < 5; i++ {
       wg.Go(func() { ... }) // 自动 Add(1) + go + Done
   }
   wg.Wait()
   ```
6. **`Wait` 不传递 panic 或 error**：普通 goroutine 未恢复的 panic 会终止进程；`WaitGroup.Go` 的契约明确要求 f 不 panic。Go 1.26.4 当前实现遇到 panic 会重新 panic 且不会先 `Done`，避免 `Wait` 抢先返回，但这不是错误传播 API。需要汇总失败就显式返回结果。

### Pool

**是什么**

`sync.Pool` 是对象池，缓存已分配的对象供复用，减轻 GC 压力。**关键特性：Pool 中的对象可在任何时刻无通知地被移除**，因此它只适合"短生命周期、可重建"的对象，**不能**当作持久缓存。

```go
type Pool struct {
    noCopy noCopy
    local     unsafe.Pointer // 实际指向每 P 的 poolLocal 数组
    localSize uintptr
    victim     unsafe.Pointer // 上一轮 GC 留下的本地池
    victimSize uintptr
    New       func() any   // 池空时构造新对象
}

func (p *Pool) Get() any
func (p *Pool) Put(x any)
```

典型用法（复用 bytes.Buffer）：

```go
package main

import (
	"bytes"
	"fmt"
	"sync"
)

var bufPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

func process(s string) string {
	b := bufPool.Get().(*bytes.Buffer)
	defer func() {
		b.Reset()
		bufPool.Put(b)
	}()
	b.WriteString(s)
	return b.String()
}

func main() {
	fmt.Println(process("hello"))
}
```

**底层结构与 Runtime 实现**

每 P 一个 `poolLocal`，避免跨 P 锁竞争：

```go
type poolLocal struct {
    poolLocalInternal
    pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
}

type poolLocalInternal struct {
    private any       // 当前 P 私有对象
    shared  poolChain // 本 P 从头部操作，其他 P 可从尾部窃取
}
```

`Get` 流程：
1. 取当前 P 的 `poolLocal`。
2. 先看 `private`（无锁），有则返回。
3. 再从本地 `shared` 的头部取。
4. 本地空则从其他 P 的 `shared` 尾部窃取。
5. 偷不到，看 `victim`（上一代缓存）。
6. 都没有，调 `New` 构造。

`Put` 流程：
1. 放当前 P 的 `private`（若空）。
2. 否则 push 到 `shared` 头部。

`poolChain` 当前是动态增长的无锁队列链：每段是单生产者、多消费者的环形队列，本 P 操作头部，其他 P 通过原子操作消费尾部。这个结构与 `procPin`、抢占和 GC 清理紧密耦合，不适合复制到业务代码中充当通用无锁队列。

**两代缓存与 GC 清理**：Runtime 在每次 GC 时调用 `poolCleanup`：
- 把当前 `local` 移到 `victim`（老一代）。
- 清空老 `victim`（即上上代，真正丢弃）。

按当前实现，一个 primary cache 在 GC 开始时变为 victim，旧 victim 被丢弃。这解释了对象为什么可能跨过一次 GC 被复用，但**不是存活两轮的 API 保证**；`Get` 本来就允许忽略池中内容。

> 关键：**Pool 不保证对象存活**。不要用 Pool 保存 DB 连接、计算结果或任何正确性状态，Runtime 可以无通知地丢弃条目。它只适合经过测量、可随时重建的临时对象。

若 `Get` 确实返回了先前 `Put(x)` 的同一个 x，则该 `Put(x)` synchronizes-before 这次 `Get`；`New` 返回 x 与后来取得 x 也有相同关系。这只建立发布可见性，不赋予归还后的继续访问权。

**工程实践与常见坑**

1. **归还前清理状态，取出后仍验证状态**：复用对象可能残留旧数据（如 Buffer 内容）。约定由归还方 `Reset`，同时不要让安全性依赖 Pool 一定返回某个已清理对象。
2. **Pool 不是缓存**：见上。需要持久缓存用 `lru` / `freelru` 等库。
3. **给大对象设置回收上限**：例如容量超过阈值的 `bytes.Buffer` 直接丢弃，避免偶发峰值长期保留大底层数组。阈值应根据 profile 和负载确定。
4. **并发安全但对象本身不一定**：Get 拿到的对象此时只有一个 goroutine 持有，可安全使用；Put 后不要再访问。
5. **适合对象**：高频、临时、可重建且构造或分配成本可观的对象，例如 `bytes.Buffer`、可正确 Reset 的压缩器。不适合：连接、文件句柄和需要确定关闭时机的外部资源。
6. **不要假定容量或命中率**：`Put` 不承诺对象会被保留，`Get` 也不承诺返回之前放入的对象。是否值得使用必须通过 allocation profile 和 benchmark 验证。

| 对象 | 适合 Pool | 原因 |
|------|-----------|------|
| bytes.Buffer | 是 | 分配内部 slice 开销大，易复用 |
| gzip.Writer | 是 | 构造开销大 |
| http.Request body 已读完的 | 否 | 生命周期与请求绑定 |
| DB 连接 | 否 | 用 sql.DB 的连接池，不是 sync.Pool |

### Map

**是什么**

`sync.Map` 是并发安全的 `map[any]any` 风格容器，零值可用。它是针对特定访问模式优化的专用类型，不是普通泛型 map 的默认替代品：

1. 同一个 key 通常只写一次、读取很多次，例如只增长的注册表。
2. 多个 goroutine 主要操作互不相交的 key。

普通 `map[K]V` 配合 `Mutex` / `RWMutex` 有静态类型检查，也更容易维护跨 key 不变量，通常应先从它开始。

```go
func (m *Map) Load(key any) (value any, ok bool)
func (m *Map) Store(key, value any)
func (m *Map) Delete(key any)
func (m *Map) LoadOrStore(key, value any) (actual any, loaded bool)
func (m *Map) LoadAndDelete(key any) (value any, loaded bool)
func (m *Map) Swap(key, value any) (previous any, loaded bool)
func (m *Map) CompareAndSwap(key, old, new any) bool
func (m *Map) CompareAndDelete(key, old any) bool
func (m *Map) Range(f func(key, value any) bool)
func (m *Map) Clear()
```

下面的注册表只发布初始化完成后不再修改的值：

```go
type User struct {
    Name string
}

var users sync.Map // map[string]*User

func register(id, name string) *User {
    candidate := &User{Name: name}
    actual, _ := users.LoadOrStore(id, candidate)
    return actual.(*User)
}
```

`LoadOrStore` 只保证每个 key 最终存入一个值，不保证 `candidate` 的构造只执行一次。初始化昂贵或有副作用时，需要额外的 per-key `Once`、singleflight 或其他协调机制。

**当前实现**

Go 1.26.4 的导出类型包装 `internal/sync.HashTrieMap[any, any]`：

```go
type Map struct {
    _ noCopy
    m internalSync.HashTrieMap[any, any]
}
```

当前实现按 key 的哈希分段遍历 trie；普通读取沿原子发布的子指针查找，修改锁住相关内部节点，再原子发布新 entry 或子树。完全相同的哈希通过 overflow 链处理，`Clear` 替换根节点。旧资料里的 `read/dirty/miss` 双表不是 Go 1.26 实现。所有这些都只是实现说明，程序只能依赖公开 API。

写操作若被读操作观察到，会按文档建立 synchronizes-before 关系。`LoadOrStore`、`CompareAndSwap` 等条件操作究竟算读还是写，取决于它是否真的修改了 map，具体分类以 `sync.Map` 文档为准。

**工程实践与常见坑**

1. **key 必须可比较**：动态类型为 slice、map 或 func 的 key 会在哈希时 panic。`CompareAndSwap` / `CompareAndDelete` 的 old 值也必须可比较。
2. **存入指针不等于保护指向的数据**：多个 goroutine 修改同一个 value 指向的对象，仍需锁、原子操作或不可变发布协议。
3. **`Range` 不是快照**：不会重复访问同一 key，但可能看到不同 key 在遍历期间不同时间点的映射；回调可以调用同一个 Map 的方法。API 允许即使回调很早返回 false，整体工作仍达到 O(N)。
4. **没有 `Len` 和有序遍历**：需要一致计数、排序结果或多 key 事务时，在锁下维护普通 map。
5. **复合操作不能拆成 Load + Store**：有条件更新应使用 `LoadOrStore`、`Swap`、`CompareAndSwap` 等单 key 原子方法；跨 key 不变量仍需外部协调。
6. **禁止复制**：首次使用后不能复制 `sync.Map`。
7. **用负载验证选型**：key 分布、读写比例、冲突位置和 value 生命周期都会影响结果；用真实工作集 benchmark，并结合 mutex/block profile 判断。

### Atomic

**是什么**

`sync/atomic` 包提供低层原子内存操作，适合独立计数器、状态位以及不可变快照的整体发布。它的 API 比 Mutex 更难组合：一旦不变量横跨多个可变字段，锁通常更清晰。标准库保证内存模型语义，不保证某个操作一定无锁、对应哪条 CPU 指令或一定快于 Mutex。

主要操作（以 int32 为例，还有 int64/uint32/uint64/uintptr/Pointer）：

```go
func LoadInt32(addr *int32) int32
func StoreInt32(addr *int32, val int32)
func AddInt32(addr *int32, delta int32) int32
func SwapInt32(addr *int32, new int32) int32
func CompareAndSwapInt32(addr *int32, old, new int32) bool
```

Go 1.19+ 新增类型安全的 `atomic.Int32` / `Int64` / `Uint32` / `Uint64` / `Bool` / `Pointer[T]`，避免手传裸指针。Go 1.23 又为整数原子类型增加 `And` / `Or`，适合原子位标志；跨多个字段的不变量仍应使用锁或整体发布不可变快照。

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

func main() {
	var n atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.Add(1)
		}()
	}
	wg.Wait()
	fmt.Println(n.Load()) // 1000
}
```

**为什么这样设计 / 底层实现**

1. **平台实现**：编译器与 Runtime 会针对目标架构实现或内建原子操作。具体可能使用单条指令、指令序列或平台辅助机制；这些都不是 Go 源码可以依赖的契约。
2. **内存顺序**：Go 原子操作表现为处于某个全局顺序一致（sequentially consistent）顺序。若原子操作 A 的效果被 B 观察到，则 A synchronized-before B。不要把 C/C++ 的可选 relaxed/acquire/release API 模型直接套到 Go。
3. **`atomic.Value` / `atomic.Pointer[T]`**：用于原子发布同一具体类型的值或泛型指针，常见用法是让读者加载不可变配置快照。

```go
func (v *Value) Load() any
func (v *Value) Store(x any) // 所有 Store 必须使用相同具体类型，且不能为 nil
func (v *Value) Swap(new any) any
func (v *Value) CompareAndSwap(old, new any) bool
```

**CAS 实现无锁计数**

```go
// 用 CAS 循环表达“满足条件才更新”；普通计数直接用 Add。
func addUnlessLimit(v *atomic.Int32, delta, limit int32) bool {
    for {
        old := v.Load()
        new := old + delta
        if new > limit {
            return false
        }
        if v.CompareAndSwap(old, new) {
            return true
        }
        // CAS 失败说明有竞争，重试
    }
}
```

> CAS 循环里的读取也必须是原子读取；用 `old := *addr` 与并发原子写混用会造成数据竞争。简单增减应直接调用 `Add`，不要手写 CAS 循环。

**工程实践与常见坑**

1. **注意 64 位对齐**：在 ARM、386 和 32 位 MIPS 等 32 位目标上，调用裸指针形式的 64 位原子函数时，调用者负责把目标地址按 64 位对齐。优先使用 `atomic.Int64` / `atomic.Uint64`，这些类型会自动对齐；所有类型化原子值使用后都不得复制。

   ```go
   // 反例（32 位平台可能出问题）
   type S struct {
       x int64
       y byte // 让 z 错位
       z int64
   }
   atomic.AddInt64(&s.z, 1) // 可能未对齐

   // 正例
   type S struct {
       x atomic.Int64
       y byte
       z atomic.Int64
   }
   s.z.Add(1) // 编译器保证对齐
   ```

2. **CAS 循环要考虑进展与争用**：高竞争下 CAS 会反复失败并消耗 CPU；原子操作和 `sync.Mutex` 都不提供业务级公平性契约。用代表性负载比较吞吐、CPU 与尾延迟，需要公平或配额时实现显式调度。
3. **`atomic.Value` 的类型一致性**：第一次 Store 决定具体类型，后续 Store 必须是完全相同的具体类型，否则 panic；存 nil 也会 panic。该约束没有因 Go 1.17 放宽。
4. **atomic 不替代所有锁**：它适合"单变量"原子操作。多变量一致性仍需 Mutex（或用 `atomic.Pointer` 整体替换不可变结构）。
5. **`atomic.Pointer[T]`（Go 1.19+）做无锁配置热更新**：

   ```go
   type Config struct { Addr string }
   var cfg atomic.Pointer[Config]

   // 更新：整体替换
   cfg.Store(&Config{Addr: "new"})

   // 读取：原子拿指针
   c := cfg.Load()
   fmt.Println(c.Addr)
   ```

   发布后不再修改 `Config`；更新时构造新值并整体替换。是否优于 `RWMutex` 仍需在实际读写比例和对象生命周期下测量。

6. **正确理解发布关系**：若原子操作 B 观察到原子操作 A 的效果，则 A synchronizes-before B；结合 goroutine 内程序顺序，可以发布 A 之前完成初始化、之后不再变化的数据。一个原子 flag 不会自动保护其他仍被并发修改的普通变量。复杂状态优先整体发布不可变快照，或放到同一把锁下。

| 场景 | 推荐 |
|------|------|
| 单变量计数 | `atomic.Int64.Add` |
| 标志位开关 | `atomic.Bool` |
| 配置热更新（整体替换） | `atomic.Pointer[T]` |
| 多变量一致性 | `Mutex` |
| 复杂无锁数据结构 | 优先采用经过验证的库；自行实现需形式化不变量和压力测试 |

**与 channel / Mutex 的选择**

| 同步需求 | 首选 |
|----------|------|
| goroutine 间传值 / 信号 | channel |
| 保护临界区（多变量） | Mutex / RWMutex |
| 单变量原子读写 | atomic |
| 等待一组 goroutine | WaitGroup |
| 一次性初始化 | Once |
| 特定访问模式的并发 key/value | sync.Map |

> 选型依据是问题语义：所有权转移和消息流用 channel，共享可变状态用锁，单个独立状态或不可变快照发布才考虑 atomic。它们可以在同一系统中配合使用，但每份状态应有唯一、清晰的同步协议。

### 本章小结

`sync` 包是 Go 显式同步的核心，每个原语都对接 Runtime 的特定机制：

- **Mutex**：导出类型包装内部锁；当前内部状态位编码锁、唤醒、饥饿和等待者计数。未加锁 Unlock 是不可恢复错误，TryLock 失败不建立同步关系。
- **RWMutex**：`readerCount` 双用途计数器实现写者优先；读锁并发、写锁独占；不可升级、不可递归。
- **Once**：双重检查 + `atomic.Bool`；f panic 时仍标记完成；`OnceFunc` / `OnceValue` 会对后续调用重放 panic。
- **Cond**：notifyList 用 ticket 协调登记与唤醒；`Wait` 不会无原因返回，但仍必须在 for 循环中重新检查受锁保护的条件。
- **WaitGroup**：状态字打包 counter 与 waiter；传统模式在启动 goroutine 前 Add，Go 1.25+ 可用 `wg.Go`，其 f 必须不 panic。
- **Pool**：每 P 的 `private + poolChain` 配合 victim cache；只适合临时、可重建对象，不能假定命中、容量或跨 GC 存活。
- **Map**：Go 1.26 当前包装并发哈希 trie；适合标准库列出的两类访问模式，`Range` 不是一致性快照。
- **Atomic**：提供顺序一致的低层原子语义；类型化 64 位值保证对齐；复杂不变量仍用锁或整体发布不可变快照。

通用红线：本章这些有状态同步值首次使用后都不要复制（用 `go vet` 守护）；锁内避免不可控的长阻塞；每份共享状态只采用一套清楚的同步协议。channel、锁和 atomic 是不同语义的工具，选型后仍要用 race detector、profile 和代表性 benchmark 验证。
