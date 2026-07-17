## 第4章 append()

> 本章深入剖析 Go 中 `append` 内置函数的语义、Runtime 扩容实现 `growslice` 的算法、Slice 的所有权与共享坑，以及 GC 对旧底层数组的处理，帮助你在工程中写出高性能且不踩坑的 Slice 代码。

### 4.1 append 到底做了什么

**是什么**

`append` 是 Go 的内置函数，用于向 Slice 追加元素，其函数签名如下：

```go
// The append built-in function appends elements to the end of a slice.
// If it has sufficient capacity, the destination is resliced to accommodate the
// new elements. If it does not, a new underlying array will be allocated.
func append(slice []Type, elems ...Type) []Type
```

它接受一个 Slice（底层数组首元素指针、长度 `len` 与容量 `cap`）和若干待追加元素，返回一个新的 Slice。其语义可以用下面这段简化伪代码描述：

```go
func append(slice []Type, elems ...Type) []Type {
    newLen := len(slice) + len(elems)
    if newLen <= cap(slice) {
        // 容量够：原地写
        copy(slice[len(slice):cap(slice)], elems)
        return slice[:newLen]
    }
    // 容量不够：分配新数组 + 拷贝旧元素 + 写入新元素
    newSlice := growslice(...)
    copy(newSlice[len(slice):], elems)
    return newSlice
}
```

**为什么这样设计 / 底层实现要点**

要理解 `append` 的行为，必须回到 Slice 的运行时表示。Go Runtime 中 Slice 的真实结构定义在 `runtime/slice.go`：

```go
type slice struct {
    array unsafe.Pointer // 指向底层数组首元素
    len   int            // 当前长度
    cap   int            // 当前容量
}
```

逐字段解释：

- `array`：是一个 `unsafe.Pointer`，指向底层数组第一个元素的地址。Slice 的所有读写操作最终都通过它定位内存。
- `len`：表示当前 Slice "可见"的元素个数，`len()` 内置函数直接读这个字段。
- `cap`：表示底层数组从 `array` 开始能容纳的元素总数。`cap()` 内置函数直接读这个字段。

Slice 头本身是一个 **值类型**（结构体），赋值和函数传参都会复制这三字段；但它内部的 `array` 指针让多个 Slice 可以共享同一个底层数组。这种"值类型头 + 指针共享底层数组"的设计，是 `append` 一切坑的根源。

编译器对 `append` 有特殊处理：它不是普通的函数调用，而是会被编译成内联的若干条机器指令 + 必要时调用 `runtime.growslice`。当 `len + n <= cap` 时，根本不会进入 `growslice`，直接在原数组上写入并返回一个新的 Slice 头（`array` 指针相同、`len` 增大、`cap` 不变）。

**工程实践与常见坑**

最经典的坑：**底层数组共享**。

```go
package main

import "fmt"

func main() {
    a := make([]int, 1, 2) // len=1, cap=2
    b := append(a, 10)     // 容量够，b.array == a.array
    b[0] = 99              // a[0] 也变成 99！
    fmt.Println(a[0], b[0])  // 99 99
    fmt.Println(len(a), len(b)) // 1 2
}
```

因为 `append` 在容量足够时不会重新分配底层数组，`a` 和 `b` 共享同一块内存，`b[0] = 99` 同时修改了 `a[0]`。这就是为什么在 [第3章 Slice](./03-Slice.md) 中强调：**任何时候通过 `append`、`reslice` 产生的新 Slice 都可能与原 Slice 共享底层数组，除非发生了扩容**。

另一个常见坑：**忘记接收返回值**。

```go
package main

import "fmt"

func main() {
    s := make([]int, 0, 1)
    s = append(s, 1) // 正确：必须接收返回值
    // 下面这行代码虽然能编译通过，但 `go vet` 会报警告：
    // "result of append is not used"
    // 因为 append 返回的新 Slice 头被丢弃，s 没有变化
    // append(s, 2)
    fmt.Println(s)
}
```

> 要点：把 Slice 想象成"数组的一段视图"。视图之间可以重叠，写操作会互相可见。`append` 返回的 Slice 头可能指向新数组，所以必须接收。

### 4.2 growslice()

**是什么**

`runtime.growslice` 是当前 Runtime 负责 Slice 扩容的入口。下面签名以 Go 1.26 为准；它属于私有 ABI，历史版本可能不同：

