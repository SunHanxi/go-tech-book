## 第7章 Interface（重点）

> 本章的语言语义基于 Go 1.26，Runtime 布局快照基于 Go 1.26.4。方法集和类型断言属于语言契约；`iface`、`abi.ITab`、装箱 helper 和去虚拟化策略是实现细节，不能当作跨版本 ABI。

### 7.1 行为契约与隐式实现

Interface 用方法集描述行为。具体类型只要拥有签名完全一致的所有方法，就实现该接口，无需 `implements`。

```go
type Reader interface {
    Read([]byte) (int, error)
}

type Buffer struct {
    data []byte
}

func (b *Buffer) Read(dst []byte) (int, error) {
    if len(b.data) == 0 {
        return 0, io.EOF
    }
    n := copy(dst, b.data)
    b.data = b.data[n:]
    return n, nil
}

var _ Reader = (*Buffer)(nil) // 编译期契约检查
```

隐式实现的主要价值是解耦。消费方可以只定义自己需要的行为，无需让实现包导入消费包：

```go
type UserFinder interface {
    FindUser(context.Context, int64) (User, error)
}

type Handler struct {
    users UserFinder
}
```

接口不是类继承：

- 它不承载实现状态、字段布局或构造规则。
- 一个类型可同时满足多个互不相关的接口。
- 同一接口可由没有继承关系的多个类型实现。

不要为每个 struct 预先创建一个镜像 interface。当调用方确实需要替换实现、隔离边界或表达稳定协议时再抽象。

接口可以嵌入其他接口来组合方法集。Go 1.14 起，嵌入的接口允许方法集重叠——只要重复方法的签名一致即可：

```go
type ReadCloser interface {
    io.Reader
    io.Closer
}

type Session interface {
    io.ReadCloser
    io.WriteCloser // Close 与上一行重复：Go 1.14+ 合法
}
```

Go 1.13 及更早版本中这种重叠是编译错误，`io.ReadWriteCloser` 等类型当年只能逐个方法展开声明。

### 7.2 方法集

对定义类型 `T`：

- `T` 的方法集包含接收者为 `T` 的方法。
- `*T` 的方法集包含接收者为 `T` 或 `*T` 的方法。

```go
type Counter int

func (Counter) Value() int { return 0 }
func (*Counter) Add(int)    {}

type Valuer interface{ Value() int }
type Adder interface{ Add(int) }

var c Counter
var _ Valuer = c
var _ Valuer = &c
// var _ Adder = c // 编译错误
var _ Adder = &c
```

`c.Add(1)` 可以编译，是因为 `c` 可寻址，编译器可把调用改写为 `(&c).Add(1)`。但赋值给接口时不会自动把 `T` 改成 `*T`；方法调用的语法糖不会改变 `T` 的方法集。

选择接收者时：

- 需要修改值、结构较大、含锁或不应复制的类型，通常使用指针接收者。
- 小型、不可变、值语义清晰的类型可使用值接收者。
- 同一类型的方法通常保持一致，不要只为了让某个接口赋值通过就随意混用。

嵌入字段会提升方法，但 `T` 与 `*T` 的提升方法集仍受接收者和嵌入形式影响。复杂组合应用 `var _ I = (*T)(nil)` 固化期望，不要靠记忆推导。

### 7.3 Runtime 表示

Go 规范将接口值描述为“动态类型 + 动态值”。Go 1.26.4 的 gc Runtime 对空接口与有方法接口使用两种两字布局：

```go
// runtime/runtime2.go，简化
type eface struct {
    typ  *abi.Type
    data unsafe.Pointer
}

type iface struct {
    tab  *abi.ITab
    data unsafe.Pointer
}
```

- `any` 是 `interface{}` 的别名，使用 `eface`。
- 包含方法的基本接口使用 `iface`。
- 在 64 位 gc 实现上通常是 16 字节，32 位实现通常是 8 字节。这不是语言规范保证，不应通过 `unsafe` 依赖。

`ITab` 关联目标接口类型、动态具体类型和方法入口：

```go
// internal/abi/iface.go，简化
type ITab struct {
    Inter *InterfaceType
    Type  *Type
    Hash  uint32
    Fun   [1]uintptr // 变长表
}
```

