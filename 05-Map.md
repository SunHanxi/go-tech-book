## 第5章 Map（重点）

> 当前实现基线为 Go 1.26。Go 1.24 起，内置 map 默认使用位于 `internal/runtime/maps` 的 Swiss Table 实现。Go 1.23 及更早版本的 `hmap`、`bmap`、overflow bucket 和逐桶搬迁只在本章末尾作为历史对照。

Map 的语言语义受 Go 1 兼容性保护，但内部结构不是公开 API。理解实现的目的，是解释性能和边界，不是让业务代码依赖 Runtime 字段。

### 5.1 语言层语义

Go map 的类型是：

```go
map[Key]Value
```

Key 必须满足 `comparable`。布尔、数字、字符串、指针、channel、interface，以及字段都可比较的 array/struct 可以做 key；slice、map 和 function 不可以。

```go
counts := map[string]int{"go": 1}

value := counts["missing"]       // int 零值 0
value, ok := counts["missing"]   // 0, false
counts["go"]++
delete(counts, "missing")        // 删除不存在的 key 也是安全的
```

读取不存在的 key 返回 Value 的零值，因此需要区分“不存在”和“值恰好为零”时必须使用 comma-ok。

### 5.2 nil、clear 与可寻址性

```go
var nilMap map[string]int
emptyMap := make(map[string]int)

fmt.Println(len(nilMap), len(emptyMap)) // 0 0
fmt.Println(nilMap["x"])                // 0

// nilMap["x"] = 1 // panic: assignment to entry in nil map
emptyMap["x"] = 1
```

nil map 可以读取、删除、clear 和 range，但不能写入。API 若允许调用方追加内容，应返回已初始化 map。

Go 1.21 的 `clear(m)` 删除全部元素。它不承诺把已分配容量立即归还给操作系统；需要释放一个历史峰值很大的 map 时，通常丢弃整个 map，让 GC 回收：

```go
clear(cache)              // 复用已有结构
cache = make(map[K]V, 64) // 放弃历史大容量
```

Map 元素不可寻址，因为插入和扩容可能移动它：

```go
type Counter struct{ N int }

m := map[string]Counter{"x": {N: 1}}
value := m["x"]
value.N++
m["x"] = value

// m["x"].N++  // 编译错误
// _ = &m["x"] // 编译错误
```

需要原地修改时可存指针，但这会增加别名、并发和 GC 扫描成本。

### 5.3 为什么改用 Swiss Table

旧实现把 8 个槽组成 bucket，冲突过多时链接 overflow bucket。它简单可靠，但长 overflow 链会产生额外指针追踪和 cache miss。

Swiss Table 仍是哈希表，但使用**开放寻址 + 分组控制字**：

```text
group
+-------------------------+
| control bytes: 8 x uint8|
+-------------------------+
| slot 0: key, value      |
| slot 1: key, value      |
| ...                     |
| slot 7: key, value      |
+-------------------------+
```

每个 control byte 表示对应槽位的状态：

- occupied：最高位为 0，其余 7 bit 保存 H2。
- empty：该槽为空，探测可在这里终止。
- deleted：墓碑，查找必须继续，插入可复用。

一个机器字运算就能并行比较 8 个 control byte，快速筛出可能匹配的槽，随后才执行真正的 key 相等比较。这减少了无效 key 比较，并改善了局部性。

### 5.4 H1、H2 与查找

Runtime 为每个 map 生成独立随机 seed。Key 的哈希拆成两部分：

```go
H1 = hash >> 7 // 高位：选择 table、初始 group 和探测序列
H2 = hash & 0x7f // 低 7 bit：写入 control byte
```

查找流程：

1. 计算带 map seed 的 hash。
2. 由 hash 高位从 directory 选择 table。
3. 由 H1 选择初始 group。
4. 将 H2 与该 group 的 8 个 control byte 并行比较。
5. 对候选槽执行真正的 key 比较。
6. 没命中且 group 有 empty，说明 key 不存在；否则沿二次探测序列继续。

简化伪代码：

```go
func lookup(key K) (V, bool) {
    hash := hashKey(key, seed)
    table := directory.select(hash)
    probe := table.probe(H1(hash))

    for {
        group := probe.next()
        for slot := range group.matchH2(H2(hash)) {
            if group.key(slot) == key {
                return group.value(slot), true
            }
        }
        if group.hasEmpty() {
            return zero[V](), false
        }
    }
}
```

这里的伪代码只表达算法。真实入口会按 key 类型生成 fast path，并处理间接 key/value、写屏障和迭代状态。

### 5.5 插入、删除与墓碑

插入先执行与查找相同的探测：

- 找到相同 key：更新 value。
- 找不到：优先复用探测路径上的 tombstone，否则使用 empty 槽。
- 超过负载预算：对当前 table grow、rehash 或 split 后重试。

普通 table 的平均最大负载是 `7/8`。至少保留一个 empty 槽，才能保证失败查找会终止。只含一个 group 的 small map 没有跨 group 探测，可使用全部 8 个槽。

