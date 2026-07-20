## 第6章 String

> 引言：String 是 Go 中看似简单却暗藏玄机的基础类型，它本质是一段只读字节序列的"头"，配合 UTF-8 与 rune，构成了 Go 文本处理的核心。
>
> 本章语言语义基线为 Go 1.26，源码快照基于 Go 1.26.4；string 的只读语义与 UTF-8 规则是语言契约，header 布局与转换优化是 gc 实现细节。

### 6.1 String Header

**(1) 是什么**

Go 的 string 不是传统意义上的"字符数组"，也不是 C 语言的 `char*`。它是一个**只读的字节序列（read-only slice of bytes）**，底层由一个指向字节数组的指针和长度组成。string 是值类型，赋值和传参时复制的是这个"头"，而不是底层的字节数据。

```go
package main

import "fmt"

func main() {
    s := "hello, 世界"
    fmt.Println(len(s)) // 13：7 个 ASCII + 2 个汉字各 3 字节
}
```

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

在 Go runtime 中（`runtime/string.go`），string 的内部表示是 `stringStruct`：

```go
// runtime/string.go
type stringStruct struct {
    str unsafe.Pointer // 指向底层字节数组的指针
    len int            // 字节的个数（不是字符个数）
}
```

而在 `reflect` 包中对外暴露的是（Go 1.20 起已标记 deprecated，但用于理解结构仍然经典）：

```go
// Deprecated: 使用 unsafe.String / unsafe.StringData 代替
type StringHeader struct {
    Data uintptr // 底层字节数组的起始地址
    Len  int     // 字节长度
}
```

逐字段解释：

| 字段 | 类型 | 含义 |
|------|------|------|
| `str` / `Data` | `unsafe.Pointer` / `uintptr` | 指向底层连续的字节数组；数组没有独立的"长度"字段，长度信息只存在于 header 中 |
| `len` / `Len` | `int` | 字节数，不是 rune 数。例如 "中文" 的 len 是 6（UTF-8 编码每个汉字 3 字节） |

在 64 位平台上，一个 string 变量占 **16 字节**（指针 8 + int 8）。可以用下面这段代码验证：

```go
package main

import (
    "fmt"
    "unsafe"
)

func main() {
    s := "hello, 世界"
    hdr := (*[2]uintptr)(unsafe.Pointer(&s))
    fmt.Printf("data ptr = %#x\n", hdr[0])
    fmt.Printf("len       = %d\n", hdr[1]) // 13 = 7 + 3 + 3
    fmt.Printf("sizeof    = %d\n", unsafe.Sizeof(s)) // 16
}
```

Runtime 要点：

- **值语义但共享数据**：传参复制的是 string header（64 位实现通常 16 字节），不是整段字节。具体 ABI 可用寄存器或栈槽传递，成本与目标平台和调用形状有关，但不会按字符串长度复制载荷。
- **没有 NUL 结尾**：Go string 不像 C 字符串那样以 `\0` 结尾，长度信息靠 `len` 字段维护。这也意味着 string 中间可以包含 `\0`。
- **字符串字面量**：通常进入只读静态数据，编译器/链接器可以合并相同常量。地址共享与具体段布局不是语言保证。

**(3) 工程实践与常见坑**

> 坑 1：不要把 `[]byte` 零拷贝转换成 string 后继续修改原切片。确需使用 Go 1.20+ 的 unsafe 原语时，写成 `unsafe.String(unsafe.SliceData(b), len(b))`，这样空切片也不会索引 `b[0]`；只要 string 仍可能被读取，任何别名都不得修改这些字节。普通业务优先使用安全转换。

> 坑 2：`len(s)` 返回的是字节数，不是"字符数"。要数 rune 用 `utf8.RuneCountInString(s)` 或 `len([]rune(s))`。

> 坑 3：substring 不会拷贝底层数据。`s2 := s[:10]` 后 `s2` 仍指向 `s` 的底层数组。如果 `s` 很大而 `s2` 很小却要长期持有，会阻止整个大数组被 GC，造成"内存泄漏"。解决：`strings.Clone(s2)`（Go 1.18+）。

### 6.2 UTF-8

**(1) 是什么**

