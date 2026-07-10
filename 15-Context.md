## 第15章 Context（重点）

> 引言：Context 是 Go 并发编程中跨 goroutine 传递"取消信号、超时、截止时间、请求级数据"的标准载体。它用一棵不可变的 Context 树，把一次请求产生的所有子任务组织起来，让任意一层都能优雅地"通知整棵树退出"。本章将沿着 `cancel / timeout / deadline / value` 四条主线，深入 `cancelCtx`、`timerCtx`、`valueCtx` 的源码实现，并给出工程实践中的避坑指南。

### Context 的设计目标

**是什么**

`context.Context` 是 Go 1.7 正式引入的标准库接口（更早源自 Google 内部的 `golang.org/x/net/context`），它定义了跨 API 边界、跨进程传递**截止时间（deadline）、取消信号（cancellation）、请求级键值数据（values）**的统一契约。Context 一般作为函数的第一个参数 `ctx context.Context` 传递，**禁止**存储到结构体字段中（少数框架内部例外）。

接口定义（Go 1.21+，`src/context/context.go`）：

```go
type Context interface {
    // Deadline 返回 ctx 应被取消的时间，ok=false 表示没有截止时间
    Deadline() (deadline time.Time, ok bool)

    // Done 返回一个 channel，ctx 被取消时该 channel 被 close
    // 返回 nil 表示永远不会取消（如 Background / TODO）
    Done() <-chan struct{}

    // Err 返回取消原因；Done 未关闭时返回 nil
    // 已取消时返回 Canceled 或 DeadlineExceeded
    Err() error

    // Value 根据 key 查找请求级数据；不存在返回 nil
    Value(key any) any
}
```

两个不可取消的根 Context：

```go
var (
    background = new(emptyCtx) // context.Background() 返回它
    todo       = new(emptyCtx) // context.TODO() 返回它
)
```

`emptyCtx` 的四个方法都返回零值（`Done()` 返回 `nil`，`Deadline()` 返回 `false`），它永远不会被取消，也不能存值，是整棵 Context 树的"根"。

**为什么这样设计**

1. **接口最小化、组合最大化**：四个方法各司其职、互不耦合，通过组合不同实现（`cancelCtx`、`timerCtx`、`valueCtx`）来叠加能力，而不是用一个"大而全"的结构体。
2. **不可变（Immutable）**：所有派生操作（`WithCancel` / `WithDeadline` / `WithTimeout` / `WithValue`）都返回**新的** Context，绝不修改原 Context，从而保证父子链路安全、并发安全。
3. **树形传播**：Context 自带父子关系，取消信号从父向子传播，恰好匹配"一次请求派生若干子任务"的拓扑结构。
4. **显式传递而非全局变量**：避免 thread-local 风格的隐式上下文，函数签名暴露依赖，便于测试、追踪、重放。
5. **控制流与数据流分离**：取消信号是"控制流"，Value 是"数据流"，二者共用同一接口但实现解耦——`cancelCtx` 不存值，`valueCtx` 不可取消（它复用父的取消能力）。

> 设计哲学：用 channel + 不可变树形结构，替代"全局变量 + 锁"，让 goroutine 树能优雅地协同退出。

底层实现要点：所有派生 Context 都是 `cancelCtx`、`timerCtx`、`valueCtx` 三种结构体的组合嵌套，每个都持有一个指向父 Context 的字段，从而形成树/链表。

**工程实践与常见坑**

- 函数签名第一个参数固定为 `ctx context.Context`，名字约定为 `ctx`。
- 不要把 Context 存到结构体里（除非该结构本身就是一个"请求处理上下文对象"，且生命周期与请求一致，如某些 handler struct）。
- `context.Background()` 用于 main、初始化、测试；`context.TODO()` 用于"还没想好传什么"的占位。二者都不应被取消。
- **不要传 `nil` context**：调用 `ctx.Done()` 等方法时会 panic（nil 接口方法调用）。
- 一个完整请求链路应该用一个根 Context（如 HTTP server 为每个请求创建的 ctx）派生，请求结束自动级联取消。