删除时不能总把槽标记为 empty。若当前 group 没有其他 empty，后续 key 可能位于同一探测链上；提前出现 empty 会让查找错误终止。因此：

- group 已有 empty：删除槽可直接变 empty。
- group 原本全满：删除槽标记为 tombstone。

Tombstone 会增加探测成本，后续插入优先复用；grow/rehash 会清理剩余墓碑。

### 5.6 Map、Table 与 Directory

Go 1.26 顶层结构的核心字段可概括为：

```go
// internal/runtime/maps/map.go，简化示意
type Map struct {
    used        uint64
    seed        uintptr
    dirPtr      unsafe.Pointer
    dirLen      int
    globalDepth uint8
    globalShift uint8
    writing     uint8
    clearSeq    uint64
}
```

- `used` 位于首字段，`len(m)` 可直接读取。
- `seed` 让不同 map 的哈希分布不同，降低构造碰撞攻击的可行性。
- `dirPtr` 指向 table directory；small map 时直接指向唯一 group。
- `globalDepth/globalShift` 决定用多少 hash 高位选择 table。
- `writing` 用于尽力检测并发写。
- `clearSeq` 帮助迭代器识别迭代期间发生的 clear。

Table 保存自己的 group 数组、容量、已用槽、剩余增长预算和 `localDepth`。多个连续 directory 项可以指向同一个 table，这是 extendible hashing 的关键。

### 5.7 扩容：table grow 与 split

开放寻址的探测序列依赖 group 数量，所以单个 table 扩容时必须重新排列该 table 的所有条目。为了避免整个大 map 一次性重哈希，Go 把 map 拆成多个有上限的 table：

1. Map 从一个 table 开始。
2. Table 未到上限时，容量翻倍并重哈希该 table。
3. Table 到达上限后分裂成两个 table，各负责一部分 hash 前缀。
4. 必要时 directory 翻倍，增加 `globalDepth`。
5. 其他 table 不受影响。

```text
directory, globalDepth=2

00 ----+
01 ----+--> table A, localDepth=1
10 --------> table B, localDepth=2
11 --------> table C, localDepth=2
```

这不是旧实现的“每次写搬几个 bucket”。当前实现一次重排一个有界 table，通过多 table 分裂把 map 级增长成本分散。预分配仍有价值，但 `make(map[K]V, hint)` 是容量提示，不是精确保留，也不构成不扩容保证。

### 5.8 遍历语义

Map 遍历顺序**未指定且实现会主动随机化**。每次输出不同是正常行为：

```go
for key, value := range m {
    use(key, value)
}
```

语言规范对同一 goroutine 内的遍历修改给出明确规则：

- 尚未遍历到的条目被删除后，不会产生该条目。
- 迭代中新增的条目可能出现，也可能不出现。
- 同一个条目不会因 grow 被返回两次。
- 已修改且未删除的条目返回其最新值。

当前迭代器在 table grow 后仍沿旧 table 决定遍历位置，再到新 table 查找最新 value 或确认删除；table split 和 directory grow 还需调整目录索引。这是当前 map 实现最复杂的部分之一。

需要稳定顺序时显式排序 key：

```go
keys := slices.Sorted(maps.Keys(m))
for _, key := range keys {
    fmt.Println(key, m[key])
}
```

不要把当前观察到的顺序写进测试或序列化协议。

### 5.9 Map 为什么不支持 ==

Map 只能与 nil 比较。内容相等需要 O(n)，而 `==` 通常应是简单、可预测的语言操作；此外 value 还可能不可比较。

Go 1.21+ 对可比较 value 可用 `maps.Equal`：

```go
a := map[string]int{"x": 1}
b := map[string]int{"x": 1}
fmt.Println(maps.Equal(a, b)) // true
```

Value 不可比较时使用 `maps.EqualFunc`。`reflect.DeepEqual` 会递归比较，但 nil map 与空 map 不相等，并且其语义未必符合领域需求。导出类型更适合定义显式 `Equal`。

### 5.10 并发安全

多个 goroutine 只读同一个不再变化的 map 是安全的；任一 goroutine 写入时，所有访问都必须遵守同一同步协议。

当前实现用 `writing` 状态尽力检测并发读写或并发写，并可能以以下 fatal 终止进程：

- `concurrent map writes`
- `concurrent map read and map write`
- `concurrent map iteration and map write`

这不是同步机制，也不保证发现所有 race。不能依赖“没 fatal”判断安全，必须运行 `go test -race` 并建立[第18章](./18-Go内存模型与数据竞争.md)所述 happens-before 关系。

最常见封装：

```go
type SafeMap[K comparable, V any] struct {
    mu sync.RWMutex
    m  map[K]V
}

func (s *SafeMap[K, V]) Load(key K) (V, bool) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    value, ok := s.m[key]
    return value, ok
}

func (s *SafeMap[K, V]) Store(key K, value V) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.m[key] = value
}
```

读写是否用 Mutex、RWMutex、分片或 copy-on-write，必须按真实竞争和不变量 benchmark，不能只按“读多写少”口号选择。

### 5.11 sync.Map

