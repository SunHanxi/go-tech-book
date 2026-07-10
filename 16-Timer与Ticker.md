## 第16章 Timer 与 Ticker（重点）

> 当前语义基线为 Go 1.26。Go 1.23 重写了 channel timer 的可回收性与 Stop/Reset 保证，旧版“必须排空 channel”“time.After 一定泄漏”的经验不能直接套用。

### 16.1 时间不是精确调度

`time.Timer` 和 `time.Ticker` 保证事件**不会早于**目标时刻发生，不保证恰好在该时刻执行。实际延迟还包括：

- OS timer 与调度精度。
- goroutine 等待 P 的时间。
- GC、系统调用和 CPU 配额导致的停顿。
- 接收者自身的处理时间。

需要协议 deadline 时使用 `context.WithTimeout/WithDeadline`；需要周期调度时使用 Ticker；需要严格任务持久化、错过补偿或跨进程选主时，应使用作业系统而不是进程内 timer。

### 16.2 Timer

Timer 表示一次触发：

```go
timer := time.NewTimer(2 * time.Second)

select {
case firedAt := <-timer.C:
    fmt.Println(firedAt)
case <-ctx.Done():
    timer.Stop()
    return ctx.Err()
}
```

Timer 触发一次后不会自动再次触发。零值 Timer 不可直接使用，必须由 `NewTimer` 或 `AfterFunc` 创建。

Go 1.23+ 中，channel timer 的对外 channel 是同步 channel，`cap(timer.C) == 0`。Runtime 可在接收路径上协同完成到期发送；不要根据 `len(timer.C)` 判断 timer 是否触发。

### 16.3 time.After

`time.After(d)` 等价于 `time.NewTimer(d).C`，适合一次性 select：

```go
select {
case result := <-results:
    return result, nil
case <-time.After(time.Second):
    return Result{}, errors.New("timeout")
}
```

Go 1.23 前，未到期且没有 Stop 的 Timer 不能被 GC 回收，长 duration 的 `time.After` 在循环中可能形成大量暂存对象。Go 1.23+ 可以回收不再可达、尚未到期的 Timer，因此这不再是生命周期泄漏。

但循环中每次 `time.After` 仍会分配新 Timer 并操作 Runtime timer heap。高频循环应复用：

```go
timer := time.NewTimer(idleTimeout)
defer timer.Stop()

for {
    select {
    case value := <-input:
        handle(value)
        timer.Reset(idleTimeout) // Go 1.23+ 可直接重置
    case <-timer.C:
        return ErrIdleTimeout
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

选择 After 还是 NewTimer 的依据是是否需要 Stop/Reset 和是否处于分配热点，不是 duration 够不够长。

### 16.4 Stop

`Timer.Stop` 阻止尚未触发的 Timer：

```go
stopped := timer.Stop()
```

- true：调用成功阻止一个仍活动的 Timer。
- false：Timer 已到期、已停止，或回调已经开始。

Go 1.23+ 对 `NewTimer` 创建的 channel timer 保证：Stop 返回后，后续接收不会拿到 Stop 之前的陈旧值。如果程序尚未接收且 Timer 仍在运行，Stop 保证返回 true。因此现代代码不需要在 Stop 返回 false 时排空 `timer.C`。

Go 1.22 及更早版本需要兼容以下旧模式：

```go
if !timer.Stop() {
    select {
    case <-timer.C:
    default:
    }
}
```

不要在只支持 Go 1.23+ 的新代码中机械保留这段逻辑，它增加状态分支，并可能掩盖多个 goroutine 同时操作 Timer 的设计问题。

Stop 不关闭 channel。关闭会让接收错误地立即成功，也会与 Runtime 发送竞争。停止之后等待 `<-timer.C` 可能永久阻塞。

### 16.5 Reset

```go
wasActive := timer.Reset(duration)
```

返回值表示重置前是否仍活动，通常不应用它推断业务结果。

Go 1.23+ 对 channel timer 保证：Reset 返回后，接收者不会读到旧配置对应的陈旧时间值。可以重置活动、停止或已到期的 Timer，无需先 Stop 和排空。

Timer 的操作仍应由一个 goroutine 拥有，或由额外同步串行化。Reset 与多个接收者并发会让“谁消费哪次触发”的业务协议无法推理，即使 Runtime 内部没有数据竞争。

### 16.6 AfterFunc

`time.AfterFunc(d, f)` 到期后在独立 goroutine 中调用 f，返回的 Timer 没有可用的 C：

```go
done := make(chan struct{})
timer := time.AfterFunc(time.Second, func() {
    defer close(done)
    runCleanup()
})
```

Stop 返回 false 时，f 已经开始或 Timer 已停止；Stop 不等待 f 完成。需要确认完成必须由 f 通过 channel/WaitGroup 明确通知。

AfterFunc 的 Reset 有一个重要差异：

- 返回 true：重新安排尚未开始的 f。
- 返回 false：安排 f 再执行一次，但不等待上一次完成，两次 f 可能并发。

因此 f 要么可并发重入，要么用额外同步阻止重叠。panic 也不会由创建 Timer 的 goroutine 自动处理。

### 16.7 Ticker

Ticker 周期性发送时间值：

```go
ticker := time.NewTicker(5 * time.Second)
defer ticker.Stop()

