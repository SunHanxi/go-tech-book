## 第23章 client-go

> client-go 是 Kubernetes 生态的基石，Informer 机制则是 client-go 的灵魂。理解 Informer 的工作原理，是读懂 controller-runtime、Operator 模式乃至整个 Kubernetes 控制平面的前提。本章自顶向下拆解 Informer 的核心组件：Informer、Reflector、DeltaFIFO、Indexer、SharedInformer，给出关键结构体与伪代码，并讨论工程实践中的常见坑。

### Informer

Informer 是 client-go 中用于在本地缓存 Kubernetes 资源并监听变更的高层抽象。它把"List + Watch"封装成一个事件驱动的流水线：从 API Server 拉取全量列表建立缓存，再通过 Watch 增量更新缓存，并通过回调把事件分发给业务逻辑。

#### 是什么

一个 Informer 至少包含以下能力：

1. 通过 ListerWatcher 执行 List（一次性全量）和 Watch（持续流式）。
2. 把 List/Watch 返回的事件塞进 DeltaFIFO。
3. 用一个消费者循环把 Delta 取出，写入 Indexer（本地缓存）。
4. 把变更以 OnAdd/OnUpdate/OnDelete 形式分发给注册的 ResourceEventHandler。
5. 周期性触发 Resync，把缓存里的对象重新以 Sync 事件喂给 handler。

它与直接调用 Watch 相比最大的好处是：本地缓存可以反复读、读不走 API Server；事件顺序在内存里被串行化，业务侧不需要处理 Watch 中断和重连。

#### 数据结构与工作流

Informer 的标准实现是 `cache.SharedIndexInformer`（在 `k8s.io/client-go/tools/cache/shared_informer.go`）。核心字段简化如下：

```go
type sharedIndexInformer struct {
    indexer       Indexer                 // 本地缓存，底层是 threadSafeMap
    controller    Controller              // 内部 Reflector + DeltaFIFO 的驱动器
    processor     *sharedProcessor        // 多消费者事件分发
    listerWatcher ListerWatcher           // List/Watch 的入口
    objectType    runtime.Object          // 关注的资源类型

    resyncCheckPeriod          time.Duration // 检查是否需要 resync 的周期
    defaultEventHandlerResyncPeriod time.Duration

    clock    clock.Clock
    started  bool
    stopped  bool
}
```

字段含义：

| 字段 | 含义 |
|---|---|
| `indexer` | 真正的本地缓存，所有读写最终落到这里 |
| `controller` | 把 Reflector 与 DeltaFIFO 串起来的驱动器，跑在一个独立 goroutine |
| `processor` | 维护所有注册的 EventHandler，负责事件广播 |
| `listerWatcher` | 用户传入的 List/Watch 封装，决定"看哪种资源、哪个 namespace" |
| `objectType` | 关注的 GVK，用于类型断言与日志 |
| `resyncCheckPeriod` | 多久检查一次"是否该 resync"，不等于实际 resync 周期 |

Informer 工作流可以用下面这张 ASCII 图描述：

```
                  +-----------------+
   List+Watch     |                 |
----------------> |   Reflector     |
                  |                 |
                  +-------+---------+
                          | Deltas (Added/Updated/Deleted/Sync)
                          v
                  +-----------------+
                  |   DeltaFIFO     |   有序、按 key 去重
                  |                 |
                  +-------+---------+
                          | Pop()
                          v
                  +-----------------+
                  |  HandleDeltas   |   Informer 的处理循环
                  |  (Controller)   |
                  +---+----------+--+
                      |          |
       AddOrUpdate    |          | Delete
                      v          v
              +-------------+   +-------------+
              |   Indexer   |   |   Indexer   |
              | (本地缓存)  |   | .Delete()   |
              +------+------+   +-------------+
                     |
                     v
              +-----------------+
              | sharedProcessor |  分发给所有注册的 handler
              +-----------------+
                     |
        +------------+------------+
        v            v            v
   OnAdd/OnUpdate  OnDelete   (各 handler 顺序调用)
```

关键源码要点（`shared_informer.go`）：

