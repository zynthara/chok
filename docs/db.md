# 数据层使用指南(db / store / where)

> **读者**:用 chok 写业务的应用开发者。读完本文你可以:装配数据库模块、
> 定义带安全默认值的模型、完成全部日常 CRUD、正确使用事务与迁移,并知道
> 每条安全栏在拦什么。
>
> 本文是**使用指南**(怎么做);架构决策与契约边界见
> [`design.md`](design.md),逐项配置见生成的 [`config.md`](config.md),
> 从 v1 迁移见 [`migration-v1-to-v2.md`](migration-v1-to-v2.md)。
> 代码引用以 `main` 分支为准。

---

## 1. 五分钟快速开始

**chok.yaml** —— 段在场即装配(`chok sync` 会替你生成装配代码):

```yaml
db:
  driver: sqlite          # sqlite | mysql | postgres
  sqlite:
    path: app.db
  migrate: auto           # auto | versioned | off(见 §8)
```

**模型 + Store + 路由** —— 一个实体的完整闭环:

```go
// model.go
type Post struct {
    db.OwnedSoftDeleteModel // 属主隔离 + 软删除 + RID/乐观锁/时间戳

    Title   string `json:"title"   store:"query,update" gorm:"size:200;not null"`
    Content string `json:"content" store:"update"       gorm:"type:text"`
    Status  string `json:"status"  store:"query,update" gorm:"size:20;default:'draft'"`
}

// RID 前缀:对外 ID 形如 pst_pbERs9oJT0AA,内部自增主键永不外泄。
func (Post) RIDPrefix() string { return "pst" }
```

```go
// routes 回调里(kernel 就绪后):
posts := store.New[Post](db.From(k), log.From(k)) // 字段白名单已随 store tag 声明

p := &Post{Title: "hello", Status: "draft"}
err := posts.Create(ctx, p)                                  // OwnerID 自动填当前用户
one, err := posts.Get(ctx, store.RID(p.RID))                 // 按对外 ID 取
page, err := posts.List(ctx, where.WithFilter("status", "draft"), where.WithCount())
err = posts.Update(ctx, store.RID(p.RID), store.Fields(one, "title")) // 带乐观锁
err = posts.Delete(ctx, store.RID(p.RID))                    // 软删模型 ⇒ 软删除
```

把表交给框架建(`migrate: auto` 下):

```go
chok.New(
    chok.Use(db.Module(db.WithTables(db.Table(&Post{})))),
    // ...
)
```

跑得通的完整示例:[`examples/blog`](../examples/blog) —— README
六步路径就是本指南的最小实践。

---

## 2. 心智模型

数据层由四个角色组成,职责不重叠:

```
chok.yaml ──▶ db.Module()        连接生命周期 + 迁移模式(kernel 组件)
                   │ db.From(k)
                   ▼
              *db.DB             瘦句柄:RunInTx / Migrate / Ping / Unsafe
                   │ store.New[T](h, logger)
                   ▼
              store.Store[T]     全部读写的唯一入口(白名单/scope/事件)
                   ▲
              where.Option       查询表达(过滤/排序/分页/游标)
```

两条设计立场,解释了本文所有"为什么":

1. **gorm 不在公开面**。业务代码只接触 `*db.DB` 与 `Store[T]`;raw gorm
   只有两扇标着 `Unsafe` 的门(§10)。`grep '\.Unsafe('` 即可审计全部越权点。
2. **fail-closed 默认**。拿不到用户就拒绝(属主隔离)、没有条件就拒绝
   (防清表)、未声明的字段就拒绝(白名单)。安全默认值汇总见 §11。

---

## 3. 定义模型

### 3.1 选择基座

| 内嵌 | 得到 | 适用 |
|---|---|---|
| `db.Model` | 自增 PK + RID + 乐观锁版本 + 时间戳 | 无属主、硬删除的表 |
| `db.SoftDeleteModel` | Model + 软删除(`deleted_at` + `delete_token`) | 需要回收站/可恢复 |
| `db.OwnedModel` | Model + `owner_id`(自动属主隔离) | 用户私有数据 |
| `db.OwnedSoftDeleteModel` | 以上全部 | 用户私有 + 可恢复(最常用) |

内嵌 `db.Model` 是强制的:`store.New[T]` 的类型约束只接受它的内嵌者。
实现 `RIDPrefix() string` 后,`Create` 自动生成 `pst_xxx` 形式的对外 ID。

