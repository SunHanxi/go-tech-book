## 第3章 Slice（重点）

> 切片是 Go 中最常用的容器，也是一个最容易踩坑的容器：它既不是数组，也不是引用，而是一个"指针 + 长度 + 容量"的值类型 header。

### 3.1 为什么需要 Slice

**(1) 是什么**

切片（Slice）是 Go 中**变长**序列的抽象，类型签名 `[]T`。它内部不直接持有数据，而是引用一段底层数组，并通过 `len` 和 `cap` 描述可读范围和容量。

```go
package main

import "fmt"

func main() {
    s := []int{10, 20, 30}
    s = append(s, 40)
    fmt.Println(s, len(s), cap(s)) // [10 20 30 40] 4 (8 或更大)
}
```

**(2) 为什么需要它**

数组 `[N]T` 的长度属于类型，无法表达"长度运行期才知道"的容器。Go 需要：

- 一个**类型与长度解耦**的容器：`[]int` 可以容纳任意长度的 int；
- 一个**廉价传参**的容器：传 header 而非整段数据；
- 一个**可扩容**的容器：`append` 能在容量不足时重新分配。

切片把这三件事一并解决。它本质是"数组的一段视图 + 一个容量字段"，既保留了数组连续内存的高效，又提供了动态长度的灵活性。

**(3) 工程实践与常见坑**

- 切片是 Go 中"动态数组"的事实标准，业务代码优先用切片。
- 但切片不是万能：它带来共享底层数组、扩容拷贝、内存泄漏等坑，本章后续逐一拆解。
- 知道容量上限时优先 `make([]T, 0, n)` 预分配，避免多次扩容。

### 3.2 Slice 与 Array 的关系

**(1) 是什么**

切片**不是数组的语法糖**，但它**总有一段数组在背后**。这段数组叫做"底层数组"（backing array）。可以理解为：

```
slice = header{array *T, len int, cap int}  →  backing array [cap]T
```

切片可以从数组、数组指针、或另一个切片"切"出来：

```go
package main

import "fmt"

func main() {
    a := [5]int{1, 2, 3, 4, 5}
    s1 := a[1:4]                       // 从数组切：len=3, cap=4
    s2 := s1[:2]                       // 从切片再切：len=2, cap=4
    fmt.Println(s1, s2)                // [2 3 4] [2 3]
    fmt.Println(len(s1), cap(s1))      // 3 4
    fmt.Println(len(s2), cap(s2))      // 2 4
}
```

**(2) 底层关系**

- 切片表达式 `a[low:high]` 产生一个新 header，其中 `array = &a[low]`，`len = high-low`，`cap = len(a)-low`。
- 切片本身只是 24 字节（64 位）的 header，**不持有数据**；数据由底层数组持有。
- 多个切片可以共享同一段底层数组（见 3.8）。

**(3) 工程实践与常见坑**

- 从数组切出的切片指向原数组，只要切片活着，原数组就无法被 GC（即使你只用了 1 个元素）。
- 切片表达式可加第三个参数 `a[low:high:max]` 显式控制 cap，用于"截断共享"：

  ```go
  s := a[1:3:3] // len=2, cap=2，禁止向右扩展，append 会重新分配
  ```

- 数组指针 `(*[5]int)(&arr)` 也可切片，常用于避免数组值拷贝。

### 3.3 Slice Header（ptr、len、cap）

**(1) 是什么**

切片在内存中就是一个三字段结构体，称为 Slice Header。在 Go 1.21+ 的 runtime 源码里（`runtime/slice.go`），切片的内部表示是：

```go
// runtime/slice.go
type slice struct {
    array unsafe.Pointer // 指向底层数组第一个元素
    len   int            // 当前长度（可见元素数）
    cap   int            // 容量（底层数组从 array 起到末尾的元素数）
}
```

对外（`reflect` 包）等价表示为：

```go
// reflect/value.go（外部可见版本，仅用于理解）
type SliceHeader struct {
    Data uintptr
    Len  int
    Cap  int
}
```

