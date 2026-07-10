## 第22章 Escape Analysis

> 引言：逃逸分析（Escape Analysis）是 Go 编译器在编译期决定"每个变量分配在栈上还是堆上"的静态数据流分析。它是 GC 的最大盟友——不逃逸的变量随栈帧销毁，零 GC 开销。理解逃逸的判定规则与编译器实现，是写出低分配、低延迟 Go 代码的内功。本章将讲清逃逸的本质、典型场景、规避技巧与编译器内部算法。

### 什么是逃逸

#### 1. 是什么

逃逸分析（Escape Analysis）是编译器在编译期进行的静态分析，决定**每个变量分配在栈上还是堆上**。如果变量"逃出"了当前函数的作用域（被外部引用、生命周期超出函数），就必须分配在堆上由 GC 管理；否则可以安全地分配在栈上，函数返回时随栈帧销毁，零 GC 开销。

#### 2. 为什么重要：栈分配 vs 堆分配

| 维度 | 栈分配 | 堆分配 |
|------|--------|--------|
| 分配开销 | 几乎 0（移动 SP 寄存器） | 需 `mallocgc` + 位图维护 |
| 回收开销 | 函数返回即销毁 | GC 标记-清扫，有 STW/写屏障开销 |
| GC 压力 | 无 | 直接增加（参与三色标记，见[第21章 GC](./21-GC.md)） |
| 缓存友好性 | 高（栈局部性强） | 低（堆对象分散） |
| 触发条件 | 不被外部引用 | 被外部引用（逃逸） |

```go
package main

import "fmt"

func stackAlloc() int {
    x := 42      // x 不逃逸，栈分配
    return x     // 返回值是副本，x 随栈帧销毁
}

func heapAlloc() *int {
    x := 42      // x 逃逸：返回了它的地址
    return &x    // 调用者可能在任意时刻解引用，x 必须堆分配
}

func main() {
    fmt.Println(stackAlloc(), *heapAlloc())
}
```

`heapAlloc` 中的 `x` 逃逸到堆，因为它的地址被返回；调用者可能在任意时刻解引用，`x` 必须活到调用者用完。`stackAlloc` 返回的是值副本，`x` 可以随栈帧销毁。

> 逃逸分析是"编译期优化"，不是运行时机制。编译器在生成机器码前就决定好每个变量的归宿，运行时不再判断。

#### 3. 工程实践与常见坑

- **逃逸不一定是坏事**：合理返回指针（大 struct 避免拷贝）是正确的工程选择。逃逸分析的目的是"避免**不必要的**逃逸"，不是"消灭所有逃逸"。
- **如何确认**：`go build -gcflags="-m" ./...` 看每个逃逸点；`-m -m` 给出更详细的决策理由（flow 路径）。这是日常最常用的逃逸分析工具。
- **栈分配有上限**：单 goroutine 栈初始仅 2KiB（1.4+），可动态增长到 1GB。但逃逸分析判定"栈分配"的对象，编译器会确保栈空间足够；过大或大小不定的对象会直接判逃逸。

### 为什么逃逸

#### 1. 几大类逃逸场景

**1) 返回局部变量地址**

```go
func newInt() *int {
    x := 1
    return &x   // 逃逸：x 的生命周期超出函数
}
```

变量地址被返回，调用方持有引用，`x` 必须活到调用方用完——只能堆分配。

**2) 被接口捕获（interface{} / any）**

```go
func print(v any) { fmt.Println(v) }
print(42)   // 42 装箱成 *int 堆对象（逃逸）
```

接口的底层数据是指针。传值给 `any` 会触发装箱：编译器生成 `runtime.convT64(42)`，把值复制到堆上，再以指针存入 interface header（type + data 两个字）。这是接口带来灵活性的代价，也是 `fmt.Println` 慢的根源。

**3) 闭包捕获**

```go
func counter() func() int {
    n := 0
    return func() int {
        n++       // 闭包捕获 n，n 必须堆分配
        return n
    }
}
```

闭包返回后仍可能被调用，被捕获的 `n` 必须存活到闭包释放。闭包本质是一个"逃逸到堆的结构体"，捕获的变量成为它的字段。

**4) 大小在运行时确定**

```go
func makeBuf(n int) []byte {
    return make([]byte, n)  // 逃逸：n 是变量，编译期不知大小
}
func makeBufFixed() []byte {
    return make([]byte, 64) // 小常量，1.17+ 可能栈分配
}
```

编译期不知道 slice 长度时，无法保证栈空间足够，必须堆分配。注：1.17+ 对小常量 `make` 在某些场景会栈分配，但通常仍逃逸——以 `-gcflags="-m"` 实测为准。