UTF-8 是一种变长编码，能表示 Unicode 的所有码点（code point），每个码点编码为 1~4 字节。用户感知字符可能由多个码点组成。Go 源码文本使用 UTF-8；源码字符按 UTF-8 出现在字符串字面量中，解释型字面量还可通过 `\xNN` 等转义构造任意字节，因此 string 值本身不保证合法 UTF-8。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

UTF-8 由 Ken Thompson（也是 Go 作者之一）与 Rob Pike 设计，它有几个天然优势，这也是 Go 选它作为"默认编码"的原因：

- **完全兼容 ASCII**：0~127 的码点用单字节，与 ASCII 一致。纯英文文本没有任何膨胀。
- **变长但自同步**：任一字节出错只影响当前字符，不会"错位"扩散。
- **前缀码可前向解析**：从任意位置开始扫描，能跳过 continuation byte 找到下一个字符起点。
- **常见文本较紧凑**：ASCII 码点占 1 字节，许多汉字占 3 字节；具体语言文本的平均大小取决于码点分布，不能把整个拉丁语系都概括为单字节。

UTF-8 的编码规则（位模式）：

| 字节数 | 码点范围 | 字节模式 |
|--------|----------|----------|
| 1 | U+0000 ~ U+007F | `0xxxxxxx` |
| 2 | U+0080 ~ U+07FF | `110xxxxx 10xxxxxx` |
| 3 | U+0800 ~ U+FFFF | `1110xxxx 10xxxxxx 10xxxxxx` |
| 4 | U+10000 ~ U+10FFFF | `11110xxx 10xxxxxx 10xxxxxx 10xxxxxx` |

首字节中前导 `1` 的个数表示该字符占几个字节；`10xxxxxx` 是 continuation byte。

Runtime 内建了 UTF-8 解码能力，`runtime` 包里有 `decoderune` 等内部函数。标准库 `unicode/utf8` 提供完整工具：

```go
package main

import (
    "fmt"
    "unicode/utf8"
)

func main() {
    s := "世界"
    fmt.Println("字节长度:", len(s))                     // 6
    fmt.Println("rune 数量:", utf8.RuneCountInString(s)) // 2
    fmt.Println("是否合法 UTF-8:", utf8.ValidString(s))   // true

    r, size := utf8.DecodeRuneInString(s)
    fmt.Printf("首个 rune: %c (U+%04X), 占 %d 字节\n", r, r, size) // 世 U+4E16, 3
}
```

`range` 字符串时，Go 会自动按 UTF-8 解码出 rune：

```go
package main

import "fmt"

func main() {
    s := "Go语言"
    for i, r := range s {
        fmt.Printf("byte offset=%d, rune=%c\n", i, r)
    }
}
```

输出：

```
byte offset=0, rune=G
byte offset=1, rune=o
byte offset=2, rune=语
byte offset=5, rune=言
```

注意 `i` 是**字节偏移**而不是字符索引。

**(3) 工程实践与常见坑**

> 坑 1：string 不保证是合法 UTF-8。`string(b)` 其中 `b` 含任意字节时，`s` 仍是一个合法的 string，但 `utf8.ValidString(s)` 可能为 false。访问非法字节序列时 `utf8.DecodeRuneInString` 会返回 `U+FFFD`（替换字符）。
>
> 坑 1.1：`range` 遍历遇到非法 UTF-8 序列时，产出的 rune 是 `U+FFFD` 且只前进 **1 字节**（规范规定），因此一段连续坏字节会产出多个 `U+FFFD`。例如 `for i, r := range "a\xffb"` 依次得到 `(0, 'a')`、`(1, U+FFFD)`、`(2, 'b')`。注意 `U+FFFD` 也可能来自输入中真实存在的替换字符，不能反推原字节。

> 坑 2：不能用 `s[i]` 取"第 i 个字符"，`s[i]` 是第 i 个**字节**。对中文做下标会切到多字节字符中间，得到一个非法的 byte。

> 坑 3：需要随机访问"第 N 个字符"时，先把 string 转成 `[]rune`：`rs := []rune(s); r := rs[3]`。但这是 O(n) 拷贝，对长文本不友好。

> 实践：网络/文件 IO 的文本协议（如 HTTP header）一般是 ASCII，用 `[]byte` 处理即可；只有面向"人类可读字符"的逻辑（分词、排版）才需要 rune 化。

### 6.3 rune

**(1) 是什么**

`rune` 是 Go 的内置类型别名，定义于 `builtin/builtin.go`：

