# chok v2 设计文档

> 本文是 chok v2 架构的**中文正文**（团队惯例：设计文档中文、代码
> 注释英文）。源代码是真相的最终来源；本文负责讲清「为什么」与
> 「契约边界」。公开契约变更时与代码同步更新。
>
> 标注「生成区块」的部分由 `chok docs gen` 从代码产出，手改无效
> （CI 的 `docs gen --check` 拦截漂移）；其余为手写正文。
>
> v1 的设计文档随 `v0.1.4` 封版，可在该 tag 的仓库快照中查阅。

---

## 1. 定位

chok 是一个 Go Web 框架，模块路径 `github.com/zynthara/chok/v2`。
三个不可变形容词：

- **全家桶**：HTTP + 数据层 + 缓存 + 认证 + 授权（RBAC）+ 定时任务
  + 可观测性在同一个仓库内，不把能力外推给用户自组装。
- **配置驱动**：`chok.yaml` 既声明装配哪些模块，也配置它们；
  `enabled: true/false` 是运行期启停的主开关。
- **单一官方实现**：每个能力只给一个成熟方案——HTTP 是 stdlib
  `ServeMux`（Go 1.22+ 模式路由）、ORM 是 gorm（藏在 store 之后）、
  缓存是 otter + redis、cron 是 robfig、JWT 是 golang-jwt、RBAC 是
  casbin、观测是 Prometheus + OpenTelemetry。不提供平行选择。

## 2. 设计公理

继承 v1 三条，v2 新增两条：

1. **Config is the only knob** —— 用户 90% 的需求靠 yaml 解决。
2. **One blessed implementation** —— 接口留扩展点，不做多后端。
3. **Internally complex, externally trivial** —— 复杂度归框架。
4. **Invariants live in types, not docs** —— 需要写进「陷阱清单」
   的约束，就是还没设计完的约束。v1 靠文档与 review 维持的十几条
   不变量，v2 翻译成类型系统与单线程控制面的结构性保证（§12）。
5. **One source of truth** —— 组件清单、依赖图、配置参考只存在于
   `Descriptor` 与 Options 类型里；文档表格、`/componentz`、JSON
   Schema 一律生成。

## 3. 装配模型

一个 v2 应用的全部接线：

```go
func main() {
    chok.New("blog",
        chokModules(),                    // chok_modules_gen.go：chok sync 从 chok.yaml 生成
        chok.WithErrorMapper(store.MapError),
        chok.Override(db.Module(          // 定制 = 显式替换同键模块
            db.WithTables(db.Table(&Post{})),
        )),
        chok.Routes(routes),              // 业务路由回调
    ).Execute()
}
```

- **`chok.Use(...)` 是唯一注册面**。每个子系统包导出
  `Module(...) kernel.Component`；`Use` 内同 `(kind, instance)` 键
  重复 ⇒ 启动失败（fail-fast）；有意替换写 `chok.Override(c)`，
  替换不存在的键同样报错（防 typo）。
- **yaml 段在场 = 链接期意图；`enabled` = 运行期开关**。`chok sync`
  读 chok.yaml 生成 `chok_modules_gen.go`：段在场的模块进 Use 清单
  （含 `enabled: false` 的——「注册-禁用」模型，见下），段缺席的
  模块不 import，Go 链接器自然裁剪二进制。手写 Use 清单同样成立，
  sync 纯属可选。
- **disabled 的四条语义**（注册-禁用模型）：
  1. 硬依赖指向 disabled 组件 ⇒ 启动失败，错误信息点名
     「X requires Y which is disabled」；软依赖视为缺席，正常降级。
  2. `chok.Get[T](k, kind)` 对 disabled 实例返回 `(zero, false)`——
     拿不到半初始化对象。
  3. disabled 组件出现在 Health 与 `/componentz` 中，状态
     `disabled`（信息态，聚合视为 OK）——可诊断，不凭空消失。
  4. `enabled` 恒为 restart-only（框架级规则，模块自身的 reload
     tag 不可覆盖）；disabled 组件不参与任何生命周期 phase。
- **业务配置**走 `chok.Section[T](app, "myapp")`——`Run` 前注册、
  返回类型化句柄，`handle.Get()` 每次解码当前快照。框架不再需要
  用户巨型 Config 结构体。
- **多实例**按 `(kind, instance)` 二元组注册：yaml 用段内保留子键
  `instances.<name>`（如 `db.instances.read`），代码侧
  `db.Module(db.As("read"))`，访问 `db.From(k, "read")`。实例配置
  `read_only: true` 会移除事务、迁移和写能力，而非只靠命名约定。

## 4. 内核（`kernel/`）

### 4.1 单 actor 控制面

```
              commands（有缓冲 chan）
  Start ──┐
  Stop  ──┼──▶ 控制 goroutine（唯一写者）──▶ 原子发布 view 快照
  Reload ─┘        │                            │
                   │ phase 序：拓扑排序 → 分层并行 │  Get / Health / Ready
                   │ Init → 串行 Migrate → mount │  读快照，无锁
                   │ → serve → draining → Close  │
```

- **所有生命周期变更**（start / stop / reload / 失败回滚）由单一
  控制 goroutine 串行处理——不存在可写错的锁序。
- **读路径不进 actor**：组件集合经 `atomic.Pointer[view]` 发布，
  `Get` / `Health` / `Ready` 读快照。Stop 按层发布收缩后的 view，
  所以「Close 里拿 peer」结构上不可能成功——这不是守则，是事实。
- **reload 合并**：控制循环处理 reload 期间到达的后续 reload 直接
  回执 `ErrReloadInProgress`（CAS 门），不排队。
- 执行语义：Kahn 拓扑排序、同层并行 Init（per-component 超时）、
  Migrate 按层串行、必需组件失败反序回滚、`Optional` 组件失败降级
  （warn + Degraded）、逆拓扑同层并行 Close、错误 joined。
- **draining** 是 serve 与 close 之间的固定 phase：对全部 `Drainer`
  广播（health 借此翻 `/readyz` 503）→ 等 `DrainDelay` → 取消各
  `Serve` 并等其返回 → 逆序 Close。长任务组件（scheduler）的
  in-flight 收尾发生在 `Serve` 返回前，因此 drain 期间其依赖（db
  等）全部存活。

### 4.2 组件契约：Descriptor + 行为接口

