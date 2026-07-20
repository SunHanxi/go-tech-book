## 第15章 Context（重点）

> 引言：Context 是 Go 并发编程中跨 API 边界传递取消信号、超时、截止时间和请求级数据的标准载体。派生操作建立稳定父链接，内部取消状态通过锁和原子操作安全发布。本章沿 `cancel / timeout / deadline / value` 四条主线，对照 Go 1.26.4 的实现并给出工程边界。

### Context 的设计目标

**是什么**

`context.Context` 是 Go 1.7 正式引入的标准库接口（更早源自 `golang.org/x/net/context`），它定义了在一个进程的 API 调用链中传递**截止时间、取消信号和请求级键值数据**的统一契约。Context 本身不可序列化，也不会自动跨进程传播；HTTP/RPC 库需将 deadline、trace 与身份等选定元数据显式编码到协议，并在服务端构造新 Context。传参与存放约定见本章末的最佳实践清单。

接口自 Go 1.7 起保持稳定；下面对照 Go 1.26.4 的 `src/context/context.go`：

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

两个不可取消的根 Context 分别返回不同的零值类型：

```go
type backgroundCtx struct{ emptyCtx }
type todoCtx struct{ emptyCtx }

func Background() Context { return backgroundCtx{} }
func TODO() Context       { return todoCtx{} }
```

`emptyCtx` 的四个方法都返回零值（`Done()` 返回 `nil`，`Deadline()` 返回 `false`），不会被取消也不存值。`Background()` 与 `TODO()` 是两个不同的不可取消根值，各自都可成为派生关系的起点；进程里并不存在唯一的一棵 Context 树。

**为什么这样设计**

1. **接口最小化、组合最大化**：四个方法各司其职、互不耦合，通过组合不同实现（`cancelCtx`、`timerCtx`、`valueCtx`）来叠加能力，而不是用一个"大而全"的结构体。
2. **派生而非改写父节点**：`WithCancel` / `WithDeadline` / `WithTimeout` / `WithValue` 返回新的 Context，不改变父 Context 的公开语义；新节点自己的取消状态仍是受同步保护的可变状态。
3. **树形传播**：Context 自带父子关系，取消信号从父向子传播，恰好匹配"一次请求派生若干子任务"的拓扑结构。
4. **显式传递而非全局变量**：避免 thread-local 风格的隐式上下文，函数签名暴露依赖，便于测试、追踪、重放。
5. **控制流与数据流分离**：取消信号是"控制流"，Value 是"数据流"，二者共用同一接口但实现解耦——`cancelCtx` 不存值，`valueCtx` 不可取消（它复用父的取消能力）。

> 设计哲学：用显式传递的 Context、可关闭 channel 和稳定父链接表达请求生命周期，不把取消与元数据藏在全局变量或 thread-local 状态中。

底层实现要点：标准包用 `cancelCtx`、`timerCtx`、`valueCtx`、`afterFuncCtx`、`withoutCancelCtx` 等小型实现组合能力。多数派生节点持有父 Context，但 `WithoutCancel` 会显式屏蔽父节点的取消、deadline 和 cause。

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

核心结构 `cancelCtx`（Go 1.26.4，删去与主线无关的细节）：

```go
// cancelCtx 可被取消；嵌入它即获得取消能力
type cancelCtx struct {
    Context                  // 父 Context（嵌入接口字段，形成树）

    mu        sync.Mutex     // 保护下面字段
    done      atomic.Value   // chan struct{}，懒初始化；用 atomic.Value 让 Done() 走无锁路径
    children  map[canceler]struct{} // 子节点集合，cancel 时需要级联取消
    err       atomic.Value   // error；首次取消时设置，Err() 可走原子读快路径
    cause     error          // Cause(ctx) 返回的具体原因，由 mu 保护
}
```