> **规则**:API 响应永远用 RID(JSON 名就是 `id`),内部 `uint` 主键
> 不出现在任何对外面。

### 3.2 用 `store` tag 声明字段面

模型自己声明哪些字段可查、可写:

```go
Title   string `json:"title"   store:"query,update"` // 可过滤、可更新
Content string `json:"content" store:"update"`       // 只可更新(大文本不进过滤面)
Secret  string `json:"-"       store:"query"`        // JSON 不可见但可过滤(过滤名 secret)
Internal string `json:"internal"`                     // 无 tag = 两个面都不暴露
```

规则:

- tag 值只有 `query`、`update` 两个词,逗号分隔;**其他值在构造期
  panic**(声明拼错必须炸,不能静默收窄面)。
- 过滤名 = JSON 名;`json:"-"` 或无 JSON tag 时用字段名的 snake_case。
- 内嵌基座自动向 **query 面**贡献 `id` / `created_at` / `updated_at`;
  **update 面永远不含基座字段**(版本、时间戳、RID 不可经 store 改写)。
- 完全不写 tag 也能跑:回退到 JSON tag 自动发现,但每次构造都会打
  warn 日志(`store: auto-discovered query fields; ...`)——隐式集合会
  随结构体悄悄变大,生产代码请显式声明。

### 3.3 软删除下的唯一约束:`SoftUnique`

普通 `uniqueIndex` 会让"删掉的行"永远占着唯一槽。软删模型的唯一约束
用 `SoftUnique` 声明:

```go
db.Module(db.WithTables(
    db.Table(&User{}, db.SoftUnique("uk_user_email", "email")),
))
```

生成 `UNIQUE(email, delete_token)`:活跃行的 `delete_token` 恒为空串,
软删时框架写入随机 token 释放槽位;`Restore` 归还空串重新入槽(§7.5)。

---

## 4. 构造 Store

```go
posts := store.New[Post](db.From(k), log.From(k))
```

字段白名单的解析优先级(query / update 两面各自独立):

1. `WithQueryFields` / `WithUpdateFields` —— 调用点显式列表,**整体覆盖**
   tag。用于给同一模型开不同视图:

   ```go
   // 特权面:服务内部用
   adminUsers := store.New[User](h, logger,
       store.WithQueryFields("id", "email", "active"),
       store.WithUpdateFields("name", "email", "password_hash", "roles", "active"))
   // 公开面:暴露给 HTTP 的那个
   publicUsers := store.New[User](h, logger,
       store.WithQueryFields("id", "email", "created_at"),
       store.WithUpdateFields("name", "email"))
   ```

2. `WithAllQueryFields(exclude...)` / `WithAllUpdateFields(exclude...)`
   —— 显式要求"全字段自动发现,除了这些"。
3. **`store` tag** —— 模型自带的缺省声明(§3.2)。
4. 自动发现 + warn —— 无任何声明时的回退。

常用选项速览(完整见 godoc):

| 选项 | 作用 |
|---|---|
| `WithBus(k.Bus())` | 写操作发布 `EntityChanged[T]` 事件,**锚定事务提交**(回滚不发) |
| `WithStrict()` | 未声明字段/未知查询参数从"忽略"升级为报错;拒绝隐式自动发现 |
| `WithMaxPageSize(n)` / `WithDefaultPageSize(n)` | 分页护栏 |
| `WithBeforeCreate/Update/Delete(fn)` | 写前钩子(同步,返回错误即中止) |
| `WithColumnAlias(field, col)` | 过滤名→列名的显式映射 |
| `WithoutOwnerScope()` | ⚠️ 关闭属主隔离——构造期打 warn,确认你真的要全局可见 |

---

## 5. 读

### 5.1 定位一行:`Get` + Locator

```go
p, err := posts.Get(ctx, store.RID("pst_abc123"))       // 对外 ID(最常用)
p, err := posts.Get(ctx, store.ID(42))                  // 内部主键(仅服务内部)
p, err := posts.Get(ctx, store.Where(                   // 条件定位,多行匹配取第一行
    where.WithFilter("status", "draft")))
p, err := posts.Get(ctx, store.RID(rid), store.WithPreload("Author"))
```

没有命中返回 `store.ErrNotFound`(可 `errors.Is` 判断)。三种 Locator
(`RID` / `ID` / `Where`)在 `Update` / `Delete` / `Restore` / `Exists`
中通用。

