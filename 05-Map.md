## 第5章 Map（重点）

> 本章系统剖析 Go Map 的实现：从 `hmap`/`bmap` 数据结构、bucket 与 overflow 链、渐进式扩容、随机遍历，到并发不安全与 `sync.Map` 的设计取舍，帮助你写出高性能且无并发 bug 的 Map 代码。

### 5.1 为什么需要 Map

**是什么**

Map（映射、字典、关联数组）是一种存储"键值对"（key-value pair）的数据结构，支持通过 key 快速查找、插入、删除 value。Go 的内置 map 类型签名：

```go
map[KeyType]ValueType
```

其中 `KeyType` 必须是可比较类型（`comparable`），`ValueType` 任意。

**为什么这样设计 / 底层实现要点**

为什么需要 Map 这种数据结构？对比其他选择：

| 数据结构 | 查找复杂度 | 插入复杂度 | 删除复杂度 | 适用场景 |
|---|---|---|---|---|
| 数组/Slice | O(n) 顺序查找 | O(1) 末尾 / O(n) 中间 | O(n) | 有序、按下标访问 |
| 排序数组 | O(log n) 二分 | O(n) | O(n) | 静态、少量查找 |
| 二叉搜索树 | O(log n) 平均 | O(log n) 平均 | O(log n) 平均 | 动态、需有序遍历 |
| HashMap | O(1) 平均 | O(1) 平均 | O(1) 平均 | 通用 key-value 存储 |

HashMap 在"无序、key 任意、查找频繁"的场景下完胜其他结构。Go 的 map 实现就是基于 HashMap，使用 **拉链法**（separate chaining）处理哈希冲突，每个 bucket 容纳 8 个 KV 对。

**工程实践与常见坑**

Map 在 Go 工程中无处不在：

- 计数：`map[string]int` 统计词频。
- 去重：`map[T]struct{}` 当集合用。
- 缓存：`map[K]V` 缓存计算结果。
- 配置：`map[string]any` 解析 JSON 配置。
- 路由：HTTP 路由表 `map[string]HandlerFunc`。

> 注意：Map 不是并发安全的，多 goroutine 读写必须用 `sync.Map` 或加锁，详见 5.9、5.10 节。

### 5.2 HashMap 基础

**是什么**

HashMap 通过哈希函数把 key 映射到数组下标，实现 O(1) 平均访问。但不同 key 可能映射到同一位置（哈希冲突），需要冲突处理策略。

**为什么这样设计 / 底层实现要点**

**核心三件套**：哈希函数 + 桶数组 + 冲突处理。

**哈希函数**：把任意 key 映射到固定宽度的整数（Go 用 64 位）。Go Runtime 内置针对不同 key 类型的哈希函数（`runtime/alg.go`），并用 `hmap.hash0` 作为随机种子防止哈希攻击。

**桶数组**：长度为 `2^B` 的数组，每个槽位叫一个 bucket。下标 = `hash & (2^B - 1)`（取低 B 位）。

**冲突处理的两种主流方案**：

1. **开放寻址法**（Open Addressing）：冲突时按某种策略（线性探测、二次探测、Robin Hood 等）找下一个空槽。优点：缓存友好；缺点：删除复杂、聚集问题。Python dict、Lua table 用此法。

2. **拉链法**（Separate Chaining）：每个桶存一个链表，冲突元素挂在链表上。优点：实现简单、删除方便、装载因子可以超过 1；缺点：链表节点分散，缓存不友好。Java HashMap、C++ `std::unordered_map` 用此法。

**Go 的改进**：Go 的 bucket 不是"一个槽一个元素"，而是"一个桶 8 个槽"——bucket 内是数组，bucket 间才用链表（overflow bucket）。这种"数组+链表"混合方案兼顾缓存友好与冲突容忍：

```
[hash & (2^B-1)] -> bucket[8] -> overflow bucket[8] -> overflow bucket[8] -> nil
```

**装载因子（Load Factor）**：`count / 2^B`。Go 的装载因子阈值是 `6.5`（`loadFactorNum/loadFactorDen = 13/2`）。超过阈值或 overflow bucket 过多时触发扩容。

> 为什么 Go 选 6.5？这是 Go 团队基于大量基准测试得出的经验值：太小浪费内存，太大查找变慢。8 槽 bucket 配 6.5 装载因子意味着平均每个桶装 6.5 个元素，overflow 链平均不到 1 节，兼顾内存与性能。

**工程实践与常见坑**

- **key 必须可比较**：Go 中只有 `comparable` 类型能做 map key（`==` 和 `!=` 必须可用）。slice、map、function 不能做 key。指针、interface 可以但要注意 nil 与动态类型。
- **key 的哈希分布影响性能**：如果自定义类型的 `==` 实现差，可能造成大量冲突。Go 内置类型不必担心。
- **不要用浮点数做 key**：浮点 `==` 不可靠（NaN != NaN，且精度问题），虽然语法允许，但行为反直觉。

### 5.3 hmap 结构

**是什么**

`hmap` 是 Go map 的运行时顶层结构，定义在 `runtime/map.go`。每个 `make(map[K]V)` 在运行时对应一个 `*hmap`。

**为什么这样设计 / 底层实现要点**

Go 1.21 中 `hmap` 的定义（简化）：

```go
// runtime/map.go
const (
    bucketCntBits = 3
    bucketCnt     = 1 << bucketCntBits // 8
)

type hmap struct {
    count     int            // map 中元素个数，len() 直接读这个字段
    flags     uint8          // 状态标志位，如 hashWriting（并发写检测）
    B         uint8          // 桶数 = 2^B
    noverflow uint16         // overflow bucket 的近似数量
    hash0     uint32         // 哈希种子，防止哈希攻击

    buckets    unsafe.Pointer // 当前桶数组，长度 2^B；count==0 时可能为 nil
    oldbuckets unsafe.Pointer // 扩容时的旧桶数组，长度 2^(B-1)，扩容完毕后置 nil
    nevacuate  uintptr        // 渐进式扩容进度指针：下标 < nevacuate 的桶已迁移

    extra *mapextra // 可选字段，存放 overflow bucket 池
}

type mapextra struct {
    overflow     *[]*bmap      // 当前桶数组的 overflow bucket 列表
    oldoverflow  *[]*bmap      // 旧桶数组的 overflow bucket 列表
    nextOverflow *bmap         // 下一个可用的预分配 overflow bucket
}
```

