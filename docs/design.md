# chok 设计文档

> 本文是 chok（v0.2 起）架构的中文正文（团队惯例：设计文档中文、代码注释英文）。
> 源代码是真相的最终来源；本文讲清「为什么」与契约的边界。逐键的配置参考
> 是生成的 [config.md](config.md)，逐符号的公开面是生成的 [api.md](api.md)，
> 从 v2 升级看 [migration-from-v2.md](migration-from-v2.md)。

---

## 1. 定位与三条规则

chok 是一套有主见的 Go 应用工具包，模块路径 `github.com/zynthara/chok`，
要求 Go 1.27。应用 `import` 它、用普通 Go 代码装配：没有内核、没有注册表、
没有生成文件。一份严格的类型化配置、带运行时 OpenAPI 的类型化 handler、
带 owner 隔离的 gorm 存储，用四个 import 就能写出一个 CRUD 资源；身份、
任务、遥测、缓存、实时推送、邮件这些电池是你构造出来交给 `app.Use` 的值，不是
去查找的服务。

v2 是「有内核、由 yaml 段驱动模块装配的框架」：段在场即装配、`chok sync`
生成装配代码、组件声明 `Descriptor`、依赖经 `k.Get` 查找、配置是可热换的
RCU 快照。v0.2 把这些全部拿掉，只留三条规则：

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

公开包 12 个（预算的上限，provider 子包不计；`realtime` 与 `mail` 是 2026-10-25
加的第 11、12 个），其余一律 `internal/`：

| 包 | 职责 | 依赖 |
|---|---|---|
| `chok`（根） | 配置装载、App 与生命周期、路由与类型化 handler、绑定与校验、错误信封、默认中间件、OpenAPI 3.1 与 `/docs`、健康探针、slog、`Principal` | 不 import 任何电池，也不 import gorm |
| `db` | 打开三方言（SQLite / MySQL / Postgres）、池、事务、模型基座、RID、`Table` / `SoftUnique`、三种迁移模式与单账本 | gorm 与三个驱动 |
| `store` | 泛型 `Store[T]`：CRUD、owner 隔离、Patch 与乐观锁、游标、聚合与投影、自描述错误 | `chok`、`db`、`store/where` |
| `store/where` | 查询 DSL：`Eq` / `In` / `Like` / `Order` / `Page` / `Cursor` / `FromQuery` … | gorm（`Option` 的签名里出现 `*gorm.DB`，D11） |
| `identity` | 账户、JWT、登录限速、OAuth、邮件验证码的无密码登录、cookie 会话、审计；`Authn()` 中间件；`/auth/*` 路由 | `chok`、`db`、`store` |
| `identity/rbac` | casbin 权限：`Require` / `RequireInDomain`（读 `Principal.Roles`，`admin` 全通）、策略服务、Redis watcher | `identity`；casbin 与 go-redis 只在这里 |
| `identity/{google,github,apple,facebook}` | 四个 OAuth provider，各只导出 `New` | `identity` |
| `jobs` | 周期任务（内置 cron 解析器）与事务性 outbox worker | `chok`、`db` |
| `telemetry` | Prometheus 指标、OTel 链路、slog → OTel Logs、gorm 埋点 | `chok`、`db`；client_golang 与 otel |
| `cache` | 内存层（otter）、Redis 层、熔断器 | go-redis、otter |
| `realtime` | 实时推送：Hub、一次性 ticket、WebSocket 与 SSE 两种传输、续传缓冲、Redis backplane | `chok`；coder/websocket、go-redis |
| `mail` | 事务邮件：SMTP（STARTTLS / 隐式 TLS、AUTH PLAIN）、`.eml` 目录、日志三种传输，`html/template` + `text/template` 模板，附件与内嵌图片 | `chok`；只用标准库 |
| `choktest` | 测试辅助：`DB` / `Store` / `App` / `Client` / `Principal` | `chok`、`db`、`store` |

依赖方向只有一个：电池 → 根包（与 `db`）。电池之间不互相 import：两个电池要
协作时，由应用写一个闭包把它们接起来——`jobs.Observe(func(ev jobs.Event) {
hub.Publish(…) })` 把任务的运行事件交给 realtime。需要跨包协作而又不该成为公开
API 的地方，走 `internal/` 下的挂点：`internal/dbx`
（db 告诉 store 与电池的东西：SoftUnique 索引、无基座的普通表、语句观察者、
电池的迁移文件）、`internal/logtap`（telemetry 桥接 App 的 logger）、
`internal/tracectx`（审计记录补 trace id）、`internal/redislog`（go-redis
日志进 slog）、`internal/apptest`（choktest 读 App 的就绪状态）。挂点都由提供方
在 `init` 里赋值，使用方读取，公开面不变。另有一个内部接口
`internal/openapi.Securer`：identity 的 `Authn()` 返回的 handler 实现它，报告
自己接受的认证方案，根包在套中间件时识别它（§5.4）。

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

**停机相**：`/readyz` 与 `/healthz` 改 503 → 等 `http.drain_delay`（给负载均衡
摘流）→ `http.Server.Shutdown(http.shutdown_timeout)` → 取消 Starter 的 ctx 并
等待 → 逆序 `Close`，整个停机相受 `app.shutdown_timeout` 总预算约束。drain 期间
`/healthz` 不再跑检查，直接答 503、status 为 down、不列组件。drain 按需：只在
进程启动以来 `/readyz` 或 `/healthz`（`http.health.path` 配到哪就是哪）被请求过
时才等（有人在看——Kubernetes 探针、负载均衡的健康检查，后者常指向
`/healthz`），等之前记一条 Info；都没被请求过就跳过并记一条 Debug，开发时
Ctrl-C 立即停。开发时手动 curl 过 `/healthz` 也会等，再按一次 Ctrl-C 即可结束
（见下面的信号）。`/livez` 不算：存活探针不决定流量往哪去。
所有停机路径从 `context.WithoutCancel(ctx)` 派生，trace id 与 request id 保持
关联。

