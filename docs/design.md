# chok 设计文档

> chok 的架构设计与 API 契约。源代码是真相的最终来源；本文负责把
> 「为什么」和「契约边界」讲清楚。每次公开 API 调整时与代码同步更新。

---

## 1. 定位

chok 是一个 Go Web 框架。三个不可变形容词：

- **全家桶**：HTTP + 数据层 + 缓存 + 认证 + 任务调度 + 观测在同一个
  仓库内，不把能力外推到用户自组装。
- **配置驱动**：`chok.yaml` 的 `enabled: true/false` 是启停子系统的
  主要方式，Go 代码改动最小化。
- **单一官方实现**：每个能力只给一个成熟方案（HTTP=gin、ORM=gorm、
  缓存=otter+badger+redis 三层、cron=robfig、JWT=golang-jwt、
  可观测性=prometheus + OpenTelemetry）。

Module path: `github.com/zynthara/chok`

---

## 2. 设计公理

1. **Config is the only knob** — 用户 90% 需求靠 yaml 解决，零 Go
   代码。
2. **One blessed implementation** — 接口留扩展点，不提供平行选择。
3. **Internally complex, externally trivial** — Component 抽象内部
   含拓扑排序 / reload 分发 / 健康聚合；对外是 `app.Register` +
   `app.Logger()`。
4. **Everything is a Component** — 所有子系统实现同一契约
   （Init / Close + 可选 Reload / Health / Router / Migrate）。
5. **Future-proof registration** — Component 通过 Resolver / Builder
   模式解耦具体 config schema，新能力纯增量。

---

## 3. 架构总览

```
                        ┌────────────────────────┐
                        │       chok.App         │
                        │  (lifecycle owner)     │
                        └───────┬────────────────┘
                                │
              ┌─────────────────┼──────────────────┐
              │                 │                  │
         servers []       component.Registry   cleanupFns []
         (chok.Server)    (Component lifecycle) (legacy hooks)
                                │
          ┌────────────┬────────┴──────┬────────────┐
          ▼            ▼               ▼            ▼
       parts/       parts/          parts/       user-defined
       LoggerComp   DBComp          HealthComp    Components
       (Reloadable) (Migratable)    (Router)
```

**关键依赖方向**（单向，无循环）：

```
chok (root) ─── parts ─── component
   │             │          │
   │             ├── store  │
   │             ├── db     │
   │             ├── cache  │
   │             ├── redis  │
   │             ├── auth   │
   │             ├── swagger│
   │             └── ...    │
   │                        │
   └──── log ◀──────────────┘
```

`component` 只依赖 `log`；`parts` 依赖 `component` + 各子系统；
`chok` 根包依赖 `parts`（用于 auto-register 内置 LoggerComponent）。

---

## 4. 核心抽象

### 4.1 Component

每个子系统实现的强制契约（仅三个方法）：

```go
// component/component.go
type Component interface {
    Name() string         // 唯一标识，e.g. "db"
    Init(ctx context.Context, k Kernel) error
    Close(ctx context.Context) error
}
```

**可选能力**（按需实现，Registry 按类型断言调用）：

```go
type Reloadable        interface{ Reload(ctx) error }                // 热加载配置
type Healther          interface{ Health(ctx) HealthStatus }         // /healthz 聚合
type Router            interface{ Mount(router any) error }          // HTTP 路由
type Migratable        interface{ Migrate(ctx) error }               // 启动期 schema 迁移
type Dependent         interface{ Dependencies() []string }          // 声明硬依赖
type OptionalDependent interface{ OptionalDependencies() []string }  // 声明软依赖
type Optionaler        interface{ Optional() bool }                  // Init 失败不中止启动
type InitTimeouter     interface{ InitTimeout() time.Duration }      // 自定义 Init 超时
type CloseTimeouter    interface{ CloseTimeout() time.Duration }     // 自定义 Close 超时
type ReloadTimeouter   interface{ ReloadTimeout() time.Duration }    // 自定义 Reload 超时
type ConfigKeyer       interface{ ConfigKey() string }               // yaml 段 key（可选）
type ReadyChecker      interface{ ReadyCheck(ctx) error }            // warm-up 就绪检查
type DepsValidator     interface{ ValidateDeps(k Kernel) error }     // Init 前校验依赖
```

**类型安全访问辅助**：

```go
component.Get[T](k, "redis")      // → (T, bool)，nil-safe
component.MustGet[T](k, "redis")  // → T，miss 时 panic
```

### 4.2 Kernel

Component 看到的 App 视图（依赖倒置）：

```go
type Kernel interface {
    Config() any                                 // 应用 config（live 指针，单字段读安全）
    ConfigSnapshot() any                         // 原子快照（多字段读安全，Resolver 首选）
    Logger() log.Logger                          // 共享 logger
    Get(name string) Component                   // 按名取其它组件
    On(event Event, hook Hook)                   // 订阅事件
    Health(ctx) HealthReport                     // 聚合健康报告
    ReadyCheck(ctx) error                        // 聚合就绪检查
}
```

Component 在 `Init(ctx, k)` 时捕获 Kernel 引用，后续 `Reload` /
`Close` / `Health` 都可通过它复用。内置 Resolver 均调用
`k.ConfigSnapshot()` 读取配置，避免 Reload 期间的 torn read。

**ConfigSnapshot 浅拷贝语义**：`ConfigSnapshot()` 返回 config 结构体
的**浅拷贝**副本——顶层字段（值类型、指针、slice/map header）是独立
的，但 slice/map 的底层存储仍与 live config 共享。内置 `*Options` 中
也已有 slice 字段（如 `HTTPOptions.TrustedProxies`、`SlogOptions.Output
/Files/AccessFiles`），所以**不应依赖"修改 snapshot 不影响 live"**：
把 snapshot 当只读。Reload 管线会重新发布 snapshot，读者看到一致视图。
需要深拷贝时调用方自己 clone slice/map。

### 4.3 Registry

Component 生命周期引擎。

```go
// component/registry.go
reg := component.New(cfg, logger)
reg.Register(&DBComponent{...})
reg.Register(&AccountComponent{...})  // Dependencies() = ["db", "log"]

reg.Verify()     // dry-run：只验证依赖图，不 Init
reg.Order()      // 返回拓扑排序后���组件名列表

reg.Start(ctx)           // topo 排序 → Init + Migrate → AfterStart hooks
reg.StartOnly(ctx, ...)  // 只启动指定组件 + 传递依赖（集成测试用）
reg.Reload(ctx)          // 只派发给 Reloadable
reg.Stop(ctx)            // 反序 Close
reg.Health(ctx)          // 并行聚合状态（带 per-probe 超时）
reg.ReadyCheck(ctx)      // 聚合 ReadyChecker（warm-up gate）

// 配置（App 设置保守默认值：Init 30s, Close 15s, Health 3s）
reg.SetDefaultInitTimeout(30 * time.Second) // 每组件 Init 超时
reg.SetCloseTimeout(15 * time.Second)       // 每组件 Close 超时
reg.SetHealthTimeout(3 * time.Second)       // 每探针 Health 超时 + 硬 fan-in deadline
```