### 5.2 列表与过滤:`List` + where DSL

```go
page, err := posts.List(ctx,
    where.WithFilter("status", "published"),           // =
    where.WithFilterOp("created_at", where.Gte, t0),   // Eq/Ne/Gt/Gte/Lt/Lte
    where.WithFilterIn("status", "draft", "review"),   // IN
    where.WithFilterContains("title", "go"),           // LIKE %go%(通配已转义)
    where.WithOrder("created_at", true),               // desc
    where.WithPage(1, 20),
    where.WithCount(),                                 // ← 要 Total 必须给
)
for _, p := range page.Items { ... }
total := page.Total
```

> ⚠️ **`Page.Total` 只在传了 `where.WithCount()` 时才计算**,否则恒为
> 0——总数是一条额外的 COUNT 查询,按需付费。只要数字不要行,用
> `Count`(§5.3)。

过滤字段一律经查询白名单解析,未声明的字段返回错误(不是静默忽略);
`WithFilterLike` 家族对用户输入做了通配转义,`%`/`_` 不会穿透。

### 5.3 只要数字:`Count`

```go
n, err := posts.Count(ctx)                                    // scope 内全量
n, err := posts.Count(ctx, where.WithFilter("status", "draft"))
```

分页/排序选项被剥离(`Count(WithPage(1,1))` 仍是全量总数),软删行
默认不计。

### 5.4 其他读法

```go
ok, err := posts.Exists(ctx, store.RID(rid))          // 只探存在性,不取数据
items, err := posts.ListByIDs(ctx, []uint{1, 2, 3})   // 主键批取(服务内部)

// HTTP 列表页直通:?page=&size=&order=field:desc&<声明过的字段>=值
items, total, err := posts.ListFromQuery(ctx, r.URL.Query())

// 游标分页(深分页/无限滚动;NextCursor 为空即最后一页)
cp, err := posts.ListWithCursor(ctx, "created_at", where.CursorAfter, cursor, 20)
```

把列表页直接挂成路由只要一行(blog 在用):

```go
api.Handle(http.MethodGet, "/posts", handler.HandleList[Post](posts))
```

### 5.5 看见软删行(管理/回收站视图)

```go
page, err := posts.ListQ(ctx,
    []store.QueryOption{store.WithOnlyTrashed()},      // 或 WithTrashed():活+删都要
    where.WithCount())
```

scope 依旧生效:软删行可见,但依然只有属主/管理员能看到自己的。

---

## 6. 写

### 6.1 `Create`

```go
p := &Post{Title: "hi"}
err := posts.Create(ctx, p)
// p.RID / p.Version / p.CreatedAt 已回填;Owned 模型的 OwnerID 已自动填当前用户
```

唯一约束冲突返回 `store.ErrDuplicate`。批量用 `BatchCreate(ctx, objs)`。

### 6.2 `Update`:`Fields` 与 `Set` 的选择

```go
// ✅ 首选:Fields —— 从对象取值,自动带上 obj.Version 做乐观锁
p, _ := posts.Get(ctx, store.RID(rid))
p.Title = "new title"
err := posts.Update(ctx, store.RID(rid), store.Fields(p, "title"))
// 版本被并发改过 ⇒ store.ErrStaleVersion(HTTP 409),重读重试

// Set:裸 map,无乐观锁 —— 只用于计数器类"最后写赢"的字段
err := posts.Update(ctx, store.RID(rid), store.Set(map[string]any{"status": "published"}))

// 对 Set 手动加锁 / 对 Fields 显式指定版本:
err := posts.Update(ctx, store.RID(rid), store.Set(m), store.WithVersion(v))
```

规则:列名走 **update 白名单**;每次成功更新 `version` 自增;确实不要
锁时用 `store.Fields(p, cols...).NoLock()` 把意图写在代码里。

### 6.3 `Delete`

```go
err := posts.Delete(ctx, store.RID(rid))                          // 幂等:没有命中也是 nil
err := posts.Delete(ctx, store.RID(rid), store.WithVersion(v))    // 带乐观锁的删除
```

- 软删模型:写 `deleted_at` + 随机 `delete_token`(释放 SoftUnique 槽);
  普通模型:物理删除。
- 带 `WithVersion` 时零命中会区分:行存在但版本不符 ⇒ `ErrStaleVersion`;
  行根本不在 ⇒ `ErrNotFound`。