可以验证其大小：

```go
package main

import (
    "fmt"
    "unsafe"
)

func main() {
    var s []int
    fmt.Println(unsafe.Sizeof(s)) // 24（64 位），12（32 位）
}
```

**(2) 字段逐个解释**

| 字段 | 含义 | 关键约束 |
|---|---|---|
| `array` | 底层数组首元素指针 | nil 切片时为 `nil`；空切片时通常指向 `runtime.zerobase` |
| `len`   | 当前可读可写的元素数 | `s[i]` 合法当且仅当 `0 <= i < len`；`s[len]` panic |
| `cap`   | 从 `array` 起算的可用元素数 | `len <= cap` 恒成立；`len < cap` 时 append 原地写 |

**(3) 三者关系图**

```
底层数组:  [ _ | _ | _ | _ | _ | _ | _ | _ ]   (cap=8)
            ↑
slice.array |
slice:     [array, len=3, cap=8]
                 ↑     ↑
             可读范围  可写但未写（append 先用这里）
```

> 切片的"三个数字"决定了它的一切行为：寻址、边界检查、扩容、共享、复制。把它们印在脑子里，切片的坑就少了一半。

### 3.4 编译器如何创建 Slice

**(1) 是什么**

切片字面量、`make`、切片表达式在编译期会被 lowering 成不同的 runtime 调用或内联指令。

**(2) 三种创建方式的底层实现**

**a) 字面量 `[]int{1,2,3}`**

编译器先在只读数据段（如可能）或栈上生成一个临时数组，再用 `&arr[0]` 构造 header：

```go
// []int{1, 2, 3} 等价伪代码
var arr = [3]int{1, 2, 3}
s := slice{array: &arr[0], len: 3, cap: 3}
```

若数组逃逸到堆，则等价于 `runtime.makeslice` + 元素写入。

**b) `make([]T, len, cap)`**

直接调用 `runtime.makeslice`（或 `makeslicecopy`）：

```go
// runtime/slice.go（Go 1.21，简化）
func makeslice(et *_type, len, cap int) unsafe.Pointer {
    mem, overflow := math.MulUintptr(et.size, uintptr(cap))
    if overflow || mem > maxAlloc || len < 0 || len > cap {
        // 二次校验 len，区分 len / cap 越界两种 panic
        mem, overflow := math.MulUintptr(et.size, uintptr(len))
        if overflow || mem > maxAlloc || len < 0 {
            panicmakeslicelen()
        }
        panicmakeslicecap()
    }
    return mallocgc(mem, et, true) // true = needzero，分配并清零
}
```

逐行解释：
- `MulUintptr` 计算 `cap * sizeof(T)`，同时通过高位非零检测溢出。
- 若 `mem` 超过单次最大分配 `maxAlloc`，或 `len` 越界，panic（区分 `len` 还是 `cap` 出问题，便于定位）。
- `mallocgc(mem, et, true)` 分配并清零内存，`et` 携带 GC 需要的指针 bitmap。

**c) 切片表达式 `a[low:high]` / `s[low:high]`**

编译器生成 header 构造代码，**不调用 runtime**：

```go
// s[low:high] 等价伪代码（带运行时边界检查）
if low < 0 || high > cap(s) || low > high {
    panicSlice()
}
newSlice := slice{
    array: s.array + low*sizeof(T),
    len:   high - low,
    cap:   cap(s) - low,
}
```

**(3) 工程实践与常见坑**

- `make([]T, 0, 1024)` 预分配容量可避免多次扩容拷贝，是热点路径优化的常见手段。
- 字面量创建的切片在元素数量大时会逃逸到堆。
- 切片表达式第三参数 `a[i:j:k]` 用于"限制 cap = k-i"，是切断共享的关键技巧。
- `make([]T, n)` 等价于 `make([]T, n, n)`，已分配 n 个零值元素。

### 3.5 nil Slice

