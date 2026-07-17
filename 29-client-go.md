## 第29章 client-go

> 版本基线：`k8s.io/client-go v0.36.2`，对应 Kubernetes 1.36，要求 Go 1.26。Informer 的公开契约较稳定，但 Reflector 字段、WatchList 和队列类型会随版本变化；伪代码不代表跨版本 ABI。

### Informer

Informer 是 client-go 中用于在本地缓存 Kubernetes 资源并监听变更的高层抽象。它把初始状态获取（WatchList，或传统 List）与后续 Watch 封装成事件驱动流水线，再通过回调把变化分发给业务逻辑。

#### 是什么

一个 Informer 至少包含以下能力：

1. 通过 ListerWatcher 获取初始状态并建立持续 Watch。
2. 把初始状态和 Watch 事件写入内部 Queue；v0.36 默认是原子事件模式的 RealFIFO。
3. 用一个消费者循环取出 Delta，更新 Indexer（本地缓存）。
4. 把变更以 OnAdd/OnUpdate/OnDelete 形式分发给注册的 ResourceEventHandler。
5. 周期性触发 Resync，把缓存里的对象重新以 Sync 事件喂给 handler。

它与直接调用 Watch 相比最大的好处是：本地缓存可以反复读、读不访问 API Server；client-go 负责 Watch 中断、续传和 relist，业务侧主要编写幂等的状态协调逻辑。

#### 数据结构与工作流

Informer 的标准实现是 `cache.SharedIndexInformer`（在 `k8s.io/client-go/tools/cache/shared_informer.go`）。核心字段简化如下：

```go
type sharedIndexInformer struct {
    indexer       Indexer                 // 本地缓存，最终由 threadSafeMap 存储
    controller    Controller              // 内部 Reflector + Queue 的驱动器
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
| `controller` | 把 Reflector 与内部 Queue 串起来的驱动器，跑在独立 goroutine 中 |
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
                          | Delta / atomic Delta
                          v
                  +-----------------+
                  |    RealFIFO     |   v0.36 默认：按到达顺序保留事件
                  | (DeltaFIFO 兼容) |
                  |                 |
                  +-------+---------+
                          | Pop()/PopBatch()
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

- `RunWithContext(ctx)`：通过 `newQueueFIFO` 构造 Queue 和 controller，启动 processor，再运行 controller；controller 停止后才停止 processor，避免丢掉已分发的通知。
- `handleDeltas` / `handleBatchDeltas`：消费 Queue，更新 Indexer，再通过 `processor.distribute` 广播通知。
- `AddEventHandler`：把外部消费者注册到 `sharedProcessor`，每个 handler 会被包一层缓冲队列（`processorListener`）。
- `WaitForCacheSync(stopCh)`：等待传入的 `HasSynced` 函数为 true；若 stop channel 先关闭则返回 false。它不提供“此刻与 apiserver/etcd 完全一致”的保证。

v0.36.2 的默认 feature gate 组合很重要：`InOrderInformers=true`（GA 且锁定）选择 RealFIFO，`AtomicFIFO=true` 让 Replace/Resync 分别以单个 `ReplacedAll` / `SyncAll` 事件提交，`InOrderInformersBatchProcess=true` 启用批处理，`UnlockWhileProcessingFIFO=true` 允许满足条件时在处理期间释放队列锁。不要把旧版 DeltaFIFO 的按 key 聚合语义套到这条默认路径上。

伪代码简化版：

```go
func (s *sharedIndexInformer) RunWithContext(ctx context.Context) {
    logger, queue := newQueueFIFO(
        klog.FromContext(ctx), s.objectType, s.indexer, s.transform,
        s.identifier, s.informerMetricsProvider,
    )
    cfg := &Config{
        Queue:            queue,
        ListerWatcher:    s.listerWatcher,
        ObjectType:       s.objectType,
        Process: func(obj interface{}, initial bool) error {
            return s.handleDeltas(logger, obj, initial)
        },
        ProcessBatch: func(deltas []Delta, initial bool) error {
            return s.handleBatchDeltas(logger, deltas, initial)
        },
        FullResyncPeriod: s.resyncCheckPeriod,
        ShouldResync:     s.processor.shouldResync,
    }
    s.controller = New(cfg)
    go s.processor.run(ctx)
    s.controller.RunWithContext(ctx)
}

