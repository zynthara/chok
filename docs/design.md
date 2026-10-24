# chok v3 设计文档

> 本文是 chok v3 架构的中文正文（团队惯例：设计文档中文、代码注释英文）。
> 源代码是真相的最终来源；本文讲清「为什么」与契约的边界。逐键的配置参考
> 是生成的 [config.md](config.md)，逐符号的公开面是生成的 [api.md](api.md)，
> 从 v2 升级看 [migration-v2-to-v3.md](migration-v2-to-v3.md)。

---

## 1. 定位与三条规则

chok 是一套有主见的 Go 应用工具包，模块路径 `github.com/zynthara/chok/v3`，
要求 Go 1.27。应用 `import` 它、用普通 Go 代码装配：没有内核、没有注册表、
没有生成文件。一份严格的类型化配置、带运行时 OpenAPI 的类型化 handler、
带 owner 隔离的 gorm 存储，用四个 import 就能写出一个 CRUD 资源；身份、
任务、遥测、缓存这些电池是你构造出来交给 `app.Use` 的值，不是去查找的服务。

v2 是「有内核、由 yaml 段驱动模块装配的框架」：段在场即装配、`chok sync`
生成装配代码、组件声明 `Descriptor`、依赖经 `k.Get` 查找、配置是可热换的
RCU 快照。v3 把这些全部拿掉，只留三条规则：

1. **配置是严格的。** 一个类型化 struct 由 `chok.Load[T]` 装入：默认值 <
   `chok.yaml` < 环境变量 < flag。未知键让启动失败并给出候选；只有
   `log.level` 能热更（SIGHUP）；没有任何东西在监听文件。
2. **错误描述自己。** 实现 `chok.Coder` 的错误自动映射为 HTTP 信封
   `code / reason / message / request_id / metadata`；没有错误映射注册表。
3. **装配就是代码。** 依赖是值：`db.Open` 返回的句柄交给 `store.New[T](d)`
   与 `identity.New(cfg, d)`。`app.Use` 只登记生命周期（Start / Close /
   Health / Mount），不做查找。

由这三条推出的约束：函数式选项不带 `With` 前缀、类型只叫 `Option` /
`RouteOption` / `LoadOption`；yaml 结构叫 `*Config`；构造函数以 `New` /
`MustNew` / `Open` / `MustOpen` 开头；`*gorm.DB` 只经 `Unsafe(ctx)` 离开
`db` / `store`；日志只用 `*slog.Logger`；停机路径从 `context.WithoutCancel`
派生；没有兼容垫片、没有 v2 旧名。命名规则由 `internal/lint/naming_test.go`
扫描每个公开包，豁免表里只有契约刻意保留的七个名字。

## 2. 包地图

公开包 10 个（预算的上限，provider 子包不计），其余一律 `internal/`：

| 包 | 职责 | 依赖 |
|---|---|---|
| `chok`（根） | 配置装载、App 与生命周期、路由与类型化 handler、绑定与校验、错误信封、默认中间件、OpenAPI 3.1 与 `/docs`、健康探针、slog、`Principal` | 不 import 任何电池，也不 import gorm |
| `db` | 打开三方言（SQLite / MySQL / Postgres）、池、事务、模型基座、RID、`Table` / `SoftUnique`、三种迁移模式与单账本 | gorm 与三个驱动 |
| `store` | 泛型 `Store[T]`：CRUD、owner 隔离、Patch 与乐观锁、游标、聚合与投影、自描述错误 | `chok`、`db`、`store/where` |
| `store/where` | 查询 DSL：`Eq` / `In` / `Like` / `Order` / `Page` / `Cursor` / `FromQuery` … | gorm（`Option` 的签名里出现 `*gorm.DB`，D11） |
| `identity` | 账户、JWT、登录限速、OAuth、审计；`Authn()` 中间件；`/auth/*` 路由 | `chok`、`db`、`store` |
| `identity/rbac` | casbin RBAC：`Require` / `RequireInDomain`、策略服务、Redis watcher | `identity`；casbin 与 go-redis 只在这里 |
| `identity/{google,github,apple,facebook}` | 四个 OAuth provider，各只导出 `New` | `identity` |
| `jobs` | 周期任务（内置 cron 解析器）与事务性 outbox worker | `chok`、`db` |
| `telemetry` | Prometheus 指标、OTel 链路、slog → OTel Logs、gorm 埋点 | `chok`、`db`；client_golang 与 otel |
| `cache` | 内存层（otter）、Redis 层、熔断器 | go-redis、otter |
| `choktest` | 测试辅助：`DB` / `Store` / `App` / `Client` / `Principal` | `chok`、`db`、`store` |