**5) 过大**：编译器有阈值（一般 >64KB 或函数栈帧超限），超大局部变量直接堆分配避免栈溢出。

**6) goroutine 引用**

```go
func work() {
    x := bigStruct{}
    go func() { use(&x) }()  // x 逃逸：goroutine 寿命可能 > work()
}
```

goroutine 启动后调用方返回，goroutine 仍持有 `x`，`x` 必须堆分配。即便 `work` 返回，goroutine 里的 `use(&x)` 仍可能执行。

**7) channel 传递指针**

```go
ch <- &obj  // obj 逃逸：接收方在另一作用域
```

channel 把对象送到未知的接收方，接收方可能在任意时间、任意 goroutine 解引用——必须堆分配。

#### 2. 底层原理：为什么编译器必须保守

逃逸分析是**保守的**：宁可错堆分配，不可错栈分配（后者会释放仍被引用的内存，致命）。编译器只在能**证明**变量不逃逸时才栈分配；任何"可能"被外部引用的路径都视为逃逸。

举例：

```go
func maybe(b bool) *int {
    x := 1
    if b {
        return &x  // 这条路径让 x 逃逸
    }
    return nil      // 即使 b=false 不走 if，x 整体也逃逸
}
```

只要存在一条逃逸路径，整个变量逃逸。这是保守性的体现——编译器不做运行时分支预测，只做静态可达性证明。

> 保守性意味着 `-gcflags="-m"` 报告的逃逸，有些"理论上可以不逃逸"，但编译器无法证明。这是 Go 团队持续优化的方向（1.17 的新算法就改善了很多），但永远不可能完全消除。

#### 3. 工程实践与常见坑

- **`fmt.Println(x)` 默认装箱**：`Println(args ...any)`，每个参数都 `any` 装箱。热路径日志用 `strconv` 或 `log` 包中直接接受 `int`/`string` 的方法。
- **`map[K]V` 写入大 V**：`map` 自身在堆，写入时 `V` 拷贝进 map 内部桶。这不算逃逸，但同样有分配开销（map 桶扩容时）。
- **`append` 内层 slice**：`s := make([]int, 0, 10); s = append(s, x)` 当 cap 是常量小值时可能栈分配 backing array；但返回 slice 或 cap 来自变量会逃逸。
- **defer 闭包 vs defer 函数调用**：`defer func() { use(x) }()` 捕获 `x` 可能让 `x` 逃逸；`defer use(x)` 直接传值，`x` 在 defer 时就求值，常更优。

### 如何避免

#### 1. 检测工具

```bash
# 单文件
go build -gcflags="-m" main.go
# 整个项目，含决策理由
go build -gcflags="-m -m" ./...
# 竞争检测 + 逃逸一起看
go test -race -gcflags="-m" ./...
```

输出示例：

```
./main.go:8:9: &x escapes to heap
./main.go:8:9: moved to heap: x
./main.go:14:13: ... argument does not escape
```

`moved to heap: x` 明确告知变量被搬到堆。`does not escape` 是好消息——参数不逃逸。配合 `go tool pprof -alloc_objects` 找分配热点，能定位"哪个函数分配最多"。

#### 2. 实战技巧

**技巧 1：值返回 vs 指针返回**

```go
// 逃逸
func newPoint() *Point { p := Point{1, 2}; return &p }

// 不逃逸（小 struct）
func point() Point { return Point{1, 2} }
```

经验：struct ≤ 4 个字（32B on 64bit）用值返回，更省（栈分配 + 寄存器传值）。大 struct 用指针避免拷贝，但接受逃逸。

**技巧 2：预分配 slice**

```go
// 可能多次 growslice，每次堆分配
var s []int
for i := 0; i < n; i++ { s = append(s, i) }

// 一次分配到位（n 常量可能栈分配）
s := make([]int, 0, 64)
for i := 0; i < 64; i++ { s = append(s, i) }
```

**技巧 3：避免接口装箱——泛型（1.18+）**

```go
// 1.18 前：max(any, any) 装箱
func maxAny(a, b any) any { ... }

// 1.18+：泛型保留静态类型，不需要业务代码做 any 断言
func maxT[T constraints.Ordered](a, b T) T {
    if a > b { return a }
    return b
}
```

泛型可以避免显式转换到 `any`，但 Go 规范不承诺完全特化或零分配。当前编译器可能共享相同 GC shape 的机器码并通过 dictionary 传递类型操作；约束方法、接口转换和逃逸仍可能有成本。使用 `-gcflags=-m=2` 和 benchmark 验证具体调用点。

