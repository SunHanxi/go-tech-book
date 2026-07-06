# 《Go 底层原理与工程实践》

[![Deploy mdBook](https://github.com/SunHanxi/go-tech-book/actions/workflows/deploy.yml/badge.svg)](https://github.com/SunHanxi/go-tech-book/actions/workflows/deploy.yml)

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
- [第5章 Map(重点)](./05-Map.md) — hmap、bucket、渐进式扩容、并发不安全、sync.Map
- [第6章 String](./06-String.md) — String Header、UTF-8、rune/byte、strings.Builder
- [第7章 Interface(重点)](./07-Interface.md) — iface/eface、方法集、nil interface、性能

### 第三篇 函数与方法

- [第8章 函数](./08-函数.md) — 参数传递、多返回值、defer、panic、recover
- [第9章 方法](./09-方法.md) — Receiver、值/指针接收者、方法集、嵌入字段、方法提升

### 第四篇 Go 并发(重点)

- [第10章 Goroutine](./10-Goroutine.md) — Scheduler、M/P/G、GMP、work stealing、抢占、netpoll
- [第11章 Channel(重点)](./11-Channel.md) — hchan、有/无缓冲、close、range、select
- [第12章 select(重点)](./12-select.md) — 语法、default、随机选择、公平性、Runtime 实现
- [第13章 Context(重点)](./13-Context.md) — cancel/timeout/deadline/value、Context Tree
- [第14章 Timer 与 Ticker](./14-Timer与Ticker.md) — Timer/After/AfterFunc、Ticker、Stop/Reset、Runtime Timer
- [第15章 sync 包](./15-sync包.md) — Mutex、RWMutex、Once、Cond、WaitGroup、Pool、Atomic

### 第五篇 Runtime(重点)

- [第16章 Runtime 总览](./16-Runtime总览.md) — 启动流程、Scheduler、GC、内存管理
- [第17章 内存管理](./17-内存管理.md) — Heap/Stack、Tiny Allocator、Span、MCache/MCentral/MHeap
- [第18章 GC(重点)](./18-GC.md) — 三色标记、写屏障、混合写屏障、GC 周期、GOGC/GOMEMLIMIT
- [第19章 Escape Analysis](./19-Escape-Analysis.md) — 什么是逃逸、为什么、如何避免、编译器分析

### 第六篇 工程实践

- [第20章 错误处理](./20-错误处理.md) — error、errors.Is/As/Join、包装错误、日志
- [第21章 性能优化](./21-性能优化.md) — Benchmark、pprof、trace、alloc/cpu/memory
- [第22章 Go 常见设计模式](./22-Go常见设计模式.md) — Option/Functional Option/Builder/Pipeline/Worker Pool/Fan-In/Fan-Out/Pub-Sub

### 第七篇 Kubernetes 源码中的 Go(重点)

- [第23章 client-go](./23-client-go.md) — Informer、Reflector、DeltaFIFO、Indexer、SharedInformer
- [第24章 Controller](./24-Controller.md) — Reconcile、WorkQueue、RateLimiter、Retry
- [第25章 Operator](./25-Operator.md) — controller-runtime、Manager、Cache、Client、Webhook
- [第26章 Prometheus Go Client](./26-Prometheus-Go-Client.md) — Collector、Registry、Gather、Exporter
- [第27章 Go 项目最佳实践](./27-Go项目最佳实践.md) — 包组织、命名、Context、日志、配置、并发、错误处理、Benchmark、Profiling

### 附录

- [附录](./28-附录.md) — 常见坑 / 面试高频问题 / 源码阅读路线 / 推荐书籍博客项目 / 版本更新

## 阅读建议

- **入门**:前言 → 第1章 → 第二篇(2~7) → 第三篇(8~9)
- **进阶并发**:第四篇(10~15),重点读 10/11/13
- **底层原理**:第五篇(16~19),重点读 17/18
- **工程化**:第六篇(20~22) + 第七篇(23~27)
- **面试速览**:附录 → 各章「本章小结」

## 说明

- 所有源码分析基于 **Go 1.21+** 实现。
- 标注「(重点)」的章节是面试与工程的高频核心,建议精读。
- 章节间用 Markdown 相对链接互相引用,可在支持跳转的 Markdown 阅读器中浏览。