| 场景 | 推荐做法 |
|------|----------|
| main 函数顶层 | `context.Background()` |
| 函数未接入 context 但计划改造 | `context.TODO()` |
| HTTP/RPC 入口 | 从框架拿到根 ctx，再 `WithCancel` / `WithTimeout` 派生 |
| 后台定时任务 | 用 `context.Background()` 派生带 timeout 的 ctx |

### cancel

**是什么**

`context.WithCancel(parent)` 返回一个派生 Context 和一个 `cancel CancelFunc`。调用 `cancel()`（或父 Context 被取消）后，该 Context 的 `Done()` channel 被关闭，所有监听它的 goroutine 收到退出信号。cancel 是**幂等**的，多次调用安全。

```go
func WithCancel(parent Context) (ctx Context, cancel CancelFunc)
```

典型用法：

```go
package main

import (
	"context"
	"fmt"
	"time"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		select {
		case <-ctx.Done():
			fmt.Println("worker exit:", ctx.Err()) // worker exit: context canceled
		case <-time.After(time.Hour):
		}
	}()

	time.Sleep(100 * time.Millisecond)
	cancel() // 通知 worker 退出
	time.Sleep(100 * time.Millisecond)
}
```

**底层结构与 Runtime 实现要点**

核心结构 `cancelCtx`（Go 1.21，`src/context/context.go`）：

```go
// cancelCtx 可被取消；嵌入它即获得取消能力
type cancelCtx struct {
    Context                  // 父 Context（嵌入接口字段，形成树）

    mu        sync.Mutex     // 保护下面字段
    done      atomic.Value   // chan struct{}，懒初始化；用 atomic.Value 让 Done() 走无锁路径
    children  map[canceler]struct{} // 子节点集合，cancel 时需要级联取消
    err       error          // 取消原因，nil 表示未取消
}
```

字段解释：
- `Context`：父 Context，形成树。
- `mu`：保护 `children` 和 `err`；`done` 用 `atomic.Value`，所以 `Done()` 不需要持锁，可被高频调用。
- `done`：懒初始化的 channel，关闭它即广播取消。用 `atomic.Value` 而非直接字段，是为了让 `Done()` 在无锁路径下也能安全返回。
- `children`：所有"可取消"子节点的集合（实现了 `canceler` 接口的子 Context）。父取消时遍历并级联取消。
- `err`：取消后保存 `context.Canceled`（或父链传播上来的 err）。

`canceler` 接口（只有 `cancel(removeFromParent bool, err, cause error)` 和 `Done() <-chan struct{}` 两个方法），`cancelCtx` 和 `timerCtx` 都实现了它，因此都能被父节点级联取消。

关键流程 `cancelCtx.cancel(removeFromParent, err, cause)`：
1. 持锁后设置 `c.err = err`。
2. 关闭 `c.done`（如果之前为 nil，则先赋值一个已关闭的 channel，保证幂等）。
3. 遍历 `children`，递归 `cancel(false, err, cause)`（不再从父移除，因为父正在清理）。
4. 若 `removeFromParent` 为 true，从父节点的 children 中移除自己。

`propagateCancel(parent, child)`：在 `WithCancel` 时调用，把自己注册到父节点的 children（若父也是 cancelCtx 族且未取消）；若父已取消，则立即取消子节点。这是树形传播的关键：

```go
// 简化的取消传播逻辑
func propagateCancel(parent Context, child canceler) {
    done := parent.Done()
    if done == nil {
        return // 父永远不会取消，无需注册
    }
    select {
    case <-done:
        // 父已取消，立即取消子
        child.cancel(false, parent.Err(), Cause(parent))
        return
    default:
    }
    if p, ok := parentCancelCtx(parent); ok {
        p.mu.Lock()
        if p.err != nil {
            child.cancel(false, p.err, Cause(p))
        } else {
            if p.children == nil {
                p.children = make(map[canceler]struct{})
            }
            p.children[child] = struct{}{} // 注册到父
        }
        p.mu.Unlock()
    } else {
        // 父不是标准 cancelCtx 族，启动 goroutine 桥接取消信号
        go func() {
            select {
            case <-parent.Done():
                child.cancel(false, parent.Err(), Cause(parent))
            case <-child.Done():
            }
        }()
    }
}
```

