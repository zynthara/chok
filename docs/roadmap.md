# Roadmap

> 0.1.0 后的功能增量，按"用户感受到的优先级"排序。每条工作量是粗
> 估（小=半天到一天 / 中=数天 / 大=一两周），以方便愿意贡献的人挑
> 题。设计公理（配置驱动、单一官方实现、内部复杂外部极简）始终适用。

---

## Tier 1 — 真正用起来很快会需要

| 功能 | 说明 | 工作量 |
|---|---|---|
| **PostgreSQL driver** | `config.PostgresOptions` + `db.NewPostgres` + 通过 `DatabaseOptions{Driver: "postgres"}` auto-register。需要兼容 SoftUnique 的 partial index 写法 | 中 |
| **CORS auto-register** | `middleware.CORS` 已存在但未进入 HTTPComponent 默认栈；需要 `config.CORSOptions` + auto-register | 小 |
| **Email verification flow** | `Sender` 接口已定义，缺注册后的邮箱验证流程（发验证码 → 确认 → 激活） | 中 |
| **Store `Count`** | 只需要总数不需要数据时，避免 `List` 的全量查询开销 | 小 |
| **Store `Restore`** | 软删除恢复方法。`WithTrashed` 已支持查询，缺写入侧 | 小 |

## Tier 2 — 规模变大后需要

| 功能 | 说明 | 工作量 |
|---|---|---|
| **Versioned migration** | 当前 `AutoMigrate` 适合开发，生产需要版本号、forward-only、dry-run、migration lock | 大 |
| **gRPC ServerComponent** | `Server` 接口已支持，缺 `parts.GRPCComponent` 封装 `google.golang.org/grpc` | 中 |
| **HTTP rate limiting** | 通用 per-route / per-IP / per-user 限速 middleware，支持 Redis 后端。当前只有 account 模块的登录限速 | 中 |
| **Event bus** | Store after-hooks + Pool 提供了基础设施，缺一个正式的 publish/subscribe 抽象 | 中 |
| **Multi-DB instances** | 当前只有一个 `db` Component。读写分离 / 多库场景需要 named DB（`db:read` / `db:write`） | 中 |
| **Self-update binary** | `chok update` 当前是 `go install` 包装。M2 改用 selfupdate 库直接下载 GitHub Release artifact，免 Go toolchain | 中 |

## Tier 3 — 生态完善

| 功能 | 说明 | 工作量 |
|---|---|---|
| **WebSocket Component** | gin 原生支持 `gorilla/websocket`，缺一个标准化封装 | 中 |
| **i18n error messages** | `apierr.Error.Message` 当前硬编码英文，需要 locale 支持 | 中 |
| **Batch update / upsert** | `BatchCreate` 已有，`BatchUpdate` / `BatchUpsert` 缺位 | 中 |
| **Admin dashboard** | 基于 `/componentz` + `/healthz` + `/metrics` 的内置 Web UI | 大 |
| **OpenAPI export CLI** | swagger 包已生成 spec，加 `chok openapi export` 命令导出 JSON/YAML 文件 | 小 |
| **Config full RCU** | 当前 `ConfigSnapshot` 是浅拷贝；真正的 `atomic.Pointer[Config]` 需改 `WithConfig` API | 大 |

## 不会做

| 功能 | 原因 |
|---|---|
| 插件系统 / 多 provider 选择 | 违反"单一官方实现"定位 |
| ORM query builder 替代 GORM | 投入产出比极低，GORM 生态已足够 |
| 前端模板渲染 | 定位是 API 框架，不是 full-stack |
| 内置消息队列 (Kafka/RabbitMQ) | 超出 Web 框架边界，用户自行集成更合理 |
| 内置 ORM / cache / log 抽象层供切换 | 同"单一官方实现"——可观测性靠 `Component` 替换实现，不靠抽象层 |

---

愿意领走某项？开 issue 标 `roadmap` + 对应 tier 标签即可。Tier 1
的项不强求依赖讨论，可以直接发 PR；Tier 2 / Tier 3 建议先在 issue
里对齐 API 形态再动手。