**Start 流程**：

1. `EventBeforeStart` hook 触发
2. 拓扑排序（Kahn 算法），检测循环依赖 / 未知依赖 / 自依赖
3. 按拓扑层级并行 Init（带 per-component 超时）；同层组件全部 Init
   完成后，按层序串行调用 Migrate（DDL 对并发通常不安全，串行更可靠）
4. `Optionaler` 组件 Init/Migrate 失败 → warn 日志，跳过继续
5. 必需组件 Init 失败 → 进入 `phaseStopping`，回滚已成功的 Component
   （反序 Close）；期间并发 Reload 被阻断
6. `EventAfterStart` hook 触发

**Stop 流程**：按拓扑层级逆序 Close，同层组件并行 Close（与 Start
的并行 Init 对称）；错误 joined 返回（一个失败不阻塞其它）。Stop 接
受 `phaseStarted` 和 `phaseStopping` 两种进入状态。

**Reload**：按 topo 序派发给 `Reloadable`，**不**触发 `Migratable.Migrate`
——schema 变更（新增表 / 列 / 索引）需要重启进程才能生效，避免热加载
路径上的 DDL 与活跃事务冲突。如果业务确实需要热加载 schema，请在
`reloadFn` 中显式调用 `db.Migrate`，自行承担与 in-flight 查询的并发
风险。错误收集后一起返回。非
`phaseStarted` 状态（包括 rollback 期间的 `phaseStopping`）直接拒绝。

**Verify** (`reg.Verify()`)：dry-run 不 Init，依次做 (1) 拓扑排序 +
依赖完备性校验，(2) 对每个实现 `DepsValidator` 的组件调用
`ValidateDeps(kernel)` 收集错误。

### 4.4 Event 总线

```go
const (
    EventBeforeStart  Event = "before_start"
    EventAfterStart   Event = "after_start"
    EventBeforeStop   Event = "before_stop"
    EventAfterStop    Event = "after_stop"
    EventBeforeReload Event = "before_reload"
    EventAfterReload  Event = "after_reload"
)

type Hook func(ctx context.Context) error
```

典型用途：`EventAfterStart` 里把所有 `Router` Component 的路由
Mount 到 HTTP engine 上——此时 Component 都已 Init 就绪。

**注意**：`EventAfterStart` 表示"所有 Component 已 Init + 路由已
挂载"，**不**表示 HTTP server 已监听。Server 在 `EventAfterStart`
hook 完成后才开始 `Start(ctx, ready)`。应用真正可接流量的标志是
HTTP server 调用 `ready()`，反映在 App 日志 `"all servers ready"`。

**双生命周期模型**：Component 管理资源初始化（Init/Close），Server
管理阻塞监听（Start/Stop）。HTTPComponent 在 Init 时构建 gin
Engine，App 在 `registry.Start` 后提取 HTTPServer 交给
`runServers` 管理。两者通过 `extractHTTPServer()` 桥接。

---

## 5. App 生命周期

### 5.1 构造与启动

```go
app := chok.New("myapp",
    chok.WithConfig(&cfg, "configs/myapp.yaml"),
    chok.WithSetup(setup),
)
app.Execute()   // 加信号处理 + os.Exit
// or
app.Run(ctx, chok.WithSignals(), chok.WithConfigWatch())
```

### 5.2 Run 流程

```
 1. loadConfig              viper 读 file + env + flag → 填 configPtr
                            解析后的路径写回 a.configPath（供 watcher 使用）
                            默认路径依次探测 ./{name}.yaml → ./configs/{name}.yaml
 2. initLogger              构造 slog + access logger；auto-register
                            LoggerComponent(WithPreBuilt)
 3. setupFn                 用户 Register Components / SetDB / AddServer
                            注意：a.Cacher() 在 setup 阶段返回 nil，缓存由
                            CacheComponent.Init 构建（可集成 Redis）。
                            registry.Start 完成后 a.cacher 才可用。
                            若 setup 需要提前使用缓存，显式调用 SetCacher
 4. autoRegisterComponents  扫描 configPtr 中已知 Options 类型，自动注册
                            未被用户显式 Register 的内置 Component
                            cache 通过 autoRegisterCache 统一处理（pre-built
                            或 DefaultCacheBuilder 含 Redis 层）
                            配置歧义 fail-fast：返回错误终止启动
 5. internalMountHook       只要 HTTP Server 存在就注册 EventAfterStart hook
                            （不依赖 WithRoutes）：挂载非 swagger Router →
                            routesFn → swagger（最后）。无 HTTP 时若已注册
                            Router Component 会打 warn 日志提示
 6. registry.Start          Components Init + Migrate；AfterStart hooks
                            失败时调用 registry.Stop 清理已 Init 的组件
 7. runServers              并行启动每个 chok.Server，等待所有 ready
                            SIGHUP / file-change → handleReload → Reload(ctx)
                            SIGINT/SIGTERM → shutdown
 8. registry.Stop           Components 反序 Close（含 LoggerComponent）
                            phaseBuild 时为 no-op（topoSort/before_start 失败无需清理）
 9. runCleanups             旧风格 AddCleanup 回调 LIFO 执行
```

**关键不变量**：

- setupFn 运行时 registry 尚未构造，用户调 `app.Register(c)` 是 push
  到 pending 队列；setupFn 结束后统一转入 registry
- setupFn 里 `app.Logger()` 已就绪；`app.Cacher()` 默认返回 nil，因为
  缓存由 CacheComponent.Init 在 registry.Start 期间构建。若 setup 中
  显式 `SetCacher(c)` 注入外部缓存，后续 `Cacher()` 立即返回该实例
- setupFn 可以 `app.On(event, hook)` 注册 hook（也走 pending 队列）
- SetCacher 在 setupFn 中可注入外部 cache；autoRegisterCache
  取 a.cacher 的最终值，确保 CacheComponent 不会持有过期引用
- 内置 config Options 字段必须用 value embedding（不支持 pointer field），
  因为 reload 时 Set() 拷贝值到原始内存，pointer field 会导致旧指针
- 用户显式 Register("db") 等优先于 auto-register
- autoRegisterComponents 在 setupFn 之后执行，不覆盖用户选择
- WithRoutes 的回调在 EventAfterStart 中执行，此时所有 Component 已 Init
- Server.Stop 是 shutdown 唯一触发器，idempotent
- Run 是 single-use（再次调用返回 error）

### 5.3 构造 Options

来自 `options.go`：

