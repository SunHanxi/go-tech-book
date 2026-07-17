## 第32章 Prometheus Go Client

> 版本基线：`github.com/prometheus/client_golang v1.23.2`。核心链路是 Collector → Registry → Gather → Exposition；指标语义、基数预算和抓取资源上限比 API 调用本身更重要。

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
	"log"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	registry := prometheus.NewRegistry()
	requestsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests.",
		},
		[]string{"method", "route", "status_class"},
	)
	registry.MustRegister(requestsTotal)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requestsTotal.WithLabelValues(r.Method, "/", "2xx").Inc()
		w.WriteHeader(http.StatusOK)
	})
	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}
```

这里使用独立 Registry，让注册范围和测试生命周期显式可控。生产服务还应按[第26章](./26-io与net-http.md)的方式处理信号、优雅退出和其余超时；示例中的超时不是通用推荐值。

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

> 坑：自定义 `Collect` 必须是**并发安全**且**快速、阻塞短**，因为抓取会同步等待，多个抓取也可能并发调用它。同一 Collector 注册到多个 Registry 时，`Describe` 也可能并发发生。耗时采集应后台采样写共享变量，`Collect` 只读有界快照。

### Registry

`Registry` 是 Collector 的注册中心，负责去重、Describe 校验、协调一次抓取中所有 Collector 的 `Collect`。

```go
type Registry struct {
	mtx                 sync.RWMutex
	collectorsByID      map[uint64]Collector
	descIDs             map[uint64]struct{}
	dimHashesByName     map[string]uint64
	uncheckedCollectors []Collector
}
```

- `Register(c)` / `MustRegister(c)`：注册并校验 `Describe` 声明的指标名、label 维度无冲突。
- `Gather()`：触发所有已注册 Collector 的 `Collect`，聚合为 `[]*dto.MetricFamily`（protobuf），供 `/metrics` 序列化。

默认全局 Registry：`prometheus.DefaultRegisterer` / `prometheus.DefaultGatherer`，`MustRegister` 等快捷函数都注册到这里。`promhttp.Handler()` 抓取的就是默认 Registry。

**为什么需要 Describe 校验**：防止同名指标使用不同 help 或 label 维度。相同描述集合对应的 Collector 已注册时，`Register` 返回 `AlreadyRegisteredError`；同名 descriptor 的维度不一致则返回普通注册错误。若 `Describe` 不发送任何 Desc，该 Collector 会进入 unchecked 集合，冲突只能在 Gather 时发现。

> 坑：`MustRegister` 在重复注册时会 panic。库包优先接收 `prometheus.Registerer`，测试和多实例组件使用独立 Registry；用全局 `sync.Once` 掩盖生命周期问题会让测试相互污染。

### Gather

`Gather()` 是 Registry → exposition 的桥梁。它在一次抓取中：

1. 在读锁下把 checked/unchecked Collector 复制到内部 channel，必要时复制 pedantic 校验数据，然后释放锁；
2. 按可用 goroutine budget 并发调用 Collector 的 `Collect(ch)`；
3. 按 metric family（同名同类型）聚合，检查一致性（同名指标类型必须一致、label 名集合必须一致）；
4. 返回 `[]*dto.MetricFamily`，由 `promhttp.Handler` 按内容协商写成 Prometheus text、OpenMetrics 或支持的 protobuf 格式。

```go
mfs, err := prometheus.DefaultGatherer.Gather()
// mfs 是 []*dto.MetricFamily，可被 prometheus.TextEncoder 序列化
```

**性能要点**：`Registry.Gather` 在调用用户 `Collect` 前已经释放 Registry 的读锁，因此慢 Collector 不会一直阻塞 Register/Unregister；但它会拖慢本次抓取。多个抓取可并发进入同一个 Collector，所以实现仍必须并发安全且有界。

`promhttp.HandlerFor` 可指定自定义 Registry 与编码：

```go
handler := promhttp.HandlerFor(myRegistry, promhttp.HandlerOpts{
	MaxRequestsInFlight: 4,
	Timeout:             10 * time.Second,
	EnableOpenMetrics:   true,
})
```

这些数值只是示例，应按 scrape interval、Collector 延迟和实例资源预算设置。`Timeout` 只结束 HTTP 响应，当前实现不会取消仍在后台运行的 Gather；慢 Collector 必须自己使用有界快照或独立的超时/取消机制。`EnableOpenMetrics` 在该版本仍标为实验选项，启用前核对抓取端兼容性。

### Exporter

Exporter = 把外部系统指标转成 Prometheus 指标的进程。狭义上指官方/社区的独立 exporter（node_exporter、mysqld_exporter）；广义上，任何在业务进程内嵌 `/metrics` 的服务也算“内嵌 exporter”。

`client_golang` 提供开箱即用的 Go Runtime 与进程 Collector。独立 Registry 需要显式选择：

```go
import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

registry := prometheus.NewRegistry()
registry.MustRegister(
	collectors.NewGoCollector(),
	collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
)
```

默认全局 Registry 在 package init 时注册 Go Collector 和平台支持的 Process Collector；`promhttp.Handler()` 只是使用 `DefaultGatherer`，并为 handler 自身增加抓取 instrumentation。新建的 `prometheus.NewRegistry()` 是空的，如需 `go_*` / `process_*` 必须显式注册 `collectors.NewGoCollector()` 和 `collectors.NewProcessCollector(...)`。

**自定义 Exporter 核心示例**：采集一个任务队列的当前值与累计值。

```go
package main

