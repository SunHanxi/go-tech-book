## 第7章 Interface（重点）

> 引言：Interface 是 Go 实现"多态"与"抽象"的核心机制，理解其 iface/eface 的双字结构与 itab 缓存，是写出高性能、低坑接口代码的前提。

### 7.1 Interface 是什么

**(1) 是什么**

Interface 是 Go 的一种类型（type），它定义了一组**方法签名（method set）**。任何具体类型（concrete type）只要实现了这些方法，就被认为"满足"该接口，可以赋值给接口变量。接口变量本身不存数据，只存"我是什么类型 + 我的值在哪"。

```go
package main

import "fmt"

// 定义接口
type Animal interface {
    Sound() string
}

// 具体类型
type Dog struct{ Name string }

func (d Dog) Sound() string { return "Woof" }

type Cat struct{ Name string }

func (c Cat) Sound() string { return "Meow" }

func main() {
    var a Animal
    a = Dog{Name: "Rex"}
    fmt.Println(a.Sound()) // Woof
    a = Cat{Name: "Tom"}
    fmt.Println(a.Sound()) // Meow
}
```

`a` 是接口变量，它可以持有任何实现了 `Animal` 的具体值。调用 `a.Sound()` 时，运行时根据 `a` 内部记录的具体类型找到对应方法。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

接口有两层语义：

1. **方法集契约**：接口声明"我需要哪些方法"。
2. **(type, value) 二元组**：接口变量的运行时表示是一个具体类型加上一个具体值。

这两层对应到底层数据结构就是下一节的 iface/eface。

接口变量在内存里固定是 **2 个字（word）**：64 位平台共 16 字节。这两个字分别是"类型信息指针"和"数据指针"（详见 7.3、7.4）。这使得接口变量的传递代价恒定，与具体类型大小无关。

与其它语言的对比：

| 语言 | 抽象机制 | 特点 |
|------|----------|------|
| Java/C# | 显式 `implements` | 名义类型（nominal），必须在类声明里写明 |
| Go | 隐式实现 | 结构类型（structural），不写 `implements` |
| Rust | trait | 显式 `impl Trait for Type` |
| Python | duck typing | 运行时检查，无接口声明 |

Go 的"隐式实现"是其设计精髓，下一节展开。

**(3) 工程实践与常见坑**

> 坑 1：接口不是"类继承"的替代品，它是"行为抽象"。不要为了"有接口"而定义接口，应遵循"消费者定义接口"原则（见 7.10）。

> 实践：Go 标准库大量使用小接口（1~3 个方法），如 `io.Reader`、`io.Writer`、`fmt.Stringer`。接口越小，被实现的概率越高，复用性越强。

### 7.2 Duck Typing

**(1) 是什么**

Duck Typing（鸭子类型）来自一句话："如果它走起来像鸭子，叫起来像鸭子，那它就是鸭子。"在类型系统里，这意味着：**一个类型是否满足某接口，只看它有没有那些方法，不看它的名字或继承关系**。

Go 的 interface 就是 duck typing 的静态版：编译期检查方法集，无需 `implements` 声明。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

Go 选择隐式实现有深层原因：

1. **解耦定义方与实现方**。Java 里要给一个第三方库的类"加"一个接口，只能改源码或包一层。Go 里你可以为任何类型定义接口，只要它有匹配方法，无需修改原类型。这是"接受接口返回结构体"的基础。

2. **避免继承层级地狱**。显式 implements 容易催生深继承树和"接口爆炸"。Go 没有继承，只有组合，接口作为薄薄的契约层。

3. **向后兼容的演化**。新增接口不需要改动老类型，只要老类型已有匹配方法。`context.Context` 引入后，大量已有类型自动满足。

4. **测试友好**。定义一个小接口，mock 时只需实现那几个方法，无需继承框架。

与运行时 duck typing 的区别：Python/JS 的 duck typing 是**运行时**检查，调用 `x.quack()` 时若 x 没有 quack 方法才报错。Go 是**编译期**检查，把 x 赋给 `Quacker` 接口时编译器验证方法集。这结合了 duck typing 的灵活与静态类型的早期错误发现。

```go
package main

import "fmt"

type Quacker interface{ Quack() string }

type Duck struct{}

func (Duck) Quack() string { return "quack" }

type Toy struct{}

func (Toy) Quack() string { return "quack (recording)" }

func quackIt(q Quacker) { fmt.Println(q.Quack()) }

func main() {
    quackIt(Duck{}) // 编译期已确认 Duck 满足 Quacker
    quackIt(Toy{})
}
```

Runtime 实现要点：隐式实现意味着把具体类型 `T` 赋值给接口 `I` 时，编译器生成代码去检查/构建一个 `itab`（接口表），里面记录"I 的哪些方法对应 T 的哪些方法"。这个 itab 会被缓存（详见 7.3）。这一切对程序员透明。