```go
// builtin/builtin.go
type rune = int32
```

它用来表示一个 **Unicode 码点（code point）**。`rune` 只是 `int32` 的别名，不是新类型，编译器层面没有任何区别。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

Unicode 码点的范围是 U+0000 ~ U+10FFFF，最大需要 21 位，`int32`（4 字节）足够容纳。别名 `rune` 用来表达“这里在处理 Unicode 码点”，而不是普通整数；它不等同于 grapheme cluster。

rune 字面量用单引号：`'中'`、`'a'`、`'\n'`，其值为该字符的码点（int32）。

```go
package main

import "fmt"

func main() {
    var r rune = '中'
    fmt.Printf("r = %d (U+%04X)\n", r, r) // r = 20013 (U+4E2D)
    fmt.Printf("sizeof(rune) = %d\n", 4)  // 固定 4 字节
}
```

与 string 的关系：

- string 是 UTF-8 编码的字节序列。
- `[]rune` 是把 string 解码后得到的码点切片，每个元素固定 4 字节。
- `range string` 每次迭代产出的是 `rune`，而不是 `byte`。

`[]rune(s)` 的 Runtime 实现（`runtime/string.go` 中的 `stringtoslicerune`）会逐字节 UTF-8 解码，分配一个 `[]int32`，因此：

- `len([]rune(s))` 得到字符数，但代价是 O(n) 时间 + O(n) 内存。
- `string(rs)`（rune 切片转 string）会逐个 rune 编码回 UTF-8，长度可变。

```go
package main

import "fmt"

func main() {
    s := "abc中"
    rs := []rune(s)
    fmt.Println(len(rs), len(s))           // 4 6
    fmt.Printf("%c\n", rs[3])              // 中
    fmt.Println(string([]rune{'G', 'o', '语', '言'})) // Go语言
}
```

**(3) 工程实践与常见坑**

> 坑 1：`len(runeSlice)` 是 rune 数，`len(string)` 是字节数，二者只在纯 ASCII 时相等。

> 坑 2：rune 不等于"用户感知的字符（grapheme cluster）"。比如 `é` 可能是单个码点 U+00E9，也可能是 `e` (U+0065) + 组合重音 U+0301 两个码点。emoji 表情如 👨‍👩‍👧‍👦 由多个码点组合而成。规范化可用 `golang.org/x/text/unicode/norm`；按 grapheme cluster 切分则需要实现 Unicode 文本分割规则的库，二者不是同一件事。

> 实践：处理"字符数限制"（如用户名长度、短信字数）时，先想清楚要的是字节、rune 还是 grapheme。三者结果可能不同。

### 6.4 byte

**(1) 是什么**

`byte` 同样是内置别名：

```go
// builtin/builtin.go
type byte = uint8
```

它表示一个 8 位无符号字节，取值范围 0~255。在 string 和 `[]byte` 的语境下，byte 就是 UTF-8 字节流中的一个字节。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

byte 与 rune 的对比：

| 特性 | `byte` (`uint8`) | `rune` (`int32`) |
|------|------------------|------------------|
| 大小 | 1 字节 | 4 字节 |
| 语义 | 原始字节 / ASCII 字符 | Unicode 码点 |
| 用于 | 二进制数据、UTF-8 字节流、ASCII | 解码后的字符 |
| 遍历 string 得到 | `for i := 0; i < len(s); i++ { s[i] }` | `for i, r := range s {}` |

string 底层是字节序列，二进制和文本共用同一数据结构。`byte` 强调"这是字节"，`rune` 强调"这是字符"。这种命名让代码意图清晰，避免在二进制协议和文本处理之间混淆。

```go
package main

import "fmt"

func main() {
    s := "Hi"
    // 用 byte 遍历：处理字节
    for i := 0; i < len(s); i++ {
        fmt.Printf("byte[%d]=%d\n", i, s[i])
    }
    // 用 rune 遍历：处理字符
    for i, r := range s {
        fmt.Printf("rune[%d]=%c\n", i, r)
    }
}
```

**(3) 工程实践与常见坑**

> 实践：处理 HTTP body、文件 IO、加密哈希等**二进制**数据用 `[]byte`；处理"文本语义"用 string。不要因为"都是字节"就混用，IO 边界尤其要注意。

