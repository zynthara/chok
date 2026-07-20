# Design Changelog

> 此文档记录 chok 公开契约层面的设计变迁——新增能力、不兼容变更、
> 弃用与移除。**实现细节不在此处**，请直接看 [`docs/design.md`](design.md)。
>
> 项目使用 [Conventional Commits](https://www.conventionalcommits.org/)；
> 自 v2 起 release 手动裁切（写根目录 [`CHANGELOG.md`](../CHANGELOG.md)
> 条目 → 打 tag → goreleaser 发布）。本文与之互补：那份记录"哪个
> 变更进了哪个版本"，本文记录"为什么这次发布的设计选择是这样"。

---

## Unreleased — cast/patch 写路径：`Patch(req)`（arch-backlog #5）

> store 架构复审的第二块 DX 架构债：写路径缺 Ecto changeset 的 **cast**
> 一环——`Set(map)`（终点）与 `Fields(obj)`（终点，且刻意拒收 DTO）都有，
> 唯独没有"外部部分输入 → 合法变更集"的起点，于是每个 update handler 把
> `if req.X != nil { obj.X = *req.X; cols = append(cols, ...) }` 重抄一遍。
> `Patch(req)` 补上这个起点：从请求 DTO 的非 nil 指针字段推导变更集，作第
> 三个 `Changes` 构造器与任意 Locator/选项组合。`Fields` 拒收 DTO 的禁令
> （值与锁元数据须同源于 GORM schema）正是给 Patch 留的正门——Patch 是
> DTO 正门，`Fields` 保持模型专用。
>
> **参与规则镜像 encoding/json**：仅指针字段参与（nil=缺席、零值照写），
> 公开名取 JSON tag（无 tag 回退 Go 字段名，不匹配就响亮 500），`store:"-"`
> 豁免。两个失败模式的取舍是设计核心：忘写 `*`="该字段不更新"（开发期
> 可见）优于"非指针恒写入"的 zero-clobber（没发的字段被静默清零、毁数据）；
> DTO 完整形状每次 build 校验（nil 字段的名字/托管/类型错也在首个请求即
> 500）优于"只校验非 nil 字段"埋下的生产地雷；类型规则=严格 assignable
> （可空列 `*E` 收 `E`），不做隐式转换。`.Onto(&obj)` 复用 `Fields` 的隐式
> 乐观锁与成功回写；全 nil = `ErrEmptyPatch`（400），无可 patch 字段 =
> `ErrNoPatchableFields`（500），`IsEmpty()` 给 no-op PATCH 免触库早退。
>
> **刻意否决**：`Optional[T]`（区分 JSON null 与未发送，需 binder/validator/
> openapi 全链改造，独立立项）、handler 层泛型 `HandleUpdate`（update
> handler 形状不统一，HandleList 的前提不成立）、map 输入（那是 `Set` 的
> 地盘，DTO 类型即编译期 cast 白名单）、create-cast 与 BatchPatch（无重复
> 样板/无需求先例）、值校验进 store（validate 半件已在 handler
> `binding`+`Validated`+before-update hook 成体系，constraints 半件在
> `MapError`+`WithConstraintFields`）。写内核／接口／白名单机制**零改动**，
> 新增面全 additive（`Patch`/`PatchChanges` 三方法 + 两个哨兵 + `MapError`
> 一行），apidiff 仅见新增不触破坏。

---

## Unreleased — 字段引用生成：`chok gen fields`（arch-backlog #4）

> store 架构复审把「字符串字段引用」定性为全层系统性弱点：白名单挡得住
> 恶意输入，挡不住自己人的 typo——`"titel"` 编译照过，运行时才炸成 500。
> 既定框架是**运行时白名单机制一行不动，只加编译期的壳**：新命令
> `chok gen fields` 用 go/parser 静态扫描带 `store:` tag 的模型，生成同包
> `chok_fields_gen.go`，每模型一个 `<Model>Fields` 结构体 var，值=白名单
> map 的 **key**（公开字段名，JSON 名/隐藏时回退 GORM 列名）——取 key 而
> 非列名，`WithColumnAlias` 之下引用天然稳定，`id` 的 rid 自动 alias 也
> 不受影响。基座三字段只生成 query 面，`version` 与托管列**连符号都不存
> 在**，结构上杜绝误传。
>
> **刻意否决**了两条更重的路线：Ent 式 typed predicate（与 `WithFilter*`
> 家族重复同一能力，正面违反单一钦定实现公理）与 defined type 签名
> （字符串字面量 untyped 隐转让类型保证对 typo 恰好无效，动态字符串场景
> 反而全数吃转换噪音）。因此本项**零新增可 import 公开 Go API**，apidiff
> 全程静默；裸字符串永远合法，`FromQuery` 的 HTTP 通道照旧运行时校验。
> 扫描选语法级而非 go/types：包编译不过时再生成必须仍能跑（改模型→旧引
> 用编译错→需先 gen 的鸡生蛋场景）。**列性由语法分类镜像 GORM 的解析结
> 果**（round-1〜4 复审逐轮收紧）：内建标量/`[]byte` 与 `[N]byte`（GORM
> 不支持的 `uintptr` 报错）/精确 `driver.Valuer`（`Value() (int, error)`
> 之类不算，签名经 alias 书写仍精确）/`GormDataType` 类型/已知跨包列类型
> （datatypes 只认**存储类型**白名单，查询表达式类型排除）直接生成；方法
> 集按 Go **selector 真规则**——最浅且唯一才算（同层歧义、浅层错签名或
> 同名字段遮蔽 = 非 Valuer），别名继承、定义类型不继承但保留底层 struct
> 嵌入的提升（`Box{Money}` 是列、`type Badge Money` 是关系）；本地定义
> 类型沿底层形状递归（定义标量含匿名嵌入是列、定义切片是 has-many、
> `type Stamp time.Time` 经可转换性仍是列），**泛型按实例化实参替换**
> （`Bytes[byte]` 是 bytes 列），`gorm:"type:"`/serializer（含
> `gorm:"json"` 简写）对**具名**字段是证明；**匿名 struct 另循 GORM 嵌入
> 规则**——真 Valuer/time 形态/`GormDataType` 字面量 "time"/"bytes" 是
> 列，其余 GormDataType/serializer/type: 都不阻止展开（tag 死，warn），
> 动态 GormDataType **报错拒猜**；关系形状跳过并 warn（运行时同样忽略，
> 不产死符号）；无法静态判定（陌生跨包类型、方法集含扫不到的嵌入）
> **报错拒猜**。
> 提升（promotion）是残留边界：本包内可验证的嵌入 tag 会
> warn——含「全部 tag 来自嵌入」的静默形态（直接内嵌 chok 基座即点名）与
> 未导出目标类型的具名 `gorm:"embedded"`（GORM 按字段名判导出性）；未导出
> **匿名**嵌入运行时整体跳过（两侧一致）；纯跨包嵌入承载 tag 的模型不可
> 见，文档诚实记录。命名推导自此双实现（生成器 vs
> GORM 运行时），这一长期架构代价由 store 包的**语义闩测试**钉死：对
> fixture 模型跑生成 → 建真 store → 双向断言生成值集合与两面白名单键集合
> 完全一致，边界形态（关系字段、提升嵌入）在 edge fixture 里对真 store
> 钉住精确差集。`--check` 进 CI（blog 已挂），生成物随代码 commit，与
> sync/docs gen 同一纪律。

## Unreleased — 聚合正门：Sum/Avg/Min/Max/CountDistinct/GroupBy（arch-backlog #7）

> 仪表盘聚合此前只能走 `Unsafe`，是数据层最后一块「DSL 层能保住安全语义、
> blessed 面却没有」的能力缺口。这批 API 与 `Pluck` 家族同位：**自由函数**
> （方法不能引入新类型参数，结果类型化只能走这条路；也因此天然不进
> `Reader`/任何接口），字段引用维持**白名单内裸字符串**——typed 字段常量是
> backlog #4 codegen 的统一职责，本项不预支。读语义完全等同 `Count`：
> 白名单（字段名 typo 是服务端 bug，原样 `where.ErrUnknownField` → 500，
> 对齐 #3 的 provenance 划界）、fail-closed scope、软删排除、filter 收窄，
> 分页/排序选项按 Count 先例静默剥离（单值聚合是「总量形状」，剥了不会
> 说谎）。**GroupBy 例外：拒收而非剥离**——行集结果上静默吞掉
> `WithOrder`+`WithLimit` 会让调用方以为拿到了 top-N（ListIn 守卫同理）。
>
> **NULL 语义**（跨三方言钉死）：SQL 聚合忽略 NULL，零行/全 NULL 时数据库
> 返回 SQL NULL——Go 侧收敛为 `(值, ok, error)` 的 comma-ok，`ok=false`
> 永不与「合法的 0」混淆；不选指针（调用点解引用噪音）也不选错误（空集
> 聚合是仪表盘常态而非异常）。GroupBy 聚合值同理走 `AggValue.IsNull`；
> **NULL group key 直接报错**并指向 `WithFilterNotNull`——SQL 里 NULL 组
> 与零值组是两个组，Go 侧静默折叠等于合并两个不同答案。
>
> **结果类型收敛**：SUM(int) 在 PG 返 bigint/numeric、MySQL 返 DECIMAL
> （wire 是 []byte 十进制串）、SQLite 动态类型——收敛函数
> （`coerceAggValue`）就是跨方言契约本身，三条 lane 用同一组断言钉死。
> 调用方声明目标类型：`Sum[int64]` 仅限整数列（精确、超 int64 值域响亮
> 报错，绝不静默截断）、`Sum[float64]` 允许拓宽整数列（2^53 精度取舍
> 文档化）、`Avg` 固定 `float64`（三方言都返回小数类型，不存在精确整数
> 读法；金额级精确统计不要用它）。`Min/Max` 额外放开**时间列**（“每组
> 最新 created_at”是真实仪表盘需求，MIN/MAX 是序运算不是算术运算）；
> 列类型校验复用游标的 schema wire-kind 探针（serializer/Valuer 字段按
> wire 类型判定），构造期即拒，不等数据依赖的运行时 parse 失败。
>
> **SQLite 时间聚合的瞬间正确性**（round-1 复审修正）：SQLite 把
> `time.Time` 按写入方时区存成文本、按字典序比较，混合偏移下裸
> MIN/MAX 会选**字典序**而非**时间序**极值、同一瞬间两种偏移写入会
> 分成两组/计成两个 distinct——复审实测坐实。修正不是拒绝该能力
> （SQLite 是默认 lane 与钦定单机生产形态，聚合正是为它的仪表盘立项）
> 也不是改写存储格式（游标一族依赖 writer-zone 文本不变量），而是**仅
> 在聚合读取处**把时间列包进 `strftime('%Y-%m-%d %H:%M:%f', col,
> 'auto')`：定宽 UTC 文本上字典序=时间序，毫秒精度与 MySQL DATETIME(3)
> 的写入精度对齐（亚毫秒折叠是文档化取舍），`'auto'` 兼容
> serializer:unixtime 的整数存储；本机 SQLite 3.41 无 `subsec` 修饰符，
> 故用 strftime 而非 `datetime(...,'subsec')`。表达式由框架自产、列名
> 仍过白名单——不触碰「调用方表达式不可白名单化」的红线。PG
> （timestamptz）与 MySQL（驱动统一会话时区重写写入）原生瞬间比较，
> 不加表达式；三方言用同一组混合偏移断言钉死。归一化只作用于聚合：
> 存储、filter、排序、游标不动（后者的既有 writer-zone 语义另属其
> 契约）。同轮收紧：`CountDistinct` 契约从「任意已声明字段」收窄为
> **可比较标量**（PG `json` 无等值运算符，入口即拒好过查询中途炸；
> 字符串基数按列 collation、方言自定，不再笼统承诺跨方言一致）；bool
> group key 只认规范 0/1（SQL 把裸 2 分成独立组，Go 折叠成重复 true
> key 会在调用方转 map 时静默覆盖——响亮报错）；GroupBy 守卫改在
> `where.Apply` 下运行使 `WithCount` 不再静默漏网；表限定列别名在
> 限定名为本模型表时正常解析、异表限定显式拒绝。
>
> **round-2 复审修正**（三条，逐条真库红→绿）：① round-1 对 MySQL 的
> 「驱动统一会话时区=瞬间正确」表述**过度承诺**——时间列是 DATETIME
> （墙钟、不转 UTC，MySQL 手册明文；chok 固定 Loc=time.Local），单进程
> 内恒一致，跨进程只在**部署不变量「同库所有写入方同一时区」**下成立；
> 混合时区存量墙钟不带时区、读取侧原理上不可修复（与 SQLite 带偏移
> 文本可归一是本质区别）。这条不变量先于聚合存在（排序/范围过滤/游标
> 同样骑在上面），聚合的错在文档宣称——修正为诚实声明 + 机械 pin 测试
> （模拟异时区写入方的墙钟、断言 MySQL 真实裂值），是否改为 UTC 写入
> 基准（Breaking、需存量策略、牵动全部时间面）另立 backlog #17 决策，
> 不混进正确性补丁（#16 先例）。② SQLite 数值时间的 'auto' 启发会把
> 1970 年头 63 天的 Unix 秒误读成 Julian day（官方文档明写，复审经
> Store 路径复现 2440588 → 1970-01-01T12:00Z）；且 serializer:unixtime
> 在 SQLite 实存**文本**（wire 是 time.Time）而非 round-1 注释以为的
> 整数——数值存量只来自 Unsafe/外部。改为显式 typeof 分支：数值一律按
> Unix 秒（'unixepoch'）、文本走普通解析，Julian REAL 刻意不支持。
> ③ wire kind 门禁挡不住 `string` + `gorm:"type:json"`（kind=str 放行，
> PG 运行期在 COUNT(DISTINCT) 上炸——json 无等值运算符、jsonb 才有）；
> 门禁补查声明的 DB 类型，json 族列在所有方言统一入口拒绝（对 JSON
> 文档做分组/去重计数不是 chok 能跨方言承诺的语义）。
>
> **round-3 复审修正**（两条）：① 「同一时区」仍不足——带 DST 的时区
> 在秋季回拨把两个瞬间折成同一墙钟（America/New_York 2026-11-01 的
> 05:30Z 与 06:30Z 都写成 01:30），**单进程也发生**，DATETIME 存量
> 无从修复。不变量收紧为「同一**固定偏移、无 DST** 时区，推荐
> TZ=UTC」；DST fold 回归经**真实驱动连接**（第二条 loc=America/
> New_York 的 go-sql-driver 连接）落库断言折叠，round-2 的墙钟裂值
> pin 测试同步升级为真驱动连接（原 raw UPDATE 手工拼串 pin 不住驱动
> /列映射变更）；backlog #17 的 UTC 决策论据随之加重。② JSON 门禁只
> 查逻辑 `DataType`，而自定义类型可经 `GormDBDataType` 仅在方言层映射
> 到 JSON（schema 层仍是 kind 推导的 string）——复审经 Store 路径复现
> 绕过。门禁补问 migrator 的 `FullDataTypeOf`（方言真实列型；只取首
> token，DDL 尾部的 DEFAULT 字面量不得误伤），tag / GormDataType /
> GormDBDataType 三条路径统一入口拒绝。
>
> **round-4 复审修正**（两条，均红→绿）：① round-3 把 `FullDataTypeOf`
> 放上了请求路径——但 migrator **不是只读的**（gorm mysql 解析时间列
> 时原地写共享 `schema.Field.Precision`），并发聚合在 -race 下稳定报
> 写写竞争；AutoMigrate 恰好提前写好 Precision 掩盖问题，`migrate:
> versioned/off` 形态（store 句柄从未跑过迁移解析）首次并发时间聚合
> 即触发——回归测试用「迁移走一个句柄、store 挂第二个未迁移句柄」还原
> 该形态，真 MySQL 上红转绿。修法：列型改在 **Store 构造期**（最后的
> 单线程时刻）于字段**副本**上解析并缓存（`aggColumnTypes`），请求
> 路径零 migrator 调用，round-3 加的 ctx 线程随之撤销。② 方言列型此前
> 只用于识别 JSON，其余错配静默算错——SQLite 上 `int64` +
> `gorm:"type:text"` 的 Min 返回字典序极值（2 输给 10），PG 的
> SUM(text) 运行期报错。能力矩阵补齐为两半：**wire kind 管 Go 结果
> 收敛，方言列型管数据库操作合法性**，六 kind 各配一张按三方言实测
> 渲染钉死的列型家族表（int→integer/bigint/serial/decimal 族、
> float→real/double/float/decimal 族、time→*time*/date 前缀、
> str→char/text/clob/uuid/enum 族、bool→bool/numeric(SQLite)/tinyint
> 族），错配与不认识的列型一律 fail-closed 指向 Unsafe。
>
> **round-5 复审修正**（两条，均红→绿）：① round-4 缓存的
> `FullDataTypeOf` 只渲染「模型将建成什么」、**不查 catalog**——
> `versioned/off` 下真列可能与模型不符（SQL 迁移把列建成 TEXT、模型
> 却是无 tag 的 int64），门禁缓存 integer、数据库却按文本求 MIN（2
> 输给 10，静默错），复审经 Store 路径复现。改为读**真实 catalog 类型**
> （`Migrator.ColumnTypes` 的 `DatabaseTypeName`）：请求路径首次聚合
> 懒解析 + 互斥缓存（构造可能先于迁移，catalog 只在迁移后反映真相；
> `ColumnTypes` 查 catalog 不改共享 schema，无 round-4 的 Precision
> 原地写竞态——round-4 的构造期字段副本方案随之撤销），缓存挂 Store
> 指针字段，跨 tx clone（`cp := *s` 浅拷贝）共享。② round-4 的类型族
> 用**子串**匹配，非 fail-closed——PG 的 `interval`/`int4range` 含
> "int" 混入整数族、`daterange` 前缀 "date" 混入时间族、`time`/`timetz`
> 含 "time" 当瞬间、`integer[]` 数组含 "int"，最小探针全翻红。改为按
> 方言的**精确白名单**（catalog `DatabaseTypeName` 逐名匹配，三方言
> 实测枚举：PG int2/int4/int8、float4/float8/numeric、timestamptz/
> timestamp/date、varchar/text/bpchar/uuid、bool；MySQL/SQLite 同理），
> 键按 wire kind 分组以消解 "numeric"（PG 是 float、SQLite 是 bool 存储）
> 的方言歧义——不在名单的 interval/range/array/time-of-day/json 一律
> fail-closed。PG lane 加真 interval/time/int4range 列的端到端拒收回归。
>
> **round-6 复审修正**（两条，均红→绿）：① catalog 列名此前按原样
> （`c.Name()`）作 map key，而 lookup 用模型的小写 `DBName`——
> SQLite/MySQL 标识符大小写不敏感，`versioned/off` 建的 `QTY INTEGER`
> 列 catalog key 是 "QTY"、查 "qty" 落空，合法表被误拒（查询本身因大小写
> 不敏感能跑，只是门禁自己漏配）。改为 `aggCatalogKey`：SQLite/MySQL 建
> map 与 lookup 都折小写、PG 保留（quoted 标识符大小写敏感，且 GORM 发的
> 就是模型名）。② 精确白名单漏了 round-4 明确承诺的字符串族成员——SQLite
> 的 `char`/`nchar`（此前只有 `character`/`nvarchar`，CHAR 是 TEXT
> affinity 完全可分组）、MySQL 的 `enum`（INFORMATION_SCHEMA 基础类型名
> "enum"，本质是有界字符串）。补齐后 SQLite CHAR、MySQL ENUM 的
> GroupBy/CountDistinct 各加真库回归（后者进 make test-mysql 正则）。
> 探针实测确认 catalog 名：SQLite `type:char(8)`→"char"、MySQL
> `type:enum(...)`→"enum"、MySQL `nchar`→"char"（已被 char 覆盖）。
>
> **round-7 复审修正**（1H/1M，均红→绿）：① round-6 的大小写折叠用了
> Go 的 `strings.ToLower`（完整 Unicode 折叠），把 Kelvin 记号 U+212A
> 折成 ASCII `k`——但 SQLite 的标识符比较是 `sqlite3_stricmp`（**仅
> ASCII**），一张同时含 `k TEXT` 与 Unicode-distinct `U+212A INTEGER`
> 的表（SQLite 视为两列）会在 map 里键碰撞、后者覆盖前者，门禁把 TEXT
> 列误判为 integer，`Min[int64]` 又退回文本字典序（2 输给 10，静默错，
> Store 路径复现）。改 `asciiLower`（只折 A–Z），与数据库自身的标识符
> 比较对齐——两列保持 distinct，TEXT 列 fail-closed。② 首次聚合的
> catalog 读（`ColumnTypes` 会跑 `sqlite_master` 查询 + `SELECT ...
> LIMIT 1`，MySQL 查 information_schema + 数据表）排在 scope **之前**，
> 未认证请求对属主模型会先触碰数据库、可能先返回表/权限错误而非
> `ErrUnauthenticated`。改为**先建 scoped base**（`applyScopes` 纯内存、
> 无 round-trip，未认证即 401）再解析 catalog，并复用该 base 走查询
> （scope 只跑一次）——回归用 gorm callback 计数断言未认证聚合的 DB
> 操作数为 0、认证聚合 >0。
>
> **round-8 复审修正**（1M，红→绿）：round-7 只挡住了未认证路径——
> **已认证**的首次聚合里，gorm `Migrator.ColumnTypes` 的实现会对数据表
> 跑无 scope 的 `SELECT * FROM <table> LIMIT 1` 来嗅探列型（SQLite/
> MySQL migrator 皆如此），等于读了任意租户的一行（值不外泄，但违反
> 「所有读取经过 scope」边界；round-7 的操作数回归只数次数、看不出
> SQL 形状，故漏检）。按复审首选方案改**纯元数据查询**：SQLite
> `pragma_table_info(?)`、PG `information_schema.columns` 的
> `udt_name`、MySQL 的 `data_type`——catalog 表是 schema 元数据非租户
> 数据，且任何语句不再引用数据表。类型名与既有白名单兼容（udt_name/
> data_type 正是此前 probe 到的拼法；SQLite pragma 原样带长度，补
> `aggBaseType` 截 "(" 归一——"char(8)"→"char"）；零列改为**报错不缓存**
> （元数据查询对缺表不报错，缓存空 map 会把先建 store 后迁移的形态
> 永久毒化）。回归改为 **SQL 形状断言**：callback 捕获全部语句，凡引用
> 数据表者必须带 owner 谓词（修前捕获到裸 `SELECT * FROM products
> LIMIT 1`，红）；另 pin 缺表不毒化缓存的重试语义。
>
> **round-9 复审修正**（2M/1L，M 均红→绿）：① round-8 的纯元数据查询把
> `modelSchema.Table` **整串**当裸 table_name——点限定 TableName
> （SQLite `main.t`、PG `schema.t`、MySQL `db.t`，GORM quoter 渲染数据
> 查询时按点拆分引用）在三方言 catalog 里都查不到，已迁移的表首次聚合
> 即报 "no columns / 未迁移"。改为按方言拆 qualifier 与裸表名（与
> quoter 拆分一致）：SQLite `pragma_table_info(?, ?)` 第二参数钉
> attached 库、MySQL `table_schema = ?`。② PG 的
> `table_schema = current_schema()` 只看 search_path **表头**：
> `search_path = first, second` 且表只在 second 时，未限定 SELECT 照常
> 解析、聚合却误报未迁移。改经 `pg_catalog`：`to_regclass`（各段 Go 侧
> 预 quote，防未限定段被折小写）按 SQL parser 同款规则解析——限定名归
> 属自己的 schema、未限定名走整条 search_path，列型取 `pg_attribute` +
> `pg_type.typname`（与 udt_name 同拼法，白名单不变）。回归覆盖三方言
> 限定表 + PG 双 schema search_path（事务内 SET LOCAL，同连接上裸
> SELECT 正常而聚合曾红）。③（L）清理仍按现在时描述已删
> `Migrator.ColumnTypes` 实现的内部注释（resolveAggCatalog / 白名单 /
> aggScopedBase 及孤儿 aggBase 段落），db.md/design.md 同步真实机制。
>
> **round-10 复审修正**（2H，均红→绿）：① round-9 取 `pg_type.typname`
> 作列型——但 typname 可被用户 domain 冒充：`CREATE DOMAIN app."int8"
> AS text` 的列按裸名过整数门禁，`Min[int64]` 对 '2'/'10' 静默返回
> 字典序 10（information_schema 的 udt_name 本会解到底层 text，
> round-9 换源时丢了该语义）。改为递归 CTE 沿 `typbasetype` 解 domain
> 至最终非 domain 类型，且仅 `pg_catalog` 命名空间保留裸名——用户类型
> 渲染为 `schema.name`，永不匹配白名单；诚实的 domain-over-bigint
> 照常聚合（round-8 语义保留）。② catalog cache 原为每 Store 单值，
> round-9 又允许未限定表随事务 search_path 解析——schema-per-tenant
> 下同一 Store 两个事务命中不同表，门禁复用首个表的列型（bigint 缓存
> 罩住 text 表，字典序 MIN 静默错）。改为**按 relation OID 分键**：
> 每次聚合先 `to_regclass` 探针（COALESCE→0 视为未迁移，错误不缓存），
> 列读改以该 OID 为参——探针、列读、聚合同连接同解析，缓存键与列读
> 天然一致；SQLite/MySQL 仍单条目（blessed 栈无 ATTACH/USE，连接级
> 解析固定）。回归：shadow domain（含 domain-over-bigint 正例）+
> 双 schema 同名异型表三事务往返（换 relation 各自 fail-closed/正确，
> 回到首个 relation 缓存仍有效）。
>
> **round-11 复审修正**（2M）：① 表名拆分用裸 `strings.Split(".")`，
> 不认引用边界——而 GORM quoter 是 **quote-aware 且引用符分方言**的：
> `"a.b".t` 在 PG（引用符 `"`）是两段、引号内的点属于 schema 名，在
> MySQL/SQLite（引用符反引号）却真是三段。盲拆把 PG 上可正常迁移/写入
> 的表打成三段，聚合报 `cross-database references are not implemented`
> （红测复现原文）。改为按方言引用符拆分（引号内点不分割、`""` 反转义
> 为字面引号），引用符**问 dialector 自己**（渲染 `a` 取首字节）而非
> 硬编码；拆完再用 GORM quoter **回渲比对**，任何不一致（未闭合引号、
> 未来 GORM 改规则）一律 fail-closed 指向 Unsafe——这条校验才是安全性
> 的来源，而非拆分器本身写得多细。回归：三方言真 dialector 逐例比对
> 拆分回渲（含 `"a.b".t` 的 PG 两段 / MySQL 三段分歧、`"W""Q".t`
> 转义、未闭合引号闭门），加 PG 上「schema 名含点」真库端到端。
> ② round-10 的 catalog cache 用 atomic copy-on-write 单 map，每发现
> 一个新 relation 都在全局锁下整份复制——正是文档承诺支持的
> schema-per-tenant 形态，N 个租户首访累计 O(N²) 复制且彼此串行。改
> `sync.Map`（键稳定、只增、warm-up 后读多写少），首访不复制不串行；
> 同一 relation 并发首访至多重复一次纯元数据读，`LoadOrStore` 定唯一
> 赢家。无独立红测（性能特征，非正确性回归——正确性由 round-10 的
> relation 分键回归守着），改以 8 租户并发首访测试在 `-race` 下证明新
> 路径无竞态且各租户拿到自己的列型与结果。
>
> **round-12 复审修正**（1M + 二过跟进 1H/1M + 三过跟进 1M/1L，均
> 红→绿）：
> round-11 声称的「同一 relation 并发首访至多重复一次元数据读」不成立
> ——每个冷调用者都是 miss 后先各自查完、`LoadOrStore` 才裁决缓存
> 赢家：它去重的是**缓存**不是**加载**，K 个并发冷请求就发 K 条相同
> catalog 读（复审以屏障在真 PG 稳定捕获 8/8）。改为按缓存键
> （PG=relation OID，SQLite/MySQL=0）singleflight 合并：同一波非事务
> 调用者共享一次读，等待服从各自 ctx；失败从不缓存，真实 catalog 失败
> 整波共享一个错误。**显式事务调用者不进 flight**、在自己连接上直读：
> 停靠等待的事务持着连接，而 SQLite 写池钉死单连接（db/open.go），会
> 构成 leader 等池、池等事务、事务等 leader 的环；非事务等待者不持
> 任何连接，等待图无环。
>
> 首版实现复审二过再修两处：①（H）事务直读后曾照常 `LoadOrStore`
> 发布——但事务读到的可能是**只有它自己可见**的未提交 DDL：SQLite 上
> 事务内建表（qty INTEGER）→聚合→**回滚**，泄漏的条目罩住随后真实
> 建出的 qty TEXT 表，`Min[int64]` 过门禁静默返回字典序结果、不
> fail-closed。改为事务读只供本次调用；三过再修（1M）：二过曾以
> 「AfterCommit 只省一次读」为由**完全不发布**，并拿「保留缓存读让
> OID cache 对 SET LOCAL 租户流有效」自辩——但该流程**全在事务里**，
> 事务只读不写即全程无 writer，缓存永不回暖：同 relation 连续两次
> 成功事务稳定 = 2 次 catalog 读（SQLite 与真 PG 皆复测），二过的性能
> 论证自相矛盾，round-10「tx1 缓存/tx3 命中」与 round-11 并发租户测试
> 描述的缓存行为亦随之失真。终版：**成功提交后经 `db.AfterCommit`
> 发布**（与 WithBus 事件同一 staging 锚点，抽共用 stageOnCommit；
> 回滚整体丢弃、不泄漏）——提交那一刻，事务中途读到的私有可见性恰好
> 成为已提交状态，round-10/11 的缓存叙事也随之复真。共享缓存的
> **读**路径事务保留：条目恒为已提交状态（写入者 = 非事务 leader +
> 已提交事务的 staged 发布），事务自建的 relation 在契约内不可能命中
> 既有条目（PG 新表是新 OID；SQLite/MySQL 键 0 已有条目意味着表早已
> 存在、与事务内 CREATE 矛盾），剩余分歧即「存量 relation 的运行期
> DDL」= 文档化的重启契约。（1L）楔死 flight 测试内一处注释仍称事务
> 读「会发布」、与相邻断言相反，随语义翻转一并修正。
> ②（M）leader 被取消时健康等待者曾**各自直读**：8 个停靠等待者 = 8
> 次元数据读，一次首请求超时即重建 round-11 的 herd。改为健康等待者
> **回流二次 singleflight**（失败 call 在结果交付前已从 group 移除，
> 二波正常合并），循环只在 channel 上停靠、经自身 ctx 或非取消错误
> 退出。回归：8 并发冷首访恰好 1 次读（修前 8 次，无需屏障即稳定）；
> 楔死 flight 证明事务不停靠、**提交前不发布、提交后恰好发布**、
> 非事务等待者服从自身 ctx；事务内建表→聚合→**回滚**不泄漏、后建
> TEXT 表 fail-closed（修前静默字典序）；同 relation 连续两次成功
> 事务恰好 1 次读（修前 2 次）；取消 leader 后 8 健康等待者恰好 1 次
> 读（修前 8 次）。
>
> **刻意不做**（v1 边界，均已写进 db.md/design.md）：HAVING（聚合结果上
> 的表达式谓词，与表达式 ORDER BY 同类，无法白名单化——小结果集在内存
> 过滤）；按聚合值 ORDER BY + LIMIT 的 top-N 下推（组基数=白名单列的
> distinct 值，仪表盘量级在内存排序即可；若真实需求出现，预批准方向是
> **序数 ORDER BY**——按框架自产 select 位置排序，不引入调用方表达式）；
> 多列 GROUP BY（无 codegen 时结果形状无法类型化，留给 backlog #4）；
> 字符串/布尔列的 MIN/MAX（文本序按方言 collation 定义，跨方言不可
> 承诺）。`CountDistinct`/`CountDistinctOf` **进** v1：单列 COUNT(DISTINCT)
> 三方言一致，对照框架（Rails/Django/Ecto）皆为标配，排除它会把最常见的
> unique-count 仪表盘继续压回 Unsafe——正与本项立项理由相悖。

## 2.0.0-beta.6 — 数据契约收口：接口划线 + 悲观锁 + 错误 provenance + 批量写与游标工具 + 迁移 manifest/repair 留痕

> 架构复核暴露的八处数据层契约缺口在同一轮收口：显式 update 白名单和
> alias 不能再把 RID/version/时间戳/软删/owner 等框架托管列重新打开，
> 普通 Update、软删与 Restore 的成功 SQL 都推进 version；Upsert 不推进
> version 仍作为单独、已公开的方言限制保留。`Fields` 只接受 Store 的具体
> 模型类型，不再把形状兼容 DTO 交给 GORM 推断字段与乐观锁元数据。事件
> payload 改为包外可解释的 Locator/Object/Change snapshot，递归隔离调用方
> 与多个异步订阅者的可变数据。
>
> `where.Option` 因已有公开扩展契约暂不封闭，而是明确提升到与 Unsafe 相同的
> 可信代码边界，并以内建 NULL 谓词覆盖已知逃逸理由；分页 cap 只允许逐层
> 收紧。auto 迁移在首条 DDL 前预检全部声明，字段解析统一服从 GORM schema；
> PG/MySQL unlock 有独立 deadline，失败即废弃 session；SQLite 依据扩展错误码
> 区分 UNIQUE 与 NOT NULL/CHECK/FK，不再把所有 constraint failed 都报重复键。

> `BatchUpdate` 与 `BatchUpsert` 补齐了“每行不同 payload”和“批量
> insert-or-update”两块操作面，同时不把 `Writer[T]` 扩成会打碎下游 mock
> 的胖接口；需要批量能力的依赖显式使用 `BatchWriter[T]`。
>
> 这次没有照搬单行 Upsert 的历史事件行为。conflict-update 时 create hook
> 生成的 RID 不一定属于数据库旧行，因此 Upsert 统一发布无 payload 的
> `OpUpsert`，让订阅者做实体类型级失效，而不是缓存一个可能不存在的对象；
> `BatchUpsert` 每次调用只发布一条，输入规模不会放大相同的失效工作。
> BatchUpsert 还把静态白名单、空指针与完全相同的批内 conflict key 校验
> 前置到 hooks/SQL 之前，并将“数据库相等规则下批内键必须唯一”写成公开
> 前置条件，避免固定分片边界改变 PostgreSQL 结果。
>
> `BatchUpdate` 复用单行 Update 内核，scope、零值写入与乐观锁只有一份
> 事实源；自管事务失败时恢复此前由框架递增的内存 Version，避免数据库
> 已回滚而对象重试立即产生伪 stale conflict。
>
> GA 前把接口视图的划线一次理顺（不兼容）。`Writer[T]` 的历史形状
> （含 BatchCreate + Upsert）违反了上文「不扩张 Writer 以保 mock」的
> 拆分理由，划线原则改为**单行 vs 批量**：Upsert 是单行写、留在
> Writer；BatchCreate 移出 Writer（连带 ReadWriter）、归 BatchWriter；
> `*Store` 方法集不变，经 `Writer[T]` 依赖批建的调用方改声明
> `BatchWriter[T]`。同一轮把 `ListFromQuery` 移出 `Reader[T]`——
> repository 读契约不该 import `net/url`，解析传输层输入属于边缘
> （`handler.HandleList`，或 `*Store` 上保留的 HTTP 糖）；其返回值
> 与 `List` 统一为 `*Page[T]`，消灭四返回值的形状分裂。
>
> 信封类型本体随之下沉：`Page[T]` 移入 `where`（store 与 handler
> 共同的 query 层），`store.Page` 变为泛型别名——handler 由此与
> store 说同一个信封而维持「handler 不 import store」的既有边界。
> 别名保住拼写与 JSON 形状（字段零变化），但**类型身份**变了：
> 反射（`PkgPath`/`Name`）、type registry 与 apidiff 眼中
> `store.Page` 及以其为返回值的 `List` / `ListQ` / `Reader.List`
> 都记为类型变化；以类型身份做注册表键或缓存键的下游需要知道
> 这一点。HTTP 响应面零变化。
>
> 悲观锁补上了与乐观锁的不对称：`GetForUpdate` 在事务内做
> `SELECT ... FOR UPDATE`，是「必须赢」的读-改-写序列的正门（此前只能
> `Unsafe`）。入口按可验证性设计：不在本句柄事务内直接返回
> `ErrLockRequiresTx`——autocommit 下行锁在方法返回前就释放，而裸透传
> clause 只会制造静默降级（glebarez 恰好就是静默丢弃 Locking 的驱动）；
> 只读 store 拒绝（锁是写意图）；`WithPreload` 拒绝（关联查询在锁外，
> 拼在一起是「看起来原子」的陷阱）。SQLite 的语义不依赖驱动渲染而依赖
> 既有形态：事务独占唯一写连接（文件库单连接写池、默认
> `_txlock=immediate`；内存库整库仅一条固定连接），并发写者不存在，
> 强于行锁，三方言可观测保证一致（锁定读到提交之间无并发写者）。
> `SKIP LOCKED` / `NOWAIT` 刻意未做，等真实需求出现再议。
>
> 游标契约补上尺寸纪律。不透明令牌此前两侧皆无界：decode 侧任意长的
> token 先解码再报错，令牌长度直接换服务端 base64/JSON 分配；encode 侧
> 超长字符串边界照签，签出的令牌可能大到下一页 URL 装不下。现在两个
> 数值成为公开契约（`MaxCursorTokenLen` 4KB / `MaxCursorValueLen` 1KB）：
> 客户端令牌超限在任何解码前 400 拒收，边界值超限在签发侧作为服务端
> 字段契约错误拒签——游标列是短标量键不是 payload。两界互锁：JSON 转义
> 能把逼近 1KB 的控制字符边界膨胀过 4KB，因此签发侧在组装后再验一次
> 令牌长度，「绝不签出自己拒收的令牌」由此机械成立而非靠推理。截断
> 从一开始不在选项里——被截断的边界会让下一页从错误位置继续，是静默
> 错页的另一种拼法。
>
> 查询错误的 HTTP 语义此前只看错误种类不看来源：`where.ErrUnknownField`
> 一律预映射 400，于是 `List(ctx, where.WithFilter("typo", v))` 这种服务端
> 编程 bug 伪装成「客户端传参错误」沉底，监控永远看不见（不兼容变更）。
> 现在按**入口**划界——字段名在程序化入口（List/Count/Pluck/ListIn/游标
> 字段/locator）由服务端代码书写，未声明原样返回 → 500 且保留错误链；
> 只有 `ListFromQuery` 链的字段名来自 URL，维持整链 400（parse 腿映射，
> List 腿留幂等兜底防未来漂移）。**值**错误（`ErrInvalidParam`）不分入口
> 维持 400——page/size/游标 token/过滤值本来就合法地从客户端流进这些入口。
> 划界的代价写成公开约定：handler 不得把客户端**字段名**直接拼进
> `WithFilter`/`WithOrder`（typo 会以 500 浮出），要么走 `ListFromQuery`
> 要么先校验；update 侧 `ErrUnknownUpdateField` 本来就是 500，不动。
> 这与 `ErrFieldNotConfigured`（一直 500）终于同一逻辑：**谁写的错，谁的
> 状态码。**
>
> 重复键错误此前只会把驱动消息里提取的约束/索引名放进响应 metadata——
> 那是 schema 命名，对客户端既是布局泄露、又随迁移改名而漂移。现在
> Store 可以用 `WithConstraintFields` 做 Ecto `unique_constraint` 式的
> 声明映射：命中的重复键报**公开字段名**（API 自己的词汇表，迁移改索引
> 名不破坏客户端分支逻辑），未声明的约束保持现状——渐进收敛而非一刀切。
> 映射放构造选项而非模型 tag：约束名是迁移层工件，属于装配处知识，而
> `store` tag 体系管的是字段暴露面；且同一模型在不同 store（公开/管理）
> 可能要不同的归因。匹配按驱动上报形态归一化——PG 报裸索引名、MySQL 8
> 报 `table.key`、SQLite 根本不报索引名只报列清单（SoftUnique 含
> delete_token）——方言差异写进文档而不是假装不存在。
>
> 两步 IN 是设计钦定的跨表读形态（JOIN DSL 刻意不做），钦定形态就该配
> 全套工具：`ListIn` 把 `where.MaxInList` 之上的手动分块循环收进框架。
> 语义锚点是「一条大 IN」的**集合语义**，它要两层去重才成立：值集先按
> Go 等值去重（值跨块重复不得让行翻倍），跨块结果再按**主键**去重——
> 数据库等值可能比 Go 宽（大小写不敏感 collation 把 "a"/"A" 视作同值，
> MySQL 默认 collation 即如此），同一行合法命中两块时单条 IN 只返回一次，
> 分块不得改变这一点；身份键选内部主键而非 RID（RID 唯一但绕过 store 的
> 裸写入可为空，主键永不冲突）。空值集不走捷径：仍执行一次退化查询
> （WHERE 1=0），白名单 / 守卫 / fail-closed scope 全部照常——空输入
> 不得把字段 typo 或未认证拒绝藏成一张静默空页，与
> `List(WithFilterIn(field))` 空集行为严格一致。每块完整走 List 的
> 白名单 / scope / 软删路径，读面绝不比 List 宽。只收过滤 option：排序、
> 分页、count、页大小 cap 在单块内成立、跨块静默失效，这类「看着完整
> 实则缺行/错序」的选项直接拒收；Store 的 max-page-size cap 同理被刻意
> 绕开——那是面向客户端列表的护栏，按块套用等于把行悄悄裁掉。多块是
> 多条 SELECT 而非单语句，并发写下合并结果不是单快照读——而且「放进
> 事务」只在默认隔离给出**事务级快照**的方言上成立（`Store.Tx` /
> `db.RunInTx` 用数据库默认隔离）：SQLite（事务独占唯一写连接）与
> MySQL/InnoDB（REPEATABLE READ 首读定快照）满足，PostgreSQL 默认
> READ COMMITTED 每语句新快照**不满足**——按方言诚实写进契约，PG 需要
> 时经 Unsafe 在事务首句自行升级 REPEATABLE READ；不为此新开隔离级别
> 旋钮（等真实需求）。结果因此
> 不保证顺序、大小由值集决定，与 `ListByIDs` 同属服务端管道，文档明确
> 指向键形字段。
> 声明 owner，全局 manifest 用数据库 claim 把 kind/账本归属持久化，并以
> engine floor 阻止较旧的 manifest-aware 引擎写入。claim 校验位于迁移锁内，
> 覆盖 apply、repair 与 owner transfer；存量账本先完成只读 TOFU 预检，再在
> 第一笔账本/schema 写之前持久化 owner，崩溃不会把已验证的归属重新暴露为
> unclaimed。
>
> 通用 CLI 能通过 manifest + 前缀扫描展示第三方账本，但不能凭空取得组件
> 内嵌 SQL，因此严格 `status --check` 将 content-unverified 视为非 clean；
> 操作者必须显式选择 `--ledger-health-only` 才只检查 dirty/fence/floor 等
> 文件无关健康状态。声明式组件身份保留 fork/vendor 可控性，没有采用脆弱的
> runtime caller/build-info 推导。
>
> 电池迁移的 catalog 指纹闸门新增了真实 DML 半边和 N-1 三路径：SoftUnique
> 重占槽、自增续接、JSON 语义与 casbin 数据库层去重按方言对照；fresh、
> versioned 前缀升级、旧 auto 基线采纳必须收敛到同一指纹和行为轨迹。迁移
> 文件前缀只证明 versioned schema frontier，旧运行时回填由电池 fixture
> 显式复现，避免把历史二进制能力错误归因给 checksum。
>
> repair 是迁移体系里唯一「人说了算」的写入，此前只回传一次性报告、旧状态
> 被原地覆盖。现在每次 repair（含 claim 转移）在自己的事务里向 append-only
> 的 `schema_migrations_chok_repairs` 落一行完整证据，写不进历史即整体失败；
> fence 清理在 PG/MySQL 并入同一提交。历史行的语义刻意收窄为「业务状态 CAS
> 已提交」而非「调用方观察到成功」，防篡改边界诚实声明为授权纪律而非密码学。
> claim 转移补上了此前缺失的 reason；operator 显式输入不容静默丢弃。

## 2.0.0-beta.5 — 数据层加固：只读实例 + 电池独立迁移账本

> 本轮把 db-layer 架构 review 的收敛推进到类型、配置与生成物里。
>
> **只读实例**：`db.read_only: true` 让"实例角色"成为受强制的能力而非
> 命名约定——有效迁移模式降为 off、拒绝显式 versioned、事务/迁移/store
> 写/GORM 写回调一律 `db.ErrReadOnly`，store 绑定需显式
> `store.WithReadOnly()`。事务上下文携带句柄亲和，具名实例不再静默搭上
> 另一实例的事务；SQLite `mode=ro`、PG/MySQL 每连接只读默认为纵深防线，
> 数据库只读账号/物理副本仍是最终权限边界。
>
> **电池独立迁移账本**：account、audit、authz 不再在 `migrate: versioned`
> 下寄生 AutoMigrate，而是各自携带三方言迁移集与
> `schema_migrations_chok_<kind>` 审计账本。应用迁移序号保持纯业务所有；
> 框架电池也获得 checksum、dirty、repair 与方言身份语义。存量表只有在
> 完整 catalog 指纹与声明等价版本精确一致时才自动采纳，不完整或更老的
> 形状 fail-closed。部署可用 `chok migrate up --component` / `--all-owned`
> 将 DDL 权限移出业务进程。首次引入账本保持表形状不变；未来不兼容 DDL
> 必须先排空旧副本或采用 expand/contract，不能把账本 fence 当成对仍
> 运行旧进程的保护。
>
> **同批收敛**：`db.store` 应用级 store 策略（strict / require_principal /
> 分页上限，生产硬化变成配置翻转）；versioned 迁移审计链（checksum、
> crash-persistent dirty、fence、`repair retry|mark-applied|accept-drift`）；
> db 运行态 Prometheus 观测（查询/连接池/迁移/SQLite maintenance，迁移
> 指标带 `sequence` 标签）；组件 `Descriptor.Schema` 声明框架表所有权，
> `db.FrameworkTables()` 由此生成。

## 2.0.0-beta.4 — 一行到位的路由动词

> 主题:「对 gin 式极简的回答是越过它」。`web.GET/POST/PUT/PATCH/
> DELETE` 把路由与类型化绑定层融成一行——一行 = 路由 + 请求绑定 +
> 响应编码 + 错误映射 + OpenAPI 登记;gin 的闭包里仍要手写 bind 与
> JSON,样板真正消失在类型层。每个 helper 严格等于 `r.Handle +
> handler.HandleRequest`(DELETE 走 `HandleAction`,204,REST 惯例),
> 零绕行:Meta 依旧进路由表(测试钉住防未来绕行),中间件依旧走
> Group。配套把「零 yaml、单文件 21 行、`go run .` 即起且自带访问
> 日志/优雅关停」的实测 hello world 放上双语 README 首屏——极简的
> 另一半本来就在(缺省路径缺 yaml 不报错、`Execute()` 即
> `r.Run()`),这次把它亮出来。装配仪式(`chok.Use`)是显式装配
> 公理的自觉代价,由 `chok init` 生成而非删除。

## 2.0.0-beta.3 — SQLite 并发默认值拉满

> 主题:「嵌入式数据库也不该让用户背 DSN 咒语」。文件库 SQLite 现在
> 默认注入 `_txlock=immediate`(写事务 BEGIN 即取写锁,消灭 deferred
> 读写升级在竞争下不吃 busy_timeout 直接 SQLITE_BUSY 的经典陷阱——
> 反证实验 3/3 必现,回归测试钉住排队行为)与 `_synchronous=NORMAL`
> (WAL 下安全,写吞吐数倍于 FULL);WAL 沿 v1 已内建,busy_timeout
> 经核实为驱动自带 5000ms 默认、不再重复注入。用户显式 DSN 参数
> (含别名拼写)恒优先。新增 `sqlite.max_open_conns`(idle 跟随),
> 写重场景压 1 让写者在 Go 侧排队。配套:`docs/db.md` 新增 §16
> 项目组织与分层、故障排除补 "database is locked" 条目。

## 2.0.0-beta.2 — 数据层的声明式收口

> beta.1 后的第一轮增量,主题是「**定义模型即定义操作面**」——把
> 数据层的日常使用成本再往下压,同时保持全部 fail-closed 语义。

- **`store` tag 字段声明**:白名单从调用点搬到字段旁
  (`store:"query,update"`),加字段时声明就在手边;调用点的
  `WithQueryFields`/`WithUpdateFields` 保留为按消费者收窄的手段
  (特权/公开双视图场景)。曾评估过「继承 model 即得操作」的
  ActiveRecord 方向并否决:Go 无 self-type、携带句柄的 model 破坏
  事务与 scope 语义、且违背单一 blessed 实现公理——声明式 tag 是
  同一诉求在类型系统内的正确表达。
- **`Restore` 与 `Count` 补齐常规操作面**:软删恢复由框架持有全部
  不变量(`delete_token` 归还、SoftUnique 槽冲突映射、scope 不泄露
  存在性);计数不再借道 `List(WithCount)`。`Reader[T]` 接口随之
  扩面(对自定义实现者是编译期可见的一行增补)。
- **使用文档**:[`docs/db.md`](db.md) 按 Diátaxis 四象限组织,
  安全默认值与故障排除入表;配置表链接生成物而非复制,与三道
  防漂移闸同一纪律。

## 2.0.0-beta.1 — v2：把不变量翻译进类型与控制面

> **v2 重写的首个可安装版本**（M0-M5 六个里程碑的产物）。module
> path 升级 `github.com/zynthara/chok/v2`；39 条破坏性变更逐条见
> [迁移指南](migration-v1-to-v2.md)。一句话主题：**把 v1 靠文档、
> warn 日志和 code review 维持的十几条不变量，翻译成类型系统和
> 单线程控制面的结构性保证**。

### 为什么重写

v1 的结构性债务在同一处反复付息：反射式 auto-register 需要一张
多模式矩阵去解释；live config 指针带来 torn read 与整套拷贝纪律；
`reloadMu → mu` 锁序靠背诵；13 个类型断言可选接口是接口动物园；
gin 与 gorm 类型钉死公开 API。v2 的答案不是修补而是换地基：

- **显式装配替代反射扫描**——`chok.Use(模块())` 是唯一注册面，
  链接器自然裁剪二进制；`chok sync` 让 yaml 驱动的零思考体验与
  显式 import 和解。
- **单 actor 控制面替代锁序**——生命周期只有一个写者，读路径走
  原子快照；「Close 里别调 Get」从守则变成结构性不可能。
- **不可变 RCU 替代 live 指针**——reload 失败零污染不再靠小心
  拷贝，而是「没交换就没发生」。
- **Descriptor + 行为接口替代接口动物园**——声明是数据，能力是
  行为，7 个静态声明接口消失。
- **stdlib ServeMux 替代 gin、`*db.DB` 薄句柄藏死 gorm**——公开
  面的第三方类型清零；raw SQL 收敛为两扇自名其险的 `Unsafe` 门。

### 设计层新增

- 注册-禁用模型：disabled 组件有四条定义良好的语义，注册拓扑不再
  随配置漂移。
- `reload:"hot|restart"` 字段 tag：热载语义上提到类型层，框架统一
  diff 与 warn。
- 事务模型收敛：context 传播唯一化，`AfterCommit` 把实体事件锚定
  提交，回滚无幻影。
- 版本化迁移双轨 + 电池表白名单 + Postgres day-one。
- casbin 决策审计真值表：`audit_enabled=true` 时审计缺失即启动
  失败——合规语义绝不静默退化。
- 生成面治漂移：组件表 / 配置参考 / JSON Schema 全部由 Descriptor
  与 Options 生成，CI 三道闸（docs gen --check / apidiff /
  examples 冒烟）把漂移挡在合入前。

### 移除

`component/`、`parts/`、`server/`、`config/` 四包；hook 系统
（`On`）、`AddCleanup`、`WithConfig/WithSetup`；after-hooks；
gin、badger 及其传递依赖树。逐条对照见迁移指南。

---

## 0.1.4 — v1 封版：account 多 provider / authz(Casbin) / audit 落地

> **本版本是 v1 功能线的封版（feature freeze）**：此后 v0.1.x 只接收
> 安全修复；`main` 转入 v2 重构，module path 升级为
> `github.com/zynthara/chok/v2`。v1 用户请锁定 `v0.1.4`。

### account：多登录方式体系（密码 + OAuth）

- **User Store 双面化**：`Module.Store()` 返回的公开 store 白名单收窄
  到 `name` / `email`；`password_hash` / `password_version` / `roles` /
  `active` / `email_verified` 等敏感列只能经 Module 的管理 API 写入
  （store API 层强制，raw `*gorm.DB` 逃生舱不在此列，见 design.md）。
- **管理 API**：`UpdateUserRoles` / `SetUserActive` /
  `BumpPasswordVersion` / `MarkEmailVerified` —— 前三者以单条原子
  UPDATE 实现并递增 password_version（任何隔离级别下无丢失更新），
  使被禁用/改权用户的存量 access token 在下一次请求即失效。
- **AuthChain()**：返回 blessed 的 `[Authn, ActiveCheck]` 中间件对；
  裸 `middleware.Authn` 不查库，无法感知 PV bump —— 业务受保护路由
  应一律挂 AuthChain。
- **OAuth 抽象层**：`AuthProvider` 接口 + `ProviderCapabilities`
  能力声明（回调方法 / nonce / PKCE / form_post）；`Identity` 表以
  `(provider, provider_account_id)` 唯一化；OAuth 会话与一次性
  auth code 各自独立存储（默认内存 LRU + TTL，**多副本部署必须注入
  Redis 后端**）；sid 走 HMAC-SHA256 签名 HttpOnly cookie，密钥由
  SigningKey 经 HKDF 派生（与 JWT 签名材料密码学隔离）。
- **登录路由**：`GET /auth/{name}/start` →（IdP）→
  `GET|POST /auth/{name}/callback` → 302 前端 `?code=…` →
  `POST /auth/exchange` 用一次性 code 换 JWT（token 永不进 URL）；
  `redirect_back` 仅允许相对路径或显式 allowlist 前缀。
- **配置驱动装配**：`AccountOptions` 新增 `LinkByEmail` /
  `AllowedRedirectBacks` / `OAuthCallbackFrontendURL` /
  `Providers map[string]ProviderRawOptions`（`,remain` 捕获 provider
  专有键）；启用任一 provider 而缺 frontend URL 启动即 fail-fast；
  未知 provider 名 fail-fast 并列出可用名单；`enabled: false` 即
  kill switch。
- **blessed provider 全家桶**：chok 核心默认 blank-import
  `account/providers/blessed`，google / github / facebook / apple
  四个 provider 零 Go 代码即可用（yaml 开关）。Apple 处理 form_post
  回调、ES256 client_secret（golang-jwt）与 go-oidc id_token 校验。
  精简构建的逃生舱：fork 掉 providers.go 的一行 import。
- **/login 语义**：OAuth-only 账号（PV=0 且仅有 Identity 记录）用
  密码登录返回 401 + reason `OAUTH_ONLY_ACCOUNT`，不计入登录限速。
- **可选关闭公开注册**沿 0.1.3 的 `DisableRegister` 继续有效，与
  admin provisioning 流程配合。
- apierr 新增 `ErrGone`(410) / `ErrFailedPrecondition`(412) 与
  `WithReason` / `WithDetails`。

### authz：Casbin 授权器 + 多租户中间件

- `authz.DomainAuthorizer` 子接口（`AuthorizeInDomain`）与
  `DomainAuthorizerFunc` 适配器。
- 中间件面改版（**不兼容变更**）：URL 模式的 `Authz(az)` 移除，代之
  `RequireAuthz(obj, act)` / `RequireAuthzInDomain(obj, act,
  domainParam)` / `AttachAuthz(az)`；租户中间件遇到非
  DomainAuthorizer **fail-closed 返回 500**（静默降级会放行跨租户
  请求）。HTTPComponent 经 optional dependency 自动注入 AttachAuthz。
- `authz/casbin` 新包：RBAC-with-domains 内置模型（直接授权 / 域内
  角色 / `*` 全局角色三段 matcher）、`SyncedEnforcer`、Service 管理
  面（域名归一化、拒绝 `*` 伪装租户）、幂等 admin bootstrap。
- **自研 GORM adapter**：与 gorm-adapter v3 的 `casbin_rule` schema
  线上兼容（存量部署无迁移），但把 pgx / go-mssqldb / 纯 Go SQLite
  等十余个传递驱动依赖挡在直接依赖之外；examples/blog 全量构建的
  Casbin 增量从 +9.93 MB（gorm-adapter v3）降到 +1.21 MB。
- **自研 Redis Watcher**：骑 `parts/redis` 做多实例策略同步（仅
  `persist.Watcher` 契约；实例 ID 抑制自发布；Close 与
  AuthzComponent 的关停顺序有保证）。`redis_watcher: true` 而 redis
  组件缺失时 Build fail-fast。
- `config.AuthzOptions`（driver 仅 `casbin`）+ autoregister 接线
  （硬依赖 db，软依赖 redis / audit）。`audit_enabled: true` 当前
  fail-fast（audit 集成随 v2 里程碑补齐）。
- 三轮评审加固：adapter 线上兼容细节、LoadPolicy CSV 安全解析与
  fail-fast、`Update*` 精确规则匹配、Watcher 生命周期契约。

### audit：审计数据面 + 异步 DB sink（Phase 7.A/7.B）

- `audit` 包：`Log` 模型（表名钉死 `audit_logs`，三组按
  actor / resource / action × time 的组合索引）、`Entry` / `Query` /
  `Logger`（`LogSync` / `Log` / `Query`）契约、`FromContext` /
  `MergeContext`（自动提取 Principal / ClientIP / TraceID /
  RequestID，全部可缺省）、`Stats` / `Statser` 观测逃生舱。
- `config.AuditOptions`：`Enabled` / `AsyncBufferSize` / `DropOnFull` /
  `RetentionDays` / `PurgeInterval` / `PurgeBatchSize` /
  `EnableAdminAPI`；reload 语义已注记（Retention/Purge* 热更，
  Buffer/DropOnFull 需重启）。
- `parts.AuditComponent` + 异步批量 DB sink（单 worker，批 100 条或
  1s flush；`DropOnFull` 决定满载丢弃计数或阻塞含 ctx 取消）。
- **已知限制**：Phase 7.C/7.D（admin 查询 API、retention purge cron、
  autoregister 接线）未落地 —— `EnableAdminAPI` / `Purge*` 字段当前
  无消费者，组件需经 `WithSetup` 手动装配。该欠账转入 v2 电池迁移
  里程碑补齐。

### 依赖面

- 新增直接依赖：`casbin/v3`、`coreos/go-oidc/v3`、`golang.org/x/oauth2`、
  `go-viper/mapstructure/v2`（转正）、`gorm.io/datatypes`；
  测试依赖 `miniredis/v2`。
- 移除：`casbin/gorm-adapter/v3` 及其带入的全部驱动直接依赖。

---

## 0.1.3 — account 管理面 + 嵌套配置发现

- **account**：`Module.Store()` 暴露 User store（继承 New 时配置的
  查询/更新白名单，调用方绕不过 schema 限制）；`ActiveCheck` 增加
  pv claim 校验 —— 管理操作 bump PasswordVersion 后，存量 access
  token 下一次请求即失效，不再等刷新；新增
  `AccountOptions.DisableRegister` / `account.WithoutPublicRegister()`
  关闭公开注册（`POST /register` 返回 404），适配「管理员预置账号」
  部署。
- **config**：auto-register 的 `discoverOne` 从只扫顶层字段升级为
  全树 DFS —— 业务 Config 可以用 `cache: { memory, file }` 这类自然
  嵌套组织内置 Options。六条扫描规则（指针字段整棵跳过、命中 `*T`
  停止下降、`SelfValidating` 是不透明边界、`time.Time` 类原子结构体
  跳过等）与 `validateNoPointerOptions` 的递归保持对称。
- **apierr**：`RegisterMapper`（进程级全局）弃用，脚手架与示例切换
  到 per-App 的 `chok.WithErrorMapper` —— 全局 mapper 在多 App /
  并行测试场景下互相踩踏。解析顺序、nil-panic 语义不变。

---

## 0.1.2 — release 构建的版本语义修正

- **`chok version` 的 dirty 位语义**（对外可见的行为契约）：一旦
  ldflags 注入了显式版本（release tag / `make build` / goreleaser），
  `debug.ReadBuildInfo` 的 `vcs.modified` 即被忽略 —— release runner
  的 worktree 噪声（merge commit、构建缓存、go.sum touch）不再产出
  `+dirty` 后缀；dev 构建（`go run` / `go test` / `go install @latest`）
  保留实时 dirty 位，那里它才与"用户改没改代码"相关。
- **release 流水线**：release-please 工作流内联 goreleaser ——
  GITHUB_TOKEN 创建的 tag 因 GitHub 反循环策略不会触发跨 workflow，
  独立的 goreleaser.yml 永远看不到 release-please 打的 tag。
  goreleaser.yml 保留为开发机手动 push tag 的 fallback（幂等，
  `mode: replace`）。
- 无 Go API 变更。

---

## 0.1.1 — release 流水线修复

- goreleaser before-hook 移除冗余 `go mod tidy`：它会把 release
  runner 的 worktree 弄成 git 眼中的 modified 状态，导致干净的
  release tag 构建出 `0.1.0+dirty` 版本串。CI 已在每次 push 时跑
  tidy，release 阶段无需重复。
- 无 Go API 变更。

---

## 0.1.0 — Initial public release

chok 的首个公开版本。提供「配置驱动的 Go Web 全家桶」的完整骨架：
HTTP / 数据层 / 缓存 / 鉴权 / 调度 / 可观测性都收敛到一份 `chok.yaml`
+ 一个 `Config` 结构体。

### 内置能力

15 个内置 Component（详见 `design.md` §9）覆盖：

- HTTP server (gin) + 默认中间件栈
- ORM (gorm) + 自动迁移 + 软删除 + Owned 模型
- 多层缓存：Otter (memory) → Badger (file) → Redis，含 circuit breaker
- 账号模块：注册 / 登录 / JWT / 密码重置 / 登录限速
- 任务调度：robfig cron + panic-safe + 统计
- 异步任务池：有界 worker pool
- 可观测性：Prometheus `/metrics` + OpenTelemetry tracing + Health/Ready/Live
- API 文档：从泛型 handler 自动生成 OpenAPI 3.0

### 关键设计契约

- **Component 抽象**：所有子系统统一 `Init` / `Close` 强制契约，
  `Reload` / `Health` / `Router` / `Migrate` / `ReadyChecker` 等可选
  接口由 Registry 通过类型断言发现。
- **Config-driven**：自动从 `Config` 结构体字段反射出需要的内置
  Component；`enabled: true|false` 是启停子系统的主要开关。
- **Discriminator 配置**：`DatabaseOptions.Driver` 选择 sqlite/mysql；
  `config.SelfValidating` 标记类型让递归校验器跳过未选分支。
- **Two-phase atomic reload**：配置热加载先验证副本，全部通过才原子
  替换 live config，失败时零字段污染。
- **Lock order**：`reloadMu → mu`。`Stop` 与 `Reload` 互斥；
  `Reload` 用 `TryLock`，并发触发返回 `ErrReloadInProgress`。
- **Reload 不触发 Migrate**：schema 变更需要重启进程。
- **Shutdown context**：所有 shutdown 路径用 `context.WithoutCancel(parent)`
  保留 trace_id / request_id。
- **Get during phaseStopping**：只返回 `available` 集合中的组件，
  Stop 在每次 Close 后立即清除。

### Store API

- 6 个方法（Create / Get / List / Update / Delete / Tx）取代独立 CRUD
- Locator 抽象：`RID(x)` / `ID(x)` / `Where(opts...)`
- Changes 抽象：`Set(map)` / `Fields(&obj)`，后者自动提取 `obj.Version`
  实现乐观锁
- 强制 `WithQueryFields` / `WithUpdateFields` 白名单（auto-discovery
  仅做开发期 warn）
- `Where` Locator 用于 Update/Delete 时强制至少一个 filter，防止误清表
- Cursor pagination：`WithCursor`（严格唯一字段） / `WithCursorBy`
  （复合 keyset，处理同值边界）
- 标识符校验：`store/where/where.go` 在 `resolveField` 处拒绝非
  `[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?` 的列名

### CLI

`chok` 二进制提供：

- `chok init <name>` — 脚手架（项目名取自路径 basename）
- `chok version [--json|--short]` — 构建/VCS/运行时元信息
- `chok update [--ref|--check|--dry-run]` — 通过 `go install` 升级

版本元信息按 ldflags → `debug.ReadBuildInfo` → `dev`/`unknown`
顺序解析。

### 已知限制

- 数据库驱动仅 sqlite 与 mysql；postgres 在路线图（见
  [`roadmap.md`](roadmap.md)）
- 内置 Component 不含 WebSocket / gRPC（同上）
- Schema 迁移仅 `AutoMigrate`，无版本化迁移工具

### 下游兼容性承诺

0.x 版本期间可能有不兼容变更，但每次都会在本文档对应版本段落明确
列出，配合 release-please 生成的 [`CHANGELOG.md`](../CHANGELOG.md)
让升级者一眼看到影响面。1.0 之后遵循语义化版本。
