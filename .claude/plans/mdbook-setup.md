# 用 mdBook 把《Go 底层原理与工程实践》变成可在线浏览的 book 站点

## 目标
保持现有 29 个 markdown 文件不动，叠加一层 mdBook 配置 + GitHub Actions，让 push 到 main 后自动构建并部署到 GitHub Pages，得到「左侧目录树 + 右侧正文 + 全文搜索」的阅读站点。

## 方案选型理由
- mdBook 单二进制、零运行时依赖，Go/Rust 官方文档均采用，与本书「Go 技术书」气质一致。
- 现有文件命名 `NN-名称.md` + 章节间 `./NN-名称.md` 相对链接，与 mdBook 的 `SUMMARY.md` 模型完全契合，mdBook 构建时会自动把 `.md` 链接转成 `.html`，**正文内容零改动**。
- 采用 `src = "."`（根目录即源目录），不移动任何文件，git 历史干净，README 现有链接不破坏。

## 文件清单（新增 4 个，不改动任何现有 .md）

### 1. `book.toml`（根目录）
mdBook 配置：
- `src = "."`、`build-dir = "book"`
- `title = "Go 底层原理与工程实践"`、`language = "zh-CN"`
- 启用 `output.html.search`（全文搜索）
- 配 `git-repository-url = "https://github.com/SunHanxi/go-tech-book"` 与编辑链接
- 默认浅色主题，提供深色切换

### 2. `SUMMARY.md`（根目录）
mdBook 的目录树（左侧导航），按 README 现有「篇」结构组织，用 `# 篇名` 作为分组分隔符，`- [章节](./NN-名称.md)` 作为条目。覆盖 00-前言 ~ 28-附录 共 29 章。README.md 不纳入 book（它是 GitHub landing 页，与 SUMMARY 内容重复）。

### 3. `.github/workflows/deploy.yml`
GitHub Actions：
- 触发：push 到 main + 手动 dispatch
- 用 `peaceiris/actions-mdbook@v2` 安装 mdBook
- `mdbook build` 生成到 `book/`
- 用 `actions/configure-pages` + `actions/upload-pages-artifact` + `actions/deploy-pages` 部署
- 配 concurrency 避免并发部署

### 4. `.gitignore`
忽略 mdBook 输出目录 `book/`。

## 你需要做的事（配置完成后）
1. 仓库 Settings → Pages → Source 选 **GitHub Actions**
2. push 后等 Actions 跑完，访问 `https://sunhanxi.github.io/go-tech-book/`
3. 本地预览（可选）：`brew install mdbook` → `mdbook serve --open`

## 已知问题（不阻塞本次配置，建议后续单独修）
`01-Go为什么如此设计.md` 内有数处断链，链接文本与实际章节不符，mdbook build 时会 warning：
- `[第6章 调度器](./06-调度器.md)` → 实际第6章是 String，调度器在第10章
- `[第5章 interface](./05-interface.md)` → 实际第5章是 Map，interface 在第7章
- `[第7章 error 处理](./07-error.md)` → 实际第7章是 Interface，错误处理在第20章
- `[第3章 闭包与逃逸](./03-闭卷与逃逸.md)` → 实际第3章是 Slice
- `[第12章 并发原语](./12-Sync.md)` → 实际第12章是 select
这些是内容层面的引用错误，建议后续按真实章节号统一修正，本次配置不动正文。