func processDeltas(handler ResourceEventHandler, store Store, deltas Deltas) error {
    for _, d := range deltas {
        switch d.Type {
        case ReplacedAll:
            // 与 store 当前状态比较后，原子 Replace，再发出 Delete/Add/Update。
        case SyncAll:
            // 遍历 store，以 OnUpdate(obj, obj) 触发本地 resync。
        case Sync, Replaced, Added, Updated:
            if old, exists, _ := store.Get(d.Object); exists {
                store.Update(d.Object)
                handler.OnUpdate(old, d.Object)
            } else {
                store.Add(d.Object)
                handler.OnAdd(d.Object, false)
            }
        case Deleted:
            store.Delete(d.Object)
            handler.OnDelete(d.Object)
        case Bookmark:
            // 只推进 store 记录的 ResourceVersion，不产生业务回调。
        }
    }
    return nil
}
```

#### 工程实践与常见坑

- **必须检查 WaitForCacheSync 的返回值**：Informer 启动是异步的，启动后立刻读 Indexer 可能读到空集合。控制器应在 `cache.WaitForCacheSync(stop, inf.HasSynced)` 返回 true 后启动 worker；false 表示等待被取消，应直接退出。

- **事件处理要快**：每个 listener 有独立消费 goroutine，慢 handler 通常不会立刻阻塞其他 handler，但会让自己的 pending ring 无界增长并延迟处理。标准做法是回调只提取 key 并入 workqueue，真正处理放到 Reconcile（见[第30章](./30-Controller.md)）。

- **同一个 GVR 不要建多个 Informer**：每个 Informer 都会与 API Server 建立一条 Watch 长连接。同一个资源类型请用 `SharedInformerFactory` 复用，否则 API Server 压力大、Watch 也更容易被限流。

- **Resync 周期不是越短越好**：Resync 不会重新向 apiserver List，也不补回历史 Watch 事件；它只是把本地缓存对象重新送给 handler，推动水平触发的 Reconcile。多数控制器可设为 0，确有周期校验需求时再开启。

- **ResourceVersion 是不透明令牌**：不能按数字解析、比较或自行加一。Reflector 从 List/Watch 响应取得 RV，再交回 apiserver 续传。

- **Watch 中断不等于最终状态丢失**：Reflector 会续传或重新 List。若 RV 已 compact，410 后的 relist 建立新的当前状态；水平触发控制器不依赖重放每个中间事件。

- **先 Start，再 WaitForCacheSync**：factory 只等待已经启动的 informer。若在 `Start` 之前调用 factory 的 `WaitForCacheSync`，它可能因为待等待集合为空而立即返回，造成“已经同步”的假象，并非可靠的启动屏障。

### Reflector

Reflector 是 Informer 流水线的"水源"，负责从 API Server 获取初始状态并持续接收变化，再写入上层提供的 `ReflectorStore`（SharedInformer 场景下就是内部 Queue）。

#### 是什么

`Reflector` 位于 `k8s.io/client-go/tools/cache/reflector.go`，核心职责：

1. 优先用 WatchList 获取一致的初始状态；服务端不支持时退回传统 List。
2. 用初始状态的 `ResourceVersion` 延续 Watch，把变化写成 `Added/Updated/Deleted`；bookmark 只推进进度。
3. Watch 结束后按错误类型续传、退避或 relist；RV 不可用时重新建立当前快照。
4. 在独立 goroutine 中按配置调用 `store.Resync()`。

#### 关键结构体

```go
type Reflector struct {
    name          string
    expectedType  reflect.Type
    expectedGVK   *schema.GroupVersionKind
    store         ReflectorStore
    listerWatcher ListerWatcherWithContext

    resyncPeriod   time.Duration
    ShouldResync   func() bool
    delayHandler   wait.DelayFunc
    minWatchTimeout time.Duration
    maxWatchTimeout time.Duration
    clock          clock.Clock

    paginatedResult bool
    lastSyncResourceVersion              string
    isLastSyncResourceVersionUnavailable bool
    lastSyncResourceVersionMutex         sync.RWMutex

    watchErrorHandler WatchErrorHandlerWithContext
    WatchListPageSize int64
    MaxInternalErrorRetryDuration time.Duration
    useWatchList bool
}
```

字段说明：

| 字段 | 含义 |
|---|---|
| `expectedType` | 收到的事件对象做类型断言，防止误用 |
| `store` | Queue 实现的 `ReflectorStore`，接收 Replace/Add/Update/Delete/Resync |
| `listerWatcher` | 封装带 context 的 List 和 Watch 请求 |
| `resyncPeriod` / `ShouldResync` | resync 定时周期与本轮是否执行的判定函数；周期为 0 时禁用 |
| `delayHandler` | ListAndWatch 外层循环及可重试 Watch 请求的退避策略 |
| `lastSyncResourceVersion` | 最近确认的 RV，受 mutex 保护，用于后续 Watch 或 relist |
| `isLastSyncResourceVersionUnavailable` | 最近使用该 RV 的请求报告过期或 RV 过大；下一次从空 RV 重建 |
| `WatchListPageSize` | 传统 List 路径的显式分页大小；非零值可能迫使请求绕过 watch cache 直达 etcd |
| `useWatchList` | 是否优先使用 streaming list；由 `WatchListClient` feature gate 和 ListerWatcher 能力决定 |

#### 工作原理与源码要点

`Reflector.RunWithContext` 主体是带退避的 `ListAndWatchWithContext` 循环：

```go
func (r *Reflector) RunWithContext(ctx context.Context) {
    r.delayHandler.Until(ctx, true, true, func(ctx context.Context) (bool, error) {
        if err := r.ListAndWatchWithContext(ctx); err != nil {
            r.watchErrorHandler(ctx, r, err)
        }
        return false, nil
    })
}

