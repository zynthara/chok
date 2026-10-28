# chok v2 → v3 迁移指南

> 适用：从 `github.com/zynthara/chok/v2`（`v2.0.0-beta.N`）迁到
> `github.com/zynthara/chok/v3`。v2 在分支 `v2` 上封版、只收安全修复，
> 已发布的 beta tag 永久可安装。
>
> v3 不是 v2 的增量版本，而是按三条规则（配置严格、错误自描述、装配即代码）
> 重写的工具包：内核、`Descriptor`、`k.Get` 查找、`chok sync` 生成物、RCU 配置
> 与热重载都不再存在。本文按主题逐条列出「v2 写法 → v3 写法」，结尾的附录 A 是
> v2 每个包的去向总表。逐键的新配置见 [config.md](config.md)，逐符号的新公开面见
> [api.md](api.md)，设计见 [design.md](design.md)。

---

## 0. 总路线

1. **Go 1.27，import 换 `/v3`。** v3 用到泛型方法、`encoding/json/v2` 与
   `testing/synctest`。
2. **删掉生成的装配。** `chok_modules_gen.go` 与 `chok sync` 都没有了：写一个
   嵌入 `chok.Base` 的 `Config` struct，`main` 里依次 `Load` → `New` → 打开数据库
   → 构造电池 → `app.Use` → 声明路由 → `Execute`（§1）。
3. **改 chok.yaml。** 段的位置与少数键名变了（§2 的映射表）；`enabled: false`
   的模块段直接删掉、代码里不构造；`${VAR}` 插值改成按路径派生的环境变量。
   `go run . --check-config` 会逐条指出未知键并给出候选。
4. **改 handler。** `web.GET` / `handler.HandleRequest` → `chok.GET`；请求 tag
   `uri` / `form` / `binding` → `path` / `query` / `validate`（§3）。
5. **改错误。** 错误映射注册表没有了：让错误类型实现 `chok.Coder`（§3.3）。
6. **改 store。** `WithQueryFields` / `WithUpdateFields` → 模型字段上的
   `store:"query,update"` tag；10 个泛型自由函数 → 方法（§4.3）。
7. **改数据库。** 版本化迁移改用 14 位时间戳版本与单账本 `chok_migrations`，
   已应用的文件用 `chok migrate mark-applied` 登记；identity 的审计表与
   identities 的索引有改动；outbox 的表换成 jobs 的新表（§4.2、§11）。
8. **改看板与告警。** 指标名按 OTel 语义约定改名（§7、§11）。
9. **改测试。** `choktest.NewTestKernel` / `NewTestDB` → `choktest.App` /
   `choktest.DB`（§9）。

`examples/blog` 是迁移后的样板：同一个博客应用在 v2 里是 yaml + 生成的装配 +
路由回调，在 v3 里是 98 行普通 Go 代码。

## 1. 装配与生命周期

```go
// v2
func main() {
	chok.New("myapp",
		chokModules(), // chok_modules_gen.go，由 chok sync 从 chok.yaml 生成
		chok.Override(db.Module(db.WithTables(db.Table(&Note{})))),
		chok.Routes(func(r chok.Router, k chok.Kernel) error {
			notes := store.New[Note](db.From(k), log.From(k),
				store.WithQueryFields("id", "title"), store.WithUpdateFields("title"))
			api := r.Group("/api/v1", account.Authn(k))
			web.GET(api, "/notes/{rid}", getNote(notes))
			return nil
		}),
	).Execute()
}

// v3
type Config struct {
	chok.Base                    // app / http / log 三段，内联在顶层
	DB   db.Config       `yaml:"db"`
	Auth identity.Config `yaml:"auth"`
}

func main() {
	cfg := chok.MustLoad[Config]("myapp")
	app := chok.New(cfg.Base)
	d := db.MustOpen(cfg.DB, db.Tables(db.Table(&Note{})))
	auth := identity.MustNew(cfg.Auth, d)
	app.Use(d, auth)
	notes := store.New[Note](d) // 字段由模型上的 store tag 声明
	api := app.Group("/api/v1", auth.Authn())
	chok.GET(api, "/notes/{id}", getNote(notes))
	app.Execute()
}
```

| v2 | v3 |
|---|---|
| `chok.New(name, opts...)` | `cfg := chok.MustLoad[Config](name)`，`app := chok.New(cfg.Base, opts...)` |
| `chok.Use(mod.Module(), …)`、`chokModules()`、`chok sync` | 构造值、`app.Use(值…)`；没有生成文件 |
| `chok.Override(c)` | 直接用想要的参数构造那个值 |
| `chok.Routes(func(r chok.Router, k chok.Kernel) error)` | `app.Router()` / `app.Group(prefix, mw...)` 上直接注册 |
| `chok.Section[T](app, key)` 句柄 | 配置 struct 的字段 |
| `chok.Get[T](k, kind)`、`db.From(k)`、`log.From(k)`、`outbox.From(k)` | 持有构造出来的值（`d`、`app.Logger()`、`j`） |
| `kernel.Component`：`Describe` / `Init` / `Close` + 行为接口 | 任意值；`app.Use` 按它实现的 `Starter` / `Closer` / `Healther` / `Mounter` / `Namer` / `Instrumenter` 处理 |
| 组件在 `Init` 里返回错误以否决启动 | 构造函数返回错误（`identity.New` …）；或 `Start` 返回错误；或 `Health` 在 `app.start_timeout` 内不变好 |
| `Descriptor.Needs` 拓扑排序 | `Use` 的顺序就是 Start / Mount 顺序，Close 逆序 |
| 事件总线（`kernel/event`）、`EntityChanged` | 删除；跨组件的事后动作用 `d.AfterCommit` 或 jobs 的 `Worker` + `Enqueue` |
| `WithShutdownTimeout`、`WithInitTimeout`、`WithCloseTimeout`、`WithDrainDelay`、`WithReloadTimeout`、`WithComponentReloadTimeout` | `app.shutdown_timeout`、`app.start_timeout`、`http.drain_delay`（配置键）；reload 相关删除 |
| `WithConfigFile` / `WithEnvPrefix` / `WithFlags` | `chok.Load` 的 `ConfigFile` / `EnvPrefix` / `Flags` 选项 |
| `WithLogger(log.Logger)` | `chok.Logger(*slog.Logger)` |
| `WithVersion(version.Info)` | `chok.Version(string)` |
| `WithErrorMapper(m)` | 错误实现 `chok.Coder`；兜底用 `chok.OnError(hook)` |
| `Run(ctx, WithSignals(), WithConfigWatch())` | `app.Execute()`（信号）或 `app.Run(ctx)`；没有配置监听 |
| SIGHUP：重读配置、热应用 `reload:"hot"` 字段 | SIGHUP 只应用 `log.level` |
| 命名实例（`db.instances.<name>`） | 在代码里再 `db.Open` 一个句柄，配置放进自己的段 |