```go
// runtime/slice.go
func growslice(oldPtr unsafe.Pointer, newLen, oldCap, num int, et *_type) slice
```

参数含义：

- `oldPtr`：旧底层数组首元素指针，用于拷贝旧元素。
- `newLen`：扩容后新 Slice 的长度（`oldLen + num`）。
- `oldCap`：扩容前的容量。
- `num`：本次要追加的元素个数。
- `et`：元素类型信息（`runtime._type`），含大小、对齐等。

返回值是一个新的 `slice` 结构体（即上一节的三字段结构），其 `array` 指向新分配的内存。

**为什么这样设计 / 底层实现要点**

`growslice` 的核心流程（简化伪代码）：

```go
func growslice(oldPtr unsafe.Pointer, newLen, oldCap, num int, et *_type) slice {
    oldLen := newLen - num

    // 1. 元素大小为 0 的特殊处理
    if et.Size_ == 0 {
        return slice{unsafe.Pointer(&zerobase), newLen, newLen}
    }

    // 2. 计算候选容量；3. 按元素大小和 size class 圆整字节数
    newcap := nextslicecap(newLen, oldCap)
    noscan := !et.Pointers()
    capmem, newcap := roundCapacity(newcap, et.Size_, noscan) // 概念辅助函数
    lenmem := uintptr(oldLen) * et.Size_
    newlenmem := uintptr(newLen) * et.Size_

    // 4. 分配新内存：概念分支，省略溢出和写屏障细节
    var p unsafe.Pointer
    if noscan {
        p = mallocgc(capmem, nil, false)
        // 只清 append 不会立即覆盖的尾部
        memclrNoHeapPointers(add(p, newlenmem), capmem-newlenmem)
    } else {
        p = mallocgc(capmem, et, true)
        // 当前实现还会在需要时执行批量写屏障
    }

    // 5. 把旧元素拷贝到新内存
    memmove(p, oldPtr, lenmem)

    return slice{p, newLen, newcap}
}
```

几个关键设计点：

1. **`et.size == 0` 的特判**：空结构体 `struct{}` slice 不需要元素载荷内存。Go 1.26.4 的 `growslice` 返回以 `runtime.zerobase` 为数据指针的结果并只更新 `len`/`cap`；零大小对象的地址是否相等不是语言契约。Slice 头本身仍存在。

2. **三段式容量计算**：`newLen > 2*oldCap` 时直接采用 `newLen`；`oldCap < 256` 时翻倍；超过 256 后走平滑过渡。这部分在 4.3 节展开。

3. **`roundupsize` 对齐**：Go 的小对象分配器使用 size class，大对象按 page 对齐。`roundupsize` 把 `newcap * et.Size_` 圆整到分配器可提供的字节数，再反推实际 `newcap`。元素是否含指针还会影响 malloc header 与圆整路径，因此 `cap` 不只由元素个数决定。

4. **`memmove` 拷贝**：Runtime 用架构相关的 `memmove` 搬运旧元素。它可能使用向量化或针对小尺寸的专门路径；程序不应依赖具体指令。扩容时源、目标是不同分配。

5. **清零与写屏障**：含指针元素使用带类型信息的 `mallocgc`，新区域必须保持可安全扫描的零值，并在复制旧指针时配合批量写屏障。无指针元素可省去扫描和部分清零，只清理本次 append 不会覆盖的尾部。

**工程实践与常见坑**

- **不要假设 `cap` 一定翻倍**：很多人记得"Go Slice 扩容是 2 倍"，但实际还受 `nextslicecap`、元素大小和 `roundupsize` 影响。例如 `make([]int, 1000, 1000)` 再 append 才会触发扩容；`make([]int, 0, 1000)` 的第一次 append 容量仍是 1000。
- **大 Slice 扩容代价可能很高**：对非零大小元素，增长需要保留已有元素，真实搬运量为 O(n)。能可靠估计最终规模时预分配通常有益；输入上界不可信或估计偏差很大时，应在复制成本与闲置内存之间权衡。
- **零大小元素也有开销**：`[]struct{}` 虽然不分配数据内存，但 Slice 头本身仍要分配，且 Runtime 仍要维护 `len`/`cap`。

### 4.3 扩容算法