| Option | 作用 |
|---|---|
| `WithVersion(version.Info)` | 打印启动日志时的版本标识 |
| `WithConfig(cfg any, path...)` | 注册 config struct 与可选显式 yaml 路径 |
| `WithEnvPrefix(string)` | 环境变量前缀（默认由 app name 推出：字母转大写、数字保留、其他字符一律转为 `_`，如 `my-blog` → `MY_BLOG`） |
| `WithLogConfig(*SlogOptions)` | 显式指定 log options 指针（绕开反射） |
| `WithCacheConfig(mem, file)` | 同上，指定 cache |
| `WithLogger(log.Logger)` | 直接注入 logger（最高优先级） |
| `WithSetup(fn)` | setup 回调（高级：显式 Register 自定义 Component） |
| `WithTables(...db.TableSpec)` | 声明 DB 迁移表，auto-register 的 DBComponent 使用 |
| `WithRoutes(fn)` | AfterStart 业务路由回调；框架自动编排 mount 顺序 |
| `WithCleanup(fn)` | 追加 cleanup 回调 |
| `WithShutdownTimeout(d)` | 默认 30s |
| `WithInitTimeout(d)` | 每组件 Init+Migrate 超时，默认 30s（`InitTimeouter` 可覆盖） |
| `WithCloseTimeout(d)` | 每组件 Close 超时，默认 15s（`CloseTimeouter` 可覆盖） |
| `WithHealthTimeout(d)` | 每探针 Health 超时，默认 3s |
| `WithReloadFunc(fn)` | 用户 reload 钩子（Reload 管线的最后一步） |
| `WithReloadTimeout(d)` | 默认 10s |
| `WithDrainDelay(d)` | shutdown 时等待 LB 摘流量的延迟（K8s preStop） |
| `WithHookTimeout(d)` | 生命周期 hook 聚合超时 |
| `WithComponentReloadTimeout(d)` | 每组件 Reload 超时覆盖 |
| `WithFlags(*pflag.FlagSet)` | 注入 CLI flag，最高配置优先级 |

**RunOption**：

| RunOption | 作用 |
|---|---|
| `WithSignals()` | 监听 SIGINT/SIGTERM/SIGHUP/SIGQUIT |
| `WithConfigWatch()` | 启用 fsnotify 监听 config 文件变化 → auto Reload |

### 5.4 公开 App API

```go
// 服务器与清理
app.AddServer(srv chok.Server)
app.AddCleanup(func(context.Context) error)

// Component 生命周期
app.Register(c component.Component)
app.Registry() *component.Registry
app.On(event component.Event, hook component.Hook)

// 访问器
app.Logger() log.Logger
app.AccessLogger() log.Logger
app.AccessLogEnabled() bool
app.Cacher() cache.Cache
app.DB() any                    // legacy convenience；推荐 component.Get[*parts.DBComponent](k, "db")

// 便捷路由（WithRoutes 回调内使用）
app.API(path, mw...) *gin.RouterGroup  // 从 HTTPServer 获取 group
app.AuthMiddleware() gin.HandlerFunc   // 从 AccountComponent 获取 Authn

// 设置器（setupFn 中使用）
app.SetCacher(c cache.Cache)
app.SetDB(gdb any)

// 生命周期控制
app.Run(ctx, ...RunOption) error
app.Execute()                      // Run + WithSignals + os.Exit
app.Reload(ctx) error
app.ReloadConfig() (bool, map[string]bool, error)
```

### 5.5 Server 契约

```go
// chok.go
type Server interface {
    Start(ctx context.Context, ready func()) error
    Stop(ctx context.Context) error
}
```

- `Start` 阻塞直到 Stop 被调或发生错误
- `ready()` 在服务器真正可接受请求时调用一次
- `Stop` 是唯一 shutdown 触发，idempotent

`server.HTTPServer`（基于 gin）是框架提供的标准实现。

---

## 6. 配置系统

### 6.1 加载优先级

CLI flag > env var > 配置文件 > struct tag `default`。

文件解析路径顺序：

1. `WithConfig(cfg, "explicit/path.yaml")` 显式路径（找不到报错）
2. `{PREFIX}_CONFIG` 环境变量（找不到报错）
3. `./{app.name}.yaml` 默认路径（找不到回退到下一项）
4. `./configs/{app.name}.yaml` 次级默认（找不到静默跳过）

### 6.2 验证

每个 `*Options` 可实现 `Validate() error`；root `Config` 亦可。
App 加载后自动递归调用（先叶后根）。

### 6.3 热加载（不可变两阶段 Reload）

```
SIGHUP ──┐
fsnotify─┼──▶ handleReload ──▶ App.Reload(ctx)
手动调用──┘                        │
                                   ├─ ReloadConfig（两阶段不可变 reload）
                                   │   1. 创建 config 零值副本
                                   │   2. 加载 file/env/flag → 副本
                                   │   3. 验证副本
                                   │   4. 验证通过 → atomic 拷贝到 live config
                                   │   5. 验证失败 → live config 完全不变
                                   ├─ registry.Reload（Reloadable 组件）
                                   └─ reloadFn（用户钩子，可选）
```

- **ReloadConfig 保证原子性**：失败时 live config 零字段污染
- fsnotify 监听**父目录**而非文件路径（抗 atomic-save 编辑器的
  rename+replace）
- 100ms debounce 合并短时间内的多次 Write 事件
- 非 Reloadable Component 被跳过（不报错）
- 任一步骤失败会短路后续步骤，Reload 整体返回错误
- **三源合并**（SIGHUP / fsnotify / `App.Reload(ctx)`）：`App.Reload`
  用 `reloadMu.TryLock()` 串行化；并发触发的第二个调用立即返回
  `ErrReloadInProgress` 而不是排队。设计假设是 in-flight reload 已经
  会读到最新的 on-disk 配置，再排一份只是徒增延迟。需要确保某次配置
  变更被吸收的调用方应在收到 `ErrReloadInProgress` 后退避重试，或
  直接等下一次触发自然合并

### 6.4 内置 config.Options

定义在 `config/config.go`：

| 类型 | 覆盖 |
|---|---|
| `HTTPOptions` | addr / read_timeout / write_timeout / ... |
| `MySQLOptions` | host / port / user / pass / db / pool |
| `SQLiteOptions` | path / WAL 自动启用 |
| `RedisOptions` | addr / password / db |
| `SlogOptions` | level / format / output / files / access_* |
| `LogFileOptions` | path / rotation 参数（lumberjack） |
| `CacheMemoryOptions` | enabled / capacity / TTL |
| `CacheFileOptions` | enabled / path / TTL（badger） |
| `SwaggerOptions` | enabled / title / version / prefix / bearer_auth |
| `AccountOptions` | enabled / signing_key / expirations |

---

## 7. 数据层

### 7.1 db 包

GORM 包装层，提供：