逐字段解释：

- **`count`**：map 中实际 KV 对数量。Go 内置 `len(m)` 编译为直接读这个字段，O(1)。`count == 0` 时 `buckets` 可以为 nil（懒分配）。注释明确写 "Must be first (used by len() builtin)"——放在第一个字段是为了让 `len()` 编译出的指令无需偏移计算，性能最优。
- **`flags`**：状态位。最重要的是 `hashWriting`（位 2）：进入 map 写操作前会检查并设置它，写完后清除。如果检测到 `hashWriting` 已被设置，说明有并发写，触发 `concurrent map writes` panic。详见 5.9 节。
- **`B`**：桶数量的对数。`2^B` 是桶数组长度。B 的初始值由 `make(map[K]V, hint)` 的 `hint` 决定：估算需要的桶数，向上取整到 2 的幂，再取对数。
- **`noverflow`**：overflow bucket 的近似数量。用于判断是否触发"等量扩容"（same-size grow）。之所以是"近似"，是为了避免每次新增 overflow 都更新它（性能考虑）。详见 5.5 节。
- **`hash0`**：哈希种子。每个 map 实例创建时随机生成，混入哈希计算，防止恶意构造的 key 触发大量冲突（哈希碰撞攻击）。
- **`buckets`**：当前桶数组指针，指向一段连续内存，里面是 `2^B` 个 `bmap`。空 map（`make(map[K]V)` 不带 hint）可能延迟到第一次插入才分配。
- **`oldbuckets`**：扩容过程中的旧桶数组。扩容开始时 `buckets` 指向新数组，`oldbuckets` 指向旧数组；渐进式迁移每完成一桶 `nevacuate++`；全部迁移完后 `oldbuckets` 置 nil。
- **`nevacuate`**：渐进式扩容进度。下标小于它的旧桶已被迁移到新数组。查找时若 key 命中的旧桶未迁移，需在旧数组里找。
- **`extra`**：overflow 管理结构。预分配 overflow bucket 池，避免每次冲突都调用 `mallocgc`。`nextOverflow` 是一个指针，从预分配池里取出下一个空闲 bucket。

**为什么用 `B` 而非直接存桶数**：桶数永远是 2 的幂，用 `B` 可以用位运算 `hash & (2^B - 1)` 算下标，比取模快。`B` 是 `uint8`，最大 255，意味着理论上最多 `2^255` 桶（实际受内存限制远达不到）。

**工程实践与常见坑**

- **空 map 与 nil map 的区别**：

```go
package main

import "fmt"

func main() {
    var m1 map[string]int       // nil map
    m2 := make(map[string]int)  // 空 map

    fmt.Println(m1 == nil, m2 == nil) // true false
    fmt.Println(len(m1), len(m2))     // 0 0
    fmt.Println(m1["a"], m2["a"])     // 0 0（读取都 OK）

    m2["a"] = 1                    // OK
    // m1["a"] = 1                 // panic: assignment to entry in nil map
}
```

`nil map` 可以读、可以 `range`、可以 `len`，但不能写。所以函数返回 `map[T]V` 时，若调用方可能写入，应返回 `make(map[T]V)` 而非 `nil`。

- **`make(map[K]V, hint)` 的 hint 要靠谱**：hint 决定初始 B，估太小会频繁扩容，估太大会浪费内存。Go Runtime 会根据 hint 计算合适的 B，但不会"看 hint 是 0 就不分配"——还是会预分配少量桶。

### 5.4 bucket

**是什么**

`bmap` 是 map 的桶结构，每个桶最多放 8 个 KV 对。`bmap` 在源码里看起来只有一个字段，但实际内存布局要复杂得多。

**为什么这样设计 / 底层实现要点**

`bmap` 的源码定义：

```go
// runtime/map.go
type bmap struct {
    // tophash generally contains the top byte of the hash value
    // for each key in this bucket. If tophash[0] < minTopHash,
    // tophash[0] is a bucket evacuation state instead.
    tophash [bucketCnt]uint8 // bucketCnt = 8
}
```

看起来只有一个字段？这是因为 Go Runtime 用 **强转** 在这块内存上构造了完整布局。一个完整 bucket 的内存布局实际是：

```
+----------------------+ <- bmap 起始地址
| tophash [8]uint8     | 8 字节，存每个槽的 tophash
+----------------------+
| keys [8]KeyType      | 8 个 key 连续存放
+----------------------+
| values [8]ValueType  | 8 个 value 连续存放
+----------------------+
| overflow *bmap       | overflow 指针（仅当有 overflow 时存在）
+----------------------+
```

为什么 keys 和 values 分开存放，而不是 `[8]struct{K, V}` 交错存放？

考虑 `map[int64]int8`。如果交错存放，每个 KV 对要按 `int64` 对齐到 8 字节，`int8` 后面有 7 字节填充，每对占 16 字节，8 对共 128 字节。

分开存放：keys 区 8×8=64 字节，values 区 8×1=8 字节，共 72 字节（再加 tophash 8 字节 + overflow 指针 8 字节 = 88 字节）。**节省 40 字节**。

这就是 keys/values 分离的核心理由：**消除因 key 和 value 大小不一致带来的对齐填充**。

**tophash 的作用**：

`tophash[i]` 存储第 i 个槽位 key 的哈希值高 8 位。查找时先比对 `tophash`，匹配再比对完整 key。8 位整数比较远比完整 key 比较快（尤其 key 是长字符串时），起到 **快速过滤** 作用。

哈希值高 8 位有 256 种可能，桶里 8 个槽即使全满，平均只有 8/256 = 3% 概率 tophash 匹配但 key 不同（假阳性）。绝大多数不匹配的 key 在 tophash 阶段就被排除。

