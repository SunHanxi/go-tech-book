## 第30章 Controller

> 版本基线：`sigs.k8s.io/controller-runtime v0.24.1`，其 go.mod 固定 Kubernetes libraries `v0.36.0`，要求 Go 1.26。队列、rate limiter 和 Result API 会随版本演进，示例不应与其他 minor 的 client-go 混搭。

### Reconcile

Reconcile（调和）是 controller-runtime 时代 Kubernetes 控制器的核心抽象：给定一个对象 key，让对象的实际状态向期望状态收敛。它是水平触发（level-triggered）的，不关心"发生了什么变化"，只关心"现在该不该是这个样子"。

#### 是什么

`Reconciler` 是 controller-runtime（`sigs.k8s.io/controller-runtime`）定义的一个接口：

```go
type Reconciler interface {
    Reconcile(context.Context, Request) (Result, error)
}

type Request struct {
    types.NamespacedName
}

type Result struct {
    Requeue      bool          // deprecated：使用 RequeueAfter
    RequeueAfter time.Duration // 延迟一段时间后重新入队
    Priority     *int          // 再入队时的优先级；默认队列中数值越大越优先
}
```

字段说明：

| 字段 | 含义 |
|---|---|
| `Request.NamespacedName` | 触发 Reconcile 的对象 namespace/name，唯一标识 |
| `Result.Requeue` | deprecated；true 会按 RateLimiter 再入队，兼容旧代码 |
| `Result.RequeueAfter` | 大于 0 表示延迟 N 后再调和（用于等待外部系统就绪、轮询） |
| `Result.Priority` | 若本次结果再次入队，则覆盖其优先级；本次处理不受影响 |
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
     |   WorkQueue     |  Priority + RateLimited + Delayed
     +--------+--------+
              |
              | GetWithPriority(key)
              v
     +-----------------+
     |   Reconcile     |  读缓存 -> 计算期望 -> 调 API
     +--------+--------+
              |
              | Result{RequeueAfter, Priority}, error
              v
     +-----------------+
     |   再入队决策     |  err  -> AddWithOpts(RateLimited)
     +-----------------+  RequeueAfter -> AddWithOpts(After)
                          Requeue=true -> AddWithOpts(RateLimited，deprecated)
                          都没有 -> Forget (成功)
```

controller-runtime 内部核心循环（简化自 `pkg/internal/controller/controller.go`；省略 metrics 与日志）：

```go
func (c *Controller[request]) processNextWorkItem(ctx context.Context) bool {
    req, priority, shutdown := c.Queue.GetWithPriority()
    if shutdown {
        return false
    }
    defer c.Queue.Done(req)

    result, err := c.Reconcile(ctx, req)
    if result.Priority != nil {
        priority = *result.Priority
    }
    switch {
    case err != nil:
        if !errors.Is(err, reconcile.TerminalError(nil)) {
            c.Queue.AddWithOpts(priorityqueue.AddOpts{
                RateLimited: true, Priority: &priority,
            }, req)
        }
    case result.RequeueAfter > 0:
        c.Queue.Forget(req)
        c.Queue.AddWithOpts(priorityqueue.AddOpts{
            After: result.RequeueAfter, Priority: &priority,
        }, req)
    case result.Requeue: // deprecated
        c.Queue.AddWithOpts(priorityqueue.AddOpts{
            RateLimited: true, Priority: &priority,
        }, req)
    default:
        c.Queue.Forget(req)
    }
    return true
}
```

要点：

- **水平触发**：Reconcile 拿到 key 后，去缓存里 Get 对象，根据当前状态决策。即便中间漏掉了 N 个事件，最终只要触发一次 Reconcile，状态就能收敛。这与传统的"事件回调"（edge-triggered）截然不同。
- **幂等**：Reconcile 必须可重入。同一个 key 可能因为 Resync、Requeue、Watch 重连被反复调和。
- **不返回 error 也要重试**：返回 `Result{RequeueAfter: 30*time.Second}` 是"成功但稍后再调"，比如等待 Job 完成。
- **调用包装**：当前版本为每次调用注入 logger 与 reconcile ID，记录 metrics；`RecoverPanic` 默认 true，可关闭；`ReconciliationTimeout` 大于 0 时使用带 cause 的 timeout Context。分布式 tracing 需要应用或集成显式接入，不能假设框架自动创建 span。

#### 工程实践与常见坑

- **Reconcile 里不要 Watch**：Reconcile 是消费者，不应再发起订阅。需要的依赖应该在 Builder 阶段用 `Watches` 注册，依赖对象的变更通过 `MapTo`/`EnqueueRequestForOwner` 转换成本对象的 key。

- **每次 Reconcile 时长要可控**：默认 `ReconciliationTimeout=0`，即没有框架单次超时。可在 `controller.Options` 或 Manager 的 controller config 设置超时，也可为某个下游调用派生更短 deadline；增加 `MaxConcurrentReconciles` 不能修复不响应取消的调用。

- **优先依赖 watch，轮询要有预算**：能 watch 的集群内依赖用 `Owns`/`Watches` 触发；外部系统必须轮询时，根据其 SLA、对象数量和 API 预算选择 `RequeueAfter`，不要套固定秒数。

- **不要在 Reconcile 里 sleep**：阻塞 worker。改用 `RequeueAfter` 让出执行权。

- **用 Finalizer 协调外部清理**：对象进入 terminating 后，幂等清理成功再移除自己拥有的 finalizer；finalizer 保留期间删除不会完成，应提供 Condition、告警和人工修复路径。

- **MaxConcurrentReconciles 不是越大越好**：当前默认是 1。合理值取决于 API client 限流、外部依赖、单次内存和热点 key 分布；通过 queue latency、work duration、限流与下游指标逐步调整，不使用固定 5-10 公式。

- **不要新写 `Requeue: true`**：它已 deprecated，且走失败退避语义。等待时间推进时只设置 `RequeueAfter`。返回 error 时 Requeue/RequeueAfter 被忽略；`Priority` 的字段契约明确允许影响 error 重入。

### WorkQueue

WorkQueue 把事件生产与 Reconcile 解耦，并提供 key 去重、延迟、优先级和失败退避。`controller-runtime v0.24.1` 默认 `UsePriorityQueue=true`，使用自己的 `pkg/controller/priorityqueue`，不是直接使用 client-go 的经典 FIFO 队列。

#### 默认 PriorityQueue

当前默认接口在 client-go 的类型化 rate-limiting queue 之上增加优先级与组合入队参数：

```go
type AddOpts struct {
    After       time.Duration
    RateLimited bool
    Priority    *int // 数字越大，优先级越高
}