> 注意最后那段：如果父 Context 是"自定义实现"（非标准 cancelCtx 族），Runtime 会启动一个 goroutine 来桥接取消信号。这有性能开销，所以尽量用标准 `With*` 函数派生，避免自定义 Context。

**工程实践与常见坑**

1. **必须调用 cancel**：`WithCancel` 返回的 cancel 函数必须被调用，否则子 Context 及其 children 会一直留在父节点的 children map 中，导致内存泄漏。推荐 `defer cancel()`。

   ```go
   ctx, cancel := context.WithCancel(parent)
   defer cancel() // 即便任务提前返回也要释放
   ```

2. **cancel 是幂等的**：可被多个 goroutine 安全调用，重复调用不会 panic、不会重复关闭 channel。

3. **不要用 `close(done)` 模拟取消**：自定义 Context 时务必复用 `cancelCtx`，而不是手搓 channel 关闭，否则会破坏传播链。

4. **取消原因（cause）**：Go 1.20+ 新增 `WithCancelCause`、`WithDeadlineCause`、`WithTimeoutCause`，以及 `Cause(ctx)` 函数，可携带更细的取消原因，便于排查"是谁取消了请求"。

   ```go
   ctx, cancel := context.WithCancelCause(parent)
   cancel(fmt.Errorf("upstream 503")) // Cause(ctx) 返回该 err，而 ctx.Err() 仍是 context.Canceled
   ```

5. **cancel 后立即返回**：`cancel()` 是同步的，会递归取消所有 children 后才返回。若 children 链很深，可能耗时；但通常子节点只关闭 channel，开销很小。

### timeout

**是什么**

`context.WithTimeout(parent, d)` 返回一个在 `d` 时间后自动取消的派生 Context。它是对 `WithDeadline` 的封装，把"相对时长"换算成"绝对截止时刻"：

```go
func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc) {
    return WithDeadline(parent, time.Now().Add(timeout))
}
```

典型用法（HTTP 请求超时控制）：

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("request failed:", err) // 可能是 context.DeadlineExceeded
		return
	}
	defer resp.Body.Close()
	fmt.Println("status:", resp.StatusCode)
}
```

**底层结构与 Runtime 实现要点**

`WithTimeout` 直接委托给 `WithDeadline`，二者共用 `timerCtx` 结构体（详见 [deadline](#deadline) 一节）。简化定义：

```go
// timerCtx 在 cancelCtx 基础上叠加一个定时器，到点自动取消
type timerCtx struct {
    cancelCtx             // 嵌入 cancelCtx，获得取消能力
    deadline   time.Time  // 绝对截止时刻
    timer      *time.Timer // 懒初始化的定时器，cancel 时需 Stop
}
```

字段解释：
- `cancelCtx`：嵌入，复用 cancel/级联/children 等全部能力。
- `deadline`：记录截止时刻，`Deadline()` 直接返回它。
- `timer`：`WithDeadline` 创建时调用 `time.AfterFunc(d, func(){ c.cancel(true, DeadlineExceeded, ...) })` 注册。到点触发自动取消；若在到点前手动 `cancel()`，则需 `timer.Stop()` 释放定时器资源。

> 即 timeout 的"自动取消"本质是：Runtime 起一个 timer，到点后调用 `c.cancel(...)`，与手动调 `cancel()` 走同一条路径。

**工程实践与常见坑**

1. **相对时间 vs 绝对时间**：`WithTimeout` 用相对时长，易受系统时钟跳变影响；`WithDeadline` 用绝对时刻，适合"会议 10:00 结束"这类语义。多数业务用 `WithTimeout` 即可。
2. **timeout 仍需 `defer cancel()`**：即使到点自动取消，也必须调用返回的 cancel，否则 timer 资源和 children 引用不会被及时清理（自动取消只清理一次，手动 cancel 负责从父节点摘除自己）。
3. **超时是"截止"不是"中止"**：Context 取消只是发出信号，被取消的函数是否真正返回取决于它是否检查 `ctx.Done()`。一个忽略 ctx 的 `time.Sleep` 不会被超时打断。
4. **不要用 `time.After` 代替 `WithTimeout`**：`time.After` 在 select 未命中时会泄漏 timer 直到触发；`WithTimeout` 配合 `defer cancel()` 能立即释放。

| 写法 | 是否泄漏 | 是否传播取消 |
|------|----------|--------------|
| `select { case <-time.After(d): ... }` | 未命中分支时泄漏至 d 到期 | 否 |
| `select { case <-ctx.Done(): ... }` + `WithTimeout` | `defer cancel()` 后立即释放 | 是 |

### deadline

**是什么**

`context.WithDeadline(parent, d)` 返回一个在绝对时刻 `d` 自动取消的派生 Context。它是 timeout 的底层原语：`WithTimeout` = `WithDeadline(now + d)`。

```go
func WithDeadline(parent Context, d time.Time) (Context, CancelFunc)
```

**底层结构与 Runtime 实现要点**

完整结构 `timerCtx`（Go 1.21）：

```go
type timerCtx struct {
    cancelCtx
    deadline time.Time
    timer    *time.Timer // 在 WithDeadline 中创建
}