func (r *Reflector) ListAndWatchWithContext(ctx context.Context) error {
    var w watch.Interface
    fallbackToList := !r.useWatchList

    if r.useWatchList {
        // 收集 synthetic Added，直到 initial-events-end bookmark；
        // 然后 store.Replace(snapshot, rv)，并复用同一条 Watch。
        w, err = r.watchList(ctx)
        if err != nil {
            fallbackToList = true
            w = nil
        }
    }
    if fallbackToList {
        // 分页或非分页 List，最终同样调用 store.Replace(items, rv)。
        if err := r.list(ctx); err != nil { return err }
    }

    // startResync 在后台运行；watch 消费增量事件并按需重建连接。
    return r.watchWithResync(ctx, w)
}
```

要点：

- **v0.36 默认优先 WatchList**：`WatchListClient` 默认开启。WatchList 用 synthetic Added 构造临时快照，以带 `k8s.io/initial-events-end` 标记的 bookmark 确认初始流结束，再一次性 Replace Queue；不支持时自动退回传统 List + Watch。
- **ResourceVersion 是续传凭证**：Watch 使用最近确认的 RV。RV 过期时 Reflector 先尝试“不早于最近 RV”的 relist；若 List 仍报告过期或 RV 过大，再用空 RV 取得最新一致快照。
- **错误处理分层**：可重试的建链错误和 429 会退避，部分内部错误在期限内原地重试；其他错误结束本轮 ListAndWatch，由外层退避后重新初始化。context 取消则退出。
- **Resync 与 Watch 并行**：`startResync` 使用独立 timer；到期且 `ShouldResync` 返回 true 时调用 `store.Resync()`。默认原子 RealFIFO 只入队一个 `SyncAll`，之后由消费端遍历 Indexer 产生 OnUpdate。
- **Watch 事件分发**：Added/Modified/Deleted 分别调用 `store.Add/Update/Delete`。bookmark 可推进 store 与 Reflector 记录的 RV，但不产生 OnAdd/OnUpdate/OnDelete。

#### 工程实践与常见坑

- **ResourceVersion 匹配语义要交给 API 文档**：空值、`0`、`resourceVersionMatch=NotOlderThan/Exact` 的一致性和缓存行为不同。通用 Informer 优先使用 client-go 默认策略，不自行拼 query。

- **List/Watch 必须有整体容量预算**：超时、分页、WatchList、apiserver inflight 限制和客户端 rate limiter 要一起设计，不能只把超时调大掩盖对象数量失控。

- **selector 和 namespace 是缓存身份的一部分**：服务端过滤能显著减少网络与内存，但不同过滤条件不能安全共享同一份 Informer。明确缓存边界，并确保 List 与 Watch 使用完全相同的条件。

- **自定义 watchErrorHandler 必须快速返回**：网络抖动时错误可能连续出现。不要在 handler 内执行同步网络 I/O；按错误类别聚合日志与指标，避免高基数标签和重复告警。

- **不要复制旧版 Reflector 修复**：使用受支持的 v0.36 补丁版本，并把 List、Watch 重启、410、解码错误和最后成功同步时间纳入指标。

- **大集群先缩小缓存集合**：优先使用 namespace/selector，并验证默认 WatchList 是否被服务端接受。不要把 `WatchListPageSize` 当作通用 OOM 开关；显式分页会直接访问 etcd，可能放大控制面负载，应基于压测和 apiserver 指标调整。

### Queue：RealFIFO 与 DeltaFIFO

Reflector 和 Indexer 之间有一个实现 `Queue` 的缓冲层。v0.36.2 的标准 SharedInformer 使用 RealFIFO；DeltaFIFO 仍保留在包中，主要用于兼容旧版行为或显式构造的低层组件。

#### v0.36 默认：RealFIFO

RealFIFO 的核心目标是按进入 Queue 的顺序传递每个通知，不把同 key 的多次变化折叠成一个队列项：

```go
type RealFIFO struct {
    lock sync.RWMutex
    cond sync.Cond

    items []Delta

    populated             bool
    initialPopulationCount int
    synced                chan struct{}

    keyFunc KeyFunc
    transformer TransformFunc

    batchSize              int // 默认 1000
    emitAtomicEvents       bool
    unlockWhileProcessing bool
    emitDeltaTypeBookmark bool
}