type PriorityQueue[T comparable] interface {
    workqueue.TypedRateLimitingInterface[T]
    AddWithOpts(opts AddOpts, items ...T)
    GetWithPriority() (item T, priority int, shutdown bool)
}
```

`reconcile.Result` 的 `Priority *int` 只影响该 key **再次入队**时使用的优先级；本次 Reconcile 已经从队列取出，无法被追溯修改。未指定时保留传入请求的当前优先级。

#### 关键结构体

默认队列把可运行与等待中的 item 分别放在两棵 B-tree 中，并用 map 做全局去重：

```go
type item[T comparable] struct {
    Key          T
    AddedCounter uint64     // 同优先级内维持 FIFO
    Priority     int
    ReadyAt      *time.Time // nil 表示已经可运行
}

type priorityQueue[T comparable] struct {
    items   map[T]*item[T] // ready + waiting 中的去重索引
    ready   bTree[*item[T]]
    waiting bTree[*item[T]]

    locked  sets.Set[T] // 已由 Get 交给 worker、尚未 Done 的 key
    rateLimiter workqueue.TypedRateLimiter[T]
    // 省略锁、唤醒 channel、metrics 与 shutdown 字段
}
```

合并规则是当前公开注释明确说明的行为：

- 同一 key 多次入队只保留一项；优先级取最大值，ready time 取最早值。
- `ready` 先按优先级降序，再按 `AddedCounter` 升序，因此同一优先级内是 FIFO。
- `waiting` 先按 `ReadyAt` 排序；到期后移入 `ready`，并重新分配 `AddedCounter`。
- `locked` 阻止同一 key 同时交给两个 worker。处理期间再次 Add 的请求会被记录，待 `Done` 解锁后才可再次取出。

#### client-go 兼容路径

设置 `controller.Options.UsePriorityQueue=false` 时，controller-runtime 使用 client-go v0.36 的类型化 rate-limiting queue，并包一层不支持真实优先级的适配器。其接口仍值得认识：

```go
type TypedRateLimitingInterface[T comparable] interface {
    TypedDelayingInterface[T]
    AddRateLimited(item T)
    Forget(item T)
    NumRequeues(item T) int
}
```

经典基本队列用 `dirty` 表示“仍需处理”，用 `processing` 表示“已经 Get、尚未 Done”。`Get` 会把 key 从 dirty 移到 processing；若处理期间再次 Add，它重新进入 dirty 但不会立即入 FIFO，`Done` 看到 dirty 后才再次入队。这个状态转换保证同一 key 不并发，同时不丢掉处理期间的新通知。

client-go 的 `delayingType` 用 `waitingForAddCh` 把请求送给单独的 `waitingLoop`。最小堆和去重 map 是该循环的局部状态，不是 `delayingType` 字段；循环按最早 `readyAt` 创建 timer。`maxWait=10s` 的 heartbeat 只是防止过久不复查的兜底，不是 10ms 精度限制，短 `AddAfter` 正常由最早到期 timer 唤醒。

#### 工作原理

worker 从队列取得 key 与当前优先级，调用 Reconciler 后再决定是否重新入队。v0.24.1 主线可简化为：

```go
req, priority, shutdown := c.Queue.GetWithPriority()
if shutdown {
    return false
}
defer c.Queue.Done(req)