**几个特殊的 tophash 值**（正常 tophash 是 0~255 的某个值，但 Runtime 保留了一些值作标记）：

```go
const (
    emptyRest      = 0 // 该槽空，且后续 overflow 链也都空（查找可提前终止）
    emptyOne       = 1 // 该槽空，但后续可能有数据
    evacuatedX     = 2 // 扩容中，该桶已迁移到新数组前半部分
    evacuatedY     = 3 // 扩容中，该桶已迁移到新数组后半部分
    evacuatedEmpty = 4 // 扩容中，该桶原本就是空的
    minTopHash     = 5 // 正常 tophash 的最小值，低于 5 都是标记
)
```

这些标记让 Runtime 在渐进式扩容时能判断每个 bucket 的迁移状态（详见 5.6 节）。

**为什么 bucket 容量是 8**？Go 团队的权衡：

- 太小（如 1）：退化为纯链表，cache 不友好，overflow 指针开销大。
- 太大（如 16、32）：单 bucket 内存大，小 map 浪费；删除后内存难复用。
- 8 是经验最优值：64 字节级别，能装下多数小 KV 对，cache 友好，overflow 链短。

**查找流程**（`mapaccess` 简化伪代码）：

```go
func mapaccess(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
    hash := t.hasher(key, uintptr(h.hash0))
    m := bucketMask(h.B) // 2^B - 1
    b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.bucketsize)))
    if c := h.oldbuckets; c != nil {
        // 扩容中：可能在旧数组
        if !evacuated(c) {
            b = (*bmap)(add(c, (hash&bucketMask(h.B-1))*uintptr(t.bucketsize)))
        }
    }
    top := tophash(hash)
    for ; b != nil; b = b.overflow(t) {
        for i := 0; i < bucketCnt; i++ {
            if b.tophash[i] != top {
                if b.tophash[i] == emptyRest {
                    return unsafe.Pointer(&zeroVal[0]) // 提前终止
                }
                continue
            }
            k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
            if t.key.equal(key, k) {
                e := add(unsafe.Pointer(b), dataOffset+
                    bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
                return e
            }
        }
    }
    return unsafe.Pointer(&zeroVal[0])
}
```

逐行要点：

1. 用 `t.hasher` 计算 64 位哈希，混入 `hash0`。
2. `hash & bucketMask(B)` 取低 B 位作为桶下标。
3. 如果正在扩容且该桶未迁移，去旧数组找。
4. 计算 `tophash`（高 8 位），遍历 bucket 及 overflow 链。
5. tophash 不匹配时遇到 `emptyRest` 提前终止（后续都是空）。
6. tophash 匹配再用 `t.key.equal` 比对完整 key。
7. 都不匹配返回零值指针。

**工程实践与常见坑**

- **小 map 也有固定开销**：每个 bucket 至少 8 字节 tophash + 8 个 key + 8 个 value + overflow 指针。`map[int8]int8` 一个 bucket 也要 8+8+8+8=32 字节起。少量元素的 map 用 `[]struct{K; V}` 切片可能更省内存。
- **`make(map[K]V, n)` 与 `make([]T, n)` 不同**：map 的 n 是 hint，不是精确数量，实际桶数是 `2^B`。
- **不要直接操作 `hmap` 内部**：`unsafe.Pointer` 操作 map 内部极易出 bug，且 Go 版本升级会破坏兼容性。

### 5.5 overflow bucket

**是什么**

当某个 bucket 的 8 个槽都满了，再插入该 bucket 对应哈希范围的 key 时，Runtime 会分配一个新 bucket 挂在原 bucket 的 overflow 指针上，形成 overflow 链。

**为什么这样设计 / 底层实现要点**

**overflow 链的结构**：

```
buckets[i] -> bmap{8 KV} -> overflow bmap{8 KV} -> overflow bmap{8 KV} -> nil
```

**overflow bucket 的分配**（简化伪代码）：

```go
func (h *hmap) newoverflow(t *maptype, b *bmap) *bmap {
    var ovf *bmap
    if h.extra != nil && h.extra.nextOverflow != nil {
        // 从预分配池取
        ovf = h.extra.nextOverflow
        if ovf.overflow(t) == nil {
            // 池里还有下一个
            h.extra.nextOverflow = (*bmap)(add(unsafe.Pointer(ovf),
                uintptr(t.bucketsize)))
        } else {
            // 池用完了，这是最后一个预分配的
            h.extra.nextOverflow = nil
            ovf.setoverflow(t, nil)
        }
    } else {
        // 池没有，临时分配
        ovf = (*bmap)(newobject(t.bucket))
    }
    h.incrnoverflow()
    b.setoverflow(t, ovf)
    return ovf
}
```

设计要点：

1. **预分配池**：`map` 初始化时（`make` 带 hint），如果 hint 较大，会一次性分配桶数组 + 若干预分配 overflow bucket，挂到 `extra.nextOverflow`。这样后续插入冲突时直接从池里取，避免每次都调 `mallocgc`（减少 GC 压力）。
2. **池的内存布局**：预分配的 overflow bucket 紧跟在主桶数组后面，连续内存。每个预分配 bucket 的 overflow 指针临时指向"下一个预分配 bucket"，作为链表使用；最后一个的 overflow 指针为 nil，表示池用完。
3. **`incrnoverflow`**：更新 `h.noverflow`。但不是每次都更新，而是概率性更新：

```go
func (h *hmap) incrnoverflow() {
    if h.B < 16 {
        h.noverflow++
    } else if h.B > 15 {
        // 大 map 直接按 2^(B-15) 步进，避免溢出 uint16
        h.noverflow += uint16(1 << (h.B - 15))
    } else {
        // 1/2 概率更新
        if fastrand()&1 == 0 {
            h.noverflow++
        }
    }
}
```

大 map（B 大）的 overflow 数量可能超过 `uint16` 范围（65535），所以用概率采样近似。这种近似足够触发"等量扩容"的判断（见 5.6）。

