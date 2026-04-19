# Design Changelog

> 此文档记录 chok 公开契约层面的设计变迁——新增能力、不兼容变更、
> 弃用与移除。**实现细节不在此处**，请直接看 [`docs/design.md`](design.md)。
>
> 项目使用 [Conventional Commits](https://www.conventionalcommits.org/)
> 与 [release-please](https://github.com/googleapis/release-please)
> 自动生成根目录的 [`CHANGELOG.md`](../CHANGELOG.md)。本文与之互补：
> 那份记录"哪个 commit 进了哪个版本"，本文记录"为什么这次发布的设计
> 选择是这样"。

---

## 0.1.0 — Initial public release

chok 的首个公开版本。提供「配置驱动的 Go Web 全家桶」的完整骨架：
HTTP / 数据层 / 缓存 / 鉴权 / 调度 / 可观测性都收敛到一份 `chok.yaml`
+ 一个 `Config` 结构体。

### 内置能力

15 个内置 Component（详见 `design.md` §9）覆盖：

- HTTP server (gin) + 默认中间件栈
- ORM (gorm) + 自动迁移 + 软删除 + Owned 模型
- 多层缓存：Otter (memory) → Badger (file) → Redis，含 circuit breaker
- 账号模块：注册 / 登录 / JWT / 密码重置 / 登录限速
- 任务调度：robfig cron + panic-safe + 统计
- 异步任务池：有界 worker pool
- 可观测性：Prometheus `/metrics` + OpenTelemetry tracing + Health/Ready/Live
- API 文档：从泛型 handler 自动生成 OpenAPI 3.0

### 关键设计契约

- **Component 抽象**：所有子系统统一 `Init` / `Close` 强制契约，
  `Reload` / `Health` / `Router` / `Migrate` / `ReadyChecker` 等可选
  接口由 Registry 通过类型断言发现。
- **Config-driven**：自动从 `Config` 结构体字段反射出需要的内置
  Component；`enabled: true|false` 是启停子系统的主要开关。
- **Discriminator 配置**：`DatabaseOptions.Driver` 选择 sqlite/mysql；
  `config.SelfValidating` 标记类型让递归校验器跳过未选分支。
- **Two-phase atomic reload**：配置热加载先验证副本，全部通过才原子
  替换 live config，失败时零字段污染。
- **Lock order**：`reloadMu → mu`。`Stop` 与 `Reload` 互斥；
  `Reload` 用 `TryLock`，并发触发返回 `ErrReloadInProgress`。
- **Reload 不触发 Migrate**：schema 变更需要重启进程。
- **Shutdown context**：所有 shutdown 路径用 `context.WithoutCancel(parent)`
  保留 trace_id / request_id。
- **Get during phaseStopping**：只返回 `available` 集合中的组件，
  Stop 在每次 Close 后立即清除。

### Store API

- 6 个方法（Create / Get / List / Update / Delete / Tx）取代独立 CRUD
- Locator 抽象：`RID(x)` / `ID(x)` / `Where(opts...)`
- Changes 抽象：`Set(map)` / `Fields(&obj)`，后者自动提取 `obj.Version`
  实现乐观锁
- 强制 `WithQueryFields` / `WithUpdateFields` 白名单（auto-discovery
  仅做开发期 warn）
- `Where` Locator 用于 Update/Delete 时强制至少一个 filter，防止误清表
- Cursor pagination：`WithCursor`（严格唯一字段） / `WithCursorBy`
  （复合 keyset，处理同值边界）
- 标识符校验：`store/where/where.go` 在 `resolveField` 处拒绝非
  `[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?` 的列名

### CLI

`chok` 二进制提供：

- `chok init <name>` — 脚手架（项目名取自路径 basename）
- `chok version [--json|--short]` — 构建/VCS/运行时元信息
- `chok update [--ref|--check|--dry-run]` — 通过 `go install` 升级

版本元信息按 ldflags → `debug.ReadBuildInfo` → `dev`/`unknown`
顺序解析。

### 已知限制

- 数据库驱动仅 sqlite 与 mysql；postgres 在路线图（见
  [`roadmap.md`](roadmap.md)）
- 内置 Component 不含 WebSocket / gRPC（同上）
- Schema 迁移仅 `AutoMigrate`，无版本化迁移工具

### 下游兼容性承诺

0.x 版本期间可能有不兼容变更，但每次都会在本文档对应版本段落明确
列出，配合 release-please 生成的 [`CHANGELOG.md`](../CHANGELOG.md)
让升级者一眼看到影响面。1.0 之后遵循语义化版本。
