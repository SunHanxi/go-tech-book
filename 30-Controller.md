## 第30章 Controller

> Informer 解决了"如何拿到资源"的问题，Controller 解决了"如何让资源状态收敛到期望"的问题。本章围绕 controller-runtime 的 Reconcile 模式，剖析 WorkQueue、RateLimiter、Retry、Context 的源码要点，把"声明式 + 水平触发 + 幂等重试"的控制器内核讲透。

### Reconcile

Reconcile（调和）是 controller-runtime 时代 Kubernetes 控制器的核心抽象：给定一个对象 key，让对象的实际状态向期望状态收敛。它是水平触发（level-triggered）的，不关心"发生了什么变化"，只关心"现在该不该是这个样子"。

#### 是什么

`Reconciler` 是 controller-runtime（`sigs.k8s.io/controller-runtime`）定义的一个接口：

```go
type Reconciler interface {
    Reconcile(context.Context, Request) (Result, error)
}

type Request struct {
    NamespacedName types.NamespacedName `json:"namespacedName"`
}

type Result struct {
    Requeue      bool          // 显式要求重新入队
    RequeueAfter time.Duration // 延迟一段时间后重新入队
}
```

字段说明：

| 字段 | 含义 |
|---|---|
| `Request.NamespacedName` | 触发 Reconcile 的对象 namespace/name，唯一标识 |
| `Result.Requeue` | true 表示"我处理完了但请再调和一次"，相当于立刻再排队（受速率限制） |
| `Result.RequeueAfter` | 大于 0 表示延迟 N 后再调和（用于等待外部系统就绪、轮询） |
| 返回值 `error` | 非 nil 表示"我失败了，请按退避策略重试" |

#### 工作原理与源码要点

controller-runtime 的 Controller 接口实现是 `controller.Controller`，内部组合了 client-go 的 Informer、workqueue 和 Reconciler。控制循环可以用下面这张 ASCII 图描述：

```
     +-----------------+
     |  Kubernetes API |
     +--------+--------+
              |
     Watch    |   Events (Add/Update/Delete)
              v
     +-----------------+
     |  EventHandler   |  Enqueue(obj) -> 计算 key -> workqueue.Add
     +--------+--------+
              |
              v
     +-----------------+
     |   WorkQueue     |  RateLimited + Delayed
     +--------+--------+
              |
              | Get(key)
              v
     +-----------------+
     |   Reconcile     |  读缓存 -> 计算期望 -> 调 API
     +--------+--------+
              |
              | Result{Requeue, RequeueAfter}, error
              v
     +-----------------+
     |   再入队决策     |  err  -> AddRateLimited
     +-----------------+  RequeueAfter -> AddAfter
                          Requeue=true -> AddRateLimited
                          都没有 -> Forget (成功)
```

controller-runtime 内部核心循环（简化自 `pkg/internal/controller/controller.go`）：

```go
func (c *Controller) processNextWorkItem(ctx context.Context) bool {
    obj, shutdown := c.Queue.Get()
    if shutdown {
        return false
    }
    defer c.Queue.Done(obj)

    req, ok := obj.(reconcile.Request)
    if !ok {
        c.Queue.Forget(obj)
        return true
    }

    result, err := c.Reconcile.Handle(ctx, req)
    switch {
    case err != nil:
        // 失败：按速率限制重试
        c.Queue.AddRateLimited(req)
        log.Error(err, "Reconciler error")
    case result.RequeueAfter > 0:
        // 显式延迟：Forget 后 AddAfter，不计入重试次数
        c.Queue.Forget(obj)
        c.Queue.AddAfter(req, result.RequeueAfter)
    case result.Requeue:
        // 显式立即重试：也走速率限制
        c.Queue.AddRateLimited(req)
    default:
        // 成功：Forget 重置计数器
        c.Queue.Forget(obj)
    }
    return true
}
```

要点：