```go
type Component interface {
    Describe() Descriptor
    Init(ctx context.Context, k Kernel) error
    Close(ctx context.Context) error
}
```

`Descriptor` 是纯数据声明：`Kind` / `Instance` / `ConfigKey` /
`Options`（零值样本，conf 据此重建 default/env/validate）/
`Needs []Dep`（软依赖 `Optional: true`）/ `Timeouts` / `Optional` /
`MountOrder`。

可选能力是**行为接口**，Registry 以类型断言发现：

| 接口 | 语义 |
|---|---|
| `Reloader` | 配置段热变更派发（见 §5 reload tag） |
| `Healther` | `Health(ctx) error`——正常/故障二值 + disabled 信息态 |
| `Mounter` | `Mount(r kernel.Router) error`，mount phase 挂路由 |
| `Migrator` | Init 后、serve 前的 schema 步骤 |
| `Readier` | `/readyz` 参与方 |
| `Server` | `Serve(ctx, ready)`——长驻循环（http、scheduler） |
| `Drainer` | draining phase 广播（health 摘流量） |
| `RouterProvider` | 路由器提供方（角色型，恒单一；web.Module 实现） |

- mount 顺序：`MountOrder ≤ 0` 的 Mounter → 用户 `Routes` 回调 →
  `MountOrder > 0` 升序（swagger 自声明 100，故它能看见全部路由）
  ——kernel 不认识任何电池名。
- 存在 Mounter / Routes 而无 RouterProvider ⇒ 启动失败（门禁）；
  出现两个 RouterProvider 同样 fail-fast。

### 4.3 事件总线（层二，异步）

生命周期 phase（层一，同步、可 veto——组件 `Init` 返回错误即中止
启动）与业务事件总线（层二）严格分离。总线是类型化 pub/sub：

```go
event.Publish[T](ctx, bus, ev)
event.Subscribe[T](bus, fn, opts...) (cancel func())
```

订阅者默认异步 + 每订阅者有界队列（默认 64），溢出策略
`Block | DropOldest`（默认 DropOldest + 限速 warn；Publish 永不
反压生命周期）；订阅者 panic recover 后订阅存活。生命周期事件
（`ComponentInitialized` / `ReloadApplied` / ...）发布到总线，
metrics、`/componentz` 从这里取数。关闭顺序：组件逆序 Close →
发布最终事件 → bus drain（5s 预算）→ 根 logger 最后关。

根 logger 的 close-last 由**所有权**表达：App 在装配期构建 logger
（先于控制面）、控制面完全停止后由 App flush/close——组件 Close
期间日志始终可用；`WithLogger` 注入的实例归调用方关。

## 5. 配置引擎（`conf/`）

- **装载栈**：viper（file + env + flag），优先级 flag > env > file >
  default；路径解析 `WithConfigFile` 显式 > `{PREFIX}_CONFIG` env >
  `./{name}.yaml` > `./configs/{name}.yaml`。装载结果是不可变树。
- **RCU**：`atomic.Pointer[Snapshot]`。Reload = 全新构建 → 逐段
  decode → 递归 Validate → 原子交换 → 按段 diff 派发。失败零污染
  是结构性的：没交换就没发生。用户拿不到 live 指针。
- **类型注册**：框架段来自 `Descriptor.Options` 零值样本；业务段
  来自 `chok.Section[T]`（Run 前）。据此重建三件事：逐叶
  `BindEnv`、`default` tag 注册、启动期递归 `Validate`。动态 map
  键（`account.providers.*`）走 imperative env 覆盖 pass。
- **校验协议**：Options 实现 `conf.Validatable`；discriminator 类型
  （driver 选分支）另实现 `conf.SelfValidating`——递归走查器到此
  停降，未选分支不被误校验。
- **reload 语义在类型层**：

```go
type Options struct {
    Level  string   `mapstructure:"level"  reload:"hot"`
    Output []string `mapstructure:"output" reload:"restart"`
}
```

  hot 字段变更 ⇒ 调该组件 `Reload`；restart 字段变更 ⇒ 框架统一
  warn（组件不再手写）；无 tag 默认 restart（保守）；嵌套字段继承
  外层 tag、可覆盖；map/slice 整体 DeepEqual；`enabled` 恒 restart
  且置 `EnabledFlipped`。`ConfigKey == ""` 的 Reloader 在每次配置
  变更时都被派发、排在全部有段组件之后——无派发死角。
- **敏感字段**：`sensitive:"true"` tag 驱动 `Redact` /
  `RedactedSettings`（含 provider map 的键启发式：`*secret*` /
  `*password*` / `private_key` / ...）；`GoString/String` 掩码有
  断言测试。生成的 JSON Schema 同步标 `writeOnly` 且不带默认值。

## 6. HTTP 层（`web/` + `middleware/` + `handler/`）

- **契约在 kernel、实现在 web、应用拼写在 chok**：`kernel.Router`
  （`Handle` / `Group`，`Middleware = func(http.Handler) http.Handler`）
  只依赖 net/http；web 提供 ServeMux 实现并保有**路由表**。根包
  `chok` 对出现在自身签名里的契约做类型别名（`chok.Router` /
  `chok.Kernel` / `chok.Middleware` / `chok.Component`），应用代码
  只写 chok 词汇、不 import kernel；模块作者（Descriptor / Mounter
  等扩展面）保持 kernel 拼写。别名是恒等类型，两种拼写可互换。
- **路由表是 swagger 的数据源**：泛型 handler 构造产物实现
  `web.HandlerMeta`（`Meta()` 携带 req/resp 类型、summary、tags、
  success code），`Handle` 注册时断言入表；swagger 从表生成
  OpenAPI 3 spec（`/swagger/doc.json` + UI）。v1 的 unsafe 闭包
  地址注册表不复存在。
- **中间件栈**（web.Module Init 构建）：RequestID → ClientIP
  （XFF + 可信代理链校验，登录限速的输入，安全敏感项有伪造回归
  测试）→ 访问日志（专用轮转文件或根 logger）→ RED metrics →
  tracing span → Recovery → Timeout（written-tracking，deadline
  未写响应补 504）→ AttachAuthz（软依赖，缺席不挂不报错）。
  未匹配请求走完整栈（标签 `unmatched`）；404/405 出 apierr
  envelope。