**(1) 是什么**

`var s []int` 声明但未初始化的切片就是 nil 切片。其 header 三个字段全为零：

```go
type slice struct {
    array unsafe.Pointer // nil
    len   int            // 0
    cap   int            // 0
}
```

```go
package main

import "fmt"

func main() {
    var s []int
    fmt.Println(s == nil)      // true
    fmt.Println(len(s), cap(s)) // 0 0
    s = append(s, 1)           // append 对 nil 切片安全
    fmt.Println(s)             // [1]
}
```

**(2) 为什么这样设计**

- nil 切片代表"什么都没有"，是零值语义的自然延伸。
- `append`、`len`、`cap`、`range`、`copy` 对 nil 切片都安全工作，避免大量 `if s == nil` 判空。
- JSON 序列化时 nil 切片编码为 `null`（区分于空切片的 `[]`），便于表达"未提供" vs "空集"。

**(3) 工程实践与常见坑**

- **JSON 坑**：API 返回 `var s []int` 序列化为 `null`；想返回 `[]` 应显式 `s := []int{}`。
- **reflect 坑**：`reflect.ValueOf(s).IsNil()` 对 nil 切片返回 true，对空切片 panic（空切片非 nil）；调用前先判断 `Kind() == Slice`。
- **迭代坑**：`for range nilSlice` 不执行循环体，安全；无需额外判空。

### 3.6 Empty Slice

**(1) 是什么**

空切片是长度为 0 但**指针非 nil** 的切片，通常指向 `runtime.zerobase`：

```go
package main

import "fmt"

func main() {
    s1 := []int{}         // 空切片
    s2 := make([]int, 0)  // 空切片
    var s3 []int          // nil 切片
    fmt.Println(s1 == nil, s2 == nil, s3 == nil) // false false true
    fmt.Println(len(s1), len(s2), len(s3))        // 0 0 0
}
```

**(2) 底层实现**

- `[]int{}`：编译器把 `array` 指向 `runtime.zerobase`（一个全局的零字节对象），`len=cap=0`。
- `make([]int, 0)`：`mallocgc(0, ...)` 也返回 `zerobase`，因此同样指向 zerobase。
- 所以 nil 切片和空切片在 `len/cap` 上等价，区别仅在 `array` 指针是否为 nil。

**(3) 工程实践与常见坑**

| 行为 | nil 切片 | 空切片 |
|---|---|---|
| `s == nil` | true | false |
| `len(s)` / `cap(s)` | 0 / 0 | 0 / 0 |
| `append(s, x)` | 安全 | 安全 |
| JSON Marshal | `null` | `[]` |
| `fmt.Println(s)` | `[]` | `[]` |
| 底层指针 | `nil` | `runtime.zerobase` |

> 写 API 时如果想保证 JSON 返回 `[]` 而不是 `null`，用 `[]int{}`；如果"无数据"语义上代表"未提供"，用 nil。两者在内部逻辑里几乎可互换，但在序列化、反射、与外部系统交互时差异明显。

### 3.7 Slice 的底层数组

**(1) 是什么**

每个非空切片都有一段底层数组支撑它。底层数组可能：

- 是某个显式声明的 `[N]T` 数组；
- 是 `makeslice` 在堆上分配的一段连续内存；
- 是另一个切片的底层数组（共享）。

切片本身只是 header，GC 通过 header.array 追踪底层数组的存活。

**(2) Runtime 视角**

- `makeslice` 调用 `mallocgc` 分配 `cap*sizeof(T)` 字节，得到 `array` 指针。
- 这段内存像普通 Go 对象一样有 bitmap 标记指针位（若 T 含指针），供 GC 扫描；若 T 不含指针（如 `[]byte`），则不增加 GC 扫描负担。
- 切片赋值/传参只复制 24 字节 header，底层数组不动，因此廉价。

**(3) 工程实践与常见坑**