**信号**（`Execute`）：SIGINT / SIGTERM 优雅停；停机过程中再来一个 SIGINT /
SIGTERM，或任何时候的 SIGQUIT，快停（剩下的 drain 立即结束，剩余预算封顶 5 秒，
记一条 Info）；SIGHUP 重读配置文件、只应用 `log.level`，其他改动记一条
「需要重启」。`Run(ctx)` 是不带信号的版本，供嵌入与测试；一个 App 只能 Run
一次。`app.Handler()` 返回完整处理链，测试与嵌入用它，不必监听端口。

电池的停机次序有讲究：`app.Use(tel, d, auth)` 把 telemetry 放第一个，它最后
关，才能把其他组件停机时产生的 span 与日志冲出去；`d` 在 `auth` 之前，于是
identity 关审计队列时数据库还在；realtime 的 Hub 放在会发布事件的电池之后
（`app.Use(d, auth, jobs, hub)`），它先关、先向浏览器道别，而 jobs 在停机前跑完
的运行仍能发布出去（Starter 在任何 Close 之前就已停下）。

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
  对不上任何键的 `{PREFIX}_*` 环境变量只告警：`Load` 把告警存进 `Base`（未导出
  字段），`New` 建好 logger 后按原顺序经 App 的 logger 发出（`log.format: json`
  时就是 JSON 行，给了 `chok.Logger` 就走注入的 logger）；配置类型里没有 `Base`、
  或 `Load` 失败时当场经 `slog.Default` 发出；`--check-config` 打到 stderr。
- **`--check-config`**：`Load` 在命令行上看到它就打印脱敏后的有效配置并
  exit 0，失败 exit 2，不构建 App。CI 可以直接拿应用二进制校验配置（D16）。
- **热更**：只有 `log.level`，经 SIGHUP。其余改动需要重启——这是刻意的：v2 的
  RCU 快照与三阶段 reload 换来的复杂度远大于收益。

电池的 Config 由应用嵌进自己的 struct（`Auth identity.Config \`yaml:"auth"\``），
段名由应用决定；`identity.New` / `jobs.New` 等在构造时再跑一次 `Validate()`，
并给代码里构造的零值填默认（布尔开关除外：`jobs.Enabled`、
`telemetry.Metrics.Enabled` 与 cache 的三个 `Enabled` 没有零值默认；
`jobs.Retention` 也除外，0 表示永久保留，`168h` 只在配置文件没写这个键时生效）。

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
`Status`、`Created`、`Use`（路由级中间件）、`MaxBody`、`Deprecated`。

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
CSRF（`http.csrf`，默认开）→ `chok.Wrap(...)` → 路由组 → 路由级 `Use(...)`。
请求内用 `chok.RequestID(ctx)`、`chok.ClientIP(ctx)`、`chok.LoggerFrom(ctx)`
（带 request_id 的子 logger）。

CSRF 是标准库的 `http.CrossOriginProtection`：只查不安全方法（POST / PUT /
PATCH / DELETE）的 `Sec-Fetch-Site` 与 `Origin`，浏览器从别的网站发来的这类请求
得 403 信封；curl、移动端、服务间调用不带这两个头，不受影响。
`http.cors.allow_origins`（`*` 除外）加为可信来源，配了 CORS 的跨域前端照常可用。
它默认开着，关掉会告警：`http.csrf: false` 而有路由用 cookie 认证（认证中间件
报告了 cookieAuth，§5.4）时，`Run` 在开始监听前记一条 Warn。

第三方用表单把浏览器送回应用的回调（支付网关的回跳、3-D Secure、SAML 的 POST
绑定）是别的网站发起的 POST，由 `chok.AllowCrossSitePost(r, pattern)` 放行：只放
这一条路由的 POST，它的其他方法和别的路由照查。这样的路由要用第三方的签名或应用
自己发的 state 参数验明请求，不能只凭 session cookie 行事；应用自己的表单不用它。
identity 也用它为每个 OAuth provider 的 `POST {prefix}/{name}/callback` 放行
（Apple 的 `form_post` 回调是 appleid.apple.com 发起的跨站 POST，防线是 state
参数）。

### 5.4 OpenAPI 与内建端点

OpenAPI 3.1 文档在 `http.openapi.path`（默认 `/openapi.json`），swagger-ui
在 `http.openapi.docs_path`（默认 `/docs`，资源走 CDN）。文档从路由表反射：
参数、请求与响应 schema、`validate` 规则（required / min / max / oneof →
enum / email → format）、List 的分页与过滤参数；命名类型进
`components.schemas`。没有写 `Summary` 时按「POST 集合 → Create <单数>、GET
带尾参 → Get <单数>、集合 → List <复数>、PUT → Replace、PATCH → Update、
DELETE → Delete」生成。

一条路由的 `security` 由它实际经过的认证中间件决定：注册时 chok 逐层套中间件，
哪一层的 handler 报告了认证方案，这条路由就列出这些方案（任一即可，按链的顺序
去重）。计入的链是 `Group` 的中间件、`Handle` 的 `mw`、路由级 `Use(...)`，以及
`chok.Wrap(...)`（作用于所有路由，排在路由自己的方案前面）。identity 的
`Authn()` 报告 `bearerAuth`（http bearer，JWT），cookie 模式下再报
`cookieAuth`（cookie 里的 apiKey，名字是 `auth.cookie.name`）。文档里有任何方案
时，没经过认证中间件的路由写 `security: []`；整个应用都没有时，文档不写
security，也没有 `securitySchemes`；`components.securitySchemes` 是所有路由
报告的并集。所以文档与实际一致：忘了挂 Authn 的路由在文档里就是公开的，公开
路由也不用另外标。应用自己写的认证中间件不被识别，文档里显示为公开；要被识别
就用 identity 的 `Authn()`。