- **水平触发**：Reconcile 拿到 key 后，去缓存里 Get 对象，根据当前状态决策。即便中间漏掉了 N 个事件，最终只要触发一次 Reconcile，状态就能收敛。这与传统的"事件回调"（edge-triggered）截然不同。
- **幂等**：Reconcile 必须可重入。同一个 key 可能因为 Resync、Requeue、Watch 重连被反复调和。
- **不返回 error 也要重试**：返回 `Result{RequeueAfter: 30*time.Second}` 是"成功但稍后再调"，比如等待 Job 完成。
- **controller-runtime 的 Handle**：实际调用 `Reconciler.Reconcile` 时还会注入 metrics、tracing、recover panic，避免单次 Reconcile panic 拖垮整个 controller。

#### 工程实践与常见坑

- **Reconcile 里不要 Watch**：Reconcile 是消费者，不应再发起订阅。需要的依赖应该在 Builder 阶段用 `Watches` 注册，依赖对象的变更通过 `MapTo`/`EnqueueRequestForOwner` 转换成本对象的 key。

- **每次 Reconcile 时长要可控**：controller-runtime 默认没有单次超时，长 Reconcile 会阻塞 worker。建议在 Reconcile 内部用 `context.WithTimeout`，或者 Manager 设置 `MaxConcurrentReconciles` 控制并发。

- **`RequeueAfter` 不要短于外部系统实际就绪时间**：比如等一个 Deployment ready 通常 30s 起，`RequeueAfter: 1*time.Second` 只会刷爆 workqueue。

- **不要在 Reconcile 里 sleep**：阻塞 worker。改用 `RequeueAfter` 让出执行权。

- **用 Finalizer 处理删除**：删除事件先到，但对象可能还有外部资源没清理。Reconcile 里判断 `DeletionTimestamp != nil`，执行清理后 remove finalizer，否则对象永远删不掉。

- **MaxConcurrentReconciles 不是越大越好**：每多一个 worker，对 API Server 的并发压力就多一份。一般 1（默认）够用，IO 密集场景调到 5~10。

- **`Requeue: true` 与 `RequeueAfter` 互斥**：同时设置时 `RequeueAfter` 生效（看上面的 switch case 顺序），别误以为两个都会触发。

### WorkQueue

WorkQueue 是控制器的"任务队列"，把 Informer 的事件回调与 Reconcile 解耦。它的核心特性是去重、限速、延迟入队，保证同一个对象不会被并发处理。

#### 是什么

`workqueue` 位于 `k8s.io/client-go/util/workqueue`，按功能分三层接口：

```go
type Interface interface {
    Add(item interface{})
    Len() int
    Get() (item interface{}, shutdown bool)
    Done(item interface{})
    ShutDown()
    ShuttingDown() bool
}

type DelayingInterface interface {
    Interface
    AddAfter(item interface{}, duration time.Duration)
}

type RateLimitingInterface interface {
    DelayingInterface
    AddRateLimited(item interface{})
    Forget(item interface{})
    NumRequeues(item interface{}) int
}
```

- `Interface`：基本 FIFO + 去重。
- `DelayingInterface`：支持延迟入队（最小堆实现）。
- `RateLimitingInterface`：在 Delaying 之上加按 item 的速率限制。

#### 关键结构体

基本队列 `Type`：

```go
type Type struct {
    queue []t                       // FIFO 队列，存 key

    dirty set                       // 已经 Add 但还没 Get 的 key 集合
    processing set                  // 正在被处理的 key 集合

    cond *sync.Cond
    shuttingDown bool

    metrics queueMetrics
}
```

字段说明：

| 字段 | 含义 |
|---|---|
| `queue` | 真正的 FIFO 数组 |
| `dirty` | "在队列里或刚出队还没 Done"的 key 集合，用于去重 |
| `processing` | "已经 Get 但还没 Done"的 key 集合，用于并发保护 |
| `cond` | 条件变量，Get 时如果队列空就 Wait |
| `shuttingDown` | 关停标志，关停后 Get 返回 shutdown=true |

`Add` 的关键去重逻辑：

```go
func (q *Type) Add(item interface{}) {
    q.cond.L.Lock()
    defer q.cond.L.Unlock()
    if q.shuttingDown {
        return
    }
    if q.dirty.has(item) {
        // 已经在队列里，或正在被处理（处理时会留在 dirty 直到 Done）
        return
    }
    q.metrics.add(item)
    q.dirty.insert(item)
    if q.processing.has(item) {
        // 正在被处理：不立刻入队，等 Done 时统一 requeue
        return
    }
    q.queue = append(q.queue, item)
    q.cond.Signal()
}

func (q *Type) Done(item interface{}) {
    q.cond.L.Lock()
    defer q.cond.L.Unlock()
    q.metrics.done(item)
    q.processing.delete(item)
    if q.dirty.has(item) {
        // 在处理期间又有新的 Add，此时重新入队
        q.queue = append(q.queue, item)
        q.cond.Signal()
    } else {
        q.dirty.delete(item)
    }
}
```