**overflow 与扩容触发**：

```go
func tooManyOverflowBuckets(noverflow uint16, B uint8) bool {
    if B > 15 {
        B = 15
    }
    return noverflow > uint16(1)<<B
}
```

当 `noverflow >= 2^min(B, 15)` 时，触发 **等量扩容**（same-size grow）：B 不变，重新分配桶数组，把所有 KV 重新哈希到新桶里，把 overflow 链"压平"。这是处理"大量删除后 overflow 链长但元素少"的情况。

**工程实践与常见坑**

- **大量删除后内存不释放**：`delete(m, k)` 只是把对应槽标记为 `emptyOne`/`emptyRest`，bucket 和 overflow bucket 的内存不会立即释放。需要等量扩容才会"压缩"。如果 map 历史上很大、现在很小，建议新建一个 map 把数据搬过去，让老 map 被 GC。
- **不要无脑 `make(map[K]V, hugeHint)`**：hint 过大会预分配大量桶和 overflow，浪费内存。
- **overflow 链过长影响性能**：查找一个 key 最坏要遍历整条 overflow 链。如果你的 key 哈希分布差，链可能很长。`map[string]V` 的字符串哈希通常分布良好，但自定义类型要小心。

### 5.6 渐进式扩容

**是什么**

Go map 的扩容是 **渐进式** 的：扩容开始时只分配新桶数组、设置 `oldbuckets`，然后每次 map 操作（插入、删除）时迁移少量桶，直到全部迁移完毕。这与 Redis rehash、Java HashMap resize 类似，目的是避免一次性扩容造成的延迟尖峰。

**为什么这样设计 / 底层实现要点**

**两种扩容**：

1. **翻倍扩容（doubling grow）**：当 `count > loadFactor * 2^B`（即 `count > 6.5 * 2^B`）时触发。B 加 1，桶数组翻倍。目的是降低装载因子，缓解 overflow。

2. **等量扩容（same-size grow）**：当 overflow bucket 过多（`noverflow >= 2^min(B,15)`）但元素不多时触发。B 不变，桶数组大小不变，但重新分配内存、重新哈希所有 KV，把分散在 overflow 链上的数据"压"回主桶。目的是清理删除留下的碎片。

**触发点**：`mapassign`（写入）时检查。`mapaccess`（读取）不触发扩容，但如果正在扩容会顺带做一点迁移工作（`growWork`）。

**`hashGrow` 启动扩容**（简化伪代码）：

```go
func hashGrow(t *maptype, h *hmap) {
    bigger := uint8(1)
    if !overLoadFactor(h.count+1, h.B) {
        // 装载因子没超，是等量扩容
        bigger = 0
        h.flags |= sameSizeGrow
    }
    oldbuckets := h.buckets
    newbuckets, nextOverflow := makeBucketArray(t, h.B+bigger, nil)

    h.B += bigger
    h.flags ^= sameSizeGrow   // 翻转标志位
    h.oldbuckets = oldbuckets
    h.buckets = newbuckets
    h.nevacuate = 0
    h.noverflow = 0

    if h.extra != nil && h.extra.overflow != nil {
        h.extra.oldoverflow = h.extra.overflow
        h.extra.overflow = nil
    }
    if nextOverflow != nil {
        h.extra.nextOverflow = nextOverflow
    }
}
```

要点：

- 先决定 `bigger`（0 或 1）。
- 分配新桶数组（可能含预分配 overflow）。
- 把旧数组挪到 `oldbuckets`，新数组放到 `buckets`，`nevacuate = 0`。
- overflow 列表也跟着挪到 `oldoverflow`。

**`growWork` 渐进迁移**（简化伪代码）：

```go
func growWork(t *maptype, h *hmap, bucket uintptr) {
    // 迁移当前 bucket 对应的旧桶
    evacuate(t, h, bucket&h.oldbucketmask())

    if h.growing() {
        // 顺带推进一个桶
        h.nevacuate++
        evacuate(t, h, h.nevacuate)
    }
}
```

每次 `mapassign`/`mapdelete` 时：

1. 迁移当前 key 对应的旧桶。
2. 顺带迁移 `nevacuate` 指向的桶（按顺序推进）。

这样高频写入的 map 会快速完成迁移；冷 map 则靠后续操作慢慢推。注意：**纯读不迁移**，所以一个"写一次后只读"的 map 在扩容期间会一直保留 `oldbuckets`，直到下一次写操作触发迁移。这也是为什么 5.4 节查找代码要处理"扩容中可能在旧数组"的情况。

**`evacuate` 单桶迁移要点**：

- 翻倍扩容时，每个旧桶的 KV 会被分到新数组的两个桶（原下标、原下标+oldbucket 数量）。判断依据是哈希值的第 B 位（旧 B）。
- 等量扩容时，KV 仍去同一个下标，只是 bucket 重新分配、overflow 链重组。
- 旧桶的 tophash 被改成 `evacuatedX`/`evacuatedY` 标记，表示"已迁移"，便于查找时跳过。

**工程实践与常见坑**

- **扩容期间读写性能抖动**：迁移工作是分摊到多次操作里的，但单次操作可能触发两次 `evacuate`，比平时慢。对延迟敏感的场景，预估容量避免运行期扩容。
- **冷 map 内存占用**：扩容期间 `oldbuckets` 不释放，内存占用接近 2 倍。如果一个 map 只在初始化时写入大量数据、之后只读，建议初始化后用 `make(newMap, len(old))` 拷贝一次，丢弃老 map。
- **不能依赖"扩容时机"**：Runtime 何时触发扩容是黑盒，不要写"扩容后才能正确读"这种代码——map 的对外语义在扩容期间完全正确。

### 5.7 为什么遍历顺序随机

**是什么**

Go map 的 `for k, v := range m` 遍历，**每次的顺序都是随机的**，即使 map 内容不变。这是 Go 有意为之，与 Python 3.7+（保证插入顺序）、Java（HashMap 无序但不保证随机）不同。