依赖方向只有一个：电池 → 根包（与 `db`）。电池之间不互相 import；需要跨包
协作而又不该成为公开 API 的地方，走 `internal/` 下的挂点：`internal/dbx`
（db 告诉 store 与电池的东西：SoftUnique 索引、无基座的普通表、语句观察者、
电池的迁移文件）、`internal/logtap`（telemetry 桥接 App 的 logger）、
`internal/tracectx`（审计记录补 trace id）、`internal/redislog`（go-redis
日志进 slog）、`internal/apptest`（choktest 读 App 的就绪状态）。挂点都由提供方
在 `init` 里赋值，使用方读取，公开面不变。

命令行 `cmd/chok` 是 main 包，不计入公开包与导出预算；cobra 只出现在那里。

## 3. 生命周期

```go
cfg := chok.MustLoad[Config]("blog")       // 失败：打印到 stderr，exit 2
app := chok.New(cfg.Base)                  // 按 log 段建 logger 并 slog.SetDefault
d := db.MustOpen(cfg.DB, db.Tables(...))   // 连接并按 migrate 模式处理 schema
auth := identity.MustNew(cfg.Auth, d)      // 注册自己的表，起后台协程
app.Use(tel, d, auth)                      // 登记生命周期；Mount 立即发生
chok.POST(app.Group("/api", auth.Authn()), "/posts", createPost)
app.Execute()                              // Run + 信号
```

`app.Use(components...)` 按值实现的接口分别处理：`Starter`（长驻，ctx 取消后
返回）、`Closer`、`Healther`、`Mounter`（电池挂自己的路由，在用户路由之前）、
`Namer`（健康报告里的名字）、`Instrumenter`（插进中间件栈的遥测槽位）；
什么都没实现的值被忽略并告警。Use 顺序 = Start 顺序 = Mount 顺序，Close 逆序。

**启动相**：按 Use 顺序在各自的 goroutine 里调用 `Start`，然后轮询所有
`Healther` 直到全部健康（受 `app.start_timeout` 约束）；期间任何 `Start`
返回错误即中止。全部健康后才开始监听 HTTP。没有长驻工作的 Starter 可以立即
返回 nil（如 `jobs.enabled: false`）；App 只把 ctx 未取消时返回的错误当失败。

**运行相**：`/readyz` 200；`/healthz` 聚合所有 Healther（任一失败 → 503，
body 列出每个组件的状态与耗时）；`/livez` 只说明进程活着。就绪后某个 Start
返回错误 → App 停机，`Run` 返回该错误。

**停机相**：`/readyz` 改 503 → 等 `http.drain_delay`（给负载均衡摘流）→
`http.Server.Shutdown(http.shutdown_timeout)` → 取消 Starter 的 ctx 并等待 →
逆序 `Close`，整个停机相受 `app.shutdown_timeout` 总预算约束。所有停机路径
从 `context.WithoutCancel(ctx)` 派生，trace id 与 request id 保持关联。

**信号**（`Execute`）：SIGINT / SIGTERM 优雅停；SIGQUIT 快停（跳过 drain，
预算封顶 5 秒）；SIGHUP 重读配置文件、只应用 `log.level`，其他改动记一条
「需要重启」。`Run(ctx)` 是不带信号的版本，供嵌入与测试；一个 App 只能 Run
一次。`app.Handler()` 返回完整处理链，测试与嵌入用它，不必监听端口。

电池的停机次序有讲究：`app.Use(tel, d, auth)` 把 telemetry 放第一个，它最后
关，才能把其他组件停机时产生的 span 与日志冲出去；`d` 在 `auth` 之前，于是
identity 关审计队列时数据库还在。

## 4. 配置