```go
db.NewMySQL(*config.MySQLOptions) (*gorm.DB, error)
db.NewSQLite(*config.SQLiteOptions) (*gorm.DB, error)
db.Close(*gorm.DB) error
db.Transaction(ctx, gdb, fn) error
db.RunInTx(ctx, gdb, fn(txCtx)) error  // context-scoped transaction propagation

// Migrate
db.Table(model, indexes...) TableSpec
db.SoftUnique(name, columns...) SoftIndex
db.Migrate(ctx, gdb, specs ...TableSpec) error

// Model mixins
type Model struct { ID uint; RID string; Version int; CreatedAt/UpdatedAt }
type SoftDeleteModel struct { Model; DeletedAt; DeleteToken }
type OwnedModel struct { Model; Owned }
type OwnedSoftDeleteModel struct { SoftDeleteModel; Owned }

// Model hooks
(m *Model) BeforeCreate(tx)  // 自动生成 RID + 初始化 Version=1
```

RID 前缀约束：1-10 小写字母+数字，总长 ≤ 23；非法 panic。

### 7.2 store 包

泛型 CRUD 门面，构建在 gorm + db.Model 之上。

#### 核心 API（6 个方法）

```go
s := store.New[User](gdb, logger,
    store.WithQueryFields("id", "name", "email"),
    store.WithUpdateFields("name", "email"),
    store.WithScope(store.OwnerScope("admin")),
)

s.Create(ctx, obj)                                               // INSERT
s.Get(ctx, locator, ...QueryOption) (*T, error)                  // SELECT 单条（支持 WithPreload）
s.List(ctx, opts ...where.Option) (*Page[T], error)              // SELECT 列表；Page{Items, Total}
s.Update(ctx, locator, changes, opts ...UpdateOption) error      // UPDATE
s.Delete(ctx, locator, opts ...DeleteOption) error               // DELETE
s.Tx(ctx, fn)                                                    // 事务
```

`Page[T] = struct{ Items []T; Total int64 }`；`Total` 只有
`where.WithCount()` 参与时才填充，否则为 0。`ListWithCursor` 返回
`*CursorPage[T]`（`Items []T; NextCursor string`，`NextCursor` 非空表示还有下一页）。

#### Locator 抽象（"谁"）

```go
store.RID("usr_abc")            // 按 RID 定位（对外契约）
store.ID(42)                    // 按内部 PK 定位（join / batch）
store.Where(where.Options...)   // 按条件定位（批量 / 复杂查询）
```

`Where` Locator 强制要求至少一个 filter 条件；仅有 order/pagination
的 Where 返回 `ErrMissingConditions`——防止 `Delete(Where())`
清表。

#### Changes 抽象（"改什么"）

```go
store.Set(map[string]any{"name": "Alice"})        // map 形态，无锁
store.Fields(&user, "name", "email")              // 对象 + 白名单
store.Fields(&user)                               // 不列字段 = 白名单全集
store.Fields(&user, "name").NoLock()              // 显式禁用乐观锁
```

**Fields 的关键特性**：

- 自动从 `obj.Version` 提取乐观锁版本号（obj 嵌入 `db.Model`）
- 零值**强制落库**（内部用 `Select(cols...).Updates(obj)` 绕过 GORM
  默认的 skip-zero 行为）
- 白名单外字段返回 `ErrUnknownUpdateField`

#### UpdateOption / DeleteOption

```go
store.WithVersion(v int)
```

为 `Set(map)` 启用乐观锁；覆盖 `Fields` 的自动版本。Update/Delete
共享此 Option（`versionOpt` 同时实现两个 interface）。

#### 选型决策表

| 场景 | 推荐写法 |
|---|---|
| 标准 HTTP 编辑（带乐观锁） | `Update(RID(x), Fields(&obj, ...))` |
| 拖拽排序（并发覆盖可接受） | `Update(RID(x), Set(cols))` |
| Worker 回填 | `Update(ID(id), Set(cols))` |
| 单字段 map + 乐观锁 | `Update(RID(x), Set(m), WithVersion(v))` |
| 强制覆盖 | `Update(RID(x), Fields(&obj).NoLock())` |
| 按 RID 读 | `Get(RID(x))` |
| 按 ID 读（内部 join） | `Get(ID(id))` |
| 按条件读单条 | `Get(Where(where.WithFilter("email", x)))` |
| 幂等删除 | `Delete(RID(x))` |
| 乐观锁删除 | `Delete(RID(x), WithVersion(v))` |
| 批量删除 | `Delete(Where(opts...))` |

#### Upsert 限制

`Upsert` 在以下情况被禁止（返回 `ErrUpsertScoped`）：

1. Store 注册了任何 scope（`WithScope(...)`）
2. Store 的模型嵌入 `db.Owned`——即便 `WithoutOwnerScope()` 关闭了
   自动 OwnerScope，只要模型本身是 Owned，Upsert 仍被拒绝

原因：SQL `INSERT ... ON CONFLICT DO UPDATE` 不会把 scope 产生的
`WHERE` 条件应用到冲突更新路径，导致租户隔离等安全 scope 被绕过。
Owned 模型的第二条规则是防御纵深——即使用户"故意"禁用了 OwnerScope，
攻击者仍不能通过冲突键直达 UPDATE 路径修改他人的行。

需要 upsert 语义时，请用 `Create` + 检测 `ErrDuplicate` + `Update`
组合，或通过 `s.DB()` 逃生门自行构造带安全约束的 SQL。

#### 其它方法

```go
s.Exists(ctx, locator) (bool, error)       // 存在性检查（比 Get 高效）
s.BatchCreate(ctx, []*T)                   // 事务批量插入
s.ListByIDs(ctx, []uint)                   // 批量按 ID 查
s.ListQ(ctx, []QueryOption, ...where.Option) // List + WithTrashed/WithPreload
s.ListWithCursor(ctx, field, dir, cursor, size) // 游标分页（keyset pagination）
s.ListFromQuery(ctx, url.Values)           // 从 HTTP query 解析分页 + filter
s.Transaction(ctx, fn)                     // 同 Tx
s.WithTx(tx *gorm.DB)                      // 绑定外部事务
s.DB() *gorm.DB                            // 逃生门（无 scope）
s.ScopedDB(ctx) (*gorm.DB, error)          // 逃生门（含 scope）
```

#### Scope 系统

```go
type ScopeFunc func(ctx, db) (*gorm.DB, error)

store.WithScope(scope)                     // 注册自定义 scope
store.WithoutOwnerScope()                  // 禁用自动 OwnerScope

// 内置
store.OwnerScope(adminRoles ...string) ScopeFunc
```

`OwnerScope` 对实现 `db.OwnerAccessor` 的模型自动添加 `WHERE
owner_id = <principal.Subject>`；admin 角色跳过过滤；未认证 context
返回 `apierr.ErrUnauthenticated`（fail-closed）。

**写入侧的 owner 强制**：Create/BatchCreate 调用 `fillOwner` 时，
非管理员 principal 提供的 OwnerID 会被**强制覆盖**为
`principal.Subject`。这防止攻击者通过请求体伪造 owner_id 字段冒充
其他用户。管理员（默认角色 `"admin"`，可通过
`store.SetDefaultAdminRoles(...)` 覆盖）保留显式指定 owner 的能力，
用于数据导入/跨租户维护等合法场景。无 principal（后台任务/测试）
时 `fillOwner` 是 no-op——请在 HTTP 层通过 Authn 中间件把关。