内建端点 `/healthz`、`/livez`、`/readyz`、OpenAPI、`/docs` 与
`app.debug` 打开时的 `/debug/pprof/*`（含 goroutineleak）都带自己的路由模板，
访问日志的 `route` 与指标的 `http_route` 用它命名。

### 5.5 单页应用：`chok.SPA`

`chok.SPA(r, prefix, fsys)` 在 prefix 下服务打包好的前端（Vite、webpack 等的
`dist`），通常嵌进二进制：

```go
//go:embed dist
var dist embed.FS

web, _ := fs.Sub(dist, "dist") // 去掉 dist/ 这一层，index.html 在根上
chok.SPA(app.Router(), "/", web)
```

- 注册 GET（ServeMux 的 GET 也接 HEAD）的 `{prefix}` 与 `{prefix}/{rest...}`；
  prefix 为 "/" 时就是全站兜底。更具体的路由——API、内建端点——照旧胜出，SPA
  不遮挡它们；不写方法的路由（method ""）与 SPA 的模式冲突，ServeMux 会 panic，
  所以 API 路由要写明方法。不进 OpenAPI，访问日志的 route 是注册的模式；fsys
  根上没有 index.html 时 panic。
- 存在的文件原样给（`http.ServeContent`：Content-Type、Range、条件请求）。ETag
  是内容的 SHA-256（强 ETag），按路径缓存，文件的大小或修改时间变了就重算：
  `embed.FS` 永远不变，开发时用 `os.DirFS` 改了文件，下个请求就是新的 ETag。
  `{prefix}/assets/` 下是打包器带哈希的产物，`Cache-Control: public,
  max-age=31536000, immutable`；其余文件与 index.html 一律 `no-cache`（每次带
  ETag 回源校验，没变就是 304）。
- 找不到：最后一段带 `.` 的路径（像文件）→ 404 信封；否则 Accept 含
  `text/html`（浏览器打开页面）→ 回 index.html，由前端路由接管；否则 → 404
  信封，API 客户端打错路径不会收到一页 HTML。目录不列出。
- 与 API 同域：前端与 API 同源时，cookie 会话（`auth.cookie`）不用 CORS；
  `http.csrf`（默认开）挡住别的网站借浏览器发来的写请求，前端自己的请求同源，
  照常通过（§5.3）。前端在别的域时，在 `http.cors` 里列出它并开
  `allow_credentials`，CSRF 也就把它当作可信来源。

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

- **tag 管客户端，不管代码**：`store:"query"` 只管 URL——`ListFromQuery` /
  `chok.List` 的过滤与排序、`QueryFields()`、OpenAPI 的过滤参数；
  `store:"update"` 只管 Patch；基座的 `id` / `created_at` / `updated_at` 默认
  可查询。没有 tag 的模型在 New 时 panic，并提示怎么写。代码路径——where 选项、
  `store.Where`、聚合与投影、`ListIn`、Upsert 的冲突字段、`where.Order`、
  `store.Set`——按公开名接受模型的每一列：公开名是 JSON 名，没有 JSON 名、
  `json:"-"` 或字段在具名结构体里时用列名；`id` 永远是 RID，数字主键不能按名字
  寻址（用 `store.ID` / `PluckIDs` / `ListByIDs`）。托管列（version、时间戳、
  owner_id、软删两列）可以出现在读条件里，`Set` 与 Patch 仍然不写它们。两个
  字段同名不同列，New panic。URL 里的未知字段是 400，并列出可用字段；代码里的
  未知字段是程序错误，500，日志里有模型名与字段、响应里没有，`errors.Is(err,
  where.ErrUnknownField)` 照样成立。所以把客户端给的字段名交给 `where.*` 之前，
  先用 `QueryFields()` 核对。
- **页大小也只管客户端**：代码里的 `List` 不带分页选项时返回全部行，带
  `where.Page` / `Limit` 就按给的大小取（没给大小用缺省页大小）。
  `db.store.default_page_size` / `max_page_size` 管的是 URL 来的列表：
  `where.FromQuery` 读到的分页（`ListFromQuery`、`chok.List` 都经它）总是一页，
  没给 size 用缺省值，给多了截到上限。`ListWithCursor` 同理。单页仍以 where
  接受的 10,000 行为限，超了报 `where.ErrInvalidParam`，不会悄悄截断。
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
  `ListIn[V]`（每 500 个值一块）、`ListByIDs`（按数字主键的 `ListIn`，结果按主键
  排序）、`GroupBy[K]`；结果类型由 Go 类型决定，入口做静态检查，不读数据库
  catalog。
- **写**：`Create` / `BatchCreate` / `Update` / `Delete`（软删写 token）/
  `Restore` / `Upsert` / `BatchUpsert`（缺省冲突键 rid；带 scope 的 store 拒绝
  Upsert，因为更新半边无法围栏）。`Hooks` 的 BeforeCreate / BeforeUpdate /
  BeforeDelete 在全部校验之后、任何 SQL 之前调用。
- **错误是类型**：`NotFoundError`（404）、`VersionConflictError` 与
  `DuplicateError`（409，`metadata.field` 由约束推出字段名）、
  `EmptyChangesError`（400）、`NoPrincipalError`（401）、`MissingConditionsError`
  （500，程序错误）。都实现 `chok.Coder`，`errors.Is` 对应 `chok.Err*`。