**(3) 工程实践与常见坑**

> 坑 1：duck typing 让"接口满足"是隐式的，有时一个类型"恰好"有同名方法就意外满足某接口。这通常无害，但如果接口语义严格要求（如 `io.Closer` 要求 Close 后资源释放），实现方必须保证语义，而不只是签名匹配。

> 坑 2：方法集差异（见 7.5）。`T` 和 `*T` 的方法集不同，赋值时要注意接收者类型。

> 实践：定义接口时，方法数尽量少（1~3 个），方法名要表达"行为"而非"实现"。`Reader`/`Writer`/`Closer` 都是单一行为。

### 7.3 iface

**(1) 是什么**

`iface` 是 Go runtime 中**非空接口**（即声明了方法的接口）的内部表示。当你写 `var r io.Reader` 并赋值一个 `*os.File` 时，底层就是这个结构。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

定义在 `runtime/runtime2.go`（简化）：

```go
// runtime/runtime2.go
type iface struct {
    tab  *itab          // 接口表：描述"哪个接口 + 哪个具体类型 + 方法函数表"
    data unsafe.Pointer // 指向具体值（通常在堆上）
}

type itab struct {
    inter *interfacetype // 接口类型描述
    _type *_type         // 具体类型描述
    hash  uint32         // _type.hash 的拷贝，用于 type switch 加速
    _     [4]byte        // 对齐填充
    fun   [1]uintptr     // 变长方法表：fun[0] 是接口第一个方法对应的实现函数地址；fun[0]==0 表示类型不实现该接口（缓存否定结果）
}
```

逐字段解释：

**`iface.tab` (`*itab`)**

`itab` 是接口转换的核心，它回答两个问题："这个接口变量满足的是哪个接口？"和"它内部具体类型是哪个？以及方法怎么派发？"

| 字段 | 含义 |
|------|------|
| `inter` | 指向接口自身的类型元信息 `interfacetype`，包含接口的包路径、方法列表（按名排序） |
| `_type` | 指向具体类型的元信息 `_type`，含大小、对齐、hash、kind 等 |
| `hash` | 拷贝自 `_type.hash`，type switch 时直接用，省一次解引用 |
| `fun` | **方法函数指针表**。`fun[0]` 对应接口的第一个方法，`fun[1]` 第二个……每个是具体类型实现该方法的函数地址。数组声明为 `[1]uintptr` 但实际是变长，runtime 通过 `add(itab.fun, i*8)` 取第 i 个 |

> 关键：`itab.fun` 里存的是**具体类型的方法地址**。调用 `r.Read(buf)` 时，runtime 从 `itab.fun[0]` 取出 `*os.File.Read` 的地址直接调用。这就是接口方法分发的本质。

**`iface.data` (`unsafe.Pointer`)**

指向具体值。如果具体类型是 `*os.File`，data 就指向那个 File 结构。如果具体类型是 `int`，data 指向一个存了 int 的内存。Runtime 会做优化：对于 0~255 的小整数，使用预分配的静态表 `staticuint64s`，避免每次装箱都分配；其它值则在堆上分配一份拷贝，data 指向它。这也是接口赋值导致堆分配的根源（详见 7.9）。

`_type` 简化结构：

```go
// runtime/type.go
type _type struct {
    size       uintptr // 类型大小
    ptrdata    uintptr // 前多少字节含指针（GC 用）
    hash       uint32  // 类型 hash，type switch / itab 用
    tflag      tflag   // 标志位
    align      uint8   // 对齐
    fieldalign uint8   // 字段对齐
    kind       uint8   // 类型种类（kindBool, kindInt, kindPtr...）
    // ... 还有 equal、gcdata 等
}
```

`kind` 让 runtime 能快速判断"这是个指针？struct？func？"，是 type switch 和 GC 的基础。

**itab 的构建与缓存**

itab 的构建代价不小：要把接口的方法列表和具体类型的方法列表做匹配（按名+签名），填好 fun 表。为避免每次接口赋值都重算，runtime 用一个全局哈希表缓存 itab：

```go
// runtime/iface.go
var itabCache = &itabCacheTable{} // 全局 itab 缓存
```

赋值 `r = (*os.File)(f)` 时：

1. 用 `(接口类型, 具体类型)` 作 key 查 itabCache。
2. 命中则直接用；未命中则调用 `getitab` 重新构建（含方法集匹配），构建失败说明类型不满足接口，会 panic（赋值时）或返回 ok=false（断言时）。
3. 构建成功后存入缓存，下次复用；构建失败的组合也会以 `fun[0]==0` 的形式缓存，避免重复尝试。

