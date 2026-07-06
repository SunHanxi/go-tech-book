## 第15章 sync 包

> 引言：`sync` 包是 Go 并发的"显式同步"工具箱，与 channel 的"通信式同步"互补。它提供 `Mutex` / `RWMutex`（互斥）、`Once`（单次执行）、`Cond`（条件变量）、`WaitGroup`（等待组）、`Pool`（对象复用）、`atomic`（原子操作）。这些原语直接对接 Runtime 的信号量与自旋机制，性能远超 channel，但用错代价惨重（死锁、内存破坏、可见性问题）。本章逐个剖析其底层数据结构、状态机与工程陷阱。

### Mutex

**是什么**

`sync.Mutex` 是 Go 的互斥锁，保证同一时刻只有一个 goroutine 进入临界区。它不可重入（同 goroutine 二次 Lock 会死锁），且**禁止复制**（用 `go vet` 检测）。

```go
type Mutex struct {
    state int32  // 状态位：锁状态、饥饿、唤醒、等待者计数
    sema  uint32 // 信号量，阻塞/唤醒等待者
}

func (m *Mutex) Lock()
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

`Mutex` 只有 8 字节，却编码了丰富的状态（Go 1.21，`src/sync/mutex.go`）：

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
2. **饥饿模式（Starving）**：当一个等待者排队超过 **1ms** 仍未拿到锁，Mutex 切到饥饿模式。此模式下锁**直接交给队首等待者**，新来者不自旋、直接排队。等待者队列尾部或队首等待时间 < 1ms 时切回正常模式。

**Lock 流程（简化伪代码）**

```go
func (m *Mutex) Lock() {
    // 快路径：CAS 抢锁
    if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
        return // 直接拿到
    }
    m.lockSlow() // 自旋 + 排队
}

func (m *Mutex) lockSlow() {
    for {
        old := m.state
        new := old | mutexLocked
        if old&mutexStarving == 0 {
            // 正常模式：尝试抢锁
        } else {
            // 饥饿模式：不自旋，排队
            new = old + 1<<mutexWaiterShift // waiter+1
        }
        if atomic.CompareAndSwapInt32(&m.state, old, new) {
            // 入队：runtime_SemacquireMutex(&m.sema, ...)
            runtime_SemacquireMutex(&m.sema, queueLifo, 1)
            // 被唤醒后检查是否切饥饿模式
        }
    }
}
```

**自旋（Spin）**：在多核且 `GOMAXPROCS>1`、本地 P 有空闲 G 时，等待者会执行最多 30 个 `PAUSE`/`YIELD` 自旋，避免立即挂起 goroutine（挂起/唤醒开销大）。自旋期间用 `mutexWoken` 标记，防止 Unlock 时误发唤醒。

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
4. **`Unlock` 未锁的 Mutex 会 panic**：`runtime: unlock of unlocked mutex`。
5. **尽量缩小临界区**：锁内不要做 IO、长计算，避免吞吐坍塌。
6. **避免锁嵌套**：A.Lock 后再 B.Lock 易死锁，固定加锁顺序。

| 误用 | 后果 |
|------|------|
| 复制 Mutex | 状态错乱，`go vet` 报错 |
| 重复 Lock 同一锁 | 死锁 |
| Unlock 未 Lock | panic |
| 锁内阻塞 IO | 吞骤降 |

### RWMutex

**是什么**

`sync.RWMutex` 是读写锁：多个读锁可并发，写锁独占。读多写少的场景能显著提升并发度。写锁不可升级（持读锁时再 Lock 写锁会死锁）。

```go
type RWMutex struct {
    w           Mutex        // 写锁互斥（串行化写者）
    writerSem   uint32       // 写者信号量（等待读者退出）
    readerSem   uint32       // 读者信号量（等待写者完成）
    readerCount int32        // 当前读者数（含"待退出写者"标记）
    readerWait  int32        // 写者到达后，还需等待退出的读者数
}