func (c *timerCtx) Deadline() (time.Time, bool) {
    return c.deadline, true
}

func (c *timerCtx) cancel(removeFromParent bool, err, cause error) {
    c.cancelCtx.cancel(false, err, cause) // 先做 cancelCtx 的取消逻辑
    if removeFromParent {
        removeChild(c.cancelCtx.Context, c) // 从父节点摘除
    }
    c.mu.Lock()
    if c.timer != nil {
        c.timer.Stop() // 关键：停止尚未触发的 timer，避免泄漏
        c.timer = nil
    }
    c.mu.Unlock()
}
```

`WithDeadline` 的核心逻辑：

```go
func WithDeadline(parent Context, d time.Time) (Context, CancelFunc) {
    // 若父的 deadline 已经更早，直接返回一个 cancelCtx（无需额外 timer）
    if cur, ok := parent.Deadline(); ok && cur.Before(d) {
        return WithCancel(parent)
    }
    c := &timerCtx{
        cancelCtx: newCancelCtx(parent),
        deadline:  d,
    }
    propagateCancel(parent, c) // 注册到父
    dur := time.Until(d)
    if dur <= 0 {
        c.cancel(true, DeadlineExceeded, nil) // 已过期，立即取消
        return c, func() { c.cancel(false, Canceled, nil) }
    }
    c.mu.Lock()
    defer c.mu.Unlock()
    if c.err == nil {
        c.timer = time.AfterFunc(dur, func() {
            c.cancel(true, DeadlineExceeded, nil) // 到点自动取消
        })
    }
    return c, func() { c.cancel(true, Canceled, nil) }
}
```

要点：
1. **父 deadline 更早则退化为 cancelCtx**：避免重复 timer，遵循"最严格的截止时间生效"原则。
2. **`time.AfterFunc`**：把取消动作注册到 Runtime 的 timer 堆（详见 [第16章 Runtime Timer](./16-Timer与Ticker.md#runtime-timer)）。到点 Runtime 在独立 goroutine 执行 `c.cancel(...)`。
3. **手动 cancel 时 Stop timer**：避免 timer 已派发但尚未执行造成的资源悬挂。

**工程实践与常见坑**

1. **deadline 是"硬截止"**：到点必定取消，无法推迟。要"延长"只能新建 Context。
2. **多个 WithDeadline 嵌套取最严**：父 deadline 早于子时，子退化为 cancelCtx，实际生效的是父的 deadline。
3. **不要把 `time.Now()` 算出的 deadline 跨进程传递后直接用**：不同机器时钟可能不同步，跨进程应传递 timeout 时长而非绝对时刻，或使用 NTP 同步。
4. **`DeadlineExceeded` vs `Canceled`**：到点自动取消时 `Err()` 返回 `DeadlineExceeded`；手动调 cancel 返回 `Canceled`。可据此区分"超时"与"主动取消"。

```go
ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
defer cancel()
<-ctx.Done()
switch ctx.Err() {
case context.DeadlineExceeded:
    fmt.Println("超时")
case context.Canceled:
    fmt.Println("被取消")
}
```

### value

**是什么**

`context.WithValue(parent, key, val)` 返回一个携带一对键值的派生 Context，通过 `ctx.Value(key)` 沿父链查找。它只用于传递**请求级**（request-scoped）数据，如 trace ID、request ID、认证 token、租户 ID 等。

```go
func WithValue(parent Context, key, val any) Context
func (c *valueCtx) Value(key any) any
```

**底层结构与 Runtime 实现要点**

```go
// valueCtx 是一条单链表节点，只存一对 key/val，查找沿父链向上
type valueCtx struct {
    Context    // 父 Context
    key, val any
}