- `Run(stopCh)`：启动时构造 DeltaFIFO 与 controller，再启动 processor 与 controller，最后在 `Stop()` 里清理。
- `HandleDeltas(obj interface{})`：从 DeltaFIFO 弹出 Deltas 列表，逐个处理；对每个 Delta 调用 `indexer.Add/Update/Delete`，再通过 `processor.distribute` 把事件广播出去。
- `AddEventHandler`：把外部消费者注册到 `sharedProcessor`，每个 handler 会被包一层缓冲队列（`processorListener`）。
- `WaitForCacheSync(stopCh)`：阻塞等待首次 List 完成、缓存与 etcd 一致。

伪代码简化版：

```go
func (s *sharedIndexInformer) Run(stopCh <-chan struct{}) {
    fifo := NewDeltaFIFO(MetaNamespaceKeyFunc, s.indexer)
    cfg := &Config{
        Queue:            fifo,
        ListerWatcher:    s.listerWatcher,
        ObjectType:       s.objectType,
        Process:          s.HandleDeltas,
        FullResyncPeriod: s.resyncCheckPeriod,
    }
    s.controller = New(cfg)
    s.processor.Run(stopCh)
    s.controller.Run(stopCh)
}

func (s *sharedIndexInformer) HandleDeltas(obj interface{}) error {
    for _, d := range obj.(Deltas) {
        switch d.Type {
        case Sync, Added, Updated:
            if old, exists, _ := s.indexer.Get(d.Object); exists {
                s.indexer.Update(d.Object)
                s.processor.OnUpdate(old, d.Object)
            } else {
                s.indexer.Add(d.Object)
                s.processor.OnAdd(d.Object)
            }
        case Deleted:
            s.indexer.Delete(d.Object)
            s.processor.OnDelete(d.Object)
        }
    }
    return nil
}
```

#### 工程实践与常见坑

- **必须 WaitForCacheSync 后再读缓存**：Informer 启动是异步的，启动后立刻读 Indexer 可能读到空集合，导致误判"资源不存在"。控制器入口都要 `cache.WaitForCacheSync(stop, inf.HasSynced)`。

- **事件处理要快**：`OnAdd/OnUpdate/OnDelete` 是在 `sharedProcessor` 的 goroutine 里同步执行的。如果回调里做了重活（HTTP、DB 写入），会阻塞整个 Informer 的事件分发。标准做法是回调只把 key 入队到 workqueue，真正的处理在 Reconcile 循环里做（见 [第24章 Controller](./24-Controller.md)）。

- **同一个 GVR 不要建多个 Informer**：每个 Informer 都会与 API Server 建立一条 Watch 长连接。同一个资源类型请用 `SharedInformerFactory` 复用，否则 API Server 压力大、Watch 也更容易被限流。

- **Resync 周期不是越短越好**：Resync 会把缓存全量以 Sync 事件重放一遍，业务侧会收到大量"无变化"的 OnUpdate。一般 10 分钟到 1 小时之间，且要保证业务幂等。

- **List 期间 ResourceVersion 可能过期**：如果 List 数据量很大，期间 etcd 的 compact 可能让 RV 失效，导致 Watch 报 `410 Gone`，Reflector 会重新 List。这是设计上正常的，但要在监控里观测 Reflector 的 List 次数。

- **Watch 不保证不丢事件**：网络抖动、API Server 重启都会导致 Watch 断开。Informer 的"自愈"靠 List 重建 + ResourceVersion 续传，但中间被 compact 掉的事件是补不回来的，只能靠 Resync 兜底。

- **`SharedInformerFactory` 的 `Start` 与 `WaitForCacheSync` 要分开调用**：先 `Start` 启动所有 informer 的 goroutine，再 `WaitForCacheSync` 阻塞等待；如果 Start 之前调用 Wait 会死锁。

### Reflector

Reflector 是 Informer 流水线的"水源"，负责把 API Server 上的对象列表和变更流式拉到本地 DeltaFIFO 里。

#### 是什么

`Reflector` 位于 `k8s.io/client-go/tools/cache/reflector.go`，核心职责：

