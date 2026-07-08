<p align="center">
  中文 | <a href="README.md">English</a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.26+" />
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green" alt="MIT" /></a>
  <a href="https://pkg.go.dev/github.com/zynthara/chok/v2"><img src="https://pkg.go.dev/badge/github.com/zynthara/chok/v2.svg" alt="Go Reference" /></a>
  <a href="https://github.com/zynthara/chok/actions/workflows/ci.yml"><img src="https://github.com/zynthara/chok/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
</p>

<h1 align="center"><code>chok</code></h1>
<p align="center"><b>一个有主见的、配置驱动的 Go Web 框架。</b></p>

---

> [!NOTE]
> 这里是 **chok v2**（`github.com/zynthara/chok/v2`），目前处于
> beta。v1 已封版于
> [`v0.1.4`](https://github.com/zynthara/chok/releases/tag/v0.1.4)
> （只收安全修复），永久可安装：
> `go get github.com/zynthara/chok@v0.1.4`。从 v1 迁移请看
> [v1 → v2 迁移指南](docs/migration-v1-to-v2.md)。

---

`chok` 把 HTTP、数据库、缓存、JWT 认证、RBAC、定时任务与可观测性
装进同一个 Go module。一份 YAML 既声明装配哪些模块、也配置它们；
装配代码由工具生成。

```yaml
# chok.yaml — 段在场 = 模块装配；enabled = 运行期开关
log:     { level: info, format: json }
http:    { addr: ":8080" }
db:      { driver: sqlite, migrate: auto, sqlite: { path: app.db } }
health:  { path: /healthz }
swagger: { title: "My API" }
account: { signing_key: "${MYAPP_ACCOUNT_SIGNING_KEY}" }
```

```go
// main.go — 全部接线
func main() {
    chok.New("myapp",
        chokModules(), // chok_modules_gen.go —— `chok sync` 重新生成
        chok.Override(db.Module(db.WithTables(db.Table(&Note{})))),
        chok.Routes(func(r kernel.Router, k kernel.Kernel) error {
            notes := store.New[Note](db.From(k), log.From(k),
                store.WithQueryFields("id", "title", "created_at"),
                store.WithUpdateFields("title", "body"))
            api := r.Group("/api/v1", account.Authn(k))
            api.Handle("GET", "/notes/{rid}", handler.HandleRequest(getNote))
            return nil
        }),
    ).Execute()
}
```

跑起来就有：JSON 日志的 HTTP 服务器、自动迁移的 SQLite、JWT 的
`/auth/register|login|refresh-token|...`、`/swagger` 的 OpenAPI 3
文档、面向 Kubernetes 的 `/healthz` `/livez` `/readyz`、Prometheus
`/metrics` —— 且二进制里只链入你声明过的模块。

## 设计

三个不可变形容词：

- **配置驱动。** yaml 段声明跑什么；`enabled: true|false` 是运行期
  开关；`reload:"hot"` 字段 SIGHUP 即生效不用重启。代码只留给业务。
- **每个能力一个钦定实现。** HTTP 是 stdlib `ServeMux`（Go 1.22+
  模式路由）；ORM 是 gorm —— 藏在 store 后面不可见；缓存是
  otter + redis；cron 是 robfig；JWT 是 golang-jwt；RBAC 是 casbin；
  观测是 Prometheus + OpenTelemetry。不提供平行选择。
- **内部复杂，外部极简。** 单 actor 内核跑生命周期（拓扑 Init、
  mount、serve、drain、逆序 Close）；配置是不可变 RCU 快照；组件
  声明 `Descriptor`，能力靠类型发现。这些都不出现在业务代码里。

结构性拿到的保证：owned 模型的未认证查询 fail-closed、对外 ID 是
带前缀的 RID（数字主键不外泄）、乐观锁走 version 列、日志里密钥
自动脱敏、raw SQL 只有两扇门 —— 都叫 `Unsafe`。

## 30 秒 hello world

零 yaml、单文件、`go run .` 即起——而且起来的是生产形态:带
request_id 的结构化访问日志、SIGINT 优雅关停、环境变量配置覆盖,
全在默认值里。

```go
package main

import (
	"context"

	"github.com/zynthara/chok/v2"
	"github.com/zynthara/chok/v2/kernel"
	"github.com/zynthara/chok/v2/web"
)

func main() {
	chok.New("hello",
		chok.Use(web.Module()),
		chok.Routes(func(r kernel.Router, _ kernel.Kernel) error {
			web.GET(r, "/ping", func(context.Context, *struct{}) (string, error) {
				return "pong", nil
			})
			return nil
		}),
	).Execute()
}
```

`web.GET/POST/PUT/PATCH/DELETE` 把路由与类型化绑定层融成一行:
一行 = 路由 + 请求绑定 + 响应编码 + 错误映射 + OpenAPI 登记。

## 快速开始

```bash
go install github.com/zynthara/chok/v2/cmd/chok@latest

chok init myapp
cd myapp && go mod tidy && go run .
```

`curl localhost:8080/healthz`，再打开 <http://localhost:8080/swagger>。
脚手架自带能跑的 Note API（模型 + 三条路由）、声明标准模块的
`chok.yaml`、versioned 模式用的 `migrations/` 骨架。加电池 = 加一个
yaml 段，跑 `chok sync`，完事。

带讲解的完整路径 —— 认证、属主隔离的 CRUD、curl 里的乐观锁 ——
走一遍 [`examples/blog`](examples/blog)（五分钟）。

## 内置模块

每个子系统都是模块：经 `chok.Use`（或生成的 `chokModules()`）装配、
以 `Descriptor` 声明、由各自的 yaml 段配置。能力由内核按类型发现
（mount/serve/migrate/reload/health/ready/drain），下表由事实源生成
—— `chok docs gen` 保真。

<!-- gen:components -->
| 模块 | 配置段 | 依赖（`?` = 软依赖） | 能力 | 默认启用 | 说明 |
|---|---|---|---|---|---|
| `log.Module()` | `log` | — | reload | always | 根日志段（级别/格式/输出）；级别热更新。 |
| `web.Module()` | `http` | log?, metrics?, tracing?, authz? | serve, router | true | stdlib HTTP 服务器 + 路由 + 默认中间件栈。 |
| `health.Module()` | `health` | — | reload, mount, drain | true | /healthz /livez /readyz 探针；停机时先摘除就绪。 |
| `metrics.Module()` | `metrics` | — | mount | true | Prometheus 注册表 + /metrics 端点。 |
| `debug.Module()` | `debug` | — | mount | false | /componentz 拓扑与生命周期事件视图（默认关闭）。 |
| `swagger.Module()` | `swagger` | http | mount | true | 由路由表生成 OpenAPI 3 spec + Swagger UI。 |
| `tracing.Module()` | `tracing` | — | — | false | OpenTelemetry tracer provider（stdout/OTLP 导出）。 |
| `db.Module()` | `db` | log?, tracing? | health, migrate | true | 数据库连接池（sqlite/mysql/postgres）+ 迁移（auto/versioned/off）。sqlite 为纯 Go 驱动 + 读写分池 + 内建维护循环（§7.5）。 |
| `redis.Module()` | `redis` | log? | health | true | go-redis 客户端（TLS/CA 支持）；健康探针。 |
| `cache.Module()` | `cache` | redis?, log? | — | true | 分层缓存：otter 内存层 + redis 层 + 熔断器。 |
| `scheduler.Module()` | `scheduler` | log? | health, serve | true | robfig cron（panic 防护、重叠策略、统计）。 |
| `audit.Module()` | `audit` | db, scheduler?, account?, log? | reload, mount, migrate | false | 合规审计日志：异步 DB sink、清理 cron、admin API（显式启用）。 |
| `authz.Module()` | `authz` | db, redis?, audit?, log? | migrate, ready | true | casbin RBAC 引擎：adapter、Redis watcher、bootstrap 播种、决策审计。 |
| `account.Module()` | `account` | db, log? | mount, migrate | true | 用户模块：注册/登录/JWT/重置 + 登录限速 + OAuth providers。 |
<!-- /gen:components -->

各模块的全部配置项见 [`docs/config.md`](docs/config.md)；
[`docs/chok.schema.json`](docs/chok.schema.json) 给编辑器补全与 CI
校验用。

自定义组件实现 `kernel.Component`（`Describe` / `Init` / `Close` +
所需行为接口）即可加入同一套生命周期。

## CLI

```bash
chok init <name>            # 脚手架一个 v2 项目（生成即可启动）
chok sync [--check]         # chok.yaml → chok_modules_gen.go（可做 CI 闸）
chok migrate create|up|status   # forward-only 版本化迁移
chok docs gen [--check]     # 组件表 + 配置参考 + JSON Schema
chok openapi export         # 运行中应用的 OpenAPI spec → .json/.yaml
chok version [--json]       # 构建 / VCS / 运行时元数据
chok update [--ref vX.Y]    # 经 go install 升级本地 CLI
```

## 示例

| 路径 | 展示内容 |
|---|---|
| [`examples/blog`](examples/blog) | 快速上手级：JWT 认证 + 带属主隔离、乐观锁、软删除的 `Post` 资源 + 生成的 OpenAPI。其验收测试在 CI 里用真实 HTTP 走一遍 README 路径。 |

覆盖 authz、audit、scheduler 与自定义组件的完整示例
（`examples/tasker`）在路线图上。

## 文档

| 主题 | 位置 |
|---|---|
| 架构与契约（中文） | [`docs/design.md`](docs/design.md) |
| 数据层使用指南 | [`docs/db.md`](docs/db.md) |
| 配置参考（生成） | [`docs/config.md`](docs/config.md) |
| v1 → v2 迁移指南 | [`docs/migration-v1-to-v2.md`](docs/migration-v1-to-v2.md) |
| 设计变更日志 | [`docs/changelog.md`](docs/changelog.md) |
| 路线图 | [`docs/roadmap.md`](docs/roadmap.md) |
| GoDoc | <https://pkg.go.dev/github.com/zynthara/chok/v2> |
| Agent / AI 协作指引 | [`CLAUDE.md`](CLAUDE.md)、[`AGENTS.md`](AGENTS.md) |

## 参与贡献

Conventional commits（`feat:`、`fix:`、`docs:`、`chore:`）。release
手动裁切：写 changelog → 打 tag → goreleaser 发布。公开 API 变更
必须伴随 changelog 条目 —— CI 对最近的 release tag 跑 `apidiff`，
外加 `chok docs gen --check` 与 `chok sync --check`，生成面不允许
漂移。

```bash
make all      # tidy + lint + test + build
make smoke    # 启动 examples/blog 做冒烟
make test-pg  # store/db 套件跑 Postgres（设 CHOK_TEST_PG_DSN）
```

架构不变量记录在 [`CLAUDE.md`](CLAUDE.md) 与
[`docs/design.md`](docs/design.md)。

## 许可证

[MIT License](LICENSE)。