- **`Delete(ctx, store.Where())` 无条件清表会被拒绝**:返回
  `ErrMissingConditions`(`store: operation called without conditions`)。
  这是防呆栏,真要全删走 §10 的逃生门并三思。

### 6.4 `Restore`:软删恢复

```go
err := posts.Restore(ctx, store.RID(rid))
```

恢复不只是清 `deleted_at`:`delete_token` 必须归还空串,行才重新进入
SoftUnique 槽位。`Restore` 持有这套不变量:

- 槽位已被新活跃行占用 ⇒ `ErrDuplicate`,行保持已删;
- scope 生效:恢复不了别人的行,且对方行读作 `ErrNotFound`(不泄露存在性);
- 幂等镜像 `Delete`:行本来就活着 ⇒ nil;行不存在 ⇒ `ErrNotFound`;
- 硬删模型调用 ⇒ 错误(`not a soft-delete model`)。

### 6.5 `Upsert` 与属主模型不兼容

`Upsert` 在带 scope 的 Store、以及**内嵌 `db.Owned` 的模型**上直接报错:
SQL 的 `ON CONFLICT UPDATE` 不会给更新路径套 `owner_id` 过滤,攻击者
可以用一个冲突键改到别人的行。替代写法:`Create` → 捕获 `ErrDuplicate`
→ 显式 `Update`。

---

## 7. 事务

```go
h := db.From(k)
err := h.RunInTx(ctx, func(txCtx context.Context) error {
    if err := posts.Create(txCtx, p); err != nil {   // 同一事务
        return err
    }
    return users.Update(txCtx, store.RID(uid), changes) // 跨 Store 也在同一事务
})
```

- **事务随 ctx 传播**:`RunInTx` 给回调的 `txCtx` 带着事务,任何 Store
  方法收到它就自动加入;返回错误即回滚。
- 单 Store 便捷形态:`posts.Tx(ctx, func(tx *store.Store[Post]) error {...})`
  ——回调收到绑定事务的 Store 克隆;外层 ctx 已有事务时复用、不嵌套。
- `db.InTx(ctx) bool` 用于断言("这段必须在事务里跑"),它只回答是否,
  不交出句柄。
- `WithBus` 事件锚定提交:事务内的写先暂存,`Commit` 后按序发布,
  回滚全部丢弃——订阅者永远不会看到没发生过的写。

---

## 8. 迁移

三种模式(`chok.yaml` 的 `db.migrate`),启动时执行,**Reload 永远不会
触发迁移**——改了 schema 相关配置需要重启:

| 模式 | 行为 | 适用 |
|---|---|---|
| `auto`(默认) | 启动时对 `WithTables` 声明的表 AutoMigrate | 开发、单体小服务 |
| `versioned` | 只执行编号 SQL 迁移文件,拒绝隐式改表 | 生产、多副本 |
| `off` | 框架完全不碰 schema(电池表也不建) | DBA 全权管理 |

`versioned` 工作流(SQL 文件前向单向,没有 down——改错就发下一个前向
迁移):

```bash
chok migrate create add_posts_table   # 生成 migrations/0001_add_posts_table.sql
# 编辑 SQL 后:
chok migrate up                       # 跨进程锁下执行全部 pending
chok migrate status --check           # 全景状态；非 clean 时退出 1
```

每个文件按 CRLF→LF 归一化后计算 SHA-256。执行任何 SQL 前，框架先
提交 `dirty=true` 账本行与临时兼容 fence；因此进程死亡、MySQL DDL
部分提交以及回滚旧 chok 二进制都不会把半成品误认为成功。`status`
严格只读，展示 applied / pending / dirty / drift / missing / unverified /
out-of-order / name-drift / fenced。旧三列账本第一次执行 `up` 时按当前文件建立
checksum 基线；这只能保证此后的改写可检测，不能追溯基线前的历史。

dirty 不能自动判断为“该重跑”还是“其实已全部生效”。先人工核对数据库，
再针对一个版本执行显式 repair（checksum 从 `status` 输出复制）：

```bash
# 已恢复到迁移前状态，下次 up 整文件重跑
chok migrate repair retry 12 --checksum <ledger-sha256> --reason "restored partial DDL"

# 已确认或手工补齐全部效果，只清 dirty
chok migrate repair mark-applied 12 --checksum <ledger-sha256> --reason "completed manually"

# 已应用文件的改写经过审核，接受当前字节作为新基线
chok migrate repair accept-drift 7 --checksum <old-ledger-sha256> --reason "approved rewrite"
```