1. 调用 `ListFunc` 拉取资源的全量列表，把结果作为初始 Deltas 推入 DeltaFIFO（以 `Sync` 类型）。
2. 用上一步拿到的 `ResourceVersion` 启动 `WatchFunc`，把收到的事件以 `Added/Updated/Deleted` 推入 DeltaFIFO。
3. Watch 出错或断开时，根据错误类型退避重试；遇到 `410 Gone` 等致命错误则重新 List。
4. 周期性检查是否到了 resync 时间。

#### 关键结构体

```go
type Reflector struct {
    name                string                 // 用于日志和指标
    expectedType        reflect.Type           // 期望的对象类型，用于类型断言
    expectedGVK         *schema.GroupVersionKind
    store               Store                  // 通常是 DeltaFIFO
    listerWatcher       ListerWatcher          // List/Watch 的入口
    resyncPeriod        time.Duration          // resync 周期
    ShouldResync        func() bool            // 自定义 resync 判定
    clock               clock.Clock
    lastSyncResourceVersion string             // 上次同步到的 RV
    isLastSyncResourceVersionUnavailable bool  // RV 是否已经不可用（410）
    paginatedResult     bool                   // List 是否走分页
    WatchListPageSize   int64                  // List 分页大小
    nextResync          time.Time              // 下次 resync 时间
}
```

字段说明：

| 字段 | 含义 |
|---|---|
| `expectedType` | 收到的事件对象做类型断言，防止误用 |
| `store` | 通常是 DeltaFIFO，所有事件最终被 `store.Add` 进去 |
| `listerWatcher` | 由用户传入，封装了 List 和 Watch 两个 HTTP 请求 |
| `resyncPeriod` | 多久检查一次"是否该 resync"；为 0 表示不 resync |
| `lastSyncResourceVersion` | 每次 List/Watch 后更新，作为下次 Watch 的起点 |
| `isLastSyncResourceVersionUnavailable` | 收到 410 时置 true，强制下次 List |
| `WatchListPageSize` | 当 List 结果很大时启用分页，避免一次性请求超时 |
| `nextResync` | 下次触发 Resync 的绝对时间，每次 resync 后顺延 |

#### 工作原理与源码要点

`Reflector.Run(stopCh)` 主体是 `ListAndWatch` 循环：

```go
func (r *Reflector) Run(stopCh <-chan struct{}) {
    wait.Until(func() {
        if err := r.ListAndWatch(stopCh); err != nil {
            r.watchErrorHandler(r, err)
        }
    }, r.period, stopCh)
}

func (r *Reflector) ListAndWatch(stopCh <-chan struct{}) error {
    // 1. List：分页或一次性拉取
    list, listRV, err := r.list(stopCh)
    r.setLastSyncResourceVersion(listRV)
    // 2. 把 List 结果作为 Sync 事件同步到 store
    r.syncWith(list, listRV)
    // 3. 进入 Watch 循环
    for {
        select {
        case <-stopCh:
            return nil
        default:
        }
        // 4. Watch：阻塞读事件，直到出错
        w, err := r.watch(listRV, stopCh)
        if err := r.watchHandler(w, r.store, r.expectedType, ...); err != nil {
            if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
                // 410 Gone：重新 List
                r.setLastSyncResourceVersionUnavailable(true)
                return nil
            }
            return err
        }
    }
}
```

要点：

- **List 不一定一次到位**：默认情况下 client-go 不分页，但对于大资源（如 Pod、EndpointSlice），可以设置 `WatchListPageSize`，逐页拉取并合并，最后用最后一页的 RV 作为 Watch 起点。新版 client-go 还引入了 `WatchList`（基于 etcd 的 `progressNotify`），可以"边 Watch 边补全"，无需独立 List。
- **ResourceVersion 是续传凭证**：每次 Watch 都把上次收到的 RV 作为 `?resourceVersion=`。如果该 RV 在 etcd 已被 compact，API Server 返回 410，Reflector 重新 List。
- **错误分类**：网络错误直接退避重试 Watch；410/Forbidden 这类返回到外层重新 List；context canceled 退出。
- **Resync 时机**：在 Watch 循环里，每次进入循环前检查 `nextResync`，到了就调用 `store.Resync()`，把 store（DeltaFIFO）里所有对象以 Sync 事件入队。
- **WatchHandler 的事件分发**：收到 `watch.Event` 后，按 `EventType` 调用 `store.Add/Update/Delete`，并更新 `lastSyncResourceVersion`。