- **handler 绑定层**：`HandleRequest[T,R]` / `HandleAction[T]` /
  `HandleList[T]` 返回 `http.Handler`；uri/query/json 三绑定器 +
  validator/v10（`binding` tag）；错误经 apierr + per-App
  `MapperRegistry`（`chok.WithErrorMapper` 注入，装配期结构化握手
  交给 web 模块）渲染统一 envelope。
- **对 v1 的声明变更**（迁移指南详列）：trailing-slash / fixed-path
  不再纠错（ServeMux 语义）；路由标签 `:rid` → `{rid}`；span 命名
  时机后移。`shutdown_timeout`（默认 10s，Shutdown 预算超时后
  force-Close）与 `h2c` 是 v2 新增。

## 7. 数据层（`db/` + `store/`）

### 7.1 薄句柄与两扇 Unsafe 门

公开面不出现 `*gorm.DB`。`*db.DB` 是 chok 自有薄句柄：
`store.New` 的入参、`RunInTx` / `Migrate` / `Ping` / `Close` 的
载体。取用路径：`db.From(k, instance...)`（blessed 便捷，缺席即
panic——装配错误应 fail-fast）或 `chok.Get[*db.Component]` 双值
形式。

raw SQL 恰有两扇门，名字即警示（M5 §5.2 复评定案）：

| 门 | 语义 | 边界 |
|---|---|---|
| `Store.Unsafe(ctx)` | tx-aware + **scope 已应用** + scope 失败 fail-closed | store DSL 表达不了、但租户/属主语义必须保持的 SQL |
| `(*db.DB).Unsafe(ctx)` | tx-aware + 无 scope | 基建层：外形表 AutoMigrate、事务内行锁 |

`db.InTx(ctx) bool` 提供无句柄的事务内省；事务上下文的 gorm 载体与
所属 `*db.DB` 身份在 `internal/txctx`，用户代码拿不到。只有同一句柄
的 store/Unsafe 会加入该事务，跨实例 ctx 不会偷换连接池。

### 7.2 事务模型

`db.RunInTx(ctx, h, fn)` / `h.RunInTx` / `Store.Tx`——context 传播
是唯一模型：fn 收到 txCtx，同句柄 store 操作带 txCtx 即自动加入事务，
同句柄嵌套复用最外层。`db.AfterCommit(ctx, fn)` 把回调暂存到事务上下文，
COMMIT 后按序执行、回滚整体丢弃——`store.WithBus` 的实体事件锚定
提交，杜绝幻影事件。

### 7.3 store 安全栏（v1 王牌，原样继承）

Query/Update 字段白名单（生产必须显式 `WithQueryFields` /
`WithUpdateFields`，自动发现仅 dev 且带 warn）、
`ErrMissingConditions` 防清表、scope 传播进 Preload（失败
fail-closed `1=0`）、owned 模型自动 OwnerScope（未认证 401、写侧
owner 强制覆盖）、`Fields` 乐观锁 + 零值强制落库、scope 化 store
禁 Upsert、RID 双 ID 模型（外部 `pst_xxx`，数字主键不出进程）。
`Fields` 只接受 Store 的具体模型类型（`T` / `*T`），不把形状兼容 DTO
交给 GORM 猜测字段与锁元数据。
字段到列的映射以 GORM parser 的 `Field.DBName` 为唯一事实源，不另写
snake_case；显式 update 列表与 alias 也不能重开 id/RID/version/时间戳/
软删状态/owner 等框架托管列，执行内核再做第二道检查。`version` 是行修订号：
普通 Update（含 `Set`/`NoLock`）、软删与 Restore 的成功 SQL 都推进它；
是否带旧 version 条件只决定冲突检测，不决定修订号是否增长。Upsert 的
conflict-update 不增 version 仍是单独、已文档化的方言能力限制。

`where.Option` 保留为已公开的可信代码扩展点，但它直接获得 `*gorm.DB` 与
可写 Config，信任级等同 `Store.Unsafe`，不得由请求动态构造；常用空值谓词
由 `WithFilterNull` / `WithFilterNotNull` 内建覆盖。**gorm 是永久基底**
（决策 2026-07-16）：公开面隐藏 gorm（薄句柄、`Unsafe` 命名）的目的是
**收窄误用面，不是后端可替换性**——`ScopeFunc(ctx, *gorm.DB)` 与
`where.Option(*gorm.DB, ...)` 两个可信扩展点有意暴露 gorm 类型并长期保持；
替换 ORM 不是目标（单一钦定实现公理的一部分），不为假想的可替换性支付
收口扩展点的 API 代价。分页 cap 按所有策略层的
最小值合成，调用点只能收紧 Store/handle 上限，且不超过包级 10k ceiling。
`EntityChanged` 不再携带包外不可解释的 Locator/Changes 接口引用：事件暴露
`LocatorSnapshot`（RID/ID/Where 类型）与 `ChangeSnapshot`（public field →
值），Create 对象和 Update 值递归拷贝，访问器再返回拷贝；Where 事件按类型级
失效。事务提交锚定语义不变。

批量写分成三种明确语义:`BatchCreate` 是分片 insert;`BatchUpdate` 是
事务内逐行 `Fields` 更新（每行 payload 不同，失败恢复框架递增的内存
Version）;`BatchUpsert` 是分片 conflict-update，要求批内 conflict key
唯一。所有静态白名单/空指针/重复键校验先于 hooks——单行 `Update` 同样
先 build 后 hooks；before-update 钩子统一收到已解析的 `ChangeSnapshot`
（公开字段名→值，递归拷贝访问器），可内省、不可改写。Upsert 无法跨三方言
可靠返回 conflict 分支的真实持久化对象，事件统一为无 payload 的
`OpUpsert`，订阅者按实体类型失效；`BatchUpsert` 每次调用只发一条，避免
N 个无身份 payload 触发 N 次相同失效。禁止把 create hook 生成的新 RID
当成已存身份。接口视图按**单行/批量**划线：`Writer[T]` 只含单行写
（Create/Update/Upsert/Delete），批量三件套在独立 `BatchWriter[T]`——
批量面再扩张也不触碰下游 `Writer` mock 的方法集。