`chok.Load[T](name, opts...)` 一次装入整个类型化配置：

- **来源**（低到高）：`default` tag → 配置文件 → 环境变量 → flag。文件按
  `--config <file>` / `{PREFIX}_CONFIG` / `ConfigFile(...)` / `./chok.yaml` /
  `./{name}.yaml` / `./configs/{name}.yaml` 的顺序取第一个；都没有就只用默认值
  与环境变量。环境变量名由键路径派生：`http.addr` 是 `{PREFIX}_HTTP_ADDR`，
  前缀是大写的 name（`-` 与 `.` 换成 `_`）。列表用逗号分隔；flag 名等于点分
  键路径，只有 Changed 的生效。没有 `${VAR}` 插值：按路径派生的环境变量是
  唯一的注入方式。
- **结构**：`yaml:"key"` 命名键；无 tag 的匿名嵌入 struct 内联（`chok.Base`
  的 app / http / log 因此在顶层）；`default:"v"` 给默认值；`secret:"true"`
  在 `--check-config` 与 `conf.Dump` 里打码。
- **严格**：未知键 → `unknown key http.adr (did you mean addr?)`；未知段同理；
  类型错误点名路径与行号；先跑 `validate` tag（go-playground/validator，错误按
  yaml 路径报告），再递归调用 `Validate() error`；所有问题聚合成一个错误。
  对不上任何键的 `{PREFIX}_*` 环境变量只告警。
- **`--check-config`**：`Load` 在命令行上看到它就打印脱敏后的有效配置并
  exit 0，失败 exit 2，不构建 App。CI 可以直接拿应用二进制校验配置（D16）。
- **热更**：只有 `log.level`，经 SIGHUP。其余改动需要重启——这是刻意的：v2 的
  RCU 快照与三阶段 reload 换来的复杂度远大于收益。

电池的 Config 由应用嵌进自己的 struct（`Auth identity.Config \`yaml:"auth"\``），
段名由应用决定；`identity.New` / `jobs.New` 等在构造时再跑一次 `Validate()`，
并给代码里构造的零值填默认（布尔开关除外：`jobs.Enabled`、
`telemetry.Metrics.Enabled` 与 cache 的三个 `Enabled` 没有零值默认）。

## 5. HTTP

### 5.1 路由与类型化 handler

```go
type HandlerFunc[T, R any] func(ctx context.Context, req *T) (R, error)
chok.GET / POST / PUT / PATCH / DELETE[T, R](r, pattern, fn, ...RouteOption)
chok.List[T](r, pattern, lister, ...RouteOption)
```

路由只有这一种写法，底层是标准库 `ServeMux` 的模式路由；逃生门是
`r.Handle(method, pattern, http.Handler)`。一行注册同时完成：路由、请求绑定、
校验、响应编码、错误映射与 OpenAPI 登记。`RouteOption`：`Summary`、`Tags`、
`Status`、`Created`、`Public`、`Use`（路由级中间件）、`MaxBody`、`Deprecated`。

**绑定**：请求 struct 的字段 tag 是 `path` / `query` / `header` / `json` /
`validate`。JSON 体先解码（`encoding/json/v2`：大小写敏感、重复键与未知字段
400），再依次以 query、header、path 覆盖；带 path / query / header tag 的
字段不从 JSON 体取值，路径参数因此不能被请求体伪造。空 body 不解码；非 JSON
的 Content-Type 415，超上限 413。实现 `Default()` 的请求类型在校验前调用。
`time.Duration` 在请求与响应里写成字符串（`"1h30m0s"`），读入也接受纳秒整数。

**响应**：`R` 直接序列化，没有信封；`R` 为 nil 指针 → 200 `null`；
`chok.None` → 204；`T = chok.None` 表示不绑定。

**List**：`page` / `size` / `order=field:asc|desc` / 过滤字段 `field=value`
（等值；字段集合来自 `Lister.QueryFields()`，`store.Store` 由 `store` tag
得出）；`cursor` 是保留名；未知参数 400 并列出允许的字段。

### 5.2 错误

```go
type Error struct{ Code int; Reason, Message string; Metadata map[string]any }
type Coder interface{ APIError() *Error }
```