延迟队列 `delayingType`：

```go
type delayingType struct {
    Interface
    clock clock.Clock
    heartbeat clock.Ticker
    waitingForAddCh chan *waitFor  // AddAfter 写入
    stopCh chan struct{}

    // 延迟队列（最小堆），按 readyAt 排序
    waitForPriorityQueue
    waitingForByToken map[string]*waitFor  // 去重
}
```

字段说明：

| 字段 | 含义 |
|---|---|
| `waitingForAddCh` | AddAfter 的入口 channel，避免直接操作堆 |
| `waitForPriorityQueue` | 最小堆，按 `readyAt` 排序 |
| `waitingForByToken` | 同 key 去重，新延迟会更新已存在的 entry |

`waitFor` 结构：

```go
type waitFor struct {
    data    t                // 实际 item
    readyAt time.Time        // 何时可以入队
    index   int              // 在堆中的位置
}
```

#### 工作原理

整体流程：

```
EventHandler.OnAdd(obj)  ┐
EventHandler.OnUpdate    ├──> q.Add(key)  ─────────────┐
EventHandler.OnDelete    ┘                              │
                                                        v
Reconcile 返回 RequeueAfter  ──> q.AddAfter(key, dur) ──> 延迟堆
Reconcile 返回 error         ──> q.AddRateLimited(key) ─┘
                                                        |
                                                        v
                                              +--------------------+
                                              |  worker goroutine  |
                                              |   for { Get();     |
                                              |     Reconcile();   |
                                              |     Done(); }      |
                                              +--------------------+
```

延迟队列的工作循环（简化）：

```go
func (q *delayingType) waitingLoop() {
    for {
        now := q.clock.Now()
        // 1. 把堆顶已到 readyAt 的 entry 取出，Add 到基本队列
        for q.waitForPriorityQueue.Len() > 0 {
            head := q.waitForPriorityQueue.Peek().(*waitFor)
            if head.readyAt.After(now) {
                break
            }
            pop := heap.Pop(&q.waitForPriorityQueue).(*waitFor)
            q.Add(pop.data)
            delete(q.waitingForByToken, token)
        }
        // 2. 计算下次唤醒时间
        nextReadyAt := ...
        select {
        case <-q.stopCh:
            return
        case <-q.heartbeat.C():
            // 周期性 tick，重新检查
        case wait := <-q.waitingForAddCh:
            if wait.readyAt.After(q.clock.Now()) {
                heap.Push(&q.waitForPriorityQueue, wait)
            } else {
                q.Add(wait.data)
            }
        }
    }
}
```

要点：

- **去重三态**：`dirty`（在队列里或待 requeue）、`processing`（正在处理）。`Add` 时如果 key 在 `dirty` 里直接丢弃；如果 key 在 `processing` 里，也丢弃，但 `dirty` 里会保留，等 `Done` 时再入队。这样保证"正在处理期间收到的新事件不会丢，但也不会重复入队"。
- **延迟堆的精度**：heartbeat 默认 10ms，对绝大多数控制器够用；但如果你 `AddAfter(1ms)`，实际可能 10ms 后才入队。
- **`AddRateLimited` 内部就是 `AddAfter + RateLimiter`**：先调 `When(item)` 算延迟，再 `AddAfter`。

#### 工程实践与常见坑

- **同一 key 不会并发 Reconcile**：因为 `processing` 集合的存在，一个 key 在被处理期间再次 Add 只会被记到 `dirty`，不会立刻入队。这是控制器串行化保证。

- **`Done` 必须被调用**：否则 `processing` 永远不清空，key 永远不会被重新入队。controller-runtime 的循环里用 `defer c.Queue.Done(obj)` 保证。