#### 工程实践与常见坑

- **ListFunc 中的 ResourceVersion 选择**：从缓存读还是从 etcd 读，看场景。如果需要"最新"数据，应不带 RV（让 API Server 走 quorum read）；如果只是 warm up，可用 `RV=0` 走 kube-apiserver 缓存。但 `RV=0` 在大规模集群里可能返回旧数据。

- **List 超时要设大**：全量 List 一万个 Pod 可能要几秒到几十秒。`ListWatch` 的 `Timeout` 参数务必调大，否则 List 失败反复重试，Informers 永远 sync 不上。

- **FieldSelector / LabelSelector 要慎用**：在 List/Watch 上加 selector 看似省流量，但 SharedInformer 复用时不同 selector 的 Informer 不能共享。如果业务侧关心的是"我自己的 namespace"，不如全量 Informer + Indexer Index，按 namespace 索引。

- **Watch 报错日志会刷屏**：网络抖动时 Reflector 会不断重连，日志里大量 "watch for *v1.Pod ended with: too old resource version"。建议在 `watchErrorHandler` 里加节流，并暴露 Prometheus 指标。

- **`lastSyncResourceVersionUnavailable` 误判**：早期版本某些 410 错误处理不全，会导致 Reflector 卡住。务必用较新的 client-go（v0.24+）。

- **大集群 List OOM**：List 几十万对象一次性放进内存可能 OOM。务必启用 `WatchListPageSize` 或新版 `WithWatchList` 模式，配合 server-side 分页。

### DeltaFIFO

DeltaFIFO 是 Reflector 和 Informer 处理循环之间的"缓冲带"。它是一个先入先出队列，但同一个对象（key）的多次变更会被合并成 Deltas 列表，保证消费端能拿到完整的"事件链"。

#### 是什么

`DeltaFIFO` 位于 `k8s.io/client-go/tools/cache/delta_fifo.go`，特性：

- FIFO 语义：按入队顺序出队。
- 同一 key 的多次变更会被合并为一个 Deltas（`[]Delta`）。
- 同一 key 同时只能"在队列里一份"（通过 `queue` 数组 + `dirty` 集合去重）。
- 支持 `Sync`、`Added`、`Updated`、`Deleted`、`Replaced` 几种 DeltaType。
- `Replace()` 用于处理 List 全量结果：会把"新列表里没有但缓存里有"的对象标记为 Deleted（带 tombstone）。

#### 关键结构体

```go
type DeltaFIFO struct {
    lock sync.RWMutex
    cond sync.Cond

    // 实际存储：key -> Deltas
    items map[string]Deltas

    // 按 FIFO 顺序记录的 key 列表
    queue []string

    // 已经在队列里的 key 集合（用于去重，新版用 keys map[string]struct{}）
    keyedMutexs keyedLock

    // 是否已经完成首次 List（Replace）
    populated bool
    // 第一次 Replace 进来的对象数
    initialPopulationCount int

    // 用来从对象算 key 的函数
    keyFunc KeyFunc

    // 已知对象集合，通常是 Indexer
    knownObjects KeyListerGetter

    // 控制是否把 Replaced 事件作为 Replaced 而非 Sync 暴露
    emitDeltaTypeReplaced bool
}

type DeltaType string

const (
    Added    DeltaType = "Added"
    Updated  DeltaType = "Updated"
    Deleted  DeltaType = "Deleted"
    Sync     DeltaType = "Sync"
    Replaced DeltaType = "Replaced"
)

type Delta struct {
    Type   DeltaType
    Object interface{}
}

type Deltas []Delta
```

字段说明：

| 字段 | 含义 |
|---|---|
| `items` | 真正的数据存储，key 到 Deltas 列表的映射 |
| `queue` | 维持 FIFO 顺序的 key 数组 |
| `keyedMutexs` / `keys` | 同 key 去重，防止一个 key 在 queue 里出现多次 |
| `populated` | 是否已经至少 Replace 过一次 |
| `initialPopulationCount` | 首次 Replace 的对象数，用来判断首次同步是否结束 |
| `keyFunc` | 通常 `MetaNamespaceKeyFunc`，返回 `namespace/name` |
| `knownObjects` | 一般就是 Indexer，用于 Replace 时算"谁被删了" |
| `emitDeltaTypeReplaced` | 决定 Replace 事件对上层是 Replaced 还是 Sync（影响 handler 行为） |