### 7.3 where 包

查询 DSL。Option 模式构造 WHERE / ORDER / LIMIT。

```go
where.WithFilter(field, value)
where.WithFilterOp(field, op, value)        // op: Eq/Ne/Gt/Gte/Lt/Lte
where.WithFilterIn(field, values...)
where.WithFilterLike(field, pattern)        // 自动转义 % _ \
where.WithFilterLikeRaw(field, pattern)     // 原样透传，仅供受信调用
where.WithOrder(field, desc...)
where.WithPage(page, size)
where.WithOffset(n) / where.WithLimit(n)
where.WithCount()                           // 触发 COUNT 查询
```

字段名全部走 Store 的 query whitelist；未注册字段返回
`ErrUnknownField`。

`WithPage` / `WithLimit` 强制 `1 ≤ size ≤ where.MaxPageSize`（默认
10000）且 `(page-1)*size` 不能溢出 int32，防止客户端构造异常分页
参数触发整数溢出或 OOM。`WithOffset` 拒绝负值。`FromQuery` 在
handler 层再次 clamp `size` 作为 defense-in-depth。

`WithFilterLike` 默认对 pattern 中的 `% _ \` 做转义并加 `ESCAPE '\'`
子句——用户输入不会扩张匹配集。需要保留原始 LIKE 语义（如内部
管理工具基于服务端构造的模式）时改用 `WithFilterLikeRaw`，由调用方
自行负责转义。`WithFilterContains` / `WithFilterStartsWith` /
`WithFilterEndsWith` 在 `WithFilterLike` 的转义基础上额外注入
前后置 `%` 通配。

`WithFilterIn` 受 `where.MaxInList`（默认 500）保护，超过返回
`ErrInvalidParam`——避免 SQLite `max_variable_number` 或 PostgreSQL
参数上限被触发。

`WithCursor` 要求游标字段**严格唯一**（通常 `id` / `rid`）。对非唯一
列（如 `created_at`）用 `WithCursorBy(field, direction, fieldCursor,
idCursor, size)`，内部拼 `(field, id)` 复合 keyset，确保同值行不会在
分页边界被跳过。

**Preload scope 传播**：`WithPreload(relation)` 在加载关联时会把
Store 当前 ctx + scopes 链路重新应用到子查询——防止 OwnerScope 对
主表生效、关联表放行的越权读取。自定义 scope 如果对关联表不适用，
应通过 ctx 判断后返回 `q` 自身 opt-out。

**安全提示**：`store.New` 未显式指定 `WithQueryFields` /
`WithUpdateFields` 时，框架从 JSON tag 自动发现并以 warn 级别
日志输出发现的字段集合。生产环境建议显式声明白名单。

---

## 8. HTTP 层

### 8.1 handler 包

泛型化的请求处理器：

```go
type HandlerFunc[T, R any] func(ctx, req *T) (R, error)
type ActionFunc[T any]     func(ctx, req *T) error
type QueryLister[T any]    interface{ ListFromQuery(ctx, url.Values) ([]T, int64, error) }

handler.HandleRequest(fn, opts ...HandleOption) gin.HandlerFunc
handler.HandleAction(fn, opts ...HandleOption) gin.HandlerFunc
handler.HandleList[T](lister, opts ...HandleOption) gin.HandlerFunc

// Options
handler.WithSuccessCode(int)
handler.WithSummary(string)
handler.WithTags(...string)
handler.WithBinders(...Binder)
```

**请求绑定**默认合并 uri + query + json（按 tag 选择）；最后统一
validator/v10 校验。

### 8.2 middleware 包

```go
middleware.Recovery()
middleware.RequestID()                      // 写入 ctx，access log 关联
middleware.Logger(log.Logger)               // per-request logger；自动注入 trace_id/span_id
middleware.AccessLog(log.Logger)            // method/path/status/latency；tracing 激活时含 trace_id
middleware.CORS(opts ...CORSOption)         // AllowCredentials + "*" 组合在构造时 panic
middleware.Authn(tokenParser, principalResolver)
middleware.Authz(authz.Authorizer)
middleware.Timeout(d time.Duration)         // 注入 deadline；deadline 触发且未写响应则写 504
```

### 8.3 server 包

```go
srv := server.NewHTTPServer(*config.HTTPOptions)
srv.Use(middlewares...)
srv.Engine() *gin.Engine        // 逃生门，给 Router Mount 用
srv.Group(path, mw...) *gin.RouterGroup
srv.Start(ctx, ready) / srv.Stop(ctx)
```

`HTTPServer` 实现 `chok.Server`，直接 `app.AddServer(srv)`。

---

## 9. parts 包：15 个内置 Component

所有 Component 都在 `parts/` 下。每个接受一个 **Resolver**（从 app
config 抽取自己那段）或 **Builder**（用 kernel 构造实例），让
Component 零耦合具体 config schema。

| Component | 硬依赖 | 软依赖 (optional) | 可选能力 | 关键 API |
|---|---|---|---|---|
| `LoggerComponent` | - | - | Reloadable, Healther | `.WithPreBuilt(l, access)`, `Logger()`, `AccessLogger()` |
| `RedisComponent` | - | - | Healther | `Client() *redis.Client`, `.SetPingTimeout(d)` |
| `DBComponent` | - | `[tracing]` | Migratable, Healther | `DB() *gorm.DB`, `.WithoutClose()` |
| `CacheComponent` | - | `[redis]` | Healther | `Cache() cache.Cache`, `.WithHardDependencies(...)`, `.WithoutOptionalDependencies()`, `.WithPreBuilt(c, owned)` |
| `HTTPComponent` | - | `[metrics, log, tracing]` | - | `Server() *server.HTTPServer`, `Engine() *gin.Engine` |
| `JWTComponent` | - | - | - | `Manager() *jwt.Manager`；自定义 Name 支持多实例 |
| `AuthzComponent` | - | - | - | `Authorizer() authz.Authorizer` |
| `SchedulerComponent` | `[log]` | - | Healther | `Scheduler() *scheduler.Scheduler`；AfterStart 自动 Start |
| `SwaggerComponent` | - | - | Router | `Spec() *swagger.Spec`；`Mount(engine)` |
| `AccountComponent` | `[db, log]` | - | Migratable, Router | `Module() *account.Module`；Mount /auth 路由 |
| `HealthComponent` | - | - | Router | `/healthz` `/livez` `/readyz` JSON 聚合报告 |
| `MetricsComponent` | - | - | Router | `/metrics` Prometheus；`PrometheusRegistry()` 暴露 |
| `TracingComponent` | - | - | - | `TracerProvider()` 永远非 nil（禁用时 noop） |
| `DebugComponent` | - | - | Router | `/componentz` 拓扑/耗时/能力诊断（默认禁用） |
| `PoolComponent` | - | - | Healther | `Pool() *scheduler.Pool`；异步任务池；`NewPoolComponentWithParent(parent, opts)` 注入长生命周期 parent ctx |