- **不要直接 `Add` 一个正在 Reconcile 的 key**：会被去重。如果确实想强制重新调和，应该用 `RequeueAfter: time.Millisecond`（绕过 dirty 检查）。但通常这是反模式。

- **`ShutDown` 会丢弃未处理项**：controller 退出时，未 Get 的 item 会丢失。如果 Reconcile 有外部副作用，要靠 Finalizer 保证最终一致性。

- **队列深度监控**：`workqueue_depth`、`workqueue_adds_total`、`workqueue_retries_total` 是核心指标，发现 depth 持续上涨说明 Reconcile 跟不上事件速率。

- **不要用 workqueue 做业务队列**：它是为控制器内部事件去重设计的，吞吐量、持久化都不适合业务消息流。

- **`ShutDownWithDrain` 与 `ShutDown` 的区别**：前者会等所有正在处理的 item 被 Done，后者不等待。controller-runtime 默认用 `ShutDownWithDrain` 保证优雅停机。

### RateLimiter

RateLimiter 决定"同一个 item 重试时的退避策略"。它是控制器面对失败时保护 API Server 的关键防线。

#### 是什么

`RateLimiter` 接口位于 `k8s.io/client-go/util/workqueue/default_rate_limiters.go`：

```go
type RateLimiter interface {
    When(item interface{}) time.Duration  // 该 item 应该等多久再入队
    Forget(item interface{})              // 该 item 处理成功，重置计数
    NumRequeues(item interface{}) int     // 该 item 已经重试了多少次
}
```

client-go 内置了几种实现：

| 实现 | 退避策略 | 适用场景 |
|---|---|---|
| `BucketRateLimiter` | 全局令牌桶，所有 item 共享 | 限流外部 API 调用频率 |
| `ItemExponentialFailureRateLimiter` | 按 item 指数退避 `base * 2^retries`，封顶 `max` | 默认推荐 |
| `ItemFastSlowRateLimiter` | 前 fast 个重试用 fastDelay，之后用 slowDelay | 已知"瞬时失败多"的场景 |
| `MaxOfRateLimiter` | 多个 limiter 取最大值 | 组合使用 |
| `DefaultControllerRateLimiter` | `MaxOf(Bucket(10qps,100), Exponential(5ms,1000s))` | controller-runtime 默认 |

#### 关键结构体

**ItemExponentialFailureRateLimiter**：

```go
type ItemExponentialFailureRateLimiter struct {
    failures     map[interface{}]int   // item -> 失败次数
    failuresLock sync.Mutex
    baseDelay    time.Duration         // 初始延迟
    maxDelay     time.Duration         // 最大延迟
}

func (r *ItemExponentialFailureRateLimiter) When(item interface{}) time.Duration {
    r.failuresLock.Lock()
    defer r.failuresLock.Unlock()
    exp := r.failures[item]
    r.failures[item] = r.failures[item] + 1

    // 指数退避：base * 2^exp，封顶 maxDelay
    backoff := float64(r.baseDelay.Nanoseconds()) * math.Pow(2, float64(exp))
    if backoff > math.MaxInt64 {
        return r.maxDelay
    }
    calculated := time.Duration(backoff)
    if calculated > r.maxDelay {
        return r.maxDelay
    }
    return calculated
}
```

字段说明：

| 字段 | 含义 |
|---|---|
| `failures` | 每个 item 的失败次数计数 |
| `baseDelay` | 第一次重试的延迟（默认 5ms） |
| `maxDelay` | 单次重试的最大延迟（默认 1000s） |

**BucketRateLimiter**（基于 `golang.org/x/time/rate`）：

```go
type BucketRateLimiter struct {
    *rate.Limiter
}

func NewBucketRateLimiter(qps float32, bucketSize int) *BucketRateLimiter {
    return &BucketRateLimiter{rate.NewLimiter(rate.Limit(qps), bucketSize)}
}

func (r *BucketRateLimiter) When(item interface{}) time.Duration {
    return r.Limiter.Reserve().Delay()
}
```

字段说明：

| 字段 | 含义 |
|---|---|
| `Limiter` | `golang.org/x/time/rate` 的令牌桶，`qps` 是填充速率，`bucketSize` 是桶容量 |
| `When` 行为 | `Reserve()` 预订一个令牌，返回需要等待的时间；item 之间共享令牌 |