**是什么**

Go Slice 的扩容算法决定 `append` 时新 `cap` 的取值。算法在 Go 1.18 做过一次重要调整，从"硬阈值 1024"改为"基于 256 的平滑过渡"；Go 1.26.4 仍使用这条主线。

**为什么这样设计 / 底层实现要点**

旧算法（Go 1.17 及之前）：

```go
if newLen > doublecap {
    newcap = newLen
} else {
    if oldCap < 1024 {
        newcap = doublecap
    } else {
        newcap = oldCap + oldCap/4  // 1.25 倍
    }
}
```

新算法（Go 1.18+）核心逻辑：

```go
newcap := oldCap
doublecap := newcap + newcap
if newLen > doublecap {
    newcap = newLen
} else {
    const threshold = 256
    if oldCap < threshold {
        newcap = doublecap            // 小 Slice 翻倍
    } else {
        for 0 < newcap && newcap < newLen {
            newcap += (newcap + 3*threshold) / 4  // 平滑过渡
        }
        if newcap <= 0 {
            newcap = newLen
        }
    }
}
```

为什么把阈值从 1024 改为 256？为什么用循环？

1. **内存利用率**：旧算法在 `oldCap >= 1024` 之后一刀切为 1.25 倍，导致在 512~2048 区间内扩容行为不够平滑——小 Slice 浪费内存（翻倍后用不到），中 Slice 又扩得太少。新算法通过 `newcap += (newcap + 3*threshold)/4 = newcap*1.25 + 192` 的循环，让增长系数从 2.0 平滑过渡到 1.25。

2. **批量追加直接满足需求**：当 `newLen > 2*oldCap` 时直接返回 `newLen`，不会从旧容量逐级循环增长。平滑循环只处理目标没有超过两倍旧容量的情况。

3. **`threshold = 256` 是元素数阈值**：它不是 256 字节，也不对应所有元素类型的同一个 size class。它属于 Runtime 在扩容次数与闲置容量之间的实现权衡。

> 新算法等价于：当 `oldCap < 256` 时 `newcap = max(newLen, oldCap*2)`；之后每次按 `newcap = newcap*1.25 + 192` 增长直到不小于 `newLen`。

完成 `newcap` 计算后，还要经过 `roundupsize` 把总字节数对齐到 size class。比如 `et.size == 8`、`newcap == 30` 时，总字节数 240 对齐到 256（`sizeclass` 表中 256 是一个 class），实际 `newcap` 就是 32。

下表对比新旧算法在几个典型 `oldCap` 下的表现（`newLen = oldCap + 1`）：

| oldCap | 旧算法 newcap（理论） | 新算法 newcap（理论） | 说明 |
|---|---|---|---|
| 64 | 128（2x） | 128（2x） | 一致 |
| 256 | 512（2x） | 512（边界仍 2x） | 一致 |
| 512 | 1024（2x） | 832（1.625x） | 新算法更省 |
| 1024 | 1280（1.25x） | 1472（1.4375x） | 新算法略多 |
| 4096 | 5120（1.25x） | 5312（1.296x） | 接近 |

> 注意：上表是 `roundupsize` 之前的"理论值"，实际 `cap` 还会被 size class 对齐再放大一些。

**工程实践与常见坑**

- **不要依赖精确的 `cap` 值**：算法会随 Go 版本变化，代码里写死 `cap` 判断是反模式。
- **大 Slice 的扩容倍数更接近 1.25**：对几百万级别的 Slice，每次扩容只多 25% 左右，意味着频繁扩容。务必预分配。
- **批量 append 能一次表达所需增长量**：`s = append(s, arr...)` 可一次检查容量并批量复制；逐项循环可能经历多次增长，但如果容量已预留，两者路径会不同。性能差距取决于元素类型、内联和输入规模，按语义选择并对热点测量。

### 4.4 为什么 append 返回新的 Slice

**是什么**

`append` 的签名要求调用者接收返回值：`s = append(s, x)`。如果你只是 `append(s, x)` 而不接收，`go vet` 会警告 `result of append is not used`。这是因为 `append` 不会修改原 Slice 头变量，而是返回一个新的 Slice 头。

**为什么这样设计 / 底层实现要点**

