## 第9章 反射、unsafe 与 cgo（重点）

> 本章基于 Go 1.26。`reflect` 是受检查的运行时类型操作；`unsafe` 和 cgo 会绕过部分检查，必须把生命周期、指针可见性和版本边界写进设计。

反射、`unsafe` 和 cgo 经常同时出现在序列化、驱动、系统库和高性能框架中，但它们处于不同抽象层：

- `reflect` 保留 Go 类型系统的运行时检查，错误通常表现为 panic。
- `unsafe` 允许重解释地址和布局，错误可能表现为静默内存破坏。
- cgo 跨越 Go 与 C 的运行时边界，还会引入调度、GC 和所有权问题。

### 9.1 `Type` 与 `Value`

`reflect.Type` 描述类型，`reflect.Value` 描述一个带类型的运行时值：

```go
type User struct {
    ID   int    `json:"id"`
    Name string `json:"name"`
}

u := User{ID: 1, Name: "Ada"}
t := reflect.TypeOf(u)
v := reflect.ValueOf(u)

fmt.Println(t.Name(), t.Kind()) // User struct
fmt.Println(v.FieldByName("Name").String())
```

`Kind` 是底层分类，如 `Int`、`Slice`、`Struct`；`Name` 是命名类型的名称。两个不同命名类型可以有相同 Kind。

泛型代码中需要获得 T 的 `reflect.Type` 时，使用 Go 1.22 增加的 `TypeFor`：

```go
func TypeName[T any]() string {
    return reflect.TypeFor[T]().String()
}
```

它比构造 `(*T)(nil)` 再取 `Elem` 更直接。

### 9.2 无效值、零值与 nil

`reflect.Value{}` 是无效值，调用大多数方法会 panic。先检查 `IsValid`：

```go
value := reflect.ValueOf(nil)
fmt.Println(value.IsValid()) // false
```

对可为 nil 的 Kind 才能调用 `IsNil`：

```go
func IsNil(x any) bool {
    if x == nil {
        return true
    }
    v := reflect.ValueOf(x)
    switch v.Kind() {
    case reflect.Chan, reflect.Func, reflect.Interface,
        reflect.Map, reflect.Pointer, reflect.Slice:
        return v.IsNil()
    default:
        return false
    }
}
```

`IsZero` 判断是否为该类型的零值，但不要用它替代业务上的“未设置”语义。`0`、空字符串和 false 可能是有效业务值。

### 9.3 可寻址与可设置

反射只能修改可寻址且可设置的值。把结构体值传给 `ValueOf` 得到的是副本：

```go
u := User{Name: "before"}

v := reflect.ValueOf(&u).Elem()
field := v.FieldByName("Name")
if field.CanSet() {
    field.SetString("after")
}
```

修改前至少检查：

- `IsValid`：查找是否成功。
- `CanSet`：值是否可设置。
- `Type().AssignableTo` 或 `ConvertibleTo`：传入类型是否合法。

不要用 `unsafe` 强行修改未导出字段。这会破坏包封装，并可能在版本升级后直接失效。

### 9.4 方法、标签与缓存

结构体标签是类型元数据，常用于编码和校验：

```go
field, ok := reflect.TypeFor[User]().FieldByName("Name")
if ok {
    name, options := field.Tag.Lookup("json")
    fmt.Println(name, options)
}
```

反射扫描类型有可见成本。序列化器通常按 `reflect.Type` 缓存字段计划，而不是每个对象重复遍历：

```go
var plans sync.Map // map[reflect.Type]*fieldPlan
```

缓存值必须不可变或自行同步。`reflect.Type` 可比较，适合作为 map key。

Go 1.23 为 `Value` 增加 `Seq`/`Seq2`，Go 1.26 为结构字段和方法增加迭代 API。它们减少索引样板，但不会消除反射检查成本。

### 9.5 什么时候不用反射

优先级通常是：

1. 具体类型和普通函数。
2. 小 interface。
3. 泛型。
4. 代码生成。
5. 反射。

反射适合类型在运行时才知道的边界，如通用编码器、依赖注入和 ORM 元数据。业务核心路径若类型集合在编译期已知，泛型或生成代码更容易审计和优化。

### 9.6 `unsafe` 的核心规则

`unsafe` 不是“关闭 GC”，而是允许表达编译器无法证明安全的转换。常用 API 包括：

- `Sizeof`、`Alignof`、`Offsetof`
- `Add`
- `Slice`、`SliceData`
- `String`、`StringData`
- `Pointer`

零拷贝 byte/string 转换必须同时满足不可变和生命周期要求：

```go
func BytesToReadOnlyString(data []byte) string {
    if len(data) == 0 {
        return ""
    }
    return unsafe.String(unsafe.SliceData(data), len(data))
}
```