type ReplacedAllInfo struct {
    ResourceVersion string
    Objects         []interface{}
}

type SyncAllInfo struct{}
type BookmarkInfo struct{ ResourceVersion string }
```

默认原子事件路径的行为：

- `Add/Update/Delete` 各追加一个 Delta；同一 key 连续变化也不会合并。
- `Replace(items, rv)` 只入队一个 `ReplacedAll`。消费时先将新快照与 Indexer 比较，再调用 `Store.Replace`，最后按 delete、add/update 的顺序分发回调。
- `Resync()` 只入队一个 `SyncAll`。消费时遍历当前 Indexer，并对每个对象调用 `OnUpdate(obj, obj)`；这不会访问 API Server。
- Watch bookmark 入队为 `Bookmark`，消费后只更新 Indexer 记录的 RV，不触发业务 handler。
- 默认批处理从队首最多取 1000 个不同 key 的普通事件；遇到重复 key 或 `ReplacedAll/SyncAll/Bookmark` 等不可批处理事件就结束本批。支持事务的 Store 会先批量更新缓存，再执行对应回调。
- `HasSynced` 在首次 Replace 对应的队列项处理完成后为 true。这表示初始快照已应用到本地 Store，不表示 listener 回调都已执行完，更不表示缓存此刻与服务端线性一致。

RealFIFO 解决的是队列层的通知顺序，不把 Informer 变成持久事件日志。断线 relist 仍可能只恢复当前状态，handler 必须面向当前 level 幂等协调。

#### 兼容实现：DeltaFIFO

DeltaFIFO 的数据模型不同：`items` 按 key 累积 Deltas，`queue` 只保存每个待处理 key 的一个位置。

```go
type DeltaFIFO struct {
    lock sync.RWMutex
    cond sync.Cond

    items map[string]Deltas // key -> 尚未处理的变化序列
    queue []string          // 不含重复 key

    populated             bool
    initialPopulationCount int
    synced                chan struct{}

    keyFunc     KeyFunc
    knownObjects KeyListerGetter
    emitDeltaTypeReplaced bool
}
```

这里没有额外的 `dirty`、`keys` 或 `keyedMutexs` 字段。`items[key]` 是否存在本身就决定 key 是否已经位于 `queue` 中。

DeltaFIFO 的关键语义：

- 同一 key 在 Pop 前收到的 Added/Updated/Deleted 会依次追加到一个 `Deltas` 中；key 只在 queue 中出现一次。
- `dedupDeltas` 只检查最后两个 Delta，而且当前仅合并连续的 `Deleted, Deleted`，尽量保留信息更完整的删除对象。连续 `Updated, Updated` 不会被压缩。
- `Replace` 为新列表逐项入队 `Replaced`（兼容选项关闭时为 `Sync`），并根据排队项与 `knownObjects` 为快照中消失的 key 生成 `DeletedFinalStateUnknown`。
- `Resync` 遍历 `knownObjects`，只为当前尚未排队的 key 增加 `Sync`。
- `Pop` 在锁内移除并调用 process；process 返回错误时，DeltaFIFO 只把错误返回给调用者，不会自动重入队。低层自定义消费者若要重试，必须明确调用 `AddIfNotPresent`。
- `initialPopulationCount` 统计首次 Replace 需要消费的 key 数；降到 0 后 `HasSynced` 才为 true。

#### 工程实践与常见坑

- **让 Informer 选择 Queue**：业务代码优先使用 SharedInformer 构造器，不直接绑定 RealFIFO 或 DeltaFIFO。队列默认值和 feature gate 随 client-go minor 版本演进。
- **删除回调要处理 tombstone**：relist 发现缓存中消失的对象时，`OnDelete` 可能收到 `DeletedFinalStateUnknown`。只需要 key 时可用 `DeletionHandlingMetaNamespaceKeyFunc`；需要对象时先做类型分支。
- **不要依赖收到每个中间状态**：RealFIFO 保留进入客户端队列的通知，DeltaFIFO 保留 key 在队列中积累的 Deltas，但网络断线、服务端压缩和 relist 都可能让中间状态不可见。
- **监控积压而非假定有界**：Queue 和每个 processorListener 都可能积压。handler 应只提取 key 并写入带限速与去重的 workqueue，业务 I/O 放到 worker 中。
- **自定义 keyFunc 必须稳定**：同一对象在生命周期内应映射到稳定 key。Kubernetes 对象通常使用 `MetaNamespaceKeyFunc`，即 namespaced 对象为 `namespace/name`、cluster-scoped 对象为 `name`。

### Indexer

Indexer 是 Informer 的本地缓存层，提供按 key 读写、按自定义索引检索的能力。它在内存里维护一份"对象的全量副本"，让控制器读资源时不必每次都打 API Server。

#### 是什么

`Indexer` 是一个接口（`k8s.io/client-go/tools/cache/index.go`）。`NewIndexer` 返回一个负责 key 计算的 `cache` 包装器，线程安全存储最终由 `threadSafeMap` 完成。它支持：

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
    LastStoreSyncResourceVersion() string
    Bookmark(rv string)
    Get(obj interface{}) (item interface{}, exists bool, err error)
    GetByKey(key string) (item interface{}, exists bool, err error)
    Replace(items []interface{}, resourceVersion string) error
    Resync() error
}
```