这意味着：**接口赋值的开销主要是查缓存（一次哈希）+ 一次指针写入**，非常便宜。但首次构建某 (接口,类型) 对的 itab 会有一次性开销。

**(3) 工程实践与常见坑**

> 坑 1：接口变量持有大 struct 时，data 指向它在堆上的拷贝，触发堆分配。如果 struct 很大且频繁进接口，考虑用 `*T` 而非 `T`（指针固定一个字，且方法集更宽，见 7.5）。

> 坑 2：把值类型赋给接口后，修改原值不影响接口内的值（接口持有的是拷贝）。

```go
package main

import "fmt"

type Val struct{ n int }
type Getter interface{ Get() int }
func (v Val) Get() int { return v.n }

func main() {
    v := Val{n: 1}
    var g Getter = v
    v.n = 100
    fmt.Println(g.Get()) // 1：接口持有的是 v 的旧拷贝
}
```

> 实践：性能敏感路径里，把"接口类型"作为局部变量没问题（itab 缓存命中后几乎免费），但要警惕接口赋值导致的小对象逃逸到堆（见 7.9）。

### 7.4 eface

**(1) 是什么**

`eface` 是 **空接口** `interface{}`（Go 1.18 起可写作 `any`）的内部表示。因为空接口没有方法，不需要 itab（没有方法表要存），结构比 iface 更简单。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

```go
// runtime/runtime2.go
type eface struct {
    _type *_type         // 具体类型描述
    data  unsafe.Pointer // 指向具体值
}
```

逐字段：

| 字段 | 含义 |
|------|------|
| `_type` | 指向具体类型的 `_type` 元信息（同 iface 里的 `_type`），但这里直接放在第一个字，因为不需要 itab |
| `data` | 指向具体值，语义同 iface.data |

对比 iface：

| 结构 | 第一个字 | 第二个字 | 用途 |
|------|----------|----------|------|
| `iface` | `*itab`（接口+类型+方法表） | `data` | 非空接口 |
| `eface` | `*_type`（仅类型） | `data` | 空接口 |

为什么 eface 没有 itab？因为空接口不关心方法，只要"是个值"就行。itab 的核心是方法表，空接口没有方法表，所以省掉 `inter` 和 `fun`，直接放 `_type`。

**Go 1.18 的 any**

Go 1.18 引入 `any` 作为 `interface{}` 的别名：

```go
// builtin/builtin.go (Go 1.18+)
type any = interface{}
```

二者完全等价，`any` 只是更短更顺手。底层都是 eface。

**赋值过程**

```go
package main

import "fmt"

func main() {
    var e any
    e = 42       // e._type = int 的 _type; e.data 指向静态表中的 42
    e = "hello"  // e._type = string 的 _type; e.data 指向堆上分配的 string header
    fmt.Println(e)
}
```

赋值时 runtime 根据 `_type` 决定如何存放值。`42` 是小整数，runtime 用预分配的 `staticuint64s` 表避免分配（data 指向表中对应槽位）；`"hello"` 是非空 string，16 字节，runtime 在堆上分配 string header，data 指向它。这是接口装箱产生堆分配的根源。

**type switch 的实现**

空接口的 type switch（`switch v := x.(type)`）通过比较 `_type` 指针或 `_type.hash` 实现：

```go
package main

import "fmt"

func describe(e any) {
    switch v := e.(type) {
    case int:
        fmt.Printf("int: %d\n", v)
    case string:
        fmt.Printf("string: %q\n", v)
    default:
        fmt.Printf("other: %T\n", v)
    }
}

func main() {
    describe(42)
    describe("hi")
    describe(3.14)
}
```

runtime 比较每个 case 的 `_type` 与 eface 里的 `_type`，hash 不等直接跳过，hash 相等再比指针。这是 O(1) per case 的快速分发。

**(3) 工程实践与常见坑**

> 坑 1（重要）：`any` 不是"无类型"，它有明确的内部表示。把 `nil` 赋给 `any` 后，`_type` 和 `data` 都是 nil；但把一个 `*int` 类型的 nil 赋给 `any`，`_type` 非 nil（指向 int 的类型信息），`data` 是 nil。这导致 `e == nil` 判断的坑（见 7.8）。

> 坑 2：`any` 滥用会丧失类型安全。Go 1.18+ 的泛型（`func F[T any]`）通常比 `any` 参数更安全高效，因为泛型在编译期保留具体类型，接口赋值的装箱/逃逸被消除。

> 实践：函数签名尽量用具体类型或泛型，只有真正"任意类型"的场景（如序列化、`fmt.Println`）才用 `any`。

### 7.5 方法集

**(1) 是什么**

方法集（method set）是一个类型所拥有的全部方法的集合。理解方法集的关键在于：**类型 `T` 和 `*T` 的方法集不同**。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