result, err := c.Reconcile(ctx, req)
if result.Priority != nil {
    priority = *result.Priority
}

switch {
case err != nil:
    if !errors.Is(err, reconcile.TerminalError(nil)) {
        c.Queue.AddWithOpts(priorityqueue.AddOpts{
            RateLimited: true,
            Priority:    &priority,
        }, req)
    }
case result.RequeueAfter > 0:
    c.Queue.Forget(req)
    c.Queue.AddWithOpts(priorityqueue.AddOpts{
        After:    result.RequeueAfter,
        Priority: &priority,
    }, req)
case result.Requeue: // deprecated，仅为兼容保留
    c.Queue.AddWithOpts(priorityqueue.AddOpts{
        RateLimited: true,
        Priority:    &priority,
    }, req)
default:
    c.Queue.Forget(req)
}
```

非 nil error 会使其他 Result 字段失效；`TerminalError` 仍记录为错误，但不重新入队。`RequeueAfter` 先 `Forget`，因此不会延续失败计数。普通成功也 `Forget`，等待下一次 watch、resync 或显式事件。

#### 工程实践与常见坑

- **同一 key 不会并发 Reconcile**：默认队列用 `locked`，client-go 兼容队列用 `processing`；二者都把处理期间的新通知延后到 `Done` 之后。

- **`Done` 必须被调用**：否则 key 保持 locked/processing，后续通知无法正常处理。controller-runtime 的循环用 defer 保证调用。

- **没有“绕过去重”的 RequeueAfter**：处理期间的 Add 与延迟重入都要遵守队列的同 key 串行化。需要再次观察时返回有业务依据的 `RequeueAfter`，不要用极短延迟模拟递归调用。

- **队列不是持久化日志**：默认 priority queue 的 `ShutDown` 会让等待中的 `Get` 退出，`ShutDownWithDrain` 在该实现中等同于 `ShutDown`；未处理内存项不能作为恢复依据。client-go 基本队列的 Shutdown 语义不同，但 leader 切换或进程退出后同样要依靠 List/Watch、幂等 Reconcile、Status 和 finalizer 重建工作。

- **队列监控**：关注 depth、adds、retries、queue duration、work duration、unfinished work 和 longest running processor。depth 上升可能来自输入突增、等待项、优先级挤压或 worker 变慢，需要结合这些指标判断。

- **不要用 workqueue 做业务队列**：它是为控制器内部事件去重设计的，吞吐量、持久化都不适合业务消息流。

- **优先级不是配额**：持续到来的高优先级 key 可能让低优先级 key 长时间等待。需要租户公平、老化或配额时应在事件映射与控制器拆分层设计，不能只依赖一个整数优先级。

### RateLimiter

RateLimiter 决定"同一个 item 重试时的退避策略"。它是控制器面对失败时保护 API Server 的关键防线。

#### 是什么

v0.36 的接口名是 `TypedRateLimiter[T]`：

```go
type TypedRateLimiter[T comparable] interface {
    When(item T) time.Duration
    Forget(item T)
    NumRequeues(item T) int
}
```

client-go 内置了几种实现：

| 实现 | 退避策略 | 适用场景 |
|---|---|---|
| `TypedBucketRateLimiter[T]` | 全局令牌桶，所有 item 共享 | 限制整体重试速率 |
| `TypedItemExponentialFailureRateLimiter[T]` | 按 item 指数退避 `base * 2^retries`，封顶 `max` | 临时错误退避 |
| `TypedItemFastSlowRateLimiter[T]` | 前若干次快速，之后慢速 | 分阶段重试 |
| `TypedMaxOfRateLimiter[T]` | 多个 limiter 取最大值 | 组合全局与单 item 限制 |
| `DefaultTypedControllerRateLimiter[T]` | `MaxOf(Bucket(10qps,100), Exponential(5ms,1000s))` | client-go 导出的组合构造器；不是 v0.24.1 默认 priority queue 的默认值 |

#### 关键结构体

**TypedItemExponentialFailureRateLimiter**（简化）：

```go
type TypedItemExponentialFailureRateLimiter[T comparable] struct {
    failures     map[T]int
    failuresLock sync.Mutex
    baseDelay    time.Duration         // 初始延迟
    maxDelay     time.Duration         // 最大延迟
}