**MaxOfRateLimiter**：

```go
type MaxOfRateLimiter struct {
    limiters []RateLimiter
}

func (r *MaxOfRateLimiter) When(item interface{}) time.Duration {
    var ret time.Duration
    for _, limiter := range r.limiters {
        curr := limiter.When(item)
        if curr > ret {
            ret = curr
        }
    }
    return ret
}

func (r *MaxOfRateLimiter) Forget(item interface{}) {
    for _, limiter := range r.limiters {
        limiter.Forget(item)
    }
}
```

#### 工作原理

`DefaultControllerRateLimiter` 是组合策略：

```go
func DefaultControllerRateLimiter() RateLimiter {
    return NewMaxOfRateLimiter(
        NewBucketRateLimiter(10, 100),                   // 全局 10 QPS，桶 100
        NewItemExponentialFailureRateLimiter(5*time.Millisecond, 1000*time.Second),
    )
}
```

含义：

- **全局**：所有 item 共享 10 QPS、桶 100 的令牌桶，防止整体打爆 API Server。
- **单 item**：指数退避，从 5ms 起步，每次翻倍，封顶 1000s。第 N 次重试的延迟约为 `5ms * 2^N`。
- **取最大值**：实际入队延迟 = max(全局令牌桶延迟, 单 item 指数延迟)。

退避序列示例（base=5ms, max=1000s）：

| 重试次数 | 延迟 |
|---|---|
| 1 | 10ms |
| 2 | 20ms |
| 3 | 40ms |
| 4 | 80ms |
| 5 | 160ms |
| 10 | ~5s |
| 15 | ~2.7min |
| 20 | 1000s（封顶） |

#### 工程实践与常见坑

- **默认 1000s 封顶太大**：生产中如果一个对象已经重试 20 次还失败，通常意味着永久性错误（如配置错误、依赖服务挂了），让它每 16 分钟重试一次意义不大。建议把 `maxDelay` 调到 5~10 分钟，配合告警。

- **BucketRateLimiter 是全局共享的**：所有 controller 用同一个 client 时，多个 controller 的 workqueue 限流是独立的，但底层 API Server 的限流是共享的。要按 client 维度做限流，参考 `client-go/rest` 的 `QPS` / `Burst` 配置。

- **`Forget` 必须在成功时调用**：否则 `failures` 计数永远不重置，下次失败会从很高的退避起步。controller-runtime 的循环在 `err == nil && !Requeue` 分支里调 `Forget`。

- **自定义 RateLimiter**：controller-runtime 通过 `controller.Options{RateLimiter: ...}` 注入。例如 CRD 控制器想用 `ItemFastSlowRateLimiter`：

```go
package main

import (
    "time"

    "k8s.io/client-go/util/workqueue"
    ctrl "sigs.k8s.io/controller-runtime"
    appsv1 "k8s.io/api/apps/v1"
)

func setup(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&appsv1.Deployment{}).
        WithOptions(ctrl.Options{
            RateLimiter: workqueue.NewItemFastSlowRateLimiter(
                time.Second,   // fastDelay
                10*time.Second, // slowDelay
                5,              // maxFastAttempts
            ),
        }).
        Complete(&myReconciler{})
}
```

- **不同 item 用不同限流**：默认 RateLimiter 不区分 item 类型。如果控制多种资源，可以包一层 switch，按 item 类型选择不同 limiter。

- **观测重试次数**：`workqueue_retries_total` 指标按 item 统计。配合 `NumRequeues` 可以在 Reconcile 里做"超过 N 次就降级处理"的逻辑。

### Retry

Retry（重试）是控制器"最终一致"的核心保障。但 Kubernetes 的重试不是简单的"失败重试 N 次"，而是结合 Requeue、RateLimiter、Finalizer、Status Conditions 的复合策略。

#### 是什么

控制器的重试分两类：

1. **隐式重试**：Reconcile 返回 error → workqueue 按指数退避自动重试，直到成功或封顶。
2. **显式重试**：Reconcile 返回 `Result{RequeueAfter: d}` → 延迟 d 后重新入队，用于轮询外部状态。

两者的区别：