func (rw *RWMutex) RLock()
func (rw *RWMutex) RUnlock()
func (rw *RWMutex) Lock()    // 写锁
func (rw *RWMutex) Unlock()  // 写锁
```

**底层结构与状态机**

关键字段 `readerCount` 是一个**双用途计数器**：
- 正常时：当前活跃读者数。
- 有写者等待时：`readerCount -= rwmutexMaxReaders`（`rwmutexMaxReaders = 1 << 30`），既记录"写者已到"，又保留原读者数（通过 `readerCount + rwmutexMaxReaders` 还原）。

`RLock` 流程：
```go
func (rw *RWMutex) RLock() {
    if atomic.AddInt32(&rw.readerCount, 1) < 0 {
        // readerCount < 0 说明有写者持有/等待，本读者阻塞
        runtime_SemacquireRWMutexR(&rw.readerSem, false, 0)
    }
}
```

`Lock`（写锁）流程：
```go
func (rw *RWMutex) Lock() {
    rw.w.Lock()                       // 串行化写者
    r := atomic.AddInt32(&rw.readerCount, -rwmutexMaxReaders) + rwmutexMaxReaders
    // r 是 Lock 时已有的读者数
    if r != 0 && atomic.AddInt32(&rw.readerWait, r) != 0 {
        // 还有读者未退出，写者阻塞
        runtime_SemacquireRWMutex(&rw.writerSem, false, 0)
    }
}
```

`RUnlock`：减少 readerCount；若 < 0（有写者等待）且自己是最后一个待退读者，唤醒写者。

**写者优先语义**：当写者到达（Lock 中减 readerCount 后），**新来的读者会阻塞**（readerCount < 0 触发 RLock 阻塞）。这避免写者饥饿，但要小心：高写压力下读者可能长时间拿不到锁。

**工程实践与常见坑**

1. **`RLock` / `RUnlock` 必须配对**：多 Unlock 一次会让 readerCount 错乱，触发 panic 或死锁。
2. **不能在读锁内升级写锁**：持读锁时调 `Lock()` 会自死锁（写锁等所有读者退出，而自己是读者）。
   ```go
   rw.RLock()
   rw.Lock() // 死锁
   ```
3. **递归读锁不安全**：同 goroutine 两次 RLock 看似无碍，但若期间有写者到达，第二次 RUnlock 可能让 readerCount 提前归零唤醒写者，而第一次还没 RUnlock，造成写者读到不一致数据。**RWMutex 不可重入**。
4. **写少读多才划算**：纯写场景 RWMutex 比 Mutex 慢（状态更复杂）。基准测试后选择。
5. **禁止复制**：同 Mutex。

| 场景 | 推荐 |
|------|------|
| 读 90% / 写 10% | RWMutex |
| 读写各半 | Mutex（更简单、更快） |
| 短临界区 | Mutex |
| 长临界区 + 多读 | RWMutex |

### Once

**是什么**

`sync.Once` 保证某个动作**恰好执行一次**，即使多个 goroutine 并发调用 `Do(f)`。常用于单例初始化、配置加载。

```go
type Once struct {
    done atomic.Uint32 // Go 1.21+ 用 atomic.Uint32；早期是 uint32
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
    if o.done.Load() == 0 {
        o.doSlow(f)
    }
}