repair 使用 version + checksum 做 compare-and-swap，并返回包含 old/current
checksum、reason、时间的结构化报告。v2 不额外创建 repair history 表；有
合规留存要求时应把该报告写入部署平台或集中审计日志。MySQL 尤其不能在
未核对已提交 DDL 的情况下直接选择 retry。

应用侧把目录嵌进二进制:

```go
//go:embed migrations/*.sql
var migrations embed.FS

db.Module(db.WithMigrations(migrations))
```

框架自有表由各内建组件的 `Descriptor.Schema` 声明，并由
`chok docs gen` 聚合成字母序的 `db.FrameworkTables()` 目录；已装配组件
自行演进这些表，不占用你的迁移序号。该目录与具体装配及 named DB
instance 无关，因此不表示列出的每张表都存在于当前数据库。

---

## 9. 多实例

```yaml
db:
  driver: postgres
  postgres: { dsn: "postgres://.../main" }
  instances:
    analytics:
      driver: postgres
      read_only: true
      postgres: { dsn: "postgres://.../olap" }
```

```go
chok.Use(db.Module(), db.Module(db.As("analytics")))

main := db.From(k)                 // 默认实例
olap := db.From(k, "analytics")    // 具名实例
```

`read_only: true` 是实例能力而不只是命名：有效迁移模式强制为 `off`，
`RunInTx` / `Migrate` 与所有 blessed store 写方法返回 `db.ErrReadOnly`；
构造 store 时必须显式写 `store.WithReadOnly()`，否则启动期 panic。只读
句柄不会加入其他实例放进 context 的事务。

`Unsafe` 仍可作复杂查询，但只放行以 `SELECT` 开头且不带行锁的 raw SQL；
`WITH`、`FOR UPDATE` 和全部 ORM/Exec 写在 GORM callback 层拒绝。SQLite
还用 `mode=ro` 打开文件；Postgres/MySQL 为每条新连接设置只读 session
默认。应用层判定用于防误用，**数据库只读账号或物理副本才是权限边界**。
需要同一 DSN 的管理写时，装配另一个可写具名实例，而不是运行时开逃逸门。

---

## 10. 逃生门(危险区)

raw gorm 只有两扇门,**都叫 Unsafe**,选哪扇看你要不要租户语义:

| 门 | 事务感知 | scope | 用途 |
|---|---|---|---|
| `Store.Unsafe(ctx)` | ✅（仅同句柄事务） | ✅ 已应用,scope 失败 fail-closed | store DSL 写不出的 SQL,但 owner/租户过滤必须保持 |
| `(*db.DB).Unsafe(ctx)` | ✅（仅同句柄事务） | ❌ 无 | 基建层:外形表 AutoMigrate、事务内行锁 |

```go
gdb, err := posts.Unsafe(ctx)      // 注意:会返回 error(scope 解析失败即拒)
gdb := h.Unsafe(ctx)               // 句柄级:无 scope,自己负责
```

纪律:不进 HTTP handler 的快乐路径;包在 repository/store 层内;每处
调用都是 code review 的检查点——这正是命名成 Unsafe 的意义。

---

## 11. 安全默认值一览

| 栏 | 行为 | 关闭方式(显式) |
|---|---|---|
| 属主隔离 | `Owned` 模型自动 `owner_id` 过滤;**ctx 无登录用户 ⇒ 拒绝(401)** | `WithoutOwnerScope()`(构造期 warn) |
| 管理员越权 | principal 带全局管理员角色时跳过属主过滤 | `store.SetDefaultAdminRoles(...)` 配置角色集 |
| 防清表 | 写操作的 `Where()` 必须至少一个条件,否则 `ErrMissingConditions` | 无(走逃生门) |
| 字段白名单 | 过滤/更新只认声明过的字段,未声明报错 | 无 |
| 大文本防护 | 自动发现不把 text/blob 列放进过滤面 | tag/显式声明可放行 |
| 通配转义 | `WithFilterContains` 等对 `%`/`_` 转义 | `WithFilterLikeRaw`(自己负责) |
| 敏感配置 | DSN/密码带 `sensitive` 标注,日志输出自动掩码 | 无 |
| 只读实例 | `read_only: true` 强制 migrate off，拒绝事务、DDL、store/GORM 写；driver 层再兜底 | 另装配可写具名实例 |