ITab 分配在非 GC 内存中。对编译期已知的具体类型到接口转换，编译器和链接器常能直接引用对应 itab；动态接口转换和断言可通过 Runtime itab table 查找、构建并缓存。因此不能概括为“每次接口赋值都要哈希查表”。

`data` 如何表示动态值取决于类型。部分指针形状类型可直接表示，其他值需要一份可寻址副本。副本可能在：

- 调用方栈上，若逃逸分析证明它不流出；
- 静态数据中，例如某些常量或 Runtime 的小整数复用表；
- Go 堆上，若它通过接口流出可证明的生命周期。

所以“值转接口必然堆分配”、“小整数转接口永远零分配”和“字符串转 `any` 必然分配堆上 header”都不成立。用当前工具链的 `-gcflags=-m=2` 与 `-benchmem` 验证具体调用点。

将 slice 放入 `any` 会复制 slice descriptor，不会自动深拷贝 backing array。复制接口值也不会为其中指针指向的对象做深拷贝。

### 7.4 nil Interface 与 Typed Nil

接口只在动态类型和动态值都不存在时等于 `nil`：

```go
var a any
fmt.Println(a == nil) // true

var p *bytes.Buffer
var b any = p
fmt.Println(b == nil) // false；动态类型是 *bytes.Buffer
```

最常见的 bug 是返回 typed nil error：

```go
type ParseError struct {
    Field string
}

func (e *ParseError) Error() string {
    if e == nil {
        return "<nil>"
    }
    return "invalid field: " + e.Field
}

func bad() error {
    var err *ParseError
    return err // 非 nil error：(*ParseError, nil)
}

func good() error {
    return nil
}
```

规则不是“不能在接口里放 nil 指针”。某些 API 有意用 typed nil 表达状态，指针 receiver 也可定义 nil-safe 行为。真正要求是：导出 API 写清 nil 契约，“无错误”必须直接 `return nil`。

nil slice、nil map、nil func 和 nil channel 放入 `any` 后也都是非 nil 接口，因为它们各自有动态类型。

### 7.5 类型断言与 Type Switch

断言检查接口的动态类型是否满足目标类型：

```go
value, ok := x.(string)
if !ok {
    // value 是 string 零值
}

value = x.(string) // 失败时 panic
```

目标可以是具体类型，也可以是另一个接口：

```go
type Flusher interface{ Flush() error }

if f, ok := writer.(Flusher); ok {
    if err := f.Flush(); err != nil {
        return err
    }
}
```

这种小能力接口适合可选能力探测。但若能力是业务正确性的必备条件，应把它放进函数参数类型，不要到运行时才发现实现不完整。

对 error 值做类型断言时，注意错误可能被 `fmt.Errorf("%w", ...)` 层层包装，直接 `err.(*ParseError)` 只检查最外层。`errors.As` 是沿 error 链逐层做类型断言的标准库封装，`errors.Is` 则是沿链的相等比较，应作为默认选择。详见[第23章 错误处理](./23-错误处理.md)。

Type switch 把多个断言集中表达：

```go
func describe(x any) string {
    switch v := x.(type) {
    case nil:
        return "nil"
    case string:
        return "string: " + v
    case fmt.Stringer:
        return "stringer: " + v.String()
    default:
        return fmt.Sprintf("%T", v)
    }
}
```

case 顺序可影响匹配结果：一个动态类型可同时满足多个接口 case，执行第一个匹配分支。编译器可根据 case 集生成 hash、jump table 或 cache 等策略，不应把它固定理解为从上到下每次比较 `_type.hash`。

在请求边界对外部输入做不可控的单值断言会将普通输入错误变成 panic。边界解码应用 comma-ok 或结构化 schema 校验返回错误。

### 7.6 接口比较

接口值是可比较的类型，但运行时比较还要求动态类型可比较：

```go
var a any = 1
var b any = 1
fmt.Println(a == b) // true

var c any = []int{1}
var d any = []int{1}
fmt.Println(c == d) // panic: comparing uncomparable type []int
```

两个非 nil 接口相等需要：