func (c *valueCtx) Value(key any) any {
    if c.key == key {
        return c.val
    }
    return c.Context.Value(key) // 递归向上查找
}
```

字段解释：
- `Context`：父节点，链表 next 指针。
- `key, val`：本节点存的一对值。`key` 必须是**可比较类型**（因为要用 `==` 比较）。

查找复杂度：**O(n)**，n 为从当前节点到根的 valueCtx 链长度。这与 map 的 O(1) 不同——之所以用链表而非 map，是因为：
1. value 通常很少（一两个 trace 字段），链表常数更小、内存更省。
2. 链表天然不可变、无锁，与 Context 不可变设计一致。
3. 避免每个 Context 都维护一个 map 的开销。

为了减少 key 冲突并防止别的包读到你的值，**key 必须用自定义未导出类型**：

```go
package main

import (
	"context"
	"fmt"
)

// 推荐写法：用未导出的结构体类型作为 key，杜绝跨包冲突
type ctxKey struct{ name string }

var traceIDKey = ctxKey{"trace_id"}

func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey, id)
}

func TraceID(ctx context.Context) string {
	v, _ := ctx.Value(traceIDKey).(string)
	return v
}

func main() {
	ctx := WithTraceID(context.Background(), "abc-123")
	fmt.Println(TraceID(ctx)) // abc-123
}
```

> 也可以用 `int` / `string` 作为 key，但极易冲突。社区惯例是定义未导出结构体类型 + helper 函数（如上的 `WithTraceID` / `TraceID`），既类型安全又封装了 key。

**工程实践与常见坑**

1. **key 必须可比较**：用 `func`、`map`、`slice` 做 key 会在 `WithValue` 时 panic（运行时检测）。
2. **不要存业务数据**：详见 [为什么不能存业务数据](#为什么不能存业务数据) 一节。
3. **查找是 O(n)**：在深层嵌套链路上频繁 `Value()` 有累积开销，可缓存到局部变量。
4. **类型断言要安全**：`ctx.Value(key)` 返回 `any`，务必用 `v, ok := x.(T)` 形式断言，避免类型不匹配 panic。

| key 类型 | 是否推荐 | 原因 |
|----------|----------|------|
| 未导出结构体 `type ctxKey struct{}` | 推荐 | 无冲突、类型安全 |
| `string` | 不推荐 | 全局命名空间，易冲突 |
| `int` 常量 | 一般 | 需要约定常量值，仍易冲突 |
| `func` / `map` / `slice` | 禁止 | 不可比较，运行时 panic |

### Done Channel

**是什么**

`Done() <-chan struct{}` 是 Context 接口中**最核心**的方法：返回一个只读 channel，当 Context 被取消（手动 cancel 或 deadline 到点）时，该 channel 被**关闭**。所有阻塞在 `<-ctx.Done()` 的 goroutine 会立即被唤醒。返回 `nil` 表示该 Context 永远不会取消（如 `Background`、`TODO`）。

```go
Done() <-chan struct{}
```

**为什么用"关闭 channel"广播取消**

1. **一对多广播**：channel 的 close 会被所有接收者同时感知，无需逐个通知，天然支持"一个父取消 N 个子"。
2. **零值有效**：`struct{}` 不占内存，关闭它只发信号、不带数据。
3. **与 select 天然契合**：可同时监听多个 channel（ctx.Done、结果、超时），是 Go 并发的惯用模式。

`cancelCtx.Done()` 的实现（无锁路径）：

```go
func (c *cancelCtx) Done() <-chan struct{} {
    d := c.done.Load()
    if d != nil {
        return d.(chan struct{})
    }
    c.mu.Lock()
    defer c.mu.Unlock()
    d = c.done.Load()
    if d == nil {
        d = make(chan struct{})
        c.done.Store(d)
    }
    return d.(chan struct{})
}
```

要点：
- 用 `atomic.Value` 懒初始化，保证 `Done()` 多次调用返回**同一个** channel。
- 第一次调用才创建 channel，避免无监听者时白白分配。
- cancel 时关闭这个 channel；若 cancel 时 channel 仍为 nil，则直接存一个**已关闭**的 channel，保证后续 `Done()` 也能收到信号。

典型 select 模式：

```go
func worker(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err() // 收到取消，优雅退出
		case data := <-jobs:
			if err := process(ctx, data); err != nil {
				return err
			}
		}
	}
}
```

**工程实践与常见坑**

1. **不要发送数据到 Done channel**：它是只读的（`<-chan`），且由 Runtime 关闭。手动 close 会 panic。
2. **`Background().Done()` 返回 nil**：直接 `<-ctx.Done()` 在 nil channel 上会**永久阻塞**。务必先判空，或保证 ctx 一定来自 `With*` 派生。
3. **select 中优先检查 Done**：长循环任务每轮都应 select ctx.Done()，否则可能"取消后还在跑"。
4. **Done 关闭后 Err 一定非 nil**：`Err()` 在 Done 关闭前返回 nil，关闭后返回 `Canceled` 或 `DeadlineExceeded`，可用作退出原因日志。

```go
// 错误：nil channel 永久阻塞
func bad(ctx context.Context) {
	<-ctx.Done() // 若 ctx 是 Background，永远卡住
}
```

### Context Tree

**是什么**

Context 本质是一棵**不可变的有向树**：每个派生 Context 持有一个父 Context 引用，`WithCancel` / `WithDeadline` / `WithTimeout` / `WithValue` 都是"在父节点下挂一个子节点"。取消信号沿着树**从父向子**传播，值查找沿着树**从子向父**回溯。

```
            Background
            /        \
     WithTimeout    WithCancel
       /    \          |
    query  dbCall   worker