**错误语义两条契约**（2026-07-17）：① provenance 划界（Breaking）——
`where.ErrUnknownField` 在程序化读入口（List/Count/Pluck*/ListIn/游标
字段/locator）**原样返回**：字段名是服务端代码写的，typo 是编程 bug，
500 惊动监控而非伪装客户端错误；仅 `ListFromQuery` 链（字段名来自 URL）
整链预映射 400。值错误 `where.ErrInvalidParam` 不分入口 400（page/size/
令牌/过滤值合法地来自客户端）。公开约定：客户端**字段名**走
`ListFromQuery` 或先校验，不得直拼 `WithFilter`/`WithOrder`。
② 重复键归因——`WithConstraintFields` 声明约束 → 公开字段映射后，命中
的 `ErrDuplicate` 报 metadata `field` 而非泄 schema 命名、随迁移漂移的
索引名（Ecto `unique_constraint` 思路）；未声明保持 `constraint` 现状。
映射骑在错误值上（`DuplicateEntryError.Field`）——app 级 `MapError`
不知来源 store；匹配按方言归一化（PG 裸名 / MySQL 8 `table.key` 剥表名 /
SQLite 只报列清单、SoftUnique 含 delete_token、glebarez 尾缀码剥除）。

**安全策略应用级默认（`db.store` 块）**：strict /
require_principal / admin_roles / 分页上限不再依赖每个构造点记得写
`WithStrict()`——`db.store`（及 `db.instances.<name>.store`）随
`*db.DB` 句柄下发，`store.New` 对构造点未显式设置的开关取策略值；
显式选项永远胜出，局部逃逸必须写成看得见的
`WithoutStrict()` / `WithoutRequirePrincipal()`（review 要求：
opt-out 是刻意的调用点噪音）。策略在构造期烙进 Store，故与 db
其余字段一样 restart-only。bus/logger 仍显式注入——SPEC §3.5 的
"store 对 kernel 仅一个显式接触点"不因此松动；策略是纯数据，
不构成 kernel 依赖。电池 store 全部显式声明字段面，开启 strict
不影响电池。

admin_roles 单独一提（arch-review 修复）：一份名单同时驱动
**读侧**自动 OwnerScope 的旁路和**写侧** owner 填充的显式指定权，
解析顺序 `store.WithAdminRoles`（替换语义，零参数 = 全关）→
`db.store.admin_roles` → 包默认 `["admin"]`，构造期定格。调整角色
**不要**再叠 `WithScope(OwnerScope(...))`——scope 按 AND 组合，第二个
OwnerScope 得到旁路交集而非覆盖，这正是旧文档引导过的误配。

**投影与跨表读的钦定形态**（下游遥测驱动的补强）：

- `store.Pluck[F]` / `PluckDistinct[F]` 单列投影——列名走查询白名单
  （拿不到 `password_hash` 这类未声明列），scope / 软删规则全程生效；
  `"id"` 字段沿用 id→rid 常设别名产出公共 RID。
- `store.PluckIDs`（`ListByIDs` 的逆操作）投影内部数字主键，服务端
  专用。**两步 IN 是跨表读的钦定模式**：
  `PluckIDs(父, 条件)` → `子.List(where.WithFilterIn("fk", ids))`——
  两侧白名单都在场；超过 `where.MaxInList`（500）用 `store.ListIn`
  自动分块（锚定单条大 IN 的集合语义：值集 Go 等值去重 + 跨块结果按
  主键去重——CI collation 下数据库等值比 Go 宽；空值集仍走一次退化
  查询，白名单/守卫/fail-closed scope 照常；FILTERS-ONLY，绕开页
  大小 cap；多块非单语句，跨块快照一致性要求**事务级快照**隔离——
  事务走默认隔离，SQLite/InnoDB 满足、PG 默认 READ COMMITTED 不满足，
  按方言诚实声明，见 db.md §5.5）。
- `store.WithRowsAffected(&n)` 同时实现 UpdateOption/DeleteOption，
  观测 Where locator 批量写的命中行数；纯观测，不改变语义。
- **聚合正门**（arch-backlog #7）：`store.Sum[N]` / `Avg` / `Min[N]` /
  `Max[N]` / `CountDistinct` / `GroupBy[K]`——仪表盘统计不再下 Unsafe。
  读语义 = Count：白名单（字段 typo 原样 `ErrUnknownField` → 500）、
  fail-closed scope、软删排除、filter 收窄、分页/排序按 Count 先例剥离
  （GroupBy 例外：**拒收**非 filter 选项——行集结果上静默剥离
  `WithOrder`+`WithLimit` 会伪装成 top-N）。列 kind 复用游标的 schema
  wire-kind 探针：Sum/Avg 仅数值列，Min/Max 放开时间列（序运算），
  字符串/布尔不可聚合（collation 序不跨方言），CountDistinct 限可比较
  标量（json 声明列即拒——PG json 无等值运算符；字符串基数按列
  collation）。时间聚合**按瞬间比较**：SQLite 的 writer-zone 文本在
  聚合读取处经框架自产 strftime 表达式归一 UTC/毫秒（混合偏移不再
  选错极值/裂组；数值存量按 Unix 秒读，不用会误读 1970 年初的
  'auto'；仅聚合，存储与 filter/排序/游标不动）；MySQL DATETIME 存
  进程时区墙钟、不转 UTC——部署不变量：**同库所有写入方同一「固定
  无 DST」时区，推荐 TZ=UTC**（同时区必要非充分：DST 回拨把两个瞬间
  折成同一墙钟，单进程也发生；约束排序/过滤/游标全部时间比较面，
  聚合只是继承），折叠/跨时区存量读取侧不可修复；PG timestamptz 无
  此约束。能力矩阵两半：wire kind 管 Go 收敛，**数据库真实列型**管
  操作合法性——真实列型读自 catalog（`Migrator.ColumnTypes`，首次
  聚合懒解析+缓存；不是 `FullDataTypeOf` 渲染的模型 DDL 型，
  `versioned/off` 下真列可能与模型不符），按方言**精确白名单**匹配
  （不用子串——否则 PG 的 `interval`/`int4range` 混入整数族、
  `daterange` 混入时间族、`time`/`timetz` 当瞬间、`integer[]` 数组
  当整数）。真 `TEXT` 列上的 int64 拒收，range/interval/数组/纯时刻
  及不认识列型 fail-closed 指 Unsafe；JSON 门禁查逻辑 DataType 与
  catalog（json/jsonb，GormDBDataType 不漏）。懒解析（而非构造期）
  因构造可能先于迁移，且 catalog 只在迁移后才反映真相；`ColumnTypes`
  查 catalog 不改共享 schema（无 round-4 的 FullDataTypeOf 原地写
  Precision 竞态），一次解析、互斥缓存。catalog 列名在 SQLite/MySQL
  按大小写不敏感匹配（`versioned/off` 建的大写列照认）、PG 保留
  quoted 语义；字符串族含 char/nchar/enum。类型收敛是显式契约：
  `Sum[int64]` 精确、越 int64 值域响亮报错；`Avg` 恒 float64；SQL NULL
  （零行/全 NULL）→ comma-ok 的 `ok=false`，GroupBy 值走
  `AggValue.IsNull`，NULL group key 报错不折叠。`GroupBy` 恒按 group
  key 升序，`K` 与列 wire kind 精确匹配。自由函数（类型参数），不进
  `Reader`。