> 坑 1：`byte('中')` 中的常量超出 `uint8` 表示范围，编译器会报 overflow；若先存入变量再执行 `byte(r)`，运行时转换才会丢弃高位。把 rune 转 byte 前应先检查范围，例如 `0 <= r && r <= 0x7f`。

> 坑 2：`[]byte` 的零值是 `nil`，`string` 的零值是 `""`。`string(nil)` 是编译错误（nil 无法转换为 string），但 `string([]byte(nil)) == ""` 成立；反向的 `[]byte("")` 在当前 gc 实现中返回非 nil 的空切片——这是实现行为而非规范保证，代码不应依赖其 nil 性，只应依赖 `len == 0`。

### 6.5 String 与 []byte

**(1) 是什么**

string 和 `[]byte` 在底层都是"一段连续字节 + 长度"。区别在于：

- string **只读**，`[]byte` 可读可写。
- 在当前 64 位 gc 实现中，string header 通常是 16 字节（指针+长度），`[]byte` header 通常是 24 字节（指针+长度+容量）；32 位目标相应更小，语言不保证具体布局。

二者可以互相转换，这是 Go 中最常见的操作之一。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

`[]byte` 的 slice header（`reflect.SliceHeader`，同样 deprecated）：

```go
type SliceHeader struct {
    Data uintptr // 底层数组指针
    Len  int     // 长度
    Cap  int     // 容量
}
```

转换的 Runtime 实现在 `runtime/string.go`：

```go
// string -> []byte，需要分配 + 拷贝
func stringtoslicebyte(buf *tmpBuf, s string) []byte

// []byte -> string，同样需要分配 + 拷贝
func slicebytetostring(buf *tmpBuf, ptr *byte, n int) string
```

从语言语义看，`[]byte(s)` 得到可独立修改的字节序列，`string(b)` 得到不受后续 b 修改影响的字符串。通常需要复制，但编译器可在结果不逃逸或不可观察时省去堆分配甚至物理拷贝。

```go
package main

import "fmt"

func main() {
    b := []byte{'h', 'i'}
    s := string(b) // 语义上与 b 独立；编译器可消除不可观察的物理拷贝
    b[0] = 'H'     // 修改 b 不影响 s
    fmt.Println(s) // hi
}
```

虽然语义上要拷贝，但编译器会在保证安全的前提下省去拷贝。常见的零拷贝优化场景：

1. **`string(b)` 立即用作 map 查找的 key**：当前 gc 编译器通常把 `m[string(b)]` 降为临时只读视图，不分配；这不是语言契约。
2. **`string(b)` 用于比较**：`if string(b) == "foo"` 可省略拷贝。
3. **`for i, c := range []byte(s)`**：编译器优化为直接遍历 string 字节，不分配。

Go 1.20+ 提供 `unsafe.String` 和 `unsafe.Slice` 作为官方的零拷贝原语：

```go
package main

import (
    "fmt"
    "unsafe"
)

func main() {
    b := []byte{'h', 'i'}
    // []byte -> string，零拷贝，但要求 b 之后不再修改
    s := unsafe.String(unsafe.SliceData(b), len(b))
    fmt.Println(s)

    // string -> []byte，零拷贝，但要求之后不修改返回的 slice 且不超出原 string 生命周期
    bs := unsafe.Slice(unsafe.StringData(s), len(s))
    fmt.Printf("%c\n", bs[0])
}
```

性能对比：

| 转换方式 | 是否分配 | 是否拷贝 | 安全 |
|----------|----------|----------|------|
| `string(b)` / `[]byte(s)` | 语义上独立；堆分配可被优化 | 语义上复制；不可观察时可省略 | 是 |
| `m[string(b)]` map 查找 | 当前编译器通常不分配 | 当前编译器通常省略 | 是 |
| `unsafe.String` / `unsafe.Slice` | 否 | 否 | 需调用方维持不可变性和生命周期 |

**(3) 工程实践与常见坑**

> 坑 1：在热路径里反复 `string(b)` → 处理 → `[]byte(s)` 会产生大量短命对象，加重 GC。能用 `[]byte` 贯穿就别转来转去。

> 坑 2：把大 `[]byte` 转成 string 再 `s[i]` 访问，会有一次大拷贝。要么直接用 `b[i]`，要么用 `unsafe` 系列原语（谨慎）。