`store/where` 是查询 DSL：字段名一律是模型的公开名，由 store 映射到列，未知
字段按来源分 400 与 500（见上）。`Contains` / `StartsWith` / `EndsWith` 是字面匹配（转义字符 `!`，
因为反斜杠会被 MySQL 吞掉）；`In` 至多 500 个值；`Preload` 对带 Owned 基座的
关联按本 store 的策略用关联表自己的 owner_id 围栏。标识符不加引号，所以模型的
列名要避开 MySQL 保留字（`key`、`order`、`group`……）。

## 7. 电池

电池是值：构造、登记、用。它们只 import 根包（大多还有 `db`，identity 另用
`store`；realtime 与 mail 不碰数据库），彼此不认识。

### 7.1 `identity`

`identity.New(cfg, d, opts...)` 在同一个 `*db.DB` 上注册 users、identities、
audit_logs、auth_login_codes 四张表（owner `identity`），并挂 `{prefix}`（默认
`/auth`）下的路由：register（201，`disable_register` 时不挂）、login（200）、
refresh-token、change-password、me，以及有 `Sender` 时的 forgot / reset
password；`auth.passwordless.enabled` 时的 passwordless/start 与 /verify；
`auth.cookie.enabled` 时的 logout；有 provider 时加 OAuth 的 start / callback /
exchange 与 identities 管理，其中有只推定邮箱的 provider（Facebook）且有
`Sender` 时再加 confirm-email/start 与 confirm-email；审计开着且 `admin_api` 时挂
`GET /audit/logs`。

- **`Authn()`**：Bearer HS256 JWT，**每个请求按主键读一次用户**：存在、
  active、token 的 `pv` 等于当前 `password_version`——禁用、改密、改角色、
  `RevokeTokens` 都立即生效。通过后写入 `chok.Principal{UserID: usr_…, Email,
  Roles, Extra}`，store 的 owner 隔离就读它。
- **会话上限**：`refresh_ttl` 从登录起算，refresh-token 不能无限续。
- **登录限速**：按 email 与客户端 IP 两维、只记失败，429 带 `retry_after`。
- **发信**：identity 不发信，只调应用给的函数（`Sender(fn)`）：
  `fn(ctx, m.To, m.Kind, m)`——模板名就是 `Mail.Kind`（password-reset /
  login-code），数据是 `identity.Mail{Kind, To, Token, Code, Link, ExpiresAt}`
  值；mail 电池的 `SendTemplate` 正是这个形状，装配一行
  `identity.Sender(mailer.SendTemplate)`。发信在后台
  goroutine 里做，响应不等它（是否有这个账户也就从时长上看不出来），失败只记
  Warn；`Close` 在 ctx 内等在途的发送，超时则取消它们。`reset_url` /
  `passwordless.magic_link_url` 配了，`Mail.Link` 就是带 `?token=` 的页面地址。
- **无密码登录**（`auth.passwordless`，需要 `Sender`）：start 一律 202、无体，
  按邮箱 upsert 一行 `auth_login_codes`（码与 magic-link token 都只存
  HMAC-SHA256(signing_key, ·) 的 hex），码未用时在 `resend_after` 内再 start 不换码、
  不发信（码用掉即删行，之后的 start 照常发新码）；按客户端 IP 计 start 次数，与登录
  限速同一张表（另起一个维度名）、同一清扫。
  verify 收 `{email, code}` 或 `{token}`：码错 401 InvalidCode 带
  `attempts_left`，过期、用过、试满 `max_attempts`、没 start 过一律 401
  CodeExpired；比较前先用条件 UPDATE 认领一次尝试、成功时条件 DELETE 消耗该行，
  并发的猜测拿不到多于 `max_attempts` 次比较，并发的成功只有一个。未注册的邮箱
  在 `auto_register`（nil 即 true）下建号：email 已验证、无密码、无名字。过期行由
  identity 自己的每小时清扫删除。`passwordless.only`（须同时 `enabled`）给「邮箱
  就是账号」的应用关掉密码面：register、login、forgot / reset password、
  change-password 都不挂，refresh-token、me、logout、OAuth 照常，`Users()` 的
  Create / ResetPassword 仍可用。
- **cookie 模式**（`auth.cookie`）：签发 token 的五处（register、login、
  refresh-token、passwordless verify、OAuth exchange）同时 Set-Cookie（HttpOnly、
  Secure 缺省开、SameSite 缺省 lax、Max-Age = token_ttl）；`only` 时响应体只剩
  `{expires_at}`，OpenAPI 的响应 schema 随之改变。类型化 handler 写不了响应头，
  所以这几条路由带一个路由级中间件，把 ResponseWriter 放进请求 ctx。`Authn` 在
  没有 Authorization 头时读这个 cookie（有头以头为准）；logout 清 cookie、不吊销
  token（token 无状态，要立即失效用 `Users().RevokeTokens`）。浏览器会给每个请求
  自动带上 cookie，所以 cookie 会话靠 `http.csrf`（默认开着，关掉会告警，§5.3）
  并让前端与 API 同源（或在 `http.cors` 里列出并开 `allow_credentials`）。
  cookie 模式下 `Authn` 向 OpenAPI 报告 bearer 与 `cookieAuth`（apiKey in
  cookie）两种（§5.4）。
- **OAuth**：Google / Apple 走 OIDC（自实现校验）、GitHub、Facebook；
  `Provider` 是封闭接口，四个子包各只导出 `New(identity.ProviderConfig)`。
  配置了却没在代码里链接的 provider 让 `New` 失败。进行中的 OAuth 状态只在进程
  内：多副本须会话粘滞。Facebook 返回邮箱却不说是否已确认（Graph API 的参考只
  说「有效」），它的邮箱只算推定已验证：能新建账户，但不按 `link_by_email` 并入
  已有账户，也不能新建 `auth.admins` 列出的邮箱的账户。它建的账户记下邮箱是推定
  的（`users.email_assumed`，`/me` 与用户列表里的 `email_assumed`）：平时与已验证
  无异，但 `auth.admins` 不授予（邮箱列进名单之前就建好的账户也一样）；之后第一次
  从账户外证明这个邮箱——reset-password、passwordless 的码、开了 `link_by_email`
  时用明说已验证的 provider 登录而并入——就接管这个账户：同一事务里解绑此前的所有
  绑定、清掉密码、吊销 token，用 Facebook 注册的人（不一定是邮箱的主人）被踢出去。
  用户在自己的会话里确认邮箱则一切保留，见下面「在会话里确认邮箱」。