关键实现层次：

```go
type cache struct {
    cacheStorage ThreadSafeStore
    keyFunc      KeyFunc
    transformer  TransformFunc
}

type threadSafeMap struct {
    lock  sync.RWMutex
    items map[string]interface{} // key -> object
    index *storeIndex
    rv    string
}

type storeIndex struct {
    indexers Indexers // 索引名 -> 索引函数
    indices  Indices  // 索引名 -> {索引值 -> set(key)}
}

type Indexers map[string]IndexFunc
type Indices  map[string]index
type index    map[string]sets.Set[string]

// IndexFunc 把对象映射为若干索引值
type IndexFunc func(obj interface{}) ([]string, error)
```

字段说明：

| 字段 | 含义 |
|---|---|
| `cache.keyFunc` | 从对象计算 Store key；读写对象接口与内部 key-value 存储的适配层 |
| `threadSafeMap.items` | 真正的对象存储，key 通常为 `namespace/name` |
| `storeIndex.indexers` | 索引定义：索引名 → 索引函数 |
| `storeIndex.indices` | 索引数据：索引名 → {索引值 → 该值下的所有 key 集合} |
| `threadSafeMap.rv` | Store 最近观察到的 ResourceVersion；相关能力受版本和 feature gate 影响 |

预置索引函数：

- `MetaNamespaceIndexFunc`：按 namespace 建索引，索引值为 `obj.GetNamespace()`。
- `MetaNamespaceKeyFunc`：作为 keyFunc，namespaced 对象返回 `namespace/name`，cluster-scoped 对象返回 `name`。