#### 工作原理与源码要点

生产端（Reflector）调用的方法：

- `Add(obj)` / `Update(obj)` / `Delete(obj)`：内部计算 key，调用 `queueActionLocked` 把对应 Delta 追加到 `items[key]`，并把 key 推入 `queue`（如果还没在里面）。
- `Replace(list, resourceVersion)`：把 List 的全量结果作为 Sync/Replaced Delta 入队；同时用 `knownObjects` 比对，把缓存里有但 list 里没有的对象以 Deleted 入队（带 tombstone）。这个动作是"删除漂移对象"的关键。
- `Resync()`：遍历 `knownObjects`（Indexer），把每个对象以 Sync Delta 入队。

消费端（Informer 的 controller）调用的方法：

- `Pop(process PopProcessFunc)`：阻塞直到队列非空。取出 queue[0]，把对应的 Deltas 交给 process 处理；如果 process 返回 `ErrRequeue`，调用 `AddIfNotPresent` 把 key 重新塞回队列。

伪代码：

```go
func (f *DeltaFIFO) Pop(process PopProcessFunc) (interface{}, error) {
    f.lock.Lock()
    defer f.lock.Unlock()
    for {
        for len(f.queue) == 0 {
            f.cond.Wait()
        }
        id := f.queue[0]
        f.queue = f.queue[1:]
        item, exists := f.items[id]
        if !exists {
            continue
        }
        delete(f.items, id)
        if err := process(item); err != nil {
            // 重新入队
            f.AddIfNotPresent(item)
            return nil, err
        }
        return item, nil
    }
}
```

合并 Deltas 的简化逻辑：

```go
func (f *DeltaFIFO) queueActionLocked(actionType DeltaType, obj interface{}) error {
    id := f.keyOf(obj)
    list := f.items[id]
    list = append(list, Delta{actionType, obj})
    // 去重相邻相同类型的 Delta，节省空间（DeletedFinalStateUnknown 特殊处理）
    list = dedupDeltas(list)
    f.items[id] = list
    if !f.keyInQueue(id) {
        f.queue = append(f.queue, id)
        f.cond.Signal()
    }
    return nil
}
```

要点：

- **Deltas 合并不等于事件合并**：同一个对象的 `Added, Updated, Updated` 会被合并成 3 个 Delta，但相邻同类型的 `Updated, Updated` 会被压成 1 个。
- **Deleted tombstone**：当 Reflector 检测到本地缓存里有、但 API Server 已经没有了的对象，会构造一个 `DeletedFinalStateUnknown` 作为 Object，避免业务侧误用。Handler 里要注意这种对象可能是过期的。
- **Replace 不等于 Sync**：Replace 是"用全量列表覆盖"，会把不在列表里的对象标记 Deleted；Sync 是"把缓存里的对象重新喂一遍"，不删任何东西。
- **initialPopulationCount 的作用**：Informer 用它判断"首次 List 是否处理完"，进而决定 `HasSynced()` 返回 true 还是 false。

#### 工程实践与常见坑

- **不要直接复用 DeltaFIFO**：它本质上是为 Informer 设计的内部组件，外部几乎不需要直接使用。如果要写自定义 Informer，用 `cache.NewDeltaFIFO` 配合 `Indexer` 即可。

- **Pop 是阻塞的**：消费端必须保证一直在 Pop，否则队列会无限增长，内存爆掉。Informer 里这个 Pop 是 controller 的 worker goroutine 在循环里做的。

- **Replace 期间对象激增**：如果集群里有几十万个资源，首次 Replace 会让 DeltaFIFO 瞬间膨胀。务必给控制器配置合理的 `WatchListPageSize` 和 List 限流。

- **keyFunc 必须稳定**：如果 keyFunc 对同一对象在不同时间返回不同 key（比如依赖可变字段），会导致同对象被当成多个 key 处理，Replace 时还会触发幽灵 Deleted。默认用 `MetaNamespaceKeyFunc`（namespace/name）就稳。