字段解释：
- `Context`：父 Context，形成树。
- `mu`：保护 `children`、`cause` 以及取消过程；`done` 和 `err` 用 `atomic.Value` 提供常用读路径。
- `done`：懒初始化的 channel，关闭它即广播取消。用 `atomic.Value` 而非直接字段，是为了让 `Done()` 在无锁路径下也能安全返回。
- `children`：所有"可取消"子节点的集合（实现了 `canceler` 接口的子 Context）。父取消时遍历并级联取消。
- `err`：取消后保存 `context.Canceled` 或 `context.DeadlineExceeded`；`cause` 另行保留业务原因。

`canceler` 接口（只有 `cancel(removeFromParent bool, err, cause error)` 和 `Done() <-chan struct{}` 两个方法），`cancelCtx` 和 `timerCtx` 都实现了它，因此都能被父节点级联取消。

关键流程 `cancelCtx.cancel(removeFromParent, err, cause)`：
1. 持锁后原子写入 `err`，并设置 `cause`（未提供 cause 时使用 err）。
2. 关闭 `c.done`（如果之前为 nil，则先赋值一个已关闭的 channel，保证幂等）。
3. 遍历 `children`，递归 `cancel(false, err, cause)`（不再从父移除，因为父正在清理）。
4. 若 `removeFromParent` 为 true，从父节点的 children 中移除自己。

`propagateCancel`：在 `WithCancel` 时调用，把自己注册到父节点的 children（若父也是 cancelCtx 族且未取消）；若父已取消，则立即取消子节点。Go 1.21 起它从包级函数改为 `cancelCtx` 的方法 `(c *cancelCtx) propagateCancel(parent, child)`，并在其中完成 `c.Context = parent` 的父链接赋值。这是树形传播的关键：

```go
// 简化的取消传播逻辑（Go 1.26.4 形态）
func (c *cancelCtx) propagateCancel(parent Context, child canceler) {
    c.Context = parent // 建立父链接

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
        if err := p.err.Load(); err != nil {
            child.cancel(false, err.(error), p.cause)
        } else {
            if p.children == nil {
                p.children = make(map[canceler]struct{})
            }
            p.children[child] = struct{}{} // 注册到父
        }
        p.mu.Unlock()
    } else if a, ok := parent.(interface{ AfterFunc(func()) func() bool }); ok {
        // 父实现了 AfterFunc：注册回调，子节点先取消时可停止注册
        stop := a.AfterFunc(func() {
            child.cancel(false, parent.Err(), Cause(parent))
        })
        _ = stop // 真实源码用 stopCtx 保存 stop 函数
    } else {
        // 最后才启动 goroutine 桥接取消信号
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

> 当父节点既不能识别为标准 `cancelCtx`、也不提供 `AfterFunc` 时，`context` 包才会启动一个 goroutine 桥接取消信号。自定义 Context 实现应用运行时 profile 确认这项成本。

**工程实践与常见坑**

1. **必须安排调用 cancel**：如果操作比父 Context 更早结束，不调用 cancel 会让子节点及关联资源留在父链上，直到父取消或 deadline 到期。这会造成不必要的资源滞留；最常见的写法是紧接着 `defer cancel()`。

   ```go
   ctx, cancel := context.WithCancel(parent)
   defer cancel() // 即便任务提前返回也要释放
   ```

2. **cancel 是幂等的**：可被多个 goroutine 安全调用，重复调用不会 panic、不会重复关闭 channel。

3. **不要用业务 channel 伪装 Context**：`cancelCtx` 是标准包未导出实现，用户代码无法直接复用。绝大多数代码应从标准 `WithCancel`/`WithTimeout` 派生；真正自定义 Context 时必须完整遵守四个方法的并发契约。

4. **取消原因（cause）**：Go 1.20 新增 `WithCancelCause` 和 `Cause`，Go 1.21 增加 `WithDeadlineCause`、`WithTimeoutCause`，可保留更具体的取消原因。

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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
	if err != nil {
		fmt.Println("build request:", err)
		return
	}
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

1. **相对预算 vs 共享截止时刻**：`WithTimeout` 适合“从现在起最多运行 d”，`WithDeadline` 适合同一进程调用链共享已有截止时刻。`time.Now().Add(d)` 会保留单调时钟分量，因此在同一进程内不应简化为“WithTimeout 更容易受墙上时钟跳变”。序列化到跨进程协议后单调分量会丢失，远端应重新计算可用预算并考虑网络开销。
2. **timeout 仍需 `defer cancel()`**：如果操作在 deadline 前结束，及时调用 cancel 会停止回调并从父链移除子节点；不应等到 deadline 才回收这些关联资源。
3. **超时是"截止"不是"中止"**：Context 取消只是发出信号，被取消的函数是否真正返回取决于它是否检查 `ctx.Done()`。一个忽略 ctx 的 `time.Sleep` 不会被超时打断。
4. **请求链优先 `WithTimeout`**：它能把 deadline 和取消向下游传播；`time.After` 只提供本地事件。Go 1.23+ 的不可达 Timer 可被 GC 回收，但高频创建仍有分配成本。

| 写法 | 生命周期 | 是否传播取消 |
|------|----------|--------------|
| `select { case <-time.After(d): ... }` | Go 1.23+ 可回收，但每次创建新 Timer | 否 |
| `select { case <-ctx.Done(): ... }` + `WithTimeout` | `cancel()` 及时从父链移除并停止关联 timer | 是 |

### deadline

**是什么**

`context.WithDeadline(parent, d)` 返回一个在绝对时刻 `d` 自动取消的派生 Context。它是 timeout 的底层原语：`WithTimeout` = `WithDeadline(now + d)`。

```go
func WithDeadline(parent Context, d time.Time) (Context, CancelFunc)
```

**底层结构与 Runtime 实现要点**

结构与取消路径（Go 1.26.4）：

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
        c.timer.Stop() // 防止尚未触发的回调继续占用资源
        c.timer = nil
    }
    c.mu.Unlock()
}
```