func (r *TypedItemExponentialFailureRateLimiter[T]) When(item T) time.Duration {
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
| `baseDelay` | 构造时传入的第一次 `When` 延迟；controller-runtime 当前默认传 5ms |
| `maxDelay` | 构造时传入的单次上限；controller-runtime 当前默认传 1000s |

**TypedBucketRateLimiter**（基于 `golang.org/x/time/rate`）：

```go
type TypedBucketRateLimiter[T comparable] struct {
    *rate.Limiter
}

func (r *TypedBucketRateLimiter[T]) When(item T) time.Duration {
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
type TypedMaxOfRateLimiter[T comparable] struct {
    limiters []TypedRateLimiter[T]
}

func (r *TypedMaxOfRateLimiter[T]) When(item T) time.Duration {
    var ret time.Duration
    for _, limiter := range r.limiters {
        curr := limiter.When(item)
        if curr > ret {
            ret = curr
        }
    }
    return ret
}

func (r *TypedMaxOfRateLimiter[T]) Forget(item T) {
    for _, limiter := range r.limiters {
        limiter.Forget(item)
    }
}
```

#### 工作原理

这里必须区分两个“默认值”。client-go 的 `DefaultTypedControllerRateLimiter` 是组合策略：

```go
func DefaultTypedControllerRateLimiter[T comparable]() TypedRateLimiter[T] {
    return NewTypedMaxOfRateLimiter(
        &TypedBucketRateLimiter[T]{Limiter: rate.NewLimiter(10, 100)},
        NewTypedItemExponentialFailureRateLimiter[T](5*time.Millisecond, 1000*time.Second),
    )
}
```

它让该 queue 内的 item 共享 10 QPS、burst 100 的令牌桶，同时按 key 指数退避，最终取两者较大延迟。

但 controller-runtime v0.24.1 默认启用 priority queue，并显式选择**只有 per-item 指数退避**的 limiter：

```go
if options.RateLimiter == nil && usePriorityQueue {
    options.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[request](
        5*time.Millisecond,
        1000*time.Second,
    )
}
```

只有关闭 priority queue 的兼容路径，未自定义时才使用 client-go 的组合构造器。因此“默认一定有全局 10 QPS 限制”在本章版本基线上是错误的；API client 自身的 `rest.Config.QPS/Burst` 又是另一层限流。

per-item limiter 的含义：

- 第 k 次调用 `When(item)`（k 从 1 开始）返回 `min(maxDelay, baseDelay * 2^(k-1))`。
- `Forget(item)` 删除该 key 的计数；下一次失败重新从 baseDelay 开始。
- 达到 maxDelay 后不会停止重试，只是后续延迟保持在上限。

退避序列示例（base=5ms, max=1000s）：

| 重试次数 | 延迟 |
|---|---|
| 1 | 5ms |
| 2 | 10ms |
| 3 | 20ms |
| 4 | 40ms |
| 5 | 80ms |
| 10 | 2.56s |
| 15 | 81.92s |
| 18 | 655.36s |
| 19 | 1000s（封顶） |
| 20 | 1000s（封顶） |

#### 工程实践与常见坑

- **按恢复目标调整上限**：1000s 是当前默认实现值，不一定适合所有依赖。上限过大会延长恢复，过小会让长期故障持续施压；结合依赖恢复时间、对象数量、告警和 API 预算决定，并用 `TerminalError` 或状态机处理确认不可重试的输入。

- **BucketRateLimiter 只在其 limiter 实例内共享**：每个 controller queue 通常有独立实例；它不等于多个 controller 共享的 API client 限流。按 HTTP client 维度控制请求速率要看 `rest.Config.QPS/Burst`，还要考虑 apiserver flow control。

- **成功和 RequeueAfter 都会 Forget**：普通成功以及显式 `RequeueAfter` 会重置失败计数；error 与已废弃的 `Requeue=true` 保留并增加计数。自建队列消费循环必须复刻这一契约。

- **自定义 RateLimiter**：controller-runtime 通过 `controller.Options{RateLimiter: ...}` 注入。例如 CRD 控制器想用 `ItemFastSlowRateLimiter`：

```go
package main

import (
    "time"

    "k8s.io/client-go/util/workqueue"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/controller"
    "sigs.k8s.io/controller-runtime/pkg/reconcile"
    appsv1 "k8s.io/api/apps/v1"
)

func setup(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&appsv1.Deployment{}).
        WithOptions(controller.Options{
            RateLimiter: workqueue.NewTypedItemFastSlowRateLimiter[reconcile.Request](
                time.Second,   // fastDelay
                10*time.Second, // slowDelay
                5,              // maxFastAttempts
            ),
        }).
        Complete(&myReconciler{})
}
```

- **自定义 limiter 要保持 key 状态有界**：per-item limiter 会为失败 key 保存计数，成功或转为显式等待后必须 Forget。复杂分类可通过拆分 controller 或包装 limiter 实现，但不要从 `namespace/name` 猜测资源类型。

- **指标避免 item 高基数**：`workqueue_retries_total` 通常按 queue 聚合，不按 key 打 label。`NumRequeues(item)` 是 queue/limiter API，但普通 Reconciler 不直接拿到内部 queue；业务级失败次数应建模到 Status、外部状态或低基数指标，而不是给每个对象创建指标标签。

### Retry

Retry（重试）是控制器"最终一致"的核心保障。但 Kubernetes 的重试不是简单的"失败重试 N 次"，而是结合 Requeue、RateLimiter、Finalizer、Status Conditions 的复合策略。

#### 是什么

控制器的重试分两类：

1. **失败重试**：Reconcile 返回普通 error → workqueue 按 limiter 计算延迟并重新入队；延迟会封顶，但重试次数默认不封顶。
2. **显式重试**：Reconcile 返回 `Result{RequeueAfter: d}` → 延迟 d 后重新入队，用于轮询外部状态。

两者的区别：

| 维度 | 隐式重试 (error) | 显式重试 (RequeueAfter) |
|---|---|---|
| 触发条件 | 处理失败 | 处理"成功但未完成" |
| 调度策略 | 配置的 RateLimiter | 调用方指定延迟 |
| 是否计入 NumRequeues | 是 | 否（Forget 后 AddAfter） |
| 监控指标 | `workqueue_retries_total` | 业务自定义 |
| 观测重点 | 错误率、重试率、退避与失败对象 | 轮询对象数、外部等待时长与 API 预算 |

#### 工作原理与源码要点

controller-runtime 的 Reconcile 后处理主线已在 WorkQueue 小节列出。这里强调 error 分支：

```go
result, err := c.Reconcile(ctx, req)
if err != nil {
    // 非 nil error 时 Requeue/RequeueAfter 被忽略；Priority 已在 switch 前读取，
    // 可用于这次失败重入。TerminalError 被记录但不重新入队。
    if !errors.Is(err, reconcile.TerminalError(nil)) {
        c.Queue.AddWithOpts(priorityqueue.AddOpts{RateLimited: true}, req)
    }
}
```

要点：

- **普通 error 默认没有次数上限**：指数延迟达到 1000s 后保持该上限，直到成功 Forget，或某次 Reconcile 返回 `TerminalError`/成功。对象删除本身不会神奇清除 key；通常下一次 Get 返回 NotFound，Reconciler 把它当成功后才 Forget。

- **RequeueAfter 不算重试**：因为调用了 `Forget`。所以"等待外部就绪"用 RequeueAfter，"处理失败"用 return error，二者监控含义不同。

- **冲突重试要缩小范围**：整次 Reconcile 遇到 409 时直接返回 error 通常最清晰。对必须立即完成的短小 read-modify-write，可使用 `retry.RetryOnConflict`，但每次都要通过 APIReader/live client 重新 Get 并重算该小操作；缓存 Get 可能重复返回旧 RV。不要在一个 worker 内无限重跑全部副作用。

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
        // 读取失败：return error 走退避重试
        return ctrl.Result{}, err
    }

    // 只提交实际需要的变更；示例用 Merge patch，避免整对象覆盖
    before := obj.DeepCopy()
    ensureDesiredMetadata(&obj)
    patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
    if err := r.Patch(ctx, &obj, patch); err != nil {
        if apierrors.IsConflict(err) {
            // 冲突：交给 workqueue 退避后重新读取
            return ctrl.Result{}, err
        }
        return ctrl.Result{}, err
    }

    // 检查是否就绪
    if !isReady(&obj) {
        // 只有无法 watch 的外部状态才轮询；间隔来自该依赖的预算
        return ctrl.Result{RequeueAfter: externalPollInterval}, nil
    }

    // 成功：Forget 由 controller-runtime 自动完成
    return ctrl.Result{}, nil
}