- **ErrRequeue 慎用**：在 Pop 的 process 函数里返回 `ErrRequeue` 会把 Deltas 塞回队列，但 Deltas 不会被合并，会再次全量重放，容易造成放大效应。生产中一般让 Informer 直接丢弃并信任下次 resync。

- **Replace 与 KnownObjects**：DeltaFIFO 本身不维护"全集"，它依赖 `knownObjects`（即 Indexer）来判断哪些对象该被 Replace 删除。如果 `knownObjects` 为 nil，Replace 会把所有 list 项作为 Sync 入队，但无法算出"漂移删除"。

### Indexer

Indexer 是 Informer 的本地缓存层，提供按 key 读写、按自定义索引检索的能力。它在内存里维护一份"对象的全量副本"，让控制器读资源时不必每次都打 API Server。

#### 是什么

`Indexer` 是一个接口（`k8s.io/client-go/tools/cache/index.go`），底层默认实现是 `threadSafeMap`。它支持：

1. 基本读写：`Add/Update/Delete/Get/List`。
2. 按 key 查询：`GetByKey`。
3. 多维索引：通过 `Indexers` 注册多个索引函数，再用 `Index` / `ByIndex` 按 indexName + indexValue 取对象。
4. 线程安全。

#### 接口与结构体

```go
type Indexer interface {
    Store
    Index(indexName string, obj interface{}) ([]interface{}, error)
    IndexKeys(indexName, indexKey string) ([]string, error)
    ListIndexFuncValues(indexName string) []string
    ByIndex(indexName, indexKey string) ([]interface{}, error)
    GetIndexers() Indexers
    AddIndexers(newIndexers Indexers) error
}

type Store interface {
    Add(obj interface{}) error
    Update(obj interface{}) error
    Delete(obj interface{}) error
    List() []interface{}
    ListKeys() []string
    Get(obj interface{}) (item interface{}, exists bool, err error)
    GetByKey(key string) (item interface{}, exists bool, err error)
    Resync() error
}
```

底层 `threadSafeMap`：

```go
type threadSafeMap struct {
    lock  sync.RWMutex
    items map[string]interface{}     // key -> object

    indexers Indexers                 // 索引名 -> 索引函数
    indices  Indices                  // 索引名 -> {索引值 -> set(key)}
}

type Indexers map[string]IndexFunc
type Indices map[string]Index
type Index   map[string]sets.String

// IndexFunc 把对象映射为若干索引值
type IndexFunc func(obj interface{}) ([]string, error)
```

字段说明：

| 字段 | 含义 |
|---|---|
| `items` | 真正的对象存储，key 通常为 `namespace/name` |
| `indexers` | 索引定义：索引名 → 索引函数 |
| `indices` | 索引数据：索引名 → {索引值 → 该值下的所有 key 集合} |

预置索引函数：

- `MetaNamespaceIndexFunc`：按 namespace 建索引，索引值为 `obj.GetNamespace()`。
- `MetaNamespaceKeyFunc`：作为 keyFunc，返回 `namespace/name`（跨 namespace 时只有 `name`）。

#### 工作原理

Add/Update/Delete 时除了改 `items`，还要更新所有 `indices`：

```go
func (c *threadSafeMap) Add(key string, obj interface{}) {
    c.lock.Lock()
    defer c.lock.Unlock()
    oldObj := c.items[key]
    c.items[key] = obj
    c.updateIndices(oldObj, obj, key)
}

func (c *threadSafeMap) updateIndices(oldObj interface{}, newObj interface{}, key string) {
    for name, indexFunc := range c.indexers {
        // 老对象的索引值集合，从 indices 里删掉 key
        if oldObj != nil {
            oldValues, _ := indexFunc(oldObj)
            for _, v := range oldValues {
                if set, ok := c.indices[name][v]; ok {
                    set.Delete(key)
                }
            }
        }
        // 新对象的索引值集合，写入 indices
        newValues, _ := indexFunc(newObj)
        for _, v := range newValues {
            if c.indices[name] == nil {
                c.indices[name] = Index{}
            }
            if c.indices[name][v] == nil {
                c.indices[name][v] = sets.NewString()
            }
            c.indices[name][v].Insert(key)
        }
    }
}

func (c *threadSafeMap) ByIndex(indexName, indexKey string) ([]interface{}, error) {
    c.lock.RLock()
    defer c.lock.RUnlock()
    set := c.indices[indexName][indexKey]
    var ret []interface{}
    for key := range set {
        ret = append(ret, c.items[key])
    }
    return ret, nil
}
```