### 9.0 Auto-Register 矩阵

| 组件 | Config 类型 | 默认启用 | Auto-Register | Reloadable | Router | Optional | 配置 key |
|------|------------|---------|--------------|-----------|--------|---------|---------|
| Logger | `SlogOptions` | 是 | 是（pre-built） | 是 | - | - | `log` |
| HTTP | `HTTPOptions` | 是 | 是 | - | - | - | `http` |
| DB | `DatabaseOptions` | driver 决定 | 是 | - | - | - | `database` |
| Redis | `RedisOptions` | 是 | 是 | - | - | - | `redis` |
| Cache | `CacheMemory/FileOptions` | 否 | 是 | - | - | - | `cache` |
| Account | `AccountOptions` | 否 | 是 | - | 是 | - | `account` |
| Swagger | `SwaggerOptions` | 否 | 是 | - | 是 | - | `swagger` |
| Health | `HealthOptions` | 是（需 HTTP） | 是 | - | 是 | - | `health` |
| Metrics | `MetricsOptions` | 是（需 HTTP） | 是 | - | 是 | - | `metrics` |
| Debug | `DebugOptions` | 否 | 是 | - | 是 | - | `debug` |
| Tracing | - | - | **否（显式）** | - | - | 是 | `tracing` |
| Scheduler | - | - | **否（显式）** | - | - | - | `scheduler` |
| Pool | - | - | **否（显式）** | - | - | - | `pool` |
| JWT | - | - | **否（显式）** | - | - | - | `jwt` |
| Authz | - | - | **否（显式）** | - | - | - | `authz` |

**规则**：有 `config.Options` 类型的组件走 auto-register；无 Options
的组件必须在 `WithSetup` 中显式 `Register`。

### 9.1 Resolver 模式示例

```go
// log
parts.NewLoggerComponent(func(cfg any) *config.SlogOptions {
    return &cfg.(*MyAppConfig).Log
})

// redis
parts.NewRedisComponent(func(cfg any) *config.RedisOptions {
    return &cfg.(*MyAppConfig).Redis
})
```

### 9.2 Builder 模式示例

```go
// db（用户决定 MySQL/SQLite）
parts.NewDBComponent(
    func(k component.Kernel) (*gorm.DB, error) {
        return db.NewSQLite(&cfg.SQLite)
    },
    db.Table(&User{}),
    db.Table(&Post{}, db.SoftUnique("uk_slug", "slug")),
)

// account（依赖 db 已就绪）
parts.NewAccountComponent(
    func(k component.Kernel, gdb *gorm.DB) (*account.Module, error) {
        return account.New(gdb, k.Logger(),
            account.WithSigningKey(cfg.Account.SigningKey))
    },
    "/auth",  // group path
)
```

### 9.3 特殊模式

**Pre-built 模式**（LoggerComponent / CacheComponent）：

```go
logger := log.NewSlog(...)  // 已有 logger
parts.NewLoggerComponent(resolver).WithPreBuilt(logger, accessLogger)
// Component 在 Init 时采用 logger，跳过 NewSlog；Reload 仍有效
```

chok App 内部用这模式让 `App.Logger()` 和 registry 里的
LoggerComponent 共享同一实例。

**Router 挂载**（框架在 `internalMountHook` 中自动编排）：

```go
// Phase 1: mount all Router components except swagger/http (topo start order).
for _, c := range a.Registry().StartedComponents() {
    if c.Name() == "swagger" || c.Name() == "http" { continue }
    if r, ok := c.(interface{ Mount(any) error }); ok {
        r.Mount(engine)
    }
}
// Phase 2: user business routes (WithRoutes callback).
// Phase 3: mount swagger last (sees all routes).
```

---

## 10. 可观测性

### 10.1 Health 聚合

Registry.Health **并行**执行所有 `Healther` 探针（默认 per-probe
超时 3s），避免串行探活超过 K8s liveness probe 的默认 1s 超时。
Fan-in 层强制在 timeout+1s 内返回，即使某个 probe 不尊重
context 也不会卡住 `/healthz`。

聚合规则：

- 任一 Down → 整体 Down（HTTP 503）
- 非 Down 但有 Degraded → 整体 Degraded（HTTP 200）
- 全部 OK → OK（HTTP 200）

非 `Healther` Component 默认视为 OK。`Optionaler` 组件 Init
失败后标记为 `HealthDegraded`（非 Down），不影响 `/readyz`。

HealthComponent 暴露三个 Kubernetes 风格端点：

| 端点 | 语义 | Shutdown 行为 |
|---|---|---|
| `GET /healthz` | 完整诊断报告（聚合所有 Healther） | 不变 |
| `GET /livez` | 进程存活（永远 200，不检查 DB/Redis） | 不变 |
| `GET /readyz` | 是否可接收流量（聚合 Healther） | 立��返回 503 |

`/readyz` 在 `EventBeforeStop` hook 中自动标记 `shuttingDown`，
load balancer 摘流量后再关闭连接。

```json
{
  "status": "ok",
  "components": {
    "db":    { "status": "ok", "details": { "latency_ms": 2 } },
    "redis": { "status": "ok", "details": { "latency_ms": 1 } },
    "cache": { "status": "ok" }
  }
}
```

### 10.2 Metrics

MetricsComponent 包装 prometheus.Registry，默认装好 GoCollector +
ProcessCollector。用户通过 `PrometheusRegistry()` 注册自定义 collector：

```go
m := parts.NewMetricsComponent("/metrics")
counter := prometheus.NewCounter(prometheus.CounterOpts{
    Name: "myapp_requests_total",
    Help: "Total requests",
})
m.PrometheusRegistry().MustRegister(counter)
```

### 10.3 Tracing

TracingComponent 构建 OpenTelemetry TracerProvider，设置为 otel
global：

```go
parts.NewTracingComponent(func(cfg any) *parts.TracingSettings {
    return &parts.TracingSettings{
        Enabled:      true,
        ServiceName:  "myapp",
        Exporter:     "otlp",
        OTLPEndpoint: "http://collector:4318",
    }
})
```

Exporter 支持 `stdout`（dev）和 `otlp`（prod）。禁用时
`TracerProvider()` 返回 noop 实例——instrumentation 代码无需 nil
check。

---

## 11. 辅助子系统

### 11.1 apierr

统一错误模型。默认错误：

```go
apierr.ErrNotFound        // 404
apierr.ErrInvalidArgument // 400
apierr.ErrUnauthenticated // 401
apierr.ErrPermissionDenied// 403
apierr.ErrInternal        // 500
apierr.ErrConflict        // 409
apierr.ErrMapper(err) *Error  // 全局映射器（store.MapError 常注册到此）
```

### 11.2 auth

纯上下文 + 密码工具：

```go
auth.WithPrincipal(ctx, Principal) context.Context
auth.PrincipalFrom(ctx) (Principal, bool)
auth.HashPassword(plain) (hash, error)
auth.ComparePassword(hash, plain) error
```