> 警告：`unsafe.StringData` 返回的字节不得修改。违反 unsafe 契约后程序不再受 Go 内存安全保证，可能数据损坏或崩溃；不要把它封装成可写 `[]byte` 暴露给调用者。

> 实践：以 `[]byte` 为核心处理二进制协议，最后只在"对外输出"（写日志、JSON 字段）时转 string，能显著降低分配。

### 6.6 String 为什么不可变

**(1) 是什么**

Go 的 string 类型**只读**：你无法通过 `s[i] = 'x'` 修改 string 的某个字节，编译器直接拒绝。这是语言层面的约束，不是 runtime 的运行时检查。

```go
package main

func main() {
    s := "hello"
    // s[0] = 'H' // 编译错误：cannot assign to s[0]
    _ = s
}
```

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

1. **内存安全与共享**。string 传值只复制小型 header，底层字节可在多个 string 之间共享。如果可变，修改一处会影响所有共享者；只读语义让这种共享成为安全默认。header 的具体大小取决于平台。

2. **并发安全**。只要没有通过 unsafe 破坏约束，string 的字节内容可跨 goroutine 共享而无需同步；承载它的 map、slice 或其他可变容器仍需按各自规则同步。

3. **map key 的稳定性**。string 常用作 map key；若内容可变，插入后的哈希与相等关系会失效。Runtime 可按当前 map 实现选择哈希优化，但 `_type.Hash` 是类型元数据的哈希，不是某个字符串内容哈希。

4. **允许字面量共享**。编译器和链接器可以让相同字符串常量共享只读静态数据，从而减少空间；是否共享以及具体段布局都不是语言可观察的保证。

5. **静态数据与无指针载荷**。字面量可放入静态只读数据；运行时构造的字符串载荷是字节，不含需要扫描的 Go 指针。后一点同样适用于其他无指针字节数组，不是 string 独有的 GC 特权。

Runtime 层面的体现：

- string 字面量通常进入静态只读数据；具体目标平台和链接方式下可能映射为不可写页，但程序不能依赖段名或地址身份。
- 安全转换必须呈现独立可变性语义；编译器只有在不会改变可观察结果时才能消除实际拷贝。
- `runtime.memmove` 用于 string 拼接时拷贝到新内存。

可以绕过类型系统取得底层指针，但文档明确禁止修改 `unsafe.StringData` 返回的字节。违反该契约后程序不再受 Go 内存安全保证：

```go
package main

import (
    "fmt"
    "unsafe"
)

func main() {
    s := "hello"
    p := unsafe.StringData(s)
    fmt.Println(s, p)
    // *p = 'H'  // 千万别这么做：可能段错误，也可能改坏其它共享该字面量的 string
}
```

修改字面量尤其危险：数据可能被共享，也可能位于只读内存，结果可能是数据损坏或进程崩溃。

**(3) 工程实践与常见坑**

> 坑 1：需要在"字符串"上做大量修改时，不要用 string 拼接，用 `[]byte` 或 `strings.Builder`，完成后再转 string。

> 坑 2：substring 共享底层导致大字符串无法释放（见 6.1）。用 `strings.Clone` 显式拷贝。

> 实践：把 string 当成"成品文本"，把 `[]byte` 当成"工作台"。文本处理流水线应是 `string -> []byte (处理) -> string`。

### 6.7 strings.Builder

**(1) 是什么**

`strings.Builder` 是 Go 1.10 引入的、用于高效拼接字符串的类型。它内部维护一个 `[]byte`，通过 `WriteString`、`WriteByte`、`WriteRune` 等方法追加内容，最后用 `String()` 方法零拷贝转成 string。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

底层结构：

```go
// strings/builder.go
type Builder struct {
    addr *Builder // 指向自身，用于 copyCheck 检测被拷贝
    buf  []byte   // 累积的字节缓冲区
}
```

逐字段：

| 字段 | 含义 |
|------|------|
| `addr` | 逻辑上记录 Builder 自身地址；当前实现借助 `abi.NoEscape` 设置它，避免该检查本身迫使 Builder 逃逸。值拷贝后 `addr` 与新接收者地址不一致，后续写入会 panic，防止两个 Builder 共享同一缓冲区 |
| `buf` | 实际累积字节的 `[]byte`。WriteString 直接 append，扩容按 slice 扩容策略 |