根本原因：**Slice 头是值类型**。Slice 在 Go 中没有引用语义，参数传递、变量赋值都是复制三字段结构体。`append` 接收的 `slice` 参数是一个副本，对副本的 `len`/`cap`/`array` 修改不会反映到调用方的变量上。

考虑两种情形：

1. **容量足够**：`append` 在原底层数组上写入新元素，返回的新 Slice 头的 `array` 与原 Slice 相同，但 `len` 增加了。如果你不接收返回值，原 Slice 变量的 `len` 不变，新写的数据对你"不可见"——但实际它已经写到了底层数组里，可能导致后续诡异 bug。

2. **容量不足**：`append` 分配了新底层数组，返回的新 Slice 头的 `array` 是新地址。如果你不接收返回值，原 Slice 变量仍然指向旧底层数组，追加的数据完全丢失。

为什么不把 Slice 设计成引用类型（像 C++ 的 `std::vector&`）？这是 Go 的核心设计哲学：**显式优于隐式**。Go 选择让所有东西默认是值语义，引用通过指针显式表达。这样：

- 函数签名 `func f(s []int)` 一眼看出"我可能修改底层数组，但不会修改你的 Slice 头"。
- 调用者写 `s = append(s, x)` 一眼看出"我的 Slice 头会变"。

**工程实践与常见坑**

- **永远写 `s = append(s, x)`**：哪怕是单行也必须赋值回去。

```go
package main

import "fmt"

func push(s []int, x int) []int {
    return append(s, x) // 必须返回
}

func main() {
    s := []int{1, 2, 3}
    s = push(s, 4)
    fmt.Println(s) // [1 2 3 4]
}
```

- **`append` 后的别名问题**：`a := s; s = append(s, x)` 后，如果发生了扩容，`a` 仍然指向旧底层数组，`a` 和 `s` 不再共享。但如果没扩容，它们仍共享。这种"有时共享有时不共享"是最容易出 bug 的地方，解决方案见 4.9 节。

```go
package main

import "fmt"

func main() {
    s := make([]int, 3, 5)
    a := s                    // a 与 s 共享底层数组
    s = append(s, 1)          // 容量够，不扩容，a 和 s 仍共享
    a[0] = 99                 // s[0] 也变成 99
    fmt.Println(s[0], a[0])   // 99 99

    for i := 0; i < 10; i++ {
        s = append(s, i)      // 触发扩容，s 指向新数组
    }
    a[1] = 88                 // 只影响 a，不影响 s
    fmt.Println(s[1], a[1])   // 原 s[1] 值 88
}
```

### 4.5 copy()

**是什么**

`copy` 是另一个 Slice 相关的内置函数：

```go
// The copy built-in function copies elements from a source slice into a
// destination slice and returns the number of elements copied.
func copy(dst, src []Type) int
```

它把 `src` 的元素复制到 `dst`，复制数量是 `min(len(dst), len(src))`，并返回复制了多少个元素。`copy` 处理 `dst` 和 `src` 重叠的情况（底层用 `memmove`）。

**为什么这样设计 / 底层实现要点**

`copy` 的 Runtime 实现是 `runtime.typedslicecopy`（带类型信息）或 `runtime.slicecopy`（小型化版本）。简化伪代码：

```go
func slicecopy(to, from unsafe.Pointer, n uintptr, wid uintptr) int {
    if n == 0 || wid == 0 {
        return int(n)
    }
    // memmove 内部会判断方向，正确处理源/目标重叠
    memmove(to, from, n*wid)
    return int(n)
}
```

参数含义：

- `to`/`from`：目标/源底层数组首元素地址。
- `n`：实际要拷贝的元素个数（调用前已求 `min(len(dst), len(src))`）。
- `wid`：每个元素的字节大小。

几个设计要点：

1. **`min(len(dst), len(src))` 自动截断**：你不需要手动计算长度，`copy` 不会越界。如果 `dst` 比 `src` 短，只复制 `dst` 能装下的部分；反之亦然。

2. **`memmove` 处理重叠**：当你 `copy(s[1:], s[:len(s)-1])` 这种"Slice 内部搬运"时，源和目标指向同一块内存。`memmove` 内部会判断方向，从后向前或从前向后拷贝，保证结果正确。

3. **`copy` 不是 `clone`**：`copy` 不会自动分配目标 Slice。常见错误：