- **游标分页钦定形态**：`ListWithCursor` = 复合 keyset `(field, rid)` +
  不透明令牌（base64url(JSON)，绑定格式版本/字段/方向；值带类型标签保真，
  Kind 期望由**零值行跑编码器完整管线**推导（`Field.ValueOf` 含 serializer
	  包装 + Valuer 解析——`datatypes.Time`、`serializer:unixtime` 这类 wire ≠
	  Go 底层类型的字段按 wire 定 Kind），令牌可伪造，类型事实源永远是 schema，
	  解码按字段位宽做值域校验并拒绝 NaN（编码端绝不签发解码器不认、或解码后
	  变形的令牌）；Kind 静态推不出的字段入口即拒，serializer/Valuer
	  还须保证所有值的 wire Kind 稳定；每个实际边界在签发前按 schema pin 复验
	  Kind / 位宽 / 值域，动态类型漂移、NaN、无效 UTF-8 字符串（JSON 静默替换
	  U+FFFD）、RFC3339 不可表示时间均拒签；
	  tie-breaker 直接绑定模型 RID 列，不依赖 `id` 是否进白名单，数字主键不进任何
	  客户端可见令牌；filter 不绑定，跨页稳定是调用方契约；lookahead 确认有下一
	  页时 NULL 边界值报错，绝不静默截断；尺寸纪律是公开契约——客户端令牌
	  ≤4KB（`MaxCursorTokenLen`，解码前拒收 400），边界值 repr ≤1KB
	  （`MaxCursorValueLen`）且组装后令牌 ≤4KB（JSON 转义可膨胀），超限
	  在签发侧作为服务端错拒签，绝不签出自己拒收的令牌）。DSL 级 `WithCursor`（单列，等值边界会跳行）/
  `WithCursorBy`（内部 id tie）/`WithCursorByField`（tie 列直连 +
  identifier 校验）保留为可信服务端底层。
- **分页信封同源**：`where.Config` 产出 `PageInfo`（生效
  page/size/offset，钳制时 offset 按生效 size 重算，保持三者自洽）；
  `Page[T].Meta` 携带它（`List` 与 `ListFromQuery` 同返 `*Page[T]`；
  类型本体在 `where`，`store.Page` 是别名，handler 因此不 import
  store），`handler.HandleList` 只渲染不重新解析请求——信封与 SQL
  LIMIT/OFFSET 出自同一份 Config，结构上无法漂移。`ListFromQuery`
  是 `*Store` 上的 HTTP 糖，刻意不进 `Reader` 契约——解析传输层
  输入属于边缘（handler），数据接口保持 transport-free。
- **悲观锁最小面**：`GetForUpdate` 是唯一入口，按可验证性设计——
  强制本句柄事务（`Store.Tx` 的克隆或 `RunInTx` 的 `txCtx`，
  autocommit 直接 `ErrLockRequiresTx`）、只读 store 拒绝、
  `WithPreload` 拒绝（关联查询在锁外）。SQLite 语义不依赖驱动渲染
  （glebarez 会静默丢弃 `FOR UPDATE`）而依赖既有形态：事务独占唯一
  写连接（文件库单连接写池、默认 `_txlock=immediate`；内存库整库
  单连接），并发写者不存在 ⊇ 行锁，三方言可观测保证一致。
  `SKIP LOCKED` / `NOWAIT` 未提供，按需求再议。
- **刻意不做**：JOIN DSL（单表 store 的边界；跨表读走两步 IN）、
  表达式 ORDER BY（无法白名单化）、HAVING 与按聚合值排序的 top-N
  下推（两者都是聚合结果上的表达式，表达式 ORDER BY 的同类——GroupBy
  结果集按分组列 distinct 值定大小，仪表盘量级在内存排序/过滤；真到
  百万组的下推需求再议**序数 ORDER BY**：按框架自产 select 位置排序，
  不引入调用方表达式）、多列 GROUP BY（结果形状没有 codegen 无法
  类型化，backlog #4 后再议）——这些是 `Unsafe` 舱口的正当用途，
  逃逸应当稀少而非为零。

### 7.4 迁移双轨 + 框架表 ownership

```yaml
db:
  driver: postgres        # sqlite | mysql | postgres（day-one）
  migrate: versioned      # auto（dev 默认）| versioned | off
```

- versioned：embedded `migrations/NNNN_name.sql`（forward-only、
  `schema_migrations` 审计账本、跨进程迁移锁三分支：PG advisory /
  MySQL GET_LOCK / SQLite ledger lease）；`chok migrate
  create|up|status|repair`。
- auto：在第一条 DDL 前解析 GORM schema 并完成全部 `TableSpec` 的静态
  校验，再按声明顺序 AutoMigrate；后置声明错误不会留下可避免的前缀迁移。
- PG/MySQL session lock 的释放使用保留父 context value 的独立 5 秒 deadline；
  unlock 报错、返回未持锁或超时时将物理连接标为 bad connection 后关闭，
  不依赖 `sql.Conn.Close`（它只归还连接池）释放 session lock。SQLite ledger
  lease 使用同一 cleanup timeout 模式。
- **迁移审计状态机**：文件按 CRLF→LF 归一化后记录 SHA-256；任何 SQL
  执行前先提交 `dirty=true` 行与临时 version-zero fence，旧二进制在
  dirty 期间也会 fail-closed。PG/SQLite 把 SQL 与 clean 转换放在同一
  事务；MySQL 的隐式 DDL 提交由 dirty 行保留人工判定点。status 只读并
  如实分类 pending / dirty / drift / missing / unverified /
  out-of-order / name-drift / fenced，`status --check` 任一非净态退出 1。