- 切片赋值 `b := a` 后，`a` 和 `b` 共享底层数组，`b[0] = X` 会改变 `a[0]`。
- 切片传参后，函数内 `s[i] = X` 对调用方可见（共享底层数组）。
- 但 `append` 可能让切片指向新的底层数组，调用方不可见（见 3.9、3.12）。
- 切片越界写 `s[len] = X` 会触发 panic；但 `s[len:cap]` 范围内的内存"属于"底层数组，可以通过 `s = s[:cap]` 重新启用。

### 3.8 Slice 的共享机制

**(1) 是什么**

两个切片共享底层数组时，对元素的修改互相可见。常见来源：

- 切片赋值：`b := a`
- 切片表达式：`b := a[1:3]`
- 多次切片同一个数组

```go
package main

import "fmt"

func main() {
    a := []int{1, 2, 3, 4, 5}
    b := a[1:3]
    b[0] = 99
    fmt.Println(a) // [1 99 3 4 5]   a 也变了
}
```

**(2) 为什么这样设计**

- 共享是切片"廉价"的代价。如果不共享，每次切片都要拷贝整段数组，等同于数组。
- 共享也带来能力：可以零拷贝地"开窗口"看大数组（如 `bytes.Reader`、`strings.Reader`、网络 buffer 的零拷贝解析）。

**(3) 工程实践与常见坑**

- **隐式共享坑**：`b := a` 后改 `b` 影响 `a`，初学者常踩。
- **append 截断共享**：`b := append([]T(nil), a...)` 是传统的"深拷贝切片"写法；Go 1.21+ 应直接用 `slices.Clone(a)`。
- **跨协程共享**：多个 goroutine 同时读写同一段底层数组是 data race，需要同步（mutex 或 channel）。
- **三参数切片** `a[i:j:k]` 限制 cap = k-i，防止 append 越界写入共享区域。
- 切片传给 `sort.Slice` / `sort.Ints` 是就地排序，原切片顺序会被改。

### 3.9 Slice 为什么不是引用

**(1) 是什么**

切片常被误称为"引用类型"，但严格说它是一个**值类型 header**，只是 header 里有指针。赋值/传参复制 header，不复制底层数组。

```go
package main

import "fmt"

func grow(s []int) {
    s = append(s, 100) // cap 不足时分配新数组，s 指向新数组
    fmt.Println("in grow:", s)
}

func main() {
    a := make([]int, 2, 2) // len=2, cap=2
    grow(a)
    fmt.Println("after grow:", a) // a 不变，因为 grow 内的 s 是副本
}
```

**(2) 为什么这样设计**

- Go 没有"引用类型"这个概念，只有"值类型"和"指针"。
- 切片 header 是 struct，按值传递；header 里的 array 是指针，所以底层数组共享。
- 这等价于 C 里传 `struct { int *p; int len, cap; }`：struct 被拷贝，但 `p` 指向同一块内存。

| 操作 | 调用方可见？ | 原因 |
|---|---|---|
| `s[i] = X` | 可见 | 共享底层数组 |
| `s = append(s, x)`（cap 足够） | 不可见 | header.len 改了但调用方 header 是副本 |
| `s = append(s, x)`（cap 不足） | 不可见 | header.array 换了，调用方看不到 |
| `s = nil` | 不可见 | 改的是副本 |

**(3) 工程实践与常见坑**

- 想让函数修改切片的 `len`/`cap`/`array`（如 append 后让调用方看到），必须返回新切片或传 `*[]T`。
- 修改元素 ≠ 修改 header：前者通过共享指针可见，后者不可见。
- 这就是为什么标准库里 `append` 返回新切片，而 `sort.Ints` 直接就地排序不返回。
- 传 `*[]T` 会失去 ABI 寄存器传参优化，**仅在需要让函数扩容时才用**；普通扩容请返回新切片。

### 3.10 Slice 为什么不能 ==

**(1) 是什么**

切片之间不能用 `==` 比较（只能与 nil 比）：

