<p align="center">
  中文 | <a href="README.md">English</a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26+-00ADD8?style=for-the-badge&logo=go&logoColor=white" />
  <img src="https://img.shields.io/badge/License-MIT-green?style=for-the-badge" />
  <img src="https://img.shields.io/badge/OpenAPI-3.0-85EA2D?style=for-the-badge&logo=swagger&logoColor=black" />
</p>

<h1 align="center">
  <br>
  <code>chok</code>
  <br>
  <sub>约定优于配置的 Go Web 框架</sub>
</h1>

<p align="center">
  <b>类型安全 &middot; 零配置 &middot; 自动生成 API 文档 &middot; 内置用户系统</b>
</p>

---

**chok** 是一个基于约定的 Go Web 框架，通过泛型和结构体标签消除重复代码。定义模型、注册路由 —— 框架自动处理查询字段、更新字段、权限隔离、分页、校验、错误映射和 API 文档。

```go
// 模型 —— 标签即真相
type Post struct {
    db.OwnedSoftDeleteModel
    Title   string `json:"title"   gorm:"size:200;not null" binding:"required,max=200"`
    Content string `json:"content" gorm:"type:text;not null" binding:"required"`
    Status  string `json:"status"  gorm:"size:20;default:'draft'" binding:"oneof=draft published"`
}

// Store —— 零配置
posts := store.New[Post](gdb, logger)

// 路由 —— 一行搞定，完全类型安全
rg.GET("/posts", handler.HandleList[Post](posts))

// Swagger —— 自动从上面的代码生成
swagger.Generate(&cfg.Swagger, engine)
```

---

## 为什么选择 chok？

| 痛点 | chok 的解决方案 |
|---|---|
| 每个模型都要手动声明查询/更新字段 | **自动发现** —— 从 `json` 标签推导。`json:"-"` 自动排除，`type:text` 不可查询，基础模型字段不可更新。 |
| 每个列表接口都要写分页/过滤/排序样板代码 | **`HandleList`** —— 自动解析 `?page=1&size=20&status=draft&order=created_at:desc`。 |
| 每个 Store 都要手写 `WHERE owner_id = ?` | **自动检测** —— 模型嵌入 `db.Owned` 即自动应用 `OwnerScope`。 |
| Swagger 注解重复声明 handler 已有的类型信息 | **零注解** —— 通过反射从 handler 的类型参数自动生成 OpenAPI 规范。 |
| 从零搭建注册/登录/密码重置 | **一行启用：** `account.Setup(gdb, logger, &cfg.Account, srv.Group("/auth"))` |

---

## 架构

```
                          ┌─────────────────────────────────┐
                          │           chok.App               │
                          │  配置 · 日志 · 缓存 · 数据库     │
                          └────────────┬────────────────────┘
                                       │
              ┌────────────────────────┼────────────────────────┐
              │                        │                        │
     ┌────────▼────────┐    ┌─────────▼─────────┐   ┌─────────▼─────────┐
     │   HTTP 服务器     │    │   用户模块         │   │   自定义服务器     │
     │   (Gin Engine)   │    │   注册/登录/JWT    │   │   gRPC / Worker  │
     └────────┬────────┘    └───────────────────┘   └───────────────────┘
              │
    ┌─────────┼──────────┐
    │     中间件栈        │
    │  Recovery · 请求ID  │
    │  日志 · CORS        │
    │  认证 · 授权        │
    └─────────┬──────────┘
              │
    ┌─────────▼──────────┐       ┌──────────────┐
    │   类型化 Handler    │──────▶│   Swagger    │
    │  HandleRequest[T,R] │       │   自动生成    │
    │  HandleAction[T]    │       │  OpenAPI 3.0 │
    │  HandleList[T]      │       └──────────────┘
    └─────────┬──────────┘
              │
    ┌─────────▼──────────┐
    │   通用 Store        │
    │  Store[T]           │
    │  自动查询/更新字段   │
    │  OwnerScope         │
    │  乐观锁             │
    └─────────┬──────────┘
              │
    ┌─────────▼──────────┐
    │     模型层          │
    │  Model              │
    │  SoftDeleteModel    │
    │  OwnedModel         │
    │  OwnedSoftDelete    │
    └────────────────────┘
```

---

## 核心特性

### 类型安全的 Handler

```go
// 请求和响应类型在编译时确定，没有 interface{}
func (h *PostHandler) create(ctx context.Context, req *CreateReq) (*Post, error) {
    // req 已从 JSON body + URI 参数 + Query 参数自动绑定
    // req 已通过 binding 标签自动校验
    // 错误自动映射为结构化 JSON 响应
}

rg.POST("/posts", handler.HandleRequest(h.create, handler.WithSuccessCode(201)))
```

一个结构体，多源绑定：

```go
type UpdateReq struct {
    RID     string  `uri:"rid"     binding:"required"`          // 来自 URL 路径
    Title   *string `json:"title"  binding:"omitempty"`          // 来自 JSON Body
    Status  *string `json:"status" binding:"oneof=draft published"`
}
```

### 零配置 Store

```go
// 这就是全部。查询字段、更新字段、权限隔离
// 全部从模型的结构体标签自动推导。
posts := store.New[Post](gdb, logger)

// 自动行为：
// - 查询：title, status, id, version, created_at, updated_at（json 标签，排除 text/blob）
// - 更新：title, content, status（json 标签，排除 id/version/时间戳）
// - 权限：WHERE owner_id = principal.Subject（从 db.Owned 自动检测）
// - 管理员：绕过 owner 过滤（默认角色："admin"）
```