func (o *Once) doSlow(f func()) {
    o.m.Lock()
    defer o.m.Unlock()
    if o.done.Load() == 0 {
        defer o.done.Store(1) // f 完成后才标记
        f()
    }
}
```

要点：
- **双重检查（Double-Checked Locking）**：先 atomic 读 `done`，未执行才加锁；锁内再读一次，防止并发重复执行。
- `done` 用 atomic 保证可见性——不加锁的快速路径也能正确看到"已完成"状态。
- `f()` 在 `done.Store(1)` **之前**执行（defer LIFO），保证 `done==1` 时 f 的副作用已对其他 goroutine 可见。

> Go 1.21+ 改用 `atomic.Uint32`，去掉了早期版本对 `atomic.Store` 的显式调用，但语义不变。

**工程实践与常见坑**

1. **`f` panic 后 `done` 不会置 1**：`defer o.done.Store(1)` 在 `f()` 返回后才执行；若 f panic 且未 recover，store 不执行，后续 Do 会再调 f。可用 `OnceFunc` / `OnceValue`（Go 1.21+）获得更安全的行为。
2. **`f` 内不要再调同一个 Once 的 Do**：递归调用死锁（持锁状态下再次 Lock）。
3. **Once 不能复用**：执行一次后永久"已完成"，无法 Reset。需要重复初始化用 `sync.Once` + 标志位或 `atomic.Pointer`。
4. **Go 1.21+ 的 `OnceFunc` / `OnceValue` / `OnceValues`**：封装常见模式，避免手写 Once。

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

**为什么用 ticket 而非简单队列**：早期 Go 用简单链表，但在 `Signal` 与并发 `Wait` 竞争时会丢失唤醒（lost wakeup）。ticket 机制保证每个 `Wait` 都被精确记账，避免唤醒丢失。

**工程实践与常见坑**

1. **`Wait` 必须在 `for` 循环中**：被唤醒后条件可能已被其他 goroutine 改变（虚假唤醒或竞态），必须重新检查。
   ```go
   for !condition {
       c.Wait()
   }
   ```
   **不要用 `if`**——这是 Cond 最经典的 bug。
2. **`Signal` / `Broadcast` 不需要持锁，但持锁更安全**：不持锁也能调，但为避免"唤醒在 Wait 拿 ticket 之前"的窗口，通常持锁调。
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
    state atomic.Uint64 // 高 32 位 = 计数；低 32 位 = 等待者数
    sema  uint32        // 信号量，阻塞 Wait
}

func (wg *WaitGroup) Add(delta int)
func (wg *WaitGroup) Done()
func (wg *WaitGroup) Wait()
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
    state atomic.Uint64 // 高32位=counter，低32位=waiter
    sema  uint32
}
```

字段解释：
- `state`：64 位打包两个 32 位值。高 32 位是**计数器**（待完成 goroutine 数），低 32 位是**等待者数**（调用 Wait 阻塞的 goroutine 数）。
- `sema`：信号量。Wait 把 waiter+1，若 counter>0 则 `runtime_Semacquire` 阻塞；Add 让 counter 归零时 `runtime_Semrelease` 唤醒所有 waiter。
- `noCopy`：禁止复制（`go vet` 检测）。

`Add` 简化逻辑：
```go
func (wg *WaitGroup) Add(delta int) {
    state := wg.state.Add(uint64(delta) << 32)
    v := int32(state >> 32)      // counter
    w := uint32(state)           // waiter
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

> 为什么 counter 和 waiter 打包成一个 64 位原子？因为"判断 counter 归零并唤醒"必须**原子**完成，否则 Add 与 Wait 会有竞争（Add 看到 waiter=0 不唤醒，Wait 又在 Add 之后增加 waiter，导致永远不唤醒）。打包后单次 CAS 即可原子更新。

**工程实践与常见坑**

1. **`Add` 必须在 goroutine **外部**调用**：
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
4. **WaitGroup 不能复用除非归零**：在 Wait 返回前不要重新 Add 正值（行为未定义）。复用应在 Wait 返回后。
5. **Go 1.20+ 的 `WaitGroup.Go`**：简化 `Add(1); go f()` 模式。
   ```go
   var wg sync.WaitGroup
   for i := 0; i < 5; i++ {
       wg.Go(func() { ... }) // 自动 Add(1) + go + Done
   }
   wg.Wait()
   ```
6. **panic 传播**：worker 内 panic 不会自动传到 Wait（除非 recover）。生产代码应在 worker 内 recover 并通过其他渠道上报。

### Pool

**是什么**

`sync.Pool` 是对象池，缓存已分配的对象供复用，减轻 GC 压力。**关键特性：Pool 中的对象可能在任意 GC 时刻被清除**，因此它只适合"短生命周期、可重建"的对象，**不能**当作持久缓存。

```go
type Pool struct {
    noCopy noCopy
    local     []poolLocal // 每 P 一个本地池
    localSize uintptr
    victim     []poolLocal // 上一轮 GC 的本地池（两代缓存）
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
    pad [128]byte // padding 防 false sharing
}