| 维度 | 隐式重试 (error) | 显式重试 (RequeueAfter) |
|---|---|---|
| 触发条件 | 处理失败 | 处理"成功但未完成" |
| 退避策略 | RateLimiter 指数退避 | 固定/自定义延迟 |
| 是否计入 NumRequeues | 是 | 否（Forget 后 AddAfter） |
| 监控指标 | `workqueue_retries_total` | 业务自定义 |
| 是否需要告警 | 通常需要 | 通常不需要 |

#### 工作原理与源码要点

controller-runtime 的 Reconcile 后处理（简化自 `pkg/internal/controller/controller.go`）：

```go
result, err := c.Reconcile.Handle(ctx, req)
switch {
case err != nil:
    // 1. 失败：按 RateLimiter 退避重试
    c.Queue.AddRateLimited(req)
case result.RequeueAfter > 0:
    // 2. 显式延迟：Forget 后 AddAfter，不计入重试次数
    c.Queue.Forget(obj)
    c.Queue.AddAfter(req, result.RequeueAfter)
case result.Requeue:
    // 3. 显式立即重试：也走速率限制
    c.Queue.AddRateLimited(req)
default:
    // 4. 成功：Forget 重置计数器
    c.Queue.Forget(obj)
}
```

要点：

- **error 重试有上限吗？默认没有**：`ItemExponentialFailureRateLimiter` 封顶 1000s 后会一直重试，直到 Reconcile 成功或对象被删。生产中要靠监控+告警发现"永久失败"对象，或自己在 Reconcile 里判断 `NumRequeues > N` 后写入 Status Condition 并停止重试。

- **RequeueAfter 不算重试**：因为调用了 `Forget`。所以"等待外部就绪"用 RequeueAfter，"处理失败"用 return error，二者监控含义不同。

- **冲突重试（ConflictRetry）**：Update Status 时如果遇到 `409 Conflict`（说明对象在 Reconcile 期间被改了），应该 return error 让 workqueue 重试，而不是在 Reconcile 里循环重试。后者会阻塞 worker。

- **Finalizer 重试**：删除流程里如果外部清理失败，return error 让 workqueue 重试；成功后 remove finalizer 再 update。注意：remove finalizer 本身可能 409，也要 return error。

#### 重试模式代码示例

典型的 Reconcile 重试骨架：

```go
package main

import (
    "context"
    "time"

    apierrors "k8s.io/apimachinery/pkg/api/errors"
    appsv1 "k8s.io/api/apps/v1"
    "k8s.io/apimachinery/pkg/types"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
)

type MyReconciler struct {
    client.Client
}

func (r *MyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var obj appsv1.Deployment
    if err := r.Get(ctx, req.NamespacedName, &obj); err != nil {
        if apierrors.IsNotFound(err) {
            // 对象已删，无需处理
            return ctrl.Result{}, nil
        }
        // 缓存未同步等：return error 走重试
        return ctrl.Result{}, err
    }

    // 期望状态计算
    desired := buildDesired(&obj)
    if err := r.Patch(ctx, desired, client.Apply, client.ForceOwnership, client.FieldOwner("my-controller")); err != nil {
        if apierrors.IsConflict(err) {
            // 冲突：立刻重试（return error，让 workqueue 退避）
            return ctrl.Result{}, err
        }
        return ctrl.Result{}, err
    }

    // 检查是否就绪
    if !isReady(&obj) {
        // 未就绪：30s 后再来
        return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
    }

    // 成功：Forget 由 controller-runtime 自动完成
    return ctrl.Result{}, nil
}

func buildDesired(obj *appsv1.Deployment) *appsv1.Deployment { return nil }
func isReady(obj *appsv1.Deployment) bool                     { return false }

var _ types.NamespacedName
```

#### 工程实践与常见坑

- **不要在 Reconcile 里 for 循环重试**：阻塞 worker，影响其他对象。让 workqueue 来重试。

- **区分"瞬时错误"和"永久错误"**：瞬时错误（网络、5xx、Conflict）return error 让退避重试；永久错误（配置非法、CRD schema 不匹配）应该写入 Status Condition 并 `return ctrl.Result{}, nil` 停止重试，否则会无限刷日志。