handler 返回 error，渲染器按顺序解析：`OnError` 钩子 → `*chok.Error` →
`Coder`（`errors.AsType`）→ validator 的错误（400 Validation，
`metadata.fields` 以字段的来源名为键）→ `context.DeadlineExceeded`（504
Timeout）→ 其余 500（原错误只进日志，不进响应）。信封总带 `request_id`；
`metadata.retry_after` 同时写 `Retry-After` 头。哨兵 `ErrBadRequest` …
`ErrInternal` 用 `With` / `Msgf` / `Wrap` 派生副本，`errors.Is` 按 code +
reason 匹配副本。store、identity、jobs 的错误都实现 `Coder`，所以 404 / 409 /
401 不需要任何注册。中间件（`identity.Authn`、`rbac.Require`）经
`chok.WriteError` 写同一种信封。没有路由 → 404 信封；方法不对 → 405 带
`Allow`。

### 5.3 默认中间件栈

外到内，顺序冻结：Recover → RequestID（`X-Request-Id`，响应回写）→
ClientIP（只信 `http.trusted_proxies` 里的代理）→ 遥测槽位（`Instrumenter`）
→ AccessLog（字段固定：method、path、route、status、bytes、duration、
request_id、client_ip；JSON 格式下 duration 是 `"1.5ms"` 这样的字符串）→
Timeout（`http.request_timeout` > 0 时）→ CORS（配置了 `http.cors` 时）→
CSRF（`http.csrf` 时套 `http.CrossOriginProtection`）→ `chok.Wrap(...)` →
路由组 → 路由级 `Use(...)`。请求内用 `chok.RequestID(ctx)`、
`chok.ClientIP(ctx)`、`chok.LoggerFrom(ctx)`（带 request_id 的子 logger）。

### 5.4 OpenAPI 与内建端点

OpenAPI 3.1 文档在 `http.openapi.path`（默认 `/openapi.json`），swagger-ui
在 `http.openapi.docs_path`（默认 `/docs`，资源走 CDN）。文档从路由表反射：
参数、请求与响应 schema、`validate` 规则（required / min / max / oneof →
enum / email → format）、List 的分页与过滤参数；命名类型进
`components.schemas`。没有写 `Summary` 时按「POST 集合 → Create <单数>、GET
带尾参 → Get <单数>、集合 → List <复数>、PUT → Replace、PATCH → Update、
DELETE → Delete」生成。非 `Public()` 的路由标 bearer（`http.openapi.bearer_auth`
可关）。内建端点 `/healthz`、`/livez`、`/readyz`、OpenAPI、`/docs` 与
`app.debug` 打开时的 `/debug/pprof/*`（含 goroutineleak）都带自己的路由模板，
访问日志的 `route` 与指标的 `http_route` 用它命名。

## 6. 数据

### 6.1 `db`：句柄、事务、模型

`db.Open(cfg, opts...)` 打开 `driver` 指定的方言并按 `migrate` 模式处理
schema；`*db.DB` 实现 Closer / Healther / Namer，交给 `app.Use`。

- **三方言同等地位**：SQLite 用纯 Go 的 glebarez 驱动，文件库是 WAL、一个写
  连接加一个读池（读路由：不在事务里、没有 `FOR` 锁子句的 SELECT 走读池）；
  MySQL 强制 `parseTime=true&loc=UTC`、会话 `time_zone='+00:00'`；Postgres
  会话固定 UTC。时间一律以 UTC 写入。
- **事务**：`d.RunInTx(ctx, fn)` 把事务放进 ctx，`store` 与 `d.Unsafe(ctx)`
  自动加入；`d.InTx(ctx)` 判断，`d.AfterCommit(ctx, fn)` 登记提交后的动作。
- **gorm 的唯一出口**：`d.Unsafe(ctx)`（事务感知）。`grep '\.Unsafe('` 就是
  全部越权点。
- **模型基座**：`db.Model`（`ID uint`、`RID string`、`Version int`、
  `CreatedAt` / `UpdatedAt`）、`SoftDeleteModel`（加 `DeletedAt` +
  `DeleteToken`）、`OwnedModel`（加 `OwnerID`）、`OwnedSoftDeleteModel`。
  对外 ID 是带前缀的 RID（`pst_…`，模型实现 `RIDPrefix()`），数字主键不出进程。