Go 语言规范明确：

- 类型 `T`（值类型）的方法集 = 所有接收者为 `T` 的方法。
- 类型 `*T`（指针类型）的方法集 = 接收者为 `T` 和 `*T` 的所有方法。

也就是说，`*T` 的方法集是 `T` 的方法集的**超集**。

```go
package main

import "fmt"

type Counter struct{ n int }

func (c Counter) Get() int { return c.n } // 接收者 T
func (c *Counter) Inc()    { c.n++ }      // 接收者 *T

type Incrementer interface{ Inc() }

func main() {
    c := Counter{n: 0}
    // var i Incrementer = c   // 编译错误：Counter 的方法集不含 Inc
    var i Incrementer = &c     // OK：*Counter 的方法集含 Inc
    i.Inc()
    fmt.Println(c.n) // 1
}
```

`c`（值）不能赋给 `Incrementer`，因为 `Inc` 的接收者是 `*Counter`，不在 `Counter` 的方法集里。`&c`（指针）可以。

**为什么这样设计 —— 地址性（addressability）**

核心是"方法接收者能否取地址"。当调用 `c.Inc()` 时，若接收者要求 `*Counter`，runtime 需要 `&c` 来修改 c。这要求 c 是**可寻址**的。

- 局部变量 `c` 可寻址，所以 `c.Inc()` 会被自动改写为 `(&c).Inc()`（编译器语法糖）。
- 但 map 的元素、字面量、函数返回值不可寻址：`m["k"].Inc()` 不行，因为无法取 `m["k"]` 的地址。
- 把 `c` 赋给接口时，接口持有的是 c 的拷贝，这个拷贝在接口内部不可寻址，无法再取 `&c`。所以接口要求方法必须在 c 的方法集里。

指针接收者方法要修改原对象，必须能拿到地址；值接收者方法只读拷贝，无需地址。这就是方法集规则的底层动机。

**值接收者 vs 指针接收者的语义差异**

```go
package main

import "fmt"

type V struct{ x int }

func (v V) Set(x int)  { v.x = x }   // 值接收者：改的是拷贝，原对象不变
func (p *V) SetP(x int) { p.x = x }  // 指针接收者：改原对象

func main() {
    v := V{x: 1}
    v.Set(100)
    fmt.Println(v.x) // 1，没变
    (&v).SetP(100)
    fmt.Println(v.x) // 100
}
```

如何选择接收者类型：

| 场景 | 推荐 |
|------|------|
| 方法需要修改 receiver | `*T` |
| receiver 很大（struct 大） | `*T`（避免每次调用拷贝） |
| receiver 是小值类型且不可变（如 time.Time） | `T` |
| 类型需要被 map/slice 的值持有并满足接口 | 看 method set 需求，常需 `*T` |
| 一致性 | 一个类型的方法尽量统一接收者风格 |

**(3) 工程实践与常见坑**

> 坑 1：实现 `json.Unmarshaler` 等要求修改 receiver 的接口时，必须用指针接收者。`func (m MyType) UnmarshalJSON(...)` 看似实现了接口，但赋值时 `var json.Unmarshaler = m` 会失败（m 的方法集不含 UnmarshalJSON）。

> 坑 2：`[]T` 的元素可寻址，`map[K]V` 的元素不可寻址。`s := []V{{1}}; s[0].SetP(100)` 是 OK 的，但 `m := map[string]V{"k": {1}}; m["k"].SetP(100)` 编译错误。

> 实践：除非有强理由，struct 的方法默认用 `*T` 接收者，避免拷贝大对象、避免方法集不一致的坑。

### 7.6 接口断言

**(1) 是什么**

接口断言（type assertion）是从接口变量中取出具体类型，或判断接口变量是否满足另一个接口。两种形式：

```go
x.(T)          // 单返回：若 x 不是 T，panic
v, ok := x.(T) // 双返回：ok 为 bool，不 panic
```

T 可以是具体类型，也可以是另一个接口类型。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

用法示例：

```go
package main

import "fmt"

func main() {
    var i any = "hello"

    // 1. 断言为具体类型
    s, ok := i.(string)
    fmt.Println(s, ok) // hello true

    n, ok := i.(int)
    fmt.Println(n, ok) // 0 false

    // 2. 断言为另一接口（判断是否同时满足）
    // rc, ok := r.(io.Closer)
}
```

type switch 是批量断言的语法糖：

```go
package main

import "fmt"

func classify(i any) {
    switch v := i.(type) {
    case int:
        fmt.Printf("int: %d\n", v)
    case string:
        fmt.Printf("string: %q\n", v)
    case []byte:
        fmt.Printf("bytes: %d\n", len(v))
    case nil:
        fmt.Println("nil")
    default:
        fmt.Printf("other: %T\n", v)
    }
}
```

