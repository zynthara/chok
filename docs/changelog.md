# Design Changelog

> 此文档记录 chok 公开契约层面的设计变迁——新增能力、不兼容变更、
> 弃用与移除。**实现细节不在此处**，请直接看 [`docs/design.md`](design.md)。
>
> 项目使用 [Conventional Commits](https://www.conventionalcommits.org/)；
> 自 v2 起 release 手动裁切（写根目录 [`CHANGELOG.md`](../CHANGELOG.md)
> 条目 → 打 tag → goreleaser 发布）。本文与之互补：那份记录"哪个
> 变更进了哪个版本"，本文记录"为什么这次发布的设计选择是这样"。

---

## Unreleased — 数据契约收口 + 批量写 + 接口划线 + 悲观锁 + 游标尺寸契约 + 两步 IN 工具补全 + 迁移 manifest/repair 留痕

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
> 两步 IN 是设计钦定的跨表读形态（JOIN DSL 刻意不做），钦定形态就该配
> 全套工具：`ListIn` 把 `where.MaxInList` 之上的手动分块循环收进框架。
> 语义锚点是「一条大 IN」——值集先去重（IN 是集合语义，值跨块重复不得
> 让行翻倍），每块完整走 List 的白名单 / scope / 软删路径，读面绝不比
> List 宽。只收过滤 option：排序、分页、count、页大小 cap 在单块内成立、
> 跨块静默失效，这类「看着完整实则缺行/错序」的选项直接拒收；Store 的
> max-page-size cap 同理被刻意绕开——那是面向客户端列表的护栏，按块
> 套用等于把行悄悄裁掉。结果因此不保证顺序、大小由值集决定，与
> `ListByIDs` 同属服务端管道，文档明确指向键形字段。
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