生命周期的次序：启动相按 `Use` 顺序调用 `Start`，然后等所有 `Healther` 健康
（`app.start_timeout`）才开始监听；停机相 `/readyz` 503 → `http.drain_delay` →
HTTP Shutdown → 取消 Starter → 逆序 `Close`。telemetry 放在 `Use` 的第一个，
它最后关、能冲出其他组件停机时的 span 与日志。

## 2. 配置

### 2.1 规则的变化

| v2 | v3 |
|---|---|
| viper + mapstructure，`mapstructure:"key"` tag | 自写的严格装载，`yaml:"key"` tag |
| 段在场即装配（`chok sync`），`enabled` 是运行期开关 | 段只是配置；装不装由代码决定；`enabled` 只在电池选层的地方保留 |
| 未知键忽略 | 未知键、未知段让启动失败，并给出候选（`did you mean …?`） |
| `${MYAPP_ACCOUNT_SIGNING_KEY}` 插值 | 没有插值：每个键都有按路径派生的环境变量，`auth.signing_key` 是 `MYAPP_AUTH_SIGNING_KEY` |
| 敏感键按名字启发式脱敏 | `secret:"true"` tag 脱敏 |
| `reload:"hot"` 字段、配置文件监听、三阶段 reload | 只有 `log.level` 经 SIGHUP 热更 |
| 命名实例 `instances.<name>` | 删除 |
| `docs/chok.schema.json` 给编辑器补全 | 删除；`--check-config` 用应用二进制严格校验配置并打印生效值 |

文件查找顺序：`--config <file>` / `{PREFIX}_CONFIG` / `./chok.yaml` /
`./{name}.yaml` / `./configs/{name}.yaml`；优先级：默认值 < 文件 < 环境变量
< flag（`--http.addr`）。

### 2.2 键的映射

v3 的段名由应用的 Config struct 决定；下表右列用 `examples/blog` 与
`chok init` 的约定（`db`、`auth`、`jobs`、`telemetry`、`cache`）。未列出的键
同名同位置。

| v2 键 | v3 键 | 说明 |
|---|---|---|
| `log.level` / `format` / `output` / `files[]` / `access_files[]` | `log.*` | 同名 |
| `log.access_enabled` | `log.access` | |
| `http.*`（addr、各超时、`drain_delay`、`trusted_proxies`、`h2c`） | `http.*` | 同名 |
| — | `http.max_body_bytes`、`http.cors.*`、`http.csrf` | 新增（v2 在代码里配） |
| `health.enabled` / `path` / `probe_timeout` | `http.health.enabled` / `path` / `probe_timeout` | |
| `swagger.enabled` / `title` / `version` | `http.openapi.enabled` / `title` / `version` | `version` 不再缺省 1.0.0 |
| `swagger.bearer_auth` | — | 删除：一条路由的 security 由它经过的认证中间件决定，identity 的 `Authn()` 报告 `bearerAuth`（cookie 模式另报 `cookieAuth`）；公开路由不用标 |
| `swagger.prefix`（`/swagger`，文档在 `/swagger/doc.json`） | `http.openapi.path`（`/openapi.json`）、`http.openapi.docs_path`（`/docs`） | |
| `debug.enabled` | `app.debug` | 只挂 `/debug/pprof/*`；`/componentz` 删除 |
| `debug.path` / `event_history` | — | 删除 |
| 代码选项的 6 个超时 | `app.shutdown_timeout`、`app.start_timeout`、`http.drain_delay` | |
| `metrics.enabled` / `path` | `telemetry.metrics.enabled` / `path` | |
| `tracing.enabled` / `exporter` / `service_name` | `telemetry.tracing.enabled` / `exporter` / `service_name` | service name 缺省从 `"chok"` 改为 OTel 缺省 |
| `tracing.otlp_endpoint` | `telemetry.tracing.endpoint` | 只写 collector 基址（`http://otel-collector:4318`），写了 `/v1/traces` 是配置错误 |
| — | `telemetry.tracing.sample_ratio`、`telemetry.logs.otel` | 新增 |
| `db.enabled`、`db.read_only`、`db.migration_status_interval` | — | 删除（不想要就不打开；只读实例与迁移状态 gauge 删除） |
| `db.driver` / `migrate` / `slow_threshold` | `db.*` | 同名 |
| `db.sqlite.max_open_conns` | `db.pool.max_open_conns` | SQLite 文件库的读池大小 |
| `db.mysql.max_open_conns` / `max_idle_conns` / `conn_max_lifetime` / `conn_max_idle_time`（postgres 同） | `db.pool.*` | 池参数合并成一段，换驱动不用搬 |
| `db.mysql.time_zone` | — | 删除：MySQL 固定 UTC |
| `db.store.strict` | — | 删除：列表查询总是严格（未知参数 400） |
| `db.store.require_principal` / `admin_roles` / `default_page_size` / `max_page_size` | `db.store.*` | 同名；新增 `db.store.owner_scope` |
| `db.instances.<name>.*` | — | 删除（见 §1） |
| `account.enabled` | — | 删除：不想要就不构造 identity |
| `account.signing_key` | `auth.signing_key` | secret，经 `MYAPP_AUTH_SIGNING_KEY` 注入 |
| `account.expiration` | `auth.token_ttl` | |
| `account.reset_expiration` | `auth.reset_ttl` | |
| — | `auth.refresh_ttl`（168h）、`auth.prefix`（`/auth`） | 新增；refresh-token 不再能无限续 |
| `account.login_rate_window` / `login_rate_limit` | `auth.login_rate_window` / `login_rate_limit` | yaml 默认 15m / 10（v2 默认不开） |
| `account.disable_register` / `link_by_email` / `allowed_redirect_backs` / `oauth_callback_frontend_url` | `auth.*` | 同名 |
| `account.providers.<name>.enabled` | — | 删除：代码决定链接哪些 provider |
| apple 的 `service_id` / `private_key` / `issuer_url` / `client_secret_ttl` | `auth.providers.apple.client_id` / `client_secret` / `endpoint` / — | client secret 固定 1h、到期前重签 |
| google 的 `issuer_url` | `auth.providers.google.endpoint` | |
| github 的 `enterprise_url` | `auth.providers.github.endpoint` | |
| facebook 的 `api_version` | `auth.providers.facebook.endpoint` | 写带版本的完整 Graph 地址，如 `https://graph.facebook.com/v21.0`；必填 |
| `audit.enabled` / `drop_on_full` | `auth.audit.enabled` / `drop_on_full` | 审计并入 identity |
| `audit.async_buffer_size` | `auth.audit.async_buffer` | |
| `audit.retention_days`（天数，180） | `auth.audit.retention`（时长，4320h；0 永久） | |
| `audit.enable_admin_api` | `auth.audit.admin_api` | |
| `audit.purge_interval` / `purge_batch_size` | — | 删除：固定每小时、每批 1000 |
| `authz.*`（`enabled`、`driver`、`casbin.*`） | — | 整段删除，改为代码：`rbac.New(auth, d, rbac.Model(text), rbac.Watcher(client))` |
| `authz.casbin.bootstrap_admin_user_id` | — | 代码：`rb.Policy().BootstrapAdmin(ctx, userID)` |
| `authz.casbin.redis_watcher_channel` | — | 固定 `chok:authz:policy` |
| `authz.casbin.audit_enabled` | — | 跟随 `auth.audit.enabled` |
| `scheduler.enabled`、`outbox.enabled` | `jobs.enabled` | 合并：false 时本进程只 Enqueue |
| `scheduler.stop_budget` | `jobs.stop_budget` | |
| `outbox.poll_interval` / `batch_size` / `settle_window` / `retention` / `cleanup_interval` | `jobs.*` | 同名；`batch_size` / `retention` 不再热更 |
| `cache.enabled` | — | 删除：三层全关是错误 |
| `cache.memory.*`、`cache.redis.enabled` / `ttl`、`cache.breaker.*` | `cache.*` | 同名 |
| `redis.enabled` | `cache.redis.enabled` | 独立的 redis 模块并入 cache |
| `redis.addr` / `username` / `password` / `db` / `tls` / `ca_cert` / `dial_timeout` / `read_timeout` / `write_timeout` / `pool_size` | `cache.redis.*` | 同名 |
| `redis.pool_timeout` | — | 删除（go-redis 缺省 ReadTimeout + 1s） |