`WithDeadline` 的核心逻辑：

```go
// Go 1.21 起 WithDeadline 委托给 WithDeadlineCause(parent, d, nil)
func WithDeadlineCause(parent Context, d time.Time, cause error) (Context, CancelFunc) {
    // 若父的 deadline 已经更早，直接返回一个 cancelCtx（无需额外 timer）
    if cur, ok := parent.Deadline(); ok && cur.Before(d) {
        return WithCancel(parent)
    }
    c := &timerCtx{deadline: d}
    c.cancelCtx.propagateCancel(parent, c) // 建立父链接并注册到父
    dur := time.Until(d)
    if dur <= 0 {
        c.cancel(true, DeadlineExceeded, cause) // 已过期，立即取消
        return c, func() { c.cancel(false, Canceled, nil) }
    }
    c.mu.Lock()
    defer c.mu.Unlock()
    if c.err.Load() == nil {
        c.timer = time.AfterFunc(dur, func() {
            c.cancel(true, DeadlineExceeded, cause) // 到点自动取消
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

1. **deadline 是最晚意图，不是实时保证**：计时器到期后会安排取消，但 `Done` 关闭和业务 goroutine 观察到它都可受调度延迟。已派生 Context 的 deadline 不可就地延长，需要新建派生 Context。
2. **多个 WithDeadline 嵌套取最严**：父 deadline 早于子时，子退化为 cancelCtx，实际生效的是父的 deadline。
3. **Context 本身不跨进程**：协议可传递剩余预算或绝对时刻，两者分别受网络耗时和机器时钟偏差影响。RPC 框架应定义明确协议并在远端派生新 Context，而不序列化 Go Context 对象。
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

### AfterFunc 与 WithoutCancel（Go 1.21+）

**是什么**

Go 1.21 为 context 包新增了两个补齐生命周期表达能力的 API：

```go
func AfterFunc(ctx Context, f func()) (stop func() bool)
func WithoutCancel(parent Context) Context
```

**AfterFunc：取消时执行回调**

`AfterFunc(ctx, f)` 安排在 `ctx` 被取消后，在**独立 goroutine** 中调用 `f`；若 `ctx` 已经取消，则立即（仍在独立 goroutine 中）调用。多次对同一 ctx 调用 AfterFunc 相互独立，不会互相覆盖。

返回的 `stop` 函数语义需要精确理解（对照 Go 1.26.4 文档）：

- `stop()` 返回 `true`：成功解除关联，`f` 保证不会再被运行。
- `stop()` 返回 `false`：要么 ctx 已取消且 `f` 已在自己的 goroutine 中**启动**，要么 `f` 已被先前的 stop 停止。
- `stop` **不等待** `f` 执行完成；若调用方需要知道 `f` 是否已结束，必须自行与 `f` 协调（如 WaitGroup 或 done channel）。

这与 `time.Timer.Stop` 的返回值语义同构。典型用途是把 Context 取消桥接到不认识 Context 的等待原语上，例如唤醒 `sync.Cond`：

```go
stop := context.AfterFunc(ctx, func() {
    cond.Broadcast() // ctx 取消时唤醒等待者
})
defer stop()
```

标准库的 `propagateCancel` 也会优先利用父 Context 的 `AfterFunc(func()) func() bool` 方法来桥接自定义 Context 的取消信号，避免额外 goroutine（见上文 cancel 一节）。

**WithoutCancel：脱离父取消的收尾工作**

`WithoutCancel(parent)` 返回一个仍指向父节点的派生 Context：它保留父链上的全部 Value（trace ID、认证信息等继续可见），但**不会**随父取消——`Deadline()` 返回零值、`Err()` 恒为 nil、`Done()` 返回 nil channel、`Cause` 返回 nil。

典型场景是"请求已结束，但收尾必须完成"的任务：写审计日志、上报 metrics、异步落盘。这些操作不应因请求 Context 取消而半途而废，但仍需要请求的元数据：

```go
func handler(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()
    result := process(ctx)

    // 收尾工作：脱离请求取消，但保留 trace 等 Value，并自设超时
    bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
    go func() {
        defer cancel()
        auditLog(bgCtx, result)
    }()
}
```

注意 `WithoutCancel` 之后通常应重新加上自己的超时，否则收尾任务会失去一切时间约束。

**context.Cause 在超时场景的行为**

`Cause(ctx)` 优先返回取消时记录的 cause；超时场景下：

```go
// 普通 WithTimeout：cause 就是 DeadlineExceeded
ctx1, cancel1 := context.WithTimeout(parent, time.Millisecond)
defer cancel1()
<-ctx1.Done()
fmt.Println(context.Cause(ctx1)) // context deadline exceeded