要点：

- **索引是双向的**：`indices[name][value]` 存的是 key 集合，要拿到对象还得回到 `items[key]`。
- **更新对象时索引也要更新**：先从旧索引值集合里删除该 key，再加到新值集合里，否则索引会脏。
- **AddIndexers 必须在缓存为空时调用**：如果缓存已经有数据，新加的索引不会回填到老对象上，会抛错。

#### 工程实践与常见坑

- **善用 Index 减少 List**：比如 `cache.Indexers{"node": func(obj) { return []string{pod.Spec.NodeName} }}`，可以 O(1) 查出某个节点上的所有 Pod，比 `List` + 过滤快几个数量级。

- **不要在 IndexFunc 里抛 panic**：IndexFunc 是在 Add/Update 时同步调用的，panic 会让整个 Informer 崩溃。

- **缓存不是数据库**：Indexer 里的对象可能因为 410/Gone 暂时与 etcd 不一致，业务逻辑要做幂等，不能依赖"Indexer 一定是最新"。

- **大对象不要全量缓存**：例如 ConfigMap、Secret 这种"巨型"对象，全量缓存会吃内存。可以用 List-Watch + selector，或干脆不用 Informer 直接 Get。

- **不要直接修改缓存对象**：从 Indexer 拿到的对象是指针，业务侧修改它等于修改缓存。要修改请 deepcopy（`obj.DeepCopyObject()`），否则下一个 OnUpdate 收到的 `oldObj` 已经被你改过了，diff 失效。

- **`AddIndexers` 时机**：必须在 `sharedIndexInformer.AddIndexers` 且 Informer 启动前（缓存还没数据）调用。一旦 `factory.Start` 之后再加，会报错。

### SharedInformer

SharedInformer 是 Informer 的"多租户"版本：多个业务方共享同一个 Informer 实例，但每个方注册自己的 EventHandler，互不干扰。

#### 是什么

`SharedInformer` 接口（`k8s.io/client-go/informers`）由 `sharedIndexInformer` 实现，配合 `SharedInformerFactory` 使用：

- 同一个 GVR（Group/Version/Resource）只创建一个 Informer 实例。
- 多个 Controller 注册自己的 EventHandler，事件通过 `sharedProcessor` 广播。
- 所有方共享同一份 Indexer 缓存。

#### 关键结构体

```go
type sharedProcessor struct {
    listenersStarted bool
    listenersLock    sync.RWMutex
    listeners        []*processorListener     // 所有已注册的 listener
    syncingListeners []*processorListener     // 还在首次同步的 listener 子集
    clock            clock.Clock
    wg               wait.Group
}

type processorListener struct {
    nextCh chan interface{}      // 给用户回调的 goroutine 读
    addCh  chan interface{}      // distribute 写入

    handler ResourceEventHandler // 用户注册的 OnAdd/OnUpdate/OnDelete

    pendingNotifications buffer.RingGrowing  // 缓冲队列

    requestedResyncPeriod time.Duration
    resyncPeriod          time.Duration
    nextResync            time.Time
    resyncLock            sync.Mutex
}
```

字段说明：

| 字段 | 含义 |
|---|---|
| `listeners` | 所有已注册的 EventHandler |
| `syncingListeners` | 还没完成首次同步的 listener 子集 |
| `nextCh` / `addCh` | 用两个 channel + ring buffer 实现"非阻塞分发 + 顺序消费" |
| `handler` | 用户注册的回调 |
| `requestedResyncPeriod` | 每个 listener 可以单独配置 resync 周期 |
| `pendingNotifications` | 当 nextCh 满时，事件先落到 ring buffer，避免丢 |

#### 工作原理

事件分发链路：

