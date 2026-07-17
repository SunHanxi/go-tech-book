## 第26章 io 与 net/http（重点）

> `io.Reader`/`Writer` 是 Go 组合式 API 的核心，`net/http` 则把 interface、context、并发、超时和资源所有权集中到一条真实请求链路中。

生产 HTTP 服务最常见的问题通常不是路由，而是没有限制输入、错误理解连接复用、缺少超时、忘记关闭响应体，或在停机时直接终止正在处理的请求。

### 26.1 Reader 与 Writer 契约

```go
type Reader interface {
    Read(p []byte) (n int, err error)
}

type Writer interface {
    Write(p []byte) (n int, err error)
}
```

正确使用 Reader 必须处理 `n > 0` 与 `err != nil` 同时出现。调用方应先处理前 n 字节，再处理错误：

```go
buffer := make([]byte, 32*1024)
for {
    n, err := source.Read(buffer)
    if n > 0 {
        if _, writeErr := destination.Write(buffer[:n]); writeErr != nil {
            return writeErr
        }
    }
    if errors.Is(err, io.EOF) {
        break
    }
    if err != nil {
        return err
    }
}
```

实际代码优先使用 `io.Copy`，它还能利用 `WriterTo` 或 `ReaderFrom` 快路径：

```go
_, err := io.Copy(destination, source)
```

Writer 返回 `n < len(p)` 且 `err == nil` 违反契约，调用方会得到 `io.ErrShortWrite`。自定义 Reader/Writer 时必须写清阻塞、并发和所有权语义。

### 26.2 组合式 I/O

标准库用小接口组合出常见数据流：

| 工具 | 用途 |
|---|---|
| `io.LimitReader` | 限制最多读取的字节数 |
| `io.TeeReader` | 读取时复制到 Writer，如计算审计摘要 |
| `io.MultiReader` | 顺序拼接多个 Reader |
| `io.MultiWriter` | 把同一数据写给多个 Writer |
| `io.SectionReader` | 只暴露文件的一段 |
| `io.Pipe` | 用同步管道连接生产者和消费者 |
| `bufio.Reader/Writer` | 减少小读写系统调用 |

`io.Pipe` 没有内部缓冲，写入会等待读取，天然形成背压：

```go
reader, writer := io.Pipe()

go func() {
    err := encode(writer)
    _ = writer.CloseWithError(err)
}()

if _, err := io.Copy(destination, reader); err != nil {
    return err
}
```

生产者必须关闭 writer；错误用 `CloseWithError` 传给读端。若消费方提前退出，也要关闭 reader，避免生产 goroutine 永久阻塞。

### 26.3 资源所有权

获得 `io.Closer` 的一方通常负责关闭，除非 API 明确转移所有权：

```go
file, err := os.Open(path)
if err != nil {
    return err
}
defer file.Close()
```

库函数若只接收 `io.Reader`，不应擅自断言并关闭它；调用方可能还要复用该资源。构造函数返回 `io.ReadCloser` 时，应在文档写清谁关闭、何时关闭以及 Close 的错误是否重要。

不要在长循环里直接 `defer` 大量 Close。把单次处理提取成函数，让 defer 在每次迭代结束时执行。

### 26.4 HTTP Server 的基本结构

不要直接使用没有超时的包级 `http.ListenAndServe`。显式构造 Server：

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /healthz", healthHandler)
mux.HandleFunc("POST /v1/items/{id}", itemHandler)

server := &http.Server{
    Addr:              ":8080",
    Handler:           requestIDMiddleware(mux),
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       15 * time.Second,
    WriteTimeout:      30 * time.Second,
    IdleTimeout:       60 * time.Second,
    MaxHeaderBytes:    1 << 20,
}
```

Go 1.22 的 ServeMux 支持方法、通配符和 `Request.PathValue`。复杂路由仍可使用第三方库，但不应为了基本参数路由默认引入框架。

四个超时含义不同：

- `ReadHeaderTimeout`：读取请求头的上限，直接防御 slowloris。
- `ReadTimeout`：读取整个请求（含 body）的连接级上限；流式上传需谨慎设置。
- `WriteTimeout`：响应写出的上限；流式响应需要单独设计。
- `IdleTimeout`：keep-alive 连接等待下一请求的时间。

示例中的数值不是通用默认值。应根据请求体大小、正常延迟分位数、流式协议和上游重试预算设定，并用超时指标持续校准。业务处理还应使用 context deadline；连接超时不能替代下游调用超时。

### 26.5 限制请求体与严格解码

永远不要对不可信 body 直接 `io.ReadAll`。在 handler 层限制大小：

```go
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
    r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

    decoder := json.NewDecoder(r.Body)
    decoder.DisallowUnknownFields()
    if err := decoder.Decode(dst); err != nil {
        return fmt.Errorf("decode request: %w", err)
    }

    var extra any
    if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
        return errors.New("request body must contain one JSON value")
    }
    return nil
}
```

大小限制、Content-Type 校验、字段校验和认证应在进入核心业务逻辑前完成。错误响应不要泄露内部堆栈、SQL 或文件路径。

### 26.6 Handler 与 Context

客户端连接关闭、HTTP/2 请求取消或 `ServeHTTP` 返回时，`r.Context()` 会被取消。`Server.Shutdown` 本身不会取消仍在处理的请求；如果停机时需要通知 handler，应把应用生命周期 context 通过 `Server.BaseContext` 或自己的取消机制传入。下游调用必须透传请求 context：

```go
func itemHandler(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
    defer cancel()

    item, err := repository.Load(ctx, r.PathValue("id"))
    if err != nil {
        writeError(w, err)
        return
    }
    writeJSON(w, http.StatusOK, item)
}
```

不要把 Request 或 ResponseWriter 保存到 handler 返回之后使用。异步任务应提取所需的不可变数据，并使用独立、受控生命周期的 context；不能直接用已经取消的请求 context 假装后台任务可靠。

### 26.7 Middleware

Middleware 的标准形状是 Handler 到 Handler：

```go
func requestIDMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        requestID := r.Header.Get("X-Request-ID")
        if requestID == "" {
            requestID = newRequestID()
        }
        ctx := context.WithValue(r.Context(), requestIDKey{}, requestID)
        w.Header().Set("X-Request-ID", requestID)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}
