## 第14章 select（重点）

> `select` 在一组 channel 收发中选择一次可执行通信。语言规范保证“当前可执行 case 中均匀伪随机选择”，不保证优先级、轮转、公平等待上限或某个 goroutine 的执行顺序。本章公共语义基于 Go 1.26，Runtime 快照基于 Go 1.26.4。

### 语法与规范语义

`select` 的每个通信 case 必须是 channel 发送或接收，最多一个 `default`：

```go
select {
case value := <-input:
    consume(value)
case output <- result:
    markSent()
case value, ok := <-updates:
    handle(value, ok)
case <-ctx.Done():
    return ctx.Err()
default:
    recordIdle()
}
```

执行一次 select 时：

1. 所有 receive 的 channel operand，以及所有 send 的 channel operand 和右侧待发送表达式，按源码顺序求值且只求值一次。
2. 若一个或多个通信可以立即执行，从它们中均匀伪随机选择一个。
3. 若都不能执行且存在 `default`，执行 default。
4. 若都不能执行且没有 default，当前 goroutine 阻塞，直到某个通信可以执行。
5. 只执行被选 case 的通信和分支体。

receive 赋值左侧的表达式只在该 case 被选择后求值。这与 channel operand 的进入时求值不同：

```go
select {
case slots[index()] = <-source():
    // source() 进入 select 时求值。
    // index() 只有该 receive case 被选中后才求值。
case sink() <- payload():
    // sink() 和 payload() 都在进入 select 时求值。
}
```

因此，不应把有副作用或昂贵计算藏在 case operand 中；先在 select 外明确求值通常更易审查。

#### 空 select 与 nil channel

`select {}` 没有任何 case，会永久阻塞当前 goroutine。它不会等待某个可关闭资源，也没有退出协议；服务主函数通常应等待信号并执行 graceful shutdown，而不是用空 select 代替生命周期管理。

对 nil channel 的发送和接收永远不能继续，所以 nil case 会被禁用：

```go
var input <-chan Item // nil

select {
case item := <-input:
    use(item) // 当前不会进入
case <-ctx.Done():
    return ctx.Err()
}
```

如果所有通信 channel 都为 nil、又没有 default，select 将永久阻塞。

### default 与非阻塞操作

带 default 的 select 不等待：

```go
select {
case value := <-ch:
    use(value)
default:
    // 此刻不能接收
}
```

这是非阻塞收发的正确表达。下面的写法有 TOCTOU 竞争：

```go
// 错误：len 只是瞬时快照。
if len(ch) > 0 {
    value := <-ch // 仍可能阻塞
    use(value)
}
```

`value, ok := <-ch` 本身仍是阻塞接收；`ok` 只区分值来自发送还是 channel 已关闭，不能把它当作非阻塞语法。

#### 忙循环

```go
for {
    select {
    case item := <-jobs:
        process(item)
    default:
    }
}
```

没有工作时，上述循环仍持续占用 CPU。`runtime.Gosched` 或很短的 `time.Sleep` 只能改变症状，并不形成清晰背压。通常应删除 default 让 goroutine 阻塞，或用明确的 ticker、批处理窗口和速率限制。

default 也不是“低优先级 case”。只要任一通信可立即执行，default 就不会被选；通信 case 之间仍按规范伪随机选择。

### 随机选择不等于公平调度

当多个通信在选择时都可以执行，规范要求均匀伪随机选择。下面的示例始终保持两个 channel 都有值：

```go
func sample(n int) [2]int {
    first := make(chan struct{}, 1)
    second := make(chan struct{}, 1)
    first <- struct{}{}
    second <- struct{}{}

    var count [2]int
    for range n {
        select {
        case <-first:
            count[0]++
            first <- struct{}{}
        case <-second:
            count[1]++
            second <- struct{}{}
        }
    }
    return count
}
```

大样本通常接近各半，但测试不应断言精确比例。有限序列可以连续多次选择同一 case。

这个保证的边界是：

- 只比较**本次选择时**可执行的通信。
- 不记录上次结果，不做 round-robin。
- 不保证某个低频 case 在有限时间内一定被选择。
- 不保证被唤醒 goroutine 何时获得 P。
- 不保证不同 goroutine 在同一 channel 上的等待延迟上界。

Go 1.26.4 当前使用随机 `pollorder`，channel 等待队列也有当前实现顺序，但这些私有细节不能升级为业务公平性契约。需要租户公平、严格优先级或配额时，应由单一调度者维护显式队列。

#### 重复 case

同一个 channel 可以出现在多个 case 中。规范按 case 选择，所以重复相同通信会改变概率权重：