```

**为什么是树而非链**

一次请求往往派生多个并行子任务（如并行查询多个服务、worker 池），这些子任务共享同一个父，形成树而非链。树形结构让"取消整棵子树"成为 O(子树大小) 的递归操作，且每个分支独立、互不影响。

底层实现：
- **取消向下游传播**：`cancelCtx.children` map 保存所有可取消子节点，`cancel()` 递归遍历。
- **值向上游查找**：`valueCtx.Value(key)` 沿 `Context` 父字段递归。
- **摘除节点**：手动 `cancel()` 时 `removeFromParent=true`，从父的 children map 删除自己，让 GC 回收整棵子树。

```go
// removeChild 把子节点从父的 children 中摘除
func removeChild(parent Context, child canceler) {
    p, ok := parentCancelCtx(parent)
    if !ok {
        return
    }
    p.mu.Lock()
    if p.children != nil {
        delete(p.children, child)
    }
    p.mu.Unlock()
}
```

> 这就是为什么"必须调用 cancel"——它是从父节点摘除子树的唯一入口。不调用 cancel，子树会一直挂在父上，即便任务早已结束。

**工程实践与常见坑**

1. **派生即成树，不要构造环**：Context 父引用是单向的，绝不能让子成为自己的祖先，否则 value 查找会无限递归。标准库的 `With*` 函数已保证无环。
2. **每个子任务派生自己的 ctx**：worker 池里每个 worker 应从父 ctx 派生独立 ctx，避免一个 worker 的取消影响兄弟。

   ```go
   for i := 0; i < n; i++ {
       go func(ctx context.Context) {
           ctx, cancel := context.WithCancel(ctx)
           defer cancel()
           // worker 用自己的 ctx
       }(ctx)
   }
   ```

3. **树的深度别太深**：value 查找是 O(深度)，且深层嵌套可读性差。一般 3~5 层以内。
4. **不要跨 goroutine 共享可变 cancel**：cancel 函数本身线程安全，但"谁负责调用"要有清晰归属，否则容易漏调或重复调。

### 为什么不能存业务数据

**是什么**

Go 官方文档明确要求：Context 的 Value **只用于传递请求级（request-scoped）数据**，如 trace ID、request ID、认证 token、租户 ID、日志字段。**不要**用来传递业务参数、数据库连接、配置对象、用户业务实体等。

> The same Context may be passed to functions running in different goroutines; Contexts are safe for simultaneous use by multiple goroutines. **Use context Values only for request-scoped data that transits processes and APIs, not for passing optional parameters to functions.** —— context 包文档

**为什么这样设计**

1. **类型不安全**：`Value(key any) any` 返回 `any`，必须类型断言，编译期无法检查。业务参数用强类型函数参数更安全。
2. **查找是 O(n)**：valueCtx 是链表，存业务数据后链路变长，频繁查找有性能损耗。
3. **隐式依赖**：把参数塞进 Context，函数签名不再暴露依赖，调用方不知道函数读了哪些值，可测试性、可读性骤降。
4. **生命周期错配**：Context 的生命周期是"请求"，而业务实体（如 User、Order）的生命周期可能更长或更短，强塞会导致语义混乱。
5. **掩盖坏设计**：当一个函数需要从 Context 取 5 个业务参数时，往往说明它该被拆分或重新组织。

对比：

```go
// 反例：把业务参数塞进 context
func ProcessOrder(ctx context.Context) error {
    orderID := ctx.Value("order_id").(string)      // 类型不安全
    userID := ctx.Value("user_id").(string)
    amount := ctx.Value("amount").(float64)
    // ... 调用方根本不知道要塞什么
}