// WithDeadlineCause / WithTimeoutCause：deadline 到期时返回指定 cause
ctx2, cancel2 := context.WithTimeoutCause(parent, time.Millisecond,
    errors.New("配置中心响应预算耗尽"))
defer cancel2()
<-ctx2.Done()
fmt.Println(ctx2.Err())          // context deadline exceeded（Err 不变）
fmt.Println(context.Cause(ctx2)) // 配置中心响应预算耗尽
```

即 `Err()` 始终是标准哨兵错误（便于 `errors.Is` 判断），而 `Cause` 承载更具体的业务原因；注意 `WithDeadlineCause` / `WithTimeoutCause` 的 cause 只在 **deadline 到期**时生效，手动调用返回的 cancel 仍记录 `Canceled`。

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

`Done() <-chan struct{}` 返回一个只读 channel；Context 被取消时该 channel 被关闭。等待者会变为可运行，但何时实际执行仍由调度器决定。返回 `nil` 表示该 Context 不会自行取消（如 `Background`、`TODO`）。

```go
Done() <-chan struct{}
```

**为什么用"关闭 channel"广播取消**

1. **一对多广播**：channel 的 close 会被所有接收者同时感知，无需逐个通知，天然支持"一个父取消 N 个子"。
2. **无数据载荷**：`struct{}` 的值大小为零；channel 自身仍有分配与同步成本，关闭操作只表达信号、不携带结果。
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
        case data, ok := <-jobs:
            if !ok {
                return nil
            }
            if err := process(ctx, data); err != nil {
				return err
			}
		}
	}
}
```

