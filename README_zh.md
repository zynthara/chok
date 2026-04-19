<p align="center">
  中文 | <a href="README.md">English</a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.26+" />
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green" alt="MIT" /></a>
  <a href="https://pkg.go.dev/github.com/zynthara/chok"><img src="https://pkg.go.dev/badge/github.com/zynthara/chok.svg" alt="Go Reference" /></a>
  <a href="https://github.com/zynthara/chok/actions/workflows/ci.yml"><img src="https://github.com/zynthara/chok/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
</p>

<h1 align="center"><code>chok</code></h1>
<p align="center"><b>强约定、配置驱动的 Go Web 框架。</b></p>

---

`chok` 把 HTTP、数据库、缓存、JWT 鉴权、调度器、可观测性打包成一个 Go
模块。一份 YAML 控制每个子系统的开关，所有组装代码由你的 Config 结构
体自动生成。

```yaml
# chok.yaml
http:      { addr: ":8080" }
log:       { level: info, format: json, output: [stdout] }
database:  { driver: sqlite, sqlite: { path: "app.db" } }
account:   { enabled: true, signing_key: "..." }
swagger:   { enabled: true, title: "My API" }
```

```go
// main.go —— 三行串起整个应用
type Config struct {
    HTTP     config.HTTPOptions     `mapstructure:"http"`
    Log      config.SlogOptions     `mapstructure:"log"`
    Database config.DatabaseOptions `mapstructure:"database"`
    Account  config.AccountOptions  `mapstructure:"account"`
    Swagger  config.SwaggerOptions  `mapstructure:"swagger"`
}

var cfg Config

func main() {
    chok.New("myapp",
        chok.WithConfig(&cfg),
        chok.WithRoutes(func(_ context.Context, a *chok.App) error {
            a.API("/api/v1", a.AuthMiddleware()).GET("/me", meHandler)
            return nil
        }),
    ).Execute()
}
```

跑起来你会得到：JSON 日志的 HTTP 服务器、SQLite + 自动迁移、JWT
驱动的 `/auth/register|login|refresh-token|change-password|...`、
`/swagger` 上的 OpenAPI 3.0 规范、Kubernetes 友好的 `/healthz`
`/livez` `/readyz`，以及 Prometheus `/metrics`——全程没有一个手写
的 `Register` 调用。

## 设计

三个不可变形容词：

- **配置驱动**：`enabled: true|false` 是启停子系统的主要方式。代码改
  动留给业务逻辑，不留给装配。
- **每个能力只给一个官方实现**：HTTP 是 gin；ORM 是 gorm；缓存是
  Otter + Badger + Redis；定时任务是 robfig；JWT 是 golang-jwt；
  追踪是 OpenTelemetry。不提供平行选择。
- **内部复杂、外部极简**：拓扑生命周期、热加载分发、健康聚合都封装
  在 `Component` 抽象下，对外暴露的是 `app.Register(c)` 与
  `app.Logger()`。

## 快速上手

```bash
go install github.com/zynthara/chok/cmd/chok@latest

chok init myapp
cd myapp && go mod tidy && make run
```

脚手架会生成 `cmd/`、`internal/{app,handler}`、`configs/`、一个会
通过 ldflags 注入构建元信息的 `Makefile`，以及一份默认开启 HTTP +
日志 + SQLite + JWT 鉴权 + Swagger 的 `chok.yaml`。访问
<http://127.0.0.1:8080/healthz> 验证启动成功。

## 内置 Component

15 个子系统，全部以 `Component` 实现，强制 `Init` / `Close` 方法 +
按需 `Reload` / `Health` / `Router` / `Migrate` 等可选能力。多数
能从 `Config` 结构体字段自动注册；其余在 `WithSetup` 中显式注册。

| Component | 自动注册 | 选中条件 | 能力 |
|---|---|---|---|
| `LoggerComponent`    | 是 | `SlogOptions`        | 日志 + 访问日志 + 热加载 |
| `HTTPComponent`      | 是 | `HTTPOptions`        | gin + 默认中间件栈 |
| `DBComponent`        | 是 | `DatabaseOptions`    | gorm + 自动迁移 (sqlite/mysql) |
| `RedisComponent`     | 是 | `RedisOptions`       | go-redis 客户端 + 健康 |
| `CacheComponent`     | 是 | `CacheMemory/FileOptions` | 内存 + 文件 + Redis 链 |
| `AccountComponent`   | 是 | `AccountOptions`     | 注册 / 登录 / JWT / 重置 |
| `SwaggerComponent`   | 是 | `SwaggerOptions`     | OpenAPI 3.0 + Swagger UI |
| `HealthComponent`    | 是 | （只要有 HTTP）       | `/healthz` `/livez` `/readyz` |
| `MetricsComponent`   | 是 | （只要有 HTTP）       | Prometheus `/metrics` |
| `DebugComponent`     | 是 | `DebugOptions`       | `/componentz` 拓扑诊断 |
| `TracingComponent`   | 显式 | 代码注册             | OTel tracer + OTLP exporter |
| `SchedulerComponent` | 显式 | 代码注册             | robfig cron + 统计 |
| `PoolComponent`      | 显式 | 代码注册             | 有界异步任务池 |
| `JWTComponent`       | 显式 | 代码注册             | 额外 JWT manager |
| `AuthzComponent`     | 显式 | 代码注册             | 可插拔 Authorizer |

自定义组件实现同一接口即可纳入 registry 的生命周期、热加载与健康
检查。详见 [`docs/design.md`](docs/design.md) §13。

## CLI

```bash
chok init <name>          # 创建新项目
chok version [--json]     # 构建 / VCS / 运行时元信息
chok update [--ref vX.Y]  # 通过 go install 升级本地 CLI
```

版本元信息按以下顺序解析：`make build` / goreleaser 注入的 ldflags
→ `debug.ReadBuildInfo`（让 `go install ...@latest` 也能显示真实
的 pseudo-version + git hash）→ `dev` / `unknown` 兜底。

## 示例

| 路径 | 演示内容 |
|---|---|
| [`examples/blog`](examples/blog) | 入门级：HTTP + SQLite + JWT + Swagger + 一个带乐观锁、软删除、owner scope 的 `Post` 资源。5 分钟跑通。 |

完整覆盖所有 Component 与自定义组件模式的例子（`examples/tasker`）
在路线图上，见 [`docs/roadmap.md`](docs/roadmap.md)。

## 文档

| 主题 | 位置 |
|---|---|
| 架构与 API 契约 | [`docs/design.md`](docs/design.md) |
| 设计变更记录（按发布） | [`docs/changelog.md`](docs/changelog.md) |
| 路线图 | [`docs/roadmap.md`](docs/roadmap.md) |
| GoDoc 参考 | <https://pkg.go.dev/github.com/zynthara/chok> |
| AI / Agent 工具指引 | [`CLAUDE.md`](CLAUDE.md)、[`AGENTS.md`](AGENTS.md) |

## 贡献

项目使用 Conventional Commits（`feat:` / `fix:` / `docs:` /
`chore:`），release-please 据此自动生成版本号与 CHANGELOG；tag
推送触发 goreleaser 构建 linux / darwin / windows × amd64 /
arm64 的二进制。

```bash
make all      # tidy + lint + test + build
make smoke    # 启动 examples/blog 自检
make snapshot # 本地试跑 goreleaser，不上传
```

项目恪守的硬规则与架构不变量记录在 [`CLAUDE.md`](CLAUDE.md)。

## 许可证

[MIT License](LICENSE)。