- **审计**：`Audit().Log`（异步入队，不进调用方事务）/ `LogSync`（同步，随 ctx
  事务）；trace id 由 telemetry 经内部钩子补上。
- **角色只有一套**：全局角色只存在 identity——`users.roles` → `Principal.Roles`，
  用 `Users().SetRoles` 管（会吊销该用户的 token，下个请求就是新角色）。内置的
  `admin` 在 rbac 里无所不能，在 identity 里可读审计；store 的
  `db.store.admin_roles` 默认仍为空，要让管理员越过 owner 隔离须显式写
  `admin_roles: [admin]`。
- **首个管理员**：`auth.admins` 是邮箱列表（规范化后比较）。列表里**且已证明**
  （已验证，且不只是 Facebook 推定）的用户，在签发 token（register、login、
  refresh-token、passwordless verify、OAuth exchange）或 `Authn` 读到他时，
  角色里还没有 `admin` 就加上并落库：按行的 version 乐观锁，并发请求只写一次；
  这次写与审计记录 `user.role.grant`（actor 是 system，`LogSync`）在同一事务里
  提交，记录写不进去就不授予。不吊销已有 token；写失败只记 Warn、请求照常，
  下个请求再试。只增不删：从列表里删掉邮箱不撤销角色（要撤用 `SetRoles`）。
  未证明的邮箱不授予（未验证的，或 Facebook 推定的）——开放注册时，抢注管理员
  邮箱的人拿不到权限。reset-password 成功即证明邮箱可达，置 `email_verified`，
  所以用密码登录的应用也能走 `auth.admins`；运维调用的 `Users().ResetPassword`
  不置。
- **绑定要先验证邮箱**：注册邮箱的人不一定是邮箱的主人。未验证邮箱的账户不能
  绑定新的登录方式（`POST {prefix}/identities/link` 与回调都答 403
  `EMAIL_NOT_VERIFIED`）；邮箱第一次被证明时（reset-password、passwordless
  的码、`Users().MarkEmailVerified`），同一事务里解绑此前的所有绑定——v0.2 不会
  产生这种绑定，v2 的数据里可能有。否则抢注者绑定的登录方式，会在主人接管账户
  之后留下来。Facebook 建的账户（邮箱推定）可以绑定，因为从外面第一次证明邮箱时
  这些绑定一并解除。
- **第一次用码证明邮箱**：passwordless 的码第一次证明一个已有账户的邮箱时（未验证
  的，或 Facebook 推定的），清掉此前设的密码（`has_password` 置 false，hash 换成
  随机串）并吊销 token（bump `password_version`），抢注者设的密码与拿到的 token
  一起失效；之后要密码就走 reset-password。reset-password 本来就换密码、吊销
  token。`link_by_email` 并入 Facebook 建的账户时同样接管。接管只切断抢注者的
  访问，它在账户里留下的东西（资料、地址等）不会清掉。
  `Users().MarkEmailVerified` 是应用自己的验证（多半是注册后马上点的链接），密码
  与 token 都不动：`auth.admins` 里的邮箱应由主人自己注册，或者关掉开放注册。它不
  改 Facebook 推定的邮箱：应用分不清是谁打开了邮件，接管会把账户的持有者锁在外面，
  保留又会把邮箱交给持有者。
- **在会话里确认邮箱**：有 `Sender`、且有只推定邮箱的 provider（Facebook）时，
  `Authn` 之后挂 `POST {prefix}/confirm-email/start`（给本人的邮箱发码，202；
  `resend_after` 内不重发；发码与输错按用户计入登录限速）与
  `POST {prefix}/confirm-email` `{code}`（204）。码在登录的会话里输入，证明会话的
  持有者收得到这个邮箱，所以邮箱算已证明（`email_assumed` 清掉、`email_verified`
  置上），密码、绑定、token 都不动——与从外面证明时的接管相反。前端在需要时（比如
  要拿 `admin`，或注册后提示一次）按 `/me` 的 `email_assumed` 请用户确认。码借用
  `auth_login_codes` 表和 `auth.passwordless` 的 code_ttl、code_length、
  max_attempts、resend_after，但那一行按用户 id 存，无密码登录碰不到；MAC 同时
  覆盖发往的邮箱。码错或过期答 400（`InvalidCode` 带 `attempts_left` /
  `CodeExpired`），不用 401：会话本身没问题，前端见 401 会当成登录失效。发信的
  `Mail.Kind` 是 `confirm-email`（模板名同），不带 Token 与 Link。在会话里绑定一个
  明说已验证、邮箱与账户相同的 provider（比如 Google）也算确认。
- **用户管理**：`Users()` 是运维面（Create、Get、`GetByEmail`、SetRoles、
  SetActive、ResetPassword、MarkEmailVerified、RevokeTokens），并实现
  `chok.Lister[User]`：全表、没有 owner 围栏，可按 email、name、email_verified、
  email_assumed、active、created_at 过滤，每页默认 20、最多 100。管理接口因此是
  一行 `chok.List(admin, "/users", auth.Users())`。列表自己把关，挂在哪个组都
  一样：只回应持有 `admin` 角色、或装了 `identity/rbac` 时被授予 `user/read`
  的 principal，其余 principal 403、没有 principal 401，挂错了组（比如只有 Authn
  的组）也列不出别人。程序里直接调用 `ListFromQuery`，要先用
  `chok.WithPrincipal` 把 principal 放进 ctx。响应体是 `User` 的 JSON，不含
  roles 与 active，要显示它们的管理页自己写 handler。
