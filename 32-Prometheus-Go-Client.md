## 第32章 Prometheus Go Client

> Prometheus 的 Pull 模型要求被监控进程通过 `/metrics` 暴露指标。`client_golang`（`github.com/prometheus/client_golang/prometheus`）是 Go 侧的标准实现，核心抽象是 Collector → Registry → Gather → Exposition。理解这条链路，就能写出高效、无锁竞争的自定义指标。

### Collector

`Collector` 是指标的来源接口。Registry 在每次抓取时调用 `Collect(ch chan<- Metric)`，Collector 把自己产出的 metric 通过 channel 送出去。

```go
type Collector interface {
	Describe(chan<- *Desc)   // 声明指标元信息（Desc）
	Collect(chan<- Metric)   // 实际采集
}
```

`client_golang` 内置了已实现 `Collector` 的指标类型，覆盖 4 种 Prometheus 指标语义：

| 类型 | Go 类型 | 用途 |
|---|---|---|
| Counter | `prometheus.Counter` | 单调递增（请求数、字节数） |
| Gauge | `prometheus.Gauge` | 可增可减（goroutine 数、队列长度） |
| Histogram | `prometheus.Histogram` | 分布统计（延迟分桶） |
| Summary | `prometheus.Summary` | 分位数（p99 延迟，客户端计算） |

注册一个 Counter：

```go
package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests.",
		},
		[]string{"method", "path", "status"},
	)
)

func init() {
	prometheus.MustRegister(requestsTotal)
}

func main() {
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requestsTotal.WithLabelValues(r.Method, r.URL.Path, "200").Inc()
		w.WriteHeader(200)
	})
	http.ListenAndServe(":8080", nil)
}
```

**自定义 Collector**：当指标值来自外部系统（如“数据库连接池当前使用数”），无法用 Counter/Gauge 直接 Inc/Set 时，需实现自己的 `Collector`，在 `Collect` 时按需采样。典型场景：采集 cgroup、运行时、第三方 API 数据。

```go
type queueSizeCollector struct {
	desc  *prometheus.Desc
	queue *Queue
}

func NewQueueSizeCollector(q *Queue) *queueSizeCollector {
	return &queueSizeCollector{
		desc:  prometheus.NewDesc("queue_size", "Current queue size", nil, nil),
		queue: q,
	}
}

func (c *queueSizeCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *queueSizeCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(c.queue.Len()))
}
```

> 坑：自定义 `Collect` 必须是**并发安全**且**快速、阻塞短**——Prometheus 抓取是同步等待的。耗时采集应后台采样写共享变量，`Collect` 只读返回。

### Registry

`Registry` 是 Collector 的注册中心，负责去重、Describe 校验、协调一次抓取中所有 Collector 的 `Collect`。

```go
type Registry struct {
	mu            sync.RWMutex
	collectorsByID map[uint64]Collector // 按 Desc 指纹去重
	dimHashes     map[uint64]uint64
}
```

- `Register(c)` / `MustRegister(c)`：注册并校验 `Describe` 声明的指标名、label 维度无冲突。
- `Gather()`：触发所有已注册 Collector 的 `Collect`，聚合为 `[]*dto.MetricFamily`（protobuf），供 `/metrics` 序列化。

默认全局 Registry：`prometheus.DefaultRegisterer` / `prometheus.DefaultGatherer`，`MustRegister` 等快捷函数都注册到这里。`promhttp.Handler()` 抓取的就是默认 Registry。

**为什么需要 Describe 校验**：防止两个 Collector 声明同名指标但 label 集合不同，导致抓取结果语义混乱。注册时若发现 `Desc` 指纹冲突，`Register` 返回 `AlreadyRegisteredError`。

> 坑：`MustRegister` 在重复注册时会 **panic**。单例指标用 `sync.Once` 包裹注册，避免多次 init。

### Gather

`Gather()` 是 Registry → exposition 的桥梁。它在一次抓取中：

