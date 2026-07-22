# chok v1 → v2 迁移指南

> 适用：从 `github.com/zynthara/chok`（≤ v0.1.4）迁到
> `github.com/zynthara/chok/v2`。v1 永久可安装、只收安全修复。
> 本文按主题分组列出**全部**破坏性变更（39 条），每条给出 v1 写法
> 与 v2 写法；条目编号（#n）与内部变更台账一致，便于对账。
>
> 迁移的总路线：先把 import 换成 `/v2`，删掉巨型 Config 结构体与
> `WithConfig/WithSetup/Register` 调用，改成 `chok.Use(模块…) +
> chok.Routes(回调)`（或让 `chok sync` 从 chok.yaml 生成装配），再
> 按下文逐条核对你用到的面。`examples/blog` 就是迁移后的样板。

---

## 一、装配与生命周期

### #29 `component/` 与 `parts/` 整体删除

Registry、`parts.NewXxxComponent`、builders、Resolver 全部消失。
装配面 = `chok.Use(<模块>.Module())`。

```go
// v1
chok.New("app",
    chok.WithConfig(&cfg),                    // 反射扫描 Config 字段
    chok.WithSetup(func(ctx, a) error {       // 手动补注册
        a.Register(parts.NewTracingComponent(...))
        return nil
    }),
)

// v2
chok.New("app",
    chok.Use(log.Module(), web.Module(), db.Module(), tracing.Module()),
    // 或：chokModules() —— chok sync 从 chok.yaml 生成
)
```

- `WithConfig` / `WithSetup` 删除；显式配置文件路径用
  `chok.WithConfigFile(path)`。
- `Register` / `Replace` 收敛为 `Use`（同键重复 ⇒ 启动失败）与
  `chok.Override(c)`（替换不存在的键同样报错）。
- `JWTComponent` 删除（`auth/jwt` 保持纯库，多实例自行持有多个
  Manager）；`PoolComponent` 降为 `scheduler/pool` 工具库。

### `On(event, hook)` 删除

启动前 veto → 注册自定义 Component，`Init` 返回错误即中止启动；
启动后动作 → 订阅事件总线。

```go
// v1
a.On(chok.EventAfterStart, func(ctx, a) error { warmup(); return nil })

// v2（veto 型）
type warmupComp struct{ ... }
func (c *warmupComp) Init(ctx context.Context, k kernel.Kernel) error {
    return warmup() // 失败 = 启动失败
}
// v2（启动后动作型）
event.Subscribe(bus, func(ctx context.Context, ev kernel.AppStarted) { ... })
```

### `AddCleanup` / `WithCleanup` 删除

「全部组件停止后的最后收尾」= `main()` 里 `defer`（`Run` 返回时
控制面已完全停止，时机等价 v1 post-stop LIFO）；组件资源释放归
组件自己的 `Close`。根 logger 的 close-last 由 App 所有权保证，
无需用户操心。

### #8 `server/` 包删除

`NewHTTPServer` / `RegisterHealthz` / `RegisterPprof` 无对应物：
healthz 归 health 模块；pprof 迁移模式 = Routes 回调里挂
`net/http/pprof`（两行）。

### #30 account 面

- 服务类型 `account.Module` 更名 **`account.Service`**；
  `account.Module()` 现在是装配构造器。
- `New` 首参改 `*db.DB`；`Setup` / `OptionsFromConfig` /
  `RegisterConfiguredProviders` / `RouteGroup` / `AuthChain`（gin
  形态）删除。
- provider 全局工厂注册表 + blank-import 策展包删除：

```go
// v1：import 副作用注册
import _ "github.com/zynthara/chok/account/providers"

// v2：显式装配（chok sync 依据 yaml providers.*.enabled 自动生成本行）
account.Module(account.WithProviders(google.Provider(), apple.Provider()))
```

  yaml `providers.<name>.enabled` 仍是运行期开关；**enabled 而未
  装配 ⇒ 启动失败**（v1 的 Options.Enabled 双记账删除，顺带修复了
  yaml 驱动 provider 无法启用的 v1 缺陷）。
- 路由守卫：`a.AuthMiddleware()` → `account.Authn(k)`（AuthChain
  语义：Authn + ActiveCheck）。
- `SessionCarrier.Issue/Read` 改 stdlib 签名（`w, r` 形参）。

### #10 parts 观测组件

`parts.NewHTTPComponent / NewSwaggerComponent / NewTracingComponent`
→ `web.Module() / swagger.Module() / tracing.Module()`。

---

## 二、配置

### #23 `database` 段更名 `db` 并扩容