```go
package main

import "fmt"

func main() {
    var dst []int
    src := []int{1, 2, 3}
    n := copy(dst, src)        // 啥也没拷贝！dst 的 len 还是 0
    fmt.Println(n, dst)        // 0 []

    dst = make([]int, len(src))
    copy(dst, src)             // 正确
    fmt.Println(dst)           // [1 2 3]
}
```

4. **支持 `[]byte` 与 `string` 互转**：`copy([]byte, string)` 和 `copy([]byte, string)` 是编译器特例，因为 string 内部是只读字节序列，需要专门处理。

**工程实践与常见坑**

- **复制 Slice 必须先 `make` 目标**：

```go
dst := make([]int, len(src))
copy(dst, src)
// 或用 append 的语法糖（推荐用 Go 1.21+ 的 slices.Clone）：
// dst := append([]int(nil), src...)
```

- **删除中间元素**：利用 `copy` 把后面的元素前移。

```go
package main

import "fmt"

func removeAt(s []int, i int) []int {
    copy(s[i:], s[i+1:])
    return s[:len(s)-1]
}

func main() {
    s := []int{1, 2, 3, 4, 5}
    s = removeAt(s, 2)
    fmt.Println(s) // [1 2 4 5]
}
```

- **批量插入**：

```go
package main

import "fmt"

func insertAt(s []int, i int, xs ...int) []int {
    if cap(s) >= len(s)+len(xs) {
        s = s[:len(s)+len(xs)]
    } else {
        news := make([]int, len(s)+len(xs))
        copy(news, s)
        s = news
    }
    // 把 i 之后的内容后移 len(xs) 位
    copy(s[i+len(xs):], s[i:])
    // 把新元素填入 i 处
    copy(s[i:], xs)
    return s
}

func main() {
    s := []int{1, 2, 5}
    s = insertAt(s, 2, 3, 4)
    fmt.Println(s) // [1 2 3 4 5]
}
```

- **`copy` 与 `append` 的取舍**：目标长度已确定且只是搬运元素时，`copy` 能直接表达批量复制，编译器/Runtime 可使用 `memmove`；`append(dst, src...)` 也可能走高效批量路径并负责增长。按语义选择，再对热点 benchmark，不要宣称 `copy` 在所有情况下必然更快。

### 4.6 cap 的变化规律

**是什么**

本节通过实验数据揭示 `cap` 在不同初始容量、不同元素大小下的实际变化规律，让你直观感受 `roundupsize` 的影响。

**为什么这样设计 / 底层实现要点**

回顾 `growslice`：先算理论 `newcap`，再通过 `roundupsize` 对齐。小对象使用 size class，大对象按 page 圆整。`internal/runtime/gc/sizeclasses.go` 定义了小对象尺寸类；Go 1.26.4 的部分可用字节数如下，编号和整张表都是实现细节：

| 8 | 16 | 24 | 32 | 48 | 64 | 80 | 128 | 256 | 512 | 896 | 1024 | 1536 | 2048 | 4096 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|

`roundupsize(n)` 会把请求向上圆整到分配器可提供的大小。这就是为什么实际 `cap` 经常与理论值不同。

下面把 `len` 和 `cap` 都设为 n，再 append 一个 `int64`，确保触发扩容。结果来自 Go 1.26.4、darwin/arm64，只用于说明圆整效应：

| 初始 cap | 期望 (翻倍) | 实测 cap | 说明 |
|---|---|---|---|
| 1 | 2 | 2 | 16 字节正好是 sizeclass 2 |
| 2 | 4 | 4 | 32 字节正好是 sizeclass 4 |
| 4 | 8 | 8 | 64 字节正好是 sizeclass 6 |
| 8 | 16 | 16 | 128 字节正好匹配 |
| 16 | 32 | 32 | 256 字节正好匹配 |
| 32 | 64 | 64 | 512 字节正好匹配 |
| 64 | 128 | 128 | 1024 字节匹配 |
| 128 | 256 | 256 | 2048 字节匹配 |
| 256 | 512 | 512 | 阈值边界，理论翻倍后对齐仍为 512 |
| 512 | 1024 | 848 | 理论 newcap=832，6656 字节圆整到 6784 |
| 1024 | 2048 | 1536 | 理论 newcap=1472，11776 字节圆整到 12288 |
| 2048 | 4096 | 3072 | 理论 newcap=2752，再按 size class 圆整 |
| 4096 | 8192 | 6144 | 理论 newcap=5312，大对象按 page 圆整 |