需要时可以覆盖：

```go
store.WithQueryFields("id", "title")       // 显式白名单
store.WithAllQueryFields("internal_note")  // 自动 + 自定义排除
store.WithoutOwnerScope()                   // 关闭权限隔离
store.WithDefaultPageSize(50)               // 默认 20
store.SetDefaultAdminRoles("superuser")     // 全局管理员角色
```

### 自动生成 OpenAPI 文档

```go
// 路由注册就是文档声明。零注解。
rg.POST("/posts", handler.HandleRequest(h.create, handler.WithSuccessCode(201)))
rg.GET("/posts", handler.HandleList[Post](posts))
rg.GET("/posts/:rid", handler.HandleRequest(h.get))

// 一行自动生成完整的 OpenAPI 3.0 规范 + Swagger UI
swagger.Generate(&cfg.Swagger, srv.Engine())
```

Schema、参数、校验规则、安全方案 —— 全部从 Go 类型推导。

### 内置用户模块

```yaml
# 在配置文件中启用 —— 仅此而已
account:
  enabled: true
  signing_key: "你的密钥至少32字节"
```

```go
acct, _ := account.Setup(gdb, logger, &cfg.Account, srv.Group("/auth"))
```

自动提供：`POST /register`、`POST /login`、`POST /refresh-token`、`PUT /change-password`、`POST /forgot-password`、`POST /reset-password`。基于 JWT，bcrypt 密码加密，邮箱大小写归一化，脱敏显示名，可配置 token 过期时间。

### 资源归属模型

```go
type Product struct {
    db.OwnedModel  // 自动添加 owner_id，从 JWT Subject 填充
    Name string `json:"name"`
}

// 用户只能看到自己的产品，管理员看到全部。
// 无需任何代码 —— 从模型类型自动检测。
products := store.New[Product](gdb, logger)
```

### 多级缓存

```yaml
cache:
  memory:
    enabled: true
    capacity: 10000
    ttl: 5m
  file:
    enabled: true
    path: .cache
    ttl: 1h
```

内存 (Otter) → 文件 (Badger) → Redis。从配置结构体自动发现。

### 结构化生命周期

```go
app := chok.New("myapp",
    chok.WithConfig(&cfg),
    chok.WithSetup(setup),
)
app.Execute()

// 生命周期：加载配置 → 初始化日志 → 初始化缓存 → Setup → 启动服务
//           → 等待信号 → 停止服务（逆序）→ 清理回调（LIFO）
```

---

## 快速开始

```bash
# 安装 CLI
go install github.com/zynthara/chok/cmd/chok@latest

# 创建新项目
chok init myapp
cd myapp
go mod tidy
make run
```

生成的项目结构：

```
myapp/
├── cmd/myapp/main.go
├── internal/
│   ├── app/
│   │   ├── config.go      # 配置：Account + Swagger
│   │   └── server.go       # Setup：数据库、用户模块、路由、Swagger
│   └── handler/
│       └── handler.go       # 示例 /me 端点
├── configs/myapp.yaml
├── Makefile
└── go.mod
```

开箱即有：用户注册、登录、JWT 认证、Swagger UI（`/swagger/`）。

---

## 包一览

| 包 | 说明 |
|---|---|
| `chok` | 应用生命周期、服务管理、配置加载、信号处理 |
| `account` | 用户注册、登录、JWT、密码重置 |
| `apierr` | 带 HTTP 状态码的类型化 API 错误 |
| `auth` | Principal 上下文、密码哈希（bcrypt） |
| `auth/jwt` | HS256 JWT 签发与解析 |
| `authz` | 可插拔授权接口 |
| `cache` | 多后端缓存（内存 / 文件 / Redis / 链式） |
| `config` | 带验证的类型化配置选项 |
| `db` | GORM 辅助、基础模型、数据迁移 |
| `handler` | 类型安全的请求处理器，多源绑定 |
| `log` | 结构化日志接口（基于 slog） |
| `middleware` | Recovery、RequestID、Logger、CORS、Authn、Authz |
| `rid` | 前缀资源标识符（如 `pst_aB3cD4eF5g`） |
| `server` | 基于 Gin 的 HTTP 服务器 |
| `store` | 通用 CRUD Store，支持作用域和乐观锁 |
| `swagger` | 从 handler 元数据自动生成 OpenAPI 3.0 |
| `validate` | 类型化验证函数 |
| `version` | 构建时版本信息注入 |

---

## 设计原则

- **约定优于配置** —— 结构体标签是唯一真相来源。框架读取 `json`、`gorm`、`binding`、`uri`、`form` 标签来确定查询字段、更新字段、参数来源、校验规则和 API Schema。

- **泛型带来类型安全** —— `HandlerFunc[T, R]`、`Store[T]`、`HandleList[T]` 提供编译时保证。用户代码中没有 `interface{}`，没有类型断言。

- **启动时快速失败** —— 无效的 RID 前缀、配置错误的 Store、nil scope —— 全部在构造时 panic，不在请求时。

- **运行时安全关闭** —— 未认证请求访问有作用域的 Store 返回 401，而不是空结果。缺失字段返回错误，而不是静默的零值。

- **零注解** —— OpenAPI 规范从 Go 类型生成。不需要注释注解，不需要代码生成步骤。

---

<p align="center">
  <sub>基于 Go 泛型构建 &middot; 驱动于 Gin + GORM &middot; 为开发效率而设计</sub>
</p>