- **`identity/rbac`**：casbin 的 (sub, dom, obj, act) 模型管权限。判定读
  `chok.Principal` 的 UserID 与 Roles（不必是 identity 签发的 principal）：
  `Require(obj, act)` 放行，当且仅当 Roles 含 `admin`，或 casbin 允许这个用户
  （`GrantUser`），或允许 Roles 里任一角色（`GrantRole`）；无 principal 401、
  拒绝 403。`RequireInDomain` 同理：`admin` 跨所有域，后两条在该域判定，域内的
  角色由 `AddUserToRoleInDomain` 管——域不能是 "" 或 `*`（全局角色用
  `Users().SetRoles`）；v2 在 casbin_rule 里留下的全局角色绑定（域 `*`）不算数，
  升级时要搬进 `users.roles`（迁移文档 §5）。`HasPermission(ctx, userID, …)`
  按 userID 读 identity 的用户后同规则判定，用户不存在或已停用（`SetActive`）
  → false。`New` /
  `MustNew`、策略服务、Redis watcher 同步多副本。不 import rbac 的应用不链接
  casbin，也不建 casbin_rule 表。

### 7.2 `jobs`

一个 `jobs.New(cfg, d, opts...)` 管两类工作：

- `jobs.Periodic(name, spec, fn, overlap)`：robfig 语法的 cron（5 字段、可选
  秒、`@hourly`、`@every 10m`、逐条 `CRON_TZ=`），解析器移植进
  `jobs/internal/cron`（来源与许可见仓库根 `NOTICE`）。`OverlapSkip` 丢弃运行
  中到来的触发，`OverlapQueue` 至多排一次，`OverlapAllow` 并发运行。
- `jobs.Worker[T](topic, fn)` + `j.Enqueue(txCtx, topic, payload)`：事务性
  outbox。Enqueue 必须在同一个 `*db.DB` 的 `RunInTx` 里（否则
  `ErrOutsideTx`），消息与业务写入同生共死；worker 把 payload 按 `T` 用
  `encoding/json/v2` 解码（要原始 JSON 用 `Worker[jsontext.Value]`），以
  `(created_at, id)` 顺序**至少一次**投递，每个 topic 一个水位，失败只阻塞自己的
  topic。handler 必须幂等（按 `Message.ID` 去重）。保证的前提：enqueue 事务在
  `settle_window` 内提交，各进程时钟同步。
- 失败与死信：handler 返回错误或 panic，这条消息的失败次数 +1 并写在消息行上
  （重启不清零），`retry_backoff` 后重试、每次翻倍、不超过 `max_backoff`，等待期间
  不再每轮重试；第 `max_attempts` 次（默认 10）仍失败就**停放**进死信表
  `chok_jobs_dead`，算作已投递，topic 继续往下走；payload 解码失败直接停放。
  `j.Dead(ctx, topic, limit)` 列出、`j.Requeue(ctx, ids...)` 重新入队成新消息、
  `j.Discard(ctx, ids...)` 删除，只用数据库，`enabled: false` 的进程也能调。
  优雅停止时，各 worker 已投递、水位还没越过的消息写进 `chok_jobs_delivered`，
  重启后的第一轮读回、不再投递；崩溃没有这一步，仍可能重投。retention 清理只删
  消息表，不碰死信；配置文件不写 `retention` 时默认 168h，代码里构造的 Config
  不写就是 0（永久）。

部署假设：所有 worker 与周期任务跑在**一个** `jobs.enabled: true` 的进程里，
其余进程 `enabled: false` 只 Enqueue。`Stats()` 给出每个作业的运行统计；
`Health` 恒 nil——毒消息不该把实例拉出轮转，失败看统计与日志：worker 的统计另有
`parked`（本进程停放的条数）、`head_attempts` 与 `next_retry`（topic 卡在一条失败
消息上时，它的失败次数与下次重试时间）。那几轮只等下次重试的扫描算作成功，但不清
`last_error`，所以卡住期间它一直是最近一次失败的错误。

`jobs.Observe(fn)` 是统计的流式版本：每次运行（周期任务、worker 的一轮 sweep、
retention 清理）开始、结束、失败，以及因运行中而被跳过的触发，都同步交给 fn 一个
`jobs.Event{Kind, Job, Type, StartedAt, Duration, Error, Next, Messages}`——
`Kind` 是 run.started / run.finished / run.failed / run.skipped，worker 停放
一条消息时还有 message.parked（`Error` 是它最后的错误），`Type` 是
periodic / worker / cleanup。fn 在运行的 goroutine 里调用，要快；它的 panic 被
recover 并记 Warn，不影响运行与统计。典型用法是交给 realtime 做实时的任务面板。

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

### 7.5 `realtime`

`realtime.New(cfg, opts...)` 一个 Hub：主题的订阅表、经 WebSocket 或 SSE 订阅
它们的连接、打开连接用的 ticket，以及有 Redis 时通往其他副本的 backplane。
`app.Use(hub)` 挂 `{path}`（默认 `/realtime`）下的三个端点：`POST /ticket`、
`GET /ws`、`GET /sse`，都在 OpenAPI 里。