```go
select {
case value := <-ch:
    handleA(value)
case value := <-ch:
    handleB(value)
case value := <-other:
    handleOther(value)
}
```

如果 `ch` 和 `other` 都可接收，前两个 case 各自参与抽签。除非确实要表达这种权重，否则应避免重复 case。

### 关闭 channel 与 select

从已经关闭且已排空的 channel 接收永远可以立即执行，返回元素零值和 `ok=false`：

```go
for updates != nil {
    select {
    case value, ok := <-updates:
        if !ok {
            updates = nil // 禁用该 case，避免零值忙循环
            continue
        }
        apply(value)
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

若不检查 ok，关闭的 receive case 会一直 ready，循环可能不断消费零值并压制其他工作。

向已关闭 channel 发送会 panic。在 select 中，这个 send 不是“自动跳过”的安全分支；不要用 select 探测 channel 是否已关闭。发送与关闭必须通过所有权协议协调。

关闭 channel synchronizes-before 因关闭而返回零值的接收。select 的随机选择不改变 channel 的内存模型关系。

### 取消没有隐含优先级

常见代码：

```go
select {
case <-ctx.Done():
    return ctx.Err()
case job := <-jobs:
    process(job)
}
```

如果取消和 job 同时 ready，任一 case 都可能被选中。把取消 case 写在最前面不会提高优先级。

若取消后不应再开始新工作，可做分阶段检查：

```go
if err := ctx.Err(); err != nil {
    return err
}