**技巧 4：`sync.Pool` 复用堆对象**

```go
var bufPool = sync.Pool{
    New: func() any { return new(bytes.Buffer) },
}
func handle(req []byte) {
    buf := bufPool.Get().(*bytes.Buffer)
    defer bufPool.Put(buf)
    buf.Reset()
    buf.Write(req)
    // ...
}
```

不能消除"第一次分配的逃逸"，但能让后续复用，等效降低分配速率。注意 Pool 对象在 GC 时会被清空，不是缓存。

**技巧 5：分离接口与具体类型**

```go
// 装箱
type Logger interface { Log(string) }
func do(l Logger) { l.Log("hi") }

// 直接调用具体类型
func do(l myLogger) { l.Log("hi") }
```

热点路径避免 interface，性能敏感处用具象类型。接口适合"对外 API"，内部热路径用具体类型。

**技巧 6：分离分配与使用**

把会逃逸的对象在"冷"路径预先分配（如对象池、struct 字段内嵌），热路径只引用不新建。

#### 3. 坑速查表

| 坑 | 现象 | 解法 |
|----|------|------|
| `fmt.Sprintf` / `errors.Errorf` 在热路径 | 每次分配字符串 + interface | 用 `strconv.AppendInt`、`errors.New`（无格式化） |
| defer 闭包捕获大对象 | 函数返回后闭包仍持有 | 改 `defer f(x)` 直接传值 |
| interface{} 字段 + 反射 | 反射值是 interface，对象常堆分配 | 热路径避反射 |
| `any` 参数装箱 | `pprof` 显示 `runtime.convT*` | 泛型或具体类型 |
| 返回大 struct 值 | 巨大拷贝 | 返回指针（接受逃逸）或拆字段 |
| 闭包返回持有大对象 | 内存不释放 | 显式 `=nil` 或不捕获 |
| 过度优化 | 代码丑陋、可读性差 | 先 profile 再优化，别为逃逸牺牲设计 |

> 过度优化的反面陷阱：为不逃逸把所有 struct 都改值传递，会让函数签名丑陋、大 struct 拷贝反而更慢。**先 profile，再优化**——逃逸分析报告只是参考，最终以 benchmark 为准。

### 编译器如何分析

#### 1. 算法概览

Go 的逃逸分析在 `cmd/compile/internal/escape` 包中实现，是一种**基于赋值图（assignment graph）的保守数据流分析**。核心思想：把每个变量当作节点，每次赋值/传参/返回当作"流"边，追踪"变量的地址是否流出当前函数"。

历史上 Go 用过两套算法：

- **旧算法（1.16 及以前）**：每个变量有个 `esc` 等级（`escNone`/`escReturn`/`escHeap`），通过函数参数的逃逸注解传播。
- **新算法（1.17+）**：基于更精细的有向图，每条边带"间接层级"（deref depth）。变量 `x` 逃逸当且仅当存在从 `x` 到"函数出口/全局/接口"且间接层级足够的路径。新算法更精确，让一些原本逃逸的不再逃逸。

#### 2. 经典算法：DAG 与 leaks 标记

简化伪代码：

```go
// cmd/compile/internal/escape (简化概念模型)
type holes struct {
    depth int    // 间接层级：0=直接，1=*x，2=**x...
    where *Node  // 发生位置（用于 -m -m 输出）
    next  *holes
}

// leak(x, h)：变量 x 流入 hole h
func leak(x *Node, h *holes) {
    // x 的地址被外部以 depth=h.depth 间接引用
    // 若 depth 足够浅（外部能直接访问 x 本体）且 x 是局部变量，标记逃逸
    if isLocal(x) && h.depth <= 0 {
        x.esc = escHeap
        return
    }
    // 递归传播到 x 的来源（x 可能来自另一变量）
    for src := range x.sources {
        leak(src, &holes{
            depth: h.depth - x.indirection,
            where: h.where,
            next:  h,
        })
    }
}
```

每次出现 `&x` 传给某处、`x` 作为参数、`x` 被返回，编译器都生成一条 leak 边。函数结束时，所有"边到达函数出口"且 depth 足够浅的局部变量标记为逃逸。

#### 3. 关键概念：间接层级（indirection）

```
&x           // 0 层间接：x 的地址（外部直接拿到 x 本体）
*(&x)        // 1 层间接：外部拿到 *x，即 x 指向的内容
**(&(&x))    // 2 层间接：外部拿到 **x
```