## 3. HTTP

### 3.1 路由与 handler

| v2 | v3 |
|---|---|
| `web.GET/POST/PUT/PATCH[T, R](r, pattern, fn, opts...)` | `chok.GET/POST/PUT/PATCH[T, R](r, pattern, fn, opts...)` |
| `web.DELETE[T](r, pattern, fn handler.ActionFunc[T])`、`handler.HandleAction` | `chok.DELETE[T, chok.None](…)`：`R = chok.None` 即 204 无体 |
| `handler.HandleRequest(fn, opts...)` + `r.Handle(...)` | `chok.GET` 等一行注册 |
| `handler.HandleList[T](lister)` | `chok.List(r, pattern, lister)`；`store.Store` 实现 `chok.Lister` |
| `handler.WithSuccessCode(201)` | `chok.Created()` 或 `chok.Status(code)` |
| `handler.WithSummary` / `WithTags` / `WithMaxBodySize` | `chok.Summary` / `chok.Tags` / `chok.MaxBody` |
| `handler.WithPublic` | 删除：文档按路由经过的认证中间件标 security，没有认证中间件的路由就是公开的 |
| — | `chok.Use(mw...)`（路由级中间件）、`chok.Deprecated()` |
| `handler.Validated(fn, validators...)`、自定义 `Binder` | 删除：用 `validate` tag，或请求类型的 `Default()`，或在 handler 里检查 |
| `web.Module(web.WithMiddleware(mw...))` | `chok.New(base, chok.Wrap(mw...))` |
| `kernel.Middleware` | `chok.Middleware`（`func(http.Handler) http.Handler`） |
| `handler.WriteError` | `chok.WriteError`（给中间件用；handler 直接返回 error） |

### 3.2 绑定

| v2 tag | v3 tag |
|---|---|
| `uri:"rid"` | `path:"id"`（名字跟着路由模板） |
| `form:"q"` | `query:"q"` |
| `header:"X-Key"` | `header:"X-Key"` |
| `json:"title"` | `json:"title"` |
| `binding:"required,max=200"` | `validate:"required,max=200"` |

v3 用 `encoding/json/v2` 解码请求体：字段名大小写敏感、重复键与未知字段都是
400（`metadata.field` 指出哪个）；非 JSON 的 Content-Type 是 415（v2 是 400），
超过上限 413。JSON 体先解码，再由 query、header、path 覆盖，带这三种 tag 的
字段不从 JSON 体取值。`time.Duration` 在请求与响应里是字符串（`"1h30m0s"`），
读入也接受纳秒整数。响应里 nil slice 写成 `[]`。校验错误 400，reason
`Validation`，`metadata.fields` 以 JSON 名（或 path / query / header 名）为键。

### 3.3 错误

| v2 | v3 |
|---|---|
| `apierr.Error`、`apierr.New(code, reason, msg)` | `chok.Error`、`chok.NewError(code, reason, msg)` |
| `apierr.ErrNotFound` … | `chok.ErrNotFound` …（`With`、`Msgf`、`Wrap` 派生副本，`errors.Is` 按 code + reason 匹配） |
| `apierr.RegisterMapper(m)`、`WithErrorMapper`、`MapperRegistry` | 删除：错误类型实现 `chok.Coder{ APIError() *chok.Error }` |
| `apierr.RenderHook` | `chok.OnError(hook)`：一个 App 至多一个，返回 nil 表示不接管 |
| `store.MapError(err)` | 删除：store 的错误类型自己实现 `Coder` |

信封形状不变：`{"code","reason","message","request_id","metadata"}`，下游客户端
不用改。未实现 `Coder` 的错误一律 500，原错误只进日志；`context.DeadlineExceeded`
是 504（reason `Timeout`）；没有路由 404 信封、方法不对 405 带 `Allow`。

### 3.4 中间件、OpenAPI、健康与调试

- 默认中间件栈（Recover、RequestID、ClientIP、访问日志、超时、CORS）保留，次序
  冻结；遥测由 `telemetry` 作为 `Instrumenter` 插进去，不再由 web 模块按角色自动挂。
  CSRF 用标准库的 `http.CrossOriginProtection`（`http.csrf: true`）。
