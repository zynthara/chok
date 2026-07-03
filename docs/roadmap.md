# Roadmap

> v2.0.0-beta.1 之后的功能增量，按「用户感受到的优先级」排序。
> 工作量为粗估（小=半天到一天 / 中=数天 / 大=一两周）。设计公理
> （配置驱动、单一官方实现、内部复杂外部极简、不变量进类型、
> 单一事实源）始终适用。
>
> v1 roadmap 里的大项已随 v2 交付：PostgreSQL driver、版本化迁移、
> 事件总线、多 db 实例、config 全量 RCU、`chok openapi export`。

---

## Tier 0 — v2.0.0 GA 之前

| 功能 | 说明 | 工作量 |
|---|---|---|
| **examples/tasker** | 进阶示例：authz + audit + scheduler + 自定义组件 + versioned 迁移全覆盖 | 中 |
| **beta 反馈吸收** | beta 系列期间的 API 微调窗口（GA 后 v2 API 冻结走 semver） | — |

## Tier 1 — 真正用起来很快会需要

| 功能 | 说明 | 工作量 |
|---|---|---|
| **CORS 配置段** | `middleware.CORS` 已存在但未进 web 默认栈；补 `http.cors` 子段接入 | 小 |
| **Email verification flow** | `Sender` 接口已定义，缺注册后的邮箱验证流程（发验证码 → 确认 → 激活） | 中 |
| **Store `Count`** | 只需要总数不需要数据时，避免 `List` 的全量查询开销 | 小 |
| **Store `Restore`** | 软删除恢复方法。`WithTrashed` 已支持查询，缺写入侧 | 小 |

## Tier 2 — 规模变大后需要

| 功能 | 说明 | 工作量 |
|---|---|---|
| **gRPC 模块** | kernel `Server` 行为接口已支持长驻循环，缺 `grpc.Module()` 封装 | 中 |
| **HTTP rate limiting** | 通用 per-route / per-IP / per-user 限速中间件，支持 Redis 后端（当前只有 account 登录限速） | 中 |
| **badger 文件缓存层回归** | v2 砍掉了 cache 的 badger 层（依赖树收益）；若需求真实存在，以独立模块形态回归 | 中 |
| **toffs 增量回迁** | i18n / wshub / notify / stateguard / statemachine 在 v2 契约上回迁（v2.1+ 议题） | 大 |
| **Self-update binary** | `chok update` 当前是 `go install` 包装；改 selfupdate 直接下载 Release artifact，免 Go toolchain | 中 |

## Tier 3 — 生态完善

| 功能 | 说明 | 工作量 |
|---|---|---|
| **WebSocket 模块** | stdlib / gorilla 封装 + kernel 生命周期接入 | 中 |
| **i18n error messages** | `apierr` RenderHook 机制已就位，缺 locale 资源与语言协商 | 中 |
| **Batch update / upsert** | `BatchCreate` 已有，`BatchUpdate` / `BatchUpsert` 缺位 | 中 |
| **Admin dashboard** | 基于 `/componentz` + `/healthz` + `/metrics` 的内置 Web UI | 大 |
| **电池独立迁移序列** | versioned 模式下电池表走 `schema_migrations_chok_<battery>` 命名空间（替代 AutoMigrate 白名单的 v2.x 演进路径） | 大 |

## 不会做

| 功能 | 原因 |
|---|---|
| 插件系统 / 多 provider 选择 | 违反「单一官方实现」定位 |
| ORM query builder 替代 GORM | 投入产出比极低；且 gorm 已藏在 store 之后，可替换性由编译器守卫 |
| 前端模板渲染 | 定位是 API 框架，不是 full-stack |
| 内置消息队列 (Kafka/RabbitMQ) | 超出 Web 框架边界，用户自行集成更合理 |
| 内置 ORM / cache / log 抽象层供切换 | 同「单一官方实现」 |

---

愿意领走某项？开 issue 标 `roadmap` + 对应 tier 标签即可。Tier 1
的项不强求依赖讨论，可以直接发 PR；Tier 2 / Tier 3 建议先在 issue
里对齐 API 形态再动手。