只有当"地址流出"且间接层级足够浅（即外部拿到的是 `x` 本体或 `*x`，能直接访问 `x`）时才逃逸。如果外部只能拿到 `**x`（更深的间接），往往不构成逃逸——因为外部无法直接访问 `x`，`x` 仍可随栈帧销毁。这是新算法比旧算法更精确的核心原因：旧算法只看"是否流出"，新算法还看"以多深的间接流出"。

#### 4. 编译器指令干预

| 指令 | 作用 |
|------|------|
| `//go:noescape` | 标记函数参数不逃逸（用于汇编实现的 runtime 函数，编译器无法分析函数体） |
| `//go:nosplit` | 跳过栈分裂检查（与逃逸无直接关系，但常一起出现在 leaf 优化） |
| `//go:inline` | 提示内联（1.17+，内联有助于逃逸分析） |

`//go:noescape` 是手写注解，告诉编译器"相信我，这个函数不会让参数逃逸"。`syscall` 包大量用它避免系统调用参数逃逸——因为系统调用是汇编实现的，编译器看不到函数体。

#### 5. 实战：读 `-gcflags="-m -m"` 输出

```
./foo.go:10:9: &x escapes to heap:
./foo.go:10:9:   flow: ~r0 = &x:
./foo.go:10:9:     from &x (spilled) at ./foo.go:10:9
./foo.go:10:9:     from return &x at ./foo.go:10:2
```

逐行解读：
- `&x escapes to heap`：结论，`x` 逃逸。
- `flow: ~r0 = &x`：逃逸路径——返回值 `~r0`（编译器内部命名）等于 `&x`。
- `from &x (spilled) at line 10:9`：`&x` 在 10:9 处被取出（spilled 表示赋值到内存）。
- `from return &x at line 10:2`：在 return 处流出函数。

通过这些 flow，你能精确定位是哪条语句触发的逃逸，再针对性优化。

#### 6. 内联与逃逸的协同

跨包调用若未被内联，编译器看不到函数体，参数只能按"逃逸"处理——因为编译器必须假设被调函数可能让参数逃逸。小函数被内联展开后，原本"调用方局部变量传给被调函数返回"可能变成"调用方内直接返回"，从而避免逃逸。

```go
// 若 getPtr 被内联，x 可能不逃逸
func getPtr(x int) *int { return &x }
func caller() *int {
    y := 42
    return getPtr(y)  // 内联后等价于 return &y → 仍逃逸（返回地址）
}
```

> 关键点：`go build -gcflags="-l"` 会关闭内联，导致更多逃逸——不要在生产关内联。内联是逃逸分析的重要前提。

#### 7. 工程实践与常见坑

- **跨包内联受限**：标准库的函数若未被内联（如 `fmt.Println`），其参数必然按"逃逸"处理。这就是为什么 `fmt.Println` 的参数总装箱——编译器看不到 `Println` 内部不会保留参数。
- **`-l` 关闭内联会让逃逸分析变差**：内联展开后，很多逃逸会消失。CI 别用 `-l` 跑生产构建。
- **CI 里加逃逸检查**：对核心热路径包用 `-gcflags="-m"` 做 lint，防止"无意中引入逃逸"回归。可以 grep `escapes to heap` 做断言。
- **逃逸分析结果随版本变**：1.17 的新算法让一些原本逃逸的不再逃逸。升级 Go 后应重测关键路径的 `-m` 输出，可能获得"免费"的性能提升。
- **`//go:noescape` 慎用**：误用会导致"本该堆分配的变量栈分配"，函数返回后内存被覆盖，引发难以排查的内存损坏。只在你 100% 确定函数不保留参数引用时用。

### 本章小结

- **逃逸分析**在编译期决定变量栈分配还是堆分配，是 GC 的最大盟友——不逃逸的变量随栈帧销毁，零 GC 开销。
- **逃逸场景**：返回指针、接口装箱、闭包捕获、运行时大小、goroutine/channel 引用、过大变量。编译器保守判定，宁可错堆分配不可错栈分配。
- **避免逃逸**：值返回小 struct、预分配 slice、泛型替接口、`sync.Pool`、热路径避 interface、defer 直接传值。
- **编译器实现**：基于赋值图 + 间接层级（deref depth）的保守数据流分析（1.17+ 新算法），`//go:noescape` 提供手动注解，内联是逃逸分析的重要前提。
- **工具链**：`-gcflags="-m"`（结论）/`-m -m`（flow 路径）是日常武器，配合 `pprof -alloc_objects` 找分配热点，最后用 benchmark 验证优化效果。