- repair 必须指定单个版本、已观察到的 checksum 与 reason，并显式选择
  `retry`（已恢复迁移前状态）、`mark-applied`（已确认全部效果存在）或
  `accept-drift`（接受当前文件基线）；不自动猜测 MySQL 的部分提交状态。
  旧三列账本第一次 `up` 采用 trust-on-first-use 建立 checksum 基线，
  审计保证从该时刻开始，无法追溯基线建立前的历史改写。
- **框架表 ownership**：每个内建组件通过 `Descriptor.Schema` 声明自己
  负责演进的表，`chok docs gen` 聚合并生成 `db.FrameworkTables()` 的
  字母序目录。该目录描述所有内建组件的潜在 ownership，与本次装配及
  所选 named DB instance 无关，不代表目录中每张表都存在于当前数据库。
  versioned 下 account/audit/authz 各自携带三方言迁移集与独立账本
  `schema_migrations_chok_<kind>`，不占应用迁移序号。存量 AutoMigrate
  schema 只有在完整 catalog 指纹（表/列/默认值/索引定义/约束/方言）与
  该电池声明的等价版本完全一致时才自动采纳；部分表、旧形状或应用同名表
  一律 fail-closed。账本持久化 dialect，跨方言恢复不能用普通
  `accept-drift` 静默重基线。
  `chok migrate up --component account` / `--all-owned` 允许部署期迁移 job
  先行，业务账号无需 DDL 权限；status/repair 均展示 sequence、ledger、
  dialect。引入账本的过渡发布不改变表形状；未来不兼容 DDL 必须在迁移前
  排空旧副本或采用 expand/contract，并形成不可回滚边界——Missing 检查
  只会阻止迁移后再次启动的旧二进制，不能保护仍在运行的旧进程。
- **电池形状变更闸门**：迁移文件前缀只代表 versioned schema frontier，不
  等同于历史二进制。每个改变电池表形状的 PR 必须通过 fresh、严格 N-1
  前缀升级、旧 auto 基线采纳三路径；后两条的 catalog 指纹与 DML 行为轨迹
  均须收敛到 fresh。auto 路径由电池自有 fixture 复现旧数据及旧运行时回填，
  并在 apply 前证明没有序列账本或 manifest claim。`EquivalentVersion` 在
  首个超出 AutoMigrate 等价面的形状迁移前永久冻结。
- **第三方序列 manifest**：`OwnedSequence` 以完整组件包路径声明 owner，kind
  派生唯一账本；`account`/`audit`/`authz` 绑定各自保留 owner，`manifest`
  永久禁作 kind。每库的 `schema_migrations_chok_manifest` 持久化
  kind→owner、engine floor 与信息性组件/chok 版本。写入防护分四层：构造期
  保留映射 → 单 db.Component 进程内 descriptor/bytes 注册 → 数据库 claim →
  既有 checksum/结构/fence。授权位于迁移锁内并覆盖 apply、repair、claim
  transfer；存量账本只读预检通过后、任何后续账本元数据/schema 写之前持久化
  owner。
  PG advisory 与 MySQL GET_LOCK 当前使用固定全局键；SQLite 账本 lease 按 kind
  分散，因此共享 manifest 的 additive DDL 另用短 `BEGIN IMMEDIATE` 事务串行。
  status 将 manifest 与账本前缀扫描合并，第三方 SQL 不在通用 CLI 中时明确
  标为 unverified。claim 强制力从全部写入方 manifest-aware 起完整成立；旧
  二进制混跑期间仍只能依靠 checksum/结构/fence。
  `migrate: off` ⇒ 框架零 DDL（casbin 缺表则 LoadPolicy 启动失败）。
- **repair 留痕**：每次 repair（账本三动作 + claim 转移）在**自己的事务里**
  向 `schema_migrations_chok_repairs` 追加完整证据行，写不进历史即整体
  失败；PostgreSQL/MySQL 的 fence 清理并入同一事务，SQLite 的 lease 释放
  维持提交后 best-effort。历史行语义 = 业务状态 CAS 已提交，不承诺调用方
  观察到成功。reason 全路径必填；operator 显式传入必须过校验、自动推导
  失败降级空串（写入端永不产出读取端拒绝的值）。列契约两级：core（建表
  即有、缺失即 corrupt——`reason`/`repaired_at` 无默认值故只能生在
  CREATE）与 additive（未来列必须可 DEFAULT/nullable，读取用 fallback）。
  `app`/`repairs` 与 `manifest` 同列永久保留 kind；`OwnedSequence` 与所有
  kind 派生路径统一经 `ValidateSequenceKind`。防篡改边界 = 授权纪律 +
  外部审计管道，非密码学。
- `SoftUnique` 在 PG 用 partial unique index（`WHERE deleted_at IS
  NULL`），mysql/sqlite 用 `(cols..., delete_token)` 复合唯一——
  可观测行为等价。
- **Reload 不触发 Migrate**；schema 变更须重启（结构保证）。
- **只读实例**：`read_only: true` 将默认的 `migrate: auto` 降为 off；显式
  `migrate: versioned` 配置期拒绝，要求操作者明确改为 off；
  `RunInTx` / `Migrate` / store 写返回 `db.ErrReadOnly`，raw GORM 写由最前置
  callback 拒绝。SQLite 使用 `mode=ro`，PG/MySQL 设置 session 只读默认；
  数据库只读账号/物理副本仍是防恶意绕过的最终权限边界。account/audit/
  authz 依赖运行期写表，绑定只读默认实例时 Init fail-fast。

### 7.5 SQLite 单机生产形态

Web 服务常驻单进程、独占数据库文件，恰好取消了 SQLite 并发难点的
前提——进程自己充当缺失的"协调者"。db 模块把这套形态做成默认：

- **驱动**：`github.com/glebarez/sqlite`（modernc 纯 Go 转译，免
  CGO——交叉编译 / Windows / scratch 镜像开箱即用）。mattn 拼法的
  DSN 参数启动即拒（新驱动会静默忽略，fail-fast 防调优悄悄失效）。
- **读写分池**（文件库）：写侧单连接 + `_txlock=immediate`——SQLite
  物理上单写者，写请求在 Go 池上公平排队，且 `BEGIN` 即取写锁，
  杜绝先读后写事务升锁时不吃 busy_timeout 的 `SQLITE_BUSY`；读侧
  `max(4, NumCPU)` 连接池走 WAL 快照与写者并行。
  `gorm.io/plugin/dbresolver` 按回调路由（查询→读池，写/事务/raw
  非 SELECT→写池；`INSERT ... RETURNING` 因按回调而非 SQL 动词
  路由，稳落写侧）。读池由句柄持有、随 Close 关闭。`:memory:`
  无法分池（每连接一个新库），维持钉单连接。