```yaml
# v1
database: { driver: sqlite, sqlite: { path: app.db } }

# v2
db:
  driver: sqlite          # sqlite | mysql | postgres（postgres 为 v2 新增，pgx）
  migrate: auto           # auto | versioned | off（新增，默认 auto）
  sqlite: { path: app.db }
  # 命名实例：db.instances.<name>（v2 新增）
```

mysql 增 `tls` / `ca_cert`；postgres 支持 `dsn`（与离散字段互斥）
与 `ssl_mode` / `ca_cert`；密码与 dsn 挂 `sensitive`，日志脱敏。

### #12 `http` 段新增字段

`shutdown_timeout`（默认 10s，Shutdown 预算超时 force-Close）与
`h2c`（默认 false，v2 新能力）。其余字段语义与 v1 一致；
`drain_delay` 继承关系保留（显式 `WithDrainDelay` 优先）。

### #13 / #31 `enabled` 缺省翻转（装配即意图）

| 段 | v1 缺省 | v2 缺省 |
|---|---|---|
| `swagger.enabled` | false | **true** |
| `account.enabled` | false | **true** |
| `authz.enabled` | false | **true** |
| `audit.enabled` | false | false（合规组件保持显式 opt-in） |

不想暴露的模块：写 `enabled: false`，或干脆不装配（不写该段 +
重跑 `chok sync`，链接期即裁剪）。

### #14 `tracing` 成为一等 yaml 段

v1 须手动 Register 组件；v2 写段即可：`enabled`（默认 false）/
`service_name` / `exporter`（stdout|otlp）/ `otlp_endpoint`。

### #33 `redis` / `cache` / `scheduler` 段

- `redis`：v1 字段 1:1 + 新增 `username` / `tls` / `ca_cert`；
  `redis.TLSConfigFor` 导出。
- `cache`：合并 v1 `cache.memory` / `cache.file` 两段为
  `memory` + `redis`（显式层开关——**redis 层 enabled 而 redis
  模块缺席 ⇒ 启动失败**，替代 v1「RedisComponent 在场即自动挂」）
  + `breaker`（默认关）。badger 文件层删除（`cache.file` 段、
  `CacheFileOptions` 无对应物）。
- `scheduler`：新段，`stop_budget`（v1 构造参数进配置，默认 15s）。

### 根 `config` 包删除（M5）

`config.SlogOptions` / `LogFileOptions` 的能力并入 `log` 包
（`log.Options` / `log.FileOptions`，yaml `log` 段形态不变）；
logger 构造统一为 `log.New(log.Options)`（`NewSlog` /
`NewDefaultSlog` 删除）。其余 `config.*Options` 在 v2 归各模块
（`web.Options` / `db.Options` / `account.Options` / ...）。

---

## 三、HTTP 与路由

### #3 路由注册语法换 ServeMux pattern

```go
// v1（gin）
rg.GET("/posts/:rid", h)
rg.GET("/files/*path", h)

// v2
api.Handle(http.MethodGet, "/posts/{rid}", h)
api.Handle(http.MethodGet, "/files/{path...}", h)
// 精确根匹配用 {$}；非法/冲突 pattern 在 mount 期 panic
```

路径参数读取：`req` 结构体 `uri:"rid"` tag 不变（绑定层适配）。

### #1 trailing-slash / fixed-path 不再纠错【声明变更】

- 精确 pattern（`/exact`）：`/exact/` 直接 404（v1 会 301 纠错）。
- 子树 pattern（`/tree/`）：`/tree` 307 → `/tree/`（方向与 v1
  相反、状态码不同）。
- 路径清理（`//`、`..`）：307 到规范路径（v1 为 301）。

### #2 指标/日志/span 的路由标签 `:rid` → `{rid}`【声明变更】

`http_requests_total{path="/users/{rid}"}`、access log `path` 字段、
span 名全部换风格——**dashboards / 告警 / 采样规则需同步**。
未匹配请求标签 `"unmatched"` 保留不变。

### #4 tracing span 命名时机【声明变更】

span 以 `METHOD` 开名、handler 返回后改名 `METHOD pattern`（v1
请求开始即含路由名）。基于 span 名的头部采样改用属性规则。

### #5 保留项（防误判）

404/405 的 apierr envelope、未匹配请求走完整中间件栈、
`X-Request-ID` 生成/清洗、504 envelope、CORS 语义、ClientIP
fail-closed 均与 v1 一致（矩阵测试钉住）。

### #6 handler 层签名

- `HandleRequest / HandleAction / HandleList` 返回 `http.Handler`
  （原 `gin.HandlerFunc`）。
