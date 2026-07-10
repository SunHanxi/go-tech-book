# 《Go 底层原理与工程实践》

[![Live on Cloudflare Workers](https://img.shields.io/badge/Live-gobook.bringwater.top-F38020?logo=cloudflare&logoColor=white)](https://gobook.bringwater.top)

> 一本从语言设计、Runtime、标准库到 Kubernetes 工程实践的 Go 原理书。

本目录按目录结构生成,每章一个独立 Markdown 文件,可按顺序阅读,也可按需跳读。

## 目录

### 前言

- [前言](./00-前言.md) — 为什么写这本书 / 适合谁 / 如何阅读 / 学习路线

### 第一篇 Go 的设计哲学

- [第1章 Go 为什么如此设计](./01-Go为什么如此设计.md) — 诞生背景、设计哲学、Less is More、与 Java/C++/Rust 对比

### 第二篇 类型系统

- [第2章 Array](./02-Array.md) — 为什么长度属于类型、值类型、内存布局
- [第3章 Slice(重点)](./03-Slice.md) — Slice Header、共享机制、内存泄漏、常见坑
- [第4章 append()](./04-append.md) — growslice、扩容算法、copy、GC 与旧数组
- [第5章 Map(重点)](./05-Map.md) — Swiss Table、扩容、并发不安全、sync.Map、旧实现对照
- [第6章 String](./06-String.md) — String Header、UTF-8、rune/byte、strings.Builder
- [第7章 Interface(重点)](./07-Interface.md) — iface/eface、方法集、nil interface、性能
- [第8章 现代类型系统与泛型](./08-现代类型系统与泛型.md) — 类型参数、类型集、约束、推断、迭代器、实现取舍
- [第9章 反射、unsafe 与 cgo](./09-反射unsafe与cgo.md) — reflect、unsafe.Pointer、checkptr、Pinner、cgo 指针规则

### 第三篇 函数与方法

- [第10章 函数](./10-函数.md) — 参数传递、多返回值、defer、panic、recover
- [第11章 方法](./11-方法.md) — Receiver、值/指针接收者、方法集、嵌入字段、方法提升

### 第四篇 Go 并发(重点)

- [第12章 Goroutine](./12-Goroutine.md) — Scheduler、M/P/G、GMP、work stealing、抢占、netpoll
- [第13章 Channel(重点)](./13-Channel.md) — hchan、有/无缓冲、close、range、select
- [第14章 select(重点)](./14-select.md) — 语法、default、随机选择、公平性、Runtime 实现
- [第15章 Context(重点)](./15-Context.md) — cancel/timeout/deadline/value、Context Tree
- [第16章 Timer 与 Ticker](./16-Timer与Ticker.md) — Timer/After/AfterFunc、Ticker、Stop/Reset、Runtime Timer
- [第17章 sync 包](./17-sync包.md) — Mutex、RWMutex、Once、Cond、WaitGroup、Pool、Atomic
- [第18章 Go 内存模型与数据竞争](./18-Go内存模型与数据竞争.md) — happens-before、安全发布、原子操作、race detector

### 第五篇 Runtime(重点)

- [第19章 Runtime 总览](./19-Runtime总览.md) — 启动流程、Scheduler、GC、内存管理
- [第20章 内存管理](./20-内存管理.md) — Heap/Stack、Tiny Allocator、Span、MCache/MCentral/MHeap
- [第21章 GC(重点)](./21-GC.md) — 三色标记、写屏障、混合写屏障、GC 周期、GOGC/GOMEMLIMIT
- [第22章 Escape Analysis](./22-Escape-Analysis.md) — 什么是逃逸、为什么、如何避免、编译器分析

### 第六篇 工程实践

- [第23章 错误处理](./23-错误处理.md) — error、errors.Is/As/Join、包装错误、日志
- [第24章 性能优化](./24-性能优化.md) — Benchmark、pprof、trace、alloc/cpu/memory
- [第25章 Go 常见设计模式](./25-Go常见设计模式.md) — Option/Functional Option/Builder/Pipeline/Worker Pool/Fan-In/Fan-Out/Pub-Sub
- [第26章 io 与 net/http](./26-io与net-http.md) — Reader/Writer、流式处理、Transport、超时、优雅停机
- [第27章 测试与工具链](./27-测试与工具链.md) — 单元测试、Fuzz、B.Loop、synctest、Modules、PGO
- [第28章 可观测性与安全](./28-可观测性与安全.md) — slog、Metrics、Tracing、pprof、govulncheck、TLS

### 第七篇 Kubernetes 源码中的 Go(重点)

- [第29章 client-go](./29-client-go.md) — Informer、Reflector、DeltaFIFO、Indexer、SharedInformer
- [第30章 Controller](./30-Controller.md) — Reconcile、WorkQueue、RateLimiter、Retry
- [第31章 Operator](./31-Operator.md) — controller-runtime、Manager、Cache、Client、Webhook
- [第32章 Prometheus Go Client](./32-Prometheus-Go-Client.md) — Collector、Registry、Gather、Exporter
- [第33章 Go 项目最佳实践](./33-Go项目最佳实践.md) — 包组织、命名、Context、日志、配置、并发、错误处理、Benchmark、Profiling

### 附录

- [附录](./34-附录.md) — 常见坑 / 面试高频问题 / 源码阅读路线 / 推荐书籍博客项目 / 版本更新

## 阅读建议

- **语言与类型**：前言 → 第1章 → 第二篇（2~9）→ 第三篇（10~11）
- **进阶并发**：第四篇（12~18），重点读 12/13/15/18
- **底层原理**：第五篇（19~22），重点读 20/21/22
- **工程化**：第六篇（23~28）+ 第七篇（29~33）
- **面试速览**：附录 → 各章「本章小结」

## 说明

- 语言与标准库以 **Go 1.26** 为当前基线；涉及历史行为时明确标注版本边界。
- Runtime 实现细节以对应章节声明的 Go tag 为准，不把内部结构当作 Go 1 兼容性承诺。
- `examples/` 中的代码由 CI 编译和测试；正文中的不可编译片段会明确标记为伪代码。
- 标注「(重点)」的章节是面试与工程的高频核心,建议精读。
- 章节间用 Markdown 相对链接互相引用,可在支持跳转的 Markdown 阅读器中浏览。