type poolLocalInternal struct {
    private any       // 当前 P 私有对象（无锁快速路径）
    shared  []any     // 共享队列，其他 P 可偷
    lock    Mutex     // 保护 shared
}
```

`Get` 流程：
1. 取当前 P 的 `poolLocal`。
2. 先看 `private`（无锁），有则返回。
3. 再看本地 `shared`（加锁），pop 尾部。
4. 本地空则偷其他 P 的 `shared`（加对方锁，pop 头部）。
5. 偷不到，看 `victim`（上一代缓存）。
6. 都没有，调 `New` 构造。

`Put` 流程：
1. 放当前 P 的 `private`（若空）。
2. 否则 append 到 `shared`。

**两代缓存与 GC 清理**：Runtime 在每次 GC 时调用 `poolCleanup`：
- 把当前 `local` 移到 `victim`（老一代）。
- 清空老 `victim`（即上上代，真正丢弃）。

这样 Pool 有"两代"生命周期：第一代 GC 后变 victim，第二代 GC 后被清。设计目的是平衡"复用收益"与"内存占用"——GC 时部分清理，避免池无限增长，又留一代缓冲让频繁分配的对象仍可复用。

> 关键：**Pool 不保证对象存活**。不要用 Pool 做缓存（如缓存 DB 连接、计算结果），它会随时被 GC 清空。它只优化"分配开销大、生命周期短、可重建"的对象。

**工程实践与常见坑**

1. **Put 前必须 Reset 状态**：复用的对象可能残留旧数据（如 Buffer 旧内容），Put 前清空。
2. **Pool 不是缓存**：见上。需要持久缓存用 `lru` / `freelru` 等库。
3. **不要 Put 比 New 更大的对象**：会让 Pool 内存膨胀。统一对象大小。
4. **并发安全但对象本身不一定**：Get 拿到的对象此时只有一个 goroutine 持有，可安全使用；Put 后不要再访问。
5. **适合对象**：`bytes.Buffer`、`gzip.Writer`、`json.Encoder`、大 slice header 等。不适合：连接、文件句柄、有外部资源的对象。
6. **容量无上限**：`shared` 是 slice，Put 多少存多少（受 GC 清理约束）。注意别在 Put 路径无脑堆积。

| 对象 | 适合 Pool | 原因 |
|------|-----------|------|
| bytes.Buffer | 是 | 分配内部 slice 开销大，易复用 |
| gzip.Writer | 是 | 构造开销大 |
| http.Request body 已读完的 | 否 | 生命周期与请求绑定 |
| DB 连接 | 否 | 用 sql.DB 的连接池，不是 sync.Pool |

### Atomic

**是什么**

`sync/atomic` 包提供**原子操作**，是比 Mutex 更底层的同步原语。它直接映射到 CPU 的原子指令（CAS、Load-Linked/Store-Conditional 等），无锁、无阻塞，性能远超 Mutex。用于计数器、标志位、无锁数据结构。

主要操作（以 int32 为例，还有 int64/uint32/uint64/uintptr/Pointer）：

```go
func LoadInt32(addr *int32) int32
func StoreInt32(addr *int32, val int32)
func AddInt32(addr *int32, delta int32) int32
func SwapInt32(addr *int32, new int32) int32
func CompareAndSwapInt32(addr *int32, old, new int32) bool
```

Go 1.19+ 新增类型安全的 `atomic.Int32` / `Int64` / `Uint32` / `Uint64` / `Bool` / `Pointer[T]`，避免手传 `*int32` 的易错。

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

1. **CPU 原子指令**：`CompareAndSwap` 对应 x86 的 `LOCK CMPXCHG`，ARM 的 `LDREX/STREX`。单条指令原子完成"比较+交换"，无需锁。
2. **内存屏障**：原子操作附带内存屏障，保证可见性顺序。`Load` 是 acquire（后续读写不重排到它之前），`Store` 是 release（之前读写不重排到它之后）。
3. **`atomic.Value` / `atomic.Pointer[T]`**：用于原子读写"任意类型"或泛型指针，常做无锁配置热更新。

```go
type Value struct {
    v any // atomic.Value 内部用 unsafe 原子交换
}