#### 工作原理

Add/Update/Delete 时除了改 `items`，还要更新所有 `indices`：

```go
func (c *threadSafeMap) Add(key string, obj interface{}) {
    c.Update(key, obj)
}

func (c *threadSafeMap) updateLocked(key string, obj interface{}) {
    oldObj := c.items[key]
    c.items[key] = obj
    c.index.updateIndices(oldObj, obj, key)
}

func (c *threadSafeMap) ByIndex(indexName, indexedValue string) ([]interface{}, error) {
    c.lock.RLock()
    defer c.lock.RUnlock()

    keys, err := c.index.getKeysByIndex(indexName, indexedValue)
    if err != nil { return nil, err }
    result := make([]interface{}, 0, len(keys))
    for key := range keys {
        result = append(result, c.items[key])
    }
    return result, nil
}
```

要点：

- **索引是间接表**：`indices[name][value]` 存的是 key 集合，要拿到对象还要回到 `items[key]`。
- **更新对象时索引也要更新**：先从旧索引值集合里删除该 key，再加到新值集合里，否则索引会脏。
- **新版 AddIndexers 会回填已有对象**：当前 `threadSafeMap.AddIndexers` 在写锁下注册索引，并遍历缓存中的现有对象建立索引。老版本行为不同；对大缓存动态添加索引会阻塞并产生明显 CPU 开销，应按所用 client-go 版本测试。

#### 工程实践与常见坑

- **善用 Index 减少 List**：比如 `cache.Indexers{"node": func(obj) { return []string{pod.Spec.NodeName} }}`。索引值到 key 集合的定位通常是平均 O(1)，但返回 k 个对象仍需 O(k) 遍历和结果分配；相对全量 `List` + 过滤的收益取决于缓存规模与命中数。

- **IndexFunc 必须快速、确定且覆盖所有对象**：默认 `threadSafeMap` 在持有写锁时调用它；若它返回 error，更新索引的内部路径会 panic。不要做 I/O，不要依赖可变外部状态，并用 typed/unstructured 对象及缺失可选字段等边界做测试。

- **缓存不是数据库**：Indexer 里的对象可能因为 410/Gone 暂时与 etcd 不一致，业务逻辑要做幂等，不能依赖"Indexer 一定是最新"。

- **评估大对象与敏感对象的缓存成本**：ConfigMap、Secret 等可能显著增加内存和权限暴露。优先缩小 namespace/selector 范围；确实只需少数字段时，可在启动前配置经过契约审查的 `TransformFunc`。

- **不要直接修改缓存对象**：从 Indexer 拿到的对象是指针，业务侧修改它等于修改缓存。要修改请 deepcopy（`obj.DeepCopyObject()`），否则下一个 OnUpdate 收到的 `oldObj` 已经被你改过了，diff 失效。

- **`AddIndexers` 时机**：优先在 Informer 启动前注册，避免运行中回填阻塞事件处理。当前版本允许在启动后、停止前添加并回填现有对象，但这是版本相关行为，且大缓存上的代价不可忽略。

### SharedInformer

SharedInformer 让多个业务方共享同一个 Informer 实例，并为每个 EventHandler 维护独立的顺序与待处理缓冲。它们仍共享进程、缓存和上游流水线；某个 handler 长期落后造成的内存增长会影响整个进程。

#### 是什么

`SharedInformer` 接口位于 `k8s.io/client-go/tools/cache`，由 `sharedIndexInformer` 实现；通常通过 `k8s.io/client-go/informers` 中的 `SharedInformerFactory` 创建：

- 同一个 factory 配置下，同一对象类型只创建一个 Informer 实例。
- 多个 Controller 注册自己的 EventHandler，事件通过 `sharedProcessor` 广播。
- 所有方共享同一份 Indexer 缓存。

#### 关键结构体