**底层实现**

断言为具体类型时，runtime 比较 iface/eface 里的 `_type`：

- eface: 直接比 `_type` 指针（或先比 hash 再比指针）。
- iface: 从 `itab._type` 取具体类型比较。

断言为接口类型时，runtime 调用 `getitab(目标接口, 具体类型, canFail)`：

1. 查 itabCache，看 (目标接口, 具体类型) 是否已构建。
2. 命中则用缓存的 itab，返回（接口变量 = {itab, data}）。
3. 未命中则构建新 itab（方法集匹配），失败则 `canFail=true` 时返回 nil（ok=false），`canFail=false` 时 panic。

简化伪代码：

```go
// runtime/iface.go 简化
func assertE2I(inter *interfacetype, e eface) (r iface) {
    t := e._type
    if t == nil {
        panic("interface conversion: nil is not " + inter.name)
    }
    tab := getitab(inter, t, false) // false = panic on failure
    r.tab = tab
    r.data = e.data
    return
}
```

type switch 在 runtime 层是一连串类型比较，编译器会生成优化代码（用 hash 跳表）。

**(3) 工程实践与常见坑**

> 坑 1：单返回断言失败会 panic。生产代码里除非能 100% 保证类型，否则一律用双返回 `v, ok := i.(T)`。

> 坑 2：断言接口时要理解方法集。`var r io.Reader = bytes.NewReader(...); _, ok := r.(io.Closer)`，bytes.Reader 实现了 Close（noop），所以 ok 为 true。注意断言接口成功仅表示"方法集满足"，不代表语义等同。

> 实践：在错误处理里频繁见到 `if e, ok := err.(interface{ Unwrap() error }); ok {...}`，这是 errors.Is/As 的底层机制（详见[第20章 错误处理](20-错误处理.md)）。

> 实践：用 type switch 处理"联合类型"比连串 if-assert 更清晰，且编译器有优化。

### 7.7 类型转换

**(1) 是什么**

"类型转换"在接口语境下有几层含义，要区分清楚：

1. **具体类型 → 接口**（装箱，boxing）：把一个具体值塞进接口变量。
2. **接口 → 具体类型**（断言，unboxing）：见 7.6。
3. **接口 A → 接口 B**（接口间转换）：判断具体类型是否也满足 B。
4. **具体类型间的转换**（如 `int(float64)`）：与接口无关，本节不展开。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

**具体类型 → 接口（装箱）**

```go
package main

import "fmt"

type MyInt int

func (m MyInt) String() string { return fmt.Sprintf("MyInt(%d)", m) }

func main() {
    var s fmt.Stringer = MyInt(42) // 装箱：构造 iface{tab: itab(MyInt, Stringer), data: 指向 42}
    fmt.Println(s)
}
```

装箱的 runtime 过程：

1. 编译器在赋值点生成调用，构建 iface。
2. 找到/构建 itab（MyInt, Stringer），放入 `iface.tab`。
3. 把 MyInt(42) 放到某处，`iface.data` 指向它。小整数用静态表，否则在堆上分配拷贝。

**装箱导致逃逸**

值类型赋给接口时，runtime 通常需要在堆上分配一份拷贝（小整数等特例除外），data 指向它。这就是"接口赋值导致逃逸"的根源（详见 7.9）。

```go
package main

type Adder interface{ Add(int) int }

type myInt struct{ n int }

func (m *myInt) Add(x int) int { m.n += x; return m.n }

func main() {
    var a Adder = &myInt{n: 0} // &myInt 逃逸到堆
    _ = a
}
```

**接口 → 接口**

```go
package main

import "io"

func main() {
    var r io.Reader // 假设持有某具体类型
    // 转 io.ReadWriter：要求具体类型同时满足 Reader 和 Writer
    // rw, ok := r.(io.ReadWriter)
    _ = r
}
```

runtime 用 getitab 查 (ReadWriter, 具体类型)，命中则用，未命中则构建。

**泛型 vs 接口转换**

Go 1.18 泛型让很多"接口转换 + 反射"的场景可以编译期解决：

```go
package main

import "fmt"

// 泛型版本：T 在编译期已知，无装箱
func Max[T int | float64](a, b T) T {
    if a > b {
        return a
    }
    return b
}

// 接口版本：要断言、要装箱
func MaxIface(a, b any) any {
    // ... 需要类型断言，性能差
    return nil
}

func main() {
    fmt.Println(Max(1, 2))
    fmt.Println(Max(1.5, 2.5))
}
```

泛型函数内部对 T 的操作直接用具体类型指令，无 itab 查找、无装箱，性能接近手写特化版本。

**(3) 工程实践与常见坑**

