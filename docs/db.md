# 数据层指南：db · store · where

> **读者**：用 chok 写业务的应用开发者。读完你能：装配数据库、定义带安全
> 默认值的模型、完成全部日常读写、正确使用事务与迁移，并清楚每一条安全栏
> 在拦什么。
>
> 本文是**使用指南**（怎么做）。架构决策与契约边界见
> [`design.md`](design.md)，逐项配置见生成的 [`config.md`](config.md)，从 v1
> 迁移见 [`migration-v1-to-v2.md`](migration-v1-to-v2.md)。想直接跑起来，看
> [`examples/blog`](../examples/blog)。代码引用以 `main` 分支为准。

**阅读约定** —— 全文用这几个记号区分内容性质，扫一眼就知道能不能跳过：

| 记号 | 含义 |
|---|---|
| ✅ | 推荐做法，日常照此写 |
| ⚠️ | 陷阱 / 边界，用到相关能力前读一眼 |
| 🚫 | fail-closed 安全栏，框架会主动拒绝的操作 |
| 🔒 | 隔离 / 权限边界——不是"框架会报错"，而是"这里划着一条安全线" |
| 🧪 | 进阶 / 少数派场景，首次阅读可跳过 |

---

## 按任务查找

| 我想…… | 去 |
|---|---|
| 跑起来看效果 | [§2 五分钟上手](#2-五分钟上手) |
| 定义一张表 / 选属主与软删 | [§3 定义模型](#3-定义模型) |
| 字段名要编译期检查（去手打字符串） | [§3.3 字段引用生成](#33-字段引用生成chok-gen-fields) |
| 构造一个 Store、控制可查可写字段 | [§4 构造 Store](#4-构造-store) |
| 按 ID 取一行 / 列表过滤分页 / 只要数量 | [§5 读取数据](#5-读取数据) |
| 仪表盘统计：求和 / 均值 / 极值 / 分组计数 | [§5.7 聚合与分组统计](#57-聚合与分组统计sum--avg--min--max--groupby) |
| 增、改（乐观锁）、删、恢复、批量、Upsert | [§6 写入数据](#6-写入数据) |
| 从请求 DTO 部分更新（PATCH，去 `if != nil` 舞蹈） | [§6.2 `Update`](#62-updatefields--set--patch-怎么选) |
| 跨 Store 同一事务 / 订阅数据变更 | [§7 事务与事件](#7-事务与事件) |
| 建表、版本化迁移、修 dirty、追溯 repair | [§8 迁移](#8-迁移) |
| 接只读副本 / 分析库 | [§9 多实例与只读](#9-多实例与只读) |
| store DSL 写不出的 SQL | [§10 逃生门 Unsafe](#10-逃生门unsafe危险区) |
| 错误码怎么映射成 HTTP | [§11.2 错误处理](#112-错误处理) |
| 写测试 / 组织项目 / 排查报错 | [§12 实践](#12-实践) |

---

## 1. 这一层是什么

数据层由四个角色组成，职责不重叠——记住这张图，本文其余内容都是它的展开：

```
chok.yaml ──▶ db.Module()        连接生命周期 + 迁移模式（kernel 组件）
                   │ db.From(k)
                   ▼
              *db.DB             瘦句柄：RunInTx / Migrate / Ping / Unsafe
                   │ store.New[T](h, logger)
                   ▼
              store.Store[T]     全部读写的唯一入口（白名单 / scope / 事件）
                   ▲
              where.Option       查询表达（过滤 / 排序 / 分页 / 游标）
```

两条设计立场解释了本文所有的"为什么"：

1. **gorm 基本不在公开面。** 业务代码只接触 `*db.DB` 与 `Store[T]`；**返回
   raw gorm 句柄**的只有两扇标着 `Unsafe` 的门（[§10](#10-逃生门unsafe危险区)）。
   另有一个可信扩展边界——自定义 `where.Option` 也能拿到 raw `*gorm.DB`
   并绕过白名单记账（详见 §5.2）。审计越权点要搜 `grep '\.Unsafe('` **加上**
   自定义 `where.Option` 的定义/传入点。
2. **fail-closed 默认。** 属主隔离下**读 / 改 / 删**拿不到登录用户就拒绝、
   没有条件就拒绝（防清表）、未声明的字段就拒绝（白名单）。⚠️ 一个例外：
   `Create` 的属主填充默认更宽松（缺登录用户时 no-op、不报错），要让创建也
   fail-closed 需开 `require_principal`（详见 §6.1）。安全默认值全表见
   [§11.1](#111-安全默认值一览)。

---

## 2. 五分钟上手

**① `chok.yaml`** —— 段在场即装配（`chok sync` 生成装配代码）：

```yaml
db:
  driver: sqlite          # sqlite | mysql | postgres
  sqlite:
    path: app.db
  migrate: auto           # auto | versioned | off（见 §8）
  store:
    require_principal: true   # Owned 模型：无登录用户时连 Create 也拒绝（见 §6.1）
    # admin_roles: [admin]    # 属主隔离的管理员角色（读侧旁路 + 写侧可指定 OwnerID，见 §11.1）
```

**② 模型 + Store + 路由** —— 一个实体的完整闭环：

```go
// model.go
type Post struct {
    db.OwnedSoftDeleteModel // 属主隔离 + 软删除 + RID / 乐观锁 / 时间戳

    Title   string `json:"title"   store:"query,update" gorm:"size:200;not null"`
    Content string `json:"content" store:"update"       gorm:"type:text"`
    Status  string `json:"status"  store:"query,update" gorm:"size:20;default:'draft'"`
}

// RID 前缀：对外 ID 形如 pst_pbERs9oJT0AA，内部自增主键永不外泄。
func (Post) RIDPrefix() string { return "pst" }
```

```go
// routes 回调里（kernel 就绪后）：
posts := store.New[Post](db.From(k), log.From(k)) // 字段白名单随 store tag 声明

p := &Post{Title: "hello", Status: "draft"}
err := posts.Create(ctx, p)                          // OwnerID 自动填当前用户
one, err := posts.Get(ctx, store.RID(p.RID))         // 按对外 ID 取
page, err := posts.List(ctx,
    where.WithFilter("status", "draft"), where.WithCount())
p.Title = "hi"
err = posts.Update(ctx, store.RID(p.RID), store.Fields(p, "title")) // 带乐观锁
err = posts.Delete(ctx, store.RID(p.RID))            // 软删模型 ⇒ 软删除
```

**③ 交给框架建表**（`migrate: auto` 下）：

```go
chok.New("blog",
    chok.Use(db.Module(db.WithTables(db.Table(&Post{})))),
    // ...
)
```

跑得通的完整示例：[`examples/blog`](../examples/blog)——它 README 的六步
`curl` 路径就是本指南的最小实践（健康检查 → 注册并登录 → 建帖 → 只看自己的
→ 乐观锁发布 → 浏览 OpenAPI）。

---

## 3. 定义模型

### 3.1 选择基座

内嵌 `db.Model`（或它的增强版）是强制的——`store.New[T]` 的类型约束只接受
它的内嵌者：

| 内嵌 | 得到 | 适用 |
|---|---|---|
| `db.Model` | 自增 PK + RID + 乐观锁版本 + 时间戳 | 无属主、硬删除的表 |
| `db.SoftDeleteModel` | Model + 软删除（`deleted_at` + `delete_token`） | 需要回收站 / 可恢复 |
| `db.OwnedModel` | Model + `owner_id`（自动属主隔离） | 用户私有数据 |
| `db.OwnedSoftDeleteModel` | 以上全部 | 用户私有 + 可恢复（**最常用**） |

实现 `RIDPrefix() string` 后，`Create` 自动生成 `pst_xxx` 形式的对外 ID。

> ✅ **规则**：API 响应永远用 RID（JSON 名就是 `id`），内部 `uint` 主键不出
> 现在任何对外面。

### 3.2 用 `store` tag 声明字段面

模型自己声明哪些字段**可查（query）**、**可写（update）**：

```go
Title    string `json:"title"    store:"query,update"` // 可过滤、可更新
Content  string `json:"content"  store:"update"`       // 只可更新（大文本不进过滤面）
Secret   string `json:"-"        store:"query"`        // JSON 不可见但可过滤（过滤名 secret）
Internal string `json:"internal"`                      // 无 tag = 两个面都不暴露
```

- tag 值只有 `query`、`update` 两个词，逗号分隔；🚫 **其他值在构造期
  panic**——声明拼错必须炸，不能静默收窄面。
- 过滤名 = JSON 名；`json:"-"` 或无 JSON tag 时用 GORM schema 解析出的
  `Field.DBName`（因此 `HTTPStatus` 与 GORM 一致，过滤名为 `http_status`）。
- 内嵌基座自动向 **query 面**贡献 `id` / `created_at` / `updated_at`；🚫
  **update 面永远不含框架托管字段**（数字主键、RID、version、时间戳、软删
  状态、`owner_id`）。见下方边界。

> 🚫 **托管列不可重开**：即便显式写进 `WithUpdateFields` 或用
> `WithColumnAlias` 改名，也无法把上述托管列放进 update 面。失败分两层：
> 显式列表 / alias 命中托管列是**构造期硬失败**——`store.New` 直接 panic，把
> 装配错误挡在启动阶段；运行期 `Changes` 构造时还有一道纵深防御，返回可
> `errors.Is` 的 `ErrProtectedUpdateField`。确需修数据走可审计的
> [`Unsafe`](#10-逃生门unsafe危险区)。

> ⚠️ **不写 tag 也能跑，但别在生产依赖它**：回退到 JSON tag 自动发现，且每
> 次构造都会打 warn 日志（`store: auto-discovered query fields; ...`）。隐式
> 集合会随结构体悄悄变大，生产代码请显式声明。

### 3.3 字段引用生成：`chok gen fields`

`store` tag 声明的字段面可以再生成一层**编译期引用**——每个带 tag 的模型得到
一个 `<Model>Fields` 结构体变量，值就是白名单的公开字段名（§3.2 的过滤名）：

```bash
chok gen fields --dir ./model     # 缺省 --dir .；可重复传多个目录
```

产出同包的 `chok_fields_gen.go`（与 `chok_modules_gen.go` 同一命名家族，随
代码提交）。§3.2 的 Post 生成出来是：

```go
// Code generated by chok gen fields; DO NOT EDIT.

// PostFields enumerates Post's declared field surface (`store` tags) as
// compile-checked references. Values are the public field names the
// store's whitelists key on; they are stable under WithColumnAlias.
var PostFields = struct {
	Title   string // faces: query, update
	Content string // faces: update
	Status  string // faces: query, update

	ID        string // base model, query-only (resolves to the rid column)
	CreatedAt string // base model, query-only
	UpdatedAt string // base model, query-only
}{
	Title:   "title",
	Content: "content",
	Status:  "status",

	ID:        "id",
	CreatedAt: "created_at",
	UpdatedAt: "updated_at",
}
```

调用点从手打字符串换成引用，拼错字段名从运行时 500 变成编译错误：

```go
page, err := posts.List(ctx, where.WithFilter(PostFields.Status, "published"))
err = posts.Update(ctx, store.RID(rid), store.Fields(p, PostFields.Title))
```

要点：

- **纯编译期的壳**：值就是白名单 map 的 key，运行时解析路径一行未变；裸字
  符串**永远合法**（HTTP 入口 `ListFromQuery` 的字段名来自 URL，只能运行时
  校验）。引用保证的是「模型面里存在」；`WithQueryFields` 收窄后的 store 拒
  收未列入的引用仍是**运行时**行为。
- **值取公开名而非列名**，因此 `WithColumnAlias` 之下引用依然成立（alias 只
  改列、不改 key）。基座三字段 `ID` / `CreatedAt` / `UpdatedAt` 只在 query 面
  （`ID` 解析到 `rid` 列）；`version` 与托管列不生成任何符号——update 面
  **结构上就不存在**可误传的基座引用（真传进去也仍是 §3.2 的运行期防线）。
- **`--check` 做 CI 闸**：重命名模型字段后忘了重新生成，CI 红；重新生成后，
  下游过期引用变编译错——两级闭环（blog 的 CI 就挂着
  `chok gen fields --check --dir examples/blog`）。目录里最后一个 tag 消失
  时，再跑生成会**删除**孤儿生成文件。
- **列性按语法判定**（无类型检查，包暂时编译不过也能再生成）：内建标量/
  指针/`[]byte` 与 `[N]byte`（`uintptr` 是 GORM 不支持的例外，直接报错）、
  实现 `driver.Valuer` 或 `GormDataType() string` 的本地类型（**精确签名**
  ——`Value() (int, error)` 之类不算；签名经**别名**书写仍精确，如
  `type DV = driver.Value`）、`time.Time` / `sql.Null*` / `gorm.DeletedAt`
  / `gorm.io/datatypes` 的**存储类型**（`JSON`/`Date`/`UUID` 等；
  `JSONQueryExpression` 一类查询表达式不是列）直接生成。方法集按 Go 的
  **selector 真规则**计算——最浅层且唯一才算：同层两个 `Value` 歧义、浅层
  错签名方法或同名**字段**遮蔽深层正确方法，都不是 Valuer（运行时同样解析
  为关系）；**别名**继承全部方法，**定义类型**（`type Badge Money`）不继承
  来源方法、但保留底层 struct 匿名嵌入的**提升**（`type Box struct{ Money
  }` 是列）。本地定义类型沿底层形状递归（`type Code string` 含匿名嵌入形态
  是列、`type Children []Child` 是 has-many 关系、`type Stamp time.Time`
  经可转换性仍是列），**泛型按实例化实参替换**（`type Bytes[T any] []T` 的
  `Bytes[byte]` 是 bytes 列，`Bytes[string]` 即 `[]string`——见下）；显式
  `gorm:"type:..."`、serializer tag 或其简写 `gorm:"json"` 对任何**具名**
  字段都是列性证明。**struct 形状的关系**（含 `[]Child` 这类 struct 元素
  容器）上的 `store` tag **跳过并 warn**——运行时 DBName 为空，两侧一致；
  而 GORM **根本建不出 schema 的形状**——标量元素容器（`[]string`、
  `[]Defined`）、**任意定长数组**（relation switch 只收 Struct/Slice
  kind，`[2]Child` 一样炸）、map / chan / func / interface、uintptr /
  complex、`GormDataType()` 返回空串——会在运行时 **abort 整个模型**
  （unsupported data type，对照钉死版本 GORM 实测），生成器在运行时模型上
  对它们**直接报错**，具名/匿名、带不带 tag 一致；致命性还**沿嵌入图传播**
  ：经本地 struct 链间接内嵌 chok 基座同样算模型，展开嵌入所提升的字段会
  重新进入外层 relation gate，嵌入体内的致命形状照样炸外层模型。只有
  `gorm:"-"` 族或全关权限（`gorm:"->:false;<-:false"`；注意 `->:false`
  单独出现即全关）能让字段惰性存在。证明的优先级完全按 GORM 管线：
  **非空 `gorm:"type:..."` 最后执行、是唯一终局证明**——对任何拼写（含
  别名、方法集不可见的跨包类型）都直接成列；serializer 在 GormDataType
  覆盖**之前**执行，本地类型上方法集全可见所以照常是证明，但**跨包具名
  类型上不可见的空 GormDataType 能事后抹掉它**，所以那里 serializer 只算
  未知、需 type: 自证（字面量形状如 `[]string` 无处藏方法，serializer
  照常有效）；`gorm:"type:"` **空值**反向抹掉最终 DataType——本可成列的
  字段变成无 DBName 的死字段（tag 死了会 warn），致命形状照旧致命。泛型
  嵌入按**实例化实参**展开传播（`Box[string]` 提升出的 `Data []string`
  炸模型，`Box[byte]` 的 `Data []byte` 是合法 bytes 列）。基座嵌入自己被
  `gorm:"-"` 或全关权限禁用时整个模型报错（运行时没有 rid，store.New
  必失败）——显式 `gorm:"embedded"` 无视权限、基座照常展开可用。基座
  必须**沿途每层 wrapper 都真的展开**才算数：wrapper 自己是 Valuer
  （整个变成一列）、被 `gorm:"-"`/全关权限惰性化、或字段名未导出时，
  内部基座永远到不了模型 schema——Go 层仍满足 `db.Modeler` 但运行时没有
  rid，这些都直接报错。导出性闸门在 GORM 读 tag **之前**执行：未导出
  字段连 `gorm:"embedded"` 都救不回，藏在导出 wrapper 更深处的未导出层
  同样被查出；wrapper 的展开性无法静态判定（方法集含不可见的
  跨包成员）而又携带基座时，同样报错拒猜。另有一个启用基座时以启用者
  为准。复合泛型实参（`Inner[[]T]`）按闭包语义携带外层绑定，
  `Outer[string]` 提升出的 `Data []string` 照样被查出。无法静态判定列性时（陌生跨包类型、方法集含扫不到的嵌入）同样
  **报错拒猜**：用 type / serializer tag 自证，或去掉 tag。构建约束按生成
  时平台生效（`//go:build`、平台后缀、`_` 前缀文件遵循 go/build 规则）。
- ⚠️ **匿名字段另有一套嵌入规则**（与 GORM 一致，实测钉死）：匿名嵌入只有
  **真 driver.Valuer**、time 可转换形态、或 `GormDataType()` **字面量返回
  "time"/"bytes"**（GORM 嵌入条件豁免的两个值）不进嵌入分支；进了分支后
  按 kind 定生死——struct 展开（嵌入行上的 `store` tag 是死的，生成器跳过
  并 warn）、标量落穿分支仍是普通列、**其余 kind 一律 abort**（invalid
  embedded struct）：serializer、`gorm:"type:"`、非豁免的 `GormDataType`
  返回值都救不了匿名容器，生成器直接报错。显式 `gorm:"embedded"` 更狠：
  **无视权限 tag、连 `[]byte` 都炸**，目标必须是 struct（标量目标是
  no-op，tag 照常生效）。`GormDataType` 返回值无法静态读出（非单一字面量
  return）时**报错拒猜**。同一个 `GormDataType` 类型作**具名**字段：字面量
  非空才是列，**空串会抹掉 DataType 反而炸模型**，struct 形态则降级为普通
  关系。
- ⚠️ **提升（promotion）是语法级扫描的已知边界**：匿名内嵌的**用户**结构体
  （或 `gorm:"embedded"` 字段）内部的 `store` tag，GORM 运行时会提升、生成
  器不展开。本包内可验证的形态会打 warn——包括「全部 tag 都来自嵌入」的
  结构体：只要它直接内嵌 chok 基座就会被点名，而不是静默消失；未导出的
  **匿名**嵌入运行时整体跳过（两侧一致，不告警），而具名 `gorm:"embedded"`
  包装按**字段名**判导出性——目标类型未导出照样提升、照样告警。**跨包**
  嵌入只在已识别为模型
  的结构体上提示；纯靠跨包嵌入承载 tag 的模型对扫描不可见——这是唯一的
  诚实残留。需要时把 tag 上提到顶层结构体。chok 基座（`db.Model` 家族）
  不触发任何 warn。

本指南此后的示例统一用生成引用书写；`"status"` 这类裸字符串与之逐字节等价。

### 3.4 软删除下的唯一约束：`SoftUnique`

普通 `uniqueIndex` 会让"删掉的行"永远占着唯一槽。软删模型的唯一约束改用
`SoftUnique` 声明：

```go
db.Module(db.WithTables(
    db.Table(&User{}, db.SoftUnique("uk_user_email", "email")),
))
```

不同方言用不同索引形状实现同一行为，你不必关心细节：

- **PostgreSQL**：业务列上的 partial unique index，谓词 `WHERE deleted_at IS NULL`。
- **MySQL / SQLite**：`UNIQUE(email, delete_token)` 复合唯一索引。

活跃行的 `delete_token` 恒为空串；软删时框架写入随机 token 释放槽位，
`Restore` 归还空串重新入槽（[§6.5](#65-restore软删恢复)）。

> ⚠️ **两条前提**：`SoftUnique` 只能用于软删模型；每个业务列必须是 schema 里
> 存在的 **NOT NULL** 字段——指针或 `sql.Null*` 会在建表前 fail-fast。
> 🔒 另外 **`versioned` 模式忽略 `WithTables`**（含其中的 `SoftUnique`）：应用
> schema 全部来自迁移文件，索引要在编号 SQL 里按方言手写（PG partial index /
> MySQL·SQLite 含 `delete_token` 的复合唯一索引）。

---

## 4. 构造 Store

```go
posts := store.New[Post](db.From(k), log.From(k))
```

`Store` 是**无状态配置对象、并发安全**。构造走反射有成本，因此：

> ✅ **进程级构造一次**（routes 回调或应用构造函数里），注入 handler 共享。
> ⚠️ 别每请求 `store.New`——有成本，discovery warn 还会刷屏。

### 4.1 字段白名单的解析优先级

query / update 两面各自独立，按以下顺序取第一个命中的来源：

1. **`WithQueryFields` / `WithUpdateFields`** —— 调用点显式列表，**整体覆盖**
   tag。用于给同一模型开不同视图：

   ```go
   // 特权面：服务内部用（引用来自 chok gen fields，§3.3；收窄视图拒收
   // 未列入的引用是运行时行为，引用只保证「模型面里存在」）
   adminUsers := store.New[User](h, logger,
       store.WithQueryFields(UserFields.ID, UserFields.Email, UserFields.Active),
       store.WithUpdateFields(UserFields.Name, UserFields.Email,
           UserFields.PasswordHash, UserFields.Roles, UserFields.Active))
   // 公开面：暴露给 HTTP 的那个
   publicUsers := store.New[User](h, logger,
       store.WithQueryFields(UserFields.ID, UserFields.Email, UserFields.CreatedAt),
       store.WithUpdateFields(UserFields.Name, UserFields.Email))
   ```

2. **`WithAllQueryFields(exclude...)` / `WithAllUpdateFields(exclude...)`** ——
   显式要求"全字段自动发现，除了这些"。
3. **`store` tag** —— 模型自带的缺省声明（[§3.2](#32-用-store-tag-声明字段面)）。
4. **自动发现 + warn** —— 无任何声明时的回退。

### 4.2 常用选项速览

（完整见 godoc。）

| 选项 | 作用 |
|---|---|
| `WithBus(k.Bus())` | 写操作发布 `EntityChanged[T]` 事件，**锚定事务提交**（回滚不发，[§7.2](#72-数据变更事件entitychanged)） |
| `WithStrict()` | 拒绝隐式字段自动发现（无 `WithAllQueryFields/UpdateFields` 时**构造期 panic**）；`ListFromQuery` 未知 URL 参数从忽略变为报错。注：直接 where DSL 的未知字段**始终**报错，与 strict 无关 |
| `WithMaxPageSize(n)` | 该 Store 分页硬上限，**覆盖**句柄 `db.store` 策略的继承值；`WithMaxPageSize(0)` 关闭本 Store 上限（仍受包级 `where.MaxPageSize` 天花板约束）。查询级 `where.WithMaxPageSize` 则**只能进一步收紧**本次查询 |
| `WithDefaultPageSize(n)` | `ListFromQuery` / `HandleList` 未传 `size` 时的默认页大小（直接 `List(ctx)` 不加默认分页 = 无 LIMIT） |
| `WithBeforeCreate/Update/Delete(fn)` | 写前钩子（同步，返回 error 即中止） |
| `WithColumnAlias(field, col)` | 过滤名 → 列名的显式映射 |
| `WithConstraintFields(map)` | 唯一约束 → 公开字段名的声明映射：命中的 `ErrDuplicate` 报字段名（metadata `field`）而不是泄 schema 命名、随迁移漂移的索引名；未声明的保持现状（metadata `constraint`）。跨方言要同时声明索引名（PG/MySQL 报它）与列清单（SQLite 只报列，SoftUnique 含 `delete_token`），见 §11.2 |
| `WithScope(fn)` | 追加自定义 scope（在属主隔离之上再叠条件） |
| `WithReadOnly()` | 声明这是只读 Store，所有写方法返回 `db.ErrReadOnly`（[§9](#9-多实例与只读)） |
| `WithoutOwnerScope()` | ⚠️ 关闭属主隔离——构造期打 warn，确认你真的要全局可见 |

---

## 5. 读取数据

### 5.1 定位一行：`Get` + Locator

```go
p, err := posts.Get(ctx, store.RID("pst_abc123"))        // 对外 ID（最常用）
p, err := posts.Get(ctx, store.ID(42))                   // 内部主键（仅服务内部）
p, err := posts.Get(ctx, store.Where(                    // 条件定位，多行匹配取第一行
    where.WithFilter(PostFields.Status, "review")))
p, err := posts.Get(ctx, store.RID(rid), store.WithPreload("Author"))
```

没有命中返回 `store.ErrNotFound`（可 `errors.Is`）。三种 Locator（`RID` /
`ID` / `Where`）在 `Update` / `Delete` / `Restore` / `Exists` 中通用，是
CRUD 的"who"轴。

> ⚠️ `WithPreload` 会把 Store 的 scope **重新施加到关联查询**上，预加载不会
> 泄露别人的行——比裸 `Unsafe().Preload()` 安全。

### 5.2 列表与过滤：`List` + where DSL

```go
page, err := posts.List(ctx,
    where.WithFilter(PostFields.Status, "published"),           // =
    where.WithFilterOp(PostFields.CreatedAt, where.Gte, t0),    // Eq/Ne/Gt/Gte/Lt/Lte
    where.WithFilterIn(PostFields.Status, "draft", "review"),   // IN
    where.WithFilterContains(PostFields.Title, "go"),           // LIKE %go%（通配已转义）
    where.WithOrder(PostFields.CreatedAt, true),                // desc
    where.WithPage(1, 20),
    where.WithCount(),                                          // ← 要 Total 必须给
)
for _, p := range page.Items { /* ... */ }
total := page.Total
```

返回 `*store.Page[T]`：

```go
type Page[T any] struct {
    Items []T            `json:"items"`
    Total int64          `json:"total"`           // 见下方注意
    Meta  where.PageInfo `json:"meta,omitzero"`   // 生效的 page/size/offset/has_more
}
```

> ⚠️ **`Page.Total` 只在传了 `where.WithCount()` 时才计算**，否则恒为 0——
> 总数是一条额外的 COUNT 查询，按需付费。只要数字不要行，用
> [`Count`](#53-只要数字count)。
>
> `Meta`（`where.PageInfo`）报告 SQL 实际生效的分页：`Page` 仅在用 `WithPage`
> 时非零且从 1 起，`Size==0` 表示无 limit，`HasMore` 只有走了 count 的 List
> 才有意义（否则恒 `false`）。

过滤字段一律经查询白名单解析，未声明的字段会被拒绝（不是静默忽略）——哨兵
是 `where.ErrUnknownField`，**按错误来源划界**：`List` / `Count` / `Pluck*` /
`ListIn` / 游标字段 / locator 这些**程序化入口**的字段名由服务端代码书写，
未声明即编程 bug，错误**原样返回**（可 `errors.Is`；`MapError` 不认识它 →
500，惊动监控而不是伪装成客户端传参错误沉底）。只有 `ListFromQuery` 链的
字段名来自 URL，仍映射 400。
`WithFilterNull` / `WithFilterNotNull` 的目标须是**已声明**的列（否则
`ErrUnknownField`）——框架不校验可空性，用在 NOT NULL 列上不报错、只是恒真 /
恒空；
`WithFilterContains` / `StartsWith` / `EndsWith` 对用户输入的 `%`、`_` 做了
转义，不会穿透（要原样通配用 `WithFilterLikeRaw`，自己负责）。

> 📌 **约定：客户端输入进 where DSL 之前先过界。** 划界的代价是——handler
> 若把客户端给的**字段名**直接拼进 `WithFilter` / `WithOrder`，typo 会以
> 500 而非 400 浮出。客户端字段名走 `ListFromQuery`（它按输入校验并映射
> 400），或先在 handler 层校验成白名单内的值再进 DSL。**值**（page/size/
> 游标 token/过滤值）不受影响：`where.ErrInvalidParam` 在所有入口都映射
> 400，`WithPage(page, size)` 直接吃客户端分页值仍然安全。

> 🧪 `where.Option` 是已公开的高级扩展点，但自定义函数会直接拿到 `*gorm.DB`
> 与可写查询元数据，能绕过字段白名单和 `Where` 写保护的条件记账。它与
> `Store.Unsafe` 同属**可信应用代码边界**，不可由请求参数动态拼装；常用表达
> 式（含 `IS NULL` / `IS NOT NULL`）优先用内建 helper。

### 5.3 只要数字：`Count`

```go
n, err := posts.Count(ctx)                                     // scope 内全量
n, err := posts.Count(ctx, where.WithFilter(PostFields.Status, "draft"))
```

分页 / 排序选项被剥离（`Count(WithPage(1,1))` 仍是全量总数），软删行默认
不计。要的不是行数而是求和 / 均值 / 极值 / 分组计数，见
[§5.7 聚合](#57-聚合与分组统计sum--avg--min--max--groupby)——同一读语义。

### 5.4 其他读法

```go
ok, err := posts.Exists(ctx, store.RID(rid))          // 只探存在性，不取数据
items, err := posts.ListByIDs(ctx, []uint{1, 2, 3})   // 主键批取（服务内部）

// HTTP 列表页直通：?page=&size=&order=field:desc&<声明过的字段>=值
// ⚠️ ListFromQuery 总是带 WithCount()，每次都会跑一条 COUNT。
// 返回与 List 同形的 *store.Page[T]（Items/Total/Meta）；它是 *Store 上的
// HTTP 糖，不在 Reader 接口里——数据接口不掺 URL 形状。
page, err := posts.ListFromQuery(ctx, r.URL.Query())

// 游标分页（深分页 / 无限滚动；O(1) 不随页深退化）
// cursor 是**不透明令牌**：首页传 ""，之后把 cp.NextCursor 原样传回
cp, err := posts.ListWithCursor(ctx, PostFields.CreatedAt, where.CursorAfter, cursor, 20)
// cp.NextCursor 为空即最后一页；cp.Items 保证非 nil
```

> 游标是**复合 keyset `(field, rid)`**：按非唯一列（如 `created_at`）分页时由
> 公开 RID 打散等值边界，**不会跳行**；数字主键不进任何客户端可见令牌。令牌
> 绑定格式版本 / 字段 / 方向——换任何一样，旧令牌返回 400 而不是从错误位置
> 静默继续；过滤条件**不**绑定（复用旧令牌换 filter 得不到 filter 本身给不了
> 的东西，scope 照常生效），跨页保持 filter 稳定是调用方的契约。客户端**不得
> 解析或构造**令牌——Kind 期望由**零值行跑编码器完整管线**推导（serializer /
> `driver.Valuer` 全都按 **wire 类型**定 Kind：`datatypes.Time` 是 str、
> `serializer:unixtime` 的 int64 是 time），伪造类型标签、窄整数越界（如
> int8 列塞 int64）、NaN 一律 400。游标列必须 NOT NULL 且 Kind **静态可推
> 导**：普通/defined 标量、其指针、零值探针能给出标量 wire 样本的
> serializer/`driver.Valuer` 字段；后两者还必须让所有值保持稳定的 wire Kind。
> `sql.Null*`（零值即 NULL）与 `[]byte` 这类推不出的字段**入口即拒**；实际边界
> 在签发前会按推导出的 Kind / 位宽 / 值域再次自校验，动态 Valuer 的类型漂移、
> NaN、RFC3339 无法表示的时间、**无效 UTF-8 字符串**（JSON 会静默替换成
> U+FFFD，令牌解码后不等于真实边界）都作为服务端字段契约错误拒签，绝不返回
> 下一页无法消费或**变形**的令牌。确认还有下一页时遇到 NULL 边界值同样会
> **报错**而非静默截断。
> **尺寸纪律（公开契约）**：客户端令牌 ≤ **4KB**（`store.MaxCursorTokenLen`），
> 超限在任何 base64/JSON 解码发生前就 400 拒收；边界值的字符串表示 ≤ **1KB**
> （`store.MaxCursorValueLen`），超限——或 JSON 转义膨胀后组装出的令牌超过
> 4KB——在签发侧作为**服务端错误**拒签。游标列是短标量键不是 payload，
> 框架绝不把值截断进令牌。
> tie-breaker 直接绑定模型的 RID 列，**不要求**把 `id` 暴露进查询白名单。
> 服务端内部自组 keyset 时用 `where.WithCursorBy` / `WithCursorByField`。

把列表页直接挂成路由只要一行（blog 在用）：

```go
api.Handle(http.MethodGet, "/posts", handler.HandleList[Post](posts))
```

### 5.5 单列投影与两步 IN：`Pluck`

只要某一列的值，或要做"先取一批 ID、再跨表查"，用 `Pluck` 系列而不是
`Unsafe`——字段同样过查询白名单、scope 与软删规则照旧，Store 漏不出它不暴露
的列（这几个是自由函数：`F` 是列的 Go 类型，`T` 由 Store 推断）：

```go
// 投影单列；PostFields.ID（键 id）解析为公开 RID，内部数字键要用 PluckIDs
titles, err := store.Pluck[string](ctx, posts, PostFields.Title,
    where.WithFilter(PostFields.Status, "published"))
statuses, err := store.PluckDistinct[string](ctx, posts, PostFields.Status)   // 去重

// 两步 IN：先取一批内部主键，再喂另一个 Store 的 ListByIDs（两侧白名单都在场，
// 强过手写一条穿透白名单的 JOIN + Unsafe）
ids, err := store.PluckIDs(ctx, posts, where.WithFilter(PostFields.Status, "published"))
comments, err := commentStore.ListByIDs(ctx, ids)

// 值集超过 where.MaxInList（500）时用 ListIn：自动去重 + 分块，每块都走
// List 的白名单 / scope / 软删路径，语义等价一条大 IN 的**集合语义**——
// 值集 Go 等值去重之外，跨块结果还按主键去重（大小写不敏感 collation 下
// 数据库等值比 Go 宽，同一行可能命中两块）。空值集也走一次退化查询：
// 白名单 / 守卫 / fail-closed scope 照常校验，不会静默空页。只收过滤
// option（排序 / 分页 / count 跨块不可组合，直接拒绝），结果不保证顺序、
// 不受 Store 页大小 cap 限制——这是按值集定大小的服务端管道，对准键形
// 字段用。多块 = 多条 SELECT 非单语句：并发写下合并结果不是单快照读；
// 事务能救回来的前提是隔离级别给出**事务级快照**（见代码块下方说明）
items, err := store.ListIn(ctx, bookStore, BookFields.SourceID, ids,
    where.WithFilter(BookFields.Status, "active"))
```

> ⚠️ **跨块快照一致性按方言诚实声明**：`Store.Tx` / `db.RunInTx` 以数据库
> **默认隔离级别**开事务（空 `sql.TxOptions`）。SQLite（事务独占唯一写
> 连接，不存在并发写者）与 MySQL/InnoDB（默认 REPEATABLE READ，首读定
> 快照）在事务内即得事务级快照；**PostgreSQL 默认 READ COMMITTED 每条
> 语句取新快照，放进事务也不够**。chok 不提供隔离级别旋钮；PG 上确需
> 跨块快照时，在事务首句经 Unsafe 执行
> `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`
> （`h.Unsafe(txCtx)` 是事务感知的），或接受逐块快照。

### 5.6 看见软删行（管理 / 回收站视图）

```go
page, err := posts.ListQ(ctx,
    []store.QueryOption{store.WithOnlyTrashed()},      // 或 WithTrashed()：活 + 删都要
    where.WithCount())
```

> 🔒 scope 依旧生效：软删行可见，但依然只有属主 / 管理员能看到自己的。

### 5.7 聚合与分组统计：`Sum` / `Avg` / `Min` / `Max` / `GroupBy`

仪表盘统计（求和、均值、极值、分组计数）用聚合正门，别下 `Unsafe`——
字段照走查询白名单、scope 与软删规则照旧、filter 收窄统计范围，读语义
与 `Count` 完全一致。与 `Pluck` 同理都是自由函数（方法不能引入类型
参数），分页/排序选项按 `Count` 先例剥离：

```go
// 单值聚合：返回 (值, ok, error)。SQL 聚合忽略 NULL；零行或全 NULL 时
// 数据库返回 SQL NULL —— Go 侧 ok=false + 零值，绝不与「合法的 0」混淆。
total, ok, err := store.Sum[int64](ctx, orders, OrderFields.Amount,
    where.WithFilter(OrderFields.Status, "paid"))
avg, ok, err := store.Avg(ctx, orders, OrderFields.Amount)          // 恒为 float64
latest, ok, err := store.Max[time.Time](ctx, orders, OrderFields.CreatedAt)
users, err := store.CountDistinct(ctx, orders, OrderFields.CustomerID) // COUNT 无 NULL，无 ok

// 分组聚合：按一个白名单列分桶，每组一到多个聚合值（位置对应）。
// 结果恒按 group key 升序（确定性输出）。
groups, err := store.GroupBy[string](ctx, orders, OrderFields.Status,
    []store.Aggregate{store.CountRows(), store.SumOf(OrderFields.Amount), store.MaxOf(OrderFields.CreatedAt)},
    where.WithFilterOp(OrderFields.CreatedAt, where.Gte, since))
for _, g := range groups {
    n, _ := g.Values[0].Int64()      // CountRows / CountDistinctOf → int64
    sum, _ := g.Values[1].Int64()    // SumOf(整数列) → int64；浮点列 → Float64()
    at, _ := g.Values[2].Time()      // MinOf/MaxOf(时间列) → time.Time
    _ = g.Key                        // K 必须与列的 wire kind 精确匹配（见下）
    _, _, _ = n, sum, at
}
```

**列类型规则**（复用游标的 schema wire-kind 探针，serializer /
`driver.Valuer` 字段按 wire 类型判定）。能力矩阵有两半：**wire kind
管 Go 结果收敛，数据库真实列型管操作是否合法**——真实列型读自
**catalog 纯元数据**（SQLite `pragma_table_info` / PG `pg_catalog` /
MySQL `information_schema`，首次聚合时懒解析并缓存；**绝不采样数据表**——
gorm 的 `ColumnTypes` 会跑无 scope 的 `SELECT * ... LIMIT 1`，刻意
不用；也不是 `FullDataTypeOf` 渲染的「模型将建成什么」——
`migrate: versioned/off` 下真列可能与模型不符），按方言用
**精确白名单**匹配（不用子串——
子串会把 PG 的 `interval`/`int4range` 当整数、`daterange` 当时间、
`time`/`timetz` 当瞬间、`integer[]` 数组当整数）。`int64` 字段配
真实 `TEXT` 列这类错配在入口 fail-closed（否则 SQLite 按文本字典序
算 MIN、PG 运行期报错），range / interval / 数组 / 纯时刻(time)等
不认识的列型同样拒绝并指向 Unsafe（catalog 列名在 SQLite/MySQL 按
**ASCII** 大小写不敏感匹配——`versioned/off` 建的 `QTY` 列照样认，
与数据库自身的标识符比较一致，不做完整 Unicode 折叠；PG 保留 quoted
标识符大小写）。catalog 读按**数据查询同款规则**解析表名：限定
`TableName`（`main.t`/`schema.t`/`db.t`）按**方言引用符**做
quote-aware 拆分——引号内的点是数据不是分隔符，且引用符本身分方言
（PG 是 `"`、MySQL/SQLite 是反引号，故 `"a.b".t` 在 PG 是两段、在
MySQL/SQLite 是三段），拆完再用 GORM 自己的 quoter **回渲校验**，
不一致即 fail-closed；PG 未限定名经 `to_regclass` 沿**整条**
`search_path` 解析，不只 `current_schema()` 表头。PG 列型沿
`typbasetype` 递归解 domain 至**最终基类型**、且仅认 `pg_catalog`
命名空间的内建名——用户 domain/enum 渲染为 schema 限定名，永不入
白名单（裸 `typname` 可被 `CREATE DOMAIN` 冒充内建名）；catalog
缓存按解析出的 **relation OID** 分键（`sync.Map`，各 relation 首访
互不串行、不复制既有条目；同一 relation 的并发冷首访按键
singleflight 合并成一次元数据读，leader 被取消时健康等待者回流
二次合并——显式事务调用者除外：它持连接停靠会在有界连接池上与
读取方互等，且事务可能看到自己未提交的 DDL，故它在自己连接上
直读、结果先只供本次调用，**成功提交后才经 after-commit 缓冲
发布**（回滚整体丢弃；纯事务 workload 由此回暖缓存）；共享条目
恒为已提交状态，事务命中缓存的读仍安全），`SET LOCAL search_path` 的
schema-per-tenant 形态下同一 Store 命中不同表时各持各的列型（动态
search_path 仅事务内 `SET LOCAL` 形态连贯）。属主/自定义 scope
**先于** catalog 读执行：未认证
请求在纯内存的 fail-closed 阶段即被拒（401），根本不碰数据库：

| 函数 | 接受的列 | Go 侧类型 |
|---|---|---|
| `Sum[N]` / `SumOf` | 整数（含指针）、浮点 | `Sum[int64]` 仅限整数列——**精确**，超 int64 值域响亮报错、绝不静默截断（SUM(int) 在 PG 返 bigint/numeric、MySQL 返 DECIMAL、SQLite 动态类型，收敛函数逐 lane 测试钉死）；`Sum[float64]` 也可拓宽整数列（>2^53 精度取舍）。`SumOf` 按列定：整数列 → `Int64()`，浮点列 → `Float64()` |
| `Avg` / `AvgOf` | 整数、浮点 | 恒 `float64`（三方言 AVG 都返回小数类型）。⚠️ 金额级精确统计不要用它——精确小数聚合是 raw SQL 的活 |
| `Min[N]` / `Max[N]` / `MinOf` / `MaxOf` | 数值列之外**放开时间列**（“每组最新 created_at”是真实仪表盘需求；MIN/MAX 是序运算不是算术） | 数值同 Sum；时间列 → `time.Time`，**按瞬间比较**（见下方 SQLite 条目），比较结果用 `Equal` 而非 `==`（返回值的墙钟时区随方言） |
| `CountDistinct` / `CountDistinctOf` | **可比较标量列**（六个可推导 kind：str/int/uint/float/bool/time）——数据库比较不了的列构造期即拒，不等运行期报错；`gorm:"type:json"` 声明的列即便 Go 侧是 string 也拒（PG `json` 无等值运算符，JSON 文档不是跨方言可比较标量，分组/去重计数同拒） | `int64`；COUNT 永不为 SQL NULL。⚠️ 字符串基数遵循列的 collation（CI collation 下 Go 认为不同的值算一个），各方言自定 |

- **NULL 语义**：SQL 聚合忽略 NULL；单值入口零行/全 NULL → `ok=false`；
  GroupBy 聚合值用 `AggValue.IsNull()`（访问器此时一律返回 ok=false）。
  🚫 **NULL group key 直接报错**（SQL 的 NULL 组 ≠ 零值组，Go 侧折叠等于
  合并两个不同答案）——按 NOT NULL 列分组，或加
  `where.WithFilterNotNull(field)`。
- **group key 类型**：`K` 与列 wire kind 精确匹配——string / int64 /
  uint64 / float64 / bool / time.Time 六选一，进查询前校验，错配即拒。
- **AggValue 访问器纪律**：`Float64()` 可拓宽 int64 值（仪表盘一个访问器
  读遍数值聚合）；`Int64()` 拒绝浮点值（截断方向不放行）；`Time()` 只认
  时间值。kind 由构造该位置的 `Aggregate` 静态决定，调用点写死即可。
- **选项纪律**：单值聚合剥离分页/排序（与 `Count` 一致，总量形状剥了
  不说谎）；🚫 **GroupBy 拒收非 filter 选项**——行集结果上静默吞掉
  `WithOrder`+`WithLimit` 会伪装成 top-N（`ListIn` 守卫同理，报
  `ErrInvalidParam` → 400）。
- **时间聚合按瞬间比较，两条方言注记**。① SQLite 把时间戳按写入方时区
  存成文本、按字典序比较——混合时区偏移下裸 MIN/MAX 会选错瞬间、同一
  瞬间两种偏移写入会分成两组。聚合读取时间列时自动经 `strftime` 归一化
  到 **UTC / 毫秒精度**（与 MySQL DATETIME(3) 的写入精度一致；亚毫秒
  差异折叠），返回 UTC 值；数值存量按 **Unix 秒**读取（typeof 分支，
  不用 SQLite 的 'auto' 启发——它会把 1970 年头 63 天的 Unix 秒误读成
  Julian day；Julian REAL 刻意不支持）。这只影响聚合，存储值、filter
  与排序不动。② **MySQL 的时间列是 DATETIME，存裸墙钟；chok 把写入
  基准双钉在 UTC**（驱动 `Loc=time.UTC` 管 DATETIME 读写；每连接
  `SET time_zone='+00:00'` 管 SQL 侧求值——软删写 `deleted_at` 的
  `CURRENT_TIMESTAMP`、用户 SQL 的 `NOW()`、TIMESTAMP 列转换，否则
  这半边悬在服务器时区上，与驱动侧分叉成第二条基准）。chok 写入方
  因此**结构性正确**：进程 TZ 任意、跨实例任意混合、Go 值带什么时区
  都无所谓——UTC 无 DST 转换，瞬间→墙钟是单射，不存在秋季回拨把两个
  瞬间折成同一墙钟的折叠（America/New_York 的 11 月切换日 05:30Z 与
  06:30Z 都是 01:30——v2.0.0-beta.6 及更早的 `Loc=time.Local` 下这在
  单进程内就会发生）。残余约束只剩一条**外部写入方须知**：DATETIME
  不带时区，框架管不到别人的连接——同库的非 chok 写入方必须同样按
  UTC 墙钟写入，否则同一瞬间两种墙钟、读取侧无法修复。这条约束同样
  覆盖排序 / 范围过滤 / 游标，不只聚合。运维注记：`time_zone` 经
  每连接 SET 下发（与只读句柄的 transaction_read_only 同通道；
  charset 的 SET NAMES 是另一条连接建立期路径，同属会话状态）——
  若中间隔着会话复用型代理（ProxySQL 一类），须确认其保持 session
  状态与后端连接的绑定，否则 SQL 侧求值会退回服务器时区（驱动侧
  DATETIME 读写不受影响）。PG（timestamptz）写入即归一
  UTC，无此题。**存量迁移**：≤ v2.0.0-beta.6 写入的库按**来源**
  逐列重基——旧基准其实有两个时区（驱动写入列=旧**进程**时区；SQL
  求值列=旧 **session**（通常即服务器）时区），两者都是 UTC 才可整体
  跳过。驱动写入的 DATETIME（created_at/updated_at/业务字段）按旧
  进程时区、SQL 求值的 DATETIME（软删 `deleted_at`、NOW() 喂的列）
  按旧 session 时区 `CONVERT_TZ` 到 +00:00；**参数写入的 TIMESTAMP
  列**（含框架迁移账本 applied_at/claimed_at 等）在旧进程≠旧 session
  时区时内部瞬间偏斜两者之差（旧读取的反向抵消掩盖了它，UTC 对称
  读取会暴露），按 `CONVERT_TZ(col, '<旧进程>', '<旧 session>')`
  重基；纯 `DEFAULT CURRENT_TIMESTAMP` 生成的 TIMESTAMP 值瞬间本就
  正确、无需处理——但这个免迁/须迁之分是**按行**不是按列：混合来源
  的列只转参数写入的行（整列盲转会把正确行搬错同样的差值；chok
  账本恰有此混合——≤beta.4 的 applied_at 走 DEFAULT、beta.5 起参数
  写入，账本只转 `provenance IN ('applied','baseline')` 的行——旧引擎
  写入时即打标，正向谓词对 beta.4 时代的空串行（DEFAULT 写入、本就
  正确）fail-safe。**全部重基须在新版首次启动之前、停写窗口内完成**——
  停的是**该库全部写入方**：旧 chok 实例之外还包括外部服务、
  ETL/定时任务、运维 SQL（正是不变量里那些「非 chok 写入方」）——
  配方转换的是冻结快照，窗口内任何写入不是漏转就是被双转（窗口内
  禁止滚动升级：旧实例事后再写留下未转换行、新实例提前启动触发
  兜底），且用**新**二进制跑
  `chok migrate` 的**写命令**本身就算首启——裸 `up` 以新基准写
  应用账本、`up --all-owned`/`--component` 还会刷新 manifest、
  `repair mark-applied` 重写账本行 applied_at；`status` 纯读、
  窗口内可跑：首启的新写入带着同样的标与列、事后重基会把正确的
  新值反向搬歪。beta.4 直跳的库连这些列都没有——语句响亮
  失败即是信号：纯 beta.4 账本全为 DEFAULT 写入、本就无需重基，
  跳过即可；若已先启动，账本语句加版本边界（AND version <=
  升级前最高版本）、manifest 只转**升级前已存在 kind** 的
  claimed_at（首启时新 claim/adopt 的行是新基准，数据侧无标可分，
  kind 集合只有运维者知道）；新版若还跑过 repair：被新版**重建或刷新**过的
  version 都要从边界内剔除——mark-applied 重写的，和 retry 删行后
  再次 up 以同 version 重建的；repair history 以升级前
  MAX(id) 为界而非整列；任何边界不明的记账表整体跳过——偏斜仅
  记账留痕。**业务表没有这个豁免**，且 id 边界只是半条线：
  AND id <= 升级前 MAX(id) 只能排除首启后**新插入**的行；新版会在
  存量行上**改写**的列——autoUpdateTime 的 updated_at、迟到软删的
  deleted_at、登录刷新的 last_used_at——UPDATE 不改 id、值上无标
  可分（与上文 manifest updated_at 同性质）。插入型列
  （created_at/不可变事件戳）按 id 界转；被首启后流量可能触碰过的
  可刷新列没有正确的就地转换——回滚备份、按正确顺序重来）。免迁的 DEFAULT 值另有一条**读侧披露**：旧读取原本
  把它们偏斜 (旧 session−旧进程) 返回，升级后 API 可见瞬间被校正
  这个差值（数据不动、可见值移动）。**DATE 列**存量不动（历日无时区可重基），但写入
  契约随基准改变：存的是**瞬间的 UTC 历日**——date-only 值请以 UTC
  午夜构造（`time.Date(y, m, d, 0, 0, 0, 0, time.UTC)`），东偏时区
  的本地午夜此后落到前一 UTC 日，读回为存量历日的 UTC 午夜。四条执行纪律：①**命名时区先跑探针**
  （`SELECT CONVERT_TZ('2026-01-01 00:00:00','<ZONE>','+00:00')` 非
  NULL 才继续——tz 表未装载/时区名错时它**静默返 NULL**，可空列如
  `deleted_at` 会被写成 NULL、软删行复活）；②**每条 UPDATE 前按同
  谓词扫描数据**（converted IS NULL 或 converted=原值 的计数须为
  0——超出 CONVERT_TZ 支持范围的值**原样返回不报错**，1960 年史料
  能过探针却不被转换；固定偏移的范围外行改用区间算术
  `DATE_SUB(col, INTERVAL ...)`，命名时区的走应用侧处置）；③**单
  事务整跑前核验目标表全为 InnoDB**（information_schema.tables 引擎
  扫描须空——非事务表无视 ROLLBACK、留下半迁）；④**配方不可重复
  执行**（二遍越过正确瞬间）——无事务则逐语句记完成点、状态不明
  回滚备份。完整
  配方见根目录 CHANGELOG 的 Breaking 条目（可执行形态由
  `TestMySQLUTCBaseline_LegacyRebaseRecipe` 及其姊妹测试 pin 住）。
- 🚫 **字段名 typo 是服务端 bug**：原样返回 `where.ErrUnknownField`
  （→ 500），provenance 划界与 `Pluck`/`List` 一致（§11.2 例外①）。
- 字符串/布尔列不可聚合（文本 MIN/MAX 的序由方言 collation 定义，跨
  方言不可承诺）；`Sum`/`Avg` 不接受时间列。bool group key 只认规范的
  0/1 存储值——`Unsafe`/动态类型写入的 2 会在 SQL 侧成为独立组，Go 侧
  折叠成重复的 `true` key 会静默互相覆盖，因此**响亮报错**。

> **top-N 与 HAVING 刻意不做**：按聚合值 ORDER BY 是表达式排序（红线，
> 无法白名单化），HAVING 是聚合结果上的表达式谓词（同类）。GroupBy 结果
> 集的大小 = 分组列的 distinct 值数——仪表盘形状的列（status/type/日期
> 桶）就是几十上百行，**在内存排序/过滤后取前 N**：
>
> ```go
> slices.SortFunc(groups, func(a, b store.Group[string]) int {
>     x, _ := a.Values[1].Int64()
>     y, _ := b.Values[1].Int64()
>     return cmp.Compare(y, x)          // 按 sum 降序
> })
> top := groups[:min(10, len(groups))]
> ```
>
> 组基数真到内存吃不下的量级（如按 user_id 分组的百万组 top-N 下推），
> 那是 `Unsafe` 的正当用途（逃逸应当稀少而非为零）。多列 GROUP BY 也不
> 在 v1（结果形状没有 codegen 无法类型化，backlog #4 之后再议）。

---

## 6. 写入数据

所有写方法先过 `rejectWrite`（只读 Store 直接 `db.ErrReadOnly`）再执行 SQL。
前置钩子**按方法分派**——不是每个写方法都有：

| 方法 | 前置钩子 |
|---|---|
| `Create` / `BatchCreate` / `Upsert` / `BatchUpsert` | `WithBeforeCreate` |
| `Update` / `BatchUpdate` | `WithBeforeUpdate` |
| `Delete` | `WithBeforeDelete` |
| `Restore` | ⚠️ **无**——靠钩子做审计 / 校验 / 副作用时，恢复操作会被漏掉 |

`WithBeforeUpdate` 的回调收到**已解析的 `ChangeSnapshot`**（公开字段名 →
即将写入的值；`Fields(&obj)` 全白名单更新会展开成完整字段集），访问器返回
递归拷贝——钩子能内省、不能改写（要改值，在调用方或 `WithBeforeCreate`
里做，后者拿到的是可变对象）。Changes 的静态校验（更新白名单 / 托管列）在
钩子**之前**执行：钩子只会看到结构合法的变更集。

### 6.1 `Create` / `BatchCreate`

```go
p := &Post{Title: "hi"}
err := posts.Create(ctx, p)
// p.RID / p.Version / p.CreatedAt 已回填；有登录用户时 Owned 模型的 OwnerID
// 自动填成当前用户（普通用户请求体自带的 OwnerID 被忽略，防伪造属主）

err := posts.BatchCreate(ctx, []*Post{p1, p2, p3})   // 整批一个事务，中途失败全回滚
```

唯一约束冲突返回 `store.ErrDuplicate`。

> ⚠️ **缺登录用户时 Create 的属主填充默认宽松**：不同于读 / 改 / 删的
> fail-closed，`Create` / `BatchCreate` 在 ctx 无 principal 时默认 **no-op**
> （不写 `OwnerID`、不报错），方便后台任务 / 测试用预设 `OwnerID` 建行。要让
> 创建也在缺登录用户时拒绝（`apierr.ErrUnauthenticated`），开 `require_principal`
> ——chok.yaml 的 `db.store.require_principal: true`（§2 已开）或构造时
> `store.WithRequirePrincipal()`。HTTP 面建议开启。
>
> ⚠️ **管理员例外**：principal 持有管理员角色时，请求体显式的 `OwnerID` 会被
> **尊重**（仅为空时才自动补填）——这是管理导入 / 跨用户写入的既定逃生门；只有
> 非管理员的 `OwnerID` 才被无条件覆盖。角色名单按
> `store.WithAdminRoles`（每 Store）→ `db.store.admin_roles`（应用级）→
> 包默认 `["admin"]` 解析，**构造期定格、读写两侧共用同一份**：跳过属主过滤
> 的角色和可显式指定 `OwnerID` 的角色永远一致（见 §11.1）。

### 6.2 `Update`：`Fields` / `Set` / `Patch` 怎么选

先记住一条不变量：**`version` 是行修订号，每次成功的普通 `Update`（以及软删
/ 恢复）都在同一条 SQL 中把它 +1**，与你是否要乐观锁无关。乐观锁只是"更新
前先断言旧 version 相等"这个额外条件。

```go
// ✅ 首选：Fields —— 从对象取值，自动带上 obj.Version 做乐观锁
p, _ := posts.Get(ctx, store.RID(rid))
p.Title = "new title"
err := posts.Update(ctx, store.RID(rid), store.Fields(p, PostFields.Title))
// 版本被并发改过 ⇒ store.ErrStaleVersion（HTTP 409），重读重试

// Set：裸 map，无隐式乐观锁 —— 用于计数器类"最后写赢"的字段
err := posts.Update(ctx, store.RID(rid), store.Set(map[string]any{PostFields.Status: "published"}))

// 对 Set 手动加锁 / 对 Fields 显式覆盖版本：
err := posts.Update(ctx, store.RID(rid), store.Set(m), store.WithVersion(v))

// 确实不要锁时，把意图写进代码（管理员强制改状态，忽略版本）：
err := posts.Update(ctx, store.RID(rid), store.Fields(p, PostFields.Status).NoLock())
```

规则要点：

- 列名走 **update 白名单**；`Fields` 中的**零值也会被写入**（框架用
  `Select` 绕过 GORM"跳过零值"的默认，避免把字段清空的更新被静默丢掉）。
- `Set` 或 `.NoLock()` 只是不做旧 version 条件检查，**不会停止修订号推进**；
  这两种无锁写无法可靠回填调用方对象的最终 version，需要时重新读取。
  （`WithVersion(v)` 的 `v <= 0` 同样表示"不加锁"，`Fields(obj)` 的
  `obj.Version == 0` 也不会生成 version 条件。）
- 🚫 `Fields` 的对象必须是该 Store 的**具体模型类型**（`T` 或 `*T`），不接受
  字段形状相同的 DTO——保证字段值与乐观锁元数据始终来自同一份 GORM schema。
- 需要知道命中几行（尤其 `Where` 批量更新）时挂 `store.WithRowsAffected(&n)`。
- 版本冲突时**不能**重读重试（必须赢）的序列 → 事务内悲观锁
  `GetForUpdate`，见 [§7.1](#71-一个事务串起多次写)。

**部分更新走 `Patch(req)`**——从请求 DTO 的非 nil 指针字段推导变更集，
消灭 handler 里每字段一遍的 `if req.X != nil` 舞蹈（Ecto changeset 的 cast
对应物；`Fields` 拒收 DTO 正是给 Patch 留的正门）。它是第三个 `Changes`
构造器，与任意 Locator、任意选项自由组合，写内核零改动：

```go
// req 是 *updatePostRequest{ Title, Content, Status *string }
p, _ := posts.Get(ctx, store.RID(rid))
pc := store.Patch(req).Onto(p)   // 非 nil 指针 → 变更集；Onto 带上 p.Version 做乐观锁
if pc.IsEmpty() {                 // 客户端没发任何可更新字段 → 无事可做
    return p, nil
}
err := posts.Update(ctx, store.RID(rid), pc)
// 成功后 p 已含新值与推进后的 Version；给模型加一个可更新字段，handler 零改动

// 纯写流（免读回程）：裸 Patch + 显式版本
err = posts.Update(ctx, store.RID(rid), store.Patch(req), store.WithVersion(req.Version))
```

参与规则（**encoding/json 可见性的受限子集**——字段命名/导出性/提升以
encoding/json 为基线，Patch 再叠加 pointer-only / `store:"-"` 退出 / 命名
嵌入排除三重过滤）：

- **只有指针字段参与**：nil = 客户端未发送（跳过），非 nil = 写入（**零值
  照写**，`*""` 是"清空该列"）。非指针字段（uri 参数、`Version int` 等）
  一律不参与——它们表达不了"缺席"；忘写 `*` 只是"该字段永不更新"（开发期
  即可见），而"非指针恒写入"会把没发的字段静默清零（毁数据）。
- 公开名取 JSON tag 首段；`json:"-"` 排除；**无 JSON tag 用 Go 字段名**
  参与（与白名单 key 不匹配就响亮 `ErrUnknownUpdateField` → 500，提示补
  JSON tag）。DTO 字段可用 `store:"-"` 显式退出（放控制类指针字段，如
  `Force *bool`）。
- **嵌入按 encoding/json 提升**：无 JSON 名的匿名 struct 提升其字段（浅层
  遮蔽深层）；命名嵌入（`json:"meta"`，值或指针）是嵌套对象、**不参与**。
  被排除的浅层字段（非指针、命名嵌入、`store:"-"`）仍会**遮蔽**同名深层
  字段——与 encoding/json 一致，深层字段不会从被路由到别处的名字下"冒出来"。
  同深度两个**可 patch** 字段同名 = 构造期 500（比 encoding/json 的
  tag-dominance tie-break 更严，响亮而非静默）。**建议 PATCH DTO 保持扁平**，
  勿跨深度复用公开名。
- 每次 build 校验 **DTO 的完整形状**（含本次为 nil 的字段）：字段名不在
  update 白名单、解析到托管列、类型不匹配都在**首个到达 `Update` 的请求**
  即 500，而不是等客户端第一次发那个字段才炸。（`IsEmpty()` 早退的全 nil
  请求不触发 build，所以坏 DTO 的 500 在首个真正构建的请求上报出。）类型
  规则=严格 assignable（可空列 `*E` 收 `E`），不做隐式转换（`int`→`string`
  这类是陷阱不是 patch）。
- `.Onto(&obj)` 把值应用到已加载模型并复用 `Fields` 的隐式乐观锁与版本
  回写；失败后 obj 持有已应用值但 Version 未推进，**丢弃或重读**。裸
  `Patch`（无 Onto）无隐式锁，配 `WithVersion`，语义同 `Set`。
- 全 nil = `store.ErrEmptyPatch`（映射 400）；用 `pc.IsEmpty()` 让 no-op
  PATCH 直接返回当前对象（不触库、不需 schema）。类型无任何可 patch 指针
  字段 = `ErrNoPatchableFields`（500，类型用错了地方）。

### 6.3 每行不同值：`BatchUpdate`

```go
// 本例假设 Post 另声明了 Position int `store:"update"` 用于排序
for _, p := range postsToReorder {
    p.Position = nextPosition(p)
}
err := posts.BatchUpdate(ctx, postsToReorder, PostFields.Position)
```

`BatchUpdate` 对每个对象执行一条等价于 `Update(locator, Fields(obj, fields...))`
的 SQL，**整批放在一个事务**里。定位优先用对象的内部 `ID`，没有时用 `RID`；
两者都没有会在任何 hook / SQL 前报出 item 序号。乐观锁、零值写入、scope 与
单行 `Update` 完全相同；任一行失败回滚整批，并恢复本方法已递增的内存
`Version`。

> ✅ 所有行要写**同一个值**时不要用它做 O(N) SQL，直接一条语句：
> ```go
> err := posts.Update(ctx,
>     store.Where(where.WithFilterIn(PostFields.ID, ids)),
>     store.Set(map[string]any{PostFields.Status: "archived"}))
> ```

> ⚠️ **加入调用方已有事务时**：若 `BatchUpdate` 在外层事务中返回错误，调用方
> 必须回滚外层事务（或在继续前重读全部对象）——忽略错误继续提交，在部分
> 数据库上可能提交前面已执行的 SQL，而本方法已把对应对象的内存 `Version`
> 恢复成旧值。外层事务成功返回后又回滚，同样要丢弃或重读这些对象。

### 6.4 `Delete`

```go
err := posts.Delete(ctx, store.RID(rid))                       // 幂等：没命中也是 nil
err := posts.Delete(ctx, store.RID(rid), store.WithVersion(v)) // 带乐观锁的删除
```

- **软删模型**：写 `deleted_at` + 随机 `delete_token`（释放 SoftUnique 槽），
  同一条 SQL 把 `version` +1。**普通模型**物理删除。
- 带 `WithVersion` 时零命中会区分：行存在但版本不符 ⇒ `ErrStaleVersion`；行
  根本不在 ⇒ `ErrNotFound`。
- 🚫 **`Delete(ctx, store.Where())` 无条件清表会被拒绝**：返回
  `ErrMissingConditions`（`store: operation called without conditions`）。真要
  全删走 [§10](#10-逃生门unsafe危险区) 并三思。

### 6.5 `Restore`：软删恢复

```go
err := posts.Restore(ctx, store.RID(rid))
```

恢复不只是清 `deleted_at`——`delete_token` 必须归还空串，行才重新进入
SoftUnique 槽位。`Restore` 持有这套不变量：

- 槽位已被新活跃行占用 ⇒ `ErrDuplicate`，行保持已删。
- 🔒 scope 生效：恢复不了别人的行，且对方行读作 `ErrNotFound`（不泄露存在
  性）。
- 幂等镜像 `Delete`：行本来就活着 ⇒ nil；行不存在 ⇒ `ErrNotFound`。
- 成功恢复与软删一样推进 `version`，旧对象后续乐观更新会正确冲突。
- 🚫 硬删模型调用 ⇒ 错误（`not a soft-delete model`）。

### 6.6 `Upsert` / `BatchUpsert`

**happy path** —— 无属主的配置类表最常用：

```go
err := settings.Upsert(ctx, one, []string{SettingFields.Key}, SettingFields.Value)
err = settings.BatchUpsert(ctx, many, []string{SettingFields.Key}, SettingFields.Value)
```

两者都先校验 conflict / update 白名单，再跑 create hooks，最后所有分片在同一
事务执行。`conflictColumns` 不可为空；`BatchUpsert` 还要求批内冲突键元组按
目标数据库的相等规则互不重复（框架在 hook / SQL 前先拦完全相同的 Go 值，
避免 100 行分片边界改变结果）。

⚠️ **边界必须显式理解**（Upsert 家族的 SQL 语义天生受限）：

- 🚫 **带 scope 的 Store、以及内嵌 `db.Owned` 的模型直接返回
  `ErrUpsertScoped`**：`ON CONFLICT UPDATE` 路径不会自动带 `owner_id` 条件，
  攻击者用别人的冲突键就能改别人的行。替代写法：`Create` → 捕获
  `ErrDuplicate` → 显式 `Update`。
- **方言差异**：PostgreSQL / SQLite 要求 conflict target 精确匹配可用唯一
  索引（PG partial index 还要匹配谓词）；MySQL 渲染为
  `ON DUPLICATE KEY UPDATE`，**忽略 `conflictColumns`**，任意唯一键冲突都
  可能进更新分支。
- ⚠️ 因此 **`SoftUnique` 模型不要用 Upsert 系列**：PG 需要
  `deleted_at IS NULL` 谓词、SQLite 复合索引含 `delete_token`，当前公共 API
  都不暴露这两种方言细节。
- **不做乐观锁 / 不自增 version**。输入对象也不是数据库快照：create hook
  可能生成新 RID，而数据库保留旧行 RID——需要持久化身份或版本时按业务键
  重新读取。
- `WithBus` 对 Upsert 发布无 payload 的 `OpUpsert`（不发可能指向不存在 RID
  的伪 `OpCreate`）；`BatchUpsert` 每次成功的非空调用只发**一条**类型级失效
  事件。订阅者应按实体类型失效缓存后重新读取。

---

## 7. 事务与事件

### 7.1 一个事务串起多次写

```go
h := db.From(k)
err := h.RunInTx(ctx, func(txCtx context.Context) error {
    if err := posts.Create(txCtx, p); err != nil {       // 同一事务
        return err
    }
    return users.Update(txCtx, store.RID(uid), changes)  // 跨 Store 也在同一事务
})
```

- **事务随 ctx 传播**：`RunInTx` 给回调的 `txCtx` 带着事务，任何 Store 方法
  收到它就自动加入；返回 error 即回滚。
- 单 Store 便捷形态：`posts.Tx(ctx, func(tx *store.Store[Post]) error {...})`
  ——回调收到绑定事务的 Store 克隆；外层 ctx 已有事务时复用、不嵌套。
- `db.InTx(ctx) bool` 用于断言"这段必须在事务里跑"，它只回答是否，不交出
  句柄。

> ⚠️ 事务内**所有**操作都要传 `txCtx`。SQLite 尤其：拿根 ctx 在事务内再发写
> 会在池上排队等那个被外层事务占着的唯一写连接，直到超时（见
> [§11.1 SQLite 单机生产形态](#sqlite-单机生产形态默认生效)）。

**读-改-写必须赢 —— 悲观锁 `GetForUpdate`**：

```go
err := wallets.Tx(ctx, func(tx *store.Store[Wallet]) error {
    w, err := tx.GetForUpdate(ctx, store.RID(rid))   // SELECT ... FOR UPDATE
    if err != nil {
        return err
    }
    w.Balance -= amount                              // 到提交为止无并发写者
    return tx.Update(ctx, store.RID(rid), store.Fields(w, WalletFields.Balance))
})
```

- 它是 `Update` 自动乐观锁的悲观对应物：靠 `ErrStaleVersion` 重读重试不可
  接受（扣款、库存、状态机推进）的读-改-写序列用它；能重试的场景优先乐观锁。
- **必须在本句柄事务内**（`Store.Tx` 给的克隆，或 `RunInTx` 的 `txCtx`），
  否则返回 `ErrLockRequiresTx`——autocommit 下行锁在方法返回前就释放了，
  守卫把误用挡在入口。只读 store 返回 `db.ErrReadOnly`（锁是写意图）。
- `WithPreload` 被拒（`ErrLockPreload`）：关联行走独立查询，锁盖不住；先锁
  行，需要时再用普通 `Get` 取关联。
- **方言语义一致**：PG / MySQL 渲染 `FOR UPDATE`，并发锁者/写者阻塞到提交；
  SQLite 没有行锁、驱动会丢弃该子句——保证来自 chok 的 SQLite 形态本身：
  事务与写语句全部走**唯一写连接**（文件库=单连接写池，默认注入
  `_txlock=immediate`、DSN 显式指定时以调用方为准；内存库=整库仅一条固定
  连接），事务期间独占该连接，不可能出现并发写者，强于行锁。三方言的可观测
  保证相同：锁定读到提交之间无并发写者。

### 7.2 数据变更事件：`EntityChanged`

挂 `WithBus` 后，写操作发布 `EntityChanged[T]`。事件**锚定提交**：事务内的
写先暂存，`Commit` 后按序发布，回滚全部丢弃——订阅者永远不会看到没发生过
的写。

```go
event.Subscribe(bus, func(_ context.Context, ev store.EntityChanged[Post]) {
    switch ev.Locator.Kind {
    case store.LocatorRID:
        invalidate(ev.Locator.RID)
    case store.LocatorWhere:
        invalidateAllPosts() // Where 谓词可能是自定义 Option，按类型级失效
    }
    values := ev.Changes.Values() // public field → 递归拷贝后的值
    _ = values
})
```

payload 是订阅者可直接读取的**不可变快照**：Create 用 `ev.Object.Value()`
取对象，Update 用 `ev.Changes.Value/Values()` 取字段；两者都递归快照
map / slice / pointer，访问器再次返回拷贝，异步订阅者不与调用方或彼此共享
可变数据。`Op` 覆盖 `OpCreate` / `OpUpdate` / `OpDelete` / `OpRestore` /
`OpUpsert`。

> ⚠️ **传递保证：进程内 at-most-once——只可承载"丢了能自愈"的消费**。
> 事件只存在于本进程内存：COMMIT 与 flush 之间进程崩溃即丢、订阅者队列
> 满时默认 drop-oldest（计数 + 限频 warn）、无持久化无重放。缓存失效这类
> 靠 TTL / 重读兜底的用途没问题；**审计流、物化投影等"必须看到每一次已
> 提交写"的场景不要挂在这上面**——那需要事务性 outbox，chok 目前不提供。

---

## 8. 迁移

三种模式（`chok.yaml` 的 `db.migrate`），**启动时执行，Reload 永不触发**——
改了 schema 相关配置需要重启：

| 模式 | 行为 | 适用 |
|---|---|---|
| `auto`（默认） | 启动对 `WithTables` 声明的表 AutoMigrate | 开发、单体小服务 |
| `versioned` | 只执行编号 SQL 迁移文件，拒绝隐式改表 | 生产、多副本 |
| `off` | 框架完全不碰 schema（电池表也不建） | DBA 全权管理 |

> `auto` 会先对**全部** `TableSpec` 完成 GORM schema 解析与 SoftUnique 静态
> 校验，全绿后才执行第一条 DDL——声明错误不会留下可避免的前缀半迁移。

### 8.1 `versioned` 工作流（happy path）

SQL 文件前向单向，没有 down——改错就发下一个前向迁移：

```bash
chok migrate create add_posts_table   # 生成 migrations/0001_add_posts_table.sql
# 编辑 SQL 后：
chok migrate up                       # 跨进程锁下执行全部 pending
chok migrate status                   # 展示 applied / pending / dirty / drift / ...
chok migrate status --check           # 非 clean 时退出 1（进 CI / 发布门禁）
```

应用侧把目录嵌进二进制：

```go
//go:embed migrations/*.sql
var migrationsFS embed.FS

// ⚠️ 加载器只读 FS 根目录、跳过子目录。//go:embed 把文件放在 migrations/ 下，
// 所以必须先 fs.Sub 到该目录——否则会静默加载 0 个迁移、不报错。
sub, err := fs.Sub(migrationsFS, "migrations")
if err != nil {
    return err
}
db.Module(db.WithMigrations(sub))
```

每个文件按 CRLF→LF 归一化后算 SHA-256。执行任何 SQL 前，框架先提交
`dirty=true` 账本行与临时兼容 fence；因此进程死亡、MySQL DDL 部分提交、回滚
旧 chok 二进制都不会把半成品误认为成功。旧三列账本第一次执行 `up` 时按当前
文件建立 checksum 基线（trust-on-first-use）；这只保证此后的改写可检测，不能
追溯基线前的历史。

### 8.2 修 `dirty`：显式 repair

`dirty` 不能自动判断为"该重跑"还是"其实已全部生效"。**先人工核对数据库**，
再针对一个版本执行显式 repair（checksum 从 `status` 输出复制）：

```bash
# 已恢复到迁移前状态，下次 up 整文件重跑
chok migrate repair retry 12 --checksum <ledger-sha256> --reason "restored partial DDL"

# 已确认或手工补齐全部效果，只清 dirty
chok migrate repair mark-applied 12 --checksum <ledger-sha256> --reason "completed manually"

# 已应用文件的改写经过审核，接受当前字节作为新基线
chok migrate repair accept-drift 7 --checksum <old-ledger-sha256> \
  --new-checksum <current-file-sha256> --reason "approved rewrite"
```

repair 用 version + checksum 做 compare-and-swap，返回含 old / current
checksum、reason、时间的结构化报告。三个 action 还都接受
`--component <account|audit|authz>`（修某个内建电池序列而非应用账本）与
`--operator`（覆盖记进历史的默认 `user@host`）。

> ⚠️ MySQL 尤其不能在未核对已提交 DDL 的情况下直接选 `retry`——它的 DDL
> 会隐式提交，半成品可能已落地。

### 8.3 repair 历史：append-only 证据

每次 repair（应用账本与 owned 序列的 `retry` / `mark-applied` /
`accept-drift`，以及 manifest claim 转移）都会把**完整证据**（动作、校验和或
owner、必填 reason、operator、chok 版本）在 **repair 自己的事务里**追加写入
每库一张的 `schema_migrations_chok_repairs`：

- **历史行存在 ⇔ 该次 repair 的业务状态迁移已提交**。写不进历史的 repair
  会整体失败，而不是提交一次无据可查的改写。
- 「事务已提交」不等于「调用方观察到成功」——提交后进程崩溃同样会留下一行
  调用方没收到成功响应的历史。

查询：

```bash
chok migrate repair history                        # 最近优先（CLI --limit 默认 20）
chok migrate repair history --kind account --limit 50   # 按 kind 过滤（应用账本恒为 app）
```

```go
records, err := db.RepairHistory(ctx, h, db.RepairHistoryFilter{Kind: "account", Limit: 20})
// Kind：""=全部；"app"=应用账本；其余须过 ValidateSequenceKind。
// Limit：0 → 默认 50（注意 CLI 默认是 20），>1000 clamp 到 1000，负数报错。
// 表 append-only、永不清理，无界读是坑。
```

> 🔒 表由框架 append-only 维护、永不清理；防篡改是**权限纪律**而非密码学——
> 建议对应用账号 `REVOKE` 该表的 `UPDATE`/`DELETE`。行内自洽性被破坏时读取
> 返回 `ErrRepairHistoryCorrupt`。有更强合规要求就继续把报告同步到外部审计
> 管道，两者不互斥。历史行只由 history-aware 二进制写入，旧二进制缺行属于
> 既有混跑边界。

### 8.4 框架自有表与电池账本

框架自有表由各内建组件的 `Descriptor.Schema` 声明，`chok docs gen` 聚合成
字母序的 `db.FrameworkTables()` 目录；已装配组件自行演进这些表，**不占用你
的迁移序号**。`versioned` 模式下 account / audit / authz 分别用
`schema_migrations_chok_account` / `_audit` / `_authz`，迁移文件按
sqlite/mysql/postgres 选择，账本记录实际方言与 checksum。

```bash
chok migrate up --component account   # 只执行 account 内建序列
chok migrate up --all-owned           # 执行全部内建电池序列
```

> ✅ **生产建议**：由有 DDL 权限的 migration job 先 `up --all-owned`，业务进程
> 只保留 DML 权限。存量 AutoMigrate 表只有在**全部 owned tables 均存在且完整
> catalog 指纹一致**时才被采纳为 versioned 等价版本，否则 fail-closed 并输出
> 结构差异。`auto → versioned` 用同一协议；`versioned → auto` 不支持。一旦
> 执行不兼容前向迁移，回滚到账本前版本属于明确禁止的操作。

### 8.5 🧪 下游组件的独立迁移序列（第三方库作者向）

第三方组件可以用与内建电池**相同的三方言、独立账本协议**。kind 决定
`schema_migrations_chok_<kind>`，owner 是声明该序列的组件包完整 import path
（同 module 下不同组件因此有不同身份）：

```text
migrations/
├── sqlite/0001_init.sql
├── mysql/0001_init.sql
└── postgres/0001_init.sql
```

```go
seq, err := db.OwnedSequence(
    "billing",
    migrations,   // FS 根须直接是 sqlite/ mysql/ postgres/（嵌在上层目录里就先 fs.Sub）
    db.Baseline{},                                        // 新库留空即可
    db.SequenceOwner("github.com/acme/platform/billing"), // 必填，全 import path
    db.SequenceVersion("v1.4.0"),                         // 选填、仅信息性
)
if err != nil {
    return err
}
_, err = databaseComponent.ApplyOwnedMigrations(ctx, seq) // 运行时在 Migrate phase 调用
```

- **manifest**：每个 owned sequence 把 owner、兼容 `engine_floor`、信息性
  组件 / chok 版本持久化到全局 `schema_migrations_chok_manifest`。apply、账本
  repair、claim transfer 都在迁移锁内校验 owner 与 floor；更高 floor 只允许
  只读 status，旧引擎不能写入。
- **保留 kind**：`manifest` / `app` / `repairs`（分别是全局 manifest、应用
  账本历史身份、repair 历史表）永不可作 kind；`account` / `audit` / `authz`
  保留给对应 chok 包。`db.ValidateSequenceKind(kind)` 是所有工具在从外部
  观察到的 kind 字符串派生账本标识前的共享校验门。
- **只读观察 API**：`db.ManifestEntries`（目录）、`db.LedgerSnapshot`（文件
  无关的账本健康）。
- **显式修复 API（写）**：`db.RepairSequenceClaim` 在迁移锁下做 owner
  compare-and-swap、写 manifest 并追加 repair 历史——**不是只读**；强制
  `Reason`、精确 `expected-owner` CAS。
- `chok migrate status` 同时发现 claimed 与 legacy-unclaimed 账本；严格
  `--check` 对 unclaimed 或 file-unverified 第三方内容 fail-closed，除非显式
  加 `--ledger-health-only`（只查账本健康：存在性 / dirty / unverified 行 / fence
  与 engine floor，跳过内容与归属 claim 核验）。

组件包改名时用精确 owner CAS 转移已有 claim：

```bash
chok migrate repair claim \
  --kind billing \
  --expected-owner github.com/acme/platform/billing \
  --new-owner github.com/acme/platform/payments \
  --reason "package rename"
```

> 🧪 **改内建电池表形状的合入清单**（贡献 chok 本身时才需要）：电池迁移
> 文件是 append-only 历史，其前缀只能复现某个 versioned schema frontier，不能
> 复现旧二进制的 Go 回填 / hooks / AutoMigrate 模型。这类 PR 必须同时满足
> 三方言 catalog 等价 + DML 行为矩阵、N-1 三路径（fresh / 严格前缀灌数据 /
> 旧 auto 库 baseline adoption）、`EquivalentVersion` 冻结、SQLite/PG/MySQL
> 三 lane 全绿等硬门禁。完整清单见 [`design.md`](design.md) 与
> [`roadmap.md`](roadmap.md)。

---

## 9. 多实例与只读

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

`read_only: true` 是**实例能力**而不只是命名：

- 默认的 `migrate: auto` 降为 `off`；显式配 `migrate: versioned` 会校验失败，
  必须由操作者明确改 `off`。
- `RunInTx` / `Migrate` 与所有 store 写方法返回 `db.ErrReadOnly`；构造 store
  时必须显式 `store.WithReadOnly()`，否则启动 panic。只读句柄不会加入其他
  实例放进 context 的事务。
- `Unsafe` 仍可做复杂查询，但只放行以 `SELECT` 开头且不带行锁的 raw SQL；
  `WITH`、`FOR UPDATE`、全部 ORM/Exec 写在 GORM callback 层拒绝。SQLite 还
  用 `mode=ro` 打开文件；PG/MySQL 为每条新连接设只读 session 默认。

> ⚠️ 应用层判定用于**防误用**，真正的权限边界是**数据库只读账号或物理副本**。
> 需要同一 DSN 的管理写时，装配另一个可写具名实例，而不是运行时开逃逸门。

---

## 10. 逃生门（Unsafe，危险区）

> ✅ **先想想能不能不下探**：单列投影、跨表两步 IN 用 `store.Pluck` /
> `PluckIDs` / `PluckDistinct`（§5.5）；仪表盘统计（求和/均值/极值/分组
> 计数）用 `store.Sum` / `Avg` / `Min` / `Max` / `CountDistinct` /
> `GroupBy`（§5.7）——以前这类查询只能下 Unsafe，现在它们是正门，保留
> 白名单与 scope。真写不出来时才动 Unsafe。

**返回 raw gorm 句柄**的门有两扇，都叫 `Unsafe`，选哪扇看你要不要租户语义：

| 门 | 事务感知 | scope | 用途 |
|---|---|---|---|
| `Store.Unsafe(ctx)` | ✅（仅同句柄事务） | 🔒 已应用，scope 失败 fail-closed | store DSL 写不出的 SQL，但 owner / 租户过滤必须保持 |
| `(*db.DB).Unsafe(ctx)` | ✅（仅同句柄事务） | ❌ 无 | 基建层：外形表 AutoMigrate、事务内行锁 |

```go
gdb, err := posts.Unsafe(ctx)  // 注意：返回 error（scope 解析失败即拒）
gdb := h.Unsafe(ctx)           // 句柄级：无 scope，自己负责
```

> ⚠️ 还有**第三个**能拿到 raw `*gorm.DB` 的可信边界——自定义 `where.Option`
> （§5.2）。它绕过白名单记账，同样不可由请求参数拼装。
>
> 🔒 **纪律**：不进 HTTP handler 的快乐路径；包在 repository / store 层内；
> 审计要搜 `grep '\.Unsafe('` **加上**自定义 `where.Option` 的定义 / 传入点
> ——每处都是 code review 检查点。

---

## 11. 参考

### 11.1 安全默认值一览

| 栏 | 行为 | 关闭方式（显式） |
|---|---|---|
| 属主隔离 | `Owned` 模型自动 `owner_id` 过滤；🚫 **读 / 改 / 删缺登录用户 ⇒ 拒绝（401）**。Create 缺 principal 默认 no-op，须开 `require_principal` 才拒绝（§6.1） | `WithoutOwnerScope()`（构造期 warn） |
| 管理员越权 | principal 带管理员角色时跳过属主过滤，创建时可显式指定 `OwnerID`；名单**读写两侧共用**、构造期定格 | 应用级 `db.store.admin_roles`；每 Store 用 `store.WithAdminRoles("ops", ...)` 覆盖（**替换**继承名单，零参数 = 关闭全部旁路）。⚠️ 别再用 `WithScope(store.OwnerScope(...))` 调角色——scope 按 AND 组合，第二个 OwnerScope 是旁路**交集**不是覆盖；全局 `store.SetDefaultAdminRoles(...)` 已 **Deprecated**（仅作兜底默认） |
| 防清表 | 写操作的 `Where()` 必须至少一个条件，否则 `ErrMissingConditions` | 无（走逃生门） |
| 字段白名单 | 过滤 / 更新只认声明过的字段；托管列不能被显式列表 / alias 重开 | 无（修复走 `Unsafe`） |
| 大文本防护 | 自动发现不把 text/blob 列放进过滤面 | tag / 显式声明可放行 |
| 通配转义 | `WithFilterContains` 等对 `%`/`_` 转义 | `WithFilterLikeRaw`（自己负责） |
| 游标尺寸 | 客户端令牌 ≤ 4KB（解码前拒收，400）；边界值 repr ≤ 1KB、组装令牌 ≤ 4KB（超限拒签，服务端错），绝不静默截断（§5.4） | 无 |
| 敏感配置 | DSN / 密码带 `sensitive` 标注，日志自动掩码 | 无 |
| 只读实例 | `read_only: true` 降 auto 为 off、拒显式 versioned，并拒事务 / DDL / store / GORM 写；driver 层再兜底 | 另装配可写具名实例 |

#### SQLite 单机生产形态（默认生效）

`driver: sqlite` 时框架自动落成单进程生产形态，零配置：

- **纯 Go 驱动**（glebarez/modernc）：无 CGO，交叉编译、Windows 开发机、
  scratch 镜像开箱即用。⚠️ mattn 拼法的 DSN 参数（`_synchronous=` 等）会在
  启动时被**拒绝**（fail-fast 好过悄悄失效）；改写成
  `_pragma=synchronous(NORMAL)` 形式（`_txlock` 拼法不变）。
- **读写分池**：写侧固定单连接 + `_txlock=immediate`（`BEGIN` 即取写锁，杜绝
  "先读后写"事务升锁时那个不吃 busy_timeout 的 `SQLITE_BUSY`）；读侧独立
  连接池（默认 `max(4, NumCPU)`）靠 WAL 快照与写者并行。路由按 gorm 回调
  自动分流，业务无感知；`:memory:` 库无法分池，维持钉单连接。
- **每连接默认**：`foreign_keys(1)`、`synchronous(NORMAL)`、busy_timeout 5s
  ——这些**连接级** pragma 可被你在 `path` 里写的合法 `_pragma=...` 覆盖；
  但文件级 `journal_mode=WAL` 由框架在打开后**无条件确保**，不受 DSN 影响。
- **后台维护**：每 `checkpoint_interval`（默认 5m）跑
  `wal_checkpoint(TRUNCATE)`，每 `optimize_interval`（默认 1h）跑
  `PRAGMA optimize`；设 0 关闭。**Close 前只补跑一次 `PRAGMA optimize`**
  （不含 checkpoint）。备份挂 litestream 边车即可。
- ⚠️ **前提是单进程独占数据库文件**（框架不强制、也无法强制）。多实例部署
  时这个前提被打破——用 LiteFS/litestream 做只读副本、写仍回单点，或者那就是
  换 `driver: postgres` 的时刻。

### 11.2 错误处理

下表哨兵可 `errors.Is` 匹配；挂上 `store.MapError` 后 HTTP 状态码自动正确。
⚠️ 两点例外：① `where.ErrUnknownField` **按入口划界**——程序化读入口
（`List` / `Count` / `Pluck*` / `ListIn` / 聚合家族 `Sum`/`Avg`/`Min`/`Max`/
`CountDistinct`/`GroupBy` / 游标字段 / locator）原样返回，可正常
`errors.Is`，`MapError` 不映射它 → 500（字段名是服务端代码写的，typo 是编程
bug）；`ListFromQuery` 链（字段名来自 URL）内部预映射 400 的 apierr，对**那条
链**的返回值不要再 `errors.Is` 它。值错误 `where.ErrInvalidParam` 则在所有入口
都预映射 400。② 并非每个返回
错误都有稳定哨兵（如 `Fields`/`Patch` 传形状不符的 DTO、对硬删模型
`Restore`，都是普通包装错误）。

```go
chok.New("app",
    chok.WithErrorMapper(store.MapError),  // 一次装配，处处生效
)
```

| 哨兵 | 含义 | HTTP |
|---|---|---|
| `store.ErrNotFound` | 定位无命中（或无权看见） | 404 |
| `store.ErrStaleVersion` | 乐观锁冲突 | 409 |
| `store.ErrDuplicate` | 唯一约束冲突 | 409 |
| `store.ErrMissingConflictColumns` | Upsert 未声明冲突目标 | 500（编程错误） |
| `store.ErrDuplicateBatchConflict` | BatchUpsert 批内冲突键重复 | 500（编程错误） |
| `store.ErrMissingConditions` | 无条件写操作被拦 | 500（编程错误） |
| `store.ErrDegenerateConditions` | 退化过滤（`WithFilter(x,nil)` / 空 `WithFilterIn`）用作 `Where` Locator | 400 |
| `store.ErrEmptyPatch` | `Patch` 的 DTO 本次全字段为 nil（客户端没发可更新字段） | 400 |
| `store.ErrNoPatchableFields` | `Patch` 的类型无任何可 patch 指针字段（类型用错了地方） | 500（编程错误） |
| `store.ErrUpsertScoped` | 对 scoped / Owned 模型调 Upsert | 500（编程错误） |
| `store.ErrProtectedUpdateField` | 运行期试图更新框架托管列（显式列表 / alias 重开是**构造期 panic**，不走这里） | 500（编程错误） |
| `store.ErrLockRequiresTx` | `GetForUpdate` 不在本句柄事务内 | 500（编程错误） |
| `store.ErrLockPreload` | `GetForUpdate` 带 `WithPreload`（锁盖不住关联查询） | 500（编程错误） |
| `db.ErrReadOnly` | 只读实例 / 只读 store 收到写 | 500（装配 / 编程错误） |
| `where.ErrUnknownField` | 过滤/排序字段未声明。程序化入口原样返回（编程错误）；仅 `ListFromQuery` 链预映射 400（见上方例外①） | 500 / 400（按入口） |
| `db.ErrSequenceClaimConflict` | 两组件声明同 kind，或包路径变更 | 启动失败 |
| `db.ErrMigrationEngineTooOld` | manifest 的 `engine_floor` 高于当前二进制 | 启动失败 |
| `db.ErrRepairHistoryCorrupt` | repair 历史表被改写 / 结构被重塑 | —— |

> **方言差异（SQLite）**：只有扩展码为 PRIMARY KEY / UNIQUE / ROWID 的冲突才
> 映射为 `ErrDuplicate`（409）；NOT NULL、CHECK、外键失败**保留原始错误、不当
> 冲突**——业务别把所有 SQLite constraint 失败一律按 409 处理。

> **重复键报什么**：默认 409 的 metadata 带 `constraint`（驱动消息里提取的
> 约束标识）——它泄 schema 命名且随迁移漂移。构造 Store 时用
> `WithConstraintFields` 声明约束 → 公开字段名的映射后，命中的重复键改报
> metadata `field`（Ecto `unique_constraint` 的思路），未声明的保持
> `constraint` 现状。键按驱动上报形态匹配并做表前缀归一化：PG/MySQL 报
> **索引名**（MySQL 8 报 `table.key`，已剥表名），SQLite 不报索引名、只报
> **列清单**（SoftUnique 索引含 `delete_token`），跨方言的声明两种拼法都要写：
>
> ```go
> store.WithConstraintFields(map[string]string{
>     "uk_email":           UserFields.Email, // PG / MySQL：索引名
>     "email,delete_token": UserFields.Email, // SQLite：列清单（复合索引按序逗号拼接）
> })
> ```
>
> （map 的**键**是 schema 层标识符，保持字符串；**值**是公开字段名，可用
> §3.3 的生成引用。）

### 11.3 API 速查

```
构造与句柄
  db.From(k, instance...)   db.Module(opts...)   db.As(name)
  db.WithTables(specs...)   db.WithMigrations(fs)
  db.Table(model, indexes...)   db.SoftUnique(name, cols...)
  h.RunInTx(ctx, fn)   h.Migrate(ctx, specs...)   h.Ping(ctx)   h.Unsafe(ctx)
  db.InTx(ctx)

版本化迁移
  db.LoadMigrations(fs)   db.ApplyMigrations(ctx, h, fs)
  db.ApplyMigrationsWithReport(ctx, h, fs)   db.MigrationsStatus(ctx, h, fs)
  db.RepairMigration(ctx, h, fs, db.RepairOptions)
  db.OwnedSequence(kind, fs, baseline, db.SequenceOwner(path), db.SequenceVersion(v))
  db.ApplySequence(ctx, h, seq)   db.SequenceStatus(ctx, h, seq)
  db.RepairSequence(ctx, h, seq, opts)   db.SequencePresent(ctx, h, seq)
  db.ManifestEntries(ctx, h)   db.LedgerSnapshot(ctx, h, kind)
  db.RepairSequenceClaim(ctx, h, kind, db.RepairClaimOptions)
  db.RepairHistory(ctx, h, db.RepairHistoryFilter)   db.ValidateSequenceKind(kind)

Store[T]（读）
  Get(ctx, loc, ...QueryOption)         List(ctx, ...where.Option)
  ListQ(ctx, []QueryOption, ...)        ListFromQuery(ctx, url.Values)
  ListByIDs(ctx, ids)                   ListWithCursor(ctx, field, dir, cur, n)
  Count(ctx, ...where.Option)           Exists(ctx, loc)
  GetForUpdate(ctx, loc, ...QueryOption)   // 悲观锁，仅限本句柄事务内（§7.1）

自由函数读（保留白名单 + scope）
  store.Pluck[F](ctx, s, field, ...opts)     store.PluckDistinct[F](ctx, s, field, ...opts)
  store.PluckIDs(ctx, s, ...opts)            // 内部主键；两步 IN 的前半段
  store.ListIn(ctx, s, field, values, ...opts)   // 两步 IN 后半段；>MaxInList 自动分块

聚合（§5.7；单值返回 (值, ok, error)，ok=false ⇔ SQL NULL）
  store.Sum[int64|float64](ctx, s, field, ...opts)
  store.Avg(ctx, s, field, ...opts)              // 恒 float64
  store.Min[N](ctx, s, field, ...opts)   store.Max[N](ctx, s, field, ...opts)  // N 含 time.Time
  store.CountDistinct(ctx, s, field, ...opts)    // int64，无 ok
  store.GroupBy[K](ctx, s, field, []store.Aggregate{...}, ...opts)
    store.CountRows()  store.CountDistinctOf(f)  store.SumOf(f)
    store.AvgOf(f)     store.MinOf(f)            store.MaxOf(f)
    Group[K]{Key, Values}   AggValue.Int64/Float64/Time/IsNull

Store[T]（写）
  Create(ctx, *T)      BatchCreate(ctx, []*T)
  Update(ctx, loc, changes, ...UpdateOption)      BatchUpdate(ctx, []*T, fields...)
  Delete(ctx, loc, ...DeleteOption)     Restore(ctx, loc)
  Upsert(ctx, *T, conflictCols, updateCols...)
  BatchUpsert(ctx, []*T, conflictCols, updateCols...)    // 两者均禁属主模型
  Tx(ctx, fn)          Unsafe(ctx)

定位 / 变更 / 选项
  store.RID(s)  store.ID(n)  store.Where(opts...)
  store.Fields(obj, cols...)[.NoLock()]   store.Set(map)   store.WithVersion(v)
  store.Patch(req)[.Onto(&obj)][.NoLock()]   // DTO 部分更新；pc.IsEmpty() 判空
  store.WithRowsAffected(&n)   store.WithPreload(rel)
  store.WithTrashed()   store.WithOnlyTrashed()

where DSL
  WithFilter(f, v)  WithFilterOp(f, where.Gte, v)  WithFilterIn(f, vs...)
  WithFilterNull(f)  WithFilterNotNull(f)
  WithFilterContains/StartsWith/EndsWith(f, s)  WithFilterLike/LikeRaw(f, p)
  WithOrder(f, desc...)  WithPage(p, size)  WithOffset(o)  WithLimit(n)
  WithCursor(f, dir, cur, size)  WithCount()

事件快照
  store.EntityChanged[T]   store.LocatorSnapshot   store.ObjectSnapshot[T]
  store.ChangeSnapshot     Object.Value()          Changes.Value/Values()
```

### 11.4 接口视图（依赖注入用）

消费方声明依赖时用**窄接口**而非 `*store.Store[T]`——只读消费者拿不到写
方法，测试替身只需实现窄面：

```go
store.Reader[T]       // Get / List / ListByIDs / Exists / Count（transport-free：ListFromQuery 是 *Store 的 HTTP 糖，不进契约）
store.Writer[T]       // Create / Update / Upsert / Delete（单行写面）
store.BatchWriter[T]  // BatchCreate / BatchUpdate / BatchUpsert（批量写面；按单行/批量划线，批量扩张不动 Writer 的 mock 面）
store.ReadWriter[T]   // Reader + Writer（单行 CRUD，无逃逸舱口）
```

---

## 12. 实践

### 12.1 测试

```go
func TestPostFlow(t *testing.T) {
    h := choktest.NewTestDB(t, &Post{})        // 真实 SQLite 内存库，自动建表清理
    posts := store.New[Post](h, log.Empty())
    // ... 正常使用，断言真实 SQL 行为
}
```

- ✅ **不要 mock 数据库**——内存 SQLite 一样快，且能抓到真实 schema 问题。
- 需要 Postgres 行为差异时跑双道：
  `CHOK_TEST_DRIVER=postgres CHOK_TEST_PG_DSN=... go test ./...`（CI 的 PG
  service 自动跑同一套）。
- MySQL 隐式 DDL 提交的 dirty / repair 主路径跑
  `CHOK_TEST_MYSQL_DSN=... make test-mysql`（CI 提供 MySQL 8.4 service）。
  该 DSN 用户除建/删临时库外，还需**全局 `PROCESS` 权限**——`GetForUpdate`
  的争锁测试要查 `information_schema.innodb_trx`（MySQL 以 PROCESS 门控
  INNODB_* 信息表；root 自带，自建用户需 `GRANT PROCESS ON *.*`）。
- 属主隔离的测试给 ctx 注入用户：
  `auth.WithPrincipal(ctx, auth.Principal{Subject: "usr_alice"})`。

### 12.2 项目组织与分层

`Store[T]` 本身就是数据操作层的实现载体——不需要再手写一层 DAO 包住它。
分层只回答两个问题：实体定义放哪、要不要在 `Store[T]` 外再包一层领域 store。

```
myapp/
├── chok.yaml
├── main.go                    # 装配点
├── model/                     # ① 实体：纯数据 + 声明，只 import db
│   ├── post.go
│   ├── chok_fields_gen.go     # `chok gen fields --dir ./model` 产出（§3.3）
│   └── tables.go              # 建表清单与实体同包
├── store/                     # ② 数据操作层（小项目可整层省略）
│   └── posts.go
└── api/                       # ③ HTTP handlers
    └── posts.go
```

model 包保持单向依赖（只 import `db`），实体靠 `store` tag 自带操作面声明，
字段引用在同包生成（模型改动后重跑 `chok gen fields --dir ./model`，CI 用
`--check` 盯漂移）；建表清单收在同包，`main.go` 只管转交：

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

**store 层的两种形态**：

- **形态 A（起步默认）**：`Store[T]` 就是 store 层。装配点构造、注入 handler，
  不写任何包装——`examples/blog` 即此形态，单实体 CRUD 到此为止就够了。
- **形态 B（领域词汇出现后）**：**内嵌透出**。当你需要 `PublishedSince` 这类
  带业务语义的查询名时再包一层——常规 CRUD 免费获得，只写领域方法，绝不手抄
  转发：

  ```go
  type PostStore struct {
      *store.Store[model.Post]   // Get/List/Create/.../Restore/Count 直接透出
  }

  func NewPostStore(h *db.DB, l log.Logger) *PostStore {
      return &PostStore{Store: store.New[model.Post](h, l)}
  }

  // 名字属于业务；实现仍走白名单与 scope，安全栏不因包装而松动
  func (s *PostStore) PublishedSince(ctx context.Context, t time.Time) (*store.Page[model.Post], error) {
      return s.List(ctx,
          where.WithFilter(model.PostFields.Status, "published"),
          where.WithFilterOp(model.PostFields.CreatedAt, where.Gte, t),
          where.WithOrder(model.PostFields.CreatedAt, true))
  }
  ```

> ✅ **service 层的存在理由是跨实体编排**：多 store 同事务走 `h.RunInTx`
> （[§7](#7-事务与事件)），事务随 `txCtx` 传播——这是 handler 不该直接干、
> 单实体 store 也管不到的那一层。只有单实体 CRUD 时不需要 service 层，别为
> 分层而分层。

### 12.3 故障排除

| 症状 | 原因 | 解法 |
|---|---|---|
| 日志刷 `store: auto-discovered query fields` | 模型没有任何字段声明 | 给字段加 `store:` tag（[§3.2](#32-用-store-tag-声明字段面)） |
| 构造期 panic `bad store:"..." tag value` | tag 拼错（如 `quer`） | 只有 `query` / `update` 两个词 |
| 读 / 改 / 删 401 | `Owned` 模型 + ctx 无登录用户（fail-closed；Create 还需 `require_principal` 才拒绝） | 请求路径挂 `account.Authn(k)`；测试用 `auth.WithPrincipal` |
| `where: unknown field: "xxx"` | 过滤字段不在查询白名单 | 加进 tag / `WithQueryFields`；检查用的是 JSON 名不是列名 |
| `Page.Total` 恒为 0 | 没传 `where.WithCount()` | 加上；或只要数字用 `Count` |
| 更新总报 `ErrStaleVersion` | 对象是旧读、版本落后于数据库 | 重读后再 `Fields` 更新；查并发写。注：`WithVersion(0)` / `Version==0` 是"不加锁"，**不会**引发此错 |
| 构造期 panic `store.New: update field ... framework-managed column`，或运行期 `ErrProtectedUpdateField` | 把 id/rid/version/时间戳/owner_id 等托管列放进 update 面（显式列表 / alias 是构造 panic，运行期 Changes 是哨兵） | 别改托管列；确需修数据走 `Unsafe` |
| `store: operation called without conditions` | `Update`/`Delete` 传了空 `Where()` | 补条件；真要全表操作走逃生门 |
| `Restore` 报 `ErrDuplicate` | 唯一槽已被新活跃行占用 | 业务决策：删新行、改字段后重试，或放弃恢复 |
| 写入时报 `db: BeforeCreate: invalid RID` | 手工构造 / 导入的 RID 形状非法（运行期 INSERT 钩子，非启动期；启动期非法 `RIDPrefix` 是另一条 panic） | 用 `rid.New(prefix)` 生成；导入数据先校验 |
| SQLite 并发下 `database is locked` | 某个写事务持锁超 5s，或 DSN 显式改了 `_txlock` | 缩短写事务；查长事务 / 未 Close 的 `Rows()`；持续写超载换 `driver: postgres` |
| SQLite 写操作不报错但一直不返回 | `RunInTx` 里拿根 ctx（而非 `txCtx`）又发写——外层事务占着唯一写连接 | 事务内所有操作传 `txCtx`；确要旁路写就放到事务外 |
| 启动报 `DSN parameter "_synchronous" is a mattn/go-sqlite3 spelling` | 驱动是纯 Go 构建（glebarez），mattn 拼法被 fail-fast | 改成 `_pragma=synchronous(NORMAL)` 形式 |
| `versioned` 模式下写入报表不存在 | 忘了 `chok migrate up`，或 SQL 没 embed | 检查 `WithMigrations` 与 `migrate status` |
| 启动报 `dirty migration attempt` | 上次迁移失败或进程在 clean 前退出 | `migrate status` 核对实际 schema，再按 [§8.2](#82-修-dirty显式-repair) 选 repair |
| `status --check` 报 `unverified` | 旧三列账本尚未建立 checksum 基线 | 用当前可信发布执行一次 `migrate up` 完成 TOFU adoption |
| 启动报 `migration sequence claim conflict` | 两组件声明同 kind，或组件包路径变更 | 冲突组件改 kind，合法改名用 `migrate repair claim` 做 expected-owner CAS |
| 启动报 `migration engine generation is too old` | manifest 的 `engine_floor` 高于当前二进制 | 升级 chok；不要用旧 CLI repair 绕过 manifest |
| `repair history` 报 `repair history is corrupt` | 历史表被 UPDATE/DELETE 改写或结构被重塑 | 历史是 append-only 证据表；核对数据库审计与账号授权，必要时以外部审计副本重建信任 |
| `status --check` 报 `content is unavailable` | 通用 CLI 看得到第三方账本但不持有组件内嵌 SQL | 由应用组件执行完整校验；只查账本健康（存在性/dirty/unverified/fence/floor）时显式加 `--ledger-health-only` |

---

## 相关文档

- [`config.md`](config.md) —— db 段全部配置项（生成，永不漂移）
- [`design.md`](design.md) §5 —— 数据层架构决策（为什么长这样）
- [`migration-v1-to-v2.md`](migration-v1-to-v2.md) —— v1 用法对照
- [`examples/blog`](../examples/blog) —— 全部概念的可运行样例