- 访问日志字段：method、path、route、status、bytes、duration、request_id、
  client_ip；JSON 格式下 `duration` 是 `"1.5ms"` 这样的字符串。
- OpenAPI 从 3.0 升到 3.1；文档在 `/openapi.json`、UI 在 `/docs`（v2 是
  `/swagger` 与 `/swagger/doc.json`）。自动摘要从「Create login」式拼接改为
  「Create post」「Get post」「List posts」。
- `/healthz`、`/livez`、`/readyz` 路径不变；`/componentz` 删除；
  `app.debug: true` 挂 `/debug/pprof/*` 与 goroutineleak profile。

## 4. 数据

### 4.1 db

| v2 | v3 |
|---|---|
| `db.Module(db.WithTables(...), db.WithMigrations(fs))`、`db.From(k)` | `d := db.MustOpen(cfg.DB, db.Tables(...), db.Migrations(fs))` |
| `db.Open(db.Options)` | `db.Open(db.Config, opts...)` |
| `db.RunInTx(ctx, h, fn)` | `d.RunInTx(ctx, fn)` |
| `db.InTx(ctx)`、`db.AfterCommit(ctx, fn)` | `d.InTx(ctx)`、`d.AfterCommit(ctx, fn)` |
| `h.Unsafe(ctx)` | `d.Unsafe(ctx)`（同样是唯一的 gorm 出口） |
| `db.Migrate(ctx, gdb, specs...)`、`db.ApplyMigrations(ctx, h, fs)` | `d.Migrate(ctx)`（Open 已按 `db.migrate` 模式做过）或 `chok migrate up` |
| `db.MigrationsStatus(ctx, h, fs)` | `d.MigrationStatus(ctx)` 或 `chok migrate status` |
| `chok migrate repair retry / mark-applied / accept-drift` | `d.MarkApplied(ctx, version)` / `chok migrate mark-applied <version>` 是唯一的人工干预 |
| `db.EnableTracing(gdb)` | `tel.Instrument(d)`（§7） |
| `db.ForeignTable`、公开的 `AppendOnlyModel` / `AppendStore` | 删除（电池内部的普通表经 internal 声明） |
| 只读句柄（`read_only`）、多实例、dbresolver、sequence、manifest、repair history、迁移 fence、baseline 收养 | 删除；SQLite 文件库自带读池 |
| `rid.New(prefix)`、`rid.Parse` | `db.NewRID(prefix)`、`db.ParseRID(rid)` |
| 模型基座 `db.Model` / `SoftDeleteModel` / `OwnedModel` / `OwnedSoftDeleteModel` | 同名，字段相同 |
| `db.Table(model, db.SoftUnique(...))` | 同名；三方言统一为 `UNIQUE(cols..., delete_token)`（v2 在 Postgres 上是部分索引） |

时间：v3 以 UTC 写入所有时间（MySQL 会话 `+00:00`、Postgres 会话 UTC、SQLite 存
`+00:00` 结尾的文本）。两种 v2 库要换算已有数据：

- **SQLite，机器时区不是 UTC**：v2 按进程的本地时区写时间（带偏移的文本），而
  SQLite 按文本比较时间，新旧行在排序、范围查询与游标里的次序会错（聚合按瞬时
  比较，不受影响）。迁移时把各表的时间列改写成 UTC。
- **MySQL，`mysql.time_zone` 不是缺省的 `utc`**：DATETIME 列存的是那个时区的
  墙钟，v3 按 UTC 读写，迁移时按原时区换算成 UTC。

Postgres 的 `timestamptz` 存的是瞬时，不受影响。

### 4.2 版本化迁移

| v2 | v3 |
|---|---|
| 文件 `NNNN_name.sql`（递增序号） | `<14 位版本>_<name>.sql`，版本是 UTC 时间戳，如 `20261025080000_create_notes.sql` |
| 应用账本 `schema_migrations`；电池各自的 `schema_migrations_chok_<kind>`；`schema_migrations_chok_manifest` / `_repairs` | 单表 `chok_migrations(owner, version, name, checksum, applied_at, statement_index)`，应用的 owner 是 `app`，电池用 `identity` / `rbac` / `jobs` |
| `chok migrate up --component <kind>` / `--all-owned` | `chok migrate up --battery <name>`；账本里已有的电池自动包括 |
| dirty 状态与 `repair` 三个动作 | MySQL 逐语句断点续跑；其余方言整文件一个事务；`mark-applied` |

已经用 v2 版本化迁移的库，迁移一次即可：

1. 把每个已应用的 `NNNN_name.sql` 改名为 14 位版本（保持顺序，例如
   `0001_init.sql` → `20260101000001_init.sql`），内容不动；需要按方言区分的放进
   `sqlite/` `mysql/` `postgres/` 子目录。
2. 用 v3 的配置（`db.migrate: off` 或 `versioned`）对这个库逐个登记：
   `chok migrate mark-applied 20260101000001` …（它只写账本，不执行 SQL）。
3. `chok migrate status` 应全部是 applied；之后的新迁移用 `chok migrate create`。
4. 确认无误后可以删除 v2 的 `schema_migrations*` 表。

电池的表另见 §5（identity、rbac）与 §6（jobs）。`migrate: auto` 的库不需要这些
步骤：AutoMigrate 会补上缺的列与索引，但不会改名列——identity 审计表的两列要手工
改名（§5）。

### 4.3 store