- `WriteResponse` 改 `(w, r, code, data, err)`；新增 `WriteError`。
- `HandlerMeta` 结构体更名 `handler.Meta`，经构造产物的 `Meta()`
  暴露；`IndexRoutes / LookupRoute / LookupMeta` 删除。

### #7 middleware 全部改 `func(http.Handler) http.Handler`

- `Recovery(logger)` 收 fallback logger。
- `AttachAuthz` 改写请求 context（`WithAuthorizer /
  AuthorizerFrom` 替代导出常量 `ContextKeyAuthz`）。
- `RequireAuthzInDomain` 的 domain 经 `r.PathValue` 读取。
- 新增 `middleware.ClientIP`（自 v1 RequestID 内联行为拆出）。

### #9 swagger 面

`swagger.Setup / Generate / Populate / Post / Get / Put / Patch /
Action / List` 删除。替代 = `swagger.Module()` 自动从路由表生成 +
`handler.WithSummary / WithTags / WithPublic` 注解；纯函数入口
`swagger.BuildSpec`。

### #15 web disabled + Mounter ⇒ 启动失败【行为收紧】

v1 是 warn + 继续（路由静默不可达）；v2 kernel 对「有 Mounter /
Routes 而无 RouterProvider」fail-fast。kill-switch 场景须同时
禁用/移除依赖 HTTP 的模块。

### #16 绑定层细节

form/query 只绑**带 tag 字段**（v1 对无 tag 字段按字段名绑）；
不支持 map 目标与 `time_format` tag（time 固定 RFC3339）；标量
多值取首值（同 v1）；`binding` 校验 tag 全兼容。

### #17 panic 请求无 access log 行

与 v1 行为一致（仅记录以免误判回归）；Recovery 的 panic 日志以
根 logger 记录，envelope 与日志的 request_id 关联保留。

---

## 四、数据层

### #18 `store.New` 首参 `*gorm.DB` → `*db.DB`

```go
// v1
gdb := a.DB().(*gorm.DB)
posts := store.New[Post](gdb, a.Logger())

// v2
posts := store.New[Post](db.From(k), log.From(k),
    store.WithQueryFields(...), store.WithUpdateFields(...))
```

句柄来源：`db.From(k)`（缺席即 panic——装配错误 fail-fast）/
`chok.Get[*db.Component]` / 库级 `db.Open(db.Options)`。

### #19 事务面收敛：ctx 传播是唯一模型

包级 `db.Transaction` 与 `Store.WithTx` 删除。

```go
// v1
db.Transaction(gdb, func(tx *gorm.DB) error {
    return userStore.WithTx(tx).Create(ctx, &u)
})

// v2
h.RunInTx(ctx, func(txCtx context.Context) error {
    return userStore.Create(txCtx, &u) // 带 txCtx 自动加入事务
})
```

跨 store 原子写 = 同一 txCtx；嵌套 RunInTx 复用最外层。

### #20 / #39 raw gorm 出口收敛为两扇 Unsafe 门

- `Store.DB()` / `Store.ScopedDB(ctx)` 合并为
  `Store.Unsafe(ctx) (*gorm.DB, error)`——事务感知、**scope 已
  应用**、scope 失败 fail-closed。无 scope 的 `DB()` 形态消失。
- `db.DBFromContext` 删除（M5）：事务内取 raw gorm 用
  `h.Unsafe(txCtx)`（tx-aware，返回 ctx 事务）；只需判断「是否在
  事务内」用新增的 `db.InTx(ctx) bool`。

```go
// v1 / v2 早期
txDB := db.DBFromContext(txCtx)

// v2（M5 起）
txDB := h.Unsafe(txCtx)
```

### #21 after-hooks 全套删除 → `store.WithBus`

`WithAfterCreate/Update/Delete`、`WithAsyncAfter*`、`Submitter`
删除。替代 = opt-in `store.WithBus(bus)` 发布类型化
`store.EntityChanged[T]`：**发布锚定事务提交**（Tx 内暂存、
Commit 按序 flush、Rollback 整体丢弃——杜绝幻影事件）；
Update/Delete 保留 RowsAffected>0 门控。**sync → async 是有意的
语义变更**；需要写路径内同步逻辑用 before-hooks（保留不变）或
`db.AfterCommit`。

### #22 库级构造收敛

`db.NewMySQL / NewSQLite`（config 参数形）与 `db.Close(gdb)`
删除：v2 构造 = `db.Open(db.Options)` 或 `db.Module()`；关闭 =
`h.Close()`。