**工程实践与常见坑**

1. **不要把 Done 当数据 channel**：它是只读的（`<-chan`），由 Context 实现在取消时关闭。调用方既不能向它发送，也不拥有关闭权。
2. **不可取消 Context 的 Done 返回 nil**：直接从 nil channel 接收会永久阻塞。通常把 `ctx.Done()` 放入带其他 case 的 select，nil case 会自动禁用；若要单独等待则先判空。不能仅凭“来自 With*”判断，因为 `WithoutCancel` 和自定义 Context 也可能返回 nil。
3. **select 不提供 case 优先级**：应把 Done 纳入长循环的等待集合，但当 Done 与工作 case 同时就绪时，select 会伪随机选择。需要停止接收新任务时，可在协议层关闭输入、分阶段检查取消，并让处理逻辑可幂等退出。
4. **Done 关闭后 Err 一定非 nil**：`Err()` 在 Done 关闭前返回 nil，关闭后返回 `Canceled` 或 `DeadlineExceeded`，可用作退出原因日志。

```go
// 错误：nil channel 永久阻塞
func bad(ctx context.Context) {
	<-ctx.Done() // 若 ctx 是 Background，永远卡住
}
```

### Context Tree

**是什么**

Context 派生后形成父链接；这些链接和 value 节点不会改写，但 `cancelCtx` 的取消状态、children 集合和 timer 是受同步保护的可变状态。因此更准确的说法是“派生关系稳定、取消状态并发安全”，而不是把整个实现笼统称为不可变树。取消信号沿派生关系**从父向子**传播，值查找沿父链**从子向父**回溯。

```
            Background
            /        \
     WithTimeout    WithCancel
       /    \          |
    query  dbCall   worker
```

**为什么取消关系会分叉**

一次请求往往派生多个并行子任务，这些任务共享同一个父，因此取消关系会分叉。可取消子节点会登记到可识别的父 `cancelCtx`；父取消时遍历这些 children。仅包装 value 的节点不需要单独登记，它通过父 Context 继承取消能力。

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

> 及时调用 cancel 可以把 child 从父的 children 中摘除，并停止关联 timer。若不调用，资源可能保留到父取消或自身 deadline 到期；它不等同于每次都会新建一个 goroutine，也不能笼统写成“必然 goroutine 泄漏”。

**工程实践与常见坑**

1. **Context 可并发共享**：多个 worker 可以安全接收同一个父 Context。只有需要更窄 deadline、独立取消原因或新增请求级 value 时才派生，不要为“每个 goroutine 必须有自己的 ctx”制造无意义节点。
2. **控制链深度与 Value 数量**：Value 查找沿父链进行；更重要的是，过多包装通常意味着隐式依赖和预算规则难以审查。没有通用的固定层数上限。
3. **明确 cancel 所有权**：CancelFunc 可被多个 goroutine 并发调用且重复调用无副作用，但团队仍应约定谁在工作结束时负责调用，避免过早取消或漏调。
4. **自定义实现要谨慎**：标准 `With*` 函数建立无环父链。自定义 Context 必须保证方法可并发调用、Done/Err 语义一致，并避免递归父链；绝大多数业务不需要自定义。

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

3. **不再需要派生 Context 时及时调用 cancel**。通常在检查 error 后立即 `defer cancel()`；只有把 cancel 所有权明确交给调用者时才由对方负责：

   ```go
   ctx, cancel := context.WithTimeout(parent, 5*time.Second)
   defer cancel()
   ```

4. **不要传 `nil` Context**：函数若不确定传什么，用 `context.TODO()`。

5. **让长任务和阻塞 API 响应取消**：循环可 select `ctx.Done()`；IO、数据库和 RPC 应调用接收 Context 或 deadline 的 API。一个已经阻塞在不支持取消的函数中的 goroutine，无法靠外层 select 强行中断。

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