- **`db.Table(model, db.SoftUnique(name, cols...))`**：软删模型上的唯一约束
  三方言同一形状 `UNIQUE(cols..., delete_token)`，活行的 token 为空串，软删时
  写入随机值，于是删除释放唯一值。可空列、非软删模型在 `db.Table` 处 panic。

### 6.2 迁移：三种模式、单账本

| `db.migrate` | 行为 |
|---|---|
| `auto`（默认） | AutoMigrate 声明的表并建 SoftUnique 索引；版本化 SQL 不执行 |
| `versioned` | 启动时在跨进程锁下按版本顺序应用未应用的 SQL 文件 |
| `off` | 什么都不做，但有待应用 / 部分完成 / 漂移 / 缺失的文件就拒绝启动（`db.ErrNotMigrated`） |

迁移文件名 `<14 位版本>_<名字>.sql`，可以分进 `sqlite/` `mysql/` `postgres/`
子目录。只前进：没有 down，改已应用的文件 → 启动失败（`ErrChecksumDrift`），
修正就是下一个迁移。账本是单表 `chok_migrations(owner, version, name,
checksum, applied_at, statement_index)`：应用的 owner 是 `app`，电池
（`identity`、`rbac`、`jobs`）经 `d.RegisterSchema(owner, tables, fs)` 用
自己的 owner 记在同一张表里。锁：Postgres advisory lock、MySQL `GET_LOCK`、
SQLite 写连接的 `BEGIN IMMEDIATE`。

MySQL 的 DDL 隐式提交，所以 MySQL 上一条语句一条语句地执行并逐条记断点：
半途失败的文件留下 `applied_at` 为空的行，`checksum` 是已执行前缀的指纹；
下次从断点续跑，前缀被改过则拒绝。SQLite 与 Postgres 整个文件一个事务。
`MarkApplied(version)` 是唯一的人工干预（手工修好 schema 之后）。`chok migrate`
（§9）是这些能力的命令行门面。

### 6.3 `store`：安全栏

`store.New[T](d, opts...)` 校验模型（必须有 `store` tag、字段名拼写、
SoftUnique 列）后返回 `*Store[T]`，`T` 必须嵌入某个 db 基座：

- **字段只认 tag**：`store:"query"`（可过滤、排序）、`store:"update"`（可被
  Patch 写）、`store:"query,update"`；基座的 `id` / `created_at` /
  `updated_at` 默认可查询。没有 tag 的模型在 New 时 panic，并提示怎么写。
- **owner 隔离，失败关闭**：嵌入 Owned 基座就自动启用；principal 从
  `chok.PrincipalFrom(ctx)` 读。没有 principal：读返回空、写返回
  `NoPrincipalError`（401）。admin 角色由 `db.store.admin_roles` /
  `store.Policy` 决定。别人的行等于不存在（404，而不是 403）。
- **Patch 与乐观锁**：`store.Patch(req).Onto(obj)` 取请求 DTO 的非 nil 指针
  字段（只认 json 来源），携带版本；请求上 json 名为 `version` 的字段作为
  客户端声明的版本，不匹配 → `VersionConflictError`（409）。成功后 `obj` 的
  version 与 updated_at 已是新值。
- **读**：`Get` / `GetForUpdate`（必须在事务里）/ `Exists` / `Count` / `List`
  / `ListFromQuery`（实现 `chok.Lister`）/ `ListWithCursor`（不透明游标，绑定
  排序字段与方向，并列用 rid 裁决）。聚合与投影是 Go 1.27 的泛型方法：
  `Sum[N]`、`Avg`、`Min[N]`、`Max[N]`、`CountDistinct`、`Pluck[F]`、`PluckIDs`、
  `ListIn[V]`（每 500 个值一块）、`GroupBy[K]`；结果类型由 Go 类型决定，入口做
  静态检查，不读数据库 catalog。