**为什么这样设计 / 底层实现要点**

实现代码在 `runtime/map.go` 的 `mapiterinit`：

```go
func mapiterinit(t *maptype, h *hmap, it *hiter) {
    // ...
    r := uintptr(fastrand())
    if h.B > 0 {
        r >>= uintptr(60 - h.B) // 取高 B 位作起始 bucket
    }
    it.startBucket = r & bucketMask(h.B)
    it.offset = uint8(fastrand() & (bucketCnt - 1)) // 起始槽
    // ...
}
```

每次 `range` 开始时，Runtime 用 `fastrand` 随机选择一个起始 bucket 和起始槽位，从那里开始遍历。

**为什么这么设计**？官方理由有两层：

1. **防止用户依赖遍历顺序**：map 本质上是无序的（哈希分布决定位置，扩容会重排），如果 Go 保证某种"看似稳定"的顺序，用户就会写出依赖该顺序的代码，一旦实现细节变化（如扩容、Go 版本升级）代码就崩。Go 选择"主动随机"，从根源上杜绝依赖。

2. **历史教训**：早期 Go 版本 map 遍历顺序看起来稳定（但不保证），很多代码依赖了它，导致 Go 1.0 升级时大量代码出 bug。Go 团队在 Go 1.0 之前就引入随机遍历，从此再没人能依赖顺序。

**遍历的细节**：`hiter` 是迭代器结构，包含 `startBucket`、`offset`、`b`（当前 bucket）、`i`（当前槽）、`key`、`value` 等字段。`mapiternext` 沿着 bucket 顺序、overflow 链推进，遇到 `emptyRest` 跳过，遇到 `evacuatedX/Y`（扩容中）按迁移后的位置遍历。

**一个微妙的坑**：遍历中如果其他 goroutine 修改 map（写、删、扩容），可能触发 panic（`concurrent map iteration and map write`）。即使加锁，也要注意遍历中修改 map 的语义：

```go
package main

import "fmt"

func main() {
    m := map[int]int{1: 1, 2: 2, 3: 3}
    for k := range m {
        m[k+10] = k + 10 // 新增元素
        // 行为未定义：可能遍历到新元素，也可能不；可能扩容导致迭代器失效
    }
    fmt.Println(m)
}
```

Go spec 明确说：遍历过程中修改 map 的行为是未定义的。如果要在遍历中删除，Go 允许（删除当前 key 安全），但新增 key 不可预测。

**工程实践与常见坑**

- **需要有序遍历，单独维护 key 列表**：

```go
package main

import (
    "fmt"
    "sort"
)

func main() {
    m := map[string]int{"b": 2, "a": 1, "c": 3}
    keys := make([]string, 0, len(m))
    for k := range m {
        keys = append(keys, k)
    }
    sort.Strings(keys)
    for _, k := range keys {
        fmt.Println(k, m[k])
    }
}
```

- **插入顺序遍历，用第三方 ordered map**：标准库没有，社区有 `github.com/iancoleman/orderedmap` 等实现，原理是 map + slice 双维护。
- **不要靠"测试发现顺序稳定"就放心依赖**：随机种子由 `fastrand` 提供，理论上可能某次跑出来顺序一致。CI 多跑几次或换 Go 版本就暴露问题。

### 5.8 为什么 Map 不能 ==

**是什么**

Go 中 map 类型不能用 `==` 直接比较：

```go
package main

func main() {
    var m1, m2 map[string]int
    _ = m1 == m2 // 编译错误：invalid operation: m1 == m2 (map can only be compared to nil)
}
```

只能与 `nil` 比较：`m == nil`。

**为什么这样设计 / 底层实现要点**

Go spec 明确：map、slice、function 类型只能与 nil 比较，不能互相比较。原因：

1. **语义不明**：map 的 `==` 是"引用相等"还是"内容相等"？Java 用 `==` 表示引用相等，`equals` 表示内容相等，初学者经常混淆。Go 选择一刀切禁止，强制用 `reflect.DeepEqual` 表达"内容相等"。
2. **内容相等的代价高**：map 无序，比较两个 map 相等需要 O(n) 遍历，且每个 key 都要 `==`。Go 不愿意为 `==` 引入隐藏的 O(n) 操作。
3. **扩容导致位置变化**：即使两个 map 内容相同，元素在 bucket 里的位置可能不同（扩容、`hash0` 不同），引用比较无意义。
4. **可以作为 map 的 value，但不能做 key**：map 类型本身不满足 `comparable`，所以不能做另一个 map 的 key。

**如何比较两个 map 内容相等**：

```go
package main

import (
    "fmt"
    "reflect"
)

func main() {
    m1 := map[string]int{"a": 1, "b": 2}
    m2 := map[string]int{"a": 1, "b": 2}
    fmt.Println(reflect.DeepEqual(m1, m2)) // true

    m2["c"] = 3
    fmt.Println(reflect.DeepEqual(m1, m2)) // false
}
```

`reflect.DeepEqual` 递归比较，处理嵌套 map、slice、struct。但它有性能开销，热路径慎用。

**手动比较**（更快）：

```go
package main

import "fmt"

func mapEqual(a, b map[string]int) bool {
    if len(a) != len(b) {
        return false
    }
    for k, v := range a {
        if bv, ok := b[k]; !ok || bv != v {
            return false
        }
    }
    return true
}

func main() {
    m1 := map[string]int{"a": 1, "b": 2}
    m2 := map[string]int{"a": 1, "b": 2}
    fmt.Println(mapEqual(m1, m2)) // true
}
```

**slice 也类似**：slice 也不能 `==`（除 `[]byte` 可与 string 比较的特殊语法），原因相同。

**工程实践与常见坑**

- **结构体里含 map 字段，结构体也不能 `==`**：

```go
package main

type S struct {
    m map[int]int
}

func main() {
    var s1, s2 S
    _ = s1 == s2 // 编译错误：struct containing map[int]int cannot be compared
}
```

如果结构体需要比较，要么去掉 map 字段，要么自定义 `Equal` 方法。