1. 动态类型相同。
2. 该动态类型可比较。
3. 动态值按该类型的 `==` 规则相等。

将 `any` 用作 map key 时同样要小心：静态 key 类型 `any` 可用于 map，但插入动态类型为 slice、map 或 func 的值会在运行时 panic。业务 key 应优先使用显式的可比较类型或 tagged key。

### 7.7 基本接口与类型集接口

Go 1.18 以后，interface 还可在泛型约束中描述类型集：

```go
type Integer interface {
    ~int | ~int8 | ~int16 | ~int32 | ~int64
}
```

包含类型项、`~T` 或 union 的非基本接口只能用作类型参数约束，不能声明普通运行时变量：

```go
// var x Integer // 编译错误：只能用作约束
```

只包含方法的基本接口，如 `io.Reader`，既可作为运行时值类型，也可作为约束。两种用法不应混为一个性能模型：泛型函数可以使用 shape 与 dictionary，运行时接口方法通常使用 itab 入口，而编译器还可对两者做内联和去虚拟化。详见[第8章 现代类型系统与泛型](./08-现代类型系统与泛型.md)。

### 7.8 Interface 与泛型如何选

| 需求 | 优先考虑 |
|---|---|
| 运行时持有不同行为实现 | Interface |
| API 只依赖少量方法 | Interface |
| 同一算法用于编译期已知类型集 | 泛型 |
| 容器需保留元素具体类型 | 泛型 |
| 异构消息且有固定种类 | 显式 tagged union 或 Interface |
| 解码未知 schema 的外部数据 | `any` + 严格边界校验 |

例如 `func Decode(r io.Reader) error` 已是精确的行为契约。改成 `func Decode[R io.Reader](r R) error` 通常不增加类型安全，却可能让函数值和导出 API 更复杂。

反过来，对 `slices.Sort`、`maps.Clone` 这种需保留具体容器和元素类型的算法，泛型比 `[]any` 或反射更自然。

### 7.9 性能边界

接口可能带来四类成本：

1. 构造接口值时的值复制或转换 helper。
2. 数据经接口流出后的堆分配。
3. 接口方法调用的间接分派。
4. 动态转换、断言和 type switch 的类型检查。

但这些成本都不是固定的：

- 编译器若知道动态类型，可以去虚拟化接口调用，并继续内联具体方法。
- PGO 可对热路径的高概率动态类型做 profile-guided devirtualization，效果依赖 profile 代表性。
- 接口副本不逃逸时可以位于栈上或被完全优化。
- 真实工作如 I/O、加密、JSON 解析往往远高于一次间接调用。

不要使用“接口调用固定慢 3~10 倍”或“itab 命中固定 1ns”的表格。CPU、Go 版本、内联、去虚拟化和 benchmark 形状都会改变结果。

可重复的测量方式：

```go
type Adder interface {
    Add(int) int
}

type Counter struct{ n int }

func (c *Counter) Add(n int) int {
    c.n += n
    return c.n
}

var result int

func BenchmarkInterfaceCall(b *testing.B) {
    var a Adder = &Counter{}
    for b.Loop() {
        result = a.Add(1)
    }
}
```

与具体类型版本对比时：

- 将构造和 cleanup 放在 `b.Loop()` 外。
- 使用可观察 sink，检查编译器没有删掉工作。
- 报告 `-benchmem`，用 `benchstat` 比较多次样本。
- 用 `-gcflags=-m=2` 查是否内联、去虚拟化和逃逸。
- 在与生产相同的 PGO 设置下测试。

若 profile 证明接口处在极热循环，可依次考虑：

1. 把不变的接口转换移到循环外。
2. 给内部热路径增加具体类型快路径，保留接口边界。
3. 对编译期已知的类型集使用泛型，但重新测试 dictionary 和代码体积。
4. 改变 API 前确认收益大于可测性、演进性与复杂度代价。

### 7.10 API 设计

#### 由消费者定义最小接口

```go
// package report
type UserLoader interface {
    LoadUser(context.Context, string) (User, error)
}

func Build(ctx context.Context, loader UserLoader, id string) (Report, error) {
    // ...
}
```