```go
if isPermanentError(err) {
    setCondition(&obj, "Ready", "False", err.Error())
    _ = r.Status().Update(ctx, &obj)
    return ctrl.Result{}, nil  // 不重试，等用户改配置
}
return ctrl.Result{}, err      // 瞬时错误，重试
```

- **`Requeue: true` 慎用**：它走 `AddRateLimited`，第一次会立刻入队（baseDelay），但持续使用会让退避迅速增长。一般用 `RequeueAfter` 更可控。

- **`RequeueAfter` 不要短于变更传播时间**：比如等 Deployment rollout，至少 5s 起。1s 的 RequeueAfter 在大集群里会刷爆 workqueue。

- **Update Status 失败要重试**：很多人忘记 Status 也可能 409。把 Status update 也包进 error 返回。

- **监控退避队列**：`workqueue_unfinished_work_seconds` 反映"队列里最老的 item 等了多久"，如果持续增长说明退避太重或 worker 卡死。

- **Leader 切换时的重试**：controller-runtime 的 leader election 切换时，旧 leader 的 workqueue 会 ShutDown，未处理 item 丢失。新 leader 选举成功后，靠 Informer 的 Resync 重新触发 Reconcile。所以 Finalizer 必须幂等。

- **`NumRequeues` 降级**：在 Reconcile 里 `c.Queue.NumRequeues(req) > 10` 时，把 Status 改为 Failed 并停止返回 error（改 return nil），避免无限退避。

### Context

Context（`context.Context`）在 controller-runtime 里贯穿 Manager、Controller、Reconcile 三层，承担"取消信号 + 截止时间 + 请求作用域"的职责。理解它的传递链路，对正确处理优雅停机、超时、tracing 至关重要。

#### 是什么

Reconcile 的签名强制要求 `context.Context`：

```go
func (r *MyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
```

这个 ctx 不是凭空来的，它来自 Manager，承载了：

1. **取消信号**：Manager 收到 SIGTERM/SIGINT 时，`ctx.Done()` 会被关闭，Reconcile 应及时退出。
2. **Tracing/Metrics 注入**：controller-runtime 会在 ctx 里塞 span，方便 tracing。
3. **Logger**：controller-runtime v0.15+ 默认把 logger 也放进 ctx（`logr.FromContext`）。
4. **超时**：可在 Reconcile 内部 `context.WithTimeout` 限定单次调和时长。

#### 工作原理与源码要点

Manager 启动时构造根 ctx：

```go
// pkg/manager/internal.go 简化
func (cm *controllerManager) Start(ctx context.Context) error {
    ctx, cancel := context.WithCancel(ctx)
    cm.cancelCancel = cancel

    // 监听信号
    go cm.signalHandler(ctx)

    // 启动每个 controller
    for _, c := range cm.controllers {
        go func(ctrl controller.Controller) {
            err := ctrl.Start(ctx)  // 把根 ctx 传下去
            _ = err
        }(c)
    }

    <-ctx.Done()
    // 优雅停机：等所有 worker 退出
}
```

Controller 把 ctx 传给 worker，worker 再传给 Reconcile：

```go
// pkg/internal/controller/controller.go 简化
func (c *Controller) Start(ctx context.Context) error {
    for i := 0; i < c.MaxConcurrentReconciles; i++ {
        go wait.UntilWithContext(ctx, c.worker, time.Second)
    }
    return nil
}

func (c *Controller) worker(ctx context.Context) {
    for c.processNextWorkItem(ctx) {
    }
}

func (c *Controller) processNextWorkItem(ctx context.Context) bool {
    obj, shutdown := c.Queue.Get()
    if shutdown {
        return false
    }
    defer c.Queue.Done(obj)

    // 单次 Reconcile 可选超时
    reconcileTimeout := time.Duration(0) // 通常 0 表示不限
    rctx := ctx
    if reconcileTimeout > 0 {
        var cancel context.CancelFunc
        rctx, cancel = context.WithTimeout(ctx, reconcileTimeout)
        defer cancel()
    }

    result, err := c.Reconcile.Handle(rctx, req)
    _ = result
    _ = err
    // ...
}
```

要点：