### SQLite 单机生产形态(默认生效)

`driver: sqlite` 时框架自动落成单进程生产形态,零配置:

- **纯 Go 驱动**(glebarez/modernc):无 CGO,交叉编译、Windows 开发机、
  scratch 镜像开箱即用。注意 mattn 拼法的 DSN 参数(`_synchronous=` 等)
  会在启动时被拒绝——新驱动会静默忽略它们,fail-fast 好过悄悄失效;
  改写成 `_pragma=synchronous(NORMAL)` 形式。
- **读写分池**:写侧固定单连接 + `_txlock=immediate`(`BEGIN` 即取写锁,
  杜绝"先读后写"事务升锁时那个不吃 busy_timeout 的 `SQLITE_BUSY`)。
  SQLite 物理上只允许一个写者——单连接让写请求在 Go 侧公平排队,
  而不是多连接撞文件锁空转。读侧独立连接池(`max_open_conns`,默认
  `max(4, NumCPU)`)靠 WAL 快照与写者并行。路由按 gorm 回调自动分流
  (查询走读池,写/事务/raw 非 SELECT 走写池),业务代码无感知;
  `:memory:` 库无法分池,维持钉单连接。
- **每连接默认**:`foreign_keys(1)`(外键真正生效,与 Postgres 双跑
  行为对齐)、`synchronous(NORMAL)`(WAL 下安全)、busy_timeout 5s;
  文件级 `journal_mode=WAL`。自己写在 `path` 里的 DSN 参数永远优先。
- **后台维护**:db 模块每 `checkpoint_interval`(默认 5m)跑
  `wal_checkpoint(TRUNCATE)` 防长读者让 WAL 无界膨胀,每
  `optimize_interval`(默认 1h)跑 `PRAGMA optimize` 刷新查询计划
  统计,Close 前再补一次;设 0 关闭。备份挂 litestream 边车即可,
  WAL 模式天然适配。
- **纪律**:写事务保持短小;`Rows()` 流式读及时 Close(长快照会顶住
  checkpoint);批量写走 `BatchCreate` 或一个 `RunInTx` 合并提交;
  事务内的所有操作必须传 `txCtx`(Store 已自动)——拿根 ctx 在事务内
  再发写,会在池上排队等那个被外层事务占着的唯一写连接。

边界:这套形态的前提是**单进程独占数据库文件**。多实例部署时前提
被打破——用 LiteFS/litestream 做只读副本、写仍回单点,或者那就是换
`driver: postgres` 的时刻。

---

## 12. 错误处理

所有错误可 `errors.Is` 匹配哨兵;挂上映射器后 HTTP 状态码自动正确:

```go
chok.New(
    chok.WithErrorMapper(store.MapError),  // 一次装配,处处生效
)
```

| 哨兵 | 含义 | HTTP |
|---|---|---|
| `store.ErrNotFound` | 定位无命中(或无权看见) | 404 |
| `store.ErrStaleVersion` | 乐观锁冲突 | 409 |
| `store.ErrDuplicate` | 唯一约束冲突 | 409 |
| `store.ErrMissingConditions` | 无条件写操作被拦 | 500(编程错误) |
| `db.ErrReadOnly` | 只读实例或只读 store 收到写操作 | 500(装配/编程错误) |
| `where.ErrUnknownField` | 过滤字段未声明 | 400 |

---

## 13. 测试

```go
func TestPostFlow(t *testing.T) {
    h := choktest.NewTestDB(t, &Post{})        // 真实 SQLite 内存库,自动建表清理
    posts := store.New[Post](h, log.Empty())
    // ... 正常使用,断言真实 SQL 行为
}
```

- **不要 mock 数据库**——内存 SQLite 一样快,且能抓到真实 schema 问题。
- 需要 Postgres 行为差异时跑双道:
  `CHOK_TEST_DRIVER=postgres CHOK_TEST_PG_DSN=... go test ./...`
  (CI 的 PG service 自动跑同一套)。
- MySQL 隐式 DDL 提交的 dirty/repair 主路径跑
  `CHOK_TEST_MYSQL_DSN=... make test-mysql`（CI 提供 MySQL 8.4 service）。
- 属主隔离的测试给 ctx 注入用户:
  `auth.WithPrincipal(ctx, auth.Principal{Subject: "usr_alice"})`。