```go
type sharedProcessor struct {
    listenersStarted bool
    listenersLock    sync.RWMutex
    listeners        map[*processorListener]bool // bool 表示当前是否接收 Sync 事件
    clock            clock.Clock
    wg               wait.Group
}

type processorListener struct {
    nextCh chan interface{}      // 无缓冲：pop 写，run 读
    addCh  chan interface{}      // 无缓冲：distribute 写，pop 读

    handler ResourceEventHandler // 用户注册的 OnAdd/OnUpdate/OnDelete

    pendingNotifications buffer.RingGrowing  // 缓冲队列
    syncTracker         *synctrack.SingleFileTracker
    upstreamHasSynced   DoneChecker

    requestedResyncPeriod time.Duration
    resyncPeriod          time.Duration
    nextResync            time.Time
    resyncLock            sync.Mutex
}
```

字段说明：

| 字段 | 含义 |
|---|---|
| `listeners` | EventHandler listener 到“本轮是否接收 Sync 事件”的映射 |
| `nextCh` / `addCh` | 两个无缓冲 channel；`pop` 在中间用 ring buffer 解耦分发与回调 |
| `handler` | 用户注册的回调 |
| `requestedResyncPeriod` | 每个 listener 可以单独配置 resync 周期 |
| `pendingNotifications` | `run` 跟不上时保存待处理事件；当前是无界增长队列，不会主动背压 |
| `syncTracker` | 合并上游 Informer 同步状态与本 listener 初始事件处理进度 |

#### 工作原理

事件分发链路：

```
HandleDeltas
   |
   v
sharedProcessor.distribute(obj, sync)
   |
   | (持 listeners 读锁并同步遍历)
   v
processorListener.addCh <- obj    // 无缓冲，等待该 listener 的 pop 接收
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

- **每个 listener 三个 goroutine**：`run()` 从 `nextCh` 读取并同步调用 handler；`pop()` 从 `addCh` 接收，再把暂时发不进 `nextCh` 的通知放入 `pendingNotifications`；`watchSynced()` 把上游同步完成信号交给 listener 的同步 tracker。这保留了单个 listener 的通知顺序。
- **慢 handler 主要转化为无界内存积压**：`addCh` 和 `nextCh` 都是无缓冲 channel，ring buffer 位于 `pop()` 内部。通常 `pop()` 仍能接收分发并让 ring 增长，因此 handler 慢不会立刻阻塞所有 listener，但持续落后可能最终 OOM；若 `pop()` 本身不能运行，`distribute` 的同步发送也会停住。回调应只做轻量入队，并监控积压与内存。
- **Resync 由上游统一触发、下游按 listener 过滤**：`processor.shouldResync()` 根据每个 listener 的 `nextResync` 更新 `listeners` map 中的 bool；只要一个 listener 到期，Reflector 就调用 Queue 的 `Resync()`。默认路径消费 `SyncAll` 后产生 RV 不变的 OnUpdate，`sharedProcessor` 仅转发给本轮 bool 为 true 的 listener。
- **listeners 中 bool 的作用**：普通变更发给全部 listener；resync 通知只发给当前标记为 syncing 的 listener。这个 bool 不是“首次同步是否完成”，首次事件处理进度由 `syncTracker` 单独跟踪。

#### 工程实践与常见坑

- **用 SharedInformerFactory 而不是手搓 Informer**：

```go
import (
    "context"

    "k8s.io/client-go/informers"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/tools/cache"
)