- **map 作为函数参数判断"是否为空"**：用 `len(m) == 0`，不要用 `m == nil`（空 map 不等于 nil map）。
- **`reflect.DeepEqual` 对 nil map 和空 map 视为不等**：

```go
package main

import (
    "fmt"
    "reflect"
)

func main() {
    var m1 map[string]int      // nil
    m2 := map[string]int{}     // 空
    fmt.Println(reflect.DeepEqual(m1, m2)) // false
}
```

### 5.9 为什么并发不安全

**是什么**

Go map 不是并发安全的：多 goroutine 同时读写同一个 map 会触发运行时 panic（`concurrent map writes` 或 `concurrent map read and map write`），甚至可能导致 map 内部结构损坏。

**为什么这样设计 / 底层实现要点**

Go Runtime 在 `mapassign`（写）和 `mapaccess`（读）中通过 `hmap.flags` 的 `hashWriting` 位做检测：

```go
const (
    iterator     = 1  // 可能有线程在迭代
    oldIterator  = 2
    hashWriting  = 4  // 有线程在写
    sameSizeGrow = 8
)

func mapassign(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
    // ...
    if h.flags&hashWriting != 0 {
        fatal("concurrent map writes") // 不可恢复
    }
    // 计算 hash
    hash := t.hasher(key, uintptr(h.hash0))
    // 设置 hashWriting
    h.flags ^= hashWriting
    // ... 实际写入 ...
    // 清除 hashWriting
    h.flags &^= hashWriting
    return inserted
}

func mapaccess(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
    // ...
    if h.flags&hashWriting != 0 {
        fatal("concurrent map read and map write")
    }
    // ...
}
```

`hashWriting` 在写操作期间被设置。如果另一个 goroutine 此时也尝试写或读，检测到 `hashWriting` 已被设置，立即 `fatal`。注意 `fatal` 不是普通 `panic`，**不可 recover**。

为什么不加锁而用 `fatal`？Go 团队的考虑：

1. **性能**：加锁会让所有 map 操作变慢，绝大多数 map 都是单 goroutine 使用。给所有 map 加锁代价过高。
2. **暴露 bug**：并发写 map 几乎一定是 bug，与其让 bug 隐藏到不可控的状态损坏，不如直接 crash。
3. **不保证全部检测**：`hashWriting` 检测是尽力而为（best-effort），极端情况下仍可能漏检导致内存损坏。所以不要依赖"runtime 会报错"就放心写并发代码。

**为什么不直接做成并发安全**？Go 选择把"并发安全"留给 `sync.Map`（5.10 节），让普通 map 极致优化单线程性能。这与 Go 的 "don't pay for what you don't use" 哲学一致。

**遍历与写的并发检测**：`mapiternext` 同样检测 `hashWriting`，遍历中如果有其他 goroutine 写 map，触发 `concurrent map iteration and map write`。

**工程实践与常见坑**

- **并发安全方案一：`sync.RWMutex` + map**：

```go
package main

import "sync"

type SafeMap struct {
    mu sync.RWMutex
    m  map[string]int
}

func (s *SafeMap) Get(k string) (int, bool) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    v, ok := s.m[k]
    return v, ok
}

func (s *SafeMap) Set(k string, v int) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.m[k] = v
}

func NewSafeMap() *SafeMap {
    return &SafeMap{m: make(map[string]int)}
}
```

读多写少用 RWMutex；读写均衡甚至写多读少用 Mutex（RWMutex 写锁成本更高）。

- **方案二：`sync.Map`**：见 5.10 节。
- **方案三：分片 map（sharded map）**：高并发场景下，把数据按 key 哈希分到 N 个分片，每个分片一把锁，减少锁竞争。

```go
package main

import (
    "hash/fnv"
    "sync"
)

type Shard struct {
    mu sync.RWMutex
    m  map[string]int
}

type ShardedMap struct {
    shards []*Shard
    n      int
}

func NewShardedMap(n int) *ShardedMap {
    sm := &ShardedMap{shards: make([]*Shard, n), n: n}
    for i := range sm.shards {
        sm.shards[i] = &Shard{m: make(map[string]int)}
    }
    return sm
}

func (sm *ShardedMap) shard(k string) *Shard {
    h := fnv.New32a()
    _, _ = h.Write([]byte(k))
    return sm.shards[int(h.Sum32())%sm.n]
}

func (sm *ShardedMap) Set(k string, v int) {
    s := sm.shard(k)
    s.mu.Lock()
    defer s.mu.Unlock()
    s.m[k] = v
}

func (sm *ShardedMap) Get(k string) (int, bool) {
    s := sm.shard(k)
    s.mu.RLock()
    defer s.mu.RUnlock()
    v, ok := s.m[k]
    return v, ok
}
```

- **常见坑：在函数里悄悄开 goroutine 操作 map**：

```go
func process(m map[string]int, key string) {
    go func() {
        m[key] = compute() // 隐蔽的并发写！
    }()
}
```

调用方以为 `process` 是同步的，结果 map 被异步写。规则：**任何接收 map 参数的函数，文档里要明确是否还持有该 map 的引用**。

### 5.10 sync.Map

**是什么**

`sync.Map` 是 Go 标准库 `sync` 包提供的并发安全 map。与"加锁 map"相比，它针对 **读多写少、key 集合相对稳定** 的场景做了优化。

**为什么这样设计 / 底层实现要点**

`sync.Map` 的核心结构：

```go
// sync/map.go
type Map struct {
    mu Mutex

    // read 是 atomic.Value，存 readOnly 结构。读优先走这里，无锁。
    read atomic.Value

    // dirty 是带 mu 锁的 map，包含 read 中所有 entry + 新写入的 entry。
    dirty map[any]*entry

    // misses 是穿透 read 命中 dirty 的次数。
    // 达到阈值后把 dirty 升级为 read。
    misses int
}

type readOnly struct {
    m       map[any]*entry
    amended bool // dirty 包含 read 没有的 key
}

type entry struct {
    p atomic.Pointer[any] // 指针，可能指向实际值、nil（已删除）、expunged（被标记删除）
}

var expunged = any(new(interface{})) // 标记"已从 dirty 中清除"
```