| v2 | v3 |
|---|---|
| `store.New[T](h, logger, opts...)` | `store.New[T](d, opts...)` |
| `store.WithQueryFields(...)` / `WithUpdateFields(...)` / `WithAllQueryFields` / `WithAllUpdateFields`、按 JSON tag 自动发现 | 模型字段上的 `store:"query"`（URL 的过滤与排序）/ `store:"update"`（Patch）/ `store:"query,update"`；没有任何 tag 的模型在 New 时 panic。代码里的 where 选项、聚合、`Pluck`、`ListIn`、Upsert 冲突字段与 `store.Set` 不受 tag 限制，接受模型的每一列；代码里的未知字段是 500（同 v2），URL 里的是 400 |
| `WithRequirePrincipal` / `WithoutRequirePrincipal` / `WithAdminRoles` / `WithDefaultPageSize` / `WithMaxPageSize` / `WithoutOwnerScope` | `db.store.*` 配置，或 `store.Policy(db.StorePolicy{…})` |
| `store.SetDefaultAdminRoles(...)` | 删除：`db.store.admin_roles` |
| `WithScope(fn)`、`OwnerScope(...)` | `store.Scope(fn)`；owner scope 由 Owned 基座自动启用 |
| `WithColumnAlias` / `WithConstraintFields` | `store.Alias` / `store.Constraints` |
| `WithBeforeCreate` / `WithBeforeUpdate` / `WithBeforeDelete` | `store.Hooks(store.HookSet[T]{BeforeCreate, BeforeUpdate, BeforeDelete})`；BeforeUpdate 收到 `map[string]any` 变更快照 |
| `WithBus` / `EntityChanged`、`WithReadOnly`、`WithStrict` | 删除 |
| `store.Sum[N](ctx, s, field)`、`Avg`、`Min`、`Max`、`CountDistinct`、`Pluck`、`PluckIDs`、`ListIn`、`GroupBy` | 方法：`s.Sum[N](ctx, field)`、`s.Avg(ctx, field)` … `s.GroupBy[K](ctx, field, aggs)` |
| `store.PluckDistinct`、`ListByIDs`、`BatchUpdate`、`WithRowsAffected`、`Fields(obj, ...)`、`NoLock` | 删除：`Pluck` 后自行去重、`ListIn`、逐条 `Update`、`store.Set(map)` |
| `store.Patch(req)` + `Onto(obj)`、`WithVersion(v)` | `store.Patch(req).Onto(obj)`；版本来自请求的 `version` 字段或 `where.Version(v)`；`Onto(nil)` 是不读行的补丁 |
| `store.Set(kv)` 返回 `Changes` 接口 | `store.Set(kv)` 返回 `*store.Changes` |
| 聚合读数据库 catalog 校验真实列类型 | 按模型的 Go 类型与声明的列类型做静态检查，不读 catalog |
| 哨兵 `ErrNotFound`、`ErrStaleVersion`、`ErrDuplicate`、`ErrMissingConditions` … | 错误类型 `NotFoundError`（404）、`VersionConflictError`（409）、`DuplicateError`（409）、`EmptyChangesError`（400）、`NoPrincipalError`（401）、`MissingConditionsError`（500）；`errors.As` 取类型，或 `errors.Is(err, chok.ErrNotFound)` 等 |
| `where.Page[T]`、`CursorPage[T]` | `chok.Page[T]`（游标页同一个类型，`Cursor` 字段） |
| `Upsert` / `BatchUpsert` 必须给冲突列 | 缺省冲突键是 rid；带 owner scope 或自定义 scope 的 store 拒绝 Upsert |

### 4.4 store/where

| v2 | v3 |
|---|---|
| `where.WithFilter(f, v)` | `where.Eq(f, v)` |
| `WithFilterOp(f, op, v)` | `where.Ne` / `Gt` / `Gte` / `Lt` / `Lte` |
| `WithFilterIn` / `WithFilterLike` / `WithFilterContains` / `WithFilterStartsWith` / `WithFilterEndsWith` / `WithFilterNull` / `WithFilterNotNull` | `where.In` / `Like` / `Contains` / `StartsWith` / `EndsWith` / `Null` / `NotNull` |
| `WithFilterLikeRaw` | `where.Like`（本来就保留调用方的通配） |
| `WithOrder` / `WithPage` / `WithLimit` / `WithOffset` / `WithCount` | `where.Order` / `Page` / `Limit` / `Offset` / `Count` |
| `WithCursor` / `WithCursorBy` / `WithCursorByField` 与方向 | `where.Cursor(token, size)`（不透明游标，绑定排序字段与方向） |
| `store.WithPreload` / `WithTrashed` / `WithOnlyTrashed`（QueryOption） | `where.Preload` / `WithTrashed` / `OnlyTrashed` |
| `where.FromQuery` / `FromQueryStrict` | `where.FromQuery(q, allowed, strict)` |
| `WithMaxPageSize`、`Apply`、`PageInfo`、`MaxInList` 等导出常量 | 删除 |

`Contains` / `StartsWith` / `EndsWith` 的转义字符是 `!`（反斜杠会被 MySQL 吞掉）；
`In` 至多 500 个值，更多用 `s.ListIn`。

## 5. identity（account + auth + authz + audit）

| v2 | v3 |
|---|---|
| `account.Module(account.WithOptions(...), account.WithProviders(...))` | `auth := identity.MustNew(cfg.Auth, d, identity.Providers(google.New(cfg.Auth.Providers["google"])), identity.Sender(mail))`，`app.Use(auth)` |
| `account.Authn(k)` | `auth.Authn()` |
| `auth.Principal`、`auth.PrincipalFrom` | `chok.Principal`、`chok.PrincipalFrom(ctx)`；`UserID` 是用户的 RID（`usr_…`） |
| `account.WithExpiration` / `WithResetExpiration` / `WithLoginRateLimit` … | `auth.*` 配置键（§2.2） |
| `account.WithPasswordCost` | 删除（bcrypt cost 12；测试经 choktest 自动降低） |
| `account.Sender`：`Send(ctx, to, code string) error`，`WithSender` | `identity.MailSender`：`Send(ctx, identity.Mail) error`，`identity.Sender(s)`；`Mail` 带 `Kind`（`password-reset` / `login-code`）、`To`、`Token`、`Code`、`Link`、`ExpiresAt`，按 `Kind` 选模板，通常交给新的 `mail` 电池发（见 [mail.md](mail.md)）。发信在后台进行，失败只记 Warn，`Close` 会等在途的发送 |
| `account.WithOAuthSessionStore` / `WithAuthCodeStore`、`MemorySessionStore`、`CookieCarrier` | 删除：OAuth 进行中的状态只在进程内（多副本须会话粘滞） |
| `auth/jwt` 的可插拔 `TokenParser` / `PrincipalResolver` | 删除：`Authn` 只认本 Identity 签发的 HS256 token，每个请求按主键读一次用户 |
| `account/providers/{google,github,apple,facebook}` | `identity/{google,github,apple,facebook}`，各只导出 `New(identity.ProviderConfig)` |
| `authz.Module()`、`authz/casbin` | `rb, err := rbac.New(auth, d, opts...)`，`app.Use(rb)`；`rb.Require(obj, act)` / `rb.RequireInDomain(obj, act, param)` 中间件；`rb.Policy()` 策略服务 |
| `audit.Module()` 与它的 `audit.Logger`：`Log(ctx, audit.Entry)` / `LogSync` / `Query` | `auth.Audit().Log(ctx, identity.Event)`（异步）/ `LogSync`（随 ctx 的事务）/ `ListFromQuery`（`/audit/logs` 用）；常量 `audit.ResultSuccess`、`ActorTypeUser` → `identity.ResultSuccess`、`ActorUser` |