### #24 / #27 版本化迁移 + 框架表 ownership

`migrate: versioned` 下应用表 schema 出自嵌入的
`migrations/NNNN_name.sql`（forward-only、`schema_migrations`
账本、跨进程迁移锁）；内建组件通过 `Descriptor.Schema` 声明框架表
ownership，生成的 `db.FrameworkTables()` 目录在 `chok migrate status`
呈现，已装配电池仍自行演进其 schema。`migrate: off`
⇒ 框架零 DDL（电池表也不建）。CLI：
`chok migrate create|up|status|repair`（up/status/repair 经 conf 装载栈读
`--config`，env 覆盖 opt-in `--env-prefix`）。

### #25 SoftUnique 在 Postgres 用 partial unique index

`WHERE deleted_at IS NULL`（不含 delete_token 列）；与
mysql/sqlite 的 `(cols..., delete_token)` 复合唯一在可观测行为上
等价（活行冲突、软删释放、软删行互不冲突）。

### #26 / #38 测试面

`choktest.NewTestDB` 与 `db/dbtest.Open` 返回 `*db.DB`（v1
`*gorm.DB`）；`NewTestStore` 跟随新签名；测试内 raw gorm =
`h.Unsafe(ctx)`。`choktest` 新增 `NewTestKernel` / `StartKernel`
（真装配启动）与 `NewServeRouter`（真派发 Router 替身）。

### #28 依赖树

gin 与 badger 自 go.mod 消失（含 sonic / quic-go / ristretto 等
传递树）。若你的应用间接依赖它们，需自行显式引入。

### MySQL 时间写入基准 → UTC【行为变更，beta.6 之后】

v1 与 v2（≤ beta.6）的 MySQL 驱动 `Loc=time.Local`：DATETIME 列存
**进程时区**的墙钟，而 `CURRENT_TIMESTAMP`/`NOW()` 求值与
TIMESTAMP 列解释骑在 **session（通常即服务器）时区**上——旧基准
实为两个时区。此后 v2 双钉 UTC——驱动 `Loc=time.UTC` + 每连接
`SET time_zone='+00:00'`（后续 v2 又加了 `mysql.time_zone`，可把
基准配成一个固定数字偏移、默认仍 UTC——本节配方按默认写，配了
偏移的把目标 `'+00:00'` 换成所配偏移）。**两个旧时区都等于目标
基准（默认 UTC）的部署才是零动作**——UTC 存量配偏移目标同样要重基
（进程时区按 Go 解析顺序核实：`TZ` → `/etc/localtime` → UTC，烘了
`/etc/localtime` 的镜像不是 UTC；session 时区即服务器 `time_zone`
除非应用显式设过——UTC 进程配非 UTC 服务器仍有 session 写入的
存量要迁）。重基按**来源**逐列：备份、停**该库全部写入方**（旧 chok 实例 +
外部服务/ETL/运维 SQL——窗口内任何写入不是漏转就是被双转；禁滚动
升级；新二进制的 `chok migrate` 写命令本身就算首启，`status`
纯读可跑）后——
驱动写入的
DATETIME 按旧进程时区、SQL 求值的 DATETIME（软删 `deleted_at`）按
旧 session 时区 `CONVERT_TZ` 到 +00:00；参数写入的 TIMESTAMP 列
（含框架迁移账本）在两个旧时区不同时按
`CONVERT_TZ(col, '<旧进程>', '<旧 session>')` 消偏斜，纯
`DEFAULT CURRENT_TIMESTAMP` 生成的值无需处理——该区分**按行**：
混合来源的列只转参数写入的行（chok 账本只转
`provenance IN ('applied','baseline')` 的行——旧引擎写入时即打标；
**全部重基在新版首次启动之前、停写窗口内完成**：首启会以新基准
写入同标记的账本行并刷新 manifest 的 updated_at，事后重基会把
正确的新值搬歪；beta.4 直跳的库无这些列、语句响亮失败即「纯
DEFAULT 账本无需重基」的信号；已先启动则账本语句加
`AND version <= 升级前最高版本`（新版**重建/刷新**过的 version 再
剔除：mark-applied 重写的、retry 删行后再次 up 重建的）、repair
history 以升级前 MAX(id) 为界、
manifest 只转**升级前已存在 kind** 的 claimed_at（首启新
claim/adopt 的行是新基准）；边界不明的记账表整体跳过（偏斜仅
记账留痕），**业务表则每条语句都要带升级前行边界**——且 id 界只
排除新插入：updated_at/deleted_at/last_used_at 这类新版会在存量
行上改写的列 UPDATE 不改 id、无标可分，被首启后流量触碰过就没有
正确的就地转换，回滚备份重来）。免迁的
DEFAULT 值升级后 API 可见瞬间会被校正 (旧 session−旧进程) 的差
（旧读取原本偏斜返回；数据不动、可见值移动）。**DATE 列**存量
不动，但写入契约改为「存瞬间按所配基准（默认 UTC）的历日」——
date-only 值以所配基准的午夜构造，其他时区的本地午夜落到相邻
历日。四条执行纪律：命名时区先跑
`CONVERT_TZ` 探针、非 NULL 才继续（tz 表未装载时静默返 NULL，
可空列被写 NULL——软删行复活）；每条 UPDATE 前按同谓词扫描
（converted IS NULL 或 =原值 计数须 0——范围外值原样返回不报错；
按命中臂分流：IS NULL 行=坏日期、DATE_SUB 同 NULL 救不了→人工；
仅 =原值 行改**带符号** interval 区间算术：东偏正号减、西偏负号
加，照抄正号会反向，step-3 目标 TIMESTAMP 自身 1970–2038 存不进
即人工；命名时区走应用侧）；单事务前核验目标表全
InnoDB（非事务表无视 ROLLBACK）；配方不可重复执行，无事务则逐
语句记完成点、状态不明回滚备份。完整配方与 DST
存量注意事项见根目录 CHANGELOG 对应 Breaking 条目。同库的非 chok
写入方此后须同样按所配基准（默认 UTC）的墙钟写入 DATETIME。API 响应里 MySQL 后端
的时间戳渲染变为 `Z` 后缀——驱动写入的 DATETIME 是同一瞬间换
渲染，免迁的 DEFAULT TIMESTAMP 则如上所述可见瞬间被校正。