- **ticket**：浏览器的 WebSocket 与 `EventSource` 带不了 `Authorization` 头，长期
  token 放进 URL 又会进代理日志，所以页面先拿 token 换一张一次性 ticket（32 字节
  随机数，`ticket_ttl` 内兑换一次），ws / sse 端点兑换它并把连接绑定到签发时的
  principal。`realtime.Authn(auth.Authn())` 守 ticket 端点；不给则端点公开、ticket
  匿名（游客页面）。`EventSource` 的自动重连带着用过的 ticket，必然 401：页面在
  onerror 里换新 ticket 重开，并用 `?last_event_id=` 续传。
- **主题与授权**：主题是 1–128 个 `A-Z a-z 0-9 _ . : -`。`realtime.Authorize(fn)`
  逐个订阅判断，ctx 带 ticket 的 principal；返回 `*chok.Error` 或 `chok.Coder`
  时其 reason / message 进 refused 事件，否则 Forbidden 并记 Warn。没有 Authorize 时
  只有 `user:<自己的 UserID>` 能订；`user:` 前缀保留，Authorize 对它不被调用，
  `hub.Send(ctx, userID, …)` 发到它。被拒的订阅是一条 refused 事件，连接不断（不叫
  error：EventSource 自己的连接错误用这个名字，页面惯用 onerror 重连）；SSE 的主题全部
  被拒时回 403。refused、reset 与 error 三个类型 `Publish` 一律拒绝（backplane 收到的
  也丢弃）：error 若能发布，页面的 onerror 会把它当成断线。
- **投递**：`Publish(ctx, topic, typ, data)` 在锁内给事件编号
  `<origin>-<seq>`（origin 是 Hub 在 New 时抽的随机标识）、写进续传缓冲、放进每个
  订阅者的发送队列，从不等慢连接：队列里已有 `send_buffer` 条实时事件的连接被当作
  慢消费者断开（WS 关闭码 4408），计入 `Stats.Dropped`；订阅时补发的事件与 hub 自己
  的 refused / reset 不计入，但整条队列以 `send_buffer + replay + 2 × max_subscriptions`
  封顶，只发帧不读的客户端也撑不大它。WS 的每次写最多等两个 ping 间隔；连接结束后
  5 秒内关掉（卡在写上的连接发不出关闭帧，就直接切断）。data 用 chok 处理请求体的
  同一套 json/v2 选项编码，Duration 是字符串。
- **续传**：最近 `replay` 条事件（所有主题共用一条序列）留在内存。WS 的
  `{"subscribe":[…],"after":"<id>"}`、SSE 的 `Last-Event-ID` 头或
  `?last_event_id=` 续上该事件之后、所订主题的事件；该事件已不在缓冲里时每个主题
  一条 reset，客户端从 API 重新拉全量。缓冲是进程内的：新起的副本是空的。
- **两种传输**：WS 每帧一条 Event JSON；客户端帧只有 subscribe / unsubscribe，
  其它帧、坏主题或超出 `max_subscriptions` → 关闭 1008；每 `ping_interval` 一次
  协议 ping，两个间隔内没有 pong 即断开；Origin 不在 `allowed_origins`（或同 Host）
  时升级前 403。SSE 先写 `retry: 3000`，每个事件 `id` / `event` / `data`（整条
  Event），每 `ping_interval` 一行注释；服务器的写截止时间被清除、改为每次写各自
  的截止时间（两个 ping 间隔），所以 `http.write_timeout` 不再切断流，
  `http.request_timeout` 则如常结束它。WS 升级后不受 HTTP 超时约束。两种连接都在
  结束时记一条访问日志（101 / 200，时长即连接时长）。
- **停机**：`http.Server.Shutdown` 会等所有在途请求，而一条 SSE 流永远不会自己
  结束，所以 Hub 在第一个流请求到来时经 `http.ServerContextKey` 找到服务器、用
  `RegisterOnShutdown` 登记一个钩子，Shutdown 一开始就结束该服务器上的流；WS 是
  hijack 出去的连接，Shutdown 不管，由随后的 `Close` 发 1001。`Close` 等所有连接
  结束，受 `app.shutdown_timeout` 约束。
- **多副本**：`realtime.Redis(client)`（`cache.Redis()` 可直接传）让 ticket 存进
  Redis（`{channel}:ticket:<ticket>`，SET NX 加 TTL 签发、GETDEL 兑换，跨副本一次
  性），每个 Publish 本地投递后再 `PUBLISH {channel}`；其他副本投递给自己的订阅者
  并写入续传缓冲，各自忽略回声。`Start` 维持订阅、断线自动重连并记 Warn；`Health`
  在订阅未建立或已断开时报错，于是 App 的启动相要等订阅建立。

### 7.6 `mail`

`mail.New(cfg, opts...)` 一个 Mailer：组信、渲染模板、交给传输。没有池、没有
队列、没有生命周期——`Health` 恒 nil、没有 Start / Close，`app.Use(m)` 可有可无
（只为在 `/healthz` 里露个面）。只用标准库，不加依赖。

- **组信**：RFC 5322。From / Reply-To 来自配置（`Headers["Reply-To"]` 可逐封
  覆盖），To / Cc，Subject（换行折成空格，非 ASCII 用 `mime.QEncoding`），Date
  （UTC），Message-ID（`<随机 hex@From 的域>`），MIME-Version；Bcc 只在信封上。
  正文 quoted-printable，附件 base64；text + html → `multipart/alternative`，有
  inline 图片 → 外包 `multipart/related`（Content-ID 即 `<Name>`），有附件 → 再包
  `multipart/mixed`；附件的 Content-Type 缺省按扩展名猜。头字段在 76 列处折行，
  `Headers` 不能写 From / To / Cc / Bcc / Subject / Date / Message-ID /
  MIME-Version / Content-*，值不能跨行。地址不合法、模板不存在、Template 与正文
  同时给，都是发出前的普通错误（不是 Coder，handler 自己包装）。