- **`ctx.Done()` 不会立刻中断 Reconcile**：context 是协作式的，Reconcile 必须自己 `select <-ctx.Done()` 或在 API 调用里检查。controller-runtime 不会强行 kill。
- **`Queue.Get()` 是阻塞的**：当 ctx cancel 后，Controller 会调用 `Queue.ShutDown()` 让 Get 返回 `(nil, true)`，worker 才能退出。这是优雅停机的关键。
- **API 调用自动带 ctx**：`r.Get(ctx, ...)`、`r.Patch(ctx, ...)` 会把 ctx 传给底层 client，HTTP 请求会在 ctx cancel 时被中断。

#### 优雅停机流程

```
SIGTERM/SIGINT
     |
     v
Manager.signalHandler
     |
     v
root ctx.Done() 关闭
     |
     +---> Controller.worker 退出 for 循环（Queue.Get 返回 shutdown=true）
     +---> informerFactory 停止 Watch
     +---> leader-election 释放
     |
     v
等所有 worker 退出（GracefulShutdownTimeout，默认 30s）
     |
     v
进程退出
```

#### 工程实践与常见坑

- **Reconcile 里要响应 ctx.Done()**：尤其是有外部 HTTP/DB 调用时。简单做法：

```go
func (r *MyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    select {
    case <-ctx.Done():
        return ctrl.Result{}, ctx.Err()
    default:
    }
    // 业务逻辑
    if err := externalCall(ctx); err != nil {
        return ctrl.Result{}, err
    }
    return ctrl.Result{}, nil
}
```

- **不要把 ctx 存进结构体**：ctx 应该作为参数传递。存进结构体容易导致跨 Reconcile 复用，违反 ctx 生命周期语义。

- **`context.Background()` 是反模式**：Reconcile 里永远用传入的 ctx。如果真的需要脱离 ctx 的后台任务，应该用 Manager 的 `Add` 注册 Runnable，由 Manager 管理生命周期。

- **单次 Reconcile 超时**：默认 controller-runtime 不限单次 Reconcile 时长。如果 Reconcile 可能卡住（如外部 HTTP 不返回），用 `context.WithTimeout`：

```go
func (r *MyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()
    // ...
    return ctrl.Result{}, nil
}
```

- **ctx 与 leader election**：非 leader 副本的 Controller 不会启动 worker，但 informer 还在跑。Manager 切换 leader 时会 Stop 旧 Controller、Start 新 Controller，ctx 在这个过程中被 cancel 再重建。Reconcile 里不要假设 ctx 是"永生"的。

- **tracing/metrics 通过 ctx 传递**：不要在 Reconcile 里 `context.Background()` 新建 ctx，会丢掉 span，tracing 链路断掉。更多 context 的细节可参考 [第15章 Context](./15-Context.md)。

- **Webhook 也要用 ctx**：`webhook.AdmissionHandler` 同样接收 ctx，处理超时由 webhook server 控制（默认 10s）。Webhook 里不要做重活，否则拖慢 apiserver 准入。

- **`WaitForCacheSync` 也要带 ctx**：`cache.WaitForCacheSync(ctx.Done(), inf.HasSynced)`，否则 Manager 退出时这里会卡住。

### 本章小结

- Reconcile 是声明式控制器的核心：水平触发、幂等、返回 Result/error 控制 requeue 行为。
- WorkQueue 通过 `dirty`/`processing` 三态去重，保证同 key 不并发；`AddAfter`/`AddRateLimited` 提供延迟与限速能力。
- RateLimiter 默认是"全局令牌桶 + 单 item 指数退避"的 MaxOf 组合，第 N 次重试延迟约为 `5ms * 2^N`，封顶 1000s（生产建议调小）。
- Retry 分隐式（error → 退避重试）与显式（RequeueAfter → 定时轮询），永久错误应写入 Status Condition 停止重试。
- Context 贯穿 Manager → Controller → Reconcile，承载取消、超时、tracing；Reconcile 必须响应 ctx.Done()，优雅停机依赖 Queue.ShutDown 让 worker 退出。
- 控制器的三大原则：**水平触发不依赖事件、幂等可重入、失败交 workqueue 退避重试而非自己循环**。

读完本章，你应该能读懂 controller-runtime 的核心循环，并能写出生产级的 Reconciler。结合 [第29章 client-go](./29-client-go.md) 的 Informer 机制，Kubernetes 控制平面的"读-调-写"闭环就完整了。