```go
package main

func main() {
    a := []int{1, 2}
    b := []int{1, 2}
    // _ = a == b // 编译错误：invalid operation: a == b (slice can only be compared to nil)
    _ = a == nil // 合法
    _ = a
    _ = b
}
```

**(2) 为什么这样设计**

Go 规范明确禁止切片比较，原因有三：

1. **语义模糊**：是"指向同一数组"还是"元素逐个相等"？两种语义都常见，没法默认。
2. **深层比较的歧义**：若按元素比，遇到 `[][]int` 这种含切片元素的切片又得递归，且循环引用无法处理。
3. **不可哈希**：map key 要求可哈希，但切片的底层数组可变，哈希值会随之改变，无法做一致性保证。

只允许 `s == nil` 是为了零值检测，这是明确的、无歧义的。

**(3) 工程实践与常见坑**

- 比较是否同底层数组：`&a[0] == &b[0]`（要确保 len>0）。
- 比较元素是否相等：用 `slices.Equal`（见 3.11）。
- 想把"切片内容"做 map key：先转字符串（`string(b)` 对 `[]byte` 合法且拷贝）或哈希后再用。
- `[]byte` 可以与 `string` 比较：`string(b) == "abc"`，因为这是语言内建的特殊规则（隐式转换）。

### 3.11 slices.Equal

**(1) 是什么**

Go 1.21 标准库 `slices` 包提供了泛型的元素相等比较：

```go
// slices/slices.go
func Equal[S ~[]E, E comparable](s1, s2 S) bool {
    if len(s1) != len(s2) {
        return false
    }
    for i := range s1 {
        if s1[i] != s2[i] {
            return false
        }
    }
    return true
}
```

逐行解释：
- 泛型约束 `S ~[]E` 接受任意切片类型（包括基于切片的自定义类型，如 `type Ints []int`）。
- `E comparable` 要求元素可比较（支持 `==`），编译期保证。
- 长度不同直接 false；否则逐元素 `!=`，遇到不等即返回 false。

```go
package main

import (
    "fmt"
    "slices"
)

func main() {
    a := []int{1, 2, 3}
    b := []int{1, 2, 3}
    c := []int{1, 2}
    fmt.Println(slices.Equal(a, b)) // true
    fmt.Println(slices.Equal(a, c)) // false
}
```

**(2) 为什么这样设计**

- 标准库提供明确语义的"逐元素相等"，避免每个项目自己写循环。
- `comparable` 约束让编译器在编译期保证元素可比，规避了"切片元素含切片"的递归问题（含切片的类型不是 comparable）。
- O(N) 复杂度，明确写在文档里，避免被误用为哈希。

**(3) 工程实践与常见坑**

- **NaN 坑**：`slices.Equal([]float64{math.NaN()}, []float64{math.NaN()})` 返回 false，因为 `NaN != NaN`。需要自定义相等用 `slices.EqualFunc`。
- **性能**：对 `[]byte` 用 `bytes.Equal` 比 `slices.Equal` 更快（专门优化为 mempcmp，且能利用 SIMD）。
- 配套函数：`slices.EqualFunc`（自定义比较）、`slices.Compare`（有序比较，返回 -1/0/1）、`slices.Clone`（深拷贝）、`slices.Contains`（成员检测）。

### 3.12 Slice 作为函数参数

**(1) 是什么**

传切片 = 传 24 字节 header 的副本。函数内对元素的修改对调用方可见，对 len/cap/array 的修改不可见。

```go
package main

import "fmt"

func setFirst(s []int) { s[0] = 100 }        // 可见：共享底层数组
func appendOne(s []int) { s = append(s, 1) } // 不可见：改的是 header 副本

func main() {
    a := []int{1, 2, 3}
    setFirst(a)
    fmt.Println(a) // [100 2 3]
    appendOne(a)
    fmt.Println(a) // [100 2 3]  没变
}
```

**(2) 底层原理**