`Builder.String()` 的实现利用了 `unsafe` 做零拷贝（Go 1.20+）：

```go
func (b *Builder) String() string {
    return unsafe.String(unsafe.SliceData(b.buf), len(b.buf))
}
```

（Go 1.20 之前使用等价的 unsafe 转换。）当前实现把 `buf[:len]` 作为 string 返回而不复制。继续向 Builder 追加是允许的：append 只写旧长度之后的位置；若扩容，旧 string 继续引用旧数组。Builder 的方法不会回头修改已返回 string 覆盖的字节，`Reset` 也只是丢弃 Builder 的引用。

关键方法：

| 方法 | 作用 |
|------|------|
| `WriteString(s string) (int, error)` | 追加字符串，对应 `io.StringWriter` 方法 |
| `WriteByte(c byte) error` | 追加单字节 |
| `WriteRune(r rune) (int, error)` | 追加一个 rune（自动 UTF-8 编码）；非法 rune（负值或超出 U+10FFFF、surrogate 区）会被替换为 `U+FFFD` 写入，不返回错误 |
| `Write([]byte) (int, error)` | 追加字节切片 |
| `Len() int` | 当前字节数 |
| `Cap() int` | 当前缓冲区容量 |
| `Grow(n int)` | 预留至少 n 字节空间，避免多次扩容 |
| `Reset()` | 清空（buf 置 nil，addr 置 nil） |
| `String() string` | 零拷贝返回结果 |

**(3) 工程实践与常见坑**

> 坑 1（重要）：**不要拷贝 Builder**。`b2 := b` 之后在 `b2` 上调用 Write 会 panic（`strings: illegal use of non-zero Builder copied by value`）。需要传递时用指针 `*strings.Builder`。

```go
package main

import "strings"

func main() {
    var b strings.Builder
    b.WriteString("a")
    b2 := b                  // 值拷贝
    b2.WriteString("b")      // panic: illegal use of non-zero Builder copied by value
}
```

> 坑 2：调用 `String()` 后继续 Write 是允许的，先前返回的 string 内容保持不变。真正禁止的是复制非零 Builder 后继续使用副本。

> 实践：拼接数量已知时，先 `b.Grow(n)` 预分配，避免多次扩容拷贝：

```go
package main

import "strings"

func join(parts []string) string {
    var b strings.Builder
    total := 0
    for _, p := range parts {
        total += len(p)
    }
    b.Grow(total)
    for _, p := range parts {
        b.WriteString(p)
    }
    return b.String()
}
```

> 实践：最终目标是 string 时优先评估 Builder；需要读回、二进制操作或 `io.Reader` 能力时 `bytes.Buffer` 更合适。性能差异用具体写入模式 benchmark。

### 6.8 String 性能优化

**(1) 是什么**

string 操作是 Go 程序中最常见的内存分配来源之一。本节总结一组实战优化技巧。

**(2) 为什么这样设计 / 底层数据结构与 Runtime 实现要点**

**1. 用 strings.Builder 代替循环累加拼接**

在循环里反复执行 `s += p` 会随着已有前缀增长而反复复制，可能形成 O(N^2) 的总字节搬运量。单个表达式 `a + b + c` 可被编译器一次性拼接，不能与循环累加混为一谈。Builder 用可增长的 `[]byte` 累积，最后直接返回 string 视图。

```go
package main

import (
    "strings"
    "testing"
)

var concatSink string

func concatPlus(parts []string) string {
    s := ""
    for _, p := range parts {
        s += p
    }
    return s
}

func concatBuilder(parts []string) string {
    var b strings.Builder
    for _, p := range parts {
        b.WriteString(p)
    }
    return b.String()
}

func BenchmarkPlus(b *testing.B) {
    parts := []string{"a", "b", "c", "d", "e"}
    for b.Loop() {
        concatSink = concatPlus(parts)
    }
}

func BenchmarkBuilder(b *testing.B) {
    parts := []string{"a", "b", "c", "d", "e"}
    for b.Loop() {
        concatSink = concatBuilder(parts)
    }
}
```

拼接越多，Builder 优势越明显。

**2. 预分配**

知道目标大小时用 `b.Grow(n)` 或 `make([]byte, 0, n)`，避免多次扩容。

**3. 用 strings.Join 替代循环 +**

`strings.Join` 内部就是 Builder + 预分配，且实现经过优化：