1. 加读锁，遍历所有 Collector；
2. 为每个 Collector 启动一个 `Collect(ch)`，把 metric 写入内部 channel；
3. 按 metric family（同名同类型）聚合，检查一致性（同名指标类型必须一致、label 名集合必须一致）；
4. 返回 `[]*dto.MetricFamily`，由 `promhttp.Handler` 用 text/exprotobuf 格式写出。

```go
mfs, err := prometheus.DefaultGatherer.Gather()
// mfs 是 []*dto.MetricFamily，可被 prometheus.TextEncoder 序列化
```

**性能要点**：Gather 持有读锁期间所有 Collect 并发执行。若某 Collector `Collect` 很慢，会拖慢整个 `/metrics` 响应，进而导致 Prometheus 抓取超时、指标断点。

`promhttp.HandlerFor` 可指定自定义 Registry 与编码：

```go
handler := promhttp.HandlerFor(myRegistry, promhttp.HandlerOpts{
	Timeout:           10 * time.Second,
	EnableOpenMetrics: true,
})
```

### Exporter

Exporter = 把外部系统指标转成 Prometheus 指标的进程。狭义上指官方/社区的独立 exporter（node_exporter、mysqld_exporter）；广义上，任何在业务进程内嵌 `/metrics` 的服务也算“内嵌 exporter”。

`client_golang` 提供开箱即用的 **Go 运行时 Exporter**：

```go
import "github.com/prometheus/client_golang/prometheus/promauto"

// promauto 包注册到默认 Registry
var (
	goRoutines = promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "myapp_goroutines",
	}, func() float64 { return float64(runtime.NumGoroutine()) })
)
```

`promhttp.Handler()` 已自动包含 `collectors.NewGoCollector()`（go_*、process_* 系列）。

**自定义 Exporter 完整示例**：采集一个任务队列的多维指标。

```go
package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Queue struct {
	mu    sync.Mutex
	items []int
}

func (q *Queue) Len() int { q.mu.Lock(); defer q.mu.Unlock(); return len(q.items) }

type QueueExporter struct {
	queue  *Queue
	size   *prometheus.Desc
	pushed prometheus.Counter
}

func NewQueueExporter(q *Queue) *QueueExporter {
	return &QueueExporter{
		queue: q,
		size:  prometheus.NewDesc("queue_size", "Current items in queue", nil, nil),
		pushed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "queue_pushed_total", Help: "Total pushed items",
		}),
	}
}

func (e *QueueExporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.size
	e.pushed.Describe(ch)
}

func (e *QueueExporter) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(e.size, prometheus.GaugeValue, float64(e.queue.Len()))
	e.pushed.Collect(ch)
}

func main() {
	q := &Queue{}
	exp := NewQueueExporter(q)
	prometheus.MustRegister(exp)

	go func() {
		for {
			q.mu.Lock(); q.items = append(q.items, 1); q.mu.Unlock()
			exp.pushed.Inc()
			time.Sleep(time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(":8080", nil)
}
```

**最佳实践**：

| 实践 | 说明 |
|---|---|
| Counter 用 `_total` 后缀 | Prometheus 规范，自动加 |
| 高基数 label 慎用 | 如 `user_id`、`path` 全量，会撑爆 Registry 内存 |
| Histogram vs Summary | 服务端聚合用 Histogram；客户端算分位数用 Summary |
| 后台采样 | 慢采集放后台写 Gauge，`Collect` 只读 |
| 用 `promauto` 简化注册 | 注册与定义合一，避免忘记 Register |

### 本章小结

- 指标链路：`Collector.Describe/Collect` → `Registry.Register/Gather` → `promhttp.Handler` 序列化 → Prometheus 抓取。
- 4 种内置类型 Counter/Gauge/Histogram/Summary 已覆盖绝大多数场景，自定义 Collector 只用于按需采样的外部数据源。
- Registry 做去重与 Describe 校验；`MustRegister` 重复注册会 panic，用 `sync.Once`。
- Gather 持读锁并发跑所有 Collect，慢 Collector 会拖垮抓取——保持 Collect 快、并发安全。
- Exporter 是“外部数据 → Prometheus 指标”的适配器；内嵌 exporter 用 `promauto` + 默认 Registry 最省事。
- 高基数 label 是内存杀手，谨慎设计 label 维度。