- **写**：`Create` / `BatchCreate` / `Update` / `Delete`（软删写 token）/
  `Restore` / `Upsert` / `BatchUpsert`（缺省冲突键 rid；带 scope 的 store 拒绝
  Upsert，因为更新半边无法围栏）。`Hooks` 的 BeforeCreate / BeforeUpdate /
  BeforeDelete 在全部校验之后、任何 SQL 之前调用。
- **错误是类型**：`NotFoundError`（404）、`VersionConflictError` 与
  `DuplicateError`（409，`metadata.field` 由约束推出字段名）、
  `EmptyChangesError`（400）、`NoPrincipalError`（401）、`MissingConditionsError`
  （500，程序错误）。都实现 `chok.Coder`，`errors.Is` 对应 `chok.Err*`。

`store/where` 是查询 DSL：字段名一律是模型的公开名，由 store 映射到列，未声明
的字段 400。`Contains` / `StartsWith` / `EndsWith` 是字面匹配（转义字符 `!`，
因为反斜杠会被 MySQL 吞掉）；`In` 至多 500 个值；`Preload` 对带 Owned 基座的
关联按本 store 的策略用关联表自己的 owner_id 围栏。标识符不加引号，所以模型的
列名要避开 MySQL 保留字（`key`、`order`、`group`……）。

## 7. 电池

电池是值：构造、登记、用。它们只 import 根包与 `db`（identity 另用 `store`），
彼此不认识。

### 7.1 `identity`

`identity.New(cfg, d, opts...)` 在同一个 `*db.DB` 上注册 users、identities、
audit_logs 三张表（owner `identity`），并挂 `{prefix}`（默认 `/auth`）下的路由：
register（201，`disable_register` 时不挂）、login（200）、refresh-token、
change-password、me，以及有 `Sender` 时的 forgot / reset password；有 provider
时加 OAuth 的 start / callback / exchange 与 identities 管理；审计开着且
`admin_api` 时挂 `GET /audit/logs`。

- **`Authn()`**：Bearer HS256 JWT，**每个请求按主键读一次用户**：存在、
  active、token 的 `pv` 等于当前 `password_version`——禁用、改密、改角色、
  `RevokeTokens` 都立即生效。通过后写入 `chok.Principal{UserID: usr_…, Email,
  Roles, Extra}`，store 的 owner 隔离就读它。
- **会话上限**：`refresh_ttl` 从登录起算，refresh-token 不能无限续。
- **登录限速**：按 email 与客户端 IP 两维、只记失败，429 带 `retry_after`。
- **OAuth**：Google / Apple 走 OIDC（自实现校验）、GitHub、Facebook；
  `Provider` 是封闭接口，四个子包各只导出 `New(identity.ProviderConfig)`。
  配置了却没在代码里链接的 provider 让 `New` 失败。进行中的 OAuth 状态只在进程
  内：多副本须会话粘滞。
- **审计**：`Audit().Log`（异步入队，不进调用方事务）/ `LogSync`（同步，随 ctx
  事务）；trace id 由 telemetry 经内部钩子补上。
- **`identity/rbac`**：casbin 的 (sub, dom, obj, act) 模型，`Require` /
  `RequireInDomain` 中间件（无 principal 401、拒绝 403），策略服务，Redis
  watcher 同步多副本。不 import rbac 的应用不链接 casbin，也不建 casbin_rule 表。

### 7.2 `jobs`

一个 `jobs.New(cfg, d, opts...)` 管两类工作：

- `jobs.Periodic(name, spec, fn, overlap)`：robfig 语法的 cron（5 字段、可选
  秒、`@hourly`、`@every 10m`、逐条 `CRON_TZ=`），解析器移植进
  `jobs/internal/cron`（来源与许可见仓库根 `NOTICE`）。`OverlapSkip` 丢弃运行
  中到来的触发，`OverlapQueue` 至多排一次，`OverlapAllow` 并发运行。
- `jobs.Worker(topic, fn)` + `j.Enqueue(txCtx, topic, payload)`：事务性
  outbox。Enqueue 必须在同一个 `*db.DB` 的 `RunInTx` 里（否则
  `ErrOutsideTx`），消息与业务写入同生共死；worker 以 `(created_at, id)` 顺序
  **至少一次**投递，每个 topic 一个水位，失败只阻塞自己的 topic。handler 必须
  幂等（按 `Message.ID` 去重）。保证的前提：enqueue 事务在 `settle_window` 内
  提交，各进程时钟同步。