- **每连接默认**：`foreign_keys(1)`（与 PG 双跑行为对齐）、
  `synchronous(NORMAL)`；busy_timeout 5s（驱动默认）；文件级
  `journal_mode=WAL`。用户 DSN 参数优先。
- **内建维护**：`wal_checkpoint(TRUNCATE)` 每 `checkpoint_interval`
  （默认 5m）、`PRAGMA optimize` 每 `optimize_interval`（默认 1h，
  Close 前再补一次）；0 关闭。模块管理生命周期，库级 `db.Open`
  不起后台 goroutine。
- **不做 writer goroutine 组提交**：WAL + NORMAL 下 COMMIT 不逐笔
  fsync，组提交收益远小于独立应用场景；框架层保持 store 直写模型，
  批量吞吐走 `BatchCreate` / `BatchUpdate` / `BatchUpsert`，跨操作合并
  走 `RunInTx`。多实例部署 =
  前提失效，换 LiteFS 只读副本或 `driver: postgres`。

### 7.6 运行态观测

db 通过 `metrics` 软依赖接入进程 Prometheus registry；缺席或关闭时
数据库行为不变。查询回调导出按实例和 CRUD/row/raw 操作聚合的时长与
错误数（`record not found` 属业务态，不计错误），表名不进入标签；连接
池在 scrape 时读取 `sql.DB.Stats()`，文件 SQLite 的 primary/read 两池
分别可见。SQLite checkpoint/optimize 以 `ok/error/deferred` 记录结果。

| 指标 | 类型 | 标签 |
|---|---|---|
| `db_query_duration_seconds` | Histogram | `instance,op` |
| `db_query_errors_total` | Counter | `instance,op` |
| `db_pool_connections` | Gauge | `instance,pool,state` |
| `db_pool_wait_total` / `db_pool_wait_seconds_total` | Counter | `instance,pool` |
| `db_migration_expected_version` / `db_migration_applied_version` | Gauge | `instance,sequence` |
| `db_migrations_applied` / `db_migrations_pending` / `db_migrations_dirty` | Gauge | `instance,sequence` |
| `db_sqlite_maintenance_runs_total` | Counter | `instance,job,result` |

versioned 模式分别导出二进制内嵌迁移的 expected version 与数据库当前
observed applied version，并以 `migration_status_interval`（默认 30s，0
关闭周期刷新）低频更新 applied/pending/dirty 状态；查询带 5s 上限，不在
Prometheus scrape 路径执行 SQL。`sequence` 为 `app` 或
`chok_account/chok_audit/chok_authz`；这样滚动发布时可以区分「旧二进制仍在
运行」与「共享数据库实际版本/dirty 状态发生变化」。auto/off 模式不产生
迁移指标。

模块管理的连接安装 chok GORM logger：`slow_threshold` 默认 200ms、0
仅关闭慢查询日志，查询错误仍记录；`record not found` 豁免。SQL 始终保留
参数占位符，不把绑定值写入日志。库级 `db.Open` 仍使用 Discard，工具、
测试和嵌入用法不会被动获得日志副作用。

## 8. 电池

| 电池 | 要点 |
|---|---|
| **account** | 注册/登录/refresh/改密/忘记/重置 + 登录限速 + OAuth 四 provider（google/github/facebook/apple）。provider 显式装配：`account.Module(account.WithProviders(google.Provider()))`，yaml `providers.<name>.enabled` 是运行期开关，enabled 而未装配 ⇒ 启动失败。路由守卫 `account.Authn(k)`（Authn + ActiveCheck）。服务面 `Component.Service()`，类型 `account.Service` |
| **authz** | casbin：自研 adapter、Redis Watcher、bootstrap 播种（Migrate 尾：建表 → NewEngine → watcher → audit hook → 播种 → 原子发布，任一步失败即启动失败）。`casbin.audit_enabled=true` ⇒ audit 硬前置真值表：未装配 / disabled / Init 失败 / Migrate 期同步探针写入失败任一 ⇒ authz 启动失败——「必须审计 policy mutation」绝不静默退化 |
| **audit** | 异步 DB sink（单 worker 批量 insert，Block/DropOnFull 取舍显式）、purge cron（经 scheduler 软依赖；缺席 ⇒ purge 禁用 + 注记）、`GET /audit/logs` admin API（RequireAuthz("audit","read") fail-closed）。默认 `enabled: false`（合规组件显式 opt-in）。Needs = db + scheduler? + account?（authz/http 关系走 mount/request-time，避免三节点软依赖环） |
| **scheduler** | robfig cron，panic-safe、重叠策略、统计；实现 `Server`：ctx done 后停调度并有界等待 in-flight（`stop_budget`，默认 15s）再返回——job 收尾期间依赖全部存活 |
| **cache** | otter 内存层 + redis 层 + Breaker（默认关）。显式层开关：redis 层 enabled 而 redis 模块缺席 ⇒ 启动失败（不再「在场即自动挂」） |
| **redis** | go-redis + TLS/CA/username（`TLSConfigFor` 导出）；健康探针 |

## 9. 可观测性

- **health**：`/healthz`（并行探针 + fan-in 硬超时 `probe_timeout`，
  hot）、`/livez`、`/readyz`；draining 先翻 503 再摘流量。
- **metrics**：Prometheus registry + `/metrics`；HTTP RED 指标由
  web 中间件打点（路由标签 = ServeMux pattern），db 软接入查询、
  连接池、迁移和 SQLite maintenance 指标。
- **tracing**：OTel provider（stdout/OTLP），web/db 按角色接入。
- **debug**：`/componentz` 展示 Descriptor 拓扑、组件状态（含
  disabled）与生命周期事件环形缓冲（默认关闭）。

健康语义：`Healther` 是 error 型（正常/故障 + disabled 信息态）。
v1 的 Down/Degraded 分级不再存在——单个 flaky cron job 或 sink
抖动不该摘流量；细节走日志、Stats() 与 metrics。

## 10. CLI 与生成面（`cmd/chok`）

四件套 + 常规命令：