var externalPollInterval = 30 * time.Second

func ensureDesiredMetadata(obj *appsv1.Deployment) {
    if obj.Labels == nil {
        obj.Labels = map[string]string{}
    }
    obj.Labels["example.com/managed"] = "true"
}

func isReady(obj *appsv1.Deployment) bool {
    return obj.Status.ObservedGeneration >= obj.Generation
}
```

#### 工程实践与常见坑

- **不要在 Reconcile 里 for 循环重试**：阻塞 worker，影响其他对象。让 workqueue 来重试。

- **区分可重试与终止错误**：网络、5xx、Conflict 等通常返回普通 error；用户 spec 在业务上不支持等终止错误应先持久化 Condition，再返回 `reconcile.TerminalError(err)`。后续对象更新仍会产生新事件并再次 Reconcile。

```go
if isPermanentError(err) {
    before := obj.DeepCopy()
    setCondition(&obj, "Ready", "False", err.Error())
    if statusErr := r.Status().Patch(ctx, &obj, client.MergeFrom(before)); statusErr != nil {
        return ctrl.Result{}, statusErr // Condition 没写成功，仍需重试
    }
    return ctrl.Result{}, reconcile.TerminalError(err)
}
return ctrl.Result{}, err
```

- **不再使用 `Requeue: true`**：该字段已 deprecated，并复用失败 RateLimiter，容易混淆“失败”与“等待”。时间驱动的成功路径使用 `RequeueAfter`，失败返回 error。

- **RequeueAfter 不是 watch 的替代品**：集群内对象变化优先注册 watch；外部轮询间隔按对象规模、依赖 SLA、抖动和请求预算推导，并对大规模同时到期考虑 jitter。

- **Update Status 失败要重试**：很多人忘记 Status 也可能 409。把 Status update 也包进 error 返回。

- **正确解释 workqueue 指标**：`unfinished_work_seconds` 是当前所有处理中 item 已耗时的总和，`longest_running_processor_seconds` 才是最长处理中 item 的时长；排队等待看 queue duration/depth，重试看 retries。不要用单一指标诊断退避。

- **Leader 切换时的恢复**：旧实例退出时队列中的瞬时 item 可以丢失；新实例靠初始 List/Watch 和当前对象状态重新入队，不能依赖 Resync。Reconcile、finalizer 和外部副作用都必须幂等。

- **不要让普通 Reconciler 依赖内部 NumRequeues**：接口没有把 queue 暴露给 Reconcile，失败次数也不是领域状态。需要有限尝试时，把阶段、最近错误和可审计计数建模到 Status/外部任务；确认不可重试时使用 TerminalError。

### 生产级 Reconcile 契约

一个可恢复的 Reconcile 只依赖**当前观察状态**：

1. 从 cache 读取主对象和必要依赖。
2. 若对象不存在，确认没有仍需清理的外部状态后返回。
3. 计算 desired state，不根据“这次是 Add 还是 Update”分叉核心逻辑。
4. 比较 actual 与 desired，只提交必要 Patch。
5. 更新 Status/Condition，表达最近一次观察结果。
6. 临时错误返回 error；等待外部时间推进才使用 `RequeueAfter`。

不要在一次 Reconcile 内循环到成功，也不要用 Sleep 等待缓存追上写入。一次写成功后缓存可能暂时仍是旧版本，下一次 Reconcile 再观察即可。

外部副作用必须有幂等键，例如 `namespace/name/uid`，并记录远端资源 ID。发生“远端创建成功但本地 Status 写失败”时，重试要查询并复用已有远端对象，而不是再次创建。

### Finalizer

OwnerReference 只能让 Kubernetes 垃圾回收集群内对象；云资源、DNS、数据库账号等外部资源必须用 finalizer 协调删除：

```go
const finalizer = "example.com/external-cleanup"