select {
case <-ctx.Done():
    return ctx.Err()
case job, ok := <-jobs:
    if !ok {
        return nil
    }
    if err := ctx.Err(); err != nil {
        return err
    }
    return process(ctx, job)
}
```

这仍不能把取消与 job 接收变成一个跨 channel 的原子优先级操作。真正严格的停止协议应由 producer 停止投递、关闭队列或由单一 coordinator 决策；已经取出的任务还要定义丢弃、回滚或完成语义。

### 动态启用 case

把 channel 变量设为 nil 可以动态关闭某个分支。下面合并两个输入，直到都关闭：

```go
func merge(ctx context.Context, left, right <-chan Item, out chan<- Item) error {
    for left != nil || right != nil {
        var next Item
        var send chan<- Item

        select {
        case value, ok := <-left:
            if !ok {
                left = nil
                continue
            }
            next, send = value, out
        case value, ok := <-right:
            if !ok {
                right = nil
                continue
            }
            next, send = value, out
        case <-ctx.Done():
            return ctx.Err()
        }

        select {
        case send <- next:
        case <-ctx.Done():
            return ctx.Err()
        }
    }
    return nil
}
```

局部变量 `send` 在取到值之前为 nil，因此不会误发。更复杂的 fan-in 通常应让调用者负责关闭 out，或明确规定 merge 是唯一发送者后由它关闭。

### timeout 与 Timer

一次性等待可以使用 `time.After`：

```go
select {
case result := <-results:
    return result, nil
case <-time.After(timeout):
    return Result{}, ErrTimeout
case <-ctx.Done():
    return Result{}, ctx.Err()
}
```

Go 1.23+ 可回收不再可达的未到期 Timer，因此旧版“time.After 必然泄漏”已经过时。但高频循环中每次 `time.After` 仍会创建 Timer；需要复用时用 `time.NewTimer` 和 `Reset`，并由一个 goroutine 串行管理。

协议已有 Context deadline 时，通常直接监听 `ctx.Done()`，避免每层重新创建相同 timeout。参见[第15章 Context](./15-Context.md)和[第16章 Timer 与 Ticker](./16-Timer与Ticker.md)。

### Runtime 实现

编译器会把静态 0 case、1 case 和部分带 default 的简单 select 改写为更直接的代码。一般多 case select 进入 `runtime.selectgo`。

Go 1.26.4 的 case 描述很小：

```go
type scase struct {
    c    *hchan
    elem unsafe.Pointer
}
```

发送 case 在数组前部，接收 case 在后部，`nsends` / `nrecvs` 表示边界，因此 `scase` 不需要 kind 字段。编译器还准备 `pollorder` 与 `lockorder` 工作区；当前实现明确让这些数组位于调用 G 的栈上。

#### 第一步：构造两种顺序

1. nil channel case 从 poll/lock order 中省略。
2. 对 channel timer，先让 Runtime 检查惰性到期状态。
3. 用 Runtime 伪随机数构造 `pollorder`。
4. 按 `hchan` 地址排序得到 `lockorder`，相同 channel 只加锁一次。

地址排序使两个重叠 select 以一致顺序获取 channel 锁，避免交叉锁死。当前使用 heap sort，以 O(n log n) 时间和常量额外栈完成。

#### 第二步：锁住 channel 后探测

`selectgo` 先按 lockorder 锁住所有相关 channel，再按随机 pollorder 检查：

- 是否有等待的对端；
- buffer 是否可读/可写；
- channel 是否关闭。

找到可执行通信后，在锁保护下提交操作、解锁并返回。若没有可执行通信且有 default，则解锁并返回 default。这里仍有 Runtime 调用、随机排列、排序和 channel 加锁，不能描述成“完全在用户栈、几乎零开销”。

#### 第三步：登记并挂起

无 default 且暂无通信时，Runtime 在仍持有相关 channel 锁的情况下：

1. 为每个非 nil case 获取一个 `sudog`。
2. 按 lockorder 把 sudog 链到 G 的 waiting 列表和 channel 的 sendq/recvq。
3. 调用 `gopark`；park commit 在安全发布栈状态后释放 channel 锁。

因为“复核状态、入队、释放锁并休眠”处在同一锁协议中，不会出现通信刚好发生在检查与登记之间而丢失唤醒。

#### 第四步：唤醒后清理

某个 channel 完成中标 case 后使 G 可运行。G 恢复时：

1. 再按 lockorder 锁住所有 channel。
2. 识别中标 sudog。
3. 从其他 channel 的等待队列移除未中标 sudog。
4. 清理指向栈的元素地址并归还 sudog。
5. 解锁，返回 case index 与 receive ok。

阻塞路径的登记和清理都是 O(n)，加上 lockorder 排序与多把锁竞争，case 很多时成本会增长。不要凭固定阈值决定“几十个就一定慢”；用真实 case 数、就绪分布和争用 benchmark。

### 常见模式

#### 非阻塞投递

```go
select {
case queue <- event:
    return nil
default:
    return ErrQueueFull
}
```

丢弃、重试、降级或阻塞是业务策略，必须配合指标。静默 default 会掩盖容量不足。

#### 可取消发送

```go
select {
case out <- value:
    return nil
case <-ctx.Done():
    return ctx.Err()
}
```

这避免 consumer 提前退出后 producer 永久阻塞，但前提是调用链确实传播并最终取消 ctx。

#### 等待首个结果

```go
func first(ctx context.Context, a, b <-chan Result) (Result, error) {
    select {
    case result := <-a:
        return result, nil
    case result := <-b:
        return result, nil
    case <-ctx.Done():
        return Result{}, ctx.Err()
    }
}
```

拿到首个结果后还必须取消或排空未中标 producer，否则其发送可能永久阻塞。通常由调用者先派生可取消 Context，并在 first 返回时调用 cancel。

### 测试与排障

- 不测试“case A 一定先执行”或精确 50/50 分布。
- 对关闭路径测试 `ok=false` 后 case 被设为 nil 或函数退出。
- 对 producer 提前退出、consumer 提前退出、Context 取消和 queue 满分别测试。
- 用 `go test -race` 查 value 交接后的共享访问；channel 自身安全不代表 payload 安全。
- 用 block profile 定位 select/channel 阻塞，用 goroutine profile 看等待栈，用 trace 看 runnable 与 wakeup。
- Go 1.25+ 的 `testing/synctest` 适合含 timer 的并发测试，但它不替代协议断言和 race detector。

### 源码阅读路线

1. 语言规范 Select statements：求值、选择与阻塞规则。
2. `src/cmd/compile/internal/walk/select.go`：简单 select 改写与 `scase` 构造。
3. `src/runtime/select.go`：pollorder、lockorder、三次 pass 与清理。
4. `src/runtime/chan.go`：等待队列、`sudog.isSelect` 和唤醒竞争。

### 本章小结

- 进入 select 时，channel operand 和 send RHS 按源码顺序求值一次；receive 左值仅在中标后求值。
- 多个通信可执行时按 case 均匀伪随机选择，但没有优先级、轮转或有限等待保证。
- default 表示“不等待”，不是低优先级；循环中的空 default 容易形成忙等待。
- nil channel 禁用 case；关闭且排空的 receive case 永远 ready，必须检查 ok 以免零值忙循环。
- Context case 没有隐含优先级；严格停止需要生产者和 coordinator 的协议。
- 当前 `selectgo` 构造随机探测顺序和地址锁顺序，锁住相关 channel 后探测；阻塞时为每个 case 登记 sudog，唤醒后移除未中标等待。
- case 数量、就绪概率和锁争用共同决定成本，应通过 benchmark、block profile 和 trace 验证。