> 坑 1：把小整数频繁装箱进 `any`/`interface{}`（如 `[]any{1,2,3}`）会触发堆分配，每个 int 一份（小整数特例除外，但跨函数传递后通常仍需分配）。改用泛型 `[]T` 或具体 `[]int` 可消除。

> 坑 2：接口 → 接口转换不是免费的，要查 itab。热路径里避免反复转换，提前转一次存好。

> 实践：能用泛型就用泛型，既类型安全又无装箱开销；只有真正"多态分发"（运行时才知道具体类型）才用接口。

### 7.8 nil interface

**(1) 是什么**

"nil interface"是 Go 里最经典的坑之一。一个接口变量为 nil，当且仅当它的**类型字段和数据字段都为 nil**。

回顾 eface/iface 结构：

```go
type eface struct {
    _type *_type         // nil 时为 nil
    data  unsafe.Pointer // nil 时为 nil
}

type iface struct {
    tab  *itab           // nil 时为 nil
    data unsafe.Pointer  // nil 时为 nil
}
```

只有 `tab == nil && data == nil`（eface 同理 `_type == nil && data == nil`）时，接口变量才 `== nil`。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

**经典坑**

```go
package main

import "fmt"

type MyError struct{ msg string }

func (e *MyError) Error() string { return e.msg }

func doSomething() error {
    var err *MyError = nil
    // 业务上没出错，返回 nil 错误
    return err
}

func main() {
    err := doSomething()
    fmt.Println(err == nil) // false！
}
```

`doSomething` 返回的 `err` 类型是 `error`（接口）。返回时 `var err *MyError = nil` 这个 nil 指针被**装箱**进 error 接口：`iface.tab = itab(*MyError, error)`（非 nil！），`iface.data = nil`。结果接口变量不为 nil。

这是"具体类型的 nil"与"接口的 nil"的根本区别：接口记录了"我是 *MyError 类型的 nil"。

**正确写法**

```go
package main

type MyError struct{ msg string }

func (e *MyError) Error() string { return e.msg }

func doSomething() error {
    // 直接 return nil，让接口本身为 nil
    return nil
}
```

或者显式声明返回类型为接口：

```go
package main

type MyError struct{ msg string }

func (e *MyError) Error() string { return e.msg }

func doSomething() error {
    var err error // 接口类型，初始 nil
    if false {
        err = &MyError{msg: "fail"}
    }
    return err // 没赋值时 err 仍是 nil 接口
}
```

**判断接口是否"真正"为 nil**

有时拿到一个接口，想判断它的具体值是不是 nil。直接 `== nil` 不够（如上坑）。需要反射：

```go
package main

import (
    "fmt"
    "reflect"
)

func isNil(i any) bool {
    if i == nil {
        return true
    }
    v := reflect.ValueOf(i)
    switch v.Kind() {
    case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
        return v.IsNil()
    }
    return false
}

type MyError struct{ msg string }

func (e *MyError) Error() string { return e.msg }

func main() {
    var e *MyError = nil
    var i any = e
    fmt.Println(i == nil) // false
    fmt.Println(isNil(i)) // true
}
```

但更推荐的做法是**在源头避免**：函数返回接口时，要么返回明确的 nil，要么返回非 nil 接口，不要把"具体类型的 nil"返回给接口。

**Runtime 层面的 nil 检查**

`if err != nil` 编译为对 `iface.tab`（或 eface `_type`）的比较。只要 tab 非 nil，接口就非 nil。这是 O(1) 操作，无开销。

**(3) 工程实践与常见坑**

> 坑 1（最高频）：函数返回 `error` 时，绝不要 `return myTypedNil`，要 `return nil`。

> 坑 2：把 nil 指针/nil slice/nil map 装进接口后，接口非 nil。在测试断言 `assert.Nil(t, err)` 时容易失败，需用 `assert.NoError(t, err)` 或显式 nil 检查。

> 坑 3：`fmt.Println(nilInterface)` 打印 `<nil>`，但 `fmt.Println(typedNilInterface)` 也可能打印 `<nil>`（因为 String() 没定义时）或调用到 nil 指针的方法导致 panic。要警惕。

> 实践：定义返回 error 的函数时，统一用 `return nil` 表达"无错误"，不要把内部的具体 nil 暴露出去。

### 7.9 Interface 性能

**(1) 是什么**

接口提供抽象，但不是零成本。本节分析接口的性能开销与优化。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

开销来源：

1. **间接调用**：接口方法调用要经过 itab.fun 表查地址，是间接跳转（indirect call）。这阻碍 CPU 分支预测与编译器内联。
2. **装箱/逃逸**：具体值赋给接口可能触发堆分配（见 7.7）。
3. **itab 查找**：首次接口赋值要构建 itab（一次性，之后缓存命中）。
4. **缓存未命中**：itab 和 _type 是堆上对象，访问它们可能引起 cache miss。