> 上表数据会随 Go 版本与平台变化，请以你本机实测为准。可以用下面的程序验证。

**实测程序**：

```go
package main

import "fmt"

func capAfterAppend(n int) int {
    s := make([]int64, n, n)
    s = append(s, 1)
    return cap(s)
}

func main() {
    for _, n := range []int{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096} {
        fmt.Printf("init cap=%-6d -> after append cap=%d\n", n, capAfterAppend(n))
    }
}
```

运行后你会发现：

- 小 Slice（`oldCap < 256`）基本是精确翻倍。
- 大 Slice 进入平滑过渡，理论增长系数逐渐接近 1.25，但 size class 或 page 圆整会改变实际比例。
- size class 或 page 圆整会让实际 `cap` 大于理论 `newcap`。

**工程实践与常见坑**

- **不要硬编码 `cap`**：基于 `cap` 的精确值写逻辑会让代码与 Go 版本耦合。
- **预分配避免依赖扩容**：若最终长度不超过 expected，`make([]T, 0, expected)` 不需要扩容；低估仍会增长，高估则会保留闲置容量。
- **观察分配与保留**：用 `-benchmem`、alloc profile 和 heap profile 判断扩容与过度预分配，不从某次 `cap` 猜整体 GC 压力。

### 4.7 扩容性能分析

**是什么**

本节从均摊复杂度、缓存友好性、内存分配开销三个角度分析 `append` 的性能特征，并给出 Benchmark 实测数据。

**为什么这样设计 / 底层实现要点**

**均摊 O(1) 分析**：

假设初始 `cap = 1`，每次扩容翻倍，追加 n 个元素的总拷贝次数为：

```
1 + 2 + 4 + 8 + ... + n/2 + n ≈ 2n - 1
```

在当前几何增长实现下，n 次逐项 append 的总元素搬运为 O(n)，均摊每次 O(1)；大容量路径趋近 1.25 倍也仍满足几何级数收敛。这是实现提供的重要性能性质，不是规范对未来 `cap` 策略的固定承诺。

**缓存友好性**：

Slice 的连续 backing store 通常有利于顺序访问的 cache locality。链表节点往往分散且多一次指针追踪，但实际差距受元素大小、访问模式、预取、逃逸和算法复杂度影响；不能脱离 workload 宣称某一种访问路径“最高效”。

**内存分配开销**：

`mallocgc` 调用涉及 tiny/small/large 分流以及 mcache、mcentral、mheap/pageAlloc（参见内存管理章节）。不超过当前 `MaxSmallSize` 的小对象可从 P 的 mcache 快速分配；refill 与大对象需要进入更下层分配器。大对象从页分配器取得连续 page，不是“每个对象直接 mmap”。扩容通常包含新分配和 `memmove`；旧数组只有在不再可达后才由 GC 回收。

**Benchmark 对比**：

```go
package main

import "testing"

func BenchmarkAppendDynamic(b *testing.B) {
    for i := 0; i < b.N; i++ {
        s := make([]int, 0)
        for j := 0; j < 1000; j++ {
            s = append(s, j)
        }
    }
}

func BenchmarkAppendPrealloc(b *testing.B) {
    for i := 0; i < b.N; i++ {
        s := make([]int, 0, 1000)
        for j := 0; j < 1000; j++ {
            s = append(s, j)
        }
    }
}
```

运行结果必须附 CPU、GOOS/GOARCH、Go 版本和统计方法。这个输入下预分配通常会减少扩容与分配总字节数，但具体时间、分配次数和容量圆整随工具链与元素类型变化；用 `-count` 配合 `benchstat` 比较，不引用“固定快 5 倍”。

**工程实践与常见坑**

- **按可信上限预分配**：合理 hint 能减少复制；严重高估会增加保留内存、清零和 GC 扫描成本。输入不可信时先设置容量上限。
- **批量 `append`**：`s = append(s, bigSlice...)` 一次扩容到位，比循环 append 触发的扩容次数少。
- **复用 Slice**：用 `s = s[:0]` 重置长度，保留底层数组，避免重复分配。但要注意 GC 不会回收底层数组里被"逻辑删除"的对象引用（详见 4.8 节）。
- **在热点中验证扩容成本**：序列化、网络处理等路径可根据历史分布设置 hint 或复用有上限的缓冲区，并用 profile 验证；不要为避免扩容而无界预留。