逐字段解释：

- **`read`**：`atomic.Value` 存 `readOnly` 结构。读操作通过原子 Load 无锁访问。`readOnly.m` 是 `map[any]*entry`，注意 value 是 `*entry` 指针，多个 map 共享同一个 entry。
- **`dirty`**：普通 `map[any]*entry`，受 `mu` 保护。新写入的 key 先进 dirty。`amended == true` 表示 dirty 有 read 没有的 key。
- **`misses`**：read 没命中而需要查 dirty 的次数。达到 `len(dirty)` 时触发 `dirty -> read` 提升。
- **`entry.p`**：原子指针。三种状态：
  - 正常指针：指向实际值。
  - `nil`：逻辑删除（在 read 中标记，但 dirty 还有引用）。
  - `expunged`：彻底删除（dirty 提升为 read 时，原 nil entry 被标记为 expunged，禁止再写入 dirty）。

**读流程（`Load` 简化伪代码）**：

```go
func (m *Map) Load(key any) (value any, ok bool) {
    read, _ := m.loadReadOnly()
    e, ok := read.m[key]
    if !ok && read.amended {
        m.mu.Lock()
        // double-check（避免 TOCTOU）
        read, _ = m.loadReadOnly()
        e, ok = read.m[key]
        if !ok && read.amended {
            e, ok = m.dirty[key]
            m.missLocked() // miss 计数
        }
        m.mu.Unlock()
    }
    if !ok {
        return nil, false
    }
    return e.load()
}
```

要点：

1. 先无锁读 `read`。
2. 没命中且 `amended == true`（dirty 有额外 key），加锁查 dirty。
3. 加锁后 double-check（避免 TOCTOU）。
4. miss 计数，达到阈值触发提升。

**写流程（`Store`）**：

1. 先无锁尝试原子更新 `read` 中已有的 entry（命中且未删除时）。
2. 否则加锁，再次检查 read，必要时把 read 中 expunged 的 entry "un-expunge" 后写入 dirty。
3. 若 key 是新的，直接写 dirty，并在第一次写入时把 read 全量拷贝到 dirty（这是 `sync.Map` 的写放大点）。

**`missLocked` 触发提升**：

```go
func (m *Map) missLocked() {
    m.misses++
    if m.misses < len(m.dirty) {
        return
    }
    m.read.Store(readOnly{m: m.dirty})
    m.dirty = nil
    m.misses = 0
}
```

misses 达到 `len(dirty)` 时，dirty 升级为 read，dirty 清空。

**适用场景**：

| 场景 | 是否适合 sync.Map |
|---|---|
| 多读少写、key 集合稳定 | 非常适合，read 几乎全命中，无锁 |
| 写多读少 | 不适合，频繁触发 dirty 全量拷贝 |
| key 不断新增 | 不适合，每次新 key 都要加锁写 dirty |
| 多 goroutine 操作不相交的 key 子集 | 适合，dirty 锁竞争小 |
| 需要有序遍历 | 不适合（sync.Map 遍历也不保证顺序） |

**工程实践与常见坑**

- **`Range` 期间修改安全但快照可能不一致**：`sync.Map.Range` 会先提升 dirty 到 read（如果 amended），然后遍历 read。遍历中对 entry 的修改可见，但新 key 可能不可见。
- **不要用 `sync.Map` 替代所有 map**：写多场景下，`sync.Map` 性能可能比 `RWMutex + map` 还差。基准测试再选。
- **`LoadOrStore` 是原子的**：常用于单次初始化缓存。
- **`Delete` 不会立即释放内存**：与普通 map 一样，标记删除，等下次 dirty 提升才清理。

### 5.11 Map 性能分析

**是什么**

本节从时间复杂度、内存开销、哈希函数开销三个维度分析 map 性能，给出 Benchmark 数据。

**为什么这样设计 / 底层实现要点**

**时间复杂度**：

| 操作 | 平均 | 最坏 |
|---|---|---|
| `m[k] = v`（已存在） | O(1) | O(n)（overflow 链长） |
| `m[k] = v`（新 key） | O(1) + 可能扩容 | O(n) |
| `m[k]` / `delete` | O(1) | O(n) |
| `len(m)` | O(1) | O(1) |
| `range m` | O(n) | O(n) |

最坏情况出现在 overflow 链很长时。Go 的装载因子阈值 6.5 + 8 槽 bucket，让平均链长 < 1，最坏链长通常也只有几节。但极端构造的 key（哈希攻击）可能让所有 key 落到同一 bucket，退化为 O(n)。`hash0` 随机种子是主要的防御手段。

**内存开销**：

每个 bucket 大小 = `8 (tophash) + 8 * sizeof(K) + 8 * sizeof(V) + 8 (overflow ptr)`，对齐到 8 字节。

`map[int64]int64`：8 + 64 + 64 + 8 = 144 字节。装满 8 对，每对 18 字节，比裸 `[]int64` 对（16 字节）多 12.5%。但因为 bucket 是连续分配，cache 友好。

`map[string]string`：8 + 8*16(string header) + 8*16 + 8 = 264 字节。注意 string header 是 16 字节（指针 + 长度），实际字符串内容在另一处分配。

**哈希函数开销**：

`map[string]V` 用 `runtime.aeshashstr`（AES 指令加速，AMD64）。`map[int64]V` 用 `runtime.aeshash64`。这些函数利用 CPU AES 指令，单次哈希几纳秒。`hash0` 混入防止攻击。

**Benchmark 对比**：