- 24 字节 header 通过寄存器或栈传递，几乎免费（Go ABI 中切片 header 可拆成 3 个寄存器参数）。
- header 里的 array 指针指向调用方的底层数组，所以 `s[i] = X` 改的是同一段内存。
- `append` 可能分配新数组并改 header.array，但函数内的 header 是副本，调用方看不到。

**(3) 工程实践与常见坑**

- **append 必须返回**：标准写法 `s = append(s, x)`。
- **想让函数扩容**：返回新切片 `func grow(s []int) []int`，或传指针 `func grow(s *[]int)`。
- **大切片传参不要先取指针**：`*[]T` 反而失去 register 传参优化，直接传 `[]T` 即可。
- **只读切片参数**：可以加文档注释"不修改 s"，但语言不强制；想强制可用 `slices.Clone` 复制后传，或传 `s` 的子串 `s[:len:len]` 限制 cap。
- **接口转换**：切片赋值给 `any` 会触发逃逸到堆，热路径避免。

### 3.13 Slice 的生命周期

**(1) 是什么**

切片 header 是值，但其底层数组是堆对象（一般情况）。底层数组的存活由"所有引用它的 header"共同决定，只要任一 header 还在用，数组就活。

**(2) 生命周期阶段**

1. **创建**：`make` / 字面量 / 切片表达式 → 分配或复用底层数组。
2. **使用**：`s[i]`、`range`、传参、append（cap 足够时原地写，不足时换数组）。
3. **扩容**：append 触发 `growslice`，分配新数组 + 拷贝 + 更新 header.array。
4. **死亡**：所有引用 header 都不可达时，底层数组被 GC 回收。

**(3) growslice 简化伪代码（Go 1.21）**

```go
// runtime/slice.go（简化）
func growslice(oldPtr unsafe.Pointer, oldLen, oldCap int, num int, et *_type) slice {
    newCap := nextslicecap(oldCap, num) // 计算新容量
    // ... overflow 校验
    var p unsafe.Pointer
    if et.ptrdata == 0 {
        p = mallocgc(capmem, nil, false) // 无指针类型，不需要 bitmap
    } else {
        p = mallocgc(capmem, et, true)
        // 写入屏障：覆盖旧元素区，确保 GC 正确
    }
    memmove(p, oldPtr, lenmem)           // 拷贝旧元素
    return slice{p, oldLen + num, newCap}
}
```

扩容规则（Go 1.18+，threshold 从 1024 调整为 256）：

- 若新需要的 cap > 旧 cap × 2，直接用新 cap；
- 否则若旧 cap < 256，翻倍；
- 否则按 `newcap += (newcap + 3*256) / 4` 增长，渐近 1.25 倍；
- 最后根据 `sizeof(T)` 和内存对齐做圆整，得到实际分配大小。

| 旧 cap | 翻倍后 | 实际新 cap（近似） |
|---|---|---|
| 1 | 2 | 2 |
| 100 | 200 | 200 |
| 256 | 512 | 512 |
| 1000 | — | ~1250 |

> 扩容是切片性能的关键：知道大概容量时一定要 `make([]T, 0, n)` 预分配，避免 N 次 growslice 的 O(N²) 拷贝。

### 3.14 Slice 导致的内存泄漏

切片是 Go 内存泄漏的高发地带，根因都是"小切片引用了大数组"。

**(1) 经典场景一：子切片保活**

```go
package main

import "fmt"

func bigData() []byte {
    b := make([]byte, 1<<30) // 1 GiB
    // ... 填充
    return b[:10]            // 只返回前 10 字节，但 1 GiB 全活
}

func main() {
    s := bigData()
    fmt.Println(len(s)) // 10，但底层 1 GiB 不会被 GC
}
```

修复：`return bytes.Clone(b[:10])` 或 `slices.Clone(b[:10])`，强制重新分配一段 10 字节的独立数组。

**(2) 经典场景二：append 不释放旧数组**

```go
b := make([]byte, 1<<20) // 1 MiB
// 只想保留前 10 字节并继续 append
b = append(b[:0:0], b[:10]...) // cap=0 强制重新分配，避免共享旧 1 MiB
```