### 4.8 GC 如何处理旧数组

**是什么**

当 Slice 扩容后，旧底层数组可能成为垃圾。本节解释 Go GC 如何回收旧数组，以及一种常见的"隐性内存泄漏"模式。

**为什么这样设计 / 底层实现要点**

Go 使用并发三色标记清除 GC（细节参见 GC 章节）。对 Slice 而言：

1. **扩容时**：`growslice` 调用 `mallocgc` 分配新数组，旧数组仍由原 Slice 头（如果还存在）或共享它的其他 Slice 持有。
2. **GC 标记阶段**：GC 从根集合出发，扫描所有可达的 Slice 头，通过 `array` 指针找到并标记底层数组。
3. **清除阶段**：未被标记的旧数组所在的 mspan 会被回收。

关键点：**只要还有一个 Slice 头指向旧数组，旧数组就不会被回收**。这就引出了经典的"大数组小引用"内存泄漏：

```go
package main

import "fmt"

func main() {
    big := make([]byte, 1<<20) // 1 MB
    small := big[:10]          // small.array 仍指向 big 的底层数组
    big = nil                  // 期望释放 1 MB
    // 实际上：1 MB 仍然被 small 持有，不会回收！
    fmt.Println(len(small))
}
```

`big = nil` 只是把 `big` 这个 Slice 头的 `array` 置零，但 `small` 的 `array` 仍指向那 1MB 内存。GC 通过 `small` 标记了整个 1MB 数组。

**更隐蔽的版本：`append` 后的别名**：

```go
package main

import "fmt"

type Foo struct{ X int }

func main() {
    s := make([]*Foo, 1, 1024) // cap=1024，底层数组 8KB
    s[0] = &Foo{}
    big := s                   // big 与 s 共享底层数组
    // 假设后续 s 触发扩容（这里只是示意，实际 cap=1024 足够）
    // s = append(s, more...)
    // big 仍持有旧底层数组，里面的 &Foo{} 不会被 GC
    fmt.Println(big)
}
```

**正确做法：拷贝并切断引用**：

```go
package main

import "fmt"

type Foo struct{ X int }

func main() {
    big := make([]*Foo, 1<<10)
    big[0] = &Foo{X: 1}

    // 只需要前 10 个，但不想持有整个 1<<10 数组
    small := make([]*Foo, 10)
    copy(small, big[:10])
    big = nil // 现在 1<<10 数组可被 GC 回收

    fmt.Println(small[0])
}
```

**`copy` + `nil` 切断**是 Go 中显式释放大 Slice 内存的标准模式。

**工程实践与常见坑**

- **`s[:0]` 复用要小心对象引用**：如果 Slice 里存的是指针，`s[:0]` 后底层数组里仍持有旧对象，阻止它们被 GC。处理方法是显式置零：

```go
for i := range s {
    s[i] = nil // 或 Foo{}
}
s = s[:0]
```

- **解码大 Slice 后只取小段**：典型如 `json.Unmarshal` 把整个 JSON 读到 `[]byte`，然后解析出一个小结构体。如果你保留了对那个 `[]byte` 的引用（哪怕只是切片），整个 JSON 缓冲区都不会被回收。解决：解析后立即 `data = nil`，或用流式 `json.Decoder`。
- **`bytes.Buffer.Reset()` 同理**：`Reset` 只是把长度置零，底层数组保留。如果 buffer 曾经很大，内存不会自动释放；需要 `buffer = bytes.Buffer{}` 重新分配一个空 buffer。

### 4.9 append 的最佳实践

**是什么**

本节汇总 `append` 与 Slice 扩容相关的工程实践要点，作为日常编码的速查表。

**为什么这样设计 / 底层实现要点**

实践要点全部源自前面的分析：

- Slice 头是值类型 → `append` 必须赋值回。
- 扩容会拷贝 → 预分配避免拷贝。
- 共享底层数组 → 用 `copy` 切断。
- GC 看引用 → 显式置零释放内存。

**工程实践与常见坑**

**1. 使用 `append` 返回值**

