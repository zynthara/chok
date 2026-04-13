<p align="center">
  <a href="README_zh.md">中文</a> | English
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
  <sub>Convention-Driven Go Web Framework</sub>
</h1>

<p align="center">
  <b>Type-safe handlers &middot; Zero-config stores &middot; Auto-generated OpenAPI &middot; Built-in auth</b>
</p>

---

**chok** is an opinionated Go web framework that eliminates boilerplate through convention and generics. Define your model, register a route — the framework handles query fields, update fields, owner scoping, pagination, validation, error mapping, and API documentation automatically.

```go
// Model — tags are the single source of truth
type Post struct {
    db.OwnedSoftDeleteModel
    Title   string `json:"title"   gorm:"size:200;not null" binding:"required,max=200"`
    Content string `json:"content" gorm:"type:text;not null" binding:"required"`
    Status  string `json:"status"  gorm:"size:20;default:'draft'" binding:"oneof=draft published"`
}

// Store — zero configuration needed
posts := store.New[Post](gdb, logger)

// Route — one line, fully typed
rg.GET("/posts", handler.HandleList[Post](posts))

// Swagger — auto-generated from the above
swagger.Generate(&cfg.Swagger, engine)
```

---

## Why chok?

| Pain Point | chok's Answer |
|---|---|
| Declaring queryable/updatable fields for every model | **Auto-discovered** from `json` tags. `json:"-"` = excluded. `type:text` = not queryable. Base model fields = not updatable. |
| Writing pagination/filter/sort boilerplate for every list endpoint | **`HandleList`** parses `?page=1&size=20&status=draft&order=created_at:desc` automatically. |
| Hand-wiring owner isolation (`WHERE owner_id = ?`) | **Auto-detected.** Models embedding `db.Owned` get `OwnerScope` applied automatically. |
| Writing Swagger annotations that duplicate handler signatures | **Zero annotations.** OpenAPI spec generated from handler type parameters via reflection. |
| Setting up register/login/password-reset from scratch | **One line:** `account.Setup(gdb, logger, &cfg.Account, srv.Group("/auth"))` |

---

## Architecture

```
                          ┌─────────────────────────────────┐
                          │           chok.App               │
                          │  Config · Logger · Cache · DB    │
                          └────────────┬────────────────────┘
                                       │
              ┌────────────────────────┼────────────────────────┐
              │                        │                        │
     ┌────────▼────────┐    ┌─────────▼─────────┐   ┌─────────▼─────────┐
     │   HTTPServer     │    │   Account Module   │   │   Custom Servers  │
     │   (Gin Engine)   │    │   register/login   │   │   gRPC / Workers  │
     └────────┬────────┘    │   JWT / password    │   └───────────────────┘
              │              └───────────────────┘
    ┌─────────┼──────────┐
    │   Middleware Stack  │
    │  Recovery · ReqID   │
    │  Logger · CORS      │
    │  Authn · Authz      │
    └─────────┬──────────┘
              │
    ┌─────────▼──────────┐       ┌──────────────┐
    │   Typed Handlers    │──────▶│   Swagger    │
    │  HandleRequest[T,R] │       │  Auto-Gen    │
    │  HandleAction[T]    │       │  OpenAPI 3.0 │
    │  HandleList[T]      │       └──────────────┘
    └─────────┬──────────┘
              │
    ┌─────────▼──────────┐
    │   Generic Store     │
    │  Store[T]           │
    │  Auto query/update  │
    │  OwnerScope         │
    │  Optimistic Lock    │
    └─────────┬──────────┘
              │
    ┌─────────▼──────────┐
    │   Model Layer       │
    │  Model              │
    │  SoftDeleteModel    │
    │  OwnedModel         │
    │  OwnedSoftDelete    │
    └────────────────────┘
```

---

## Feature Highlights

### Typed Handlers — Compile-Time Safety

```go
// Request + response types known at compile time. No interface{}.
func (h *PostHandler) create(ctx context.Context, req *CreateReq) (*Post, error) {
    // req is already bound from JSON body + URI params + query params
    // req is already validated via binding tags
    // errors automatically mapped to structured JSON responses
}

rg.POST("/posts", handler.HandleRequest(h.create, handler.WithSuccessCode(201)))
```

Multi-source binding from a single struct:

```go
type UpdateReq struct {
    RID     string  `uri:"rid"     binding:"required"`     // from URL path
    Title   *string `json:"title"  binding:"omitempty"`     // from JSON body
    Status  *string `json:"status" binding:"oneof=draft published"`
}
```

### Zero-Config Store

```go
// This is all you need. Query fields, update fields, owner scope
// are all auto-discovered from the model's struct tags.
posts := store.New[Post](gdb, logger)

// Auto-discovered behavior:
// - Query:  title, status, id, version, created_at, updated_at (json tags, excludes text/blob)
// - Update: title, content, status (json tags, excludes id/version/timestamps)
// - Owner:  WHERE owner_id = principal.Subject (auto-detected from db.Owned)
// - Admin:  bypasses owner filter (default role: "admin")
```