import (
	"net/http"
	"sync"

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

func (e *QueueExporter) RecordPush() { e.pushed.Inc() }

func NewMetricsHandler(q *Queue) (http.Handler, *QueueExporter, error) {
	registry := prometheus.NewRegistry()
	exporter := NewQueueExporter(q)
	if err := registry.Register(exporter); err != nil {
		return nil, nil, err
	}
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{}), exporter, nil
}
```

**最佳实践**：

| 实践 | 说明 |
|---|---|
| Counter 用 `_total` 后缀 | Prometheus 命名约定；`NewCounter` 不会替任意 Name 自动补后缀 |
| 高基数 label 慎用 | 如 `user_id`、`path` 全量，会撑爆 Registry 内存 |
| Histogram vs Summary | 服务端聚合用 Histogram；客户端算分位数用 Summary |
| 后台采样 | 慢采集放后台写 Gauge，`Collect` 只读 |
| 谨慎使用 `promauto` | 应用入口可简化注册；复用库优先接收 Registerer 并显式处理错误 |

### 指标语义与基数

指标名使用 base unit 和稳定后缀：

- Counter 以 `_total` 结尾。
- Duration 使用 `_seconds`，字节使用 `_bytes`。
- 不把单位写成毫秒后又返回秒。
- Help 描述“测量什么”，不复述名字。

Label 的序列数近似为每个维度取值数量的乘积。禁止把以下值直接做 label：request ID、user ID、原始 URL、错误消息、SQL、文件名和无界资源 UID。HTTP 使用路由模板与状态码类别，详细值放日志或 trace。

MetricVec 为某组 label values 创建子指标后会一直保留，直到显式调用 `DeleteLabelValues` / `DeletePartialMatch` / `Reset` 或进程退出。即使瞬时活跃对象不多，无界 churn 也会持续推高内存和抓取体积；动态对象指标必须设计删除生命周期。

Histogram 桶应围绕 SLO 设计：

```go
latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
    Name:    "http_server_request_duration_seconds",
    Help:    "Request handling duration.",
    Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
}, []string{"method", "route", "status_class"})
```

Summary 的客户端 quantile 不能跨实例正确聚合，服务端 SLO 通常使用 Histogram。Native Histogram 可降低手工桶配置成本，但启用前要核对 Prometheus 服务端、remote-write 后端、存储成本和当前 client 的稳定性声明。

### Registry 生命周期

可复用组件不要隐式注册全局变量：

```go
type Metrics struct {
    requests *prometheus.CounterVec
}

func NewMetrics(registerer prometheus.Registerer) (*Metrics, error) {
    metrics := &Metrics{requests: prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "worker_jobs_total", Help: "Processed jobs."},
        []string{"result"},
    )}
    if err := registerer.Register(metrics.requests); err != nil {
        return nil, err
    }
    return metrics, nil
}
```

应用入口决定使用默认 Registry 还是自定义 Registry。独立 Registry 能控制暴露面，避免第三方包通过 init 静默增加指标，也便于测试。

### 抓取与并发

- 同一服务可能被多个 Prometheus 副本并发抓取，`Collect` 必须允许并发调用。
- `Collect` 不做慢网络请求，不持有业务热锁，不创建无界 goroutine。
- `promhttp.HandlerOpts.Timeout` 只是 HTTP 保护；Collector 仍应自身有界。
- 对抓取错误、并发数和响应体大小做监控。
- `/metrics` 放在管理端口并配置网络访问控制，避免把内部拓扑和版本公开到互联网。

### 指标测试

使用 `prometheus/testutil` 验证值和 exposition：

```go
func TestMetrics(t *testing.T) {
    registry := prometheus.NewRegistry()
    metrics, err := NewMetrics(registry)
    if err != nil {
        t.Fatal(err)
    }

    metrics.requests.WithLabelValues("success").Inc()
    if got := testutil.ToFloat64(metrics.requests.WithLabelValues("success")); got != 1 {
        t.Fatalf("requests = %v, want 1", got)
    }

    if err := testutil.GatherAndCompare(registry, strings.NewReader(`# HELP worker_jobs_total Processed jobs.
# TYPE worker_jobs_total counter
worker_jobs_total{result="success"} 1
`), "worker_jobs_total"); err != nil {
        t.Fatal(err)
    }
}
```

测试还应覆盖重复注册、非法 label、并发 Collect 和高基数保护。完整可观测性与安全边界见[第28章](./28-可观测性与安全.md)。

### 本章小结

- 指标链路：`Collector.Describe/Collect` → `Registry.Register/Gather` → `promhttp.Handler` 序列化 → Prometheus 抓取。
- Counter/Gauge/Histogram/Summary 覆盖常见语义；只有现有指标类型无法表达采集来源或一致快照时才实现自定义 Collector。
- Registry 做 descriptor 去重与一致性校验；`MustRegister` 失败会 panic，复用组件应返回 `Register` 错误。
- Gather 在锁内复制 Collector 集合后释放锁，再并发 Collect；慢 Collector 仍会拖慢抓取，且 HTTP Timeout 不会取消后台 Gather。
- Exporter 是“外部数据 → Prometheus 指标”的适配器；应用可选择默认 Registry，复用组件应把 Registerer/Gatherer 作为显式依赖。
- 高基数 label 会同时放大进程、Prometheus 和远端存储成本；每个指标都要有 series 预算。
- 可复用组件接收 Registerer，测试使用独立 Registry 和 `prometheus/testutil`。