```
HandleDeltas
   |
   v
sharedProcessor.distribute(obj, sync)
   |
   | (持 listeners 锁)
   v
processorListener.addCh <- obj    // 非阻塞写，缓冲满则走 ring
   |
   v
processorListener.pop()           // 内部 goroutine 把 addCh 和 ring 合并
   |
   v
processorListener.run()           // 内部 goroutine 从 nextCh 读，调 handler
   |
   v
ResourceEventHandler.OnAdd/OnUpdate/OnDelete
```

关键点：

- **每个 listener 一对 goroutine**：`run()` 和 `pop()`。`pop()` 负责把 addCh 与 resync 事件合并到 nextCh；`run()` 负责从 nextCh 读并调用 handler。这样 handler 慢不会阻塞 distribute（最多让 ring 增长）。
- **distribute 是同步的**：`sharedProcessor` 持锁遍历所有 listener，往各自的 `addCh` 写。如果某个 listener 的 `addCh` 满，会被阻塞，从而影响整体分发。这就是为什么 handler 一定要"轻"。
- **Resync 独立**：每个 listener 有自己的 `nextResync`，到点了由 `pop()` 注入 Sync 事件。
- **syncingListeners 的作用**：首次同步阶段，distribute 只把事件发给 syncingListeners 子集，已经 synced 的 listener 不再收 Sync 事件。

#### 工程实践与常见坑

- **用 SharedInformerFactory 而不是手搓 Informer**：

```go
package main

import (
    "context"
    "time"

    "k8s.io/client-go/informers"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/tools/cache"
)

func main() {
    var client kubernetes.Interface // 假设已初始化
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    factory := informers.NewSharedInformerFactory(client, time.Minute)
    podInformer := factory.Core().V1().Pods()
    podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc: func(obj interface{}) { /* 入队 */ },
        UpdateFunc: func(old, cur interface{}) { /* 入队 */ },
        DeleteFunc: func(obj interface{}) { /* 入队 */ },
    })
    factory.Start(ctx.Done())
    cache.WaitForCacheSync(ctx.Done(), podInformer.Informer().HasSynced)
}
```

- **不同 namespace 用不同 factory**：`NewSharedInformerFactoryWithOptions(client, resync, informers.WithNamespace(ns))`。同 namespace 内的 GVR 共享；不同 namespace 是独立的 Informer 实例。

- **Resync 周期要协调**：所有 listener 共享一个 Reflector，但 resync 是 per-listener 的。listenerA 设 30s、listenerB 设 10min 是允许的，但 Reflector 实际取最小值检查。

- **EventHandler 注册时机**：必须在 `factory.Start` 之前 `AddEventHandler`，否则首次 List 的事件可能漏掉。如果必须动态注册，新版 client-go 的 `AddEventHandlerWithResyncPeriod` 可以正确处理"中途加入"，但首次同步事件不保证。

- **`WaitForCacheSync` 要传 Informer 的 HasSynced**：工厂级 `WaitForCacheSync` 是等待所有 informer，业务侧一般单独 `podInformer.Informer().HasSynced`。

- **不要在 handler 里直接修改缓存对象**：见 Indexer 章节，要 deepcopy。

- **`RemoveEventHandler` 是 v0.27+ 才稳定**：动态注销 listener 在老版本里有内存泄漏问题，升级前要确认版本。

### 本章小结

- Informer 是 client-go 的核心抽象，把 List+Watch 封装为"本地缓存 + 事件分发"流水线。
- Reflector 负责从 API Server 拉数据，处理 List/Watch/Resync，是流水线的"水源"，靠 ResourceVersion 续传、410 重 List 实现自愈。
- DeltaFIFO 是中间缓冲，按 key 合并 Deltas，保证消费端拿到完整事件链；Replace 处理全量同步，Resync 周期性重放。
- Indexer 是本地缓存（threadSafeMap），支持按 key 与自定义索引查询，是控制器读资源的"快路径"。
- SharedInformer 让多业务方共享同一份缓存和 Reflector，配合 SharedInformerFactory 实现资源复用。
- 工程实践的核心是：**用工厂复用、handler 入队不干活、WaitForCacheSync 后再读、Resync 不要太频繁、对象不可信要 deepcopy**。

掌握 Informer 后，下一章我们将进入 [第24章 Controller](./24-Controller.md)，看 controller-runtime 如何在 Informer 之上构建 Reconcile 控制循环。