func startPodInformer(ctx context.Context, client kubernetes.Interface) (informers.SharedInformerFactory, error) {
    factory := informers.NewSharedInformerFactory(client, 0)
    podInformer := factory.Core().V1().Pods()

    registration, err := podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc: func(obj interface{}) { /* 入队 */ },
        UpdateFunc: func(old, cur interface{}) { /* 入队 */ },
        DeleteFunc: func(obj interface{}) { /* 入队 */ },
    })
    if err != nil {
        return nil, err
    }

    factory.StartWithContext(ctx)
    if err := factory.WaitForCacheSyncWithContext(ctx).AsError(); err != nil {
        return nil, err
    }
    if !cache.WaitFor(ctx, "Pod event handler", registration.HasSyncedChecker()) {
        return nil, context.Cause(ctx)
    }
    return factory, nil
}
```

- **不同 namespace 用不同 factory**：`NewSharedInformerFactoryWithOptions(client, resync, informers.WithNamespace(ns))`。同 namespace 内的 GVR 共享；不同 namespace 是独立的 Informer 实例。

- **Resync 周期要协调**：所有 listener 共享一个 Reflector，但 resync 是 per-listener 的。listenerA 设 30s、listenerB 设 10min 是允许的，但 Reflector 实际取最小值检查。

- **EventHandler 注册时机**：初始化阶段优先在 `factory.Start` 前注册，最易推理。动态注册要保留返回的 registration，检查它自身的同步状态，并为可能收到的当前缓存对象保持幂等。

- **区分 cache sync 与 handler sync**：factory 的 `WaitForCacheSyncWithContext` 只保证已启动 Informer 的初始快照进入 Store。若后续 worker 依赖某个 handler 已消费完初始通知，还要等待 `ResourceEventHandlerRegistration.HasSyncedChecker()`。

- **不要在 handler 里直接修改缓存对象**：见 Indexer 章节，要 deepcopy。

- **动态注销**：使用 `RemoveEventHandler(registration)`，并把 handler 自己启动的 goroutine、队列和指标一起释放；移除回调不等于自动回收业务资源。

### ResourceVersion 与一致性契约

**List + Watch 的目标是维护当前状态，不是持久事件日志。** List 返回一个快照及其 RV，Watch 从该位置报告后续变化。网络中断时尝试续传；RV 过旧时 relist。中间发生过多少次变化可能不可知；在权限、网络和服务端恢复正常后，当前状态会通过续传或新快照重新进入缓存。

Watch bookmark 只推进进度，不携带业务对象变化。收到 bookmark 后可更新最近 RV，但不能触发 Reconcile 或当作健康对象。

`HasSynced()` 只表示初始数据已进入本地 store，不表示缓存与 apiserver 在这一时刻完全一致。controller-runtime 的常见读写路径是缓存读、直连写，因此一次 Update/Patch 成功后立刻从缓存 Get，仍可能读到旧 `resourceVersion`。正确做法是：

1. 把写成功视为 apiserver 已接受，不依赖立即 read-your-writes。
2. 下一次 Reconcile 从缓存重新观察状态。
3. 确实需要线性化读取时使用显式 uncached client，并承担 apiserver 压力。
4. Reconcile 按当前 level 计算，不依赖“恰好收到某个边沿事件”。

Resync 只是本地重新入队，不能修复错误的 List/Watch selector，也不能保证读取最新服务端状态。把它作为周期性审计信号，而不是 Watch 可靠性的补丁。

### v0.36 生产检查

- 所有 Kubernetes module 使用同一 minor 版本；controller-runtime 遵循其 go.mod 的 Kubernetes 版本，不随意混搭。
- `rest.Config` 设置稳定 User-Agent，并按控制器规模配置 QPS/Burst；限流等待必须响应 context。
- handler 只做 key 提取和入队，不执行网络 I/O。
- 启动 worker 前等待相关 informer `HasSynced`；若依赖初始回调完成，再等待 registration 的同步状态。退出时取消 context 并关闭业务 workqueue。
- 只缓存真正需要 Watch 的 GVK/namespace；Secret 和大 ConfigMap 要评估内存与权限暴露。
- 监控 List 次数/耗时、Watch 重启、410、队列深度、最长未同步时间和 handler 延迟。
- 缓存对象只读；修改前 `DeepCopy`，写入使用 Patch/Update 的独立对象。

### 本章小结

- Informer 是 client-go 的核心抽象，把 WatchList（或 List）+ Watch 封装为"本地缓存 + 事件分发"流水线。
- Reflector 负责初始状态、持续 Watch、ResourceVersion 续传、错误恢复和本地 resync。
- v0.36 默认使用原子事件 RealFIFO，按进入客户端 Queue 的顺序保留通知；DeltaFIFO 是按 key 聚合 Deltas 的兼容实现。
- Indexer 通过 `cache` + `threadSafeMap` 保存当前对象，并提供按 key 与自定义索引查询。
- SharedInformer 让多业务方共享同一份缓存和 Reflector，配合 SharedInformerFactory 实现资源复用。
- 工程实践的核心是：**用 factory 复用、handler 只入队、检查同步结果、按需 resync、缓存对象只读且修改前 DeepCopy**。

掌握 Informer 后，下一章我们将进入 [第30章 Controller](./30-Controller.md)，看 controller-runtime 如何在 Informer 之上构建 Reconcile 控制循环。