```go
package main

import (
    "sync"
    "testing"
)

func BenchmarkMapRead(b *testing.B) {
    m := make(map[int]int, 1000)
    for i := 0; i < 1000; i++ {
        m[i] = i
    }
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        _ = m[i%1000]
    }
}

func BenchmarkRWMutexMapRead(b *testing.B) {
    var mu sync.RWMutex
    m := make(map[int]int, 1000)
    for i := 0; i < 1000; i++ {
        m[i] = i
    }
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        mu.RLock()
        _ = m[i%1000]
        mu.RUnlock()
    }
}

func BenchmarkSyncMapRead(b *testing.B) {
    var m sync.Map
    for i := 0; i < 1000; i++ {
        m.Store(i, i)
    }
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        _, _ = m.Load(i % 1000)
    }
}
```

典型结果（Go 1.21，AMD64）：

| Benchmark | 时间/操作 | 说明 |
|---|---|---|
| `MapRead` | ~5 ns | 单线程无锁 |
| `RWMutexMapRead` | ~15 ns | RLock 开销 |
| `SyncMapRead` | ~10 ns | atomic Load |

单线程裸 map 最快；`sync.Map` 比 `RWMutex + map` 略快（因为 read 路径无锁）。多线程下 `sync.Map` 优势更明显。

**工程实践与常见坑**

- **预分配 `make(map[K]V, hint)`**：减少扩容。
- **小 map 用 slice 可能更快**：元素 < 几十个时，线性搜索 `[]struct{K; V}` 可能比 map 快（cache 友好、无哈希开销）。
- **避免 `map[interface{}]interface{}`**：类型断言 + 接口装箱开销大，且失去类型安全。
- **value 大对象用指针**：`map[K]*BigStruct` 比 `map[K]BigStruct` 更省拷贝开销，但增加一次指针解引用和 GC 压力，需权衡。

### 5.12 Map 最佳实践

**是什么**

本节汇总 map 工程实践要点。

**为什么这样设计 / 底层实现要点**

所有要点源自前面的分析：bucket 8 槽、overflow 链、渐进式扩容、并发不安全、`sync.Map` 取舍。

**工程实践与常见坑**

**1. 预分配容量**

```go
// 不好
m := map[string]int{}

// 好（如果知道大概大小）
m := make(map[string]int, 1000)
```

**2. 用 `map[T]struct{}` 当集合**

```go
set := make(map[string]struct{})
set["a"] = struct{}{}
if _, ok := set["a"]; ok {
    // 存在
}
delete(set, "a")
```

`struct{}` 不占内存，比 `map[T]bool` 省空间。

**3. 检查 key 是否存在**

```go
v, ok := m[k]
if !ok {
    // key 不存在
}
```

不要只看 `v` 的零值——零值可能是合法值。

**4. 并发安全选型**

| 场景 | 推荐 |
|---|---|
| 单 goroutine | 普通 map |
| 多 goroutine、读多写少 | sync.Map |
| 多 goroutine、读写均衡 | RWMutex + map |
| 极高并发、写多 | 分片 map |

**5. 删除大 map 释放内存**

```go
// 不释放底层数组（等量扩容才压缩）
for k := range m {
    delete(m, k)
}

// 真正释放
m = make(map[K]V)
```

**6. 遍历时安全删除**

```go
for k := range m {
    if shouldDelete(k) {
        delete(m, k) // 遍历中删除当前 key 安全
    }
}
```

但遍历中新增 key 行为未定义。

**7. 不要取 map 元素地址**

```go
m := map[string]int{"a": 1}
// p := &m["a"] // 编译错误：cannot take the address of m["a"]
```

map 扩容会重定位元素，地址会失效，所以 Go 直接禁止。如果需要指针，把 value 类型设为指针：`map[string]*int`。

**8. 用 `ok` 模式避免零值歧义**

```go
type Config struct {
    Timeout int
}

configs := map[string]Config{
    "a": {Timeout: 0}, // 0 是合法值
}

// 错误：无法区分"不存在"和"Timeout=0"
// if c := configs["x"]; c.Timeout == 0 { ... }

// 正确
if c, ok := configs["x"]; ok {
    _ = c
}
```

**9. JSON 反序列化用 `map[string]any`**

```go
package main

import (
    "encoding/json"
    "fmt"
)

func main() {
    data := []byte(`{"name": "Alice", "age": 30}`)
    var m map[string]any
    if err := json.Unmarshal(data, &m); err != nil {
        panic(err)
    }
    fmt.Println(m["name"]) // Alice
    // 数字会被解析为 float64！
    fmt.Printf("%T\n", m["age"]) // float64
}
```

坑：JSON 数字默认解析为 `float64`，大整数会丢精度。用 `json.Number` 或 `json.Decoder.UseNumber()` 解决。

**10. 用 `clear` 一次清空（Go 1.21+）**

```go
package main

import "fmt"

func main() {
    m := map[string]int{"a": 1, "b": 2}
    clear(m) // Go 1.21 引入
    fmt.Println(len(m)) // 0
}
```

`clear` 比 `for { delete }` 快，且语义清晰。

**11. 不要在 map 里存闭包持有大对象**

```go
// 隐性泄漏：cache 的 value 是闭包，闭包捕获了 bigData
func newProcessor(bigData []byte) func() {
    return func() {
        // 使用 bigData
    }
}
cache := map[string]func(){}
cache["x"] = newProcessor(big) // big 不会释放，除非 cache["x"] 被删除
```

### 本章小结

本章深入 Go Map 的实现：

1. `hmap` 是顶层结构，`count`/`B`/`buckets`/`oldbuckets`/`hash0` 是关键字段。
2. bucket 是 8 槽数组 + overflow 链，tophash 加速过滤，keys/values 分离消除对齐填充。
3. 扩容分翻倍与等量两种，渐进式迁移分摊到 map 操作中，避免延迟尖峰。
4. 遍历顺序随机是 Runtime 主动设计，防止用户依赖顺序。
5. map 不可 `==`，需用 `reflect.DeepEqual` 或自定义比较。
6. map 并发不安全，`sync.Map` 适合读多写少，分片 map 适合高并发写。
7. 工程实践：预分配、`ok` 模式、`map[T]struct{}` 集合、`clear` 清空、并发安全选型。

理解 map 的内部结构后，下一章我们将进入 Channel 的并发原语世界。