`sync.Map` 针对两类场景优化：条目只写一次但读很多次，或不同 goroutine 操作互不相交的 key 集。普通 map + 锁通常有更好的类型安全和不变量表达，应作为默认起点。

常用原子操作：

```go
var m sync.Map

actual, loaded := m.LoadOrStore("key", value)
old, swapped := m.CompareAndSwap("key", actual, replacement)
value, loaded := m.LoadAndDelete("key")
m.Clear() // Go 1.23+
```

`CompareAndSwap` / `CompareAndDelete` 的 old 必须可比较。`Range` 不是一致性快照：一次遍历可能观察到各 key 在不同时间点的值，回调返回 false 时停止；即便很早停止，API 仍允许其复杂度达到 O(N)。

Go 1.26.4 的 `sync.Map` 是 `internal/sync.HashTrieMap[any, any]` 的包装，不再是旧资料中的 `read/dirty/miss` 双表。当前哈希 trie 的内部节点有原子发布的子指针：读取沿哈希路径遍历，修改锁住相关内部节点并发布新节点，哈希完全冲突时使用 overflow 链；`Clear` 通过替换根节点清空。内部结构不是兼容性承诺，选型仍应以公开语义和 benchmark 为准。

`sync.Map` 的 key 必须可比较，且它没有 `Len`；涉及多 key 不变量、类型安全或一致性快照时，普通 `map[K]V` 配合锁通常更合适。

### 5.12 性能与内存

平均查找、插入、删除是 O(1)，最坏情况仍可能退化。影响实际性能的主要因素：

- key 哈希与相等比较成本。
- 负载率、tombstone 和探测长度。
- key/value 大小、间接存储和 GC 指针数量。
- grow/rehash 时机。
- cache 局部性与并发同步。

不要引用跨机器的固定纳秒结论。用真实 key/value 和读写比例测试：

```go
func BenchmarkLookup(b *testing.B) {
    m := make(map[int]int, 1024)
    for i := range 1024 {
        m[i] = i
    }

    var sink int
    for b.Loop() {
        sink = m[511]
    }
    _ = sink
}
```

对几十个以内且频繁顺序遍历的小集合，排序 slice 可能更省内存、更 cache 友好；结论仍需测量。

### 5.13 工程实践

**预分配**

```go
result := make(map[string]Item, len(input))
```

已知数量时传 hint 可减少 grow，但估得过大也会浪费内存。

**集合**

```go
set := make(map[string]struct{})
set["go"] = struct{}{}
_, exists := set["go"]
```

**复制与过滤**

```go
clone := maps.Clone(original) // 浅拷贝
maps.DeleteFunc(clone, func(key string, value Item) bool {
    return value.Expired()
})
```

Map、slice、pointer、interface 作为 value 时，Clone 不复制其指向对象。

**JSON**

动态 JSON 使用 `map[string]any` 时，默认数字进入 `float64`。需要保留数值文本或大整数时用 `json.Decoder.UseNumber`；稳定协议优先定义 struct。

**浮点 key**

浮点类型语法上可做 key，但 NaN 不等于自身，插入后的 NaN key无法通过普通查找再次命中，也难以删除。除非明确接受 IEEE 语义，否则避免使用。

**不要依赖内部布局**

使用 `unsafe` 读取 Map、table 或 group 字段会随 Go 版本失效，并可能破坏 GC。调优使用 benchmark、pprof 和 trace，不使用 Runtime 私有指针。

### 5.14 旧实现对照

| Go 1.23 及更早 | Go 1.24+ |
|---|---|
| `runtime.hmap` | `internal/runtime/maps.Map` |
| `bmap`，每 bucket 8 槽 | group，8 槽 + 8 control bytes |
| bucket + overflow 链 | 开放寻址 + 二次探测 |
| `tophash` | H2 control byte |
| load factor 约 6.5 | 普通 table 最大平均负载 7/8 |
| map 级翻倍/等量 grow | table grow、rehash、split + directory |
| 每次写渐进搬迁旧 bucket | 一次重排有界 table，map 由多 table 分摊增长 |

旧资料中的 `B`、`oldbuckets`、`nevacuate`、overflow bucket 仍有历史价值，但不能用于解释当前 Go 1.26 的 profile、内存布局或扩容路径。

### 本章小结

- Go 1.24+ map 使用 Swiss Table：H2 control bytes 并行筛选 8 个槽，H1 驱动 table/group 选择和二次探测。
- Tombstone 保持探测链正确；table grow/rehash 与 extendible-hashing directory 共同控制增长成本。
- 遍历顺序未指定，迭代期间删除和新增有明确语言语义，但并发写仍是 race。
- 普通 map 默认不并发安全；`sync.Map` 只适合特定访问模式。
- 预分配、key/value 设计和实测比依赖旧 bucket 常数更重要。

进一步阅读：

- [Go map implementation, Go 1.26](https://cs.opensource.google/go/go/+/refs/tags/go1.26.0:src/internal/runtime/maps/map.go)
- [Abseil Swiss Tables](https://abseil.io/about/design/swisstables)
- [Go specification: map types](https://go.dev/ref/spec#Map_types)