行为变化：

- **会话上限**：refresh-token 沿用登录时的会话开始时间签新 token，超过
  `auth.refresh_ttl`（168h）就 401，用户要重新登录；v2 只要 token 有效就能一直续。
- **吊销即时**：`Authn` 每个请求读用户（存在、active、`password_version` 与
  token 一致），禁用、改密、改角色、`RevokeTokens` 立即生效。
- **新路由** `GET {prefix}/me`：`{id, email, name, email_verified, roles,
  created_at}`。其余路由与请求 / 响应体逐字沿用 v2（register 201、login 200，体都是
  `{token, expires_at}`；forgot-password 一律 204）。
- **登录限速**默认开（15m / 10，按 email 与客户端 IP），429 带 `Retry-After`。
- **`/audit/logs`** 路径不变：没有 rbac 时要求 `users.roles` 含 `admin`，挂了 rbac
  后改由 rbac 判 `audit/read`。
- **决策审计**跟随 `auth.audit.enabled`；watcher 频道固定 `chok:authz:policy`。

表的变化（v3 的表名沿用 v2，不加前缀）：

| 表 | 变化 | v2 库怎么升 |
|---|---|---|
| `users` | 不变 | — |
| `identities` | `ix_identity_user_provider` 改为真正的 `(user_id, provider)` 复合索引；单列 `idx_identities_user_id` 去掉 | 按 v3 的 SQL（`identity/migrations/<方言>/`）重建这两个索引 |
| `audit_logs` | `before` / `after` 改名 `before_data` / `after_data`（MySQL 保留字）；`actor_id` / `resource_id` / `request_id` 加宽到 128；`action` / `result` NOT NULL；三个 `*_time` 索引真正包含 `occurred_at` | 改名并加宽列、重建索引 |
| `casbin_rule` | 不变 | — |
| `auth_login_codes` | v3 新增（无密码登录的验证码，只存 HMAC） | 跑版本化迁移 `20261025000002` 建表，见下 |
| `schema_migrations_chok_account` / `_audit` / `_authz` | 不再使用 | 见下 |

用版本化迁移（`db.migrate: versioned` 或 `off`）的 v2 库：先按 v3 的 SQL
（`identity/migrations/<方言>/`、`identity/rbac/migrations/<方言>/`）把
identities 的索引与 audit_logs 的列改到一致，再登记电池的版本（只写账本，不执行
SQL）：

```sh
chok migrate mark-applied 20261024000001   # identity：users、identities
chok migrate mark-applied 20261024000002   # identity：audit_logs
chok migrate mark-applied 20261024000003   # rbac：casbin_rule
chok migrate up --battery identity         # 真正执行 v3 新增的 20261025000002（auth_login_codes）
chok migrate status --battery identity --battery rbac   # 全部 applied
```

版本被多个 schema 共享时用 `--owner` 指明。

`db.migrate: auto` 的库不用登记：auto 模式下电池不写账本，AutoMigrate 会补上缺的列
与索引，但不会改名列——`before` / `after` 两列仍要手工改名。

## 6. jobs（scheduler + outbox）

| v2 | v3 |
|---|---|
| `scheduler.Module()` + 同伴组件 `Register(job)` | `jobs.Periodic(name, spec, fn, overlap)` 选项 |
| `scheduler.Policy`：`PolicyDelayIfRunning` / `PolicySkipIfRunning` | `jobs.Overlap`：`OverlapQueue`（至多排一次）/ `OverlapSkip`；新增 `OverlapAllow`（并发运行） |
| `scheduler.RunNow`、`Entries()` | 删除 / `j.Stats()` |
| `scheduler.ErrBusy` | 删除：重叠由 `Overlap` 处理 |
| `scheduler.Pool`、`NewWithLocation(loc)` | 删除；时区写在每条 spec 前：`CRON_TZ=Asia/Shanghai 0 9 * * *` |
| `outbox.Module(outbox.WithRelay(name, h, outbox.OnTopics(t)))` | `jobs.Worker(t, fn)`：一个 topic 一个 worker，名字就是 topic |
| `outbox.From(k).EnqueueJSON(txCtx, topic, v)` | `j.Enqueue(txCtx, topic, v)`（payload 用 `encoding/json/v2` 编码；已有 JSON 传 `jsontext.Value`） |
| `outbox.Enqueue(txCtx, topic, []byte)` | 传 `jsontext.Value` |
| `func(ctx, outbox.Record) error` | `func(ctx, jobs.Message) error`（`ID uint64`、`Topic`、`Payload`、`CreatedAt` UTC） |
| `outbox.WithRelayFor[T]`、不过滤 topic 的 relay | 删除 |
| `outbox.ErrOutsideTx` | `jobs.ErrOutsideTx`（实现 `chok.Coder`，对客户端是通用 500） |

```go
j := jobs.MustNew(cfg.Jobs, d,
	jobs.Periodic("purge-sessions", "@every 10m", purge, jobs.OverlapSkip),
	jobs.Worker("order.placed", onOrderPlaced),
)
app.Use(d, j)
// 在业务事务里：
err := d.RunInTx(ctx, func(txCtx context.Context) error {
	if err := orders.Create(txCtx, o); err != nil {
		return err
	}
	return j.Enqueue(txCtx, "order.placed", o)
})
```

语义不变：至少一次、按 `(created_at, id)` 顺序、每个 topic 独立水位、失败只阻塞
自己的 topic、handler 必须幂等（按 `Message.ID` 去重）。部署假设也不变：worker 与
周期任务跑在一个 `jobs.enabled: true` 的进程里，其余进程 `enabled: false` 只
Enqueue；多进程分 topic 时 `retention` 必须为 0。

表：v2 的 `outbox_messages` / `outbox_relay_state` 不复用，v3 是新表
`chok_jobs_messages` / `chok_jobs_watermarks`（owner `jobs`），**没有数据迁移**——
升级前先让 v2 的 relay 把积压投递完。

## 7. telemetry（metrics + tracing）