| 命令 | 职责 |
|---|---|
| `chok init <name>` | v2 脚手架：chok.yaml + 生成装配 + main.go + migrations/ 骨架，生成即可 `go run .` |
| `chok sync [--check]` | chok.yaml ⇒ `chok_modules_gen.go`（幂等、字节稳定；`--check` 做 CI 闸）。定制走 `chok.Override`，永不改生成文件 |
| `chok migrate create\|up\|status\|repair` | 带 checksum / dirty 审计的版本化迁移；`status --check` 可作 CI 闸 |
| `chok docs gen [--check]` | 组件表（README ×2 + 本文生成区块）、docs/config.md、docs/chok.schema.json、db/framework_tables_gen.go |
| `chok openapi export` | 取运行中应用的 spec 落 .json/.yaml |

**三道 CI 闸**：`docs gen --check`（生成面漂移即红）+
`hack/apidiff.sh`（公开 API 变更须伴随 CHANGELOG 条目；基线锚定
最近 release tag，tag 未发布时 armed-skip）+ examples 冒烟
（`make smoke`：起 blog、等 `/healthz`、SIGINT、要求干净退出）。
另有 blog 的 `sync --check` 与 schema 对示例 yaml 的校验（测试内）。

JSON Schema 的立场：**结构化 + 冻结枚举**——类型/默认值/嵌套由
Options 反射（与 conf 走查器同规则），枚举只收 SPEC 冻结的封闭集，
行为级校验永远留在 `Validate()`；schema 不可能领先代码。

## 11. 组件总览（生成区块）

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
| `db.Module()` | `db` | log?, tracing?, metrics? | health, migrate | true | 数据库连接池（sqlite/mysql/postgres）+ 迁移（auto/versioned/off）。sqlite 为纯 Go 驱动 + 读写分池 + 内建维护循环（§7.5）。 |
| `redis.Module()` | `redis` | log? | health | true | go-redis 客户端（TLS/CA 支持）；健康探针。 |
| `cache.Module()` | `cache` | redis?, log? | — | true | 分层缓存：otter 内存层 + redis 层 + 熔断器。 |
| `scheduler.Module()` | `scheduler` | log? | health, serve | true | robfig cron（panic 防护、重叠策略、统计）。 |
| `audit.Module()` | `audit` | db, scheduler?, account?, log? | reload, mount, migrate | false | 合规审计日志：异步 DB sink、清理 cron、admin API（显式启用）。 |
| `authz.Module()` | `authz` | db, redis?, audit?, log? | migrate, ready | true | casbin RBAC 引擎：adapter、Redis watcher、bootstrap 播种、决策审计。 |
| `account.Module()` | `account` | db, log? | mount, migrate | true | 用户模块：注册/登录/JWT/重置 + 登录限速 + OAuth providers。 |
<!-- /gen:components -->

各模块配置项全表：[`config.md`](config.md)（生成）。

## 12. 不变量清单（由结构保证）

以下每条由类型系统或控制面结构保证，不依赖文档背诵：

1. 生命周期变更全部经控制 goroutine —— 不存在可写错的锁序。
2. 配置只有不可变快照 —— 不存在 torn read / 指针字段禁令。
3. 组件声明是数据（Descriptor）—— 不存在「忘记实现某声明接口」。
4. 依赖访问只在 Init（经 Kernel 快照）—— Close 拿不到 peer。
5. disabled 组件有定义良好的四条语义 —— 注册拓扑不随配置漂移。
6. reload-safe/restart-only 是字段 tag —— 组件不再手写 warn。
7. gorm/gin 类型不在公开面 —— 两扇 Unsafe 门是仅有的例外且
   自名其险；`grep "\.Unsafe("` 即见全部越权点。
8. veto = 组件 Init 返回错误 —— 没有「哪些 hook 能中止启动」的
   口口相传。
9. 组件表 / 配置文档 / JSON Schema 由生成器产出 —— 漂移在 CI
   挂掉而不是靠人巡检。

（store 安全栏、`context.WithoutCancel` shutdown 纪律等 v1 已
结构化的不变量原样继承。）

## 13. 目录结构

```
chok/
├── chok.go              App 薄壳：New / Use / Override / Routes / Section / Run
├── kernel/              Descriptor / 行为接口 / Router 契约 / actor Registry / 事件总线
├── conf/                装载（viper）+ RCU 快照 + reload tag diff + redact
├── web/  middleware/  handler/
├── db/  store/  store/where/
├── log/  apierr/  rid/  validate/  auth/(jwt)
├── account/  authz/(casbin)  audit/  scheduler/  cache/  redis/
├── swagger/  health/  metrics/  tracing/  debug/
├── cmd/chok/            init / sync / migrate / docs / openapi / version / update
├── internal/blessed/    模块清单（生成器的事实源入口）
├── internal/docgen/     组件表 / 配置参考 / JSON Schema 渲染器
├── choktest/            NewTestDB / NewTestStore / NewTestKernel / StartKernel
├── examples/blog/       快速上手（验收测试 = README 路径）
└── docs/                design.md（本文）/ config.md（生成）/ chok.schema.json（生成）
                         / migration-v1-to-v2.md / changelog.md / roadmap.md
```

## 14. 测试哲学

- 反 mock：store/db 测试开真数据库（`db/dbtest.Open(t)`——SQLite
  默认，`CHOK_TEST_DRIVER=postgres` + DSN 走 PG 道；CI 双跑）。
- `choktest` 是下游可用的测试基建：`NewTestDB`（返回 `*db.DB`）、
  `NewTestStore`、`NewTestKernel` / `StartKernel`（真装配、真
  生命周期、TestRouter 真派发）。
- 验收测试即文档：blog 的 `blog_test.go` 用真实 HTTP 走一遍 README
  五分钟路径；`chok init` 的自检测试真跑 `go build` + 启动 +
  SIGINT。
- 每个 bug fix 同 commit 附回归测试。

## 15. 版本与发布

- v1 封版 `v0.1.4`（仅安全修复，module proxy 永续可取）。
- v2 自 `v2.0.0-beta.1` 起走 beta 系列；release **手动**裁切：
  CHANGELOG 条目 → 全量绿 → tag → push（goreleaser workflow 建
  产物与 GitHub Release）。release-please 已停用（版本演算无法
  表达人工节奏的 beta 系，且有历史误算前科）。
- apidiff 基线随 release tag 前移（`.apidiff-baseline`）；公开 API
  变更未带 CHANGELOG 条目会被 CI 拒绝。
- beta tag 一旦推送不可重打（module proxy 永续缓存）。