**间接调用 vs 直接调用**

```go
package main

import "testing"

type Adder interface{ Add(int) int }

type myInt int

func (m *myInt) Add(x int) int { *m += myInt(x); return int(*m) }

// 直接调用
func directAdd(m *myInt, x int) int { return m.Add(x) }

// 接口调用
func ifaceAdd(a Adder, x int) int { return a.Add(x) }

var sink int

func BenchmarkDirect(b *testing.B) {
    m := new(myInt)
    for i := 0; i < b.N; i++ {
        sink = directAdd(m, 1)
    }
}

func BenchmarkIface(b *testing.B) {
    var a Adder = new(myInt)
    for i := 0; i < b.N; i++ {
        sink = ifaceAdd(a, 1)
    }
}
```

通常 BenchmarkDirect 比 BenchmarkIface 快数倍，原因：

- 直接调用编译器能内联 `m.Add`，常量传播后可能优化成一条指令。
- 接口调用走 itab.fun，无法内联（编译期不知具体类型），还有一次间接跳转。

**装箱逃逸示例**

用 `go build -gcflags=-m` 能看到逃逸分析结果。值接收者且大小不超过一个字时逃逸压力较小，指针接收者 `&T{}` 通常会逃逸到堆。

```go
package main

type Box interface{ get() int }

type intBox int

func (i intBox) get() int { return int(i) }

func makeBox(n int) Box {
    return intBox(n) // 装箱：intBox 值塞进 Box 接口
}

func main() {
    for i := 0; i < 1000; i++ {
        _ = makeBox(i)
    }
}
```

**itab 缓存命中后的成本**

接口赋值 `var r io.Reader = f` 在 itab 缓存命中后，成本约：一次哈希查找 + 两次指针写（tab、data）。微秒级以下，绝大多数场景可忽略。方法调用成本主要在间接跳转和无法内联。

**优化策略**

1. **热路径用具体类型**。循环里频繁调用的方法，用具体类型而非接口。
2. **避免在循环里装箱**。把接口赋值移到循环外。接口变量本身不重复装箱，但接口方法参数若为接口会装箱。

```go
package main

type Adder interface{ Add(int) int }

type myInt int

func (m *myInt) Add(x int) int { *m += myInt(x); return int(*m) }

func slow(a Adder, xs []int) int {
    sum := 0
    for _, x := range xs {
        sum += a.Add(x) // a 已是接口，调用本身不装箱；x 是 int 参数也不装箱
    }
    return sum
}
```

3. **用泛型替代接口**。Go 1.18+ 泛型在编译期特化，无 itab、无间接调用：

```go
package main

func addGen[T int | int64](m *T, x T) T { *m += x; return *m }
```

4. **小接口 + 结构体接收者**。1~2 方法的接口编译器有特殊优化（如 sort 包的 interface 调用经过优化）。
5. **避免不必要的接口**。Go 标准库很多 hot path 用具体类型（如 `strings.Builder` 的方法不是接口）。

性能对比速查：

| 操作 | 相对成本 |
|------|----------|
| 直接方法调用（可内联） | 1x |
| 接口方法调用（不可内联） | 3~10x |
| 接口赋值（itab 命中） | ~1ns |
| 接口赋值（itab 未命中，首次构建） | 数百 ns |
| 装箱小值（不逃逸） | ~0 |
| 装箱大值（堆分配） | 取决于 GC |

**(3) 工程实践与常见坑**

> 坑 1：性能测试要用 `benchstat` 对比，单次 benchmark 噪声大。接口开销在微基准里明显，在真实业务（IO 密集）里常被掩盖。

> 坑 2：过早优化是万恶之源。先用接口写清楚，profile 发现热点再换具体类型。

> 实践：API 边界（公开函数）用接口增加灵活性，内部实现用具体类型保性能。

### 7.10 Interface 最佳实践

**(1) 是什么**

综合前几节，本节给出一组工程实践中被广泛验证的接口使用原则。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

**1. Accept interfaces, return structs**

接受接口、返回结构体。这是 Go 社区最著名的箴言。

```go
package main

import "io"

// 推荐
func Process(r io.Reader) *Result {
    // ...
    return &Result{}
}

type Result struct{}
```

为什么：

- 接口参数让函数可接受多种实现（mock、文件、网络），便于测试。
- 返回具体类型让调用方拿到完整能力，且实现可自由演化（增字段、增方法）。
- 返回接口会让"实现想加方法"变成破坏性变更（接口契约限制了它）。

**2. 接口由消费者定义，而非提供者**