- **模板**：`Templates(fsys)` 在 New 时解析 fs 里任意深度的 `.html` / `.txt`，模板
  名即文件名去扩展名；一个模板可以只有 `{name}.html`（html/template，发
  text/html）、只有 `{name}.txt`（text/template，发 text/plain），或两者都有（发
  multipart/alternative）；`_*.html` / `_*.txt` 是同类模板共用的局部，只有局部、
  没有正文的名字不是模板；`{{define "subject"}}` 是主题（.txt 的优先，html 的
  反转义），`Message.Subject` 非空时覆盖。`SendTemplate(ctx, to, template, data)`
  是单收件人模板信的简写。解析失败、同名文件、只有局部、给两次 Templates 都让
  New 失败，坏模板不会等到第一封信才暴露。
- **传输**：`smtp` 每次 Send 拨号 → EHLO → STARTTLS（中继不提供就报错，不降级）或
  隐式 TLS → AUTH PLAIN（有 username 时）→ MAIL → 每个收件人（含 Bcc）一个 RCPT →
  DATA → QUIT，整次受 `mail.timeout` 约束（连接的截止时间 + ctx 取消即断）；
  `tls: none` 只许回环地址。`dir` 把每封信写成 `{unix 纳秒}-{随机}.eml`（先写临时
  文件再改名，0600）并 Info 记路径；`log` 只记日志（Info：收件人、主题、字节数；
  Debug：正文）。`Sender(fn)` 整个替换传输（HTTP API、测试捕获），收到渲染后的
  Message 与组好的全文，此时 Transport 与它的字段一律不查。
- **可靠投递不在本包**：Send 是一次尝试；要重试就在业务事务里 `jobs.Enqueue`、由
  该 topic 的 Worker 调 Send（docs/mail.md 有配方）。给 identity 用时直接传
  `identity.Sender(mailer.SendTemplate)`，不用适配器。

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
  （db、store、choktest、identity、jobs、telemetry、cmd/chok、examples/chatter）
  **全量**重跑，
  不用 `-run` 挑测试；缺 DSN 时 lane 直接失败。CI 用 `postgres:16` 与
  `mysql:8.4` 服务，两条 lane 都阻断。
- **验收即文档**：`examples/blog` 的验收测试走一遍 README 的路径；
  `examples/chatter` 的验收测试在真实服务器上走它的 README 路径（三方言）：经
  mail 的 dir 传输取验证码与链接登录、cookie 会话、HTTP、WebSocket 与 SSE；`make smoke`
  起真实二进制、探 `/healthz` 与 `/metrics`、SIGINT 后 3 秒内退出；`chok init`
  的自检测试在三方言上生成、构建、启动脚手架；`internal/tools/doccheck` 编译
  quickstart、两份 README、`docs/realtime.md` 与 `docs/mail.md` 里的每个程序；
  `docs/config.md` 与 `docs/api.md` 由测试守着不腐烂。
- 时序逻辑优先 `testing/synctest`；测试从不占固定端口；每个 bug 修复与回归测试
  同一个提交。

## 9. 命令行

`cmd/chok`（`go install github.com/zynthara/chok/cmd/chok@<version>`，或在
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
  版本号时，`mark-applied --owner` 指明哪一个。
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
| 公开包（provider 不计） | 12 | 既非 internal 也非 main 的包；名单外的包直接失败 |
| 顶层导出 | 总 250；chok 64、db 30、store 30、store/where 28、identity 30、identity/rbac 8、jobs 17、telemetry 10、cache 10、choktest 10、realtime 16、mail 12、provider 各 3 | `go doc -short` 的行数（构造函数缩在类型下、分组的 var / const 一行），方法不计；逐行见 [api.md](api.md) |
| 选项构造函数 | 50（不含 store/where 的谓词） | 结果类型是 `Option` / `RouteOption` / `LoadOption` 的函数 |
| 直接依赖 | 30 | `go list -m` 里非 indirect 的模块 |
| 配置叶键 | 180 | `chok.Base` 与八个 Config 的叶子；逐键见 [config.md](config.md) |
| examples/blog | 应用代码 120 行、chok 包 5 个 | 非测试 .go 的非空非注释行；不重复的 chok import |
| examples/chatter | 应用代码 400 行、chok 包 8 个 | 同上口径（不同的 import 路径如实计数，store/where 单算），由它自己的 `TestChatter_StaysSmall` 守着（契约 §10），不在 budget.txt 里 |

质量门同样阻断：`make verify` = tidy-check、gofmt -s、vet、golangci-lint、
`go fix -diff` 为空、SQLite lane、Postgres lane、MySQL lane、budget、smoke，
本地与 CI 完全相同。

## 11. 版本与发布

- v0.1.x 封版于 `v0.1.4`；v2 封版于分支 `v2`（模块路径带 `/v2`，`v2.0.0-beta.N`
  永久可装，只收安全修复）；这一版在 `main` 上开发，模块路径
  `github.com/zynthara/chok`，不带主版本后缀。
- 不带后缀的路径只能用 v0、v1 的版本号，而这个路径已有 v0.1.0 到 v0.1.4，所以
  这一版从 `v0.2.0` 开始，不带 `-alpha`：预发布版在 `@latest` 里会输给 v0.1.4。
  v0 期间不兼容的改动升中间那位（v0.3.0…），CHANGELOG 写明怎么改。
- 手动裁切：`CHANGELOG.md` 条目 → `make verify` 全绿 → 打带注释的 tag → push
  tag，goreleaser workflow 构建 `chok` 命令的各平台产物并发布 GitHub Release
  （`chok version` 显示 tag）。tag 一经推送不可重打（module proxy 永久缓存）。
- `hack/apidiff.sh` 以上一个 release 为基线报告不兼容的 API 变更；v0 期间它在
  CI 里只报告不阻断，v1.0.0 之前改为阻断。