---

## 五、观测与电池行为

### #32 Healther 收敛为 error 型【行为变更】

v1 的 Down/Degraded 三级 → v2 正常/故障 + `disabled` 信息态。
scheduler 的 failing-jobs、audit 的 sink-failures、redis latency、
db 池水位等「Degraded」全部让位给日志 / `Stats()` / metrics——
组件健康不再因单个 flaky job 或 sink 抖动摘除流量。

### #34 audit 四个 v1 死配置全部生效

- purge cron 真跑（`retention_days` / `purge_batch_size` hot；
  `purge_interval` **restart**——cron spec 注册期固定；scheduler
  缺席 ⇒ purge 禁用 + 注记）。
- `GET /audit/logs` admin API 落地（`enable_admin_api`；
  `RequireAuthz("audit","read")` fail-closed；authn 经 account
  角色发现软装配）。

### #35 casbin `audit_enabled=true` ⇒ audit 硬前置

audit 未装配 / `enabled:false` / Init 失败 / Migrate 期同步探针
写入失败，任一 ⇒ **authz 启动失败**（含开关探针审计条目）。策略
变更（含 bootstrap 播种）经 async hook 落 audit_logs。casbin_rule
建表从 adapter 构造期挪入 authz 的 Migrate（`migrate: off` 缺表
⇒ LoadPolicy 启动失败 fail-closed）。

### #36 headless 装配不再可用

装配任一 Mounter 电池（account / audit / health / ...）而无 web
模块 ⇒ mount 门禁启动失败。纯 sink 无 HTTP 的 audit 形态在 v2.0
不可装配。

### #37 audit 的依赖拓扑

`Needs = db + scheduler? + account?`——与 authz / http 的关系改为
mount / request-time（fail-closed 语义不变），避免 v1 式软依赖
三节点环被 kernel 拓扑拒绝。

---

## 附：迁移检查清单

1. `go.mod`：`github.com/zynthara/chok` → `github.com/zynthara/chok/v2`，
   全仓 import 路径同步。
2. 删除巨型 Config 结构体；框架段交给模块（yaml 形态基本不变，
   对照上文第二节改名/新增项），业务段改 `chok.Section[T]`。
3. `WithConfig/WithSetup/Register/On/AddCleanup` → `Use/Override/
   Routes/自定义组件/总线订阅/main defer`。
4. 路由：`:param` → `{param}`；`*any` → `{any...}`；检查
   trailing-slash 依赖；dashboards 换 `{rid}` 标签。
5. store：构造签名、事务写法、Unsafe 出口按第四节替换。
6. 跑 `chok sync && go build ./...`，再用
   `docs/chok.schema.json` 校验你的 chok.yaml（编辑器或 CI）。
7. 冒烟：启动后核对 `/healthz`、`/componentz`（debug 开启时）与
   `/swagger/doc.json`。