7. **跨进程只传协议定义的预算信息**：绝对 deadline 受机器时钟偏差影响，剩余 timeout 会在转发时重新起算并可能遗漏已消耗时间。优先沿用 RPC 框架的标准传播规则，在远端扣除传输耗时和安全余量后派生新的本地 Context。

8. **HTTP 请求用 `NewRequestWithContext`**：让 client 自动响应取消。

9. **数据库操作传入 ctx**：`sql.DB` 的 `QueryContext` / `ExecContext` 会请求取消等待或查询；底层 driver 与数据库协议决定正在执行的操作能多快停止。

10. **取消原因用 `WithCancelCause` 等带 cause 的 API**：便于排查级联取消的源头。

**完整示例：带超时、取消传播、trace 的请求处理**

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build upstream request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("call upstream: %w", err)
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
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	fmt.Fprintln(w, "ok")
}

func main() {
	server := &http.Server{
		Addr:              ":8080",
		Handler:           http.HandlerFunc(handler),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServe();
		err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
```

要点回顾：handler 从 `r.Context()` 拿到根 ctx，注入 trace ID；`callUpstream` 派生带超时的子 ctx，并把它传给 HTTP client；任一层取消（客户端断开、超时、主动 cancel）都会级联到 `http.DefaultClient.Do`，终止该请求的等待或读写。具体 Transport 可能关闭 HTTP/1.x 连接，也可能只重置 HTTP/2 流，不应依赖物理连接一定关闭。

**常见反模式速查**

| 反模式 | 后果 | 正确做法 |
|--------|------|----------|
| 不调 cancel | child 引用、timer 等资源可能保留到父取消或 deadline | 任务结束即调用 cancel |
| 存 Context 到 struct | 生命周期混乱、测试困难 | 作为方法首参 |
| 请求链用 `time.After` 代替 `WithTimeout` | deadline 无法向下游传播 | `WithTimeout` + cancel |
| `ctx.Value("user")` 取业务数据 | 类型不安全、隐式依赖 | 显式参数 |
| 传 `nil` ctx | panic | `context.TODO()` |
| 长循环不检查 `ctx.Done()` | 取消失效 | select ctx.Done() |
| 无必要地自定义 Context | 传播与并发语义容易不完整 | 从标准 Context 派生 |

### 本章小结

Context 用一个四方法接口和多个小型实现（`emptyCtx`、`cancelCtx`、`timerCtx`、`valueCtx`、`afterFuncCtx`、`withoutCancelCtx` 等），组合“取消、超时、截止、回调和请求级数据”：

- **cancel**：Go 1.26.4 的 `cancelCtx` 用两个 `atomic.Value` 分别懒初始化 done channel、发布 `Err`，并在锁下维护 `children` 与 `cause`。`cancel()` 关闭 done、级联取消子节点，并按需从父节点摘除；调用方应在任务结束时及时调用返回的 cancel。
- **timeout / deadline**：`timerCtx` 在 `cancelCtx` 上叠加 `time.AfterFunc` 注册的定时器，到点自动 `cancel(DeadlineExceeded)`；`WithTimeout` 是 `WithDeadline(now+d)` 的语法糖。
- **value**：`valueCtx` 是单链表节点，`Value()` 沿父链 O(n) 查找；key 必须用未导出类型避免冲突。
- **Done Channel**：关闭 channel 实现一对多广播，是 select 模式的核心。
- **Context 派生关系**：父链接稳定，取消状态受同步保护；取消向下游传播，值沿父链查找。Context 可被多个 goroutine 共享，不要求每个子任务都派生新节点。
- **不存业务数据**：Context 只承载请求级控制流与少量元数据，业务参数务必显式传参。
- **最佳实践**：首参 `ctx`、必调 cancel、不传 nil、不存 struct、长循环必检 Done、Value 用 helper 封装。

掌握这些底层结构后，可以区分“未及时释放派生资源”“工作函数不响应取消”和“永久阻塞 goroutine”三类问题，并排查取消不生效、deadline 丢失和 trace 元数据缺失。