### 11.3 auth/jwt

HS256 manager（无全局单例）：

```go
mgr, _ := jwt.NewManager(jwt.Options{
    SigningKey: "...",           // >= 32 bytes
    Issuer:     "myapp",
    Expiration: 2 * time.Hour,   // defaults to 2h
    Leeway:     30 * time.Second, // 默认 DefaultLeeway (30s)，容忍时钟偏斜
})
token, exp, _ := mgr.Sign(subject, map[string]any{"roles": [...]})
sub, claims, _ := mgr.Parse(token)
```

**时钟偏斜**：`Parse` 对 `iat` / `exp` / `nbf` 应用 `Leeway` 容差，
避免跨节点时钟不完全同步时把刚刚签发的 token 当成"未来时间"拒绝。
要严格校验时把 `Options.Leeway` 设为负值。

### 11.4 authz

单个接口 + 函数适配器：

```go
type Authorizer interface {
    Authorize(ctx, subject, object, action string) (bool, error)
}
```

### 11.5 account

现成的用户模块（注册 / 登录 / refresh / change / forgot / reset）。
路由挂载通过 `AccountComponent` 的 Router 实现，依赖 db + log。

### 11.6 scheduler

Cron job 运行器（robfig/cron/v3 包装）：

- panic-safe：每次 Run 带 recover + 堆栈日志
- 策略：`PolicyDelayIfRunning` / `PolicySkipIfRunning`
- 统计：RunCount / FailCount / AvgDurMs / LastErr
- `ErrBusy` 不计入失败

### 11.7 cache

三层缓存：memory(otter) + file(badger) + redis，按 `cache.Build`
组合。`cache.Chain` 支持任意顺序叠加。Chain.Get 对层级错误降级为
miss（不中止查找），天然容忍单层故障。

`cache.WithBreaker(c, BreakerOptions{})` 可包裹任意 Cache 后端（通常
是 Redis）添加熔断保护。连续失败 ≥ threshold 次后 circuit open（Get
返回 miss，Set/Delete 静默跳过），resetTimeout 后半开探活。
`cache.Build` 通过 `BuildOptions.Breaker` 字段自动装配。

### 11.8 rid

资源 ID 生成。前缀 + 随机后缀，总长 ≤ 23。

### 11.9 validate

wrap validator/v10；`validate.Func` 返回 plain error 自动包装为
`apierr.ErrInvalidArgument`。

### 11.10 swagger

自动生成 OpenAPI 3.0，从 `handler.HandleRequest[T, R]` 的泛型类型反射
获取 schema。注册路由的同时调用 `swagger.Post/Get/...` 把 op 写入
`Spec`。

---

## 12. 使用示例（examples/blog）

### 12.1 入口

```go
// cmd/blog/main.go
package main

import "github.com/zynthara/chok/examples/blog/internal/app"

func main() { app.NewApp().Execute() }
```

### 12.2 Config

```go
// internal/app/config.go
type Config struct {
    HTTP     config.HTTPOptions     `mapstructure:"http"`
    Log      config.SlogOptions     `mapstructure:"log"`
    Database config.DatabaseOptions `mapstructure:"database"`
    Account  config.AccountOptions  `mapstructure:"account"`
    Swagger  config.SwaggerOptions  `mapstructure:"swagger"`
}
```

### 12.3 yaml

```yaml
# configs/blog.yaml
http:
  addr: ":8080"
log:
  level: info
  format: json
  output: [stdout]
database:
  driver: sqlite
  sqlite:
    path: "blog.db"
account:
  enabled: true
  signing_key: "CHANGE-ME-generate-with-openssl-rand-base64-32"
swagger:
  enabled: true
  title: "Blog API"
```

### 12.4 Setup（auto-register 模式）

框架从 Config struct 的字段类型自动发现并注册 Component——用户只需
提供表定义和业务路由：

```go
var cfg Config

func NewApp() *chok.App {
    apierr.RegisterMapper(chokstore.MapError)
    return chok.New("blog",
        chok.WithConfig(&cfg),
        chok.WithTables(
            db.Table(&model.Post{},
                db.SoftUnique("uk_post_title_owner", "title", "owner_id")),
        ),
        chok.WithRoutes(func(ctx context.Context, a *chok.App) error {
            api := a.API("/api/v1", a.AuthMiddleware())
            gdb := a.DB().(*gorm.DB)
            posts := blogStore.NewPostStore(chokstore.New[model.Post](gdb, a.Logger()))
            handler.RegisterPostRoutes(api, posts)
            return nil
        }),
    )
}
```

**auto-register 做了什么**（用户零代码）：

| Config 字段类型 | 自动注册 |
|---|---|
| `config.HTTPOptions` | HTTPServer + Recovery/RequestID/Logger middleware |
| `config.DatabaseOptions` | DBComponent (driver 决定 SQLite/MySQL) + WithTables 的表 |
| `config.AccountOptions` | AccountComponent ("/auth")，`enabled: false` 时跳过 |
| `config.SwaggerOptions` | SwaggerComponent，`enabled: false` 时跳过 |
| （需 HTTP Server） | HealthComponent ("/healthz") + MetricsComponent ("/metrics") |

用户在 `WithSetup` 中显式 `Register("db")` 等会**优先**于 auto-register。

**WithRoutes 做了什么**（用户零 hook 代码）：

框架在 `EventAfterStart` 中自动编排三阶段 mount（只要存在
HTTP Server 就会触发，不依赖 `WithRoutes` 是否设置）：
1. 挂载所有非 swagger 的 Router Component
2. 执行 `WithRoutes` 回调（用户业务路由，可选）
3. 最后挂载 swagger（`Populate` 能看到所有路由）

### 12.5 Handler

```go
// internal/handler/post.go
func (h *postHandler) update(ctx context.Context, req *updatePostRequest) (*model.Post, error) {
    p, err := h.posts.Get(ctx, store.RID(req.RID))
    if err != nil {
        return nil, err
    }
    var cols []string
    if req.Title != nil {
        p.Title = *req.Title
        cols = append(cols, "title")
    }
    if req.Status != nil {
        p.Status = *req.Status
        cols = append(cols, "status")
    }
    if len(cols) == 0 {
        return p, nil
    }
    // Fields 自动从 p.Version 取乐观锁版本号
    if err := h.posts.Update(ctx, store.RID(p.RID), store.Fields(p, cols...)); err != nil {
        return nil, err
    }
    return p, nil
}
```

---

## 13. 扩展：写一个新 Component

三步：

1. **实现 Component interface**（必须）+ 可选能力
2. **接受 Resolver 或 Builder**，解耦具体 config
3. **暴露一个 accessor**（如 `MyComp.Client()`）

示例（假设做一个 Memcached 缓存后端）：