部署假设：所有 worker 与周期任务跑在**一个** `jobs.enabled: true` 的进程里，
其余进程 `enabled: false` 只 Enqueue。`Stats()` 给出每个作业的运行统计；
`Health` 恒 nil——毒消息不该把实例拉出轮转，失败看统计与日志。

### 7.3 `telemetry`

`telemetry.New(cfg)` 一个值管三件事，`app.Use(tel, …)` 放第一个：

- **指标**（client_golang，`telemetry.metrics.path` 默认 `/metrics`）：
  `http_server_request_duration_seconds{http_request_method, http_route,
  http_response_status_code}`（直方图，`_count` 就是请求数）、
  `http_server_active_requests`；`tel.Instrument(d)` 之后还有
  `db_client_operation_duration_seconds{db_system_name, db_operation_name,
  db_collection_name, error_type}`、`db_client_slow_operations_total`（阈值即
  `db.slow_threshold`）与连接池 `go_sql_*{db_name}`。名字按 OTel→Prometheus
  兼容规则写出。`Registry()` 可注册应用自己的 collector。
- **链路**（OTel SDK，exporter `stdout` 或 `otlp`）：server span 名
  `{method} {route}`，入站 W3C traceparent 续接，采样
  `ParentBased(TraceIDRatioBased(sample_ratio))`；`Instrument` 给每条 SQL 一个
  client span（`SELECT posts`）。
- **日志**：`logs.otel: true` 把 App 按 `log` 段建的 logger（含访问日志与
  `chok.LoggerFrom`）复制到 OTel Logs，带 trace / span id；用 `chok.Logger`
  注入的 logger 不桥接。

埋点不注册 gorm 回调：db 在 `Open` 时已给六条回调链挂好前后钩子，telemetry 经
`internal/dbx` 原子地挂观察者，所以句柄使用中也能 `Instrument`。

### 7.4 `cache`

`cache.New(cfg)`：内存层（otter，按条目 TTL）在前、Redis 层在后，熔断器在
Redis 前；三层全关是错误。`Get` 读穿并以内存层回填，`Set` / `Delete` 写穿；
`GetOrLoad(ctx, key, ttl, load)` 合并同 key 的并发 miss。`Redis()` 交出原生
客户端（rbac 的 watcher 可以直接用）。各副本的内存层互不失效。`Health`：
只开内存层、或 Redis 前有熔断器时恒 nil；没开熔断时 PING Redis——于是 Redis
不可达会让启动相等到 `app.start_timeout` 后失败，要「Redis 不在也能启动」就开
熔断器。

## 8. 测试

- **真库，不 mock**：`choktest.DB(t)` 按 `CHOK_TEST_DRIVER` 给每个测试一个
  一次性数据库——SQLite 临时文件（默认）、Postgres 的独立 schema（search_path）、
  MySQL 的独立库 `chok_test_<rand>`——`t.Cleanup` 删除。`choktest.Store` 在上面
  建 store，`choktest.Principal` 往 ctx 塞身份。
- **进程内 App**：`choktest.App(t, base, setup)` 跑完启动相但不监听端口，返回
  的 `Client` 经 `app.Handler()` 进程内发请求（`GetJSON` / `PostJSON` /
  `JSON` / `WithBearer`），`t.Cleanup` 跑停机相。
- **三方言 lane**：`make test` 是 SQLite lane（清空三个变量，`-race`）；
  `make test-pg` 与 `make test-mysql` 把 Makefile `DB_PKG_DIRS` 里的包
  （db、store、choktest、identity、jobs、telemetry、cmd/chok）**全量**重跑，
  不用 `-run` 挑测试；缺 DSN 时 lane 直接失败。CI 用 `postgres:16` 与
  `mysql:8.4` 服务，两条 lane 都阻断。
- **验收即文档**：`examples/blog` 的验收测试走一遍 README 的路径；`make smoke`
  起真实二进制、探 `/healthz` 与 `/metrics`、SIGINT 后 3 秒内退出；`chok init`
  的自检测试在三方言上生成、构建、启动脚手架；`internal/tools/doccheck` 编译
  quickstart 里的每个程序；`docs/config.md` 与 `docs/api.md` 由测试守着不腐烂。