返回后只要 string 仍存活，底层数组会被 GC 追踪；但调用方绝不能再修改 `data`。如果无法建立这一所有权约束，就使用安全的 `string(data)` 拷贝。

反向把 string 暴露为可写 `[]byte` 是错误的，因为字符串可能位于只读内存。

### 9.7 `uintptr` 不是指针

`uintptr` 只是整数，GC 不把它当作对象引用。不要跨语句、函数调用或阻塞点保存 Go 地址：

```go
// 错误：GC 不知道 addr 仍引用 object。
addr := uintptr(unsafe.Pointer(&object))
runtime.GC()
ptr := (*T)(unsafe.Pointer(addr))
```

地址运算尽量放在一个表达式中，优先使用 `unsafe.Add`。调用系统 API 后如需确保对象活到该点，使用 `runtime.KeepAlive(object)`；它只延长生命周期，不负责固定地址。

用以下检查发现部分非法指针转换：

```bash
go test -gcflags=all=-d=checkptr=2 ./...
go vet ./...
```

`checkptr` 不能证明 unsafe 代码正确，它只是最低限度的动态检查。

### 9.8 cgo 调用与调度成本

cgo 调用比普通 Go 调用昂贵，并涉及线程状态切换。长时间 C 调用不会占住 P，但可能持续占用 OS 线程；高频短调用则容易被边界成本主导。工程上应优先批量化，而不是逐元素跨边界。

所有权必须明确：

| 内存来源 | 谁释放 | 常见规则 |
|---|---|---|
| `C.malloc` | C 或显式 `C.free` | Go GC 不管理 |
| Go 对象，C 仅在调用期间访问 | Go | 调用返回后 C 不保留指针 |
| Go 对象，C 需跨调用保存 | Go + 显式 pin/handle | 受严格指针规则限制 |

### 9.9 Go 指针传给 C

默认规则是：C 只能在 cgo 调用期间临时使用指向 Go 内存的指针，调用返回后不能保留。并且 C 可见的 Go 内存不能包含未固定的 Go 指针。

需要跨调用标识 Go 值时，优先使用 `runtime/cgo.Handle`，把整数 handle 交给 C：

```go
h := cgo.NewHandle(callback)
defer h.Delete()

// 把 uintptr(h) 传给 C；回调时再由 Go 取回 h.Value()。
```

确实需要 C 保留某块 Go 内存地址时可使用 `runtime.Pinner`：

```go
var pinner runtime.Pinner
if len(buffer) != 0 {
    ptr := unsafe.SliceData(buffer)
    pinner.Pin(ptr)
    registerWithC(unsafe.Pointer(ptr), len(buffer))

    // C 确认不再保存或访问 ptr 后，才能解除固定。
    unregisterFromC(unsafe.Pointer(ptr))
    runtime.KeepAlive(buffer)
    pinner.Unpin()
}
```

长度检查既避免 `&buffer[0]` 对空 slice panic，也避免尝试固定无实际元素的地址。实际代码应把 `Pinner`、buffer 和 C 侧注册句柄封装进同一个有明确 `Close` 生命周期的对象，并防止重复释放。

`Pinner` 不是通行证：被固定区域若包含 Go 指针，相关对象也必须满足固定规则；slice、string、interface 本身包含指针，不能把它们的描述符随意交给 C 长期保存。优先考虑 C 分配内存或 handle。

### 9.10 审查清单

- 反射入口是否检查 `IsValid`、Kind、`CanSet` 和类型兼容性。
- 是否缓存了反射元数据，而不是在热路径反复扫描。
- unsafe 转换是否写清底层内存所有者、可变性和有效期。
- 是否把 `uintptr` 当成长期引用，或缺少必要的 `KeepAlive`。
- cgo 是否逐元素高频调用，能否批量化。
- C 是否在调用返回后保存 Go 指针。
- `cgo.Handle` 是否 `Delete`，C 内存是否 `free`，Pinner 是否 `Unpin`。
- 是否在 `-race`、`checkptr=2`、目标架构和真实 GC 压力下测试。

### 本章小结

- `reflect.Type` 描述类型，`reflect.Value` 操作运行时值；可设置性来自可寻址和导出规则。
- 泛型或生成代码能解决的问题，不必用反射推迟到运行时。
- unsafe 的正确性依赖对象布局、所有权和生命周期，`uintptr` 不会维持对象存活。
- cgo 的主要风险是边界成本、内存所有权和 C 保存 Go 指针，handle 通常比裸指针更稳妥。

进一步阅读：

- [The Laws of Reflection](https://go.dev/blog/laws-of-reflection)
- [Package unsafe](https://pkg.go.dev/unsafe)
- [cgo command documentation](https://pkg.go.dev/cmd/cgo)
- [Passing pointers](https://go.dev/wiki/cgo#passing-pointers)