标准库的 `io.Reader` 不是 os 包定义的，而是 io 包定义的，os.File 去满足它。这叫"消费者定义接口"。提供者只管把功能做对，消费者按自己需要的最小集定义接口。

```go
package main

import (
    "fmt"
    "io"
)

// 我只需要"读"，就定义/使用 io.Reader，不强制依赖整个 os.File
func countBytes(r io.Reader) (int, error) {
    buf := make([]byte, 1024)
    total := 0
    for {
        n, err := r.Read(buf)
        total += n
        if err == io.EOF {
            break
        }
        if err != nil {
            return 0, err
        }
    }
    return total, nil
}

func main() {
    fmt.Println(countBytes(nil)) // 仅示意，传入真实 Reader 即可
}
```

**3. 接口尽量小（ISP）**

接口隔离原则（Interface Segregation Principle）。1~3 个方法的接口最易复用。`io.Reader` 只一个方法，全标准库数百类型实现它。大接口用组合拼装：

```go
package io

type ReadWriter interface {
    Reader
    Writer
}
```

**(3) 工程实践与常见坑**

**4. 不要为了"有接口"而定义接口**

如果一个接口只有一个实现，且没有测试 mock 需求，多半是过度设计。Go 鼓励"先用具体类型，等第二个实现出现再抽接口"。

> 反模式：`type UserService interface { ... }` + `type userService struct{}` + 一个实现，纯粹是 Java 风格的"接口+实现"映射，在 Go 里多余。

**5. 用接口做边界，做 mock**

跨模块、跨进程边界（如 DB、HTTP client）用接口，便于 mock 测试：

```go
package main

type User struct{}

type UserStore interface {
    Get(id string) (*User, error)
}

type Service struct{ store UserStore }

// 测试时 mock
type fakeStore struct{ users map[string]*User }

func (f *fakeStore) Get(id string) (*User, error) {
    return f.users[id], nil
}
```

**6. 处理 nil 接口的规范**

返回 error 时一律 `return nil`，不要返回具体类型的 nil。详见 7.8。

**7. 接口与并发安全**

接口不保证实现并发安全。文档里要写清楚，或用 `sync.Mutex` 在实现里保护。`io.Reader` 的并发安全由实现负责。

**8. 类型断言优雅降级**

利用接口断言做"能力探测"：

```go
package main

import "io"

func tryClose(r io.Reader) {
    if c, ok := r.(io.Closer); ok {
        _ = c.Close()
    }
    // 不是 Closer 也无所谓
}

func main() {
    // tryClose(someReader)
}
```

`io` 包里大量这种模式（如 `io.NopCloser`）。

**9. 不要把指针放进接口再纠结 nil**

如 7.8 所述，`var r io.Reader = (*MyReader)(nil)` 会让 r 非 nil。要么用值接收者让 nil 值有意义，要么显式管理 nil。

**10. 性能与抽象的平衡**

- 公共 API：接口优先（灵活、可测）。
- 内部 hot path：具体类型优先（快、可内联）。
- 用泛型消除"接口只为类型参数"的场景。

反模式速查：

| 反模式 | 问题 | 改进 |
|--------|------|------|
| 一个接口一个实现且无 mock 需求 | 过度设计 | 删接口，直接用 struct |
| 巨型接口（10+ 方法） | 难实现、难复用 | 拆成小接口组合 |
| 返回接口而非 struct | 限制演化、隐藏能力 | 返回 struct |
| `return typedNil` | nil 接口坑 | `return nil` |
| 在 struct 字段用大接口 | 耦合广 | 用小接口或具体类型 |
| 滥用 `any` | 丢类型安全 | 用泛型或具体类型 |

### 本章小结

- Interface 是 Go 的多态机制，由方法集定义契约，由 (type, value) 二元组承载运行时表示。
- Go 的隐式实现（duck typing 静态版）带来解耦与演化优势，配合小接口设计是 Go 风格的核心。
- 非空接口底层是 `iface{tab *itab, data}`，itab 含接口类型、具体类型、方法函数表；空接口是 `eface{_type, data}`，更简单。
- itab 全局缓存，接口赋值开销主要是查缓存 + 间接调用，首次构建有一次性成本。
- 方法集规则：`T` 只有值接收者方法，`*T` 包含全部，根源是地址性。
- 接口断言与类型转换底层都是 _type/itab 比较 + getitab 查找；Go 1.18 泛型在编译期消除大量接口装箱。
- nil 接口要求 type 和 data 都为 nil；返回具体类型 nil 是经典坑。
- 接口有间接调用与装箱逃逸开销，hot path 优先具体类型/泛型，API 边界用接口。
- 最佳实践：接受接口返回结构体、消费者定义接口、小接口、避免过度抽象、nil 处理规范、能力探测优雅降级。