---

## 14. 故障排除

| 症状 | 原因 | 解法 |
|---|---|---|
| 日志刷 `store: auto-discovered query fields; declare them with `store` tags or WithQueryFields` | 模型没有任何字段声明 | 给字段加 `store:` tag(§3.2) |
| 构造期 panic `bad `store:"..."` tag value` | tag 拼错(如 `quer`) | 只有 `query` / `update` 两个词 |
| 读写全部 401 | `Owned` 模型 + ctx 无登录用户(fail-closed) | 请求路径挂 `account.Authn(k)`;测试用 `auth.WithPrincipal` |
| `where: unknown field: "xxx"` | 过滤字段不在查询白名单 | 加进 tag/`WithQueryFields`;检查用的是 JSON 名不是列名 |
| `Page.Total` 恒为 0 | 没传 `where.WithCount()` | 加上;或只要数字用 `Count` |
| 更新总报 `ErrStaleVersion` | 对象是旧读;或 `Set(map)` 混用了 `WithVersion(0)` | 重读后 `Fields` 更新;检查并发写 |
| `store: operation called without conditions` | `Update`/`Delete` 传了空 `Where()` | 补条件;真要全表操作走逃生门 |
| `Restore` 报 `ErrDuplicate` | 唯一槽已被新活跃行占用 | 业务决策:删新行、改字段后重试,或放弃恢复 |
| 启动报 `db: BeforeCreate: invalid RID` | 手工构造/导入的 RID 形状非法 | 用 `rid.New(prefix)` 生成;导入数据先校验 |
| SQLite 并发下 `database is locked` | 框架默认已是读写分池(写侧单连接排队)+ `_txlock=immediate` + WAL + 5s busy_timeout,常规并发读写会排队不报错;仍出现说明某个写事务持锁超 5s,或 DSN 显式改了 `_txlock` | 缩短写事务;查有没有长事务/未 Close 的 `Rows()`;持续写超载则换 `driver: postgres`(§11 SQLite 小节) |
| SQLite 写操作不报错但一直不返回 | `RunInTx` 里拿根 ctx(而非 `txCtx`)又发了写——外层事务占着唯一写连接,这笔在池上排队等它,直到 ctx 超时 | 事务内所有操作传 `txCtx`;确要旁路写就放到事务外 |
| 启动报 `DSN parameter "_synchronous" is a mattn/go-sqlite3 spelling` | 驱动已换纯 Go 构建(glebarez),mattn 拼法参数会被静默忽略,框架选择 fail-fast | 改成 `_pragma=synchronous(NORMAL)` 形式(`_txlock` 拼法不变) |
| `versioned` 模式下写入报表不存在 | 忘了 `chok migrate up`,或 SQL 文件没 embed | 检查 `WithMigrations` 与 `migrate status` |
| 启动报 `dirty migration attempt` | 上次迁移失败或进程在 clean 前退出 | `migrate status` 核对实际 schema，再按 §8 选择 repair retry 或 mark-applied |
| `status --check` 报 `unverified` | 旧三列账本尚未建立 checksum 基线 | 使用当前可信发布执行一次 `migrate up` 完成 trust-on-first-use adoption |

---

## 15. API 速查

```
构造与句柄
  db.From(k, instance...)        db.Open(opts)           db.Module(opts...)
  db.As(name)  db.WithTables(specs...)  db.WithMigrations(fs)
  h.RunInTx(ctx, fn)  h.Migrate(ctx, specs...)  h.Ping(ctx)  h.Unsafe(ctx)
  db.InTx(ctx)  db.Table(model, indexes...)  db.SoftUnique(name, cols...)

版本化迁移
  db.LoadMigrations(fs)  db.ApplyMigrations(ctx, h, fs)
  db.ApplyMigrationsWithReport(ctx, h, fs)  db.MigrationsStatus(ctx, h, fs)
  db.RepairMigration(ctx, h, fs, db.RepairOptions)

Store[T](读)
  Get(ctx, loc, ...QueryOption)         List(ctx, ...where.Option)
  ListQ(ctx, []QueryOption, ...)        ListFromQuery(ctx, url.Values)
  ListByIDs(ctx, ids)                   ListWithCursor(ctx, field, dir, cur, n)
  Count(ctx, ...where.Option)           Exists(ctx, loc)

Store[T](写)
  Create(ctx, *T)      BatchCreate(ctx, []*T)
  Update(ctx, loc, changes, ...UpdateOption)
  Delete(ctx, loc, ...DeleteOption)     Restore(ctx, loc)
  Upsert(ctx, *T, conflictCols, updateCols...)   // 属主模型禁用
  Tx(ctx, fn)          Unsafe(ctx)

定位 / 变更 / 选项
  store.RID(s)  store.ID(n)  store.Where(opts...)
  store.Fields(obj, cols...)[.NoLock()]  store.Set(map)  store.WithVersion(v)
  store.WithPreload(rel)  store.WithTrashed()  store.WithOnlyTrashed()

接口视图(依赖注入用)
  store.Reader[T]  store.Writer[T]  store.ReadWriter[T]
```