```go
s = append(s, x)        // 正确
append(s, x)            // 编译错误：返回值未使用
```

**2. 知道大小时预分配**

```go
// 不好
var s []int
for i := 0; i < n; i++ {
    s = append(s, i)
}

// 好
s := make([]int, 0, n)
for i := 0; i < n; i++ {
    s = append(s, i)
}

// 也可以直接 make 长度 + 索引赋值；热点性能以 benchmark 为准
s := make([]int, n)
for i := range s {
    s[i] = i
}
```

**3. 不知道大小时给出"合理上限"**

```go
// 完全不预估
s := []int{}

// 预估上限（即便过估也比不预估好）
s := make([]int, 0, 128)
```

**4. 用 `copy` 而非循环赋值**

```go
// 慢
for i, v := range src {
    dst[i] = v
}

// 快
copy(dst, src)
```

**5. 过滤元素的惯用法**

```go
// 不分配新底层数组（但保留原数组容量）
result := src[:0]
for _, v := range src {
    if keep(v) {
        result = append(result, v)
    }
}

// 干净切断（如果 src 很大且 result 很小）
result := make([]T, 0, len(src))
for _, v := range src {
    if keep(v) {
        result = append(result, v)
    }
}
```

**6. 避免跨 goroutine 共享 Slice 头**

Slice 头不是并发安全的。多 goroutine 读写同一个 Slice 必须加锁，或者用 channel 传递所有权。

**7. `append` 链的陷阱**

```go
s := []int{1, 2, 3}
t := append(s, 4) // t 与 s 可能共享底层数组
u := append(s, 5) // u 也与 s 共享底层数组！u[3] 可能覆盖 t[3]
// 此时 t[3] 可能是 5 而非 4
```

规则：**一旦对同一 Slice 多次 `append` 并保留多个结果，必须警惕共享**。如果需要独立副本，用 `copy` 或 `append([]T(nil), s...)`。

**8. 使用 `slices` 标准库（Go 1.21+）**

Go 1.21 引入了 `slices` 包，提供 `Insert`、`Delete`、`Clone`、`Concat` 等函数，封装了底层 `copy`/`append` 细节：

```go
package main

import (
    "fmt"
    "slices"
)

func main() {
    s := []int{1, 2, 5}
    s = slices.Insert(s, 2, 3, 4) // [1 2 3 4 5]
    fmt.Println(s)
    s = slices.Delete(s, 1, 3)    // [1 4 5]
    fmt.Println(s)
    c := slices.Clone(s)          // 独立副本
    fmt.Println(c)
}
```

> `slices.Clone` 是 `append([]T(nil), s...)` 的语法糖，用于安全切断共享。

**9. 删除元素后切断引用（指针元素）**

```go
package main

import "fmt"

func deleteAtIndex(s []*int, i int) []*int {
    // 先置 nil 让 GC 回收被删除对象
    s[i] = nil
    copy(s[i:], s[i+1:])
    s[len(s)-1] = nil // 收尾置 nil
    return s[:len(s)-1]
}

func main() {
    a, b, c := 1, 2, 3
    s := []*int{&a, &b, &c}
    s = deleteAtIndex(s, 1)
    fmt.Println(*s[0], *s[1]) // 1 3
}
```

### 本章小结

本章围绕 `append` 展开，核心要点：

1. `append` 在容量足够时原地写、容量不足时调 `growslice` 分配新数组并 `memmove` 拷贝。
2. Go 1.18+ 的扩容算法用 256 阈值 + 平滑过渡替代了旧的 1024 阈值；`roundupsize` 再按 size class 或 page 圆整，使实际 `cap` 常与理论值不同。
3. Slice 头是值类型，`append` 必须接收返回值，否则丢失扩容结果。
4. `copy` 用 `memmove` 实现，是安全、高效的 Slice 复制手段。
5. GC 通过 Slice 头的 `array` 指针追踪底层数组，"大数组小引用"是常见的隐性内存泄漏。
6. 工程实践：按可信容量 hint 预分配、用 `copy` / `slices.Clone` 切断共享，并用 benchmark/profile 验证热点。

理解 `append` 等于理解 Slice 的动态行为，下一章将进入 Go Map，分析 Go 1.24+ 的 Swiss Table 与旧 bucket 实现的版本差异。