// 正例：显式参数
func ProcessOrder(ctx context.Context, orderID, userID string, amount float64) error {
    // 签名清晰，编译期检查
}
```

**什么数据适合放 Context**

| 数据 | 适合放 Context | 原因 |
|------|----------------|------|
| trace ID / request ID | 是 | 请求级、需跨函数跨进程传递 |
| 认证 token / 用户身份 | 是 | 请求级、贯穿整条调用链 |
| 租户 ID | 是 | 多租户场景的请求级隔离 |
| 日志字段（如 method） | 是 | 请求级 |
| 订单金额、商品列表 | 否 | 业务数据，应显式传参 |
| 数据库连接池 | 否 | 不是请求级，应注入依赖 |
| 配置对象 | 否 | 应用级，不是请求级 |

**工程实践与常见坑**

1. **用 helper 函数封装存取**：避免散落的 `ctx.Value` 调用，集中类型断言逻辑（见 [value](#value) 一节示例）。
2. **trace ID 用中间件注入**：在 HTTP/RPC 入口中间件 `WithValue`，下游统一 `TraceID(ctx)` 读取。
3. **不要用 Context 当依赖注入容器**：需要 DI 用显式构造函数注入，不要借道 Context。
4. **评审红线**：Code Review 时若看到 `ctx.Value("order")`、`ctx.Value("config")` 这类业务键，应直接打回。

### Context 最佳实践

**是什么**

综合前面各节，这里给出一份可直接落地的 Context 使用规范。它源于 Go 官方建议与社区工程经验，写在团队规范里能显著减少 goroutine 泄漏与可读性问题。

**规则清单**

1. **Context 作为函数第一个参数，命名为 `ctx`**：

   ```go
   func DoSomething(ctx context.Context, arg Arg) error
   ```

2. **不要把 Context 存到结构体**（除非结构体本身代表一个请求处理上下文，且生命周期与请求一致）：

   ```go
   // 反例
   type Service struct{ ctx context.Context }
   // 正例
   type Service struct{}
   func (s *Service) Do(ctx context.Context) error
   ```

3. **`WithCancel` / `WithDeadline` / `WithTimeout` 返回的 cancel 必须调用**，用 `defer cancel()`：

   ```go
   ctx, cancel := context.WithTimeout(parent, 5*time.Second)
   defer cancel()
   ```

4. **不要传 `nil` Context**：函数若不确定传什么，用 `context.TODO()`。

5. **不要忽略 `ctx.Done()`**：长循环、阻塞 IO、轮询都要 select ctx.Done()，否则取消信号失效。

   ```go
   for {
       select {
       case <-ctx.Done():
          return ctx.Err()
       case x := <-ch:
          handle(ctx, x)
       }
   }
   ```

6. **Value 只存请求级数据**，且用未导出类型 key + helper 函数封装。

7. **跨进程传递用 timeout 时长而非绝对 deadline**：避免时钟不同步。

8. **HTTP 请求用 `NewRequestWithContext`**：让 client 自动响应取消。

9. **数据库操作传入 ctx**：`sql.DB` 的 `QueryContext` / `ExecContext` 会在 ctx 取消时中断底层查询。

10. **取消原因用 `WithCancelCause` 等带 cause 的 API**：便于排查级联取消的源头。

**完整示例：带超时、取消传播、trace 的请求处理**

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type ctxKey struct{ name string }

var traceKey = ctxKey{"trace"}

func WithTrace(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceKey, id)
}

func Trace(ctx context.Context) string {
	v, _ := ctx.Value(traceKey).(string)
	return v
}

func callUpstream(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream %d", resp.StatusCode)
	}
	return nil
}

func handler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctx = WithTrace(ctx, r.Header.Get("X-Trace-Id"))

	if err := callUpstream(ctx, "https://example.com"); err != nil {
		fmt.Printf("[%s] error: %v\n", Trace(ctx), err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	fmt.Fprintln(w, "ok")
}

func main() {
	http.HandleFunc("/", handler)
	http.ListenAndServe(":8080", nil)
}
```