`b[:0:0]` 是"截断到 0 容量"的经典技巧，append 必然分配新数组。

**(3) 经典场景三：切片作为 map value 长期持有**

```go
cache := map[string][]byte{}
cache["k"] = resp.Body // resp.Body 是大缓冲，cache 长期持有整段
```

修复：`cache["k"] = bytes.Clone(resp.Body)`，只缓存真正需要的部分。

**(4) 经典场景四：字符串与切片的 unsafe 转换**

`[]byte(s)` 会拷贝，但 `unsafe.String` / `unsafe.Slice` 系列会共享底层，要小心：

```go
// 危险：s 的底层字符串只要活着，b 就能用；反过来 b 改了会影响 s
b := unsafe.Slice(unsafe.StringData(s), len(s))
```

**(5) 经典场景五：回调闭包捕获切片**

```go
func handler(resp []byte) func() {
    head := resp[:8] // 闭包捕获 head，整个 resp 底层数组保活
    return func() { use(head) }
}
```

修复：闭包里只捕获必要的小拷贝。

> 经验法则：**当切片来自外部、且你只用到一小段，就 `Clone` 一份**。Go 1.21 提供了 `slices.Clone` 和 `bytes.Clone`，没有理由不用。

### 3.15 常见坑总结

| 坑 | 现象 | 根因 | 修复 |
|---|---|---|---|
| 共享底层数组 | `b := a; b[0]=X` 改了 a | header 复制不复制数据 | `slices.Clone(a)` |
| append 不可见 | 函数内 append 调用方看不到 | header 按值传 | 返回新切片或传 `*[]T` |
| 子切片泄漏 | `b[:10]` 保活 1 GiB | 共享底层数组 | `Clone` 截断 |
| nil vs 空 JSON | `null` vs `[]` | nil/empty 指针不同 | 显式 `[]T{}` |
| 扩容拷贝 | append 后旧数组残留 | growslice 换数组 | 预分配 cap |
| for range 改值 | `for _, v := range s` 改 v 无效 | v 是副本 | 用索引 `s[i]` |
| 三参数切片误用 | `a[:2]` cap 仍是大数 | cap 默认到末尾 | `a[:2:2]` 截断 |
| 切片不能比较 | `a == b` 编译错 | 语言禁止 | `slices.Equal` |
| 跨协程竞态 | 多协程写同切片 | 共享底层数组 | 加锁或 channel |
| NaN 不等 | `slices.Equal([NaN],[NaN])` false | `NaN != NaN` | `slices.EqualFunc` |
| 接口装箱逃逸 | 切片转 `any` 触发堆分配 | 接口要求可寻址 | 热路径避免装箱 |

```go
package main

import (
    "fmt"
    "slices"
)

func main() {
    // 正确的"独立副本"
    a := []int{1, 2, 3}
    b := slices.Clone(a)
    b[0] = 99
    fmt.Println(a, b) // [1 2 3] [99 2 3]
}
```

> 切片的全部坑，几乎都源自"header 是值、array 是指针"这一对矛盾。把这句话刻在脑子里，再回头看上表，每个坑都能自己推导出来。

### 本章小结

- 切片 = `{array, len, cap}` 三字段 header，是一个值类型，但内部持有底层数组指针。
- 创建路径有字面量、`make`（makeslice）、切片表达式三种，编译器分别 lowering。
- nil 切片与空切片在 len/cap 上等价，但 `array` 指针不同，JSON 序列化结果不同。
- 切片共享底层数组带来高效，也带来共享修改、内存泄漏、跨协程竞态三大坑。
- `append` 可能换数组，所以"传参修改切片"必须返回或传指针。
- 切片不能 `==`，用 `slices.Equal`（Go 1.21+）做元素相等比较。
- 理解切片的关键模型：**header 在栈上、array 在堆上、多个 header 可指向同一 array**。