func (v *Value) Load() any
func (v *Value) Store(x any) // Go 1.17+ 放宽了类型一致性约束
```

**CAS 实现无锁计数**

```go
// AddInt32 的等价 CAS 循环
func AddInt32(addr *int32, delta int32) int32 {
    for {
        old := *addr
        new := old + delta
        if CompareAndSwapInt32(addr, old, new) {
            return new
        }
        // CAS 失败说明有竞争，重试
    }
}
```

> 实际 Runtime 用更高效的 `XADD` 指令直接完成 Add，无需 CAS 循环。这里只为说明原理。

**工程实践与常见坑**

1. **`atomic.Int64` 等需 8 字节对齐**：在 32 位平台上，非对齐的 int64 原子操作会 panic 或行为未定义。用 `atomic.Int64` 类型（编译器保证对齐）而非裸 `int64` + `atomic.AddInt64`。

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

2. **CAS 循环要小心活锁**：高竞争下 CAS 反复失败，CPU 空转。竞争激烈时 Mutex 反而更优。
3. **`atomic.Value` 的类型一致性（Go 1.17 前）**：第一次 Store 决定类型，后续 Store 必须同类型，否则 panic。Go 1.17+ 放宽：只要底层类型一致即可，但仍建议统一类型。
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

   读多写少的配置场景，比 Mutex 快得多。

6. **不要用 `atomic` 做"可见性"假象**：`atomic.Load` / `Store` 保证可见性，但普通变量读写不保证。不要假设"我 atomic 写了 flag，普通变量 a 的写也可见"——需把 a 的写放在 Store 之前（release 语义），读放在 Load 之后（acquire）。

| 场景 | 推荐 |
|------|------|
| 单变量计数 | `atomic.Int64.Add` |
| 标志位开关 | `atomic.Bool` |
| 配置热更新（整体替换） | `atomic.Pointer[T]` |
| 多变量一致性 | `Mutex` |
| 无锁队列 | `atomic.Pointer` + CAS（高级，慎用） |

**与 channel / Mutex 的选择**

| 同步需求 | 首选 |
|----------|------|
| goroutine 间传值 / 信号 | channel |
| 保护临界区（多变量） | Mutex / RWMutex |
| 单变量原子读写 | atomic |
| 等待一组 goroutine | WaitGroup |
| 一次性初始化 | Once |

> 经验：能用 channel 表达优先 channel（更安全、更 Go 风格）；性能敏感的单变量场景用 atomic；临界区用 Mutex。三者各司其职，不要混用。

### 本章小结

`sync` 包是 Go 显式同步的核心，每个原语都对接 Runtime 的特定机制：

- **Mutex**：int32 状态位编码锁/唤醒/饥饿/等待者计数；正常模式自旋抢锁、饥饿模式直接交接队首，1ms 阈值切换；不可重入、禁复制。
- **RWMutex**：`readerCount` 双用途计数器实现写者优先；读锁并发、写锁独占；不可升级、不可递归。
- **Once**：双重检查 + atomic done；f panic 不标记完成；Go 1.21+ 用 `atomic.Uint32` 与 `OnceFunc/OnceValue`。
- **Cond**：notifyList 的 ticket 机制防止丢失唤醒；`Wait` 必须 for 循环检查条件；禁复制；优先用 channel。
- **WaitGroup**：64 位打包 counter(高32) + waiter(低32) 原子更新；Add 必须在 goroutine 外；Go 1.20+ 有 `wg.Go`。
- **Pool**：每 P 本地池 + victim 两代缓存，GC 时部分清理；只适合短生命周期可重建对象，Put 前 Reset；不是持久缓存。
- **Atomic**：CPU 原子指令 + 内存屏障；`atomic.Int64` 等保证对齐；`atomic.Pointer[T]` 适合配置热更新；高竞争下 CAS 循环可能不如 Mutex。

通用红线：**所有 sync 类型都禁止复制**（`go vet` 守护）；锁内别做长阻塞 IO；优先 channel，性能瓶颈再用 atomic/Mutex 微调。理解这些底层结构后，你能排查"死锁""内存对齐 panic""Pool 失效""WaitGroup 提前返回"等典型问题，并在并发设计时做出正确的原语选型。