```

常见顺序是：panic recovery → request ID/trace → access log → metrics → authentication → authorization → body limit → handler。Recovery 只能作为进程保护，不能把 panic 当正常错误处理。

自定义 ResponseWriter 包装器要谨慎：可选接口如 `http.Flusher`、`http.Hijacker`、`io.ReaderFrom` 会影响流式响应和 WebSocket。Go 新代码可考虑 `http.ResponseController` 操作这些能力。

### 26.8 HTTP Client 与 Transport

Client 和 Transport 应长期复用。每个请求创建 Client 会丢失连接池并增加握手开销：

```go
transport := http.DefaultTransport.(*http.Transport).Clone()
transport.MaxIdleConns = 200
transport.MaxIdleConnsPerHost = 50
transport.MaxConnsPerHost = 100
transport.IdleConnTimeout = 90 * time.Second
transport.ResponseHeaderTimeout = 3 * time.Second

client := &http.Client{
    Transport: transport,
    Timeout:   10 * time.Second,
}
```

`Client.Timeout` 覆盖连接、重定向和读取 body 的整个过程。复杂系统通常还会分别配置 Dial、TLS handshake、response header 和业务 context deadline，便于定位是哪一段超时。

这些连接池上限同样只是容量示例，应按目标主机数、实例并发和下游容量计算。拥有自定义 Transport 的组件在最终退出时可调用 `transport.CloseIdleConnections()`；不要在每个请求后调用，否则会破坏复用。

每个响应都必须关闭 body：

```go
request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
if err != nil {
    return err
}

response, err := client.Do(request)
if err != nil {
    return err
}
defer response.Body.Close()
```

只有在响应体被读到 EOF 或按协议处理完时，HTTP/1.x 连接才通常能顺利复用。对有大小上限的小响应可读取后关闭；不要为了复用无界地 drain 恶意大响应。

### 26.9 重试与幂等性

网络错误不等于请求没有到达服务端。自动重试必须回答：

- 方法是否幂等，或是否带幂等键。
- body 能否重新生成（`Request.GetBody`）。
- 哪些状态码和错误可重试。
- 是否使用指数退避、随机抖动和总预算。
- context 取消后是否立即停止。

GET 通常可重试；POST 只有服务端支持幂等键时才安全。不要在 SDK、service mesh 和业务层同时无上限重试，否则会形成重试放大。

### 26.10 优雅停机

收到终止信号后先停止接收新请求，再给在途请求一个有界窗口：

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

errCh := make(chan error, 1)
go func() {
    errCh <- server.ListenAndServe()
}()

select {
case <-ctx.Done():
case err := <-errCh:
    if err != nil && !errors.Is(err, http.ErrServerClosed) {
        return err
    }
}

shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
defer cancel()
if err := server.Shutdown(shutdownCtx); err != nil {
    _ = server.Close() // 超出预算后强制关闭 net/http 管理的连接
    return fmt.Errorf("shutdown HTTP server: %w", err)
}
return nil
```

`Shutdown` 只等待 `net/http` 管理的连接变为空闲，不会关闭经 `Hijacker` 接管的连接，例如 WebSocket。可用 `RegisterOnShutdown` **发起**这类协议自己的关闭流程，但回调应立即返回，且 `Shutdown` 不等待这些回调完成；应用还要用独立的 `WaitGroup` 或等价机制等待它们，并把等待纳入同一个总预算。

Kubernetes 中还应先让 readiness 失败，等待 Endpoint 传播，再调用 Shutdown。示例中的 20 秒必须小于 Pod 的总终止宽限期，并为流量摘除和最终清理留出余量。后台 worker、消息消费者和数据库连接也要纳入同一个有序停机流程。

### 26.11 测试与排障

- Handler 使用 `httptest.NewRequest` 和 `httptest.NewRecorder`。
- 完整协议行为使用 `httptest.NewServer`。
- 自定义 RoundTripper 可隔离 Client 测试，不需要真实网络。
- 用 `httptrace.ClientTrace` 定位 DNS、连接、TLS、连接池等待和首字节延迟。
- 监控 Transport 连接池、请求阶段耗时、超时类型和响应体读取错误。
- 不要在公网暴露默认 pprof mux，详见[第28章 可观测性与安全](./28-可观测性与安全.md)。

### 本章小结

- Reader/Writer 的价值来自小接口和组合；正确处理 `n > 0, err != nil` 与 Close 所有权是基础。
- HTTP Server 必须显式配置输入限制、连接超时、业务 deadline 和优雅停机。
- Client/Transport 要复用，响应体要关闭，重试必须受幂等性和总预算约束。
- context 贯穿请求链路，但后台任务需要独立且有界的生命周期。

进一步阅读：

- [Package io](https://pkg.go.dev/io)
- [Package net/http](https://pkg.go.dev/net/http)
- [Go blog: Go Concurrency Patterns: Context](https://go.dev/blog/context)