```go
package main

import (
    "fmt"
    "strings"
)

func main() {
    fmt.Println(strings.Join([]string{"a", "b", "c"}, ", ")) // a, b, c
}
```

**4. 避免 []byte ↔ string 反复转换**

在热路径里坚持一种表示。例如 HTTP handler 内部全程 `[]byte`，最后 `w.Write(b)`。

**5. 利用当前编译器的临时转换优化**

当前 gc 编译器通常让 `m[string(byteSlice)]` 直接用临时只读 string 视图查找，不产生堆分配。若先把转换结果保存、返回或传给未知调用，独立 string 可能需要复制并逃逸。把这当作可用 `-benchmem` 验证的优化，不是规范保证。

**(3) 工程实践与常见坑**

**6. 小心 substring 内存泄漏**

```go
package main

import (
    "fmt"
    "strings"
)

func main() {
    big := strings.Repeat("x", 1<<20) // 1MB
    small := big[:10]                  // small 仍指向 big 的底层数组
    // big 可以被 GC，但底层 1MB 数组因 small 引用而无法释放
    fmt.Println(len(small))

    // 修复：显式拷贝
    small2 := strings.Clone(big[:10])
    _ = small2
}
```

`strings.Clone`（Go 1.18+）会拷贝一份独立的底层字节，让原大字符串可被回收。

**7. 运行时 canonicalization 使用 `unique`**

编译器/链接器可合并常量字面量，但普通运行时 string 不会因内容相同自动共享身份。Go 1.23+ 的 `unique` 包为 comparable 值提供并发安全的 canonical handle：

```go
package main

import (
    "fmt"
    "unique"
)

func main() {
    first := unique.Make("GET")
    second := unique.Make(string([]byte{'G', 'E', 'T'}))
    fmt.Println(first == second) // true
    fmt.Println(first.Value())   // GET
}
```

`Handle[T]` 可直接比较；只要 handle 存活，canonical value 就存活。内部索引使用 weak pointer，所有 handle 不可达后条目可被清理，比无上限全局 `map[string]string` 更适合通用 canonicalization。它仍会消耗哈希、同步和对象内存，不应对攻击者可控的高基数字符串无条件调用。

**8. 用 strconv 代替 fmt**

`fmt.Sprintf("%d", n)` 走通用格式化路径，`strconv.Itoa(n)` 或 `strconv.AppendInt` 更直接，通常减少 CPU 和分配。差距取决于格式、逃逸和工具链，不使用“固定慢一个数量级”的结论。

```go
package main

import (
    "fmt"
    "strconv"
)

func main() {
    n := 42
    fmt.Println(strconv.Itoa(n))       // 专用转换
    fmt.Println(fmt.Sprintf("%d", n)) // 通用格式化
}
```

性能优化速查表：

| 场景 | 推荐 |
|------|------|
| 拼接多个 string | `strings.Builder` + `Grow` |
| 用分隔符连接 | `strings.Join` |
| int/float 转 string | `strconv.Itoa` / `strconv.FormatFloat` |
| 大字符串取小片段长期持有 | `strings.Clone` |
| 频繁 map 查找 with []byte | 直接 `m[string(b)]` |
| 大量重复、需快速身份比较的 comparable 值 | `unique.Make`，并限制输入基数 |
| 二进制协议处理 | 全程 `[]byte`，最后转 string |

### 本章小结

- string 是只读字节序列，底层由 `stringStruct{str, len}` 组成，16 字节（64 位），传值只复制 header。
- Go 源码使用 UTF-8，`range string` 按 UTF-8 解码出 rune；string 本身仍可保存任意字节，`len(s)` 返回字节数。
- `rune = int32` 表示 Unicode 码点，`byte = uint8` 表示原始字节，二者是别名但语义不同。
- string 与 `[]byte` 的安全转换保证结果在语义上独立；当前编译器可在 map 查找等不可观察场景消除分配或拷贝，unsafe 原语则把不可变性与生命周期责任交给调用方。
- string 不可变带来内存安全、并发安全、map key 稳定等红利，代价是修改需借助 `[]byte` 或 Builder。
- `strings.Builder` 是高效拼接首选，注意不可值拷贝，用 `Grow` 预分配。
- 性能优化核心：减少分配、减少拷贝、避免转换、防 substring 泄漏。