```go
// parts/memcached.go（或用户自己的包）

type MemcachedResolver func(any) *MemcachedSettings

type MemcachedSettings struct {
    Enabled bool
    Servers []string
}

type MemcachedComponent struct {
    resolve MemcachedResolver
    client  *memcache.Client
}

func NewMemcachedComponent(r MemcachedResolver) *MemcachedComponent {
    return &MemcachedComponent{resolve: r}
}

func (m *MemcachedComponent) Name() string      { return "memcached" }
func (m *MemcachedComponent) ConfigKey() string { return "memcached" }

func (m *MemcachedComponent) Init(ctx context.Context, k component.Kernel) error {
    s := m.resolve(k.ConfigSnapshot())
    if s == nil || !s.Enabled {
        return nil
    }
    m.client = memcache.New(s.Servers...)
    return nil
}

func (m *MemcachedComponent) Close(ctx context.Context) error {
    if m.client != nil {
        m.client.Close()
    }
    return nil
}

// 可选：Healther
func (m *MemcachedComponent) Health(ctx context.Context) component.HealthStatus {
    if m.client == nil {
        return component.HealthStatus{Status: component.HealthOK}
    }
    if err := m.client.Ping(); err != nil {
        return component.HealthStatus{Status: component.HealthDown, Error: err.Error()}
    }
    return component.HealthStatus{Status: component.HealthOK}
}

func (m *MemcachedComponent) Client() *memcache.Client { return m.client }
```

用户代码：

```go
a.Register(NewMemcachedComponent(func(cfg any) *MemcachedSettings {
    return &cfg.(*MyCfg).Memcached
}))

// 其它组件依赖它
type MyWorkerComponent struct{ ... }
func (w *MyWorkerComponent) Dependencies() []string { return []string{"memcached"} }
func (w *MyWorkerComponent) Init(ctx, k) error {
    mc := k.Get("memcached").(*MemcachedComponent).Client()
    ...
}
```

---

## 14. 技术选型

| 维度 | 依赖 | 备注 |
|---|---|---|
| HTTP | gin | hardcoded；`server.HTTPServer` 包装 |
| ORM | gorm | hardcoded；`db/store/where` 基于其上 |
| Config | viper | file + env + flag |
| Validation | validator/v10 | `validate.ValidateStruct` 统一入口 |
| Cron | robfig/cron/v3 | `scheduler` 包封装 |
| JWT | golang-jwt/jwt/v5 | `auth/jwt.Manager` 包装 |
| 内存缓存 | otter/v2 | `cache.NewMemory` |
| 文件缓存 | dgraph-io/badger/v4 | `cache.NewFile` |
| Redis | go-redis/v9 | `cache.NewRedis` + `redis.New` |
| 日志轮转 | natefinch/lumberjack | `log.NewSlog` 自动装配 |
| fsnotify | fsnotify/v1 | config 文件监听 |
| Metrics | prometheus/client_golang | MetricsComponent 核心 |
| Tracing | OpenTelemetry SDK | stdout + OTLP/HTTP exporter |

---

## 15. 项目结构

```
chok/
├── chok.go              App 生命周期
├── options.go           App 构造 Option
├── config.go            loadConfig + reflect defaults/env
├── signal.go            SIGHUP/SIGTERM/SIGQUIT 分派
├── watcher.go           fsnotify 文件监听
│
├── component/           Component / Kernel / Registry / Event
├── parts/               13 个内置 Component
├── store/               泛型 CRUD + Locator + Changes
│   └── where/           查询 DSL
├── config/              所有 *Options 结构
├── db/                  gorm 包装 + Model / Migrate
├── cache/               memory/file/redis 三层
├── redis/               go-redis 包装
├── auth/                Principal / password
│   └── jwt/             HS256 Manager
├── authz/               Authorizer interface
├── account/             用户模块（注册/登录/...）
├── scheduler/           cron 运行器
├── server/              gin HTTP Server（实现 chok.Server）
├── swagger/             OpenAPI 生成 + UI
├── handler/             HandleRequest[T,R] 泛型处理器
├── middleware/          Recovery/Auth/CORS/AccessLog/...
├── apierr/              统一错误类型
├── choktest/            测试辅助（NewTestDB / NewTestStore）
├── rid/                 资源 ID
├── log/                 Logger interface + slog 实现
├── validate/            validator/v10 包装
├── version/             Info 结构
│
├── cmd/chok/            脚手架 CLI
├── internal/ctxval/     context key 存储
├── examples/blog/       端到端 Component 样板
└── docs/                本文档
```

---

## 16. 关键不变量（一页速查）

- App 是 **single-use**：`Run` / `Execute` 调用一次后拒绝再次调用
- Register 必须在 registry 构造前（即 setupFn 内或之前），否则 panic
- Store 的 `Where` Locator 用于 Get/Update/Delete 时必须含 filter
- `Fields(&obj)` 不传字段 = 白名单全量；零值强制落库
- `RID(x)` 永远用于对外；`ID(x)` 仅用于内部 join / batch
- Component 强制接口仅 `Name` / `Init` / `Close`；`ConfigKey` 是可选接口
- Component Init 失败 → 已成功的反序并行 Close 回滚（`Optionaler` 除外）
- `Optionaler` 组件 Init 失败 → warn 日志 + 跳过，不中止启动
- Stop 按拓扑层级逆序，同层并行 Close；在 runServers 结束后、runCleanups 之前
- Store Before-hooks 可中止操作（返回 error）；After-hooks 是 fire-and-forget
- **ReloadConfig 保证原子性**：验证失败时 live config 不受影响
- Reload 失败不中断 App 运行（Run 继续等待）
- 所有 ctx 在 shutdown 时级联取消
- Reload 触发源：SIGHUP / fsnotify file-change / 手动 App.Reload()；SIGINT/SIGTERM 是唯一 shutdown 触发
- Logger / Cache 的 `App.XXX()` 与 Registry 里的 Component 是同一实例
- Health 探针并行执行，默认 3s per-probe 超时 + 硬 fan-in deadline
- `/readyz` 在 shutdown 触发时无条件标记 503（在 stopServers 之前）
- `/readyz` 在 Health OK 后追加 ReadyChecker 检查，warm-up 期间返回 503
- Optional 组件 Init 失败在 Health 报告中为 Degraded（非 Down）
- App 默认 Init 30s / Close 15s / Health 3s 超时，单组件可覆盖
- Config reload 在 `configMu.Lock` 保护下写入，防止并发读 torn read
- Store 错误携带结构化上下文（locator/version），`errors.Is` 向后兼容
- `db.RunInTx` 嵌套调用复用外层事务；Store 方法通过 `effectiveDB(ctx)` 自动参与 context 事务
- `middleware.Logger` 在 tracing 激活时自动注入 `trace_id` / `span_id` 到 per-request logger
- `middleware.Timeout(d)` 注入 context deadline；handler 同步执行；若返回后 deadline 已触发且未写响应则写 504

---

版本历史与每个发布的具体变更见 [`CHANGELOG.md`](../CHANGELOG.md)。
路线图见 [`docs/roadmap.md`](roadmap.md)。