要点回顾：handler 从 `r.Context()` 拿到根 ctx，注入 trace ID；`callUpstream` 派生带超时的子 ctx，并把它传给 HTTP client；任一层取消（客户端断开、超时、主动 cancel）都会级联到 `http.DefaultClient.Do`，中断底层连接。

**常见反模式速查**

| 反模式 | 后果 | 正确做法 |
|--------|------|----------|
| 不调 cancel | goroutine/内存泄漏 | `defer cancel()` |
| 存 Context 到 struct | 生命周期混乱、测试困难 | 作为方法首参 |
| 用 `time.After` 代替 `WithTimeout` | timer 泄漏 | `WithTimeout` + cancel |
| `ctx.Value("user")` 取业务数据 | 类型不安全、隐式依赖 | 显式参数 |
| 传 `nil` ctx | panic | `context.TODO()` |
| 长循环不检查 `ctx.Done()` | 取消失效 | select ctx.Done() |
| 自定义 Context 手搓 channel | 破坏传播链 | 复用 cancelCtx |

### 本章小结

Context 用一个四方法接口 + 三种实现结构体（`cancelCtx` / `timerCtx` / `valueCtx`），把"取消、超时、截止、请求级数据"统一成树形传播机制：

- **cancel**：`cancelCtx` 用 `atomic.Value` 持有懒初始化的 done channel，用 `children` map 维护可取消子节点，`cancel()` 递归关闭并从父摘除。必须 `defer cancel()`。
- **timeout / deadline**：`timerCtx` 在 `cancelCtx` 上叠加 `time.AfterFunc` 注册的定时器，到点自动 `cancel(DeadlineExceeded)`；`WithTimeout` 是 `WithDeadline(now+d)` 的语法糖。
- **value**：`valueCtx` 是单链表节点，`Value()` 沿父链 O(n) 查找；key 必须用未导出类型避免冲突。
- **Done Channel**：关闭 channel 实现一对多广播，是 select 模式的核心。
- **Context Tree**：不可变树，取消向下游传播、值向上游查找；每个子任务应派生独立 ctx。
- **不存业务数据**：Context 只承载请求级控制流与少量元数据，业务参数务必显式传参。
- **最佳实践**：首参 `ctx`、必调 cancel、不传 nil、不存 struct、长循环必检 Done、Value 用 helper 封装。

掌握这些底层结构后，你能自信地排查"goroutine 泄漏""取消不生效""trace 丢失"等问题，并在团队规范层面守住 Context 的正确用法。