Override when needed:
```go
store.WithQueryFields("id", "title")       // explicit whitelist
store.WithAllQueryFields("internal_note")  // auto + custom exclude
store.WithoutOwnerScope()                   // disable owner isolation
store.WithDefaultPageSize(50)               // default 20
store.SetDefaultAdminRoles("superuser")     // global admin role
```

### Auto-Generated OpenAPI

```go
// Route registration is spec declaration. Zero annotations.
rg.POST("/posts", handler.HandleRequest(h.create, handler.WithSuccessCode(201)))
rg.GET("/posts", handler.HandleList[Post](posts))
rg.GET("/posts/:rid", handler.HandleRequest(h.get))

// One line generates the full OpenAPI 3.0 spec + Swagger UI
swagger.Generate(&cfg.Swagger, srv.Engine())
```

Schemas, parameters, validation rules, security requirements — all derived from Go types.

### Built-in Account Module

```yaml
# Enable in config — that's it
account:
  enabled: true
  signing_key: "your-secret-key-at-least-32-bytes"
```

```go
acct, _ := account.Setup(gdb, logger, &cfg.Account, srv.Group("/auth"))
```

Provides: `POST /register`, `POST /login`, `POST /refresh-token`, `PUT /change-password`, `POST /forgot-password`, `POST /reset-password`. JWT-based, bcrypt passwords, email normalization, masked display names, configurable token expiration.

### Ownership Model

```go
type Product struct {
    db.OwnedModel  // adds owner_id, auto-filled from JWT subject
    Name string `json:"name"`
}

// Users see only their own products. Admins see all.
// No code needed — auto-detected from model type.
products := store.New[Product](gdb, logger)
```

### Multi-Layer Caching

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

Memory (Otter) → File (Badger) → Redis. Auto-discovered from config struct.

### Structured Lifecycle

```go
app := chok.New("myapp",
    chok.WithConfig(&cfg),
    chok.WithSetup(setup),
)
app.Execute()

// Lifecycle: load config → init logger → init cache → setup → start servers
//            → wait for signal → stop servers (reverse) → cleanup (LIFO)
```

---

## Quick Start

```bash
# Install CLI
go install github.com/zynthara/chok/cmd/chok@latest

# Scaffold a new project
chok init myapp
cd myapp
go mod tidy
make run
```

Generated structure:

```
myapp/
├── cmd/myapp/main.go
├── internal/
│   ├── app/
│   │   ├── config.go      # Config with Account + Swagger options
│   │   └── server.go       # Setup: DB, account, routes, swagger
│   └── handler/
│       └── handler.go       # Example /me endpoint
├── configs/myapp.yaml
├── Makefile
└── go.mod
```

Out of the box: user registration, login, JWT auth, Swagger UI at `/swagger/`.

---

## Packages

| Package | Description |
|---|---|
| `chok` | App lifecycle, server management, config loading, signal handling |
| `account` | User registration, login, JWT, password reset |
| `apierr` | Typed API errors with HTTP status codes |
| `auth` | Principal context, password hashing (bcrypt) |
| `auth/jwt` | HS256 JWT signing and parsing |
| `authz` | Pluggable authorization interface |
| `cache` | Multi-backend caching (memory / file / Redis / chain) |
| `config` | Typed config options with validation |
| `db` | GORM helpers, base models, migrations |
| `handler` | Typed request handlers with multi-source binding |
| `log` | Structured logger interface (slog-based) |
| `middleware` | Recovery, RequestID, Logger, CORS, Authn, Authz |
| `rid` | Prefixed resource identifiers (e.g. `pst_aB3cD4eF5g`) |
| `server` | Gin-based HTTP server |
| `store` | Generic CRUD store with scoping and optimistic locking |
| `swagger` | Auto-generated OpenAPI 3.0 from handler metadata |
| `validate` | Typed validation functions |
| `version` | Build-time version info injection |

---

## Design Principles

- **Convention over configuration** — Struct tags are the single source of truth. The framework reads `json`, `gorm`, `binding`, `uri`, `form` tags to determine queryable fields, updatable fields, parameter sources, validation rules, and API schemas.

- **Type safety through generics** — `HandlerFunc[T, R]`, `Store[T]`, `HandleList[T]` provide compile-time guarantees. No `interface{}`, no type assertions in user code.

- **Fail-fast at startup** — Invalid RID prefixes, misconfigured stores, nil scopes — all panic at construction time, not at request time.

- **Fail-closed at runtime** — Unauthenticated requests to scoped stores return 401, not empty results. Missing fields return errors, not silent zeroes.

- **Zero annotations** — OpenAPI specs generated from Go types. No comment-based annotations. No code generation step.

---

<p align="center">
  <sub>Built with Go generics &middot; Powered by Gin + GORM &middot; Designed for developer velocity</sub>
</p>