func (r *Reconciler) reconcileFinalizer(ctx context.Context, obj *examplev1.Widget) (ctrl.Result, error) {
    if obj.DeletionTimestamp.IsZero() {
        if controllerutil.ContainsFinalizer(obj, finalizer) {
            return ctrl.Result{}, nil
        }
        before := obj.DeepCopy()
        controllerutil.AddFinalizer(obj, finalizer)
        patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
        return ctrl.Result{}, r.Patch(ctx, obj, patch)
    }

    if !controllerutil.ContainsFinalizer(obj, finalizer) {
        return ctrl.Result{}, nil
    }
    if err := r.deleteExternalResource(ctx, obj); err != nil {
        return ctrl.Result{}, fmt.Errorf("delete external resource: %w", err)
    }

    before := obj.DeepCopy()
    controllerutil.RemoveFinalizer(obj, finalizer)
    patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
    return ctrl.Result{}, r.Patch(ctx, obj, patch)
}
```

约束：

- 先成功写入 finalizer，再创建外部资源，避免对象在保护建立前被删除。
- 删除外部资源要把“不存在”视为成功。
- 清理失败时保留 finalizer 并返回 error，让队列退避重试。
- 不在 finalizer 中无限等待不可恢复配置；写 Condition、告警，并提供人工修复路径。
- 不自动移除其他控制器的 finalizer。
- Finalizer 不是事务，进程可能在任意两步之间退出。

### OwnerReference 与 Watch

`SetControllerReference(owner, child, scheme)` 同时建立垃圾回收关系，并让 `.Owns(&Child{})` 把子对象变化映射回 owner：

```go
if err := controllerutil.SetControllerReference(widget, deployment, r.Scheme); err != nil {
    return err
}
```

一个 dependent 最多有一个 `controller=true` owner。Namespaced dependent 的 namespaced owner 必须在同一 namespace；cluster-scoped dependent 不能由 namespaced owner 控制。跨 namespace 或外部系统关系要用显式索引和 map function，不能伪造非法 owner reference。

### Status 与 Condition

Status 是控制器对最近观察结果的声明，不是事件日志。使用 `metav1.Condition` 时至少维护：

- `Type`：稳定机器语义，如 Ready、Progressing、Degraded。
- `Status`：True/False/Unknown。
- `Reason`：稳定、可聚合的 CamelCase 原因码。
- `Message`：面向人的细节，不能作为指标 label。
- `ObservedGeneration`：本次状态对应的 spec generation。

```go
meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
    Type:               "Ready",
    Status:             metav1.ConditionFalse,
    Reason:             "DependencyUnavailable",
    Message:            err.Error(),
    ObservedGeneration: obj.Generation,
})
```

只在状态实际变化时写入，避免无意义 Update 形成自激事件。Status subresource 与 spec 分开提交；发生 conflict 时重新读取并重新计算，不覆盖其他控制器拥有的 Condition。

### Server-Side Apply

SSA 让 apiserver 记录字段所有者，但不是“自动解决所有冲突”：

```go
err := r.Patch(ctx, desired, client.Apply,
    client.FieldOwner("widgets.example.com/controller"))