---

## 16. 项目组织与分层

`Store[T]` 本身就是数据操作层的实现载体——不需要再手写一层 DAO 包住
它。分层只回答两个问题:实体定义放哪、要不要在 `Store[T]` 外再包一层
领域 store。

### 16.1 落位:实体归 model 包,操作归 store 层

```
myapp/
├── chok.yaml
├── chok_modules_gen.go        # chok sync 生成
├── main.go                    # 装配点
├── model/                     # ① 实体:纯数据 + 声明,只 import db
│   ├── post.go
│   └── tables.go              # 建表清单与实体同包
├── store/                     # ② 数据操作层(小项目可整层省略,见 16.2)
│   └── posts.go
└── api/                       # ③ HTTP handlers
    └── posts.go
```

model 包保持单向依赖(只 import `db`),实体靠 `store` tag 自带操作面
声明(§3.2);建表清单收在同包,`main.go` 只管转交:

```go
// model/tables.go
func Tables() []db.TableSpec {
    return []db.TableSpec{
        db.Table(&Post{}),
        db.Table(&Comment{}, db.SoftUnique("uk_comment_slug", "slug")),
    }
}

// main.go
chok.Use(db.Module(db.WithTables(model.Tables()...)))
```

### 16.2 store 层的两种形态

**形态 A(起步默认):`Store[T]` 就是 store 层。** 装配点构造、注入
handler,不写任何包装——`examples/blog` 即此形态,单实体 CRUD 的
项目到此为止就够了。

**形态 B(领域词汇出现后):内嵌包装。** 当你需要 `PublishedSince`
这类带业务语义的查询名时再包一层。关键是**内嵌透出**——常规 CRUD
免费获得,只写领域方法,绝不手抄转发:

```go
// store/posts.go
type PostStore struct {
    *store.Store[model.Post]   // Get/List/Create/.../Restore/Count 直接透出
}

func NewPostStore(h *db.DB, l log.Logger) *PostStore {
    return &PostStore{Store: store.New[model.Post](h, l)}
}

// 名字属于业务;实现仍走白名单与 scope,安全栏不因包装而松动
func (s *PostStore) PublishedSince(ctx context.Context, t time.Time) (*store.Page[model.Post], error) {
    return s.List(ctx,
        where.WithFilter("status", "published"),
        where.WithFilterOp("created_at", where.Gte, t),
        where.WithOrder("created_at", true))
}
```

### 16.3 构造纪律与依赖注入

- **store 构造是进程级一次**(routes 回调或应用构造函数里),然后注入
  handler 共享——`Store` 是无状态配置对象,并发安全;构造走反射,
  **不要每请求 `store.New`**(有成本,discovery warn 也会刷屏)。
- **依赖声明用接口视图**:消费方写 `store.Reader[model.Post]` /
  `store.ReadWriter[model.Post]` 而非 `*store.Store[model.Post]`——
  只读消费者拿不到写方法,测试替身只需实现窄面。
- **service 层的存在理由是跨实体编排**:多 store 同事务走
  `h.RunInTx`(§7),事务随 `txCtx` 传播——这是 handler 不该直接干、
  单实体 store 也管不到的那一层。只有单实体 CRUD 时不需要 service
  层,别为分层而分层。

---

## 相关文档

- [`config.md`](config.md) —— db 段全部配置项(生成,永不漂移)
- [`design.md`](design.md) §5 —— 数据层架构决策(为什么长这样)
- [`migration-v1-to-v2.md`](migration-v1-to-v2.md) —— v1 用法对照
- [`examples/blog`](../examples/blog) —— 全部概念的可运行样例