数据库包无需声明它实现 `report.UserLoader`。消费者只要一个方法，测试 double 也只需实现一个方法。

但“永远由消费者定义”也不是死规则。`io.Reader`、`fs.FS`、`http.RoundTripper` 这类稳定交互协议适合由提供方导出。判断标准是它是否表达稳定协议，而不是为单一实现预留虚拟层。

#### 接受接口，通常返回具体类型

返回具体类型让提供方之后可增加方法，调用方仍可把结果赋给自己需要的接口：

```go
func NewClient(cfg Config) *Client
```

但当函数的契约就是从多个隐藏实现中选择一个，或必须限制调用方可见能力时，返回 interface 也可能合理。这是演进性取舍，不是 lint 规则。

#### 签名必须精确匹配

Go 没有接口方法返回值的协变：

```go
type Animal interface{ Name() string }
type Dog struct{}
func (Dog) Name() string { return "dog" }

type Factory interface {
    New() Animal
}

type DogFactory struct{}
func (DogFactory) New() Dog { return Dog{} }

// var _ Factory = DogFactory{} // 错误：New() Dog 不等于 New() Animal
```

这避免了隐式转换与方法表 ABI 的复杂性。需要 `Factory` 时，实现的签名必须直接返回 `Animal`。

#### 文档化行为契约

方法集一致只能证明代码可编译。接口文档还应说明：

- 实现能否被多个 goroutine 并发调用。
- 参数 slice/map 是否可保留或修改。
- nil 接收者、nil 参数和零值是否有效。
- `Close` 是否幂等，失败后能否重试。
- Context 取消后的返回边界。

接口本身不会增加同步、所有权或资源生命周期保证。

### 常见误区

| 误区 | 正确理解 |
|---|---|
| Interface 是 Java/C++ 类层次的替代 | Interface 表达行为集合，Go 偏好组合和小协议 |
| 可寻址值能调指针 receiver，所以值实现该接口 | 调用改写不改变方法集 |
| 值赋给 interface 必然堆分配 | 副本可在栈、静态区或堆上，要看逃逸分析 |
| Interface 方法永远无法内联 | 编译器可静态或基于 PGO 去虚拟化 |
| Typed nil 接口等于 nil | 动态类型仍存在，所以接口非 nil |
| `any` 作 map key 可接收任何值 | 动态类型不可比较时插入或比较会 panic |
| Interface 返回值支持协变 | 方法签名必须精确匹配 |
| 把对象放入 interface 就并发安全 | Interface 不增加同步或所有权保证 |
| 所有抽象都应用泛型替换 | 泛型适合编译期类型集，Interface 适合运行时行为分派 |

### 源码阅读路线

1. Go 规范的 Method sets、Interface types、Type assertions 与 Comparison operators。
2. `src/internal/abi/iface.go` 和 `type.go`：`ITab`、`EmptyInterface`、`KindDirectIface`。
3. `src/runtime/runtime2.go`：`iface` 与 `eface` 概念布局。
4. `src/runtime/iface.go`：`getitab`、断言、转换 helper 和 interface switch cache。
5. `src/cmd/compile/internal/devirtualize/`：静态与 PGO 去虚拟化。
6. `src/cmd/compile/internal/escape/`：接口数据流与逃逸。

阅读时固定到具体 Go tag，并将语言契约与 gc Runtime 私有 ABI 分开。

### 本章小结

- 具体类型通过方法集隐式实现接口；方法调用的自动取地址不会改变方法集。
- 接口值由动态类型与动态值组成。Go 1.26.4 gc Runtime 用 `eface{type,data}` 和 `iface{itab,data}` 实现，但布局不是语言保证。
- 只有动态类型和动态值都不存在时接口才等于 nil；typed nil error 必须在 API 边界避免。
- 接口比较要求动态类型可比较，`any` 不能让 slice、map 或 func 变得可比较。
- 接口转换不必然堆分配，接口调用也不必然保留间接分派；内联、去虚拟化、PGO 和逃逸都需用当前工具链测量。
- API 应优先小而稳定的行为契约，同时写清并发、nil、所有权、取消与关闭语义。