- 时序逻辑优先 `testing/synctest`；测试从不占固定端口；每个 bug 修复与回归测试
  同一个提交。

## 9. 命令行

`cmd/chok`（`go install github.com/zynthara/chok/v3/cmd/chok@<version>`，或在
`chok init` 生成的项目里 `go tool chok`——go.mod 的 tool 指令让它与项目用的 chok
同一版本）：

- `chok init <name> [--module path]`：写一个 `go run` 即起的项目——main.go（一个
  资源与一组路由）、chok.yaml、Makefile、migrations/README.md、.gitignore 与
  带 tool 指令的 go.mod。
- `chok migrate create <name> | up | status | mark-applied <version>`：按应用
  的方式读 chok.yaml 的 db 段（含 `<NAME>_DB_*` 环境变量），作用于
  `chok_migrations`：应用的 owner 加上账本里已有或 `--battery` 指定的电池。
  `create` 在每个方言目录写一个空文件，版本是当前 UTC 时间；`status` 不改任何
  东西；`up` 与 `mark-applied` 拒绝 `migrate: auto` 的库；几个 schema 共享一个
  版本号时（identity 与 rbac 的第一个迁移），`mark-applied --owner` 指明哪一个。
- `chok openapi export <url> [--out file]`：保存运行中应用的 OpenAPI 文档，
  JSON 或 YAML。
- `chok version`：本命令构建自的 chok 版本、VCS 修订与工具链。
- `chok docs gen`（隐藏，只在 chok 仓库里用）：从 Config 类型生成
  `docs/config.md`。

没有 v2 的 `sync`（装配是代码，没有可生成的东西）、`gen fields`（字段只认
tag）与 `update`（用 `go install …@version`）。

## 10. 预算

公开面会膨胀，所以预算进了 CI：`make budget` 跑 `hack/apibudget.sh` 与
`hack/configkeys.sh`，任何一项超限或出现名单外的公开包都是红的。上限写在
`hack/budget.txt`，改它是计划层面的决定：

| 项 | 上限 | 怎么量 |
|---|---|---|
| 公开包（provider 不计） | 10 | 既非 internal 也非 main 的包；名单外的包直接失败 |
| 顶层导出 | 总 220；chok 64、db 30、store 30、store/where 28、identity 30、identity/rbac 8、jobs 15、telemetry 10、cache 10、choktest 10、provider 各 3 | `go doc -short` 的行数（构造函数缩在类型下、分组的 var / const 一行），方法不计；逐行见 [api.md](api.md) |
| 选项构造函数 | 40（不含 store/where 的谓词） | 结果类型是 `Option` / `RouteOption` / `LoadOption` 的函数 |
| 直接依赖 | 30 | `go list -m` 里非 indirect 的模块 |
| 配置叶键 | 150 | `chok.Base` 与六个 Config 的叶子；逐键见 [config.md](config.md) |
| examples/blog | 应用代码 120 行、chok 包 5 个 | 非测试 .go 的非空非注释行；不重复的 chok import |

质量门同样阻断：`make verify` = tidy-check、gofmt -s、vet、golangci-lint、
`go fix -diff` 为空、SQLite lane、Postgres lane、MySQL lane、budget、smoke，
本地与 CI 完全相同。

## 11. 版本与发布

- v1 封版于 `v0.1.4`；v2 封版于分支 `v2`（`v2.0.0-beta.N` 永久可装，只收安全
  修复）；v3 在 `main` 上开发，模块路径带 `/v3`。
- v3 以 `v3.0.0-alpha.N` 开始：手动裁切——`CHANGELOG.md` 条目 → `make verify`
  全绿 → 打带注释的 tag → push tag，goreleaser workflow 构建 `chok` 命令的
  各平台产物并发布 GitHub Release（`chok version` 显示 tag）。tag 一经推送
  不可重打（module proxy 永久缓存）。
- `hack/apidiff.sh` 以上一个 v3 release 为基线报告不兼容的 API 变更；alpha 期间
  它在 CI 里只报告不阻断，GA 前改为阻断。