```

- FieldOwner 名称要稳定，升级版本不能随意变化。
- Apply 对象只包含本控制器真正拥有的字段，不把缓存对象整份回写。
- 默认尊重 ownership conflict。`ForceOwnership` 会夺取字段，只在明确迁移所有权时使用。
- Spec/metadata 与 status 使用不同写路径和 field manager。
- 写成功后不要求缓存立即 read-your-writes，依赖后续 Watch 收敛。

### 测试分层

1. **纯函数测试**：desired-state 计算、Condition 转换、错误分类。
2. **fake client**：只用于简单 CRUD 分支；它不能真实模拟 admission、defaulting、resourceVersion、SSA ownership、watch 和垃圾回收。
3. **envtest**：启动真实 apiserver/etcd，验证 CRD schema、status subresource、webhook、conflict 和 cache/watch。它不自动运行 deployment controller 等内建控制器。
4. **真实集群测试**：Kind/Kubernetes 验证 owner GC、leader election、RBAC、网络、滚动升级和资源限制。

关键场景：重复 Reconcile、删除中重启、外部成功/Status 失败、409 conflict、缓存滞后、context 取消、leader 切换、永久错误停止热重试。

### Context

Context（`context.Context`）在 controller-runtime 里贯穿 Manager、Controller、Reconcile 三层，承担取消、可选截止时间和请求作用域数据的传递。理解它的生命周期，是优雅停机、超时和外部调用取消正确工作的前提。

#### 是什么

要实现 `reconcile.Reconciler`，方法签名包含 `context.Context`：

```go
func (r *MyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
```

该 ctx 源自传给 `Manager.Start` 的根 Context，并由 controller 包装：

1. **取消信号**：调用方取消 Manager 根 Context 后，Controller 与正在执行的 Reconcile 都会观察到取消。
2. **Logger 与 reconcile ID**：当前 Controller 把本次调用的 logger 和 reconcile ID 放入 ctx，可用 `log.FromContext` 与 `controller.ReconcileIDFromContext` 读取。
3. **可选单次超时**：`controller.Options.ReconciliationTimeout > 0` 时，框架为每次调用派生 `WithTimeoutCause` Context；默认值 0 表示不设置。
4. **应用集成数据**：tracing span、认证信息等可以由应用或中间层加入，但 v0.24.1 核心 Controller 不会自动为每次 Reconcile 创建 OpenTelemetry span。Metrics 由控制循环直接记录，不等于都从 ctx 读取。

#### 工作原理与源码要点

常见入口由应用负责把进程信号转换为根 Context，再交给 Manager：

```go
func run(mgr ctrl.Manager) error {
    return mgr.Start(ctrl.SetupSignalHandler())
}
```

Manager 为不同 runnable group 管理派生 Context。Controller 启动 sources/queue、等待 cache sync，然后启动 worker；另一个 goroutine 在 ctx 取消时调用 queue shutdown。核心路径可概括为：

```go
func (c *Controller[request]) Start(ctx context.Context) error {
    c.startEventSourcesAndQueueLocked(ctx)

    for i := 0; i < c.MaxConcurrentReconciles; i++ {
        go func() {
            for c.processNextWorkItem(ctx) {
            }
        }()
    }
    <-ctx.Done()
    // 实际实现等待全部 worker 返回
    return nil
}

func (c *Controller[request]) processNextWorkItem(ctx context.Context) bool {
    req, priority, shutdown := c.Queue.GetWithPriority()
    if shutdown {
        return false
    }
    defer c.Queue.Done(req)
    c.reconcileHandler(ctx, req, priority)
    return true
}
```

`Controller.Reconcile` 包装器再根据 `ReconciliationTimeout` 派生单次 Context，默认恢复 panic，并调用用户的 `Do.Reconcile(ctx, req)`。

要点：

- **`ctx.Done()` 不会立刻中断 Reconcile**：context 是协作式的，Reconcile 必须自己 `select <-ctx.Done()` 或在 API 调用里检查。controller-runtime 不会强行 kill。
- **queue Get 是阻塞的**：当 ctx cancel 后，Controller 调用 `Queue.ShutDown()` 唤醒/终止 Get，worker 才能退出。默认 priority queue 返回元素零值与 `shutdown=true`。
- **API 调用自动带 ctx**：`r.Get(ctx, ...)`、`r.Patch(ctx, ...)` 会把 ctx 传给底层 client，HTTP 请求会在 ctx cancel 时被中断。

#### 优雅停机流程

```
SIGTERM/SIGINT（由 SetupSignalHandler 转为取消）
     |
     v
Manager 根 Context 关闭
     |
     v
各 runnable group 收到停止信号
     |
     +---> Controller 调 Queue.ShutDown，等待正在运行的 worker 返回
     +---> cache/source 停止 Watch
     +---> webhook、metrics 等 runnable 按各自分组停止
     |
     v
Manager 等 runnable 退出（GracefulShutdownTimeout 默认 30s）
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

- **不要丢弃传入的生命周期**：Reconcile 的 API/HTTP/DB 调用应沿用传入 ctx。需要跨越单次 Reconcile 的后台任务时，用 Manager `Add` 注册 Runnable，或使用有明确 owner 和停止协议的组件，不要随手换成 `context.Background()`。

- **单次 Reconcile 超时**：默认不设。若整个控制器适合统一 guardrail，优先配置当前版本提供的 `ReconciliationTimeout`；某个下游需要更短预算时再在 Reconcile 内派生子 Context：

```go
ctrl.NewControllerManagedBy(mgr).
    For(&examplev1.Widget{}).
    WithOptions(controller.Options{
        ReconciliationTimeout: 30 * time.Second,
    }).
    Complete(reconciler)
```

- **ctx 与 leader election**：需要选主的 Controller 默认只在成为 leader 后启动。`EnableWarmup=true` 可让其 sources 在非 leader 阶段预热，默认关闭。丢失 lease 时 Manager 为安全起见跳过常规 graceful shutdown；部署应让实例退出并由外部编排重启，不能假设同一个 Manager 内取消后又重建 Controller ctx。

- **日志与 tracing 分清来源**：当前框架会把 logger/reconcile ID 放入 ctx；OpenTelemetry span 取决于你的集成。无论由谁注入，替换成 Background 都会丢失取消和已有 request-scoped 值。更多细节见[第15章 Context](./15-Context.md)。

- **Webhook 也要用 ctx**：Admission handler 应尊重请求 ctx。有效预算受 admissionregistration 的 `timeoutSeconds`、apiserver 请求和网络共同约束，不能只引用一个 controller-runtime 固定默认值；耗时外部工作应移出准入路径。

- **等待 cache sync 必须可取消**：使用 `mgr.GetCache().WaitForCacheSync(ctx)` 或把 `ctx.Done()` 传给 client-go helper，确保 Manager 停止时等待能退出。

### 本章小结

- Reconcile 是声明式控制器的核心：水平触发、幂等、返回 Result/error 控制 requeue 行为。
- v0.24.1 默认使用 B-tree priority queue：同 key 合并时取最高优先级和最早 ready time，locked 集合保证同 key 不并发；旧 client-go dirty/processing 队列是可选兼容路径。
- 默认 priority queue 的 RateLimiter 是 per-item 指数退避，第一次 5ms、封顶 1000s；client-go 的 10 QPS + 指数 MaxOf 构造器只用于兼容路径或显式选择。
- Retry 分普通 error 退避、TerminalError 不重入与 RequeueAfter 定时再观察；终止错误应先持久化 Status Condition。
- Context 贯穿 Manager → Controller → Reconcile，承载取消、可选 timeout、logger 与 reconcile ID；tracing 由具体集成决定，优雅停机依赖调用链响应取消。
- 控制器的三大原则：**水平触发不依赖事件、幂等可重入、失败交 workqueue 退避重试而非自己循环**。

读完本章，你应该能读懂 controller-runtime 的核心循环，并能写出生产级的 Reconciler。结合 [第29章 client-go](./29-client-go.md) 的 Informer 机制，Kubernetes 控制平面的"读-调-写"闭环就完整了。