for {
    select {
    case tick := <-ticker.C:
        sample(tick)
    case <-ctx.Done():
        return
    }
}
```

Ticker 会调整间隔或丢弃 tick 以适应慢接收者，不会排队补发所有错过的时刻。不要用“收到 tick 的次数”表示可靠任务次数。

`Ticker.Reset(d)` 修改周期，`d <= 0` 会 panic。`Ticker.Stop` 不关闭 C，接收循环必须同时监听 context 或其他结束信号。

### 16.8 time.Tick

`time.Tick(d)` 只返回 channel，无法 Stop 或 Reset：

```go
for tick := range time.Tick(time.Minute) {
    report(tick)
}
```

Go 1.23+ 的 GC 可以回收不再可达的 Ticker，因此 `time.Tick` 不再因为缺少 Stop 必然泄漏。需要显式结束、修改周期或清晰表达资源所有权时仍应使用 `NewTicker`。

### 16.9 Runtime Timer

当前 Runtime 的每个 P 都管理一个 timer 最小堆。核心结构可概括为：

```go
// runtime/time.go，简化示意
type timer struct {
    mu      mutex
    state   uint8
    isChan  bool
    blocked uint32
    when    int64
    period  int64
    f       func(arg any, seq uintptr, delay int64)
    arg     any
    seq     uintptr
}

type timers struct {
    mu   mutex
    heap []timerWhen
    // 下一次唤醒、已修改/已删除数量等维护字段
}
```

- `when` 是基于 Runtime 单调时钟的目标纳秒。
- `period == 0` 表示一次性 Timer，大于零表示 Ticker。
- `f` 对 channel timer 是 `sendTime`，对 AfterFunc 最终启动用户函数 goroutine。
- `seq` 用于阻止 Stop/Reset 之前的陈旧 channel send 生效。
- 每 P heap 减少全局锁竞争，调度器和 netpoll 根据最早 timer 计算下一唤醒时间。

Heap 仍采用四叉最小堆，插入、删除和调整通常是 O(log n)。调度器会调用当前 P 的 `timers.check` 运行到期项；在寻找工作时也可以从其他 P 接管 timer。旧资料中的单一 `timerproc`、旧状态常量和固定 `checkTimers` 伪代码不再对应当前实现。

### 16.10 Go 1.23 channel timer 改造

Go 1.23 的两个关键变化：

1. **可回收**：未到期、未 Stop 但已经不可达的 Timer/Ticker 可以被 GC 回收。
2. **无陈旧值**：Stop/Reset 返回后，channel 不会交付旧配置的值。

兼容开关 `GODEBUG=asynctimerchan=1` 在 Go 1.26 仍可恢复 Go 1.23 前的 buffered channel 与不可回收行为，仅应用于兼容排障，不应成为长期配置。

源码里 `NewTimer` 仍可能创建容量为 1 的底层 channel，但 Runtime 对外呈现同步语义并在 channel 路径协作；不要只看 `time/sleep.go` 的 `make` 就推断 `cap(timer.C)` 或陈旧值行为。

### 16.11 常见模式

**Debounce**

由单个 goroutine 拥有 Timer，每次事件直接 Reset：

```go
func debounce(ctx context.Context, events <-chan Event, delay time.Duration) {
    timer := time.NewTimer(time.Hour)
    if !timer.Stop() {
        <-timer.C
    }
    defer timer.Stop()

    var latest Event
    pending := false
    for {
        var timerC <-chan time.Time
        if pending {
            timerC = timer.C
        }

        select {
        case latest = <-events:
            pending = true
            timer.Reset(delay)
        case <-timerC:
            flush(latest)
            pending = false
        case <-ctx.Done():
            return
        }
    }
}
```

把未启用分支设为 nil channel，比额外布尔 select 分支更清晰。

**超时预算**

一条请求链应从入口 deadline 推导每个下游预算，不在每层无条件重新创建相同 timeout。子调用结束后总是调用 cancel，及时释放关联资源。

### 16.12 测试与排障

- Go 1.25+ 用 `testing/synctest` 测试小时级 timeout，无需真实等待。
- 不断创建 Timer 的热路径用 `-benchmem` 查看 allocs/op。
- 用 `go tool trace` 观察 goroutine 是等待 timer、网络还是调度。
- Timer 只保证“不早于”，测试应允许合理调度延迟，不断言极窄墙钟窗口。
- 海量独立长期 Timer 可能增加 heap 和唤醒维护成本；先 benchmark，再考虑时间轮或集中调度器。

### 本章小结

- Go 1.23+ 可回收不可达 Timer/Ticker，`time.After` 不再具有旧版生命周期泄漏问题，但高频使用仍有分配成本。
- channel Timer 的 Stop/Reset 不会留下陈旧值，现代代码不需要排空；旧版兼容逻辑要明确隔离。
- AfterFunc 可能在 Reset 后并发运行两次回调，Stop 也不等待回调结束。
- Ticker 允许丢 tick，Stop 不关闭 channel，可靠任务不能只依赖 tick 计数。
- 当前 Runtime 使用每 P 四叉最小堆，并与调度器、channel 和 netpoll 协作。

进一步阅读：

- [Package time](https://pkg.go.dev/time)
- [Go 1.23 Timer Channel Changes](https://go.dev/wiki/Go123Timer)
- [Runtime timer source, Go 1.26](https://cs.opensource.google/go/go/+/refs/tags/go1.26.0:src/runtime/time.go)