| v2 | v3 |
|---|---|
| `metrics.Module()`、`chok.Get[*metrics.Component](k, "metrics").Registry()` | `tel := telemetry.MustNew(cfg.Telemetry)`，`tel.Registry()` |
| `tracing.Module()`、`TracerProvider()` | `tel.TracerProvider()` |
| web 模块按角色自动挂的 `middleware.Metrics` / `middleware.Tracing` | `app.Use(tel, …)`：telemetry 实现 `chok.Instrumenter`，并且放在第一个 |
| `db.EnableTracing(gdb)` 与 db 模块自动注册的指标 | `tel.Instrument(d)`（在构造电池之前调用，电池建表的语句也会记下） |
| — | `telemetry.logs.otel`：App 的 logger 桥接到 OTel Logs |

指标名按 OTel 语义约定改名，看板与告警要跟着改：

| v2 | v3 |
|---|---|
| `http_requests_total` | 删除：用 `http_server_request_duration_seconds_count` |
| `http_request_duration_seconds{method,path}` | `http_server_request_duration_seconds{http_request_method,http_route,http_response_status_code}` |
| `http_requests_in_flight` | `http_server_active_requests` |
| `db_query_duration_seconds{instance,op}`、`db_query_errors_total` | `db_client_operation_duration_seconds{db_system_name,db_operation_name,db_collection_name,error_type}` |
| — | `db_client_slow_operations_total`（阈值 = `db.slow_threshold`） |
| `db_pool_connections`、`db_pool_wait_*` | `go_sql_*{db_name}` |
| `db_migration_*`、`db_sqlite_maintenance_runs_total` | 删除（迁移状态看 `chok migrate status`） |
| `chok_component_up`、`chok_reload_total`、`chok_uptime_seconds` | 删除（运行时长用 `process_start_time_seconds`） |

span：HTTP 的 span 名仍是 `{method} {route}`；数据库从 `gorm.query` 改为
`{operation} {table}`（如 `SELECT posts`），属性按 OTel 的 `db.*` 语义约定。
stdout exporter 改为一行一个 JSON（v2 是多行 pretty print）。

## 8. cache（cache + redis）

| v2 | v3 |
|---|---|
| `cache.Module()`、`Component.Cache()` | `c := cache.MustNew(cfg.Cache)`，`app.Use(c)` |
| `cache.Cache` 接口、`Build(BuildOptions)`、`Chain(...)`、`WithBreaker(...)`、`NewMemory`、`NewRedis(WithTTL)` | 删除：层次只由 Config 决定（内存层在前、Redis 层在后、熔断器在 Redis 前） |
| `cache.GetOrLoad(ctx, c, key, loader, ttl)` | `c.GetOrLoad(ctx, key, ttl, load)`（参数次序变了） |
| `redis.Module()`、`Client()`、`redis.New(Options)` | `c.Redis()`（开了 redis 层才有；rbac 的 `Watcher` 可以直接用） |
| `redis.TLSConfigFor` | 删除（内部） |

行为变化：三层全关是 `New` 的错误；没开熔断器时 `Health` 会 PING Redis，于是
Redis 不可达会让启动相等到 `app.start_timeout` 后失败（v2 只告警）——要「Redis
不在也能启动」就开熔断器。熔断器半开后一个探测回来就能放下一个（v2 要等很久），
调用方自己取消的请求不再计为 Redis 故障。

## 9. 测试（choktest）

| v2 | v3 |
|---|---|
| `choktest.NewTestDB(t, models...)`、`db/dbtest.Open` | `choktest.DB(t, db.Table(&M{}), …)`：按 `CHOK_TEST_DRIVER` 给每个测试一个一次性数据库 |
| `choktest.NewTestStore[T](t, opts...)` | `choktest.Store[T](t, opts...)` |
| `choktest.NewTestKernel(t, yaml, comps...)`、`StartKernel`、`TestRouter` | `choktest.App(t, base, func(app *chok.App) {...})` 返回进程内 `Client`（`GetJSON` / `PostJSON` / `JSON` / `WithBearer`） |
| `choktest.NopLogger()` | `slog.New(slog.DiscardHandler)` |
| — | `choktest.Principal(ctx, p)`：直接给 store 测试塞身份 |

数据库 lane：`CHOK_TEST_DRIVER=postgres` / `mysql` 加对应的 DSN 让同一套测试跑在
真库上；v2 那种「有 MySQL DSN 就在任何 lane 连 MySQL」的行为没有了。

## 10. 命令行

| v2 | v3 |
|---|---|
| `chok init <name>`（生成 yaml + 装配） | `chok init <name>`：普通 Go 的 main.go、chok.yaml、Makefile、migrations/、带 `tool` 指令的 go.mod |
| `chok sync [--check]` | 删除：装配是代码 |
| `chok gen fields` | 删除：字段只认 `store` tag |
| `chok migrate create / up / status / repair` | `chok migrate create / up / status / mark-applied`（§4.2、§5）；`--battery` 把电池纳入 up / status，`--owner` 为 mark-applied 指明 schema |
| `chok openapi export --addr host:port --out file` | `chok openapi export <url> [--out file]` |
| `chok docs gen [--check]`（组件表、配置参考、JSON Schema、框架表） | 只在 chok 仓库里用，只生成 `docs/config.md` |
| `chok version [--json] [--short]` | `chok version [--json]` |
| `chok update [--ref]` | 删除：`go install github.com/zynthara/chok/v3/cmd/chok@<version>`，或项目里的 `go tool chok` |

## 11. 路由、表与指标的变化（汇总）

- **路由**：`/swagger`、`/swagger/doc.json` → `/docs`、`/openapi.json`；
  `/componentz` 删除；新增 `GET /auth/me`；`/healthz` `/livez` `/readyz`
  `/metrics` `/auth/*` `/audit/logs` 不变。
- **表**：`schema_migrations*`（应用、电池、manifest、repairs）→
  `chok_migrations`；`outbox_messages` / `outbox_relay_state` →
  `chok_jobs_messages` / `chok_jobs_watermarks`；`audit_logs` 两列改名、三列加宽、
  索引修正；`identities` 索引修正；`users` 与 `casbin_rule` 不变。
- **指标**：见 §7 的表。

## 12. 删除且没有替代的功能

| 功能 | 原因与替代做法 |
|---|---|
| 内核、`Descriptor`、事件总线、`/componentz` | 装配即代码；需要事后动作用 `d.AfterCommit` 或 jobs |
| 配置热重载（除 `log.level`）、文件监听、RCU 快照 | 重启进程；配置在启动时校验完 |
| 命名实例、只读实例、dbresolver | 在代码里多打开一个句柄 |
| 迁移的 dirty / fence / repair 历史 / baseline 收养 / manifest | 单账本 + MySQL 断点续跑 + `mark-applied` |
| `AppendStore`、`ForeignTable`、`WithRelayFor` | 普通 store；jobs 的消息表只给 worker 用 |
| `scheduler.Pool`、`RunNow`、`ErrBusy` | 应用自己的 goroutine 池；`Overlap` |
| store 的事件总线、`BatchUpdate`、`PluckDistinct`、`ListByIDs`、`WithRowsAffected`、聚合 catalog 闸门 | 见 §4.3 |
| OAuth 会话的外部存储 | 进程内（多副本粘滞会话）；以后需要时再加选项 |
| `chok sync`、`chok gen fields`、`chok update`、JSON Schema | 见 §10 |

## 附录 A：v2 包 → v3 去向总表

处置：**保留**（同一能力在 v3 有对应物，可能改名）、**合并**（并入另一个 v3 包或
电池）、**删除**（能力不再提供）。

| v2 包 / 功能 | v3 去向 | 处置 |
|---|---|---|
| 根 `chok.go` / `options.go`：App、Use、Run、Execute、6 个超时选项 | `chok.New` / `Use` / `Run` / `Execute`；超时进 `chok.Base`（`app.*`、`http.*`） | 保留 |
| 根：Section 句柄、`Override`、RCU 配置与 diff、reload tag、三阶段 reload、命名实例 | — | 删除 |
| 根 `watcher.go`：配置文件监听 | — | 删除 |
| 根 `signal.go`：INT / TERM / QUIT / HUP | `chok.Execute` 的信号处理 | 保留 |
| 根 drain：停机排空、/readyz 503 | `chok` 停机相 | 保留 |
| `kernel/`、`kernel/event` | — | 删除 |
| `conf/` | `chok.Load` / `MustLoad` + `internal/conf` | 合并 |
| `log/` | `chok` 的 slog 构建、`chok.LoggerFrom` | 合并 |
| `web/` | `chok.Router`、`App.Group`、`Mounter` | 合并 |
| `handler/` | `chok.GET/POST/PUT/PATCH/DELETE`、`chok.List`、`RouteOption` | 合并 |
| `apierr/` | `chok.Error` / `Coder` / `OnError` / `WriteError` | 合并 |
| `validate/` | 绑定时的 `validate` tag | 合并 |
| `swagger/` | `/openapi.json`、`/docs`（OpenAPI 3.1） | 合并 |
| `health/` | `/healthz` / `/livez` / `/readyz`、`Healther` | 合并 |
| `debug/` | `app.debug` 挂 pprof；componentz 删除 | 合并 |
| `middleware/` recovery、requestid、accesslog、timeout、cors、clientip | `chok` 默认栈 | 合并 |
| `middleware/authn.go` | `identity.Authn()` | 合并 |
| `middleware/authz.go` | `identity/rbac` 的 `Require` / `RequireInDomain` | 合并 |
| `middleware/metrics.go`、`middleware/tracing.go` | `telemetry` 的 `Middleware()`（Instrumenter） | 合并 |
| `internal/clientip`、`internal/ctxval`、`internal/txctx` | `internal/clientip`、`internal/reqinfo` 与 ctx 键、`db` 的事务 ctx | 保留 / 合并 |
| `internal/promutil` | — | 删除 |
| `internal/docgen` | `chok docs gen`（只生成 `docs/config.md`） | 合并 |
| `internal/fieldgen`、`internal/blessed`、`internal/fixture/*`、`internal/testschema`、`internal/testseq` | — | 删除 |
| `db/`：Open、方言、池、事务、模型、迁移、`sqlsplit`、SoftUnique、gormlogger、SQLite 维护 | `db` | 保留 |
| `db/`：readonly、sequence、manifest、repair history、dirty / fence、迁移租约、baseline 收养、多实例、dbresolver、`mysql.time_zone`、`ForeignTable`、公开的 `AppendOnlyModel`、`db.Module` | — | 删除 |
| `db/tracing.go`、`db/metrics.go` 的查询与连接池指标 | `telemetry.Instrument(d)` | 合并 |
| `db/metrics.go` 的迁移版本 gauge、SQLite 维护计数 | — | 删除 |
| `db/dbtest`、`db/internal/testlane` | `internal/dbtest`、`choktest.DB` | 合并 |
| `rid/` | `db.NewRID` / `ParseRID` | 合并 |
| `store/` | `store` | 保留 |
| `store/`：WithBus、`WithAll*Fields` 与自动发现、`Fields()`、`SetDefaultAdminRoles`、AppendStore、只读 store、BatchUpdate、PluckDistinct、ListByIDs、MapError、聚合 catalog | — | 删除 |
| `store/where` | `store/where` | 保留 |
| `choktest/` | `choktest.DB` / `Store` / `App` / `Client` / `Principal` | 保留 |
| `account/` + `account/providers/*` | `identity` + `identity/{apple,facebook,github,google}` | 合并 |
| `auth/`、`auth/jwt` | `identity` | 合并 |
| `authz/`、`authz/casbin` | `identity/rbac` | 合并 |
| `audit/` | `identity.Audit` | 合并 |
| `scheduler/` | `jobs.Periodic` / `Overlap` / `Stats` | 合并 |
| `outbox/` | `jobs.Worker` / `Enqueue` | 合并 |
| `metrics/`、`tracing/` | `telemetry` | 合并 |
| `metrics/` 的事件总线指标 | — | 删除 |
| （v2 无）slog → OTel Logs | `telemetry.logs.otel` | 新增 |
| `cache/` | `cache` | 保留 |
| `redis/` | `cache` 的 Redis 层与 `Cache.Redis()` | 合并 |
| `version/` | `chok version`（与 `chok.Version` 选项） | 合并 |
| `cmd/chok`：init、migrate、openapi、docs、version | `cmd/chok` | 保留 |
| `cmd/chok`：sync、gen、update | — | 删除 |
| `examples/blog` | `examples/blog`（重写，接 db、store、identity、telemetry） | 保留 |
| `docs/`、`hack/apidiff.sh`、`CHANGELOG.md`、`README_zh.md` | `docs/`（quickstart、design、config、api、本文）、`hack/apidiff.sh`、`CHANGELOG.md`、双语 README | 重写 |
